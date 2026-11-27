# How sheaf works

**In plain words.** The box has two modems. sheaf sends every packet over the modem where it will arrive first, so
one download or upload can use both modems at once. Every packet gets a number. The server puts the packets back in
order, notices exactly which ones went missing and asks for them again, and the box resends them over whichever modem is
best at that moment. Each modem's speed and delay are measured continuously, so neither modem gets more than it can
carry and the delay stays low. Every modem's traffic is encrypted with IPsec. The mode decides what matters most:
raw speed, smooth live streams, invisible failover, or saving data on one SIM.

The rest of this page explains each mechanism in detail, as of sheaf v65 (= v64 with no behaviour
change). The packet format is in
[PROTOCOL.md](PROTOCOL.md), running it in [USAGE.md](USAGE.md), all measurements and their evaluation in
[RESULTS.md](RESULTS.md), and the overview in the [README](../README.md).

## Contents

1. [Architecture](#architecture)
2. [Mechanisms](#mechanisms)
   - [Links](#links) · [Flow queue](#flow-queue-between-the-tun-device-and-the-sender) · [Scheduler](#scheduler-earliest-arrival) ·
     [Per-link window regulator](#per-link-window-regulator) · [Pacing](#pacing) ·
     [Reliability and repair](#reliability-and-repair) · [Receiver: per-flow order](#receiver-per-flow-order-and-packet-classes) ·
     [Modes](#modes) · [Failure detection](#failure-detection) · [No fragmentation across links](#no-fragmentation-across-links) ·
     [TCP proxy and transparent proxy](#tcp-proxy-option) · [Security](#security) · [CPU on a small MIPS box](#cpu-on-a-small-mips-box)
3. [Measured results](#measured-results)
4. [Limitations and open ends](#limitations-and-open-ends)

## Architecture

```
 LAN clients                BOX (MikroTik RBM33G, OpenWrt)                       SERVER (Linux, public IPv4)
 (laptop, camera,
  phone via Wi-Fi)          ┌───────────────────────────────┐                    ┌───────────────────────────────┐
        │                   │ default route → shf0 (TUN)    │                    │          shf0 (TUN)           │
        │  192.168.1.0/24   │ 10.66.0.2/30, NAT for the LAN │                    │          10.66.0.1/30         │
        └──────────────────►│               │               │                    │               │               │
                            │             sheaf             │                    │             sheaf             │
                            │ flow queue, scheduler, repair │                    │  reorder, repair, -mode peer  │
                            │   mode ← /etc/sheaf/mode      │                    │      -pace 1.25               │
                            │      │                 │      │                    │      │                 │      │
                            │  UDP :7001       UDP :7002    │                    │ UDP :47001      UDP :47002    │
                            │  ┌───┴─────┐     ┌─────┴───┐  │                    │  ┌───┴─────┐     ┌─────┴───┐  │
                            │  │IPsec ESP│     │IPsec ESP│  │                    │  │IPsec ESP│     │IPsec ESP│  │
                            │  │ (EIP93) │     │ (EIP93) │  │                    │  └───┬─────┘     └─────┬───┘  │
                            │  └───┬─────┘     └─────┬───┘  │                    │      └────────┬────────┘      │
                            │   wwan0             wwan1     │                    │             eth0 ── NAT ──────┼──► internet
                            └─────┼─────────────────┼───────┘                    └───────────────┼───────────────┘
                                  │ modem 1 (O2)    │ modem 2 (Vodafone)                         │
                                  └─── LTE + CGNAT ─┴──── ESP-in-UDP (port 4500) ── internet ────┘
```

- On the box, LAN clients use the box as their gateway. The box's default route points into the TUN device `shf0`, and
  the firewall zone `sheaf` masquerades the LAN behind `10.66.0.2`. sheaf reads IP packets from `shf0` into a flow
  queue, schedules each one onto a link, and writes what it receives back into `shf0`.
- Each link is a UDP socket bound to one modem interface (`SO_BINDTODEVICE`, `@wwan0:7001`). Policy routing (one
  routing table per modem, `ip rule from <modem address>`) makes sure it leaves through that modem, and the address may
  change on every redial.
- **IPsec sits under sheaf, per link.** One IKEv2 SA per modem, ESP in transport mode, UDP-encapsulated (NAT-T, the
  modems are behind carrier-grade NAT), with traffic selectors `udp/7001 ↔ udp/47001` and `udp/7002 ↔ udp/47002`. The
  policies are installed as *trap* policies: nothing of sheaf's traffic can leave unencrypted, and a dropped SA is
  renegotiated on the next packet. ESP runs in the EIP93 engine of the MT7621.
- On the server, sheaf listens on one UDP port per link and learns the box's current addresses from authenticated
  datagrams. It writes the box's packets into its own `shf0` (`10.66.0.1`), and the server's firewall forwards and
  masquerades the tunnel addresses (`10.66.0.0/24`, one /30 per server instance) to the internet. To the outside
  world, every device behind the box appears with the server's public address.
- Control and measurement: a status page on the box (`/sheaf.html`, read only), a LuCI page to choose the mode, a JSON
  status file on both ends, and optional 100 ms CSV traces.

---

## Mechanisms

### Links

A link is a pair of UDP sockets, one on the box and one on the server, given on each end by `-links`. Since v55 the
two ends may list a different number of links in any order: each link learns the far end's number of its socket pair
from the packets it receives, and the two ends pair links by that number ([PROTOCOL.md 5.4](PROTOCOL.md#54-link-pairing-v55)):

```
box:     -links "@wwan0:7001>SERVER:47001,@wwan1:7002>SERVER:47002"
server:  -links "0.0.0.0:47001>-,0.0.0.0:47002>-"
```

`@dev:port` binds to a network device on any address, because a modem's address changes with every reconnect. `>-`
means "peer address unknown": the server learns it, and relearns it after carrier-grade NAT rebinding or a new modem
address, from any datagram that passes the authentication check.

On every link, the sender keeps for each DATA packet: its `linkSeq`, size, send time and global `seq`, plus a
reference to the inner bytes. This record gives bytes in flight, RTT samples, the `linkSeq → seq` map for repairs, and
the list of packets to move elsewhere if the link dies.

### Flow queue between the TUN device and the sender

The sender stops whenever every link's window is full. Until v52 the backlog then built up in the TUN device's own
ring (500 packets): one FIFO shared by all traffic, tail-dropped by the kernel. The kernel's fq_codel never got to act,
because a TUN device drops packets instead of stopping its queue. A camera stream sharing the box with a bulk upload
waited ~240 ms (p99) in that FIFO, and lost packets with it.

Since v53 (`fq.go`) a reader goroutine takes packets off the TUN device as fast as they come and files them into 1024
per-flow queues. A flow is identified by a hash of the addresses, the protocol and the ports. The sender takes packets
**deficit round robin** with a quantum of one full-size packet. A flow that had nothing queued goes first ("sparse"
flows, the same idea as fq_codel's new flows). A stream that stays below its fair share, such as a call, a camera,
DNS or a TCP handshake, therefore never waits behind a bulk transfer. When more than `-fqlimit` bytes are queued
(1 MB by default, 256 KB before the v59 deployment), the **fattest flow loses its oldest packet**, so only the bulk
transfer slows down, and TCP notices the loss as it should.

Measured through the tunnel over real LTE: a camera-like 8 Mbit/s UDP stream uploaded beside a bulk upload, one-way
delay p50 80 → 41 ms, p99 238 → 122 ms, loss 0.2 % → 0. `-fqlimit 0` restores the old behaviour (the sender reads the
TUN device directly).

**Why 1 MB.** The flow queue is the buffer that absorbs the bursts of a TCP flow (CUBIC without pacing, bursts after
batches of ACKs). Up to v57 the links' own transmit queues absorbed them, behind the scheduling decision, and that is
where packets got stuck behind a slow link ([Pacing](#pacing)). Since v59 those queues are capped at one GSO batch
(`-txqcap`), so the bursts overflowed the 256 KB flow queue instead: its drops held one TCP flow at a cwnd of ~280
packets, and the links were not full (srtt 35 ms at a base of 30). The buffer belongs **in front of** the link
decision: a packet in the flow queue has no link yet and cannot get stuck behind a slow one. With 1 MB the cwnd
reached 1400–2400 packets and one TCP flow over four LTE links ran 158–188 (1–2 MB) instead of 59–77 Mbit/s, without stalls
([measurements](RESULTS.md#bpi-r4-hub-four-lte-links)). A large flow queue only works together with the v59
caps: v57 with 1 MB stalled for up to 8.8 s, because its larger cwnd only filled the queues behind the decision. The
cost is delay for other traffic under full load (ping p50 / p99 32 / 64 → 42 / 99 ms), which waits in the base
stations' queues of links that are now fuller, not in the flow queue, where a new flow goes first.

### Scheduler: earliest arrival

`pick()` (in `main.go`) runs for every packet. Among the links that are alive, have a known peer address and have
room in their window, it chooses the one where the packet is expected to **arrive first**:

```
expected arrival(link) = relOwd(link) + (inflight(link) + size) × minRtt(link) / window(link)
```

- `relOwd` is the link's one-way delay relative to the fastest link, reported by the receiver in every feedback.
- The second term is the time to drain what is already queued on the link at the link's current pace (one window per
  base RTT).

A fast, empty link wins. A slower link gets traffic once the fast one has queued enough that the slow one would still
deliver earlier. When every window is full, the sender waits (500 µs steps) for feedback: that is real saturation.
`pick()` uses integer arithmetic only, because floating point is emulated in software on the box's MIPS CPU.

A link that has not been sent on for 500 ms gets the next packet anyway, as a probe that keeps its measurements fresh.
A dead link only ever gets a copy, as described under [Failure detection](#failure-detection).

### Per-link window regulator

Every link has a window: the bytes it may have in flight. The window is regulated from the link's own measurements and
is independent of the other links. The design goal is to use an LTE link's capacity **without** filling its huge
buffers. LTE downlink queues of 0.5 to 0.7 s were measured, and loss appears only when they are full, which is far too
late as a signal.

Each feedback that acknowledges the newest packet on a link gives an RTT sample: `now − sendTime − age`, all on the
sender's own clock. From these samples the regulator keeps:

| estimate | how |
|---|---|
| `srtt` | EWMA 1/8 of the samples |
| `minRtt` (base) | the lowest `srtt` seen, re-measured every ~10 s (`-drainevery`) by a short **drain**: one link at a time lowers its window until its queue is empty, then the base comes from ≥ 4 fresh samples (at most 1.5 s). Since v59 (`-drainfix`) a link drains only while its transmit queue is empty and it is not stuck, and an srtt above its healthy round trip postpones the drain (at most to 3 × the interval: after a cell change the base itself may have risen); the drain window is max(6 kB, ½ × rateAvg × base) instead of 6 kB; the base is the mean of the lower half of the samples (the plain mean could lift it); afterwards the window returns ×2 per round trip instead of in one line-rate step. If `srtt` stays above the growth band for 700 ms while the window is already minimal, the path itself got slower (cell change) and the base is moved up |
| jitter `J` | EWMA 1/16 of half the absolute second difference of consecutive samples. White jitter gives about σ, while a queue that grows or drains steadily adds nothing. Each sample is clipped to 4 J + 2 ms |
| delivery rate | bytes acknowledged per ≥ 100 ms; `rateAvg` (EWMA 1/4) and `rateMax` (max over the last ~1–2 s) |
| loss | packets NACKed / packets sent on this link, per window of max(srtt, 100 ms), with ≥ 50 packets |

**Bands.** Growth is allowed while the delay is within `minRtt + incEx`, and a cut happens above `minRtt + decEx`, where

```
j     = min(jitk × J, 50 ms)                 (jitk = 3 by default)
incEx = max(inc × minRtt, j)                 (inc  = 0.5)
decEx = max(dec × minRtt, 2 j)               (dec  = 1.5)
```

Between the two bands, the window is left alone, so ordinary LTE jitter does not make it oscillate. With pure ratios, a
jittery link (17 ms ± 12 ms one way in the lab) kept its window far below its bandwidth-delay product, because the noise
alone kept `srtt` above the growth limit.

**Decisions**, at most one per real round trip (only a sample of a packet sent *after* the last change can show the
effect of that change), and only when the fresh sample agrees with `srtt`:

| condition | action |
|---|---|
| loss > 3 % (`-losscut`) | window × 0.8, but not below `rateAvg × minRtt`; the learned queue cap × 0.8 (min 20 ms) |
| loss > 1 % (`-losshold`) | hold, no growth |
| delay ≤ base + incEx/2 and the window is in use | window × 2 |
| delay ≤ base + incEx and the window is in use | window × 1.25 |
| delay > base + decEx | window × 0.7, but not below `rateAvg × minRtt` (the queue is ours; a slower cell lowers `rateAvg` within ~0.5 s) |

"In use" means that at least half the window was in flight when the feedback arrived, or the window is tiny. A window
that the scheduler does not fill is not grown.

**Cap: the learned queue.** The window never exceeds `max(24 kB, rateMax × (minRtt + maxQ))`. `maxQ` is the queue this
link may hold on top of its base delay. It starts at 100 ms (`-maxq`), shrinks × 0.8 with every loss cut (min 20 ms)
and grows back by 5 ms per second without one (`-maxqadapt`). A busy daytime cell drops whatever exceeds its share,
often without any rise in delay (a policer). The learned cap brought daytime loss on Vodafone from 15–17 % down to
about 5 % at almost the same speed.

Limits: window 6 kB to 4 MB, initial 64 kB. A link idle for more than 1 s halves its window each second (not below
64 kB), because a full old window would otherwise leave as one burst. A new session of the peer, after a restart of
either end, starts every link's regulator from scratch.

### Pacing

Without pacing, a window freed by one feedback (20 ms worth of data) leaves the sender as a single burst at line rate.
From the server's 1 Gbit/s port, such bursts arrived at an LTE downlink at 1 Gbit/s, and the operator's network dropped
up to ~20 % of them at ~40 Mbit/s, on a link that carries 50–60 Mbit/s of evenly spaced UDP without loss.

`-pace G` paces each link with a token bucket. A burst is limited to about 2 ms of data at the link's rate, and never
less than 6 packets. The server runs `-pace 1.25`. Measured (v51) on Vodafone alone (backup mode, 100 MB downloads,
alternating runs): without pacing 35.0 Mbit/s on average, with `-pace 1.25` 41.4 Mbit/s (+18 %) at 3–5 % loss. Gains
of 1.1 and 1.5 were worse, and so was pacing combined with a fixed 100 ms queue cap. On the box, pacing is off: its
upload is limited by its CPU, not by bursts.

**Nothing that can be slowed down waits behind the scheduler (v59).** Up to v57 the pacer sat in each link's send
loop, behind `pick()`, and took its rate from the link's *current* window (G × window / srtt, at least 1 Mbit/s), also
for packets queued under a larger window. When a drain or the window cap then cut the window while hundreds of kB sat
in the link's transmit queue, they left at the 1 Mbit/s floor, two packets every 21.8 ms, and the whole bundle waited
head-of-line behind them. This caused the multi-second tunnel stalls seen on real LTE: 92 % of the 100 ms trace
windows of a stuck link held an even packet count (52 % elsewhere), the fingerprint of that floor, not of the radio.
Since v59 (`-pacepick`) the pace is a condition of the scheduler: a link is a candidate only when its tokens allow the
packet now and its transmit queue holds less than `-txqcap` (45 kB, about one GSO batch; an empty queue always takes a
packet), and the send loop sends what it gets at once. The rate is G × max(rateMax, window / srtt): a window cut never
lowers it below what the link delivered in the last 1–2 s (no learning from our own throttling), and the window term
lets it grow ahead of the delivery samples after a restart. Redundant mode checks only the transmit queue (a copy may
overdraw the pace). `-pacelook D` keeps a link whose tokens come within D a candidate, sent on credit; it is off,
because in the lab 2 ms cost ~3 of 55 Mbit/s.

In the lab (one TCP flow over three emulated links, a drain every 2 s and short rate dips), v57 stalled in 6/6 runs with
the same 90 % even-window fingerprint, v59 in 0/6 (Fisher two-sided p = 0.002), 41.4 → 55.5 Mbit/s, TCP rtt max 1.2 → 0.37 s.
The link transmit queue peaked at 16–27 kB with v59 and 240–407 kB with the old pacer. Steady links went 55.9 → 64.7
Mbit/s. With a shallow (150-packet) bottleneck queue pacing is still needed (without it 4–5 % loss and −17 %), and v59
stays within 2 % of v57 there.

### Reliability and repair

The box and the server both run with `-reliable`. In this mode the receiver never skips a hole, and the sender keeps a
copy of every packet until the peer's cumulative ack has passed it (at most 65 536 packets; beyond that, sheaf stops
reading the TUN device, which back-pressures the applications).

A lost packet is repaired by the first of four mechanisms that notices:

| mechanism | trigger | what the sender does |
|---|---|---|
| **NACK-RANGE** | the receiver sees a jump in a link's `linkSeq`. A link is FIFO, so this names exactly the packets that were lost (also the very first packets of a session: links number from 1) | maps each `linkSeq` to its `seq`, counts the loss for that link's regulator, resends the copy |
| **MISSING report** | the oldest packet in the reorder buffer has waited 200 ms (for example, the NACK itself was lost in the same burst); repeated every 100 ms | resends each listed `seq` whose last transmission has passed its repair time (below), so a packet that is merely late is not sent twice |
| **stuck link** (v58) | the link's oldest unacknowledged packet is older than its healthy round trip (base + allowed queue + jitter + feedback interval, × `-slowk`) and its queue does not drain within twice that at its delivery rate | the link gets no new data; with `-rescue`, the packets it would deliver later than twice its healthy round trip go once more over the other links, only while another link is acking |
| **dead link** | no ack progress on a link with data in flight for `deadAfter` (250 ms to 1 s) | moves the link's whole in-flight list to the live links |
| **RTO** | the head-of-line packet has not been acknowledged for max(1 s, its repair time) | resends only that one packet (go-back-N wasted ~3 %); back-off is capped at 2 × because long back-offs turned into multi-second stalls |

A repair is a normal DATA packet with the **same `seq`** and a new `linkSeq`. It goes through the same scheduler as new
data, before any new data (`drainRepairs`), so it takes the link with the earliest expected arrival at that moment, which
is usually not the link that just lost it. The receiver drops whichever copy arrives second.

**Repair time fixed at send (v59, `-fixguard`).** Until v57 every repair path asked "has this packet had time to
arrive" with the slowest live link's srtt *now* (NACK guard 1.5 ×, RTO 3 ×). The srtt of a link whose rate collapsed
grows with its own queue, to seconds, so it protected exactly the packets stuck in that queue from repair, for any
cause of the collapse. Now each transmission gets its repair time when it leaves, from the link it left on:
send time + max(srtt, base + q) + q + 4 × jitter + the feedback interval, where q is the queue the window cap allows
(the second q because the queue may still grow after the packet left: 208–226 ms were seen on LTE under the cap). A
collapse after the packet left does not stretch it, and a deep but legitimate queue (already in the srtt) does not
make copies. The MISSING clock is the age of the oldest buffered packet (v58): before, every packet a collapsed link
trickled in reset it, and no report was ever sent.

**Bounded receiver wait (v59, `-iprelease B`, off by default).** A reliable receiver never skips a hole. With
`-iprelease`, a buffered IP packet sent at *s* (sender clock) is released past the hole at *s* + the largest one-way
delay of the links delivering now (each counted at most as its minimum + `-ipreleasecap`, 300 ms, since a collapsed
link's delay grows without bound) + 4 × its jitter + B. The hole is still repaired and delivered late, so TCP sees
reordering instead of a multi-second stall; proxy frames never overtake. In the lab it never fired while the repairs
worked; with the v57 repairs it cut the TCP rtt max from 3.1 to 2.0 s but cost 10 % of the rate (fast retransmits
and cwnd cuts on the reordering), so it stays off.

Without `-reliable` (lab only), the receiver skips a hole after `holdTime`, and a packet that arrives after its hole
was skipped is still delivered late, since reordering hurts TCP less than loss.

### Receiver: per-flow order and packet classes

Order matters **within** a flow (a TCP connection, a QUIC connection, an RTP stream). It does not matter between flows.
Up to v52 the receiver delivered strictly in global `seq` order, so one lost packet of a bulk download held back a call,
a camera stream and every other connection until its repair arrived.

**Per-flow release (v53, `flowrel.go`).** The sender remembers, for every flow (the same hash as the flow queue), the
global `seq` of that flow's previous packet. It writes this **flow predecessor** into bytes 20..23 of every DATA header
and sets flag `0x01` in byte 3. The first packet of a flow names itself. The receiver delivers a packet as soon as its
flow predecessor has gone out (in order, or early itself). Otherwise the packet waits, registered as the successor of
its predecessor, and goes out the moment that predecessor does, followed by the rest of the chain. A hole in one flow
now delays only that flow. A hash collision merges two flows into one stricter chain, which is still correct. Proxy
frames carry no predecessor and keep the strict order. A peer older than v53 sends byte 3 = 0 and gets strict
order, and an older receiver ignores the field. `-flowrel=false` switches it off. See
[PROTOCOL.md §4](PROTOCOL.md#4-type-1-data).

Measured over real LTE, downloading a camera-like 8 Mbit/s UDP stream beside a bulk download in smooth mode: p50 151
(v52) → 56 ms, and the share of packets more than 100 ms late 83 % → 9 %.

Two more classes may overtake a hole. They are recognised by the receiver itself and need no field on the wire:

- **noDelay**: a pure TCP acknowledgement (IPv4, ACK set, no payload, no SYN/FIN/RST) and the proxy's credit frame
  (WIN). They only report progress, and holding them behind a lost data packet would slow down the opposite
  direction. The packet is delivered immediately and its slot is marked, so it is not delivered again.
- **live UDP** (smooth mode only): see below.

On the sending side, **dupWorthy** packets are small (≤ 512 B) and important: the proxy's control frames, DNS, ICMP and
TCP SYN/FIN/RST. In every mode they are also sent on a second live link, and the first copy wins. Losing a SYN or a
DNS query costs a second or more of user-visible delay, while the copies cost almost nothing. Plain TCP ACKs are not
duplicated: there are thousands per second during a download, and each costs CPU on the box.

### Modes

| mode | what it does | when to use it | cost |
|---|---|---|---|
| **speed** (default) | spreads packets over all links by earliest arrival, repairs losses, delivers each flow in order (v53; strictly in order before); TCP ACKs overtake holes | internet use, downloads, the web | after a loss or a link failure, the affected flow waits a short time for the repair |
| **smooth** | as speed (including per-flow release), and in addition a **UDP** packet waiting for a hole in its *own* flow waits at most `holdTime` = 1.5 × (spread of the links' one-way delays) + 40 ms, clamped to 20–400 ms, and is then delivered ahead of the hole. TCP stays in order | live audio and video, calls, cameras: a missing piece is better than a freeze | the application occasionally sees a gap or a reordered packet; the late repair is not delivered twice |
| **redundant** | every packet goes as a copy over every live link that has window room, and the first copy to arrive wins. From v52, a packet waits only while *no* link has room (optionally up to `-redwait` for a full link). Up to v51, every packet waited until all live links had room, which queued a live stream behind the slower window | control links, telemetry, critical calls: a modem failure is invisible | twice the data volume and twice the work for the box; when traffic exceeds the slower link, that link carries only the copies it has room for |
| **backup** | only the primary link (`-primary`, L2 on the test box) carries data; the others take over when it is dead. dupWorthy copies still use a second link | saving data on an expensive SIM | speed of one modem |

Why smooth mode is not the default for all UDP: QUIC (most video sites) runs over UDP and treats reordering as loss.
Releasing UDP ahead of holes in every mode would make QUIC downloads slower.

**Mode signalling.** The box decides. LuCI (Services → Sheaf) writes `mode [primary]` to `/etc/sheaf/mode`. sheaf
checks the file's modification time once a second (`-modefile`) and switches immediately, without a restart; a deleted
file means the default mode (speed) again (v62). Every
feedback the box sends carries its choice in byte 3: `0x80 | (primary−1)<<4 | mode`. The server runs `-mode peer` and
adopts the box's mode and primary link from the next feedback, typically within a second. A zero byte (an older peer)
changes nothing. See [PROTOCOL.md §5.2](PROTOCOL.md#52-the-mode-byte) and the
[sequence diagram](PROTOCOL.md#105-mode-change).

### Failure detection

- **Dead link.** A link with packets in flight and no newly acknowledged packet for

  ```
  deadAfter = clamp( max(1.5 × minRtt + 100 ms, 1.5 × srtt + 50 ms), 250 ms, 1 s )
  ```

  is declared dead. This is checked every 10 ms (`deadWatch`) and on every scheduling decision. The limit follows the
  queue the regulator itself allows: an earlier limit based only on the base RTT declared busy links dead, and the
  resulting cascade of resends filled the queues even more. All in-flight packets of the dead link are queued for
  resending over the live ones.
- **Stuck link (v58).** A link whose delivery rate collapses while its queue is full (real LTE: from ~40 to ~1 Mbit/s
  for 1–6 s) loses nothing and keeps acking a little, so neither the loss repair nor `deadAfter` fires. It is marked
  *stuck* instead (see the repair table), gets no new data, and its late packets are rescued over the other links
  (`stuck`, `stuck_count`, `slow_resends` in the status JSON). In the lab with such collapses, v59 stalled in 0/5
  runs, 2/5 with `-rescue=false`, and v57 in 5/5.
- **Slow link (v60).** A SIM throttled after its data allowance (a few hundred kbit/s) or a link on a very weak signal
  made the bundle slower than the good link alone: whenever the fast link's window was full the slow one got the
  packet, and it never holds less than 24 kB (0.75 s of queue at 256 kbit/s); its base RTT re-learns that queue, so it
  is neither stuck nor dead, and every packet on it held its TCP flow head-of-line. In the lab (one TCP flow, 60 s),
  25 Mbit/s plus 256 kbit/s gave 10.7 Mbit/s against 21.9 for the 25 alone (512 kbit/s: −12 %, 1 Mbit/s: +3 %,
  2 Mbit/s: +7 %). A link whose delivery rate stays below `-slowfrac` (3 %) of the best other live link's for `-slowafter` (10 s) is
  *demoted*: no new data, no starvation probes, no dupWorthy copies. Only samples taken while at least half of the
  link's window is in flight count, so an idle or lightly used link is never judged. To come back it gets *trials*
  (after 5 s, then 10, 20, 40, 60 s while it stays slow): for 1 s it carries copies of what the other links send, as
  much as its window takes. The receiver drops them as duplicates, nobody waits for them, they do not move the
  original's repair time and their loss is not repaired. A trial sample of 1.5 × the threshold brings the link back at
  once. While no other link is alive, a demoted link carries data. Status: `slow`, `slow_count`, `busy_mbit`,
  `trial_copies`; trace: `slow`, `trialcp`.
- **Control sends never wait (v60).** Feedback, NACKs and still-missing reports go out with `MSG_DONTWAIT`; a datagram
  that the socket cannot take is dropped and counted (`ctl_drops`). Before, `feedbackLoop` wrote every link in turn with
  a blocking write, so one link whose socket buffer did not drain (a stalled modem) stopped the feedback of all links.
  Data packets have their own send goroutine per link anyway.
- **Probes.** A dead link is not used for data. In reliable mode, every 500 ms it gets a copy of the oldest
  unacknowledged packet as a probe; the scheduler may also hand it a copy of a packet whose original goes over a live
  link. No data ever waits for a dead link.
- **Quiet probing (v62).** After 30 s dead the link gets no copies any more, only an empty DATA packet (40 bytes of
  UDP payload) every 5 s, and, while another link lives, no feedback, NACKs or still-missing reports (the far end gets
  those over the live links). The receiver answers a probe with one feedback at once, so the link is back within
  ~5 s plus a round trip after its path works again. Up to v61 a dead link cost a full-size copy every 500 ms and a
  feedback every 20 ms for as long as it stayed dead (100+ MB a day with a dead modem). Logged once per outage as
  `Lx quiet: …`; status `quiet`.
- **Silent link (v63, v64).** "Dead" above needs data in flight. A lightly used link whose feedback stopped (its
  modem lost the data path, its IPsec SA was gone on one side) used to stay "alive" until data happened to be sent on
  it: on box 1 that once took 55 s. Since v63 a link that brought no feedback for 8 s, while another link brought
  feedback within the last 4 s, is dead even with nothing in flight, and gets an empty probe at once. The far end
  sends feedback over every link at least every 2 s, so 8 s is four missed feedbacks. When all links are silent the
  cause is common (the server, the path) and none is declared dead. v64: a link that has just come back (its probe
  was acknowledged over another link) is not declared dead again before its own feedback can arrive (with v63 it
  could relapse on the next check).
- **Revival.** The first feedback that acknowledges something new on the link brings it back. After an outage shorter
  than 3 s (handover, dead spot), it restarts warm with half its old window. After a longer outage it starts small and
  relearns its base RTT and rate, since it may be on a different cell now.
- **Reverse path.** A feedback that arrives over link *k* marks *k*'s reverse direction as alive (`last_fb_ms` in the
  status JSON). The status page shows a link as running when it is not dead and its last feedback is younger than 3 s.
- **Below sheaf**, the box's supervisor `sheaf-run` checks every 10 s that each modem still has its address (and,
  once a minute, asks the modem itself). It redials a modem that lost its data session (with a growing back-off for a
  modem that keeps failing), reloads IPsec only for the link whose address changed (the other link keeps its SAs),
  and restarts dead processes. A sheaf whose status file is 15 s old is restarted (KILL if TERM does not end it). The
  TUN device `shf0` is persistent and owned by the supervisor, so a restart of sheaf keeps the routes and the LAN's
  TCP connections through the tunnel.
- **Recovery ladder (zombie modem).** A modem can keep its address and answer AT commands while no data passes, or
  the server can lose its IPsec state without telling the box. The supervisor counts a link as bad when its IKE SA is
  not established or sheaf has it dead (no feedback for 12 s, or dead and no feedback for 6 s), and climbs one step at
  a time: after 30 s restart its IKE SA, after 60 s also an established SA that carries nothing, after 3 min hang the
  modem up and redial, after 6 min reset the modem, after 15 min re-authorize it on USB. Only the bad link is touched;
  when no link is good the modem steps wait longer (10 / 20 / 30 min), because the cause is then more likely the
  server or the network. Details in [USAGE.md 1.2](USAGE.md#12-what-runs-on-the-box), measured in
  [RESULTS.md](RESULTS.md#reliability-tests-t1t16).
- **Restart of either end.** A random session id in every datagram makes the other end reset all its state at once
  (sequence numbers, buffers, streams, regulators). Late packets of the old session are dropped, never mixed into the
  new one. A server restart is invisible to the box apart from a gap of a few seconds.

### No fragmentation across links

sheaf never splits one IP packet across two links, and never combines two packets into one datagram. On LTE, one
1400-byte packet takes 0.1 to 0.4 ms to transmit. Splitting it would make it as slow as the slower of the two links,
double the chance that it is lost (either piece), and double the number of packets, which means more CPU and more ESP
operations on a box where per-packet cost is the bottleneck. Spreading *whole* packets over the links already uses
both links fully, since the scheduler balances them by expected arrival.

The same reasoning sets the packet size under IPsec. The TUN MTU is 1320 and the proxy frame is 1328 bytes
(`-frame 1328`), so a full DATA packet with ESP-in-UDP stays within 1500 bytes. With 1400-byte frames, the ESP packets
were fragmented, which cost 34 % (download) and 47 % (upload) instead of 21 %. See
[PROTOCOL.md §12](PROTOCOL.md#12-sizes-and-the-mtu-budget).

### TCP proxy option

`-proxy listen>target` (box) and `-serve -allow …` (server) terminate a TCP connection on the box and carry only its
byte stream, as frames inside reliable sheaf packets. The far end opens the real connection. No TCP runs between the
two ends, so a stall on one link does not trigger a TCP back-off and slow start on the whole transfer (TCP-in-TCP). In
the lab, this lifted varying links from 13 to 21 Mbit/s. Flow control is a credit of 16 MiB per stream (v56; 4 MiB
capped one stream over four LTE links). With `-proxy` the box listens on fixed addresses (the setup uses it for the
measurement endpoints `192.168.1.1:5201/5202` → `127.0.0.1:5201/5202` on the server); the transparent mode below
carries the LAN's own TCP connections. The frame format is in [PROTOCOL.md §8.1](PROTOCOL.md#81-proxy-frame).

**Transparent mode** (`-transparent` on the box, `-tallow` on the server, `TPROXY=1` in `sheaf.conf`). On the RBM33G
the tunnel costs about four times more box CPU per byte than the proxy: every packet is one TUN write plus the
kernel's forwarding and NAT of it, while the proxy hands the kernel big chunks. One TCP flow got ~20 Mbit/s through the
tunnel and 59–75 Mbit/s through the proxy over the same links. So the box's firewall redirects new TCP connections
from the LAN to public addresses to sheaf's listener (an nft table of its own, only while sheaf listens). sheaf
reads the original destination (SO_ORIGINAL_DST) and opens it on the server with an OPENT frame; the server checks its
policy, dials, and answers with WIN 0. UDP (including QUIC), ICMP and TCP to private addresses stay in the tunnel.
Memory on a 256 MB box is bounded by the receive window the box announces per stream (half of what is left of
`-tbudget`, 128 KiB..4 MiB), a cap on streams (`-tmax`) and an idle timeout. The client's TCP ends on the box: the
LAN side sees a connect at once, and a destination that refuses shows up as a reset right after it. Proxied
connections live in the sheaf process, so they end when it restarts (on either end); connections through the tunnel
survive a restart.

Measured on box 1 with a real download from the internet (a computer behind the box, IPsec on, ABAB): 17–19 Mbit/s
through the tunnel, 38–48 Mbit/s through the transparent proxy. QUIC (HTTP/3) is UDP and so stays in the slower
tunnel; `BLOCK_QUIC=1` rejects UDP 443 from the LAN at once, browsers fall back to TCP and with it to the proxy, at the
price of no HTTP/3 (box 1 runs it this way). The flowtable shortcut (below) and `-tungro` later raised the tunnel
itself to 38–41 Mbit/s, so the gap is smaller now ([RESULTS.md](RESULTS.md#single-rbm33g-box)).

### Security

Security comes in two layers:

1. **sheaf's own authentication.** Every datagram ends with a 64-bit SipHash-1-3 tag under a 128-bit shared key
   (`-key /etc/sheaf/sheaf.key`, 32 hex characters, mode 600). The tag covers the whole datagram, including the session ids.
   Unauthenticated datagrams are dropped before anything else, and only authenticated datagrams can move a link to a
   new peer address. With `-auth=off` (v62, on both ends) the tag is left out (sent as 0) and layer 2 alone
   authenticates: ESP's HMAC-SHA-256 already covers every datagram, and on the box the tag cost 4–15 % of the CPU.
   `-auth=half` (v62) uses HalfSipHash-1-3, whose 32-bit words suit the MIPS32 CPU: through the tunnel of box 1 it was
   almost as fast as `off` (41.2 vs 42.9 Mbit/s, SipHash 33.7) and keeps a second check under IPsec, so box 1 and its
   server instance run it. Both ends must use the same `-auth`. A random 32-bit **session id** per process, plus the echo of the peer's id, separates
   incarnations, so a restarted end never processes stale sequence numbers. sheaf itself provides no confidentiality
   and no replay window; both come from layer 2.
2. **IPsec ESP per link** (strongSwan on both ends). IKEv2 uses `aes256-sha384-ecp384` with PSK authentication (one IKE
   SA per modem). ESP uses `aes256-sha256-ecp384`, in transport mode, UDP-encapsulated, restricted to sheaf's UDP
   ports, with trap policies (no plaintext fallback) and DPD. On the box, ESP runs in the **EIP93** engine of the
   MT7621, which supports AES-CBC with HMAC-SHA-256 but not AES-GCM.

   On OpenWrt 25.12 (kernel 6.12), every ESP state with authentication offloaded to EIP93 failed with `EINVAL`.
   `mtk_aead_setkey()` looked the algorithm template up through `alg.skcipher.base` instead of `alg.aead.base`. With
   kernel 6.12 and non-coherent DMA, the two offsets differ by 128 bytes, so the driver read `flags = 0`. The one-line
   fix is in `openwrt/patches/861-eip93-aead-setkey-use-aead-container.patch`, submitted upstream as
   [openwrt/openwrt#26910](https://github.com/openwrt/openwrt/pull/26910). The firmware image ships the fixed module.
   Measured on the box: ESP in software 17.6 Mbit/s, ESP on EIP93 with the fix 60.6 Mbit/s.

**Transparent opens** let the box ask the server to dial any address, so the server decides by policy, never the box:
without `-tallow` nothing is dialed; `-tallow public` dials only public addresses (private, CGNAT, loopback,
link-local, documentation, multicast, IPv6 ULA/link-local, NAT64/6to4/Teredo ranges and every address of the
server itself are refused), explicit CIDRs in `-tallow` admit configured targets, `-tdeny` excludes addresses and
ports. The server must not become a relay into its own networks. The streams travel inside the authenticated sheaf
packets under ESP, the same as the tunnel; nothing from the LAN leaves the box outside them.

Secrets are never part of the repository or the firmware image. `/etc/sheaf/sheaf.key`,
`/etc/swanctl/conf.d/sheaf-secret.conf` (the IPsec PSK) and the root password are installed on each box once, and
they survive firmware upgrades (`/etc/sysupgrade.conf`).

### CPU on a small MIPS box

The MT7621 is a 880 MHz MIPS32 with 2 cores and 2 threads per core, no FPU and no 64-bit arithmetic. sheaf is shaped
by this CPU:

- one clock read per packet (on this platform Go has no vDSO clock, and `time.Now()` costs ~2.2 µs);
- integer arithmetic in the scheduler (floating point is emulated in software);
- `recvmmsg` / `sendmmsg` with up to 32 datagrams per call, plus UDP GSO (`UDP_SEGMENT`), with automatic fallback when
  the route refuses it (`SHEAF_NOGSO=1` disables it);
- a goroutine per link that signs and sends its queue in parallel with scheduling;
- zero-copy first transmission and pooled receive buffers;
- SipHash-1-3 instead of 2-4 (15 µs instead of 75 µs per packet);
- `GOMAXPROCS=2`: more Ps only spin. Measured over the cable: 87 → 93 Mbit/s and −23 % CPU per Mbit;
- integer arithmetic in the per-packet send path (v62), HalfSipHash for the tag (`-auth=half`);
- `GOMEMLIMIT=96MiB` from the supervisor, so the Go heap stays far below the box's 256 MB even while a repair hole
  holds data (sheaf itself runs at ~13 MB RSS; it stayed flat through a 9.5 h overnight run).

**Where the box's CPU goes: tunnel vs proxy.** Over the same links, one TCP flow through the tunnel reached ~20
Mbit/s and through the proxy 59–75 Mbit/s. A CPU profile of sheaf on the box showed the same CPU time in both
(~1.2 threads) but ~720 µs per downloaded packet in tunnel mode against ~190 µs per frame in proxy mode. In tunnel
mode every packet is one `write` into the TUN device (~190 µs: the kernel routes, NATs and bridges it to the LAN inside
that syscall) and the TCP ACKs of the LAN client travel back up through the TUN device, the flow queue and the
sender (~165 µs per downloaded packet); the proxy writes big chunks into a socket and sends no per-packet ACKs. The
server's window regulator only mirrors this: datagrams waiting in the box's sockets look like queueing on the links,
so the windows stay at what the box takes. In the lab the same code on a fast CPU ran the tunnel as fast as the proxy;
the tunnel ceiling appeared only below ~10 % of one x86 core.

Three changes attack the per-packet cost on the box:

- **`-tungro` (v61):** in-order TCP segments of one flow received in one batch go to the TUN as one GSO write (IFF_VNET_HDR);
  the kernel's filters, conntrack, NAT, routing and bridge then run once for ~2–4 segments. +22 % through the tunnel
  on box 1 (+60 % in the lab at 5 % of a core).
- **Flowtable (`FLOWTABLE=1`, box package 61-r4):** established forwarded LAN ↔ `shf0` connections skip the rest of the
  netfilter path: +53 % through the tunnel (20.7 → 31.7 Mbit/s, means of 4 + 4 interleaved runs). fw4's own flow
  offloading does not cover a TUN device.
- **`-auth=half` (v62):** +22 % through the tunnel.

Together the tunnel on box 1 went from 17–23 to 38–41 Mbit/s and the proxy is at 45–48 Mbit/s. That is still the
CPU's limit: with the BPI-R4 (Cortex-A73) as the brain and box 1 only carrying its modems, the same SIMs gave
68–102 Mbit/s (73–82 right next to the raw measurement, 82–92 % of the raw links used together), and the R4
throttled to 12.5 % of one core fell to 24 Mbit/s, the RBM33G's level before the box-side optimisations (17–23): per
packet the MT7621 is about 10× slower than one A73 core.
See [RESULTS.md](RESULTS.md#the-cpu-limit-proven-the-r4-as-the-brain).


---

## Measured results

All measurements, the method behind them and their evaluation are in [RESULTS.md](RESULTS.md). In short:

- **Lab** (emulated links): 95 % of an ideal bond on static and varying links, 89 % on the "drive" scenario; v59
  removed the multi-second tunnel stalls of v57 (0/6 against 6/6 runs) and v60 the "slower than the good link alone"
  case of a throttled SIM.
- **BPI-R4 hub, four LTE links, IPsec:** one TCP flow through the tunnel 158–188 Mbit/s down with the 1–2 MB flow
  queue (6 runs) and 108 up; none of the 17 v59 runs had a stall (v57: 4 of 7).
- **One RBM33G box, two LTE links, IPsec:** the box's MIPS CPU is the limit: one TCP flow 38–41 Mbit/s through the
  tunnel and 45–48 through the transparent proxy, while the same SIMs with a BPI-R4 as the brain gave 68–102 Mbit/s.
  On this box the bond buys reliability, not speed: it is slower than the best single link alone when that link is
  good.
- **Reliability tests** T1–T16 on the box (restarts, address changes, lost keys, zombie modem, throttled SIM, rekey,
  power cut, overnight run) passed, some after fixes (hardware caveat: inserting a SIM while running resets the box).
- **Final v65 on the box (27 Nov):** the tunnel survived a modem cut during a live UDP stream in every mode; a link
  that fails in one direction only is not detected per direction (known limitation, not fixed).

---

## Limitations and open ends

The project is finished as a lab project; these are the known limits and what was never verified.

- **The RBM33G is too slow for its own modems.** Its CPU per packet caps one TCP flow at about 40 Mbit/s through the
  tunnel and 45–48 through the proxy, below what a single good LTE link delivers. For speed, the brain needs an ARM64
  or x86 class CPU (the BPI-R4 was measured); the RBM33G remains useful as a satellite that only carries modems, or
  where reliability matters more than speed.
- **One box per server instance**: an instance keeps one peer state (one session, one set of links); several boxes
  need several instances (`sheaf@.service`, one `.env` per box or hub).
- **QMI and MBIM modems** (`MODEM=qmi|mbim`) are written and reviewed, but were never run on hardware. Every
  measurement used Huawei ME909s modems.
- **Measured stationary.** No drive test was done; the "drive" exists only as a lab scenario.
- **Losing a satellite** stops the hub's whole tunnel for a moment today: a change of the set of satellites restarts
  sheaf and reloads IPsec of all links.
- **Delay under full load.** With the 1 MB flow queue the links run fuller: another flow's ping p99 under a full
  download rose from 64 to 99 ms on the hub. The queue is at the base station; a smaller server `-maxq` would trade
  rate for delay (not measured).
- **Time of day.** The v59 LTE comparison ran in the morning; the evening peak (18–21 h by the cell load in the radio
  samples) was not repeated.
- **Smooth mode** only lets UDP overtake holes. It does not reconstruct the missing packet (no FEC; the data showed
  that only 0.4 % of a live stream's packets waited for a hole of their own).
- **Per-flow release** needs both ends at v53 or later; with an older peer, delivery falls back to strict order.
- No replay protection in sheaf's own layer; it relies on IPsec for that.
- **Uploads** in the lab still show a short stall in 2 of 5 runs under rate collapses: the network, not the pacer.
- `-iprelease` (bounded receiver wait) is a safety net that is off: with working repairs it never fired, and when it
  fires TCP pays for the reordering. `-pacepick` true or false with the 1 MB flow queue was not decided with enough
  runs.
- **An idle server instance** declares a link dead for a moment after one lost packet when nothing else is in flight
  (six times in a 9.5 h night, back in the same second, no effect on traffic).
- **Link state is per link, not per direction.** When only the box → server direction of a link fails, the server
  flaps the link dead/back every 8 s (its probes are acknowledged over another link) and the box notices nothing while
  idle (measured on v65, not fixed).
- **The tunnel's window cap** uses `rateMax`, which ACK bursts on LTE inflate: the queue under the cap reached
  208–226 ms instead of the intended ~133 ms.
