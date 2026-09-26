//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
)

// flakyMaterializer is a dag-engine whose MaterializeRun fails the first
// failFirst calls -- a timeout, an engine restart -- and afterwards writes a
// node, as the real one would.
type flakyMaterializer struct {
	*fakeEngine
	failFirst int32
	calls     atomic.Int32
}

func (f *flakyMaterializer) MaterializeRun(ctx context.Context, runID, _ string, _ []byte) error {
	if f.calls.Add(1) <= f.failFirst {
		return errors.New("injected: dag-engine unavailable")
	}
	_, err := f.pool.Exec(ctx, `
		INSERT INTO task_nodes (run_id, node_id, task_ref, spec_json)
		VALUES ($1::uuid, 'a', 'a:v1', '{}'::jsonb) ON CONFLICT DO NOTHING`, runID)
	return err
}

func materializeFixture(t *testing.T, pool *pgxpool.Pool) (ns string) {
	t.Helper()
	ns = fmt.Sprintf("mat-%d", time.Now().UnixNano())
	testdb.RegisterTenant(t, pool, ns)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1, 'wf', 1, 'h-'||gen_random_uuid()::text, '{}'::jsonb, 'test')`, ns); err != nil {
		t.Fatalf("seed spec: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflow_runs WHERE namespace = $1`, ns)
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflow_specs WHERE namespace = $1`, ns)
	})
	return ns
}

// F160, the create path: the run row commits, then the materialize call is
// lost. The run is PENDING with no graph -- nothing will ever start it, and it
// holds a max_active_runs slot. The schedule and sensor loops retry run
// creation under the same idempotency key, but the deduplicated branch did
// NOTHING, so every retry returned the dead run.
func TestIntegrationALostMaterializeIsRepairedByTheIdempotentRetry(t *testing.T) {
	pool := retryPool(t)
	ns := materializeFixture(t, pool)
	eng := &flakyMaterializer{fakeEngine: &fakeEngine{pool: pool}, failFirst: 1}
	svc := New(pool, &fakeEvents{}, eng, noopScheduler{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	req := CreateRequest{Namespace: ns, SpecName: "wf", IdempotencyKey: "tick-1", TriggerKind: "schedule"}

	if _, err := svc.Create(ctx, req); err == nil {
		t.Fatal("the first create succeeded; the injected materialize failure did not happen")
	}
	res, err := svc.Create(ctx, req)
	if err != nil {
		t.Fatalf("the retried create: %v", err)
	}
	if res.Created {
		t.Fatal("the retry created a second run")
	}
	counts, err := store.WFMCountRunNodesByState(ctx, pool, res.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) == 0 {
		t.Fatalf("the retried create returned run %s with no graph; it will never start "+
			"(materialize calls: %d)", res.Run.ID, eng.calls.Load())
	}

	// A healthy deduplicated create must still do nothing else.
	before := eng.calls.Load()
	if _, err := svc.Create(ctx, req); err != nil {
		t.Fatal(err)
	}
	if eng.calls.Load() != before {
		t.Error("a deduplicated create of a materialized run materialized it again")
	}
}

// F160, the backstop: a create with no retrying caller (an API client that
// gave up) leaves the same dead run. The sweep lists PENDING runs with no
// graph once they are older than the race guard, and EnsureMaterialized
// finishes them.
func TestIntegrationUnmaterializedRunsAreSweptAndMaterialized(t *testing.T) {
	pool := retryPool(t)
	ns := materializeFixture(t, pool)
	eng := &flakyMaterializer{fakeEngine: &fakeEngine{pool: pool}, failFirst: 1}
	svc := New(pool, &fakeEvents{}, eng, noopScheduler{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	if _, err := svc.Create(ctx, CreateRequest{Namespace: ns, SpecName: "wf"}); err == nil {
		t.Fatal("the create succeeded; the injected failure did not happen")
	}
	var dead string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM workflow_runs WHERE namespace = $1`, ns).Scan(&dead); err != nil {
		t.Fatal(err)
	}

	// Fresh: inside the race guard, a create may be mid-materialize.
	found, err := store.WFMListUnmaterializedRuns(ctx, pool, time.Minute, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(found, dead) {
		t.Fatal("a run created seconds ago was swept; that races its own create")
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET created_at = now() - interval '1 hour' WHERE id = $1::uuid`, dead); err != nil {
		t.Fatal(err)
	}
	found, _ = store.WFMListUnmaterializedRuns(ctx, pool, time.Minute, 1000)
	if !containsString(found, dead) {
		t.Fatalf("an hour-old PENDING run with no graph was not listed (run %s)", dead)
	}

	if err := svc.EnsureMaterialized(ctx, dead); err != nil {
		t.Fatalf("EnsureMaterialized: %v", err)
	}
	found, _ = store.WFMListUnmaterializedRuns(ctx, pool, time.Minute, 1000)
	if containsString(found, dead) {
		t.Error("the run is still unmaterialized after EnsureMaterialized")
	}

	// Not PENDING any more (an operator cancelled it): nothing to materialize.
	if _, err := pool.Exec(ctx, `
		INSERT INTO workflow_runs (spec_id, namespace, state, trigger_kind, created_at)
		SELECT id, namespace, 'CANCELLED', 'test', now() - interval '1 hour' FROM workflow_specs WHERE namespace = $1`, ns); err != nil {
		t.Fatal(err)
	}
	found, _ = store.WFMListUnmaterializedRuns(ctx, pool, time.Minute, 1000)
	for _, id := range found {
		var st string
		_ = pool.QueryRow(ctx, `SELECT state::text FROM workflow_runs WHERE id = $1::uuid`, id).Scan(&st)
		if st != "PENDING" {
			t.Errorf("a %s run was listed as unmaterialized", st)
		}
	}
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
