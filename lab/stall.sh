#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Liveness reproducer for the "download stalls ~50 s, then finishes ok" bug (2026-11-24).
# B = long-running server (-serve, keeps its windows from the previous run), A = the box: a FRESH sheaf process
# for every run (new session, like the real test), one short download per run (tcpbench -recv, content check).
# Bursty loss on the links (netem gemodel) + short queues: the server's first burst after the session reset
# loses packets, also the very first ones on a link, and some NACKs on the way back.
# Usage (lab VM, root):  BIN=/tmp/sheaf-x N=60 MB=20 bash stall.sh
#   LB / LA = netem loss spec B->A (data) / A->B (feedback, NACKs); LIMIT = netem queue (packets) on B->A.
# Output: one line per run (seconds, Mbit/s, verdict); runs slower than SLOW s keep their logs in $LOG/stall-slow-<i>-{A,B}.log.
# C1 (HLT) idle state off for the whole measurement: the lab VM oversleeps timers by 100-600 ms when a vCPU
# halts (netem and sheaf freeze, false dead links). The previous state is restored on any exit. NOHLT=0 keeps C1.
idle_hlt() { for f in /sys/devices/system/cpu/cpu*/cpuidle/state1/disable; do echo $1 > $f; done; } # 1 = no HLT
c1_off() { [ "${NOHLT:-1}" = 1 ] || return 0; c1_was=$(cat /sys/devices/system/cpu/cpu0/cpuidle/state1/disable); idle_hlt 1
           trap "idle_hlt $c1_was; $1" EXIT; trap 'exit 130' INT TERM HUP; }

BIN=${BIN:-/tmp/sheaf}; N=${N:-60}; MB=${MB:-20}; SLOW=${SLOW:-10}; LIMIT=${LIMIT:-300}
LB=${LB:-"loss gemodel 1% 25%"}; LA=${LA:-"loss gemodel 1% 25%"}
TB=${TB:-/tmp/tcpbench}; K=/tmp/sheaf-lab.key; LOG=/var/log/sim; mkdir -p $LOG
L="10.1.0.1:7000>10.1.0.2:7000,10.2.0.1:7000>10.2.0.2:7000"
R="10.1.0.2:7000>-,10.2.0.2:7000>-"
DN=(0 30 40); UP=(0 20 25)
qd() { ip netns exec $1 tc qdisc replace dev $2 root netem rate $3 delay 20ms 3ms 25% distribution normal limit $4 $5; }
setup() { local i; for i in 1 2; do qd B b$i ${DN[$i]}mbit $LIMIT "$LB"; qd A a$i ${UP[$i]}mbit 1000 "$LA"; done; }
stop() { pkill -x 'sheaf(-.*)?'; pkill -x "$(basename $TB)"; sleep 1; for ns in A B; do ip netns exec $ns ip link del shf0 2>/dev/null; done; }
startA() { ip netns exec A $BIN -reliable -key $K -tun shf0 -links "$L" -proxy "127.0.0.1:5202>127.0.0.1:5202" > $LOG/stall-A.log 2>&1 & A=$!; }
trap 'stop; LB=; LA=; setup; bash /tmp/download.sh restore >/dev/null 2>&1' EXIT # no loss left behind
c1_off 'stop; LB=; LA=; setup; bash /tmp/download.sh restore >/dev/null 2>&1'
stop; setup
ip netns exec B $BIN -reliable -serve -key $K -tun shf0 -links "$R" > $LOG/stall-B.log 2>&1 &
ip netns exec B $TB -listen 127.0.0.1:5202 -send $MB > $LOG/stall-src.log 2>&1 &
sleep 1
for i in $(seq 1 $N); do
  off=$(stat -c %s $LOG/stall-B.log)
  startA; sleep 1
  t0=$(date +%s.%N)
  r=$(ip netns exec A timeout 120 $TB -connect 127.0.0.1:5202 -recv 2>&1 | tail -1)
  t=$(echo "$(date +%s.%N) - $t0" | bc)
  kill $A; sleep 0.5; ip netns exec A ip link del shf0 2>/dev/null
  printf "%s run %d: %.1f s | %s | dead A=%d B=%d\n" "$(date +%T)" $i $t "$r" $(grep -c ' dead' $LOG/stall-A.log) $(tail -c +$((off + 1)) $LOG/stall-B.log | grep -c ' dead')
  if [ $(echo "$t > $SLOW" | bc) = 1 ]; then
    cp $LOG/stall-A.log $LOG/stall-slow-$i-A.log; tail -c +$((off + 1)) $LOG/stall-B.log > $LOG/stall-slow-$i-B.log
  fi
done
