#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Continuous operation test through the box's running sheaf service (IPsec on), no SSH to the box:
# every 5 min download 100 MB (nc | tcpbench -stdin) and upload 30 MB (tcpbench -gen | nc; the sink line from the server).
# Pause: touch /tmp/longrun.pause (a measurement round needs the box). Log: results/longrun-<date>.csv
T=${TCPBENCH:?path to a tcpbench binary for this computer}
OUT=results/longrun-$(date +%Y-%m-%d).csv
[ -f $OUT ] || echo "time,down_mbit,down_content,up_mbit,up_content" > $OUT
S() { ssh ${SERVER_SSH:?ssh options and user@host of your sheaf server} "$@"; }
while true; do
  if [ -f /tmp/longrun.pause ]; then sleep 30; continue; fi
  d=$(perl -e 'alarm 120; exec @ARGV' nc 192.168.1.1 5202 | $T -stdin 2>&1)
  dm=$(echo "$d" | sed -n 's/.*= \([0-9.]*\) Mbit.*/\1/p'); dok=$(echo "$d" | grep -q "content ok" && echo ok || echo ERROR)
  sleep 2
  $T -gen 30 | perl -e 'alarm 90; exec @ARGV' nc -w 3 192.168.1.1 5201 2>/dev/null; sleep 3
  u=$(S 'journalctl -u sheaf-bench -n 3 -o cat --no-pager | grep sink | tail -1')
  um=$(echo "$u" | sed -n 's/.*= \([0-9.]*\) Mbit.*/\1/p'); uok=$(echo "$u" | grep -q "content ok" && echo ok || echo ERROR)
  echo "$(date +%H:%M:%S),${dm:-0},$dok,${um:-0},$uok" >> $OUT
  sleep 290
done
