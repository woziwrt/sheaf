// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

// sheaf – bonding of several links with different, varying delay.
//
// Every link has a window = bytes allowed in flight. The window grows while the
// link's smoothed RTT stays close to its base RTT, shrinks only when it is well
// above it, and is left alone in between – LTE jitter does not make it oscillate.
// A packet goes to the link where it is expected to arrive first among those
// with room in their window; the far end puts packets back in order and repairs
// losses (per-link NACK, dead-link reinjection). See docs/HOW-IT-WORKS.md.
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

const (
	magic     = 0x5A
	tData     = 1
	tFeedback = 2
	hdrLen    = 24 // magic, type, link, 0, seq u32, linkSeq u32, sendTime i64 (µs), prevSeq u32
	tNack     = 3
	tNackR    = 4
	fbEvery   = 20 * time.Millisecond

	incFast   = 1.25
	decFactor = 0.7
	winMin    = 6000.0
	winInit   = 64000.0
	winMax    = 4e6
	starveFor = 500 * time.Millisecond // a link idle this long gets a packet (keeps it measured)
)

var drainEvery = 10 * time.Second // -drainevery (a lab knob: frequent drains)

// window regulation: grow while srtt <= base*(1+incLimit), shrink above base*(1+decLimit).
// base = sliding minimum of the SMOOTHED rtt, so jitter is not mistaken for a queue.
var incLimit, decLimit = 0.5, 1.5

// loss reaction: above lossCut of a window's packets NACKed the link's window shrinks by lossFactor (once per real
// RTT, never below what it delivers per base RTT); between lossHold and lossCut it only stops growing.
var lossCut, lossHold, lossFactor = 0.03, 0.01, 0.8 // real LTE, daytime Vodafone: 3 % / 1 % with the learned queue cap

// maxQueueUs: queue a link may hold on top of its base delay (window cap = rateMax × (minRtt + maxQueue)). 100 ms
// suits deep, quiet cells; a busy cell (daytime) has less room and drops what overflows.
var maxQueueUs = 100000.0

// maxQAdapt: learn the queue cap per link from loss (shrink ×0.8 per loss cut, min 20 ms; regrow +5 ms/s)
var maxQAdapt = true

// jitter-aware bands: the grow / cut excess over the base is at least jitK / 2·jitK times the link's rtt jitter
// (capped at jitCap/2 / jitCap). Pure ratios of the base mis-fire on a jittery link: the base (a minimum) sits at the
// bottom of the noise, the noise alone keeps srtt above the grow limit, and the window stays far below the path's BDP
// (lab, 17 ms ± 12 ms one way: a link held a 26 kB window for 20 s). 0 = pure ratios.
var jitK = 3.0

const jitCap = 100000.0 // µs

// reliable: the receiver never skips a hole; the sender keeps every packet until the peer's cumulative ack
// passed it, and resends it after a timeout if nothing else repaired it (for TCP through the tunnel / proxy).
var reliable bool

var startTime = time.Now()

// wait counters (stats line, reset every -statsevery): where does the sending path sleep?
var wWin, wPace, wCopies, wCredit, rdN, rdBytes, txPkts, txMsgs atomic.Uint32

var dupSent atomic.Uint32 // small important packets sent over a second link too (dupWorthy)

const batch = 32 // datagrams per recvmmsg / sendmmsg (each socket call costs 64-bit atomics, emulated with a lock on MIPS32)

const maxCopies = 65536 // reliable mode: unacknowledged packets kept; more = stop reading the TUN (backpressure)

// mono: nanoseconds since this process started (monotonic). On linux/mips Go has no vDSO clock: time.Now() is two
// clock_gettime syscalls (~2.2 µs on the board), time.Since one (~1.1 µs). The hot paths read the clock once per
// packet through this and keep per-packet times as int64.
func mono() int64 { return int64(time.Since(startTime)) }

// nowUs: the wire's time unit (µs, process-relative: both ends only ever compare a peer's stamps with each other
// or their own stamps with their own clock, so the offset between the two processes cancels).
func nowUs() int64 { return mono() / 1000 }

type sentPkt struct {
	lseq uint32
	size int
	at   int64
	seq  uint32
	data []byte // copy of the inner packet, for reinjection if the link dies
	ep   uint32 // session epoch the seq belongs to (a packet of an older epoch is never sent)
	// sent again over another link because this one got stuck (slowlink.go): at most once per transmission
	rescued bool
}

// lmapEnt: a recent transmission on a link (linkSeq -> it): global seq and send time (µs, as sentPkt.at).
type lmapEnt struct {
	seq uint32
	at  int64
}

type txItem struct {
	b  []byte
	ep uint32
}

type link struct {
	id    int
	conn  *net.UDPConn
	peerA atomic.Pointer[net.UDPAddr]
	learn bool // the peer address is learned ("-" in -links), not configured: a new session forgets it
	// the far end's id of this link (its socket at the other end of our socket pair), learned from its data and
	// feedback; 0 = not known yet. Links are paired by this id, not by list position: the two ends may list a
	// different number of links or in another order (v55; until v54 feedback with another link count was dropped)
	peerLid atomic.Int32

	mu sync.Mutex
	// sender side
	lseq     uint32
	hist     []sentPkt // sent, not yet covered by the peer's highest received linkSeq
	inflight int       // bytes
	win      float64
	rtt      float64 // µs, latest sample
	minRtt   float64
	minRttAt time.Time
	minCur   float64
	minPrev  float64
	relOwd   float64 // µs, one-way delay relative to the fastest link (from the peer)
	// integer copies of the regulator's state for pick (softfloat costs ~1 µs per operation on the board); derive() sets them
	winB        int                // int(win)
	relOwdUs    int64              // int64(relOwd)
	usPerB24    int64              // max(minRtt,1000)/win µs per byte, <<24 fixed point (expected drain time of the queue)
	deadAfterNs int64              // silence with data in flight after which the link counts as dead
	adjUs       int64              // µs (send-time clock): last window change; only samples of packets sent after it judge the new window
	progress    int64              // mono ns: last feedback that acknowledged something new on this link
	fbRx        time.Time          // last feedback packet that ARRIVED over this link (its reverse direction lives)
	lastSeq     uint32             // global seq of the last packet sent on this link
	lmap        map[uint32]lmapEnt // linkSeq -> global seq of recent packets on this link (precise loss repair)
	lmapQ       []uint32
	srtt        float64 // µs, smoothed current RTT
	jit         float64 // µs, EWMA of half the 2nd difference of the rtt samples (≈ σ for white jitter)
	prevRtt     float64
	prevRtt2    float64
	slowSince   time.Time // srtt above the grow limit while the window is already minimal
	highSince   time.Time // srtt above the grow limit (any window): a new, slower cell? -> re-measure the base early
	deadAt      time.Time // when the link was declared dead
	deadWin     float64   // its window then (warm restart after a short outage)
	deadCount   int64     // how many times the link was declared dead (status page)
	lossCuts    int64     // window cuts caused by loss (status page)
	baseAt      time.Time // when the base rtt was last confirmed
	draining    bool      // drain phase: window minimal so the queue empties and the true base shows
	drainSaved  float64
	drainStart  time.Time
	drainLowAt  time.Time // queue emptied at this moment; samples of packets sent after it measure the base
	drainSum    float64
	drainN      int
	ackBytes    int // delivery-rate measurement: bytes acknowledged since ackAt
	ackAt       time.Time
	rate        float64 // bytes/µs, latest delivery-rate sample
	rate2       float64 // bytes/µs, the sample before it (stuckCheck: two samples must agree)
	rateAvg     float64 // bytes/µs, EWMA of the samples (~0.4 s): floor of a cut (the bursts that inflate rateMax average out)
	rateMax     float64 // max of the samples over ~6 s (two halves)
	rateCur     float64
	ratePrev    float64
	rateAt      time.Time
	dead        bool
	stuck       bool  // its queue drains far slower than its healthy round trip allows (slowlink.go): no new data
	stuckCount  int64 // times it got stuck (status page)
	lastSend    int64 // mono ns
	idleCut     int64 // mono ns: last idle halving of the window
	sent        int64
	// receiver side
	hiLseq  uint32
	hiAt    int64 // when hiLseq arrived (µs)
	haveHi  bool
	owdEwma int64 // µs
	haveOwd bool
	rxBytes int64
	txBytes int64 // data bytes sent on this link (first sends and repairs), status page
	// loss on this link as the peer reports it (NACKed / sent), per window of max(srtt, 100 ms): a link that drops
	// without its delay rising (a policer) is invisible to the delay regulator
	lossSent, lossNacked int64
	lossAt               int64   // mono ns: start of the current window
	lossFrac             float64 // last window's loss fraction
	maxQ                 float64 // µs, learned queue cap (0 = not set yet)
	maxQAt               int64   // mono ns of the last maxQ change
	rxCounted            int64
	lastRx               int64  // mono ns, receiver: last data packet on this link
	rxLseq               uint32 // receiver: last linkSeq seen on this link (a link is a FIFO)
	// transmit queue: data packets in link order, signed and sent by this link's own goroutine (txLoop),
	// so the expensive part (auth tag + syscall) runs in parallel with picking links and reading sockets
	txMu   sync.Mutex
	txCond *sync.Cond
	txQ    []txItem
	// trace counters (reset every trace interval)
	trTx, trTxDead, trRx int64
	trCuts, trGrows      int64
	trRescued            int64
	// v59 (v59.go)
	txqB      atomic.Int32  // bytes queued for txLoop and not yet handed to the kernel (P0 trace, P1 cap)
	txSentTr  atomic.Uint32 // packets txLoop really sent, this trace slice (Lx_tx counts what was queued)
	txSentN   atomic.Uint32 // the same, since start (status)
	paceBps   int64         // pacing in pick: bytes per second, 0 = unpaced (derive)
	paceTok   int64         // bytes that may leave now
	paceAt    int64         // mono ns of the last refill
	winTarget float64       // window to return to, doubling per round trip (drain end, short outage), 0 = none
	drainS    [4]float64    // the drain's rtt samples (drainN of them; the drain ends at 4)
	drainWin  float64       // the drain's window
	owdJit    int64         // receiver: µs, mean deviation of the one-way delay
	owdMinCur int64         // receiver: sliding minimum of the one-way delay (two halves of owdMinHalf)
	owdMinPrv int64
	owdMinAt  int64     // mono ns: start of the current half
	sl        slowState // v60: persistently slow link (v60.go)
	// v62 integer copies (derive): the per-packet send path does no softfloat work on the MIPS box
	repairUs   int64 // repairSpanUs()
	redStallNs int64 // redStall(srtt)
	// v62 (v62.go)
	deadAtM  int64       // deadAt in mono ns: quiet probing after quietAfter
	quietLog bool        // "quiet" logged for this outage
	quiet    atomic.Bool // long dead while another link lives: no control datagrams (wr)
}

