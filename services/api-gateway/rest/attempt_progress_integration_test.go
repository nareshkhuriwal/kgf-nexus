//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// The attempt's executor status (F96) on the REST wire, against REAL Postgres.
//
// The scheduler stores the latest ReportProgress in attempt_progress
// (migration 0024); the gateway must join it onto every attempt of the run
// detail, as four ADDITIVE keys, and must keep them distinct from
// pending_reason. An attempt with no progress row must still carry the keys,
// as null, so the console can tell "the worker never reported" from "this
// gateway does not serve the field".
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

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

func TestIntegrationRunDetailAttemptProgress(t *testing.T) {
	ctx := context.Background()
	pool := attentionPool(t)

	ns := fmt.Sprintf("rest-progress-%d", time.Now().UnixNano())
	testdb.RegisterTenant(t, pool, ns)
	const key = "nx_progress_probe_key"

	keyID, err := store.GatewayCreateAPIKey(ctx, pool, ns, "progress-test", "admin", key)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, keyID) })

	var specID, runID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1, 'k8s', 1, 'hash-'||gen_random_uuid()::text, '{}'::jsonb, 'test')
		RETURNING id::text`, ns).Scan(&specID); err != nil {
		t.Fatalf("seed spec: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM workflow_runs  WHERE namespace = $1`, ns)
		_, _ = pool.Exec(ctx, `DELETE FROM workflow_specs WHERE namespace = $1`, ns)
	})
	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_runs (spec_id, namespace, state, trigger_kind, started_at)
		VALUES ($1, $2, 'RUNNING', 'manual', now())
		RETURNING id::text`, specID, ns).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	attempt := func(node, state, pendingReason string) string {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO task_nodes (run_id, node_id, task_ref, state, max_attempts, attempts_used, spec_json)
			VALUES ($1, $2, 'ml.'||$2, 'RUNNING', 3, 1, '{}'::jsonb)`, runID, node); err != nil {
			t.Fatalf("seed node %s: %v", node, err)
		}
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO task_attempts (run_id, node_id, attempt_number, state, queue,
			                           resource_request, idempotency_key, pending_reason)
			VALUES ($1, $2, 1, $3::attempt_state, 'default', '{}'::jsonb, 'idem-'||$2, NULLIF($4, ''))
			RETURNING id::text`, runID, node, state, pendingReason).Scan(&id); err != nil {
			t.Fatalf("seed attempt %s: %v", node, err)
		}
		return id
	}
	const podMsg = "pod Pending: Unschedulable: 0/3 nodes are available: 3 Insufficient cpu"
	withProgress := attempt("train", "RUNNING", "")
	silent := attempt("eval", "RUNNING", "")
	pending := attempt("report", "PENDING", "waiting_for_resources: cpu (need 8000, free 2000)")

	reported := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Millisecond)
	changed := reported.Add(-3 * time.Minute)
	if _, err := pool.Exec(ctx, `
		INSERT INTO attempt_progress (attempt_id, run_id, lease_epoch, message, fraction,
		                              reported_at, message_changed_at)
		VALUES ($1, $2, 0, $3, 0.4, $4, $5)`, withProgress, runID, podMsg, reported, changed); err != nil {
		t.Fatalf("seed progress: %v", err)
	}

	api := New(pool, authn.New(pool), &fakeWFM{}, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/v1/namespaces/"+ns+"/runs/"+runID, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	api.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Attempts []map[string]json.RawMessage `json:"attempts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]map[string]json.RawMessage{}
	for _, a := range body.Attempts {
		var id string
		_ = json.Unmarshal(a["id"], &id)
		byID[id] = a
	}
	keys := []string{"progress_message", "progress_fraction", "progress_at", "progress_message_at"}

	got := byID[withProgress]
	if got == nil {
		t.Fatalf("attempt %s missing from run detail: %s", withProgress, rec.Body.String())
	}
	var msg string
	var frac float64
	var at, msgAt time.Time
	for k, dst := range map[string]any{
		"progress_message": &msg, "progress_fraction": &frac,
		"progress_at": &at, "progress_message_at": &msgAt,
	} {
		if err := json.Unmarshal(got[k], dst); err != nil {
			t.Fatalf("%s = %s: %v", k, got[k], err)
		}
	}
	if msg != podMsg {
		t.Errorf("progress_message = %q, want %q", msg, podMsg)
	}
	if frac != 0.4 {
		t.Errorf("progress_fraction = %v, want 0.4", frac)
	}
	if !at.Equal(reported) || !msgAt.Equal(changed) {
		t.Errorf("progress_at/progress_message_at = %v/%v, want %v/%v", at, msgAt, reported, changed)
	}
	// The executor's status must not leak into the scheduler's field.
	if string(got["pending_reason"]) != "null" {
		t.Errorf("pending_reason = %s on a placed attempt; the executor status must stay in its own field", got["pending_reason"])
	}

	for _, id := range []string{silent, pending} {
		a := byID[id]
		for _, k := range keys {
			raw, ok := a[k]
			if !ok {
				t.Errorf("attempt %s has no %q key; absent and null are different answers", id, k)
			} else if string(raw) != "null" {
				t.Errorf("attempt %s %s = %s, want null (the worker never reported)", id, k, raw)
			}
		}
	}
	var reason string
	_ = json.Unmarshal(byID[pending]["pending_reason"], &reason)
	if reason == "" {
		t.Error("the pending attempt lost its pending_reason")
	}
}
