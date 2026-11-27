// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

// v59 (v59.go): pacing in pick, repair time fixed at send time, drain hygiene, bounded receiver wait, rescue switch.

import (
	"encoding/binary"
	"testing"
	"time"
)

// setFlags sets the v59 switches for one test and restores them afterwards.
func setFlags(t *testing.T, gain float64, pick bool, capB int32) {
	og, op, oc := paceGain, pacePick, txqCapB
	paceGain, pacePick, txqCapB = gain, pick, capB
	t.Cleanup(func() { paceGain, pacePick, txqCapB = og, op, oc })
}

func setReliable(t *testing.T, fix bool) {
	or, of := reliable, fixGuard
	reliable, fixGuard = true, fix
	t.Cleanup(func() { reliable, fixGuard = or, of })
}

// P1: the pace comes from the delivery maximum or win/srtt, whichever is larger – a window cut does not lower it.
func TestPaceRate(t *testing.T) {
	setFlags(t, 1.25, true, 45000)
	l := &link{win: 200000, srtt: 40000, rateMax: 5} // 200 kB / 40 ms = 5 B/µs = 40 Mbit/s
	l.derive()
	if l.paceBps != 6250000 {
		t.Fatalf("pace %d B/s, want 6250000", l.paceBps)
	}
	l.win = winMin // a drain: the window drops to 6 kB, the link still delivers 40 Mbit/s
	l.derive()
	if l.paceBps != 6250000 {
		t.Fatalf("pace after a window cut %d B/s, want 6250000 (v57 would pace at the 1 Mbit/s floor)", l.paceBps)
	}
	l.win, l.rateMax = 400000, 2 // ramp-up: the window is ahead of the delivery samples
	l.derive()
	if l.paceBps != 12500000 {
		t.Fatalf("pace %d B/s, want 12500000 from win/srtt", l.paceBps)
	}
	pacePick = false // v57: no pacing in pick
	l.derive()
	if l.paceBps != 0 {
		t.Fatal("-pacepick=false must leave pick unpaced")
	}
}

// P1: a link without tokens is no candidate; pickW says how long until it is.
func TestPickPaceGate(t *testing.T) {
	setFlags(t, 1.25, true, 45000)
	paceLook = int64(2 * time.Millisecond)
	defer func() { paceLook = 0 }()
	p, l1, l2 := twoLinkPeer(t)
	l1.relOwd, l2.relOwd = 50000, 0 // l2 arrives first
	l2.win, l2.srtt, l2.rateMax = 60000, 60000, 1
	l1.derive()
	l2.derive() // 1.25 × 1 B/µs = 1.25e6 B/s, bucket 8400 B
	now := mono()
	l1.lastSend, l2.lastSend = now, now
	n := 0
	for ; n < 10; n++ {
		if p.pick(1424, false, now) != l2 {
			break
		}
		l2.mu.Lock()
		l2.spendPace(1424, now)
		l2.inflight += 1424
		l2.mu.Unlock()
	}
	if n != 7 { // 5 from the full 8400 B bucket, 2 more on credit (their tokens within 2 ms)
		t.Fatalf("l2 took %d packets, want 5 + 2 on credit", n)
	}
	if p.pick(1424, false, now) != l1 {
		t.Fatal("a link beyond its 2 ms of credit must give way")
	}
	l2.paceTok = 1280 // tokens 115 µs away: l2 still arrives ~45 ms before l1, so it takes the packet on credit
	if l, wait, _ := p.pickW(1424, false, now); l != l2 || wait != 0 {
		t.Fatalf("pickW %v wait %d, want l2 on credit", l, wait)
	}
	l1.relOwd = 0 // l1 as near as l2 and with tokens: it beats l2's wait
	l1.derive()
	if p.pick(1424, false, now) != l1 {
		t.Fatal("a link free now must beat an equally near one that has to wait")
	}
	l1.relOwd = 50000
	l1.derive()
	l2.paceTok = -100000 // tokens 81 ms away: no candidate, the other link takes it
	if p.pick(1424, false, now) != l1 {
		t.Fatal("a link out of tokens for longer than paceLook must give way")
	}
	l1.stuck = true
	if l, wait, _ := p.pickW(1424, false, now); l != nil || wait != (1424+100000)*800+1 {
		t.Fatalf("pickW %v wait %d", l, wait)
	}
}

