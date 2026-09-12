// Package metrics is a minimal Prometheus-text-format exposition built on the
// standard library only.
//
// It exists rather than github.com/prometheus/client_golang because that library
// pulls in a transitive tree (protobuf extensions, expfmt, procfs, client_model)
// for a feature this platform uses only the simplest slice of: a handful of
// counters, gauges and histograms scraped over HTTP. The Prometheus text
// exposition format is versioned, stable and a few dozen lines to write, so the
// dependency would buy convenience at the cost of a supply-chain surface — the same
// trade the Python SDK makes by declaring zero runtime dependencies.
//
// What is deliberately NOT here: exemplars, native histograms, the protobuf
// negotiation, and process/Go runtime collectors. If any of those is ever needed,
// swapping in client_golang behind this package's API is the right move; until then
// the cost is not justified.
package metrics

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Labels is a fixed label set attached to one metric instance.
//
// Labels are fixed at construction rather than passed per-observation. That rules
// out the classic failure of dynamic label values — a label carrying a run_id or a
// task_id, which produces one time series per run and eventually exhausts the
// scrape target's memory. Callers that need a new label combination must ask the
// family for it explicitly (CounterVec.With and friends), which makes cardinality a
// visible decision rather than an accident.
type Labels map[string]string

// Counter is a monotonically increasing value.
type Counter struct {
	// Stored as bits of a float64 so a counter can carry fractional amounts
	// (bytes-per-second sums, seconds of work) without a second type.
	bits atomic.Uint64
}

// Inc adds one.
func (c *Counter) Inc() { c.Add(1) }

// Add increases the counter by delta.
//
// A negative delta is DROPPED rather than applied: a counter that can go down
// breaks rate() silently, producing plausible-looking graphs that understate the
// truth, and a caller passing a negative delta has a bug worth failing loudly in
// review rather than quietly in a dashboard.
func (c *Counter) Add(delta float64) {
	if delta < 0 || math.IsNaN(delta) {
		return
	}
	for {
		old := c.bits.Load()
		next := math.Float64bits(math.Float64frombits(old) + delta)
		if c.bits.CompareAndSwap(old, next) {
			return
		}
	}
}

// Value reads the current count.
func (c *Counter) Value() float64 { return math.Float64frombits(c.bits.Load()) }

// Gauge is a value that can go up and down.
type Gauge struct {
	bits atomic.Uint64
}

// Set replaces the gauge's value.
func (g *Gauge) Set(v float64) { g.bits.Store(math.Float64bits(v)) }

// Add adjusts the gauge by delta, which may be negative.
func (g *Gauge) Add(delta float64) {
	for {
		old := g.bits.Load()
		next := math.Float64bits(math.Float64frombits(old) + delta)
		if g.bits.CompareAndSwap(old, next) {
			return
		}
	}
}

// Inc adds one; Dec subtracts one.
func (g *Gauge) Inc() { g.Add(1) }

// Dec subtracts one.
func (g *Gauge) Dec() { g.Add(-1) }

// Value reads the current value.
func (g *Gauge) Value() float64 { return math.Float64frombits(g.bits.Load()) }

// Histogram is a cumulative bucket histogram plus a sum and count.
type Histogram struct {
	// upper is sorted ascending and never mutated after construction, so readers
	// need no lock for it.
	upper []float64

	mu     sync.Mutex
	counts []uint64
	sum    float64
	count  uint64
}

