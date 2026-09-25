//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// The replay assignment endpoint against REAL Postgres with two tenants
// (ADR-0022, CONTRACT-FINDINGS F101): the store's namespace predicate, the
// run_deletions filter and the pass-through of stored spec/input JSON are
// exercised by the SQL the gateway actually runs.
package rest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

func seedAttempt(t *testing.T, pool *pgxpool.Pool, runID string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		UPDATE task_nodes SET spec_json = $2::jsonb WHERE run_id = $1::uuid AND node_id = 'a'`,
		runID, `{"id":"a","task_ref":"t.a","version":1,"runtime":{"executor":"subprocess",`+
			`"command":["sh","-c","exit 1"],"secrets":[{"name":"db","ref":"vault://kv/db"}]}}`); err != nil {
		t.Fatalf("seed spec_json: %v", err)
	}
	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO task_attempts (run_id, node_id, attempt_number, state, resource_request,
		                           idempotency_key, input_refs)
		VALUES ($1::uuid, 'a', 1, 'FAILED', '{}'::jsonb, 'idem-replay',
		        '{"x":{"inline":"NDI=","content_type":"application/json"}}'::jsonb)
		RETURNING id::text`, runID).Scan(&id); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	return id
}

func TestIntegrationAttemptAssignmentIsTenantScoped(t *testing.T) {
	pool := attentionPool(t)
	attacker := seedTenant(t, pool, "asg-a", "FAILED")
	victim := seedTenant(t, pool, "asg-b", "FAILED")
	victimAttempt := seedAttempt(t, pool, victim.runID)

	api := New(pool, authn.New(pool), &fakeWFM{}, fakeRM{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := api.Routes()
	call := func(ns, key, run string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet,
			"/v1/namespaces/"+ns+"/runs/"+run+"/attempts/"+victimAttempt+"/assignment", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for _, run := range []string{victim.runID, "-"} {
		rec := call(victim.ns, victim.key, run)
		if rec.Code != http.StatusOK {
			t.Fatalf("own attempt via run %q: status %d body=%s", run, rec.Code, rec.Body)
		}
		var got struct {
			Assignment struct {
				IdempotencyKey string                     `json:"idempotency_key"`
				Inputs         map[string]json.RawMessage `json:"inputs"`
				Spec           struct {
					TaskRef string `json:"task_ref"`
				} `json:"spec"`
			} `json:"assignment"`
			Secrets []map[string]string `json:"secrets"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Assignment.IdempotencyKey != "idem-replay" || got.Assignment.Spec.TaskRef != "t.a" {
			t.Errorf("assignment = %+v", got.Assignment)
		}
		if !strings.Contains(string(got.Assignment.Inputs["x"]), `"NDI="`) {
			t.Errorf("input x = %s, want the stored inline ref", got.Assignment.Inputs["x"])
		}
		if len(got.Secrets) != 1 || got.Secrets[0]["ref"] != "vault://kv/db" {
			t.Errorf("secrets = %v", got.Secrets)
		}
		if strings.Contains(rec.Body.String(), "lease_token\":\"") {
			t.Error("a lease token was returned")
		}
	}

	// Tenant A naming B's attempt through A's own namespace: by run, and by
	// the wildcard that skips the run check and relies on the SQL predicate.
	if rec := call(attacker.ns, attacker.key, victim.runID); rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant by run: status %d, want 404", rec.Code)
	}
	if rec := call(attacker.ns, attacker.key, "-"); rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant by wildcard: status %d, want 404 body=%s", rec.Code, rec.Body)
	}
	// A's own run id with B's attempt id: the run belongs to A, the attempt
	// does not belong to that run.
	if rec := call(attacker.ns, attacker.key, attacker.runID); rec.Code != http.StatusNotFound {
		t.Errorf("foreign attempt under own run: status %d, want 404", rec.Code)
	}
	// A's key through B's namespace path.
	if rec := call(victim.ns, attacker.key, "-"); rec.Code != http.StatusForbidden {
		t.Errorf("foreign namespace in the path: status %d, want 403", rec.Code)
	}

	// A deleted run's attempts are gone from this endpoint too.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO run_deletions (run_id, namespace) VALUES ($1::uuid, $2)`,
		victim.runID, victim.ns); err != nil {
		t.Fatalf("mark run deleted: %v", err)
	}
	if rec := call(victim.ns, victim.key, "-"); rec.Code != http.StatusNotFound {
		t.Errorf("attempt of a deleted run: status %d, want 404", rec.Code)
	}
}
