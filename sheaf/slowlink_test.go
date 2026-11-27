// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

// v58: a link whose rate collapses while its queue is full must not hold the bundle head-of-line (slowlink.go).

import (
	"encoding/binary"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// slowLink: a link with a 35 ms base, 2 ms jitter and n packets of 1400 B in flight, sent at µs 0..n-1, delivering
// rate B/µs (both of the last two samples). Healthy round trip = 35 + max(100, 52.5) + 8 + 40 = 183 ms.
func slowLink(n int, rate float64) *link {
	l := &link{id: 2, win: winMin, minRtt: 35000, jit: 2000, srtt: 35000, rate: rate, rate2: rate, rateMax: 5,
		lmap: map[uint32]lmapEnt{}}
	for i := 0; i < n; i++ {
		l.hist = append(l.hist, sentPkt{lseq: uint32(i + 1), size: 1400, at: int64(i), seq: uint32(100 + i), ep: 0})
		l.inflight += 1400
	}
	l.derive()
	return l
}

func TestHealthyRtt(t *testing.T) {
	l := slowLink(0, 0)
	if h := l.healthyUs(); h != 183000 {
		t.Fatalf("healthy %v, want 183000", h)
	}
	l.srtt = 2e6 // a queue draining at a trickle
	if g := l.guardRttUs(); g != 2e6 {
		t.Fatalf("guard rtt %v of a link that is not stuck, want its srtt", g)
	}
	l.stuck = true
	if g := l.guardRttUs(); g != 183000 {
		t.Fatalf("guard rtt %v of a stuck link, want the healthy 183000", g)
	}
	l.srtt = 90000
	if g := l.guardRttUs(); g != 90000 {
		t.Fatalf("guard rtt %v, want the srtt 90000", g)
	}
}

// A link at 1 Mbit/s with 420 kB in flight for 400 ms is stuck; it still delivers 0.125 B/µs × 2 × 183 ms = 45.75 kB
// in time.
func TestStuckCheck(t *testing.T) {
	l := slowLink(300, 0.125)
	if st, keep := l.stuckCheck(400000); !st || keep != 45750 {
		t.Fatalf("stuck %v keep %v", st, keep)
	}
}

// Not stuck: a fast link with a deep queue, a young queue, an ordinary halving of the rate (672 kB at 20 Mbit/s
// drains in 269 ms < 2 × 183 ms, review of 0429080), one noisy low sample, no rate sample yet (new session), slowk 0.
func TestStuckCheckNotStuck(t *testing.T) {
	for _, tc := range []struct {
		name  string
		l     *link
		nowUs int64
	}{
		{"fast link", slowLink(300, 5), 400000},
		{"young queue", slowLink(300, 0.125), 150000},
		{"rate halved", slowLink(480, 2.5), 250000},
		{"one noisy sample", func() *link { l := slowLink(300, 0.125); l.rate2 = 5; return l }(), 400000},
		{"no sample yet", func() *link { l := slowLink(300, 0); l.rateMax = 0; return l }(), 400000},
	} {
		if st, _ := tc.l.stuckCheck(tc.nowUs); st {
			t.Errorf("%s: counted as stuck", tc.name)
		}
	}
	old := slowK
	slowK = 0
	defer func() { slowK = old }()
	if st, _ := slowLink(300, 0.125).stuckCheck(400000); st {
		t.Fatal("slowk 0 must switch it off")
	}
}

// rescueLate queues the packets beyond keep once; it skips (and does not mark) packets acknowledged meanwhile,
// without a copy, or sent again since (NACK, RTO, dead-link probe) – a dead link must still reinject those.
func TestRescueLateMarksOnlyQueued(t *testing.T) {
	p := regPeer(t)
	l := p.links[0]
	now := mono()
	for i := uint32(0); i < 10; i++ {
		at := now/1000 - 400000 + int64(i)
		seq := 10 + i
		l.hist = append(l.hist, sentPkt{lseq: i + 1, size: 1400, at: at, seq: seq})
		p.copies[seq] = []byte{byte(i)}
		p.copySent[seq] = at * 1000
	}
	p.cumAck = 12                                     // 10 (within keep anyway) and 11 acknowledged
	p.copySent[13] = now - int64(50*time.Millisecond) // 13 resent 50 ms ago by a NACK
	delete(p.copies, 14)                              // 14 has no copy
	if n := p.rescueLate(l, 1400, now); n != 6 || len(p.reinject) != 6 || p.rescued != 6 || p.rescuedB != 6*1400 {
		t.Fatalf("rescued %d, queued %d, counter %d / %d B", n, len(p.reinject), p.rescued, p.rescuedB)
	}
	for _, e := range l.hist {
		if want := e.seq == 12 || e.seq >= 15; e.rescued != want {
			t.Errorf("seq %d rescued %v, want %v", e.seq, e.rescued, want)
		}
	}
	if n := p.rescueLate(l, 1400, now+int64(time.Millisecond)); n != 0 {
		t.Fatalf("rescued %d packets a second time", n)
	}
	if r := appendUnrescued(nil, l.hist); len(r) != 4 || r[0].seq != 10 || r[3].seq != 14 {
		t.Fatalf("a dead link would reinject %d packets", len(r))
	}
}

func twoLinkPeer(t *testing.T) (*peer, *link, *link) {
	p := regPeer(t)
	l1 := p.links[0]
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	l2 := &link{id: 2, conn: c, win: winMax, lastSend: mono(), lmap: map[uint32]lmapEnt{}}
	l2.txCond = sync.NewCond(&l2.txMu)
	l2.peerA.Store(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})
	p.links = append(p.links, l2)
	for _, l := range p.links {
		l.minRtt, l.srtt, l.jit = 35000, 60000, 2000
		l.derive()
	}
	return p, l1, l2
}

