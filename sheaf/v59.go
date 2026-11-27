// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"flag"
	"fmt"
	"math"
	"time"
)

// v59: the changes after the analysis of the tunnel stalls on real LTE (25/26 Nov). Each has its own switch so it
// can be compared with v57 and rolled back on its own; -v57 switches all of them off (and the v58 repair changes).
//
// P0 instrumentation (always on): per link the bytes queued for its txLoop and not yet handed to the kernel (txq),
// and the packets txLoop really sent (Lx_tx counts what pick queued). A pacer stall shows as a growing txq with
// tx > txsent; a slow network shows as an empty txq.
//
// P1 (-pacepick, -txqcap): nothing a pacer could slow down waits behind the scheduling decision. Until v57 the pacer
// sat in txLoop, behind pick, and took its rate from the CURRENT window (1.25 × win / srtt, at least 1 Mbit/s) –
// also for packets queued under a larger window. A drain or the window cap then shrank the window while hundreds of
// kB sat in the link's queue, and they left in pairs every 21.8 ms (the 1 Mbit/s floor; on the hub 92 % of the
// 100 ms windows of a stuck link held an even packet count, 52 % elsewhere): the whole bundle waited head-of-line.
// Now a link is a candidate in pick only when its pace allows the packet now and its transmit queue holds less than
// -txqcap (one GSO batch); txLoop sends what it gets at once. The pace is paceGain × the larger of the delivery rate
// maximum of the last 1-2 s (rateMax) and win / srtt: a window cut never lowers it below what the link delivered
// lately (no learning from our own throttling), and the window term only lets it grow ahead of the delivery samples
// (ramp-up after a restart). The bucket holds 2 ms at the pace, at least 6 packets (as v51). With -pacelook D a link
// whose tokens come within D stays a candidate with that wait added to its expected arrival and takes the packet on
// credit when it wins (a burst stays on the fast link instead of spilling onto a slower one; bursts reach D of credit
// on top of the bucket) – off by default: in the lab 2 ms cost ~3 of 55 Mbit/s, so a paced-out link gives way at once.
// Redundant mode checks only the transmit queue (a copy may overdraw the pace).
//
// P2 (-fixguard): the time after which a transmission counts as late is fixed when it leaves, from the link it went
// on: send time + max(srtt, base + q) + q + 4 × jitter + the feedback interval, q = the queue the window cap allows
// (the second q: the queue may still grow after the packet left; LTE showed 208-226 ms under the cap). The NACK
// guard of the still-missing report and the head's RTO (at least the 1 s fallback) use it, not the srtt of the
// slowest link now – a link whose rate collapses after the packet left does not stretch it, and a deep but
// legitimate queue (in the srtt at send time) does not make copies.
//
// P3 (-drainfix): drain hygiene. No drain while the link's transmit queue is not empty, while it is stuck or its
// srtt is above its healthy round trip; the drain window is max(winMin, ½ × rateAvg × base) instead of 6 kB; after
// the drain (and after a short outage) the window returns ×2 per round trip, not in one line-rate step; the base
// after a drain is the mean of the lower half of its samples, not the plain average (which could lift it). A high
// srtt postpones a drain at most to 3 × drainEvery (after a cell change the base itself may have risen).
//
// P4 (-iprelease B): a bounded wait of the receiver for IP packets in the tunnel (reliable mode never skipped a hole).
// A buffered IP packet sent at s (sender clock) is released past the hole at s + max over the links delivering now of
// (one-way delay EWMA, at most its minimum + -ipreleasecap) + 4 × its jitter + B. The hole is still
// repaired and delivered late when it arrives (TCP sees reordering, not a multi-second stall); proxy frames (a
// reliable byte stream) never overtake. The cap of a link's delay by its minimum + -ipreleasecap (300 ms, more than
// any queue the far end's window cap allows): a link whose rate collapsed is still "live" and its delay EWMA grows
// with its queue to seconds – the deadline would never come. The receiver's own -maxq says nothing about the queue
// the far end builds towards it. All buffered IP packets up to the highest due one go out in sequence order.
//
// P5 (-rescue): the v58 rescue of a stuck link's late packets over the other links (stuck detection itself – no new
// data on such a link – stays on with -slowk > 0).

