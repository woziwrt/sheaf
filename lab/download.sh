#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# LTE-downlink-like lab scenario (2 links) for the DOWNLOAD problem: B (server, -serve) sends, A (box) receives.
# B->A (b1, b2) = LTE downlinks: 30 / 40 Mbit, 17 ms, deep queue (LIMIT packets), optional rate wobble (WOBBLE=1:
# every 100-400 ms each downlink jumps to WLO-WHI % (default 50-150) of its mean, like a base station scheduler with other users).
# A->B (a1, a2) = LTE uplinks: 20 / 25 Mbit, 17 ms, normal queue (WOBUP=1: the uplinks wobble too). Link 3 is not used.
# Usage (on the lab VM, as root):
#   download.sh setup            configure the links (LIMIT, WOBBLE from the environment)
#   download.sh raw [s]          raw TCP download per link (iperf3 -R), s seconds; RAWUP=1: upload
#   download.sh down BIN [N]     N sheaf downloads (MB megabytes, tcpbench -recv, content check), trace of run 1
#   download.sh up BIN [N]       N sheaf uploads in the same scenario
#   download.sh restore          back to the default lab profile (20/10/5 Mbit, limit 1000) for bench.sh & co.
# NOHLT=1 (default): the lab VM oversleeps timers by 100-600 ms when a vCPU halts in idle (ping -i 0.01 on loopback
# shows the gaps even with nothing running, while ping from the host to the VM stays < 7 ms). That freezes netem (both links
# at once) and the sheaf timers -> false "dead" links. With the C1 (HLT) idle state disabled the vCPUs poll and the gaps
# are gone. Runtime only, switched back at the end (costs host CPU while tests run). Every run reports the worst stall.
#   download.sh nohlt | hlt     switch it by hand (e.g. around bench.sh)
# JIT=4 (default): base delay jitter (ms, normal, both directions, 17 ms base); JIT=12 = jittery base RTT. Netem keeps
# packets in order with a rate, so a standing queue absorbs the jitter of its own direction: on the data direction it
# shows only when the queue is empty, on the feedback direction (no queue) always.
# TRACEALL=1: sender trace of every run ($LOG/st-<mode>-<bin>-<scenario>-r<i>-{A,B}.csv), not only of run 1.
# C1 (HLT) idle state off for the whole measurement: the lab VM oversleeps timers by 100-600 ms when a vCPU
# halts (netem and sheaf freeze, false dead links). The previous state is restored on any exit. NOHLT=0 keeps C1.
idle_hlt() { for f in /sys/devices/system/cpu/cpu*/cpuidle/state1/disable; do echo $1 > $f; done; } # 1 = no HLT
c1_off() { [ "${NOHLT:-1}" = 1 ] || return 0; c1_was=$(cat /sys/devices/system/cpu/cpu0/cpuidle/state1/disable); idle_hlt 1
           trap "idle_hlt $c1_was; $1" EXIT; trap 'exit 130' INT TERM HUP; }

LIMIT=${LIMIT:-2000}; WLO=${WLO:-50}; WHI=${WHI:-150}; MB=${MB:-150}; WOBBLE=${WOBBLE:-1}; JIT=${JIT:-4}; NOHLT=${NOHLT:-1}; K=/tmp/sheaf-lab.key; LOG=/var/log/sim; mkdir -p $LOG
L="10.1.0.1:7000>10.1.0.2:7000,10.2.0.1:7000>10.2.0.2:7000"
R="10.1.0.2:7000>-,10.2.0.2:7000>-"
DN=(0 ${DN1:-30} ${DN2:-40}); UP=(0 20 25)
# QMS=700: each downlink queue holds QMS ms at its mean rate (Vodafone ramp 24 Nov: ~650-700 ms of buffer at capacity
# before loss, knee ~50 Mbit) instead of LIMIT packets on both. DN1/DN2 = downlink mean rates (Mbit).
LIM=(0 $LIMIT $LIMIT); if [ -n "$QMS" ]; then for i in 1 2; do LIM[$i]=$(( ${DN[$i]} * 1000 * QMS / 8 / 1468 )); done; fi
qd() { # ns dev rate limit
  ip netns exec $1 tc qdisc change dev $2 root netem rate $3 delay 17ms ${JIT}ms 25% distribution normal limit $4 2>/dev/null ||
  ip netns exec $1 tc qdisc replace dev $2 root netem rate $3 delay 17ms ${JIT}ms 25% distribution normal limit $4
}
setup() { local i; for i in 1 2; do qd B b$i ${DN[$i]}mbit ${LIM[$i]}; qd A a$i ${UP[$i]}mbit 1000; done; }
wobble() { # seconds seed: random rate jumps on the downlinks (mean stays 30 / 40)
  local i end=$(( $(date +%s) + $1 )); RANDOM=$2
  while [ $(date +%s) -lt $end ]; do
    for i in 1 2; do qd B b$i $(( ${DN[$i]} * (WLO + RANDOM % (WHI - WLO + 1)) / 100 ))mbit ${LIM[$i]}
      [ "$WOBUP" = 1 ] && qd A a$i $(( ${UP[$i]} * (WLO + RANDOM % (WHI - WLO + 1)) / 100 ))mbit 1000; done
    sleep 0.$((1 + RANDOM % 4))
  done
  setup
}
stop() { pkill -x 'sheaf(-.*)?'; pkill -x tcpbench; killall -q iperf3; pkill -f 'download.sh wob'; pkill -f 'ping -D -i 0.01 127.0.0.1'; sleep 1
         for ns in A B; do ip netns exec $ns ip link del shf0 2>/dev/null; done; }
