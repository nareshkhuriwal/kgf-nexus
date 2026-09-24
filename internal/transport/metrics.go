// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"sync"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
)

// Metric names. Exported so an alert rule and a test can refer to the same
// string rather than two copies that drift.
const (
	// MetricNotAfter is the expiry of the material currently being SERVED, as a
	// Unix timestamp, per role (server, client, ca).
	//
	// This is the series to alert on: `MetricNotAfter - time()` keeps counting
	// down even if this process's poller stalls, whereas a "seconds remaining"
	// gauge freezes at its last value and would hide exactly that failure.
	MetricNotAfter = "nexus_tls_cert_not_after_timestamp_seconds"

	// MetricExpiry is seconds until MetricNotAfter, refreshed every reload
	// interval. For dashboards and humans; alert on MetricNotAfter.
	MetricExpiry = "nexus_tls_cert_expiry_seconds"

	// MetricReloads counts applied (result="success") and rejected
	// (result="failure") changes of on-disk material, per role. A rejection is
	// counted once per distinct content, not once per poll.
	MetricReloads = "nexus_tls_reload_total"

	// MetricStale is 1 while the files on disk differ from what is being served
	// because the new content was rejected. It is the condition an operator
	// needs to act on -- "the renewal happened and this process refuses it" --
	// and unlike a counter it stays raised for as long as that is true.
	MetricStale = "nexus_tls_reload_stale"
)

// sink is one registry's handles on the transport metric families.
type sink struct {
	notAfter *metrics.GaugeVec
	expiry   *metrics.GaugeVec
	stale    *metrics.GaugeVec
	reloads  *metrics.CounterVec
}

// sinks is every registry RegisterMetrics was given. Guarded by one mutex that is
// also held while counters are incremented, so a registry added mid-flight gets
// each event exactly once: either in the replay or as a live increment.
var sinks struct {
	mu   sync.Mutex
	list []*sink
}

// RegisterMetrics exposes the transport's certificate metrics on reg.
//
// Separate from ServerOptions/DialOption rather than a parameter to them,
// because the credentials are resolved at eight call sites and the registry
// exists at one (the service's main). Calling it before or after the
// credentials are resolved gives the same result: current values and counts so
// far are replayed into the registry on registration.
//
// Readiness is deliberately NOT tied to any of this. A certificate that is
// close to expiry still works, and a /readyz that failed on it would pull every
// replica out of rotation at the same moment -- turning an alert into an outage.
func RegisterMetrics(reg *metrics.Registry) {
	s := &sink{
		notAfter: reg.NewGauge(MetricNotAfter,
			"Expiry (Unix seconds) of the TLS material currently served, by role (server, client, ca).", "role"),
		expiry: reg.NewGauge(MetricExpiry,
			"Seconds until the TLS material currently served expires, by role. Alert on "+MetricNotAfter+" instead.", "role"),
		stale: reg.NewGauge(MetricStale,
			"1 while TLS material on disk was rejected and the previous material is still served.", "role"),
		reloads: reg.NewCounter(MetricReloads,
			"Changes of on-disk TLS material applied (success) or rejected (failure), by role.", "role", "result"),
	}

	live.mu.Lock()
	rs := make([]*reloader, 0, len(live.m))
	for _, r := range live.m {
		rs = append(rs, r)
	}
	live.mu.Unlock()

	sinks.mu.Lock()
	sinks.list = append(sinks.list, s)
	for _, r := range rs {
		if n := r.successes.Load(); n > 0 {
			s.reloads.With(reloadLabels(r.role, "success")).Add(float64(n))
		}
		if n := r.failures.Load(); n > 0 {
			s.reloads.With(reloadLabels(r.role, "failure")).Add(float64(n))
		}
	}
	sinks.mu.Unlock()

	for _, r := range rs {
		r.publish()
	}
}

func reloadLabels(role, result string) metrics.Labels {
	return metrics.Labels{"role": role, "result": result}
}

func roleLabels(role string) metrics.Labels { return metrics.Labels{"role": role} }

// publish pushes the current snapshot's gauges to every sink.
func (r *reloader) publish() {
	r.publishExpiry()
	r.publishStale()
}

func (r *reloader) publishExpiry() {
	s := r.current()
	if s == nil {
		return
	}
	now := r.now()
	sinks.mu.Lock()
	defer sinks.mu.Unlock()
	for _, k := range sinks.list {
		if s.leaf != nil && s.leaf.Leaf != nil {
			na := s.leaf.Leaf.NotAfter
			k.notAfter.With(roleLabels(r.role)).Set(float64(na.Unix()))
			k.expiry.With(roleLabels(r.role)).Set(na.Sub(now).Seconds())
		}
		if s.pool != nil {
			// Server and client reloaders both carry the one CA bundle
			// (NEXUS_TLS_CA_FILE), so both write the same value here.
			k.notAfter.With(roleLabels(roleCA)).Set(float64(s.caNotAfter.Unix()))
			k.expiry.With(roleLabels(roleCA)).Set(s.caNotAfter.Sub(now).Seconds())
		}
	}
}

func (r *reloader) publishStale() {
	v := 0.0
	if r.stale.Load() {
		v = 1
	}
	sinks.mu.Lock()
	defer sinks.mu.Unlock()
	for _, k := range sinks.list {
		k.stale.With(roleLabels(r.role)).Set(v)
	}
}
