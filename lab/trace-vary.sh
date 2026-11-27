#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Run sheaf (binary $1, default /tmp/sheaf) with 100 ms traces through the 60 s vary.sh scenario.
# A = sender of the iperf data (link windows, tx), B = receiver (delivery, holes). Out: /var/log/sim/trace-{A,B}.csv, iperf.txt, vary.log
BIN=${1:-/tmp/sheaf}
L="10.1.0.1:7000>10.1.0.2:7000,10.2.0.1:7000>10.2.0.2:7000,10.3.0.1:7000>10.3.0.2:7000"
R="10.1.0.2:7000>10.1.0.1:7000,10.2.0.2:7000>10.2.0.1:7000,10.3.0.2:7000>10.3.0.1:7000"
pkill -x 'sheaf(-.*)?'; killall -q iperf3; sleep 1
ip netns exec A ip link del shf0 2>/dev/null; ip netns exec B ip link del shf0 2>/dev/null
ip netns exec A $BIN -tun shf0 -links "$L" -trace /var/log/sim/trace-A.csv > /var/log/sim/sheaf-A.log 2>&1 &
ip netns exec B $BIN -tun shf0 -links "$R" -trace /var/log/sim/trace-B.csv > /var/log/sim/sheaf-B.log 2>&1 &
sleep 1
ip netns exec A ip addr replace 10.50.0.1/30 dev shf0; ip netns exec A ip link set shf0 mtu 1400 up
ip netns exec B ip addr replace 10.50.0.2/30 dev shf0; ip netns exec B ip link set shf0 mtu 1400 up
sleep 2
T=10.50.0.2; [ "$PROXY" = 1 ] && T=127.0.0.1   # PROXY=1: binary is a proxy wrapper, iperf goes through the built-in proxy
ip netns exec B iperf3 -s -1 -D -B $T >/dev/null 2>&1; sleep 1
echo "start $(date +%s%3N)" > /var/log/sim/vary.log
if [ "$SCEN" = drive ]; then bash /tmp/drive.sh 60 ${SEED:-41} >> /var/log/sim/vary.log 2>&1 &
else bash /tmp/vary.sh >> /var/log/sim/vary.log 2>&1 & fi
ip netns exec A iperf3 -c $T -t 60 -i 0.5 -f m > /var/log/sim/iperf.txt 2>&1
while pgrep -f "bash /tmp/(vary|drive).sh" >/dev/null; do sleep 1; done
sleep 1; pkill -x 'sheaf(-.*)?'
grep -E 'receiver' /var/log/sim/iperf.txt
