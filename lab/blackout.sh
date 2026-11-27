#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# All lab links fail at the same time (tunnel, dead zone) – 3 blackouts in 45 s.
set_all() { # loss
  for i in 1 2 3; do for ns in A B; do d=$([ $ns = A ] && echo a$i || echo b$i)
    case $i in 1) r=20mbit; dl=40ms; j=10ms;; 2) r=10mbit; dl=70ms; j=20ms;; 3) r=5mbit; dl=120ms; j=40ms;; esac
    ip netns exec $ns tc qdisc change dev $d root netem rate $r delay $dl $j 25% distribution normal loss $1 limit 1000
  done; done
}
log() { echo "$(date +%T) $*"; }
set_all 0%; log "0 s normal"; sleep 5
set_all 100%; log "5 s ALL links down 3 s"; sleep 3
set_all 0%; sleep 12
set_all 100%; log "20 s ALL links down 2 s"; sleep 2
set_all 0%; sleep 13
set_all 100%; log "35 s ALL links down 4 s"; sleep 4
set_all 0%; log "39 s normal"; sleep 6
