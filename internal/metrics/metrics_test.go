// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// render is the assertion workhorse: almost every property of this package is
// observable only in the exposition text a scraper would actually read.
func render(r *Registry) string {
	var b strings.Builder
	r.WriteTo(&b)
	return b.String()
}

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("exposition is missing %q\n--- got ---\n%s", want, got)
	}
}

// ── concurrency ───────────────────────────────────────────────────────────

// TestCounterConcurrentInc: every increment must land. A counter that loses
// increments under load understates exactly the incident it exists to measure, and
// does so without any error anywhere.
func TestCounterConcurrentInc(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("nexus_test_total", "help").Counter()

	const goroutines, each = 64, 1000
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				c.Inc()
			}
		}()
	}
	wg.Wait()

	if got, want := c.Value(), float64(goroutines*each); got != want {
		t.Errorf("Value = %v, want %v; increments were lost under contention", got, want)
	}
}

// TestGaugeConcurrentAddDec: the in-flight-requests pattern. Equal Inc and Dec from
// many goroutines must settle back to the starting value.
func TestGaugeConcurrentAddDec(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("nexus_test_inflight", "help").Gauge()

	var wg sync.WaitGroup
	wg.Add(100)
	for i := 0; i < 100; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				g.Inc()
				g.Dec()
			}
		}()
	}
	wg.Wait()

	if got := g.Value(); got != 0 {
		t.Errorf("Value = %v, want 0; matched Inc/Dec pairs did not cancel", got)
	}
}

// TestHistogramConcurrentObserve: count and sum must both be exact under
// contention, since a percentile computed from a partially-recorded histogram is
// wrong in a way no alert can detect.
func TestHistogramConcurrentObserve(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("nexus_test_seconds", "help", []float64{0.5, 1, 2}).Histogram()

	const goroutines, each = 32, 500
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				h.Observe(1.0)
			}
		}()
	}
	wg.Wait()

	cumulative, sum, count := h.Snapshot()
	if want := uint64(goroutines * each); count != want {
		t.Errorf("count = %d, want %d", count, want)
	}
	if want := float64(goroutines * each); sum != want {
		t.Errorf("sum = %v, want %v", sum, want)
	}
	// 1.0 falls in the le=1 bucket, so le=0.5 stays empty and le=1 and le=2 both
	// hold everything — cumulative semantics.
	if cumulative[0] != 0 {
		t.Errorf("le=0.5 bucket = %d, want 0", cumulative[0])
	}
	if cumulative[1] != count || cumulative[2] != count {
		t.Errorf("cumulative = %v, want the le=1 and le=2 buckets to both hold %d", cumulative, count)
	}
}

// TestConcurrentWithRacesToOneSeries: two goroutines asking for the same label set
// must get the SAME instance. Handing out two would silently split one series, and
// the scrape would then emit a duplicate line that breaks the whole parse.
func TestConcurrentWithRacesToOneSeries(t *testing.T) {
	r := NewRegistry()
	v := r.NewCounter("nexus_test_labelled_total", "help", "queue")

	var wg sync.WaitGroup
	wg.Add(50)
	for i := 0; i < 50; i++ {
		go func() {
			defer wg.Done()
			v.With(Labels{"queue": "default"}).Inc()
		}()
	}
	wg.Wait()

	if got := v.With(Labels{"queue": "default"}).Value(); got != 50 {
		t.Errorf("Value = %v, want 50; concurrent With calls split the series", got)
	}
	if n := strings.Count(render(r), "nexus_test_labelled_total{"); n != 1 {
		t.Errorf("exposition has %d series for one label set, want 1", n)
	}
}

// TestConcurrentScrapeDuringWrites is a race-detector target: scraping while
// counters move must not corrupt anything. Run with -race, this is the test that
// proves /metrics is safe to expose on a live process.
func TestConcurrentScrapeDuringWrites(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("nexus_test_churn_total", "help", "kind")
	h := r.NewHistogram("nexus_test_churn_seconds", "help", nil, "kind")

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	for i := 0; i < 2; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-done:
					return
				default:
				}
				lbl := Labels{"kind": fmt.Sprintf("k%d", j%4)}
				c.With(lbl).Inc()
				h.With(lbl).Observe(float64(j%100) / 100)
			}
		}(i)
	}
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = render(r)
		}
		close(done)
	}()
	wg.Wait()
}

// ── text format ───────────────────────────────────────────────────────────

func TestCounterTextFormat(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("nexus_attempts_total", "Attempts by terminal state.", "state")
	c.With(Labels{"state": "succeeded"}).Add(3)
	c.With(Labels{"state": "failed"}).Inc()

	got := render(r)
	mustContain(t, got, "# HELP nexus_attempts_total Attempts by terminal state.\n")
	mustContain(t, got, "# TYPE nexus_attempts_total counter\n")
	mustContain(t, got, `nexus_attempts_total{state="succeeded"} 3`)
	mustContain(t, got, `nexus_attempts_total{state="failed"} 1`)
	if n := strings.Count(got, "# TYPE nexus_attempts_total"); n != 1 {
		t.Errorf("TYPE line appears %d times, want 1; a repeated TYPE fails the scrape parse", n)
	}
}

