#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Link profile of ONE modem (thesis method): ramp a paced UDP rate up and down with linkprof, per step rate/loss/RTT.
# On the server first: systemctl stop sheaf; linkprof -listen :47002. Usage: sh /tmp/lte-profile.sh [vf|o2] [up_max] [down_max]
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
WHO=${1:-vf}; UPMAX=${2:-50}; DNMAX=${3:-120}
if [ "$WHO" = o2 ]; then U=1-2; IF=wwan0; else U=1-1; IF=wwan1; fi
up $U $IF 10 || exit 1
B0=$(bytes $IF)
echo "== $WHO up (0..$UPMAX Mbit/s):"
/tmp/linkprof -connect $SRV:47002 -dev $IF -dir up -start 2 -step 2 -steps $((UPMAX/2)) -ms 1000
sleep 2
echo "== $WHO down (0..$DNMAX Mbit/s):"
/tmp/linkprof -connect $SRV:47002 -dev $IF -dir down -start 4 -step 4 -steps $((DNMAX/4)) -ms 1000
set -- $B0; A0=$(bytes $IF); set -- $B0 $A0; echo "$WHO data used: down $((($3-$1)/1000000)) MB, up $((($4-$2)/1000000)) MB"
down $U $IF
echo DONE
