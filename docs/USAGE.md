# Using sheaf

How to install and run the box and the server, build sheaf, its packages and the firmware, and measure. For the
protocol see [PROTOCOL.md](PROTOCOL.md), for the mechanisms [HOW-IT-WORKS.md](HOW-IT-WORKS.md), for the measured
results [RESULTS.md](RESULTS.md), and for the overview the [README](../README.md).

This describes sheaf v65 and the packages 65-r1 (v65 = v64 with no behaviour change).

1. [The box](#1-the-box)
2. [The server](#2-the-server)
3. [Building sheaf](#3-building-sheaf)
4. [Building the firmware image](#4-building-the-firmware-image)
5. [Command-line reference](#5-command-line-reference)
6. [Status JSON](#6-status-json)
7. [Trace CSV](#7-trace-csv)
8. [Measuring tools and lab scripts](#8-measuring-tools-and-lab-scripts)
9. [Repository layout](#9-repository-layout)

---

## 1. The box

Hardware: MikroTik RBM33G (MT7621A, 880 MHz, 2 cores × 2 threads), two LTE modules with SIMs of two operators,
OpenWrt 25.12.6. On the test box, USB path `1-2` is modem 1 = `wwan0` = O2 = **L1**, and `1-1` is modem 2 = `wwan1` =
Vodafone = **L2**. The second mini-PCIe slot is connected to USB only after its jumper is removed. Both modems use the
APN `internet`.

**Modems.** sheaf itself does not care what the modem is: it only needs a network interface with an address. Only
`sheaf-run` talks to the modem (dial, check, radio values), and it knows three kinds (`MODEM`, 1.6):

- **`mbim`**, for current modules that speak MBIM (Linux `cdc_mbim`, dialled with `umbim` the way OpenWrt's own
  `mbim.sh` does it). Needs `umbim` and `kmod-usb-net-cdc-mbim`. No radio values on the status page yet.
- **`qmi`**, for current modules that speak QMI (Linux `qmi_wwan`, dialled with `uqmi`), M.2 or mini-PCIe.
- ⚠️ `mbim` and `qmi` are written and reviewed, not yet tested on hardware (November 2026). For a new build they are
  the way to go.
- **`huawei`**: Huawei ME909s-120 (mini-PCIe), AT commands and DHCP. All the measurements in these documents were made
  with it, but it is no longer made or sold. It stays here as the tested example.

The two modems do not have to be the same: the type can be set per slot (`MODEM_wwan1=mbim`). Another kind of modem means one more dial function in `sheaf-run` (see `huawei_dial` and `qmi_dial`); nothing else
changes.

### 1.1 Daily use

| what | how |
|---|---|
| start | power on, wait ~75 s (firmware image) to ~1.5 min; everything starts by itself |
| status | http://192.168.1.1/sheaf.html from the LAN, no login, read only, refreshes every 2 s |
| mode | http://192.168.1.1 → LuCI login (root) → **Services → Sheaf** → mode, primary modem for backup → *Save*; applies at once, the server follows |
| log | `logread -e sheaf` (supervisor) and `logread -e sheaf-core` (sheaf itself) |
| radio samples | `/tmp/radio.csv` (RAM, every 10 s per modem: RAT, RSRP, SINR, RSRQ, cell, PCI, EARFCN); cell changes also go to the log |
| restart the service | `/etc/init.d/sheaf restart` |

The mode file can also be written by hand: `echo "backup 2" > /etc/sheaf/mode` (format `mode [primary link number]`,
modes `speed | smooth | redundant | backup`).

### 1.2 What runs on the box

Everything except the keys comes from two packages, `sheaf` and `luci-app-sheaf`
(`openwrt/package/*/Makefile`):

| file | from the repo | role |
|---|---|---|
| `/etc/init.d/sheaf` | `openwrt/service/sheaf.init` | procd service, enabled at boot, respawns the supervisor; on TERM the supervisor cleans up |
| `/usr/sbin/sheaf-run` | `openwrt/service/sheaf-run` | supervisor loop, every 10 s (below) |
| `/usr/sbin/sheaf-role` | `openwrt/service/sheaf-role` | switches the box between its own box and a satellite of the hub ([1.5](#15-hub-and-satellites)) |
| `/etc/sheaf/sheaf.conf` | `openwrt/service/sheaf.conf` | `SRV` (server address), `FRAME` (1328 with IPsec, 1400 without), `IPSEC` (1/0), `GOMAXPROCS` (2), `EXTRA` (further sheaf flags, [5](#5-command-line-reference); the template sets `-maxq 20 -tungro -auth=half`); `TPROXY` (0/1, the template sets 1: the server instance needs `-tallow public`), `TPORT` (5300), `BLOCK_QUIC` (0/1, template 0): transparent TCP proxy, see step 4; `FLOWTABLE` (1/0, default 1): flowtable shortcut, step 4; `GOMEMLIMIT` (default `96MiB`, Go heap soft limit); role, modem type and IPsec algorithms: [1.5](#15-hub-and-satellites), [1.6](#16-modem-types-and-ipsec-algorithms) |
| `/etc/sheaf/mode` | written by LuCI | `mode [primary]` |
| `/usr/sbin/sheaf` | `sheaf/` built for mipsle | the bonding program |
| `/www/sheaf.html`, `/www/cgi-bin/sheaf-status` | `openwrt/service/www/` | status page and its JSON source (sheaf's status file + radio + IPsec SAs + modem addresses) |
| LuCI app | `openwrt/service/luci/` | menu entry, ACL (may write only `/etc/sheaf/mode`, read the status file), view `sheaf/mode` |
| `/etc/uci-defaults/90-sheaf` | `openwrt/firmware/90-sheaf` | one-time setup (runs once after installation or first boot): firewall zone, DNS, system log ring of at least 512 kB (the default 64 kB held about 10 minutes of log), keeps secrets over sysupgrade, enables the service |
| `/etc/sheaf/sheaf.key` | **not in the repo** | sheaf's shared key, 32 hex characters, mode 600 |
| `/etc/swanctl/conf.d/sheaf-secret.conf` | **not in the repo** | IPsec PSK, mode 600 |

The supervisor `sheaf-run` does the following every 10 s:

1. For each modem: is there a lease, and does the interface still have that address? Once a minute it also asks the
   modem (Huawei: `AT^NDISSTATQRY?`; QMI: `uqmi --get-data-status`; MBIM: its address in `umbim config`). If not, it dials. Huawei:
   `AT^NDISDUP=1,1,"internet"`, up to 3 tries (a modem sometimes swallows the command), then DHCP. QMI: raw IP,
   `uqmi --start-network`, the address and gateway from `--get-current-settings`. MBIM: `umbim` caps, subscriber,
   registration, attach, connect, then the address and gateway from `config`. The address is configured as /32 with
   a route to the gateway. Then it sets up
   policy routing: table 100 (wwan0) or 101 (wwan1), `ip rule from <address>`.
   The minute check redials a Huawei modem only on an explicit `^NDISSTATQRY: 0,`; an empty or garbled answer leaves
   the link alone. A modem that is there but fails to dial is tried less and less often (10 s, 20 s, 40 s … 5 min,
   `/tmp/sheaf-backoff.<if>`), so it does not hold up the loop; a modem missing on USB is tried every round.
2. Records a radio sample per modem.
3. IPsec (`IPSEC=1`): starts `charon` if it is not running, and writes `/var/sheaf/swanctl.conf` with one IKE
   connection per modem that has an address (local address = the modem's, remote = `SRV`, IKE `$IKE_PROP`, default
   `aes256-sha384-ecp384`, PSK, ids `o2.box1.sheaf` / `vf.box1.sheaf` ↔ `server.sheaf`, CHILD in transport mode for
   `udp/7001 ↔ udp/47001` (modem 1) or `udp/7002 ↔ udp/47002` (modem 2), ESP `$ESP_PROP`, default
   `aes256-sha256-ecp384`, `encap = yes`, trap on start, DPD and close).
   When the file changes, it terminates the IKE SAs of only those links whose address changed or
   disappeared (the previous addresses are in `/var/sheaf/swanctl.conf.links`) and reloads; the other link keeps its
   SAs, and the trap policies renegotiate the changed one on the next packet. A failed load is retried every round;
   a running sheaf is left running meanwhile as long as the nft guard (no plaintext sheaf UDP to the server) is in
   place, and stopped only without it. When the key files in `/etc/swanctl/conf.d/` change (names or contents: a PSK
   put back or replaced), it reloads and initiates at once the links that have no IKE SA.
4. Starts sheaf if it is not running:

   ```
   sheaf -reliable -key /etc/sheaf/sheaf.key -tun shf0 -frame $FRAME -status /tmp/sheaf-status.json \
          -modefile /etc/sheaf/mode -links "@wwan0:7001>$SRV:47001,@wwan1:7002>$SRV:47002" \
          -proxy "192.168.1.1:5201>127.0.0.1:5201;192.168.1.1:5202>127.0.0.1:5202"
   ```

   `shf0` is a persistent TUN that `sheaf-run` creates itself (`ip tuntap add dev shf0 mode tun`) and configures
   every round (10.66.0.2/30, MTU 1320, default route via 10.66.0.1, metric 1); sheaf only attaches to it. A restart
   of sheaf (a modem re-enumerated, the watchdog, a service restart) therefore keeps the device, its address,
   the routes and the firewall's NAT state, and the LAN's TCP connections survive it. While no sheaf reads `shf0`,
   the kernel drops what is routed into it, silently. The box's own LTE traffic (IKE, ESP, sheaf's UDP) stays in
   tables 100/101.

   LAN guard: traffic arriving on `br-lan` (`LAN_IF`) is routed by rule 900 (`lookup main suppress_prefixlength 0`:
   main without its default routes) and rule 901 (table 102: default via `shf0`, behind it `blackhole default`). LAN
   traffic never reaches a modem's default route in main, even if `shf0` is gone.

   Watchdog: sheaf writes its status file every second. If the file is 15 s old, `sheaf-run` restarts sheaf: TERM,
   and after 5 s KILL (a stopped or hung process ignores TERM and keeps its sockets, and the next one would then fail
   with `address already in use` in a loop). The status page shows a status older than 5 s as "not responding".

   **Flowtable** (`FLOWTABLE=1`, the default). `ft_sync` keeps an nft table of its own, `inet sheaf_ft`, with a
   software flowtable over the LAN bridge's ports and `shf0`: once a forwarded LAN ↔ tunnel connection is established
   (its first packets went through fw4's forward chain as usual), its packets skip the rest of the netfilter and
   routing path. On the RBM33G that path is part of every write into the tunnel: one TCP download through the tunnel
   went from 20.7 to 31.7 Mbit/s (+53 %, means of 4 + 4 interleaved runs,
   [RESULTS.md](RESULTS.md#single-rbm33g-box)). Proxied TCP is local, not
   forwarded, and is not affected. fw4's own offloading (`flow_offloading=1` in the firewall defaults) covers
   `br-lan` and `wan` but not `shf0`; when it is on, `ft_sync` removes its table, so the same ports are never in two
   flowtables. The satellite role has no flowtable.

   **Transparent TCP proxy** (`TPROXY=1`; `sheaf-run` defaults to 0 when the variable is unset, the `sheaf.conf` template sets 1). sheaf also gets `-transparent 0.0.0.0:$TPORT`, and every round
   `tproxy_sync` keeps the nft table `inet sheaf_tproxy` in step with it: while sheaf runs and listens on `TPORT`,
   a new TCP connection (SYN) arriving on `LAN_IF` to a public address (not the box itself, not `SRV`, not
   private/CGNAT/link-local/multicast ranges) is redirected to the listener and carried as a proxy stream; the server
   dials the destination from its own address (it needs `-tallow public`, [2.1](#21-sheaf)). UDP, ICMP and TCP to
   private addresses keep using `shf0`. When sheaf is not running (or not listening) the table is removed, so new
   connections go into `shf0` as without the option. Connections already in `shf0` stay there (only a SYN is
   redirected). Proxied connections end with a restart of sheaf; tunnel connections survive it. The table is ours,
   so fw4 reloads leave it alone. The redirected SYN is accepted by the `lan` zone's input (ACCEPT by default). A
   connection to the listener itself (not redirected) is closed at once. Changing `TPROXY` needs
   `/etc/init.d/sheaf restart`. Log: `tproxy: ...`; counters in the status JSON (`tproxy`).

   `BLOCK_QUIC=1` (with `TPROXY=1`, default 0) also rejects UDP 443 from the LAN (ICMP port unreachable, at once).
   Browsers then fall back to TCP (HTTP/2) and so to the proxy, instead of QUIC through the tunnel (~20 Mbit/s on the
   RBM33G). The price: no HTTP/3 (0-RTT, connection migration), a few services that only offer QUIC fail, and Chrome
   keeps QUIC off for that network for a while even after the option is switched off. The template leaves it at 0;
   box 1 runs it at 1.

5. Recovery ladder, per link (IKE id): a link is *bad* when its modem has an address but its IKE SA is not
   `ESTABLISHED` (one `swanctl --list-sas --noblock` per round), or sheaf has it dead (`/tmp/sheaf-status.json`:
   no feedback over it for 12 s, or `dead` with no feedback for 6 s; sheaf's link numbers are mapped to the ids
   through `/tmp/sheaf-links.ids`, written when sheaf starts). Counted on uptime from the first bad round
   (`/tmp/sheaf-ladder.<id>`), one step at a time, each logged as `ladder: ...`:

   | after | step |
   |---|---|
   | 30 s | no IKE SA: end what is half open, reload creds + connections, `swanctl --initiate --child sheaf-<id>` (detached) |
   | 60 s | the same, now also with an `ESTABLISHED` SA that carries nothing (the server lost its side without a DELETE) |
   | 3 min | hang the modem up (Huawei `AT^NDISDUP=1,0`, QMI/MBIM: end the session); the dial loop dials it again |
   | 6 min | reset the modem (Huawei `AT^RESET`; QMI/MBIM: USB `authorized` 0/1) |
   | 15 min | USB `authorized` 0/1; the ladder ends, a new one starts if the link is still bad |

   When no link is good, the cause is more likely the server, the key or the network than a modem: the modem steps
   then come at 10 / 20 / 30 min. At least 30 s between two steps of a link, 2 min after a modem step. The link
   that is good again ends its ladder (`ladder: <id> alive again after N s`). Only the bad link's own SA and modem
   are touched. A modem without an address is left to the dial loop (no ladder starts for it); sheaf's status
   counts only while sheaf runs, its file is fresh and it has been up 30 s.

On TERM it stops sheaf, terminates the IKE SAs, stops charon, flushes the xfrm states and policies, hangs up both
modems and removes the modems' routing rules. `shf0`, its routes and the LAN guard stay, so the LAN stays cut off
from the modems while the service is stopped. The satellite role removes them (`ip link del shf0` by hand otherwise).

The firewall (from `90-sheaf`, or `openwrt/service/uci-sheaf.sh` on an existing install) adds zone `sheaf` on
`shf0` (input REJECT, forward REJECT, masquerade, MSS clamping) and the forwarding `lan → sheaf`. dnsmasq uses public
resolvers through the tunnel.

### 1.3 Installing or upgrading sheaf on a box (packages)

sheaf is installed as two OpenWrt packages (apk, OpenWrt 25.12, `ramips/mt7621`):

| package | contents | depends on |
|---|---|---|
| `sheaf-<ver>-r1.apk` | the sheaf binary, `sheaf-run`, init script, `sheaf.conf` (a conffile: kept on upgrade, see below), the one-time setup script, status page and CGI | `kmod-tun`, `ip-full`, `kmod-ipsec4`, strongSwan (charon, swanctl, kernel-netlink, random, socket-default, vici), and per target: RBM33G `wolfssl`, `sms-tool`, `wwan`, the Huawei USB modem drivers; BPI-R4 hub `strongswan-mod-openssl`, `uqmi`, `qmi_wwan` (`build-apk.sh`, `TARGET=r4`) |
| `luci-app-sheaf-<ver>-r1.apk` | the LuCI page Services → Sheaf (menu, ACL, view); reloads rpcd after installation | `sheaf`, `luci-base`, `rpcd-mod-file` |

The packages are **unsigned** on purpose (no private package feed). Install them either through LuCI → System →
Software → *Upload package*, or from a shell:

```sh
scp sheaf-<ver>-r<rel>.apk luci-app-sheaf-<ver>-r<rel>.apk root@192.168.1.1:/tmp/
apk add --allow-untrusted /tmp/sheaf-<ver>-r<rel>.apk /tmp/luci-app-sheaf-<ver>-r<rel>.apk
```

On a box that has never had sheaf, put the keys in place once and start the service:

```sh
# /etc/sheaf/sheaf.key (32 hex characters) and /etc/swanctl/conf.d/sheaf-secret.conf (IPsec PSK), both mode 600
chmod 600 /etc/sheaf/sheaf.key /etc/swanctl/conf.d/sheaf-secret.conf
/etc/init.d/sheaf restart
```

**Making the two keys.** Make them once, on your computer, and put the same files on the box and on the server:

```sh
openssl rand -hex 16 > sheaf.key                  # sheaf's key: 32 hex characters
psk=$(openssl rand -base64 33)                     # IPsec PSK
cat > sheaf-secret.conf <<EOF
secrets { ike-sheaf { id-1 = server.sheaf  id-2 = o2.box1.sheaf  id-3 = vf.box1.sheaf  secret = "$psk" } }
EOF
```

The ids are `<ID1>.<BOX>.sheaf` and `<ID2>.<BOX>.sheaf` from `/etc/sheaf/sheaf.conf` (defaults `o2`, `vf`,
`box1`), and the server's id `server.sheaf`. If you change `BOX` or the link ids, change them here and on the server.
On the box: `/etc/sheaf/sheaf.key` and `/etc/swanctl/conf.d/sheaf-secret.conf`; on the server: the same key as
`/etc/sheaf/box1.key` (one key file per server instance, [2.1](#21-sheaf)) and the same `secrets` block in its swanctl
configuration (2.3). Both files mode 600, never in a repository. When the key files in `/etc/swanctl/conf.d/` change,
`sheaf-run` reloads IPsec by itself.

⚠️ **An upgrade restarts the service.** The modems redial and IPsec renegotiates, so the box has no tunnel for about
a minute. Upgrade from a computer on the box's **LAN**, never through the tunnel itself. The restart comes from the
package's post-install script, detached, 5 s after `apk add` (from 59-r3 on; OpenWrt's own post-install only runs
`start`, which procd ignores while the old supervisor runs, so packages up to 59-r2 left it running until
`/etc/init.d/sheaf restart`). Check with `logread -e sheaf | tail`: a new `starting (...)` line.

**`sheaf.conf` on upgrade.** `/etc/sheaf/sheaf.conf` is a conffile. If it was changed on the box (it always is:
`SRV`, keys of the role, `TPROXY` ...) and the package brings a new template, apk keeps the box's file and writes the
template next to it as `/etc/sheaf/sheaf.conf.apk-new`. Nothing from the new template takes effect until you merge
it: `diff /etc/sheaf/sheaf.conf /etc/sheaf/sheaf.conf.apk-new`, copy the lines you want, delete the `.apk-new`,
then `/etc/init.d/sheaf restart`. The box-1 settings of the 65 template (all other lines as before):

```sh
EXTRA="-maxq 20 -tungro -auth=half"   # -tungro from v61, -auth from v62; the server instance must run -auth=half too
TPROXY=1                              # only with -tallow public on the server instance (2.1)
BLOCK_QUIC=0                          # box 1: 1 (trade-off in step 4 above)
#FLOWTABLE=1                          # the default
```


Order when switching `TPROXY` on: first the server (`-tallow public`), then the box. Back: `TPROXY=0` + restart on
the box first, then remove `-tallow` on the server.

The packages do not contain the fixed EIP93 module. That module belongs to one exact kernel build, so it ships in the
firmware image (below), not in a package.

### 1.4 A new box (firmware image)

1. Flash OpenWrt on the RBM33G. With RouterBOOT 6.x, the 25.12.6 initramfs downloads over the network but does not
   start. What works is netbooting the 19.07.10 initramfs (BOOTP/TFTP), and from it `sysupgrade -F -n` with the 25.12.6
   sysupgrade image (the `-v7` image for RouterBOOT 7.x).
2. Install the sheaf image
   `openwrt-25.12.6-sheaf-ramips-mt7621-mikrotik_routerboard-m33g-squashfs-sysupgrade.bin`, built with
   [section 4](#4-building-the-firmware-image) (images are not kept in the repository; check the SHA-256 published
   with an image). Use LuCI → System → Backup/Flash → Flash image, or `sysupgrade -v /tmp/fw.bin`.
   It contains OpenWrt, all dependencies, sheaf and the **fixed EIP93 module**. With "keep settings", the keys survive
   later image upgrades.
3. Once per box: put the two keys in place (as above), set a root password (and an SSH key if wanted), then run
   `/etc/init.d/sheaf restart`. Set the root password before anybody else is connected to the box's LAN.
4. Later sheaf versions can then be installed as packages (1.3), without reflashing.

### 1.5 Hub and satellites

For more than two modems, one box (the **hub**) runs sheaf and IPsec, and the RBM33G boxes become **satellites**
that only dial their modems and forward the hub's links. The role is set in `sheaf.conf` (`ROLE=box` is the default).

| role | dials modems | IPsec, sheaf | what else |
|---|---|---|---|
| `box` | yes | yes | one box on its own (sections above) |
| `satellite` | yes | no | `ip rule from $SAT_SRC1` → table 100 (modem 1), `from $SAT_SRC2` → table 101 (modem 2); firewall zone `lte` on the modems with masquerade, forwarding `lan → lte` |
| `hub` | no | yes, over `HUB_LINKS` | each link leaves from its own source address and is routed to its satellite (`ip rule from <src>` → table 201, 202, …) |

**Reference hub: BPI-R4** (4 GB, an OpenWrt 25.12 build for the BPI-R4 with Wi-Fi), one port per satellite, set up by
`hub/hub-setup.sh`: `lan3` = the computer (stays in `br-lan` with Wi-Fi), `lan1` = satellite 1 (`10.77.1.1/24`),
`lan2` = satellite 2 (`10.77.2.1/24`), `wan` free; firewall zone `sat` for `lan1`/`lan2`. Each satellite has its own
small network, so the factory `192.168.1.1` and DHCP of the RBM33G never meet. Satellites on a shared segment behind a
switch also work, as long as each has its own address and its DHCP server is off. `sheaf-role satellite N` (below)
turns a box into satellite `N`: LAN `10.77.N.1N/24`, DHCP off, `ROLE=satellite`, `SAT_SRC1/2` = `10.77.N.(99+2N)` and
`+1`. `hub/sat-setup.sh` is a wrapper of it with the older usage (`N=1 sh -s < sat-setup.sh`).

```
ROLE=hub; BOX=hub; TUN=10.66.0.10/30; TUNGW=10.66.0.9
HUB_LINKS="10.77.1.101:7001:47021:10.77.1.11:l1 10.77.1.102:7002:47022:10.77.1.11:l2
           10.77.2.103:7003:47023:10.77.2.12:l3 10.77.2.104:7004:47024:10.77.2.12:l4"
IKE_PROP=aes256gcm16-prfsha384-ecp384; ESP_PROP=aes256gcm16-ecp384; GOMAXPROCS=4
```

`HUB_LINKS` entries are `source:localport:serverport:satellite:id`. The port of each link is the one with a direct route
to its satellite; `HUB_IF` is used only when there is none.

**Autodetection.** `HUB_LINKS` lists every possible link once. Every 10 s the hub checks each satellite
(`hub_scan`): it counts when it has a direct route on one of the hub's ports (not via the tunnel) and answers one ping.
A satellite comes in after 2 good checks in a row (20 s) and goes out after 3 bad ones (30 s). sheaf, IPsec and the
routing use only the links of the satellites that are in; a change of that set restarts sheaf. Without any satellite
sheaf is not started. A satellite whose modems are down still counts: sheaf itself marks those links dead.

The server instance of the hub keeps all its links permanently (`LINKS` with ports 47021–47024): since v55 the two
ends pair links by id, so the hub may use any subset ([PROTOCOL.md 5.4](PROTOCOL.md#54-link-pairing-v55)).

#### Switching a box: `sheaf-role`

The main product is one box on its own; a satellite is the same box with another role. One command on the box switches
it, both ways:

```sh
sheaf-role                  # current role, LAN address, DHCP server
sheaf-role -n satellite 1   # only print what would change (also DRYRUN=1)
sheaf-role satellite 1      # satellite 1 of the hub (2: the hub's lan2)
sheaf-role box              # back to a box of its own
```

| | `sheaf-role box` | `sheaf-role satellite N` |
|---|---|---|
| `/etc/sheaf/sheaf.conf` | `ROLE` / `SAT_SRC1` / `SAT_SRC2` lines removed (default role `box`) | one line `ROLE=satellite; SAT_SRC1=10.77.N.(99+2N); SAT_SRC2=10.77.N.(100+2N)` |
| `network.lan` | `192.168.1.1/255.255.255.0` | `10.77.N.1N/255.255.255.0` |
| `dhcp.lan.ignore` | removed (DHCP server on) | `1` |
| firewall | zone `lte` and forwarding `lan → lte` removed (otherwise the LAN could reach the modems outside the tunnel); zone `sheaf`, forwarding `lan → sheaf` and the dnsmasq resolvers put back as `90-sheaf` sets them, only if missing | unchanged; `sheaf-run` adds zone `lte` + `lan → lte` itself in the satellite role |
| `ip rule from <SAT_SRC>` | the old satellite's rules removed | added by `sheaf-run` |

All other lines of `sheaf.conf` stay (`BOX`, `ID1/2`, `PORT1/2`, `TUN`, `TUNGW`, `SRV`, `IPSEC`, `APN`, `EXTRA`, …). Only
what differs is changed, so running it again does nothing. It refuses a board with `ROLE=hub` and uncommitted uci
changes. Before a change it copies `sheaf.conf` and `/etc/config/{network,dhcp,firewall}` to
`/etc/sheaf/role-backup/<time>/` (the 5 newest are kept). The change is applied detached at the end (sheaf stopped,
network, firewall and dnsmasq reloaded, sheaf started; log `/tmp/sheaf-role.log`): the box drops off its old address
and the SSH session with it. A box back in the `box` role serves DHCP on whatever its LAN port is cabled to: harmless on the
hub's own port (static addresses there), not on a shared segment with other satellites; reach it at `192.168.1.1` from
a computer on its LAN port.

### 1.6 Modem types and IPsec algorithms

| setting | default | meaning |
|---|---|---|
| `MODEM` | `huawei` | `mbim`: MBIM modules, dialled with `umbim` (the address comes from the modem, no DHCP, home network only). `qmi`: QMI modules, dialled with `uqmi` (raw IP, the address comes from the modem, no DHCP). Both not yet tested on hardware. `huawei`: ME909s-120 (no longer sold, used for all measurements), dialled with AT (`AT^NDISDUP`) and DHCP |
| `MODEM_wwan0`, `MODEM_wwan1` | `$MODEM` | the type of one slot (wwan0 = slot 1, wwan1 = slot 2): the two modems may be of different kinds, e.g. `MODEM_wwan1=mbim` with a Huawei in slot 1 |
| `USB1`, `USB2` | `1-2`, `1-1` | USB path of modem 1 and 2. Modems are found by slot, never by the order they enumerated in; a QMI or MBIM modem's interface is renamed to `wwan0`/`wwan1`. BPI-R4 Pro 8X with M.2 modules in CN18 and CN21: `USB1=4-1.2; USB2=4-1.4` |
| `APN`, `APN_wwan0`, `APN_wwan1` | `internet` | APN for all modems or per modem |
| `IKE_PROP` | `aes256-sha384-ecp384` | IKE proposal. AES-CBC because the RBM33G's EIP93 has no GCM |
| `ESP_PROP` | `aes256-sha256-ecp384` | ESP proposal (PFS group included) |

On a hub with ARMv8 crypto extensions (BPI-R4) use AES-GCM: `IKE_PROP=aes256gcm16-prfsha384-ecp384`,
`ESP_PROP=aes256gcm16-ecp384`, and the same on the server's responder. The key exchange is ECP-384 everywhere; DH group
14 is never used. The BPI-R4 needs the strongSwan `openssl` plugin for ECP-384 (`strongswan-mod-openssl`, a userspace
package of the same OpenWrt release).

---

## 2. The server

Any Linux host with a public IPv4 address and strongSwan (swanctl) for the IPsec side.

### 2.1 sheaf

**One server instance per box.** An instance keeps one peer state (one session, one set of links), so every box (or
hub) has its own instance on the server, with its own UDP ports, TUN device, tunnel subnet and key:

| instance | unit | binary | ports | TUN, server address | key |
|---|---|---|---|---|---|
| box 1 | `sheaf.service` | `/opt/sheaf/sheaf-box` | 47001–47002 | `shf0`, 10.66.0.1/30 | `/etc/sheaf/box1.key` |
| box 2 | `sheaf@box2` | `/opt/sheaf/sheaf` | 47011–47012 | `shf1`, 10.66.0.5/30 | `/etc/sheaf/box2.key` |
| hub | `sheaf@hub` | `/opt/sheaf/sheaf` | 47021–47024 | `shf2`, 10.66.0.9/30 | `/etc/sheaf/hub.key` |

Box 1 has a binary of its own, so an experiment on the `sheaf@` instances (which share `/opt/sheaf/sheaf`; replacing
it changes all of them at their next restart) never swaps the binary under box 1.

```
/opt/sheaf/sheaf-box         (amd64 build, box-1 instance)
/opt/sheaf/sheaf             (amd64 build, sheaf@ instances)
/opt/sheaf/tcpbench          (measurement endpoints, optional)
/etc/sheaf/box1.key          (the same key as /etc/sheaf/sheaf.key on box 1, mode 600)
/etc/systemd/system/sheaf.service        ← server/sheaf.service
/etc/systemd/system/sheaf@.service       ← server/sheaf@.service (further boxes, the hub)
/etc/systemd/system/sheaf-bench.service  ← server/sheaf-bench.service (optional)
/opt/sheaf/env               environment of sheaf.service (example: server/box1.env)
/opt/sheaf/<instance>.env    environment of sheaf@<instance> (examples: server/box2.env, hub.env)
```

`sheaf.service` runs

```
/opt/sheaf/sheaf-box -reliable -serve -allow 127.0.0.1:5201,127.0.0.1:5202 -key /etc/sheaf/box1.key -tun shf0 \
    -links 0.0.0.0:47001>-,0.0.0.0:47002>- -trace=${TRACE} -frame=${FRAME} $EXTRA
```

and afterwards configures `shf0` as 10.66.0.1/30 with MTU 1320. The variables come from `/opt/sheaf/env`:

| variable | value on the reference server | meaning |
|---|---|---|
| `FRAME` | `1328` | must match the box (1328 with IPsec) |
| `EXTRA` | `-status /run/sheaf-status.json -mode peer -pace 1.25 -fqlimit 1048576 -tallow public -tdeny :25 -auth=half` | status file, follow the box's mode, pace each link, 1 MB flow queue (the default since v59, explicit for rollbacks), transparent opens for a box with `TPROXY=1`, never SMTP, the same datagram tag as the box (`-auth=half`) |
| `TRACE` | empty | path of a 100 ms CSV trace, empty = off |

So the box-1 instance runs in full: `-reliable -serve -allow 127.0.0.1:5201,127.0.0.1:5202 -key /etc/sheaf/box1.key -tun
shf0 -links 0.0.0.0:47001>-,0.0.0.0:47002>- -frame=1328 -status /run/sheaf-status.json -mode peer -pace 1.25
-fqlimit 1048576 -tallow public -tdeny :25 -auth=half`. The example file is `server/box1.env` (install it as
`/opt/sheaf/env`). An instance without `-tallow` refuses transparent opens and otherwise works as before. `-auth`
must be the same on the box and on its instance: on a mismatch every datagram is rejected (logged
`auth mismatch?`, status `auth_rejects`).

`-allow` limits which targets the box's proxy may open (here only the two local measurement endpoints). Without
`-allow`, `-serve` would let the box open any TCP target.

For boxes with `TPROXY=1` add `-tallow public` to `EXTRA` (transparent opens to any public address; private,
loopback, link-local, CGNAT, multicast and the server's own addresses are refused unless listed as a CIDR in
`-tallow`; `-tdeny` adds addresses or `:port`s that are always refused). Without `-tallow` every transparent open is
refused. The server dials these connections itself (its OUTPUT path), so a FORWARD rule for `shf0` traffic does not
apply to them; `-tdeny` is the place for such limits (e.g. `-tdeny :25`).

`server/sheaf-server.sh` is a helper: `status`, `logs [N]`, `trace-on [file]` / `trace-off`, `frame N`, `src MB` (size
of the download source), `raw-on [MB]` / `raw-off` (plain per-modem TCP endpoints on 47003/47004 for comparison, which
stop by themselves after 30 min).

To restart, restart only the service (`systemctl restart sheaf`). The box notices the new session and resets
within seconds. The tunnel survives it (TCP inside the tunnel lives in the end hosts), but **TCP connections carried by
the proxy end**: their state is in the server process. Restart the server instance only for an upgrade, at a quiet
time.

Further boxes run as instances of `server/sheaf@.service`, each with `/opt/sheaf/<box>.env` (`LINKS`, `TUN`,
`ADDR`, `KEY`), as in the table above. A server instance may list more links than the box uses (v55).

### 2.2 Network and firewall

- Inbound: UDP 47001–47099 (sheaf, one or more ports per instance), UDP 500 and 4500 (IKE, NAT-T); optionally TCP
  47003–47004 for `raw-on`.
- Accept sheaf's UDP ports **only under IPsec** (e.g. an iptables `policy --dir in --pol ipsec` match): otherwise, after
  the server's SA was cleared, its probes of a dead link could leave or arrive in plaintext. The box has the same
  guard (nft table `sheaf_ipsec`: no plaintext sheaf UDP to the server).
- Forwarding: from `shf+` to the internet only, replies back, and masquerade `10.66.0.0/24` (one /30 per instance)
  behind the public address.


### 2.3 IPsec responder

The server side is configured by hand (swanctl). It mirrors what `sheaf-run` generates on the box; `server/` has
working examples for a box and a hub (`swanctl-sheaf-box2.conf`, `swanctl-sheaf-hub.conf`, one file each in
`/etc/swanctl/conf.d/`). A sketch:

```
connections {
    sheaf-o2 {
        version = 2
        remote_addrs = %any
        proposals = aes256-sha384-ecp384
        encap = yes
        local  { auth = psk  id = server.sheaf }
        remote { auth = psk  id = o2.box1.sheaf }
        children { sheaf-o2 { mode = transport
                               local_ts = dynamic[udp/47001]  remote_ts = dynamic[udp/7001]
                               esp_proposals = aes256-sha256-ecp384 } }
    }
    # sheaf-vf: the same with id vf.box1.sheaf, udp/47002 <-> udp/7002
}
secrets { ike-sheaf { id-1 = server.sheaf  id-2 = o2.box1.sheaf  id-3 = vf.box1.sheaf  secret = <PSK> } }
```

The PSK itself belongs in a separate file with mode 600, never in a repository.

---

## 3. Building sheaf

Requires Go (the module says `go 1.27.2`) and two modules (`golang.org/x/net`, `golang.org/x/sys`).

```sh
cd sheaf
GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build -trimpath -o sheaf-mipsle .   # box: MT7621 has no FPU
GOOS=linux GOARCH=amd64 go build -trimpath -o sheaf-amd64 .                      # server
GOOS=linux go vet ./...
```

- sheaf is Linux only (TUN, `SO_BINDTODEVICE`, `UDP_SEGMENT`). On macOS, `go vet` and `go build` need `GOOS=linux`,
  and the tests (`go test ./...`) must run on a Linux host or VM.
- Tests: `wire_test.go` (SipHash-1-3 against the reference vectors, all lengths and alignments), `halfsiphash_test.go`
  (HalfSipHash-1-3), `gap_test.go` (link gap detection incl. wrap-around and the session start), `modes_test.go`
  (smooth mode releases UDP, not TCP; speed mode keeps UDP in order; the mode byte round trip), `fq_test.go` (sparse
  flow first, fair share, the fattest flow is dropped), `flowrel_test.go` (a lost packet holds back only its own flow;
  the sender's flow chains), `linkpair_test.go` (links paired by id, different link counts on the two ends),
  `slowlink_test.go` (stuck links, v58), `v59_test.go` (pacing in the scheduler, transmit queue cap, repair time,
  drain, bounded receiver wait), `v60_test.go` (slow-link demotion and trials, non-blocking control sends),
  `v62_test.go` (`-auth`, quiet probing, mode file removal, the silent idle link of v63/v64), `tungro_test.go`
  (GSO writes into the TUN), `tproxy_test.go` (OPENT, server policy, windows), `review_test.go`, `learn_test.go`,
  `intpath_test.go` (regressions found in reviews, address learning, integer send path). The tests ran on amd64 and,
  for the MIPS arithmetic, on 386 with softfloat.
- The race detector was used in the lab (`go build -race`).
- `tcpbench` has its own module (`sheaf/tcpbench`, build it from there). `udpstream` and `linkprof` are packages of the
  main module (`go build ./udpstream`). `tcpbench` and `udpstream` also build for macOS; `linkprof` is Linux only.

On the box, run sheaf with `GOMAXPROCS=2` (set in `sheaf.conf`).

### Packages

`openwrt/build-apk.sh` builds both packages:

```sh
sh openwrt/build-apk.sh     # -> openwrt/apk/sheaf-<ver>-r1.apk, luci-app-sheaf-<ver>-r1.apk
```

It cross-compiles sheaf on the local machine (static, `GOOS=linux GOARCH=mipsle GOMIPS=softfloat CGO_ENABLED=0`,
version taken from `sheaf/status.go`), stages it with the service files, and builds the packages in the OpenWrt SDK
25.12.6 on a Linux build VM over SSH: `VM=user@host` (required), `VMPORT` (default 22), `VMPASS` (the VM user's
password, required: the script logs in with `sshpass`, which must be installed on the local machine) and `SDK` (the
SDK directory on the VM). `TARGET=x86` or `TARGET=r4` builds for x86-64 or the BPI-R4 instead (other SDK, output in
`apk/x86_64/` or `apk/aarch64/`). The `apk/` directory is not in git. `REL=<n>` sets
the package release (scripts changed, same sheaf), `SHEAF_SRC=<dir>` takes the binary from another sheaf tree
(e.g. `git archive <commit> sheaf | tar -x -C /tmp/v59`, then `SHEAF_SRC=/tmp/v59/sheaf`) with the scripts of
this one.

---

## 4. Building the firmware image

`openwrt/firmware/build-fw.sh` runs the OpenWrt 25.12.6 ImageBuilder (`ramips/mt7621`, profile
`mikrotik_routerboard-m33g`) on a Linux build machine. It takes two arguments: the overlay directory and the directory
with the two packages from `build-apk.sh`:

```sh
IB=~/sheaf-fw/openwrt-imagebuilder-25.12.6-ramips-mt7621.Linux-x86_64 sh build-fw.sh <overlay-dir> <apk-dir>
```

sheaf comes into the image as its own packages (`sheaf-*.apk`, `luci-app-sheaf-*.apk`, copied into the ImageBuilder's
`packages/`), so the image's sheaf is owned by apk and a later `apk add sheaf-<new>.apk` upgrades it cleanly; the
ImageBuilder signs its local package index with its own build key, so the official packages stay signature-checked.
The package list covers LuCI, `ip-full`, `kmod-tun`, `kmod-ipsec4`, `kmod-crypto-hw-eip93`, strongSwan (charon, swanctl,
kernel-netlink, socket-default, vici, wolfssl, random), the modem stack (`sms-tool`, `picocom`, `chat`, `wwan`, NCM/ECM
USB network drivers, option/wwan serial drivers), `rpcd-mod-file` (needed by the LuCI page to read and write the mode
file) and `sheaf`, `luci-app-sheaf`.

The overlay directory becomes the image's root file system overlay; the script does not assemble it. It holds only
the fixed EIP93 module (everything else, including the first-boot script `90-sheaf`, comes with the packages):

```
lib/modules/6.12.112/crypto-hw-eip93.ko         EIP93 module with patches/861-*.patch
```

The fixed EIP93 module is built with the OpenWrt 25.12.6 SDK (same kernel and vermagic) and replaces the packaged one.
Reinstalling the `kmod-crypto-hw-eip93` package on a box would bring back the broken module. The image contains no
keys or passwords.

---

## 5. Command-line reference

`sheaf [flags]`. Flags as defined in `main.go`, defaults in brackets.

**Links and tunnel**

| flag | default | meaning |
|---|---|---|
| `-links` | – | comma-separated `local>peer` per link. Since v55 the two ends may list a different number of links in any order (links are paired by id, [PROTOCOL.md 5.4](PROTOCOL.md#54-link-pairing-v55)); against a v54 peer keep the same number in the same order. `local` = `host:port` or `@device:port` (bind to an interface on any address). `peer` = `host:port`, or `-` = learn it from authenticated datagrams (needs `-key`, or `-auth=off` under IPsec) |
| `-tun` | `shf0` | TUN device name (created by sheaf; the address and MTU are set by the service) |
| `-key` | – | file with a 32-hex-character shared key: authenticate every datagram, allow address learning |
| `-auth` | `sip` | v62: the tag on every datagram (with `-key`), written `-auth=VALUE`. `sip` = SipHash-1-3 (as up to v61). `half` = HalfSipHash-1-3 (32-bit words, cheaper on the MIPS box): one TCP flow through the tunnel of box 1 went from 33.7 to 41.2 Mbit/s with it (ABBA), almost as fast as `off`, and it keeps a second check below IPsec, so box 1 and its server instance run `half`. `off` **only for links that run under IPsec** (ESP's HMAC authenticates every datagram): no tag is computed or checked, the 8-byte tag field is sent as 0, peer addresses are learned without `-key`; saves the tag's CPU (box profile: ~4 % in tunnel mode, ~15 % in proxy mode). `true` / `false` = `sip` / `off`. Status `auth` (tag in use) and `auth_alg`. Both ends must agree: on a mismatch every datagram is rejected and logged (`datagram rejected: … auth mismatch?`, at most every 10 s; status `auth_rejects`) |
| `-reliable` | false | never skip a hole; keep copies until cumulatively acknowledged; resend. Use on both ends |

**Modes**

| flag | default | meaning |
|---|---|---|
| `-mode` | `speed` | `speed`, `smooth`, `redundant`, `backup`, or `peer` = follow the mode the far end states in its feedback (server) |
| `-primary` | 1 | backup mode: the link (1 = first in `-links`) that carries data while it lives |
| `-redwait` | 0 | redundant mode (v52): wait at most this long (Go duration, e.g. `20ms`) for a full link before sending without its copy |

**Flows (v53)**

| flag | default | meaning |
|---|---|---|
| `-fqlimit` | 1048576 | flow queue between the TUN device and the sender, in bytes: per-flow queues, deficit round robin, sparse flows first; above the limit the fattest flow loses its oldest packet. `0` = read the TUN device directly, as up to v52. 262144 in builds before the v59 deployment (too small once `-txqcap` caps the link queues, [HOW-IT-WORKS](HOW-IT-WORKS.md#flow-queue-between-the-tun-device-and-the-sender)) |
| `-flowrel` | true | per-flow release: mark every IP packet with its flow predecessor, and on receipt deliver a packet as soon as the earlier packets of its own flow are through. `false` = strict order |
| `-modefile` | – | file with `mode [primary]`, re-read when its modification time changes (polled every second). v62: a missing file means the default mode (`-mode` / `-primary`, normally speed), applied at the next poll; up to v61 the last mode stayed until a restart |

**Regulator and pacing**

| flag | default | meaning |
|---|---|---|
| `-inc` | 0.5 | grow the window while srtt ≤ base × (1 + inc) (or the jitter band, if wider) |
| `-dec` | 1.5 | shrink the window when srtt > base × (1 + dec) (or twice the jitter band) |
| `-jitk` | 3 | jitter-aware bands: grow / cut excess at least jitk / 2·jitk × the RTT jitter (capped at 50 / 100 ms); 0 = pure ratios |
| `-maxq` | 100 | queue (ms) a link may hold on top of its base delay; upper bound when adaptive |
| `-maxqadapt` | true | learn the queue cap per link from loss (× 0.8 per loss cut, min 20 ms, +5 ms/s back) |
| `-losscut` | 0.03 | cut the window (× 0.8) when this share of the link's packets was NACKed in the last window |
| `-losshold` | 0.01 | stop growing above this NACKed share |
| `-pace` | 0 | pace each link at this gain; 0 = send at line rate. Server: 1.25. The rate is gain × max(delivery rate maximum of the last 1–2 s, window / srtt), checked in the scheduler (`-pacepick`); with `-pacepick=false`, gain × window / srtt (at least 1 Mbit/s) in the link's send loop, as up to v57 |
| `-pacepick` | true | v59: a link is a candidate in the scheduler only when its pace allows the packet now; `false` = the v57 pacer behind the scheduling decision |
| `-pacelook` | 0 | v59: a paced-out link whose tokens come within this duration stays a candidate (the wait added to its expected arrival, the packet sent on credit); 0 = it gives way at once (in the lab `2ms` cost ~3 of 55 Mbit/s) |
| `-txqcap` | 45 | v59: kB; a link takes no new packet while its transmit queue (handed over by the scheduler, not yet to the kernel) holds this much, about one GSO batch; an empty queue always takes one. 0 = unlimited, as v57 |
| `-drainfix` | true | v59: drain only with an empty transmit queue and a link that is not stuck, to ½ BDP instead of 6 kB, base = mean of the lower half of the drain's samples, window back ×2 per round trip; `false` = as v57 |
| `-drainevery` | 10s | interval of a link's base re-measurement (drain); a lab knob |

**Repair, stuck and slow links (v58–v60)**

| flag | default | meaning |
|---|---|---|
| `-slowk` | 1 | v58: a link is stuck when its oldest unacknowledged packet is older than slowk × its healthy round trip and its queue does not drain within 2 × slowk × that; a stuck link gets no new data. 0 = all v58 repair changes off (v57) |
| `-rescue` | true | v59: resend a stuck link's late packets over the other links (the v58 rescue; needs `-slowk` > 0) |
| `-fixguard` | true | v59: the time after which a transmission counts as late is fixed when it leaves, from its link; the NACK guard and the head-of-line RTO use it. `false` = the slowest live link's srtt now |
| `-iprelease` | 0 | v59, reliable: release buffered IP packets past a hole at send time + the largest link delay + this; the hole is still repaired and delivered late; proxy frames never overtake. 0 = off (never skip, as v58) |
| `-ipreleasecap` | 300ms | v59: with `-iprelease`, a link's one-way delay counts at most its minimum + this (a collapsed link's delay grows without bound) |
| `-slowfrac` | 0.03 | v60: a link whose delivery rate (samples taken with its window in use) stays below this share of the best other live link's for `-slowafter` is demoted: no data, no probes, only trial copies; back when a trial delivers 1.5 × the threshold. Speed and smooth modes; a demoted link still carries data when no other link lives. 0 = off (v59) |
| `-slowafter` | 10s | v60: how long a link must stay below `-slowfrac` before it is demoted (first trial after half of it, then every 10, 20, 40, 60 s) |
| `-tungro` | false | v61: the TUN with a virtio-net header; in-order TCP segments of one flow delivered in one receive batch go to the TUN as one GSO write (the kernel splits them again when it forwards). Fewer syscalls and kernel passes per packet on a slow receiver (the box). Off by default; on the RBM33G +22 % through the tunnel (box 1, 26 Nov), so the box's `sheaf.conf` template sets it. It concerns only the local TUN device: the far end needs no change |

| `-v57` | false | switch every v58, v59 and v60 change off (= v57 behaviour), for A/B comparisons and rollback (the v60 non-blocking control sends stay). It leaves `-fqlimit` alone: v57 with the 1 MB flow queue stalls, so roll back with `-v57 -fqlimit 262144` |

**TCP proxy**

| flag | default | meaning |
|---|---|---|
| `-proxy` | – | `listen>target`, several separated by `;`: TCP connections accepted on `listen` are opened to `target` by the far end. Needs `-reliable` |
| `-serve` | false | open the connections the far end asks for |
| `-allow` | – | with `-serve`: comma-separated `host:port` targets that may be opened (empty = any) |
| `-frame` | 1400 | proxy frame size in bytes, 256..1400; 1328 when the links run under IPsec |
| `-transparent` | – | box: listener (`addr:port`, e.g. `0.0.0.0:5300`) for TCP connections the firewall redirected to it; the original destination (SO_ORIGINAL_DST, IPv4) is opened by the far end (OPENT). Needs `-reliable` |
| `-tallow` | – | server: destinations of transparent opens, `public` and/or CIDRs, comma separated; empty = refuse them all |
| `-tdeny` | – | server: CIDRs, addresses and `:port`s never dialed for a transparent open |
| `-tmax` | 256 | transparent streams at a time; more are reset (box) or refused (server) |
| `-twin` | 4096 | box: largest receive window per transparent stream, KiB |
| `-tbudget` | 16384 | box: receive windows of all transparent streams, KiB; a new stream gets half of what is left, within 128 KiB..`-twin` |
| `-tidle` | 2h | close a transparent stream that moved no byte for this long |

**Observation**

| flag | default | meaning |
|---|---|---|
| `-status` | – | write the status JSON here every second (atomic rename) |
| `-trace` | – | write a CSV line every 100 ms |
| `-cpuprofile` | – | write a Go CPU profile, stopped by SIGTERM/SIGINT |
| `-statsevery` | 60s | v62: interval of the periodic stats log line; `0` = off. Up to v61 it was every 5 s and filled the box's 64 kB logread ring in ~10 minutes. Event lines (dead, back, quiet, slow, mode, auth) are logged as they happen |

**Environment**: `GOMAXPROCS` (2 on the box), `GOMEMLIMIT` (Go heap soft limit, `96MiB` on the box from `sheaf-run`),
`SHEAF_NOGSO=1` disables UDP GSO, `SHEAF_SEQBASE=N` (test only, the same on both ends) starts the numbering at N to
test the wrap.

At start sheaf logs its version (`sheaf v65 …`) and one `v59:`, `v60:` and `v61:` line with the switches in force
(the same text as `features` in the status JSON).
 A demotion and a return are logged as `Lx slow: demoted (…)` / `Lx slow: back (…)`. Every `-statsevery` (60 s) it logs one stats line: per link the window, in-flight bytes, transmit queue, RTT/base, relative
delay and packets sent; then rescued/still-missing resends, the wait counters (window, pace, copies, credit), and
delivered/skipped/late/NACKed/missing/rtx/buffered/hold/noDelay/dup/released counts.

---

## 6. Status JSON

Written by `-status` once a second (`status.go`). Rates are for the last second.

| field | meaning |
|---|---|
| `version`, `session`, `uptime_s`, `updated_unix` | build, own session id (hex), process uptime, time of writing |
| `reliable`, `mode`, `primary`, `follow_peer` | `-reliable`; current mode; primary link (1-based); whether the mode is taken from the peer |
| `delivered` | inner packets delivered in order |
| `nacked` | packets this side reported lost (NACK-RANGE) |
| `missing_reports` | sequence numbers named again in MISSING reports |
| `rtx` | head-of-line timeout resends |
| `buffered` | packets waiting in the reorder buffer |
| `nodelay` | TCP ACKs / WIN frames delivered ahead of a hole |
| `live_early` | smooth mode: UDP packets released ahead of a hole |
| `flow_early` | packets delivered ahead of a hole that belongs to another flow (per-flow release) |
| `fq_drops`, `fq_bytes` | packets dropped by the flow queue (from the fattest flow), bytes queued in it now |
| `repaired`, `miss_resends` | resends for NACKs and still-missing reports; of them for still-missing reports |
| `slow_resends`, `slow_resend_kb` | packets (and kB) sent again because their link got stuck (v58) |
| `hole_wait_ms` | how long the oldest packet in the reorder buffer has waited |
| `ip_released` | IP packets released past a hole by their deadline (`-iprelease`, v59) |
| `tun_writes`, `tun_segs`, `tun_supers` | `-tungro`: TUN writes, IP packets in them, GSO super-packets among them (`tun_segs / tun_writes` = packets per write) |
| `features` | the v59 and v60 switches in force, e.g. `pacepick=true pacelook=0s txqcap=45kB fixguard=true slowk=1.00 … slowfrac=0.030 slowafter=10s ctlsend=nowait` |
| `ctl_drops` | control datagrams (feedback, NACKs, still-missing reports) dropped because a link's socket was full; they never wait (v60) |
| `dups` | packets that arrived twice (a repair whose original was only late, or a dupWorthy copy) |
| `dup_sent` | copies sent over a second link (dupWorthy, redundant mode, probes) |
| `tproxy` | transparent proxy, only when on: `streams` now, `window_kb` granted now (box), `opened`, `refused` (server), `capped` (`-tmax`), `idle_closed`, `unanswered` (box: no answer in 30 s), `not_redirected` (direct connections to the listener) |
| `links[]` | per link: |
| ` .id`, `.dead` | link number, declared dead |
| ` .rx_mbit`, `.tx_mbit` | received / sent data on this link (first sends and repairs) |
| ` .tx_est_mbit` | delivery rate the peer's acknowledgements show (`rateAvg`) |
| ` .srtt_ms`, `.min_rtt_ms` | smoothed RTT and base RTT |
| ` .win_kb`, `.inflight_kb` | window and bytes in flight |
| ` .last_rx_ms`, `.last_fb_ms` | ms since the last data packet / feedback arrived on this link (−1 = never) |
| ` .dead_count` | times declared dead since start |
| ` .loss_pct`, `.loss_cuts`, `.max_queue_ms` | NACKed share of what this side sent (last window), window cuts due to loss, learned queue cap |
| ` .stuck`, `.stuck_count` | the link's queue drains far slower than its healthy round trip (it gets no new data); times it got stuck (v58) |
| ` .txq_kb` | bytes queued for the link's send loop, not yet handed to the kernel (v59; held to about `-txqcap`) |
| ` .tx_pkts` | packets the send loop really sent since start (v59) |
| ` .pace_mbit` | the link's pacing rate in the scheduler, 0 = unpaced (v59) |
| ` .slow`, `.slow_count` | demoted as persistently slow (no data, only trial copies); times demoted since start (v60) |
| ` .busy_mbit` | the latest delivery-rate sample taken with the link's window in use, what the demotion compares (v60) |
| ` .trial_copies` | copies sent on the link during its trials (v60) |

On the box, `/cgi-bin/sheaf-status` wraps this file as `{"sheaf": …, "modems": [ … ], "uptime_s", "tunnel_ip"}`.
Each entry in `modems` has the name, interface, address, RAT, RSRP, SINR, RSRQ, cell, PCI, EARFCN, the redial count,
and the IPsec state (up, IKE and ESP algorithms, mode, rekey times, byte counters).

---

## 7. Trace CSV

`-trace FILE` writes one line every 100 ms (`trace()` in `main.go`). The columns:

- `t_ms`;
- per link `L<n>_win_k, L<n>_infl_k, L<n>_srtt_ms, L<n>_dead, L<n>_tx, L<n>_txdead, L<n>_rx` (packets in this 100 ms);
- `deliv, skip, late, nack, repaired, buf, stall_ms, hold_ms`;
- per link `L<n>_minrtt_ms, L<n>_rtt_ms, L<n>_rateavg_mbit, L<n>_ratemax_mbit, L<n>_grows, L<n>_cuts`;
- per link `L<n>_jit_ms`;
- `rescued, rescued_tot, missrtx, dups, wait_ms, stuck` (v58): packets resent because their link got stuck (this
  slice / total), resends for still-missing reports, duplicates received (totals), how long the oldest packet in the
  reorder buffer has waited, and a bit mask of stuck links (bit n−1 = link n);
- per link `L<n>_txq_k, L<n>_txsent, L<n>_pace_mbit` (v59): kB queued for the send loop and not yet handed to the
  kernel, packets the send loop really sent in this slice (`L<n>_tx` counts what the scheduler queued: a pacer behind
  the scheduler shows as `tx` > `txsent` and a growing `txq_k`), and the pacing rate (0 = unpaced);
- `iprel` (v59): IP packets released past a hole by their deadline in this slice.

`trace-summary.awk` summarises a trace.

---

## 8. Measuring tools and lab scripts

### Tools (Go, in `sheaf/`)

| tool | use |
|---|---|
| **tcpbench** | TCP source and sink without iperf. The stream content is checkable: every 8-byte word holds its own offset, so the receiver reports `content ok` or the first bad offset. `-listen :5202 [-send MB]` server (sink, or source of MB per client) · `-connect host:port -mb 50` upload · `-connect host:port -recv` download · `-stdin` checks a stream piped from `nc` · `-gen MB` writes the pattern to stdout |
| **udpstream** | a constant-rate UDP stream like a camera or a call, and a receiver that reports what a live player would see: loss, reordering, and lateness relative to the stream's fastest packet (no clock sync needed). `-recv :47999 -dur 70` · `-send host:47999 -rate 8 -size 1200 -dur 60` · `-hello host:port` / `-lport` for a receiver behind a firewall |
| **linkprof** | profiles one path: ramps a paced UDP rate (`-start 2 -step 2 -steps 40 -ms 1000 -size 1200`) and reports per step offered and acknowledged rate, loss and RTT p10/p50/p90/max. `-listen :47002` server · `-connect host:47002 -dev wwan1 -dir up\|down` |

### Lab (network namespaces with netem in a Linux VM)

The lab scripts run as root on a Linux VM. They expect the binaries in `/tmp` (`/tmp/sheaf`, `/tmp/sheaf-<name>` for
variants, `/tmp/tcpbench`) and a shared key in `/tmp/sheaf-lab.key` (`openssl rand -hex 16 > /tmp/sheaf-lab.key`);
they copy themselves to `/tmp` where they call each other (`/tmp/vary.sh`, `/tmp/drive.sh`, `/tmp/download.sh`).

| script | what |
|---|---|
| `lab.sh up \| single \| bond \| down \| show` | namespace A (box side) and B (server) over 3 emulated LTE links (20 / 10 / 5 Mbit/s, 40 / 70 / 120 ms) |
| `vary.sh`, `drive.sh [s] [seed]` | links that change over time: phases with outages; a reproducible "drive" with handovers and dead spots every 2–6 s |
| `blackout.sh` | all links fail together, three times in 45 s |
| `download.sh` | download scenario: deep LTE-like downlink queues, rate wobble, jitter, an LTE profile (`QMS`, `JIT`, `WOBBLE`, `WOBUP`) |
| `oracle.sh`, `oracle-drive.sh` | ideal ceiling: one link carrying the sum of the available rates at the delay of the fastest live link |
| `bench.sh` | repeated runs, variants interleaved run by run; results as CSV (`MODES="static vary drive"`) |
| `run-vary.sh` | one 60 s run through `vary.sh`, the tunnel or the MPTCP proxy baseline |
| `integrity.sh`, `integrity-all.sh` | byte-exact transfers during link outages with client or server restarts |
| `stall.sh` | reproducer for download stalls (fresh session per run, bursty loss) |
| `tuntest.sh` | long download through the TUN device (more than 65 536 pure ACKs upstream) |
| `extra-links.sh` | the server lists more links than the box uses (v55 link pairing); with `SHEAF_SEQBASE`, the sequence wrap |
| `trace-vary.sh`, `trace-static.sh`, `trace-summary.awk` | runs with 100 ms traces, and a summary of a sender trace |

The VM's idle C-state (HLT) made timers oversleep by 100–600 ms. The scripts switch it off for the duration of a
measurement (`NOHLT=0` keeps it).

### Real LTE (box + server)

| script | what |
|---|---|
| `round.sh NAME` | one measurement round from a computer on the box's LAN: plain TCP per modem, then sheaf without and with IPsec, both directions, then latency (runs `raw-up.sh`, `sheaf-lte-ipsec.sh`, `latency.sh` on the box) |
| `longrun.sh` | continuous test through the running service: every 5 min 100 MB down and 30 MB up |
| `openwrt/raw-up.sh` | plain TCP per modem, without sheaf |
| `openwrt/sheaf-lte.sh`, `sheaf-lte-ipsec.sh` | sheaf over both modems from the box itself, without / with IPsec |
| `openwrt/sheaf-lte-mac.sh` | the box only forwards (proxy on the LAN side), the client on the LAN measures |
| `openwrt/sheaf-lan.sh` | the box's CPU ceiling without SIM data: sheaf over the LAN cable to a lab server |
| `openwrt/latency.sh` | latency through the bond vs straight over each modem, small page fetch times through the tunnel |
| `openwrt/lte-profile.sh` | `linkprof` ramp over one modem |
| `openwrt/esp-cost.sh` | ESP cost on the box: two namespaces over veth, plain vs ESP, hardware vs software |
| `openwrt/cpu-prof.sh`, `cpu-prof-dl.sh`, `cpu-test.sh` | CPU profiles of sheaf on the box: the sending side on loopback (`cpu-prof.sh`), the receiving side during a download over the LAN cable (`cpu-prof-dl.sh`); its CPU ceiling with both ends on loopback (`cpu-test.sh`) |

---

## 9. Repository layout

| path | contents |
|---|---|
| `sheaf/` | the program (Go): `main.go` links, scheduler, regulator, repair, receiver · `wire.go` trailer, sessions, sockets · `siphash.go`, `halfsiphash.go` datagram tags · `modes.go` modes · `fq.go` flow queue · `flowrel.go` per-flow release · `slowlink.go` stuck links (v58) · `v59.go` pacing in the scheduler, repair time, drain hygiene, bounded receiver wait · `v60.go` slow-link demotion, non-blocking control sends · `tungro.go` GSO writes into the TUN (v61) · `v62.go` `-auth`, quiet probing, silent links (v62–v64) · `proxy.go` TCP proxy · `tproxy.go` transparent proxy · `status.go` status JSON · `pool.go` · tests `*_test.go` |
| `sheaf/tcpbench/` | TCP source/sink with byte-exact content check (for boxes without iperf; its own module) |
| `sheaf/udpstream/` | constant-rate UDP stream (camera/call), receiver reports loss, reordering, lateness |
| `sheaf/linkprof/` | profiles one path by ramping a paced UDP rate (base RTT, knee, delay at the onset of loss) |
| `sheaf/mp/`, `sheaf/proxy/` | early experiments over Linux MPTCP, kept as a lab baseline |
| `openwrt/service/` | box service: `sheaf-run` supervisor, `sheaf-role`, init script, config, status page and its CGI, LuCI app |
| `openwrt/package/`, `openwrt/build-apk.sh` | OpenWrt package definitions (`sheaf`, `luci-app-sheaf`) and the script that builds the `.apk` files |
| `openwrt/firmware/` | `build-fw.sh` (ImageBuilder) and the first-boot script `90-sheaf` |
| `openwrt/patches/` | the EIP93 driver fix |
| `openwrt/*.sh` | test scripts run on the box (real LTE, CPU profiles, ESP cost, link profiles) |
| `server/` | server systemd units (`sheaf.service`, `sheaf@.service`, `sheaf-bench.service`), one `.env` per box or hub (`box1.env`, `box2.env`, `hub.env`), swanctl examples (`swanctl-sheaf-box2.conf`, `swanctl-sheaf-hub.conf`), helper script `sheaf-server.sh` |
| `hub/` | BPI-R4 as the hub: `hub-setup.sh` (ports), `sat-setup.sh` (RBM33G → satellite); images come from the BPI-R4 OpenWrt build, not stored here |
| `lab/` | lab: emulated links (`lab.sh`), scenarios (`vary.sh`, `drive.sh`, `download.sh`, `blackout.sh`), repeated benchmarks, integrity and stall reproducers, real-LTE rounds (`round.sh`, `longrun.sh`) |
| `docs/` | this guide, [HOW-IT-WORKS.md](HOW-IT-WORKS.md), [PROTOCOL.md](PROTOCOL.md), [RESULTS.md](RESULTS.md) |
