// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/expfmt"
)

// ContentType is the Prometheus text exposition media type a plain scrape (no
// Accept header, or one asking for text) is answered with.
//
// The version parameter is not decoration: a scraper that sees text/plain with no
// version falls back to guessing, and some versions of some scrapers guess the
// protobuf negotiation instead and drop the body. The value is expfmt's own, so it
// cannot drift from what the encoder actually writes.
var ContentType = string(expfmt.NewFormat(expfmt.TypeTextPlain))

// nameRE is the metric and label name grammar from the exposition format.
var nameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// labelRE is the label-name grammar: the metric grammar minus the colon, which
// is reserved for recording rules.
var labelRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Registry holds the metric families a process exposes.
//
// Metrics are registered against an explicit Registry rather than a package-level
// default. A global default makes a test's metrics leak into the next test's
// assertions, and makes it impossible to scrape one subsystem in isolation — both
// of which are exactly the problems that make instrumentation get deleted.
//
// Exposition is delegated to github.com/prometheus/client_golang (ADR-0019): the
// instruments below stay in-house, so call sites and semantics are unchanged,
// while the wire format, content negotiation, compression and the standard Go
// runtime / process collectors come from the reference implementation rather
// than from a hand-maintained encoder.
type Registry struct {
	mu       sync.RWMutex
	families map[string]*family
	funcs    map[string]*funcFamily

	prom *prometheus.Registry
}

// NewRegistry builds an empty registry: no runtime, process or build-info
// collectors. Services use NewServiceRegistry; this form exists so tests can
// assert on exactly the families they created.
func NewRegistry() *Registry {
	r := &Registry{
		families: make(map[string]*family),
		funcs:    make(map[string]*funcFamily),
		prom:     prometheus.NewRegistry(),
	}
	// An UNCHECKED collector (Describe emits nothing): families are created at
	// runtime by the instrumented packages, not declared up front. Consistency
	// is still enforced -- Gather rejects a family whose series disagree on
	// label names or type -- and With() normalizes label sets so that never
	// happens.
	r.prom.MustRegister(collector{r})
	return r
}

// CounterVec, GaugeVec and HistogramVec are handles to a metric family. A Vec hands
// out one instance per label set, created on first use.
type CounterVec struct{ f *family }

// GaugeVec is a gauge family.
type GaugeVec struct{ f *family }

// HistogramVec is a histogram family.
type HistogramVec struct{ f *family }

// NewCounter registers a counter family.
//
// Re-registering the same name with the same type returns the EXISTING family
// rather than panicking. Two packages instrumenting the same concept is normal, and
// a panic at init in a control-plane process would turn an instrumentation mistake
// into a service that will not boot — a far worse failure than a shared series.
// A name registered with a DIFFERENT type is a real contradiction and panics,
// because the resulting scrape would be unparseable.
func (r *Registry) NewCounter(name, help string, labelNames ...string) *CounterVec {
	return &CounterVec{f: r.register(name, help, kindCounter, nil, labelNames)}
}

// NewGauge registers a gauge family.
func (r *Registry) NewGauge(name, help string, labelNames ...string) *GaugeVec {
	return &GaugeVec{f: r.register(name, help, kindGauge, nil, labelNames)}
}

// NewHistogram registers a histogram family. A nil or empty buckets slice uses
// DefaultBuckets.
//
// Buckets are sorted, de-duplicated, and stripped of +Inf and NaN: the +Inf
// bucket is implicit and always emitted, and a duplicated bound would emit two
// series with the same `le`, which a scraper rejects -- losing the whole target,
// not just the histogram.
func (r *Registry) NewHistogram(name, help string, buckets []float64, labelNames ...string) *HistogramVec {
	if len(buckets) == 0 {
		buckets = DefaultBuckets
	}
	clean := make([]float64, 0, len(buckets))
	for _, b := range buckets {
		if math.IsInf(b, 1) || math.IsNaN(b) {
			continue
		}
		clean = append(clean, b)
	}
	sort.Float64s(clean)
	uniq := clean[:0]
	for i, b := range clean {
		if i == 0 || b != clean[i-1] {
			uniq = append(uniq, b)
		}
	}
	for _, ln := range labelNames {
		if ln == "le" {
			// `le` is the bucket label. A user label of the same name produces
			// {le="x",le="0.1"}, which does not parse.
			panic(fmt.Sprintf("metrics: histogram %s may not use the reserved label name \"le\"", name))
		}
	}
	return &HistogramVec{f: r.register(name, help, kindHistogram, uniq, labelNames)}
}

