// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics/metricstest"
	"github.com/khuriwalgroup/kgf-nexus/services/cluster-controller/internal/controller"
)

// TestMetricsServedOnHealthListener: the controller's scrape target is its
// health listener. The controller itself is nil, so the scrape cannot depend
// on the reconcile loop or on the resource manager.
func TestMetricsServedOnHealthListener(t *testing.T) {
	reg := metrics.NewServiceRegistry("cluster-controller", "test")
	var leading controller.Leading
	reg.NewGaugeFunc("nexus_controller_leader", "leader", func() float64 {
		if leading.Is() {
			return 1
		}
		return 0
	})
	srv := healthServer(":0", nil, &leading, 15*time.Second, reg)

	fams := metricstest.Scrape(t, srv.Handler, "/metrics")
	metricstest.RequireFamilies(t, fams, "nexus_controller_leader", "go_goroutines", metrics.BuildInfoName)
	if v := fams["nexus_controller_leader"].GetMetric()[0].GetGauge().GetValue(); v != 0 {
		t.Errorf("leader = %v on a standby, want 0", v)
	}
}