// P1 (review): several links, paced, a burst from the TUN: packets go where they arrive first, not to whichever link
// still has tokens – reordering stays as without pacing.
func TestPacedBurstKeepsOrder(t *testing.T) {
	setFlags(t, 1.25, true, 45000)
	paceLook = int64(2 * time.Millisecond)
	defer func() { paceLook = 0 }()
	p, l1, l2 := twoLinkPeer(t)
	l1.relOwd, l2.relOwd = 80000, 0 // l1 80 ms slower
	for _, l := range []*link{l1, l2} {
		l.win, l.srtt, l.rateMax = 1e6, 40000, 5 // 31.25e6 B/s, bucket 62.5 kB
		l.derive()
	}
	now := mono()
	l1.lastSend, l2.lastSend = now, now
	toL1 := 0
	at := now
	// 80 packets (114 kB) at once: the bucket (62.5 kB) empties after 43, the rest goes on credit (2 ms of the pace);
	// then one packet per 46 µs (the pace)
	for seq := uint32(0); seq < 200; seq++ {
		if seq >= 80 {
			at += 46000
		}
		l, _, _ := p.pickW(1400+hdrLen, false, at)
		if l == nil {
			t.Fatalf("seq %d: no link", seq)
		}
		if l == l1 {
			toL1++
		}
		p.sendOn(l, seq, make([]byte, 1400), 0, at, nil)
		l.txqB.Store(0) // txLoop sent it
	}
	if toL1 != 0 {
		t.Fatalf("%d of 200 packets spilled onto the link 80 ms slower", toL1)
	}
}

// P1: a full transmit queue makes the link no candidate; an empty one always takes a packet.
func TestPickTxqCap(t *testing.T) {
	setFlags(t, 0, true, 45000)
	p, l1, l2 := twoLinkPeer(t)
	l1.relOwd, l2.relOwd = 50000, 0
	l1.derive()
	l2.derive()
	now := mono()
	l1.lastSend, l2.lastSend = now, now
	l2.txqB.Store(44000)
	if p.pick(1424, false, now) != l1 {
		t.Fatal("a link with a full transmit queue took a packet")
	}
	l1.stuck = true
	if l, wait, txq := p.pickW(1424, false, now); l != nil || wait != txqWaitNs || !txq {
		t.Fatalf("pickW %v %d %v", l, wait, txq)
	}
	l2.txqB.Store(0)
	txqCapB = 1000 // smaller than a packet: an empty queue still takes it
	if p.pick(1424, false, now) != l2 {
		t.Fatal("an empty queue must take a packet")
	}
}

// fill: pick + sendOn at one moment (no txLoop running) until pick refuses; returns the bytes queued.
func fill(p *peer, now int64) int {
	for seq := uint32(0); seq < 2000; seq++ {
		l := p.pick(1400+hdrLen, false, now)
		if l == nil {
			break
		}
		p.sendOn(l, seq, make([]byte, 1400), 0, now, nil)
	}
	return int(p.links[0].txqB.Load())
}

// P1, the cause of the stalls A/B/C: pick hands a link no more than its transmit queue cap, so a window cut cannot
// leave hundreds of kB behind a pacer. v57 queues the whole window (here 1 MB).
func TestNoStrandedQueue(t *testing.T) {
	setup := func() *peer {
		p := regPeer(t)
		l := p.links[0]
		l.win, l.srtt, l.minRtt, l.rateMax = 1e6, 40000, 35000, 5
		l.lastSend = mono()
		l.derive()
		return p
	}
	setFlags(t, 1.25, true, 45000)
	if q := fill(setup(), mono()); q > 45000 || q < 40000 {
		t.Fatalf("v59: %d B queued, want one GSO batch (<= 45000)", q)
	}
	pacePick, txqCapB = false, 0
	if q := fill(setup(), mono()); q < 900000 {
		t.Fatalf("v57: %d B queued, expected the whole window", q)
	}
}

