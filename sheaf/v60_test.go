// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

// v60 (v60.go): demotion of a persistently slow link with trials, control sends that never wait.

import (
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// slowPeer: l1 delivers 25 Mbit/s, l2 is busy (window full) at rate2 B/µs.
func slowPeer(t *testing.T, rate2 float64) (*peer, *link, *link) {
	p, l1, l2 := twoLinkPeer(t)
	setMode(modeSpeed, 0, "test")
	sf, sa := slowFrac, slowAfter
	slowFrac, slowAfter = 0.03, 10*time.Second
	t.Cleanup(func() { slowFrac, slowAfter = sf, sa })
	l1.rateMax = 3.125 // 25 Mbit/s
	l2.win, l2.rateMax = 24000, rate2
	l2.derive()
	return p, l1, l2
}

// busySample: one delivery-rate sample of l with its window full (as onFeedback takes it).
func busySample(l *link, rate float64, nowM int64) {
	l.mu.Lock()
	l.noteRate(rate, l.win, nowM)
	l.mu.Unlock()
}

// A link far below the others for -slowafter is demoted (not before), gets no data and no probes, and its trial
// copies bring it back once it delivers again.
func TestSlowDemoteTrialPromote(t *testing.T) {
	p, l1, l2 := slowPeer(t, 0.032) // 256 kbit/s
	t0 := mono()
	step := int64(100 * time.Millisecond)
	now := t0
	for ; now < t0+int64(slowAfter)-step; now += step {
		busySample(l2, 0.032, now)
		p.slowTick(true, now)
	}
	if l2.sl.slow {
		t.Fatal("demoted before -slowafter")
	}
	for ; now <= t0+int64(slowAfter)+step; now += step {
		busySample(l2, 0.032, now)
		p.slowTick(true, now)
	}
	if !l2.sl.slow || l2.sl.slowCount != 1 || l1.sl.slow {
		t.Fatalf("after %v: l2 slow=%v count=%d, l1 slow=%v", slowAfter, l2.sl.slow, l2.sl.slowCount, l1.sl.slow)
	}
	// no data, no starvation probe (l2 idle for 1 s, l1 busy)
	l1.lastSend, l2.lastSend = now, now-int64(time.Second)
	l2.inflight, l2.hist = 0, nil
	if got := p.pick(1400, true, now); got != l1 {
		t.Fatalf("pick gave a demoted link a packet (%v)", got)
	}
	// the trial: slowAfter/2 after the demotion
	if l2.sl.trialGap != int64(slowAfter)/2 {
		t.Fatalf("first pause %v", time.Duration(l2.sl.trialGap))
	}
	now = l2.sl.trialAt
	p.slowTick(true, now-step)
	if p.trial.Load() != nil {
		t.Fatal("trial too early")
	}
	p.slowTick(true, now)
	if p.trial.Load() != l2 {
		t.Fatal("no trial after slowafter/2")
	}
	// still slow during the trial: no promotion, the pause doubles
	busySample(l2, 0.032, now+step)
	p.slowTick(true, now+int64(slowTrialLen)+step)
	if !l2.sl.slow || p.trial.Load() != nil || l2.sl.trialGap != int64(slowAfter) {
		t.Fatalf("after a slow trial: slow=%v trial=%v gap=%v", l2.sl.slow, p.trial.Load(), time.Duration(l2.sl.trialGap))
	}
	// next trial: the SIM is back to 10 Mbit/s – promoted during the trial
	now += int64(slowTrialLen) + step + int64(slowAfter)
	p.slowTick(true, now)
	if p.trial.Load() != l2 {
		t.Fatal("no second trial")
	}
	busySample(l2, 1.25, now+step)
	p.slowTick(true, now+2*step)
	if l2.sl.slow || p.trial.Load() != nil {
		t.Fatalf("not promoted by a 10 Mbit/s trial (slow=%v)", l2.sl.slow)
	}
}

// App-limited samples (window not in use) say nothing: an idle link is never demoted; nor is a 1 Mbit/s link (4 %).
func TestSlowNotDemoted(t *testing.T) {
	p, _, l2 := slowPeer(t, 0.032)
	t0 := mono()
	for now := t0; now < t0+3*int64(slowAfter); now += int64(100 * time.Millisecond) {
		l2.mu.Lock()
		l2.noteRate(0.001, 0.1*l2.win, now)
		l2.mu.Unlock()
		p.slowTick(true, now)
	}
	if l2.sl.slow {
		t.Fatal("demoted on app-limited samples")
	}
	p, _, l2 = slowPeer(t, 0.125) // 1 Mbit/s = 4 %
	for now := t0; now < t0+3*int64(slowAfter); now += int64(100 * time.Millisecond) {
		busySample(l2, 0.125, now)
		p.slowTick(true, now)
	}
	if l2.sl.slow {
		t.Fatal("1 Mbit/s next to 25 Mbit/s demoted (threshold 3 %)")
	}
	// one busy sample above the threshold starts the clock again
	p, _, l2 = slowPeer(t, 0.032)
	for i, now := 0, t0; now < t0+2*int64(slowAfter); i, now = i+1, now+int64(100*time.Millisecond) {
		r := 0.032
		if i%50 == 49 { // every 5 s
			r = 1
		}
		busySample(l2, r, now)
		p.slowTick(true, now)
	}
	if l2.sl.slow {
		t.Fatal("demoted although it delivered well every 5 s")
	}
}

// The demoted link carries data while no other live link is left (and is out again when one is back);
// off (-slowfrac 0 / backup) clears it.
func TestSlowBackWhenAlone(t *testing.T) {
	p, l1, l2 := slowPeer(t, 0.032)
	l2.sl.slow = true
	now := mono()
	l1.lastSend, l2.lastSend = now, now
	l1.dead = true
	p.slowTick(true, now)
	if p.pick(1400, false, now) != l2 || !l2.sl.slow {
		t.Fatal("the only live link gets no data")
	}
	l1.dead = false
	p.slowTick(true, now)
	if p.pick(1400, false, now) != l1 {
		t.Fatal("the slow link still carries data with l1 back")
	}
	p.slowTick(false, now)
	if l2.sl.slow {
		t.Fatal("slow kept with the feature off")
	}
}

// A trial copy does not move the original's repair time, needs no repair (rescued, no NACK mapping) and
// is only sent while the window has room.
func TestTrialCopy(t *testing.T) {
	setReliable(t, true)
	p, l1, l2 := slowPeer(t, 0.032)
	l2.sl.slow, l2.sl.trialUntil = true, mono()+int64(time.Second)
	p.trial.Store(l2)
	inner := make([]byte, 1000)
	now := mono()
	p.reMu.Lock()
	p.copies[7], p.copySent[7] = inner, 0
	p.reMu.Unlock()
	p.send(7, inner, 0, now, nil)
	if len(l1.hist) != 1 || len(l2.hist) != 1 {
		t.Fatalf("hist l1 %d l2 %d, want the original on l1 and a copy on l2", len(l1.hist), len(l2.hist))
	}
	if !l2.hist[0].rescued || l1.hist[0].rescued || len(l2.lmap) != 0 {
		t.Fatal("the copy must be marked rescued and have no NACK mapping")
	}
	if d, ok := p.dueOf(7); !ok || d != int64(l1.repairSpanUs())*1000+now {
		t.Fatalf("repair time %d (ok %v) is not the original's on l1", d, ok)
	}
	l2.inflight = l2.winB // full window: no copy
	p.send(8, inner, 0, now, nil)
	if len(l2.hist) != 1 || l2.sl.copies != 1 {
		t.Fatalf("copy sent into a full window (%d)", len(l2.hist))
	}
}

// sendRaw never waits: a full socket (a datagram socket pair nobody reads) returns at once with EAGAIN, where a
// plain write blocks.
func TestSendRawNoWait(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	unix.SetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_SNDBUF, 4096)
	f := os.NewFile(uintptr(fds[0]), "a")
	c, err := net.FileConn(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	defer unix.Close(fds[1])
	b := make([]byte, 1000)
	full := false
	for i := 0; i < 100000 && !full; i++ {
		start := time.Now()
		err := sendRaw(c.(syscall.Conn), b, nil)
		if time.Since(start) > 100*time.Millisecond {
			t.Fatal("sendRaw waited")
		}
		full = err == unix.EAGAIN
	}
	if !full {
		t.Fatal("socket never filled")
	}
	done := make(chan struct{})
	go func() { c.Write(b); close(done) }()
	select {
	case <-done:
		t.Fatal("a plain write did not block on the full socket: the test proves nothing")
	case <-time.After(200 * time.Millisecond):
	}
	c.Close() // releases the blocked writer
}

// sendNow delivers over UDP (address conversion).
func TestSendNowUDP(t *testing.T) {
	r, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer s.Close()
	if err := sendNow(s, []byte("fb"), r.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	r.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 10)
	n, _, err := r.ReadFromUDP(b)
	if err != nil || string(b[:n]) != "fb" {
		t.Fatalf("got %q, %v", b[:n], err)
	}
}