// watchLink: a link as deadTick sees it: base 35 ms, n packets sent age ms ago, last ack ackAge ms ago, delivering
// rate B/µs. srtt 700 ms: dead only after 1 s without an ack.
func watchLink(p *peer, l *link, now int64, n int, age, ackAge int64, rate float64) {
	l.minRtt, l.srtt, l.jit, l.win = 35000, 700000, 2000, winMin
	l.rate, l.rate2, l.rateMax, l.progress = rate, rate, 5, now-ackAge*int64(time.Millisecond)
	l.hist, l.inflight = nil, 0
	for i := 0; i < n; i++ {
		at := now/1000 - age*1000
		seq := uint32(1000*l.id + i)
		l.hist = append(l.hist, sentPkt{lseq: uint32(i + 1), size: 1400, at: at, seq: seq})
		l.inflight += 1400
		p.copies[seq], p.copySent[seq] = []byte{1}, at*1000
	}
	l.derive()
}

// One link collapsed while the other acks: the stuck one is rescued, once (keep = 45.75 kB = 32 packets).
func TestDeadTickRescuesStuckLink(t *testing.T) {
	old := reliable
	reliable = true
	defer func() { reliable = old }()
	p, l1, l2 := twoLinkPeer(t)
	now := mono()
	watchLink(p, l1, now, 0, 0, 20, 5)
	watchLink(p, l2, now, 200, 400, 50, 0.125)
	var w deadState
	p.deadTick(&w, now)
	if !l2.stuck || l1.stuck || l2.stuckCount != 1 || p.rescued != 200-32 {
		t.Fatalf("stuck %v/%v count %d rescued %d", l1.stuck, l2.stuck, l2.stuckCount, p.rescued)
	}
	p.deadTick(&w, now+int64(10*time.Millisecond))
	if !l2.stuck || l2.stuckCount != 1 || p.rescued != 200-32 {
		t.Fatalf("second tick: stuck %v count %d rescued %d", l2.stuck, l2.stuckCount, p.rescued)
	}
}

// Acks stop on ALL links at once (CPU or GC stall, delayed feedback, the reverse path): a common cause, no rescue,
// no flapping (review of 0429080: 368 of 400 packets resent, stuckCount 1 -> 6 in 100 ms).
func TestDeadTickNoRescueWhenAllStall(t *testing.T) {
	old := reliable
	reliable = true
	defer func() { reliable = old }()
	p, l1, l2 := twoLinkPeer(t)
	now := mono()
	watchLink(p, l1, now, 200, 400, 400, 0.125)
	watchLink(p, l2, now, 200, 400, 400, 0.125)
	var w deadState
	for i := int64(0); i < 10; i++ {
		p.deadTick(&w, now+i*int64(10*time.Millisecond))
		if l1.stuck || l2.stuck || l1.stuckCount+l2.stuckCount != 0 || p.rescued != 0 || l1.dead || l2.dead {
			t.Fatalf("tick %d: stuck %v/%v count %d/%d rescued %d dead %v/%v", i, l1.stuck, l2.stuck,
				l1.stuckCount, l2.stuckCount, p.rescued, l1.dead, l2.dead)
		}
	}
}

