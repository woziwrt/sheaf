// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

// Built-in TCP proxy: a TCP connection is terminated here,
// its bytes travel as frames inside reliable sheaf packets and the far end opens the real connection.
// No TCP runs between the two ends, so only our per-link regulator decides how fast data moves.
//
// Flow control per stream (credit): a sender may have at most streamWin bytes the far end has not yet
// written to its socket. Delivery therefore never blocks the receive path (a slow consumer only slows
// its own stream), and TCP back-pressure reaches the sending application end to end.

import (
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	fOpen  = 1 // data = target "host:port"
	fData  = 2
	fFin   = 3 // sender finished writing (half close)
	fRst   = 4 // abort: error, refused dial, reset
	fWin   = 5 // data = u64 bytes this side has written to its socket (credit for the sender)
	fOpenT = 6 // transparent open: data = flags u8, window u32, port u16, IPv4 or IPv6 address (tproxy.go)
	fLast  = fOpenT
	fHdr   = 7 // kind u8, stream u32, len u16 (not a kind)

	// bytes in flight per stream: bandwidth x delay of the whole bond with margin for repairs. 4 MB capped one stream
	// of four LTE links (~240 Mbit/s, loaded RTT 100+ ms, plus NACK repairs) – the credit ran out first (25 Nov).
	streamWin   = 16 << 20
	winEvery    = streamWin / 16 // credit update after this many bytes written (1 MB): the sender never waits on a stale one
	dialTimeout = 10 * time.Second
)

var serve bool // dial the targets the far end asks for

// frameMax: stream bytes per frame. A full frame + sheaf's header and trailer is a ~1464 B IP packet; under IPsec
// (ESP in UDP: +~55-70 B) that fragments, so -frame lowers it (1328 keeps ESP-in-UDP within 1500).
var frameMax = 1400 - fHdr

var allowTargets map[string]bool // -allow: with -serve, dial only these "host:port" (nil = any)

// qItem: received stream data (d) inside its receive buffer (base, returned to the pool once written), or the FIN.
type qItem struct {
	d, base []byte
	fin     bool
}

type stream struct {
	id    uint32
	ep    uint32 // session epoch the stream was opened in (set with its insertion into streams, under smu)
	conn  net.Conn
	ready chan struct{} // closed when conn is set (or the dial failed)

	mu      sync.Mutex
	cond    *sync.Cond
	waiters int // goroutines in cond.Wait (under mu): onFrame wakes only when someone waits – a wake per frame is
	// a scheduler round trip, and on MIPS32 every one costs 64-bit atomics emulated with a lock
	q     []qItem // received data for the local socket. Bounded by the peer's credit.
	rst   bool
	finTx bool // we sent FIN
	finRx bool // peer's FIN written out
	done  bool

	sent         uint64 // bytes we submitted
	peerConsumed uint64 // bytes the peer wrote to its socket (credit)
	consumed     uint64 // bytes we wrote to our socket
	lastWin      uint64

	win uint64 // credit the peer grants: at most this many of our bytes it has not written yet (streamWin, or
	// the window a transparent OPEN announced); fixed before the reader starts
	rwin uint64   // credit we grant the peer (our receive window): WIN after every rwin/16 bytes written
	tp   *tpState // transparent stream (tproxy.go), nil for a -proxy stream
}

// Stream ids: the opener numbers its streams 1, 3, 5, … (below dirBit). On the wire every frame carries the
// SENDER's key of the stream; the receiver flips dirBit to get its own key. A stream I opened has key k,
// one the peer opened has key k|dirBit – so both ends may open stream 1 at the same time without mixing.
const dirBit = 1 << 31

var (
	smu     sync.Mutex
	streams        = map[uint32]*stream{}
	nextID  uint32 = 1
)

func newStream(id uint32) *stream {
	s := &stream{id: id, ready: make(chan struct{}), win: streamWin, rwin: streamWin}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// frame builds a proxy frame in a packet buffer (see newPkt: the data header and trailer go around it without a copy).
func frame(kind byte, id uint32, data []byte) []byte {
	pkt := newPkt(fHdr + len(data))
	b := pkt[hdrLen:]
	b[0] = kind
	binary.BigEndian.PutUint32(b[1:], id)
	binary.BigEndian.PutUint16(b[5:], uint16(len(data)))
	copy(b[fHdr:], data)
	return pkt
}

// out hands a delivered inner packet to its consumer: IP packets to the TUN, frames to the proxy.
// It runs under the receive lock and must never block.
func (p *peer) out(pl []byte) {
	if len(pl) == 0 {
		return
	}
	switch v := pl[0] >> 4; {
	case v == 4 || v == 6:
		p.tunWrite(pl) // v61 -tungro may coalesce it with the next segments of its flow
	case pl[0] >= fOpen && pl[0] <= fLast && len(pl) >= fHdr:
		if p.onFrame(pl) {
			return // a stream queued its data: the writer returns the buffer
		}
	}
	putRx(pl)
}

func tune(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		t.SetNoDelay(true)
		t.SetReadBuffer(1 << 20)
		t.SetWriteBuffer(1 << 20)
	}
}