// P0 + P1: txLoop accounts what it hands to the kernel; with the pace in pick it sends at once – v57's pacer in
// txLoop drained the same queue at the 1 Mbit/s floor after a window cut (pairs every 21.8 ms).
func TestTxLoopDrainsAtOnce(t *testing.T) {
	for _, pick := range []bool{true, false} {
		setFlags(t, 1.25, pick, 0)
		p := regPeer(t)
		l := p.links[0]
		l.win, l.srtt, l.minRtt = 300000, 40000, 35000
		l.derive()
		now := mono()
		for seq := uint32(0); seq < 100; seq++ {
			p.sendOn(l, seq, make([]byte, 1400), 0, now, nil)
		}
		if q := l.txqB.Load(); q != 100*(1400+hdrLen) {
			t.Fatalf("txq %d B", q)
		}
		l.mu.Lock()
		l.win, l.srtt = winMin, 1e6 // the window drops (drain) while the queue is full
		l.derive()
		l.mu.Unlock()
		go p.txLoop(l)
		time.Sleep(300 * time.Millisecond)
		sent := l.txSentN.Load()
		if pick && (sent != 100 || l.txqB.Load() != 0) {
			t.Fatalf("v59: %d of 100 sent in 300 ms, txq %d B", sent, l.txqB.Load())
		}
		if !pick && sent > 60 {
			t.Fatalf("v57 pacer: %d of 100 sent in 300 ms, expected the 1 Mbit/s floor", sent)
		}
		for end := time.Now().Add(3 * time.Second); l.txSentN.Load() < 100 && time.Now().Before(end); {
			time.Sleep(20 * time.Millisecond) // let the v57 pacer finish before the flags change (race detector)
		}
	}
}

// P2: the repair time is fixed when the packet leaves, from its link at that moment.
func TestRepairSpan(t *testing.T) {
	l := &link{minRtt: 35000, srtt: 60000, jit: 2000}
	if s := l.repairSpanUs(); s != 263000 { // max(60, 35 + 100) + 100 + 8 + 20 ms
		t.Fatalf("span %v, want 263000", s)
	}
	l.srtt = 300000 // a deep but legitimate queue
	if s := l.repairSpanUs(); s != 428000 {
		t.Fatalf("span %v, want 428000", s)
	}
	if s := (&link{}).repairSpanUs(); s != 1e6 {
		t.Fatalf("unmeasured link: span %v, want the 1 s fallback", s)
	}
}

// P2: a collapse of the link AFTER the packet left does not protect it from repair (v57: 1.5 × the slowest srtt,
// which is that collapsed link's queue); a packet still within its time and one behind a legitimate deep queue are
// not resent; a repeated report does not resend twice.
func TestFixedGuardNack(t *testing.T) {
	setReliable(t, true)
	p := regPeer(t)
	l := p.links[0]
	l.minRtt, l.srtt, l.jit = 35000, 60000, 2000
	l.derive() // as onFeedback does after every change (v62: sendOn takes the repair time from derive's copy)
	now := mono()
	send := func(seq uint32, ago time.Duration) {
		p.copies[seq], p.copySent[seq] = []byte{byte(seq)}, 0
		p.sendOn(l, seq, []byte{byte(seq)}, 0, now-int64(ago), nil)
	}
	send(100, 300*time.Millisecond) // due 263 ms after it left: late
	send(101, 200*time.Millisecond) // within its time
	l.srtt = 300000
	l.derive()
	send(102, 300*time.Millisecond) // sent behind a 300 ms queue: due after 428 ms
	l.srtt = 3e6                    // the link collapses now
	m := []byte{magic, tNack, 0, 0}
	for _, s := range []uint32{100, 101, 102} {
		m = binary.BigEndian.AppendUint32(m, s)
	}
	p.onNack(m)
	if len(p.reinject) != 1 || p.reinject[0].seq != 100 {
		t.Fatalf("queued %d resends, want only seq 100", len(p.reinject))
	}
	p.onNack(m)
	if len(p.reinject) != 1 {
		t.Fatalf("repeated report: %d resends", len(p.reinject))
	}
}

