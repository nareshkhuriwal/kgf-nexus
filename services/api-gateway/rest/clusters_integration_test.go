// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// TestIntegrationClusterViewShape pins the JSON the console's Infrastructure
// page codes against for the fields ADR-0031 and ADR-0036 added to a cluster:
// labels, headroom and scheduler_autoscaling. Asserted key by key on the wire,
// not through the Go struct, because the wire shape IS the contract: a renamed
// key is a broken panel, and a struct round trip would not notice.
func TestIntegrationClusterViewShape(t *testing.T) {
	ctx := context.Background()
	pool := attentionPool(t)
	ns := fmt.Sprintf("rest-cluster-%d", time.Now().UnixNano())
	testdb.RegisterTenant(t, pool, ns)
	const key = "nx_cluster_view_probe_key"
	keyID, err := store.GatewayCreateAPIKey(ctx, pool, ns, "cluster-view-test", "admin", key)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, keyID)
		_, _ = pool.Exec(ctx, `DELETE FROM clusters WHERE namespace = $1`, ns)
		_, _ = pool.Exec(ctx, `DELETE FROM platform_config WHERE namespace = $1`, ns)
	})

	// The console saved scheduler settings: 2-8 replicas, 70 % CPU, 600 s.
	cfg := platform.Default()
	cfg.Scheduler = platform.Scheduler{MinReplicas: 2, MaxReplicas: 8, TargetCPU: 70, ScaleDownSeconds: 600, FairShareWeight: 1}
	if cfg, err = store.GatewayPlatformPut(ctx, pool, ns, cfg, store.AuditEntry{ActorKind: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GatewayClusterPut(ctx, pool, ns, "tester", store.ClusterSpec{Name: ns, Provider: "aks",
		ExpectedClusterUID: "6f2d7c1e-0a9b-4c8d-b7e6-5f4a3b2c1d0e", ControlPlaneNamespace: "kgf-nexus", Mode: "apply"},
		store.AuditEntry{ActorKind: "test"}); err != nil {
		t.Fatal(err)
	}
	var clusterID string
	if err := pool.QueryRow(ctx, `SELECT id FROM clusters WHERE name = $1`, ns).Scan(&clusterID); err != nil {
		t.Fatal(err)
	}

	// What the controller reports, through the resource manager's store: the
	// scheduler HPA one bounded step short of the saved settings.
	rm := store.NewRMStore(pool)
	two, three := int32(2), int32(3)
	if _, err := rm.ReportObservations(ctx, store.ObservationReport{ClusterID: clusterID, ControllerIdentity: "nexus-cluster-controller-0",
		ObservedClusterUID: "6f2d7c1e-0a9b-4c8d-b7e6-5f4a3b2c1d0e", EffectiveMode: "apply",
		CompleteKinds: []string{"SchedulerDeployment", "HorizontalPodAutoscaler"},
		Objects: []store.ObservedObject{
			{Kind: "SchedulerDeployment", Namespace: "kgf-nexus", Name: "scheduler", SummaryJSON: []byte(`{}`)},
			{Kind: "HorizontalPodAutoscaler", Namespace: "kgf-nexus", Name: "scheduler", SpecReplicas: &three, ReadyReplicas: &two,
				SummaryJSON: []byte(`{"min_replicas":2,"max_replicas":6,"target_cpu_percent":70,"scale_down_stabilization_seconds":600,` +
					`"scale_target":"Deployment/scheduler","managed":true}`)},
		},
		Labels:         map[string]string{"nexus.io/region": "westeurope", "nexus.io/provider": "azure", "nexus.io/cost-tier": "1"},
		LabelsReported: true,
		Headroom: map[string]store.ClusterHeadroom{"nexus-tasks": {CPUMillis: 6000, MemoryBytes: 25769803776, GPUCount: -1, Pods: 40,
			Extended: map[string]int64{"nvidia.com/mig-1g.10gb": 2}}},
		HeadroomReported: true,
	}); err != nil {
		t.Fatal(err)
	}
	from, to := platform.HPASettings{MinReplicas: 1, MaxReplicas: 4, TargetCPUPercent: 65, ScaleDownStabilizationSeconds: 300},
		platform.HPASettings{MinReplicas: 2, MaxReplicas: 6, TargetCPUPercent: 70, ScaleDownStabilizationSeconds: 600}
	if err := rm.RecordSchedulerAutoscaling(ctx, store.SchedulerAutoscalingReport{ClusterID: clusterID, Actor: "nexus-cluster-controller-0",
		Mode: "apply", Complete: true, Targets: []store.SchedulerHPAReconcile{{
			KubernetesNamespace: "kgf-nexus", HPAName: "scheduler", Action: "hpa_update", Applied: true, From: &from, To: &to,
			Reason: "set min 1→2, max 4→6, cpu 65%→70%, scale-down window 300s→600s; then max 6→8 in further bounded steps (platform config revision 1)",
			Status: store.SchedulerHPAStatus{Deployment: "scheduler", UID: "hpa-uid", Managed: true, Result: "applying",
				Reason: "set min 1→2, max 4→6; then max 6→8 in further bounded steps", ConfigRevision: cfg.Revision, Observed: to},
		}}}); err != nil {
		t.Fatal(err)
	}

	h := New(pool, authn.New(pool), &fakeWFM{}, fakeRM{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).Routes()
	req := httptest.NewRequest(http.MethodGet, "/v1/namespaces/"+ns+"/platform/clusters", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp struct {
		Clusters []map[string]any `json:"clusters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Clusters) != 1 {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	c := resp.Clusters[0]
	if b, err := json.MarshalIndent(map[string]any{"labels": c["labels"], "headroom": c["headroom"],
		"headroom_observed_at": c["headroom_observed_at"], "scheduler_autoscaling": c["scheduler_autoscaling"]}, "", "  "); err == nil {
		t.Logf("cluster view (ADR-0031/0036 fields):\n%s", b)
	}

	labels, _ := c["labels"].(map[string]any)
	if labels["nexus.io/region"] != "westeurope" || labels["nexus.io/provider"] != "azure" || labels["nexus.io/cost-tier"] != "1" {
		t.Errorf("labels = %v", c["labels"])
	}
	room, _ := c["headroom"].(map[string]any)["nexus-tasks"].(map[string]any)
	for k, want := range map[string]float64{"cpu_millis": 6000, "memory_bytes": 25769803776, "gpu_count": -1, "pods": 40} {
		if room[k] != want {
			t.Errorf("headroom[nexus-tasks][%s] = %v, want %v", k, room[k], want)
		}
	}
	if ext, _ := room["extended"].(map[string]any); ext["nvidia.com/mig-1g.10gb"] != float64(2) {
		t.Errorf("headroom extended = %v", room["extended"])
	}
	if _, ok := c["headroom_observed_at"].(string); !ok {
		t.Errorf("headroom_observed_at = %v", c["headroom_observed_at"])
	}

	sa, _ := c["scheduler_autoscaling"].(map[string]any)
	for _, k := range []string{"desired", "observed", "state", "last_applied_at", "last_error", "targets"} {
		if _, ok := sa[k]; !ok {
			t.Errorf("scheduler_autoscaling lacks %q: %v", k, sa)
		}
	}
	desired, _ := sa["desired"].(map[string]any)
	for k, want := range map[string]float64{"min_replicas": 2, "max_replicas": 8, "target_cpu_percent": 70,
		"scale_down_stabilization_seconds": 600, "config_revision": float64(cfg.Revision)} {
		if desired[k] != want {
			t.Errorf("desired[%s] = %v, want %v", k, desired[k], want)
		}
	}
	observed, _ := sa["observed"].(map[string]any)
	for k, want := range map[string]any{"min_replicas": 2.0, "max_replicas": 6.0, "target_cpu_percent": 70.0,
		"scale_down_stabilization_seconds": 600.0, "current_replicas": 2.0, "desired_replicas": 3.0,
		"hpa_name": "scheduler", "deployment": "scheduler"} {
		if observed[k] != want {
			t.Errorf("observed[%s] = %v, want %v", k, observed[k], want)
		}
	}
	if sa["state"] != "applying" || sa["last_error"] != nil {
		t.Errorf("state = %v, last_error = %v; want applying and null", sa["state"], sa["last_error"])
	}
	if _, err := time.Parse(time.RFC3339, fmt.Sprint(sa["last_applied_at"])); err != nil {
		t.Errorf("last_applied_at = %v, want RFC 3339", sa["last_applied_at"])
	}
	targets, _ := sa["targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("targets = %v", sa["targets"])
	}
	tgt := targets[0].(map[string]any)
	if tgt["state"] != "applying" || tgt["managed"] != true || tgt["reason"] == "" {
		t.Errorf("target = %v", tgt)
	}
	// The HPA's memory is not a worker Deployment's; its event is in the trail.
	if scaling, _ := c["scaling"].([]any); len(scaling) != 0 {
		t.Errorf("scaling = %v, want no rows (the HPA is shown under scheduler_autoscaling)", scaling)
	}
	events, _ := c["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("events = %v", c["events"])
	}
	ev := events[0].(map[string]any)
	if ev["action"] != "hpa_update" || ev["worker_deployment"] != "hpa/scheduler" || ev["applied"] != true {
		t.Errorf("event = %v", ev)
	}
	if detail, _ := ev["detail"].(map[string]any); detail["to"] == nil || detail["from"] == nil {
		t.Errorf("event detail = %v", ev["detail"])
	}
}
