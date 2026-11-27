#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# sheaf over real LTE without and with IPsec (IKEv2 to the server, ESP aes256-sha256 transport for sheaf's UDP, EIP93),
# in one modem session. The computer on the LAN cable drives the transfers: READY1 -> /tmp/stop1, READY2 -> /tmp/stop2.
# Needs /etc/swanctl/conf.d/sheaf-secret.conf (PSK) on the board and the server side of box 1 (ids o2/vf.box1.sheaf) on the server.
# Usage on the board: sh /tmp/sheaf-lte-ipsec.sh ; on the computer: nc 192.168.1.1 5202 | tcpbench -stdin, tcpbench -gen 30 | nc ...
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

read IP0 GW0 < /tmp/lease.wwan0; read IP1 GW1 < /tmp/lease.wwan1
# source routing: whatever leaves with a modem's address leaves through that modem (IKE, ESP-in-UDP, sheaf)
ip rule add from $IP0 table 100; ip route replace $GW0 dev wwan0 table 100; ip route replace default via $GW0 dev wwan0 table 100
ip rule add from $IP1 table 101; ip route replace $GW1 dev wwan1 table 101; ip route replace default via $GW1 dev wwan1 table 101
run_sheaf() { /tmp/sheaf -reliable -key /etc/sheaf/sheaf.key -tun shf0 -frame ${FRAME:-1328} -trace /tmp/trace-$1.csv \
  -links "@wwan0:7001>$SRV:47001,@wwan1:7002>$SRV:47002" \
  -proxy "192.168.1.1:5201>127.0.0.1:5201;192.168.1.1:5202>127.0.0.1:5202" > /tmp/sheaf-$1.log 2>&1 &
  SH_PID=$!; sleep 3; }
phase() { # name: READY, wait for the computer's /tmp/stopN, CPU log
  rm -f /tmp/stop$1
  top -b -d 1 -n 120 | grep -E '^CPU:|sheaf' | grep -v grep > /tmp/top-$1.log &
  TP=$!
  echo READY$1
  i=0; while [ ! -f /tmp/stop$1 ] && [ $i -lt 150 ]; do sleep 1; i=$((i+1)); done
  kill $TP 2>/dev/null; kill $SH_PID; sleep 1
}
run_sheaf 1; phase 1
mkdir -p /var/swanctl
cat > /var/swanctl/swanctl.conf <<C
connections {
    sheaf-o2 {
        version = 2
        local_addrs = $IP0
        remote_addrs = $SRV
        proposals = aes256-sha384-ecp384
        encap = yes
        dpd_delay = 30s
        local { auth = psk
                id = o2.box1.sheaf }
        remote { auth = psk
                 id = server.sheaf }
        children { sheaf-o2 { mode = transport
                               local_ts = dynamic[udp/7001]
                               remote_ts = dynamic[udp/47001]
                               esp_proposals = ${ESP:-aes256-sha256-ecp384} } }
    }
    sheaf-vf {
        version = 2
        local_addrs = $IP1
        remote_addrs = $SRV
        proposals = aes256-sha384-ecp384
        encap = yes
        dpd_delay = 30s
        local { auth = psk
                id = vf.box1.sheaf }
        remote { auth = psk
                 id = server.sheaf }
        children { sheaf-vf { mode = transport
                               local_ts = dynamic[udp/7002]
                               remote_ts = dynamic[udp/47002]
                               esp_proposals = ${ESP:-aes256-sha256-ecp384} } }
    }
}
C
/usr/lib/ipsec/charon > /tmp/charon.log 2>&1 &
CH=$!; sleep 2
swanctl --load-all 2>&1 | tail -2
swanctl --initiate --child sheaf-o2 --timeout 15 2>&1 | tail -1
swanctl --initiate --child sheaf-vf --timeout 15 2>&1 | tail -1
swanctl --list-sas 2>&1 | grep -E "ESTABLISHED|INSTALLED|in  |out " | head -8
ip xfrm state | grep -E "^src|encap" | head -6 # no keys in the log
run_sheaf 2; phase 2
echo "== ESP counters:"; ip -s xfrm state | grep -E "^src|bytes" | grep -B1 bytes | head -8
grep -E "dead|back|restart" /tmp/sheaf-1.log /tmp/sheaf-2.log | head -10
swanctl --terminate --ike sheaf-o2 >/dev/null 2>&1; swanctl --terminate --ike sheaf-vf >/dev/null 2>&1
kill $CH; sleep 1; ip xfrm state flush; ip xfrm policy flush; rm -f /var/swanctl/swanctl.conf
ip rule del from $IP0 table 100; ip rule del from $IP1 table 101; ip route flush table 100; ip route flush table 101
set -- $B0; A0=$(bytes wwan0); set -- $B0 $A0; echo "O2       data used: down $((($3-$1)/1000000)) MB, up $((($4-$2)/1000000)) MB"
set -- $B1; A1=$(bytes wwan1); set -- $B1 $A1; echo "Vodafone data used: down $((($3-$1)/1000000)) MB, up $((($4-$2)/1000000)) MB"
down 1-2 wwan0; down 1-1 wwan1
echo DONE
