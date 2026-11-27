#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Latency through the bond (sheaf + IPsec + server) vs straight over each modem, and small page fetch times
# through the tunnel. Run detached while the service is up: sh /tmp/latency.sh > /tmp/latency.log 2>&1 &
i=0; while ! ip route | grep -q "default via 10.66.0.1 dev shf0" && [ $i -lt 60 ]; do sleep 2; i=$((i+1)); done
pi() { ping -c 20 -W 2 "$@" 2>&1 | tail -2 | tr '\n' ' '; echo; }
echo "== ping 1.1.1.1 through the tunnel: $(pi 1.1.1.1)"
echo "== ping 1.1.1.1 direct O2:          $(pi -I wwan0 1.1.1.1)"
echo "== ping 1.1.1.1 direct Vodafone:    $(pi -I wwan1 1.1.1.1)"
t() { awk '{print $1}' /proc/uptime; }
for u in http://example.com/ http://neverssl.com/; do
  s=""; for k in 1 2 3 4 5; do a=$(t); wget -q -O /dev/null -T 10 "$u" && b=$(t) && s="$s $(awk "BEGIN{printf \"%.0f\", ($b-$a)*1000}")"; done
  echo "== fetch $u through the tunnel (ms):$s"
done
echo DONE
