// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

// sheaf-proxy – TCP over bonded links without TCP-in-TCP: the client
// terminates each TCP connection locally and carries only its byte stream
// in its own MPTCP connection to the server, which opens the real TCP
// connection to the target. MPTCP spreads
// the stream over the links, one subflow per link with its own congestion
// control, and keeps it in order; no inner TCP sees outer retransmissions.
//
//	server: sheaf-proxy -server :7200
//	client: sheaf-proxy -client 127.0.0.1:5201 -via SERVER:7200 -to TARGET:5201
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if c, ok := dst.(*net.TCPConn); ok {
			c.CloseWrite() // pass the half-close on (iperf3 and HTTP rely on it)
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

func server(addr string) {
	var lc net.ListenConfig
	lc.SetMultipathTCP(true)
	l, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("server on %s (multipath)", addr)
	for {
		c, err := l.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			h := make([]byte, 2)
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.ReadFull(c, h); err != nil {
				c.Close()
				return
			}
			t := make([]byte, binary.BigEndian.Uint16(h))
			if _, err := io.ReadFull(c, t); err != nil {
				c.Close()
				return
			}
			c.SetReadDeadline(time.Time{})
			dst, err := net.DialTimeout("tcp", string(t), 10*time.Second)
			if err != nil {
				log.Printf("dial %s: %v", t, err)
				c.Close()
				return
			}
			mp, _ := c.(*net.TCPConn).MultipathTCP()
			log.Printf("%v -> %s (multipath %v)", c.RemoteAddr(), t, mp)
			relay(c, dst)
		}(c)
	}
}

func client(listen, via, to string) {
	l, err := net.Listen("tcp", listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("client on %s -> %s via %s", listen, to, via)
	for {
		c, err := l.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			var d net.Dialer
			d.SetMultipathTCP(true)
			s, err := d.Dial("tcp", via)
			if err != nil {
				log.Printf("dial %s: %v", via, err)
				c.Close()
				return
			}
			h := make([]byte, 2, 2+len(to))
			binary.BigEndian.PutUint16(h, uint16(len(to)))
			if _, err := s.Write(append(h, to...)); err != nil {
				c.Close()
				s.Close()
				return
			}
			relay(c, s)
		}(c)
	}
}

func main() {
	srv := flag.String("server", "", "server: listen address (MPTCP)")
	cli := flag.String("client", "", "client: local listen address")
	via := flag.String("via", "", "client: server address")
	to := flag.String("to", "", "client: target address the server connects to")
	flag.Parse()
	if *srv != "" {
		server(*srv)
	}
	client(*cli, *via, *to)
}
