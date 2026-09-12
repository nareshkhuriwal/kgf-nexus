//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// TestEventSeqIsAllocatedFromOneCounter pins the fix for a live collision.
//
// run_events has two writers. event-service appends through its owner API, which
// allocates from workflow_runs.event_seq (migration 0002, and the rule CONTRACT.md
// states). The shared appendEvent helper used to allocate `max(seq)+1` under a
// pg_advisory_xact_lock instead, which serialized only the callers that took that
// lock — so the two allocators raced, and observed live:
//
//	duplicate key value violates unique constraint "run_events_run_id_seq_key"
//
// Worse, max(seq)+1 never advanced the counter, so every run the helper wrote left
// event_seq stale and its NEXT owner-API append was guaranteed to collide.
//
// This asserts the two properties that make the table safe: appendEvent advances
// the counter it shares with the owner, and concurrent appends never collide.
func TestEventSeqIsAllocatedFromOneCounter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)

	runID := uuid.MustParse(f.runID)

	// One append through the helper must leave the counter consistent with the
	// rows present, which is exactly what max(seq)+1 failed to do.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := appendEvent(ctx, tx, runID, "n1", nil, "TestEvent", "integration-test", nil); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var counter, maxSeq int32
	if err := pool.QueryRow(ctx, `
		SELECT r.event_seq, COALESCE((SELECT max(seq) FROM run_events e WHERE e.run_id = r.id), 0)
		  FROM workflow_runs r WHERE r.id = $1`, runID).Scan(&counter, &maxSeq); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if counter != maxSeq {
		t.Errorf("event_seq = %d but max(seq) = %d; a stale counter guarantees the "+
			"next owner-API append collides on UNIQUE(run_id, seq)", counter, maxSeq)
	}
}

// TestConcurrentEventAppendsNeverCollide is the race itself, driven concurrently.
//
// Every append is its own transaction, as in production, so the row lock the
// allocating UPDATE takes is the only thing preventing two writers from choosing
// one seq. If the allocator regresses to max(seq)+1, transactions that overlap
// read the same max and one of them fails the unique constraint.
func TestConcurrentEventAppendsNeverCollide(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)
	runID := uuid.MustParse(f.runID)

	const writers = 12
	var wg sync.WaitGroup
	errs := make([]error, writers)
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			tx, err := pool.Begin(ctx)
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if err := appendEvent(ctx, tx, runID, "n", nil, "Concurrent", "integration-test", nil); err != nil {
				errs[i] = err
				return
			}
			errs[i] = tx.Commit(ctx)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d failed: %v", i, err)
		}
	}

	// Every writer must have landed, with a dense 1..N sequence and no gaps: a gap
	// would mean a seq was allocated and its insert lost.
	var rows, distinct, maxSeq int32
	if err := pool.QueryRow(ctx, `
		SELECT count(*), count(DISTINCT seq), COALESCE(max(seq), 0)
		  FROM run_events WHERE run_id = $1`, runID).Scan(&rows, &distinct, &maxSeq); err != nil {
		t.Fatalf("read events: %v", err)
	}
	if rows != writers || distinct != writers || maxSeq != writers {
		t.Errorf("rows=%d distinct_seq=%d max_seq=%d, want %d for all three",
			rows, distinct, maxSeq, writers)
	}
}
