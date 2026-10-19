//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package instances

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/khuriwalgroup/kgf-nexus/internal/instances/identity"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
)

// TestIntegrationHeartbeatLifecycle runs a real heartbeat against a real
// database, with readiness asked of a real /readyz handler: the row appears
// ready, follows the handler to not ready, and is marked not ready and
// shutting down when the process stops.
func TestIntegrationHeartbeatLifecycle(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.DSN(t))
	if err != nil {
		testdb.SkipUnreachable(t, err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		testdb.SkipUnreachable(t, err)
	}
	pod := fmt.Sprintf("it-heartbeat-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM service_instances WHERE instance = $1`, pod) })

	var draining atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if draining.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"draining"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	h, err := New(pool, Config{
		Component: EventService, Version: "it",
		Runtime:  identity.Runtime{Pod: pod, PodNamespace: "kgf-nexus", Node: "node-it", Cluster: "aks-it"},
		Ready:    ProbeHandler(mux, "/readyz"),
		Pool:     pool,
		Interval: 20 * time.Millisecond,
	}, metrics.NewRegistry(), nil)
	if err != nil {
		t.Fatal(err)
	}

	read := func() (store.ServiceInstanceView, bool) {
		_, rows, err := store.GatewayListServiceInstances(ctx, pool, StaleAfter)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.Component == EventService && r.Instance == pod {
				return r, true
			}
		}
		return store.ServiceInstanceView{}, false
	}
	waitUntil := func(what string, cond func(store.ServiceInstanceView) bool) store.ServiceInstanceView {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if r, ok := read(); ok && cond(r) {
				return r
			}
			if time.Now().After(deadline) {
				r, _ := read()
				t.Fatalf("%s: never reached; last row %+v", what, r)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	stop := Start(ctx, h)
	r := waitUntil("ready", func(r store.ServiceInstanceView) bool { return r.Ready })
	if r.Stale || r.Node != "node-it" || r.Cluster != "aks-it" || r.PodNamespace != "kgf-nexus" || r.Version != "it" {
		t.Errorf("row = %+v", r)
	}

	draining.Store(true)
	waitUntil("not ready while draining", func(r store.ServiceInstanceView) bool {
		return !r.Ready && strings.Contains(string(r.Detail), `"readiness": "draining"`)
	})

	draining.Store(false)
	waitUntil("ready again", func(r store.ServiceInstanceView) bool { return r.Ready })
	stop()
	r, _ = read()
	if r.Ready || !strings.Contains(string(r.Detail), `"shutting_down": true`) {
		t.Fatalf("after stop: %+v %s", r, r.Detail)
	}
}