start() { # BIN tracetag
  local tr=""; [ -n "$2" ] && tr="-trace $LOG/st-$2"
  ip netns exec B $1 -reliable -serve -key $K -tun shf0 -links "$R" ${tr:+$tr-B.csv} > $LOG/st-B.log 2>&1 &
  sleep 0.5
  ip netns exec A $1 -reliable -key $K -tun shf0 -links "$L" -proxy "127.0.0.1:5201>127.0.0.1:5201;127.0.0.1:5202>127.0.0.1:5202" ${tr:+$tr-A.csv} > $LOG/st-A.log 2>&1 &
  ip netns exec B /tmp/tcpbench -listen 127.0.0.1:5201 > $LOG/st-sink.log 2>&1 &
  ip netns exec B /tmp/tcpbench -listen 127.0.0.1:5202 -send $MB > $LOG/st-src.log 2>&1 &
  sleep 2
}
stall_start() { ping -D -i 0.01 127.0.0.1 > $LOG/st-stall.txt 2>&1 & }
stall_report() { pkill -f 'ping -D -i 0.01 127.0.0.1'
  awk '/bytes from/{t=substr($1,2,length($1)-2)+0; if(p && t-p>0.05){g++; if(t-p>m)m=t-p}; p=t} END{printf "stalls>50ms %d max %.0f ms", g, m*1000}' $LOG/st-stall.txt; }
case $1 in raw|down|up) c1_off ;; esac
case $1 in
nohlt) idle_hlt 1 ;;
hlt) idle_hlt 0 ;;
setup) setup; ip netns exec B tc qdisc show dev b1; ip netns exec B tc qdisc show dev b2 ;;
wob) wobble $2 $3 ;;
raw)
  stop; setup
  for i in 1 2; do
    ip netns exec B iperf3 -s -1 -D -B 10.$i.0.2 >/dev/null 2>&1; sleep 1
    [ $WOBBLE = 1 ] && { bash $0 wob $((${2:-25} + 3)) $((70 + i)) & }
    stall_start
    echo "raw L$i: $(ip netns exec A iperf3 -c 10.$i.0.2 $([ "$RAWUP" = 1 ] || echo -R) -t ${2:-25} -f m 2>/dev/null | awk '/receiver/{print $7}') Mbit/s | $(stall_report)"
    pkill -f 'download.sh wob'; sleep 1; setup
  done ;;
down|up)
  BIN=$2; N=${3:-5}
  for i in $(seq 1 $N); do
    sc="w$WOBBLE-$WLO$([ $JIT = 4 ] || echo "-j$JIT")$([ -n "$QMS" ] && echo "-q$QMS-${DN[1]}-${DN[2]}")"
    stop; setup; start $BIN $([ $i = 1 ] && echo "$1-$(basename $BIN)-$sc"; [ $i != 1 ] && [ "$TRACEALL" = 1 ] && echo "$1-$(basename $BIN)-$sc-r$i")
    [ $WOBBLE = 1 ] && { bash $0 wob 60 $((40 + i)) & }
    stall_start
    if [ $1 = down ]; then r=$(ip netns exec A timeout 90 /tmp/tcpbench -connect 127.0.0.1:5202 -recv 2>&1 | tail -1)
    else r=$(ip netns exec A timeout 90 /tmp/tcpbench -connect 127.0.0.1:5201 -mb $((MB / 2)) 2>&1 | tail -1); sleep 1; r="$r | $(tail -1 $LOG/st-sink.log)"; fi
    echo "$(date +%T) $(basename $BIN) $1 $i: $r | dead A=$(grep -c ' dead' $LOG/st-A.log) B=$(grep -c ' dead' $LOG/st-B.log) | $(stall_report)"
    [ $i = 1 ] && cp $LOG/st-B.log $LOG/st-$1-$(basename $BIN)-$sc-B.log && cp $LOG/st-A.log $LOG/st-$1-$(basename $BIN)-$sc-A.log
  done
  stop; setup ;;
restore)
  stop
  for i in 1 2 3; do case $i in 1) r=20mbit; d=40ms; j=10ms;; 2) r=10mbit; d=70ms; j=20ms;; 3) r=5mbit; d=120ms; j=40ms;; esac
    for ns in A B; do dev=$([ $ns = A ] && echo a$i || echo b$i)
      ip netns exec $ns tc qdisc change dev $dev root netem rate $r delay $d $j 25% distribution normal limit 1000; done; done ;;
esac
