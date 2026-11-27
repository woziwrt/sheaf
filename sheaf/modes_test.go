// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"io"
	"os"
	"testing"
	"time"
)

// ipPkt: a minimal IPv4 packet of protocol proto with id as its 4th byte (enough for isUDP / noDelay / the test).
func ipPkt(proto byte, id byte) []byte {
	b := getRx(60)
	for i := range b {
		b[i] = 0
	}
	b[0], b[2], b[3], b[9] = 0x45, 0, 60, proto
	b[4] = id
	if proto == 6 {
		b[32] = 0x50 // data offset 20, flags 0: TCP with payload, not a pure ACK
	}
	return b
}

// Smooth mode: a UDP packet behind a hole comes out after holdTime, TCP behind the same hole waits for the repair;
// the UDP packet is not delivered a second time when the hole fills.
func TestSmoothReleasesUDPOnly(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	p := &peer{buf: map[uint32][]byte{}, skippedSet: map[uint32]bool{}, tun: w}
	setMode(modeSmooth, 0, "test")
	defer setMode(modeSpeed, 0, "test")
	go p.liveWatch()

	got := make(chan byte, 16)
	go func() {
		b := make([]byte, 60) // every test packet is 60 B; a pipe does not keep write boundaries
		for {
			if _, err := io.ReadFull(r, b); err != nil {
				return
			}
			got <- b[4]
		}
	}()
	expect := func(want []byte, within time.Duration) {
		t.Helper()
		for _, id := range want {
			select {
			case g := <-got:
				if g != id {
					t.Fatalf("got packet %d, want %d", g, id)
				}
			case <-time.After(within):
				t.Fatalf("packet %d not delivered within %v", id, within)
			}
		}
	}
	none := func(d time.Duration) {
		t.Helper()
		select {
		case g := <-got:
			t.Fatalf("unexpected packet %d", g)
		case <-time.After(d):
		}
	}

	p.deliver(1, ipPkt(6, 1)) // in order
	expect([]byte{1}, time.Second)
	// seq 2 lost; 3 = UDP, 4 = TCP
	p.deliver(3, ipPkt(17, 3))
	p.deliver(4, ipPkt(6, 4))
	none(50 * time.Millisecond)             // holdTime without links = 100 ms
	expect([]byte{3}, 300*time.Millisecond) // the UDP packet goes ahead of the hole
	none(150 * time.Millisecond)            // TCP still waits
	p.deliver(2, ipPkt(6, 2))               // the repair
	expect([]byte{2, 4}, time.Second)       // in order, 3 not again
	none(50 * time.Millisecond)
	if p.liveEarly != 1 || len(p.buf) != 0 || p.next != 5 {
		t.Fatalf("liveEarly %d buf %d next %d", p.liveEarly, len(p.buf), p.next)
	}
}

// Speed mode: UDP waits behind a hole like everything else.
func TestSpeedKeepsUDPInOrder(t *testing.T) {
	r, w, _ := os.Pipe()
	defer r.Close()
	p := &peer{buf: map[uint32][]byte{}, skippedSet: map[uint32]bool{}, tun: w}
	setMode(modeSpeed, 0, "test")
	go p.liveWatch()
	p.deliver(1, ipPkt(6, 1))
	p.deliver(3, ipPkt(17, 3))
	time.Sleep(250 * time.Millisecond)
	if p.liveEarly != 0 || len(p.buf) != 1 {
		t.Fatalf("liveEarly %d buf %d", p.liveEarly, len(p.buf))
	}
}

func TestPeerModeByte(t *testing.T) {
	modeFollow = true
	defer func() { modeFollow = false; setMode(modeSpeed, 0, "test") }()
	onPeerMode(0) // an older peer: nothing said
	if curMode() != modeSpeed {
		t.Fatal("mode changed by a zero byte")
	}
	setMode(modeBackup, 1, "test")
	b := modeByte()
	setMode(modeSpeed, 0, "test")
	onPeerMode(b)
	if curMode() != modeBackup || primaryIdx() != 1 {
		t.Fatalf("mode %d primary %d", curMode(), primaryIdx())
	}
}
