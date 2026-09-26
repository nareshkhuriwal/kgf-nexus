//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// The resource manager's placement-fact writes (ADR-0030 §4, ADR-0031), as the
// resource-manager ROLE, so the 0035 columns are proven writable by their owner
// and bounded on input.
func TestIntegrationClusterLabelsHeadroomAndCachedArtifacts(t *testing.T) {
	admin := testPool(t)
	ctx := context.Background()
	ns, _ := seedClusterNamespace(t, admin)
	gw := rolePool(t, "nexus_api_gateway")
	rm := NewRMStore(rolePool(t, "nexus_resource_manager"))

	if _, err := GatewayClusterPut(ctx, gw, ns, "tester", ClusterSpec{Name: ns, Provider: "kubernetes", Mode: "plan"},
		AuditEntry{ActorKind: "test"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	var clusterID string
	if err := admin.QueryRow(ctx, `SELECT id FROM clusters WHERE name=$1`, ns).Scan(&clusterID); err != nil {
		t.Fatal(err)
	}
	report := ObservationReport{ClusterID: clusterID, ControllerIdentity: "c-0", EffectiveMode: "plan",
		Labels: map[string]string{"nexus.io/region": "eu-west-1", "nexus.io/cost-tier": "2"}, LabelsReported: true,
		Headroom: map[string]ClusterHeadroom{"tasks": {CPUMillis: 4000, MemoryBytes: -1, GPUCount: 2, Pods: 10}},
		HeadroomReported: true}
	if _, err := rm.ReportObservations(ctx, report); err != nil {
		t.Fatalf("report: %v", err)
	}
	var labels, headroom []byte
	var at *string
	if err := admin.QueryRow(ctx, `SELECT labels, headroom, headroom_observed_at::text FROM cluster_status WHERE cluster_id=$1`,
		clusterID).Scan(&labels, &headroom, &at); err != nil {
		t.Fatal(err)
	}
	var l map[string]string
	var h map[string]ClusterHeadroom
	_ = json.Unmarshal(labels, &l)
	_ = json.Unmarshal(headroom, &h)
	if l["nexus.io/region"] != "eu-west-1" || h["tasks"].CPUMillis != 4000 || h["tasks"].MemoryBytes != -1 || at == nil {
		t.Fatalf("stored labels %v headroom %v at %v", l, h, at)
	}

	// An older controller (reports neither) leaves them alone.
	if _, err := rm.ReportObservations(ctx, ObservationReport{ClusterID: clusterID, ControllerIdentity: "c-0",
		EffectiveMode: "plan"}); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, `SELECT labels FROM cluster_status WHERE cluster_id=$1`, clusterID).Scan(&labels); err != nil {
		t.Fatal(err)
	}
	if _ = json.Unmarshal(labels, &l); l["nexus.io/region"] != "eu-west-1" {
		t.Fatalf("a report without labels erased them: %s", labels)
	}

	// Bounded input.
	big := map[string]string{}
	for i := 0; i < maxClusterLabels+1; i++ {
		big[string(rune('a'+i%26))+string(rune('A'+i/26))] = "x"
	}
	if _, err := rm.ReportObservations(ctx, ObservationReport{ClusterID: clusterID, ControllerIdentity: "c-0",
		EffectiveMode: "plan", Labels: big, LabelsReported: true}); !errors.Is(err, ErrInvalidClusterReport) {
		t.Fatalf("oversized label set accepted: %v", err)
	}

	// Cached artifacts, by the owner role.
	var wid string
	if err := admin.QueryRow(ctx, `
INSERT INTO workers (namespace, hostname, queues, capacity, committed, state, agent_version, session_token_hash)
VALUES ($1,'h',ARRAY['q'],'{}','{}','ACTIVE','t','h') RETURNING id`, ns).Scan(&wid); err != nil {
		t.Fatal(err)
	}
	if err := rm.RecordCachedArtifacts(ctx, wid, []string{"sha256:a", "sha256:b"}); err != nil {
		t.Fatal(err)
	}
	var cached []string
	if err := admin.QueryRow(ctx, `SELECT cached_artifacts FROM workers WHERE id=$1`, wid).Scan(&cached); err != nil {
		t.Fatal(err)
	}
	if len(cached) != 2 {
		t.Fatalf("cached %v", cached)
	}
	many := make([]string, 400)
	for i := range many {
		many[i] = "k"
	}
	if err := rm.RecordCachedArtifacts(ctx, wid, many); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, `SELECT cached_artifacts FROM workers WHERE id=$1`, wid).Scan(&cached); err != nil {
		t.Fatal(err)
	}
	if len(cached) != maxCachedArtifacts {
		t.Fatalf("stored %d keys, want the bound %d", len(cached), maxCachedArtifacts)
	}
}

// The scheduler ROLE can charge both retry budgets it writes (F147).
func TestIntegrationSchedulerRoleChargesRetryBudgets(t *testing.T) {
	sch := rolePool(t, "nexus_scheduler")
	ctx := context.Background()
	// A no-op UPDATE still requires the column privilege.
	if _, err := sch.Exec(ctx, `UPDATE task_nodes SET attempts_used = attempts_used, preemptions_used = preemptions_used WHERE false`); err != nil {
		t.Fatalf("scheduler role cannot write the retry budgets: %v", err)
	}
}