type peer struct {
	fq    *flowQueue // between TUN and the sender (nil: -fqlimit 0)
	links []*link
	tun   *os.File

	seqMu            sync.Mutex
	seq              uint32
	epoch            atomic.Uint32 // +1 on every session reset (under reMu); stale packets are dropped, not sent
	room             *sync.Cond
	reinject         []sentPkt // packets waiting to go out again (dead link, or NACKed)
	reMu             sync.Mutex
	copies           map[uint32][]byte // recent inner packets by seq, for NACK repair
	copyQ            []uint32
	copySent         map[uint32]int64 // mono ns, reliable: last (re)send of each kept packet
	cumAck           uint32           // reliable: the peer has everything below this
	rtx              int64            // reliable: timeout resends
	lastAck          uint32
	ackMovedAt       time.Time
	rtoBackoff       time.Duration
	nacked, repaired int64
	missRtx          int64 // reliable: resends for still-missing reports (part of repaired)
	rescued          int64 // reliable: packets sent again because their link got stuck (slowlink.go)
	rescuedB         int64 // their bytes
	lateDeliv        int64
	skippedSet       map[uint32]bool

	rxMu      sync.Mutex
	buf       map[uint32][]byte
	next      uint32
	started   bool
	gapSince  time.Time
	delivered int64
	noDelayed int64               // acknowledgements delivered ahead of a hole (noDelay)
	live      []liveItem          // smooth mode: UDP packets parked behind a hole
	liveEarly int64               // smooth mode: UDP packets released ahead of a hole
	waitOn    map[uint32]uint32   // per-flow release: flow predecessor -> the packet waiting for it (flowrel.go)
	flowOK    map[uint32]struct{} // released early with their whole flow chain through (not noDelay placeholders)
	flowEarly int64               // packets delivered ahead of a hole of another flow
	// sender side of per-flow release (under reMu)
	fprev           []uint64 // by seq & fprevMask: 1<<32 | flow predecessor, 0 = none (not IP)
	fseq            []uint32 // by seq & fprevMask: the seq the fprev slot belongs to
	flowLast        [fqBuckets]uint32
	flowHas         [fqBuckets]bool
	dups            int64 // packets that arrived again (a resend after a NACK, when the original was only late)
	skipped         int64
	drainBusy       atomic.Bool   // one link drains at a time
	lastRxData      atomic.Uint32 // ms since start of the last data packet received (32 bit: cheap on 32-bit MIPS)
	kick            chan struct{} // wakes the repair drainer
	stallTicks      int64         // 2 ms ticks with a hole waiting (trace)
	missRep         int64         // reliable: seqs named again in still-missing reports
	trDeliv, trSkip int64
	waitQ           []waitMark // reorder buffer: arrival marks, the first = the oldest waiting packet (slowlink.go)
	// v59 (v59.go)
	due        []int64       // by seq & fprevMask: mono ns after which its last transmission counts as late
	dueSeq     []uint32      // the seq each due slot belongs to
	txRoom     chan struct{} // a txLoop handed packets to the kernel: room in its queue again
	rxSent     []int64       // receiver, by seq & fprevMask: the sender's send time (µs) of a buffered packet
	rxSentSeq  []uint32      // the seq each rxSent slot belongs to
	relFrom    uint32        // receiver: where the next deadline scan starts (everything below is out or not due)
	rxTop      uint32        // receiver: highest seq with a send time noted
	ipReleased int64         // receiver: IP packets released past a hole by their deadline
	trIPRel    int64
	trial      atomic.Pointer[link] // v60: the slow link on trial (it gets copies), nil = none
	slowOnly   atomic.Bool          // v60: no live link but demoted slow ones: they carry the data
	gro        groState             // v61 -tungro: TCP segments coalesced into one TUN write (under rxMu, tungro.go)
	fbSoon     atomic.Bool          // v62: a quiet probe arrived: feedback at the next tick, even when idle
}

func openTun(name string) (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return nil, err
	}
	fl := uint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if tunGRO {
		fl |= unix.IFF_VNET_HDR // v61 -tungro: a virtio-net header on every read and write (tungro.go)
	}
	ifr.SetUint16(fl)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// derive refreshes the integer copies pick and sendOn use; call under l.mu after win, minRtt, relOwd, srtt, rateMax,
// maxQ or jit changed (deriveKey lists them).
func (l *link) derive() {
	l.winB = int(l.win)
	l.relOwdUs = int64(l.relOwd)
	l.usPerB24 = int64(fmax(l.minRtt, 1000) * (1 << 24) / l.win)
	l.paceBps = l.paceRate()
	l.repairUs = int64(l.repairSpanUs()) // v62: sendOn's per-packet repair time, without softfloat there
	l.redStallNs = redStall(l.srtt)      // v62: sendRedundant's, per link and packet
	// µs: a dead link strands its in-flight data until we notice, so notice fast – but not faster than the
	// round trip the regulator itself allows: with a full (permitted) queue the srtt is base + up to ~100 ms
	// plus jitter, and acks come in bursts. A limit from the base delay alone declared busy links dead
	// (all at once, on static lab links) and the resend cascade then filled the queues further.
	l.deadAfterNs = int64(fmin(1000000, fmax(250000, fmax(1.5*l.minRtt+100000, 1.5*l.srtt+50000)))) * 1000
}

// freshRegulator: the window regulator as at process start (under l.mu). A new session of the peer may sit behind
// another path (box rebooted, modem reconnected); and the old window – up to 2 MB – went out as ONE burst at line
// rate the moment the new session's first request arrived (seen: ~5000 packets into the lab VM's NAT, >50 % lost).
// Returns whether the link was draining (the caller releases drainBusy).
func (l *link) freshRegulator() (wasDraining bool) {
	wasDraining = l.draining
	l.win, l.rtt, l.srtt, l.minRtt, l.relOwd, l.jit, l.prevRtt, l.prevRtt2 = winInit, 0, 0, 0, 0, 0, 0, 0
	l.minRttAt, l.minCur, l.minPrev = time.Time{}, 0, 0
	l.adjUs, l.slowSince, l.highSince, l.baseAt = 0, time.Time{}, time.Time{}, time.Time{}
	l.draining, l.drainSaved, l.drainStart, l.drainLowAt, l.drainSum, l.drainN = false, 0, time.Time{}, time.Time{}, 0, 0
	l.ackBytes, l.ackAt, l.rate, l.rateAvg, l.rateMax, l.rateCur, l.ratePrev, l.rateAt = 0, time.Time{}, 0, 0, 0, 0, 0, time.Time{}
	l.rate2 = 0
	l.lossSent, l.lossNacked, l.lossAt, l.lossFrac, l.maxQ, l.maxQAt = 0, 0, 0, 0, 0, 0
	l.stuck = false
	l.sl = slowState{slowCount: l.sl.slowCount, copies: l.sl.copies} // v60: judged anew (the path may have changed)
	l.derive()
	return wasDraining
}

// idleAfter: a link with nothing in flight and nothing sent for this long halves its window (down to winInit) once
// per idleAfter – what it knew about the path is stale, and a full old window would leave as one line-rate burst
// (the idea of RFC 2861). Longer than starveFor: a link that only gets the 500 ms probes is not idle.
const idleAfter = time.Second

// pick: the link where n bytes arrive first, among links with window room.
// Expected arrival = relative one-way delay + time to drain what is in
// flight at the link's current pace (window per RTT). Nil: all windows full.
// now = mono(). Integer arithmetic only on the normal path (pick runs for every packet).
func (p *peer) pick(n int, probes bool, now int64) *link {
	l, _, _ := p.pickW(n, probes, now)
	return l
}

// pickW: pick, and when it finds no link: how long (ns) until a link with window room may take n bytes by its pace
// or transmit queue (v59 P1; 0 = only full windows, wait for feedback), and whether a full transmit queue is the cause.
func (p *peer) pickW(n int, probes bool, now int64) (*link, int64, bool) {
	var best *link
	bestT := int64(math.MaxInt64)
	var wait int64
	var txq bool
	var primary *link // backup mode: only this one carries data while it lives
	v58on := v58()    // a float comparison: once, not per link
	if curMode() == modeBackup {
		if i := primaryIdx(); i < len(p.links) && p.links[i].dst() != nil {
			c := p.links[i]
			c.mu.Lock()
			if !c.dead {
				primary = c
			}
			c.mu.Unlock()
		}
	}
	for _, l := range p.links {
		if l.dst() == nil {
			continue // peer address not learned yet
		}
		l.mu.Lock()
		silentAck := len(l.hist) > 0 && now-l.progress > l.deadAfterNs
		if l.draining && time.Since(l.drainStart) > 2*time.Second { // no samples came (link died?): end the drain anyway
			l.endDrain(time.Now())
			l.derive()
			p.drainBusy.Store(false)
		}
		if !l.dead && silentAck {
			l.dead, l.deadAt, l.deadWin = true, time.Now(), l.win // lost signal: resend what was in flight over the live links
			l.deadAtM, l.quietLog = now, false
			l.deadCount++
			oldest := (now/1000 - l.hist[0].at) / 1000
			p.reMu.Lock()
			p.reinject = appendUnrescued(p.reinject, l.hist)
			p.kickRepairs()
			nre := len(p.reinject)
			p.reMu.Unlock()
			l.hist, l.inflight = nil, 0
			log.Printf("L%d dead [pick] (%d packets to resend; no new ack %d ms, last feedback %d ms ago, oldest unacked %d ms, limit %d ms, mono-now lag %d ms)",
				l.id, nre, (now-l.progress)/1e6, time.Since(l.fbRx).Milliseconds(), oldest, l.deadAfterNs/1e6, (mono()-now)/1e6)
		}
		standby := primary != nil && l != primary
		// starving or dead: give it this packet as a probe. Not a live link with data still in flight: feedback
		// measures it anyway, and a probe behind a stuck queue was one more hole (v58)
		// (v60: nor a demoted slow link – its probe would be one more head-of-line wait; its trials measure it)
		// (v62: nor a link dead for quietAfter – it gets only deadTick's empty quiet probes)
		if probes && now-l.lastSend > int64(starveFor) &&
			(l.dead && !l.longDead(now) || !l.dead && !standby && !p.slowOut(l) && (!v58on || len(l.hist) == 0)) {
			l.mu.Unlock()
			return l, 0, false
		}
		if l.dead || standby || l.stuck || p.slowOut(l) {
			l.mu.Unlock()
			continue
		}
		if l.inflight+n <= l.winB {
			// v59 P1: nothing that a pacer could slow down queues behind this decision – a link takes the packet only
			// when its pace allows it now and its transmit queue is short. With -pacelook a link whose tokens come
			// within it stays a candidate with that wait added to its arrival and takes the packet on credit when it
			// wins (no spill of a burst onto a slower link). (Waiting for it instead – a sub-ms sleep of the one sending
			// goroutine – cost a quarter of the throughput in the lab.)
			w, q := l.sendWait(n, now)
			if q || w > paceLook {
				if wait == 0 || w < wait {
					wait = w
				}
				txq = txq || q
				l.mu.Unlock()
				continue
			}
			t := l.relOwdUs + int64(l.inflight+n)*l.usPerB24>>24 + w/1000
			if t < bestT {
				bestT, best = t, l
			}
		}
		l.mu.Unlock()
	}
	if best != nil {
		wait, txq = 0, false
	}
	return best, wait, txq
}

// newPkt: a buffer for an inner packet of n bytes at [hdrLen:], with room for the data header in front and the wire
// trailer behind – its first transmission goes out from this very buffer (no per-packet copy).
func newPkt(n int) []byte { return make([]byte, hdrLen+n, hdrLen+n+trailerLen) }

// send: full = the newPkt buffer holding inner for its first transmission, nil for resends.
func (p *peer) send(seq uint32, inner []byte, ep uint32, now int64, full []byte) {
	n := len(inner)
	if curMode() == modeRedundant && p.sendRedundant(seq, inner, ep, now, full) {
		return
	}
	var l *link
	var wait int64
	var txq bool
	for l, wait, txq = p.pickW(n+hdrLen, true, now); l == nil; l, wait, txq = p.pickW(n+hdrLen, true, now) {
		if wait > 0 { // v59 P1: a link with window room waits for its pace or its transmit queue
			wPace.Add(1)
			p.waitTx(wait, txq)
		} else {
			wWin.Add(1)
			time.Sleep(500 * time.Microsecond) // all windows full: real saturation, wait for feedback
		}
		now = mono()
		if ep != p.epoch.Load() {
			return // its session was reset meanwhile: sendOn would drop it anyway
		}
	}
	l.mu.Lock()
	probeDead := l.dead
	l.mu.Unlock()
	p.sendOn(l, seq, inner, ep, now, full)
	if !probeDead && dupWorthy(inner) { // small and important: a copy over another live link, the first one counts
		for _, o := range p.links {
			if o == l {
				continue
			}
			o.mu.Lock()
			alive := !o.dead && !o.sl.slow // v60: a copy on a slow link would set the repair time from that link
			o.mu.Unlock()
			if alive {
				p.sendOn(o, seq, inner, ep, now, nil)
				dupSent.Add(1)
				break
			}
		}
	}
	if probeDead { // a dead link only gets a COPY: the original goes over a live link, nobody waits for the probe
		lv := p.pick(n+hdrLen, false, now)
		if lv == nil { // no live link with room: park the original in the repair queue instead of waiting here
			// (if every sending goroutine waited for a live link, nothing would revive the dead ones)
			p.reMu.Lock()
			p.reinject = append(p.reinject, sentPkt{seq: seq, data: inner, ep: ep})
			p.kickRepairs()
			p.reMu.Unlock()
			return
		}
		p.sendOn(lv, seq, inner, ep, now, nil) // full is taken: a buffer goes out once
		l = lv
	}
	if t := p.trial.Load(); t != nil && t != l { // v60: the slow link on trial gets a copy
		p.trialCopy(t, seq, inner, ep, now)
	}
}

