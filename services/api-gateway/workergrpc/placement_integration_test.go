// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package workergrpc

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

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/rest"
)

// TestIntegrationPlacementReachesTheTaskRunnerView is ADR-0037's placement
// path end to end against Postgres: a kubernetes attempt's agent reports its
// pod (not yet scheduled), then its node, through the real ReportPlacement
// with the real fence; a zombie's report is refused; and GET
// .../platform/task-runners, through the real router and guard, shows it.
func TestIntegrationPlacementReachesTheTaskRunnerView(t *testing.T) {
	ctx := context.Background()
	openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pool, err := store.GWOpen(openCtx, testdb.DSN(t), 4)
	if err != nil {
		testdb.SkipUnreachable(t, err)
	}
	t.Cleanup(pool.Close)

	ns := fmt.Sprintf("placement-e2e-%d", time.Now().UnixNano())
	attemptID := testdb.NewAttempt(t, pool, ns)
	var workerID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workers (namespace, hostname, queues, capacity, agent_version, session_token_hash)
		VALUES ($1, 'nexus-agent-gpu-5d8f7c6b9-qk2lm', ARRAY['default'], '{"cpu_millis":8000}', '1.9.0', $2)
		RETURNING id::text`, ns, store.HashAPIKey("session-"+ns)).Scan(&workerID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM workers WHERE id = $1`, workerID) })
	for _, q := range []string{
		`UPDATE task_attempts SET state = 'RUNNING', worker_id = $2, dispatched_at = now(), started_at = now() WHERE id = $1`,
		`INSERT INTO leases (attempt_id, worker_id, token, expires_at) VALUES ($1, $2, 'tok-live', now() + interval '30 seconds')`,
	} {
		if _, err := pool.Exec(ctx, q, attemptID, workerID); err != nil {
			t.Fatal(err)
		}
	}

	cfg := DefaultConfig()
	if cfg.PlacementReporting, err = store.GatewayAttemptRuntimePresent(ctx, pool); err != nil || !cfg.PlacementReporting {
		t.Fatalf("migration 0042 not detected: %v", err)
	}
	srv := NewServer(pool, &fakeRM{exists: true, state: "ACTIVE"}, &fakeSched{}, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	wctx := authn.WithPrincipal(ctx, authn.Principal{KeyID: "k", Namespace: ns, Name: "agent", Role: authn.RoleWorker})
	report := func(token string, p *nexusv1.AttemptPlacement) *nexusv1.Ack {
		t.Helper()
		ack, err := srv.ReportPlacement(wctx, &nexusv1.ReportPlacementRequest{AttemptId: attemptID, LeaseToken: token, Placement: p})
		if err != nil {
			t.Fatal(err)
		}
		return ack
	}

	// The pod exists, the scheduler has not bound it yet.
	pod := &nexusv1.AttemptPlacement{Executor: "k8s", Cluster: "aks-prod", K8SNamespace: "nexus-tasks", Pod: "nexus-train-x7k2p"}
	if ack := report("tok-live", pod); !ack.GetAccepted() {
		t.Fatalf("pending pod refused: %v", ack)
	}
	// A zombie holding an old token changes nothing.
	if ack := report("tok-old", &nexusv1.AttemptPlacement{Executor: "docker", Pod: "zombie"}); ack.GetAccepted() || ack.GetReason() != store.ReasonStaleLeaseToken {
		t.Fatalf("zombie report = %v", ack)
	}
	// Scheduled.
	pod.K8SNode = "aks-gpu-27182818-vmss000003"
	if ack := report("tok-live", pod); !ack.GetAccepted() {
		t.Fatalf("scheduled pod refused: %v", ack)
	}

	adminKey := "nx_placement_e2e_" + ns
	if _, err := store.GatewayCreateAPIKey(ctx, pool, ns, "e2e-admin", "admin", adminKey); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE namespace = $1`, ns) })
	h := rest.New(pool, authn.New(pool).WithRBAC(), nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).Routes()
	r := httptest.NewRequest(http.MethodGet, "/v1/namespaces/"+ns+"/platform/task-runners", nil)
	r.Header.Set("Authorization", "Bearer "+adminKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("task-runners: %d %s", w.Code, w.Body)
	}
	t.Logf("GET /v1/namespaces/%s/platform/task-runners:\n%s", ns, w.Body)

	var body struct {
		GeneratedAt string `json:"generated_at"`
		Summary     struct {
			Running    int            `json:"running"`
			ByExecutor map[string]int `json:"by_executor"`
			ByCluster  map[string]int `json:"by_cluster"`
		} `json:"summary"`
		Runners []map[string]any `json:"runners"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, body.GeneratedAt); err != nil {
		t.Errorf("generated_at %q: %v", body.GeneratedAt, err)
	}
	if body.Summary.Running != 1 || body.Summary.ByExecutor["kubernetes"] != 1 || body.Summary.ByCluster["aks-prod"] != 1 {
		t.Errorf("summary = %+v", body.Summary)
	}
	if len(body.Runners) != 1 {
		t.Fatalf("runners = %v", body.Runners)
	}
	got := body.Runners[0]
	for k, want := range map[string]any{
		"attempt_id": attemptID, "node_id": "node", "worker_id": workerID,
		"worker_host": "nexus-agent-gpu-5d8f7c6b9-qk2lm", "executor": "kubernetes", "cluster": "aks-prod",
		"k8s_namespace": "nexus-tasks", "pod": "nexus-train-x7k2p", "k8s_node": "aks-gpu-27182818-vmss000003",
		"state": "RUNNING", "progress_message": "",
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
	for _, k := range []string{"run_id", "workflow", "started_at", "placement_reported_at"} {
		if s, _ := got[k].(string); s == "" {
			t.Errorf("%s is empty: %v", k, got[k])
		}
	}
	if len(got) != 15 {
		t.Errorf("runner has %d fields, contract C3 names 15: %v", len(got), got)
	}
}
