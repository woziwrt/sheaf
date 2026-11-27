// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

// Wire trailer on every datagram: session u32 + echo u32 + tag u64.
//  - session = random id of the sending process (new at every start).
//  - echo = the receiver's session as the sender knows it (0 = not known yet). A datagram whose echo is not
//    0 and not our own session was meant for a previous incarnation of us: its content (sequence numbers,
//    acks, stream frames) belongs to a state we no longer have, so it is never processed – we only learn
//    who the peer is and where it is (so we can answer and make it reset).
//  - a different session of the peer (not one it retired) = the peer restarted -> reset everything, retire
//    the old id: its delayed packets are dropped. No clocks involved (a box without RTC may boot in the past).
//  - tag = SipHash-1-3(key, datagram || session || echo) when -key is set (cheap on a CPU without crypto
//    instructions; confidentiality comes later from IPsec). Without a key the tag is 0 and not checked (lab only).
//  - -auth (v62) chooses the tag: sip = SipHash-1-3 (default), half = HalfSipHash-1-3 (32-bit words: cheaper on the
//    MIPS32 box, for links WITHOUT IPsec), off = no tag (links under IPsec only: ESP's HMAC already authenticates
//    every datagram; the field stays, always 0, and must be 0). Both ends must agree: a mismatch rejects every
//    datagram with a rate-limited "auth mismatch?" log line - never a silent misparse.
//  - an authenticated datagram from a new source address updates the link's peer (CGNAT rebinding, new modem IP).

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const trailerLen = 16

var (
	authKey    [2]uint64
	haveKey    bool
	mySession  = randSession()
	sessMu     sync.Mutex
	peerSess   uint32
	havePeerSs bool
	retired    []uint32      // peer sessions we switched away from (their late packets are dropped)
	peerEcho   atomic.Uint32 // = peerSess once known; written into every datagram we send

	halfKey [2]uint32 // -auth=half: the HalfSipHash key (halfKeyFrom)

	authMode    = authSip     // -auth (v62)
	authRejects atomic.Uint32 // datagrams dropped by the tag check (status auth_rejects)
	authLogAt   atomic.Int64  // mono ns of the last "auth mismatch?" line
)

// -auth values (v62). off: the links run under IPsec, which authenticates the datagrams; the tag field is sent as 0
// and a non-zero one is rejected (the peer runs sip or half).
const (
	authSip  = iota // SipHash-1-3 (up to v61 the only one)
	authHalf        // HalfSipHash-1-3, 64-bit tag
	authOff         // no tag
)

var authNames = [...]string{"sip", "half", "off"}

// authFlag: -auth=sip|half|off; true and false are accepted for off and sip (the first v62 builds had a bool flag).
// A bool-style flag: a bare -auth means sip, a value needs the -auth=VALUE form.
type authFlag struct{}

func (authFlag) String() string   { return authNames[authMode] }
func (authFlag) IsBoolFlag() bool { return true }
func (authFlag) Set(v string) error {
	switch v {
	case "sip", "true":
		authMode = authSip
	case "half":
		authMode = authHalf
	case "off", "false":
		authMode = authOff
	default:
		return fmt.Errorf("want sip, half or off")
	}
	return nil
}

// useMAC: datagrams carry and require a tag.
func useMAC() bool { return haveKey && authMode != authOff }

// trustSource: an accepted datagram vouches for its source address (address learning): our tag checked it, or
// with -auth=off the IPsec SA it arrived through did.
func trustSource() bool { return useMAC() || authMode == authOff }

// tagOf: the tag algorithm mode (authSip or authHalf) over m.
func tagOf(mode int, m []byte) uint64 {
	if mode == authHalf {
		return halfsiphash13(halfKey[0], halfKey[1], m)
	}
	return siphash13(authKey[0], authKey[1], m)
}

// authStatus: the status JSON's "auth": what the datagrams carry.
func authStatus() string {
	if !useMAC() {
		return "off"
	}
	return authNames[authMode]
}

const authLogEvery = 10 * time.Second

// authReject counts a datagram the tag check dropped and says why, at most once per authLogEvery.
func authReject(l *link, why string) {
	n := authRejects.Add(1)
	now, last := mono(), authLogAt.Load()
	if (last == 0 || now-last >= int64(authLogEvery)) && authLogAt.CompareAndSwap(last, now) {
		log.Printf("L%d: datagram rejected: %s - auth mismatch? Both ends need the same -auth and -key (%d rejected since start)",
			l.id, why, n)
	}
}

