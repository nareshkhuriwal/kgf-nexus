// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package instances

import (
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
)

// Metrics are the heartbeat's instruments. Each process serves its own
// registry, so the scrape target already says which instance a series belongs
// to (join nexus_build_info for its service); these carry only the operation.
// A nil *Metrics records nothing.
type Metrics struct {
	failures    *metrics.CounterVec
	lastSuccess *metrics.Gauge
}

// NewMetrics registers the heartbeat families on reg; nil reg returns nil.
func NewMetrics(reg *metrics.Registry) *Metrics {
	if reg == nil {
		return nil
	}
	m := &Metrics{
		failures: reg.NewCounter("nexus_instance_heartbeat_failures_total",
			"Service-instance registry writes that failed, by operation (beat, prune, shutdown). "+
				"A sustained rate of op=beat means this replica shows as stale in the console (ADR-0037).",
			"op"),
		lastSuccess: reg.NewGauge("nexus_instance_heartbeat_last_success_timestamp_seconds",
			"Unix time of this process's latest successful registry beat. "+
				"time() minus this above 30 is the condition the console reports as stale.").Gauge(),
	}
	// Every operation's series exists from the start, so a rate() over it is
	// 0 rather than absent until the first failure.
	for _, op := range []string{"beat", "prune", "shutdown"} {
		m.failures.With(metrics.Labels{"op": op})
	}
	return m
}

func (m *Metrics) failed(op string) {
	if m == nil {
		return
	}
	m.failures.With(metrics.Labels{"op": op}).Inc()
}

func (m *Metrics) succeeded(at time.Time) {
	if m == nil {
		return
	}
	m.lastSuccess.Set(float64(at.UnixNano()) / 1e9)
}
