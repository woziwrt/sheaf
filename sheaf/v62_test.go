// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// -auth: every combination of the two ends' -key / -auth either works or rejects every datagram (counted, logged as
// "auth mismatch?"); with -auth=off the tag field is sent as 0, also from a buffer whose spare room is not zero.
func TestAuthModes(t *testing.T) {
	oldKey, oldHalf, oldHave, oldMode := authKey, halfKey, haveKey, authMode
	defer func() { authKey, halfKey, haveKey, authMode = oldKey, oldHalf, oldHave, oldMode }()
	type end struct {
		key  byte // key file bytes = key, key+1, ...; 0 = no -key
		mode int  // -auth
	}
	setEnd := func(e end) {
		haveKey, authMode = e.key != 0, e.mode
		k := make([]byte, 16)
		for i := range k {
			k[i] = e.key + byte(i)
		}
		authKey = [2]uint64{binary.LittleEndian.Uint64(k), binary.LittleEndian.Uint64(k[8:])}
		halfKey = halfKeyFrom(k)
	}
	sip, sip2, half, half2 := end{1, authSip}, end{7, authSip}, end{1, authHalf}, end{7, authHalf}
	off, lab := end{1, authOff}, end{0, authSip}
	cases := []struct {
		name     string
		tx, rx   end
		accepted bool
	}{
		{"sip on both ends", sip, sip, true},
		{"half on both ends", half, half, true},
		{"sip, different keys", sip, sip2, false},
		{"half, different keys", half, half2, false},
		{"sip -> half", sip, half, false},
		{"half -> sip", half, sip, false},
		{"off on both ends", off, off, true},
		{"sip -> off", sip, off, false},
		{"half -> off", half, off, false},
		{"off -> sip", off, sip, false},
		{"off -> half", off, half, false},
		{"lab (no key) -> off", lab, off, true},
		{"off -> lab (no key)", off, lab, true},
	}
	l := &link{id: 1}
	for _, c := range cases {
		setEnd(c.tx)
		b := make([]byte, 40, 40+trailerLen)
		b[0] = magic
		for i, spare := 40, b[:cap(b)]; i < len(spare); i++ {
			spare[i] = 0xee // the room seal writes the trailer into
		}
		d := seal(b)
		if tag := binary.BigEndian.Uint64(d[len(d)-8:]); (tag != 0) != (c.tx.key != 0 && c.tx.mode != authOff) {
			t.Fatalf("%s: the sender wrote tag %016x", c.name, tag)
		}
		setEnd(c.rx)
		before := authRejects.Load()
		ok := tagOK(l, d, len(d)-trailerLen)
		if ok != c.accepted {
			t.Fatalf("%s: accepted %v, want %v", c.name, ok, c.accepted)
		}
		if rej, want := authRejects.Load()-before, map[bool]uint32{true: 0, false: 1}[ok]; rej != want {
			t.Fatalf("%s: %d rejections counted, want %d", c.name, rej, want)
		}
	}
	haveKey, authMode = false, authOff
	if useMAC() || !trustSource() {
		t.Fatal("-auth=off: no tag, and addresses are learned (the IPsec SA vouches for the source)")
	}
	haveKey, authMode = false, authSip
	if trustSource() {
		t.Fatal("no key: a source address must not be trusted")
	}
}

// -auth takes sip, half, off, and true / false from the first v62 builds; a bare -auth is sip.
func TestAuthFlag(t *testing.T) {
	old := authMode
	defer func() { authMode = old }()
	parse := func(args ...string) error {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.Var(authFlag{}, "auth", "")
		return fs.Parse(args)
	}
	for _, c := range []struct {
		arg  string
		want int
	}{
		{"-auth=half", authHalf}, {"-auth=off", authOff}, {"-auth=false", authOff},
		{"-auth=true", authSip}, {"-auth=sip", authSip}, {"-auth", authSip},
	} {
		authMode = (c.want + 1) % len(authNames) // anything else
		if err := parse(c.arg); err != nil || authMode != c.want {
			t.Fatalf("%s: mode %d err %v, want %d", c.arg, authMode, err, c.want)
		}
	}
	if parse("-auth=md5") == nil {
		t.Fatal("-auth=md5 accepted")
	}
}

// deadSince: l has been dead for d (and was last sent on ago before now).
func deadSince(l *link, now int64, d, ago time.Duration) {
	l.mu.Lock()
	l.dead, l.deadAt, l.deadAtM, l.lastSend = true, time.Now().Add(-d), now-int64(d), now-int64(ago)
	l.mu.Unlock()
}

