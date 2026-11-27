#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Board CPU ceiling without SIM data: sheaf over the cable to the computer (it forwards UDP 47001/47002 to a lab server).
# Usage: sh /tmp/sheaf-lan.sh [upload_MB] [download 0/1]
export GOMAXPROCS=${GOMAXPROCS:-2} # MT7621 = 2 cores x 2 threads: more Go Ps only spin (cable: G=2 fastest, least CPU)
SRV=${SRV:-192.168.1.10}
UP_MB=${1:-100}; DL=${2:-0}
/tmp/sheaf -reliable -key /etc/sheaf/sheaf.key -tun shf0 -trace /tmp/trace-lan.csv \
  -links "0.0.0.0:7001>$SRV:47001,0.0.0.0:7002>$SRV:47002" \
  -proxy "127.0.0.1:5201>127.0.0.1:5201;127.0.0.1:5202>127.0.0.1:5202" > /tmp/sheaf-lan.log 2>&1 &
SH_PID=$!
sleep 3
cpu() { # per-thread CPU ticks of sheaf and tcpbench over 3 s, then overall
  for p in $SH_PID $(pidof tcpbench); do for t in /proc/$p/task/*; do echo "$p $(basename $t) $(cut -d' ' -f14,15 $t/stat)"; done; done > /tmp/c1
  head -1 /proc/stat > /tmp/s1; sleep 3; head -1 /proc/stat > /tmp/s2
  for p in $SH_PID $(pidof tcpbench); do for t in /proc/$p/task/*; do echo "$p $(basename $t) $(cut -d' ' -f14,15 $t/stat)"; done; done > /tmp/c2
  awk 'NR==FNR{a[$1" "$2]=$3+$4;next} {d=$3+$4-a[$1" "$2]; if(d>5) printf "  pid %s thread %s: %d %% of one CPU\n",$1,$2,d/3}' /tmp/c1 /tmp/c2
  paste /tmp/s1 /tmp/s2 | awk '{u=$13-$2;s=$15-$4;i=$16-$5;q=$19-$8; t=u+$14-$3+s+i+q+$17-$6+$18-$7; printf "  total: usr %d%% sys %d%% sirq %d%% idle %d%%\n",100*u/t,100*s/t,100*q/t,100*i/t}'
}
echo "== upload $UP_MB MB over the cable:"
( sleep 2; cpu ) &
/tmp/tcpbench -connect 127.0.0.1:5201 -mb $UP_MB 2>&1 | tail -1
wait $!
if [ "$DL" = 1 ]; then
  echo "== download over the cable:"
  ( sleep 2; cpu ) &
  /tmp/tcpbench -connect 127.0.0.1:5202 -recv 2>&1 | tail -1
  wait $!
fi
kill $SH_PID; sleep 1; grep -E 'L1 |dead' /tmp/sheaf-lan.log | tail -3
echo DONE
