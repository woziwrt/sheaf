#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# Sequence of integrity tests (lab VM), after any running bench.sh finished.
while pgrep -f "bash /tmp/bench.sh" >/dev/null; do sleep 10; done
for t in "v29 B vary 2" "v30 A vary 3" "v30 B vary 3" "v30 A blackout 3" "v30 B blackout 2"; do
  set -- $t
  echo "######## $1 restart=$2 scen=$3 runs=$4"
  BIN=/tmp/sheaf-$1 RESTART=$2 SCEN=$3 MB=40 bash /tmp/integrity.sh $4
done
echo DONE
