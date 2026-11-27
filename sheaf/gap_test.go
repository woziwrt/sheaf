// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import "testing"

// A link's numbering starts at 1 in every session: losses before the first packet that arrives are a gap too.
func TestLinkGap(t *testing.T) {
	cases := []struct{ last, ls, from, n uint32 }{
		{0, 1, 0, 0},                   // first packet, nothing lost
		{0, 24, 1, 23},                 // the first 23 packets of the session lost
		{5, 6, 0, 0},                   // in order
		{5, 9, 6, 3},                   // 6, 7, 8 lost
		{9, 7, 0, 0},                   // older than the last one seen
		{9, 9, 0, 0},                   // duplicate
		{0xfffffffe, 2, 0xffffffff, 3}, // across the wrap
		{0, 10000, 1, 4096},            // capped
	}
	for _, c := range cases {
		if from, n := linkGap(c.last, c.ls); from != c.from || n != c.n {
			t.Errorf("linkGap(%d, %d) = %d+%d, want %d+%d", c.last, c.ls, from, n, c.from, c.n)
		}
	}
}
