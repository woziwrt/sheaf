// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

// Transparent TCP proxy. On the box, the firewall redirects TCP connections from the LAN (to public addresses) to a
// local listener (-transparent addr:port); sheaf reads the original destination with SO_ORIGINAL_DST and carries
// the byte stream as a proxy stream (proxy.go) whose OPEN names that destination (frame kind 6, OPENT). The server
// (-serve with -tallow) checks the destination against its policy and dials it from its own address. Everything that
// is not redirected (UDP, ICMP, TCP to private addresses) keeps using the tunnel.
//
// Why: on the RBM33G one TUN write per packet (plus the kernel's forwarding and NAT of it) costs ~4x the CPU per
// byte of a proxy stream, which hands the kernel big chunks: one TCP flow got ~20 Mbit/s through the tunnel and
// 59-75 Mbit/s through the proxy over the same links (26 Nov).
//
// Memory on a 256 MB box: the box announces in OPENT how many bytes the server may have in flight for the stream
// (its receive window). The windows come from one budget (-tbudget): a new stream gets half of what is left, at
// most -twin and at least tpWinMin, so a few bulk streams get big windows and dozens of browser connections small
// ones. Connections are capped (-tmax); a stream without any byte for -tidle is closed; a stream the server never
// answered (an older server ignores kind 6) is reset after tpOpenWait.

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

var (
	tpListenAddr string          // -transparent: box listener the firewall redirects the LAN's TCP to
	tpWinMax     = 4 << 20       // -twin (KiB): largest receive window of one transparent stream on the box
	tpBudget     = 16 << 20      // -tbudget (KiB): receive windows of all transparent streams (soft, see tpGrant)
	tpMax        = 256           // -tmax: transparent streams at a time (box and server)
	tpIdle       = 2 * time.Hour // -tidle: close a transparent stream without any byte for this long
	tpAllowSpec  string          // -tallow: server policy, "public" and/or CIDRs (empty = transparent opens refused)
	tpDenySpec   string          // -tdeny: CIDRs and ":port" always refused
	tpPol        *tpPolicy
)

const (
	tpWinMin   = 128 << 10        // smallest window: ~7 Mbit/s per stream at a loaded 150 ms round trip
	tpOpenWait = 30 * time.Second // no frame of the server for a new stream by then: reset it (dial timeout is 10 s)
	tpSockBuf  = 128 << 10        // LAN socket buffers on the box (the kernel doubles them); the LAN is fast and near
)

func tproxyFlags() {
	flag.StringVar(&tpListenAddr, "transparent", "", "box: transparent TCP proxy listener (addr:port) the firewall redirects the LAN's TCP to; needs -reliable")
	flag.Func("twin", "box: largest receive window per transparent stream, KiB (default 4096)", func(v string) error {
		return kib(v, &tpWinMax)
	})
	flag.Func("tbudget", "box: receive windows of all transparent streams together, KiB (default 16384)", func(v string) error {
		return kib(v, &tpBudget)
	})
	flag.IntVar(&tpMax, "tmax", tpMax, "transparent streams at a time; more are reset (box) or refused (server)")
	flag.DurationVar(&tpIdle, "tidle", tpIdle, "close a transparent stream that moved no byte for this long")
	flag.StringVar(&tpAllowSpec, "tallow", "", "server: destinations of transparent opens: \"public\" (any public address except the server's own) and/or CIDRs, comma separated; empty = refuse them")
	flag.StringVar(&tpDenySpec, "tdeny", "", "server: CIDRs and \":port\" never dialed for a transparent open, comma separated")
}

func kib(v string, dst *int) error {
	n, err := strconv.Atoi(v)
	if err != nil || n < 16 || n > 1<<20 {
		return errors.New("KiB, 16..1048576")
	}
	*dst = n << 10
	return nil
}

