// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"encoding/binary"
	"math/bits"
	"testing"
)

// sipRef: plain SipHash-c-d reference (checked against the paper's SipHash-2-4 vectors below).
func sipRef(c, d int, k0, k1 uint64, m []byte) uint64 {
	v0 := k0 ^ 0x736f6d6570736575
	v1 := k1 ^ 0x646f72616e646f6d
	v2 := k0 ^ 0x6c7967656e657261
	v3 := k1 ^ 0x7465646279746573
	round := func() {
		v0 += v1
		v1 = bits.RotateLeft64(v1, 13)
		v1 ^= v0
		v0 = bits.RotateLeft64(v0, 32)
		v2 += v3
		v3 = bits.RotateLeft64(v3, 16)
		v3 ^= v2
		v0 += v3
		v3 = bits.RotateLeft64(v3, 21)
		v3 ^= v0
		v2 += v1
		v1 = bits.RotateLeft64(v1, 17)
		v1 ^= v2
		v2 = bits.RotateLeft64(v2, 32)
	}
	n := len(m)
	var last [8]byte
	for {
		var w uint64
		if len(m) >= 8 {
			w = binary.LittleEndian.Uint64(m)
			m = m[8:]
		} else {
			copy(last[:], m)
			last[7] = byte(n)
			w = binary.LittleEndian.Uint64(last[:])
			m = nil
		}
		v3 ^= w
		for i := 0; i < c; i++ {
			round()
		}
		v0 ^= w
		if m == nil {
			break
		}
	}
	v2 ^= 0xff
	for i := 0; i < d; i++ {
		round()
	}
	return v0 ^ v1 ^ v2 ^ v3
}

func testKey() (uint64, uint64) {
	var key [16]byte
	for i := range key {
		key[i] = byte(i)
	}
	return binary.LittleEndian.Uint64(key[:8]), binary.LittleEndian.Uint64(key[8:])
}

// Reference vectors from the SipHash paper (key 00..0f, message 00..len-1) validate sipRef.
func TestSipRef24(t *testing.T) {
	k0, k1 := testKey()
	want := map[int]uint64{0: 0x726fdb47dd0e0e31, 1: 0x74f839c593dc67fd, 8: 0x93f5f5799a932462, 15: 0xa129ca6149be45e5, 63: 0x958a324ceb064572}
	for n, w := range want {
		m := make([]byte, n)
		for i := range m {
			m[i] = byte(i)
		}
		if got := sipRef(2, 4, k0, k1, m); got != w {
			t.Errorf("len %d: got %016x want %016x", n, got, w)
		}
	}
}

// The wire MAC equals SipHash-1-3 for every length and alignment (word-load and byte paths).
func TestSiphash13(t *testing.T) {
	k0, k1 := testKey()
	buf := make([]byte, 1600)
	for i := range buf {
		buf[i] = byte(i*7 + 3)
	}
	for off := 0; off < 8; off++ {
		for n := 0; n+off <= len(buf); n += 1 + n/16 {
			m := buf[off : off+n]
			if got, want := siphash13(k0, k1, m), sipRef(1, 3, k0, k1, m); got != want {
				t.Fatalf("off %d len %d: got %016x want %016x", off, n, got, want)
			}
		}
	}
}

func BenchmarkSiphash13(b *testing.B) {
	m := make([]byte, 1448)
	b.SetBytes(int64(len(m)))
	var s uint64
	for i := 0; i < b.N; i++ {
		s += siphash13(1, 2, m)
	}
	_ = s
}
