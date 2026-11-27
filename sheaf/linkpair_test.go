// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

// v55: links are paired by the far end's link id, not by list position (25 Nov 2026: a server with 4 links and an R4
// hub with 2 dropped each other's feedback - n != len(p.links) - and the bond stalled).

import (
	"encoding/binary"
	"net"
	"sync"
	"testing"
)

// lpPeer: a peer with k links (ids 1..k), each with a peer address so that writes go nowhere harmful.
func lpPeer(t *testing.T, k int) *peer {
	p := &peer{kick: make(chan struct{}, 1), buf: map[uint32][]byte{}, copies: map[uint32][]byte{}, skippedSet: map[uint32]bool{}, copySent: map[uint32]int64{}}
	for i := 1; i <= k; i++ {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		l := &link{id: i, conn: c, win: winMax, lastSend: mono(), lmap: map[uint32]lmapEnt{}}
		l.derive()
		l.txCond = sync.NewCond(&l.txMu)
		l.peerA.Store(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})
		p.links = append(p.links, l)
	}
	return p
}

// lpFeedback: feedback from a far end with n links whose entry j (0-based) reports highest linkSeq his[j]
// (0 = nothing received on that link), sent over the far end's link lid (0 = v54, no id mark).
func lpFeedback(n int, his []uint32, lid int) []byte {
	b := []byte{magic, tFeedback, byte(n), 0}
	for j := 0; j < n; j++ {
		e := make([]byte, 21)
		if his[j] != 0 {
			e[0] = 1
			binary.BigEndian.PutUint32(e[1:], his[j])
		}
		b = append(b, e...)
	}
	if lid > 0 {
		b = append(b, fbLidMark, byte(lid))
	}
	return b
}

// sent: put linkSeqs 1..k into the link's history, as if sent.
func lpSent(l *link, k int) {
	l.mu.Lock()
	for i := 1; i <= k; i++ {
		l.lseq++
		l.hist = append(l.hist, sentPkt{lseq: l.lseq, size: 1000, at: nowUs()})
		l.inflight += 1000
	}
	l.mu.Unlock()
}

func histLen(l *link) int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.hist) }

// A server with 4 links gets feedback from a box with 2 (its links 1, 2 = our 1, 2): it must be applied.
func TestFeedbackFromFewerLinks(t *testing.T) {
	p := lpPeer(t, 4)
	lpSent(p.links[0], 5)
	lpSent(p.links[1], 5)
	p.onFeedback(lpFeedback(2, []uint32{5, 3}, 0)) // v54 box: no id mark, position = id
	if h := histLen(p.links[0]); h != 0 {
		t.Fatalf("link 1: %d packets still in flight after the box reported all 5", h)
	}
	if h := histLen(p.links[1]); h != 2 {
		t.Fatalf("link 2: %d in flight, want 2 (box reported up to 3 of 5)", h)
	}
}

// A box with 2 links gets feedback from a server with 4.
func TestFeedbackFromMoreLinks(t *testing.T) {
	p := lpPeer(t, 2)
	lpSent(p.links[0], 4)
	lpSent(p.links[1], 4)
	p.onFeedback(lpFeedback(4, []uint32{4, 4, 0, 0}, 1))
	if histLen(p.links[0]) != 0 || histLen(p.links[1]) != 0 {
		t.Fatalf("feedback of a 4-link server not applied: in flight %d, %d", histLen(p.links[0]), histLen(p.links[1]))
	}
}

// A hub using only satellite 2 (its links 1, 2) talks to the server's links 3, 4: the server learns the far ids and
// must credit entry 1 to its link 3 and entry 2 to its link 4, not to its links 1, 2.
func TestFeedbackPairsByFarID(t *testing.T) {
	p := lpPeer(t, 4)
	p.links[2].peerLid.Store(1)
	p.links[3].peerLid.Store(2)
	lpSent(p.links[2], 6)
	lpSent(p.links[3], 6)
	p.onFeedback(lpFeedback(2, []uint32{6, 2}, 1))
	if h := histLen(p.links[2]); h != 0 {
		t.Fatalf("server link 3 (= hub link 1): %d in flight, want 0", h)
	}
	if h := histLen(p.links[3]); h != 4 {
		t.Fatalf("server link 4 (= hub link 2): %d in flight, want 4", h)
	}
}

// The id mark: parsed with and without the cumulative ack block, absent in v54 feedback.
func TestFbLinkID(t *testing.T) {
	b := lpFeedback(3, []uint32{1, 0, 0}, 0)
	if fbLinkID(b) != 0 {
		t.Fatal("v54 feedback must have no id")
	}
	b = append(lpFeedback(3, []uint32{1, 0, 0}, 0), 1, 0, 0, 0, 9, fbLidMark, 3) // cum ack block + id
	if id := fbLinkID(b); id != 3 {
		t.Fatalf("id %d, want 3", id)
	}
	if id := fbLinkID(lpFeedback(2, []uint32{0, 0}, 2)); id != 2 {
		t.Fatalf("id %d without cum ack, want 2", id)
	}
}

// NACK of a range from the far end's link 1 goes to our link whose far id is 1 (here our link 3).
func TestNackRangeByFarID(t *testing.T) {
	p := lpPeer(t, 4)
	p.links[2].peerLid.Store(1)
	l := p.links[2]
	l.mu.Lock()
	l.lmap[7] = lmapEnt{seq: 100}
	l.mu.Unlock()
	m := []byte{magic, tNackR, 1, 0, 0, 0, 0, 7, 0, 1}
	p.onNackRange(m)
	l.mu.Lock()
	_, still := l.lmap[7]
	l.mu.Unlock()
	if still {
		t.Fatal("the NACK from the far link 1 did not reach our link 3")
	}
}
