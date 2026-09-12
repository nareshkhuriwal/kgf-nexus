package metrics

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// ContentType is the Prometheus text exposition media type.
//
// The version parameter is not decoration: a scraper that sees text/plain with no
// version falls back to guessing, and some versions of some scrapers guess the
// protobuf negotiation instead and drop the body.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// nameRE is the metric and label name grammar from the exposition format.
var nameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// Registry holds the metric families a process exposes.
//
// Metrics are registered against an explicit Registry rather than a package-level
// default. A global default makes a test's metrics leak into the next test's
// assertions, and makes it impossible to scrape one subsystem in isolation — both
// of which are exactly the problems that make instrumentation get deleted.
type Registry struct {
	mu       sync.RWMutex
	families map[string]*family
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{families: make(map[string]*family)}
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
func (r *Registry) NewHistogram(name, help string, buckets []float64, labelNames ...string) *HistogramVec {
	if len(buckets) == 0 {
		buckets = DefaultBuckets
	}
	sorted := make([]float64, len(buckets))
	copy(sorted, buckets)
	sort.Float64s(sorted)
	return &HistogramVec{f: r.register(name, help, kindHistogram, sorted, labelNames)}
}

func (r *Registry) register(name, help string, kind metricKind, buckets []float64, labelNames []string) *family {
	if !nameRE.MatchString(name) {
		panic(fmt.Sprintf("metrics: %q is not a valid metric name", name))
	}
	for _, ln := range labelNames {
		if !nameRE.MatchString(ln) {
			panic(fmt.Sprintf("metrics: %q is not a valid label name on %s", ln, name))
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.families[name]; ok {
		if existing.kind != kind {
			panic(fmt.Sprintf("metrics: %q is already registered as a %s, cannot also be a %s", name, existing.kind, kind))
		}
		return existing
	}
	f := &family{
		name:     name,
		help:     help,
		kind:     kind,
		buckets:  buckets,
		children: make(map[string]*child),
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

// WriteTo renders the whole registry in Prometheus text format.
func (r *Registry) WriteTo(w *strings.Builder) {
	r.mu.RLock()
	names := make([]string, 0, len(r.families))
	for n := range r.families {
		names = append(names, n)
	}
	fams := r.families
	r.mu.RUnlock()

	// Families are emitted in name order. Registration order would depend on
	// package init order, which changes under refactoring and makes two scrapes
	// gratuitously un-diffable.
	sort.Strings(names)
	for _, n := range names {
		writeFamily(w, fams[n])
	}
}

func writeFamily(w *strings.Builder, f *family) {
	kids := f.snapshot()
	if len(kids) == 0 {
		// A family with no observations yet emits nothing at all. Emitting a bare
		// HELP/TYPE pair with no series is legal but makes a dashboard show "no
		// data" identically to a broken scrape; absence is the honest signal.
		return
	}
	if f.help != "" {
		fmt.Fprintf(w, "# HELP %s %s\n", f.name, escapeHelp(f.help))
	}
	fmt.Fprintf(w, "# TYPE %s %s\n", f.name, f.kind)

	for _, c := range kids {
		switch f.kind {
		case kindCounter:
			fmt.Fprintf(w, "%s%s %s\n", f.name, renderLabels(c.labels, "", ""), formatFloat(c.counter.Value()))
		case kindGauge:
			fmt.Fprintf(w, "%s%s %s\n", f.name, renderLabels(c.labels, "", ""), formatFloat(c.gauge.Value()))
		case kindHistogram:
			cumulative, sum, count := c.histogram.Snapshot()
			for i, ub := range f.buckets {
				fmt.Fprintf(w, "%s_bucket%s %d\n", f.name,
					renderLabels(c.labels, "le", formatFloat(ub)), cumulative[i])
			}
			// The +Inf bucket is mandatory and must equal _count. A histogram
			// missing it silently breaks histogram_quantile, which then reports
			// plausible but wrong percentiles.
			fmt.Fprintf(w, "%s_bucket%s %d\n", f.name,
				renderLabels(c.labels, "le", "+Inf"), count)
			fmt.Fprintf(w, "%s_sum%s %s\n", f.name, renderLabels(c.labels, "", ""), formatFloat(sum))
			fmt.Fprintf(w, "%s_count%s %d\n", f.name, renderLabels(c.labels, "", ""), count)
		}
	}
}

// Handler serves the registry at /metrics.
//
// Scraping must never be able to take the process down, so this handler touches no
// dependency: it renders values already in memory. A /metrics that queried the
// database would fail exactly when the metrics matter most, and a scrape timeout
// would then look like the service being down.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var b strings.Builder
		r.WriteTo(&b)
		w.Header().Set("Content-Type", ContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	})
}

// Routes registers the endpoint on a mux, mirroring health.Handler.Routes so a
// service's observability wiring reads the same way everywhere.
func (r *Registry) Routes(mux *http.ServeMux) {
	mux.Handle("GET /metrics", r.Handler())
}