// P2: the head's RTO is its own repair time with the 1 s fallback – not 3 × the srtt of a collapsed link (9 s here).
func TestRtoFromHeadDue(t *testing.T) {
	for _, fix := range []bool{true, false} {
		setReliable(t, fix)
		p := regPeer(t)
		l := p.links[0]
		l.minRtt, l.srtt, l.jit = 35000, 3e6, 2000 // collapsed, not (yet) marked stuck
		now, nowM := time.Now(), mono()
		sent := nowM - int64(1100*time.Millisecond)
		p.cumAck, p.lastAck, p.ackMovedAt, p.rtoBackoff = 100, 100, now.Add(-1100*time.Millisecond), 1
		p.copies[100], p.copySent[100] = []byte{1}, sent
		p.setDue(100, sent+int64(163*time.Millisecond))
		p.rtxTick(now, nowM)
		if want := map[bool]int{true: 1, false: 0}[fix]; len(p.reinject) != want || p.rtx != int64(want) {
			t.Fatalf("fixguard %v: %d resends, want %d", fix, len(p.reinject), want)
		}
	}
}

// P3: no drain with a queue of our own, a stuck link or an srtt above the healthy round trip; drain window ½ BDP.
func TestDrainHygiene(t *testing.T) {
	l := &link{minRtt: 35000, srtt: 60000, jit: 2000, rateAvg: 5}
	if !l.drainOK(0) {
		t.Fatal("a quiet link may drain")
	}
	l.txqB.Store(1400)
	if l.drainOK(0) {
		t.Fatal("drain with packets in the transmit queue")
	}
	l.txqB.Store(0)
	l.stuck = true
	if l.drainOK(0) {
		t.Fatal("drain of a stuck link")
	}
	l.stuck, l.srtt = false, 500000
	if l.drainOK(0) {
		t.Fatal("drain with the srtt above the healthy round trip (183 ms)")
	}
	if !l.drainOK(3*drainEvery + time.Second) {
		t.Fatal("a high srtt must postpone the drain at most 3 × drainEvery (the base itself may have risen)")
	}
	if b := drainBase([]float64{60000, 30000, 61000, 40000}); b != 35000 {
		t.Fatalf("drain base %v, want the mean of the lower half 35000", b)
	}
	if b := drainBase([]float64{50000}); b != 50000 {
		t.Fatalf("drain base of one sample %v", b)
	}
	if w := l.drainTarget(); w != 87500 {
		t.Fatalf("drain window %v, want ½ × 5 B/µs × 35 ms", w)
	}
	old := drainFix
	drainFix = false
	defer func() { drainFix = old }()
	if !l.drainOK(0) || l.drainTarget() != winMin {
		t.Fatal("-drainfix=false is v57")
	}
}

// feedbackFor: a one-link feedback acknowledging linkSeq hi, held age µs at the peer.
func feedbackFor(hi uint32, age int64) []byte {
	b := []byte{magic, tFeedback, 1, 0, 1}
	b = binary.BigEndian.AppendUint32(b, hi)
	b = binary.BigEndian.AppendUint64(b, uint64(age))
	return binary.BigEndian.AppendUint64(b, 1000)
}

// P3: after the drain the base is the mean of the lower half of the samples and the window returns ×2 per round trip.
func TestDrainEndMinAndStepwiseReturn(t *testing.T) {
	for _, fix := range []bool{true, false} {
		old := drainFix
		drainFix = fix
		p := regPeer(t)
		l := p.links[0]
		l.minRtt, l.srtt, l.win, l.drainWin = 20000, 40000, winMin, winMin
		l.draining, l.drainStart, l.drainLowAt = true, time.Now(), startTime
		l.drainN, l.drainSum, l.drainS, l.drainSaved = 3, 150000, [4]float64{30000, 60000, 60000}, 500000
		l.baseAt = time.Now()
		p.drainBusy.Store(true)
		for i := 0; i < 3; i++ {
			l.hist = append(l.hist, sentPkt{lseq: uint32(i + 1), size: 1400, at: nowUs() - 40000})
			l.inflight += 1400
		}
		p.onFeedback(feedbackFor(1, 0))
		if l.draining || p.drainBusy.Load() {
			t.Fatal("the drain did not end")
		}
		if fix {
			if l.minRtt < 34900 || l.minRtt > 35500 || l.win > 2*winMin || l.winTarget != 500000 { // mean(30, ~40) ms
				t.Fatalf("drainfix: base %v win %v target %v", l.minRtt, l.win, l.winTarget)
			}
			l.adjUs = 0 // a round trip later
			w := l.win
			p.onFeedback(feedbackFor(2, 0))
			if l.win != 2*w {
				t.Fatalf("window %v after a round trip, want %v", l.win, 2*w)
			}
		} else if l.minRtt < 47000 || l.minRtt > 49000 || l.win != 500000 {
			t.Fatalf("v57: base %v (want the average ~47.5 ms) win %v", l.minRtt, l.win)
		}
		drainFix = old
	}
}

