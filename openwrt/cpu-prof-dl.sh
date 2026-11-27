#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# CPU profile of the RECEIVING board during a download, over the cable (no SIM data): 40 ms netem on the board,
# server = a lab server behind the computer on the cable (UDP 47001/47002 forwarded to it); bench source on its 127.0.0.1:5202.
SRV=${SRV:-192.168.1.10}
tc qdisc replace dev br-lan root netem delay 40ms limit 10000
/tmp/sheaf -reliable -key /etc/sheaf/sheaf.key -tun shf0 -cpuprofile /tmp/prof-dl.out \
  -links "0.0.0.0:7001>$SRV:47001,0.0.0.0:7002>$SRV:47002" \
  -proxy "127.0.0.1:5202>127.0.0.1:5202" > /tmp/sheaf-prof.log 2>&1 &
SH_PID=$!
sleep 3
top -b -d 1 -n 8 | grep -E '^CPU:|sheaf|tcpbench' | grep -v grep > /tmp/top-prof.log &
/tmp/tcpbench -connect 127.0.0.1:5202 -recv 2>&1 | tail -1
sleep 1; grep -A3 '^CPU' /tmp/top-prof.log | sed -n '5,20p'
kill $SH_PID; sleep 2
tc qdisc del dev br-lan root
ls -la /tmp/prof-dl.out
echo DONE
