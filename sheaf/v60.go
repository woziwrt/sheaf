// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// v60: a persistently slow link (found in the single-box review, 26 Nov) and control sends that never wait.
//
// Slow link. A SIM throttled after its data allowance (a few hundred kbit/s) or a link with a very weak signal made
// the bundle WORSE than the good link alone (lab, 60 s: 25 Mbit/s + 256 kbit/s = 10.7 Mbit/s, the 25 alone 21.9;
// 512 kbit/s -12 %; 1 Mbit/s +3 %; 2 Mbit/s +7 %). The threshold 3 % sits between 512 kbit/s (2 %) and 1 Mbit/s (4 %
// of 25): demoting the 1 Mbit/s link (with 5 %) cost 4 % in the lab.
// pick gives the slow link a packet whenever the fast one's window is full, and the slow link never holds less than
// 4 × winMin (24 kB) – 0.75 s of queue at 256 kbit/s. In reliable
// mode every packet on it holds its TCP flow head-of-line that long. Its base delay re-learns the queue (a slower
// cell), so it looks healthy: not stuck, not dead, no repair is due.
//
// A link whose delivery rate stays below -slowfrac of the best other live link's for -slowafter is demoted: no new
// data, no starvation probes (a probe is a real packet: one more head-of-line wait), no copies of important small
// packets. Only samples of a BUSY link count (at least half its window in flight when the sample is taken): an idle
// or lightly used link says nothing about what it could carry. To come back it gets trials: from -slowafter / 2 after
// the demotion, doubling up to slowTrialMax while it stays slow, for slowTrialLen it carries COPIES of what the other
// links send (as much as its window takes; the receiver drops the duplicates, nobody waits for them, their loss needs
// no repair and they do not move the repair time of the original). A busy sample of at least slowUp × the demotion
// threshold during a trial promotes the link at once. Speed and smooth modes only (backup has its primary,
// redundant its copies); a demoted link carries data (still demoted) while no other live link is left.
var (
	slowFrac  = 0.03             // -slowfrac (0 = off, v59)
	slowAfter = 10 * time.Second // -slowafter
)

const (
	slowUp       = 1.5 // promotion threshold = slowUp × the demotion threshold (hysteresis)
	slowTrialLen = time.Second
	slowTrialMax = 60 * time.Second
	slowIdle     = int64(time.Second) // no busy sample for this long: the demotion clock starts again
)

func v60Flags() {
	flag.Float64Var(&slowFrac, "slowfrac", slowFrac, "v60: a link delivering less than this share of the best other link's rate (busy samples) for -slowafter carries no data, only trial copies (0 = off, v59)")
	flag.DurationVar(&slowAfter, "slowafter", slowAfter, "v60: how long a link must stay below -slowfrac before it is demoted")
}

func v60Summary() string {
	return fmt.Sprintf("slowfrac=%.3f slowafter=%v ctlsend=nowait", slowFrac, slowAfter)
}

// slowState: per link, under l.mu.
type slowState struct {
	slow       bool
	slowCount  int64   // times demoted (status)
	slowSince  int64   // mono ns: first busy sample below the threshold of the current run, 0 = none
	busyRate   float64 // bytes/µs, the latest busy delivery-rate sample
	busyAt     int64   // mono ns of it
	busySeen   int64   // busyAt the last slowTick looked at
	trialAt    int64   // mono ns: next trial
	trialUntil int64   // mono ns: end of the running trial, 0 = none
	trialGap   int64   // ns, the pause before the next trial (doubles)
	trialBest  float64 // bytes/µs, best busy sample of the running trial
	trialN     int     // busy samples in it
	trCopies   int64   // trial copies sent (trace slice)
	copies     int64   // trial copies sent (status)
}

// noteRate (under l.mu, onFeedback): a delivery-rate sample; infl = bytes in flight when the feedback arrived.
func (l *link) noteRate(rate, infl float64, nowM int64) {
	if infl < 0.5*l.win {
		return // app-limited: says nothing about what the link could carry
	}
	l.sl.busyRate, l.sl.busyAt = rate, nowM
	if l.sl.trialUntil != 0 {
		l.sl.trialBest = max(l.sl.trialBest, rate)
		l.sl.trialN++
	}
}

