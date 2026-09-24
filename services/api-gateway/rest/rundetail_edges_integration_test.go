//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Run detail's `edges` key, against REAL Postgres.
//
// This exists because its absence shipped. getRun returned `nodes` and
// `attempts` and nothing else, so every node in the console arrived with
// indegree zero, the layered layout assigned them all to depth 0, and a ten-node
// diamond rendered as a flat alphabetical column of apparent roots. Nothing
// failed: the console degrades gracefully on a missing field, so the graph simply
// looked wide instead of looking broken.
//
// A handler test built on this package's restDB fake could not have caught it --
// the fake cannot serve a Query -- and there was no test in the package
// mentioning edges at all. So the guard has to be an integration test that
// seeds real rows and asserts the wire shape.
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

// TestIntegrationRunDetailReturnsEdges pins the field the DAG view cannot draw
// without, plus the ordering it relies on for a stable layout.
func TestIntegrationRunDetailReturnsEdges(t *testing.T) {
	ctx := context.Background()
	pool := attentionPool(t)

	ns := fmt.Sprintf("rest-edges-%d", time.Now().UnixNano())
	testdb.RegisterTenant(t, pool, ns)
	const key = "nx_edges_probe_key"

	keyID, err := store.GatewayCreateAPIKey(ctx, pool, ns, "edges-test", "admin", key)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, keyID) })

	var specID, runID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1, 'diamond', 1, 'hash-'||gen_random_uuid()::text, '{}'::jsonb, 'test')
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

	// A diamond, because it is the shape that proves layering: `fan_in` must be
	// drawn to the right of BOTH branches, which a graph with no edges cannot
	// express and a shortest-path layout would get wrong.
	for _, n := range []string{"ingest", "left", "right", "fan_in"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO task_nodes (run_id, node_id, task_ref, state, max_attempts, attempts_used, spec_json)
			VALUES ($1, $2, 'etl.'||$2, 'SUCCEEDED', 3, 1, '{}'::jsonb)`, runID, n); err != nil {
			t.Fatalf("seed node %s: %v", n, err)
		}
	}
	// Inserted deliberately OUT of sorted order, so the ordering assertion below
	// is testing the query's ORDER BY rather than the insertion sequence.
	edges := [][2]string{
		{"right", "fan_in"},
		{"ingest", "left"},
		{"left", "fan_in"},
		{"ingest", "right"},
	}
	for _, e := range edges {
		if _, err := pool.Exec(ctx, `
			INSERT INTO task_edges (run_id, from_node_id, to_node_id, output_to_input)
			VALUES ($1, $2, $3, '{}'::jsonb)`, runID, e[0], e[1]); err != nil {
			t.Fatalf("seed edge %s->%s: %v", e[0], e[1], err)
		}
	}

	api := New(pool, authn.New(pool), &fakeWFM{}, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := api.Routes()

	req := httptest.NewRequest(http.MethodGet, "/v1/namespaces/"+ns+"/runs/"+runID, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Decoded as RawMessage first: `edges` being absent and `edges` being null
	// are different bugs, and unmarshalling straight into a slice would report
	// both as an empty graph.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	raw, present := envelope["edges"]
	if !present {
		t.Fatalf("run detail has no `edges` key; the DAG view cannot draw a graph without it. body = %s", rec.Body.String())
	}
	if string(raw) == "null" {
		t.Fatal("`edges` is null; it must be a list so an edgeless run is distinguishable from an unserialised field")
	}

	var got []struct {
		From string `json:"from_node_id"`
		To   string `json:"to_node_id"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode edges: %v (raw %s)", err, raw)
	}

	// The exact key names the console codes against (ui/src/api/types.ts).
	want := [][2]string{
		{"ingest", "left"},
		{"ingest", "right"},
		{"left", "fan_in"},
		{"right", "fan_in"},
	}
	if len(got) != len(want) {
		t.Fatalf("want %d edges, got %d: %s", len(want), len(got), raw)
	}
	for i, w := range want {
		if got[i].From != w[0] || got[i].To != w[1] {
			t.Errorf("edge %d = %s->%s, want %s->%s (ordering must be stable across polls, or the layout reshuffles on every refresh)",
				i, got[i].From, got[i].To, w[0], w[1])
		}
	}
}

// TestIntegrationRunDetailEdgesEmptyIsList covers the single-node run: the field
// must still be a list, because the console uses `[]` to mean "this run really
// has no dependencies" and would read a missing field the same way.
func TestIntegrationRunDetailEdgesEmptyIsList(t *testing.T) {
	ctx := context.Background()
	pool := attentionPool(t)

	ns := fmt.Sprintf("rest-edges-empty-%d", time.Now().UnixNano())
	testdb.RegisterTenant(t, pool, ns)
	const key = "nx_edges_empty_probe_key"

	keyID, err := store.GatewayCreateAPIKey(ctx, pool, ns, "edges-empty-test", "admin", key)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, keyID) })

	var specID, runID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1, 'solo', 1, 'hash-'||gen_random_uuid()::text, '{}'::jsonb, 'test')
		RETURNING id::text`, ns).Scan(&specID); err != nil {
		t.Fatalf("seed spec: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM workflow_runs  WHERE namespace = $1`, ns)
		_, _ = pool.Exec(ctx, `DELETE FROM workflow_specs WHERE namespace = $1`, ns)
	})
	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_runs (spec_id, namespace, state, trigger_kind, started_at)
		VALUES ($1, $2, 'SUCCEEDED', 'manual', now())
		RETURNING id::text`, specID, ns).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_nodes (run_id, node_id, task_ref, state, max_attempts, attempts_used, spec_json)
		VALUES ($1, 'only', 'etl.only', 'SUCCEEDED', 3, 1, '{}'::jsonb)`, runID); err != nil {
		t.Fatalf("seed node: %v", err)
	}

	api := New(pool, authn.New(pool), &fakeWFM{}, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := api.Routes()

	req := httptest.NewRequest(http.MethodGet, "/v1/namespaces/"+ns+"/runs/"+runID, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, present := envelope["edges"]
	if !present {
		t.Fatal("run detail has no `edges` key even for a single-node run")
	}
	if string(raw) != "[]" {
		t.Errorf("edges = %s, want [] for a run with no dependencies", raw)
	}
}