var (
	pacePick  = true
	txqCapB   = int32(45000)
	fixGuard  = true
	rescue    = true
	drainFix  = true
	ipRelease time.Duration // 0 = off
	relCap    = 300 * time.Millisecond
	paceLook  int64 // ns, -pacelook: a link whose tokens come within this stays a candidate (on credit); 0 = it gives way
	compatV57 bool
	txqCapKB  = 45
)

const (
	paceMaxBps = 1.25e9 // bytes/s: keeps paceBps × 1 s within int64
	txqWaitNs  = int64(200 * time.Microsecond)
	owdMinHalf = int64(5 * time.Second) // receiver: the one-way delay minimum covers 5-10 s
)

func v59Flags() {
	flag.BoolVar(&pacePick, "pacepick", pacePick, "v59 P1: pace (-pace) as a condition of pick, at gain × max(delivery rate max of 1-2 s, win/srtt); false = v57 pacer in txLoop at gain × win/srtt")
	flag.IntVar(&txqCapKB, "txqcap", txqCapKB, "v59 P1: a link takes no new packet while its transmit queue holds this many kB (0 = unlimited, v57)")
	flag.BoolVar(&fixGuard, "fixguard", fixGuard, "v59 P2: repair time fixed per packet at send time from its link (NACK guard, RTO); false = slowest link's srtt now")
	flag.BoolVar(&rescue, "rescue", rescue, "v59 P5: resend a stuck link's late packets over the other links (v58; needs -slowk > 0)")
	flag.BoolVar(&drainFix, "drainfix", drainFix, "v59 P3: drain only with an empty transmit queue, to ½ BDP, base = smallest sample, window back ×2 per round trip")
	flag.DurationVar(&ipRelease, "iprelease", ipRelease, "v59 P4 (reliable): release IP packets past a hole at send time + max link delay + this (0 = never, as v58)")
	flag.DurationVar(&relCap, "ipreleasecap", relCap, "v59 P4: a link's one-way delay counts at most its minimum + this (the queue the far end may build; a collapsed link's grows without bound)")
	flag.Func("pacelook", "v59 P1: a paced-out link whose tokens come within this stays a candidate in pick (wait added to its arrival, sent on credit); 0 = it gives way at once (lab: 2ms cost ~3 Mbit/s of 55)", func(v string) error {
		d, err := time.ParseDuration(v)
		paceLook = int64(d)
		return err
	})
	flag.DurationVar(&drainEvery, "drainevery", drainEvery, "base re-measurement (drain) interval per link (lab knob)")
	flag.BoolVar(&compatV57, "v57", false, "switch every v58/v59/v60 change off (= v57 behaviour, for A/B and rollback; the v60 control sends stay non-blocking)")
}

// v59Apply: after flag.Parse.
func v59Apply() {
	txqCapB = int32(txqCapKB * 1000)
	if compatV57 {
		pacePick, txqCapB, fixGuard, rescue, drainFix, ipRelease, slowK = false, 0, false, false, false, 0, 0
		slowFrac = 0 // v60
	}
}

func v59Summary() string {
	return fmt.Sprintf("pacepick=%v pacelook=%v txqcap=%dkB fixguard=%v slowk=%.2f rescue=%v drainfix=%v iprelease=%v ipreleasecap=%v drainevery=%v pace=%.2f",
		pacePick, time.Duration(paceLook), txqCapB/1000, fixGuard, slowK, rescue && v58(), drainFix, ipRelease, relCap, drainEvery, paceGain)
}

// ---- P1: pacing in pick ----

