// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestTpOpenRoundTrip(t *testing.T) {
	for _, s := range []string{"93.184.216.34:443", "[2a00:1450:4001:82b::200e]:80", "[::ffff:1.2.3.4]:8080"} {
		ap := netip.MustParseAddrPort(s)
		d := tpOpenData(1<<20, ap)
		win, got, err := tpParseOpen(d)
		if err != nil || win != 1<<20 || got.Port() != ap.Port() || got.Addr() != ap.Addr().Unmap() {
			t.Errorf("%s: got %v %d %v", s, got, win, err)
		}
		f := frame(fOpenT, 1, d)[hdrLen:]
		if f[0] != fOpenT || int(binary.BigEndian.Uint16(f[5:])) != len(d) || !dupWorthy(f) {
			t.Errorf("%s: frame % x (dupWorthy %v)", s, f, dupWorthy(f))
		}
	}
	for _, d := range [][]byte{nil, make([]byte, 10), make([]byte, 12), make([]byte, 24)} {
		if _, _, err := tpParseOpen(d); err == nil {
			t.Errorf("length %d accepted", len(d))
		}
	}
	if _, _, err := tpParseOpen(tpOpenData(0, netip.MustParseAddrPort("1.2.3.4:80"))); err == nil {
		t.Error("window 0 accepted")
	}
}

func TestTpPolicy(t *testing.T) {
	local := func(a netip.Addr) bool { return a == netip.MustParseAddr("203.0.113.10") }
	cases := []struct {
		allow, deny, dst string
		ok               bool
	}{
		{"", "", "93.184.216.34:443", false}, // no -tallow: nothing
		{"public", "", "93.184.216.34:443", true},
		{"public", "", "[2a00:1450:4001:82b::200e]:443", true},
		{"public", "", "10.1.2.3:80", false},
		{"public", "", "192.168.1.1:80", false},
		{"public", "", "172.20.0.1:80", false},
		{"public", "", "100.64.0.1:80", false},
		{"public", "", "127.0.0.1:22", false},
		{"public", "", "0.0.0.0:22", false},
		{"public", "", "169.254.169.254:80", false}, // cloud metadata
		{"public", "", "10.66.0.1:80", false},       // the tunnel's own net
		{"public", "", "224.0.0.1:80", false},
		{"public", "", "255.255.255.255:80", false},
		{"public", "", "[::1]:22", false},
		{"public", "", "[::ffff:127.0.0.1]:22", false}, // mapped
		{"public", "", "[fd00::1]:22", false},
		{"public", "", "[fe80::1]:22", false},
		{"public", "", "[64:ff9b::a00:1]:22", false},  // NAT64 of 10.0.0.1
		{"public", "", "[2002:a00:1::1]:22", false},   // 6to4 of 10.0.0.1
		{"public", "", "203.0.113.10:443", false}, // the server itself
		{"public", "", "93.184.216.34:0", false},
		{"public", ":25", "93.184.216.34:25", false},
		{"public", ":25", "93.184.216.34:587", true},
		{"public", "93.184.216.0/24", "93.184.216.34:443", false},
		{"public,10.9.0.0/16", "", "10.9.1.1:80", true}, // a configured private target
		{"127.0.0.1", "", "127.0.0.1:5201", true},
		{"127.0.0.1", "", "127.0.0.1:5202", true},
		{"127.0.0.1", "", "93.184.216.34:443", false}, // CIDRs only, no "public"
		{"public,127.0.0.1", ":5202", "127.0.0.1:5202", false},
	}
	for _, c := range cases {
		pol, err := parseTpPolicy(c.allow, c.deny)
		if err != nil {
			t.Fatalf("%q %q: %v", c.allow, c.deny, err)
		}
		if ok, why := pol.check(netip.MustParseAddrPort(c.dst), local); ok != c.ok {
			t.Errorf("allow %q deny %q dst %s: %v (%s), want %v", c.allow, c.deny, c.dst, ok, why, c.ok)
		}
	}
	for _, bad := range [][2]string{{"everything", ""}, {"public", ":0"}, {"public", ":70000"}, {"public", "x"}, {"10.0.0.0/33", ""}} {
		if _, err := parseTpPolicy(bad[0], bad[1]); err == nil {
			t.Errorf("%q %q accepted", bad[0], bad[1])
		}
	}
}

