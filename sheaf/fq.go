// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"log"
	"sync"
	"sync/atomic"
)

// Flow queueing between the TUN device and the sender.
//
// The sender blocks whenever every link's window is full. Before this queue the TUN device's own ring (txqueuelen,
// 500 packets) held the backlog: one FIFO for all flows, tail-dropped by the kernel when full (tun drops instead of
// stopping the qdisc, so the device's fq_codel never sees a queue). A camera stream behind a bulk upload waited
// ~240 ms (p99) and lost packets with it.
//
// Now a reader takes packets off the TUN device as they come and files them per flow (hash of addresses, protocol and
// ports). The sender takes them deficit-round-robin, a flow that was empty first (like fq_codel's new flows): a
// stream below its fair share - a call, a camera, DNS, a TCP handshake - never waits behind a bulk transfer. When the
// queue holds more than fqLimit bytes, the fattest flow loses its oldest packet: only the bulk transfer slows down.
const (
	fqBuckets = 1024
	fqQuantum = 1514
)

var (
	// bytes queued at most (-fqlimit; 0 = no flow queue, read TUN in the sender as before). 1 MB since v59: with the
	// per-link transmit queues capped (P1), this queue - in front of the link decision, so nothing in it waits on a
	// slow link - is what absorbs the bursts of a single TCP flow. At 256 kB it overflowed on every ACK burst and held
	// the flow's cwnd at ~300 packets: LTE A/B 26 Nov, 4 links, one flow down 158-188 Mbit/s with 1-2 MB vs 59-68
	// with 256 kB (v57: 70-96, and 1 MB there stalled up to 9 s - its queues sit behind the link decision). Other
	// flows do not wait behind it: they are served first (new flows, deficit round robin).
	fqLimit = 1 << 20
	fqDrops atomic.Int64
)

type fqFlow struct {
	q       [][]byte // newPkt buffers
	head    int
	bytes   int
	deficit int
	active  bool // in one of the lists
}

type flowQueue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	flows    [fqBuckets]fqFlow
	newFlows []int
	oldFlows []int
	bytes    int
	pkts     int
}

// compact: a flow that never empties used to keep a slot for every packet that ever passed (pop only moved head):
// ~180 MB/h on a saturated uplink (review finding, TestRegFqNoGrowth). Move the backlog down once half is consumed.
func (fl *fqFlow) compact() {
	if fl.head >= 64 && fl.head*2 >= len(fl.q) {
		n := copy(fl.q, fl.q[fl.head:])
		clear(fl.q[n:])
		fl.q, fl.head = fl.q[:n], 0
	}
}

func newFlowQueue() *flowQueue {
	f := &flowQueue{}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// flowHash: bucket of an IP packet (inner = the packet at full[hdrLen:]).
func flowHash(pl []byte) int {
	var h uint32 = 2166136261
	mix := func(b []byte) {
		for _, c := range b {
			h = (h ^ uint32(c)) * 16777619
		}
	}
	if len(pl) < 20 {
		return 0
	}
	switch pl[0] >> 4 {
	case 4:
		ihl := int(pl[0]&15) * 4
		proto := pl[9]
		mix(pl[9:10])
		mix(pl[12:20])
		if (proto == 6 || proto == 17) && len(pl) >= ihl+4 && pl[6]&0x1f == 0 && pl[7] == 0 { // first fragment only has ports
			mix(pl[ihl : ihl+4])
		}
	case 6:
		if len(pl) < 40 {
			return 0
		}
		mix(pl[6:7])
		mix(pl[8:40])
		if (pl[6] == 6 || pl[6] == 17) && len(pl) >= 44 {
			mix(pl[40:44])
		}
	}
	return int(h % fqBuckets)
}

func (f *flowQueue) push(full []byte) {
	b := flowHash(full[hdrLen:])
	f.mu.Lock()
	fl := &f.flows[b]
	fl.q = append(fl.q, full)
	fl.bytes += len(full)
	f.bytes += len(full)
	f.pkts++
	if !fl.active {
		fl.active = true
		fl.deficit = fqQuantum
		f.newFlows = append(f.newFlows, b)
	}
	for f.bytes > fqLimit {
		f.dropFattest()
	}
	f.cond.Signal()
	f.mu.Unlock()
}

// dropFattest: the oldest packet of the flow with the most bytes queued (called with mu held).
func (f *flowQueue) dropFattest() {
	fat, most := -1, 0
	for _, list := range [2][]int{f.newFlows, f.oldFlows} {
		for _, b := range list {
			if f.flows[b].bytes > most {
				fat, most = b, f.flows[b].bytes
			}
		}
	}
	if fat < 0 {
		f.bytes = 0
		return
	}
	fl := &f.flows[fat]
	if fl.head >= len(fl.q) {
		return
	}
	d := fl.q[fl.head]
	fl.q[fl.head] = nil
	fl.head++
	fl.compact()
	fl.bytes -= len(d)
	f.bytes -= len(d)
	f.pkts--
	fqDrops.Add(1)
	if fl.head == len(fl.q) {
		fl.q, fl.head = fl.q[:0], 0
	}
}

// pop: the next packet, deficit round robin with new (sparse) flows first; blocks while the queue is empty.
func (f *flowQueue) pop() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		for f.pkts == 0 {
			f.cond.Wait()
		}
		var list *[]int
		if len(f.newFlows) > 0 {
			list = &f.newFlows
		} else {
			list = &f.oldFlows
		}
		b := (*list)[0]
		fl := &f.flows[b]
		if fl.head >= len(fl.q) { // emptied (by drops): leave the lists
			*list = (*list)[1:]
			fl.active = false
			fl.q, fl.head = fl.q[:0], 0
			continue
		}
		if fl.deficit <= 0 { // used its turn: to the end of the old flows
			fl.deficit += fqQuantum
			*list = (*list)[1:]
			f.oldFlows = append(f.oldFlows, b)
			continue
		}
		d := fl.q[fl.head]
		fl.q[fl.head] = nil
		fl.head++
		fl.compact()
		fl.bytes -= len(d)
		f.bytes -= len(d)
		f.pkts--
		fl.deficit -= len(d) - hdrLen
		if fl.head == len(fl.q) {
			fl.q, fl.head = fl.q[:0], 0
			if list == &f.newFlows { // a sparse flow emptied: it stays sparse (out of the lists until it has data again)
				*list = (*list)[1:]
				fl.active = false
			}
			// an old flow that emptied stays in the list one more round (as fq_codel), so it cannot hop to the
			// new list on every packet and starve the others; the check at the top removes it next time
		}
		return d
	}
}

// tunReader: TUN -> flow queue, as fast as packets come.
func (p *peer) tunReader(f *flowQueue) {
	buf := make([]byte, 2000)
	for {
		pk, err := p.tunRead(buf)
		if err != nil {
			log.Fatalf("tun read: %v", err)
		}
		c := newPkt(len(pk))
		copy(c[hdrLen:], pk)
		f.push(c)
	}
}
