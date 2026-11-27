#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# First real-LTE test of sheaf: both modems up, short upload + download through the bond, modems down.
# Usage on the board: [SRV=ip] sh /tmp/sheaf-lte.sh [upload_MB] [DL 0/1]   (download size is set on the server: tcpbench -send)
export GOMAXPROCS=${GOMAXPROCS:-2} # MT7621 = 2 cores x 2 threads: more Go Ps only spin (cable: G=2 fastest, least CPU)
SRV=${SRV:-203.0.113.10}
UP_MB=${1:-15}
DL=${2:-1}   # 0 = skip the download test
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

up 1-2 wwan0 10 || exit 1
up 1-1 wwan1 20 || { down 1-2 wwan0; exit 1; }
B0=$(bytes wwan0); B1=$(bytes wwan1)
echo "ping $SRV (O2 / Vodafone):"
ping -c 3 -W 2 -I wwan0 $SRV 2>&1 | tail -1; ping -c 3 -W 2 -I wwan1 $SRV 2>&1 | tail -1

/tmp/sheaf -reliable -key /etc/sheaf/sheaf.key -tun shf0 -trace /tmp/trace.csv \
  -links "@wwan0:7001>$SRV:47001,@wwan1:7002>$SRV:47002" \
  -proxy "127.0.0.1:5201>127.0.0.1:5201;127.0.0.1:5202>127.0.0.1:5202" > /tmp/sheaf.log 2>&1 &
SH_PID=$!
sleep 4
echo "== upload $UP_MB MB (box -> server):"
top -b -d 1 -n 6 | grep -E '^CPU:|sheaf|tcpbench' > /tmp/top.log &
/tmp/tcpbench -connect 127.0.0.1:5201 -mb $UP_MB 2>&1 | tail -2
sleep 2
grep -E '^CPU:' /tmp/top.log | sed -n '2,6p'; grep -E 'sheaf|tcpbench' /tmp/top.log | grep -v grep | tail -4
if [ "$DL" = 1 ]; then
  echo "== download (server -> box):"
  top -b -d 1 -n 10 | grep -E '^CPU:|sheaf' > /tmp/top-dl.log &
  /tmp/tcpbench -connect 127.0.0.1:5202 -recv 2>&1 | tail -2
  sleep 1; grep -E '^CPU:' /tmp/top-dl.log | sed -n '2,8p'
fi
sleep 1
kill $SH_PID; sleep 1
cat /tmp/sheaf.log | tail -15

set -- $B0; A0=$(bytes wwan0); set -- $B0 $A0; echo "O2       data used: down $((($3-$1)/1000000)) MB, up $((($4-$2)/1000000)) MB"
set -- $B1; A1=$(bytes wwan1); set -- $B1 $A1; echo "Vodafone data used: down $((($3-$1)/1000000)) MB, up $((($4-$2)/1000000)) MB"
down 1-2 wwan0; down 1-1 wwan1
echo DONE