// sendOn queues one data packet on link l; now = mono() (the packet's send time). full: see send.
func (p *peer) sendOn(l *link, seq uint32, inner []byte, ep uint32, now int64, full []byte) {
	p.sendOnX(l, seq, inner, ep, now, full, false)
}

// sendOnX: sendOn; cp = a v60 trial copy (the original is on its way over another link): it does not move the
// packet's repair time, and its loss or its link's death needs no repair (marked rescued).
func (p *peer) sendOnX(l *link, seq uint32, inner []byte, ep uint32, now int64, full []byte, cp bool) {
	if ep != p.epoch.Load() {
		return // its session was reset while it waited (repair queue, full windows): the seq means nothing now
	}
	n := len(inner)
	pkt := full // first transmission: header and trailer are written around inner in place
	if pkt == nil {
		pkt = newPkt(n) // a resend: the earlier buffer may still wait in a link's queue
		copy(pkt[hdrLen:], inner)
	}
	nowU := now / 1000
	l.mu.Lock()
	if ep != p.epoch.Load() {
		l.mu.Unlock()
		return
	}
	l.lseq++
	ls := l.lseq
	prev := l.lastSeq
	l.lastSeq = seq
	if !cp { // a lost trial copy needs no repair: no NACK mapping
		l.lmap[ls] = lmapEnt{seq, nowU}
		l.lmapQ = append(l.lmapQ, ls)
		if len(l.lmapQ) > 32768 {
			delete(l.lmap, l.lmapQ[0])
			l.lmapQ = l.lmapQ[1:]
		}
	}
	l.hist = append(l.hist, sentPkt{ls, n + hdrLen, nowU, seq, pkt[hdrLen:], ep, cp})
	l.inflight += n + hdrLen
	if len(l.hist) == 1 {
		l.progress = now // nothing was pending: the timeout starts now
	}
	l.lastSend = now
	l.sent++
	l.txBytes += int64(n + hdrLen)
	l.spendPace(n+hdrLen, now)
	if reliable && !cp { // lock order l.mu -> reMu; before predecessor(), which reads what notePredecessor wrote under reMu
		var due int64
		if fixGuard {
			due = now + l.repairUs*1000 // v59 P2: from this link as it is now (repairSpanUs, cached by derive)
		}
		p.reMu.Lock()
		if _, ok := p.copySent[seq]; ok {
			p.copySent[seq] = now // the timeout clock starts when the packet really leaves
			p.setDue(seq, due)
		}
		p.reMu.Unlock()
	}
	l.lossSent++
	l.trTx++
	if l.dead {
		l.trTxDead++
	}
	pkt[0], pkt[1], pkt[2], pkt[3] = magic, tData, byte(l.id), 0
	if fp, ok := p.predecessor(seq); ok {
		pkt[3], prev = hasFlowPrev, fp
	}
	binary.BigEndian.PutUint32(pkt[4:], seq)
	binary.BigEndian.PutUint32(pkt[8:], ls)
	binary.BigEndian.PutUint64(pkt[12:], uint64(nowU))
	binary.BigEndian.PutUint32(pkt[20:], prev)
	l.txMu.Lock() // still under l.mu: the queue keeps linkSeq order (the receiver treats a link as a FIFO)
	l.txQ = append(l.txQ, txItem{pkt, ep})
	l.txqB.Add(int32(len(pkt)))
	l.txCond.Signal()
	l.txMu.Unlock()
	l.mu.Unlock()
}

// UDP GSO (UDP_SEGMENT): one sendmmsg message carries up to gsoMax equal-sized datagrams; the kernel cuts them
// apart only at the device. On the MIPS board the per-datagram cost of the UDP/IP send path (route, netfilter,
// qdisc) was the largest single item of the upload profile. Each datagram keeps its own header and tag.
const gsoMax = 32

var noGSO = os.Getenv("SHEAF_NOGSO") != "" // switch it off (comparison / old kernels)

// paceGain > 0: spread each link's sends over time at paceGain × window / srtt instead of sending what the window
// allows at line rate. A window freed by one feedback (20 ms of data) used to leave the server as one burst at
// 1 Gbit/s; an LTE downlink dropped up to ~20 % of such bursts at ~40 Mbit/s although it carries 50-60 Mbit/s of
// evenly spaced UDP. Bursts are limited to ~2 ms of data at the pace (at least 6 packets).
var paceGain = 0.0

// gsoCmsg: control message "segment size n" for sendmsg.
func gsoCmsg(n int) []byte {
	b := make([]byte, unix.CmsgSpace(2))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level, h.Type = unix.SOL_UDP, unix.UDP_SEGMENT
	h.SetLen(unix.CmsgLen(2))
	*(*uint16)(unsafe.Pointer(&b[unix.CmsgLen(0)])) = uint16(n)
	return b
}

// txLoop signs and sends this link's queued data packets: up to `batch` messages per sendmmsg, and with GSO up to
// gsoMax datagrams of the same size per message.
// The epoch check and the trailer (with the echo of the peer's session) happen together under sessRW.RLock:
// a packet is either old-epoch (dropped here) or carries the echo of the session it was numbered in
// (a restarted peer drops it). The send itself runs without the lock (a full socket must not stall receiving).
func (p *peer) txLoop(l *link) {
	pc := ipv4.NewPacketConn(l.conn)
	msgs := make([]ipv4.Message, batch)
	for i := range msgs {
		msgs[i].Buffers = make([][]byte, 0, gsoMax)
	}
	one := make([]ipv4.Message, 1) // fallback: one datagram per message
	one[0].Buffers = make([][]byte, 1)
	gso := !noGSO
	cms := map[int][]byte{} // segment size -> control message
	var q, spare []txItem
	var tokens float64 // pacing: bytes that may leave now
	tokAt := nowUs()
	for {
		l.txMu.Lock()
		for len(l.txQ) == 0 {
			l.txCond.Wait()
		}
		q, l.txQ, spare = l.txQ, spare[:0], nil // double buffer: the producer appends to the other slice
		l.txMu.Unlock()
		dst := l.dst()
		for i := 0; i < len(q); {
			limit, rate := math.MaxInt, 0.0 // pacing: bytes this chunk may carry, rate in bytes/µs
			if paceGain > 0 && !pacePick {  // v57: the pacer sits here, behind pick (v59 paces in pick)
				l.mu.Lock()
				if l.srtt > 0 {
					rate = math.Max(paceGain*l.win/l.srtt, 0.125) // at least 1 Mbit/s
				}
				l.mu.Unlock()
			}
			if rate > 0 {
				depth := math.Max(rate*2000, 6*1400)
				now := nowUs()
				tokens = math.Min(depth, tokens+rate*float64(now-tokAt))
				tokAt = now
				if tokens < 1500 {
					time.Sleep(time.Duration((1500-tokens)/rate) * time.Microsecond)
					now = nowUs()
					tokens = math.Min(depth, tokens+rate*float64(now-tokAt))
					tokAt = now
				}
				limit = int(tokens)
			}
			bytes, taken := 0, 0
			sessRW.RLock()
			ep := p.epoch.Load()
			k, npk := 0, 0
			for i < len(q) && k < batch && bytes < limit {
				m := &msgs[k]
				m.Buffers, m.OOB, m.Addr = m.Buffers[:0], nil, dst
				size := 0
				for i < len(q) && len(m.Buffers) < gsoMax && bytes < limit {
					if q[i].ep != ep || dst == nil {
						taken += len(q[i].b)
						q[i] = txItem{}
						i++
						continue
					}
					if len(m.Buffers) > 0 && (!gso || len(q[i].b)+trailerLen > size) {
						break // a segment may not be longer than the first (only the last may be shorter)
					}
					taken += len(q[i].b)
					d := seal(q[i].b)
					q[i] = txItem{}
					i++
					bytes += len(d)
					m.Buffers = append(m.Buffers, d)
					if len(m.Buffers) == 1 {
						size = len(d)
					} else if len(d) < size {
						break // shorter: must be the last segment
					}
				}
				if len(m.Buffers) == 0 {
					continue
				}
				if len(m.Buffers) > 1 {
					c, ok := cms[size]
					if !ok {
						c = gsoCmsg(size)
						cms[size] = c
					}
					m.OOB = c
				}
				npk += len(m.Buffers)
				k++
			}
			sessRW.RUnlock()
			if rate > 0 {
				tokens -= float64(bytes)
			}
			txPkts.Add(uint32(npk))
			txMsgs.Add(uint32(k))
			for sent := 0; sent < k; {
				n, err := pc.WriteBatch(msgs[sent:k], 0)
				if err != nil {
					// the kernel or the route refuses GSO (old kernel: EINVAL/ENOPROTOOPT, xfrm/IPsec or device: EIO):
					// send these one by one, and from now on always. Other errors (link down...) leave GSO on.
					if gso && msgs[sent].OOB != nil && (errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EINVAL) ||
						errors.Is(err, syscall.ENOPROTOOPT) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EMSGSIZE)) {
						gso = false
						log.Printf("L%d: UDP GSO not available (%v), sending datagrams one by one", l.id, err)
						for j := sent; j < k; j++ {
							for _, d := range msgs[j].Buffers {
								one[0].Buffers[0], one[0].Addr = d, dst
								pc.WriteBatch(one, 0)
							}
						}
					}
					break // socket error: the packets count as lost (NACK / dead-link logic repairs them)
				}
				sent += n
			}
			for j := 0; j < k; j++ {
				clear(msgs[j].Buffers) // do not keep sent packets alive
			}
			l.txqB.Add(-int32(taken))
			if txqCapB > 0 {
				p.signalTxRoom()
			}
			l.txSentTr.Add(uint32(npk))
			l.txSentN.Add(uint32(npk))
		}
		spare = q
	}
}

func (p *peer) sendLoop() {
	buf := make([]byte, 2000)
	if fqLimit > 0 {
		p.fq = newFlowQueue()
		go p.tunReader(p.fq)
	}
	for {
		var c []byte
		if p.fq != nil {
			c = p.fq.pop()
		} else {
			pk, err := p.tunRead(buf)
			if err != nil {
				log.Fatalf("tun read: %v", err)
			}
			c = newPkt(len(pk))
			copy(c[hdrLen:], pk)
		}
		for {
			p.reMu.Lock()
			if len(p.reinject) == 0 {
				p.reMu.Unlock()
				break
			}
			r := p.reinject[0]
			p.reinject = p.reinject[1:]
			p.reMu.Unlock()
			p.send(r.seq, r.data, r.ep, mono(), nil) // same seq: the far end drops duplicates
		}
		p.submit(c)
	}
}

// submit gives one inner packet (a TUN packet or a control frame) its sequence number, keeps a copy for repair and sends it.
// full = newPkt buffer with the inner packet at [hdrLen:].
func (p *peer) submit(full []byte) { p.submitEp(full, p.epoch.Load()) }

