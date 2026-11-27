// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Petr Wozniak

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"
)

// Status for people and tools (the board's status page): a small JSON file rewritten once a second (-status path).
// Per link: alive or dead, rates in both directions, delay; plus the reorder/repair counters. Atomic rename, so a
// reader never sees half a file.
type linkStatus struct {
	ID          int     `json:"id"`
	Dead        bool    `json:"dead"`
	RxMbit      float64 `json:"rx_mbit"`     // received on this link in the last second
	TxMbit      float64 `json:"tx_mbit"`     // sent on this link in the last second (first sends and repairs)
	TxEstMbit   float64 `json:"tx_est_mbit"` // what the peer acknowledged lately (sender's delivery estimate)
	SrttMs      float64 `json:"srtt_ms"`
	MinRttMs    float64 `json:"min_rtt_ms"`
	WinKB       float64 `json:"win_kb"`
	InflightKB  float64 `json:"inflight_kb"`
	LastRxMs    int64   `json:"last_rx_ms"`   // ms since the last data packet arrived on this link (-1 = never)
	LastFbMs    int64   `json:"last_fb_ms"`   // ms since the last feedback arrived over this link (-1 = never)
	DeadCount   int64   `json:"dead_count"`   // times declared dead since start
	LossPct     float64 `json:"loss_pct"`     // NACKed share of what this side sent on the link (last window)
	LossCuts    int64   `json:"loss_cuts"`    // window cuts because of loss
	MaxQMs      float64 `json:"max_queue_ms"` // learned queue cap
	Stuck       bool    `json:"stuck"`        // its queue drains far slower than its healthy round trip (no new data)
	StuckCount  int64   `json:"stuck_count"`  // times it got stuck since start
	TxqKB       float64 `json:"txq_kb"`       // queued for its txLoop, not yet handed to the kernel (v59)
	TxPkts      uint32  `json:"tx_pkts"`      // packets its txLoop really sent since start (v59)
	PaceMbit    float64 `json:"pace_mbit"`    // pick pacing rate, 0 = unpaced (v59)
	Slow        bool    `json:"slow"`         // demoted as persistently slow: no data, only trial copies (v60)
	SlowCount   int64   `json:"slow_count"`   // times demoted since start (v60)
	BusyMbit    float64 `json:"busy_mbit"`    // latest delivery-rate sample with the link in use (v60)
	TrialCopies int64   `json:"trial_copies"` // copies sent on it during trials (v60)
	Quiet       bool    `json:"quiet"`        // dead 30 s+: only an empty probe every 5 s, no control datagrams (v62)
}

type status struct {
	Version   string       `json:"version"`
	Session   string       `json:"session"`
	UptimeS   int64        `json:"uptime_s"`
	Reliable  bool         `json:"reliable"`
	Links     []linkStatus `json:"links"`
	Delivered int64        `json:"delivered"`
	Rtx       int64        `json:"rtx"`
	Nacked    int64        `json:"nacked"`
	Missing   int64        `json:"missing_reports"`
	Buffered  int          `json:"buffered"`
	NoDelay   int64        `json:"nodelay"`
	Dups      int64        `json:"dups"`
	DupSent   uint32       `json:"dup_sent"`
	Updated   int64        `json:"updated_unix"`
	Mode      string       `json:"mode"`
	Primary   int          `json:"primary"`
	Follow    bool         `json:"follow_peer"`
	LiveEarly int64        `json:"live_early"`
	FqDrops   int64        `json:"fq_drops"`
	FqBytes   int          `json:"fq_bytes"`
	FlowEarly int64        `json:"flow_early"`
	Repaired  int64        `json:"repaired"`     // resends for NACKs and still-missing reports
	MissRtx   int64        `json:"miss_resends"` // of them for still-missing reports
	Rescued   int64        `json:"slow_resends"` // packets sent again because their link got stuck
	RescuedKB float64      `json:"slow_resend_kb"`
	HoleWait  int64        `json:"hole_wait_ms"` // how long the oldest packet in the reorder buffer has waited
	IPRel     int64        `json:"ip_released"`  // IP packets released past a hole by their deadline (v59 P4)
	Features  string       `json:"features"`     // the v59 / v60 switches
	CtlDrops  uint32       `json:"ctl_drops"`    // control datagrams dropped because a socket was full (v60)
	TunWrites int64        `json:"tun_writes"`   // v61 -tungro: TUN writes, segments in them, GSO super-packets
	TunSegs   int64        `json:"tun_segs"`
	TunSupers int64        `json:"tun_supers"`
	TProxy    *tpStatus    `json:"tproxy,omitempty"` // transparent TCP proxy (tproxy.go), absent when off
	Auth      bool         `json:"auth"`             // v62: datagrams carry and require a tag (-key, -auth)
	AuthAlg   string       `json:"auth_alg"`         // v62: which: sip, half, or off
	AuthRej   uint32       `json:"auth_rejects"`     // v62: datagrams dropped by the tag check ("auth mismatch?")
}

