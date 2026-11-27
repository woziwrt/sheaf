// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// groPeer: a peer whose TUN is one end of a datagram socket pair (write boundaries stay visible), -tungro on.
func groPeer(t *testing.T) (*peer, *os.File) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	w, r := os.NewFile(uintptr(fds[0]), "tunw"), os.NewFile(uintptr(fds[1]), "tunr")
	t.Cleanup(func() { w.Close(); r.Close() })
	old := tunGRO
	tunGRO = true
	groOff.Store(false)
	t.Cleanup(func() { tunGRO = old; groOff.Store(false) })
	return &peer{buf: map[uint32][]byte{}, skippedSet: map[uint32]bool{}, tun: w, started: true}, r
}

// writes: every datagram written so far (non-blocking read until empty).
func writes(t *testing.T, r *os.File) [][]byte {
	t.Helper()
	var out [][]byte
	fd := int(r.Fd())
	unix.SetNonblock(fd, true)
	for {
		b := make([]byte, 70000)
		n, err := unix.Read(fd, b)
		if err != nil || n <= 0 {
			return out
		}
		out = append(out, b[:n])
	}
}

// seg: an IPv4 TCP segment 10.66.0.1:5400 -> 10.66.0.2:40000 with a timestamp option (NOP NOP TS) when ts != 0.
func seg(seq uint32, payload []byte, flags byte, ts uint32, sport uint16) []byte {
	thl := 20
	if ts != 0 {
		thl = 32
	}
	b := make([]byte, 20+thl+len(payload))
	b[0], b[8], b[9] = 0x45, 64, 6
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)))
	binary.BigEndian.PutUint16(b[4:], uint16(seq)) // IP id
	b[6] = 0x40                                    // DF
	copy(b[12:], []byte{10, 66, 0, 1, 10, 66, 0, 2})
	binary.BigEndian.PutUint16(b[10:], ^csumFold(csumAdd(0, b[:20])))
	th := b[20:]
	binary.BigEndian.PutUint16(th[0:], sport)
	binary.BigEndian.PutUint16(th[2:], 40000)
	binary.BigEndian.PutUint32(th[4:], seq)
	binary.BigEndian.PutUint32(th[8:], 777) // ack
	th[12], th[13] = byte(thl/4)<<4, flags
	binary.BigEndian.PutUint16(th[14:], 501) // window
	if ts != 0 {
		copy(th[20:], []byte{1, 1, 8, 10})
		binary.BigEndian.PutUint32(th[24:], ts)
		binary.BigEndian.PutUint32(th[28:], 99)
	}
	copy(th[thl:], payload)
	return b
}

func pay(n int, x byte) []byte { return bytes.Repeat([]byte{x}, n) }

// deliverBatch: the packets as one receive batch (recvLoop's groBegin / groEnd around out()).
func deliverBatch(p *peer, pkts ...[]byte) {
	p.groBegin()
	p.rxMu.Lock()
	for _, pk := range pkts {
		c := getRx(len(pk))
		copy(c, pk)
		p.out(c)
	}
	p.rxMu.Unlock()
	p.groEnd()
}

func TestGROCoalescesInOrderSegments(t *testing.T) {
	p, r := groPeer(t)
	var segs [][]byte
	seq := uint32(1000)
	for i := 0; i < 5; i++ {
		n := 1200
		fl := byte(0x10)
		if i == 4 {
			n, fl = 300, 0x18 // the last one shorter, with PSH
		}
		segs = append(segs, seg(seq, pay(n, byte('a'+i)), fl, 5000+uint32(i/2), 5400)) // TSval goes 5000, 5000, 5001...
		seq += uint32(n)
	}
	deliverBatch(p, segs...)
	w := writes(t, r)
	if len(w) != 1 {
		t.Fatalf("%d writes, want 1 super-packet", len(w))
	}
	h, ip := w[0][:vnetHdrLen], w[0][vnetHdrLen:]
	if h[0] != vnetNeedCsum || h[1] != vnetGSOTCPv4 || binary.NativeEndian.Uint16(h[2:]) != 52 ||
		binary.NativeEndian.Uint16(h[4:]) != 1200 || binary.NativeEndian.Uint16(h[6:]) != 20 || binary.NativeEndian.Uint16(h[8:]) != 16 {
		t.Fatalf("virtio header % x", h)
	}
	if int(binary.BigEndian.Uint16(ip[2:])) != len(ip) || len(ip) != 52+4*1200+300 {
		t.Fatalf("IP total %d, len %d", binary.BigEndian.Uint16(ip[2:]), len(ip))
	}
	if csumFold(csumAdd(0, ip[:20])) != 0xffff {
		t.Fatal("IP header checksum wrong")
	}
	ps := csumFold(csumAdd(0, ip[12:20]) + 6 + uint32(len(ip)-20))
	if binary.BigEndian.Uint16(ip[36:]) != ps {
		t.Fatalf("TCP check %04x, want the pseudo-header sum %04x", binary.BigEndian.Uint16(ip[36:]), ps)
	}
	if ip[33] != 0x18 || binary.BigEndian.Uint32(ip[24:]) != 1000 || binary.BigEndian.Uint32(ip[44:]) != 5000 {
		t.Fatalf("flags %02x seq %d tsval %d", ip[33], binary.BigEndian.Uint32(ip[24:]), binary.BigEndian.Uint32(ip[44:]))
	}
	var want []byte
	for _, s := range segs {
		want = append(want, s[52:]...)
	}
	if !bytes.Equal(ip[52:], want) {
		t.Fatal("payload of the super-packet is not the segments' payloads in order")
	}
	if p.gro.writes != 1 || p.gro.segsTot != 5 || p.gro.supers != 1 {
		t.Fatalf("counters %d %d %d", p.gro.writes, p.gro.segsTot, p.gro.supers)
	}
}

