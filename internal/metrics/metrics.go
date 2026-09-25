// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package metrics is the platform's instrumentation API: counters, gauges and
// histograms with fixed label sets, plus the HTTP and gRPC middleware and the
// per-service registry every Nexus process exposes at /metrics.
//
// The instruments are in-house (lock-free counters and gauges, a non-cumulative
// bucket histogram); the EXPOSITION is github.com/prometheus/client_golang,
// pinned in go.mod. ADR-0019 records why: the hand-written encoder was correct
// on the happy path but had no Go runtime or process collectors, no content
// negotiation or compression, and edge cases (a histogram label named `le`, a
// duplicated bucket bound, label sets that disagreed with the declared names)
// that produced an unparseable scrape. Keeping this package's API meant no call
// site had to change to fix that.
//
// What is deliberately NOT here: exemplars and native histograms. Both are a
// client_golang option away if they are ever needed.
package metrics

import (
	"math"
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