const version = "v65"

func sessionHex() string { return fmt.Sprintf("%08x", mySession) }

func (p *peer) statusWriter(path string) {
	start := time.Now()
	lastRx := make([]int64, len(p.links))
	lastTx := make([]int64, len(p.links))
	tmp := path + ".tmp"
	for range time.Tick(time.Second) {
		nowM := mono()
		st := status{Version: version, Session: sessionHex(), UptimeS: int64(time.Since(start).Seconds()), Reliable: reliable,
			Updated: time.Now().Unix()}
		for i, l := range p.links {
			l.mu.Lock()
			ls := linkStatus{ID: l.id, Dead: l.dead, RxMbit: float64(l.rxBytes-lastRx[i]) * 8 / 1e6,
				TxEstMbit: l.rateAvg * 8, SrttMs: l.srtt / 1000, MinRttMs: l.minRtt / 1000,
				WinKB: l.win / 1000, InflightKB: float64(l.inflight) / 1000, LastRxMs: -1, LastFbMs: -1, DeadCount: l.deadCount,
				LossPct: l.lossFrac * 100, LossCuts: l.lossCuts, MaxQMs: l.maxQ / 1000, Stuck: l.stuck, StuckCount: l.stuckCount,
				TxqKB: float64(l.txqB.Load()) / 1000, TxPkts: l.txSentN.Load(), PaceMbit: float64(l.paceBps) * 8 / 1e6,
				Slow: l.sl.slow, SlowCount: l.sl.slowCount, BusyMbit: l.sl.busyRate * 8, TrialCopies: l.sl.copies,
				Quiet: l.longDead(nowM)}
			lastRx[i] = l.rxBytes
			ls.TxMbit = float64(l.txBytes-lastTx[i]) * 8 / 1e6
			lastTx[i] = l.txBytes
			if l.lastRx != 0 {
				ls.LastRxMs = (nowM - l.lastRx) / 1e6
			}
			if !l.fbRx.IsZero() {
				ls.LastFbMs = time.Since(l.fbRx).Milliseconds()
			}
			l.mu.Unlock()
			st.Links = append(st.Links, ls)
		}
		p.reMu.Lock()
		st.Repaired, st.MissRtx, st.Rescued, st.RescuedKB = p.repaired, p.missRtx, p.rescued, float64(p.rescuedB)/1000
		p.reMu.Unlock()
		p.rxMu.Lock()
		st.HoleWait = p.holeWaitMs(uint32(time.Since(startTime) / time.Millisecond))
		st.Delivered, st.Rtx, st.Nacked, st.Missing, st.Buffered, st.NoDelay = p.delivered, p.rtx, p.nacked, p.missRep, len(p.buf), p.noDelayed
		st.Dups, st.LiveEarly, st.FlowEarly = p.dups, p.liveEarly, p.flowEarly
		st.IPRel, st.Features = p.ipReleased, v59Summary()+" "+v60Summary()+" "+tunGROSummary()
		st.TunWrites, st.TunSegs, st.TunSupers = p.gro.writes, p.gro.segsTot, p.gro.supers
		st.CtlDrops = ctlDrops.Load()
		st.Auth, st.AuthAlg, st.AuthRej = useMAC(), authStatus(), authRejects.Load()
		st.TProxy = tproxyStatus()
		st.Mode, st.Primary, st.Follow = modeNames[curMode()], primaryIdx()+1, modeFollow
		st.DupSent = dupSent.Load()
		st.FqDrops = fqDrops.Load()
		if p.fq != nil {
			p.fq.mu.Lock()
			st.FqBytes = p.fq.bytes
			p.fq.mu.Unlock()
		}
		p.rxMu.Unlock()
		b, _ := json.Marshal(st)
		if err := os.WriteFile(tmp, b, 0644); err == nil {
			os.Rename(tmp, path)
		} else {
			log.Printf("status: %v", err)
			return
		}
	}
}
