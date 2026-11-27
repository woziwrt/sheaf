// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

// sheaf-mp – the bonding tunnel over one MPTCP connection: every packet
// from the TUN device goes, length-prefixed, into a single multipath TCP
// connection; the kernel spreads it over the links (one subflow per link,
// each with its own congestion control – the trick that makes the link
// speeds add up) and keeps it in order.
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"flag"
	"io"
	"log"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func openTun(name string) (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return nil, err
	}
	ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func pump(tun *os.File, c net.Conn) {
	go func() { // TUN -> connection
		w := bufio.NewWriterSize(c, 64<<10)
		b := make([]byte, 2+2000)
		for {
			n, err := tun.Read(b[2:])
			if err != nil {
				log.Fatalf("tun read: %v", err)
			}
			binary.BigEndian.PutUint16(b, uint16(n))
			if _, err := w.Write(b[:2+n]); err != nil {
				log.Fatalf("conn write: %v", err)
			}
			if w.Buffered() > 0 { // flush when the TUN has nothing more right now is not visible here: flush per packet keeps latency low
				w.Flush()
			}
		}
	}()
	r := bufio.NewReaderSize(c, 64<<10) // connection -> TUN
	h := make([]byte, 2)
	b := make([]byte, 2000)
	for {
		if _, err := io.ReadFull(r, h); err != nil {
			log.Fatalf("conn read: %v", err)
		}
		n := int(binary.BigEndian.Uint16(h))
		if _, err := io.ReadFull(r, b[:n]); err != nil {
			log.Fatalf("conn read: %v", err)
		}
		tun.Write(b[:n])
	}
}

func main() {
	tunName := flag.String("tun", "shf0", "TUN device")
	listen := flag.String("listen", "", "server: address to listen on")
	dial := flag.String("dial", "", "client: server address")
	flag.Parse()
	tun, err := openTun(*tunName)
	if err != nil {
		log.Fatalf("tun: %v", err)
	}
	var c net.Conn
	if *listen != "" {
		var lc net.ListenConfig
		lc.SetMultipathTCP(true)
		l, err := lc.Listen(context.Background(), "tcp", *listen)
		if err != nil {
			log.Fatal(err)
		}
		c, err = l.Accept()
		if err != nil {
			log.Fatal(err)
		}
	} else {
		var d net.Dialer
		d.SetMultipathTCP(true)
		c, err = d.Dial("tcp", *dial)
		if err != nil {
			log.Fatal(err)
		}
	}
	mp, _ := c.(*net.TCPConn).MultipathTCP()
	log.Printf("sheaf-mp: connected %v -> %v, multipath %v", c.LocalAddr(), c.RemoteAddr(), mp)
	pump(tun, c)
}
