// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

// v62: the per-packet send path and onFeedback without softfloat work that changes nothing (derive's integer copies,
// deriveKey, fmax / fmin in main.go). Benchmarks for a softfloat build like the box's: GOARCH=386 GO386=softfloat
// go test -c, run with -test.bench.

import (
	"encoding/binary"
	"math"
	"math/rand"
	"net"
	"sync"
	"testing"
)

// benchPeer: k links with addresses, measured as twoLinkPeer's (base 35 ms, srtt 60 ms).
func benchPeer(tb testing.TB, k int) *peer {
	p := &peer{kick: make(chan struct{}, 1), buf: map[uint32][]byte{}, copies: map[uint32][]byte{}, skippedSet: map[uint32]bool{}, copySent: map[uint32]int64{}}
	for i := 1; i <= k; i++ {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { c.Close() })
		l := &link{id: i, conn: c, win: winMax, lastSend: mono(), lmap: map[uint32]lmapEnt{}}
		l.txCond = sync.NewCond(&l.txMu)
		l.peerA.Store(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})
		l.minRtt, l.srtt, l.jit = 35000, 60000, 2000
		l.derive()
		p.links = append(p.links, l)
	}
	return p
}

// fbEntries: a k-link feedback whose entry j acknowledges linkSeq his[j] (0 = nothing), held 1 ms, with one-way
// delay owds[j] µs, sent over the far end's link lid.
func fbEntries(his []uint32, owds []int64, lid int) []byte {
	b := []byte{magic, tFeedback, byte(len(his)), 0}
	for j := range his {
		e := make([]byte, 21)
		if his[j] != 0 {
			e[0] = 1
			binary.BigEndian.PutUint32(e[1:], his[j])
			binary.BigEndian.PutUint64(e[5:], 1000)
			binary.BigEndian.PutUint64(e[13:], uint64(owds[j]))
		}
		b = append(b, e...)
	}
	return append(b, fbLidMark, byte(lid))
}

// derived: everything derive computes, to compare a link's cached copies with a fresh derive.
type derived struct {
	winB                                     int
	relOwdUs, usPerB24, paceBps, deadAfterNs int64
	repairUs, redStallNs                     int64
}

func derivedOf(l *link) derived {
	return derived{l.winB, l.relOwdUs, l.usPerB24, l.paceBps, l.deadAfterNs, l.repairUs, l.redStallNs}
}

// onFeedback skips derive when none of its inputs changed (a duplicate feedback copy) and the send path uses the
// cached integer repair time: after every feedback of a random run (data sent and acknowledged on two links with
// varying delay, duplicate copies, losses, drains, -pacepick on and off) the cached values equal a fresh derive and
// the float functions they replace.
func TestDerivedCopiesStayFresh(t *testing.T) {
	setReliable(t, true)
	oldPick, oldGain := pacePick, paceGain
	defer func() { pacePick, paceGain = oldPick, oldGain }()
	r := rand.New(rand.NewSource(62))
	for run := 0; run < 4; run++ {
		pacePick, paceGain = run%2 == 0, []float64{0, 1.25}[run/2]
		p := benchPeer(t, 2)
		owd := []int64{20000, 35000}
		for step := 0; step < 3000; step++ {
			l := p.links[r.Intn(2)]
			for n := r.Intn(4); n > 0; n-- {
				p.sendOn(l, uint32(step), make([]byte, 100+r.Intn(1300)), 0, mono(), nil)
			}
			l.txQ = l.txQ[:0]
			owd[r.Intn(2)] += int64(r.Intn(4001) - 2000)
			his := []uint32{0, 0}
			for j, ll := range p.links {
				if len(ll.hist) > 0 && r.Intn(5) > 0 { // sometimes nothing acknowledged on a link
					his[j] = ll.hist[r.Intn(len(ll.hist))].lseq
				}
			}
			if his[0] == 0 && his[1] == 0 {
				continue
			}
			fb := fbEntries(his, owd, 1)
			p.onFeedback(fb)
			if r.Intn(2) == 0 {
				p.onFeedback(fb) // the copy over the other link
			}
			for _, ll := range p.links {
				ll.mu.Lock()
				got := derivedOf(ll)
				ll.derive()
				want := derivedOf(ll)
				span, stall := int64(ll.repairSpanUs()), redStall(ll.srtt)
				ll.mu.Unlock()
				if got != want || got.repairUs != span || got.redStallNs != stall {
					t.Fatalf("run %d step %d L%d: cached %+v, fresh derive %+v, repairSpanUs %d, redStall %d",
						run, step, ll.id, got, want, span, stall)
				}
			}
		}
	}
}