// NewGaugeFunc registers a gauge whose value is read from fn at scrape time.
//
// fn MUST be cheap and must not block on a dependency: it runs inside the
// scrape, and a scrape that waits on the database fails exactly when the numbers
// matter most. Reading an in-memory statistic (a connection pool's counters, a
// queue length) is the intended use.
func (r *Registry) NewGaugeFunc(name, help string, fn func() float64) {
	r.registerFunc(name, help, kindGauge, fn)
}

// NewCounterFunc registers a counter whose value is read from fn at scrape time,
// for a monotonically increasing statistic kept elsewhere (a pool's total
// acquire count). The same rules as NewGaugeFunc apply.
func (r *Registry) NewCounterFunc(name, help string, fn func() float64) {
	r.registerFunc(name, help, kindCounter, fn)
}

func validateName(name string) {
	if !nameRE.MatchString(name) {
		panic(fmt.Sprintf("metrics: %q is not a valid metric name", name))
	}
}

func (r *Registry) registerFunc(name, help string, kind metricKind, fn func() float64) {
	validateName(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.families[name]; ok {
		panic(fmt.Sprintf("metrics: %q is already registered as an instrument family", name))
	}
	if existing, ok := r.funcs[name]; ok && existing.kind != kind {
		panic(fmt.Sprintf("metrics: %q is already registered as a %s, cannot also be a %s", name, existing.kind, kind))
	}
	// Re-registration replaces the function: the newest reader wins, which is
	// what a component rebuilt in place (a reconnected pool) wants.
	r.funcs[name] = &funcFamily{name: name, help: help, kind: kind, fn: fn}
}

func (r *Registry) register(name, help string, kind metricKind, buckets []float64, labelNames []string) *family {
	validateName(name)
	seen := make(map[string]bool, len(labelNames))
	for _, ln := range labelNames {
		if !labelRE.MatchString(ln) || strings.HasPrefix(ln, "__") {
			// A leading double underscore is reserved for Prometheus-internal
			// labels and is dropped or rejected by the server.
			panic(fmt.Sprintf("metrics: %q is not a valid label name on %s", ln, name))
		}
		if seen[ln] {
			panic(fmt.Sprintf("metrics: label %q is declared twice on %s", ln, name))
		}
		seen[ln] = true
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.funcs[name]; ok {
		panic(fmt.Sprintf("metrics: %q is already registered as a function metric", name))
	}
	if existing, ok := r.families[name]; ok {
		if existing.kind != kind {
			panic(fmt.Sprintf("metrics: %q is already registered as a %s, cannot also be a %s", name, existing.kind, kind))
		}
		return existing
	}
	names := append([]string(nil), labelNames...)
	f := &family{
		name:       name,
		help:       help,
		kind:       kind,
		buckets:    buckets,
		labelNames: names,
		children:   make(map[string]*child),
	}
	r.families[name] = f
	return f
}

// With returns the counter for one label set, creating it on first use.
func (v *CounterVec) With(labels Labels) *Counter { return v.f.get(labels).counter }

// Counter returns the unlabelled instance, for a family with no labels.
func (v *CounterVec) Counter() *Counter { return v.With(nil) }

// With returns the gauge for one label set.
func (v *GaugeVec) With(labels Labels) *Gauge { return v.f.get(labels).gauge }

// Gauge returns the unlabelled instance.
func (v *GaugeVec) Gauge() *Gauge { return v.With(nil) }

// With returns the histogram for one label set.
func (v *HistogramVec) With(labels Labels) *Histogram { return v.f.get(labels).histogram }

// Histogram returns the unlabelled instance.
func (v *HistogramVec) Histogram() *Histogram { return v.With(nil) }

// Gatherer exposes the registry to client_golang tooling (the push client a
// batch job uses, testutil in tests). It includes every collector registered
// through Register as well as the in-house families.
func (r *Registry) Gatherer() prometheus.Gatherer { return r.prom }

// Register adds a client_golang collector -- the Go runtime and process
// collectors, or a third-party library's -- to the same exposition.
func (r *Registry) Register(c prometheus.Collector) error { return r.prom.Register(c) }

// WriteTo renders the whole registry in the Prometheus text format (0.0.4).
//
// Families are emitted in name order and series in label order (client_golang's
// Gather sorts both), so two scrapes are byte-comparable -- diffing them is the
// fastest way to answer "what changed in the last minute" during an incident.
// A gather error is rendered as a trailing comment rather than dropping the
// families that did gather: partial truth beats an empty page.
func (r *Registry) WriteTo(w *strings.Builder) {
	mfs, err := r.prom.Gather()
	enc := expfmt.NewEncoder(w, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		_ = enc.Encode(mf)
	}
	if err != nil {
		fmt.Fprintf(w, "# gather error: %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
	}
}

// Handler serves the registry at /metrics.
//
// Scraping must never be able to take the process down, so this handler touches no
// dependency: it renders values already in memory. A /metrics that queried the
// database would fail exactly when the metrics matter most, and a scrape timeout
// would then look like the service being down.
//
// ContinueOnError: one collector failing (say, a /proc read on an unusual
// kernel) serves every other family and logs, rather than answering 500 and
// losing all of them. MaxRequestsInFlight bounds what a misconfigured scraper
// fleet can make the process do.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.prom, promhttp.HandlerOpts{
		ErrorHandling:       promhttp.ContinueOnError,
		ErrorLog:            slogPromLogger{},
		MaxRequestsInFlight: 4,
	})
}

