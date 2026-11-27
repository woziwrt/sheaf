#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# "Ideal bonding" ceiling: link 1 alone carries, every 10 s, the SUM of the available rates of the vary.sh phases
# with the delay of the fastest live link. A single TCP over it shows what is physically reachable. N runs.
N=${1:-5}
set1() { ip netns exec A tc qdisc change dev a1 root netem rate $1 delay $2 $3 25% distribution normal limit 1000
         ip netns exec B tc qdisc change dev b1 root netem rate $1 delay $2 $3 25% distribution normal limit 1000; }
phases() {
  set1 35mbit 40ms 10ms; sleep 10   # all normal
  set1 23mbit 70ms 20ms; sleep 10   # L1 worse cell 8 + L2 10 + L3 5, fastest L2
  set1 13mbit 120ms 40ms; sleep 10  # L2 down: L1 8 (150 ms) + L3 5 (120 ms)
  set1 32mbit 40ms 10ms; sleep 10   # L1 20 + L2 10 + L3 2
  set1 35mbit 40ms 10ms; sleep 10   # all normal
  set1 15mbit 70ms 20ms; sleep 10   # L1 down: L2 10 + L3 5
  set1 20mbit 40ms 10ms             # back to lab default
}
killall -q iperf3 sheaf sheaf-proxy; sleep 1
for i in $(seq 1 $N); do
  killall -q iperf3; sleep 1; ip netns exec B iperf3 -s -1 -D -B 10.1.0.2 >/dev/null 2>&1; sleep 1
  phases & 
  r=$(ip netns exec A iperf3 -c 10.1.0.2 -t 60 -f m 2>/dev/null | awk '/receiver/{print $7}')
  wait
  echo "oracle vary $i: $r"
done
for i in $(seq 1 $N); do
  set1 35mbit 40ms 10ms
  killall -q iperf3; sleep 1; ip netns exec B iperf3 -s -1 -D -B 10.1.0.2 >/dev/null 2>&1; sleep 1
  r=$(ip netns exec A iperf3 -c 10.1.0.2 -t 30 -f m 2>/dev/null | awk '/receiver/{print $7}')
  echo "oracle static $i: $r"
done
set1 20mbit 40ms 10ms
