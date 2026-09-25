//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Cross-namespace run lifecycle, against REAL Postgres with two tenants
// (CONTRACT-FINDINGS F101).
//
// The workflow-manager acts on a run id alone, so the gateway is the only place
// a run is tied to the caller's namespace. Cancel, pause and resume checked that
// only AFTER the owner had mutated the run. The owner here is a fake that does
// exactly what the real one does -- a state CAS on workflow_runs by id, with no
// namespace predicate -- so if the gateway ever lets a foreign run through, this
// test sees the row change.
package rest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// idOnlyOwner mutates workflow_runs by id with no namespace, as the real
// workflow-manager does.
type idOnlyOwner struct {
	fakeWFM
	pool *pgxpool.Pool
}

func (o *idOnlyOwner) move(ctx context.Context, ns, id string, from []string, to string) (store.RunRow, error) {
	o.recordLifecycle(to, ns, id)
	if _, err := store.WFMTransitionRun(ctx, o.pool, id, from, to); err != nil {
		return store.RunRow{}, err
	}
	return store.GatewayGetRun(ctx, o.pool, ns, id)
}

func (o *idOnlyOwner) CancelRun(ctx context.Context, ns, id string) (store.RunRow, error) {
	return o.move(ctx, ns, id, []string{"PENDING", "RUNNING", "PAUSING", "PAUSED"}, "CANCELLING")
}
func (o *idOnlyOwner) PauseRun(ctx context.Context, ns, id string) (store.RunRow, error) {
	return o.move(ctx, ns, id, []string{"RUNNING"}, "PAUSING")
}
func (o *idOnlyOwner) ResumeRun(ctx context.Context, ns, id string) (store.RunRow, error) {
	return o.move(ctx, ns, id, []string{"PAUSED", "PAUSING"}, "RUNNING")
}
func (o *idOnlyOwner) RetryRun(ctx context.Context, ns, id string, _ []string, _ string) (store.RunRow, error) {
	return o.move(ctx, ns, id, []string{"FAILED", "TIMED_OUT"}, "RUNNING")
}

type tenantFixture struct {
	ns, key, runID string
}

// seedTenant registers a namespace with an admin key and one run in state.
func seedTenant(t *testing.T, pool *pgxpool.Pool, label, state string) tenantFixture {
	t.Helper()
	ctx := context.Background()
	f := tenantFixture{
		ns:  fmt.Sprintf("f101-%s-%d", label, time.Now().UnixNano()),
		key: fmt.Sprintf("nx_f101_%s_%d", label, time.Now().UnixNano()),
	}
	testdb.RegisterTenant(t, pool, f.ns)
	keyID, err := store.GatewayCreateAPIKey(ctx, pool, f.ns, "f101-"+label, "admin", f.key)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	var specID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1, 'f101', 1, 'hash-'||gen_random_uuid()::text, '{}'::jsonb, 'test')
		RETURNING id::text`, f.ns).Scan(&specID); err != nil {
		t.Fatalf("seed spec: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_runs (spec_id, namespace, state, trigger_kind, started_at)
		VALUES ($1, $2, $3::run_state, 'manual', now())
		RETURNING id::text`, specID, f.ns, state).Scan(&f.runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_nodes (run_id, node_id, task_ref, state, spec_json)
		VALUES ($1, 'a', 'a', 'FAILED', '{}'::jsonb)`, f.runID); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, keyID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflow_runs WHERE namespace = $1`, f.ns)
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflow_specs WHERE namespace = $1`, f.ns)
	})
	return f
}

func runStateOf(t *testing.T, pool *pgxpool.Pool, runID string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT state::text FROM workflow_runs WHERE id = $1::uuid`, runID).Scan(&s); err != nil {
		t.Fatalf("read run state: %v", err)
	}
	return s
}

func TestIntegrationLifecycleCannotReachAnotherTenantsRun(t *testing.T) {
	pool := attentionPool(t)
	cases := []struct {
		op, victimState, ownWant string
	}{
		{"cancel", "RUNNING", "CANCELLING"},
		{"pause", "RUNNING", "PAUSING"},
		{"resume", "PAUSED", "RUNNING"},
		{"retry", "FAILED", "RUNNING"},
	}
	for _, c := range cases {
		t.Run(c.op, func(t *testing.T) {
			attacker := seedTenant(t, pool, "a", c.victimState)
			victim := seedTenant(t, pool, "b", c.victimState)
			owner := &idOnlyOwner{pool: pool}
			api := New(pool, authn.New(pool), owner, fakeRM{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			h := api.Routes()

			call := func(ns, key, runID string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost,
					"/v1/namespaces/"+ns+"/runs/"+runID+"/"+c.op, strings.NewReader(""))
				req.Header.Set("Authorization", "Bearer "+key)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				return rec
			}

			// Tenant A, through its own namespace, names tenant B's run.
			rec := call(attacker.ns, attacker.key, victim.runID)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s of another tenant's run: status %d, want 404. body=%s", c.op, rec.Code, rec.Body)
			}
			if got := runStateOf(t, pool, victim.runID); got != c.victimState {
				t.Fatalf("%s of another tenant's run changed it %s -> %s", c.op, c.victimState, got)
			}
			if len(owner.lifecycles) != 0 {
				t.Fatalf("a cross-tenant %s reached the owner: %v", c.op, owner.lifecycles)
			}

			// Tenant A's key through B's namespace path is refused outright.
			if rec := call(victim.ns, attacker.key, victim.runID); rec.Code != http.StatusForbidden {
				t.Errorf("%s with a foreign namespace in the path: status %d, want 403", c.op, rec.Code)
			}

			// Each tenant still controls its own run.
			if rec := call(victim.ns, victim.key, victim.runID); rec.Code != http.StatusOK {
				t.Fatalf("%s of own run: status %d, body=%s", c.op, rec.Code, rec.Body)
			}
			if got := runStateOf(t, pool, victim.runID); got != c.ownWant {
				t.Errorf("%s of own run left it %s, want %s", c.op, got, c.ownWant)
			}
			if got := runStateOf(t, pool, attacker.runID); got != c.victimState {
				t.Errorf("tenant A's own run moved to %s during the test", got)
			}
		})
	}
}
