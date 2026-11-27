#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# LTE bonding lab: netns A (LTE side) <-> netns B (server) over 3 emulated LTE links.
# Usage: lab.sh up | single | bond | down | show
set -u
# link profiles: name  rate  delay  jitter  (each direction gets its own netem; upload slower like LTE)
PROF=("l1 20mbit 40ms 10ms" "l2 10mbit 70ms 20ms" "l3 5mbit 120ms 40ms")
A="ip netns exec A"; B="ip netns exec B"
netem() { # dev ns rate delay jitter
  ip netns exec "$2" tc qdisc replace dev "$1" root netem rate "$3" delay "$4" "$5" 25% distribution normal limit 1000
}
case "${1:-}" in
up)
  ip netns add A 2>/dev/null; ip netns add B 2>/dev/null
  $A ip link set lo up; $B ip link set lo up
  i=0; for p in "${PROF[@]}"; do set -- $p; i=$((i+1))
    ip link add a$i netns A type veth peer name b$i netns B 2>/dev/null
    $A ip addr replace 10.$i.0.1/24 dev a$i; $A ip link set a$i up
    $B ip addr replace 10.$i.0.2/24 dev b$i; $B ip link set b$i up
    netem a$i A "$2" "$3" "$4"; netem b$i B "$2" "$3" "$4"
  done
  modprobe bonding; modprobe sch_netem; echo "lab up"; "$0" show ;;
single) # TCP over link 1 only
  $B iperf3 -s -1 -D -B 10.1.0.2 >/dev/null; sleep 1
  $A iperf3 -c 10.1.0.2 -t 20 -f m | grep -E 'sender|receiver' ;;
bond) # balance-rr over all three links
  for ns in A B; do
    ip netns exec $ns ip link add bond0 type bond mode balance-rr 2>/dev/null
    for i in 1 2 3; do d=$([ $ns = A ] && echo a$i || echo b$i); ip netns exec $ns ip link set $d down; ip netns exec $ns ip link set $d master bond0; done
    ip netns exec $ns ip link set bond0 up
  done
  $A ip addr replace 10.9.0.1/24 dev bond0; $B ip addr replace 10.9.0.2/24 dev bond0
  # netem stays on the slaves; bond itself without qdisc
  i=0; for p in "${PROF[@]}"; do set -- $p; i=$((i+1)); netem a$i A "$2" "$3" "$4"; netem b$i B "$2" "$3" "$4"; done
  sleep 2; $A ping -c 3 -q 10.9.0.2 | tail -1
  $B iperf3 -s -1 -D -B 10.9.0.2 >/dev/null; sleep 1
  $A iperf3 -c 10.9.0.2 -t 20 -f m | grep -E 'sender|receiver'
  $A nstat -az TcpExtTCPOFOQueue TcpExtTCPSACKReorder TcpRetransSegs 2>/dev/null | tail -3 ;;
show)
  for ns in A B; do echo "== $ns"; ip netns exec $ns tc -s qdisc show | grep -E 'qdisc netem' ; done ;;
down)
  pkill -f 'iperf3 -s' ; ip netns del A 2>/dev/null; ip netns del B 2>/dev/null; echo "lab down" ;;
esac