// paceRate (under l.mu, from derive): bytes per second the link may take, 0 = unpaced.
func (l *link) paceRate() int64 {
	if !pacePick || paceGain <= 0 || l.srtt <= 0 {
		return 0
	}
	r := paceGain * fmax(l.rateMax, l.win/l.srtt) * 1e6
	return int64(fmin(r, paceMaxBps))
}

// refill (under l.mu): tokens for the time since the last refill, up to 2 ms at the pace (at least 6 packets).
func (l *link) refill(now int64) {
	depth := max(l.paceBps/500, 6*1400)
	if l.paceAt == 0 {
		l.paceTok, l.paceAt = depth, now
		return
	}
	if dt := now - l.paceAt; dt > 0 {
		dt = min(dt, int64(time.Second))
		l.paceTok = min(depth, l.paceTok+l.paceBps*dt/1e9)
		l.paceAt = now
	}
}

// sendWait (under l.mu): 0 = the link may take n bytes now; otherwise ns until its pace allows them, and whether its
// transmit queue is what is full. An empty queue always takes a packet.
func (l *link) sendWait(n int, now int64) (int64, bool) {
	if l.txqFull(n) {
		return txqWaitNs, true
	}
	if l.paceBps <= 0 {
		return 0, false
	}
	l.refill(now)
	if l.paceTok >= int64(n) {
		return 0, false
	}
	return (int64(n)-l.paceTok)*1e9/l.paceBps + 1, false
}

// txqFull: the link's transmit queue cannot take n more bytes now (an empty queue always can).
func (l *link) txqFull(n int) bool {
	q := l.txqB.Load()
	return txqCapB > 0 && q > 0 && q+int32(n) > txqCapB
}

// spendPace (under l.mu): n bytes leave on the link (every transmission, also probes and copies: they may overdraw).
func (l *link) spendPace(n int, now int64) {
	if l.paceBps > 0 {
		l.refill(now)
		l.paceTok -= int64(n)
	}
}

// waitTx: wait ns (20-500 µs) for a pace, or until a txLoop made room in its queue when that is what is full.
func (p *peer) waitTx(ns int64, txq bool) {
	d := time.Duration(min(max(ns, 20000), 500000))
	if !txq || p.txRoom == nil {
		time.Sleep(d)
		return
	}
	t := time.NewTimer(d)
	select {
	case <-p.txRoom:
	case <-t.C:
	}
	t.Stop()
}

func (p *peer) signalTxRoom() {
	if p.txRoom == nil {
		return
	}
	select {
	case p.txRoom <- struct{}{}:
	default:
	}
}

// ---- P2: repair time fixed at send time ----

// repairSpanUs (under l.mu): how long a packet sent on this link now may take before it counts as late.
func (l *link) repairSpanUs() float64 {
	if l.srtt <= 0 || l.minRtt <= 0 {
		return 1e6 // not measured yet: the RTO fallback
	}
	q := l.maxQ
	if q == 0 {
		q = maxQueueUs
	}
	q = fmax(q, 0.5*l.minRtt) // the queue the window cap allows (onFeedback)
	// + q of headroom: the queue may grow after the packet left, and under the cap from rateMax (bursty acks inflate
	// it) the real queue reached 208-226 ms on LTE, more than q
	return fmax(l.srtt, l.minRtt+q) + q + 4*l.jit + float64(fbEvery/time.Microsecond)
}

// setDue (under reMu): the last transmission of seq counts as late from due (mono ns) on; 0 = not known.
func (p *peer) setDue(seq uint32, due int64) {
	if !fixGuard || due == 0 {
		return
	}
	if p.due == nil {
		p.due, p.dueSeq = make([]int64, maxCopies), make([]uint32, maxCopies)
	}
	p.due[seq&fprevMask], p.dueSeq[seq&fprevMask] = due, seq
}

