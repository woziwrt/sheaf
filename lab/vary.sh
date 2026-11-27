#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Change the 3 lab links over time like real LTE (cell change, outage, congestion). 60 s scenario.
set_link() { # link rate delay jitter loss – same on both ends
  local i=$1 r=$2 d=$3 j=$4 l=$5
  ip netns exec A tc qdisc change dev a$i root netem rate $r delay $d $j 25% distribution normal loss $l limit 1000
  ip netns exec B tc qdisc change dev b$i root netem rate $r delay $d $j 25% distribution normal loss $l limit 1000
}
norm() { set_link 1 20mbit 40ms 10ms 0%; set_link 2 10mbit 70ms 20ms 0%; set_link 3 5mbit 120ms 40ms 0%; }
log() { echo "$(date +%T) $*"; }
norm; log "0 s normal"; sleep 10
set_link 1 8mbit 150ms 30ms 1%; log "10 s L1 worse cell (150 ms, 8 Mbit, 1 %)"; sleep 10
set_link 2 10mbit 70ms 20ms 100%; log "20 s L2 signal lost"; sleep 10
set_link 1 20mbit 40ms 10ms 0%; set_link 2 10mbit 70ms 20ms 0%; set_link 3 2mbit 300ms 60ms 0%; log "30 s L1+L2 back, L3 congested (300 ms, 2 Mbit)"; sleep 10
norm; log "40 s all normal"; sleep 10
set_link 1 20mbit 40ms 10ms 100%; log "50 s L1 outage"; sleep 10
norm; log "60 s end, normal"
