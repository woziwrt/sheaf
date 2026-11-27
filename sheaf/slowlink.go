// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"math"
	"time"
)

// A link whose delivery rate collapses while its queue is full (real LTE, 25 Nov: Vodafone from ~40 to ~1 Mbit/s
// for 1-6 s every 10-25 s) holds the whole bundle head-of-line. Nothing is lost and the link keeps making a little
// progress, so neither the loss repair nor the dead-link logic fires: its 100-800 kB already in flight trickle out
// at the collapsed rate while the receiver's reorder buffer fills behind the first of them. A TCP flow through the
// tunnel times out (rtt 80 ms -> 1-6 s), its cwnd collapses, and a single download reached ~55 % of the link sum.
//
// Until v57 every repair path measured "has this packet had time to arrive" by the slowest live link's srtt – and
// the collapsed link's own srtt grew with its queue (1 s ... 6.6 s), so it protected its own stuck packets from
// repair. v58 (all of it switched off by -slowk 0, which is then v57):
//   - stuckCheck / rescueLate (sender, deadTick every 10 ms): a link is "stuck" when its oldest unacknowledged packet
//     is older than its healthy round trip (healthyUs: base delay + the queue the window cap permits + jitter +
//     feedback interval) AND its queue does not drain within twice that at its delivery rate (the larger of the last
//     two 100 ms samples: one noisy sample is no collapse, an ordinary halving of the rate drains in time). A stuck
//     link gets no new data, and the packets it would deliver later than twice its healthy round trip go again over
//     the other links – once per packet, only while another link is fine (acks younger than its healthy round trip):
//     when every link stops acking at once the cause is common (CPU or GC stall, the reverse path), not one link,
//     and copies would only double the load. The originals that still arrive are duplicates the receiver drops.
//   - guardRttUs: the NACK guard and the RTO count a stuck link's srtt only up to its healthy round trip.
//   - holeWaitMs (receiver): the still-missing report waited for a hole that made no progress for missAfter; a
//     collapsed link delivers one packet of the hole every ~10 ms, which reset that clock, so no report was ever
//     sent. The age of the oldest packet waiting in the reorder buffer is the measure now.
//   - the base delay is not re-measured from a queue of our own (slowSince re-base only with little in flight), and a
//     live link with data in flight gets no starvation probe (it queued behind the stuck data).

// slowK: a link counts as stuck when its oldest unacknowledged packet is older than slowK × its healthy round trip and
// its queue does not drain within 2 × slowK × that; 0 = off: every v58 change of the repair paths off (v57 behaviour).
var slowK = 1.0

func v58() bool { return slowK > 0 }

// healthyUs (under l.mu): the round trip this link has when it behaves as the regulator lets it: base delay, the
// queue it may hold on top (the window cap's maxQueue or the cut band, whichever is larger), jitter and the
// feedback interval. 0 while the base is not known.
func (l *link) healthyUs() float64 {
	if l.minRtt <= 0 {
		return 0
	}
	q := l.maxQ
	if q == 0 {
		q = maxQueueUs
	}
	q = math.Max(q, 1.5*l.minRtt)
	return l.minRtt + q + 4*l.jit + 2*float64(fbEvery/time.Microsecond)
}

// guardRttUs (under l.mu): the link's srtt as a "packet still on its way" guard; for a stuck link never more than its
// healthy round trip – its srtt is its own queue draining at a trickle and protected exactly the packets stuck in it.
func (l *link) guardRttUs() float64 {
	if l.stuck {
		if h := l.healthyUs(); h > 0 && l.srtt > h {
			return h
		}
	}
	return l.srtt
}

// ackFresh (under l.mu): the link acknowledged something within its healthy round trip (it can take a stuck link's
// packets). now = mono().
func (l *link) ackFresh(now int64) bool {
	h := l.healthyUs()
	return !l.dead && l.dst() != nil && h > 0 && now-l.progress < int64(h)*1000
}

