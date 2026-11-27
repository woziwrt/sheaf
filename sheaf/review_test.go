// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

// Regression tests for the findings of the adversarial review of 24 Nov 2026 (v53). Each failed before its fix.

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

func regPeer(t *testing.T) *peer {
	p := &peer{kick: make(chan struct{}, 1), buf: map[uint32][]byte{}, copies: map[uint32][]byte{}, skippedSet: map[uint32]bool{}, copySent: map[uint32]int64{}}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	l := &link{id: 1, conn: c, win: winMax, lastSend: mono(), lmap: map[uint32]lmapEnt{}}
	l.derive()
	l.txCond = sync.NewCond(&l.txMu)
	l.peerA.Store(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})
	p.links = []*link{l}
	return p
}

// regTCP: a newPkt buffer with an IPv4/TCP packet (sport) carrying payload bytes, TCP flags.
func regTCP(sport byte, payload int, flags byte) []byte {
	c := newPkt(40 + payload)
	pl := c[hdrLen:]
	pl[0], pl[9] = 0x45, 6
	binary.BigEndian.PutUint16(pl[2:], uint16(40+payload))
	pl[12], pl[15], pl[16], pl[19] = 10, 2, 1, 1
	pl[20], pl[21], pl[22], pl[23] = 0x10, sport, 0x01, 0xbb
	pl[32] = 5 << 4
	pl[33] = flags
	return c
}

func regPureAck(id byte) []byte { b := ipPkt(6, id); b[3], b[33] = 40, 0x10; return b }

func regCollector(t *testing.T) (*os.File, chan byte) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan byte, 1024)
	go func() {
		b := make([]byte, 60)
		for {
			if _, err := io.ReadFull(r, b); err != nil {
				return
			}
			got <- b[4]
		}
	}()
	return w, got
}

func regDrain(got chan byte, d time.Duration) []byte {
	var out []byte
	for {
		select {
		case g := <-got:
			out = append(out, g)
		case <-time.After(d):
			return out
		}
	}
}

// fq: a flow that never empties must not keep a slot for every packet that ever passed.
func TestRegFqNoGrowth(t *testing.T) {
	f := newFlowQueue()
	f.push(udpPkt(1, 1300))
	var ms0, ms1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms0)
	for i := 0; i < 2_000_000; i++ {
		f.push(udpPkt(1, 64))
		f.pop()
	}
	runtime.GC()
	runtime.ReadMemStats(&ms1)
	fl := &f.flows[flowHash(udpPkt(1, 64)[hdrLen:])]
	t.Logf("cap %d slots for %d queued, heap %+d MB", cap(fl.q), f.pkts, (int64(ms1.HeapAlloc)-int64(ms0.HeapAlloc))>>20)
	if cap(fl.q) > 1000 {
		t.Errorf("slice holds %d slots for %d queued packets", cap(fl.q), f.pkts)
	}
}

// Concurrent submitters must not overshoot maxCopies (the flow predecessor ring of unacked packets survives).
func TestRegCopiesNoOvershoot(t *testing.T) {
	reliable = true
	defer func() { reliable = false }()
	worst := 0
	for trial := 0; trial < 50; trial++ {
		p := regPeer(t)
		p.submit(frame(fData, 1, make([]byte, 10)))
		for i := 1; i < maxCopies-1; i++ {
			p.copies[uint32(i)] = nil
			p.copyQ = append(p.copyQ, uint32(i))
		}
		p.seq = maxCopies - 1
		for i := 0; i < 50; i++ {
			p.reinject = append(p.reinject, sentPkt{seq: 1, data: make([]byte, 100), ep: 0})
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		// the epoch is taken HERE, not when a goroutine gets to run: one scheduled only after the reset below (a slow
		// or stalled VM) read the new epoch, took a number in the "new session", sent it on a link that nobody ever
		// acks, and once that link counted as dead it waited for a live link forever (hung 10 min: drainRepairs ->
		// send, seq 0x10004 ep=1)
		ep := p.epoch.Load()
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; p.submitEp(regTCP(7, 100, 0x18), ep) }()
		}
		close(start)
		time.Sleep(100 * time.Millisecond) // the rest wait for room: nobody acks
		p.reMu.Lock()
		if n := len(p.copies); n > worst {
			worst = n
		}
		// let the waiting submitters finish before the test ends (left waiting they read the global reliable while
		// later tests switch it: the race detector's reports on this test and the next): room, nothing to repair, and
		// a new session epoch, so they return without sending
		p.copies, p.copyQ, p.reinject = map[uint32][]byte{}, nil, nil
		p.epoch.Add(1)
		p.reMu.Unlock()
		wg.Wait()
		if fp, ok := p.predecessor(0); ok {
			t.Fatalf("seq 0 (a proxy frame) has flow predecessor %d", fp)
		}
	}
	if worst > maxCopies {
		t.Errorf("copies reached %d (> %d)", worst, maxCopies)
	}
}

