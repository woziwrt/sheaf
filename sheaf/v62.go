// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

// v62: the data cost of a dead link. Up to v61 a dead link got a full-size copy of a data packet every
// 500 ms (pick's starvation probe, deadTick's copy of the head of line, redundant mode's copy) and every feedback,
// NACK and still-missing report (every 20 ms while data flows) for as long as it stayed dead: a box with a dead modem
// could burn 100+ MB a day, and on a throttled egress ~12 kB/s of probes queued into the dead link.
//
// Now, once a link has been dead for quietAfter:
//   - it gets no copies of data any more, only a quiet probe every quietProbeEvery: an EMPTY DATA packet (header and
//     trailer, 40 bytes of UDP payload) with a seq the receiver already has (reliable: cumAck-1). The receiver moves
//     the link's numbering (hiLinkSeq, the FEEDBACK report) and nothing else; it answers with one feedback soon even
//     when idle (fbSoon). Its acknowledgement revives the link as before ("Lx back"): the link is back within one
//     probe interval plus a round trip after its path works again.
//   - while another link is alive it gets no control datagrams (feedback, NACKs, still-missing reports): the far end
//     receives them over the live links, so they would only feed the dead one. With no live link at all they go
//     everywhere as before (nobody knows which path works).
// For the first quietAfter nothing changes: a short outage (handover, dead spot) recovers as fast as in v61.
//
// A v61 receiver handles the empty probe as a duplicate of a seq it already delivered (reliable mode); in
// non-reliable mode the seq is the last one sent, which a v61 receiver may not have yet - run v62 on both ends.

import (
	"log"
	"time"
)

const (
	quietAfter      = 30 * time.Second // dead this long: quiet probing, no control datagrams
	quietProbeEvery = 5 * time.Second
	// v62b: the peer sends feedback over every link at least every 2 s. No feedback for fbDeadAfter while another
	// link had some within fbFreshFor = this link is dead even with nothing in flight (an idle or lightly used link
	// was declared dead only by unacknowledged data: 26 Nov, 55 s after its feedback stopped).
	fbDeadAfter = 8 * time.Second
	fbFreshFor  = 4 * time.Second
)

// longDead (under l.mu): dead for quietAfter or longer (now = mono()).
func (l *link) longDead(now int64) bool { return l.dead && now-l.deadAtM >= int64(quietAfter) }

// quietProbe sends a long-dead link its empty probe. The link's in-flight record holds only probes and copies by
// now (its real data went over the other links when it died); it is dropped, so the record does not grow for the
// whole outage, and the acknowledgement of this probe is what revives the link.
func (p *peer) quietProbe(l *link, now int64) {
	l.mu.Lock()
	if !l.quietLog {
		l.quietLog = true
		log.Printf("L%d quiet: dead for %v, an empty probe every %v, no control datagrams while another link lives",
			l.id, quietAfter, quietProbeEvery)
	}
	l.mu.Unlock()
	p.emptyProbe(l, now)
}

// emptyProbe: an empty DATA (a sequence number the receiver already has) on a dead link; its acknowledgement brings
// the link back. Used by quiet probing and, v62b, for a link that went dead without data in flight (no feedback),
// where deadTick's ordinary probe (the oldest unacknowledged packet) has nothing to send.
func (p *peer) emptyProbe(l *link, now int64) {
	p.reMu.Lock()
	ep, seq := p.epoch.Load(), p.cumAck-1 // reliable: the receiver has everything below cumAck
	p.reMu.Unlock()
	if !reliable {
		p.seqMu.Lock()
		seq = p.seq - 1
		p.seqMu.Unlock()
	}
	l.mu.Lock()
	l.hist, l.inflight = nil, 0
	l.mu.Unlock()
	p.sendOnX(l, seq, nil, ep, now, nil, true) // cp: needs no repair, no NACK mapping
}

// quietUpdate (deadTick): long[i] = link i is long dead. A long-dead link gets no control datagrams while another
// link with a known peer address is alive.
func (p *peer) quietUpdate(long []bool, alive int) {
	for i, l := range p.links {
		l.quiet.Store(long[i] && alive > 0)
	}
}
