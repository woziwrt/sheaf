// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Modes: how the bundle uses its links.
//
//	speed      (default) spread over all links, repair losses, deliver in order (TCP ACKs and the proxy's credit
//	           overtake a hole, see noDelay)
//	smooth     as speed, but a UDP packet waits behind a hole at most holdTime (spread of the links' delays plus a
//	           margin): live audio/video gets a missing piece instead of a stall. TCP stays in order.
//	redundant  every packet over every live link, the first copy counts: the speed of the slower link, twice the
//	           data, and a link that fails is not noticed at all
//	backup     only the primary link carries data; the others when it is dead
//
// The box chooses (-mode, or the mode file its status page writes); the server follows (-mode peer). Every feedback
// carries the sender's choice in its 4th byte: 0x80 | primary<<4 | mode. An older peer sends 0 there = nothing said.
// Small important packets (dupWorthy) go over two links in every mode.
const (
	modeSpeed = iota
	modeSmooth
	modeRedundant
	modeBackup
)

var modeNames = [...]string{"speed", "smooth", "redundant", "backup"}

var (
	modeCur    atomic.Uint32 // mode | primary link index << 4
	modeFollow bool          // take the mode from the peer's feedback (the server)
)

func curMode() int    { return int(modeCur.Load() & 0x0f) }
func primaryIdx() int { return int(modeCur.Load() >> 4 & 7) }
func modeByte() byte  { return 0x80 | byte(modeCur.Load()&0x7f) }

func parseMode(s string) (int, bool) {
	for i, n := range modeNames {
		if s == n {
			return i, true
		}
	}
	return 0, false
}

func setMode(m, prim int, why string) {
	v := uint32(m) | uint32(prim)<<4
	if modeCur.Swap(v) != v {
		log.Printf("mode %s, primary link L%d (%s)", modeNames[m], prim+1, why)
	}
}

// onPeerMode: the 4th byte of a feedback.
func onPeerMode(b byte) {
	if !modeFollow || b&0x80 == 0 {
		return
	}
	if m := int(b & 0x0f); m <= modeBackup {
		setMode(m, int(b>>4&7), "peer")
	}
}

// modeFile: "mode [primary link number]", written by the status page (-modefile); polled every second, read
// whenever its modification time changes. A missing file means the default mode (-mode / -primary, normally speed):
// up to v61 deleting it kept the last mode until a restart.
type modeFile struct {
	path    string
	nlinks  int
	defMode int // -1: no default (-mode peer follows the far end)
	defPrim int
	last    time.Time // modification time last read; zero while the file is missing
	missing bool
}

func watchModeFile(mf *modeFile) {
	for ; ; time.Sleep(time.Second) {
		mf.poll()
	}
}

func (mf *modeFile) poll() {
	fi, err := os.Stat(mf.path)
	if errors.Is(err, fs.ErrNotExist) {
		if !mf.missing && mf.defMode >= 0 {
			setMode(mf.defMode, mf.defPrim, mf.path+" missing: default") // logs only when the mode changes
		}
		mf.missing, mf.last = true, time.Time{}
		return
	}
	if err != nil || fi.ModTime().Equal(mf.last) {
		return
	}
	mf.missing, mf.last = false, fi.ModTime()
	b, err := os.ReadFile(mf.path)
	if err != nil {
		return
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return
	}
	m, ok := parseMode(f[0])
	if !ok {
		log.Printf("%s: unknown mode %q", mf.path, f[0])
		return
	}
	prim := 0
	if len(f) > 1 {
		if n, err := strconv.Atoi(f[1]); err == nil && n >= 1 && n <= mf.nlinks && n <= 8 {
			prim = n - 1
		}
	}
	setMode(m, prim, mf.path)
}

func isUDP(pl []byte) bool {
	if len(pl) < 20 {
		return false
	}
	switch pl[0] >> 4 {
	case 4:
		return pl[9] == 17
	case 6:
		return len(pl) >= 40 && pl[6] == 17
	}
	return false
}

// smooth mode, receiver: UDP packets parked behind a hole, released when they waited holdTime.
type liveItem struct {
	seq uint32
	at  int64 // mono ns
}

