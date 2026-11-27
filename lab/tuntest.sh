#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Bulk TCP download THROUGH the tunnel (not the proxy): the client's upstream is only pure ACKs (> 65536).
BIN=$1; MB=${2:-300}; K=/tmp/sheaf-lab.key; LOG=/var/log/sim; mkdir -p $LOG
L="10.1.0.1:7000>10.1.0.2:7000,10.2.0.1:7000>10.2.0.2:7000,10.3.0.1:7000>10.3.0.2:7000"
R="10.1.0.2:7000>-,10.2.0.2:7000>-,10.3.0.2:7000>-"
ip netns del lt 2>/dev/null
pkill -x 'sheaf(-.*)?'; pkill -x tcpbench; sleep 1
for ns in A B; do ip netns exec $ns ip link del shf0 2>/dev/null; done
[ -f $K ] || head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n' > $K
ip netns exec B $BIN -reliable -serve -key $K -tun shf0 -links "$R" > $LOG/tun-B.log 2>&1 & B=$!
sleep 1
ip netns exec A $BIN -reliable -key $K -tun shf0 -links "$L" > $LOG/tun-A.log 2>&1 & A=$!
sleep 2
ip netns exec A sh -c 'ip addr add 10.66.0.2/30 dev shf0; ip link set shf0 up mtu 1320'
ip netns exec B sh -c 'ip addr add 10.66.0.1/30 dev shf0; ip link set shf0 up mtu 1320'
ip netns exec B /tmp/tcpbench -listen 10.66.0.1:5400 -send $MB > $LOG/tun-src.log 2>&1 & T=$!
sleep 1
echo "$(basename $BIN): $(ip netns exec A timeout 280 /tmp/tcpbench -connect 10.66.0.1:5400 -recv 2>&1 | tail -1)"
ip netns exec A sh -c "grep -o 'deliv [0-9]* skip [0-9]* late [0-9]* nack [0-9]*' $LOG/tun-A.log | tail -1" 2>/dev/null
grep -o 'deliv [0-9]*.*buf [0-9]*' $LOG/tun-B.log | tail -1 | grep -o 'deliv [0-9]*\|buf [0-9]*' | tr '\n' ' '; echo
kill $A $B $T 2>/dev/null; sleep 1
