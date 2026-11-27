#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# CPU ceiling of sheaf on the board: both ends on loopback (2 "links"), 50 MB through the built-in proxy. No LTE used.
killall -q sheaf tcpbench 2>/dev/null; sleep 1
/tmp/tcpbench -listen 127.0.0.1:5202 > /tmp/sink.log 2>&1 &
echo "== direct (without sheaf):"; /tmp/tcpbench -connect 127.0.0.1:5202 -mb 50
/tmp/sheaf -tun shfb -reliable -serve -links "127.0.0.1:7101>127.0.0.1:7001,127.0.0.2:7102>127.0.0.2:7002" > /tmp/sheaf-b.log 2>&1 &
/tmp/sheaf -tun shfa -reliable -proxy "127.0.0.1:5201>127.0.0.1:5202" -links "127.0.0.1:7001>127.0.0.1:7101,127.0.0.2:7002>127.0.0.2:7102" > /tmp/sheaf-a.log 2>&1 &
sleep 2
( sleep 6; top -b -n1 | grep -E "CPU:|/tmp/sheaf" | cut -c1-70 > /tmp/top.log ) &
echo "== through sheaf (both ends on one CPU):"; /tmp/tcpbench -connect 127.0.0.1:5201 -mb 50
sleep 1; cat /tmp/top.log
killall -q sheaf tcpbench
