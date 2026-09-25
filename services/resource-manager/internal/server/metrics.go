// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
)

// Metrics are the resource manager's ledger instruments. Labels are outcome and
// source enums only; worker and attempt ids stay in the logs.
//
// A nil *Metrics records nothing, so tests construct servers without it.
type Metrics struct {
	reservations *metrics.CounterVec
	reserveDur   *metrics.HistogramVec
	releases     *metrics.CounterVec
	leaked       *metrics.CounterVec
	heartbeats   *metrics.CounterVec
	dead         *metrics.Counter
	quarantines  *metrics.CounterVec
}

// NewMetrics registers the resource-manager families on reg.
func NewMetrics(reg *metrics.Registry) *Metrics {
	return &Metrics{
		reservations: reg.NewCounter("nexus_rm_reservations_total",
			"Reserve calls, by result: granted, refused (insufficient capacity, a normal scheduling outcome), not_found, error.",
			"result"),
		reserveDur: reg.NewHistogram("nexus_rm_reserve_duration_seconds",
			"Latency of a Reserve call's ledger transaction, by result.",
			[]float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
			"result"),
		releases: reg.NewCounter("nexus_rm_releases_total",
			"Release calls, by result: released, absent (already released; expected under at-least-once reporting), error.",
			"result"),
		leaked: reg.NewCounter("nexus_rm_leaked_reservations_reclaimed_total",
			"Reservations reclaimed as leaks, by source: heartbeat (worker did not acknowledge the attempt), reconcile (explicit call), dead_worker (liveness sweep or ReleaseAllForWorker).",
			"source"),
		heartbeats: reg.NewCounter("nexus_rm_heartbeats_total",
			"Worker heartbeats processed, by result: ok, unknown_worker (answered with re-registration), error.",
			"result"),
		dead: reg.NewCounter("nexus_rm_workers_declared_dead_total",
			"Workers the resource-manager liveness sweep transitioned to DEAD.").Counter(),
		quarantines: reg.NewCounter("nexus_rm_quarantine_changes_total",
			"Automatic quarantine changes by the detector, by action (quarantined, expired).",
			"action"),
	}
}

func (m *Metrics) reserve(result string, start time.Time) {
	if m == nil {
		return
	}
	m.reservations.With(metrics.Labels{"result": result}).Inc()
	m.reserveDur.With(metrics.Labels{"result": result}).Observe(time.Since(start).Seconds())
}

func (m *Metrics) release(result string) {
	if m == nil {
		return
	}
	m.releases.With(metrics.Labels{"result": result}).Inc()
}

func (m *Metrics) leak(source string, n int) {
	if m == nil || n <= 0 {
		return
	}
	m.leaked.With(metrics.Labels{"source": source}).Add(float64(n))
}

func (m *Metrics) heartbeat(result string) {
	if m == nil {
		return
	}
	m.heartbeats.With(metrics.Labels{"result": result}).Inc()
}

func (m *Metrics) workersDead(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.dead.Add(float64(n))
}

func (m *Metrics) quarantine(action string, n int) {
	if m == nil || n <= 0 {
		return
	}
	m.quarantines.With(metrics.Labels{"action": action}).Add(float64(n))
}