// The still-missing report repairs a packet stuck on a collapsed link: that link's srtt (seconds, its queue) must not
// stretch the guard. A packet sent 50 ms ago is still on its way and is not resent; nor is one of a slow link that
// still delivers (not stuck: its srtt counts, as in v57).
func TestNackGuardIgnoresStuckLink(t *testing.T) {
	old, oldFix := reliable, fixGuard
	reliable, fixGuard = true, false // the v58 guard (v59 -fixguard=false); v59_test.go has the fixed one
	defer func() { reliable, fixGuard = old, oldFix }()
	p, _, l2 := twoLinkPeer(t)
	l2.srtt, l2.stuck = 3e6, true
	now := mono()
	p.copies[100], p.copySent[100] = []byte{1}, now-int64(400*time.Millisecond)
	p.copies[101], p.copySent[101] = []byte{2}, now-int64(50*time.Millisecond)
	m := []byte{magic, tNack, 0, 0}
	m = binary.BigEndian.AppendUint32(m, 100)
	m = binary.BigEndian.AppendUint32(m, 101)
	p.onNack(m)
	if len(p.reinject) != 1 || p.reinject[0].seq != 100 || p.missRtx != 1 {
		t.Fatalf("queued %d (missRtx %d), want only seq 100", len(p.reinject), p.missRtx)
	}
	p.onNack(m) // the report repeats (and comes over every link): no second resend within the guard
	if len(p.reinject) != 1 {
		t.Fatalf("resent twice: %d", len(p.reinject))
	}
	l2.stuck = false
	p.copySent[100] = now - int64(400*time.Millisecond)
	p.onNack(m)
	if len(p.reinject) != 1 {
		t.Fatalf("a packet of a slow, delivering link was resent: %d", len(p.reinject))
	}
}

// A link-gap NACK for a transmission that was already sent again (a rescue) does not send it a third time.
func TestNackRangeSkipsResent(t *testing.T) {
	old := reliable
	reliable = true
	defer func() { reliable = old }()
	p := regPeer(t)
	l := p.links[0]
	now := mono()
	at := now/1000 - 300000
	for i := uint32(1); i <= 2; i++ {
		l.lmap[i] = lmapEnt{seq: 50 + i, at: at}
		p.copies[50+i], p.copySent[50+i] = []byte{1}, at*1000
	}
	p.copySent[51] = now - int64(20*time.Millisecond) // rescued 20 ms ago
	p.onNackRange([]byte{magic, tNackR, 1, 0, 0, 0, 0, 1, 0, 2})
	if len(p.reinject) != 1 || p.reinject[0].seq != 52 {
		t.Fatalf("queued %d", len(p.reinject))
	}
}

// pick: no new data on a stuck link.
func TestPickSkipsStuckLink(t *testing.T) {
	p, l1, l2 := twoLinkPeer(t)
	l1.relOwd, l2.relOwd = 50000, 0 // l2 would arrive first
	l1.derive()
	l2.derive()
	now := mono()
	l1.lastSend, l2.lastSend = now, now
	if p.pick(1400, true, now) != l2 {
		t.Fatal("want l2 while it is fine")
	}
	l2.stuck = true
	if p.pick(1400, true, now) != l1 {
		t.Fatal("a stuck link got new data")
	}
	// a starving live link with its window full gets no probe behind its queue (it did until v57)
	l2.stuck, l2.win, l2.inflight = false, winMin, int(winMin)
	l2.derive()
	l2.lastSend, l2.progress = now-int64(time.Second), now // acks still come: not dead
	l2.hist = []sentPkt{{seq: 1, size: int(winMin)}}
	if p.pick(1400, true, now) != l1 {
		t.Fatal("probe behind a full queue")
	}
	l2.hist, l2.inflight = nil, 0 // idle: the probe keeps it measured
	if p.pick(1400, true, now) != l2 {
		t.Fatal("no probe for an idle starving link")
	}
}

func rxPeer(t *testing.T) *peer {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return &peer{buf: map[uint32][]byte{}, skippedSet: map[uint32]bool{}, tun: f, started: true}
}

