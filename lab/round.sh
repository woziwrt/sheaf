#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# One measurement round over real LTE, driven from a computer on the box's LAN cable:
#   service stop -> raw per-modem TCP (up 10 MB, down 30 MB) -> sheaf without IPsec -> sheaf with IPsec (both: down 100 MB
#   via nc + tcpbench -stdin, up 30 MB via tcpbench -gen | nc, sink result from the server) -> service start -> latency.
# The box runs raw-up.sh, sheaf-lte-ipsec.sh and latency.sh (openwrt/, copied to the box's /tmp beforehand);
# they print READY1 / READY2 / DONE and one "data used" line per modem.
# Usage: ./round.sh NAME   (results in results/round-NAME.log). Box SSH: one call at a time, every 10 s at most.
N=${1:?name}; OUT=results/round-$N.log
ESP=${ESP:-aes256-sha256-ecp384}   # ESP proposal of the IPsec phase (the server accepts aes256 and aes128)
T=${TCPBENCH:?path to a tcpbench binary for this computer}
B() { perl -e 'alarm 20; exec @ARGV' ssh ${BOX_KEY:+-i $BOX_KEY} -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8 -o BatchMode=yes -o LogLevel=ERROR root@192.168.1.1 "$@"; }
S() { ssh ${SERVER_SSH:?ssh options and user@host of your sheaf server} "$@"; }
waitfor() { for i in $(seq 1 40); do B "grep -q '^$1' /tmp/round.log" && return 0; sleep 10; done; echo "TIMEOUT $1"; return 1; }
transfer() { # label
  echo "== $1 down: $(perl -e 'alarm 90; exec @ARGV' nc 192.168.1.1 5202 | $T -stdin)"
  sleep 1
  $T -gen 30 | perl -e 'alarm 60; exec @ARGV' nc -w 3 192.168.1.1 5201; sleep 2
  echo "== $1 up: $(S 'journalctl -u sheaf-bench -n 3 -o cat --no-pager | grep sink | tail -1')"
}
{
echo "######## round $N $(date '+%d. %m. %H:%M'), ESP $ESP"
S 'cd /opt/sheaf; sh sheaf-server.sh src 100 >/dev/null; sh sheaf-server.sh raw-on 30 >/dev/null; echo server ready'
B '/etc/init.d/sheaf stop >/dev/null 2>&1; sleep 12; n=$(ps w | grep -cE "[s]heaf-run|[c]haron|/usr/sbin/[s]heaf "); [ $n = 0 ] || { echo "SERVICE STILL RUNNING ($n) - stop"; exit 1; }; rm -f /tmp/round.log /tmp/stop1 /tmp/stop2; (sh -c "sh /tmp/raw-up.sh 10 1; echo RAWDONE; sleep 3; ESP='"$ESP"' sh /tmp/sheaf-lte-ipsec.sh" > /tmp/round.log 2>&1 < /dev/null &); echo box started' || exit 1
waitfor READY1 && { B 'grep -E "^(== |upload|download)" /tmp/round.log'; transfer "without IPsec"; B 'touch /tmp/stop1'; }
waitfor READY2 && { transfer "with IPsec"; B 'touch /tmp/stop2'; }
waitfor DONE; sleep 12  # raw-up's DONE comes first; the second one ends the round
for i in $(seq 1 20); do [ "$(B 'grep -c ^DONE /tmp/round.log')" -ge 2 ] && break; sleep 10; done
B 'grep -E "data used" /tmp/round.log; /etc/init.d/sheaf start; echo service started'
S 'cd /opt/sheaf; sh sheaf-server.sh raw-off >/dev/null; echo raw off'
B 'rm -f /tmp/latency.log; (sh /tmp/latency.sh > /tmp/latency.log 2>&1 < /dev/null &); echo latency started'
for i in $(seq 1 18); do B 'grep -q ^DONE /tmp/latency.log' && break; sleep 10; done
B 'grep -v DONE /tmp/latency.log'
} 2>&1 | tee $OUT