// tpState: what a transparent stream has on top of a proxy stream.
type tpState struct {
	dst   netip.AddrPort
	born  int64        // mono() at the open
	last  atomic.Int64 // mono() of the last stream byte read or written
	heard atomic.Bool  // box: a frame of the server came for it (the WIN it sends after its dial, data, RST)
	grant uint64       // receive window taken from the budget (box), given back once
	gone  atomic.Bool
}

func (t *tpState) touch() { t.last.Store(mono()) }

var (
	tpMu      sync.Mutex
	tpN       int    // live transparent streams
	tpGranted uint64 // their receive windows (box)

	tpOpened, tpRefused, tpCapped, tpIdled, tpUnanswered, tpForeign atomic.Uint32
)

// tpGrant takes a slot and a receive window for a new box stream: half of the budget still free, within
// [tpWinMin, tpWinMax]. The sum of windows stays below tpBudget + tpMax*tpWinMin.
func tpGrant() (uint64, bool) {
	tpMu.Lock()
	defer tpMu.Unlock()
	if tpN >= tpMax {
		return 0, false
	}
	var w uint64 = tpWinMin
	if free := uint64(tpBudget); free > tpGranted {
		w = (free - tpGranted) / 2
	}
	w = min(max(w, tpWinMin), uint64(max(tpWinMax, tpWinMin)))
	w &^= 1<<10 - 1
	tpN++
	tpGranted += w
	return w, true
}

// tpRelease gives a transparent stream's slot and window back (once; forget calls it for every stream).
func tpRelease(s *stream) {
	if s.tp == nil || !s.tp.gone.CompareAndSwap(false, true) {
		return
	}
	tpMu.Lock()
	tpN--
	tpGranted -= s.tp.grant
	tpMu.Unlock()
}

// OPENT data: flags u8 (0), window u32 (bytes the acceptor may have in flight towards the opener), port u16,
// address (4 bytes IPv4 or 16 bytes IPv6).
func tpOpenData(win uint64, dst netip.AddrPort) []byte {
	a := dst.Addr().Unmap()
	b := make([]byte, 7, 7+16)
	binary.BigEndian.PutUint32(b[1:], uint32(min(win, 1<<31)))
	binary.BigEndian.PutUint16(b[5:], dst.Port())
	return append(b, a.AsSlice()...)
}

func tpParseOpen(d []byte) (win uint64, dst netip.AddrPort, err error) {
	if len(d) != 7+4 && len(d) != 7+16 {
		return 0, dst, fmt.Errorf("length %d", len(d))
	}
	win = uint64(binary.BigEndian.Uint32(d[1:]))
	a, _ := netip.AddrFromSlice(d[7:])
	dst = netip.AddrPortFrom(a.Unmap(), binary.BigEndian.Uint16(d[5:]))
	if win == 0 {
		return 0, dst, errors.New("window 0")
	}
	return win, dst, nil
}

// tpPolicy: which destinations the server dials for transparent opens.
type tpPolicy struct {
	public    bool           // any public address except the server's own
	allow     []netip.Prefix // explicitly allowed (also private ones: configured targets)
	deny      []netip.Prefix // never
	denyPorts map[uint16]bool
}