// submitEp: like submit, for a packet that belongs to session epoch ep (a proxy stream's frames carry the epoch
// the stream was opened in: after a session reset its leftovers must not reach a new stream with the same id).
func (p *peer) submitEp(full []byte, ep uint32) {
	c := full[hdrLen:] // the inner packet (kept for repairs; its bytes never change)
	// Wait for room BEFORE taking a number (a number taken and not sent would block the cumulative ack forever).
	// The room is checked again in the same reMu section that takes the number: several submitters (TUN, proxy
	// streams) used to pass the check together and overshoot maxCopies, so seq X+maxCopies overwrote the flow
	// predecessor slot of the still unacknowledged X (review finding, TestRegCopiesNoOvershoot).
	for {
		for reliable {
			p.reMu.Lock()
			full := len(p.copies) >= maxCopies
			p.reMu.Unlock()
			if !full {
				break
			}
			wCopies.Add(1)
			time.Sleep(time.Millisecond)
		}
		p.drainRepairs() // repairs before new data
		p.reMu.Lock()
		if !reliable || len(p.copies) < maxCopies {
			break
		}
		p.reMu.Unlock() // another submitter took the room meanwhile
	}
	{
		if ep != p.epoch.Load() {
			p.reMu.Unlock()
			return
		}
		p.seqMu.Lock()
		seq := p.seq
		p.seq++
		p.seqMu.Unlock()
		p.copies[seq] = c
		p.copyQ = append(p.copyQ, seq)
		p.notePredecessor(seq, c)
		now := mono()
		if reliable {
			p.copySent[seq] = now // refreshed in sendOn at the real transmission
		} else if len(p.copyQ) > 16384 {
			delete(p.copies, p.copyQ[0])
			delete(p.copySent, p.copyQ[0])
			p.copyQ = p.copyQ[1:]
		}
		p.reMu.Unlock()
		p.send(seq, c, ep, now, full)
	}
}

// recvLoop reads this link's datagrams in batches (one recvmmsg for up to `batch` packets: on the MIPS board
// every syscall also costs emulated 64-bit atomics in the Go poller).
func (p *peer) recvLoop(l *link) {
	pc := ipv4.NewPacketConn(l.conn)
	msgs := make([]ipv4.Message, batch)
	for i := range msgs {
		msgs[i].Buffers = [][]byte{make([]byte, 2048)}
	}
	for {
		k, err := pc.ReadBatch(msgs, 0)
		if err != nil {
			continue
		}
		p.groBegin() // v61 -tungro: segments of this batch may share one TUN write
		for i := 0; i < k; i++ {
			from, _ := msgs[i].Addr.(*net.UDPAddr)
			p.onDatagram(l, msgs[i].Buffers[0][:msgs[i].N], from)
		}
		p.groEnd()
	}
}

func (p *peer) onDatagram(l *link, b []byte, from *net.UDPAddr) {
	sessRW.RLock()
	n := p.rd(l, b, from)
	if n == newSession {
		sessRW.RUnlock()
		p.switchSession(b)
		sessRW.RLock()
		n = p.rd(l, b, from)
	}
	defer sessRW.RUnlock()
	{
		if n < 4 || b[0] != magic {
			return
		}
		switch b[1] {
		case tData:
			if n < hdrLen {
				return
			}
			if b[2] != 0 && l.peerLid.Load() != int32(b[2]) {
				l.peerLid.Store(int32(b[2])) // the sender's link id (pick writes l.id into byte 2)
			}
			nowM := mono() // the only clock read for this packet
			now := nowM / 1000
			probe := n == hdrLen // v62: an empty DATA is a dead link's quiet probe, not data
			if probe {
				p.fbSoon.Store(true) // one feedback now, even when idle: its ack revives the link
			} else {
				p.lastRxData.Store(uint32(nowM / 1e6))
			}
			ls := binary.BigEndian.Uint32(b[8:])
			sentUs := int64(binary.BigEndian.Uint64(b[12:]))
			owd := now - sentUs
			l.mu.Lock()
			gapFrom, gapN := linkGap(l.rxLseq, ls)
			if int32(ls-l.rxLseq) > 0 {
				l.rxLseq = ls
			}
			if !l.haveHi || int32(ls-l.hiLseq) > 0 {
				l.hiLseq, l.hiAt, l.haveHi = ls, now, true
			}
			if !l.haveOwd || nowM-l.lastRx > int64(500*time.Millisecond) { // link was silent: old delay is stale
				l.owdEwma, l.haveOwd, l.owdJit = owd, true, 0
			}
			l.owdEwma += (owd - l.owdEwma) / 10 // EWMA 0.9/0.1 in integer µs
			l.noteOwd(owd, nowM)
			l.rxBytes += int64(n)
			l.trRx++
			l.lastRx = nowM
			l.mu.Unlock()
			if gapN > 0 {
				p.nackRange(l.id, gapFrom, gapN)
			}
			if probe {
				return // it moved the link's numbering (reported in feedback); nothing to deliver
			}
			if n-hdrLen > rxBufSize {
				return
			}
			pl := getRx(n - hdrLen) // back to the pool when its consumer is done (pool.go)
			copy(pl, b[hdrLen:n])
			p.deliverS(binary.BigEndian.Uint32(b[4:]), binary.BigEndian.Uint32(b[20:]), b[3]&hasFlowPrev != 0, pl, sentUs)
		case tNack:
			p.onNack(b[:n])
		case tNackR:
			p.onNackRange(b[:n])
		case tFeedback:
			l.mu.Lock()
			l.fbRx = time.Now()
			l.mu.Unlock()
			if id := fbLinkID(b[:n]); id > 0 && l.peerLid.Load() != int32(id) {
				l.peerLid.Store(int32(id))
			}
			p.onFeedback(b[:n])
		}
	}
}

// linkGap: a link is a FIFO, so a jump in its numbering = exactly these packets were lost. last = the last linkSeq
// seen on the link, 0 = none yet: every session numbers each link from 1 (fresh process, or resetPeer on both ends),
// so a FIRST packet with linkSeq k > 1 means 1..k-1 were lost. (Until v34 the first packet never counted as a gap:
// a loss at the very start of a session was never NACKed, its hist entries were covered by the later linkSeq, and
// only the head-of-line timeout repaired it – one packet per second, a 23-packet burst = a 25 s stall.)
func linkGap(last, ls uint32) (from, n uint32) {
	if int32(ls-last) < 2 {
		return 0, 0
	}
	n = ls - last - 1
	if n > 4096 {
		n = 4096
	}
	return last + 1, n
}

func (p *peer) deliver(seq uint32, pl []byte) { p.deliverF(seq, 0, false, pl) }

// deliverF: fp = the packet's flow predecessor when hasFP (see flowrel.go).
func (p *peer) deliverF(seq, fp uint32, hasFP bool, pl []byte) { p.deliverS(seq, fp, hasFP, pl, 0) }

// deliverS: sentUs = the sender's send time of this transmission (µs, its clock), 0 = unknown (v59 P4).
func (p *peer) deliverS(seq, fp uint32, hasFP bool, pl []byte, sentUs int64) {
	p.rxMu.Lock()
	defer p.rxMu.Unlock()
	if !p.started {
		p.next, p.started = seq, true
	}
	if seq == p.next && len(p.buf) == 0 { // the common case: next in order, nothing waiting – no map work
		p.out(pl)
		p.next++
		p.delivered++
		p.trDeliv++
		return
	}
	if int32(seq-p.next) < 0 {
		if p.skippedSet[seq] { // arrived after we gave up on it: deliver late – reordering hurts TCP less than loss
			delete(p.skippedSet, seq)
			p.out(pl)
			p.lateDeliv++
		} else {
			putRx(pl) // a duplicate of something already delivered
			p.dups++
		}
		return
	}
	if _, dup := p.buf[seq]; dup {
		putRx(pl)
		p.dups++
		return
	}
	if noDelay(pl) { // an acknowledgement must not wait behind a lost data packet: deliver it now, keep only its place
		p.out(pl)
		p.buf[seq] = nil // flush passes it without delivering it twice
		p.noteWait(seq)
		p.noDelayed++
		// and the in-order point must move on when this was the next packet: with only pure ACKs flowing one way
		// (a download), the cumulative ack froze and that direction died once maxCopies filled up (review
		// finding, TestRegPureAcksMoveCumAck)
		p.flush()
		return
	}
	if hasFP && flowRel && isIP(pl) { // proxy frames keep the strict order whatever the header says
		if p.flowReady(seq, fp) {
			p.releaseEarly(seq, pl)
			p.noteWait(seq)
			p.flush() // seq may have been the in-order point itself
			return
		}
		if p.waitOn == nil {
			p.waitOn = map[uint32]uint32{}
		}
		p.waitOn[fp] = seq
	}
	if seq != p.next && curMode() == modeSmooth && isUDP(pl) {
		p.live = append(p.live, liveItem{seq, mono()})
	}
	if sentUs != 0 && ipRelease > 0 && reliable && seq != p.next && isIP(pl) {
		p.noteSent(seq, sentUs)
	}
	p.buf[seq] = pl
	p.noteWait(seq)
	p.flush()
}

// dupWorthy: small packets worth sending over two links at once (all modes): the proxy's control frames, DNS,
// ICMP and TCP connection setup/teardown (SYN, FIN, RST). Plain TCP ACKs are not – thousands per second during a
// download, and every packet costs CPU on the board.
func dupWorthy(pl []byte) bool {
	if len(pl) == 0 || len(pl) > 512 {
		return false
	}
	if pl[0] >= fOpen && pl[0] <= fLast && len(pl) >= fHdr {
		return pl[0] != fData
	}
	if len(pl) < 20 || pl[0]>>4 != 4 {
		return false
	}
	ihl := int(pl[0]&0x0f) * 4
	if ihl < 20 || len(pl) < ihl+4 {
		return false
	}
	switch pl[9] {
	case 1: // ICMP
		return true
	case 17: // UDP: DNS
		sp, dp := binary.BigEndian.Uint16(pl[ihl:]), binary.BigEndian.Uint16(pl[ihl+2:])
		return sp == 53 || dp == 53
	case 6: // TCP: SYN, FIN, RST
		return len(pl) >= ihl+14 && pl[ihl+13]&0x07 != 0
	}
	return false
}

// noDelay: packets that may overtake the order: a pure TCP
// acknowledgement (IPv4, no payload, no SYN/FIN/RST) and the proxy's WIN frame (credit). Both only report progress
// and carry nothing that depends on what came before; holding them behind a hole slows down the other direction.
// The receiver decides by itself – no change on the wire, works with any sender.
func noDelay(pl []byte) bool {
	if len(pl) >= fHdr && pl[0] == fWin {
		return true
	}
	if len(pl) < 40 || pl[0]>>4 != 4 || pl[9] != 6 { // IPv4 + TCP only
		return false
	}
	ihl := int(pl[0]&0x0f) * 4
	if ihl < 20 || len(pl) < ihl+20 {
		return false
	}
	total := int(binary.BigEndian.Uint16(pl[2:]))
	doff := int(pl[ihl+12]>>4) * 4
	flags := pl[ihl+13]
	return total == ihl+doff && flags&0x10 != 0 && flags&0x07 == 0 // ACK, no payload, no SYN/FIN/RST
}

func (p *peer) flush() {
	for {
		pl, ok := p.buf[p.next]
		if !ok {
			break
		}
		delete(p.buf, p.next)
		p.out(pl)
		if len(p.waitOn) > 0 { // also for a placeholder: a noDelay packet's successors wait for the in-order point
			p.releaseChain(p.next)
		}
		delete(p.flowOK, p.next)
		p.next++
		p.delivered++
		p.trDeliv++
		p.gapSince = time.Time{}
	}
	if len(p.buf) > 0 && p.gapSince.IsZero() { // gapWatch (skips) reads it: time since the last progress
		p.gapSince = time.Now()
	}
	p.dropWaits()
	if len(p.buf) == 0 { // every waiting packet sits in buf: entries left are stale (skips)
		clear(p.waitOn)
		clear(p.flowOK)
	}
}

// holdTime: how long a hole may wait – the spread of the links' delays plus margin.
func (p *peer) holdTime() time.Duration {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, l := range p.links {
		l.mu.Lock()
		if l.haveOwd && mono()-l.lastRx < int64(time.Second) { // only links that deliver now
			lo, hi = math.Min(lo, float64(l.owdEwma)), math.Max(hi, float64(l.owdEwma))
		}
		l.mu.Unlock()
	}
	h := 100000.0
	if !math.IsInf(lo, 0) {
		h = (hi-lo)*1.5 + 40000 // 40 ms like inputBufTimeout
	}
	return time.Duration(math.Max(20000, math.Min(h, 400000))) * time.Microsecond
}

