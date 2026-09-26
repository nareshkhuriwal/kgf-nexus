// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package agentmetrics holds the worker agent's instruments.
//
// It is built ONCE per process, in main, and survives re-registration: an agent
// session (and its worker_id) is replaced on REREGISTER, but a counter that
// reset with every session would read as a crash-loop to rate() and lose the
// history an operator needs. Session-scoped components receive hooks from here.
//
// Labels are closed sets -- executor kind, outcome, error class, result -- and
// never attempt, run or worker ids.
//
// A nil *Metrics is valid and records nothing, so tests and embedders need no
// metrics wiring.
package agentmetrics

import (
	"strings"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/objstore"
)

// TaskBuckets span a second to a day: tasks are batch work, and a histogram
// whose top bucket is ten seconds would put nearly every task in +Inf.
var TaskBuckets = []float64{1, 5, 15, 30, 60, 300, 900, 1800, 3600, 7200, 14400, 43200, 86400}

// Metrics are the agent's instruments.
type Metrics struct {
	running   *metrics.GaugeVec
	started   *metrics.CounterVec
	completed *metrics.CounterVec
	taskDur   *metrics.HistogramVec

	heartbeats     *metrics.CounterVec
	heartbeatDur   *metrics.Histogram
	heartbeatFails *metrics.Gauge
	lastHeartbeat  *metrics.Gauge

	puts       *metrics.CounterVec
	putBytes   *metrics.CounterVec
	putDur     *metrics.HistogramVec
	putSize    *metrics.Histogram
	sessions   *metrics.Counter
	capacity   *metrics.GaugeVec
	reportErrs *metrics.Counter

	registrations *metrics.CounterVec
	registered    *metrics.Gauge

	// objstore holds the result-store client families shared with the gateway
	// and the GC (nexus_objstore_*), labelled by backend and operation.
	objstore *objstore.Metrics
}

// ObjectStore returns the result-store client instruments, or nil (record
// nothing) on a nil Metrics.
func (m *Metrics) ObjectStore() *objstore.Metrics {
	if m == nil {
		return nil
	}
	return m.objstore
}

// New registers the agent families on reg.
func New(reg *metrics.Registry) *Metrics {
	return &Metrics{
		objstore: objstore.NewMetrics(reg),
		running: reg.NewGauge("nexus_agent_tasks_running",
			"Attempts currently executing on this agent, by executor.", "executor"),
		started: reg.NewCounter("nexus_agent_tasks_started_total",
			"Attempts this agent began executing, by executor.", "executor"),
		completed: reg.NewCounter("nexus_agent_tasks_completed_total",
			"Attempts this agent finished and reported, by executor, outcome (succeeded, failed, timed_out, indeterminate) and error class (none when succeeded).",
			"executor", "outcome", "error_class"),
		taskDur: reg.NewHistogram("nexus_agent_task_duration_seconds",
			"Wall time from an attempt starting on this agent to its result being reported, by executor and outcome.",
			TaskBuckets, "executor", "outcome"),

		heartbeats: reg.NewCounter("nexus_agent_heartbeats_total",
			"Heartbeats sent to the control plane, by result (ok, error).", "result"),
		heartbeatDur: reg.NewHistogram("nexus_agent_heartbeat_duration_seconds",
			"Heartbeat round-trip latency.",
			[]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}).Histogram(),
		heartbeatFails: reg.NewGauge("nexus_agent_heartbeat_consecutive_failures",
			"Current streak of failed heartbeats. At the liveness window the control plane declares this worker DEAD and its work LOST.").Gauge(),
		lastHeartbeat: reg.NewGauge("nexus_agent_last_heartbeat_success_timestamp_seconds",
			"Unix time of the last acknowledged heartbeat.").Gauge(),

		puts: reg.NewCounter("nexus_agent_result_store_puts_total",
			"Task outputs stored, by result: inline (small, sent with the report), uploaded, deduplicated (content already present), error.",
			"result"),
		putBytes: reg.NewCounter("nexus_agent_result_store_upload_bytes_total",
			"Bytes of task output handled by the result store, by result. result=uploaded is bytes actually transferred to object storage.",
			"result"),
		putDur: reg.NewHistogram("nexus_agent_result_store_upload_duration_seconds",
			"Latency of storing one output in object storage (stat + upload), by result. Inline outputs are not observed.",
			[]float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
			"result"),
		putSize: reg.NewHistogram("nexus_agent_result_store_upload_size_bytes",
			"Size of outputs uploaded to object storage.",
			[]float64{65536, 262144, 1 << 20, 4 << 20, 16 << 20, 64 << 20, 256 << 20, 1 << 30, 4 << 30}).Histogram(),
		sessions: reg.NewCounter("nexus_agent_sessions_total",
			"Registrations with the control plane. More than one per process means the control plane forced a re-registration.").Counter(),
		capacity: reg.NewGauge("nexus_agent_advertised_capacity",
			"Capacity this agent advertises for scheduling (not what the runtime enforces), by resource.",
			"resource"),
		reportErrs: reg.NewCounter("nexus_agent_result_report_failures_total",
			"Result reports that could not be delivered; the WAL retains them for replay.").Counter(),
		registrations: reg.NewCounter("nexus_agent_registration_attempts_total",
			"Register calls to the control plane, by result: ok, retryable_error (retried with backoff; the agent stays not-ready) or permanent_error (the agent exits).",
			"result"),
		registered: reg.NewGauge("nexus_agent_registered",
			"1 while the agent holds a registered session (the /readyz condition), else 0.").Gauge(),
	}
}

