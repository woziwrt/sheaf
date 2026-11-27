// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

// udpstream: a constant-rate UDP stream like a camera or a call, and a receiver that says what a live player would
// see: loss, reordering, and how late packets come (delay relative to the stream's fastest packet, so the two
// clocks need not agree).
//
//	udpstream -recv :47999 -dur 70                       receiver, prints the result when the stream ends
//	udpstream -send 10.66.0.2:47999 -rate 8 -dur 60      sender: 8 Mbit/s of 1200 B packets for 60 s
//
// A receiver behind a firewall that only lets replies in: -hello host:port makes the receiver send a packet there
// every second (the sender binds that port with -lport), so the stream is the reply to a flow it opened.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"time"
)

const magic = 0x75647374 // "udst"

func main() {
	recv := flag.String("recv", "", "listen address (receiver)")
	send := flag.String("send", "", "destination (sender)")
	hello := flag.String("hello", "", "receiver: send a packet here every second (opens the firewall for the stream)")
	lport := flag.Int("lport", 0, "sender: local port")
	rate := flag.Float64("rate", 8, "Mbit/s")
	size := flag.Int("size", 1200, "UDP payload bytes")
	dur := flag.Duration("dur", 60*time.Second, "how long to send / to wait at most")
	label := flag.String("label", "", "receiver: a name for the result line")
	flag.Parse()
	switch {
	case *send != "":
		sender(*send, *lport, *rate, *size, *dur)
	case *recv != "":
		receiver(*recv, *hello, *dur, *label)
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func sender(dst string, lport int, rate float64, size int, dur time.Duration) {
	ra, err := net.ResolveUDPAddr("udp", dst)
	if err != nil {
		log.Fatal(err)
	}
	c, err := net.DialUDP("udp", &net.UDPAddr{Port: lport}, ra)
	if err != nil {
		log.Fatal(err)
	}
	go func() { // swallow hellos
		b := make([]byte, 2048)
		for {
			if _, err := c.Read(b); err != nil {
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	if size < 16 {
		size = 16
	}
	gap := time.Duration(float64(size*8) / (rate * 1e6) * 1e9)
	b := make([]byte, size)
	start := time.Now()
	var seq uint32
	for {
		now := time.Now()
		if now.Sub(start) >= dur {
			break
		}
		due := uint32(now.Sub(start) / gap)
		for ; seq <= due; seq++ {
			binary.BigEndian.PutUint32(b[0:], magic)
			binary.BigEndian.PutUint32(b[4:], seq)
			binary.BigEndian.PutUint64(b[8:], uint64(time.Now().UnixNano()))
			if _, err := c.Write(b); err != nil {
				log.Printf("write: %v", err)
				time.Sleep(10 * time.Millisecond)
			}
		}
		time.Sleep(gap)
	}
	log.Printf("sent %d packets of %d B in %v", seq, size, dur)
}

type arrival struct {
	seq uint32
	d   int64 // receive - send, ns (clock offset included)
	at  int64 // receive, ns
}

func receiver(addr, hello string, dur time.Duration, label string) {
	la, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		log.Fatal(err)
	}
	c, err := net.ListenUDP("udp", la)
	if err != nil {
		log.Fatal(err)
	}
	if hello != "" {
		ha, err := net.ResolveUDPAddr("udp", hello)
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			for {
				c.WriteToUDP([]byte("hello"), ha)
				time.Sleep(time.Second)
			}
		}()
	}
	var arr []arrival
	b := make([]byte, 65536)
	deadline := time.Now().Add(dur)
	for {
		wait := 3 * time.Second // the stream ended
		if len(arr) == 0 {
			wait = time.Until(deadline)
		}
		c.SetReadDeadline(time.Now().Add(wait))
		n, _, err := c.ReadFromUDP(b)
		if err != nil || time.Now().After(deadline) {
			break
		}
		if n < 16 || binary.BigEndian.Uint32(b) != magic {
			continue
		}
		now := time.Now().UnixNano()
		arr = append(arr, arrival{binary.BigEndian.Uint32(b[4:]), now - int64(binary.BigEndian.Uint64(b[8:])), now})
	}
	fmt.Println(report(label, arr))
}

func report(label string, arr []arrival) string {
	if len(arr) == 0 {
		return label + ": nothing received"
	}
	seen := map[uint32]bool{}
	var maxSeq uint32
	dup, ooo := 0, 0
	minD := arr[0].d
	var maxGap int64
	for i, a := range arr {
		if seen[a.seq] {
			dup++
			continue
		}
		seen[a.seq] = true
		if a.seq < maxSeq {
			ooo++
		}
		if a.seq > maxSeq {
			maxSeq = a.seq
		}
		if a.d < minD {
			minD = a.d
		}
		if i > 0 && a.at-arr[i-1].at > maxGap {
			maxGap = a.at - arr[i-1].at
		}
	}
	var ds []float64
	late100, late200 := 0, 0
	for _, a := range arr {
		ms := float64(a.d-minD) / 1e6
		ds = append(ds, ms)
		if ms > 100 {
			late100++
		}
		if ms > 200 {
			late200++
		}
	}
	sort.Float64s(ds)
	q := func(p float64) float64 { return ds[int(p*float64(len(ds)-1))] }
	total := int(maxSeq) + 1
	uniq := len(seen)
	pct := func(n int) float64 { return 100 * float64(n) / float64(total) }
	return fmt.Sprintf("%s: %d/%d packets, lost %.2f %%, out of order %d, dup %d | late ms p50 %.0f p99 %.0f p99.9 %.0f max %.0f | over 100 ms %.2f %%, over 200 ms %.2f %% | longest silence %.0f ms",
		label, uniq, total, pct(total-uniq), ooo, dup, q(0.5), q(0.99), q(0.999), ds[len(ds)-1],
		pct(late100), pct(late200), float64(maxGap)/1e6)
}