// slowTick (deadTick, every 10 ms): demote and promote links. on = speed/smooth with the feature on.
func (p *peer) slowTick(on bool, nowM int64) {
	if !on {
		for _, l := range p.links {
			l.mu.Lock()
			l.sl.slow, l.sl.trialUntil, l.sl.slowSince = false, 0, 0
			l.mu.Unlock()
		}
		p.trial.Store(nil)
		p.slowOnly.Store(false)
		return
	}
	if t := p.trial.Load(); t != nil { // a session reset (freshRegulator) ended its trial
		t.mu.Lock()
		over := t.sl.trialUntil == 0
		t.mu.Unlock()
		if over {
			p.trial.CompareAndSwap(t, nil)
		}
	}
	// the two best delivery rates of the live links that carry data; how many of those there are
	var top1, top2 float64
	var topL *link
	live := 0
	for _, l := range p.links {
		l.mu.Lock()
		if !l.dead && !l.sl.slow && l.dst() != nil {
			live++
			if r := l.rateMax; r > top1 {
				top1, top2, topL = r, top1, l
			} else if r > top2 {
				top2 = r
			}
		}
		l.mu.Unlock()
	}
	for _, l := range p.links {
		ref := top1
		if l == topL {
			ref = top2
		}
		th := slowFrac * ref
		l.mu.Lock()
		s := &l.sl
		switch {
		case s.slow && s.trialUntil != 0 && s.trialBest >= slowUp*th && s.trialN > 0:
			s.slow, s.slowSince, s.trialUntil = false, 0, 0
			p.trial.CompareAndSwap(l, nil)
			log.Printf("L%d slow: back (trial %.2f Mbit/s, others %.1f)", l.id, s.trialBest*8, ref*8)
		case s.slow && s.trialUntil != 0 && nowM >= s.trialUntil: // trial over, still slow
			p.trial.CompareAndSwap(l, nil)
			s.trialUntil = 0
			if s.trialN == 0 { // nothing to copy (no traffic): try again soon, same pause
				s.trialAt = nowM + int64(time.Second)
			} else {
				s.trialGap = min(2*s.trialGap, int64(slowTrialMax))
				s.trialAt = nowM + s.trialGap
			}
		case s.slow && s.trialUntil == 0 && nowM >= s.trialAt && !l.dead:
			if p.trial.CompareAndSwap(nil, l) { // one trial at a time
				s.trialUntil, s.trialBest, s.trialN = nowM+int64(slowTrialLen), 0, 0
			}
		case !s.slow && s.busyAt != s.busySeen:
			s.busySeen = s.busyAt
			if ref > 0 && s.busyRate < th {
				if s.slowSince == 0 {
					s.slowSince = s.busyAt
				} else if nowM-s.slowSince >= int64(slowAfter) && live > 1 {
					s.slow, s.slowSince = true, 0
					s.slowCount++
					s.trialGap = int64(slowAfter) / 2
					s.trialAt = nowM + s.trialGap
					log.Printf("L%d slow: demoted (%.2f Mbit/s vs %.1f of the best other link for %v)", l.id, s.busyRate*8, ref*8, slowAfter)
				}
			} else {
				s.slowSince = 0
			}
		case !s.slow && s.slowSince != 0 && nowM-s.busyAt > slowIdle:
			s.slowSince = 0
		}
		l.mu.Unlock()
	}
	if (live == 0) != p.slowOnly.Load() {
		p.slowOnly.Store(live == 0)
		log.Printf("slow links carry data: %v (no other live link)", live == 0)
	}
}

// slowOut (under l.mu): the link is demoted and another live link carries the data.
func (p *peer) slowOut(l *link) bool { return l.sl.slow && !p.slowOnly.Load() }

// trialCopy (send): a copy of a packet just sent elsewhere for the link on trial, if its window and queue take it.
func (p *peer) trialCopy(l *link, seq uint32, inner []byte, ep uint32, now int64) {
	n := len(inner) + hdrLen
	l.mu.Lock()
	ok := !l.dead && l.sl.trialUntil != 0 && l.inflight+n <= l.winB && !l.txqFull(n)
	if ok {
		l.sl.trCopies++
		l.sl.copies++
	}
	l.mu.Unlock()
	if ok {
		p.sendOnX(l, seq, inner, ep, now, nil, true)
	}
}

// ---- control sends that never wait ----

var ctlDrops atomic.Uint32 // control datagrams the kernel could not take at once (status ctl_drops)

// sendNow: one datagram on l's socket with MSG_DONTWAIT – the poller is not asked to wait for room.
// net.UDPConn.WriteToUDP waits while the socket buffer is full: feedbackLoop writes every link in turn, so ONE
// link whose buffer does not drain (a stalled modem, a qdisc that holds everything) stopped the feedback, the
// NACKs and the still-missing reports of all links, and with them the whole tunnel.
func sendNow(c *net.UDPConn, b []byte, d *net.UDPAddr) error {
	ip4 := d.IP.To4()
	if ip4 == nil {
		return unix.EAFNOSUPPORT
	}
	sa := &unix.SockaddrInet4{Port: d.Port}
	copy(sa.Addr[:], ip4)
	return sendRaw(c, b, sa)
}

// sendRaw: one non-blocking sendto on any socket (sa nil = a connected socket).
func sendRaw(c syscall.Conn, b []byte, sa unix.Sockaddr) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Write(func(fd uintptr) bool {
		serr = unix.Sendto(int(fd), b, unix.MSG_DONTWAIT, sa)
		return true // done either way: never wait for writability
	}); err != nil {
		return err
	}
	return serr
}
