// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package agentmetrics

import (
	"errors"
	"net/http"
	"testing"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics/metricstest"
)

// TestAgentMetricsOnOpsListener: the agent's ops mux serves its families, with
// outcomes and error classes as bounded lowercase labels.
func TestAgentMetricsOnOpsListener(t *testing.T) {
	reg := metrics.NewServiceRegistry("agent", "test")
	m := New(reg)

	m.SessionStarted(&nexusv1.ResourceVector{CpuMillis: 8000, MemoryBytes: 1 << 34, GpuCount: 2})
	m.TaskStarted("subprocess")
	m.TaskReported("subprocess", nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED, nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED,
		time.Now().Add(-3*time.Second), nil)
	m.TaskFinished("subprocess")
	m.TaskStarted("docker")
	m.TaskReported("docker", nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED, nexusv1.ErrorClass_ERROR_CLASS_OOM,
		time.Now(), errors.New("gateway down"))
	m.Heartbeat(false, 20*time.Millisecond, 1)
	m.Heartbeat(true, 5*time.Millisecond, 0)
	m.ResultStorePut("uploaded", 5<<20, 800*time.Millisecond)
	m.ResultStorePut("inline", 100, time.Microsecond)
	m.ResultStorePut("error", 1<<20, 2*time.Second)
	m.RegistrationAttempt("retryable_error")
	m.RegistrationAttempt("retryable_error")
	m.RegistrationAttempt("ok")
	m.SetRegistered(true)

	mux := metrics.OpsMux(reg, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {})
	})
	fams := metricstest.Scrape(t, mux, "/metrics")
	metricstest.RequireFamilies(t, fams,
		"nexus_agent_tasks_running", "nexus_agent_tasks_started_total", "nexus_agent_tasks_completed_total",
		"nexus_agent_task_duration_seconds", "nexus_agent_heartbeats_total",
		"nexus_agent_heartbeat_duration_seconds", "nexus_agent_heartbeat_consecutive_failures",
		"nexus_agent_result_store_puts_total", "nexus_agent_result_store_upload_bytes_total",
		"nexus_agent_result_store_upload_duration_seconds", "nexus_agent_result_store_upload_size_bytes",
		"nexus_agent_sessions_total", "nexus_agent_advertised_capacity",
		"nexus_agent_result_report_failures_total", "nexus_agent_registration_attempts_total",
		"nexus_agent_registered", "go_goroutines", metrics.BuildInfoName)

	if s := metricstest.Find(fams, "nexus_agent_registration_attempts_total",
		map[string]string{"result": "retryable_error"}); s == nil || s.GetCounter().GetValue() != 2 {
		t.Errorf("registration_attempts{retryable_error} = %v, want 2", s)
	}
	if s := metricstest.Find(fams, "nexus_agent_registered", nil); s == nil || s.GetGauge().GetValue() != 1 {
		t.Errorf("registered = %v, want 1", s)
	}

	if s := metricstest.Find(fams, "nexus_agent_tasks_completed_total",
		map[string]string{"executor": "docker", "outcome": "failed", "error_class": "oom"}); s == nil {
		t.Error("failed attempt not labelled outcome=failed error_class=oom")
	}
	if s := metricstest.Find(fams, "nexus_agent_tasks_completed_total",
		map[string]string{"executor": "subprocess", "outcome": "succeeded", "error_class": "none"}); s == nil {
		t.Error("succeeded attempt not labelled error_class=none")
	}
	if s := metricstest.Find(fams, "nexus_agent_tasks_running",
		map[string]string{"executor": "docker"}); s == nil || s.GetGauge().GetValue() != 1 {
		t.Errorf("running{docker} = %v, want 1", s)
	}
	if s := metricstest.Find(fams, "nexus_agent_result_store_upload_bytes_total",
		map[string]string{"result": "uploaded"}); s == nil || s.GetCounter().GetValue() != 5<<20 {
		t.Errorf("uploaded bytes = %v, want %d", s, 5<<20)
	}
	h := fams["nexus_agent_result_store_upload_duration_seconds"]
	for _, s := range h.GetMetric() {
		for _, lp := range s.GetLabel() {
			if lp.GetValue() == "inline" {
				t.Error("inline outputs were observed as uploads")
			}
		}
	}
	if s := metricstest.Find(fams, "nexus_agent_heartbeat_consecutive_failures", nil); s == nil || s.GetGauge().GetValue() != 0 {
		t.Errorf("heartbeat streak = %v after a success, want 0", s)
	}
}

func TestNilAgentMetricsIsSafe(t *testing.T) {
	var m *Metrics
	m.TaskStarted("x")
	m.TaskFinished("x")
	m.TaskReported("x", nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED, nexusv1.ErrorClass_ERROR_CLASS_OOM, time.Now(), nil)
	m.Heartbeat(true, time.Millisecond, 0)
	m.ResultStorePut("uploaded", 1, time.Millisecond)
	m.SessionStarted(nil)
	m.RegistrationAttempt("ok")
	m.SetRegistered(false)
	// The method value the agent hands to the result store must be callable
	// even when metrics are disabled.
	f := m.ResultStorePut
	f("error", 1, time.Millisecond)
}
