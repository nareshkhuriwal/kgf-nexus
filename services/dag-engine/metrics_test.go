// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/dag"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics/metricstest"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

type failingPinger struct{}

func (failingPinger) Ping(context.Context) error { return errors.New("database is down") }

// TestMetricsServedOnHealthListener: /metrics is on the health listener beside
// the probes, answers while the database is DOWN (the pinger fails), and counts
// only transitions whose CAS was won.
func TestMetricsServedOnHealthListener(t *testing.T) {
	reg := metrics.NewServiceRegistry("dag-engine", "test")
	m := NewMetrics(reg)
	hit := "src"
	m.observeApplied(
		[]store.NodeTransition{
			{NodeID: "a", From: domain.NodePending, To: domain.NodeReady},
			{NodeID: "b", From: domain.NodePending, To: domain.NodeReady}, // lost its CAS
		},
		[]string{"b"},
		[]store.CreatedAttempt{{NodeID: "a"}, {NodeID: "c", CacheHitOf: &hit}},
		0, 1)
	m.observeOp("reconcile", time.Now(), nil)
	m.observeRunState(string(domain.RunSucceeded), nil)

	var ready atomic.Bool
	ready.Store(true)
	h := withMetrics(healthMux(failingPinger{}, &ready), reg)
	fams := metricstest.Scrape(t, h, "/metrics")
	metricstest.RequireFamilies(t, fams,
		"nexus_dag_operations_total", "nexus_dag_operation_duration_seconds",
		"nexus_dag_node_transitions_total", "nexus_dag_attempts_created_total",
		"nexus_dag_node_cas_misses_total", "nexus_dag_map_expansions_total",
		"nexus_dag_run_state_published_total", "go_goroutines", metrics.BuildInfoName)

	tr := metricstest.Find(fams, "nexus_dag_node_transitions_total",
		map[string]string{"from": string(domain.NodePending), "to": string(domain.NodeReady)})
	if tr == nil || tr.GetCounter().GetValue() != 1 {
		t.Errorf("transitions = %v, want 1 (the CAS-lost write must not count)", tr)
	}
	if s := metricstest.Find(fams, "nexus_dag_attempts_created_total", map[string]string{"kind": "cache_hit"}); s == nil {
		t.Error("memo hits are not distinguished from new attempts")
	}

	// The probes are still served by the wrapped mux.
	if code := get(h, "/healthz"); code != 200 {
		t.Errorf("/healthz = %d through the metrics wrapper, want 200", code)
	}
	if code := get(h, "/readyz"); code != 503 {
		t.Errorf("/readyz = %d with the database down, want 503", code)
	}
}

// TestOperatorRetryMetrics: every ResetNodes outcome lands in one bounded
// result label, classified the way toStatus maps it for the caller, and the
// nodes a successful retry reset are counted.
func TestOperatorRetryMetrics(t *testing.T) {
	reg := metrics.NewRegistry()
	m := NewMetrics(reg)

	cases := []struct {
		err  error
		want string
	}{
		{nil, "reset"},
		{fmt.Errorf("reset nodes of run r: %w", &dag.RetryError{Unknown: []string{"nope"}}), "invalid"},
		{fmt.Errorf("reset nodes of run r: %w", &dag.RetryError{Refused: []dag.NodeRefusal{{NodeID: "a"}}}), "not_retryable"},
		{fmt.Errorf("reset nodes of run r: %w", ErrRunNotRetryable), "not_retryable"},
		{fmt.Errorf("reset nodes of run r: %w", ErrNotMaterialized), "not_retryable"},
		{fmt.Errorf("reset nodes: %w", store.ErrDAGEngineRunNotFound), "not_found"},
		{fmt.Errorf("reset nodes of run r: %w", store.ErrNodeRetryConflict), "conflict"},
		{errors.New("connection refused"), "error"},
	}
	for _, c := range cases {
		if got := retryResult(c.err); got != c.want {
			t.Errorf("retryResult(%v) = %q, want %q", c.err, got, c.want)
		}
	}

	m.observeOperatorRetry(retryResult(nil), 3)
	m.observeOperatorRetry(retryResult(nil), 2)
	m.observeOperatorRetry(retryResult(ErrRunNotRetryable), 0)

	fams := metricstest.Scrape(t, metrics.OpsMux(reg), "/metrics")
	metricstest.RequireFamilies(t, fams, "nexus_dag_operator_retries_total",
		"nexus_dag_operator_retry_nodes_reset_total")
	if s := metricstest.Find(fams, "nexus_dag_operator_retries_total",
		map[string]string{"result": "reset"}); s == nil || s.GetCounter().GetValue() != 2 {
		t.Errorf("operator_retries{reset} = %v, want 2", s)
	}
	if s := metricstest.Find(fams, "nexus_dag_operator_retries_total",
		map[string]string{"result": "not_retryable"}); s == nil || s.GetCounter().GetValue() != 1 {
		t.Errorf("operator_retries{not_retryable} = %v, want 1", s)
	}
	if s := metricstest.Find(fams, "nexus_dag_operator_retry_nodes_reset_total", nil); s == nil || s.GetCounter().GetValue() != 5 {
		t.Errorf("operator_retry_nodes_reset = %v, want 5", s)
	}

	// Not wired: records nothing and does not panic.
	var nilM *Metrics
	nilM.observeOperatorRetry("reset", 1)
}

func get(h http.Handler, path string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code
}