// dueOf (under reMu).
func (p *peer) dueOf(seq uint32) (int64, bool) {
	if p.due == nil || p.dueSeq[seq&fprevMask] != seq || p.due[seq&fprevMask] == 0 {
		return 0, false
	}
	return p.due[seq&fprevMask], true
}

// lateNow (under reMu): has the last transmission of seq had its time to arrive? guard = the v57/v58 guard (ns) when
// -fixguard is off. A packet without a due time (numbered, not sent yet) is late only after 1 s.
func (p *peer) lateNow(seq uint32, now, guard int64) bool {
	if fixGuard {
		if d, ok := p.dueOf(seq); ok {
			return now >= d
		}
		guard = int64(time.Second)
	}
	return now-p.copySent[seq] > guard
}

// dueSpan (under reMu): ns between the last transmission of seq and its due time, 0 = not known.
func (p *peer) dueSpan(seq uint32) int64 {
	if d, ok := p.dueOf(seq); ok {
		return d - p.copySent[seq]
	}
	return 0
}

// ---- P3: drain hygiene ----

// drainOK (under l.mu): may the link start a drain now? since = time since the base was last confirmed. A high srtt
// postpones the drain at most up to 3 × drainEvery: after a cell change the base itself may have risen.
func (l *link) drainOK(since time.Duration) bool {
	if !drainFix {
		return true
	}
	if l.txqB.Load() > 0 || l.stuck {
		return false
	}
	h := l.healthyUs()
	return h == 0 || l.srtt <= h || since > 3*drainEvery
}

