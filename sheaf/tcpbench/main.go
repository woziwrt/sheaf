// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

// tcpbench: minimal TCP source/sink for measuring throughput on boxes without iperf.
//
//	tcpbench -listen :5202               sink: count and check what clients send (upload test)
//	tcpbench -listen :5202 -send 30      source: send 30 MB to every client, then close (download test)
//	tcpbench -connect host:port -mb 50   client sends 50 MB, prints Mbit/s when the far end has it all
//	tcpbench -connect host:port -recv    client receives until the far end closes, prints Mbit/s
//	tcpbench -bind 10.77.1.101 -connect ...   connect from this local address (one link of a multi-link box)
//	tcpbench -connect host:port -recv -for 20   receive for 20 s, then close (the source sends more than that)
//	tcpbench -connect host:port -for 20         send for 20 s instead of -mb (time-based: the same length on any link)
//	nc host port | tcpbench -stdin       check what arrives on stdin (when a host firewall stops a new binary)
//	tcpbench -gen 30 | nc host port      write 30 MB of the pattern to stdout (upload through nc)
//
// Receivers also print the steady rate: the bytes after the first 3 s over the time after them, without TCP's ramp-up.
//
// The stream content is checkable: every 8-byte word holds its own offset in the stream (big endian),
// so the receiver detects lost, duplicated or reordered bytes and prints "ok" or the first bad offset.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"
)

// fill writes the pattern for stream offset off (a multiple of 8) into b.
func fill(b []byte, off uint64) {
	for i := 0; i+8 <= len(b); i += 8 {
		binary.BigEndian.PutUint64(b[i:], off+uint64(i))
	}
}

// checker verifies the pattern (reads need not be aligned). Aligned words are compared as two 32-bit halves:
// a byte-by-byte check with 64-bit arithmetic costs a third of a 32-bit MIPS board at 60 Mbit/s.
type checker struct {
	off  uint64
	bad  int64     // first bad offset, -1 = none
	last time.Time // when the last bytes came (a sender behind nc may close only seconds after its last byte)
	t0   time.Time // start of the measurement
	t3   time.Time // first write at least steadyAfter after t0 (zero: not reached)
	n3   uint64    // bytes received before t3
}

const steadyAfter = 3 * time.Second

func newChecker(t0 time.Time) *checker { return &checker{bad: -1, t0: t0} }

func (c *checker) Write(b []byte) (int, error) {
	c.last = time.Now()
	if c.t3.IsZero() && c.last.Sub(c.t0) >= steadyAfter {
		c.t3, c.n3 = c.last, c.off
	}
	if c.bad >= 0 {
		c.off += uint64(len(b))
		return len(b), nil
	}
	i := 0
	for ; i < len(b) && (c.off+uint64(i))%8 != 0; i++ { // unaligned head
		c.byteAt(b[i], c.off+uint64(i))
	}
	for ; i+8 <= len(b); i += 8 {
		o := c.off + uint64(i)
		if binary.BigEndian.Uint32(b[i:]) != uint32(o>>32) || binary.BigEndian.Uint32(b[i+4:]) != uint32(o) {
			for j := 0; j < 8; j++ {
				c.byteAt(b[i+j], o+uint64(j))
			}
			break
		}
	}
	for ; i < len(b) && c.bad < 0; i++ { // tail (or the rest of a bad word)
		c.byteAt(b[i], c.off+uint64(i))
	}
	c.off += uint64(len(b))
	return len(b), nil
}

func (c *checker) byteAt(x byte, o uint64) {
	if c.bad < 0 && x != byte((o&^7)>>(8*(7-o%8))) {
		c.bad = int64(o)
	}
}

// steady: Mbit/s after the first steadyAfter, or "-" for a transfer that ended sooner
func (c *checker) steady() string {
	if c.t3.IsZero() || !c.last.After(c.t3) {
		return "steady -"
	}
	return fmt.Sprintf("steady %.1f Mbit/s", float64(c.off-c.n3)*8/1e6/c.last.Sub(c.t3).Seconds())
}

func (c *checker) verdict() string {
	if c.bad >= 0 {
		return fmt.Sprintf("DATA ERROR at byte %d", c.bad)
	}
	return "content ok"
}

