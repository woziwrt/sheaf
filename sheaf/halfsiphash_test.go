// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"encoding/binary"
	"encoding/hex"
	"math/bits"
	"math/rand"
	"testing"
)

// halfRef: plain HalfSipHash-c-d with the 64-bit output, a line-by-line transcription of the reference
// halfsiphash.c (outlen 8); its output bytes. Checked against the reference vectors below.
func halfRef(c, d int, key, m []byte) [8]byte {
	k0, k1 := binary.LittleEndian.Uint32(key), binary.LittleEndian.Uint32(key[4:])
	v0, v1, v2, v3 := k0, k1, uint32(0x6c796765)^k0, uint32(0x74656462)^k1
	v1 ^= 0xee
	round := func() {
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
	n := len(m)
	for ; len(m) >= 4; m = m[4:] {
		w := binary.LittleEndian.Uint32(m)
		v3 ^= w
		for i := 0; i < c; i++ {
			round()
		}
		v0 ^= w
	}
	b := uint32(n) << 24
	for i := len(m) - 1; i >= 0; i-- {
		b |= uint32(m[i]) << (8 * i)
	}
	v3 ^= b
	for i := 0; i < c; i++ {
		round()
	}
	v0 ^= b
	v2 ^= 0xee
	for i := 0; i < d; i++ {
		round()
	}
	var out [8]byte
	binary.LittleEndian.PutUint32(out[:], v1^v3)
	v1 ^= 0xdd
	for i := 0; i < d; i++ {
		round()
	}
	binary.LittleEndian.PutUint32(out[4:], v1^v3)
	return out
}

// vectors_hsip64 from Aumasson's reference repository (github.com/veorq/SipHash, vectors.h): HalfSipHash-2-4 with
// the 64-bit output, key 00 01 .. 07, message i bytes 00 01 .. i-1.
var hsip64Vectors = []string{
	"218d1f59b9b83cc8", "be552412f8387315", "064f39ef7c50eb57", "ce0f1a45f7060679", "d5e78a175be52ea1",
	"cb9d7c3f2f3db580", "ce3e91358aa2bc25", "ff202728b07bc684", "edfee820bce4858c", "5b51cccc13888307",
	"95b0469f06a6f2ee", "ae26333994ddcd48", "7bc71f9faef5c799", "5a2352d75a0c3744", "3bb1a870eae8e658",
	"217d0bcb4e81c902", "7336aad25f7bf3b5", "37adc0641c4c4f6a", "c9b2db2b9a3e42f9", "f910e48020ab363c",
	"1bf52b0a6feea7db", "00741dc269e8b3ef", "e20103fa1ba776ef", "4c2210e54b681d73", "70741045ae3fa6f1",
	"0c86403739714038", "0d899ed8112923f0", "226bf5fab81ee1b8", "2d925ffb1e0016b5", "361958d52cee10f1",
	"291aaf864898179d", "863c7f155c34117c", "28709d46d811626c", "248477681d28f89c", "8324e4d7528f9830",
	"f9efd4e13aea6bd8", "86d67a40ec4276dc", "3f6292eccca97e35", "cbd92ee724d42109", "368df6808d403d79",
	"5b38c81c67c8ae4c", "95ab7189d439acb3", "a91a52c025327024", "5b0087c69528acea", "1e30f3ad27dcb15a",
	"697f5c9a90324ed4", "495c0f995557dc38", "9427202a3c29f94d", "a9eaa8c04ba93e3e", "eea4c1737d011218",
	"912d568fd8f65a49", "56919596b0ff5c97", "02445a7998f550e1", "86ec466ce71d1fb2", "359569e7d289e3bc",
	"871b05ca62bb7c96", "a1a492f942f15f1d", "12ec267ff6095b6e", "5d1b5ea1b231d89d", "d8cfb4453f92ee54",
	"d6762890bf26e460", "313563a4b7ed5cf3", "f90b3ab572d46693", "2ea63c71bf326087",
}

func TestHalfRefVectors(t *testing.T) {
	key := []byte{0, 1, 2, 3, 4, 5, 6, 7}
	m := make([]byte, 64)
	for i := range m {
		m[i] = byte(i)
	}
	for i, want := range hsip64Vectors {
		if got := halfRef(2, 4, key, m[:i]); hex.EncodeToString(got[:]) != want {
			t.Fatalf("length %d: got %x, want %s", i, got, want)
		}
	}
}

// The unrolled HalfSipHash-1-3 equals the reference at c=1, d=3, for every length up to 80 bytes, a 1328-byte
// frame, aligned and unaligned starts (the aligned word-load path and the byte path) and random keys.
func TestHalfsiphash13(t *testing.T) {
	r := rand.New(rand.NewSource(62))
	buf := make([]byte, 1400)
	r.Read(buf)
	lens := []int{1328, 1360}
	for n := 0; n <= 80; n++ {
		lens = append(lens, n)
	}
	for trial := 0; trial < 20; trial++ {
		key := make([]byte, 8)
		r.Read(key)
		k0, k1 := binary.LittleEndian.Uint32(key), binary.LittleEndian.Uint32(key[4:])
		for off := 0; off < 4; off++ {
			for _, n := range lens {
				m := buf[off : off+n]
				want := halfRef(1, 3, key, m)
				var got [8]byte
				binary.LittleEndian.PutUint64(got[:], halfsiphash13(k0, k1, m))
				if got != want {
					t.Fatalf("key %x off %d len %d: got %x want %x", key, off, n, got, want)
				}
			}
		}
	}
}

// halfKeyFrom folds the 16-byte key file into HalfSipHash's 64-bit key: bytes 0..7 XOR bytes 8..15.
func TestHalfKeyFrom(t *testing.T) {
	k, _ := hex.DecodeString("01020304050607080000000000000000")
	if got := halfKeyFrom(k); got != [2]uint32{0x04030201, 0x08070605} {
		t.Fatalf("key %08x", got)
	}
	k, _ = hex.DecodeString("00112233445566778899aabbccddeeff")
	if got := halfKeyFrom(k); got != [2]uint32{0x88888888, 0x88888888} {
		t.Fatalf("key %08x", got)
	}
}

// The tag of a 1328-byte frame (the proxy frame size under IPsec) with each algorithm; on amd64 both are fast,
// the interesting numbers come from the MIPS32 box (64-bit arithmetic emulated with 32-bit pairs there).
func benchTag(b *testing.B, f func([]byte) uint64) {
	m := make([]byte, 1328+hdrLen+8)
	b.SetBytes(int64(len(m)))
	var s uint64
	for i := 0; i < b.N; i++ {
		s += f(m)
	}
	_ = s
}

func BenchmarkTagSip1328(b *testing.B) {
	benchTag(b, func(m []byte) uint64 { return siphash13(1, 2, m) })
}

func BenchmarkTagHalf1328(b *testing.B) {
	benchTag(b, func(m []byte) uint64 { return halfsiphash13(1, 2, m) })
}
