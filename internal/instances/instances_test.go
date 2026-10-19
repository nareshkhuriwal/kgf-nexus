// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package instances

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/khuriwalgroup/kgf-nexus/internal/instances/identity"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
)

// row is one upsert as the fake database received it.
type row struct {
	component, instance, podNS, node, cluster, version string
	startedAt                                          time.Time
	ready                                              bool
	detail                                             map[string]any
}

type fakeDB struct {
	mu      sync.Mutex
	rows    []row
	prunes  int
	failing error
}

func (f *fakeDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing != nil {
		return pgconn.NewCommandTag(""), f.failing
	}
	switch {
	case strings.Contains(sql, "INSERT INTO service_instances"):
		var d map[string]any
		if err := json.Unmarshal([]byte(args[8].(string)), &d); err != nil {
			return pgconn.NewCommandTag(""), err
		}
		f.rows = append(f.rows, row{
			component: args[0].(string), instance: args[1].(string), podNS: args[2].(string),
			node: args[3].(string), cluster: args[4].(string), version: args[5].(string),
			startedAt: args[6].(time.Time), ready: args[7].(bool), detail: d,
		})
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case strings.Contains(sql, "DELETE FROM service_instances"):
		f.prunes++
		return pgconn.NewCommandTag("DELETE 0"), nil
	}
	return pgconn.NewCommandTag(""), errors.New("fakeDB: unexpected statement: " + sql)
}

func (f *fakeDB) last(t *testing.T) row {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rows) == 0 {
		t.Fatal("no row was written")
	}
	return f.rows[len(f.rows)-1]
}

