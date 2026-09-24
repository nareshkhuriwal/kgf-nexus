// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
)

// The windowed pull claim is only a fix if idx_attempts_dispatch_rank actually
// serves it. The risk is silent: if the query's rank expression and the index
// expression ever stop matching -- a changed literal, a cast, a reordered
// argument -- Postgres just sorts the backlog again, every test still passes,
// and dispatch quietly goes back to growing with the queue.
//
// So this EXPLAINs the EXACT text the claim runs (claimWindowSQL, not a copy),
// with sequential scans and explicit sorts priced out, and asserts the plan
// reads the index in order. With that toggled off the planner may still pick
// the index for a tiny table; the benchmark (ADR-0018) is what shows it is
// chosen unprompted on a large one.
func TestClaimWindowIsServedByTheRankIndex(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.DSN(t))
	if err != nil {
		testdb.SkipUnreachable(t, err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		testdb.SkipUnreachable(t, err)
	}

	for _, tc := range []struct {
		name        string
		agingRate   float64
		wantOrdered bool // the index supplies the ORDER, so no Sort node
		withCursor  bool
	}{
		{"default rate, first window", 1.0, true, false},
		{"default rate, later window", 1.0, true, true},
		// A non-default rate must NOT take its ORDER from the index: the index
		// is ordered by the rate-1 rank, and using that order would pick the
		// wrong attempt. The planner may still use the index to find the
		// queue's rows, but it has to sort them by the real rank itself.
		{"non-default rate sorts by its own rank", 0.5, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultClaimConfig("q-explain")
			cfg.AgingRate = tc.agingRate
			st := NewSchedulerStore(pool, cfg, "explain-test")

			plan := explainWindow(t, ctx, pool, st.claimWindowSQL(tc.withCursor), tc.withCursor)
			sorts := strings.Contains(plan, "Sort")
			if tc.wantOrdered {
				if !strings.Contains(plan, "idx_attempts_dispatch_rank") {
					t.Fatalf("window does not read idx_attempts_dispatch_rank:\n%s", plan)
				}
				if sorts {
					// Index rows followed by a Sort: the index supplied rows
					// but not the ORDER, so the claim is linear again.
					t.Fatalf("plan reads the index but still sorts; the ORDER BY does not match it:\n%s", plan)
				}
				return
			}
			if !sorts {
				t.Fatalf("a non-default aging rate took its order from the rate-1 index; "+
					"it would claim the wrong attempt:\n%s", plan)
			}
		})
	}
}

func explainWindow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, withCursor bool) string {
	t.Helper()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// SET LOCAL: confined to this rolled-back transaction.
	for _, set := range []string{"SET LOCAL enable_seqscan = off", "SET LOCAL enable_sort = off"} {
		if _, err := tx.Exec(ctx, set); err != nil {
			t.Fatalf("%s: %v", set, err)
		}
	}
	args := []any{"q-explain", uuid.New(), time.Now().UTC(), []uuid.UUID{}, 64}
	if withCursor {
		args = append(args, 12.5, time.Now().UTC(), uuid.New())
	}
	rows, err := tx.Query(ctx, "EXPLAIN "+sql, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return b.String()
}