// RegistrationAttempt records one Register call's result: ok,
// retryable_error or permanent_error.
func (m *Metrics) RegistrationAttempt(result string) {
	if m == nil {
		return
	}
	m.registrations.With(metrics.Labels{"result": result}).Inc()
}

// SetRegistered records whether the agent holds a registered session.
func (m *Metrics) SetRegistered(ok bool) {
	if m == nil {
		return
	}
	if ok {
		m.registered.Set(1)
		return
	}
	m.registered.Set(0)
}

// TaskStarted records an attempt starting on executor kind.
func (m *Metrics) TaskStarted(kind string) {
	if m == nil {
		return
	}
	m.started.With(metrics.Labels{"executor": kind}).Inc()
	m.running.With(metrics.Labels{"executor": kind}).Inc()
}

// TaskFinished records an attempt leaving the agent (after its report).
func (m *Metrics) TaskFinished(kind string) {
	if m == nil {
		return
	}
	m.running.With(metrics.Labels{"executor": kind}).Dec()
}

// TaskReported records the outcome carried by a result report. started is
// zero when the start time is unknown (a panic path), in which case no duration
// is observed.
func (m *Metrics) TaskReported(kind string, outcome nexusv1.AttemptOutcome, class nexusv1.ErrorClass,
	started time.Time, reportErr error) {
	if m == nil {
		return
	}
	o := enumLabel(outcome.String(), "ATTEMPT_OUTCOME_")
	c := "none"
	if outcome != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		c = enumLabel(class.String(), "ERROR_CLASS_")
	}
	m.completed.With(metrics.Labels{"executor": kind, "outcome": o, "error_class": c}).Inc()
	if !started.IsZero() {
		m.taskDur.With(metrics.Labels{"executor": kind, "outcome": o}).Observe(time.Since(started).Seconds())
	}
	if reportErr != nil {
		m.reportErrs.Inc()
	}
}

// Heartbeat records one heartbeat round trip. consecutive is the failure streak
// after this beat.
func (m *Metrics) Heartbeat(ok bool, d time.Duration, consecutive int64) {
	if m == nil {
		return
	}
	result := "ok"
	if !ok {
		result = "error"
	} else {
		m.lastHeartbeat.Set(float64(time.Now().Unix()))
	}
	m.heartbeats.With(metrics.Labels{"result": result}).Inc()
	m.heartbeatDur.Observe(d.Seconds())
	m.heartbeatFails.Set(float64(consecutive))
}

// ResultStorePut records one output stored (the resultstore.Config.OnPut hook).
func (m *Metrics) ResultStorePut(result string, bytes int64, d time.Duration) {
	if m == nil {
		return
	}
	m.puts.With(metrics.Labels{"result": result}).Inc()
	m.putBytes.With(metrics.Labels{"result": result}).Add(float64(bytes))
	if result == "inline" {
		return
	}
	m.putDur.With(metrics.Labels{"result": result}).Observe(d.Seconds())
	if result == "uploaded" {
		m.putSize.Observe(float64(bytes))
	}
}

// SessionStarted records a registration and the capacity it advertised.
func (m *Metrics) SessionStarted(capacity *nexusv1.ResourceVector) {
	if m == nil {
		return
	}
	m.sessions.Inc()
	set := func(res string, v float64) { m.capacity.With(metrics.Labels{"resource": res}).Set(v) }
	set("cpu_millis", float64(capacity.GetCpuMillis()))
	set("memory_bytes", float64(capacity.GetMemoryBytes()))
	set("disk_bytes", float64(capacity.GetDiskBytes()))
	set("gpu_count", float64(capacity.GetGpuCount()))
}

// enumLabel turns "ATTEMPT_OUTCOME_TIMED_OUT" into "timed_out".
func enumLabel(s, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(s, prefix))
}