// Routes registers the endpoint on a mux, mirroring health.Handler.Routes so a
// service's observability wiring reads the same way everywhere.
func (r *Registry) Routes(mux *http.ServeMux) {
	mux.Handle("GET /metrics", r.Handler())
}

// slogPromLogger routes promhttp's error log into the process's slog logger.
type slogPromLogger struct{}

func (slogPromLogger) Println(v ...any) {
	slog.Warn("metrics exposition error", "detail", fmt.Sprint(v...))
}

// ── the bridge from in-house instruments to client_golang ──────────────────

// collector adapts the in-house families to prometheus.Collector. Values are
// read from the instruments at scrape time and emitted as const metrics, which
// is the documented client_golang pattern for "state kept elsewhere".
type collector struct{ r *Registry }

// Describe emits nothing, making this an unchecked collector: its families are
// not known at registration time.
func (collector) Describe(chan<- *prometheus.Desc) {}

func (c collector) Collect(ch chan<- prometheus.Metric) {
	c.r.mu.RLock()
	fams := make([]*family, 0, len(c.r.families))
	for _, f := range c.r.families {
		fams = append(fams, f)
	}
	funcs := make([]*funcFamily, 0, len(c.r.funcs))
	for _, f := range c.r.funcs {
		funcs = append(funcs, f)
	}
	c.r.mu.RUnlock()

	for _, f := range fams {
		kids := f.snapshot()
		if len(kids) == 0 {
			// A family with no observations yet emits nothing at all. Emitting a
			// bare HELP/TYPE pair with no series is legal but makes a dashboard
			// show "no data" identically to a broken scrape; absence is the
			// honest signal.
			continue
		}
		desc := prometheus.NewDesc(f.name, helpOrName(f.help, f.name), f.labelNames, nil)
		for _, k := range kids {
			var (
				m   prometheus.Metric
				err error
			)
			switch f.kind {
			case kindCounter:
				m, err = prometheus.NewConstMetric(desc, prometheus.CounterValue, k.counter.Value(), k.values...)
			case kindGauge:
				m, err = prometheus.NewConstMetric(desc, prometheus.GaugeValue, k.gauge.Value(), k.values...)
			case kindHistogram:
				cumulative, sum, count := k.histogram.Snapshot()
				buckets := make(map[float64]uint64, len(f.buckets))
				for i, ub := range f.buckets {
					buckets[ub] = cumulative[i]
				}
				// The +Inf bucket is added by the encoder from count, so it
				// always equals _count: a histogram whose +Inf disagrees with
				// _count silently breaks histogram_quantile.
				m, err = prometheus.NewConstHistogram(desc, count, sum, buckets, k.values...)
			}
			if err != nil {
				m = prometheus.NewInvalidMetric(desc, err)
			}
			ch <- m
		}
	}
	for _, f := range funcs {
		desc := prometheus.NewDesc(f.name, helpOrName(f.help, f.name), nil, nil)
		vt := prometheus.GaugeValue
		if f.kind == kindCounter {
			vt = prometheus.CounterValue
		}
		m, err := prometheus.NewConstMetric(desc, vt, f.fn())
		if err != nil {
			m = prometheus.NewInvalidMetric(desc, err)
		}
		ch <- m
	}
}

func helpOrName(help, name string) string {
	if help == "" {
		return name
	}
	return help
}

// funcFamily is a single unlabelled series read from a function at scrape time.
type funcFamily struct {
	name, help string
	kind       metricKind
	fn         func() float64
}