func TestGROKeepsOrderAndBoundaries(t *testing.T) {
	p, r := groPeer(t)
	a1 := seg(1000, pay(1000, 'a'), 0x10, 7, 5400)
	a2 := seg(2000, pay(1000, 'b'), 0x10, 7, 5400)
	gap := seg(4000, pay(1000, 'c'), 0x10, 7, 5400)   // not contiguous: a new super-packet
	other := seg(5000, pay(1000, 'd'), 0x10, 7, 5401) // another flow
	udp := []byte{0x45, 0, 0, 28, 0, 0, 0, 0, 64, 17, 0, 0, 10, 66, 0, 1, 10, 66, 0, 2, 0, 53, 0, 53, 0, 8, 0, 0}
	back := seg(6000, pay(1000, 'e'), 0x10, 6, 5401) // timestamp goes back: not appended
	syn := seg(9000, pay(10, 'f'), 0x12, 7, 5400)     // SYN|ACK: never coalesced
	deliverBatch(p, a1, a2, gap, other, udp, back, syn)
	w := writes(t, r)
	var got [][]byte
	for _, x := range w {
		got = append(got, x[vnetHdrLen:])
	}
	if len(got) != 6 {
		t.Fatalf("%d writes, want 6", len(got))
	}
	if len(got[0]) != 52+2000 || !bytes.Equal(got[1], gap) || !bytes.Equal(got[2], other) || !bytes.Equal(got[3], udp) ||
		!bytes.Equal(got[4], back) || !bytes.Equal(got[5], syn) {
		t.Fatal("order or boundaries wrong")
	}
	for i := 1; i < 6; i++ {
		if !bytes.Equal(w[i][:vnetHdrLen], make([]byte, vnetHdrLen)) {
			t.Fatalf("write %d: a single packet needs a zero virtio header", i)
		}
	}
}

func TestGROOutsideBatchWritesAtOnce(t *testing.T) {
	p, r := groPeer(t)
	s1 := seg(1000, pay(1000, 'a'), 0x10, 0, 5400)
	s2 := seg(2000, pay(1000, 'b'), 0x10, 0, 5400)
	p.rxMu.Lock() // a timer path (releaseDue, liveWatch): no receive batch around it
	p.out(append([]byte(nil), s1...))
	p.out(append([]byte(nil), s2...))
	p.rxMu.Unlock()
	w := writes(t, r)
	if len(w) != 2 || !bytes.Equal(w[0][vnetHdrLen:], s1) || !bytes.Equal(w[1][vnetHdrLen:], s2) {
		t.Fatalf("%d writes: a packet outside a receive batch must be written at once", len(w))
	}
}

func TestGROLimits(t *testing.T) {
	p, r := groPeer(t)
	var segs [][]byte
	for i := 0; i < 60; i++ {
		segs = append(segs, seg(uint32(1000+1400*i), pay(1400, byte(i)), 0x10, 1, 5400))
	}
	deliverBatch(p, segs...)
	w := writes(t, r)
	total := 0
	for _, x := range w {
		ip := x[vnetHdrLen:]
		if len(ip) > groMaxBytes {
			t.Fatalf("super-packet of %d bytes", len(ip))
		}
		total += len(ip) - 52 // IP + TCP with the timestamp option
	}
	if total != 60*1400 || len(w) != 2 {
		t.Fatalf("%d writes, %d payload bytes", len(w), total)
	}
}

func TestGROOffIsPlain(t *testing.T) {
	p, r := groPeer(t)
	tunGRO = false
	s1 := seg(1000, pay(1000, 'a'), 0x10, 0, 5400)
	s2 := seg(2000, pay(1000, 'b'), 0x10, 0, 5400)
	deliverBatch(p, s1, s2)
	w := writes(t, r)
	if len(w) != 2 || !bytes.Equal(w[0], s1) || !bytes.Equal(w[1], s2) {
		t.Fatal("-tungro=false must write each packet as it is, without a virtio header")
	}
}

func TestGROTunReadStripsHeader(t *testing.T) {
	p, r := groPeer(t)
	pk := seg(1, pay(100, 'x'), 0x10, 0, 5400)
	w := p.tun
	p.tun = r
	go w.Write(append(make([]byte, vnetHdrLen), pk...))
	buf := make([]byte, 2000)
	got, err := p.tunRead(buf)
	if err != nil || !bytes.Equal(got, pk) {
		t.Fatalf("tunRead: %v, %d bytes", err, len(got))
	}
}
