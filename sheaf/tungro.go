// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"sync/atomic"
	"syscall"
)

// -tungro: coalesce consecutive TCP segments of one flow into one GSO super-packet per TUN write (v61).
//
// Why: on the box (RBM33G, MIPS) the tunnel delivered ~20 Mbit/s where the proxy over the same links delivered
// 60-68. The box was CPU-bound in both cases, but a tunnel packet costs ~3-4x the CPU of a proxy frame: every IP
// packet is its own write(2) to the TUN, and the kernel runs the whole receive path (netfilter, conntrack, NAT,
// routing, the LAN bridge or the local TCP) inside that syscall, once per packet. The proxy writes the same bytes
// to its socket in large writev calls. A box that cannot read its sockets fast enough leaves the packets queued
// there; the server sees that queue as the links' delay (its delivery rate falls, srtt rises) and its window cap
// (rateMax × (base + queue)) then holds the windows at what the box drains – the regulator follows the box.
// Lab, receiver sheaf limited to 10 % of a core: tun.Write was 47 % of its CPU (the proxy's writev 24 %).
//
// How: the TUN is opened with IFF_VNET_HDR (every read and write carries a 10-byte virtio_net_hdr). While a
// receive batch (one recvmmsg) is being delivered, in-order IPv4 TCP segments of the same flow (same addresses,
// ports, ack, window, flags ACK[/PSH], contiguous sequence numbers, equal segment size, options equal except a
// non-decreasing timestamp value) are copied into one buffer and written once as a TCPv4 GSO packet with a
// partial checksum; the kernel's GSO splits it again where it is forwarded (or the local TCP takes it whole, as
// a GRO packet). Everything else, and every packet delivered outside a receive batch (timers), is written as
// before – after what is pending, so the order on the TUN stays the delivery order. Offloads for the read
// direction stay off (TUNSETOFFLOAD not used): the kernel still hands us plain packets.
// A GSO write the kernel refuses switches the coalescing off for good (that super-packet is lost; TCP repairs it).

var tunGRO bool

const (
	vnetHdrLen   = 10    // struct virtio_net_hdr
	groMaxBytes  = 64000 // IP total length of a super-packet (the field holds 65535)
	groMaxSegs   = 44
	vnetNeedCsum = 1 // VIRTIO_NET_HDR_F_NEEDS_CSUM
	vnetGSOTCPv4 = 1 // VIRTIO_NET_HDR_GSO_TCPV4
)

var groOff atomic.Bool // the kernel refused a GSO write: plain writes from now on

func init() {
	flag.BoolVar(&tunGRO, "tungro", false, "v61: TUN with virtio-net header; coalesce in-order TCP segments of a receive batch into one GSO write (fewer syscalls and kernel passes on a slow receiver)")
}

func tunGROSummary() string { return fmt.Sprintf("tungro=%v", tunGRO && !groOff.Load()) }

// groState: the super-packet being built (under rxMu, like out()).
type groState struct {
	batch  int    // receive batches being delivered now (recvLoop); out() coalesces only inside one
	buf    []byte // vnetHdrLen + IP packet being built
	n      int    // bytes used in buf (0 = nothing pending)
	segs   int    // segments in it
	mss    int    // payload of its first segment
	ihl    int    // its IP header length
	thl    int    // its TCP header length
	nextSq uint32 // sequence number the next segment must start with
	tsv    uint32 // timestamp value of the last segment (when hasTS)
	tsOff  int    // offset of the timestamp option's TSval within the TCP header, 0 = none
	done   bool   // the last segment had PSH or was shorter than mss: no more segments
	// counters (status)
	writes, segsTot, supers int64
}

// tunRead reads one packet from the TUN into buf and returns it (without the virtio header when -tungro).
func (p *peer) tunRead(buf []byte) ([]byte, error) {
	n, err := p.tun.Read(buf)
	if err != nil {
		return nil, err
	}
	if !tunGRO {
		return buf[:n], nil
	}
	if n < vnetHdrLen {
		return buf[:0], nil
	}
	return buf[vnetHdrLen:n], nil
}