func (p *peer) proxyClient(listen, target string) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		log.Fatalf("proxy listen: %v", err)
	}
	log.Printf("proxy %s -> far end %s", listen, target)
	for {
		c, err := ln.Accept()
		if err != nil {
			time.Sleep(100 * time.Millisecond) // EMFILE and friends: do not spin at 100 % CPU
			continue
		}
		tune(c)
		smu.Lock()
		id := nextID
		nextID += 2
		s := newStream(id)
		s.ep = p.epoch.Load()
		s.conn = c
		close(s.ready)
		streams[id] = s
		smu.Unlock()
		p.submitEp(frame(fOpen, id, []byte(target)), s.ep) // before any data of this stream
		go p.writer(s)
		go p.reader(s)
	}
}

// reader: local socket -> frames, only while the peer has given credit. Reads big chunks (one syscall
// for up to 16 kB) and cuts them into frames – a syscall per frame was a large part of the CPU on MIPS.
func (p *peer) reader(s *stream) {
	<-s.ready
	buf := make([]byte, 16<<10)
	for {
		s.mu.Lock()
		for s.sent-s.peerConsumed >= s.win && !s.rst {
			wCredit.Add(1)
			s.waiters++
			s.cond.Wait()
			s.waiters--
		}
		rst := s.rst
		room := s.win - (s.sent - s.peerConsumed)
		s.mu.Unlock()
		if rst {
			return
		}
		want := len(buf)
		if uint64(want) > room {
			want = int(room)
		}
		n, err := s.conn.Read(buf[:want])
		if n > 0 && s.tp != nil {
			s.tp.touch()
		}
		rdN.Add(1)
		rdBytes.Add(uint32(n))
		for off := 0; off < n; off += frameMax {
			end := off + frameMax
			if end > n {
				end = n
			}
			s.mu.Lock()
			s.sent += uint64(end - off)
			s.mu.Unlock()
			p.submitEp(frame(fData, s.id, buf[off:end]), s.ep)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				p.submitEp(frame(fFin, s.id, nil), s.ep)
				s.mu.Lock()
				s.finTx = true
				s.mu.Unlock()
				p.maybeClose(s)
			} else {
				p.abort(s, true) // reset / error is NOT a clean end: tell the peer
			}
			return
		}
	}
}

// writer: received frames -> local socket; everything queued goes out in ONE writev; returns credit as it writes
func (p *peer) writer(s *stream) {
	<-s.ready
	var bases [][]byte
	for {
		s.mu.Lock()
		for len(s.q) == 0 && !s.rst {
			s.waiters++
			s.cond.Wait()
			s.waiters--
		}
		if s.rst {
			s.mu.Unlock()
			return
		}
		var bufs net.Buffers
		bases = bases[:0]
		total, fin := 0, false
		for len(s.q) > 0 && total < 256<<10 {
			it := s.q[0]
			s.q[0] = qItem{}
			s.q = s.q[1:]
			if it.fin {
				fin = true
				break
			}
			bufs = append(bufs, it.d)
			bases = append(bases, it.base)
			total += len(it.d)
		}
		s.mu.Unlock()
		if total > 0 {
			_, err := bufs.WriteTo(s.conn)
			for i, b := range bases { // written (or the stream dies): the receive buffers go back to the pool
				putRx(b)
				bases[i] = nil
			}
			if err != nil {
				p.abort(s, true)
				return
			}
			s.mu.Lock()
			s.consumed += uint64(total)
			send := s.consumed-s.lastWin >= s.rwin/16 // winEvery for a -proxy stream
			if send {
				s.lastWin = s.consumed
			}
			c := s.consumed
			s.mu.Unlock()
			if s.tp != nil {
				s.tp.touch()
			}
			if send {
				w := make([]byte, 8)
				binary.BigEndian.PutUint64(w, c)
				p.submitEp(frame(fWin, s.id, w), s.ep)
			}
		}
		if fin {
			if t, ok := s.conn.(*net.TCPConn); ok {
				t.CloseWrite()
			}
			s.mu.Lock()
			s.finRx = true
			s.mu.Unlock()
			p.maybeClose(s)
			return
		}
	}
}

// forget removes s from the map – only if the id still belongs to s (after a session reset a new stream
// may have the same id).
func forget(s *stream) {
	smu.Lock()
	if streams[s.id] == s {
		delete(streams, s.id)
	}
	smu.Unlock()
	tpRelease(s) // a transparent stream gives back its slot and its receive window
}