// P3: a cut abandons the return; reaching the target ends it.
func TestWinTargetReached(t *testing.T) {
	p := regPeer(t)
	l := p.links[0]
	l.minRtt, l.srtt, l.win, l.winTarget, l.baseAt = 40000, 40000, 300000, 400000, time.Now()
	l.hist = []sentPkt{{lseq: 1, size: 1400, at: nowUs() - 40000}}
	l.inflight = 1400
	p.onFeedback(feedbackFor(1, 0))
	if l.win != 400000 || l.winTarget != 0 {
		t.Fatalf("win %v target %v", l.win, l.winTarget)
	}
}

// P4: buffered IP packets past a hole go out by their deadline in sequence order; the hole is delivered late when it
// arrives and the in-order point moves over the released ones; a proxy frame never overtakes.
func TestIPReleaseByDeadline(t *testing.T) {
	setReliable(t, true)
	old := ipRelease
	ipRelease = 100 * time.Millisecond
	defer func() { ipRelease = old }()
	w, got := regCollector(t)
	p := rxPeer(t)
	p.tun = w
	frame := []byte{fData, 0, 0, 0, 0, 0xff, 0xff, 0, 0, 0} // its length field is too long: out() ignores it
	for s := uint32(1); s <= 5; s++ {
		pl := ipPkt(6, byte(s))
		if s == 3 {
			pl = getRx(len(frame))
			copy(pl, frame)
		}
		p.deliverS(s, 0, false, pl, 1000+int64(s)) // seq 0 is missing
	}
	p.rxMu.Lock()
	n := p.releaseDue(1004) // seqs 1, 2, 4 were sent by 1004; 3 is a proxy frame; 5 later
	p.rxMu.Unlock()
	if n != 3 || p.ipReleased != 3 || p.next != 0 {
		t.Fatalf("released %d (counter %d), next %d", n, p.ipReleased, p.next)
	}
	if p.buf[3] == nil || p.buf[5] == nil {
		t.Fatal("the frame or the packet not yet due went out")
	}
	p.deliverS(0, 0, false, ipPkt(6, 0), 999) // the repair of the hole
	if out := regDrain(got, 100*time.Millisecond); string(out) != string([]byte{1, 2, 4, 0, 5}) {
		t.Fatalf("delivered %v, want [1 2 4 0 5]", out)
	}
	if p.next != 6 || len(p.buf) != 0 {
		t.Fatalf("next %d buf %d", p.next, len(p.buf))
	}
}

// P4: the deadline counts a collapsed link's delay only up to its minimum + the allowed queue.
func TestReleaseSpan(t *testing.T) {
	old := ipRelease
	ipRelease = 100 * time.Millisecond
	defer func() { ipRelease = old }()
	p := regPeer(t)
	l := p.links[0]
	nowM := mono()
	l.haveOwd, l.lastRx = true, nowM
	l.noteOwd(20000, nowM)
	l.owdEwma, l.owdJit = 3e6, 1000 // its queue: 3 s
	if s, ok := p.releaseSpanUs(); !ok || s != 20000+300000+4000+100000 {
		t.Fatalf("span %d %v", s, ok)
	}
	l.owdEwma = 250000 // the far end queues 230 ms (its window cap allows that much): its delay as it is
	oq := maxQueueUs
	maxQueueUs = 40000 // the receiver's own -maxq (hub: 40 ms) says nothing about the far end's queue (review)
	defer func() { maxQueueUs = oq }()
	if s, _ := p.releaseSpanUs(); s != 250000+4000+100000 {
		t.Fatalf("span %d", s)
	}
	l.lastRx = nowM - int64(2*time.Second) // silent: not counted
	if _, ok := p.releaseSpanUs(); ok {
		t.Fatal("a silent link counted")
	}
}

