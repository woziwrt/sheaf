#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Data integrity under link outages (lab VM): B = server keeps running, A restarts before every run (new session,
# like a box reboot), vary.sh drops links meanwhile, tcpbench checks every byte both ways.
# Usage: BIN=/tmp/sheaf-v30 MB=60 SCEN=vary|blackout RESTART=A|B bash /tmp/integrity.sh [runs]
#   RESTART=A: the client restarts before every run (box reboot); RESTART=B: the server restarts instead.
# C1 (HLT) idle state off for the whole measurement: the lab VM oversleeps timers by 100-600 ms when a vCPU
# halts (netem and sheaf freeze, false dead links). The previous state is restored on any exit. NOHLT=0 keeps C1.
idle_hlt() { for f in /sys/devices/system/cpu/cpu*/cpuidle/state1/disable; do echo $1 > $f; done; } # 1 = no HLT
c1_off() { [ "${NOHLT:-1}" = 1 ] || return 0; c1_was=$(cat /sys/devices/system/cpu/cpu0/cpuidle/state1/disable); idle_hlt 1
           trap "idle_hlt $c1_was; $1" EXIT; trap 'exit 130' INT TERM HUP; }
c1_off
BIN=${BIN:-/tmp/sheaf-v30}; N=${1:-4}; MB=${MB:-60}; K=/tmp/sheaf-lab.key; LOG=/var/log/sim
SCEN=${SCEN:-vary}; RESTART=${RESTART:-A}
L="10.1.0.1:7000>10.1.0.2:7000,10.2.0.1:7000>10.2.0.2:7000,10.3.0.1:7000>10.3.0.2:7000"
R="10.1.0.2:7000>-,10.2.0.2:7000>-,10.3.0.2:7000>-"   # the server learns the client's addresses
pkill -x 'sheaf(-.*)?'; pkill -x tcpbench; pkill -f "bash /tmp/(vary|drive|blackout).sh"; sleep 1
for ns in A B; do ip netns exec $ns ip link del shf0 2>/dev/null; done
startB() { ip netns exec B $BIN -reliable -serve -key $K -tun shf0 -links "$R" >> $LOG/int-B.log 2>&1 & B=$!; }
startA() { ip netns exec A $BIN -reliable -key $K -tun shf0 -links "$L" -proxy "127.0.0.1:5201>127.0.0.1:5201;127.0.0.1:5202>127.0.0.1:5202" >> $LOG/int-A.log 2>&1 & A=$!; }
: > $LOG/int-B.log; : > $LOG/int-A.log
startB
[ $RESTART = B ] && { sleep 1; startA; }
ip netns exec B /tmp/tcpbench -listen 127.0.0.1:5201 > $LOG/int-sink.log 2>&1 &
ip netns exec B /tmp/tcpbench -listen 127.0.0.1:5202 -send $MB > $LOG/int-src.log 2>&1 &
sleep 1
for i in $(seq 1 $N); do
  if [ $RESTART = A ]; then startA; sleep 2
  else kill $B; sleep 1; ip netns exec B ip link del shf0 2>/dev/null; startB; sleep 4; fi   # server restart: the client must notice and reset
  bash /tmp/$SCEN.sh >/dev/null 2>&1 &
  echo "run $i: $(ip netns exec A timeout 120 /tmp/tcpbench -connect 127.0.0.1:5201 -mb $MB 2>&1 | tail -1)"
  echo "run $i: $(ip netns exec A timeout 120 /tmp/tcpbench -connect 127.0.0.1:5202 -recv 2>&1 | tail -1)"
  while pgrep -f "bash /tmp/$SCEN.sh" >/dev/null; do sleep 1; done
  [ $RESTART = A ] && { kill $A; sleep 1; ip netns exec A ip link del shf0 2>/dev/null; }
done
echo "== server (sink):"; cat $LOG/int-sink.log
echo "== events:"; grep -h -E 'unknown stream|aborted|restarted|dead' $LOG/int-B.log $LOG/int-A.log | cut -c21- | sed -E 's/[0-9]+ packets/N packets/' | sort | uniq -c | head
pkill -x 'sheaf(-.*)?'; pkill -x tcpbench
