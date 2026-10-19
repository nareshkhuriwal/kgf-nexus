// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
)

// schedulerFixture is a cluster whose controller reported one scheduler
// Deployment and its HPA (min 2, max 6, CPU 65 %, window 300 s), plus an HPA
// of something else that must be ignored.
func schedulerFixture(mode string, hpaSummary string) *ClusterView {
	two, three := int32(2), int32(3)
	c := &ClusterView{ClusterSpec: ClusterSpec{ControlPlaneNamespace: "kgf-nexus"}, Status: &ClusterStatusView{EffectiveMode: mode}}
	c.Observations = []ObservationView{
		{Kind: "SchedulerDeployment", Namespace: "kgf-nexus", Name: "scheduler", Summary: json.RawMessage(`{}`)},
		{Kind: "HorizontalPodAutoscaler", Namespace: "kgf-nexus", Name: "scheduler", SpecReplicas: &three, ReadyReplicas: &two,
			Summary: json.RawMessage(hpaSummary)},
		{Kind: "HorizontalPodAutoscaler", Namespace: "kgf-nexus", Name: "api-gateway",
			Summary: json.RawMessage(`{"min_replicas":2,"max_replicas":4,"scale_target":"Deployment/api-gateway"}`)},
	}
	return c
}

const reportedHPA = `{"min_replicas":2,"max_replicas":6,"target_cpu_percent":65,"scale_down_stabilization_seconds":300,` +
	`"scale_target":"Deployment/scheduler","managed":true}`

func savedPolicy(rev int64, min, max, cpu, window int) platform.Config {
	cfg := platform.Default()
	cfg.Revision = rev
	cfg.Scheduler.MinReplicas, cfg.Scheduler.MaxReplicas, cfg.Scheduler.TargetCPU, cfg.Scheduler.ScaleDownSeconds = min, max, cpu, window
	return cfg
}

func TestSchedulerAutoscalingStates(t *testing.T) {
	applied := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	state := func(result, errText string, rev int64) map[string]schedulerHPAState {
		return map[string]schedulerHPAState{"kgf-nexus/scheduler": {LastAppliedAt: &applied,
			Status: SchedulerHPAStatus{Result: result, Error: errText, Reason: "reason: " + result, ConfigRevision: rev}}}
	}
	differs := savedPolicy(4, 3, 10, 70, 600)
	cases := []struct {
		name    string
		mode    string
		cfg     platform.Config
		states  map[string]schedulerHPAState
		want    string
		wantErr string
	}{
		{"equal is in sync in either mode", "apply", savedPolicy(4, 2, 6, 65, 300), state("in_sync", "", 4), "in_sync", ""},
		{"plan mode", "plan", differs, state("plan", "", 4), "plan_only", ""},
		{"apply, controller has not seen this revision", "apply", differs, state("in_sync", "", 3), "applying", ""},
		{"apply, bounded steps in progress", "apply", differs, state("applying", "", 4), "applying", ""},
		{"apply, cannot converge", "apply", differs, state("blocked", "not controller-owned", 4), "drift", "not controller-owned"},
		{"apply, unsaved settings", "apply", savedPolicy(0, 1, 4, 65, 300), state("not_configured", "never saved", 0), "drift", "never saved"},
		{"apply, controller predates ADR-0036", "apply", differs, nil, "drift", "upgrade the cluster controller"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := schedulerAutoscaling(schedulerFixture(tc.mode, reportedHPA), tc.cfg, tc.states)
			if v.State != tc.want {
				t.Fatalf("state %s, want %s (%+v)", v.State, tc.want, v)
			}
			if len(v.Targets) != 1 || v.Observed == nil || v.Observed.HPAName != "scheduler" || v.Observed.Deployment != "scheduler" ||
				v.Observed.MinReplicas != 2 || v.Observed.MaxReplicas != 6 || *v.Observed.TargetCPUPercent != 65 ||
				*v.Observed.ScaleDownStabilizationSeconds != 300 || v.Observed.CurrentReplicas != 2 || v.Observed.DesiredReplicas != 3 {
				t.Fatalf("observed %+v (targets %d)", v.Observed, len(v.Targets))
			}
			if v.Desired.ConfigRevision != tc.cfg.Revision || v.Desired.MaxReplicas != tc.cfg.Scheduler.MaxReplicas {
				t.Fatalf("desired %+v", v.Desired)
			}
			switch {
			case tc.wantErr == "" && v.LastError != nil:
				t.Fatalf("last_error %q, want none", *v.LastError)
			case tc.wantErr != "" && (v.LastError == nil || !strings.Contains(*v.LastError, tc.wantErr)):
				t.Fatalf("last_error %v, want %q", v.LastError, tc.wantErr)
			}
			if tc.states != nil && (v.LastAppliedAt == nil || !v.LastAppliedAt.Equal(applied)) {
				t.Fatalf("last_applied_at %v", v.LastAppliedAt)
			}
			if !v.Targets[0].Managed {
				t.Fatalf("managed flag lost")
			}
		})
	}
}

func TestSchedulerAutoscalingNotObservedAndWorstTarget(t *testing.T) {
	c := schedulerFixture("apply", reportedHPA)
	c.Observations = c.Observations[2:] // only the unrelated HPA
	v := schedulerAutoscaling(c, savedPolicy(4, 3, 10, 70, 600), nil)
	if v.State != "not_observed" || v.Observed != nil || len(v.Targets) != 0 || v.LastAppliedAt != nil || v.LastError != nil {
		t.Fatalf("no scheduler HPA: %+v", v)
	}

	// Two schedulers: one in sync, one drifting; the top level shows the drift.
	c = schedulerFixture("apply", reportedHPA)
	c.Observations = append(c.Observations,
		ObservationView{Kind: "SchedulerDeployment", Namespace: "kgf-nexus", Name: "scheduler-gpu", Summary: json.RawMessage(`{}`)},
		ObservationView{Kind: "HorizontalPodAutoscaler", Namespace: "kgf-nexus", Name: "scheduler-gpu",
			Summary: json.RawMessage(`{"min_replicas":1,"max_replicas":2,"target_cpu_percent":null,"scale_down_stabilization_seconds":300,` +
				`"scale_target":"Deployment/scheduler-gpu","managed":true}`)})
	states := map[string]schedulerHPAState{
		"kgf-nexus/scheduler":     {Status: SchedulerHPAStatus{Result: "in_sync", ConfigRevision: 4}},
		"kgf-nexus/scheduler-gpu": {Status: SchedulerHPAStatus{Result: "blocked", Error: "no CPU utilization metric", ConfigRevision: 4}},
	}
	v = schedulerAutoscaling(c, savedPolicy(4, 2, 6, 65, 300), states)
	if v.State != "drift" || v.Observed.HPAName != "scheduler-gpu" || v.Observed.TargetCPUPercent != nil ||
		len(v.Targets) != 2 || v.Targets[0].State != "in_sync" || v.Targets[1].State != "drift" {
		t.Fatalf("worst target not surfaced: %+v", v)
	}
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"desired", "observed", "state", "last_applied_at", "last_error", "targets"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("view JSON lacks %q: %s", k, b)
		}
	}
	if SchedulerHPATargetPrefix != "hpa/" {
		t.Fatalf("the store's SQL spells the HPA target prefix as 'hpa/%%'; keep them equal")
	}
}
