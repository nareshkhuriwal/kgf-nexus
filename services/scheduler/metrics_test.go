// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics/metricstest"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// TestMetricsServedOnHealthListener: the scrape target is the health listener
// the probes already use, and it serves the scheduler's own families plus the
// standard ones. The store is nil: a scrape must not touch the database.
func TestMetricsServedOnHealthListener(t *testing.T) {
	reg := metrics.NewServiceRegistry("scheduler", "test")
	m := NewMetrics(reg, "default")
	m.observeClaim("poll", "dispatched", 3*time.Millisecond, 2)
	m.observeClaim("poll", "empty", time.Millisecond, 0)
	m.observeLost(store.LostAttempt{Decision: domain.RetryDecision{Retry: true}}, "lease_expired")
	m.observeLost(store.LostAttempt{}, "worker_dead")
	m.observeDeadWorkers(1)
	m.setBacklog([]store.QueueBacklog{{Queue: "default", Pending: 7, OldestAgeSeconds: 12.5}})

	var ready atomic.Bool
	fams := metricstest.Scrape(t, withMetrics(healthMux(nil, nil, &ready), reg), "/metrics")
	metricstest.RequireFamilies(t, fams,
		"nexus_scheduler_claims_total", "nexus_scheduler_claim_duration_seconds",
		"nexus_scheduler_attempts_dispatched_total", "nexus_scheduler_attempts_lost_total",
		"nexus_scheduler_lease_expiries_total", "nexus_scheduler_workers_declared_dead_total",
		"nexus_scheduler_pending_attempts", "nexus_scheduler_oldest_pending_age_seconds",
		"go_goroutines", metrics.BuildInfoName)

	if s := metricstest.Find(fams, "nexus_scheduler_attempts_dispatched_total",
		map[string]string{"queue": "default", "path": "poll"}); s == nil || s.GetCounter().GetValue() != 2 {
		t.Errorf("dispatched = %v, want 2", s)
	}
	if s := metricstest.Find(fams, "nexus_scheduler_lease_expiries_total", nil); s == nil || s.GetCounter().GetValue() != 1 {
		t.Errorf("lease expiries = %v, want 1 (worker_dead losses are not lease expiries)", s)
	}
	if s := metricstest.Find(fams, "nexus_scheduler_pending_attempts",
		map[string]string{"queue": "default"}); s == nil || s.GetGauge().GetValue() != 7 {
		t.Errorf("pending{default} = %v, want 7", s)
	}
}

// TestBacklogGaugesZeroDrainedQueuesAndCapCardinality: a queue that drains
// reads 0 (not its last value), and author-chosen queue names cannot mint
// unbounded series.
func TestBacklogGaugesZeroDrainedQueuesAndCapCardinality(t *testing.T) {
	reg := metrics.NewRegistry()
	m := NewMetrics(reg, "default")

	m.setBacklog([]store.QueueBacklog{{Queue: "gpu", Pending: 4}})
	m.setBacklog(nil) // gpu drained
	fams := metricstest.Scrape(t, reg.Handler(), "/metrics")
	if s := metricstest.Find(fams, "nexus_scheduler_pending_attempts", map[string]string{"queue": "gpu"}); s == nil || s.GetGauge().GetValue() != 0 {
		t.Errorf("drained queue reads %v, want 0", s)
	}

	var sample []store.QueueBacklog
	for i := 0; i < maxBacklogQueues+25; i++ {
		sample = append(sample, store.QueueBacklog{Queue: fmt.Sprintf("q%03d", i), Pending: 1})
	}
	m.setBacklog(sample)
	fams = metricstest.Scrape(t, reg.Handler(), "/metrics")
	if n := len(fams["nexus_scheduler_pending_attempts"].GetMetric()); n > maxBacklogQueues+1 {
		t.Errorf("%d backlog series, want at most %d", n, maxBacklogQueues+1)
	}
	if s := metricstest.Find(fams, "nexus_scheduler_pending_attempts", map[string]string{"queue": OtherQueue}); s == nil || s.GetGauge().GetValue() < 25 {
		t.Errorf("overflow queues were not summed into %q: %v", OtherQueue, s)
	}
}

func TestNilMetricsIsSafe(t *testing.T) {
	var m *Metrics
	m.observeClaim("push", "error", time.Second, 0)
	m.observeLost(store.LostAttempt{}, "lease_expired")
	m.observeTimer("retry_backoff", "ok")
	m.observeLoopError("dispatch")
	m.setBacklog(nil)
}
