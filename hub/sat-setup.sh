#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Turn an RBM33G box into satellite N of the BPI-R4 hub. Run on the box:
#   ssh ... root@<box> 'N=1 sh -s' < sat-setup.sh        (box 1 -> satellite 1 on the hub's lan1)
#   ssh ... root@<box> 'N=2 sh -s' < sat-setup.sh        (box 2 -> satellite 2 on the hub's lan2)
# Satellite N: LAN 10.77.N.1N/24 (10.77.1.11, 10.77.2.12), no DHCP server (its own network with the hub, nothing else
# on it), sheaf-run in the role "satellite": it only dials its two modems and sends the hub's link addresses
# (10.77.1.101/.102 or 10.77.2.103/.104) out through them, masqueraded. No sheaf, no IPsec on the satellite: the hub
# encrypts.
# A thin wrapper of sheaf-role (in the sheaf package since v59, /usr/sbin/sheaf-role), which does the work and also
# the way back:
#   sheaf-role satellite N    the same as this script
#   sheaf-role box            back to a box of its own: LAN 192.168.1.1/24, DHCP on, satellite firewall zone removed
#   sheaf-role                the current role;  sheaf-role -n ...  only print what would change
# On a box with an older package, run sheaf-role from the repository the same way:
#   ssh ... root@<box> 'sh -s satellite 1' < openwrt/service/sheaf-role
# The change is applied detached at the end: the box drops off its old address.
case "$N" in 1|2) ;; *) echo "N=1 or N=2" >&2; exit 2 ;; esac
r=$(command -v sheaf-role || echo /usr/sbin/sheaf-role)
[ -x "$r" ] || { echo "sheaf-role missing (sheaf package older than v59): ssh ... root@<box> 'sh -s satellite $N' < openwrt/service/sheaf-role" >&2; exit 1; }
exec "$r" satellite "$N"
