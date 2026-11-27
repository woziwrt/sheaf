// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"math/rand"
	"testing"
)

// The fast checker must find exactly the first corrupted byte, whatever the read boundaries.
func TestChecker(t *testing.T) {
	buf := make([]byte, 1<<20)
	fill(buf, 0)
	for trial := 0; trial < 200; trial++ {
		bad := int64(-1)
		data := append([]byte(nil), buf...)
		if trial%2 == 1 {
			bad = int64(rand.Intn(len(data)))
			data[bad] ^= 1 << uint(rand.Intn(8))
		}
		c := &checker{bad: -1}
		for p := 0; p < len(data); {
			n := 1 + rand.Intn(5000)
			if p+n > len(data) {
				n = len(data) - p
			}
			c.Write(data[p : p+n])
			p += n
		}
		if c.bad != bad {
			t.Fatalf("trial %d: got %d want %d", trial, c.bad, bad)
		}
	}
}
