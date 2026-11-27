#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# One-time setup of a BPI-R4 (an OpenWrt 25.12 build for the BPI-R4, e.g. github.com/woziwrt/bpi-r4-deploy) as the
# sheaf hub. Run on the R4:
#   ssh root@192.168.1.1 'sh -s' < hub-setup.sh
# Ports: lan3 = the computer (stays in br-lan together with Wi-Fi), lan1 = satellite 1 (10.77.1.0/24),
# lan2 = satellite 2 (10.77.2.0/24), wan free. Each satellite has its own port and network: no switch, no two
# factory 192.168.1.1 boxes and DHCP servers on one segment.
# Safe to run again. The network is reloaded detached at the end (the SSH session may drop for a moment).
set -e
uci set system.@system[0].hostname=sheaf-hub

br=$(uci show network | sed -n "s/^\(network\.[^.]*\)\.name='br-lan'$/\1/p" | head -1)
[ -n "$br" ] || { echo "no br-lan device section" >&2; exit 1; }
for p in lan1 lan2; do uci -q del_list $br.ports=$p || true; done

n=0
for p in lan1 lan2; do
	n=$((n+1))
	uci set network.sat$n=interface
	uci set network.sat$n.proto=static
	uci set network.sat$n.device=$p
	uci set network.sat$n.ipaddr=10.77.$n.1
	uci set network.sat$n.netmask=255.255.255.0
done

# satellites are our own boxes: they may reach the hub (ping, diagnostics), nothing is forwarded to or from them;
# the hub's link traffic leaves from its own addresses (output)
uci -q get firewall.sat >/dev/null || uci set firewall.sat=zone
uci set firewall.sat.name=sat
uci -q delete firewall.sat.network || true
uci add_list firewall.sat.network=sat1
uci add_list firewall.sat.network=sat2
uci set firewall.sat.input=ACCEPT
uci set firewall.sat.output=ACCEPT
uci set firewall.sat.forward=REJECT

uci commit system
uci commit network
uci commit firewall
echo sheaf-hub > /proc/sys/kernel/hostname
setsid sh -c 'sleep 1; /etc/init.d/network reload; /etc/init.d/firewall reload' >/dev/null 2>&1 < /dev/null &
echo "hub setup done: lan1 = 10.77.1.1/24, lan2 = 10.77.2.1/24, lan3 + Wi-Fi = br-lan (network reloading)"
