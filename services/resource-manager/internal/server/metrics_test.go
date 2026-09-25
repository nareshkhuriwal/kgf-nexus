// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics/metricstest"
)

func TestLedgerMetricsExposition(t *testing.T) {
	reg := metrics.NewRegistry()
	m := NewMetrics(reg)
	m.reserve("granted", time.Now())
	m.reserve("refused", time.Now())
	m.release("absent")
	m.leak("heartbeat", 2)
	m.leak("dead_worker", 0) // zero is not an observation
	m.heartbeat("ok")
	m.workersDead(1)
	m.quarantine("quarantined", 1)

	fams := metricstest.Scrape(t, reg.Handler(), "/metrics")
	metricstest.RequireFamilies(t, fams,
		"nexus_rm_reservations_total", "nexus_rm_reserve_duration_seconds", "nexus_rm_releases_total",
		"nexus_rm_leaked_reservations_reclaimed_total", "nexus_rm_heartbeats_total",
		"nexus_rm_workers_declared_dead_total", "nexus_rm_quarantine_changes_total")
	if s := metricstest.Find(fams, "nexus_rm_leaked_reservations_reclaimed_total",
		map[string]string{"source": "heartbeat"}); s == nil || s.GetCounter().GetValue() != 2 {
		t.Errorf("leaked{heartbeat} = %v, want 2", s)
	}
	if s := metricstest.Find(fams, "nexus_rm_leaked_reservations_reclaimed_total",
		map[string]string{"source": "dead_worker"}); s != nil {
		t.Errorf("a zero-count leak created a series: %v", s)
	}
}

func TestNilLedgerMetricsIsSafe(t *testing.T) {
	var m *Metrics
	m.reserve("granted", time.Now())
	m.release("released")
	m.leak("reconcile", 3)
	m.heartbeat("error")
	m.workersDead(2)
	m.quarantine("expired", 1)
}
