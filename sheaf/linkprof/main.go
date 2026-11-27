// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

// linkprof: profile one path by ramping a paced UDP rate until it saturates (base RTT, knee, delay at the onset of
// loss, how fast the queue fills). Every data packet is acknowledged at once with its send timestamp echoed,
// so the sender sees the round-trip time of each packet and which ones never came back.
//
//	linkprof -listen :47002                                  server (answers both directions)
//	linkprof -connect host:47002 -dev wwan1 -dir up          ramp client -> server
//	linkprof -connect host:47002 -dev wwan1 -dir down        ramp server -> client (the server reports per step)
//
// Per step: offered Mbit/s, acknowledged Mbit/s, loss %, RTT p10/p50/p90/max in ms.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	tData  = 'D'
	tAck   = 'A'
	tStart = 'S' // client asks the server to ramp towards it: start, step, steps, ms per step, size
	tSum   = 'R' // server -> client: one step's summary line (text)
	tEnd   = 'E'
	hdr    = 1 + 4 + 8 // type, seq, send time µs
)

var t0 = time.Now()

func us() int64 { return time.Since(t0).Microseconds() }

type ramp struct {
	start, step         float64
	steps, stepMs, size int
}

// stepStats collects the acknowledgements of one step.
type stepStats struct {
	offered     float64
	sent, acked int
	forgiven    int // packets the pacer skipped after a stall (counted in sent, never sent)
	ackBytes    int
	rtts        []float64
}

func (s *stepStats) line(i, ms, size int) string {
	loss, real := 0.0, s.sent-s.forgiven
	if real > 0 {
		loss = 100 * float64(real-s.acked) / float64(real)
	}
	sort.Float64s(s.rtts)
	q := func(p float64) float64 {
		if len(s.rtts) == 0 {
			return 0
		}
		return s.rtts[int(p*float64(len(s.rtts)-1))]
	}
	bits := func(n int) float64 { return float64(n) * 8 / 1e3 / float64(ms) }
	return fmt.Sprintf("step %3d target %6.1f sent %6.1f acked %6.1f Mbit/s loss %5.1f %% rtt p10 %5.0f p50 %5.0f p90 %5.0f max %5.0f ms",
		i, s.offered, bits(real*size), bits(s.ackBytes), loss, q(0.1), q(0.5), q(0.9), q(1))
}

// send ramps the rate on c towards dst and reads the acks; report gets one line per step.
// Acks of a step that arrive after it ended still count for it (the step is found by the packet's seq).
// acks carries the ack packets: one socket has one reader, which hands them over.
func send(c *net.UDPConn, dst *net.UDPAddr, r ramp, acks <-chan []byte, report func(string)) {
	stats := make([]*stepStats, r.steps)
	stepOf := map[uint32]int{}
	sentAt := map[uint32]int64{}
	buf := make([]byte, r.size)
	buf[0] = tData
	var seq uint32
	drain := func() {
		for {
			select {
			case b := <-acks:
				s := binary.BigEndian.Uint32(b[1:])
				i, ok := stepOf[s]
				if !ok {
					continue
				}
				st := stats[i]
				st.acked++
				st.ackBytes += r.size
				st.rtts = append(st.rtts, float64(us()-sentAt[s])/1000)
				delete(stepOf, s)
				delete(sentAt, s)
			default:
				return
			}
		}
	}
	for i := 0; i < r.steps; i++ {
		rate := r.start + float64(i)*r.step // Mbit/s
		st := &stepStats{offered: rate}
		stats[i] = st
		perMs := rate * 1e6 / 8 / float64(r.size) / 1000 // packets per ms
		begin := time.Now()
		for time.Since(begin) < time.Duration(r.stepMs)*time.Millisecond {
			// pace by the clock, not by loop turns (a sleep may oversleep); a debt above 64 packets is forgiven,
			// so a stall never turns into a burst – the line shows the rate really sent
			due := int(perMs*float64(time.Since(begin).Microseconds())/1000) - st.sent
			if due > 64 {
				st.forgiven += due - 64
				st.sent += due - 64
				due = 64
			}
			for ; due > 0; due-- {
				seq++
				binary.BigEndian.PutUint32(buf[1:], seq)
				now := us()
				binary.BigEndian.PutUint64(buf[5:], uint64(now))
				if _, err := c.WriteToUDP(buf, dst); err == nil {
					st.sent++
					stepOf[seq], sentAt[seq] = i, now
				}
			}
			drain()
			time.Sleep(time.Millisecond)
		}
		if i > 0 {
			drain()
			report(stats[i-1].line(i-1, r.stepMs, r.size))
		}
	}
	deadline := time.Now().Add(2 * time.Second) // late acks of the last step
	for time.Now().Before(deadline) {
		drain()
		time.Sleep(10 * time.Millisecond)
	}
	report(stats[r.steps-1].line(r.steps-1, r.stepMs, r.size))
}

func ack(c *net.UDPConn, b []byte, to *net.UDPAddr) {
	a := make([]byte, hdr)
	copy(a, b[:hdr])
	a[0] = tAck
	c.WriteToUDP(a, to)
}