// P4 off (default for the proxy-only and non-reliable cases): nothing noted, nothing released.
func TestIPReleaseOff(t *testing.T) {
	setReliable(t, true)
	p := rxPeer(t)
	p.deliverS(1, 0, false, ipPkt(6, 1), 1000)
	p.rxMu.Lock()
	defer p.rxMu.Unlock()
	if p.releaseDue(1<<40) != 0 || p.buf[1] == nil {
		t.Fatal("released with -iprelease 0")
	}
}

// P5 off: a stuck link gets no new data, but nothing is sent again over the others.
func TestRescueSwitch(t *testing.T) {
	setReliable(t, true)
	old := rescue
	rescue = false
	defer func() { rescue = old }()
	p, l1, l2 := twoLinkPeer(t)
	now := mono()
	watchLink(p, l1, now, 0, 0, 20, 5)
	watchLink(p, l2, now, 200, 400, 50, 0.125)
	var w deadState
	p.deadTick(&w, now)
	if !l2.stuck || p.rescued != 0 || len(p.reinject) != 0 {
		t.Fatalf("stuck %v rescued %d queued %d", l2.stuck, p.rescued, len(p.reinject))
	}
}

// -v57 switches every change off.
func TestCompatV57(t *testing.T) {
	saved := []any{pacePick, txqCapB, fixGuard, rescue, drainFix, ipRelease, slowK, compatV57, txqCapKB}
	defer func(sf float64) { slowFrac = sf }(slowFrac)
	defer func() {
		pacePick, txqCapB, fixGuard, rescue = saved[0].(bool), saved[1].(int32), saved[2].(bool), saved[3].(bool)
		drainFix, ipRelease, slowK, compatV57, txqCapKB = saved[4].(bool), saved[5].(time.Duration), saved[6].(float64), saved[7].(bool), saved[8].(int)
	}()
	compatV57, ipRelease = true, time.Second
	v59Apply()
	if pacePick || txqCapB != 0 || fixGuard || rescue || drainFix || ipRelease != 0 || v58() || slowFrac != 0 {
		t.Fatalf("-v57 left something on: %s", v59Summary())
	}
	// and behaves so: pick queues the whole window (the pacer sits in txLoop), no repair due times, drain to 6 kB
	og := paceGain
	paceGain = 1.25
	defer func() { paceGain = og }()
	p := regPeer(t)
	l := p.links[0]
	l.win, l.srtt, l.minRtt, l.rateMax, l.rateAvg, l.lastSend = 1e6, 40000, 35000, 5, 5, mono()
	l.derive()
	if l.paceBps != 0 || fill(p, mono()) < 900000 {
		t.Fatalf("-v57: pick paced (%d B/s) or the queue was capped", l.paceBps)
	}
	if p.due != nil || l.drainTarget() != winMin || !l.drainOK(0) {
		t.Fatal("-v57: due times kept or drain changed")
	}
}