// drainBase: the base from a drain's rtt samples – the mean of the lower half. The minimum alone sits 1-2 σ of the
// jitter below the smoothed rtt the base otherwise follows (narrower bands, lower window cap); the plain mean can lift
// it above the path's.
func drainBase(s []float64) float64 {
	if len(s) == 0 {
		return 0
	}
	v := append([]float64(nil), s...)
	for i := 1; i < len(v); i++ { // insertion sort, at most 4 values
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
	k := (len(v) + 1) / 2
	sum := 0.0
	for _, x := range v[:k] {
		sum += x
	}
	return sum / float64(k)
}

// drainTarget (under l.mu): the window of a drain – half the path's BDP empties the queue as well as 6 kB does.
func (l *link) drainTarget() float64 {
	if !drainFix {
		return winMin
	}
	return math.Max(winMin, 0.5*l.rateAvg*l.minRtt)
}

// endDrain (under l.mu; the caller releases drainBusy).
func (l *link) endDrain(now time.Time) {
	l.baseAt, l.draining = now, false
	if drainFix {
		l.winTarget, l.adjUs = l.drainSaved, 0 // the regulator doubles towards it per round trip
	} else {
		l.win = l.drainSaved
	}
}

// ---- P4: bounded wait of the receiver ----

// noteOwd (under l.mu, receiver): one-way delay sample of a data packet on this link.
func (l *link) noteOwd(owd, nowM int64) {
	d := owd - l.owdEwma
	if d < 0 {
		d = -d
	}
	l.owdJit += (d - l.owdJit) / 16
	switch {
	case l.owdMinAt == 0:
		l.owdMinCur, l.owdMinPrv, l.owdMinAt = owd, owd, nowM
	case nowM-l.owdMinAt > owdMinHalf:
		l.owdMinPrv, l.owdMinCur, l.owdMinAt = l.owdMinCur, owd, nowM
	case owd < l.owdMinCur:
		l.owdMinCur = owd
	}
}

// releaseSpanUs: how long after its send time (µs, sender clock plus the clock offset the delays carry) a buffered IP
// packet waits at most; false = no link delivers now.
func (p *peer) releaseSpanUs() (int64, bool) {
	var span int64
	ok := false
	nowM := mono()
	for _, l := range p.links {
		l.mu.Lock()
		if l.haveOwd && l.owdMinAt != 0 && nowM-l.lastRx < int64(time.Second) {
			d := min(l.owdEwma, min(l.owdMinCur, l.owdMinPrv)+relCap.Microseconds()) + 4*l.owdJit
			if !ok || d > span {
				span, ok = d, true
			}
		}
		l.mu.Unlock()
	}
	return span + ipRelease.Microseconds(), ok
}

// noteSent (under rxMu): the send time of a buffered packet.
func (p *peer) noteSent(seq uint32, sentUs int64) {
	if p.rxSent == nil {
		p.rxSent, p.rxSentSeq = make([]int64, maxCopies), make([]uint32, maxCopies)
	}
	p.rxSent[seq&fprevMask], p.rxSentSeq[seq&fprevMask] = sentUs, seq
	if int32(seq-p.rxTop) > 0 {
		p.rxTop = seq
	}
	if int32(seq-p.relFrom) < 0 {
		p.relFrom = seq // a late packet below the cursor (the repair of a hole that is not the in-order point)
	}
}

func (p *peer) releaseWatch() {
	if ipRelease <= 0 || !reliable {
		return
	}
	for range time.Tick(5 * time.Millisecond) {
		p.rxMu.Lock()
		n := len(p.buf)
		p.rxMu.Unlock()
		if n == 0 {
			continue
		}
		span, ok := p.releaseSpanUs() // takes the link locks: not under rxMu
		if !ok {
			continue
		}
		limit := nowUs() - span
		p.rxMu.Lock()
		p.releaseDue(limit)
		p.rxMu.Unlock()
	}
}

// releaseDue (under rxMu): deliver, past the holes in front of them, the buffered IP packets up to the highest seq
// whose deadline passed (send time at or before limit) – all of them in sequence order, also those in between that
// arrived as a resend (a later send time): holding those back would reorder their flow for nothing. The holes stay
// the in-order point (repaired, delivered late). Returns how many went out.
// The scan starts at the cursor relFrom (everything between the in-order point and it is out or a hole; a late packet
// below it pulls it back, noteSent) and ends relLook entries after the first packet that is not due: sequence order is
// send order apart from resends, so a 5 ms tick looks at what arrived since the last one plus relLook entries.
func (p *peer) releaseDue(limit int64) int {
	if len(p.buf) == 0 || p.rxSent == nil {
		return 0
	}
	q := p.next
	if d := int32(p.relFrom - q); d > 0 && int32(p.relFrom-p.rxTop) <= 1 {
		q = p.relFrom
	}
	start := q
	var hi, firstNotDue uint32
	found, notDue, look := false, false, 0
	for i := 0; i < missScan && int32(q-p.rxTop) <= 0; q, i = q+1, i+1 {
		if notDue {
			if look++; look > relLook {
				break
			}
		}
		pl, ok := p.buf[q]
		if !ok || pl == nil || !isIP(pl) {
			continue // a hole, out already (released, noDelay), or a proxy frame (never overtakes)
		}
		sl := q & fprevMask
		if p.rxSentSeq[sl] != q {
			continue
		}
		if p.rxSent[sl] > limit {
			if !notDue {
				firstNotDue, notDue = q, true
			}
			continue
		}
		hi, found = q, true
	}
	// the next scan starts after what is out now, or at the first packet not due, or – when the scan met only holes,
	// proxy frames and packets out already – where it stopped (a late packet below pulls it back, noteSent)
	next := q
	if notDue {
		next = firstNotDue
		if found && int32(hi+1-firstNotDue) > 0 {
			next = hi + 1
		}
	}
	if !found {
		p.relFrom = next
		return 0
	}
	n := 0
	for s := start; int32(s-hi) <= 0; s++ {
		pl, ok := p.buf[s]
		if !ok || pl == nil || !isIP(pl) {
			continue
		}
		p.out(pl) // as releaseEarly, without counting it as a per-flow release
		p.buf[s] = nil
		p.markFlowOK(s)
		p.releaseChain(s)
		p.ipReleased++
		p.trIPRel++
		n++
	}
	p.relFrom = next
	return n
}

const relLook = 64
