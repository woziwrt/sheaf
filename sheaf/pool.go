// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import "sync"

// Receive buffers: a received data packet's payload used to be a fresh allocation (make + copy) – on the MIPS board
// ~5 % of the CPU in malloc plus the garbage collector's work behind it. Now it comes from a pool and goes back when
// its consumer is done with it: the TUN write, the frame handler (control frames), or the stream writer once the
// bytes are in the local socket. A buffer that is simply dropped (a stream aborted, a session reset) is garbage
// collected as before – returning is an optimisation, never a requirement.
const rxBufSize = 2048

type rxBuf [rxBufSize]byte

var rxPool = sync.Pool{New: func() any { return new(rxBuf) }}

// getRx returns a buffer of length n (n <= rxBufSize) that starts at the beginning of a pooled array.
func getRx(n int) []byte { return rxPool.Get().(*rxBuf)[:n] }

// putRx returns a buffer obtained from getRx (the whole slice as returned, not a sub-slice of it).
func putRx(b []byte) {
	if cap(b) == rxBufSize {
		rxPool.Put((*rxBuf)(b[:rxBufSize]))
	}
}
