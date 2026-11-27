// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import "testing"

// udpPkt: a newPkt buffer with an IPv4/UDP packet of size n from source port sport.
func udpPkt(sport byte, n int) []byte {
	c := newPkt(n)
	pl := c[hdrLen:]
	pl[0], pl[9] = 0x45, 17
	pl[12], pl[15], pl[16], pl[19] = 10, 2, 1, 1
	pl[20], pl[21] = 0x10, sport
	pl[22], pl[23] = 0x10, 0x00
	return c
}

func srcPort(c []byte) byte { return c[hdrLen+21] }

// A sparse flow (one packet now and then) is served before the backlog of a bulk flow.
func TestFqSparseFlowFirst(t *testing.T) {
	f := newFlowQueue()
	for i := 0; i < 100; i++ {
		f.push(udpPkt(1, 1300)) // bulk backlog
	}
	f.pop() // the bulk flow uses up its first quantum (as a new flow) ...
	f.pop() // ... and becomes an old flow
	f.push(udpPkt(2, 200))
	if g := srcPort(f.pop()); g != 2 {
		t.Fatalf("sparse packet not first, got flow %d", g)
	}
}

// Two bulk flows share the sender equally (deficit round robin by bytes).
func TestFqFairShare(t *testing.T) {
	f := newFlowQueue()
	for i := 0; i < 100; i++ {
		f.push(udpPkt(1, 1300))
		f.push(udpPkt(2, 600))
	}
	var by [3]int
	for i := 0; i < 60; i++ {
		c := f.pop()
		by[srcPort(c)] += len(c) - hdrLen
	}
	r := float64(by[1]) / float64(by[2])
	if r < 0.7 || r > 1.4 {
		t.Fatalf("bytes flow1 %d flow2 %d", by[1], by[2])
	}
}

// Over the limit the fattest flow loses its oldest packets; a small flow keeps everything.
func TestFqDropsFattest(t *testing.T) {
	old := fqLimit
	fqLimit = 20000
	defer func() { fqLimit = old }()
	f := newFlowQueue()
	d0 := fqDrops.Load()
	f.push(udpPkt(2, 200))
	for i := 0; i < 50; i++ {
		f.push(udpPkt(1, 1300))
	}
	if f.bytes > fqLimit {
		t.Fatalf("queue %d bytes over the limit", f.bytes)
	}
	if fqDrops.Load()-d0 == 0 {
		t.Fatal("nothing dropped")
	}
	seen2 := false
	for f.pkts > 0 {
		if srcPort(f.pop()) == 2 {
			seen2 = true
		}
	}
	if !seen2 {
		t.Fatal("the small flow lost its packet")
	}
}
