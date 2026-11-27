// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"encoding/binary"
	"math/bits"
	"unsafe"
)

// halfsiphash13 – HalfSipHash-1-3 with the 64-bit output (Aumasson's reference halfsiphash.c with cROUNDS=1,
// dROUNDS=3, outlen=8; the round counts the Linux kernel's hsiphash uses), result read little endian. The -auth=half
// tag (v62) for links WITHOUT IPsec: SipHash works on 64-bit words, which the MIPS32 box emulates with 32-bit pairs;
// HalfSipHash's 32-bit words are native there. Its key is 64 bits (k0, k1 = two little-endian 32-bit words, see
// halfKeyFrom). The rounds are written out, as in siphash13.
func halfsiphash13(k0, k1 uint32, m []byte) uint64 {
	v0 := k0
	v1 := k1 ^ 0xee // outlen 8
	v2 := k0 ^ 0x6c796765
	v3 := k1 ^ 0x74656462
	n := len(m)
	if littleEndian && n >= 4 && uintptr(unsafe.Pointer(&m[0]))&3 == 0 {
		p := unsafe.Pointer(&m[0])
		nw := n >> 2
		for i := 0; i < nw; i++ {
			w := *(*uint32)(unsafe.Add(p, i*4))
			v3 ^= w
			v0 += v1
			v1 = bits.RotateLeft32(v1, 5)
			v1 ^= v0
			v0 = bits.RotateLeft32(v0, 16)
			v2 += v3
			v3 = bits.RotateLeft32(v3, 8)
			v3 ^= v2
			v0 += v3
			v3 = bits.RotateLeft32(v3, 7)
			v3 ^= v0
			v2 += v1
			v1 = bits.RotateLeft32(v1, 13)
			v1 ^= v2
			v2 = bits.RotateLeft32(v2, 16)
			v0 ^= w
		}
		m = m[nw*4:]
	}
	for len(m) >= 4 {
		w := binary.LittleEndian.Uint32(m)
		v3 ^= w
		v0 += v1
		v1 = bits.RotateLeft32(v1, 5)
		v1 ^= v0
		v0 = bits.RotateLeft32(v0, 16)
		v2 += v3
		v3 = bits.RotateLeft32(v3, 8)
		v3 ^= v2
		v0 += v3
		v3 = bits.RotateLeft32(v3, 7)
		v3 ^= v0
		v2 += v1
		v1 = bits.RotateLeft32(v1, 13)
		v1 ^= v2
		v2 = bits.RotateLeft32(v2, 16)
		v0 ^= w
		m = m[4:]
	}
	w := uint32(n) << 24 // the last 0-3 bytes and the length
	switch len(m) {
	case 3:
		w |= uint32(m[2]) << 16
		fallthrough
	case 2:
		w |= uint32(m[1]) << 8
		fallthrough
	case 1:
		w |= uint32(m[0])
	}
	v3 ^= w
	v0 += v1
	v1 = bits.RotateLeft32(v1, 5)
	v1 ^= v0
	v0 = bits.RotateLeft32(v0, 16)
	v2 += v3
	v3 = bits.RotateLeft32(v3, 8)
	v3 ^= v2
	v0 += v3
	v3 = bits.RotateLeft32(v3, 7)
	v3 ^= v0
	v2 += v1
	v1 = bits.RotateLeft32(v1, 13)
	v1 ^= v2
	v2 = bits.RotateLeft32(v2, 16)
	v0 ^= w
	v2 ^= 0xee // outlen 8
	var out [2]uint32
	for half := 0; half < 2; half++ {
		if half == 1 {
			v1 ^= 0xdd
		}
		for i := 0; i < 3; i++ {
			v0 += v1
			v1 = bits.RotateLeft32(v1, 5)
			v1 ^= v0
			v0 = bits.RotateLeft32(v0, 16)
			v2 += v3
			v3 = bits.RotateLeft32(v3, 8)
			v3 ^= v2
			v0 += v3
			v3 = bits.RotateLeft32(v3, 7)
			v3 ^= v0
			v2 += v1
			v1 = bits.RotateLeft32(v1, 13)
			v1 ^= v2
			v2 = bits.RotateLeft32(v2, 16)
		}
		out[half] = v1 ^ v3
	}
	return uint64(out[1])<<32 | uint64(out[0]) // the 8 output bytes read little endian
}

// halfKeyFrom: the 64-bit HalfSipHash key from the 16-byte key file: bytes 0..7 XOR bytes 8..15 (all 128 bits
// count; HalfSipHash's key is 64 bits by definition), k0 = LE32 of byte 0..3 of that, k1 = LE32 of byte 4..7.
func halfKeyFrom(k []byte) [2]uint32 {
	var x [8]byte
	for i := range x {
		x[i] = k[i] ^ k[i+8]
	}
	return [2]uint32{binary.LittleEndian.Uint32(x[0:]), binary.LittleEndian.Uint32(x[4:])}
}