// P4 (review): the repair of a hole that is not the in-order point, arriving after the cursor passed it, is still
// released by its deadline; a packet that arrived as a resend (later send time) goes out with the due ones after it,
// in sequence order (holding it back would reorder its flow).
func TestIPReleaseCursorAndResend(t *testing.T) {
	setReliable(t, true)
	old := ipRelease
	ipRelease = 100 * time.Millisecond
	defer func() { ipRelease = old }()
	w, got := regCollector(t)
	p := rxPeer(t)
	p.tun = w
	p.next = 10
	p.deliverS(11, 0, false, ipPkt(6, 11), 1011) // 10 and 12 are holes
	p.deliverS(13, 0, false, ipPkt(6, 13), 1013)
	p.rxMu.Lock()
	n := p.releaseDue(2000)
	p.rxMu.Unlock()
	if n != 2 {
		t.Fatalf("released %d, want 11 and 13", n)
	}
	p.deliverS(12, 0, false, ipPkt(6, 12), 1012) // the repair of 12, sent long ago
	p.deliverS(14, 0, false, ipPkt(6, 14), 5000) // arrived as a resend: sent later
	p.deliverS(15, 0, false, ipPkt(6, 15), 1015)
	p.rxMu.Lock()
	n = p.releaseDue(2000)
	p.rxMu.Unlock()
	if n != 3 {
		t.Fatalf("released %d, want 12, 14 (a resend, sent later) and 15 – in order", n)
	}
	if out := regDrain(got, 100*time.Millisecond); string(out) != string([]byte{11, 13, 12, 14, 15}) {
		t.Fatalf("delivered %v", out)
	}
}

// P2 (review): a head whose repair time is longer than the 1 s fallback waits for it.
func TestRtoLongDue(t *testing.T) {
	setReliable(t, true)
	p := regPeer(t)
	now, nowM := time.Now(), mono()
	sent := nowM - int64(1100*time.Millisecond)
	p.cumAck, p.lastAck, p.ackMovedAt, p.rtoBackoff = 100, 100, now.Add(-1100*time.Millisecond), 1
	p.copies[100], p.copySent[100] = []byte{1}, sent
	p.setDue(100, sent+int64(1500*time.Millisecond))
	p.rtxTick(now, nowM)
	if len(p.reinject) != 0 {
		t.Fatal("resent before its own repair time (1.5 s)")
	}
	p.rtxTick(now.Add(500*time.Millisecond), nowM+int64(500*time.Millisecond))
	if len(p.reinject) != 1 {
		t.Fatal("not resent after its repair time")
	}
}

// P4 (review): a hole longer than missScan packets (~1-2 s at 50-100 Mbit/s, the stall range): the release keeps
// going past next + missScan as the deadline moves on.
func TestIPReleaseBeyondMissScan(t *testing.T) {
	setReliable(t, true)
	old := ipRelease
	ipRelease = 100 * time.Millisecond
	defer func() { ipRelease = old }()
	p := rxPeer(t)
	for s := uint32(1); s <= 12000; s++ { // seq 0 is missing
		p.deliverS(s, 0, false, ipPkt(6, byte(s)), 1000+int64(s))
	}
	p.rxMu.Lock()
	defer p.rxMu.Unlock()
	total := 0
	for lim := int64(1000); lim <= 14000; lim += 500 { // the deadline moves on tick by tick
		total += p.releaseDue(lim)
	}
	if total != 12000 {
		t.Fatalf("released %d of 12000 (relFrom %d next %d)", total, p.relFrom, p.next)
	}
}

// P1, default -pacelook 0 (the lab's choice): a link without tokens gives way at once.
func TestPickPaceGateNoCredit(t *testing.T) {
	setFlags(t, 1.25, true, 45000)
	p, l1, l2 := twoLinkPeer(t)
	l1.relOwd, l2.relOwd = 50000, 0
	l2.win, l2.srtt, l2.rateMax = 60000, 60000, 1
	l1.derive()
	l2.derive()
	now := mono()
	l1.lastSend, l2.lastSend = now, now
	n := 0
	for ; n < 10 && p.pick(1424, false, now) == l2; n++ {
		l2.mu.Lock()
		l2.spendPace(1424, now)
		l2.inflight += 1424
		l2.mu.Unlock()
	}
	if n != 5 || p.pick(1424, false, now) != l1 {
		t.Fatalf("l2 took %d packets from its 8400 B bucket (want 5), then the other link must take over", n)
	}
	l1.stuck = true
	l, wait, txq := p.pickW(1424, false, now)
	if want := int64((1424-(8400-5*1424))*1e9/1250000 + 1); l != nil || wait != want || txq {
		t.Fatalf("pickW %v wait %d txq %v, want nil %d false", l, wait, txq, want)
	}
}
