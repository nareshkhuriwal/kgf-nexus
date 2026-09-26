// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/khuriwalgroup/kgf-nexus/internal/artifactgc"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics/metricstest"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestSweepMetricsServed: the batch job's families are served on its optional
// listener with the numbers from the pass.
func TestSweepMetricsServed(t *testing.T) {
	gm := newGCMetrics()
	gm.sweep(artifactgc.Stats{Listed: 120, LiveRefs: 90, Reachable: 88, TooYoung: 2, Orphans: 30,
		OrphanBytes: 4096, DryRun: true, Duration: 3 * time.Second})
	gm.finish(nil)

	fams := metricstest.Scrape(t, metrics.OpsMux(gm.reg), "/metrics")
	metricstest.RequireFamilies(t, fams, "nexus_artifact_gc_last_run_objects",
		"nexus_artifact_gc_last_run_orphan_bytes", "nexus_artifact_gc_last_success_timestamp_seconds",
		"nexus_artifact_gc_dry_run", metrics.BuildInfoName)
	for d, want := range map[string]float64{"scanned": 120, "orphan": 30, "deleted": 0} {
		if s := metricstest.Find(fams, "nexus_artifact_gc_last_run_objects",
			map[string]string{"disposition": d}); s == nil || s.GetGauge().GetValue() != want {
			t.Errorf("objects{%s} = %v, want %v", d, s, want)
		}
	}
}

// TestLogCollectionMetrics: the task-log share of a deleting pass is exposed as
// objects and bytes by disposition.
func TestLogCollectionMetrics(t *testing.T) {
	gm := newGCMetrics()
	gm.sweep(artifactgc.Stats{Listed: 50, LogObjects: 20, Orphans: 12, LogOrphans: 10,
		LogOrphanBytes: 1 << 20, Deleted: 11, LogDeleted: 9, LogDeletedBytes: 900 << 10,
		DeleteErrors: 1, LogDeleteErrors: 1})
	gm.finish(nil)

	fams := metricstest.Scrape(t, metrics.OpsMux(gm.reg), "/metrics")
	metricstest.RequireFamilies(t, fams, "nexus_artifact_gc_last_run_log_objects",
		"nexus_artifact_gc_last_run_log_bytes")
	for d, want := range map[string]float64{"scanned": 20, "orphan": 10, "deleted": 9, "delete_error": 1} {
		if s := metricstest.Find(fams, "nexus_artifact_gc_last_run_log_objects",
			map[string]string{"disposition": d}); s == nil || s.GetGauge().GetValue() != want {
			t.Errorf("log_objects{%s} = %v, want %v", d, s, want)
		}
	}
	for d, want := range map[string]float64{"orphan": 1 << 20, "deleted": 900 << 10} {
		if s := metricstest.Find(fams, "nexus_artifact_gc_last_run_log_bytes",
			map[string]string{"disposition": d}); s == nil || s.GetGauge().GetValue() != want {
			t.Errorf("log_bytes{%s} = %v, want %v", d, s, want)
		}
	}
}

// TestPushSendsOnlyTheJobsFamilies: the Pushgateway receives the job's
// nexus_* numbers, and none of the go_/process_ series of a process that is
// about to exit.
func TestPushSendsOnlyTheJobsFamilies(t *testing.T) {
	var (
		mu     sync.Mutex
		method string
		path   string
		body   []byte
		ctype  string
	)
	pgw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		method, path, ctype = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer pgw.Close()

	gm := newGCMetrics()
	gm.check(artifactgc.DanglingReport{RowsScanned: 10, RefsChecked: 8, MissingKeys: 1})
	gm.finish(errors.New("1 dangling reference"))
	gm.push(pgw.URL, testLogger())

	mu.Lock()
	defer mu.Unlock()
	if method != http.MethodPut || !strings.HasPrefix(path, "/metrics/job/artifact-gc") {
		t.Fatalf("push = %s %s, want PUT /metrics/job/artifact-gc", method, path)
	}
	fams := decode(t, ctype, body)
	if _, ok := fams["nexus_artifact_gc_last_check_refs"]; !ok {
		t.Errorf("pushed families %v lack the check result", keys(fams))
	}
	if _, ok := fams["nexus_artifact_gc_last_failure_timestamp_seconds"]; !ok {
		t.Error("the failure timestamp was not pushed")
	}
	for name := range fams {
		if strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") {
			t.Errorf("pushed runtime family %s from an exiting process", name)
		}
	}
}

func decode(t *testing.T, contentType string, body []byte) map[string]*dto.MetricFamily {
	t.Helper()
	dec := expfmt.NewDecoder(bytes.NewReader(body), expfmt.ResponseFormat(http.Header{"Content-Type": {contentType}}))
	out := map[string]*dto.MetricFamily{}
	for {
		mf := &dto.MetricFamily{}
		if err := dec.Decode(mf); err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			t.Fatalf("decode pushed body: %v", err)
		}
		out[mf.GetName()] = mf
	}
}

func keys(m map[string]*dto.MetricFamily) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