func TestTpGrant(t *testing.T) {
	defer func(a, b, c int) { tpWinMax, tpBudget, tpMax = a, b, c }(tpWinMax, tpBudget, tpMax)
	tpWinMax, tpBudget, tpMax = 4<<20, 16<<20, 300
	var ss []*stream
	for i := 0; i < tpMax; i++ {
		w, ok := tpGrant()
		if !ok {
			t.Fatalf("stream %d refused", i)
		}
		if w < tpWinMin || w > uint64(tpWinMax) {
			t.Fatalf("stream %d window %d", i, w)
		}
		if i < 3 && w != 4<<20 {
			t.Errorf("stream %d window %d, want the full 4 MiB", i, w)
		}
		s := newStream(uint32(2*i + 1))
		s.tp = &tpState{grant: w}
		ss = append(ss, s)
	}
	tpMu.Lock()
	granted := tpGranted
	tpMu.Unlock()
	if granted > uint64(tpBudget)+uint64(tpMax)*tpWinMin {
		t.Errorf("windows %d MiB > budget + max x min", granted>>20)
	}
	t.Logf("%d streams: windows %d KiB together", tpMax, granted>>10)
	if _, ok := tpGrant(); ok {
		t.Error("stream above -tmax granted")
	}
	for _, s := range ss {
		tpRelease(s)
		tpRelease(s) // twice: once only
	}
	tpMu.Lock()
	n, g := tpN, tpGranted
	tpMu.Unlock()
	if n != 0 || g != 0 {
		t.Errorf("after release: %d streams, %d bytes", n, g)
	}
	if w, _ := tpGrant(); w != 4<<20 {
		t.Errorf("after release window %d", w)
	}
	tpMu.Lock()
	tpN, tpGranted = 0, 0
	tpMu.Unlock()
}

// frames of kind k this peer submitted (kept copies), keyed by stream id
func copiesOf(p *peer, k byte) (ids []uint32, data [][]byte) {
	p.reMu.Lock()
	defer p.reMu.Unlock()
	for _, c := range p.copies {
		if len(c) >= fHdr && c[0] == k {
			ids = append(ids, binary.BigEndian.Uint32(c[1:]))
			data = append(data, c[fHdr:])
		}
	}
	return
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timeout: %s", what)
}

func tpTestSetup(t *testing.T, allow string) *peer {
	reliable, serve = true, true
	pol, err := parseTpPolicy(allow, "")
	if err != nil {
		t.Fatal(err)
	}
	tpPol = pol
	tpLocalRefresh()
	t.Cleanup(func() {
		reliable, serve, tpPol = false, false, nil
		smu.Lock()
		for id, s := range streams {
			if s.tp != nil {
				delete(streams, id)
			}
		}
		smu.Unlock()
		tpMu.Lock()
		tpN, tpGranted = 0, 0
		tpMu.Unlock()
	})
	p := regPeer(t)
	p.links[0].deadAfterNs = int64(time.Hour) // nobody acks here: the link must not count as dead (sends would wait)
	return p
}

func rx(f []byte) []byte { // a received frame lives in a receive buffer (the writer returns it to the pool)
	pl := getRx(len(f))
	copy(pl, f)
	return pl
}

// A transparent OPEN to a destination the policy refuses: no stream, an RST back, no dial.
func TestTpAcceptRefused(t *testing.T) {
	p := tpTestSetup(t, "public")
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	defer ln.Close()
	dst := ln.Addr().(*net.TCPAddr).AddrPort()
	p.onFrame(rx(frame(fOpenT, 7, tpOpenData(1<<20, dst))[hdrLen:]))
	smu.Lock()
	s := streams[7^dirBit]
	smu.Unlock()
	if s != nil {
		t.Fatal("stream created for 127.0.0.1 under -tallow public")
	}
	waitFor(t, "RST", func() bool { ids, _ := copiesOf(p, fRst); return len(ids) == 1 && ids[0] == 7^dirBit })
	ln.(*net.TCPListener).SetDeadline(time.Now().Add(200 * time.Millisecond))
	if c, err := ln.Accept(); err == nil {
		c.Close()
		t.Fatal("the server dialed a refused destination")
	}
	if tpRefused.Load() == 0 {
		t.Error("refusal not counted")
	}
}

