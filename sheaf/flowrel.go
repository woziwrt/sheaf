// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

// Per-flow release: a packet waits only for the earlier packets of its own flow, not for a hole left by another flow.
//
// Up to v52 the receiver delivered strictly in sequence order: a bulk download's lost packet held back a call, a
// camera stream and every other connection until its repair arrived (smooth mode let UDP go after holdTime, which
// under load meant most UDP packets waited ~80 ms). The order that matters is the one inside a flow (TCP, QUIC).
//
// The sender numbers flows by the flow queue's hash (addresses, protocol, ports; flowHash) and writes into every
// data packet the global sequence number of the previous packet of the same flow ("flow predecessor"; the packet's
// own seq when it is the first of its flow). Flag 0x01 in byte 3 of the data header says the field (bytes 20..23,
// before v53 the link's previous seq that no receiver read) holds it; an older sender leaves byte 3 at 0 and is
// delivered strictly in order as before, an older receiver ignores both.
//
// The receiver delivers a packet at once when its predecessor is through: delivered in order, released early, or
// itself the packet (first of its flow). Otherwise the packet waits in buf like before, registered as the successor
// of its predecessor; when that one goes out, the chain follows. buf keeps a nil placeholder for an early packet so
// the in-order pass skips it (as for noDelay). Hash collisions only merge two flows into one stricter chain.
// Proxy frames (not IP) carry no predecessor: they keep the strict order.

const fprevMask = maxCopies - 1 // ring of flow predecessors by seq: a seq is resent only while its copy is kept

var flowRel = true // -flowrel

const hasFlowPrev = 0x01 // data header byte 3: bytes 20..23 hold the flow predecessor

// notePredecessor: at numbering (under reMu): remember the flow predecessor of seq for every (re)transmission.
func (p *peer) notePredecessor(seq uint32, pl []byte) {
	if p.fprev == nil || p.fseq == nil {
		p.fprev, p.fseq = make([]uint64, maxCopies), make([]uint32, maxCopies)
	}
	p.fseq[seq&fprevMask] = seq
	if !flowRel || !isIP(pl) {
		p.fprev[seq&fprevMask] = 0
		return
	}
	b := flowHash(pl)
	fp := seq
	if p.flowHas[b] {
		fp = p.flowLast[b]
	}
	p.flowLast[b], p.flowHas[b] = seq, true
	p.fprev[seq&fprevMask] = 1<<32 | uint64(fp)
}

// predecessor: (flow predecessor, known) of seq for its data header.
func (p *peer) predecessor(seq uint32) (uint32, bool) {
	if p.fprev == nil || p.fseq[seq&fprevMask] != seq { // the slot belongs to another seq: no claim
		return 0, false
	}
	v := p.fprev[seq&fprevMask]
	return uint32(v), v>>32 != 0
}

func isIP(pl []byte) bool { return len(pl) > 0 && (pl[0]>>4 == 4 || pl[0]>>4 == 6) }

// flowReady (under rxMu): may a packet whose flow predecessor is fp go out now? Through = delivered in order or
// released early with its own chain through (flowOK). A noDelay packet (a TCP ACK let out ahead of a hole) leaves
// a nil placeholder too, but its own predecessors may still be missing: it does not count (review finding,
// TestRegNoDelayKeepsFlowOrder).
func (p *peer) flowReady(seq, fp uint32) bool {
	if fp == seq || int32(fp-p.next) < 0 {
		return true
	}
	_, ok := p.flowOK[fp]
	return ok
}

func (p *peer) markFlowOK(seq uint32) {
	if p.flowOK == nil {
		p.flowOK = map[uint32]struct{}{}
	}
	p.flowOK[seq] = struct{}{}
}

// releaseEarly (under rxMu): seq goes out ahead of the in-order point, then every successor waiting on it.
func (p *peer) releaseEarly(seq uint32, pl []byte) {
	p.out(pl)
	p.buf[seq] = nil // the in-order pass skips it
	p.flowEarly++
	p.markFlowOK(seq)
	p.releaseChain(seq)
}

// releaseChain (under rxMu): seq is through - let its waiting successors go.
func (p *peer) releaseChain(seq uint32) {
	for len(p.waitOn) > 0 {
		s, ok := p.waitOn[seq]
		if !ok {
			return
		}
		delete(p.waitOn, seq)
		pl, ok := p.buf[s]
		if !ok || pl == nil {
			return // already out (smooth mode's hold, or in order)
		}
		p.out(pl)
		p.buf[s] = nil
		p.flowEarly++
		p.markFlowOK(s)
		seq = s
	}
}
