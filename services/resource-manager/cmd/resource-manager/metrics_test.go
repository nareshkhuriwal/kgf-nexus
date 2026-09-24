// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync/atomic"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics/metricstest"
)

// TestMetricsServedOnHealthListener: the resource manager's scrape target is
// its health listener. The pool is nil -- a scrape must not need the database.
func TestMetricsServedOnHealthListener(t *testing.T) {
	reg := metrics.NewServiceRegistry("resource-manager", "test")
	var ready atomic.Bool
	srv := healthServer(":0", nil, &ready, reg)

	fams := metricstest.Scrape(t, srv.Handler, "/metrics")
	metricstest.RequireFamilies(t, fams, "go_goroutines", metrics.BuildInfoName)
	if s := metricstest.Find(fams, metrics.BuildInfoName, map[string]string{"service": "resource-manager"}); s == nil {
		t.Error("build info does not name the service")
	}
}
