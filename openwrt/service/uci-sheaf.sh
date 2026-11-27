#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# One-time board setup for internet through the server: firewall zone "sheaf" on shf0 (masquerade, MSS clamp),
# LAN may forward into it; dnsmasq asks public resolvers (through the tunnel - the default route) instead of none.
uci -q delete firewall.sheaf
uci set firewall.sheaf=zone
uci set firewall.sheaf.name='sheaf'
uci add_list firewall.sheaf.device='shf0'
uci set firewall.sheaf.input='REJECT'
uci set firewall.sheaf.output='ACCEPT'
uci set firewall.sheaf.forward='REJECT'
uci set firewall.sheaf.masq='1'
uci set firewall.sheaf.mtu_fix='1'
uci -q delete firewall.lan_sheaf
uci set firewall.lan_sheaf=forwarding
uci set firewall.lan_sheaf.src='lan'
uci set firewall.lan_sheaf.dest='sheaf'
uci commit firewall
uci -q delete dhcp.@dnsmasq[0].server
uci add_list dhcp.@dnsmasq[0].server='1.1.1.1'
uci add_list dhcp.@dnsmasq[0].server='9.9.9.9'
uci set dhcp.@dnsmasq[0].noresolv='1'
uci commit dhcp
/etc/init.d/firewall reload >/dev/null 2>&1
/etc/init.d/dnsmasq restart >/dev/null 2>&1
nft list ruleset | grep -c shf0