// groBegin / groEnd: recvLoop delivers one batch of datagrams between them; groEnd writes what is pending.
func (p *peer) groBegin() {
	if !tunGRO {
		return
	}
	p.rxMu.Lock()
	p.gro.batch++
	p.rxMu.Unlock()
}

func (p *peer) groEnd() {
	if !tunGRO {
		return
	}
	p.rxMu.Lock()
	p.gro.batch--
	p.groFlush() // the other link's batch may still run: what is pending now goes out now (bounded delay)
	p.rxMu.Unlock()
}

// tunWrite (under rxMu): hand one IP packet to the TUN – appended to the pending super-packet, or written.
func (p *peer) tunWrite(pl []byte) {
	if !tunGRO {
		p.tun.Write(pl)
		return
	}
	g := &p.gro
	if g.buf == nil {
		g.buf = make([]byte, vnetHdrLen+groMaxBytes)
	}
	if g.batch > 0 && !groOff.Load() {
		if g.n > 0 && g.append(pl) {
			return
		}
		p.groFlush()
		if g.start(pl) {
			return
		}
	} else {
		p.groFlush()
	}
	// a plain packet: a zero virtio header in front
	clear(g.buf[:vnetHdrLen])
	copy(g.buf[vnetHdrLen:], pl)
	p.tun.Write(g.buf[:vnetHdrLen+len(pl)])
	g.writes++
	g.segsTot++
}

// tcpSeg: the fields of an IPv4 TCP segment that coalescing looks at; ok = a candidate (plain data segment).
type tcpSeg struct {
	ihl, thl, plen int
	seq            uint32
	flags          byte
	tsOff          int // TSval offset in the TCP header (NOP NOP TS layout at its usual places), 0 = none
}

func parseSeg(pl []byte) (s tcpSeg, ok bool) {
	if len(pl) < 40 || pl[0]>>4 != 4 || pl[9] != 6 {
		return s, false
	}
	s.ihl = int(pl[0]&0x0f) * 4
	if s.ihl != 20 { // no IP options
		return s, false
	}
	tot := int(binary.BigEndian.Uint16(pl[2:]))
	if tot != len(pl) || binary.BigEndian.Uint16(pl[6:])&0x3fff != 0 { // whole packet, not a fragment
		return s, false
	}
	s.thl = int(pl[s.ihl+12]>>4) * 4
	if s.thl < 20 || s.ihl+s.thl > tot {
		return s, false
	}
	s.plen = tot - s.ihl - s.thl
	s.flags = pl[s.ihl+13]
	if s.plen == 0 || s.flags&^0x08 != 0x10 { // data with ACK (and maybe PSH) only: no SYN/FIN/RST/URG/ECE/CWR
		return s, false
	}
	s.seq = binary.BigEndian.Uint32(pl[s.ihl+4:])
	th := pl[s.ihl:]
	switch { // timestamp option: NOP NOP TS(8,10) right after the fixed header, or TS first
	case s.thl >= 32 && th[20] == 1 && th[21] == 1 && th[22] == 8 && th[23] == 10:
		s.tsOff = 24
	case s.thl >= 30 && th[20] == 8 && th[21] == 10:
		s.tsOff = 22
	}
	return s, true
}

// start: begin a super-packet with pl (nothing pending). false = not a candidate (the caller writes it plainly).
func (g *groState) start(pl []byte) bool {
	s, ok := parseSeg(pl)
	if !ok {
		return false
	}
	copy(g.buf[vnetHdrLen:], pl)
	g.n = vnetHdrLen + len(pl)
	g.segs, g.mss, g.ihl, g.thl, g.tsOff = 1, s.plen, s.ihl, s.thl, s.tsOff
	g.nextSq = s.seq + uint32(s.plen)
	g.done = s.flags&0x08 != 0
	if s.tsOff > 0 {
		g.tsv = binary.BigEndian.Uint32(pl[s.ihl+s.tsOff:])
	}
	return true
}

