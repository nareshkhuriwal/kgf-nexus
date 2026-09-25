// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package metrics_test

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics/metricstest"
)

// TestExpositionParsesWithReferenceParser is the format contract: everything a
// Nexus process can emit -- every metric type, labels needing escaping, a
// histogram, the runtime/process/build-info collectors -- must parse with the
// parser Prometheus itself uses, with HELP and TYPE for every family.
func TestExpositionParsesWithReferenceParser(t *testing.T) {
	reg := metrics.NewServiceRegistry("test-service", "1.2.3")

	c := reg.NewCounter("nexus_test_events_total", "Events.\nSecond line with a \\ backslash.", "kind", "detail")
	c.With(metrics.Labels{"kind": "a", "detail": "quote\" backslash\\ newline\n"}).Add(3)
	c.With(metrics.Labels{"kind": "b", "detail": ""}).Inc()

	g := reg.NewGauge("nexus_test_depth", "Depth.", "queue")
	g.With(metrics.Labels{"queue": "default"}).Set(42)
	g.With(metrics.Labels{"queue": "gpu"}).Set(math.Inf(1))

	h := reg.NewHistogram("nexus_test_latency_seconds", "Latency.", []float64{0.1, 0.5, 1}, "op")
	for _, v := range []float64{0.05, 0.3, 0.3, 9} {
		h.With(metrics.Labels{"op": "claim"}).Observe(v)
	}
	reg.NewGaugeFunc("nexus_test_func", "A function gauge.", func() float64 { return 7 })

	fams := metricstest.Scrape(t, reg.Handler(), "/metrics")

	// Every family carries HELP and TYPE.
	for name, mf := range fams {
		if mf.GetHelp() == "" {
			t.Errorf("%s has no HELP line", name)
		}
		if mf.Type == nil {
			t.Errorf("%s has no TYPE line", name)
		}
	}

	// The standard collectors a production Go service is expected to expose.
	metricstest.RequireFamilies(t, fams,
		"go_goroutines", "go_gc_duration_seconds", "go_memstats_alloc_bytes",
		metrics.BuildInfoName,
		"nexus_test_events_total", "nexus_test_depth", "nexus_test_latency_seconds", "nexus_test_func")
	if process := fams["process_start_time_seconds"]; process == nil && processCollectorSupported() {
		t.Errorf("process collector is missing on a platform that supports it")
	}

	bi := metricstest.Find(fams, metrics.BuildInfoName, map[string]string{"service": "test-service", "version": "1.2.3"})
	if bi == nil || bi.GetGauge().GetValue() != 1 {
		t.Fatalf("build info series missing or not 1: %v", fams[metrics.BuildInfoName])
	}

	// Escaping survives the round trip exactly.
	esc := metricstest.Find(fams, "nexus_test_events_total", map[string]string{"kind": "a"})
	if esc == nil {
		t.Fatal("escaped series missing")
	}
	if got := labelValue(esc, "detail"); got != "quote\" backslash\\ newline\n" {
		t.Errorf("label value round-tripped as %q", got)
	}
	if esc.GetCounter().GetValue() != 3 {
		t.Errorf("counter = %v, want 3", esc.GetCounter().GetValue())
	}
	if got := fams["nexus_test_events_total"].GetHelp(); got != "Events.\nSecond line with a \\ backslash." {
		t.Errorf("HELP round-tripped as %q", got)
	}

	// Histogram shape: cumulative buckets with le, +Inf == count, _sum, _count.
	hm := metricstest.Find(fams, "nexus_test_latency_seconds", map[string]string{"op": "claim"})
	if hm == nil || fams["nexus_test_latency_seconds"].GetType() != dto.MetricType_HISTOGRAM {
		t.Fatalf("histogram missing or mistyped: %v", fams["nexus_test_latency_seconds"])
	}
	hist := hm.GetHistogram()
	want := map[float64]uint64{0.1: 1, 0.5: 3, 1: 3}
	var sawInf bool
	for _, b := range hist.GetBucket() {
		ub := b.GetUpperBound()
		if math.IsInf(ub, 1) {
			sawInf = true
			if b.GetCumulativeCount() != hist.GetSampleCount() {
				t.Errorf("+Inf bucket = %d, want _count %d", b.GetCumulativeCount(), hist.GetSampleCount())
			}
			continue
		}
		if w, ok := want[ub]; !ok || b.GetCumulativeCount() != w {
			t.Errorf("bucket le=%v = %d, want %d", ub, b.GetCumulativeCount(), w)
		}
	}
	if !sawInf {
		// The text parser materializes +Inf from the explicit le="+Inf" line.
		t.Error("no le=\"+Inf\" bucket in the exposition")
	}
	if hist.GetSampleCount() != 4 || math.Abs(hist.GetSampleSum()-9.65) > 1e-9 {
		t.Errorf("_count=%d _sum=%v, want 4 and 9.65", hist.GetSampleCount(), hist.GetSampleSum())
	}
}

