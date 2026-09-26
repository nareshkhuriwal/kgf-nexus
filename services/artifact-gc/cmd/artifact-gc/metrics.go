// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/khuriwalgroup/kgf-nexus/internal/objstore"

	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
	dto "github.com/prometheus/client_model/go"

	"github.com/khuriwalgroup/kgf-nexus/internal/artifactgc"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
)

// gcMetrics are artifact-gc's instruments.
//
// artifact-gc is a BATCH job: one pass, then exit. A pull scrape of a process
// that lives for seconds usually misses it entirely, so the job supports the two
// standard batch patterns and an operator picks one:
//
//   - -metrics-pushgateway URL: push the final values to a Prometheus
//     Pushgateway under job="artifact-gc" when the pass ends (success or
//     failure). This is the one to alert on.
//   - -metrics-addr ADDR: serve /metrics while the pass runs, for watching a
//     long sweep live.
//
// The "last run" gauges are what a batch job exposes by convention: after the
// process is gone, the Pushgateway keeps its final numbers, and
// time() - nexus_artifact_gc_last_success_timestamp_seconds is the alert that
// the scheduled sweep stopped running.
type gcMetrics struct {
	// objstore counts the pass's list, stat and delete calls by backend.
	objstore *objstore.Metrics
	reg      *metrics.Registry

	objects     *metrics.GaugeVec
	orphanBytes *metrics.Gauge
	duration    *metrics.Gauge
	lastSuccess *metrics.Gauge
	lastFailure *metrics.Gauge
	dryRun      *metrics.Gauge
	dangling    *metrics.GaugeVec
}

func newGCMetrics() *gcMetrics {
	reg := metrics.NewServiceRegistry("artifact-gc", version)
	return &gcMetrics{
		reg:      reg,
		objstore: objstore.NewMetrics(reg),
		objects: reg.NewGauge("nexus_artifact_gc_last_run_objects",
			"Objects in the last sweep, by disposition: scanned (listed), live_refs, reachable, too_young (inside the grace period), orphan (collectable), deleted, delete_error.",
			"disposition"),
		orphanBytes: reg.NewGauge("nexus_artifact_gc_last_run_orphan_bytes",
			"Reclaimable bytes across the orphans found by the last sweep.").Gauge(),
		duration: reg.NewGauge("nexus_artifact_gc_last_run_duration_seconds",
			"Wall time of the last pass.").Gauge(),
		lastSuccess: reg.NewGauge("nexus_artifact_gc_last_success_timestamp_seconds",
			"Unix time the last pass completed without error.").Gauge(),
		lastFailure: reg.NewGauge("nexus_artifact_gc_last_failure_timestamp_seconds",
			"Unix time the last pass failed.").Gauge(),
		dryRun: reg.NewGauge("nexus_artifact_gc_dry_run",
			"1 when the last pass was a dry run (nothing deleted), 0 when it deleted.").Gauge(),
		dangling: reg.NewGauge("nexus_artifact_gc_last_check_refs",
			"The last -check-refs pass, by kind: rows_scanned, keys_checked, missing_keys (objects gone), dangling_rows. Nonzero missing_keys is a data-loss fault.",
			"kind"),
	}
}

func (m *gcMetrics) sweep(st artifactgc.Stats) {
	if m == nil {
		return
	}
	for k, v := range map[string]int{
		"scanned": st.Listed, "live_refs": st.LiveRefs, "reachable": st.Reachable,
		"too_young": st.TooYoung, "orphan": st.Orphans, "deleted": st.Deleted,
		"delete_error": st.DeleteErrors,
	} {
		m.objects.With(metrics.Labels{"disposition": k}).Set(float64(v))
	}
	m.orphanBytes.Set(float64(st.OrphanBytes))
	m.duration.Set(st.Duration.Seconds())
	if st.DryRun {
		m.dryRun.Set(1)
	} else {
		m.dryRun.Set(0)
	}
}

func (m *gcMetrics) check(rep artifactgc.DanglingReport) {
	if m == nil {
		return
	}
	for k, v := range map[string]int{
		"rows_scanned": rep.RowsScanned, "keys_checked": rep.RefsChecked,
		"missing_keys": rep.MissingKeys, "dangling_rows": len(rep.Dangling),
	} {
		m.dangling.With(metrics.Labels{"kind": k}).Set(float64(v))
	}
	m.duration.Set(rep.Duration.Seconds())
}

func (m *gcMetrics) finish(err error) {
	now := float64(time.Now().Unix())
	if err != nil {
		m.lastFailure.Set(now)
		return
	}
	m.lastSuccess.Set(now)
}

// serve starts the optional /metrics listener. It returns a stop function that
// is always safe to call.
func (m *gcMetrics) serve(addr string, log *slog.Logger) (func(), error) {
	ops, err := metrics.ListenOps(addr, metrics.OpsMux(m.reg, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	}), log)
	if err != nil {
		return func() {}, fmt.Errorf("listen on %s for metrics: %w", addr, err)
	}
	go func() {
		if err := ops.Serve(); err != nil {
			log.Warn("metrics listener stopped", "error", err)
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = ops.Shutdown(ctx)
	}, nil
}

// push delivers the final values to a Pushgateway. A push failure is logged, not
// fatal: the sweep's result is what the exit code reports, and failing a
// successful GC because monitoring was unreachable would be the wrong priority.
func (m *gcMetrics) push(url string, log *slog.Logger) {
	if url == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// PUT (Push, not Add): the group's previous values are replaced whole, so a
	// family this run did not emit (dangling vs sweep) cannot linger from a
	// previous run and be mistaken for current.
	// Only the job's own families: go_* and process_* describe a process that
	// is about to exit, and pushing them would keep a dead process's heap size
	// on the Pushgateway until the next run.
	own := prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
		mfs, err := m.reg.Gatherer().Gather()
		out := mfs[:0]
		for _, mf := range mfs {
			if strings.HasPrefix(mf.GetName(), "nexus_") {
				out = append(out, mf)
			}
		}
		return out, err
	})
	if err := push.New(url, "artifact-gc").Gatherer(own).PushContext(ctx); err != nil {
		log.Warn("could not push metrics to the Pushgateway", "url", url, "error", err)
		return
	}
	log.Info("metrics pushed to the Pushgateway", "url", url, "job", "artifact-gc")
}