// Allowed: the server dials, answers with WIN 0, uses the box's window, carries data and the half close.
func TestTpAcceptDial(t *testing.T) {
	p := tpTestSetup(t, "127.0.0.1")
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	defer ln.Close()
	dst := ln.Addr().(*net.TCPAddr).AddrPort()
	got := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- nil
			return
		}
		b, _ := io.ReadAll(c) // until the box's FIN
		c.Write([]byte("pong"))
		c.Close()
		got <- b
	}()
	p.onFrame(rx(frame(fOpenT, 9, tpOpenData(256<<10, dst))[hdrLen:]))
	smu.Lock()
	s := streams[9^dirBit]
	smu.Unlock()
	if s == nil || s.tp == nil {
		t.Fatal("no transparent stream")
	}
	if s.win != 256<<10 || s.rwin != streamWin {
		t.Errorf("windows: send %d (box's 256 KiB), receive %d", s.win, s.rwin)
	}
	waitFor(t, "connected WIN", func() bool {
		ids, d := copiesOf(p, fWin)
		return len(ids) == 1 && ids[0] == 9^dirBit && binary.BigEndian.Uint64(d[0]) == 0
	})
	p.onFrame(rx(frame(fData, 9, []byte("ping"))[hdrLen:]))
	p.onFrame(rx(frame(fFin, 9, nil)[hdrLen:]))
	select {
	case b := <-got:
		if string(b) != "ping" {
			t.Fatalf("target got %q", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("target got nothing")
	}
	waitFor(t, "data + FIN back", func() bool {
		_, d := copiesOf(p, fData)
		ids, _ := copiesOf(p, fFin)
		return len(d) == 1 && string(d[0]) == "pong" && len(ids) == 1
	})
	waitFor(t, "stream gone", func() bool {
		smu.Lock()
		defer smu.Unlock()
		return streams[9^dirBit] == nil
	})
	tpMu.Lock()
	n := tpN
	tpMu.Unlock()
	if n != 0 {
		t.Errorf("%d transparent streams left", n)
	}
}

// A destination that refuses the connection: the server answers RST (the box resets its client).
func TestTpAcceptDialFails(t *testing.T) {
	p := tpTestSetup(t, "127.0.0.1")
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	dst := ln.Addr().(*net.TCPAddr).AddrPort()
	ln.Close() // nothing listens there now
	p.onFrame(rx(frame(fOpenT, 11, tpOpenData(1<<20, dst))[hdrLen:]))
	waitFor(t, "RST after the failed dial", func() bool { ids, _ := copiesOf(p, fRst); return len(ids) == 1 && ids[0] == 11^dirBit })
	if ids, _ := copiesOf(p, fWin); len(ids) != 0 {
		t.Error("WIN (connected) sent for a failed dial")
	}
}

// -tmax on the server: one more transparent open is refused.
func TestTpAcceptCap(t *testing.T) {
	defer func(m int) { tpMax = m }(tpMax)
	tpMax = 1
	p := tpTestSetup(t, "127.0.0.1")
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	defer ln.Close()
	dst := ln.Addr().(*net.TCPAddr).AddrPort()
	p.onFrame(rx(frame(fOpenT, 13, tpOpenData(1<<20, dst))[hdrLen:]))
	p.onFrame(rx(frame(fOpenT, 15, tpOpenData(1<<20, dst))[hdrLen:]))
	smu.Lock()
	a, b := streams[13^dirBit], streams[15^dirBit]
	smu.Unlock()
	if a == nil || b != nil {
		t.Fatalf("first %v second %v", a != nil, b != nil)
	}
	waitFor(t, "RST for the second", func() bool { ids, _ := copiesOf(p, fRst); return len(ids) == 1 && ids[0] == 15^dirBit })
	p.abort(a, false)
}

// An old-style OPEN still works and is not subject to -tallow (only -allow).
func TestTpOldOpenUnchanged(t *testing.T) {
	p := tpTestSetup(t, "public")
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	defer ln.Close()
	p.onFrame(rx(frame(fOpen, 17, []byte(ln.Addr().String()))[hdrLen:]))
	smu.Lock()
	s := streams[17^dirBit]
	smu.Unlock()
	if s == nil || s.tp != nil || s.win != streamWin {
		t.Fatal("static OPEN not handled as before")
	}
	ln.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	p.abort(s, false)
}
