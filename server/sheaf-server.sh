#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 Petr Wozniak
# sheaf far end on the public test server: permanent services, test helpers.
#   install: binaries in /opt/sheaf, key /etc/sheaf/box1.key, units sheaf + sheaf-bench (enabled);
#   firewall: EXTRA_UDP 47001:47002 + EXTRA_TCP 47003:47004 in the server firewall.
#
#   sh sheaf-server.sh status            services, sockets, last log lines
#   sh sheaf-server.sh raw-on [MB]       raw per-modem TCP endpoints 47003 (sink) + 47004 (source, MB default 10), stop by themselves in 30 min
#   sh sheaf-server.sh raw-off
#   sh sheaf-server.sh trace-on [file]   restart sheaf with a 100 ms CSV trace (default /run/sheaf-trace.csv)
#   sh sheaf-server.sh trace-off
#   sh sheaf-server.sh frame N           proxy frame size (1400; 1328 when the board runs sheaf over IPsec)
#   sh sheaf-server.sh src MB            size of the download the sheaf test pulls (bench source on 5202; default 50)
#   sh sheaf-server.sh logs [N]          sheaf + bench journal, last N lines (default 40)
D=/opt/sheaf
case "$1" in
status)
	systemctl --no-pager -n0 status sheaf sheaf-bench sheaf-rawsink sheaf-rawsrc 2>/dev/null | grep -E '●|Active:'
	ss -ulpn | grep -E ':4700[12] '; ss -tlpn | grep -E ':4700[34] |:520[12] '
	journalctl -u sheaf -n 3 --no-pager -o cat
	;;
raw-on)
	systemctl stop sheaf-rawsink sheaf-rawsrc 2>/dev/null
	systemctl reset-failed sheaf-rawsink sheaf-rawsrc 2>/dev/null # after RuntimeMaxSec they stay failed and block the names
	systemd-run -q --unit=sheaf-rawsink -p RuntimeMaxSec=1800 $D/tcpbench -listen 0.0.0.0:47003
	systemd-run -q --unit=sheaf-rawsrc -p RuntimeMaxSec=1800 $D/tcpbench -listen 0.0.0.0:47004 -send ${2:-10}
	sleep 1; ss -tlpn | grep -E ':4700[34] '
	;;
raw-off)
	systemctl stop sheaf-rawsink sheaf-rawsrc 2>/dev/null
	systemctl reset-failed sheaf-rawsink sheaf-rawsrc 2>/dev/null # after RuntimeMaxSec they stay failed and block the names
	journalctl -u sheaf-rawsink -u sheaf-rawsrc -n 10 --no-pager -o cat
	;;
trace-on)
	grep -v '^TRACE=' $D/env 2>/dev/null > $D/env.new; echo "TRACE=${2:-/run/sheaf-trace.csv}" >> $D/env.new; mv $D/env.new $D/env; systemctl restart sheaf; sleep 1; journalctl -u sheaf -n 2 --no-pager -o cat
	;;
trace-off)
	grep -v '^TRACE=' $D/env 2>/dev/null > $D/env.new; mv $D/env.new $D/env; systemctl restart sheaf
	;;
frame)
	grep -v '^FRAME=' $D/env 2>/dev/null > $D/env.new; echo "FRAME=${2:-1400}" >> $D/env.new; mv $D/env.new $D/env
	systemctl restart sheaf; sleep 1; journalctl -u sheaf -n 2 --no-pager -o cat
	;;
src)
	echo "SRC_MB=${2:-50}" > $D/bench.env; systemctl restart sheaf-bench; sleep 1; pgrep -a tcpbench
	;;
logs)
	journalctl -u sheaf -u sheaf-bench -n "${2:-40}" --no-pager -o short-iso
	;;
*)
	sed -n '4,16p' "$0"
	;;
esac
