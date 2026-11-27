#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Ideal ceiling for the driving scenario: same drives (seeds 41..45) as bench.sh, one TCP over link 1 carrying the live sum.
for i in $(seq 1 ${1:-5}); do
  killall -q iperf3; sleep 1
  ip netns exec B iperf3 -s -1 -D -B 10.1.0.2 >/dev/null 2>&1; sleep 1
  ORACLE=1 bash /tmp/drive.sh 60 $((40 + i)) >/dev/null 2>&1 &
  r=$(ip netns exec A iperf3 -c 10.1.0.2 -t 60 -f m 2>/dev/null | awk '/receiver/{print $7}')
  wait
  echo "oracle drive $i: $r"
done