func (f *fakeDB) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func newTestHeartbeat(t *testing.T, db *fakeDB, cfg Config, logs *bytes.Buffer) *Heartbeat {
	t.Helper()
	if cfg.Component == "" {
		cfg.Component = Scheduler
	}
	out := bytes.NewBuffer(nil)
	if logs != nil {
		out = logs
	}
	h, err := New(db, cfg, metrics.NewRegistry(), slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestBeatWritesTheProcessRow(t *testing.T) {
	db := &fakeDB{}
	started := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	h := newTestHeartbeat(t, db, Config{
		Component: Scheduler, Version: "1.9.0", StartedAt: started,
		Runtime: identity.Runtime{Pod: "sched-0", PodNamespace: "kgf-nexus", Node: "node-a", Cluster: "aks-prod"},
		Ready:   func(context.Context) (bool, string) { return true, "" },
		Detail:  func() map[string]any { return map[string]any{"queue": "default", "claims_total": 7} },
	}, nil)

	h.Beat(context.Background())
	r := db.last(t)
	if r.component != Scheduler || r.instance != "sched-0" || r.podNS != "kgf-nexus" ||
		r.node != "node-a" || r.cluster != "aks-prod" || r.version != "1.9.0" || !r.startedAt.Equal(started) {
		t.Errorf("row = %+v", r)
	}
	if !r.ready {
		t.Error("a ready process wrote ready = false")
	}
	if r.detail["queue"] != "default" || r.detail["claims_total"] != float64(7) {
		t.Errorf("component detail lost: %v", r.detail)
	}
	if r.detail[DetailReadiness] != "ok" || r.detail[DetailPID] != float64(os.Getpid()) ||
		r.detail[DetailIntervalSeconds] != DefaultInterval.Seconds() {
		t.Errorf("common detail keys wrong: %v", r.detail)
	}
	if _, ok := r.detail[DetailShuttingDown]; ok {
		t.Error("an ordinary beat claims to be shutting down")
	}
}

// TestBeatReportsTheReadinessReason: the reason is what the console shows
// next to a not-ready replica, and a component cannot overwrite the common
// keys with its own meaning.
func TestBeatReportsTheReadinessReason(t *testing.T) {
	db := &fakeDB{}
	h := newTestHeartbeat(t, db, Config{
		Ready:  func(context.Context) (bool, string) { return false, "degraded: postgres unreachable" },
		Detail: func() map[string]any { return map[string]any{DetailReadiness: "spoofed"} },
	}, nil)
	h.Beat(context.Background())
	r := db.last(t)
	if r.ready {
		t.Fatal("a not-ready process wrote ready = true")
	}
	if r.detail[DetailReadiness] != "degraded: postgres unreachable" {
		t.Errorf("readiness = %v", r.detail[DetailReadiness])
	}
}

func TestInstanceFallsBackToTheHostName(t *testing.T) {
	h := newTestHeartbeat(t, &fakeDB{}, Config{}, nil)
	host, _ := os.Hostname()
	if h.Instance() != identity.Clean(host) || h.Instance() == "" {
		t.Fatalf("instance = %q, want the host name %q", h.Instance(), host)
	}
}

func TestNewRefusesAnUnknownComponent(t *testing.T) {
	if _, err := New(&fakeDB{}, Config{Component: "cluster-controller"}, nil, nil); err == nil {
		t.Fatal("an unknown component was accepted; the API would never list it")
	}
	if _, err := New(nil, Config{Component: Scheduler}, nil, nil); err == nil {
		t.Fatal("a heartbeat without a database was accepted")
	}
}

// TestShutdownMarksNotReady is the graceful-shutdown half of the contract:
// cancelling the context ends the loop with one not-ready, shutting_down row,
// and stop waits for it.
func TestShutdownMarksNotReady(t *testing.T) {
	db := &fakeDB{}
	h := newTestHeartbeat(t, db, Config{Interval: time.Hour}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	stop := Start(ctx, h)
	waitFor(t, func() bool { return db.count() == 1 })
	cancel()
	stop()
	stop() // idempotent

	r := db.last(t)
	if r.ready || r.detail[DetailShuttingDown] != true || r.detail[DetailReadiness] != "shutting down" {
		t.Fatalf("final row = ready %v detail %v, want not ready and shutting_down", r.ready, r.detail)
	}
	if n := db.count(); n != 2 {
		t.Errorf("rows = %d, want the first beat and the shutdown write", n)
	}
}

// TestStopAloneEndsTheLoop: a service whose run() returns early (a listener
// failed) still gets its row marked not ready by the deferred stop.
func TestStopAloneEndsTheLoop(t *testing.T) {
	db := &fakeDB{}
	h := newTestHeartbeat(t, db, Config{Interval: time.Hour}, nil)
	stop := Start(context.Background(), h)
	waitFor(t, func() bool { return db.count() == 1 })
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return")
	}
	if db.last(t).ready {
		t.Fatal("stop did not write the not-ready row")
	}
}

// TestBeatsRepeatAtTheInterval: the loop keeps writing while the process runs.
func TestBeatsRepeatAtTheInterval(t *testing.T) {
	db := &fakeDB{}
	h := newTestHeartbeat(t, db, Config{Interval: 10 * time.Millisecond}, nil)
	stop := Start(context.Background(), h)
	defer stop()
	waitFor(t, func() bool { return db.count() >= 3 })
}

// TestFailuresAreMeteredAndLoggedOncePerOutage: a database outage costs one
// WARN line, every failed beat is counted, and recovery is logged.
func TestFailuresAreMeteredAndLoggedOncePerOutage(t *testing.T) {
	db := &fakeDB{failing: errors.New("connection refused")}
	var logs bytes.Buffer
	h := newTestHeartbeat(t, db, Config{}, &logs)
	for i := 0; i < 3; i++ {
		h.Beat(context.Background())
	}
	if got := h.metrics.failures.With(metrics.Labels{"op": "beat"}).Value(); got != 3 {
		t.Errorf("beat failures = %v, want 3", got)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("WARN lines = %d, want 1 for one outage:\n%s", n, logs.String())
	}
	if h.metrics.lastSuccess.Value() != 0 {
		t.Error("the last-success gauge moved without a success")
	}

	db.mu.Lock()
	db.failing = nil
	db.mu.Unlock()
	h.Beat(context.Background())
	if !strings.Contains(logs.String(), "service instance heartbeat recovered") {
		t.Error("recovery was not logged")
	}
	if h.metrics.lastSuccess.Value() == 0 {
		t.Error("the last-success gauge did not move on success")
	}
}

func TestPruneRunsOnTheFirstBeatThenAtItsInterval(t *testing.T) {
	db := &fakeDB{}
	h := newTestHeartbeat(t, db, Config{PruneInterval: 10 * time.Minute}, nil)
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }

	h.Beat(context.Background())
	h.Beat(context.Background())
	if db.prunes != 1 {
		t.Fatalf("prunes after two quick beats = %d, want 1", db.prunes)
	}
	now = now.Add(10 * time.Minute)
	h.Beat(context.Background())
	if db.prunes != 2 {
		t.Fatalf("prunes after the interval = %d, want 2", db.prunes)
	}
}

func TestVersionIsBounded(t *testing.T) {
	db := &fakeDB{}
	h := newTestHeartbeat(t, db, Config{Version: strings.Repeat("v", 300)}, nil)
	h.Beat(context.Background())
	if n := len(db.last(t).version); n != 128 {
		t.Errorf("version length = %d, want the column's 128", n)
	}
}

func TestComponentsAreTheContractOrder(t *testing.T) {
	want := "api-gateway,workflow-manager,dag-engine,scheduler,resource-manager,event-service"
	if got := strings.Join(Components(), ","); got != want {
		t.Fatalf("Components() = %s, want %s", got, want)
	}
}

func TestProbeHandlerAsksTheReadinessHandler(t *testing.T) {
	cases := []struct {
		name       string
		h          http.HandlerFunc
		wantReady  bool
		wantReason string
	}{
		{"ok JSON", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		}, true, "ok"},
		{"draining", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"draining","detail":"shutting down; not accepting new traffic"}`))
		}, false, "draining: shutting down; not accepting new traffic"},
		{"scheduler degraded", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"degraded","err":"dispatch loop: timeout"}`))
		}, false, "degraded: dispatch loop: timeout"},
		{"health checks", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"not_ready","checks":{"postgres":"timeout","kgf_context_keys":"ok","a":"x"}}`))
		}, false, "not_ready: a: x: postgres: timeout"},
		{"plain text", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "metadata store unreachable: refused", http.StatusServiceUnavailable)
		}, false, "metadata store unreachable: refused"},
		{"empty 503", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}, false, "Service Unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.Handle("GET /readyz", tc.h)
			ready, reason := ProbeHandler(mux, "/readyz")(context.Background())
			if ready != tc.wantReady || reason != tc.wantReason {
				t.Errorf("probe = (%v, %q), want (%v, %q)", ready, reason, tc.wantReady, tc.wantReason)
			}
		})
	}
}

// TestProbeBodyIsBounded: a handler that writes without end cannot grow the
// heartbeat's memory.
func TestProbeBodyIsBounded(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		for i := 0; i < 100; i++ {
			_, _ = w.Write(bytes.Repeat([]byte("x"), 1024))
		}
	})
	_, reason := ProbeHandler(h, "/readyz")(context.Background())
	if len(reason) > maxProbeBody {
		t.Fatalf("reason is %d bytes, want at most %d", len(reason), maxProbeBody)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