func TestGaugeTextFormatWithMultipleLabels(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("nexus_queue_depth", "Ready tasks per queue.", "namespace", "queue")
	g.With(Labels{"namespace": "prod", "queue": "gpu"}).Set(42)

	got := render(r)
	mustContain(t, got, "# TYPE nexus_queue_depth gauge\n")
	// Labels are emitted in sorted name order so two scrapes are byte-comparable.
	mustContain(t, got, `nexus_queue_depth{namespace="prod",queue="gpu"} 42`)
}

func TestUnlabelledMetricHasNoBraces(t *testing.T) {
	r := NewRegistry()
	r.NewGauge("nexus_timer_backlog_due", "Overdue timers.").Gauge().Set(7)

	got := render(r)
	mustContain(t, got, "nexus_timer_backlog_due 7\n")
	if strings.Contains(got, "nexus_timer_backlog_due{") {
		t.Errorf("an unlabelled metric emitted an empty label set:\n%s", got)
	}
}

func TestHistogramTextFormat(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("nexus_dispatch_seconds", "Dispatch latency.", []float64{0.1, 0.5, 1}, "queue")
	inst := h.With(Labels{"queue": "default"})
	inst.Observe(0.05) // le=0.1
	inst.Observe(0.3)  // le=0.5
	inst.Observe(9)    // +Inf only

	got := render(r)
	mustContain(t, got, "# TYPE nexus_dispatch_seconds histogram\n")
	mustContain(t, got, `nexus_dispatch_seconds_bucket{queue="default",le="0.1"} 1`)
	mustContain(t, got, `nexus_dispatch_seconds_bucket{queue="default",le="0.5"} 2`)
	mustContain(t, got, `nexus_dispatch_seconds_bucket{queue="default",le="1"} 2`)
	mustContain(t, got, `nexus_dispatch_seconds_bucket{queue="default",le="+Inf"} 3`)
	mustContain(t, got, `nexus_dispatch_seconds_sum{queue="default"} 9.35`)
	mustContain(t, got, `nexus_dispatch_seconds_count{queue="default"} 3`)
}

// TestHistogramInfBucketEqualsCount: histogram_quantile silently returns nonsense
// when +Inf disagrees with _count, so this invariant is worth its own test.
func TestHistogramInfBucketEqualsCount(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("nexus_wide_seconds", "help", []float64{0.01}).Histogram()
	for _, v := range []float64{0.001, 0.5, 100, math.Inf(1)} {
		h.Observe(v)
	}

	got := render(r)
	mustContain(t, got, `nexus_wide_seconds_bucket{le="+Inf"} 4`)
	mustContain(t, got, "nexus_wide_seconds_count 4\n")
}

func TestHistogramBucketsAreSortedRegardlessOfInput(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("nexus_unsorted_seconds", "help", []float64{1, 0.1, 0.5})
	h.Histogram().Observe(0.2)

	got := render(r)
	first := strings.Index(got, `le="0.1"`)
	mid := strings.Index(got, `le="0.5"`)
	last := strings.Index(got, `le="1"`)
	if first < 0 || mid < first || last < mid {
		t.Errorf("buckets are not ascending; histogram_quantile requires ordered le:\n%s", got)
	}
}

func TestEmptyFamilyEmitsNothing(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("nexus_never_observed_total", "help", "kind")

	if got := render(r); got != "" {
		t.Errorf("a family with no observations emitted %q, want nothing", got)
	}
}

func TestFamiliesAreEmittedInNameOrder(t *testing.T) {
	r := NewRegistry()
	r.NewGauge("zzz_last", "help").Gauge().Set(1)
	r.NewGauge("aaa_first", "help").Gauge().Set(1)

	got := render(r)
	if strings.Index(got, "aaa_first") > strings.Index(got, "zzz_last") {
		t.Errorf("families are not in name order, so two scrapes cannot be diffed:\n%s", got)
	}
}

// TestLabelValueEscaping: an unescaped quote or newline in a label value corrupts
// every subsequent line of the scrape, not just its own.
func TestLabelValueEscaping(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("nexus_escape_total", "help", "detail")
	c.With(Labels{"detail": "a\"b\\c\nd"}).Inc()

	mustContain(t, render(r), `nexus_escape_total{detail="a\"b\\c\nd"} 1`)
}

func TestHelpEscaping(t *testing.T) {
	r := NewRegistry()
	r.NewGauge("nexus_help_escape", "line one\nline two").Gauge().Set(1)

	got := render(r)
	mustContain(t, got, `# HELP nexus_help_escape line one\nline two`)
	if strings.Count(got, "\n# TYPE") != 1 {
		t.Errorf("a newline in HELP leaked into the exposition:\n%s", got)
	}
}

