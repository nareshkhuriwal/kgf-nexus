//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// These run against the real schema, because everything this query claims is a
// property of the real tables: that the CTE branches type-check against the
// actual enums, that the event-payload reads find the keys the scheduler writes,
// and above all that the ranking is a TOTAL order. A fake would let all four
// pass while the SQL was wrong.
package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// inAttentionTx runs fn against a transaction that is always rolled back, so
// these tests can INSERT real rows into the live schema without leaving any.
// Named distinctly from artifactgc's inRollbackTx because that one lives in a
// different package; the pattern is deliberately the same.
func inAttentionTx(t *testing.T, pool *pgxpool.Pool, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && err != pgx.ErrTxClosed {
			t.Errorf("rollback: %v", err)
		}
	}()
	fn(tx)
}

// seedRun inserts a spec + run and returns the run id. The chain is required by
// the foreign keys; a test that worked around them with its own table would not
// be testing the real query.
func seedRun(t *testing.T, tx pgx.Tx, ns, specName, state string) string {
	t.Helper()
	ctx := context.Background()
	var specID, runID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1, $2, 1, 'hash-'||gen_random_uuid()::text, '{}'::jsonb, 'test')
		RETURNING id::text`, ns, specName).Scan(&specID); err != nil {
		t.Fatalf("seed spec: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO workflow_runs (spec_id, namespace, state, trigger_kind, started_at)
		VALUES ($1, $2, $3::run_state, 'manual', now() - interval '2 hours')
		RETURNING id::text`, specID, ns, state).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID
}

func seedNode(t *testing.T, tx pgx.Tx, runID, nodeID, state string, used, max int) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO task_nodes (run_id, node_id, task_ref, state, max_attempts, attempts_used, spec_json)
		VALUES ($1, $2, 'pkg.task', $3::node_state, $4, $5, '{}'::jsonb)`,
		runID, nodeID, state, max, used); err != nil {
		t.Fatalf("seed node: %v", err)
	}
}

func seedAttemptRow(t *testing.T, tx pgx.Tx, runID, nodeID, state string, n int, pendingReason *string, scheduledAt time.Time) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(context.Background(), `
		INSERT INTO task_attempts (run_id, node_id, attempt_number, state, resource_request,
		                           idempotency_key, pending_reason, scheduled_at)
		VALUES ($1, $2, $3, $4::attempt_state, '{}'::jsonb, 'idem-'||gen_random_uuid()::text, $5, $6)
		RETURNING id::text`,
		runID, nodeID, n, state, pendingReason, scheduledAt).Scan(&id); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	return id
}

func seedEvent(t *testing.T, tx pgx.Tx, runID, nodeID, attemptID, kind, payload string, at time.Time) {
	t.Helper()
	var seq int
	if err := tx.QueryRow(context.Background(), `
		UPDATE workflow_runs SET event_seq = event_seq + 1 WHERE id = $1 RETURNING event_seq`,
		runID).Scan(&seq); err != nil {
		t.Fatalf("allocate event seq: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO run_events (run_id, seq, node_id, attempt_id, kind, payload, actor, occurred_at)
		VALUES ($1, $2, $3, $4::uuid, $5, $6::jsonb, 'scheduler', $7)`,
		runID, seq, nodeID, attemptID, kind, payload, at); err != nil {
		t.Fatalf("seed event: %v", err)
	}
}

func strptr(s string) *string { return &s }