// tpSpecial: addresses that are never "public": private, shared (CGNAT), loopback, link-local, documentation,
// benchmarking, multicast, reserved; IPv6 ranges that embed IPv4 or reach it (NAT64, 6to4, Teredo).
var tpSpecial = func() (ps []netip.Prefix) {
	for _, s := range strings.Fields(`0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 172.16.0.0/12
		192.0.0.0/24 192.0.2.0/24 192.88.99.0/24 192.168.0.0/16 198.18.0.0/15 198.51.100.0/24 203.0.113.0/24 224.0.0.0/3
		::/128 ::1/128 ::ffff:0:0/96 64:ff9b::/96 64:ff9b:1::/48 100::/64 2001::/32 2001:db8::/32 2002::/16 fc00::/7
		fe80::/10 ff00::/8`) {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	return
}()

func inPrefixes(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func prefixOf(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()), nil
}

func parseTpPolicy(allow, deny string) (*tpPolicy, error) {
	pol := &tpPolicy{denyPorts: map[uint16]bool{}}
	for _, f := range strings.Split(allow, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		if f == "public" {
			pol.public = true
			continue
		}
		p, err := prefixOf(f)
		if err != nil {
			return nil, fmt.Errorf("-tallow %q: public or a CIDR / address", f)
		}
		pol.allow = append(pol.allow, p)
	}
	for _, f := range strings.Split(deny, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		if port, ok := strings.CutPrefix(f, ":"); ok {
			n, err := strconv.ParseUint(port, 10, 16)
			if err != nil || n == 0 {
				return nil, fmt.Errorf("-tdeny %q: bad port", f)
			}
			pol.denyPorts[uint16(n)] = true
			continue
		}
		p, err := prefixOf(f)
		if err != nil {
			return nil, fmt.Errorf("-tdeny %q: a CIDR, an address or :port", f)
		}
		pol.deny = append(pol.deny, p)
	}
	return pol, nil
}

func (pol *tpPolicy) active() bool { return pol != nil && (pol.public || len(pol.allow) > 0) }

// check: may the server dial dst? local = is it one of the server's own addresses.
func (pol *tpPolicy) check(dst netip.AddrPort, local func(netip.Addr) bool) (bool, string) {
	a := dst.Addr().Unmap()
	switch {
	case !pol.active():
		return false, "no -tallow"
	case !a.IsValid() || dst.Port() == 0:
		return false, "bad address"
	case pol.denyPorts[dst.Port()]:
		return false, "port in -tdeny"
	case inPrefixes(pol.deny, a):
		return false, "address in -tdeny"
	case inPrefixes(pol.allow, a):
		return true, ""
	case !pol.public:
		return false, "not in -tallow"
	case inPrefixes(tpSpecial, a):
		return false, "not a public address"
	case local != nil && local(a):
		return false, "an address of the server itself"
	}
	return true, ""
}

// The server's own addresses, refreshed in the background: the check runs under the receive lock (no syscalls).
var tpLocal atomic.Pointer[map[netip.Addr]bool]

func tpLocalRefresh() {
	m := map[netip.Addr]bool{}
	if as, err := net.InterfaceAddrs(); err == nil {
		for _, x := range as {
			if n, ok := x.(*net.IPNet); ok {
				if a, ok := netip.AddrFromSlice(n.IP); ok {
					m[a.Unmap()] = true
				}
			}
		}
	}
	tpLocal.Store(&m)
}

func tpIsLocal(a netip.Addr) bool {
	m := tpLocal.Load()
	return m == nil || (*m)[a] // not known yet: refuse
}

// tpLog: refusals and resets are logged, at most 20 lines per 10 s (a client scanning must not flood the log or
// block the receive path on a full log pipe).
var (
	tpLogMu   sync.Mutex
	tpLogT    time.Time
	tpLogN    int
	tpLogLost int
)

func tpLogf(format string, a ...any) {
	tpLogMu.Lock()
	if time.Since(tpLogT) > 10*time.Second {
		if tpLogLost > 0 {
			log.Printf("tproxy: %d more messages not logged", tpLogLost)
		}
		tpLogT, tpLogN, tpLogLost = time.Now(), 0, 0
	}
	tpLogN++
	ok := tpLogN <= 20
	if !ok {
		tpLogLost++
	}
	tpLogMu.Unlock()
	if ok {
		log.Printf("tproxy: "+format, a...)
	}
}

// tpAccept (server, under smu, from onFrame under the receive lock): a transparent OPEN. Returns the new stream,
// nil when refused (an RST is then on its way).
func (p *peer) tpAccept(id uint32, data []byte) *stream {
	win, dst, err := tpParseOpen(data)
	why := ""
	switch {
	case err != nil:
		why = "malformed: " + err.Error()
	case !serve:
		why = "no -serve"
	default:
		if ok, w := tpPol.check(dst, tpIsLocal); !ok {
			why = w
		}
	}
	if why == "" {
		tpMu.Lock()
		if tpN >= tpMax {
			why = "too many streams (-tmax)"
		} else {
			tpN++
		}
		tpMu.Unlock()
	}
	if why != "" {
		tpRefused.Add(1)
		tpLogf("OPEN %d to %v refused: %s", id, dst, why)
		go p.submitEp(frame(fRst, id, nil), p.epoch.Load()) // not under the receive lock; epoch taken now
		return nil
	}
	tpOpened.Add(1)
	s := newStream(id)
	s.ep = p.epoch.Load()
	s.win = win // the box's receive window for this stream
	s.tp = &tpState{dst: dst, born: mono()}
	s.tp.touch()
	streams[id] = s
	go func() {
		d := net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
		c, err := d.Dial("tcp", dst.String())
		if err != nil {
			tpLogf("dial %v: %v", dst, err)
			close(s.ready)
			p.abort(s, true)
			return
		}
		tune(c)
		s.mu.Lock()
		s.conn = c
		aborted := s.rst
		s.mu.Unlock()
		close(s.ready)
		if aborted {
			c.Close()
			return
		}
		p.submitEp(frame(fWin, s.id, make([]byte, 8)), s.ep) // "connected": the box stops waiting for an answer
		go p.reader(s)
	}()
	go p.writer(s)
	return s
}

// origDst: the destination a redirected connection had before the firewall's redirect (IPv4).
func origDst(c *net.TCPConn) (netip.AddrPort, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var ap netip.AddrPort
	var serr error
	err = rc.Control(func(fd uintptr) {
		m, e := unix.GetsockoptIPv6Mreq(int(fd), unix.IPPROTO_IP, unix.SO_ORIGINAL_DST) // a sockaddr_in in 16 bytes
		if e != nil {
			serr = e
			return
		}
		ap = netip.AddrPortFrom(netip.AddrFrom4([4]byte(m.Multiaddr[4:8])), binary.BigEndian.Uint16(m.Multiaddr[2:4]))
	})
	if err == nil {
		err = serr
	}
	return ap, err
}

// tpServe (box): accept redirected LAN connections and open them on the server.
func (p *peer) tpServe(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			time.Sleep(100 * time.Millisecond) // EMFILE and friends: do not spin
			continue
		}
		tc := c.(*net.TCPConn)
		dst, err := origDst(tc)
		// not redirected: someone connected to the listener itself (from the LAN, or from a modem's side when it
		// listens on all addresses) - its original destination is the socket's own address
		if self := tc.LocalAddr().(*net.TCPAddr).AddrPort(); err != nil || dst.Addr().Unmap() == self.Addr().Unmap() &&
			dst.Port() == self.Port() || dst.Addr().IsLoopback() || dst.Addr().IsUnspecified() {
			tpForeign.Add(1)
			c.Close()
			continue
		}
		w, ok := tpGrant()
		if !ok {
			tpCapped.Add(1)
			tpLogf("%v -> %v reset: %d streams (-tmax)", c.RemoteAddr(), dst, tpMax)
			tc.SetLinger(0)
			c.Close()
			continue
		}
		tc.SetNoDelay(true)
		tc.SetReadBuffer(tpSockBuf)
		tc.SetWriteBuffer(tpSockBuf)
		tpOpened.Add(1)
		smu.Lock()
		id := nextID
		nextID += 2
		s := newStream(id)
		s.ep = p.epoch.Load()
		s.conn = c
		s.rwin = w
		s.tp = &tpState{dst: dst, born: mono(), grant: w}
		s.tp.touch()
		close(s.ready)
		streams[id] = s
		smu.Unlock()
		p.submitEp(frame(fOpenT, id, tpOpenData(w, dst)), s.ep) // before any data of this stream
		go p.writer(s)
		go p.reader(s)
	}
}

