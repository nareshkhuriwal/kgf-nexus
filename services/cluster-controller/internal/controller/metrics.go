// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
)

// Metrics are the controller's reconcile-loop instruments.
//
// The pool label is the registry binding's pool name, which an operator
// configures (a handful per cluster); nothing derived from pods, hosts or
// attempts is ever a label. A nil *Metrics records nothing.
type Metrics struct {
	cycles      *metrics.CounterVec
	duration    *metrics.Histogram
	poolErrors  *metrics.CounterVec
	actions     *metrics.CounterVec
	replicas    *metrics.GaugeVec
	lastSuccess *metrics.Gauge
	hpaActions  *metrics.CounterVec
	hpaBlocked  *metrics.GaugeVec
}

// NewMetrics registers the controller families on reg.
func NewMetrics(reg *metrics.Registry) *Metrics {
	return &Metrics{
		cycles: reg.NewCounter("nexus_controller_reconcile_cycles_total",
			"Reconcile cycles run by the leader, by result (ok, error).", "result"),
		duration: reg.NewHistogram("nexus_controller_reconcile_duration_seconds",
			"Duration of one observe -> plan -> apply -> report cycle.",
			[]float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}).Histogram(),
		poolErrors: reg.NewCounter("nexus_controller_pool_reconcile_errors_total",
			"Pool bindings whose reconcile failed in a cycle (nothing further was changed for that pool).", "pool"),
		actions: reg.NewCounter("nexus_controller_scale_actions_total",
			"Scaling decisions, by pool, action (scale_out, scale_in, drain, hold, refused), mode (plan, apply) and whether it was applied.",
			"pool", "action", "mode", "applied"),
		replicas: reg.NewGauge("nexus_controller_pool_replicas",
			"Worker Deployment replicas per pool as last observed (current) and as planned (desired).",
			"pool", "kind"),
		lastSuccess: reg.NewGauge("nexus_controller_last_success_timestamp_seconds",
			"Unix time of the last cycle that completed without error. Alert on time() minus this.").Gauge(),
		hpaActions: reg.NewCounter("nexus_controller_scheduler_hpa_actions_total",
			"Scheduler HPA autoscaling events (ADR-0036), by action (hpa_plan, hpa_update, hpa_refused) and mode (plan, apply).",
			"action", "mode"),
		hpaBlocked: reg.NewGauge("nexus_controller_scheduler_hpa_blocked",
			"1 while a scheduler HPA differs from the saved scheduler settings and apply mode cannot converge it without a human "+
				"(not opted in, no CPU metric, or patches refused); 0 otherwise. The hpa label is the HPA's name.",
			"hpa"),
	}
}

func (m *Metrics) cycle(start time.Time, err error, now time.Time) {
	if m == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error"
	} else {
		m.lastSuccess.Set(float64(now.Unix()))
	}
	m.cycles.With(metrics.Labels{"result": result}).Inc()
	m.duration.Observe(time.Since(start).Seconds())
}

func (m *Metrics) poolError(pool string) {
	if m == nil {
		return
	}
	m.poolErrors.With(metrics.Labels{"pool": pool}).Inc()
}

func (m *Metrics) step(pool string, st Step, mode Mode, applied bool, action string) {
	if m == nil {
		return
	}
	m.replicas.With(metrics.Labels{"pool": pool, "kind": "current"}).Set(float64(st.From))
	m.replicas.With(metrics.Labels{"pool": pool, "kind": "desired"}).Set(float64(st.To))
	if action == "" {
		return
	}
	a := "false"
	if applied {
		a = "true"
	}
	m.actions.With(metrics.Labels{"pool": pool, "action": action, "mode": mode.String(), "applied": a}).Inc()
}

func (m *Metrics) hpa(rep *rmv1.SchedulerHPAReconcile, mode Mode) {
	if m == nil {
		return
	}
	blocked := 0.0
	if mode.Apply && rep.GetResult() == "blocked" {
		blocked = 1
	}
	m.hpaBlocked.With(metrics.Labels{"hpa": rep.GetHpaName()}).Set(blocked)
	if a := rep.GetAction(); a != "" {
		m.hpaActions.With(metrics.Labels{"action": a, "mode": mode.String()}).Inc()
	}
}