func server(addr string) {
	la, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		log.Fatal(err)
	}
	c, err := net.ListenUDP("udp4", la)
	if err != nil {
		log.Fatal(err)
	}
	c.SetReadBuffer(4 << 20)
	c.SetWriteBuffer(4 << 20)
	b := make([]byte, 2048)
	var active chan []byte // acks of the running down ramp
	done := make(chan bool, 1)
	for {
		select {
		case <-done:
			active = nil
		default:
		}
		c.SetReadDeadline(time.Now().Add(time.Second))
		n, from, err := c.ReadFromUDP(b)
		if err != nil || n < 1 {
			continue
		}
		switch b[0] {
		case tData:
			if n >= hdr {
				ack(c, b, from)
			}
		case tAck:
			if active != nil && n >= hdr {
				select {
				case active <- append([]byte(nil), b[:n]...):
				default:
				}
			}
		case tStart: // ramp towards the client; this loop keeps reading and hands the acks over
			if n < 1+8+8+4+4+4 || active != nil {
				continue
			}
			r := ramp{
				start:  float64(binary.BigEndian.Uint64(b[1:])) / 1000,
				step:   float64(binary.BigEndian.Uint64(b[9:])) / 1000,
				steps:  int(binary.BigEndian.Uint32(b[17:])),
				stepMs: int(binary.BigEndian.Uint32(b[21:])),
				size:   int(binary.BigEndian.Uint32(b[25:])),
			}
			if r.steps < 1 || r.steps > 400 || r.stepMs < 100 || r.size < 64 || r.size > 1472 || r.start+float64(r.steps)*r.step > 400 {
				continue
			}
			log.Printf("down ramp to %v: %+v", from, r)
			ch := make(chan []byte, 8192)
			active = ch
			go func(to *net.UDPAddr) {
				send(c, to, r, ch, func(s string) {
					log.Print(s)
					c.WriteToUDP(append([]byte{tSum}, s...), to)
				})
				c.WriteToUDP([]byte{tEnd}, to)
				done <- true
			}(from)
		}
	}
}

func dial(dev string) *net.UDPConn {
	lc := net.ListenConfig{Control: func(network, address string, rc syscall.RawConn) error {
		if dev == "" {
			return nil
		}
		var serr error
		rc.Control(func(fd uintptr) {
			serr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, dev)
		})
		return serr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:0")
	if err != nil {
		log.Fatal(err)
	}
	c := pc.(*net.UDPConn)
	c.SetReadBuffer(4 << 20)
	c.SetWriteBuffer(4 << 20)
	return c
}

func main() {
	listen := flag.String("listen", "", "server: listen on this UDP address")
	connect := flag.String("connect", "", "client: server host:port")
	dev := flag.String("dev", "", "client: bind to this network device (one modem)")
	dir := flag.String("dir", "up", "up = client sends, down = server sends")
	start := flag.Float64("start", 2, "first rate, Mbit/s")
	step := flag.Float64("step", 2, "rate increase per step, Mbit/s")
	steps := flag.Int("steps", 40, "number of steps")
	stepMs := flag.Int("ms", 1000, "step length, ms")
	size := flag.Int("size", 1200, "UDP payload bytes")
	flag.Parse()
	if *listen != "" {
		server(*listen)
		return
	}
	dst, err := net.ResolveUDPAddr("udp4", *connect)
	if err != nil {
		log.Fatal(err)
	}
	c := dial(*dev)
	r := ramp{*start, *step, *steps, *stepMs, *size}
	fmt.Printf("%s via %s, %s: %.0f..%.0f Mbit/s, %d ms steps, %d B\n", *connect, *dev, *dir, r.start, r.start+float64(r.steps-1)*r.step, r.stepMs, r.size)
	if *dir == "up" {
		acks := make(chan []byte, 8192)
		go func() {
			for {
				b := make([]byte, 64)
				n, _, err := c.ReadFromUDP(b)
				if err != nil {
					return
				}
				if n >= hdr && b[0] == tAck {
					acks <- b[:n]
				}
			}
		}()
		send(c, dst, r, acks, func(s string) { fmt.Println(s) })
		return
	}
	req := make([]byte, 1+8+8+4+4+4)
	req[0] = tStart
	binary.BigEndian.PutUint64(req[1:], uint64(r.start*1000))
	binary.BigEndian.PutUint64(req[9:], uint64(r.step*1000))
	binary.BigEndian.PutUint32(req[17:], uint32(r.steps))
	binary.BigEndian.PutUint32(req[21:], uint32(r.stepMs))
	binary.BigEndian.PutUint32(req[25:], uint32(r.size))
	c.WriteToUDP(req, dst)
	b := make([]byte, 2048)
	c.SetReadDeadline(time.Now().Add(time.Duration(r.steps*r.stepMs)*time.Millisecond + 15*time.Second))
	for {
		n, from, err := c.ReadFromUDP(b)
		if err != nil {
			fmt.Println("end:", err)
			return
		}
		switch b[0] {
		case tData:
			ack(c, b[:n], from)
		case tSum:
			fmt.Println(strings.TrimSpace(string(b[1:n])))
		case tEnd:
			return
		}
	}
}
