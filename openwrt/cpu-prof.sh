#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# CPU profile of the sending side (A) on the board: loopback, proxy, 50 MB
killall -q sheaf tcpbench 2>/dev/null; sleep 1
/tmp/tcpbench -listen 127.0.0.1:5202 > /tmp/sink.log 2>&1 &
/tmp/sheaf -tun shfb -reliable -serve -links "127.0.0.1:7101>127.0.0.1:7001,127.0.0.2:7102>127.0.0.2:7002" -cpuprofile /tmp/prof-B.out > /tmp/sheaf-b.log 2>&1 &
/tmp/sheaf -tun shfa -reliable -proxy "127.0.0.1:5201>127.0.0.1:5202" -links "127.0.0.1:7001>127.0.0.1:7101,127.0.0.2:7002>127.0.0.2:7102" -cpuprofile /tmp/prof-A.out > /tmp/sheaf-a.log 2>&1 &
sleep 2
/tmp/tcpbench -connect 127.0.0.1:5201 -mb 50
killall sheaf; sleep 2; killall -q tcpbench; ls -la /tmp/prof-*.out
