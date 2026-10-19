// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

func TestParseComponentTopology(t *testing.T) {
	for in, want := range map[string]ComponentTopology{
		"":                         TopologyPlatformOperators,
		"platform-operators":       TopologyPlatformOperators,
		" infrastructure-viewers ": TopologyInfrastructureViewers,
	} {
		got, err := ParseComponentTopology(in)
		if err != nil || got != want {
			t.Errorf("ParseComponentTopology(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"all", "operators", "Infrastructure-Viewers"} {
		if _, err := ParseComponentTopology(bad); err == nil {
			t.Errorf("%q was accepted; a typo must not silently publish or hide the topology", bad)
		}
	}
}

func instanceRow(component, instance string, ready, stale bool) store.ServiceInstanceView {
	return store.ServiceInstanceView{
		Component: component, Instance: instance, PodNamespace: "kgf-nexus", Node: "node-a",
		Cluster: "aks-prod", Version: "1.9.0", Ready: ready, Stale: stale,
		StartedAt: time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC), LastSeenAt: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC),
		Detail: json.RawMessage(`{"readiness":"ok","queue":"default","claims_total":5,"pid":7,"heartbeat_interval_seconds":10}`),
	}
}

// TestBuildComponentsShape pins contract C3: all six components in the fixed
// order whatever the rows, states derived stale-first, the summary counted
// per component, and rows of unknown components left out.
func TestBuildComponentsShape(t *testing.T) {
	at := time.Date(2026, 10, 19, 9, 0, 5, 0, time.UTC)
	rows := []store.ServiceInstanceView{
		instanceRow("scheduler", "sched-a", true, false),
		instanceRow("scheduler", "sched-b", false, false),
		instanceRow("scheduler", "sched-c", true, true), // ready when it last wrote, silent since
		instanceRow("api-gateway", "gw-a", true, false),
		instanceRow("cluster-controller", "cc-a", true, false),
	}
	out := buildComponents(at, rows, true)
	if !out.GeneratedAt.Equal(at) || out.StaleAfterSeconds != 30 || !out.TopologyVisible {
		t.Fatalf("header = %+v", out)
	}
	var names []string
	for _, c := range out.Components {
		names = append(names, c.Component)
	}
	if got := strings.Join(names, ","); got != "api-gateway,workflow-manager,dag-engine,scheduler,resource-manager,event-service" {
		t.Fatalf("components = %s", got)
	}
	sched := out.Components[3]
	if sched.Summary != (componentSummary{Ready: 1, Total: 3, Stale: 1}) {
		t.Errorf("scheduler summary = %+v", sched.Summary)
	}
	states := map[string]string{}
	for _, i := range sched.Instances {
		states[i.Instance] = i.State
	}
	if states["sched-a"] != "ready" || states["sched-b"] != "not_ready" || states["sched-c"] != "stale" {
		t.Errorf("states = %v", states)
	}
	if wfm := out.Components[1]; wfm.Summary != (componentSummary{}) || wfm.Instances == nil || len(wfm.Instances) != 0 {
		t.Errorf("an empty component must be total 0 with instances [], got %+v", wfm)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "cluster-controller") || strings.Contains(string(b), `"instances":null`) {
		t.Errorf("wire form = %s", b)
	}
	if got := sched.Instances[0]; got.Node != "node-a" || got.Cluster != "aks-prod" || !strings.Contains(string(got.Detail), "claims_total") {
		t.Errorf("full view lost topology: %+v", got)
	}
}

// TestMaskedComponentsShowHealthOnly is ADR-0037 §4: without topology
// visibility an instance keeps its health and loses its placement and the
// platform-wide detail, under a label that is stable and distinct.
func TestMaskedComponentsShowHealthOnly(t *testing.T) {
	rows := []store.ServiceInstanceView{
		instanceRow("scheduler", "kgf-nexus-scheduler-7c9d6b5f4-2xkqp", true, false),
		instanceRow("scheduler", "kgf-nexus-scheduler-7c9d6b5f4-8mzrt", false, true),
	}
	out := buildComponents(time.Now(), rows, false)
	if out.TopologyVisible {
		t.Fatal("masked listing claims the topology is visible")
	}
	insts := out.Components[3].Instances
	if len(insts) != 2 || insts[0].Instance == insts[1].Instance {
		t.Fatalf("masked labels must stay distinct: %+v", insts)
	}
	for _, i := range insts {
		if !strings.HasPrefix(i.Instance, "scheduler-") || len(i.Instance) != len("scheduler-")+12 ||
			strings.Contains(i.Instance, "7c9d6b5f4") {
			t.Errorf("label %q is not an opaque scheduler-<12 hex>", i.Instance)
		}
		if i.Node != "" || i.Cluster != "" || i.PodNamespace != "" {
			t.Errorf("placement leaked: %+v", i)
		}
		if i.Version != "1.9.0" || i.StartedAt.IsZero() || i.LastSeenAt.IsZero() {
			t.Errorf("health lost: %+v", i)
		}
		var d map[string]any
		if err := json.Unmarshal(i.Detail, &d); err != nil {
			t.Fatal(err)
		}
		if len(d) != 2 || d["readiness"] != "ok" || d["heartbeat_interval_seconds"] != float64(10) {
			t.Errorf("masked detail = %v; want only the health keys", d)
		}
	}
	if insts[0].State != "ready" || insts[1].State != "stale" {
		t.Errorf("states = %s, %s", insts[0].State, insts[1].State)
	}
	// Stable: the same identity yields the same label on every call (and on
	// every gateway replica, since nothing per-process enters it).
	again := buildComponents(time.Now(), rows, false).Components[3].Instances
	if again[0].Instance != insts[0].Instance {
		t.Error("the masked label changed between calls")
	}
}
