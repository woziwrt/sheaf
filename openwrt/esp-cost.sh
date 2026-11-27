#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Cost of ESP (aes256-sha256, transport) on the board: two network namespaces over a veth pair, TCP 100 MB a -> b,
# plain vs ESP. Both encryption and decryption run on this board (= both directions' crypto at once).
# Throw-away manual keys (random, lab only - the real link uses IKEv2 with ECP-384).
# EIP93=0: unload the hardware driver for the ESP run (its authenc path rejects the ESP state with EINVAL).
cleanup() { ip netns del a 2>/dev/null; ip netns del b 2>/dev/null; }
cleanup
ip netns add a; ip netns add b
ip link add va netns a type veth peer name vb netns b
ip -n a addr add 10.99.1.1/24 dev va; ip -n a link set va up; ip -n a link set lo up
ip -n b addr add 10.99.1.2/24 dev vb; ip -n b link set vb up; ip -n b link set lo up
ip netns exec b /tmp/tcpbench -listen 10.99.1.2:5300 > /tmp/esp-sink.log 2>&1 &
SK=$!
sleep 1
run() {
  top -b -d 1 -n 6 | grep -E '^CPU:' > /tmp/esp-top.log &
  r=$(ip netns exec a /tmp/tcpbench -connect 10.99.1.2:5300 -mb ${MB:-100} 2>&1 | tail -1)
  sleep 1
  echo "$1: $r | CPU idle: $(awk '{print $8}' /tmp/esp-top.log | sed -n '2,6p' | tr '\n' ' ')"
}
run "without IPsec"
[ "${EIP93:-0}" = 0 ] && rmmod crypto_hw_eip93 2>/dev/null && echo "EIP93 unloaded for the ESP run"
rnd() { head -c $1 /dev/urandom | hexdump -v -e '/1 "%02x"'; }
EK=0x$(rnd 32)
AK=0x$(rnd 32)
# AUTH=null: ESP encryption only (the EIP93 authenc path is broken; sheaf authenticates its datagrams itself)
if [ "${AUTH:-sha256}" = null ]; then AUTHSPEC="auth digest_null \"\""; else AUTHSPEC="auth-trunc hmac(sha256) $AK 128"; fi
for ns in a b; do
  ip -n $ns xfrm state add src 10.99.1.1 dst 10.99.1.2 proto esp spi 0x1001 reqid 1 mode transport enc 'cbc(aes)' $EK $AUTHSPEC || echo "state1 $ns failed"
  ip -n $ns xfrm state add src 10.99.1.2 dst 10.99.1.1 proto esp spi 0x1002 reqid 1 mode transport enc 'cbc(aes)' $EK $AUTHSPEC || echo "state2 $ns failed"
done
ip -n a xfrm policy add src 10.99.1.1 dst 10.99.1.2 dir out tmpl proto esp reqid 1 mode transport
ip -n a xfrm policy add src 10.99.1.2 dst 10.99.1.1 dir in tmpl proto esp reqid 1 mode transport
ip -n b xfrm policy add src 10.99.1.2 dst 10.99.1.1 dir out tmpl proto esp reqid 1 mode transport
ip -n b xfrm policy add src 10.99.1.1 dst 10.99.1.2 dir in tmpl proto esp reqid 1 mode transport
run "ESP aes256 + ${AUTH:-sha256}, EIP93=${EIP93:-0}"
ip -n b -s xfrm state | grep -E "spi|bytes" | head -4
grep -A1 -E "^name *: (authenc\\(hmac\\(sha256\\),cbc\\(aes\\))|authenc\\(digest_null,cbc\\(aes\\)\\)|cbc\\(aes\\))$" /proc/crypto | grep driver | sort -u
kill $SK
cleanup
lsmod | grep -q crypto_hw_eip93 || modprobe crypto_hw_eip93
cat /tmp/esp-sink.log
echo DONE