// The receiver reports holes although a collapsed link delivers one packet of the hole every 10 ms: until v57 each of
// them restarted the "no progress" clock and no report was ever sent (real LTE, 25 Nov).
func TestMissingReportDespiteTrickle(t *testing.T) {
	p := rxPeer(t)
	p.lastRxData.Store(1000)
	for s := uint32(100); s < 200; s++ { // the fast links' packets; 0..99 sit in the collapsed link's queue
		p.deliver(s, ipPkt(6, byte(s)))
	}
	if p.missingReport(1100) != nil {
		t.Fatal("report after 100 ms")
	}
	for s := uint32(0); s < 30; s++ { // the trickle: one packet of the hole every 10 ms
		p.lastRxData.Store(1000 + 10*s)
		p.deliver(s, ipPkt(6, byte(s)))
	}
	if p.next != 30 {
		t.Fatalf("next %d", p.next)
	}
	if w := p.holeWaitMs(1290); w != 290 {
		t.Fatalf("oldest wait %d ms, want 290", w)
	}
	m := p.missingReport(1290)
	if m == nil || len(m) != 4+4*70 || binary.BigEndian.Uint32(m[4:]) != 30 {
		t.Fatalf("report %d B", len(m))
	}
	// the hole closes: nothing waits, no report
	p.lastRxData.Store(1300)
	for s := uint32(30); s < 100; s++ {
		p.deliver(s, ipPkt(6, byte(s)))
	}
	if len(p.buf) != 0 || len(p.waitQ) != 0 || p.missingReport(1600) != nil {
		t.Fatalf("buf %d marks %d after the hole closed", len(p.buf), len(p.waitQ))
	}
}

// Normal reordering (a packet overtaken by a few ms) never reaches the report, and the marks follow the oldest
// waiting packet, not the newest.
func TestHoleWaitFollowsOldest(t *testing.T) {
	p := rxPeer(t)
	p.lastRxData.Store(5000)
	p.deliver(1, ipPkt(6, 1)) // 0 missing
	p.lastRxData.Store(5020)
	p.deliver(3, ipPkt(6, 3)) // 2 missing
	if w := p.holeWaitMs(5030); w != 30 {
		t.Fatalf("wait %d, want 30 (packet 1)", w)
	}
	p.deliver(0, ipPkt(6, 0)) // 0 and 1 go out, 3 waits since 5020
	if w := p.holeWaitMs(5030); w != 10 {
		t.Fatalf("wait %d, want 10 (packet 3)", w)
	}
	p.deliver(2, ipPkt(6, 2))
	if w := p.holeWaitMs(5030); w != 0 || len(p.waitQ) != 0 {
		t.Fatalf("wait %d marks %d with nothing buffered", w, len(p.waitQ))
	}
}

// The base delay is not re-measured from our own queue: a link whose rate collapsed has its window at the minimum
// too, but 800 kB in flight; its srtt is that queue, not a slower cell (real LTE: base 35 ms -> 2659 ms).
func TestNoRebaseFromOwnQueue(t *testing.T) {
	for _, tc := range []struct {
		inflight int
		rebase   bool
	}{{600, false}, {4, true}} { // packets in flight: a stuck queue / a slower cell with an empty queue
		p := regPeer(t)
		l := p.links[0]
		// base 1 ms, srtt 1 s; the window has been minimal and the rtt high for 1 s already (no waiting in the test)
		l.minRtt, l.srtt, l.win, l.baseAt, l.slowSince = 1000, 1e6, winMin, time.Now(), time.Now().Add(-time.Second)
		for i := 0; i < tc.inflight+1; i++ {
			l.hist = append(l.hist, sentPkt{lseq: uint32(i + 1), size: 1400, at: 1}) // sent at µs 1: rtt = process age
			l.inflight += 1400
		}
		b := []byte{magic, tFeedback, 1, 0, 1}
		b = binary.BigEndian.AppendUint32(b, 1) // acknowledges linkSeq 1
		b = binary.BigEndian.AppendUint64(b, 0)
		b = binary.BigEndian.AppendUint64(b, 1000)
		p.onFeedback(b)
		l.mu.Lock()
		rebased := l.minRtt > 1e5
		l.mu.Unlock()
		if rebased != tc.rebase {
			t.Fatalf("%d packets in flight: base re-measured = %v, want %v", tc.inflight, rebased, tc.rebase)
		}
	}
}

// -slowk 0 is v57: the old missing-report clock (time since the last progress).
func TestSlowK0MissingReportClock(t *testing.T) {
	old := slowK
	slowK = 0
	defer func() { slowK = old }()
	p := rxPeer(t)
	p.deliver(5, ipPkt(6, 5))
	p.rxMu.Lock()
	p.gapSince = time.Now().Add(-300 * time.Millisecond)
	p.rxMu.Unlock()
	if p.missingReport(0) == nil { // the ms clock says 0 ms: only the v57 clock can report
		t.Fatal("no report with the v57 clock")
	}
}