// stuckCheck (under l.mu, reliable sender): is the link stuck, and how many bytes of its queue it still delivers in
// time (keep). Changes nothing. nowUs = the send-time clock (hist[].at).
func (l *link) stuckCheck(nowUs int64) (bool, float64) {
	// rateMax 0: no delivery sample since the (re)start – a rate of 0 would make the whole queue "late"
	if !v58() || l.dead || len(l.hist) == 0 || l.rateMax == 0 {
		return false, 0
	}
	h := slowK * l.healthyUs()
	if h <= 0 || float64(nowUs-l.hist[0].at) <= h {
		return false, 0
	}
	// the delivery rate: the larger of the last two 100 ms samples (one noisy sample is no collapse), less when no
	// feedback came for a while (the samples are taken on feedback)
	rate := math.Max(l.rate, l.rate2)
	if !l.ackAt.IsZero() {
		if el := time.Since(l.ackAt); el >= 100*time.Millisecond {
			rate = math.Min(rate, float64(l.ackBytes)/float64(el.Microseconds()))
		}
	}
	keep := rate * 2 * h // bytes the link still delivers within twice its healthy round trip
	return float64(l.inflight) > keep, keep
}

// rescueLate (under l.mu; takes reMu): queue for the other links the packets of a stuck link beyond keep bytes of its
// queue. A packet is marked rescued only when it was really queued: one acknowledged meanwhile, without a copy, of an
// old session or sent again since its transmission here (NACK, RTO, the dead-link probe) stays as it is – a dead link
// still reinjects it. now = mono().
func (p *peer) rescueLate(l *link, keep float64, now int64) int {
	ep := p.epoch.Load()
	n := 0
	off := 0.0
	p.reMu.Lock()
	for i := range l.hist {
		e := &l.hist[i]
		off += float64(e.size)
		if off <= keep || e.rescued || e.ep != ep || int32(e.seq-p.cumAck) < 0 {
			continue
		}
		d, ok := p.copies[e.seq]
		if !ok || p.copySent[e.seq] > e.at*1000+int64(time.Millisecond) {
			continue
		}
		e.rescued = true
		p.copySent[e.seq] = now                 // a still-missing report naming it now must not send it a second time
		p.setDue(e.seq, now+int64(time.Second)) // (v59 P2) until sendOn sets the due time of the rescue copy
		p.reinject = append(p.reinject, sentPkt{seq: e.seq, data: d, ep: ep})
		p.rescued++
		p.rescuedB += int64(e.size)
		n++
	}
	if n > 0 {
		p.kickRepairs()
	}
	p.reMu.Unlock()
	return n
}

// Receiver: age of the oldest packet waiting in the reorder buffer. A mark (hi, ms) says: every packet buffered
// before the next mark has seq <= hi and arrived at ms or later (marks at most every waitMarkMs, so the age is
// exact to that). A mark is dropped when the in-order point passed its hi; the first mark left is the oldest wait.
type waitMark struct {
	hi uint32
	ms uint32 // lastRxData clock (ms since start)
}

const (
	waitMarkMs  = 5
	waitMarkMax = 4096 // a hole that never closes (non-reliable mode skips it) must not grow the list forever
)

// noteWait (under rxMu): seq was just put into buf.
func (p *peer) noteWait(seq uint32) {
	ms := p.lastRxData.Load()
	n := len(p.waitQ)
	if n > 0 {
		last := &p.waitQ[n-1]
		if int32(seq-last.hi) <= 0 {
			return // goes out before that mark is dropped
		}
		if ms-last.ms < waitMarkMs || n >= waitMarkMax {
			last.hi = seq
			return
		}
	}
	p.waitQ = append(p.waitQ, waitMark{seq, ms})
}

// dropWaits (under rxMu, after the in-order point moved).
func (p *peer) dropWaits() {
	if len(p.buf) == 0 {
		p.waitQ = p.waitQ[:0]
		return
	}
	k := 0
	for k < len(p.waitQ) && int32(p.next-p.waitQ[k].hi) > 0 {
		k++
	}
	if k > 0 {
		p.waitQ = append(p.waitQ[:0], p.waitQ[k:]...)
	}
}

// holeWaitMs (under rxMu): how long the oldest packet in the reorder buffer has waited, 0 = nothing waits.
func (p *peer) holeWaitMs(nowMs uint32) int64 {
	if len(p.buf) == 0 || len(p.waitQ) == 0 {
		return 0
	}
	return int64(int32(nowMs - p.waitQ[0].ms))
}

// appendUnrescued: a dead link's packets to resend, without those a rescue already sent again.
func appendUnrescued(dst, hist []sentPkt) []sentPkt {
	for _, e := range hist {
		if !e.rescued {
			dst = append(dst, e)
		}
	}
	return dst
}