func (p *peer) gapWatch() {
	if reliable {
		return // reliable mode never skips a hole: no need to wake 500x a second
	}
	for range time.Tick(2 * time.Millisecond) {
		hold := p.holdTime()
		p.rxMu.Lock()
		if !p.gapSince.IsZero() {
			p.stallTicks++
		}
		if !reliable && !p.gapSince.IsZero() && time.Since(p.gapSince) > hold {
			var low uint32
			first := true
			for s := range p.buf {
				if first || int32(s-low) < 0 {
					low, first = s, false
				}
			}
			for q := p.next; q != low; q++ {
				p.skippedSet[q] = true
			}
			if len(p.skippedSet) > 8192 {
				for q := range p.skippedSet {
					if int32(low-q) > 8192 {
						delete(p.skippedSet, q)
					}
				}
			}
			p.skipped += int64(low - p.next)
			p.trSkip += int64(low - p.next)
			p.next = low
			p.gapSince = time.Time{}
			p.flush()
		}
		p.rxMu.Unlock()
	}
}

// feedback per link: highest linkSeq received, how long ago (µs), smoothed one-way delay.
func (p *peer) feedbackLoop() {
	var lastFb, lastMiss time.Time
	for range time.Tick(fbEvery) {
		// before the idle check: a receiver waiting for a lost packet gets no data, and that is not idle
		if reliable && time.Since(lastMiss) >= missEvery {
			sessRW.RLock()
			if m := p.missingReport(uint32(time.Since(startTime) / time.Millisecond)); m != nil {
				lastMiss = time.Now()
				for _, l := range p.links {
					p.wr(l, m)
				}
			}
			sessRW.RUnlock()
		}
		// idle: no data for a while -> feedback only every 2 s (keeps CGNAT mappings, saves the SIM's data);
		// a quiet probe arrived (v62): one feedback now
		soon := p.fbSoon.Swap(false)
		if !soon && uint32(time.Since(startTime)/time.Millisecond)-p.lastRxData.Load() > 200 && time.Since(lastFb) < 2*time.Second {
			continue
		}
		lastFb = time.Now()
		sessRW.RLock() // content and echo from the same session
		out := []byte{magic, tFeedback, byte(len(p.links)), modeByte()}
		now := nowUs()
		for _, l := range p.links {
			e := make([]byte, 21)
			l.mu.Lock()
			if l.haveHi {
				e[0] = 1
				binary.BigEndian.PutUint32(e[1:], l.hiLseq)
				binary.BigEndian.PutUint64(e[5:], uint64(now-l.hiAt))
				binary.BigEndian.PutUint64(e[13:], uint64(l.owdEwma))
			}
			l.mu.Unlock()
			out = append(out, e...)
		}
		if reliable {
			p.rxMu.Lock()
			ca := []byte{1, 0, 0, 0, 0}
			binary.BigEndian.PutUint32(ca[1:], p.next)
			p.rxMu.Unlock()
			out = append(out, ca...)
		}
		for _, l := range p.links { // each copy names the link it goes over: the far end pairs its links by it
			fb := make([]byte, len(out), len(out)+2+trailerLen)
			copy(fb, out)
			p.wr(l, append(fb, fbLidMark, byte(l.id)))
		}
		sessRW.RUnlock()
	}
}

// Still-missing report (reliable): a link-gap NACK is sent once. When all its copies are lost (one burst on a shared
// path takes both), nothing else names those packets: the hist entries were covered by later linkSeqs, so no dead
// link, and only the head-of-line timeout repairs them – ONE packet per ~second, a lost NACK of 8 packets = 8+ s of
// stall. So the receiver names its holes again by seq while they last; the sender resends only packets whose last
// transmission had a round trip to arrive (onNack), so repeating the list costs no duplicate data.
//
// The clock is the age of the oldest packet waiting in the reorder buffer (holeWaitMs), not the time since the last
// progress: a collapsed link delivers its part of the hole one packet every ~10 ms, and each of them restarted that
// clock – no report for seconds (real LTE, 25 Nov).
const (
	missAfter = 200 * time.Millisecond // a packet waited this long in the reorder buffer: report the holes
	missEvery = 100 * time.Millisecond // and again every missEvery while they last
	missMax   = 256                    // seqs per report (1 kB)
	missScan  = 8192                   // seqs looked at per report (bounded CPU with a full buffer)
)

// missingReport: tNack with the seqs missing below the highest one received (lowest first), or nil when no packet
// waited missAfter for a hole. nowMs = ms since start (the lastRxData clock).
func (p *peer) missingReport(nowMs uint32) []byte {
	p.rxMu.Lock()
	defer p.rxMu.Unlock()
	if v58() {
		if p.holeWaitMs(nowMs) < int64(missAfter/time.Millisecond) {
			return nil
		}
	} else if len(p.buf) == 0 || p.gapSince.IsZero() || time.Since(p.gapSince) < missAfter { // v57: since the last progress
		return nil
	}
	m := []byte{magic, tNack, 0, 0}
	seen := 0
	for q, i := p.next, 0; seen < len(p.buf) && len(m) < 4+4*missMax && i < missScan; q, i = q+1, i+1 {
		if _, ok := p.buf[q]; ok {
			seen++
		} else {
			m = binary.BigEndian.AppendUint32(m, q)
		}
	}
	p.missRep += int64(len(m)-4) / 4
	return m
}

// retransmitter (reliable): drop what the peer acknowledged, resend what waited longer than the RTO.
func (p *peer) retransmitter() {
	for range time.Tick(20 * time.Millisecond) {
		p.rtxTick(time.Now(), mono())
	}
}

// rtxTick: one retransmitter pass (now and nowM = mono() of the same moment).
func (p *peer) rtxTick(now time.Time, nowM int64) {
	{
		maxSrtt := 0.0 // a stuck link's srtt (its queue) counts only up to its healthy round trip (slowlink.go)
		for _, l := range p.links {
			l.mu.Lock()
			if g := l.guardRttUs(); !l.dead && g > maxSrtt {
				maxSrtt = g
			}
			l.mu.Unlock()
		}
		rto := time.Duration(math.Max(1000000, 3*maxSrtt)) * time.Microsecond // last resort: NACKs and dead links repair the rest
		p.reMu.Lock()
		k := 0
		for ; k < len(p.copyQ) && int32(p.copyQ[k]-p.cumAck) < 0; k++ {
			delete(p.copies, p.copyQ[k])
			delete(p.copySent, p.copyQ[k])
		}
		p.copyQ = p.copyQ[k:]
		// only the FIRST missing packet gets a timeout resend (everything after it the receiver may already
		// hold – resending it all was go-back-N and ~3 % waste); NACKs and dead links repair the rest
		if p.cumAck != p.lastAck {
			p.lastAck, p.ackMovedAt, p.rtoBackoff = p.cumAck, now, 1
		}
		hol := p.cumAck
		if fixGuard { // v59 P2: the head's own repair time (the link it went on), at least the 1 s fallback
			rto = time.Duration(max(int64(time.Second), p.dueSpan(hol)))
		}
		if d, ok := p.copies[hol]; ok && now.Sub(p.ackMovedAt) > rto*time.Duration(p.rtoBackoff) && nowM-p.copySent[hol] > int64(rto) {
			p.copySent[hol] = nowM
			p.setDue(hol, nowM+int64(rto)) // refreshed in sendOn when it really leaves
			p.reinject = append([]sentPkt{{seq: hol, data: d, ep: p.epoch.Load()}}, p.reinject...)
			p.kickRepairs()
			p.rtx++
			if p.rtoBackoff < 2 { // a long back-off was a multi-second stall when the resend died again
				p.rtoBackoff *= 2
			}
		}
		p.reMu.Unlock()
	}
}

// drainer sends queued repairs even when the TUN is quiet (TCP may be waiting for exactly them).
// deadWatch: a link with data in flight and no progress is dead – checked on a clock, not only when sending:
// when nothing can be sent (credit used up behind a hole that sits on that very link) nobody else would notice.
func (p *peer) deadWatch() {
	var w deadState
	for range time.Tick(10 * time.Millisecond) {
		p.deadTick(&w, mono())
	}
}

// deadState: deadWatch's scratch space, reused every tick.
type deadState struct {
	probe []*link
	quiet []*link // v62: long-dead links due for a quiet probe
	cand  []bool
	long  []bool // v62: dead for quietAfter
	keep  []float64
}

// deadTick: one deadWatch pass at now = mono().
func (p *peer) deadTick(w *deadState, now int64) {
	w.probe, w.quiet = w.probe[:0], w.quiet[:0]
	alive := 0 // live links with a known peer address (v62: a long-dead link goes quiet only while one exists)
	m := curMode()
	p.slowTick(slowFrac > 0 && (m == modeSpeed || m == modeSmooth), now) // v60
	rescueOn := reliable && v58() && (m == modeSpeed || m == modeSmooth) // redundant has its copies, backup its primary
	// pass 1: which links are stuck, and is any other link fine (acks younger than its healthy round trip)? When all
	// links stop acking in the same moment the cause is common (CPU or GC stall, delayed feedback, the reverse path):
	// copies over the same links would only double the load – no rescue.
	if len(w.cand) != len(p.links) {
		w.cand, w.keep, w.long = make([]bool, len(p.links)), make([]float64, len(p.links)), make([]bool, len(p.links))
	}
	good, fbFresh := 0, 0
	for _, l := range p.links {
		l.mu.Lock()
		if !l.fbRx.IsZero() && time.Since(l.fbRx) < fbFreshFor {
			fbFresh++
		}
		l.mu.Unlock()
	}
	for i, l := range p.links {
		w.cand[i] = false
		if !rescueOn {
			continue
		}
		l.mu.Lock()
		w.cand[i], w.keep[i] = l.stuckCheck(now / 1000)
		if !w.cand[i] && l.ackFresh(now) {
			good++
		}
		l.mu.Unlock()
	}
	for i, l := range p.links {
		l.mu.Lock()
		if w.cand[i] && good > 0 {
			// a stuck link: no new data, and what it will not deliver in time goes over the others
			if !l.stuck {
				l.stuckCount++
			}
			l.stuck = true
			if rescue {
				l.trRescued += int64(p.rescueLate(l, w.keep[i], now)) // lock order l.mu -> reMu
			}
		} else {
			l.stuck = false
		}
		noAck := len(l.hist) > 0 && now-l.progress > l.deadAfterNs
		// v62b: silent with nothing in flight (needs feedback seen once, and another link with fresh feedback, so a
		// stall common to all links does not kill them all)
		noFb := !noAck && fbFresh > 0 && !l.fbRx.IsZero() && time.Since(l.fbRx) > fbDeadAfter && l.dst() != nil &&
			now-l.progress > int64(fbDeadAfter) // just back (its probe acknowledged via another link): its own feedback is on the way
		if !l.dead && (noAck || noFb) {
			l.dead, l.deadAt, l.deadWin = true, time.Now(), l.win
			l.deadAtM, l.quietLog = now, false
			l.deadCount++
			var oldest int64
			if len(l.hist) > 0 {
				oldest = (now/1000 - l.hist[0].at) / 1000
			}
			p.reMu.Lock()
			p.reinject = appendUnrescued(p.reinject, l.hist)
			p.kickRepairs()
			n := len(l.hist)
			p.reMu.Unlock()
			l.hist, l.inflight = nil, 0
			if noFb {
				log.Printf("L%d dead (no feedback for %d ms, nothing in flight)", l.id, time.Since(l.fbRx).Milliseconds())
			} else {
				log.Printf("L%d dead (%d packets to resend; no new ack %d ms, last feedback %d ms ago, oldest unacked %d ms, limit %d ms)",
					l.id, n, (now-l.progress)/1e6, time.Since(l.fbRx).Milliseconds(), oldest, l.deadAfterNs/1e6)
			}
		}
		w.long[i] = l.longDead(now)
		if reliable && l.dead && !w.long[i] && now-l.lastSend > int64(starveFor) {
			w.probe = append(w.probe, l)
		}
		if w.long[i] && now-l.lastSend >= int64(quietProbeEvery) {
			w.quiet = append(w.quiet, l) // v62
		}
		if !l.dead && l.dst() != nil {
			alive++
		}
		if !l.dead && !l.draining && len(l.hist) == 0 && l.win > winInit &&
			now-l.lastSend > int64(idleAfter) && now-l.idleCut > int64(idleAfter) {
			l.win = math.Max(winInit, l.win/2)
			l.idleCut = now
			l.derive()
		}
		l.mu.Unlock()
	}
	// a dead link gets a probe (the oldest unacknowledged packet) on its own clock, not only from senders:
	// it comes back when the peer acknowledges the probe
	for _, l := range w.probe {
		p.reMu.Lock()
		ep, seq := p.epoch.Load(), p.cumAck
		d, ok := p.copies[seq]
		p.reMu.Unlock()
		if ok {
			p.sendOn(l, seq, d, ep, now, nil)
		} else {
			p.emptyProbe(l, now) // v62b: nothing unacknowledged to probe with (dead without data in flight)
		}
	}
	for _, l := range w.quiet {
		p.quietProbe(l, now)
	}
	p.quietUpdate(w.long, alive)
}

