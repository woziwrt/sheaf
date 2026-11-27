#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# "Driving" scenario: each link changes on its own every 2–6 s – short outages (handover / dead spot, 0.3–2 s),
# cell changes (rate and delay jump). Repeatable: same seed = same drive. Usage: drive.sh [seconds] [seed]
DUR=${1:-60}; SEED=${2:-42}
# ORACLE=1: do not change the 3 links; keep each link's state in a file and give link 1 alone the SUM of the
# live links' rates and the delay of the fastest live one = the ceiling a perfect bonding could reach on this drive.
real_set() { ip netns exec A tc qdisc change dev a$1 root netem rate $2 delay $3 $4 25% distribution normal loss $5 limit 1000
             ip netns exec B tc qdisc change dev b$1 root netem rate $2 delay $3 $4 25% distribution normal loss $5 limit 1000; }
set_link() { # link rate(…mbit) delay(…ms) jitter loss
  if [ "$ORACLE" != 1 ]; then real_set "$@"; return; fi
  local up=1; [ "$5" = 100% ] && up=0
  echo "$up ${2%mbit} ${3%ms}" > /tmp/drv.$1
  ( flock 9
    local sum=0 dmin=99999 n=0
    for k in 1 2 3; do read u r d < /tmp/drv.$k 2>/dev/null || continue
      [ "$u" = 1 ] || continue; sum=$((sum + r)); [ $d -lt $dmin ] && dmin=$d; n=$((n+1)); done
    if [ $n = 0 ]; then real_set 1 1mbit 40ms 10ms 100%; else real_set 1 ${sum}mbit ${dmin}ms $((dmin/4))ms 0%; fi
  ) 9>/tmp/drv.lock
}
drive() { # link base-rate-mbit base-delay-ms seed
  local i=$1 R=$2 D=$3 t=0; RANDOM=$4
  while [ $t -lt $DUR ]; do
    local ev=$((RANDOM % 10)) step=$((2 + RANDOM % 5))
    if [ $ev -lt 2 ]; then                       # 20 %: outage 0.3–2 s, then same cell
      local out=$((3 + RANDOM % 18))
      set_link $i ${R}mbit ${D}ms $((D/4))ms 100%; echo "$(date +%T.%N | cut -c1-12) L$i outage $((out*100)) ms"
      sleep 0.$((out % 10)); [ $out -ge 10 ] && sleep 1
      set_link $i ${R}mbit ${D}ms $((D/4))ms 0%
    else                                          # 80 %: new cell 30–130 % of base rate, 70–250 % of base delay
      local r=$(( R * (30 + RANDOM % 101) / 100 )); [ $r -lt 1 ] && r=1
      local d=$(( D * (70 + RANDOM % 181) / 100 ))
      set_link $i ${r}mbit ${d}ms $((d/4))ms 0%; echo "$(date +%T.%N | cut -c1-12) L$i cell ${r} Mbit ${d} ms"
    fi
    sleep $step; t=$((t + step + 1))
  done
}
if [ "$ORACLE" = 1 ]; then echo "1 20 40" > /tmp/drv.1; echo "1 10 70" > /tmp/drv.2; echo "1 5 120" > /tmp/drv.3; set_link 1 20mbit 40ms 10ms 0%; fi
drive 1 20 40 $SEED & drive 2 10 70 $((SEED+1)) & drive 3 5 120 $((SEED+2)) &
wait
real_set 1 20mbit 40ms 10ms 0%; real_set 2 10mbit 70ms 20ms 0%; real_set 3 5mbit 120ms 40ms 0%