func (p *peer) liveWatch() {
	for range time.Tick(5 * time.Millisecond) {
		p.rxMu.Lock()
		n := len(p.live)
		p.rxMu.Unlock()
		if n == 0 {
			continue
		}
		hold := int64(p.holdTime()) // takes the link locks: not under rxMu
		now := mono()
		p.rxMu.Lock()
		k := 0
		for _, it := range p.live {
			if int32(it.seq-p.next) < 0 {
				continue // delivered in order meanwhile
			}
			pl, ok := p.buf[it.seq]
			if !ok || pl == nil {
				continue
			}
			if now-it.at < hold {
				p.live[k] = it
				k++
				continue
			}
			p.out(pl)
			p.buf[it.seq] = nil // flush passes it without delivering it twice
			p.liveEarly++
			p.markFlowOK(it.seq)   // smooth mode gave up on the hole for this flow: later packets need not wait
			p.releaseChain(it.seq) // its own flow's later packets may follow now
		}
		p.live = p.live[:k]
		p.rxMu.Unlock()
	}
}

// sendRedundant: a copy over every live link that has window room; the packet waits only while no link has room.
// A full link may be waited for up to redWait (-redwait, default 0) so that it gets its copy too. Measured on real
// LTE (8 Mbit/s steady UDP, two ~16 Mbit/s links): waiting for every link to have room delayed the stream by its
// queue behind the slower window (p99 300 ms; with a 20 ms bound still 240 ms); without waiting p99 52 ms (speed
// mode 71), a 10 s cut of one link lost nothing (worst packet 210 ms late), and a bulk download got 31.6 Mbit/s
// instead of 18.4. The price: when the traffic exceeds the slower link, that link carries only the copies it has
// room for, and a failure of the faster one costs repairs. A full link that has acknowledged nothing for redStall
// is never waited for: a cut link's window stays full until it is declared dead (up to 1 s). Dead links get a copy
// as a probe when they starve. false = no live link: the caller's normal path probes.
func (p *peer) sendRedundant(seq uint32, inner []byte, ep uint32, now int64, full []byte) bool {
	n := len(inner) + hdrLen
	waitUntil := now + int64(redWait)
	var to [8]*link // links that get a copy now
	nto := 0
	for {
		p.pick(n, false, now) // its side effects: dead-link detection, end of a drain
		live, waiting := 0, 0
		nto = 0
		for _, l := range p.links {
			if l.dst() == nil {
				continue
			}
			l.mu.Lock()
			if !l.dead {
				live++
				switch {
				case l.inflight+n <= l.winB && !l.txqFull(n):
					// (v59: a link with a full transmit queue counts as full; its pace may be overdrawn by the copy)
					if nto < len(to) {
						to[nto] = l
						nto++
					}
				case len(l.hist) == 0 || now-l.progress < l.redStallNs: // redStall(srtt), cached by derive (v62)
					waiting++ // full but alive: worth waiting for (up to redWait)
				}
			}
			l.mu.Unlock()
		}
		if live == 0 {
			return false
		}
		if nto > 0 && (waiting == 0 || now >= waitUntil) {
			break
		}
		wWin.Add(1)
		time.Sleep(500 * time.Microsecond)
		now = mono()
	}
	for i, l := range to[:nto] {
		if i == 0 {
			p.sendOn(l, seq, inner, ep, now, full)
		} else {
			p.sendOn(l, seq, inner, ep, now, nil)
			dupSent.Add(1)
		}
	}
	for _, l := range p.links { // probes on dead links
		if l.dst() == nil {
			continue
		}
		l.mu.Lock()
		probe := l.dead && !l.longDead(now) && now-l.lastSend > int64(starveFor) // v62: long dead = quiet probes only
		l.mu.Unlock()
		if probe {
			p.sendOn(l, seq, inner, ep, now, nil)
			dupSent.Add(1)
		}
	}
	return true
}

var redWait time.Duration

// redStall: how long a full link may go without acknowledging anything before redundancy stops waiting for it
// (ns; srtt in µs): half its rtt, at least 80 ms - a hiccup of a working LTE link costs at most a few missing copies.
func redStall(srttUs float64) int64 {
	return max(80_000_000, int64(srttUs*500))
}
