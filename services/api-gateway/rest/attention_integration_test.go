//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// The attention endpoint is exercised against REAL Postgres rather than the
// restDB fake the rest of this package uses. The fake models only api_keys and
// idempotency_keys and cannot serve a Query at all, so a test built on it would
// assert the handler's map-building and nothing about the SQL, the ranking, or
// the tenancy predicate -- which is where all of this endpoint's risk lives.
package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// attentionPool connects or SKIPS, matching internal/store's testPool: these
// must not turn a laptop's `go test ./...` red, and the build tag already means
// they run only when asked for.
func attentionPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NEXUS_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:test@localhost:55432/nexus"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pool, err := store.GWOpen(ctx, dsn, 4)
	if err != nil {
		t.Skipf("skipping integration test: test database unreachable at %s: %v", dsn, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestIntegrationAttentionEndpointShape pins the JSON the UI codes against.
//
// Asserted field by field rather than by round-tripping a struct, because the
// wire shape IS the contract with the console: a renamed key is a broken panel,
// and a test that unmarshals into a struct it also defines would not notice.
func TestIntegrationAttentionEndpointShape(t *testing.T) {
	ctx := context.Background()
	pool := attentionPool(t)

	ns := fmt.Sprintf("rest-attn-%d", time.Now().UnixNano())
	const key = "nx_attention_probe_key"

	// The gateway OWNS api_keys, so this one is created for real and removed
	// afterwards; everything else is seeded in a transaction that is rolled back.
	keyID, err := store.GatewayCreateAPIKey(ctx, pool, ns, "attention-test", "admin", key)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, keyID) })

	// Seeded outside a rollback tx because the request goes through the pool on
	// its own connection and could not see an uncommitted transaction; cleaned
	// up explicitly instead. The namespace is unique, so nothing else is touched.
	var specID, runID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1, 'nightly_etl', 1, 'hash-'||gen_random_uuid()::text, '{}'::jsonb, 'test')
		RETURNING id::text`, ns).Scan(&specID); err != nil {
		t.Fatalf("seed spec: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM workflow_runs  WHERE namespace = $1`, ns)
		_, _ = pool.Exec(ctx, `DELETE FROM workflow_specs WHERE namespace = $1`, ns)
	})
	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_runs (spec_id, namespace, state, trigger_kind, started_at)
		VALUES ($1, $2, 'RUNNING', 'manual', now() - interval '1 hour')
		RETURNING id::text`, specID, ns).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_nodes (run_id, node_id, task_ref, state, max_attempts, attempts_used, spec_json)
		VALUES ($1, 'enrich', 'etl.enrich', 'RUNNING', 3, 1, '{}'::jsonb)`, runID); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	var attemptID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO task_attempts (run_id, node_id, attempt_number, state, resource_request, idempotency_key, scheduled_at)
		VALUES ($1, 'enrich', 1, 'LOST', '{}'::jsonb, 'idem-'||gen_random_uuid()::text, now() - interval '10 minutes')
		RETURNING id::text`, runID).Scan(&attemptID); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_events (run_id, seq, node_id, attempt_id, kind, payload, actor, occurred_at)
		VALUES ($1, 1, 'enrich', $2::uuid, 'IndeterminateOutcome',
		        '{"reason":"indeterminate_outcome_requires_human"}'::jsonb,
		        'scheduler', now() - interval '6 minutes')`, runID, attemptID); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	api := New(pool, authn.New(pool), &fakeWFM{}, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := api.Routes()

	req := httptest.NewRequest(http.MethodGet, "/v1/namespaces/"+ns+"/attention", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []map[string]any `json:"items"`
		Count int              `json:"count"`
		Trunc bool             `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if resp.Count != 1 || len(resp.Items) != 1 {
		t.Fatalf("want exactly 1 item, got count=%d items=%d: %s", resp.Count, len(resp.Items), rec.Body.String())
	}
	if resp.Trunc {
		t.Error("truncated should be false for a single item under the default limit")
	}

	it := resp.Items[0]
	// Every key the UI agent will code against.
	for _, k := range []string{"kind", "severity", "summary", "detail", "link",
		"run_id", "run_state", "spec_name", "occurred_at", "node_id", "attempt_id", "reason"} {
		if _, ok := it[k]; !ok {
			t.Errorf("payload is missing %q: %v", k, it)
		}
	}
	if it["kind"] != "indeterminate_outcome" || it["severity"] != "critical" {
		t.Errorf("kind/severity = %v/%v", it["kind"], it["severity"])
	}
	if it["reason"] != "indeterminate_outcome_requires_human" {
		t.Errorf("reason = %v, want the scheduler's own code", it["reason"])
	}
	if it["spec_name"] != "nightly_etl" || it["node_id"] != "enrich" {
		t.Errorf("item does not name the run/node it concerns: %v", it)
	}
	// The link is resolved server-side, deep to the attempt.
	wantLink := "/runs/" + runID + "?attempt=" + attemptID
	if it["link"] != wantLink {
		t.Errorf("link = %v, want %v", it["link"], wantLink)
	}
}

// A reader key must be able to read the panel: it is guarded on ActionRead, and
// "what is broken" is the question every operator needs answered during an
// incident.
func TestIntegrationAttentionRequiresAuth(t *testing.T) {
	ctx := context.Background()
	pool := attentionPool(t)
	ns := fmt.Sprintf("rest-attn-authz-%d", time.Now().UnixNano())

	readerID, err := store.GatewayCreateAPIKey(ctx, pool, ns, "reader", "reader", "nx_attn_reader")
	if err != nil {
		t.Fatalf("create reader key: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, readerID) })

	api := New(pool, authn.New(pool), &fakeWFM{}, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := api.Routes()

	// No credential at all is refused.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/namespaces/"+ns+"/attention", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", rec.Code)
	}

	// A read-only credential is allowed.
	req := httptest.NewRequest(http.MethodGet, "/v1/namespaces/"+ns+"/attention", nil)
	req.Header.Set("Authorization", "Bearer nx_attn_reader")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("reader status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}