// tpSweep: reset transparent streams the server never answered and those idle for -tidle.
func (p *peer) tpSweep() {
	for range time.Tick(5 * time.Second) {
		now := mono()
		var old, idle []*stream
		smu.Lock()
		for _, s := range streams {
			if s.tp == nil {
				continue
			}
			if tpListenAddr != "" && !s.tp.heard.Load() && time.Duration(now-s.tp.born) > tpOpenWait {
				old = append(old, s)
			} else if time.Duration(now-s.tp.last.Load()) > tpIdle {
				idle = append(idle, s)
			}
		}
		smu.Unlock()
		for _, s := range old {
			tpUnanswered.Add(1)
			tpLogf("stream %d to %v: no answer from the server in %v (older server without transparent opens?)", s.id, s.tp.dst, tpOpenWait)
			p.abort(s, true)
		}
		for _, s := range idle {
			tpIdled.Add(1)
			p.abort(s, true)
		}
		if len(idle) > 0 {
			tpLogf("%d streams idle for %v closed", len(idle), tpIdle)
		}
	}
}

// tproxyStart (after flag.Parse): the policy, the box's listener, the sweeper.
func (p *peer) tproxyStart() {
	pol, err := parseTpPolicy(tpAllowSpec, tpDenySpec)
	if err != nil {
		log.Fatal(err)
	}
	tpPol = pol
	if tpMax < 1 || tpIdle < 10*time.Second {
		log.Fatal("-tmax must be at least 1, -tidle at least 10s")
	}
	if tpListenAddr != "" {
		if !reliable {
			log.Fatal("-transparent needs -reliable")
		}
		lc := net.ListenConfig{KeepAlive: 30 * time.Second}
		ln, err := lc.Listen(context.Background(), "tcp4", tpListenAddr)
		if err != nil {
			log.Fatalf("transparent listen: %v", err)
		}
		go p.tpServe(ln)
	}
	if serve && pol.active() {
		if !reliable {
			log.Fatal("-tallow needs -reliable")
		}
		tpLocalRefresh()
		go func() {
			for range time.Tick(30 * time.Second) {
				tpLocalRefresh()
			}
		}()
	}
	if tpListenAddr != "" || serve && pol.active() {
		go p.tpSweep()
	}
}