// THE HEADLINE CASE. A LOST attempt the scheduler REFUSED to retry, because the
// outcome is indeterminate and the task declares no idempotency guarantee.
//
// The sentence is asserted, not just the row, because the sentence is the
// product: a panel that reports the condition but misattributes the cause sends
// an operator to fix the wrong thing.
func TestIntegrationAttentionSurfacesTheIndeterminateRefusal(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	inAttentionTx(t, pool, func(tx pgx.Tx) {
		runID := seedRun(t, tx, ns, "nightly_etl", "RUNNING")
		seedNode(t, tx, runID, "enrich", "RUNNING", 1, 3)
		att := seedAttemptRow(t, tx, runID, "enrich", "LOST", 1, nil, time.Now().Add(-10*time.Minute))
		seedEvent(t, tx, runID, "enrich", att, "IndeterminateOutcome",
			`{"reason":"indeterminate_outcome_requires_human","attempts_used":1,"max_attempts":3}`,
			time.Now().Add(-6*time.Minute))

		items, err := GatewayListAttention(ctx, tx, ns, AttentionFilter{})
		if err != nil {
			t.Fatalf("list attention: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("want exactly 1 item, got %d: %+v", len(items), items)
		}
		it := items[0]
		if it.Kind != AttentionIndeterminate {
			t.Errorf("kind = %q, want %q", it.Kind, AttentionIndeterminate)
		}
		if it.Severity != SeverityCritical {
			t.Errorf("severity = %q; the one case where the platform waits for a human must rank critical", it.Severity)
		}
		if it.Reason != "indeterminate_outcome_requires_human" {
			t.Errorf("reason = %q, want the code the scheduler recorded", it.Reason)
		}
		// The row must be actionable without a second request.
		if it.RunID != runID || it.NodeID != "enrich" || it.AttemptID != att {
			t.Errorf("item does not identify the run/node/attempt it concerns: %+v", it)
		}
		if it.SpecName != "nightly_etl" {
			t.Errorf("spec_name = %q, want nightly_etl", it.SpecName)
		}
		if it.RunState != "RUNNING" {
			t.Errorf("run_state = %q, want RUNNING", it.RunState)
		}
		// The claim about idempotency is made ONLY because the scheduler wrote
		// that exact reason code.
		if !strings.Contains(it.Detail, "no idempotency guarantee") {
			t.Errorf("detail does not explain the refusal: %q", it.Detail)
		}
		if !strings.Contains(it.Summary, "Outcome unknown") || !strings.Contains(it.Summary, "enrich") {
			t.Errorf("summary = %q, want an 'outcome unknown' sentence naming the node", it.Summary)
		}
	})
}

// THE FALSE-POSITIVE THIS DESIGN EXISTS TO AVOID.
//
// A LOST attempt that WAS retried must NOT appear. On the development fleet all
// 33 LOST attempts were idempotency=required and every one was retried; the
// obvious `state = 'LOST'` query would have reported 33 incidents where there
// were none, and an operator who learns the panel cries wolf stops reading it.
func TestIntegrationAttentionIgnoresALostAttemptThatWasRetried(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	inAttentionTx(t, pool, func(tx pgx.Tx) {
		runID := seedRun(t, tx, ns, "retried_wf", "RUNNING")
		seedNode(t, tx, runID, "slow", "RUNNING", 2, 4)
		att := seedAttemptRow(t, tx, runID, "slow", "LOST", 1, nil, time.Now().Add(-30*time.Minute))
		// The scheduler lost it AND retried it: exactly the shape the live fleet
		// is full of.
		seedEvent(t, tx, runID, "slow", att, "AttemptLost",
			`{"reason":"lease_expired"}`, time.Now().Add(-20*time.Minute))
		seedEvent(t, tx, runID, "slow", att, "RetryScheduled",
			`{"reason":"retry_indeterminate_idempotent","next_attempt_number":2}`, time.Now().Add(-20*time.Minute))
		seedAttemptRow(t, tx, runID, "slow", "RUNNING", 2, nil, time.Now().Add(-19*time.Minute))

		items, err := GatewayListAttention(ctx, tx, ns, AttentionFilter{})
		if err != nil {
			t.Fatalf("list attention: %v", err)
		}
		if len(items) != 0 {
			t.Fatalf("a LOST attempt that WAS retried must not be reported; got %d items: %+v", len(items), items)
		}
	})
}

// A blocked attempt reports the scheduler's own shortfall text verbatim, and
// only once it has waited past the threshold.
func TestIntegrationAttentionReportsCapacityBlockAfterTheThreshold(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	inAttentionTx(t, pool, func(tx pgx.Tx) {
		runID := seedRun(t, tx, ns, "gpu_train", "RUNNING")
		seedNode(t, tx, runID, "train", "READY", 0, 3)
		reason := "waiting_for_resources: gpu (need 2, free 1)"
		seedAttemptRow(t, tx, runID, "train", "PENDING", 1, strptr(reason), time.Now().Add(-5*time.Minute))
		// Freshly queued, and with a reason: normal, not an incident.
		seedNode(t, tx, runID, "fresh", "READY", 0, 3)
		seedAttemptRow(t, tx, runID, "fresh", "PENDING", 1, strptr(reason), time.Now().Add(-5*time.Second))
		// Aged, but never placed, so it has no reason: also not an incident.
		seedNode(t, tx, runID, "noreason", "READY", 0, 3)
		seedAttemptRow(t, tx, runID, "noreason", "PENDING", 1, nil, time.Now().Add(-1*time.Hour))

		items, err := GatewayListAttention(ctx, tx, ns, AttentionFilter{BlockedFor: 2 * time.Minute})
		if err != nil {
			t.Fatalf("list attention: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("want only the aged, reasoned attempt; got %d: %+v", len(items), items)
		}
		it := items[0]
		if it.Kind != AttentionBlockedOnCapacity || it.NodeID != "train" {
			t.Fatalf("wrong item: %+v", it)
		}
		// The shortfall text must survive unaltered -- paraphrasing it would
		// restate a comparison we did not perform.
		if it.Reason != reason {
			t.Errorf("reason = %q, want the pending_reason verbatim %q", it.Reason, reason)
		}
		if !strings.Contains(it.Detail, "need 2, free 1") {
			t.Errorf("detail lost the shortfall: %q", it.Detail)
		}
	})
}

// A blocked attempt whose RUN is already dead is not triage.
//
// Attempt rows are never mutated once a run is cancelled or times out (history
// is not rewritten), so their PENDING rows linger forever. On the development
// fleet 8 of the 10 attempts with a pending_reason belonged to CANCELLED or
// TIMED_OUT runs; reporting them would have made this branch permanently
// dominated by work nobody is waiting for.
func TestIntegrationAttentionIgnoresBlockedAttemptsOfDeadRuns(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	inAttentionTx(t, pool, func(tx pgx.Tx) {
		reason := "waiting_for_resources: disk (need 42949672960, free 10737418240)"

		// A live run: reported.
		live := seedRun(t, tx, ns, "live_wf", "RUNNING")
		seedNode(t, tx, live, "ingest", "READY", 0, 3)
		seedAttemptRow(t, tx, live, "ingest", "PENDING", 1, strptr(reason), time.Now().Add(-1*time.Hour))

		// Runs that are over: their leftover PENDING attempts are history.
		for _, state := range []string{"CANCELLED", "TIMED_OUT", "FAILED"} {
			dead := seedRun(t, tx, ns, "dead_"+state, state)
			// ended_at outside the failure window, so the FAILED run does not
			// arrive via the run_failed branch and confuse the count.
			if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET ended_at = now() - interval '40 days' WHERE id = $1`, dead); err != nil {
				t.Fatalf("age dead run: %v", err)
			}
			seedNode(t, tx, dead, "ingest", "READY", 0, 3)
			seedAttemptRow(t, tx, dead, "ingest", "PENDING", 1, strptr(reason), time.Now().Add(-1*time.Hour))
		}

		items, err := GatewayListAttention(ctx, tx, ns, AttentionFilter{})
		if err != nil {
			t.Fatalf("list attention: %v", err)
		}
		var blocked []AttentionItem
		for _, it := range items {
			if it.Kind == AttentionBlockedOnCapacity {
				blocked = append(blocked, it)
			}
		}
		if len(blocked) != 1 {
			t.Fatalf("only the live run's attempt should be reported as blocked; got %d: %+v", len(blocked), blocked)
		}
		if blocked[0].RunID != live {
			t.Errorf("wrong run reported: %+v", blocked[0])
		}
	})
}

// A stalled run is surfaced, and its sentence states the OBSERVATION rather than
// guessing at which upstream bug caused it.
func TestIntegrationAttentionSurfacesAStalledRun(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	inAttentionTx(t, pool, func(tx pgx.Tx) {
		// Matches WFMListStalledRuns' predicate exactly: non-terminal run, every
		// node terminal, nothing in flight, aged past the grace period.
		runID := seedRun(t, tx, ns, "stalled_wf", "PAUSING")
		seedNode(t, tx, runID, "a", "SUCCEEDED", 1, 3)
		seedAttemptRow(t, tx, runID, "a", "SUCCEEDED", 1, nil, time.Now().Add(-3*time.Hour))
		if _, err := tx.Exec(ctx, `UPDATE task_attempts SET ended_at = now() - interval '2 hours' WHERE run_id = $1`, runID); err != nil {
			t.Fatalf("age the attempt: %v", err)
		}

		items, err := GatewayListAttention(ctx, tx, ns, AttentionFilter{})
		if err != nil {
			t.Fatalf("list attention: %v", err)
		}
		var found *AttentionItem
		for i := range items {
			if items[i].Kind == AttentionRunStalled {
				found = &items[i]
			}
		}
		if found == nil {
			t.Fatalf("stalled run not reported; got %+v", items)
		}
		if found.Severity != SeverityCritical {
			t.Errorf("severity = %q, want critical: nothing will look at this run again", found.Severity)
		}
		if found.RunID != runID || found.NodeID != "" {
			t.Errorf("a stalled run is a run-level item: %+v", *found)
		}
		if !strings.Contains(found.Detail, "PAUSING") {
			t.Errorf("detail should name the state the run is stuck in: %q", found.Detail)
		}
	})
}

// Namespace scoping, like every other read: a caller authenticated for one
// tenant must not see another's conditions.
func TestIntegrationAttentionIsNamespaceScoped(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	mine, theirs := uniqueNS(t)+"-a", uniqueNS(t)+"-b"

	inAttentionTx(t, pool, func(tx pgx.Tx) {
		other := seedRun(t, tx, theirs, "secret_wf", "RUNNING")
		seedNode(t, tx, other, "n", "RUNNING", 1, 1)
		att := seedAttemptRow(t, tx, other, "n", "LOST", 1, nil, time.Now().Add(-10*time.Minute))
		seedEvent(t, tx, other, "n", att, "IndeterminateOutcome",
			`{"reason":"indeterminate_outcome_requires_human"}`, time.Now().Add(-5*time.Minute))

		items, err := GatewayListAttention(ctx, tx, mine, AttentionFilter{})
		if err != nil {
			t.Fatalf("list attention: %v", err)
		}
		if len(items) != 0 {
			t.Fatalf("another namespace's conditions leaked: %+v", items)
		}
	})
}

// RANKING. Severity first, then recency -- and the same database must produce
// the IDENTICAL list twice, or an operator cannot diff it.
//
// The repeat-call assertion is the point of this test. Rows sharing a severity
// AND an occurred_at to the microsecond (one worker death expiring several
// attempts in a single sweep) have no inherent order, and without the identity
// tiebreak Postgres may legitimately return them either way round between calls.
func TestIntegrationAttentionRankingIsDeterministic(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	inAttentionTx(t, pool, func(tx pgx.Tx) {
		runID := seedRun(t, tx, ns, "mixed_wf", "RUNNING")

		// A warning, recent.
		seedNode(t, tx, runID, "blocked", "READY", 0, 3)
		seedAttemptRow(t, tx, runID, "blocked", "PENDING", 1,
			strptr("waiting_for_resources: cpu (need 1000, free 0)"), time.Now().Add(-10*time.Minute))

		// A critical, OLDER than the warning. Severity must still win.
		seedNode(t, tx, runID, "unknown", "RUNNING", 1, 3)
		a2 := seedAttemptRow(t, tx, runID, "unknown", "LOST", 1, nil, time.Now().Add(-2*time.Hour))
		seedEvent(t, tx, runID, "unknown", a2, "IndeterminateOutcome",
			`{"reason":"indeterminate_outcome_requires_human"}`, time.Now().Add(-90*time.Minute))

		// Two criticals sharing one timestamp EXACTLY -- the tie the identity
		// tiebreak exists for.
		tie := time.Now().Add(-30 * time.Minute)
		for _, node := range []string{"tie_b", "tie_a"} {
			seedNode(t, tx, runID, node, "RUNNING", 1, 3)
			id := seedAttemptRow(t, tx, runID, node, "LOST", 1, nil, tie)
			seedEvent(t, tx, runID, node, id, "IndeterminateOutcome",
				`{"reason":"indeterminate_outcome_requires_human"}`, tie)
		}

		first, err := GatewayListAttention(ctx, tx, ns, AttentionFilter{})
		if err != nil {
			t.Fatalf("list attention: %v", err)
		}
		if len(first) != 4 {
			t.Fatalf("want 4 items, got %d: %+v", len(first), first)
		}
		// Severity is the primary key: every critical precedes every warning,
		// even though the warning is the most recent thing that happened.
		for i, it := range first {
			if i < 3 && it.Severity != SeverityCritical {
				t.Fatalf("item %d is %q; criticals must sort first: %+v", i, it.Severity, first)
			}
		}
		if first[3].Kind != AttentionBlockedOnCapacity {
			t.Errorf("the warning should sort last, got %+v", first[3])
		}
		// Within critical, newer first.
		if !first[0].OccurredAt.After(first[1].OccurredAt) && !first[0].OccurredAt.Equal(first[1].OccurredAt) {
			t.Errorf("criticals are not ordered newest-first: %v then %v", first[0].OccurredAt, first[1].OccurredAt)
		}

		// THE DETERMINISM ASSERTION: an unchanged database, called again.
		second, err := GatewayListAttention(ctx, tx, ns, AttentionFilter{})
		if err != nil {
			t.Fatalf("second list: %v", err)
		}
		if len(first) != len(second) {
			t.Fatalf("two calls returned different lengths: %d vs %d", len(first), len(second))
		}
		for i := range first {
			if first[i].AttemptID != second[i].AttemptID || first[i].Kind != second[i].Kind {
				t.Fatalf("position %d differs between two calls against an unchanged database:\n  %+v\n  %+v",
					i, first[i], second[i])
			}
		}
	})
}

// BOUNDEDNESS. The limit is enforced in SQL, and the ranking means a truncated
// list keeps the WORST items rather than an arbitrary sample.
func TestIntegrationAttentionIsBounded(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	inAttentionTx(t, pool, func(tx pgx.Tx) {
		runID := seedRun(t, tx, ns, "noisy_wf", "RUNNING")
		for i := 0; i < 12; i++ {
			node := fmt.Sprintf("n%02d", i)
			seedNode(t, tx, runID, node, "READY", 0, 3)
			seedAttemptRow(t, tx, runID, node, "PENDING", 1,
				strptr("waiting_for_resources: cpu (need 1000, free 0)"),
				time.Now().Add(-time.Duration(20+i)*time.Minute))
		}
		// One critical, the OLDEST thing in the set: it must survive truncation
		// that drops much newer warnings.
		seedNode(t, tx, runID, "crit", "RUNNING", 1, 3)
		old := time.Now().Add(-5 * time.Hour)
		id := seedAttemptRow(t, tx, runID, "crit", "LOST", 1, nil, old)
		seedEvent(t, tx, runID, "crit", id, "IndeterminateOutcome",
			`{"reason":"indeterminate_outcome_requires_human"}`, old)

		items, err := GatewayListAttention(ctx, tx, ns, AttentionFilter{Limit: 5})
		if err != nil {
			t.Fatalf("list attention: %v", err)
		}
		if len(items) != 5 {
			t.Fatalf("limit not applied: got %d items", len(items))
		}
		if items[0].Kind != AttentionIndeterminate {
			t.Errorf("truncation dropped the critical item; head is %+v", items[0])
		}
	})
}

// An unrecognised reason code is PRINTED rather than guessed at. A code added to
// the domain package must surface as itself, not as a plausible sentence about
// something else.
func TestIntegrationAttentionDoesNotInventAReasonItDoesNotKnow(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	inAttentionTx(t, pool, func(tx pgx.Tx) {
		runID := seedRun(t, tx, ns, "future_wf", "RUNNING")
		seedNode(t, tx, runID, "n", "FAILED", 2, 2)
		att := seedAttemptRow(t, tx, runID, "n", "FAILED", 2, nil, time.Now().Add(-20*time.Minute))
		seedEvent(t, tx, runID, "n", att, "RetryExhausted",
			`{"reason":"some_future_reason_code","attempts_used":2,"max_attempts":2}`,
			time.Now().Add(-15*time.Minute))

		items, err := GatewayListAttention(ctx, tx, ns, AttentionFilter{})
		if err != nil {
			t.Fatalf("list attention: %v", err)
		}
		// This run legitimately matches TWO conditions at once: the node gave up,
		// AND the run is now non-terminal with every node terminal and nothing in
		// flight, which is exactly WFMListStalledRuns' definition of stalled. Both
		// are true and both are worth telling an operator, so the assertion picks
		// out the one under test rather than demanding the panel report only one
		// thing about a run that has two things wrong with it.
		var exhausted *AttentionItem
		for i := range items {
			if items[i].Kind == AttentionRetryExhausted {
				exhausted = &items[i]
			}
		}
		if exhausted == nil {
			t.Fatalf("retry-exhausted item not reported; got %+v", items)
		}
		if !strings.Contains(exhausted.Detail, "some_future_reason_code") {
			t.Errorf("an unknown reason must be shown verbatim, got: %q", exhausted.Detail)
		}
		// The budget is read from task_nodes, so the sentence can cite it
		// without a second query.
		if !strings.Contains(exhausted.Detail, "2 of 2") {
			t.Errorf("detail should cite the retry budget from task_nodes: %q", exhausted.Detail)
		}
	})
}

// A recently FAILED run is surfaced; one that failed outside the window is not.
func TestIntegrationAttentionBoundsFailedRunsByWindow(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	inAttentionTx(t, pool, func(tx pgx.Tx) {
		recent := seedRun(t, tx, ns, "recent_fail", "FAILED")
		stale := seedRun(t, tx, ns, "stale_fail", "FAILED")
		if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET ended_at = now() - interval '1 hour' WHERE id = $1`, recent); err != nil {
			t.Fatalf("age recent: %v", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET ended_at = now() - interval '40 days' WHERE id = $1`, stale); err != nil {
			t.Fatalf("age stale: %v", err)
		}

		items, err := GatewayListAttention(ctx, tx, ns, AttentionFilter{FailedWithin: 24 * time.Hour})
		if err != nil {
			t.Fatalf("list attention: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("want only the recent failure, got %d: %+v", len(items), items)
		}
		if items[0].RunID != recent || items[0].Kind != AttentionRunFailed {
			t.Errorf("wrong item: %+v", items[0])
		}
		if items[0].Severity != SeverityWarning {
			t.Errorf("a concluded failure is a warning, not a critical: %q", items[0].Severity)
		}
	})
}