// TestExpositionEdgeCasesStayParseable pins the defects the hand-written encoder
// had (ADR-0019): each of these used to produce an exposition that Prometheus
// rejects for the WHOLE target.
func TestExpositionEdgeCasesStayParseable(t *testing.T) {
	reg := metrics.NewRegistry()

	// A duplicated or +Inf bucket bound is dropped, not emitted twice.
	h := reg.NewHistogram("nexus_dup_seconds", "help", []float64{1, 0.5, 1, math.Inf(1)})
	h.Histogram().Observe(0.7)

	// A label set that disagrees with the declaration is normalized rather than
	// producing a family whose series carry different label names.
	c := reg.NewCounter("nexus_mismatch_total", "help", "a", "b")
	c.With(metrics.Labels{"a": "1", "b": "2"}).Inc()
	c.With(metrics.Labels{"a": "1"}).Inc()                       // missing b
	c.With(metrics.Labels{"a": "1", "b": "2", "zzz": "x"}).Inc() // undeclared zzz

	// Label values containing the old key separators do not collide.
	k := reg.NewCounter("nexus_collide_total", "help", "a", "b")
	k.With(metrics.Labels{"a": "1,b=2", "b": ""}).Inc()
	k.With(metrics.Labels{"a": "1", "b": "2"}).Add(10)

	fams := metricstest.Scrape(t, reg.Handler(), "/metrics")

	var bounds []float64
	for _, b := range fams["nexus_dup_seconds"].GetMetric()[0].GetHistogram().GetBucket() {
		bounds = append(bounds, b.GetUpperBound())
	}
	if len(bounds) != 3 || bounds[0] != 0.5 || bounds[1] != 1 || !math.IsInf(bounds[2], 1) {
		t.Errorf("buckets = %v, want [0.5 1 +Inf]", bounds)
	}

	if m := metricstest.Find(fams, "nexus_mismatch_total", map[string]string{"a": "1", "b": "2"}); m == nil || m.GetCounter().GetValue() != 2 {
		t.Errorf("normalized series a=1,b=2 = %v, want 2 (the declared set and the one with an extra label)", m)
	}
	if m := metricstest.Find(fams, "nexus_mismatch_total", map[string]string{"a": "1", "b": ""}); m == nil || m.GetCounter().GetValue() != 1 {
		t.Errorf("series with the missing label filled empty = %v, want 1", m)
	}
	for _, m := range fams["nexus_mismatch_total"].GetMetric() {
		if len(m.GetLabel()) != 2 {
			t.Errorf("series %v does not carry exactly the declared labels", m.GetLabel())
		}
	}

	if m := metricstest.Find(fams, "nexus_collide_total", map[string]string{"a": "1", "b": "2"}); m == nil || m.GetCounter().GetValue() != 10 {
		t.Errorf("a=1,b=2 = %v, want 10: a value containing ',' or '=' collided with another label set", m)
	}
}

func TestHistogramRejectsLeLabel(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a histogram label named le was accepted; it produces {le=...,le=...}, which does not parse")
		}
	}()
	metrics.NewRegistry().NewHistogram("nexus_bad_seconds", "help", nil, "le")
}

func TestReservedLabelPrefixPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a __-prefixed label name was accepted")
		}
	}()
	metrics.NewRegistry().NewCounter("nexus_bad_total", "help", "__name")
}

// TestHandlerServesGzipWhenAsked: Prometheus scrapes with Accept-Encoding: gzip;
// the Go runtime collector alone is several KiB per scrape.
func TestHandlerServesGzipWhenAsked(t *testing.T) {
	reg := metrics.NewServiceRegistry("gz", "")
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
}

func TestWriteToMatchesHandlerFamilies(t *testing.T) {
	reg := metrics.NewRegistry()
	reg.NewCounter("nexus_a_total", "help").Counter().Inc()
	var b strings.Builder
	reg.WriteTo(&b)
	fams := metricstest.Parse(t, strings.NewReader(b.String()))
	metricstest.RequireFamilies(t, fams, "nexus_a_total")
}

func labelValue(m *dto.Metric, name string) string {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}
