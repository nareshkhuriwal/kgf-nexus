// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// TestInstanceDetailReportsTheReplicasActivity pins the scheduler's registry
// detail (ADR-0037 §3): its queue, the running totals, the per-beat deltas
// and its own queue's backlog from the latest sample.
func TestInstanceDetailReportsTheReplicasActivity(t *testing.T) {
	m := NewMetrics(metrics.NewRegistry(), "gpu")
	detail := m.instanceDetail()

	first := detail()
	if first["queue"] != "gpu" || first["claims_total"] != int64(0) || first["dispatched_recent"] != int64(0) {
		t.Fatalf("idle detail = %v", first)
	}
	if _, ok := first["pending_attempts"]; ok {
		t.Error("backlog keys appeared before any sample")
	}

	m.observeClaim("poll", "dispatched", time.Millisecond, 3)
	m.observeClaim("poll", "empty", time.Millisecond, 0)
	m.observeClaim("push", "error", time.Millisecond, 0)
	m.observeLost(store.LostAttempt{Decision: domain.RetryDecision{Retry: true}}, "lease_expired")
	m.setBacklog([]store.QueueBacklog{
		{Queue: "gpu", Pending: 9, OldestAgeSeconds: 41.26},
		{Queue: "default", Pending: 100, OldestAgeSeconds: 900},
	})

	d := detail()
	for k, want := range map[string]any{
		"claims_total": int64(3), "claim_errors_total": int64(1), "dispatched_total": int64(3),
		"lost_total": int64(1), "claims_recent": int64(3), "dispatched_recent": int64(3),
		"pending_attempts": int64(9), "oldest_pending_age_seconds": 41.3,
	} {
		if d[k] != want {
			t.Errorf("%s = %v (%T), want %v", k, d[k], d[k], want)
		}
	}
	if _, err := time.Parse(time.RFC3339, d["backlog_sampled_at"].(string)); err != nil {
		t.Errorf("backlog_sampled_at: %v", err)
	}

	// The next beat's deltas cover only what happened since this one.
	m.observeClaim("poll", "dispatched", time.Millisecond, 1)
	d = detail()
	if d["claims_recent"] != int64(1) || d["dispatched_recent"] != int64(1) || d["claims_total"] != int64(4) {
		t.Errorf("second beat = %v", d)
	}
	if w, _ := d["recent_window_seconds"].(float64); w < 0 || w > 5 {
		t.Errorf("recent_window_seconds = %v", d["recent_window_seconds"])
	}

	// A drained queue reads zero, not its last value.
	m.setBacklog(nil)
	if d = detail(); d["pending_attempts"] != int64(0) {
		t.Errorf("drained pending = %v", d["pending_attempts"])
	}

	var nilMetrics *Metrics
	if nilMetrics.instanceDetail()() != nil {
		t.Error("a nil *Metrics must report no detail")
	}
}
