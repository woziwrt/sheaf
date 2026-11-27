// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"io"
	"os"
	"testing"
	"time"
)

// A lost packet of one flow holds back only that flow: the other flow's packets go out at once, and the held
// packets follow in their own order when the repair comes.
func TestFlowReleaseOnlyOwnFlow(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	p := &peer{buf: map[uint32][]byte{}, skippedSet: map[uint32]bool{}, tun: w}
	setMode(modeSpeed, 0, "test")
	got := make(chan byte, 16)
	go func() {
		b := make([]byte, 60)
		for {
			if _, err := io.ReadFull(r, b); err != nil {
				return
			}
			got <- b[4]
		}
	}()
	expect := func(want ...byte) {
		t.Helper()
		for _, id := range want {
			select {
			case g := <-got:
				if g != id {
					t.Fatalf("got packet %d, want %d", g, id)
				}
			case <-time.After(time.Second):
				t.Fatalf("packet %d not delivered", id)
			}
		}
		select {
		case g := <-got:
			t.Fatalf("unexpected packet %d", g)
		case <-time.After(30 * time.Millisecond):
		}
	}
	// flow A (TCP): 1, 2, 4, 6 · flow B (UDP): 3, 5 · 2 is lost at first
	p.deliverF(1, 1, true, ipPkt(6, 1))
	expect(1)
	p.deliverF(3, 3, true, ipPkt(17, 3)) // B's first packet: behind A's hole, but not A's flow
	expect(3)
	p.deliverF(4, 2, true, ipPkt(6, 4)) // A after the hole: waits
	p.deliverF(5, 3, true, ipPkt(17, 5))
	expect(5)
	p.deliverF(6, 4, true, ipPkt(6, 6)) // waits behind 4
	expect()
	p.deliverF(2, 1, true, ipPkt(6, 2)) // the repair: A in its order
	expect(2, 4, 6)
	if len(p.buf) != 0 || p.next != 7 || len(p.waitOn) != 0 {
		t.Fatalf("buf %d next %d waitOn %d", len(p.buf), p.next, len(p.waitOn))
	}
	// a packet without a predecessor (older sender) keeps the strict order
	p.deliverF(8, 0, false, ipPkt(17, 8))
	expect()
	p.deliverF(7, 0, false, ipPkt(17, 7))
	expect(7, 8)
}

// The sender chains each flow: first packet = own seq, then the previous packet of the same flow.
func TestFlowPredecessors(t *testing.T) {
	p := &peer{fprev: make([]uint64, maxCopies)}
	a, b := ipPkt(6, 0), ipPkt(17, 0)
	b[19] = 9 // another destination: another flow
	p.notePredecessor(10, a)
	p.notePredecessor(11, b)
	p.notePredecessor(12, a)
	p.notePredecessor(13, []byte{fData, 0, 0}) // a proxy frame: none
	for _, c := range []struct {
		seq, fp uint32
		ok      bool
	}{{10, 10, true}, {11, 11, true}, {12, 10, true}, {13, 0, false}} {
		fp, ok := p.predecessor(c.seq)
		if ok != c.ok || (ok && fp != c.fp) {
			t.Fatalf("seq %d: predecessor %d %v, want %d %v", c.seq, fp, ok, c.fp, c.ok)
		}
	}
}
