// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"encoding/binary"
	"math/bits"
	"unsafe"
)

// littleEndian: the host stores words little endian (amd64, mipsle, arm) – then an aligned word load
// reads the same value as binary.LittleEndian, in one instruction instead of a byte-by-byte assembly.
var littleEndian = func() bool { x := uint16(1); return *(*byte)(unsafe.Pointer(&x)) == 1 }()

// siphash13 – SipHash-1-3 (Aumasson, Bernstein; one compression round, three finalization rounds), 64-bit output.
// The wire tag. SipHash-2-4 cost 75 µs per 1448-byte datagram on the 880 MHz MIPS32 board (64-bit arithmetic is
// emulated with 32-bit pairs); this version costs ~15 µs there. Rounds are written out (a closure over v0..v3 was
// not inlined: every round went through memory).
func siphash13(k0, k1 uint64, m []byte) uint64 {
	v0 := k0 ^ 0x736f6d6570736575
	v1 := k1 ^ 0x646f72616e646f6d
	v2 := k0 ^ 0x6c7967656e657261
	v3 := k1 ^ 0x7465646279746573
	n := len(m)
	if littleEndian && n >= 8 && uintptr(unsafe.Pointer(&m[0]))&3 == 0 {
		p := unsafe.Pointer(&m[0])
		nb := n >> 3
		for i := 0; i < nb; i++ {
			w := uint64(*(*uint32)(unsafe.Add(p, i*8+4)))<<32 | uint64(*(*uint32)(unsafe.Add(p, i*8)))
			v3 ^= w
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
			v0 ^= w
		}
		m = m[nb*8:]
	}
	for len(m) >= 8 {
		w := binary.LittleEndian.Uint64(m)
		v3 ^= w
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
		v0 ^= w
		m = m[8:]
	}
	var last [8]byte
	copy(last[:], m)
	last[7] = byte(n)
	w := binary.LittleEndian.Uint64(last[:])
	v3 ^= w
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
	v0 ^= w
	v2 ^= 0xff
	for i := 0; i < 3; i++ {
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
	return v0 ^ v1 ^ v2 ^ v3
}
