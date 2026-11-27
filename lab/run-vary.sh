#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# $1 = udp (sheaf v7 through TUN) | proxy (TCP via MPTCP proxy). iperf3 60 s, 5 s intervals, while vary.sh changes the links.
killall -q iperf3; sleep 1
if [ "$1" = udp ]; then T=10.50.0.2; ip netns exec B iperf3 -s -1 -D -B $T >/dev/null 2>&1
else T=127.0.0.1; ip netns exec B iperf3 -s -1 -D -B 127.0.0.1 >/dev/null 2>&1; fi
sleep 1
bash /tmp/vary.sh > /var/log/sim/vary.log 2>&1 &
ip netns exec A iperf3 -c $T -t 60 -i 5 -f m 2>&1 | grep -E '^\[ *[0-9]+\] +[0-9]' | awk '{print $3, $7, $8}'
wait; cat /var/log/sim/vary.log