// tagOK: the tag check of a received datagram whose trailer starts at b[n:].
func tagOK(l *link, b []byte, n int) bool {
	tag := binary.BigEndian.Uint64(b[n+8:])
	switch {
	case useMAC():
		if tag == tagOf(authMode, b[:n+8]) {
			return true
		}
		other := authHalf + authSip - authMode // a failure costs one more hash: it names the peer's setting
		switch {
		case tag == 0:
			authReject(l, "no tag (the peer runs -auth=off or without -key?)")
		case tag == tagOf(other, b[:n+8]):
			authReject(l, fmt.Sprintf("%s tag where -auth=%s expects %s (the peer runs -auth=%s)",
				authNames[other], authNames[authMode], authNames[authMode], authNames[other]))
		default:
			authReject(l, "bad tag (another -key at the peer?)")
		}
		return false
	case authMode == authOff && tag != 0:
		authReject(l, "a tag where -auth=off expects none (the peer runs -auth=sip or half?)")
		return false
	}
	return true // no key: nothing to check (lab only)
}

func randSession() uint32 {
	var b [4]byte
	for {
		crand.Read(b[:])
		if v := binary.BigEndian.Uint32(b[:]); v != 0 {
			return v
		}
	}
}

func loadKey(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("key: %v", err)
	}
	k, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(k) != 16 {
		log.Fatalf("key: need 32 hex characters (16 bytes) in %s", path)
	}
	authKey[0] = binary.LittleEndian.Uint64(k[0:])
	authKey[1] = binary.LittleEndian.Uint64(k[8:])
	halfKey = halfKeyFrom(k)
	haveKey = true
}

func (l *link) dst() *net.UDPAddr { return l.peerA.Load() }

// wr sends one control datagram (feedback, NACKs, still-missing report) on link l with the trailer. Callers that put
// session state into b hold sessRW.RLock, so the echo matches the state the content was taken from. It never waits
// (v60, sendNow): a datagram the socket cannot take now is dropped and counted; the next one follows within fbEvery.
// v62: not on a link that is long dead while another one lives (l.quiet, v62.go) - the far end gets it over those.
func (p *peer) wr(l *link, b []byte) {
	if l.quiet.Load() {
		return
	}
	if d := l.dst(); d != nil {
		if sendNow(l.conn, seal(b), d) != nil {
			ctlDrops.Add(1)
		}
	}
}

// seal appends the trailer (session + echo + tag); uses spare capacity of b when there is some.
func seal(b []byte) []byte {
	var out []byte
	if cap(b)-len(b) >= trailerLen {
		out = b[:len(b)+trailerLen]
	} else {
		out = make([]byte, len(b)+trailerLen)
		copy(out, b)
	}
	binary.BigEndian.PutUint32(out[len(b):], mySession)
	binary.BigEndian.PutUint32(out[len(b)+4:], peerEcho.Load())
	var tag uint64 // 0 = no tag (-auth=off, or no key)
	if useMAC() {
		tag = tagOf(authMode, out[:len(b)+8])
	}
	binary.BigEndian.PutUint64(out[len(b)+8:], tag)
	return out
}

// rd checks a received datagram; returns its payload length, -1 to drop it, or newSession.
// Called under sessRW.RLock.
func (p *peer) rd(l *link, b []byte, from *net.UDPAddr) int {
	n := len(b) - trailerLen
	if n < 4 {
		return -1
	}
	if !tagOK(l, b, n) {
		return -1
	}
	ss := binary.BigEndian.Uint32(b[n:])
	echo := binary.BigEndian.Uint32(b[n+4:])
	sessMu.Lock()
	if echo != 0 && echo != mySession { // meant for a previous incarnation of us: learn the peer, ignore the content
		if !havePeerSs {
			peerSess, havePeerSs = ss, true
			peerEcho.Store(ss)
		}
		sessMu.Unlock()
		p.learnAddr(l, from)
		return -1
	}
	switch {
	case !havePeerSs:
		peerSess, havePeerSs = ss, true
		peerEcho.Store(ss)
	case ss == peerSess:
	case isRetired(ss): // late packet of a session the peer left
		sessMu.Unlock()
		return -1
	default: // the peer restarted: the caller switches sessions under sessRW.Lock
		sessMu.Unlock()
		return newSession
	}
	sessMu.Unlock()
	p.learnAddr(l, from)
	return n
}

func (p *peer) learnAddr(l *link, from *net.UDPAddr) {
	if trustSource() && from != nil {
		if cur := l.dst(); cur == nil || !cur.IP.Equal(from.IP) || cur.Port != from.Port {
			l.peerA.Store(from)
			log.Printf("L%d peer address now %v", l.id, from)
		}
	}
}

func isRetired(ss uint32) bool { // under sessMu
	for _, r := range retired {
		if r == ss {
			return true
		}
	}
	return false
}

// newSession: rd saw a new session of the peer. Datagrams are processed under sessRW.RLock;
// the switch takes sessRW.Lock, so no datagram of the old session is half-processed while state is reset
// (an old packet delivered into the new state could replace new data with the same sequence number).
const newSession = -2