func TestSpecialFloatsUsePrometheusSpelling(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("nexus_special", "help", "v")
	g.With(Labels{"v": "pos"}).Set(math.Inf(1))
	g.With(Labels{"v": "neg"}).Set(math.Inf(-1))
	g.With(Labels{"v": "nan"}).Set(math.NaN())

	got := render(r)
	mustContain(t, got, `nexus_special{v="pos"} +Inf`)
	mustContain(t, got, `nexus_special{v="neg"} -Inf`)
	mustContain(t, got, `nexus_special{v="nan"} NaN`)
}

// TestLabelOrderDoesNotSplitSeries: map iteration order must not decide identity,
// or the same logical metric registers twice and the scrape has duplicate lines.
func TestLabelOrderDoesNotSplitSeries(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("nexus_order_total", "help", "a", "b")
	c.With(Labels{"a": "1", "b": "2"}).Inc()
	c.With(Labels{"b": "2", "a": "1"}).Inc()

	got := render(r)
	mustContain(t, got, `nexus_order_total{a="1",b="2"} 2`)
	if n := strings.Count(got, "nexus_order_total{"); n != 1 {
		t.Errorf("%d series for one label set, want 1:\n%s", n, got)
	}
}

// ── semantics ─────────────────────────────────────────────────────────────

// TestCounterRejectsNegativeAdd: a counter that can decrease breaks rate() into
// plausible-looking but wrong graphs.
func TestCounterRejectsNegativeAdd(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("nexus_mono_total", "help").Counter()
	c.Add(5)
	c.Add(-3)
	if got := c.Value(); got != 5 {
		t.Errorf("Value = %v, want 5; a negative Add was applied", got)
	}
}

func TestGaugeAcceptsNegative(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("nexus_signed", "help").Gauge()
	g.Set(3)
	g.Add(-10)
	if got := g.Value(); got != -7 {
		t.Errorf("Value = %v, want -7", got)
	}
}

func TestCounterCarriesFractions(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("nexus_frac_total", "help").Counter()
	c.Add(0.5)
	c.Add(0.25)
	if got := c.Value(); got != 0.75 {
		t.Errorf("Value = %v, want 0.75", got)
	}
}

// TestReRegisterSameTypeReturnsSameFamily: two packages instrumenting one concept
// must not panic a control-plane process at init.
func TestReRegisterSameTypeReturnsSameFamily(t *testing.T) {
	r := NewRegistry()
	a := r.NewCounter("nexus_shared_total", "help")
	b := r.NewCounter("nexus_shared_total", "help")
	a.Counter().Inc()
	b.Counter().Inc()

	if got := a.Counter().Value(); got != 2 {
		t.Errorf("Value = %v, want 2; re-registration created a second family", got)
	}
}

func TestReRegisterDifferentTypePanics(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("nexus_conflict", "help")
	defer func() {
		if recover() == nil {
			t.Error("registering one name as both a counter and a gauge must panic; the scrape would be unparseable")
		}
	}()
	r.NewGauge("nexus_conflict", "help")
}

func TestInvalidNamesPanic(t *testing.T) {
	for _, name := range []string{"nexus-bad", "1_leading_digit", "has space", ""} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%q was accepted as a metric name", name)
				}
			}()
			NewRegistry().NewCounter(name, "help")
		}()
	}
}

func TestInvalidLabelNamePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("an invalid label name was accepted")
		}
	}()
	NewRegistry().NewCounter("nexus_ok_total", "help", "not-a-label")
}

func TestRegistriesAreIsolated(t *testing.T) {
	a, b := NewRegistry(), NewRegistry()
	a.NewCounter("nexus_iso_total", "help").Counter().Inc()
	b.NewCounter("nexus_iso_total", "help").Counter().Add(99)

	if got := render(a); !strings.Contains(got, "nexus_iso_total 1\n") {
		t.Errorf("registry A saw registry B's value:\n%s", got)
	}
}

// ── handler ───────────────────────────────────────────────────────────────

func TestHandlerContentTypeAndBody(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("nexus_handler_total", "help").Counter().Inc()

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	// A scraper that sees bare text/plain with no version may negotiate protobuf
	// instead and drop the body entirely.
	if got := rec.Header().Get("Content-Type"); got != ContentType {
		t.Errorf("Content-Type = %q, want %q", got, ContentType)
	}
	mustContain(t, rec.Body.String(), "nexus_handler_total 1\n")
}

func TestRoutesRegistersMetricsPath(t *testing.T) {
	r := NewRegistry()
	r.NewGauge("nexus_routed", "help").Gauge().Set(1)
	mux := http.NewServeMux()
	r.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics returned %d, want 200", rec.Code)
	}
	mustContain(t, rec.Body.String(), "nexus_routed 1\n")
}

func TestHandlerOnEmptyRegistry(t *testing.T) {
	rec := httptest.NewRecorder()
	NewRegistry().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	// An empty scrape is valid and must not be a 500: a process that has served no
	// traffic yet has nothing to report, and failing the scrape would page someone.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d on an empty registry, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
}