// DefaultBuckets covers sub-millisecond to ten-second latencies.
//
// The control plane's interesting latencies span four orders of magnitude — a
// Postgres round trip is sub-millisecond, a dispatch is tens of milliseconds, and a
// schedule_to_start under capacity shortfall is seconds — so the buckets are
// roughly logarithmic. Linear buckets would put all the resolution where nothing
// happens.
var DefaultBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Observe records one sample.
func (h *Histogram) Observe(v float64) {
	if math.IsNaN(v) {
		return
	}
	// A linear scan beats sort.SearchFloat64s here: the bucket slices are a dozen
	// entries and the scan has no function-call overhead per comparison.
	idx := len(h.upper)
	for i, ub := range h.upper {
		if v <= ub {
			idx = i
			break
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	// Buckets are stored non-cumulatively and summed at scrape time. Incrementing
	// every bucket at or above the sample would make Observe O(buckets) on the hot
	// path to save work on the once-per-scrape path, which is the wrong direction.
	h.counts[idx]++
	h.sum += v
	h.count++
}

// Snapshot returns the cumulative bucket counts, the sum and the total count.
//
// The returned buckets are CUMULATIVE (`le` semantics), because that is what the
// text format and every Prometheus query function expect.
func (h *Histogram) Snapshot() (cumulative []uint64, sum float64, count uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cumulative = make([]uint64, len(h.counts))
	var running uint64
	for i, c := range h.counts {
		running += c
		cumulative[i] = running
	}
	return cumulative, h.sum, h.count
}

// Buckets reports the upper bounds, excluding the implicit +Inf bucket.
func (h *Histogram) Buckets() []float64 {
	out := make([]float64, len(h.upper))
	copy(out, h.upper)
	return out
}

// metricKind distinguishes the three exposition shapes.
type metricKind string

const (
	kindCounter   metricKind = "counter"
	kindGauge     metricKind = "gauge"
	kindHistogram metricKind = "histogram"
)

// family groups every labelled instance that shares a name, help text and type.
//
// Prometheus requires that a name's `# HELP` and `# TYPE` appear once and that all
// its series be contiguous. Modelling the family explicitly is what guarantees
// that, instead of relying on registration order.
type family struct {
	name    string
	help    string
	kind    metricKind
	buckets []float64

	mu       sync.RWMutex
	children map[string]*child
	// order preserves first-registration order so the exposition is stable
	// between scrapes. Unstable output makes diffing two scrapes — the fastest way
	// to answer "what changed in the last minute" during an incident — useless.
	order []string
}

type child struct {
	labels Labels
	key    string

	counter   *Counter
	gauge     *Gauge
	histogram *Histogram
}

func (f *family) get(labels Labels) *child {
	key := labelKey(labels)

	f.mu.RLock()
	c, ok := f.children[key]
	f.mu.RUnlock()
	if ok {
		return c
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// Re-check under the write lock: two goroutines can both miss the read path,
	// and handing them different instances would silently split one series in two.
	if c, ok := f.children[key]; ok {
		return c
	}
	c = &child{labels: cloneLabels(labels), key: key}
	switch f.kind {
	case kindCounter:
		c.counter = &Counter{}
	case kindGauge:
		c.gauge = &Gauge{}
	case kindHistogram:
		c.histogram = &Histogram{upper: f.buckets, counts: make([]uint64, len(f.buckets)+1)}
	}
	f.children[key] = c
	f.order = append(f.order, key)
	return c
}

// snapshot returns the family's children in registration order.
func (f *family) snapshot() []*child {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]*child, 0, len(f.order))
	for _, k := range f.order {
		out = append(out, f.children[k])
	}
	return out
}

// labelKey is the canonical identity of a label set.
//
// Sorted, so {a:1,b:2} and {b:2,a:1} are the same series. Without this, map
// iteration order would make the same logical metric register as two series
// whenever a caller happened to build its labels in a different order.
func labelKey(labels Labels) string {
	if len(labels) == 0 {
		return ""
	}
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteByte('=')
		b.WriteString(labels[n])
	}
	return b.String()
}

func cloneLabels(labels Labels) Labels {
	if len(labels) == 0 {
		return nil
	}
	out := make(Labels, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

// formatFloat renders a value the way the text format expects.
//
// Infinities and NaN have reserved spellings that differ from Go's, and a
// mis-rendered one makes the whole scrape fail to parse — losing every metric in
// the process, not just the bad one.
func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// escapeLabelValue escapes the three characters the text format reserves inside a
// label value. An unescaped newline or quote from, say, a queue name would corrupt
// every subsequent line of the scrape.
func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, `\"`+"\n") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v) + 8)
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeHelp escapes what `# HELP` reserves: backslash and newline (a quote is
// literal there).
func escapeHelp(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	return strings.ReplaceAll(v, "\n", `\n`)
}

// renderLabels formats a label set as `{a="1",b="2"}`, or "" when empty.
func renderLabels(labels Labels, extraName, extraValue string) string {
	if len(labels) == 0 && extraName == "" {
		return ""
	}
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		// Built by hand rather than with %q: escapeLabelValue has already applied
		// the text format's escaping, and %q would escape the backslashes again.
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(labels[n]))
		b.WriteByte('"')
	}
	if extraName != "" {
		if len(names) > 0 {
			b.WriteByte(',')
		}
		b.WriteString(extraName)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(extraValue))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}