func tproxySummary() string {
	switch {
	case tpListenAddr != "":
		return fmt.Sprintf("transparent %s (window %d-%d KiB, budget %d KiB, max %d streams, idle %v)",
			tpListenAddr, tpWinMin>>10, tpWinMax>>10, tpBudget>>10, tpMax, tpIdle)
	case serve && tpPol.active():
		return fmt.Sprintf("transparent opens: allow %q deny %q (max %d streams, idle %v)", tpAllowSpec, tpDenySpec, tpMax, tpIdle)
	}
	return "transparent off"
}

// tpStatus for the status file.
type tpStatus struct {
	Streams    int    `json:"streams"`
	WindowKB   uint64 `json:"window_kb"`
	Opened     uint32 `json:"opened"`
	Refused    uint32 `json:"refused"`
	Capped     uint32 `json:"capped"`
	Idle       uint32 `json:"idle_closed"`
	Unanswered uint32 `json:"unanswered"`
	Foreign    uint32 `json:"not_redirected"`
}

func tproxyStatus() *tpStatus {
	if tpListenAddr == "" && !(serve && tpPol.active()) {
		return nil
	}
	tpMu.Lock()
	st := &tpStatus{Streams: tpN, WindowKB: tpGranted >> 10}
	tpMu.Unlock()
	st.Opened, st.Refused, st.Capped = tpOpened.Load(), tpRefused.Load(), tpCapped.Load()
	st.Idle, st.Unanswered, st.Foreign = tpIdled.Load(), tpUnanswered.Load(), tpForeign.Load()
	return st
}