// append: pl continues the pending super-packet (then it is copied in); false = it does not.
func (g *groState) append(pl []byte) bool {
	if g.done || g.segs >= groMaxSegs {
		return false
	}
	s, ok := parseSeg(pl)
	if !ok || s.ihl != g.ihl || s.thl != g.thl || s.tsOff != g.tsOff || s.plen > g.mss || s.seq != g.nextSq {
		return false
	}
	h := g.buf[vnetHdrLen:]
	if g.n+s.plen > vnetHdrLen+groMaxBytes {
		return false
	}
	// IP: version/ihl, TOS, flags (DF), TTL, protocol, addresses; TCP: ports, ack, window, urgent pointer
	if pl[0] != h[0] || pl[1] != h[1] || pl[6] != h[6] || pl[8] != h[8] || pl[9] != h[9] ||
		string(pl[12:20]) != string(h[12:20]) {
		return false
	}
	t, u := pl[s.ihl:], h[g.ihl:]
	if string(t[0:4]) != string(u[0:4]) || string(t[8:12]) != string(u[8:12]) || t[13]&^0x08 != u[13]&^0x08 ||
		string(t[14:16]) != string(u[14:16]) || string(t[18:20]) != string(u[18:20]) {
		return false
	}
	// options equal, except a timestamp value that does not go back (the super-packet keeps the first one: the
	// receiver echoes a value at most a few ms older, PAWS only rejects values that go back)
	if s.tsOff > 0 {
		v := binary.BigEndian.Uint32(t[s.tsOff:])
		if int32(v-g.tsv) < 0 || string(t[20:s.tsOff]) != string(u[20:s.tsOff]) ||
			string(t[s.tsOff+4:s.thl]) != string(u[s.tsOff+4:g.thl]) {
			return false
		}
		g.tsv = v
	} else if string(t[20:s.thl]) != string(u[20:g.thl]) {
		return false
	}
	copy(g.buf[g.n:], pl[s.ihl+s.thl:])
	g.n += s.plen
	g.segs++
	g.nextSq += uint32(s.plen)
	if s.flags&0x08 != 0 {
		u[13] |= 0x08 // PSH: the super-packet carries it, and it ends here
		g.done = true
	}
	if s.plen < g.mss {
		g.done = true
	}
	return true
}

// groFlush (under rxMu): write the pending super-packet (a single segment as it came).
func (p *peer) groFlush() {
	g := &p.gro
	if g.n == 0 {
		return
	}
	n, segs := g.n, g.segs
	g.n = 0
	hdr, ip := g.buf[:vnetHdrLen], g.buf[vnetHdrLen:n]
	clear(hdr)
	if segs > 1 {
		tot := len(ip)
		binary.BigEndian.PutUint16(ip[2:], uint16(tot))
		ip[10], ip[11] = 0, 0
		binary.BigEndian.PutUint16(ip[10:], ^csumFold(csumAdd(0, ip[:g.ihl])))
		// TCP checksum field = the folded pseudo-header sum (CHECKSUM_PARTIAL: the kernel adds the rest)
		tl := tot - g.ihl
		ps := csumAdd(0, ip[12:20]) + 6 + uint32(tl)
		binary.BigEndian.PutUint16(ip[g.ihl+16:], csumFold(ps))
		hdr[0], hdr[1] = vnetNeedCsum, vnetGSOTCPv4
		binary.NativeEndian.PutUint16(hdr[2:], uint16(g.ihl+g.thl)) // hdr_len
		binary.NativeEndian.PutUint16(hdr[4:], uint16(g.mss))       // gso_size
		binary.NativeEndian.PutUint16(hdr[6:], uint16(g.ihl))       // csum_start
		binary.NativeEndian.PutUint16(hdr[8:], 16)                  // csum_offset (TCP checksum)
		g.supers++
	}
	_, err := p.tun.Write(g.buf[:n])
	g.writes++
	g.segsTot += int64(segs)
	if err != nil && segs > 1 && (errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EOPNOTSUPP)) && !groOff.Swap(true) {
		log.Printf("tungro: the TUN refused a GSO write (%v): plain writes from now on", err)
	}
}

// csumAdd: one's complement sum of b (big-endian 16-bit words) added to s, not folded.
func csumAdd(s uint32, b []byte) uint32 {
	for len(b) >= 2 {
		s += uint32(b[0])<<8 | uint32(b[1])
		b = b[2:]
	}
	if len(b) == 1 {
		s += uint32(b[0]) << 8
	}
	return s
}

func csumFold(s uint32) uint16 {
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return uint16(s)
}