// sendFor: the pattern until the deadline, returns the bytes written
func sendFor(c io.Writer, until time.Time) (uint64, error) {
	buf := make([]byte, 64<<10)
	var off uint64
	for time.Now().Before(until) {
		fill(buf, off)
		if _, err := c.Write(buf); err != nil {
			return off, err
		}
		off += uint64(len(buf))
	}
	return off, nil
}

func sendPattern(c io.Writer, mb int) error {
	buf := make([]byte, 64<<10)
	for i := 0; i < mb*16; i++ {
		fill(buf, uint64(i)*uint64(len(buf)))
		if _, err := c.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	listen := flag.String("listen", "", "listen address")
	send := flag.Int("send", 0, "listen mode: send this many MB to every client (download test)")
	connect := flag.String("connect", "", "connect here")
	mb := flag.Int("mb", 50, "connect mode: megabytes to send")
	recv := flag.Bool("recv", false, "connect mode: receive until EOF instead of sending")
	stdin := flag.Bool("stdin", false, "check the pattern arriving on stdin, print Mbit/s")
	gen := flag.Int("gen", 0, "write this many MB of the pattern to stdout")
	bind := flag.String("bind", "", "connect mode: local address to connect from")
	dur := flag.Float64("for", 0, "connect mode: seconds to receive (-recv) or to send (instead of -mb)")
	flag.Parse()
	if *gen > 0 {
		if err := sendPattern(os.Stdout, *gen); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *stdin {
		t := time.Now()
		ck := newChecker(t)
		n, err := io.Copy(ck, os.Stdin)
		d := time.Since(t).Seconds()
		fmt.Printf("download: %d B in %.2f s = %.1f Mbit/s, %s (err %v), %s\n", n, d, float64(n)*8/1e6/d, ck.verdict(), err, ck.steady())
		return
	}
	if *listen != "" {
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			log.Fatal(err)
		}
		for {
			c, err := ln.Accept()
			if err != nil {
				continue
			}
			go func(c net.Conn) {
				defer c.Close()
				t := time.Now()
				if *send > 0 {
					if err := sendPattern(c, *send); err != nil {
						fmt.Printf("send: %v\n", err)
						return
					}
					fmt.Printf("sent %d MB in %.2f s\n", *send, time.Since(t).Seconds())
					return
				}
				ck := newChecker(t)
				n, err := io.Copy(ck, c)
				d := time.Since(t).Seconds()
				if !ck.last.IsZero() {
					d = ck.last.Sub(t).Seconds() // up to the last byte, not to the close
				}
				fmt.Printf("sink: %d B in %.2f s = %.1f Mbit/s, %s (err %v), %s\n", n, d, float64(n)*8/1e6/d, ck.verdict(), err, ck.steady())
			}(c)
		}
	}
	var dl net.Dialer
	if *bind != "" {
		ip := net.ParseIP(*bind)
		if ip == nil {
			log.Fatalf("-bind %q: not an IP address", *bind)
		}
		dl.LocalAddr = &net.TCPAddr{IP: ip}
	}
	c, err := dl.Dial("tcp", *connect)
	if err != nil {
		log.Fatal(err)
	}
	t := time.Now()
	if *recv {
		ck := newChecker(t)
		if *dur > 0 {
			c.SetReadDeadline(t.Add(time.Duration(*dur * float64(time.Second))))
		}
		n, err := io.Copy(ck, c)
		d := time.Since(t).Seconds()
		if ne, ok := err.(net.Error); ok && ne.Timeout() && *dur > 0 {
			err, d = nil, ck.last.Sub(t).Seconds() // our own end of the measurement, not an error
		}
		fmt.Printf("download: %d B in %.2f s = %.1f Mbit/s, %s (err %v), %s\n", n, d, float64(n)*8/1e6/d, ck.verdict(), err, ck.steady())
		return
	}
	var sent uint64
	if *dur > 0 {
		if sent, err = sendFor(c, t.Add(time.Duration(*dur*float64(time.Second)))); err != nil {
			log.Fatal(err)
		}
	} else {
		if err := sendPattern(c, *mb); err != nil {
			log.Fatal(err)
		}
		sent = uint64(*mb) << 20
	}
	c.(*net.TCPConn).CloseWrite()
	io.Copy(io.Discard, c) // wait until the far end closes: everything was delivered (check the sink's line)
	d := time.Since(t).Seconds()
	fmt.Printf("upload: %.1f MB in %.2f s = %.1f Mbit/s\n", float64(sent)/(1<<20), d, float64(sent)*8/1e6/d)
}
