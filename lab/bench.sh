#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Repeated measurements: v7, v10 (UDP sheaf) and proxy over MPTCP, static 30 s and varying 60 s, N runs each.
# Output: /var/log/sim/bench.csv (variant,mode,run,Mbit received) + summary.
# C1 (HLT) idle state off for the whole measurement: the lab VM oversleeps timers by 100-600 ms when a vCPU
# halts (netem and sheaf freeze, false dead links). The previous state is restored on any exit. NOHLT=0 keeps C1.
idle_hlt() { for f in /sys/devices/system/cpu/cpu*/cpuidle/state1/disable; do echo $1 > $f; done; } # 1 = no HLT
c1_off() { [ "${NOHLT:-1}" = 1 ] || return 0; c1_was=$(cat /sys/devices/system/cpu/cpu0/cpuidle/state1/disable); idle_hlt 1
           trap "idle_hlt $c1_was; $1" EXIT; trap 'exit 130' INT TERM HUP; }
c1_off
N=${1:-5}; OUT=${OUT:-/var/log/sim/bench.csv}; echo "variant,mode,run,mbit" > $OUT
L="10.1.0.1:7000>10.1.0.2:7000,10.2.0.1:7000>10.2.0.2:7000,10.3.0.1:7000>10.3.0.2:7000"
R="10.1.0.2:7000>10.1.0.1:7000,10.2.0.2:7000>10.2.0.1:7000,10.3.0.2:7000>10.3.0.1:7000"
stop_all() { pkill -x 'sheaf(-.*)?'; killall -q iperf3; sleep 1; ip netns exec A ip link del shf0 2>/dev/null; ip netns exec B ip link del shf0 2>/dev/null; }
start_udp() { # $1 = binary
  for ns in A B; do ip netns exec $ns sysctl -q -w net.ipv4.tcp_congestion_control=cubic; done  # end hosts keep the default
  cp "$1" /tmp/sheaf; chmod +x /tmp/sheaf
  ip netns exec A /tmp/sheaf -tun shf0 -links "$L" > /var/log/sim/sheaf-A.log 2>&1 &
  ip netns exec B /tmp/sheaf -tun shf0 -links "$R" > /var/log/sim/sheaf-B.log 2>&1 &
  sleep 1
  ip netns exec A ip addr replace 10.50.0.1/30 dev shf0; ip netns exec A ip link set shf0 mtu 1400 up
  ip netns exec B ip addr replace 10.50.0.2/30 dev shf0; ip netns exec B ip link set shf0 mtu 1400 up
  sleep 3
}
start_proxy() { # $1 = congestion control of the proxy's MPTCP subflows (cubic|bbr)
  modprobe tcp_bbr 2>/dev/null
  for ns in A B; do ip netns exec $ns sysctl -q -w net.ipv4.tcp_congestion_control=${1:-cubic}
    if [ "$1" = bbr ]; then ip netns exec $ns sysctl -q -w net.mptcp.stale_loss_cnt=2; else ip netns exec $ns sysctl -q -w net.mptcp.stale_loss_cnt=4; fi
    ip netns exec $ns sysctl -q -w net.mptcp.enabled=1; ip netns exec $ns ip mptcp limits set subflow 3 add_addr_accepted 3; done
  ip netns exec B ip mptcp endpoint flush; ip netns exec B ip mptcp endpoint add 10.2.0.2 dev b2 signal; ip netns exec B ip mptcp endpoint add 10.3.0.2 dev b3 signal
  ip netns exec B /tmp/sheaf-proxy -server :7200 > /var/log/sim/px-B.log 2>&1 &
  sleep 1
  ip netns exec A /tmp/sheaf-proxy -client 127.0.0.1:5201 -via 10.1.0.2:7200 -to 127.0.0.1:5201 > /var/log/sim/px-A.log 2>&1 &
  sleep 2
}
start_pxsheaf() { # proxy over the sheaf tunnel (plain TCP between the proxies, no MPTCP): $1 = sheaf binary
  start_udp "$1"
  for ns in A B; do ip netns exec $ns sysctl -q -w net.mptcp.enabled=0; ip netns exec $ns ip mptcp endpoint flush 2>/dev/null; done
  ip netns exec B /tmp/sheaf-proxy -server :7200 > /var/log/sim/px-B.log 2>&1 &
  sleep 1
  ip netns exec A /tmp/sheaf-proxy -client 127.0.0.1:5201 -via 10.50.0.2:7200 -to 127.0.0.1:5201 > /var/log/sim/px-A.log 2>&1 &
  sleep 2
}
static_run() { # target
  ip netns exec B iperf3 -s -1 -D -B $1 >/dev/null 2>&1; sleep 1
  ip netns exec A iperf3 -c $1 -t 30 -f m 2>/dev/null | awk '/receiver/{print $7}'
}
scen() { # start the time-varying scenario of $mode in the background (run $i = drive seed)
  if [ $mode = drive ]; then bash /tmp/drive.sh 60 $((40 + i)) >/dev/null 2>&1 & else bash /tmp/vary.sh >/dev/null 2>&1 & fi
}
scen_wait() { while pgrep -f "bash /tmp/(vary|drive).sh" >/dev/null; do sleep 1; done; }
vary_run() { # target (scenario runs 60 s alongside; wait for it by name, not by job – this runs inside $( ))
  ip netns exec B iperf3 -s -1 -D -B $1 >/dev/null 2>&1; sleep 1
  scen
  ip netns exec A iperf3 -c $1 -t 60 -f m 2>/dev/null | awk '/receiver/{print $7}'
  scen_wait
}
for i in $(seq 1 $N); do   # run by run, variants alternate (a slow drift of the VM hits all of them alike)
  for v in ${VARIANTS:-v7 v10 proxy}; do
    for mode in ${MODES:-static vary}; do
      stop_all
      case $v in
        proxy|proxybbr) start_proxy $([ $v = proxybbr ] && echo bbr || echo cubic); T=127.0.0.1; ip netns exec B iperf3 -s -D -B 127.0.0.1 >/dev/null 2>&1 ;;
        sp-*) start_udp /tmp/sheaf-${v#sp-}; T=127.0.0.1; ip netns exec B iperf3 -s -D -B 127.0.0.1 >/dev/null 2>&1 ;;
        px-*) start_pxsheaf /tmp/sheaf-${v#px-}; T=127.0.0.1; ip netns exec B iperf3 -s -D -B 127.0.0.1 >/dev/null 2>&1 ;;
        *) start_udp /tmp/sheaf-$v; T=10.50.0.2 ;;
      esac
      if [ $T = 127.0.0.1 ]; then
        if [ $mode = static ]; then r=$(ip netns exec A iperf3 -c 127.0.0.1 -t 30 -f m 2>/dev/null | awk '/receiver/{print $7}')
        else scen; r=$(ip netns exec A iperf3 -c 127.0.0.1 -t 60 -f m 2>/dev/null | awk '/receiver/{print $7}'); scen_wait; fi
      else
        if [ $mode = static ]; then r=$(static_run $T); else r=$(vary_run $T); fi
      fi
      echo "$v,$mode,$i,${r:-0}" >> $OUT; echo "$(date +%T) $v $mode $i: ${r:-0}"
    done
  done
done
stop_all
echo "== summary (mean / std dev / min-max, Mbit/s)"
awk -F, 'NR>1{k=$1" "$2; n[k]++; s[k]+=$4; q[k]+=$4*$4; if(!(k in mn)||$4<mn[k])mn[k]=$4; if($4>mx[k])mx[k]=$4}
 END{for(k in n){m=s[k]/n[k]; sd=sqrt(q[k]/n[k]-m*m); printf "%-14s %5.1f  ±%4.1f  (%.1f–%.1f)  n=%d\n",k,m,sd,mn[k],mx[k],n[k]}}' $OUT | sort