var sessRW sync.RWMutex

func (p *peer) switchSession(b []byte) {
	ss := binary.BigEndian.Uint32(b[len(b)-trailerLen:])
	sessRW.Lock()
	defer sessRW.Unlock()
	sessMu.Lock()
	sw := havePeerSs && ss != peerSess && !isRetired(ss) // another link may have switched already
	if sw {
		retired = append(retired, peerSess)
		if len(retired) > 8 {
			retired = retired[1:]
		}
		peerSess = ss
		peerEcho.Store(ss)
	}
	sessMu.Unlock()
	if sw {
		p.resetPeer()
	}
}

// resetPeer: the far end restarted – start all counters from zero; its proxy connections are gone, so ours too.
// Called under sessRW.Lock (no datagram is being processed, no txLoop is between its epoch check and send).
// The send state is reset in ONE critical section with the epoch change: nothing can take an old sequence
// number, link number or copy with the new epoch.
func (p *peer) resetPeer() {
	log.Printf("peer restarted (new session) – resetting state")
	for _, l := range p.links { // lock order: l.mu (link order) -> reMu -> seqMu, as in pick/deadWatch
		l.mu.Lock()
	}
	p.reMu.Lock()
	p.seqMu.Lock()
	p.epoch.Add(1)
	p.seq = seqBase
	p.copies, p.copySent, p.copyQ = map[uint32][]byte{}, map[uint32]int64{}, nil
	p.flowHas = [fqBuckets]bool{}
	p.cumAck, p.lastAck, p.ackMovedAt, p.reinject = seqBase, seqBase, time.Now(), nil
	p.due, p.dueSeq = nil, nil
	for _, l := range p.links {
		l.lseq, l.lastSeq, l.hist, l.inflight = seqBase, seqBase, nil, 0
		l.lmap, l.lmapQ = map[uint32]lmapEnt{}, nil
		l.rxLseq, l.hiLseq, l.haveHi, l.haveOwd, l.dead = seqBase, seqBase, false, false, false
		l.quiet.Store(false)
		l.owdMinAt = 0     // the far end's clock restarted: its one-way delays (offset included) start anew
		l.peerLid.Store(0) // the new session may pair the links differently (the far end restarted with other links)
		if l.learn {
			// a learned address belongs to the old session: a link the far end no longer uses kept it, so the
			// scheduler kept sending there - into a NAT mapping and an IPsec SA that were gone (25 Nov: hub
			// 4 -> 2 links, the server's L1/L4 took downlink data, 2 Mbit/s). The new session's packets teach it again
			// (the one that switched the session does so right after this reset).
			l.peerA.Store(nil)
		}
		l.progress = mono()
		if l.freshRegulator() {
			p.drainBusy.Store(false)
		}
		l.txMu.Lock()
		for _, it := range l.txQ {
			l.txqB.Add(-int32(len(it.b)))
		}
		l.txQ = l.txQ[:0]
		l.txMu.Unlock()
		l.paceTok, l.winTarget = 0, 0
	}
	p.seqMu.Unlock()
	p.reMu.Unlock()
	for _, l := range p.links {
		l.mu.Unlock()
	}
	p.rxMu.Lock()
	p.buf, p.skippedSet, p.live, p.waitOn, p.flowOK, p.waitQ = map[uint32][]byte{}, map[uint32]bool{}, nil, nil, nil, nil
	p.next, p.started, p.gapSince = seqBase, reliable, time.Time{}
	p.relFrom, p.rxTop, p.rxSent, p.rxSentSeq = seqBase, seqBase, nil, nil
	p.rxMu.Unlock()
	// streams last: their frames already carry the old epoch and are dropped
	smu.Lock()
	var ss []*stream
	for _, s := range streams {
		ss = append(ss, s)
	}
	smu.Unlock()
	for _, s := range ss {
		p.abort(s, false)
	}
	rstMu.Lock()
	rstSent = map[uint32]bool{}
	rstMu.Unlock()
}

// listenLink opens a link's UDP socket. "host:port" binds an address; "@dev:port" binds to a network
// device (SO_BINDTODEVICE) on all addresses – an LTE modem's address changes with every reconnect.
func listenLink(spec string) (*net.UDPConn, error) {
	if !strings.HasPrefix(spec, "@") {
		la, err := net.ResolveUDPAddr("udp4", spec)
		if err != nil {
			return nil, err
		}
		return net.ListenUDP("udp4", la) // IPv4 only: batch send/receive (ipv4.PacketConn) needs AF_INET sockets
	}
	dev, port, ok := strings.Cut(spec[1:], ":")
	if !ok {
		return nil, fmt.Errorf("bad link %q, want @dev:port", spec)
	}
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, dev)
		})
		if err != nil {
			return err
		}
		return serr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:"+port)
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}
