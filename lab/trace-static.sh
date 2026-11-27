#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# One static 30 s run per version with a 100 ms trace on the sending side (A): why is v27+ slower on x86?
L="10.1.0.1:7000>10.1.0.2:7000,10.2.0.1:7000>10.2.0.2:7000,10.3.0.1:7000>10.3.0.2:7000"
R="10.1.0.2:7000>10.1.0.1:7000,10.2.0.2:7000>10.2.0.1:7000,10.3.0.2:7000>10.3.0.1:7000"
K=/tmp/sheaf-lab.key
for v in ${VERS:-26 31}; do
  pkill -x 'sheaf(-.*)?'; killall -q iperf3; sleep 1
  for ns in A B; do ip netns exec $ns ip link del shf0 2>/dev/null; done
  ip netns exec B /tmp/sheaf-v$v -reliable -serve -key $K -tun shf0 -links "$R" > /var/log/sim/ts-B-$v.log 2>&1 &
  ip netns exec A /tmp/sheaf-v$v -reliable -key $K -tun shf0 -trace /var/log/sim/ts-$v.csv -proxy "127.0.0.1:5201>127.0.0.1:5201" -links "$L" > /var/log/sim/ts-A-$v.log 2>&1 &
  sleep 2; ip netns exec B iperf3 -s -D -B 127.0.0.1 >/dev/null 2>&1; sleep 1
  q() { for i in 1 2 3; do ip netns exec A tc -s qdisc show dev a$i | awk '/dropped/{gsub(",","",$7); printf "%s ", $7}'; done; ip netns exec A awk '/^Udp:/ && $2 ~ /^[0-9]/ {printf "sndbuferr=%s ", $7}' /proc/net/snmp; }
  before=$(q)
  echo "v$v: $(ip netns exec A iperf3 -c 127.0.0.1 -t 30 -f m 2>/dev/null | awk '/receiver/{print $7}') Mbit/s"
  echo "   netem drops L1 L2 L3 + UDP: before [$before] after [$(q)]; dead A=$(grep -c dead /var/log/sim/ts-A-$v.log) B=$(grep -c dead /var/log/sim/ts-B-$v.log)"
done
pkill -x 'sheaf(-.*)?'; killall -q iperf3