// The repair time a packet gets at send time is the float repairSpanUs of its link at that moment.
func TestSendOnDueFromLink(t *testing.T) {
	setReliable(t, true)
	p := benchPeer(t, 1)
	l := p.links[0]
	for _, srtt := range []float64{0, 12345.6, 60000, 300000.4, 2e6} {
		l.mu.Lock()
		l.srtt, l.maxQ, l.jit = srtt, 40000.7, 3333.3
		l.derive()
		want := int64(l.repairSpanUs()) * 1000
		l.mu.Unlock()
		now := mono()
		p.reMu.Lock()
		p.copies[1], p.copySent[1] = []byte{1}, 0
		p.reMu.Unlock()
		p.sendOn(l, 1, []byte{1}, 0, now, nil)
		if d, ok := p.dueOf(1); !ok || d != now+want {
			t.Fatalf("srtt %v: due %d, want now + %d", srtt, d-now, want)
		}
	}
}

// One data packet through sendOn (reliable, -fixguard: the repair time is computed per packet).
func BenchmarkSendOn(b *testing.B) {
	old := reliable
	reliable = true
	defer func() { reliable = old }()
	p := benchPeer(b, 2)
	l := p.links[0]
	inner := make([]byte, 1300)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.sendOn(l, uint32(i), inner, 0, mono(), nil)
		if i&1023 == 1023 {
			l.hist, l.inflight, l.txQ = l.hist[:0], 0, l.txQ[:0]
			clear(l.lmap)
			l.lmapQ = l.lmapQ[:0]
		}
	}
}

// One feedback round of two links: the copy that acknowledges a new packet on each link, then its duplicate over
// the other link.
func BenchmarkOnFeedback(b *testing.B) {
	old := reliable
	reliable = true
	defer func() { reliable = old }()
	p := benchPeer(b, 2)
	owd := []int64{20000, 35000}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		his := []uint32{0, 0}
		for j, l := range p.links {
			l.mu.Lock()
			l.lseq++
			l.hist = append(l.hist, sentPkt{lseq: l.lseq, size: 1324, at: nowUs() - 60000})
			l.inflight += 1324
			his[j] = l.lseq
			l.mu.Unlock()
		}
		owd[i&1] += int64(i%7) - 3
		fb := fbEntries(his, owd, 1)
		p.onFeedback(fb)
		p.onFeedback(fb)
	}
}

// fmax / fmin equal math.Max / math.Min for every non-NaN pair (a zero's sign aside: -0 == +0 in every later
// comparison and conversion here).
func TestFmaxFmin(t *testing.T) {
	r := rand.New(rand.NewSource(62))
	vals := []float64{0, math.Copysign(0, -1), 1, -1, 1e-300, 6000, 4e6, 1.25e9, math.Inf(1), math.Inf(-1), math.MaxFloat64}
	for i := 0; i < 2000; i++ {
		vals = append(vals, (r.Float64()-0.3)*math.Pow(10, float64(r.Intn(12))))
	}
	for _, a := range vals {
		for _, b := range vals[:40] {
			if fmax(a, b) != math.Max(a, b) || fmin(a, b) != math.Min(a, b) ||
				fmax(b, a) != math.Max(b, a) || fmin(b, a) != math.Min(b, a) {
				t.Fatalf("a %v b %v: fmax %v / %v, fmin %v / %v", a, b, fmax(a, b), math.Max(a, b), fmin(a, b), math.Min(a, b))
			}
		}
	}
}