func txQLens(l *link) []int {
	l.txMu.Lock()
	defer l.txMu.Unlock()
	var n []int
	for _, it := range l.txQ {
		n = append(n, len(it.b))
	}
	return n
}

// A link dead for 30 s gets only an empty probe every 5 s, no data copies (deadTick, pick) and no control
// datagrams while the other link lives; the acknowledgement of a probe revives it at once. A link dead for less
// than 30 s is probed as in v61.
func TestLongDeadLinkGoesQuiet(t *testing.T) {
	old := reliable
	reliable = true
	defer func() { reliable = old }()
	p, l1, l2 := twoLinkPeer(t)
	now := mono()
	p.cumAck = 10
	p.copies[10] = make([]byte, 1000) // the head of line: v61 copied it onto the dead link every 500 ms
	l2.lastSend = now

	// dead for 10 s: the v61 probe (a copy of the head of line), not quiet
	deadSince(l1, now, 10*time.Second, time.Second)
	var w deadState
	p.deadTick(&w, now)
	if q := txQLens(l1); len(q) != 1 || q[0] != hdrLen+1000 || l1.quiet.Load() {
		t.Fatalf("dead 10 s: queued %v quiet %v, want one head-of-line copy", q, l1.quiet.Load())
	}
	l1.txQ, l1.hist = nil, nil

	// dead for 31 s, last send 6 s ago: one empty probe with a seq the receiver has (cumAck-1)
	deadSince(l1, now, 31*time.Second, 6*time.Second)
	p.deadTick(&w, now)
	q := txQLens(l1)
	if len(q) != 1 || q[0] != hdrLen {
		t.Fatalf("dead 31 s: queued %v, want one empty probe of %d B", q, hdrLen)
	}
	if seq := binary.BigEndian.Uint32(l1.txQ[0].b[4:]); seq != 9 {
		t.Fatalf("probe seq %d, want cumAck-1 = 9", seq)
	}
	if !l1.quiet.Load() || l2.quiet.Load() {
		t.Fatalf("quiet L1 %v L2 %v, want L1 only", l1.quiet.Load(), l2.quiet.Load())
	}
	for i := int64(1); i <= 4; i++ { // within the next 5 s: nothing more for L1, whatever the senders do
		at := now + i*int64(time.Second)
		l2.lastSend = at
		p.deadTick(&w, at)
		if c := p.pick(1000, true, at); c == l1 {
			t.Fatal("pick gave a long-dead link a probe")
		}
	}
	if q := txQLens(l1); len(q) != 1 {
		t.Fatalf("more than one probe within 5 s: %v", q)
	}
	p.deadTick(&w, now+int64(5*time.Second))
	if q := txQLens(l1); len(q) != 2 || q[1] != hdrLen || len(l1.hist) != 1 {
		t.Fatalf("after 5 s: queued %v, %d in flight, want a second probe and only it in flight", q, len(l1.hist))
	}

	// control datagrams (here a NACK-RANGE) go over L2 only
	sink := func(l *link) *net.UDPConn {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		l.peerA.Store(c.LocalAddr().(*net.UDPAddr))
		c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		return c
	}
	s1, s2 := sink(l1), sink(l2)
	p.nackRange(1, 5, 1)
	buf := make([]byte, 100)
	if _, err := s2.Read(buf); err != nil {
		t.Fatalf("no NACK over the live link: %v", err)
	}
	if _, err := s1.Read(buf); err == nil {
		t.Fatal("a NACK went over the quiet link")
	}

	// the far end acknowledges the probe: back at once, control datagrams again
	p.onFeedback(lpFeedback(2, []uint32{l1.lseq, 0}, 1))
	if l1.dead || l1.quiet.Load() {
		t.Fatalf("after the probe's ack: dead %v quiet %v", l1.dead, l1.quiet.Load())
	}

	// no live link at all: nobody knows which path works, control datagrams go everywhere
	deadSince(l1, now, 31*time.Second, 0)
	deadSince(l2, now, 31*time.Second, 0)
	p.deadTick(&w, now)
	if l1.quiet.Load() || l2.quiet.Load() {
		t.Fatal("all links dead: none may go quiet")
	}
}

