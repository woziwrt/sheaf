# sheaf: test results and evaluation

Everything measured during the project (November 2026), in one place, with the method and an honest evaluation. The
mechanisms behind the numbers are in [HOW-IT-WORKS.md](HOW-IT-WORKS.md), the setup in [USAGE.md](USAGE.md).

The project is closed as a finished lab project. The final code is sheaf v65 (= v64 with no behaviour change), box
packages 65-r1. Earlier versions are named where a number was measured with them.

1. [Summary](#summary)
2. [Conclusions](#conclusions)
3. [Method](#method)
4. [Lab simulation](#lab-simulation)
5. [Real LTE: first box measurements (23–24 November)](#real-lte-first-box-measurements-2324-november)
6. [BPI-R4 hub, four LTE links](#bpi-r4-hub-four-lte-links)
7. [Single RBM33G box](#single-rbm33g-box)
8. [The CPU limit, proven: the R4 as the brain](#the-cpu-limit-proven-the-r4-as-the-brain)
9. [IPsec cost](#ipsec-cost)
10. [Reliability tests T1–T16](#reliability-tests-t1t16)
11. [Overnight soak and idle data use](#overnight-soak-and-idle-data-use)
12. [27 November: final version on the box (v65)](#27-november-final-version-on-the-box-v65)
13. [Not verified, known limitations](#not-verified-known-limitations)

---

## Summary

| what | result |
|---|---|
| lab, 3 emulated links, one TCP flow (v19) | static **95 %**, varying **95 %**, "drive" **89 %** of an ideal single link with the summed rate |
| lab, tunnel stalls under link collapses (v57 → v59) | runs with a stall **6/6 → 0/6**, 41.4 → **55.5 Mbit/s** |
| lab, throttled 256 kbit/s SIM beside a 25 Mbit/s link (v59 → v60) | 10.7 → 18.6 Mbit/s over 60 s, **22.4** after the demotion (the good link alone: 21.9) |
| BPI-R4 hub, 4 LTE links, IPsec, one TCP flow through the tunnel (v59) | down **158–188 Mbit/s** with the 1–2 MB flow queue (6 runs; 59–77 with 256 KB), up **108**; runs with a stall: v59 **0 of 17**, v57 4 of 7 |
| single RBM33G box, 2 LTE links, IPsec, one TCP flow | tunnel 17–23 → **38–41 Mbit/s** after flowtable, `-tungro`, `-auth=half`; transparent proxy **45–48** |
| same SIMs, BPI-R4 as the brain, RBM33G only carrying the modems | **73, 74, 82 Mbit/s** = **82–92 %** of the raw links used together (89); 68–102 over all measured runs |
| reliability tests on the box (T1, T2, T4–T6, T8–T11, T15, T16) | **all passed**, T5 and T16 after fixes; T10 passed with a hardware caveat (inserting a SIM while running resets the box) |
| overnight soak, 9.5 h, v64 | no restart, sheaf RSS flat at **13 MB**, idle data **~15 MB per day per SIM** |
| power on → working tunnel | **75 s** (firmware image), ~95 s after a power cut |
| final v65 on the box (27 Nov) | sheaf killed → both links back in 8 s; tunnel 31.9–36.5 Mbit/s, proxy 47.2–48.0 (v64 the evening before: 38–41 / 45–48); a modem cut during a live UDP stream survived in every mode; a one-direction link failure is not detected per direction |

## Conclusions

1. **The concept works.** On a fast enough CPU, the bond adds the links up for a single TCP flow: the BPI-R4 hub
   carried one TCP download at 158–188 Mbit/s over four LTE links (with the 1–2 MB flow queue), and with the same two
   SIMs as the single box it reached 82–92 % of the two raw links used together; its best run (82) was about equal to
   the better link alone (79), within the run-to-run spread. Repair, per-flow order, pacing in
   the scheduler and the 1 MB flow queue together removed the multi-second stalls seen with v57, in the lab and on LTE.

2. **Hardware verdict: the RBM33G (MT7621, 880 MHz MIPS) is too weak for the bonding software at LTE speeds.** Its CPU
   time per packet is the limit, not the radio, the links, IPsec or the algorithm. After every optimisation that was
   cheap to make (flowtable, HalfSipHash, GSO writes into the TUN) one TCP flow gets 38–41 Mbit/s through the tunnel
   and 45–48 Mbit/s through the transparent proxy. Proof: with the same SIMs at the same time, the BPI-R4 (Cortex-A73)
   as the brain gave 73–82 Mbit/s (steady 95 in the best run) where the raw links together gave 89, and the R4
   throttled with cgroup `cpu.max` to 12.5 % of one core fell to 24 Mbit/s, the RBM33G's level before the box-side
   optimisations (17–23). Per packet the MT7621 needs ~720 µs, one
   A73 core ~70 µs. For practical use, choose an ARM64 or x86 class box (the BPI-R4 or similar; an x86 UP Squared
   with AES-NI ran ESP at 489 Mbit/s against the RBM33G's 60.6). The RBM33G stays useful as a satellite that only
   carries modems (it forwarded 80 Mbit/s at 25 % CPU), or where reliability matters more than speed.

3. **Bonded vs the best single link, honestly:**

   | setup | bonded | best single link | verdict |
   |---|---|---|---|
   | RBM33G alone, 26 Nov (same minutes) | tunnel 18.5–20.3, proxy 59–69 (v59, before the CPU work) | Vodafone raw 70.9 | slower than the best link |
   | RBM33G alone, final (v64) | tunnel 38–41, proxy 45–48 | Vodafone raw 79 (19:48 the same evening, from the R4 through the satellite) | slower than the best link |
   | RBM33G alone, 24 Nov round 5 | ~79 without IPsec, ~61 with IPsec (proxy) | Vodafone raw 107.4 | slower than the best link |
   | BPI-R4 as the brain, same two SIMs, 26 Nov 19:48–19:52 | 73, 74, 82 (steady 95 in the best run) | Vodafone raw 79, O2 raw 27; both raw together 89 | **82–92 % of the raw sum**; −8 to +4 % against the best link, within the run-to-run spread (steady +20 % in the best run) |
   | BPI-R4 hub, 4 links, v59, 26 Nov morning | 158–188 (6 runs with the 1–2 MB flow queue; the four 1 MB default runs: median 167) | backup mode on Vodafone only, **through sheaf (not raw)**: 100–104 | **+52–88 %** over that link (medians +64 %); no raw single link was measured that morning |

   On the RBM33G the bond buys **reliability, not speed**: a modem that loses its address, its signal or its IPsec SA
   is invisible to the LAN (T2: ping through the tunnel 300/300 while O2 changed its address). The raw sum of the four
   hub links used together (156 down / 121 up) was measured the evening before the v59 runs, at a different time of
   day, so the v59 result is not stated as a share of it.

4. **Tunnel vs proxy.** On a slow CPU the transparent TCP proxy is the faster path for TCP (one write of a big chunk
   instead of one TUN write and one ACK per packet); on the R4 the tunnel is fast enough. The proxy's price: its TCP
   connections end when sheaf restarts, while tunnel connections survive.

5. **Robustness of the box.** Every failure provoked on purpose (sheaf killed or frozen, new modem address, lost PSK,
   zombie modem, throttled SIM, rekey, server restart, server losing its IKE state, stuck socket, power cut, missing
   SIM) ended with the tunnel back without manual steps; the slowest recovery was ~95 s after a power cut. Three of
   the tests found real faults first (stuck forever after a lost PSK, 86 s outage after the server lost its IKE state,
   a frozen sheaf looping without a tunnel), which were fixed and retested.

---

## Method

- **One TCP flow.** Unless stated otherwise every speed is a single TCP connection, because that is the hard case
  for a bond (a download of one file, one video stream). Many parallel flows fill links much more easily.
- **Byte-exact content.** Transfers to and from the server used `tcpbench` (in `sheaf/tcpbench`): every 8-byte word
  carries its own offset, so every transfer was checked byte by byte, and every one reported here passed. The real
  internet downloads of the transparent-proxy test used `curl` into `/dev/null`.
- **Server.** A Linux server with a public IPv4 address and 1 Gbit/s; the measurement endpoints ran on it
  (`tcpbench`, sink and source on localhost behind the built-in proxy, or on the tunnel address).
- **Raw links.** "Raw" means plain TCP over one modem without sheaf and without IPsec, measured right before or in
  the same minutes. Plain TCP underestimates LTE capacity (a paced UDP ramp reached 58 Mbit/s on O2's downlink where
  plain TCP gave 25–37), so shares of a raw sum are, if anything, optimistic. Raw links used *together* are lower than
  the sum of each alone when two SIMs share a cell (the two T-Mobile SIMs at the hub did).
- **Order.** A/B comparisons ran interleaved, ABBA or ABAB, with both ends switched to the build under test and
  restarted before every run; for the hub v59 test both ends were checked to have all 4 links up before every run.
- **Stall.** Less than 100 kB acknowledged within 0.5 s, from the sender's `ss -tin` every 250 ms. A run counts once
  however many stalls it has; runs with a stall are compared with Fisher's exact test, two-sided.
- **LTE varies.** Cell load changes over the day: radio samples of the satellites showed the busiest cells at 18–21 h,
  the quietest at night. Numbers from different times of day are not compared as shares. All LTE measurements were
  stationary, indoors, in one place in the Czech Republic, with SIMs of O2, Vodafone and T-Mobile.
- **Lab.** Network namespaces in a Linux VM, links emulated with netem/tbf; the VM's idle C1 state was switched off
  during measurements (it made timers oversleep by 100–600 ms and faked dead links). The lab is good for the
  mechanism and for A/B; its x86 CPU does not show the box's per-packet cost.
- The raw logs, CSV traces and working notes are not part of this repository; the measuring scripts are
  ([USAGE.md 8](USAGE.md#8-measuring-tools-and-lab-scripts)).

---

## Lab simulation

### Bond vs an ideal single link and Linux MPTCP

Three emulated links (20 / 10 / 5 Mbit/s, 40 / 70 / 120 ms), one TCP flow, 5 runs each. "Ideal" = one link carrying the
sum of the available rates at the delay of the fastest live link (`oracle.sh`, `oracle-drive.sh`). The MPTCP baseline
is a TCP proxy over Linux kernel MPTCP (`sheaf/proxy`) on the same links.

| scenario | sheaf v19 | ideal | share | proxy over Linux MPTCP |
|---|---|---|---|---|
| static links 20 / 10 / 5 Mbit/s | 30.2 ± 0.8 | 31.7 | 95 % | 28.1 |
| varying, outages every 10 s (`vary.sh`) | 21.5 ± 0.1 | 22.7 | 95 % | 13.3 |
| driving, each link changes every 2–6 s (`drive.sh`) | 20.4 ± 1.5 | 22.9 | 89 % | 21.4 |

v21 (warm restart after a short outage): static 30.7 ± 0.4, varying 21.3 ± 0.2, drive 20.7 ± 2.0. Datagram
authentication (v23, SipHash) cost about 1–3 % in the lab. The drive scenario stayed at ~90 % of the ideal; MPTCP was
slightly ahead there (21.4 against 20.4, within sheaf's spread of ± 1.5). Tuning parameters did not move it (v20, v22
were worse or within the spread).

### Correctness: integrity, stalls, wrap

| test | result |
|---|---|
| byte-exact transfers with client or server restarts during link outages (v30, `integrity.sh`) | 26/26 content ok |
| regulator study (v47) | 320/320 content ok, 40/40 without a stall |
| stall reproducer, fresh session per run with bursty loss (`stall.sh`) | v34: 3 stalls in 60 runs; v35/v36: 0 in 160 |
| long download through the TUN with only pure ACKs upstream (`tuntest.sh`, 1.2 GB) | v53 froze after 994 MB (65 536 ACKs held); v54 complete, content ok |
| server with more links than the box uses (`extra-links.sh`) | v54 stalled every time; v55 (links paired by id) 100 MB ok 3/3 |
| sequence numbers wrapping past 2³² (`SHEAF_SEQBASE`) | 4 × 100 MB down + 8 uploads across zero, content ok, no resends |

### v59: pacing in the scheduler, repair time, drain hygiene

One TCP flow for 60 s, 3 links (25 / 35 / 20 Mbit/s down, 15 / 20 / 10 up; tbf + netem 17 ± 4 ms), server `-pace 1.25`,
ABBA, 5–6 runs per arm. Scenario a) reproduces the LTE stall mechanism (a drain every 2 s plus short rate dips),
b) rate collapses to 2.5 % for 2–5 s, c) steady and varying links.

| scenario | build | Mbit/s | runs with a stall | TCP rtt max / p99 (ms) |
|---|---|---|---|---|
| a) pacer + drain, down | v57 | 41.4 ± 2.1 | 6/6 | 1167 / 854 |
| | **v59** | **55.5 ± 1.8** | **0/6** (Fisher two-sided p = 0.002) | 372 / 319 |
| a) up | v57 | 30.8 ± 0.7 | 4/5 | 795 / 643 |
| | v59 | 33.2 ± 1.3 | 0/5 | 336 / 290 |
| b) collapse, down | v57 | 40.2 ± 2.8 | 5/5 | 3463 / 3085 |
| | **v59** | **53.0 ± 2.2** | **0/5** (two-sided p = 0.008) | 345 / 247 |
| | v59 without the rescue of stuck links | 52.7 ± 3.2 | 2/5 | 558 / 361 |
| b) up | v57 | 28.7 ± 1.6 | 5/5 | 3019 / 2769 |
| | v59 | 33.0 ± 1.0 | 2/5 | 524 / 284 |
| c) steady links, down | v57 | 55.9 ± 1.3 | 1/5 | 341 / 201 |
| | v59 | 64.7 ± 0.9 | 0/5 | 125 / 114 |
| c) varying 50–150 % | v57 / v59 | 50.0 ± 2.3 / 56.1 ± 1.3 | 3/5 / 0/5 | 904 / 183 (max) |
| c) varying 20–180 % | v57 / v59 | 39.3 ± 1.4 / 47.5 ± 1.7 | 2/5 / 0/5 | 790 / 350 (max) |

- In the stalls of v57, 90 % of the 100 ms trace windows of the stuck link held an even packet count (55 % outside
  stalls): the fingerprint of the old pacer floor (two packets every 21.8 ms). The link transmit queue peaked at
  240–407 kB with the old pacer, 16–27 kB with v59.
- Duplicates on steady and mildly varying links: 0.00 %. Uploads still show short stalls in 2 of 5 runs under
  collapses, with no pacer fingerprint (the emulated network).
- The bounded receiver wait (`-iprelease`) never fired with working repairs; as a safety net over the v57 repairs it
  cut the TCP rtt max from 3.1 to 2.0 s but cost 10 % of the rate, so it is off.
- Backup mode with a shallow 150-packet bottleneck: without pacing 27.7 Mbit/s at 4.4–5.5 % loss, with pacing
  33.2 (v57) / 32.5 (v59) Mbit/s.

### v60: a persistently slow link (throttled SIM)

Two links, link 1 25 Mbit/s down / 15 up, link 2 throttled to a fixed rate both ways; one TCP flow for 60 s.

| link 2 | direction | v59 | v60 | v60 after the demotion (from 15 s) | link 1 alone |
|---|---|---|---|---|---|
| 256 kbit/s | down | 10.7 ± 1.6 | 18.6 ± 0.4 | **22.4** | 21.9 |
| 256 kbit/s | up | 8.5 ± 0.5 | 11.5 ± 0.1 | 13.3 | 13.3 |
| 512 kbit/s | down | 19.3 ± 0.5 | 20.7 ± 0.2 | 22.3 | 22.1 |
| 2 Mbit/s | down | 23.5 ± 0.5 | 23.6 ± 0.1 (not demoted) | – | – |

v59 with a throttled SIM was slower than the good link alone; v60 demotes such a link (`-slowfrac 0.03`) and brings
it back by trial copies. In a regression over the v59 scenarios (ABBA, 3 × 60 s each) v60 was equal to v59 within the
spread everywhere. The non-blocking control sends were checked by a unit test only: the lab scenario meant for them
never filled the socket.

### Tunnel vs proxy at low CPU (`-tungro`)

Two links (50 / 70 Mbit/s down, 20 / 20 up, 17 ± 4 ms), the box side limited with cgroup `cpu.max` to a share of one
x86 core, one TCP flow for 15 s.

| CPU of the box side | no limit | 30 % | 20 % | 15 % | 10 % | 7 % | 5 % |
|---|---|---|---|---|---|---|---|
| tunnel (2 runs) | 58.7 / 69.3 | 57.3 / 56.9 | 53.7 / 56.8 | 53.4 / 58.0 | 57.0 / 42.7 | 51.7 / 40.5 | 29.7 / 27.3 |
| proxy (2 runs) | 67.3 / 64.1 | 57.4 / 69.4 | 49.9 / 60.2 | 61.8 / 58.2 | 57.7 / 56.2 | 65.9 / 68.9 | 61.5 / 50.0 |

At 5 % of a core: v59 tunnel 27.5 Mbit/s (mean of 6), v61 with `-tungro` **44.1** (+60 %, ~4 segments per TUN write),
proxy 47.0. On a fast CPU the tunnel is as fast as the proxy; the tunnel's limit appears only below ~10 % of a core.

### Transparent TCP proxy (correctness)

Client → box (sheaf + the box's nft redirect) → 2 links (30 / 40 ms) → server (`-tallow public`) → destination:
one flow down 55–64 Mbit/s through the proxy (39–58 through the tunnel on the same links), 100 parallel connections
100/100 correct (RSS 39 MB on x86-64), 200 short connections exactly 1 MiB each, refusals for the server's own
address and private destinations, `-tmax` and `-tidle` enforced, the nft table removed when sheaf dies.

---

## Real LTE: first box measurements (23–24 November)

RBM33G box with O2 and Vodafone, early versions. **These transfers went through the built-in proxy** (endpoints on
the box, or a computer on the LAN connecting to the box's proxy listener), not through the tunnel: the tunnel was
first measured on 26 November and turned out much slower on this box ([below](#single-rbm33g-box)).

| what | result |
|---|---|
| plain sum O2 + Vodafone (night) | up 67.2, down 71.6 Mbit/s |
| sheaf v32 (night, no IPsec) | up **66.1 (98 %)**, down 49.7 (69 %) |
| sheaf v37, `GOMAXPROCS=2`, a LAN client downloads 100 MB (night, no IPsec) | **81.7 Mbit/s = 84 %** of a plain sum of 97.6 |
| long transfer through the running service, IPsec | 3.1 GB in 430 s = **58.5 Mbit/s**, per minute 55–62, no stall |
| box boot to working tunnel (firmware image) | 75 s |
| ping through the tunnel, small web page | ~45 ms, 210–240 ms |

**Daytime rounds** (`round.sh`: plain → without IPsec → with IPsec, each round within ~5 minutes; Mbit/s):

| round | plain sum down / up | without IPsec down / up | with IPsec down / up |
|---|---|---|---|
| 1 (07:11, AES-256) | 114.9 / 64.7 | 83.7 (73 %) / 65.9 (~100 %) | 61.5 (54 %) / 51.9 (80 %) |
| 2 (07:16, AES-128) | 107.8 / 61.4 | 74.0 (69 %) / 66.0 (~100 %) | 54.6 (51 %) / 51.9 (85 %) |
| 3 (08:13) | 105.9 / 66.4 | 76.0 (72 %) / 61.5 (93 %) | 56.5 (53 %) / 47.9 (72 %) |
| 4 (09:02) | 105.7 / 59.5 | 66.8 (63 %) / 64.1 (~100 %) | 55.2 (52 %) / 48.6 (82 %) |
| 5 (12:07) | 137.4 / 54.3 | 78.7 (57 %) / 61.2 (~100 %) | 60.7 (44 %) / 48.8 (90 %) |

In round 5 Vodafone alone delivered 107.4 Mbit/s down while the bond held its ~79 / ~61: the box was the ceiling
already then, at about 80 Mbit/s without and 60 with IPsec through the proxy.

**Features over real LTE** (IPsec on):

| what | result |
|---|---|
| modes, 100 MB down (v50, before server pacing) | speed 51.5 · backup (Vodafone) 31.2 · redundant 18.4 |
| redundant v51 → v52 | live-stream p99 306 → 52 ms; bulk download 18.4 → 31.6; link L1 cut for 10 s: nothing lost, worst packet 210 ms late (v51: 2.6 % lost, 1.3 s stall) |
| server pacing (v51), Vodafone alone in backup mode, 4 + 4 alternating runs | 35.0 → **41.4 Mbit/s** (+18 %); gains 1.1 and 1.5 worse |
| learned queue cap (v49), daytime, alternating | fixed 100 ms: 59.9 Mbit/s at 16.8 % loss · fixed 40 ms: 57.1 / 15.1 % · learned: 56.5 / **4.7 %** |
| loss-aware regulator (v45), Vodafone daytime loss | 22 % → 11 / 3.5 / 1.4 % as it learns, same speed |
| 8 Mbit/s UDP stream down beside a bulk download (v52) | speed mode p99 0.98–2.1 s, 1.1–5.6 % lost · smooth mode p99 0.20–0.24 s, nothing lost |
| flow queue (v53): 8 Mbit/s UDP up beside a bulk upload through the tunnel | p50 80 → 41 ms, p99 238 → 122 ms, loss 0.2 % → 0 |
| per-flow release (v53): 8 Mbit/s UDP down beside a bulk download, smooth mode | p50 151 → 56 ms, packets > 100 ms late 83 % → 9 % |
| video down beside a full upload from the LAN | 4–19 % lost (the box's feedback queued behind the upload); with the box at `-maxq 20`: 0.7 % |

---

## BPI-R4 hub, four LTE links

A BPI-R4 (4 GB, OpenWrt) as the brain runs sheaf and IPsec (AES-GCM, ECP-384); two RBM33G boxes as satellites only
carry their modems: O2 + Vodafone on satellite 1, two T-Mobile SIMs on satellite 2. A computer behind the hub,
one TCP flow through the tunnel (or the hub's proxy) to the server.

### 25 November evening: tunnel vs proxy (v57, flow queue 256 KB)

4 cycles in ABBA order, 20 s per transfer, steady rate without the first 3 s, medians (Mbit/s; the proxy ran in
cycles 1 and 3 only, n = 2). The shares in brackets are the **median of the per-cycle ratios** (each bonded run
against the raw links measured in the same cycle), not the ratio of the medians shown: e.g. tunnel 4 links down
80.0 / 156.2 = 51 % as a ratio of medians, 54 % as the median of the four per-cycle ratios.

| | down | up |
|---|---|---|
| raw links, each alone: O2 / Vodafone / T-Mobile 1 / T-Mobile 2 | 43.3 / 72.5 / 55.5 / 84.0 | 41.5 / 37.2 / 39.7 / 38.9 |
| raw, sum of each alone | 248.7 | 158.2 |
| raw, all 4 at the same time (sum) | **156.2** (the T-Mobile SIMs share a cell: 30 + 30) | **121.4** |
| sheaf, 4 links, proxy | **135.6 (87 %** of the simultaneous sum) | **110.1 (93 %)** |
| sheaf, 4 links, tunnel | 80.0 (54 %) | 67.6 (56 %) |
| sheaf, 2 links (Vodafone + T-Mobile 1), proxy | 104.8 (91 % of the two alone) | 69.5 (94 %) |
| sheaf, 2 links, tunnel | 66.3 (56 %) | 48.8 (63 %) |

- The bond itself carried 87–94 % of the links (proxy); one TCP flow through the tunnel only 54–63 %, down no faster
  than the best single link. CPU was not the limit on the hub (busiest core 19–30 %, server 17–25 %).
- Ping of another flow under load: 39–56 ms through the tunnel, 58–97 ms through the proxy.
- Data: 12.4 GB of useful data cost 14.4 GB on the SIMs: 16 % overhead (IPsec, sheaf headers, repairs, feedback, TCP).
- The cause of the tunnel gap turned out to be the 256 KB flow queue together with v57's link queues (next section).

### 26 November morning: v59 against v57

07:18–08:21, both ends switched and restarted before every run, server `-pace 1.25`, hub `-maxq 40`:

| build, flow queue | runs | Mbit/s down | runs with a stall | TCP rtt max |
|---|---|---|---|---|
| v57, 256 KB (deployed until then) | 7 | 70–96 (median 82) | **4/7** | 0.2–1.6 s |
| v59, 256 KB | 5 | 59–68 (median 66) | 0/5 | 71–90 ms |
| v59 `-pacepick=false`, 256 KB | 4 | 71–77 | 0/4 | 86–99 ms |
| v57, 1 MB | 2 | 86, 113 | **2/2** (8.8 s and 1.6 s) | 6.0 s, 1.2 s |
| **v59, 1 MB** | 4 | **158–181 (median 167)** | **0/4** | 0.24–0.36 s |
| v59 2 MB / v59 `-pacepick=false` 1 MB | 1 + 1 | 188 / 175 | 0 | 0.26 / 0.23 s |
| v59 `-pacelook 5ms` / v59 `-pace 0`, 256 KB | 1 + 1 | 69 / 75 | 0 | 0.14 / 0.08 s |

| what | v57, 256 KB | v59, 1 MB |
|---|---|---|
| runs with a stall | 4/7 | **0/17** over all v59 download runs in the table above (5 + 4 + 4 + 1 + 1 + 1 + 1; Fisher two-sided p = 0.003, one-sided the same) |
| upload, hub → server, 30 s | 74–82 | **108** (3 runs) |
| backup mode, Vodafone only, 30 s | 43, 67 | **100, 104** |
| ping of another flow under full load, p50 / p90 / p99 / max | 32 / 48 / 64 / 69 ms at 82 Mbit/s | 42 / 67 / 99 / 126 ms at 171 Mbit/s (idle 31 ms for both) |

- v59 removed the stalls on LTE as well; its link transmit queues stayed ≤ 45 kB, v57's held hundreds of kB.
- With the 256 KB flow queue v59 was ~20 % *slower* than v57: the flow queue's drops held the TCP flow at a cwnd of
  ~280 packets. With 1 MB the cwnd reached 1400–2400 packets and the rate doubled. A 1 MB queue only works with the v59
  caps: v57 with it stalled for up to 8.8 s.
- The price is delay for other traffic under full load (ping p99 64 → 99 ms), in the base stations' queues of fuller
  links.
- `-pacepick` was not decided (256 KB: off faster; 1 MB: on faster in one run). It stays on.
- Not repeated in the evening peak.

---

## Single RBM33G box

Box 1, O2 + Vodafone, IPsec (EIP93), a computer on the box's LAN cable, one TCP flow down from the server through
the tunnel unless stated, 15–30 s.

### The tunnel is CPU-bound

| measurement (26 November) | result |
|---|---|
| tunnel, v57 / v59, ABBA, 30 s | 18.9, 20.3 / 20.2, 18.5 Mbit/s; v59 changed nothing here |
| raw links in the same minutes, 10 s | O2 53.5 + Vodafone 70.9 = ~124 Mbit/s (the tunnel got 16 % of it) |
| proxy, then tunnel | 65.9 / 20.3; proxy 59–69 in other runs |
| backup mode, Vodafone only | tunnel 19.6, proxy 43.9 |
| without IPsec (frame 1400) | tunnel 22.9, proxy 74.9 |
| from the box itself through the tunnel (no LAN, no NAT) | 20.4 |
| box `-maxq` 100 / 40 / 20 | 18.3 / 17.8 / ~19 |
| CPU at 20 Mbit/s through the tunnel | ~75 % busy (sheaf 46 %, softirq 20 %) |
| CPU profile of sheaf: per downloaded packet | tunnel ~720 µs (TUN write ~190 µs, the ACK path up ~165 µs), proxy ~190 µs per frame |

The tunnel had a fixed ceiling of ~20 Mbit/s independent of the number of links and of IPsec. The links, the
encryption, the box's forwarding and NAT, `-maxq` and v57/v59 were ruled out; the per-packet CPU cost of the TUN path
on the MIPS remained (moving interrupts and RPS to other cores was ~8 % worse, `GOMAXPROCS=4` worse). The server's
small windows were a consequence: datagrams waiting in the box's sockets look like link queueing. The "84 %" of
24 November was through the proxy; the tunnel was probably always this slow on the RBM33G.

### Gains on the box

| change | A/B result (Mbit/s, one TCP flow) |
|---|---|
| **transparent TCP proxy** (`TPROXY=1`), a real 1 GB download from a public internet server, 7 runs ABAB | tunnel 18.6, 17.3, 17.1 · proxy 48.3, 37.7, 47.7, 48.0 (**~2.7×**) |
| **`-tungro`** (v61), ABBA | 18.6 / 22.8 / 22.1 / 17.9: **+22 %**, ~2.3 segments per TUN write |
| **flowtable** LAN ↔ `shf0` (`FLOWTABLE=1`, package 61-r4), ABBA 4 + 4 runs | without 23.2, 16.0, 18.4, 25.1 · with 36.2, 31.1, 29.6, 29.8: **31.7 vs 20.7 (+53 %)**, every run with above every run without; all six runs without that evening (with 23.3 and 22.6 before the ABBA block) average 21.4 (+48 %) |
| **`-auth=half`** (v62), ABBA, tunnel / proxy (means of 2 runs) | SipHash 33.7 / 46.8 · **HalfSipHash 41.2 / 47.2 (+22 % tunnel)** · no tag 42.9 / 48.1 |
| throttled O2 (256 kbit/s down) beside Vodafone, download through the proxy, v61 `-slowfrac 0.03` vs v59 behaviour | 37.5, 38.6 vs 27.1, 21.5: **+55 %** (log: `L1 slow: demoted (0.20 Mbit/s vs 60.9 …)`) |

Final state (v64, packages 64-r1; v65 / 65-r1 has no behaviour change,
[27 November](#27-november-final-version-on-the-box-v65)), evening: tunnel down **38–41 Mbit/s** (40.9, steady 44.6, with
package 61-r4), up 16–29 (radio), transparent proxy **45–48**, ping 24–45 ms. A dead link costs ~125 B/s.

Did not help: fw4's flow offloading (it does not cover a TUN device: 17.5 Mbit/s, no change), interrupt and RPS
placement, more Go threads.

---

## The CPU limit, proven: the R4 as the brain

26 November evening (peak hour), the same O2 and Vodafone SIMs: box 1 switched to the satellite role (it only dials its
modems and forwards), the BPI-R4 runs sheaf and IPsec, a computer behind the R4, one TCP flow through the tunnel,
15 s, R4 v59 `-maxq 40 -fqlimit 1048576`.

| what | Mbit/s |
|---|---|
| raw links from the R4 through the satellite, without sheaf and IPsec: Vodafone / O2 alone | 79 / 27 |
| raw links, both at the same time | 27 + 62 = 89 |
| sheaf, speed mode, both links | 74, 73, **82 (steady 95)** = 83 / 82 / 92 % of the raw simultaneous sum; upload 60 |
| sheaf, backup mode, one link: O2 / Vodafone | 27 / 55, 41 |
| the RBM33G as a satellite at 80 Mbit/s over both links | CPU 25 % in total, busiest core 50 %: forwarding is cheap |

The R4 with sheaf limited by cgroup `cpu.max`:

| CPU allowed | Mbit/s | sheaf actually used |
|---|---|---|
| no limit | 68 / 102 | 41 / 60 % of one core (the radio is the limit) |
| 50 % of a core | 80 | 43 % |
| 25 % of a core | 46 | 24 % |
| **12.5 % of a core** | **24** | 12 % |

About 1.9 Mbit/s per 1 % of one Cortex-A73 core; 12.5 % of an A73 is a whole RBM33G (17–23 Mbit/s through the tunnel
before the box-side optimisations). Per packet: A73 ~70 µs, MT7621 ~720 µs. With an unthrottled R4 the bond reached
the best single link (82 against 79 in the best run, within the run-to-run spread; 102 in one `cpu.max` baseline run);
the RBM33G alone never came close. Over the measured R4-as-brain runs in speed mode the rate was 68–102 Mbit/s (two
earlier runs while the setup was being brought up gave 71 and 54).

---

## IPsec cost

| what | result |
|---|---|
| ESP on the box, TCP between two namespaces over veth (the box encrypts and decrypts) | no ESP 301 · ESP AES-256-SHA-256 in software **17.6** · EIP93 without the ESP HMAC 90.8 · **EIP93 with the fixed driver 60.6** Mbit/s |
| the same on an x86 UP Squared (AES-NI, SHA-NI) | **489** Mbit/s (no encryption 7 Gbit/s) |
| sheaf over LTE, frame 1400 → 1328 (no IPsec / IPsec), down | 69.9 / 45.8 → 88.6 / **70.2** (−34 % → −21 %) |
| the same, up | 64.4 / 34.1 → 63.4 / **50.1** (−47 % → −21 %) |
| AES-128 instead of AES-256 | no cheaper (−26 / −21 % vs −27 / −21 %): the cost is per packet (ESP-in-UDP, engine, interrupts), not the cipher |
| daytime rounds, through the proxy | with IPsec 44–54 % of the plain sum down, 72–90 % up; without 57–73 % / 93–100 % |
| tunnel on box 1, IPsec on / off | 20 / 22.9 Mbit/s: ~13 % less with IPsec, not the tunnel's limit |

On OpenWrt 25.12 (kernel 6.12) every ESP state with authentication offloaded to the EIP93 failed with `EINVAL`; the
one-line driver fix is in `openwrt/patches/` and was submitted upstream

([openwrt/openwrt#26910](https://github.com/openwrt/openwrt/pull/26910)). With 1400-byte frames the ESP packets were
fragmented; 1328-byte frames keep ESP-in-UDP within 1500 bytes.

---

## Reliability tests T1–T16

Planned from a code review of the box (17 risks), run on box 1 on 26 November, partly with the server side.

| test | what was done | result |
|---|---|---|
| T1 | sheaf killed during a download through the tunnel | **passed** (package 59-r2): restarted after 5 s, the 60 s download survived, content ok, ~4.5 s of ping lost. Before: the LAN's TCP got RST |
| T2 | O2 radio off for 15 s, back with a new address | **passed**: Vodafone's IKE/ESP untouched (same SPI), ping through the tunnel 300/300; O2 back after ~30 s. Before: IKE of both links renegotiated |
| PSK test | PSK removed + charon killed, PSK back after 2.5 min | **first a fault**: the tunnel would have stayed down forever. With the recovery ladder (59-r3): change noticed within 9 s, both links back in 21 s, no manual step |
| T4 | zombie modem: O2 keeps its address, UDP 4500 dropped for 7 min | **passed** (61-r1): 62 s IKE restart · 191 s hang up and redial · 369 s modem reset (new address) · back 15 s after the block was lifted; Vodafone untouched |
| T5 | the server loses its IKE state (terminated, the DELETE dropped) | **first 86 s** without tunnel (the ladder did not touch an ESTABLISHED SA); after the fix (61-r2) **~55 s** (DPD alone would take ~165 s); other server connections untouched |
| T6 | throttled SIM: O2 limited to 256 kbit/s down | **passed** with v61: 37.5 / 38.6 vs 27.1 / 21.5 Mbit/s without the demotion (+55 %), back at once when the limit was lifted |
| T8 | rekey: CHILD of O2, IKE of Vodafone, both, reauthentication | **passed**: ping 0/500 lost, max 53 ms, sheaf noticed nothing |
| T9 | sheaf restarted on the server | **passed** for the tunnel (ping 1/450, SSH through the tunnel survived); TCP through the proxy ends (its state is in the server process) |
| T10 | power cut; start without one SIM; SIM inserted while running | **passed** (hardware caveat: inserting a SIM while running resets the box): tunnel back **~95 s** after the cut; without the O2 SIM it runs on Vodafone and retries O2 with a growing back-off; inserting a SIM while running **resets the box** (hardware), back with both links in ~80 s: change SIMs only powered off |
| T11 | stuck socket: O2 throttled to 8 kbit/s with a huge queue for 3 min | **passed**: ping 0 lost, a proxy download ran 200 s at a steady 1 MB/s, O2 taken out, no needless IKE restart |
| T15 | overnight soak | **passed**, see below |
| T16 | sheaf frozen (`kill -STOP`) | **first a fault**: the status page showed old data, the new sheaf failed on "address already in use" in a loop. After the fix (61-r3: KILL after 5 s, hung after 15 s) **~27 s** outage, recovers by itself |

Also checked: the status page marks a stale status (> 5 s) as not responding and pairs its cards by link id (one
missing modem no longer swaps them); removing the mode file returns the default mode; the server's firewall accepts
the box's sheaf ports only under IPsec, set without interrupting the tunnel.

**Not run:** T3 (modem re-enumerated on USB; covered only by a lab test of the supervisor with fake interfaces), T7
(natural end of a data allowance), T12 (memory during a long repair hole; covered by `GOMEMLIMIT` and the soak's flat
RSS), T13 (CPU and MTU counters under bidirectional load), and from T14 only the v57/v59 A/B (not the `-maxq` 20/40
comparison).

---

## Overnight soak and idle data use

T15, v64, 26 November 20:50 → 27 November 06:20 UTC (9.5 h), the box idle (no LAN traffic), a sample every 10 min (58):

- the same sheaf process all night (no restart), the box not rebooted, both IPsec SAs up all the time, the CHILD SAs
  rekeyed every hour;
- sheaf's RSS 12.9–13.1 MB all night (peak 18 MB from the afternoon's tests): **no leak**; free RAM 154–158 MB;
- charon +168 kB in 9.5 h (in steps at rekeys), ~0.4 MB per day;
- no link died on the box's side all night;
- **idle data: O2 +3 MB down / +3 MB up, Vodafone +3 / +3 in 9.5 h = ~15 MB per day per SIM** (feedback every 2 s,
  IPsec, DPD);
- the server instance declared O2 dead six times for an instant after a single lost packet with nothing else in
  flight, back in the same second; no effect (nothing was flowing).

A dead link with quiet probing (v62–v64) costs ~125 B/s (~10 MB per day); before v62 a dead link got a full-size copy
every 500 ms and feedback every 20 ms (estimated 100+ MB per day; ~12 kB/s measured in T11).

---

## 27 November: final version on the box (v65)

Box 1 (RBM33G, O2 + Vodafone, IPsec) with sheaf v65, packages 65-r1.

- **T1 on v65:** the sheaf core killed with `kill -9`: both links back after 8 s, ping through the tunnel without loss.
- **Daytime (08:31), one TCP flow down, 15 s, three rounds:** tunnel 36.5 / 31.9 / 34.5 Mbit/s (steady 41.1 / 35.7 /
  36.2), transparent proxy 47.6 / 47.2 / 48.0: about the level of v64 (tunnel 38–41, proxy 45–48 the evening
  before, a different time of day); v65 changes no behaviour.
- **Modes, a UDP stream of 8 Mbit/s for 30 s down to the box** (`udpstream`):

  | test | loss | late p99 | late max |
  |---|---|---|---|
  | speed, beside a bulk download | 0 % | 109 ms | 178 ms |
  | smooth, beside a bulk download | 0 % | 107 ms | 151 ms |
  | redundant, O2 cut both ways for 10 s during the stream (3 runs) | 0 / 0.05 / 0 % | 542 / 129 / 268 ms | 589 / 307 / 358 ms |
  | speed, O2 cut the same way (3 runs) | 0 / 0 / 1.26 % | 219 / 396 / 1007 ms | 303 / 459 / 1052 ms |
  | backup, the primary link (O2) cut the same way | 0 % | 600 ms | 684 ms (longest silence 301 ms) |

  The cut: from second 10 of the stream, an nft table on the box dropped everything in and out of O2's interface for
  10 s (the modem itself untouched); redundant and speed ran ABAB after their first run. The tunnel survives a modem
  loss in every mode. Redundant is the steady one; speed occasionally stalls for ~1 s and then loses ~1 % of a live UDP
  stream. Smooth is now about equal to speed, because speed itself no longer stalls (v59).
- **Known limitation found: a link failing in one direction only.** With the box → server direction of O2 dropped for
  60 s and server → box working, the server declares the link dead after 8 s without feedback and back within a second
  (its probes are acknowledged over the other link), repeating every 8 s; the box notices nothing while idle. The
  state of a link is judged per link, not per direction. Not fixed.

---

## Not verified, known limitations

- **QMI and MBIM modems** are implemented and reviewed but were never run on hardware; all measurements used Huawei
  ME909s-120 modems, which are no longer made.
- **No drive test.** Everything on LTE was stationary, indoors, in one place; the "drive" exists only in the lab.
- **Evening peak** not repeated for the v59 comparison; LTE numbers depend on the time of day.
- **One TCP flow** is the measure throughout; many parallel flows were tested only in the proxy lab (100 connections).
- **Losing a hub satellite** restarts sheaf and reloads IPsec of all links today: a short outage of the whole tunnel.
- **Delay under full load** rose with the 1 MB flow queue (ping p99 64 → 99 ms at twice the rate); a smaller server
  `-maxq` was not measured.
- `-pacepick` with the 1 MB flow queue and `-maxq` 20 vs 40 on the box were not decided with enough runs.
- Lab uploads still show short stalls in 2 of 5 runs under rate collapses.
- The server instance may declare an idle link dead for an instant after one lost packet (harmless, not fixed).
- Proxied TCP connections end with a restart of sheaf on either end; tunnel connections survive.
- BLOCK_QUIC's effect on real applications was not measured (the test client had no HTTP/3).
- **A link that fails in one direction only** (box → server lost, server → box working) is not detected as such: the
  server flaps it dead/back every 8 s and the box notices nothing while idle; link state is per link, not per direction.
