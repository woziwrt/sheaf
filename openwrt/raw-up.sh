#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Raw TCP per modem, no sheaf: upload (TCP 47003) and optionally download (TCP 47004; on the server run "sheaf-server.sh raw-on" first).
# Usage on the board: [SRV=ip] sh /tmp/raw-up.sh [MB] [DL 0/1]
SRV=${SRV:-203.0.113.10}
at() { sms_tool -d /dev/$1 at "$2" > /tmp/at.$1 2>&1 & p=$!; sleep 4; kill $p 2>/dev/null; }
tty() { ls /sys/bus/usb/devices/$1:2.4/ | grep -o 'ttyUSB[0-9]*' | head -1; }
cat > /tmp/u.sh <<'U'
#!/bin/sh
[ "$1" = bound ] && { ip addr flush dev $interface; ip addr add $ip/32 dev $interface; echo "$ip $router" > /tmp/lease.$interface; }
exit 0
U
chmod +x /tmp/u.sh
up() { # usb-path ifname metric
  # unsolicited modem messages can swallow the dial command: check the state and retry
  for try in 1 2 3; do
    at $(tty $1) 'AT^NDISDUP=1,1,"internet"'; sleep 2
    at $(tty $1) 'AT^NDISSTATQRY?'; grep -q 'NDISSTATQRY: 1,' /tmp/at.$(tty $1) && break
    echo "$2: dial attempt $try failed, retrying"
  done
  ip link set $2 up; rm -f /tmp/lease.$2
  udhcpc -i $2 -q -n -t 6 -s /tmp/u.sh >/dev/null 2>&1
  read IP GW < /tmp/lease.$2 || { echo "$2: no address"; return 1; }
  # main table: sockets bound to the device (SO_BINDTODEVICE) pick the default route of their own device
  ip route replace $GW dev $2; ip route replace default via $GW dev $2 metric $3
  echo "$2 ($1): IP $IP GW $GW"
}
down() { at $(tty $1) 'AT^NDISDUP=1,0'; ip link set $2 down; }
bytes() { echo "$(cat /sys/class/net/$1/statistics/rx_bytes) $(cat /sys/class/net/$1/statistics/tx_bytes)"; }
t() { awk '{print $1}' /proc/uptime; }
# Only one modem's default route at a time (a server behind NAT needs a temporary dst-nat rule for TCP 47003).
MB=${1:-10}
DL=${2:-0}
up 1-2 wwan0 10 || exit 1
up 1-1 wwan1 20 || { down 1-2 wwan0; exit 1; }
G0=$(cut -d' ' -f2 /tmp/lease.wwan0); G1=$(cut -d' ' -f2 /tmp/lease.wwan1)
B0=$(bytes wwan0); B1=$(bytes wwan1)
ip route del default via $G1 dev wwan1
echo "== O2 alone, TCP upload $MB MB:"; /tmp/tcpbench -connect $SRV:47003 -mb $MB 2>&1 | tail -1
[ "$DL" = 1 ] && { echo "== O2 alone, TCP download:"; /tmp/tcpbench -connect $SRV:47004 -recv 2>&1 | tail -1; }
ip route replace default via $G1 dev wwan1 metric 5
echo "== Vodafone alone, TCP upload $MB MB:"; /tmp/tcpbench -connect $SRV:47003 -mb $MB 2>&1 | tail -1
[ "$DL" = 1 ] && { echo "== Vodafone alone, TCP download:"; /tmp/tcpbench -connect $SRV:47004 -recv 2>&1 | tail -1; }
set -- $B0; A0=$(bytes wwan0); set -- $B0 $A0; echo "O2       data used: down $((($3-$1)/1000000)) MB, up $((($4-$2)/1000000)) MB"
set -- $B1; A1=$(bytes wwan1); set -- $B1 $A1; echo "Vodafone data used: down $((($3-$1)/1000000)) MB, up $((($4-$2)/1000000)) MB"
down 1-2 wwan0; down 1-1 wwan1
echo DONE
