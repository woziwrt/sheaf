// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

// v57: a new session forgets learned peer addresses (a server link the far end stopped using must not take data).

import (
	"net"
	"testing"
)

func TestResetPeerForgetsLearnedAddresses(t *testing.T) {
	p := lpPeer(t, 3)
	p.links[0].learn = true
	p.links[2].learn = true // links 1 and 3 learned, link 2 configured
	p.resetPeer()
	if p.links[0].dst() != nil || p.links[2].dst() != nil {
		t.Fatalf("learned addresses kept after a new session: %v %v", p.links[0].dst(), p.links[2].dst())
	}
	if p.links[1].dst() == nil {
		t.Fatal("a configured address was forgotten")
	}
	haveKey = true
	defer func() { haveKey = false }()
	from := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7001}
	p.learnAddr(p.links[0], from)
	if d := p.links[0].dst(); d == nil || d.Port != 7001 {
		t.Fatalf("not learned again: %v", d)
	}
	if p.pick(1000, false, mono()) == p.links[2] {
		t.Fatal("pick chose a link without an address")
	}
}