// A proxy frame claiming a flow predecessor must not overtake a hole (proxy streams have no offsets).
func TestRegFrameNotReleasedEarly(t *testing.T) {
	reliable = true
	defer func() { reliable = false }()
	p := regPeer(t)
	p.started = true
	s := newStream(5)
	smu.Lock()
	streams[5] = s
	smu.Unlock()
	defer func() { smu.Lock(); delete(streams, 5); smu.Unlock() }()
	mk := func(b byte) []byte {
		f := frame(fData, 5^dirBit, []byte{b})[hdrLen:]
		pl := getRx(len(f))
		copy(pl, f)
		return pl
	}
	p.rxMu.Lock()
	p.buf[7] = nil
	p.markFlowOK(7)
	p.rxMu.Unlock()
	p.deliverF(1, 7, true, mk('B'))
	p.deliverF(0, 0, false, mk('A'))
	var got []byte
	for _, it := range s.q {
		got = append(got, it.d[0])
	}
	if string(got) != "AB" {
		t.Errorf("stream bytes %q instead of \"AB\"", got)
	}
}

// flowrel: a pure ACK let out early must not let later data of its flow overtake earlier data.
func TestRegNoDelayKeepsFlowOrder(t *testing.T) {
	w, got := regCollector(t)
	p := &peer{buf: map[uint32][]byte{}, skippedSet: map[uint32]bool{}, tun: w}
	setMode(modeSpeed, 0, "test")
	p.deliverF(0, 0, false, ipPkt(17, 100))
	p.deliverF(1, 1, true, ipPkt(6, 1))
	p.deliverF(3, 2, true, regPureAck(3))
	p.deliverF(4, 3, true, ipPkt(6, 4))
	p.deliverF(2, 1, true, ipPkt(6, 2))
	d := regDrain(got, 100*time.Millisecond)
	pos := map[byte]int{}
	for i, x := range d {
		pos[x] = i
	}
	if pos[4] < pos[2] {
		t.Errorf("delivery %v: data 4 before data 2 of the same flow", d)
	}
}

// A repaired hole that is a pure ACK must advance the in-order point (and release what waits behind it).
func TestRegNoDelayRepairFlushes(t *testing.T) {
	w, got := regCollector(t)
	p := &peer{buf: map[uint32][]byte{}, skippedSet: map[uint32]bool{}, tun: w, started: true}
	setMode(modeSpeed, 0, "test")
	p.deliverF(1, 0, true, ipPkt(6, 1))
	p.deliverF(2, 0, false, ipPkt(17, 2))
	p.deliverF(0, 0, true, regPureAck(10))
	d := regDrain(got, 200*time.Millisecond)
	p.rxMu.Lock()
	defer p.rxMu.Unlock()
	if p.next != 3 || len(p.buf) != 0 {
		t.Errorf("after the repair: delivered %v, next %d, buf %d (want next 3, buf 0)", d, p.next, len(p.buf))
	}
}

// A download's upstream (only pure ACKs) must keep the cumulative ack moving after one reordering.
func TestRegPureAcksMoveCumAck(t *testing.T) {
	r, w, _ := os.Pipe()
	go io.Copy(io.Discard, r)
	p := &peer{buf: map[uint32][]byte{}, skippedSet: map[uint32]bool{}, tun: w, started: true}
	setMode(modeSpeed, 0, "test")
	p.deliverF(0, 0, true, regPureAck(0))
	p.deliverF(2, 1, true, regPureAck(0))
	p.deliverF(1, 0, true, regPureAck(0))
	for s := uint32(3); s < 70000; s++ {
		p.deliverF(s, s-1, true, regPureAck(0))
	}
	p.rxMu.Lock()
	defer p.rxMu.Unlock()
	if p.next != 70000 || len(p.buf) != 0 {
		t.Errorf("in-order point %d, buf %d after 70000 ACKs (want 70000, 0)", p.next, len(p.buf))
	}
}