func (p *peer) maybeClose(s *stream) {
	s.mu.Lock()
	fin := s.finTx && s.finRx && !s.done
	if fin {
		s.done = true
	}
	s.mu.Unlock()
	if fin {
		s.conn.Close()
		forget(s)
	}
}

// unknownFrames counts data frames that arrived for a stream we do not have: 0, except the frames still in flight
// when this side aborted a stream (a LAN client that closes in the middle of a download).
var unknownFrames atomic.Uint32

var (
	rstMu   sync.Mutex
	rstSent = map[uint32]bool{}
)

// rstOnce: true the first time for this stream id (bounded memory).
func rstOnce(id uint32) bool {
	rstMu.Lock()
	defer rstMu.Unlock()
	if rstSent[id] {
		return false
	}
	if len(rstSent) > 4096 {
		rstSent = map[uint32]bool{}
	}
	rstSent[id] = true
	return true
}

// abort ends a stream at once (RST to the local socket); notify = also tell the far end.
func (p *peer) abort(s *stream, notify bool) {
	s.mu.Lock()
	already := s.rst
	s.rst = true
	s.cond.Broadcast()
	sent, consumed := s.sent, s.consumed
	conn := s.conn
	s.mu.Unlock()
	if already {
		return
	}
	log.Printf("proxy: stream %d aborted (notify=%v, sent %d B, written %d B)", s.id, notify, sent, consumed)
	if t, ok := conn.(*net.TCPConn); ok {
		t.SetLinger(0)
	}
	if conn != nil {
		conn.Close()
	}
	forget(s)
	if notify {
		rstOnce(s.id) // the far end gets this RST: its frames still in flight need no second one (nor a log line)
		p.submitEp(frame(fRst, s.id, nil), s.ep)
	}
}

// onFrame handles one received frame; kept = its data now waits in a stream queue (pl must not be reused yet).
func (p *peer) onFrame(pl []byte) (kept bool) {
	kind := pl[0]
	id := binary.BigEndian.Uint32(pl[1:]) ^ dirBit // the sender's key -> ours
	n := int(binary.BigEndian.Uint16(pl[5:]))
	if fHdr+n > len(pl) {
		return
	}
	data := pl[fHdr : fHdr+n]
	smu.Lock()
	s := streams[id]
	if kind == fOpenT && s == nil {
		s = p.tpAccept(id, data) // under smu; nil = refused (RST already on its way)
		smu.Unlock()
		return
	}
	if kind == fOpen && s == nil {
		if !serve || (allowTargets != nil && !allowTargets[string(data)]) {
			smu.Unlock()
			log.Printf("proxy: OPEN %d to %q refused (no -serve or not in -allow)", id, data)
			go p.submitEp(frame(fRst, id, nil), p.epoch.Load()) // not under the receive lock; epoch taken now
			return
		}
		s = newStream(id)
		s.ep = p.epoch.Load()
		streams[id] = s
		target := string(data)
		go func() {
			c, err := net.DialTimeout("tcp", target, dialTimeout)
			if err != nil {
				log.Printf("proxy: dial %s: %v", target, err)
				close(s.ready)
				p.abort(s, true)
				return
			}
			tune(c)
			s.mu.Lock()
			s.conn = c
			aborted := s.rst // aborted while dialing: abort saw no connection to close
			s.mu.Unlock()
			close(s.ready)
			if aborted {
				c.Close()
				return
			}
			go p.reader(s)
		}()
		go p.writer(s)
	}
	smu.Unlock()
	if s == nil {
		if kind == fData || kind == fFin { // the far end thinks this stream lives: tell it, or it waits for credit forever
			unknownFrames.Add(1)
			if rstOnce(id) {
				log.Printf("proxy: frame for unknown stream %d -> RST", id)
				go p.submitEp(frame(fRst, id, nil), p.epoch.Load()) // not under the receive lock; epoch taken now
			}
		}
		return
	}
	if s.tp != nil {
		s.tp.heard.Store(true)
	}
	s.mu.Lock()
	switch kind {
	case fData:
		s.q = append(s.q, qItem{d: data, base: pl})
		kept = true
	case fFin:
		s.q = append(s.q, qItem{fin: true})
	case fWin:
		if len(data) >= 8 {
			if c := binary.BigEndian.Uint64(data); c > s.peerConsumed {
				s.peerConsumed = c
			}
		}
	}
	if s.waiters > 0 {
		s.cond.Broadcast()
	}
	s.mu.Unlock()
	if kind == fRst {
		go p.abort(s, false) // abort closes sockets: not under the receive lock
	}
	return kept
}