// The receiver takes an empty DATA (a quiet probe) for the link's numbering only: reported in feedback, delivered
// nowhere, and it asks for one feedback at once instead of waking the 20 ms feedback for 200 ms.
func TestQuietProbeReceived(t *testing.T) {
	p, l, _ := twoLinkPeer(t)
	pk := newPkt(0)
	pk[0], pk[1], pk[2] = magic, tData, 1
	binary.BigEndian.PutUint32(pk[4:], 0)
	binary.BigEndian.PutUint32(pk[8:], 1)
	binary.BigEndian.PutUint64(pk[12:], uint64(nowUs()))
	rx := p.lastRxData.Load()
	p.onDatagram(l, seal(pk), nil)
	if !l.haveHi || l.hiLseq != 1 || !p.fbSoon.Load() {
		t.Fatalf("hiLseq %d (have %v), fbSoon %v", l.hiLseq, l.haveHi, p.fbSoon.Load())
	}
	if len(p.buf) != 0 || p.delivered != 0 || p.dups != 0 || p.started || p.lastRxData.Load() != rx {
		t.Fatalf("the probe was taken for data: buf %d delivered %d dups %d started %v", len(p.buf), p.delivered, p.dups, p.started)
	}
}

// A deleted mode file means the default mode on the next poll (v61 kept the last mode until a restart), logged once.
func TestModeFileMissingMeansDefault(t *testing.T) {
	defer setMode(modeSpeed, 0, "test")
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	path := filepath.Join(t.TempDir(), "mode")
	mf := &modeFile{path: path, nlinks: 2, defMode: modeSpeed, defPrim: 0}
	if err := os.WriteFile(path, []byte("backup 2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mf.poll()
	if curMode() != modeBackup || primaryIdx() != 1 {
		t.Fatalf("mode %s primary L%d, want backup L2", modeNames[curMode()], primaryIdx()+1)
	}
	os.Remove(path)
	mf.poll()
	mf.poll()
	if curMode() != modeSpeed || primaryIdx() != 0 {
		t.Fatalf("file gone: mode %s primary L%d, want speed L1", modeNames[curMode()], primaryIdx()+1)
	}
	if n := strings.Count(logs.String(), "missing: default"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, logs.String())
	}
	if err := os.WriteFile(path, []byte("smooth\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mf.poll()
	if curMode() != modeSmooth {
		t.Fatalf("file back: mode %s, want smooth", modeNames[curMode()])
	}
	following := &modeFile{path: path + ".none", nlinks: 2, defMode: -1}
	following.poll()
	if curMode() != modeSmooth {
		t.Fatal("-mode peer: a missing file must not override the far end's choice")
	}
}

// v62b: a link whose feedback stopped is dead even with nothing in flight (as long as another link still hears the
// peer), and gets an empty probe at once - deadTick's ordinary probe has no unacknowledged packet to send.
func TestSilentIdleLinkGoesDead(t *testing.T) {
	old := reliable
	reliable = true
	defer func() { reliable = old }()
	p, l1, l2 := twoLinkPeer(t)
	now := mono()
	p.cumAck = 10
	var w deadState

	// 5 s without feedback: not yet (feedback comes every 2 s; dead after 8 s)
	l1.fbRx, l2.fbRx = time.Now().Add(-5*time.Second), time.Now()
	l1.progress = now - int64(10*time.Second)
	p.deadTick(&w, now)
	if l1.dead {
		t.Fatal("dead after 5 s without feedback")
	}
	// every link silent: a common cause (stall, server), nobody is declared dead for it
	l1.fbRx, l2.fbRx = time.Now().Add(-10*time.Second), time.Now().Add(-10*time.Second)
	p.deadTick(&w, now)
	if l1.dead || l2.dead {
		t.Fatalf("all links silent: dead L1 %v L2 %v, want none", l1.dead, l2.dead)
	}
	// L1 silent for 10 s (no ack either), L2 hears the peer: L1 dead with nothing in flight
	l2.fbRx = time.Now()
	l1.progress = now - int64(10*time.Second)
	p.deadTick(&w, now)
	if !l1.dead || l2.dead {
		t.Fatalf("dead L1 %v L2 %v, want L1 only", l1.dead, l2.dead)
	}
	// next tick, nothing sent on L1 for a while: an empty probe (cumAck-1) brings it back once acknowledged
	l1.lastSend = now - int64(time.Second)
	p.deadTick(&w, now)
	q := txQLens(l1)
	if len(q) != 1 || q[0] != hdrLen {
		t.Fatalf("queued %v, want one empty probe of %d B", q, hdrLen)
	}
	if seq := binary.BigEndian.Uint32(l1.txQ[0].b[4:]); seq != 9 {
		t.Fatalf("probe seq %d, want cumAck-1 = 9", seq)
	}
	p.onFeedback(lpFeedback(2, []uint32{l1.lseq, 0}, 1))
	if l1.dead {
		t.Fatal("still dead after the probe's ack")
	}
	// back by an ack that came over L2, L1's own feedback not here yet: no relapse on the next tick
	p.deadTick(&w, mono())
	if l1.dead {
		t.Fatal("dead again right after coming back")
	}
}
