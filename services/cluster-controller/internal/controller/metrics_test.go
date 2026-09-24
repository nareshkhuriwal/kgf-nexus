// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics/metricstest"
	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
)

// TestReconcileLoopMetrics drives real cycles through the fakes: a successful
// scale-out, then a cycle that fails on the resource manager.
func TestReconcileLoopMetrics(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1), newFakeRM("apply")
	registerAll(fc, rm)
	rm.demand = &rmv1.Demand{PendingTasks: 10}
	c := newController(t, fc, rm, true, clk)
	reg := metrics.NewRegistry()
	c.SetMetrics(NewMetrics(reg))

	mustRun(t, c)
	rm.failPoolState = status.Error(codes.Unavailable, "resource manager restarting")
	if err := c.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the second cycle to fail")
	}

	fams := metricstest.Scrape(t, reg.Handler(), "/metrics")
	metricstest.RequireFamilies(t, fams,
		"nexus_controller_reconcile_cycles_total", "nexus_controller_reconcile_duration_seconds",
		"nexus_controller_pool_reconcile_errors_total", "nexus_controller_scale_actions_total",
		"nexus_controller_pool_replicas", "nexus_controller_last_success_timestamp_seconds")

	for result, want := range map[string]float64{"ok": 1, "error": 1} {
		if s := metricstest.Find(fams, "nexus_controller_reconcile_cycles_total",
			map[string]string{"result": result}); s == nil || s.GetCounter().GetValue() != want {
			t.Errorf("cycles{%s} = %v, want %v", result, s, want)
		}
	}
	pool := rm.binding().GetPool()
	if s := metricstest.Find(fams, "nexus_controller_scale_actions_total", map[string]string{
		"pool": pool, "action": "scale_out", "mode": "apply", "applied": "true",
	}); s == nil || s.GetCounter().GetValue() != 1 {
		t.Errorf("scale_out action = %v, want 1 applied", s)
	}
	if s := metricstest.Find(fams, "nexus_controller_pool_replicas",
		map[string]string{"pool": pool, "kind": "desired"}); s == nil || s.GetGauge().GetValue() != 4 {
		t.Errorf("desired replicas = %v, want 4", s)
	}
	if s := metricstest.Find(fams, "nexus_controller_pool_reconcile_errors_total",
		map[string]string{"pool": pool}); s == nil || s.GetCounter().GetValue() != 1 {
		t.Errorf("pool errors = %v, want 1", s)
	}
}