func (p *peer) drainer() {
	t := time.NewTicker(50 * time.Millisecond) // fallback only; repairs normally wake us through kick
	for {
		select {
		case <-p.kick:
		case <-t.C:
		}
		p.drainRepairs()
	}
}

func (p *peer) kickRepairs() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

func (p *peer) drainRepairs() {
	for {
		p.reMu.Lock()
		if len(p.reinject) == 0 {
			p.reMu.Unlock()
			return
		}
		r := p.reinject[0]
		p.reinject = p.reinject[1:]
		acked := reliable && int32(r.seq-p.cumAck) < 0
		p.reMu.Unlock()
		if !acked && r.ep == p.epoch.Load() { // the peer already has it, or an old session's: do not wait for a window
			p.send(r.seq, r.data, r.ep, mono(), nil)
		}
	}
}

// nackRange: "link id lost linkSeq from..from+n-1" – sent on every link (tiny, must arrive).
func (p *peer) nackRange(id int, from, n uint32) {
	m := []byte{magic, tNackR, byte(id), 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(m[4:], from)
	binary.BigEndian.PutUint16(m[8:], uint16(n))
	p.rxMu.Lock()
	p.nacked += int64(n)
	p.rxMu.Unlock()
	for _, l := range p.links {
		p.wr(l, m)
	}
}

func (p *peer) onNackRange(b []byte) {
	if len(b) < 10 {
		return
	}
	l := p.byFarID(int(b[2])) // b[2] = the far end's link that saw the gap
	if l == nil {
		return
	}
	from := binary.BigEndian.Uint32(b[4:])
	n := uint32(binary.BigEndian.Uint16(b[8:]))
	seqs := make([]lmapEnt, 0, n)
	l.mu.Lock()
	for i := uint32(0); i < n; i++ {
		if q, ok := l.lmap[from+i]; ok {
			seqs = append(seqs, q)
			delete(l.lmap, from+i) // the same NACK arrives over every link: repair once
		}
	}
	l.lossNacked += int64(len(seqs))
	l.mu.Unlock()
	now := mono()
	p.reMu.Lock()
	for _, e := range seqs {
		q := e.seq
		d, ok := p.copies[q]
		if !ok || (reliable && int32(q-p.cumAck) < 0) {
			continue
		}
		// sent again since this transmission (a stuck link's rescue, a still-missing report): that copy is on its way
		if reliable && v58() && p.copySent[q] > e.at*1000+int64(time.Millisecond) {
			continue
		}
		p.copySent[q] = now // duplicates of this NACK (it comes over every link) found nothing in lmap above
		p.setDue(q, now+int64(time.Second))
		p.reinject = append(p.reinject, sentPkt{seq: q, data: d, ep: p.epoch.Load()})
		p.kickRepairs()
		p.repaired++
	}
	p.reMu.Unlock()
}

// onNack: a list of seqs the peer misses (its still-missing report; it comes over every link and repeats while
// the holes last).
func (p *peer) onNack(b []byte) {
	if len(b) < 8 {
		return
	}
	// reliable: resend only a packet whose last transmission had a round trip to arrive before the report was made
	// (the slowest live link's srtt, with margin) – a packet still on its way is not lost, and a repeated report or
	// its copy from the other link must not send the same packet again. A stuck link's srtt counts only up to its
	// healthy round trip: the srtt of a link whose rate collapsed grows with its queue (seconds) and protected exactly
	// the packets stuck in that queue from repair (v58, slowlink.go).
	guard := int64(30 * time.Millisecond)
	if reliable && !fixGuard {
		for _, l := range p.links {
			l.mu.Lock()
			if !l.dead {
				guard = max(guard, int64(1.5*l.guardRttUs())*1000)
			}
			l.mu.Unlock()
		}
	}
	now := mono()
	p.reMu.Lock()
	n := 0
	for off := 4; off+4 <= len(b); off += 4 {
		seq := binary.BigEndian.Uint32(b[off:])
		d, ok := p.copies[seq]
		if !ok {
			continue
		}
		if !reliable {
			delete(p.copies, seq) // repair once (the NACK came over every link)
		} else if int32(seq-p.cumAck) < 0 || !p.lateNow(seq, now, guard) {
			continue
		} else {
			p.copySent[seq] = now
			p.setDue(seq, now+int64(time.Second)) // until sendOn sets the real one: no second resend meanwhile
		}
		p.reinject = append(p.reinject, sentPkt{seq: seq, data: d, ep: p.epoch.Load()})
		p.repaired++
		if reliable {
			p.missRtx++
		}
		n++
	}
	if n > 0 {
		p.kickRepairs()
	}
	p.reMu.Unlock()
}

func (p *peer) onFeedback(b []byte) {
	n := int(b[2]) // the sender's link count: its entry i is what arrived on ITS link i+1
	if len(b) < 4+21*n {
		return
	}
	onPeerMode(b[3])
	if reliable && len(b) >= 4+21*n+5 && b[4+21*n] == 1 {
		ca := binary.BigEndian.Uint32(b[4+21*n+1:])
		p.reMu.Lock()
		if int32(ca-p.cumAck) > 0 {
			p.cumAck = ca
		}
		p.reMu.Unlock()
	}
	now := time.Now()
	nowU := nowUs()
	owds := make([]int64, n) // µs, integers on the wire (v62: no float until relOwd)
	minOwd := int64(math.MaxInt64)
	for i := 0; i < n; i++ {
		e := b[4+21*i:]
		if e[0] == 1 {
			owds[i] = int64(binary.BigEndian.Uint64(e[13:]))
			minOwd = min(minOwd, owds[i])
		}
	}
	for _, l := range p.links {
		i := l.farIdx(n) // the far end's entry for what this link sent
		if i < 0 {
			continue
		}
		e := b[4+21*i:]
		if e[0] != 1 {
			continue
		}
		hi := binary.BigEndian.Uint32(e[1:])
		age := int64(binary.BigEndian.Uint64(e[5:]))
		l.mu.Lock()
		dk := l.deriveKey()
		l.relOwd = float64(owds[i] - minOwd)
		// "window in use" is judged by what was in flight when this feedback ARRIVED: after the removal below a
		// feedback that acknowledged a whole small window leaves ~0 in flight, and the window never grew again
		// (seen: 32 kB windows for 30+ s of a download when all the delay was on the feedback's way back)
		inflAtFb := l.inflight
		// drop everything up to the highest linkSeq the peer saw: delivered, or lost on the way
		k := 0
		var hiSent int64 = -1
		for ; k < len(l.hist) && int32(l.hist[k].lseq-hi) <= 0; k++ {
			l.inflight -= l.hist[k].size
			l.ackBytes += l.hist[k].size
			if l.hist[k].lseq == hi {
				hiSent = l.hist[k].at
			}
		}
		l.hist = l.hist[k:]
		if l.ackAt.IsZero() {
			l.ackAt = now
		} else if el := now.Sub(l.ackAt); el >= 100*time.Millisecond {
			l.rate2, l.rate = l.rate, float64(l.ackBytes)/float64(el.Microseconds())
			l.ackBytes, l.ackAt = 0, now
			if l.rateAvg == 0 {
				l.rateAvg = l.rate
			}
			l.rateAvg += (l.rate - l.rateAvg) / 4
			if l.rate > l.rateCur {
				l.rateCur = l.rate
			}
			if now.Sub(l.rateAt) > time.Second { // a slower cell must lower the cap within ~2 s, not 6
				l.ratePrev, l.rateCur, l.rateAt = l.rateCur, l.rate, now
			}
			l.rateMax = fmax(l.rateCur, l.ratePrev)
			l.noteRate(l.rate, float64(inflAtFb), mono()) // v60
		}
		if k > 0 {
			l.progress = mono()
			if l.dead {
				l.dead = false
				l.quiet.Store(false) // v62: control datagrams again at once
				l.srtt = 0           // restart from the first fresh sample
				if now.Sub(l.deadAt) < 3*time.Second {
					// short outage (handover, dead spot): most likely the same cell – warm restart from half the window
					l.win = fmax(4*winMin, l.deadWin/2)
					if drainFix { // v59 P3: not in one step (a line-rate burst), ×2 per round trip
						l.win, l.winTarget = 4*winMin, l.win
					}
				} else {
					l.win = 4 * winMin // long outage: maybe a different cell, start small and relearn
					l.minRtt, l.rateMax, l.rateCur, l.ratePrev = 0, 0, 0, 0
				}
				log.Printf("L%d back", l.id)
			}
		}
		if l.inflight < 0 || len(l.hist) == 0 {
			l.inflight = 0
			for _, h := range l.hist {
				l.inflight += h.size
			}
		}
		if hiSent > 0 { // fresh RTT sample: send -> peer received -> feedback here, minus the peer's holding time
			l.rtt = float64(nowU-hiSent-age) + 0
			if l.srtt == 0 {
				l.srtt = l.rtt
			}
			l.srtt = 0.875*l.srtt + 0.125*l.rtt
			if l.prevRtt2 > 0 { // jitter: |2nd difference| / 2 – a queue that grows or drains steadily adds nothing,
				// white jitter gives ≈ σ; a sample is clipped to 4× the estimate (+2 ms), so a rate step or drain barely moves it
				d := math.Abs(l.rtt-2*l.prevRtt+l.prevRtt2) / 2
				l.jit += (fmin(d, 4*l.jit+2000) - l.jit) / 16
			}
			l.prevRtt2, l.prevRtt = l.prevRtt, l.rtt
			// base rtt: lowered by any lower srtt; re-measured by a short drain every ~10 s (a queue
			// that never empties would otherwise hide the real path delay and the window would bloat)
			if l.minRtt == 0 || l.srtt <= l.minRtt {
				l.minRtt, l.baseAt = l.srtt, now
			}
			if l.draining {
				if l.drainLowAt.IsZero() && float64(l.inflight) <= fmax(2*winMin, l.drainWin) {
					l.drainLowAt = now
				}
				if !l.drainLowAt.IsZero() && hiSent >= int64(l.drainLowAt.Sub(startTime)/time.Microsecond) {
					if l.drainN < len(l.drainS) {
						l.drainS[l.drainN] = l.rtt
					}
					l.drainSum += l.rtt
					l.drainN++
				}
				if l.drainN >= 4 || now.Sub(l.drainStart) > 1500*time.Millisecond {
					if l.drainN > 0 {
						if drainFix { // v59 P3: the lower half of the samples – the plain average can lift the base
							l.minRtt = drainBase(l.drainS[:min(l.drainN, len(l.drainS))])
						} else {
							l.minRtt = l.drainSum / float64(l.drainN)
						}
					}
					l.endDrain(now)
					l.adjUs = 0
					p.drainBusy.Store(false)
				}
			} else if now.Sub(l.baseAt) > drainEvery && l.drainOK(now.Sub(l.baseAt)) && p.drainBusy.CompareAndSwap(false, true) {
				l.draining, l.drainStart, l.drainLowAt = true, now, time.Time{}
				l.drainSum, l.drainN, l.drainSaved = 0, 0, l.win
				l.win = l.drainTarget()
				l.drainWin = l.win
			}
			incEx, decEx := incLimit*l.minRtt, decLimit*l.minRtt // allowed excess of the rtt over the base: grow / cut
			if jitK > 0 {
				j := fmin(jitK*l.jit, jitCap/2)
				incEx, decEx = fmax(incEx, j), fmax(decEx, 2*j)
			}
			if l.srtt > l.minRtt+incEx && l.srtt <= l.minRtt+decEx { // stuck in the hold band: our queue, or a slower cell? a drain tells
				if l.highSince.IsZero() {
					l.highSince = now
				}
			} else {
				l.highSince = time.Time{}
			}
			// path got slower (cell change): the window is already minimal, so this delay is not our queue → new base.
			// Only with little in flight: after a rate collapse the window is minimal too, but the delay is the queue
			// sent under the old window draining (real LTE: the base jumped to 2.7 s and 4.7 s, v58)
			if !l.draining && l.srtt > l.minRtt+incEx && l.win <= 4*winMin && (!v58() || float64(l.inflight) <= 4*winMin) {
				if l.slowSince.IsZero() {
					l.slowSince = now
				} else if now.Sub(l.slowSince) > 700*time.Millisecond {
					l.minRtt, l.baseAt = l.srtt, now
					l.slowSince = time.Time{}
				}
			} else {
				l.slowSince = time.Time{}
			}
			if l.draining {
				l.derive()
				l.mu.Unlock()
				continue
			}
			// at most one change per REAL round trip: only a sample of a packet sent after the last change shows what
			// that change did. (Once per base RTT on the smoothed srtt – which lags ~160 ms – cut a deep LTE downlink
			// queue 5-7 times in a row, far below the path's BDP.) The fresh sample must agree with srtt.
			nowM := mono()
			if win := int64(fmax(100000, l.srtt)) * 1000; nowM-l.lossAt >= win { // close the loss window
				if l.lossSent >= 50 {
					l.lossFrac = float64(l.lossNacked) / float64(l.lossSent)
				} else if nowM-l.lossAt >= 4*win {
					l.lossFrac = 0 // (almost) nothing sent for long: forget the old loss
				}
				if l.lossSent >= 50 || nowM-l.lossAt >= 4*win {
					l.lossSent, l.lossNacked, l.lossAt = 0, 0, nowM
				}
			}
			if hiSent > l.adjUs {
				hiR, loR := fmax(l.srtt, l.rtt), fmin(l.srtt, l.rtt)
				switch {
				case l.lossFrac > lossCut: // the link drops what we send without (much) more delay: send less
					l.win = fmax(winMin, fmax(l.win*lossFactor, fmin(l.win, l.rateAvg*l.minRtt)))
					l.adjUs = nowU
					l.winTarget = 0
					l.lossFrac = 0 // one cut per window of evidence
					l.trCuts++
					l.lossCuts++
					if l.maxQ > 0 && maxQAdapt { // the cell drops before our queue cap: allow less queue (min 20 ms)
						l.maxQ, l.maxQAt = fmax(20000, l.maxQ*0.8), nowM
					}
				case l.lossFrac > lossHold: // some loss: hold, don't grow into it (before the grow cases)
				case l.winTarget > l.win && loR <= l.minRtt+decEx: // v59 P3: back to the window before a drain or an outage, ×2 per round trip
					l.win = fmin(l.winTarget, 2*l.win)
					l.adjUs = nowU
					if l.win >= l.winTarget {
						l.winTarget = 0
					}
				case hiR <= l.minRtt+incEx/2 && (float64(inflAtFb) >= 0.5*l.win || l.win <= 4*winMin): // far from a queue: double
					l.win = fmin(winMax, l.win*2)
					l.adjUs = nowU
					l.trGrows++
				case hiR <= l.minRtt+incEx && (float64(inflAtFb) >= 0.5*l.win || l.win <= 4*winMin): // grow only a window that is in use
					l.win = fmin(winMax, l.win*incFast)
					l.adjUs = nowU
					l.trGrows++
				case loR > l.minRtt+decEx:
					// never below what the link delivered lately per base RTT: the queue is ours, not a slower cell
					// (a slower cell lowers rateAvg within ~0.5 s, and the slowSince logic re-bases minRtt)
					l.win = fmax(winMin, fmax(l.win*decFactor, fmin(l.win, l.rateAvg*l.minRtt)))
					l.adjUs = nowU
					l.winTarget = 0
					l.trCuts++
				}
			}
			if l.rateMax > 0 { // never more in flight than 2.5× what the link really delivers per base RTT
				// µs of queue allowed on top of the base delay: learned per link (shrinks with loss, regrows without it)
				if l.maxQ == 0 {
					l.maxQ, l.maxQAt = maxQueueUs, nowM
				}
				if el := nowM - l.maxQAt; el >= 1e9 { // +5 ms per second without a loss cut, up to maxQueueUs
					l.maxQ = fmin(maxQueueUs, l.maxQ+5000*float64(el)/1e9)
					l.maxQAt = nowM
				}
				maxQueue := fmax(l.maxQ, 0.5*l.minRtt)
				wcap := fmax(4*winMin, l.rateMax*(l.minRtt+maxQueue))
				l.win = fmin(l.win, wcap)
				if l.winTarget > wcap {
					l.winTarget = wcap
				}
			}
		}
		if l.deriveKey() != dk { // v62: a duplicate copy changes nothing derive reads - skip its softfloat work
			l.derive()
		}
		l.mu.Unlock()
	}
}

// deriveKey (under l.mu): the bit patterns of everything derive reads, compared as integers (v62).
func (l *link) deriveKey() [7]uint64 {
	return [7]uint64{math.Float64bits(l.win), math.Float64bits(l.minRtt), math.Float64bits(l.relOwd),
		math.Float64bits(l.srtt), math.Float64bits(l.rateMax), math.Float64bits(l.maxQ), math.Float64bits(l.jit)}
}

// fmax, fmin: math.Max / math.Min for the regulator's values, which are never NaN (v62). The math versions also
// test for infinities, NaN and signed zeros - about eight softfloat calls on the MIPS box instead of one comparison
// (a CPU profile of onFeedback on a softfloat build: 19 % in math.Max alone).
func fmax(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func fmin(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// statsEvery (-statsevery, v62): the stats line's interval. Every 5 s (up to v61) filled the box's 64 kB logread
// ring in ~10 min and pushed out the night's dead/back lines; the counters in it cover one interval.
var statsEvery = 60 * time.Second

func (p *peer) stats(every time.Duration) {
	for range time.Tick(every) {
		var s []string
		for _, l := range p.links {
			l.mu.Lock()
			s = append(s, fmt.Sprintf("L%d win%.0fk infl%.0fk txq%.0fk rtt%.0f/%.0fms rel%.0fms sent%d", l.id, l.win/1000, float64(l.inflight)/1000, float64(l.txqB.Load())/1000, l.rtt/1000, l.minRtt/1000, l.relOwd/1000, l.sent))
			l.sent = 0
			l.mu.Unlock()
		}
		p.reMu.Lock()
		s = append(s, fmt.Sprintf("| rescued %d (%d kB) missrtx %d", p.rescued, p.rescuedB/1000, p.missRtx))
		p.reMu.Unlock()
		p.rxMu.Lock()
		s = append(s, fmt.Sprintf("| unknown %d", unknownFrames.Load()))
		s = append(s, fmt.Sprintf("| tx %d pkts in %d msgs", txPkts.Swap(0), txMsgs.Swap(0)))
		s = append(s, fmt.Sprintf("| wait win%d pace%d copies%d credit%d reads%d avg%dB", wWin.Swap(0), wPace.Swap(0), wCopies.Swap(0), wCredit.Swap(0), rdN.Load(), rdBytes.Swap(0)/max(1, rdN.Swap(0))))
		s = append(s, fmt.Sprintf("| deliv %d skip %d late %d nack %d miss %d rtx %d buf %d hold %v nodelay %d dup %d iprel %d", p.delivered, p.skipped, p.lateDeliv, p.nacked, p.missRep, p.rtx, len(p.buf), p.holdTime().Round(time.Millisecond), p.noDelayed, p.dups, p.ipReleased))
		p.rxMu.Unlock()
		log.Print(strings.Join(s, "  "))
	}
}

// trace: one CSV line per 100 ms – what every link and the reorder buffer did in that slice.
func (p *peer) trace(path string) {
	f, err := os.Create(path)
	if err != nil {
		log.Printf("trace: %v", err)
		return
	}
	hdr := "t_ms"
	for _, l := range p.links {
		hdr += fmt.Sprintf(",L%[1]d_win_k,L%[1]d_infl_k,L%[1]d_srtt_ms,L%[1]d_dead,L%[1]d_tx,L%[1]d_txdead,L%[1]d_rx", l.id)
	}
	hdr += ",deliv,skip,late,nack,repaired,buf,stall_ms,hold_ms"
	for _, l := range p.links {
		hdr += fmt.Sprintf(",L%[1]d_minrtt_ms,L%[1]d_rtt_ms,L%[1]d_rateavg_mbit,L%[1]d_ratemax_mbit,L%[1]d_grows,L%[1]d_cuts", l.id)
	}
	for _, l := range p.links {
		hdr += fmt.Sprintf(",L%[1]d_jit_ms", l.id)
	}
	// v58: rescued = packets sent again because their link got stuck (this slice), rescued_tot / missrtx / dups =
	// totals (resends for stuck links / for still-missing reports, duplicates received), wait_ms = how long the oldest
	// packet in the reorder buffer has waited, stuck = bit i-1 set while link i is stuck
	hdr += ",rescued,rescued_tot,missrtx,dups,wait_ms,stuck"
	// v59: txq_k = bytes queued for the link's txLoop and not yet handed to the kernel, txsent = packets txLoop really
	// sent in this slice (Lx_tx counts what pick queued: a pacer behind pick shows as tx > txsent and a growing txq),
	// pace = the pick pacing rate (Mbit/s, 0 = unpaced); iprel = IP packets released past a hole by their deadline
	for _, l := range p.links {
		hdr += fmt.Sprintf(",L%[1]d_txq_k,L%[1]d_txsent,L%[1]d_pace_mbit", l.id)
	}
	hdr += ",iprel"
	// v60: slow = bit i-1 set while link i is demoted as persistently slow, trialcp = trial copies sent in this slice
	hdr += ",slow,trialcp"
	fmt.Fprintln(f, hdr)
	t0 := time.Now()
	for range time.Tick(100 * time.Millisecond) {
		line := fmt.Sprintf("%d", time.Since(t0).Milliseconds())
		extra, jits, v59 := "", "", ""
		var resc, stuck, slow, trialCp int64
		for _, l := range p.links {
			l.mu.Lock()
			d := 0
			if l.dead {
				d = 1
			}
			resc += l.trRescued
			l.trRescued = 0
			if l.stuck {
				stuck |= 1 << (l.id - 1)
			}
			if l.sl.slow {
				slow |= 1 << (l.id - 1)
			}
			trialCp += l.sl.trCopies
			l.sl.trCopies = 0
			line += fmt.Sprintf(",%.0f,%.0f,%.0f,%d,%d,%d,%d", l.win/1000, float64(l.inflight)/1000, l.srtt/1000, d, l.trTx, l.trTxDead, l.trRx)
			extra += fmt.Sprintf(",%.0f,%.0f,%.1f,%.1f,%d,%d", l.minRtt/1000, l.rtt/1000, l.rateAvg*8, l.rateMax*8, l.trGrows, l.trCuts)
			jits += fmt.Sprintf(",%.1f", l.jit/1000)
			v59 += fmt.Sprintf(",%.1f,%d,%.1f", float64(l.txqB.Load())/1000, l.txSentTr.Swap(0), float64(l.paceBps)*8/1e6)
			l.trTx, l.trTxDead, l.trRx, l.trGrows, l.trCuts = 0, 0, 0, 0, 0
			l.mu.Unlock()
		}
		hold := p.holdTime()
		p.reMu.Lock()
		rep, rescTot, missRtx := p.repaired, p.rescued, p.missRtx
		p.reMu.Unlock()
		p.rxMu.Lock()
		line += fmt.Sprintf(",%d,%d,%d,%d,%d,%d,%d,%d", p.trDeliv, p.trSkip, p.lateDeliv, p.nacked, rep, len(p.buf), p.stallTicks*2, hold.Milliseconds())
		tail := fmt.Sprintf(",%d,%d,%d,%d,%d,%d", resc, rescTot, missRtx, p.dups, p.holeWaitMs(uint32(time.Since(startTime)/time.Millisecond)), stuck)
		v59 += fmt.Sprintf(",%d", p.trIPRel)
		p.trDeliv, p.trSkip, p.stallTicks, p.trIPRel = 0, 0, 0, 0
		p.rxMu.Unlock()
		fmt.Fprintln(f, line+extra+jits+tail+v59+fmt.Sprintf(",%d,%d", slow, trialCp))
	}
}

func main() {
	tunName := flag.String("tun", "shf0", "TUN device")
	links := flag.String("links", "", "local>peer per link, comma separated")
	tracePath := flag.String("trace", "", "write a 100 ms CSV trace to this file")
	statusPath := flag.String("status", "", "write a JSON status (links, rates, delays) to this file once a second")
	proxySpec := flag.String("proxy", "", "built-in TCP proxy: listen>target (connections to listen are carried to target on the far end)")
	flag.BoolVar(&serve, "serve", false, "open the far end's proxy connections (dial their targets)")
	frame := flag.Int("frame", 1400, "proxy frame size in bytes (1328 under IPsec: ESP-in-UDP must stay within 1500)")
	allow := flag.String("allow", "", "with -serve: comma separated host:port targets the far end may open (empty = any)")
	flag.BoolVar(&reliable, "reliable", false, "never skip a hole; keep and resend until acknowledged")
	cpuProf := flag.String("cpuprofile", "", "write a CPU profile here (stopped by SIGTERM/SIGINT)")
	keyPath := flag.String("key", "", "file with a 32-hex-char shared key: authenticate every datagram (and learn peer address changes)")
	flag.DurationVar(&statsEvery, "statsevery", statsEvery, "v62: interval of the periodic stats log line (0 = off); events (dead, back, slow...) are logged as they happen")
	flag.Var(authFlag{}, "auth", "v62: tag on every datagram (with -key): sip = SipHash-1-3, half = HalfSipHash-1-3 (32-bit words, cheaper on the MIPS box), off = none, ONLY for links that run under IPsec (ESP authenticates them; addresses learned without -key). Use -auth=VALUE; true/false = sip/off. Both ends must agree: a mismatch rejects every datagram (logged \"auth mismatch?\")")
	maxq := flag.Float64("maxq", maxQueueUs/1000, "queue (ms) a link may hold on top of its base delay (upper bound when adaptive)")
	flag.BoolVar(&maxQAdapt, "maxqadapt", maxQAdapt, "learn the queue cap per link from loss")
	flag.Float64Var(&lossCut, "losscut", lossCut, "cut a link's window when this share of its packets was NACKed in the last window")
	flag.Float64Var(&lossHold, "losshold", lossHold, "stop growing a link's window above this NACKed share")
	flag.Float64Var(&incLimit, "inc", incLimit, "grow window while srtt <= base*(1+inc)")
	flag.Float64Var(&decLimit, "dec", decLimit, "shrink window when srtt > base*(1+dec)")
	flag.Float64Var(&paceGain, "pace", paceGain, "spread each link's sends at this gain × window / srtt (0 = send at line rate)")
	flag.BoolVar(&flowRel, "flowrel", flowRel, "per-flow release: a packet waits only for earlier packets of its own flow")
	flag.IntVar(&fqLimit, "fqlimit", fqLimit, "flow queue between TUN and the sender, bytes (0 = read TUN directly, as up to v52)")
	flag.DurationVar(&redWait, "redwait", redWait, "redundant mode: wait at most this long for a full link before sending without its copy")
	flag.Float64Var(&slowK, "slowk", slowK, "stuck link: oldest unacked older than slowk × its healthy round trip and its queue not drained within 2 × slowk × that -> resend the late part over the other links (0 = all v58 repair changes off: v57)")
	v59Flags()
	v60Flags()
	tproxyFlags()
	flag.Float64Var(&jitK, "jitk", jitK, "jitter-aware bands: grow / cut excess at least jitk / 2*jitk × rtt jitter (0 = pure ratios, as v37)")
	modeFlag := flag.String("mode", "speed", "speed | smooth | redundant | backup, or peer = follow the far end's choice (server)")
	primary := flag.Int("primary", 1, "backup mode: the link (1 = first in -links) that carries data while it lives")
	modeFilePath := flag.String("modefile", "", "file with \"mode [primary]\" (written by the status page), read when it changes; a missing file = -mode / -primary")
	flag.Parse()
	v59Apply()
	if *modeFlag == "peer" {
		modeFollow = true
	} else if m, ok := parseMode(*modeFlag); ok {
		setMode(m, *primary-1, "-mode")
	} else {
		log.Fatalf("-mode: speed, smooth, redundant, backup or peer")
	}
	if *primary < 1 || *primary > 8 {
		log.Fatalf("-primary must be 1..8")
	}
	if *frame < 256 || *frame > 1400 {
		log.Fatalf("-frame must be 256..1400")
	}
	frameMax = *frame - fHdr
	maxQueueUs = *maxq * 1000
	if *allow != "" {
		allowTargets = map[string]bool{}
		for _, t := range strings.Split(*allow, ",") {
			allowTargets[strings.TrimSpace(t)] = true
		}
	}
	if *keyPath != "" {
		loadKey(*keyPath)
	}
	if *cpuProf != "" {
		f, err := os.Create(*cpuProf)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		go func() {
			<-sig
			pprof.StopCPUProfile()
			f.Close()
			os.Exit(0)
		}()
	}
	p := &peer{kick: make(chan struct{}, 1), buf: map[uint32][]byte{}, copies: map[uint32][]byte{}, skippedSet: map[uint32]bool{}, copySent: map[uint32]int64{},
		txRoom: make(chan struct{}, 1)}
	for i, spec := range strings.Split(*links, ",") {
		lp := strings.Split(spec, ">")
		if len(lp) != 2 {
			log.Fatalf("bad link %q", spec)
		}
		c, err := listenLink(lp[0])
		if err != nil {
			log.Fatal(err)
		}
		var pa *net.UDPAddr // "-" = unknown (server behind which the box sits at CGNAT): learned from authenticated packets
		if lp[1] != "-" {
			if pa, err = net.ResolveUDPAddr("udp", lp[1]); err != nil {
				log.Fatal(err)
			}
		} else if !trustSource() {
			log.Fatal("peer \"-\" (learn the address) needs -key, or -auth=off under IPsec")
		}
		sockBuffers(c, 4<<20)
		nl := &link{id: i + 1, conn: c, win: winInit, lastSend: mono(), lmap: map[uint32]lmapEnt{}}
		nl.derive()
		nl.txCond = sync.NewCond(&nl.txMu)
		if pa != nil {
			nl.peerA.Store(pa)
		}
		nl.learn = pa == nil
		p.links = append(p.links, nl)
	}
	t, err := openTun(*tunName)
	if err != nil {
		log.Fatalf("tun: %v", err)
	}
	p.tun = t
	p.applySeqBase() // before any goroutine: plain writes
	if reliable {
		p.started = true // both ends count from seqBase (0) – set before any packet can arrive
		p.rtoBackoff = 1
		go p.retransmitter()
	}
	for _, l := range p.links {
		go p.recvLoop(l)
		go p.txLoop(l)
	}
	go p.drainer()
	go p.deadWatch()
	if *proxySpec != "" {
		if !reliable {
			log.Fatal("-proxy needs -reliable")
		}
		for _, one := range strings.Split(*proxySpec, ";") { // several "listen>target" separated by ';'
			lt := strings.SplitN(one, ">", 2)
			if len(lt) != 2 {
				log.Fatalf("bad -proxy %q", one)
			}
			go p.proxyClient(lt[0], lt[1])
		}
	}
	p.tproxyStart()
	go p.gapWatch()
	go p.liveWatch()
	go p.releaseWatch()
	if *modeFilePath != "" {
		mf := &modeFile{path: *modeFilePath, nlinks: len(p.links), defMode: curMode(), defPrim: primaryIdx()}
		if modeFollow {
			mf.defMode = -1
		}
		go watchModeFile(mf)
	}
	go p.feedbackLoop()
	if statsEvery > 0 {
		go p.stats(statsEvery)
	}
	if *statusPath != "" {
		go p.statusWriter(*statusPath)
	}
	if *tracePath != "" {
		go p.trace(*tracePath)
	}
	log.Printf("sheaf %s key=%v auth=%s session=%08x ", version, haveKey, authStatus(), mySession)
	if authMode == authOff {
		log.Printf("auth: off - no datagram tags, the links must run under IPsec")
	}
	log.Printf("reliable=%v (inc %.2f dec %.2f): %d links, tun %s", reliable, incLimit, decLimit, len(p.links), *tunName)
	log.Printf("v59: %s", v59Summary())
	log.Printf("v60: %s", v60Summary())
	log.Printf("v61: %s", tunGROSummary())
	log.Printf("tproxy: %s", tproxySummary())
	p.sendLoop()
}

// sockBuffers: n bytes of socket buffer each way. SetReadBuffer is capped by net.core.rmem_max (176 kB on the OpenWrt
// box): with the CPU busy uploading, the downlink overflowed it - 9000 drops on the link sockets, a video stream
// to a LAN client lost 3.9 % with 0.5 s gaps. Running as root, the FORCE options ignore the cap.
func sockBuffers(c *net.UDPConn, n int) {
	c.SetReadBuffer(n)
	c.SetWriteBuffer(n)
	if rc, err := c.SyscallConn(); err == nil {
		rc.Control(func(fd uintptr) {
			unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, n)
			unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUFFORCE, n)
		})
	}
}

// fbLidMark: feedback ends with fbLidMark, link id (v55): the id of the link the copy was sent over. Older peers
// ignore the two extra bytes (they only check a minimum length).
const fbLidMark = 0x4c

// fbLinkID: the sender's link id from a feedback copy, 0 when it has none (v54 peer).
func fbLinkID(b []byte) int {
	if len(b) < 4 {
		return 0
	}
	off := 4 + 21*int(b[2])
	if len(b) >= off+5 && b[off] == 1 { // cumulative ack block (reliable)
		off += 5
	}
	if len(b) >= off+2 && b[off] == fbLidMark {
		return int(b[off+1])
	}
	return 0
}

// farIdx: index of this link's entry in a feedback from a far end with n links, -1 when it has none. The far end
// receives what this link sends on its link peerLid; until that id is known, the same position as here (v54).
func (l *link) farIdx(n int) int {
	j := int(l.peerLid.Load())
	if j == 0 {
		j = l.id
	}
	if j < 1 || j > n {
		return -1
	}
	return j - 1
}

// byFarID: the link the far end calls id; by position only while that link's far id is not known yet.
func (p *peer) byFarID(id int) *link {
	for _, l := range p.links {
		if int(l.peerLid.Load()) == id {
			return l
		}
	}
	if id >= 1 && id <= len(p.links) && p.links[id-1].peerLid.Load() == 0 {
		return p.links[id-1]
	}
	return nil
}

// seqBase: where the global and per-link numbering start (0). SHEAF_SEQBASE (test only, same value on both ends):
// start just below 2^32 so the wrap of every 32-bit counter happens within minutes instead of days, e.g.
// SHEAF_SEQBASE=4294767295 wraps after 200 000 packets (~260 MB).
var seqBase = func() uint32 {
	v, err := strconv.ParseUint(os.Getenv("SHEAF_SEQBASE"), 0, 32)
	if err != nil {
		return 0
	}
	return uint32(v)
}()

// applySeqBase: a fresh process counts from seqBase (resetPeer does the same for a new session).
func (p *peer) applySeqBase() {
	if seqBase == 0 {
		return
	}
	log.Printf("test: numbering starts at %d (SHEAF_SEQBASE), wraps after %d packets", seqBase, uint32(0)-seqBase)
	p.seq, p.cumAck, p.lastAck, p.next = seqBase, seqBase, seqBase, seqBase
	p.relFrom, p.rxTop = seqBase, seqBase
	for _, l := range p.links {
		l.lseq, l.lastSeq, l.rxLseq, l.hiLseq = seqBase, seqBase, seqBase, seqBase
	}
}
