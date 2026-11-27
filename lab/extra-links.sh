#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Lab (Linux VM, root): the server has MORE links than the box uses (25 Nov 2026: R4 hub with 2 links against a server
# instance with 4 stalled - both links "dead", data only resent; with 2 links on the server it ran).
# Both ends on this machine over loopback, each with its own TUN. Downloads 20 MB through the proxy for each server
# link count in SRVLINKS (default "2 4"); prints Mbit/s or STALL.
#   sudo ./extra-links.sh [dir with sheaf + tcpbench]      (SRVLINKS="2 4 4" to repeat)
#   UPMB=100: also upload this many MB (box -> server) after the download
#   SHEAF_SEQBASE=4294947295: both ends number from just below 2^32 (wrap test)
#   CPORTS="47023 47024": the box uses the server's links 3 and 4 (a hub with only satellite 2)
set -u
D=${1:-.}
K=/tmp/lnk.key; echo 0123456789abcdef0123456789abcdef > $K
stop() { pkill -9 -f "sheaf -reliable" 2>/dev/null; sleep 1; }
stop; pkill -f "tcpbench -listen" 2>/dev/null; sleep 1
$D/tcpbench -listen 127.0.0.1:5202 -send ${SENDMB:-20} >/dev/null 2>&1 &
BP=$!
$D/tcpbench -listen 127.0.0.1:5201 >/dev/null 2>&1 &   # sink for the upload
UP=$!
for n in ${SRVLINKS:-2 4}; do
	sl=""; for i in $(seq 1 $n); do sl="$sl${sl:+,}127.0.0.1:4702$i>-"; done
	$D/sheaf -reliable -serve -key $K -tun shfS -links "$sl" -mode peer -status /tmp/lnk-s.json > /tmp/lnk-s-$n.log 2>&1 &
	SP=$!; sleep 1; ip addr replace 10.99.0.1/30 dev shfS 2>/dev/null; ip link set shfS up 2>/dev/null
	set -- ${CPORTS:-47021 47022}
	$D/sheaf -reliable -key $K -tun shfC -links "127.0.0.2:7101>127.0.0.1:$1,127.0.0.3:7102>127.0.0.1:$2" \
		-proxy "127.0.0.1:6202>127.0.0.1:5202;127.0.0.1:6201>127.0.0.1:5201" -status /tmp/lnk-c.json > /tmp/lnk-c-$n.log 2>&1 &
	CP=$!; sleep 1; ip addr replace 10.99.0.2/30 dev shfC 2>/dev/null; ip link set shfC up 2>/dev/null
	for w in $(seq 1 20); do ss -ltn | grep -q "127.0.0.1:6202 " && break; sleep 0.5; done
	r=$(timeout ${TMO:-40} $D/tcpbench -connect 127.0.0.1:6202 -recv 2>&1 | tail -1)
	echo "server $n links, box 2 links (${CPORTS:-47021 47022}): ${r:-STALL (nothing in 40 s)}"
	if [ -n "${UPMB:-}" ]; then
		u=$(timeout ${TMO:-40} $D/tcpbench -connect 127.0.0.1:6201 -mb $UPMB 2>&1 | tail -1)
		echo "   upload $UPMB MB: ${u:-STALL}"
	fi
	stop
done
kill $BP $UP 2>/dev/null
echo "logs: /tmp/lnk-[sc]-<n>.log"
