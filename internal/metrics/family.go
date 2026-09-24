// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// metricKind distinguishes the three exposition shapes.
type metricKind string

const (
	kindCounter   metricKind = "counter"
	kindGauge     metricKind = "gauge"
	kindHistogram metricKind = "histogram"
)

// family groups every labelled instance that shares a name, help text, type and
// label schema.
//
// The label NAMES are fixed at registration. Prometheus requires every series of
// one family to carry the same label names; a family whose series disagree is
// rejected by client_golang's Gather and flagged by Prometheus as an
// inconsistent target. Declaring them once and normalizing every With() call
// against the declaration is what makes that impossible rather than merely
// unlikely.
type family struct {
	name       string
	help       string
	kind       metricKind
	buckets    []float64
	labelNames []string

	mu       sync.RWMutex
	children map[string]*child
	// order preserves first-registration order. Exposition order is decided by
	// client_golang (sorted by label values), so this only keeps snapshot()
	// deterministic for tests.
	order []string

	// warned makes the label-mismatch warning once-per-family: a hot path that
	// passes the wrong labels would otherwise log on every observation.
	warned atomic.Bool
}

type child struct {
	// values are the label values in labelNames order.
	values []string
	key    string

	counter   *Counter
	gauge     *Gauge
	histogram *Histogram
}

// normalize maps a caller's label set onto the family's declared names.
//
// An undeclared label is DROPPED and a missing one becomes the empty string,
// with a one-time warning. The alternatives are worse: panicking turns an
// instrumentation typo into a control-plane crash, and accepting the label as
// given produces a family whose series disagree on label names -- which fails
// the scrape for the whole target, not just this family.
func (f *family) normalize(labels Labels) []string {
	if len(f.labelNames) == 0 {
		if len(labels) > 0 {
			f.warnMismatch(labels)
		}
		return nil
	}
	values := make([]string, len(f.labelNames))
	matched := 0
	for i, n := range f.labelNames {
		if v, ok := labels[n]; ok {
			values[i] = v
			matched++
		}
	}
	if matched != len(labels) || matched != len(f.labelNames) {
		f.warnMismatch(labels)
	}
	return values
}

func (f *family) warnMismatch(labels Labels) {
	if !f.warned.CompareAndSwap(false, true) {
		return
	}
	got := make([]string, 0, len(labels))
	for k := range labels {
		got = append(got, k)
	}
	sort.Strings(got)
	slog.Warn("metrics: label set does not match the family's declared labels; normalized",
		"metric", f.name, "declared", f.labelNames, "given", got)
}

func (f *family) get(labels Labels) *child {
	values := f.normalize(labels)
	key := labelKey(values)

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
	c = &child{values: values, key: key}
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

// labelKey is the canonical identity of a normalized label-value tuple.
//
// The values are already in declared-name order, so {a:1,b:2} and {b:2,a:1} are
// the same series. The separator is a byte that cannot occur in valid UTF-8, so
// no label VALUE can forge a boundary: the previous "a=1,b=2" encoding let a
// value containing "," or "=" collide with a different label set.
func labelKey(values []string) string {
	return strings.Join(values, "\xff")
}
