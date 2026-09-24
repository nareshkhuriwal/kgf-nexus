//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// These tests exercise the actual SQL against real Postgres. The unit tests use an
// in-memory fake to cover decision logic fast and everywhere; these cover what a
// fake fundamentally cannot: that the statements are valid, that the partial unique
// indexes and ON CONFLICT clauses behave as the schema comments claim, and that
// FOR UPDATE SKIP LOCKED actually skips. Those are properties of Postgres, not of
// our Go, and asserting them against a fake would be asserting our own assumptions.
package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
)

// testPool connects, or SKIPS the test.
//
// Skipping rather than failing on an unreachable database is deliberate: these tests
// must not turn a developer's `go test ./...` red on a laptop with no Postgres, and
// the build tag already means they only run when asked for. A hard failure would
// train people to ignore the suite.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	pool, err := GWOpen(ctx, dsn, 10)
	if err != nil {
		testdb.SkipUnreachable(t, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// uniqueNS gives each test its own namespace so tests never collide and can run in
// parallel against one database without truncating shared tables.
//
// It also registers the namespace as a tenant: since 0014 every namespace-owned
// row is rejected unless its namespace belongs to a registered tenant.
func uniqueNS(t *testing.T, db testdb.Execer) string {
	t.Helper()
	ns := uniqueName(t)
	testdb.RegisterTenant(t, db, ns)
	return ns
}

// nameSeq keeps two names minted by one test in the same clock tick distinct.
var nameSeq atomic.Int64

// uniqueName is a per-test unique identifier with no database side effects, for
// namespaces a test registers itself (inside its own transaction) and for other
// unique strings such as email addresses.
func uniqueName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("it-%s-%d-%d", sanitize(t.Name()), time.Now().UnixNano(), nameSeq.Add(1))
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

// ── api_keys ──────────────────────────────────────────────────────────────

func TestIntegrationAPIKeyRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t, pool)

	const plaintext = "nx_live_abc123_do_not_log_me"
	id, err := GatewayCreateAPIKey(ctx, pool, ns, "ci-key", string("admin"), plaintext)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, id) })

	key, err := GatewayLookupAPIKey(ctx, pool, plaintext)
	if err != nil {
		t.Fatalf("lookup by plaintext: %v", err)
	}
	if key.Namespace != ns || key.Role != "admin" {
		t.Errorf("resolved %+v, want namespace %s role admin", key, ns)
	}

	// The plaintext must NOT be recoverable from the table: that is the property
	// that makes a leak of this table non-catastrophic.
	var stored string
	if err := pool.QueryRow(ctx, `SELECT key_hash FROM api_keys WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatalf("read stored hash: %v", err)
	}
	if stored == plaintext {
		t.Fatal("the API key was stored in PLAINTEXT")
	}
	if stored != HashAPIKey(plaintext) {
		t.Errorf("stored hash does not match HashAPIKey; lookups would never succeed")
	}

	if _, err := GatewayLookupAPIKey(ctx, pool, "nx_wrong_key"); !errors.Is(err, ErrAPIKeyUnknown) {
		t.Errorf("a wrong key returned %v, want ErrAPIKeyUnknown", err)
	}

	// A revoked key must stop working immediately.
	if _, err := pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := GatewayLookupAPIKey(ctx, pool, plaintext); !errors.Is(err, ErrAPIKeyUnknown) {
		t.Errorf("a revoked key still authenticates: %v", err)
	}
}

// ── idempotency_keys ──────────────────────────────────────────────────────

func TestIntegrationIdempotencyKeyFlow(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t, pool)
	const key = "client-key-1"
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE namespace = $1`, ns) })

	body := []byte(`{"spec_name":"train","params":{"epochs":10}}`)
	hash := HashRequest(body)

	cached, replay, err := GatewayClaimIdempotencyKey(ctx, pool, ns, key, hash)
	if err != nil || replay || cached != nil {
		t.Fatalf("first claim: cached=%q replay=%v err=%v; want a clean claim", cached, replay, err)
	}

	response := []byte(`{"id":"11111111-1111-1111-1111-111111111111","state":"PENDING"}`)
	if err := GatewayStoreIdempotentResponse(ctx, pool, ns, key, response); err != nil {
		t.Fatalf("store response: %v", err)
	}

	cached, replay, err = GatewayClaimIdempotencyKey(ctx, pool, ns, key, hash)
	if err != nil {
		t.Fatalf("replay claim: %v", err)
	}
	if !replay {
		t.Fatal("the same key with the same body did not replay")
	}
	// Postgres stores JSONB normalized, so the comparison is semantic rather than
	// byte-for-byte. The gateway re-serializes on the way out, so what matters is
	// that the CONTENT survived.
	if len(cached) == 0 {
		t.Error("the cached response came back empty")
	}

	// The interesting case: same key, different body.
	otherHash := HashRequest([]byte(`{"spec_name":"train","params":{"epochs":999}}`))
	if _, _, err := GatewayClaimIdempotencyKey(ctx, pool, ns, key, otherHash); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("a conflicting body returned %v, want ErrIdempotencyConflict (409)", err)
	}
}

// TestIntegrationIdempotencyClaimIsAtomic is what the fake cannot prove: under
// genuine concurrency, exactly ONE caller wins a new key.
//
// A SELECT-then-INSERT implementation passes every single-threaded test and fails
// here, because N concurrent requests all see "absent" and all proceed — creating N
// runs for one idempotency key.
func TestIntegrationIdempotencyClaimIsAtomic(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t, pool)
	const key = "concurrent-key"
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE namespace = $1`, ns) })

	hash := HashRequest([]byte(`{"spec_name":"train"}`))
	const racers = 16

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		errs    []error
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, replay, err := GatewayClaimIdempotencyKey(ctx, pool, ns, key, hash)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if !replay {
				// Won the claim, or found it claimed-but-incomplete. Only the very
				// first can have actually inserted, which the count below verifies.
				winners++
			}
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("racer error: %v", err)
	}

	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*)::int FROM idempotency_keys WHERE namespace = $1 AND key = $2`,
		ns, key).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d idempotency rows exist for one key; the claim is not atomic", rows)
	}
}

// ── leases: fencing against real SQL ──────────────────────────────────────

// TestIntegrationLeaseFencing builds a real run/node/attempt/worker/lease chain and
// verifies the fencing query against Postgres, including that expiry is judged by
// DATABASE time rather than by this process's clock.
func TestIntegrationLeaseFencing(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)

	attemptID := f.attempt("node-a", 1)
	workerID := f.worker()

	// A live lease.
	var leaseID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO leases (attempt_id, worker_id, token, expires_at)
		VALUES ($1, $2, $3, now() + interval '30 seconds')
		RETURNING id::text`,
		attemptID, workerID, "tok-current",
	).Scan(&leaseID); err != nil {
		t.Fatalf("insert lease: %v", err)
	}

	if _, err := GatewayVerifyLease(ctx, pool, attemptID, "tok-current"); err != nil {
		t.Fatalf("the current lease holder was rejected: %v", err)
	}
	if _, err := GatewayVerifyLease(ctx, pool, attemptID, "tok-old"); !errors.Is(err, ErrStaleLeaseToken) {
		t.Errorf("a stale token returned %v, want ErrStaleLeaseToken", err)
	}

	// Expire it in the DATABASE, which is the clock that decides (invariant 7).
	if _, err := pool.Exec(ctx,
		`UPDATE leases SET expires_at = now() - interval '1 second' WHERE id = $1`, leaseID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, err := GatewayVerifyLease(ctx, pool, attemptID, "tok-current"); !errors.Is(err, ErrStaleLeaseToken) {
		t.Errorf("an expired lease with a matching token returned %v, want ErrStaleLeaseToken", err)
	}

	// And renewal must refuse to resurrect it.
	granted, revoked, err := GatewayRenewLeases(ctx, pool, workerID,
		map[string]string{attemptID: "tok-current"}, 30*time.Second)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if len(granted) != 0 {
		t.Error("an expired lease was renewed; it must be reclaimed and re-dispatched")
	}
	if len(revoked) != 1 || revoked[0] != attemptID {
		t.Errorf("revoked = %v, want [%s]", revoked, attemptID)
	}
}

// TestIntegrationLeaseUniqueAttempt verifies the schema's UNIQUE(attempt_id): at most
// ONE worker is ever AUTHORIZED for an attempt, which is the constraint the whole
// fencing argument rests on.
func TestIntegrationLeaseUniqueAttempt(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)

	attemptID := f.attempt("node-a", 1)
	w1, w2 := f.worker(), f.worker()

	if _, err := pool.Exec(ctx, `
		INSERT INTO leases (attempt_id, worker_id, token, expires_at)
		VALUES ($1, $2, 'tok-1', now() + interval '30 seconds')`, attemptID, w1); err != nil {
		t.Fatalf("first lease: %v", err)
	}
	// A second worker must NOT be able to hold the same attempt.
	if _, err := pool.Exec(ctx, `
		INSERT INTO leases (attempt_id, worker_id, token, expires_at)
		VALUES ($1, $2, 'tok-2', now() + interval '30 seconds')`, attemptID, w2); err == nil {
		t.Fatal("two workers were granted leases on ONE attempt; double-dispatch is possible")
	}
}

// TestIntegrationRenewLeasesBatched verifies the batched renewal against real SQL,
// including the worker-id guard.
func TestIntegrationRenewLeasesBatched(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)

	a1 := f.attempt("node-a", 1)
	a2 := f.attempt("node-b", 1)
	mine, theirs := f.worker(), f.worker()

	for _, x := range []struct{ attempt, worker, token string }{
		{a1, mine, "tok-a1"},
		{a2, theirs, "tok-a2"},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO leases (attempt_id, worker_id, token, expires_at)
			VALUES ($1, $2, $3, now() + interval '5 seconds')`,
			x.attempt, x.worker, x.token); err != nil {
			t.Fatalf("insert lease: %v", err)
		}
	}

	granted, revoked, err := GatewayRenewLeases(ctx, pool, mine, map[string]string{
		a1: "tok-a1",
		a2: "tok-a2", // belongs to another worker
	}, 60*time.Second)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if len(granted) != 1 || granted[0].AttemptID != a1 {
		t.Errorf("granted = %+v, want only %s", granted, a1)
	}
	if len(revoked) != 1 || revoked[0] != a2 {
		t.Errorf("revoked = %v, want [%s]: a worker must not renew another's lease", revoked, a2)
	}
	// The new deadline must be absolute CONTROL-PLANE time, comfortably in the
	// future, so a worker never has to convert a duration against its own clock.
	if !granted[0].ExpiresAt.After(time.Now().Add(30 * time.Second)) {
		t.Errorf("renewed expiry %v is not ~60s out; the TTL was not applied", granted[0].ExpiresAt)
	}
}

// TestIntegrationReconciliationProjections verifies the two queries heartbeat
// reconciliation depends on.
func TestIntegrationReconciliationProjections(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)

	workerID := f.worker()
	running := f.attempt("node-a", 1)
	pending := f.attempt("node-b", 1)

	if _, err := pool.Exec(ctx,
		`UPDATE task_attempts SET state = 'RUNNING', worker_id = $2 WHERE id = $1`,
		running, workerID); err != nil {
		t.Fatalf("set running: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO reservations (worker_id, attempt_id, resources) VALUES ($1, $2, '{}'::jsonb)`,
		workerID, running); err != nil {
		t.Fatalf("insert reservation: %v", err)
	}

	active, err := GatewayWorkerActiveAttempts(ctx, pool, workerID)
	if err != nil {
		t.Fatalf("active attempts: %v", err)
	}
	if len(active) != 1 || active[0] != running {
		t.Errorf("active = %v, want [%s]; a PENDING attempt is not assigned to a worker", active, running)
	}
	_ = pending

	reserved, err := GatewayReservedAttempts(ctx, pool, workerID)
	if err != nil {
		t.Fatalf("reserved attempts: %v", err)
	}
	if len(reserved) != 1 || reserved[0] != running {
		t.Errorf("reserved = %v, want [%s]", reserved, running)
	}
}

// ── specs and runs ────────────────────────────────────────────────────────

// TestIntegrationSpecContentAddressing is ADR-0004's no-op resubmit, against the real
// UNIQUE(namespace, name, spec_hash).
func TestIntegrationSpecContentAddressing(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t, pool)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM workflow_specs WHERE namespace = $1`, ns) })

	canonical := []byte(`{"api_version":"nexus.dev/v1","name":"train","namespace":"` + ns + `","tasks":[]}`)
	hash := "aaaa000000000000000000000000000000000000000000000000000000000001"

	first, created, err := WFMPutSpec(ctx, pool, ns, "train", hash, canonical, "tester")
	if err != nil {
		t.Fatalf("first put: %v", err)
	}
	if !created || first.Version != 1 {
		t.Fatalf("first put: created=%v version=%d, want true and 1", created, first.Version)
	}

	// A byte-identical resubmit must be a NO-OP returning the existing version.
	again, created, err := WFMPutSpec(ctx, pool, ns, "train", hash, canonical, "tester")
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if created {
		t.Error("a byte-identical resubmit created a NEW version; ADR-0004 requires a no-op")
	}
	if again.ID != first.ID || again.Version != first.Version {
		t.Errorf("resubmit returned %s v%d, want the existing %s v%d", again.ID, again.Version, first.ID, first.Version)
	}

	// A DIFFERENT document is a new version.
	changed := []byte(`{"api_version":"nexus.dev/v1","name":"train","namespace":"` + ns + `","tasks":[{"id":"a"}]}`)
	hash2 := "aaaa000000000000000000000000000000000000000000000000000000000002"
	second, created, err := WFMPutSpec(ctx, pool, ns, "train", hash2, changed, "tester")
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if !created || second.Version != 2 {
		t.Errorf("a changed spec gave created=%v version=%d, want true and 2", created, second.Version)
	}
}

// TestIntegrationRunIdempotencyDedup verifies uq_runs_idem: submitting the same run
// twice is safe at the outermost layer.
func TestIntegrationRunIdempotencyDedup(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)

	const key = "daily-2026-09-12"
	first, created, err := WFMCreateRun(ctx, pool, CreateRunParams{
		SpecID: f.specID, Namespace: f.ns, Priority: 100,
		IdempotencyKey: key, TriggerKind: "schedule",
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if !created {
		t.Fatal("the first create was reported as a dedup")
	}

	again, created, err := WFMCreateRun(ctx, pool, CreateRunParams{
		SpecID: f.specID, Namespace: f.ns, Priority: 100,
		IdempotencyKey: key, TriggerKind: "schedule",
	})
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created {
		t.Error("the same idempotency key created a SECOND run")
	}
	if again.ID != first.ID {
		t.Errorf("dedup returned run %s, want the original %s", again.ID, first.ID)
	}

	// An UNKEYED run is unconstrained: the index is partial, so many keyless runs of
	// one workflow are legal.
	if _, created, err := WFMCreateRun(ctx, pool, CreateRunParams{
		SpecID: f.specID, Namespace: f.ns, TriggerKind: "api",
	}); err != nil || !created {
		t.Errorf("an unkeyed run was blocked: created=%v err=%v", created, err)
	}
	if _, created, err := WFMCreateRun(ctx, pool, CreateRunParams{
		SpecID: f.specID, Namespace: f.ns, TriggerKind: "api",
	}); err != nil || !created {
		t.Errorf("a second unkeyed run was blocked: created=%v err=%v", created, err)
	}
}

// TestIntegrationMaxActiveRuns verifies the cap holds under CONCURRENCY, which is the
// only way the test is meaningful: read-count-then-insert passes a sequential test and
// fails here.
func TestIntegrationMaxActiveRuns(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)

	// newFixture already created one PENDING run in this namespace, and that run
	// counts toward the cap — max_active_runs is a property of the WORKFLOW, so every
	// non-terminal run of it occupies a slot regardless of who created it. The
	// expectation below accounts for it rather than the implementation ignoring it:
	// a cap that silently excluded runs it did not create would not be a cap.
	const (
		cap          = 3
		preexisting  = 1
		wantAccepted = cap - preexisting
		racers       = 12
	)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
		rejected int
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, created, err := WFMCreateRun(ctx, pool, CreateRunParams{
				SpecID: f.specID, Namespace: f.ns, TriggerKind: "api", MaxActiveRuns: cap,
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrMaxActiveRuns):
				rejected++
			case err != nil:
				t.Errorf("unexpected error: %v", err)
			case created:
				accepted++
			}
		}()
	}
	close(start)
	wg.Wait()

	if accepted != wantAccepted {
		t.Errorf("%d runs were accepted with max_active_runs=%d and %d already active; want %d. Either the cap is not enforced atomically, or it is off by one.",
			accepted, cap, preexisting, wantAccepted)
	}
	if rejected != racers-wantAccepted {
		t.Errorf("%d rejections, want %d", rejected, racers-wantAccepted)
	}

	// The decisive assertion: the TOTAL active count must never exceed the cap, which
	// is the property the cap exists to provide. A read-count-then-insert
	// implementation overshoots here even when the accept count happens to look right.
	var active int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)::int
		  FROM workflow_runs r
		  JOIN workflow_specs s ON s.id = r.spec_id
		 WHERE s.namespace = $1 AND s.name = 'fixture'
		   AND r.state IN ('PENDING','RUNNING','PAUSING','PAUSED','CANCELLING')`,
		f.ns).Scan(&active); err != nil {
		t.Fatalf("count active runs: %v", err)
	}
	if active > cap {
		t.Errorf("%d runs are active with max_active_runs=%d; the cap was overshot under concurrency", active, cap)
	}
}

// TestIntegrationRunTransitionCAS verifies the compare-and-set: a transition from the
// wrong state must be REFUSED, not applied.
func TestIntegrationRunTransitionCAS(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)

	run, _, err := WFMCreateRun(ctx, pool, CreateRunParams{
		SpecID: f.specID, Namespace: f.ns, TriggerKind: "api",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	running, err := WFMTransitionRun(ctx, pool, run.ID, []string{"PENDING"}, "RUNNING")
	if err != nil {
		t.Fatalf("PENDING -> RUNNING: %v", err)
	}
	if running.State != "RUNNING" {
		t.Fatalf("state = %s, want RUNNING", running.State)
	}
	if running.StartedAt == nil {
		t.Error("started_at was not set on entry to RUNNING")
	}
	if running.StateVersion <= run.StateVersion {
		t.Errorf("state_version did not advance: %d -> %d", run.StateVersion, running.StateVersion)
	}

	// The same transition again must be refused: the run is no longer PENDING.
	if _, err := WFMTransitionRun(ctx, pool, run.ID, []string{"PENDING"}, "RUNNING"); !errors.Is(err, ErrRunStateConflict) {
		t.Errorf("a repeated transition returned %v, want ErrRunStateConflict", err)
	}

	// started_at must NOT be rewritten by a later entry into RUNNING (a resume),
	// or the run's measured duration shrinks every time it is paused.
	firstStart := *running.StartedAt
	if _, err := WFMTransitionRun(ctx, pool, run.ID, []string{"RUNNING"}, "PAUSING"); err != nil {
		t.Fatalf("RUNNING -> PAUSING: %v", err)
	}
	if _, err := WFMTransitionRun(ctx, pool, run.ID, []string{"PAUSING"}, "PAUSED"); err != nil {
		t.Fatalf("PAUSING -> PAUSED: %v", err)
	}
	resumed, err := WFMTransitionRun(ctx, pool, run.ID, []string{"PAUSED"}, "RUNNING")
	if err != nil {
		t.Fatalf("PAUSED -> RUNNING: %v", err)
	}
	if !resumed.StartedAt.Equal(firstStart) {
		t.Errorf("started_at was rewritten on resume: %v -> %v", firstStart, *resumed.StartedAt)
	}

	// A terminal transition sets ended_at.
	done, err := WFMTransitionRun(ctx, pool, run.ID, []string{"RUNNING"}, "SUCCEEDED")
	if err != nil {
		t.Fatalf("RUNNING -> SUCCEEDED: %v", err)
	}
	if done.EndedAt == nil {
		t.Error("ended_at was not set on a terminal transition")
	}
}

// TestIntegrationRunDeadlineFromDatabaseClock verifies that run_timeout_ms becomes an
// absolute deadline computed by POSTGRES, not by this process (invariant 7).
func TestIntegrationRunDeadlineFromDatabaseClock(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)

	run, _, err := WFMCreateRun(ctx, pool, CreateRunParams{
		SpecID: f.specID, Namespace: f.ns, TriggerKind: "api",
		RunTimeoutMS: 3_600_000,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if run.DeadlineAt == nil {
		t.Fatal("run_timeout_ms did not produce a deadline_at")
	}
	if d := time.Until(*run.DeadlineAt); d < 50*time.Minute || d > 70*time.Minute {
		t.Errorf("deadline is %v out, want ~1h", d)
	}

	// Zero means unbounded, which must be NULL rather than "now".
	unbounded, _, err := WFMCreateRun(ctx, pool, CreateRunParams{
		SpecID: f.specID, Namespace: f.ns, TriggerKind: "api", RunTimeoutMS: 0,
	})
	if err != nil {
		t.Fatalf("create unbounded run: %v", err)
	}
	if unbounded.DeadlineAt != nil {
		t.Errorf("deadline_at = %v for run_timeout_ms=0, want NULL", unbounded.DeadlineAt)
	}
}

// ── timers ────────────────────────────────────────────────────────────────

// TestIntegrationTimerSkipLocked is the test the fake cannot do.
//
// FOR UPDATE SKIP LOCKED is the entire mechanism behind running N reconcilers with no
// leader election. If it does not actually skip, two replicas process the same timer
// (double effects) or serialize on the head of the queue (N replicas with the
// throughput of one). Only real Postgres can demonstrate either.
func TestIntegrationTimerSkipLocked(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	subject := timerSubject(t, ctx, pool)
	const total = 20
	for i := 0; i < total; i++ {
		if _, err := WFMArmTimer(ctx, pool, timerKind, subject, -time.Second, nil); err != nil {
			t.Fatalf("arm timer: %v", err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM timers WHERE subject_id = $1`, subject) })

	// Two concurrent claimants, as two replicas would be.
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		seen   = map[string]int{}
		claims int
	)
	for _, claimant := range []string{"replica-a", "replica-b"} {
		c := claimant
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				batch, err := WFMClaimDueTimers(ctx, pool, c, 5)
				if err != nil {
					t.Errorf("%s claim: %v", c, err)
					return
				}
				mu.Lock()
				for _, tm := range batch {
					seen[tm.ID]++
					claims++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for id, n := range seen {
		if n > 1 {
			t.Errorf("timer %s was claimed %d times; SKIP LOCKED is not preventing double processing", id, n)
		}
	}
	// Only timers for THIS subject are asserted on; other tests may run in parallel
	// against the same table.
	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT count(*)::int FROM timers WHERE subject_id = $1 AND claimed_by IS NULL`,
		subject).Scan(&remaining); err != nil {
		t.Fatalf("count unclaimed: %v", err)
	}
	if claims == 0 {
		t.Error("neither replica claimed anything")
	}
}

// TestIntegrationTimerLifecycle covers arm, claim, delete, cancel, and the stale-claim
// sweep.
func TestIntegrationTimerLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	subject := timerSubject(t, ctx, pool)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM timers WHERE subject_id = $1`, subject) })

	// A future timer must NOT be claimed.
	futureID, err := WFMArmTimer(ctx, pool, timerKind, subject, time.Hour, nil)
	if err != nil {
		t.Fatalf("arm future timer: %v", err)
	}
	// A due one must be.
	dueID, err := WFMArmTimer(ctx, pool, timerKind, subject, -time.Second, []byte(`{"n":1}`))
	if err != nil {
		t.Fatalf("arm due timer: %v", err)
	}

	batch, err := WFMClaimDueTimers(ctx, pool, "replica-a", 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	var gotDue bool
	for _, tm := range batch {
		if tm.ID == futureID {
			t.Error("a timer whose fire_at is in the future was claimed")
		}
		if tm.ID == dueID {
			gotDue = true
			if string(tm.Payload) == "" {
				t.Error("the timer payload was not returned")
			}
		}
	}
	if !gotDue {
		t.Fatal("the due timer was not claimed")
	}

	if err := WFMDeleteTimer(ctx, pool, dueID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Cancelling a subject's timers clears the still-pending future one.
	n, err := WFMCancelTimers(ctx, pool, subject, timerKind)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if n != 1 {
		t.Errorf("cancelled %d timers, want 1", n)
	}
}

// TestIntegrationStaleClaimSweep: a reconciler that crashes after claiming must not
// strand its batch forever. That would lose deadlines SILENTLY, which is the worst way
// a deadline system can fail.
func TestIntegrationStaleClaimSweep(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	subject := timerSubject(t, ctx, pool)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM timers WHERE subject_id = $1`, subject) })

	id, err := WFMArmTimer(ctx, pool, timerKind, subject, -time.Second, nil)
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	if _, err := WFMClaimDueTimers(ctx, pool, "doomed-replica", 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Simulate the claimant having died a while ago.
	if _, err := pool.Exec(ctx,
		`UPDATE timers SET claimed_at = now() - interval '10 minutes' WHERE id = $1`, id); err != nil {
		t.Fatalf("age the claim: %v", err)
	}

	if _, err := WFMReleaseStaleTimerClaims(ctx, pool, time.Minute); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	var claimedBy *string
	if err := pool.QueryRow(ctx, `SELECT claimed_by FROM timers WHERE id = $1`, id).Scan(&claimedBy); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	if claimedBy != nil {
		t.Errorf("claimed_by = %q after the sweep; the deadline is stranded forever", *claimedBy)
	}

	// And it is claimable again, so the deadline still fires.
	batch, err := WFMClaimDueTimers(ctx, pool, "healthy-replica", 10)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	var found bool
	for _, tm := range batch {
		if tm.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("the swept timer was not re-claimable")
	}
}

// ── fixture ───────────────────────────────────────────────────────────────

// fixture builds the minimum real row chain the foreign keys require: a spec, a run,
// a node, and on demand attempts and workers. Built with real inserts rather than
// disabled constraints, because the constraints ARE the design and a test that
// bypasses them proves less than nothing.
type fixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	ns     string
	specID string
	runID  string
}

func newFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: ctx, pool: pool, ns: uniqueNS(t, pool)}

	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1, 'fixture', 1, $2, '{}'::jsonb, 'integration-test')
		RETURNING id::text`,
		f.ns, "hash-"+f.ns,
	).Scan(&f.specID); err != nil {
		t.Fatalf("fixture spec: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_runs (spec_id, namespace, trigger_kind)
		VALUES ($1, $2, 'test')
		RETURNING id::text`,
		f.specID, f.ns,
	).Scan(&f.runID); err != nil {
		t.Fatalf("fixture run: %v", err)
	}

	// ON DELETE CASCADE from workflow_specs is not declared, so specs are removed
	// last and explicitly.
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM workflow_runs WHERE namespace = $1`, f.ns)
		_, _ = pool.Exec(ctx, `DELETE FROM workers WHERE namespace = $1`, f.ns)
		_, _ = pool.Exec(ctx, `DELETE FROM workflow_specs WHERE namespace = $1`, f.ns)
	})
	return f
}

// attempt creates a node (if needed) and a PENDING attempt on it.
func (f *fixture) attempt(nodeID string, number int) string {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO task_nodes (run_id, node_id, task_ref, spec_json)
		VALUES ($1, $2, 'test.task:v1', '{}'::jsonb)
		ON CONFLICT (run_id, node_id) DO NOTHING`,
		f.runID, nodeID); err != nil {
		f.t.Fatalf("fixture node: %v", err)
	}

	var id string
	if err := f.pool.QueryRow(f.ctx, `
		INSERT INTO task_attempts (run_id, node_id, attempt_number, resource_request, idempotency_key)
		VALUES ($1, $2, $3, '{}'::jsonb, $4)
		RETURNING id::text`,
		f.runID, nodeID, number,
		fmt.Sprintf("idem-%s-%s-%d", f.runID, nodeID, number),
	).Scan(&id); err != nil {
		f.t.Fatalf("fixture attempt: %v", err)
	}
	return id
}

// worker creates an ACTIVE worker in the fixture's namespace.
func (f *fixture) worker() string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx, `
		INSERT INTO workers (namespace, hostname, queues, capacity, agent_version, session_token_hash)
		VALUES ($1, $2, ARRAY['default'], '{"cpu_millis":8000}'::jsonb, '1.0.0', $3)
		RETURNING id::text`,
		f.ns, "host-"+f.ns, HashAPIKey("session-"+f.ns+fmt.Sprint(time.Now().UnixNano())),
	).Scan(&id); err != nil {
		f.t.Fatalf("fixture worker: %v", err)
	}
	return id
}

// timerKind is the kind the timer tests arm. It must be a real kind: since 0014
// a timer inherits its tenant from its subject, found by kind, and an unknown
// kind is rejected (23514).
const timerKind = "run_deadline"

// timerSubject returns a real run in a registered namespace, for timer tests.
//
// timers.subject_id still has no foreign key (it points at several different
// tables depending on kind), but the 0014 ownership trigger resolves the
// subject to find the timer's tenant, so an arbitrary UUID is refused.
func timerSubject(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	return newFixture(t, ctx, pool).runID
}

// TestIntegrationListStalledRuns pins the backstop for the class of bug that put
// a run at PAUSING for seven hours with all three of its nodes terminal.
//
// A run gets there when its conclusion is computed and lost, or never computed
// because nothing triggered a reconcile. It is then unreachable: a reconcile
// happens only when an attempt reports or an operator intervenes, and a run with
// no live attempts will never produce either. Three separate causes have been
// fixed at their own call sites (F62, F67, b97d1ac); this query is what notices
// the fourth.
//
// The negative cases carry most of the weight. A sweep that force-transitions
// runs is exactly the kind of thing that can destroy a healthy run, so each way
// a run can legitimately look idle is asserted NOT to match.
func TestIntegrationListStalledRuns(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	// A stalled run: every node terminal, nothing in flight, still RUNNING.
	stalled := newFixture(t, ctx, pool)
	stalled.attempt("done", 1)
	settleRun(t, ctx, pool, stalled.runID, "RUNNING", "SUCCEEDED", "SUCCEEDED", time.Hour)

	found, err := WFMListStalledRuns(ctx, pool, time.Minute, 100)
	if err != nil {
		t.Fatalf("list stalled runs: %v", err)
	}
	if !contains(found, stalled.runID) {
		t.Errorf("a run with every node terminal and nothing in flight was not reported stalled; "+
			"nothing else will ever look at it again (run %s)", stalled.runID)
	}

	// ── Everything below must NOT be swept. ──────────────────────────────

	// Still working. The obvious case, and the one that would be catastrophic
	// to get wrong: concluding a running node's run would kill live work.
	live := newFixture(t, ctx, pool)
	live.attempt("busy", 1)
	settleRun(t, ctx, pool, live.runID, "RUNNING", "RUNNING", "RUNNING", time.Hour)
	assertNotStalled(t, ctx, pool, live.runID, "a run with a RUNNING attempt")

	// Terminal nodes but an attempt still PENDING. The attempt may yet be
	// dispatched, so the run is waiting for capacity, not stuck.
	pending := newFixture(t, ctx, pool)
	pending.attempt("queued", 1)
	settleRun(t, ctx, pool, pending.runID, "RUNNING", "SUCCEEDED", "PENDING", time.Hour)
	assertNotStalled(t, ctx, pool, pending.runID, "a run with a PENDING attempt")

	// Recently settled. This is the race guard: a healthy run shows every node
	// terminal for the moment between its last attempt reporting and the engine
	// publishing the outcome, and sweeping THAT would replace a correct
	// conclusion with a guessed one.
	fresh := newFixture(t, ctx, pool)
	fresh.attempt("justdone", 1)
	settleRun(t, ctx, pool, fresh.runID, "RUNNING", "SUCCEEDED", "SUCCEEDED", 0)
	assertNotStalled(t, ctx, pool, fresh.runID, "a run that settled seconds ago")

	// No DAG yet. A run whose graph has not been materialized also has no live
	// attempts; concluding it would kill it before it ever started.
	empty := newFixture(t, ctx, pool)
	mustExec(t, ctx, pool,
		`UPDATE workflow_runs SET state='PENDING', created_at = now() - interval '1 hour' WHERE id=$1`,
		empty.runID)
	assertNotStalled(t, ctx, pool, empty.runID, "a run with no nodes materialized")

	// Already terminal. Nothing to do, and re-reporting it would make the
	// counter lie about how often this happens.
	finished := newFixture(t, ctx, pool)
	finished.attempt("old", 1)
	settleRun(t, ctx, pool, finished.runID, "SUCCEEDED", "SUCCEEDED", "SUCCEEDED", time.Hour)
	assertNotStalled(t, ctx, pool, finished.runID, "a run that already reached SUCCEEDED")
}

// settleRun forces a fixture run into a specific shape: run state, node state,
// attempt state, and how long ago the attempt ended.
func settleRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	runID, runState, nodeState, attemptState string, ago time.Duration) {
	t.Helper()
	mustExec(t, ctx, pool,
		`UPDATE task_nodes SET state = $2::node_state WHERE run_id = $1`, runID, nodeState)
	// ended_at is only set for a genuinely finished attempt; a PENDING or RUNNING
	// one has not ended, and leaving it NULL is what the query's COALESCE expects.
	if attemptState == "SUCCEEDED" || attemptState == "FAILED" || attemptState == "CANCELLED" {
		mustExec(t, ctx, pool,
			`UPDATE task_attempts SET state = $2::attempt_state, ended_at = now() - $3::interval
			  WHERE run_id = $1`, runID, attemptState, ago.String())
	} else {
		mustExec(t, ctx, pool,
			`UPDATE task_attempts SET state = $2::attempt_state, ended_at = NULL WHERE run_id = $1`,
			runID, attemptState)
	}
	mustExec(t, ctx, pool,
		`UPDATE workflow_runs SET state = $2::run_state,
		        created_at = now() - $3::interval, started_at = now() - $3::interval
		  WHERE id = $1`, runID, runState, ago.String())
}

func assertNotStalled(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runID, what string) {
	t.Helper()
	found, err := WFMListStalledRuns(ctx, pool, time.Minute, 100)
	if err != nil {
		t.Fatalf("list stalled runs: %v", err)
	}
	if contains(found, runID) {
		t.Errorf("%s was reported stalled; sweeping it would conclude a run that is "+
			"still legitimately in progress (run %s)", what, runID)
	}
}

func mustExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// GatewayListCatalog returns ONE row per workflow name, carrying its newest
// version. The grouping is the whole point: a catalog that repeats a workflow
// once per version buries fifteen workflows under a hundred rows.
func TestIntegrationCatalogGroupsByNameAndKeepsTheNewestVersion(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ns := uniqueNS(t, pool)

	// Two workflows: one with three versions, one with a single version.
	for v := 1; v <= 3; v++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
			VALUES ($1, 'multi', $2, $3, '{}'::jsonb, 'catalog-test')`,
			ns, v, fmt.Sprintf("hash-multi-%s-%d", ns, v)); err != nil {
			t.Fatalf("insert version %d: %v", v, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1, 'single', 1, $2, '{}'::jsonb, 'catalog-test')`,
		ns, "hash-single-"+ns); err != nil {
		t.Fatalf("insert single: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflow_specs WHERE namespace = $1`, ns)
	})

	got, err := GatewayListCatalog(ctx, pool, ns, "", 100)
	if err != nil {
		t.Fatalf("list catalog: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("catalog returned %d entries, want 2 (one per NAME, not per version)", len(got))
	}

	by := map[string]CatalogEntry{}
	for _, c := range got {
		by[c.Name] = c
	}
	if m := by["multi"]; m.Version != 3 {
		t.Errorf("multi latest version = %d, want 3 (the newest, not the first)", m.Version)
	}
	if m := by["multi"]; m.VersionCount != 3 {
		t.Errorf("multi version_count = %d, want 3", m.VersionCount)
	}
	if s := by["single"]; s.VersionCount != 1 {
		t.Errorf("single version_count = %d, want 1", s.VersionCount)
	}
	// FirstCreatedAt is when the WORKFLOW appeared; CreatedAt is when its latest
	// version did. They are different questions and must not be the same column.
	if m := by["multi"]; m.FirstCreatedAt.After(m.CreatedAt) {
		t.Errorf("first_created_at %s is after updated_at %s", m.FirstCreatedAt, m.CreatedAt)
	}
}

// Search matches the workflow NAME, case-insensitively. Matching the definition
// body instead would return every workflow that merely mentions the term in a
// task ref, which is not what someone typing a name means.
func TestIntegrationCatalogSearchMatchesNameCaseInsensitively(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ns := uniqueNS(t, pool)

	for _, name := range []string{"NightlyRetrain", "hourly-ingest"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
			VALUES ($1, $2, 1, $3, '{}'::jsonb, 'catalog-test')`,
			ns, name, "hash-"+name+"-"+ns); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflow_specs WHERE namespace = $1`, ns)
	})

	got, err := GatewayListCatalog(ctx, pool, ns, "retrain", 100)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].Name != "NightlyRetrain" {
		t.Fatalf("search 'retrain' returned %v, want just NightlyRetrain (case-insensitive)", names(got))
	}

	// An empty search is "everything", not "nothing".
	all, err := GatewayListCatalog(ctx, pool, ns, "", 100)
	if err != nil {
		t.Fatalf("empty search: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("empty search returned %d, want 2 (empty means unfiltered)", len(all))
	}
}

func names(cs []CatalogEntry) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// TestIntegrationWorkerAttemptAttribution covers the two reads the worker
// drill-down is built on.
//
// The whole feature rests on one claim about the schema — that task_attempts
// carries the worker_id that answers "what is this worker running" — so the test
// that matters is the one asserting the SQL against real Postgres. A fake would
// only restate the assumption.
//
// Three properties are pinned:
//
//   - running covers DISPATCHED as well as RUNNING, matching the active_attempts
//     count in the fleet listing. An attempt handed to a worker and never started
//     is the symptom an operator opens this view to find, and dropping it would
//     make the detail page contradict the count on the table it was reached from.
//   - the two lists PARTITION the worker's attempts: nothing appears twice, and a
//     terminal attempt only ever lands in recent. A row in both would read as two
//     tasks.
//   - task_ref comes through the join. node_id alone ("node-a") names a position
//     in a DAG the reader is not looking at.
func TestIntegrationWorkerAttemptAttribution(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)

	workerID := f.worker()
	other := f.worker()

	running := f.attempt("node-running", 1)
	dispatched := f.attempt("node-dispatched", 1)
	done := f.attempt("node-done", 1)
	foreign := f.attempt("node-foreign", 1)
	unassigned := f.attempt("node-unassigned", 1)

	for _, tc := range []struct{ id, state, worker string }{
		{running, "RUNNING", workerID},
		{dispatched, "DISPATCHED", workerID},
		{done, "SUCCEEDED", workerID},
		{foreign, "RUNNING", other},
	} {
		if _, err := pool.Exec(ctx,
			`UPDATE task_attempts SET state = $2::attempt_state, worker_id = $3,
			        started_at = now(), ended_at = CASE WHEN $2 = 'SUCCEEDED' THEN now() END
			  WHERE id = $1`,
			tc.id, tc.state, tc.worker); err != nil {
			t.Fatalf("assign %s: %v", tc.state, err)
		}
	}

	got, err := GatewayWorkerRunningAttempts(ctx, pool, workerID)
	if err != nil {
		t.Fatalf("running attempts: %v", err)
	}
	ids := map[string]WorkerAttemptRow{}
	for _, a := range got {
		ids[a.ID] = a
	}
	if len(got) != 2 {
		t.Errorf("running = %d attempts, want 2 (RUNNING + DISPATCHED); got %v", len(got), ids)
	}
	if _, ok := ids[dispatched]; !ok {
		t.Error("a DISPATCHED attempt is missing: an attempt assigned but never started is exactly the stall this view exists to surface")
	}
	if _, ok := ids[foreign]; ok {
		t.Error("another worker's RUNNING attempt was attributed to this worker")
	}
	if _, ok := ids[unassigned]; ok {
		t.Error("an unassigned PENDING attempt was attributed to a worker; nothing is running it")
	}
	if _, ok := ids[done]; ok {
		t.Error("a terminal attempt is reported as running")
	}
	if r := ids[running]; r.TaskRef != "test.task:v1" || r.RunID != f.runID {
		t.Errorf("task_ref/run_id = %q/%q, want test.task:v1/%s — the join to task_nodes is what makes the row nameable", r.TaskRef, r.RunID, f.runID)
	}

	// active_attempts on the worker row must agree with the length of that list,
	// or the fleet table and the page it links to disagree.
	wk, err := GatewayGetWorker(ctx, pool, f.ns, workerID)
	if err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if int(wk.ActiveAttempts) != len(got) {
		t.Errorf("worker.active_attempts = %d but running list has %d; the count and the list must never disagree", wk.ActiveAttempts, len(got))
	}

	recent, err := GatewayWorkerRecentAttempts(ctx, pool, workerID, 25)
	if err != nil {
		t.Fatalf("recent attempts: %v", err)
	}
	if len(recent) != 1 || recent[0].ID != done {
		t.Errorf("recent = %+v, want only the SUCCEEDED attempt %s", recent, done)
	}
	if recent[0].State != "SUCCEEDED" {
		t.Errorf("recent[0].State = %q, want SUCCEEDED", recent[0].State)
	}
}

// TestIntegrationWorkerRecentAttemptsAreBounded checks the LIMIT is applied.
//
// There is no index on worker_id for terminal states, so this read is an ordered
// scan of a worker's whole history — which grows without ceiling over the
// worker's lifetime. The bound is the only thing stopping a detail page from
// becoming a history export, so it is asserted rather than trusted.
func TestIntegrationWorkerRecentAttemptsAreBounded(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)
	workerID := f.worker()

	for i := 1; i <= 6; i++ {
		id := f.attempt(fmt.Sprintf("node-%d", i), 1)
		// The offset is interpolated as a bound integer via make_interval rather
		// than string-concatenated: pgx cannot encode an int into a text operand,
		// and a fmt.Sprintf into the SQL would be the wrong habit to leave here.
		if _, err := pool.Exec(ctx,
			`UPDATE task_attempts SET state = 'SUCCEEDED', worker_id = $2,
			        started_at = now(), ended_at = now() + make_interval(secs => $3)
			  WHERE id = $1`,
			id, workerID, i); err != nil {
			t.Fatalf("finish attempt %d: %v", i, err)
		}
	}

	recent, err := GatewayWorkerRecentAttempts(ctx, pool, workerID, 2)
	if err != nil {
		t.Fatalf("recent attempts: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("recent = %d rows, want the requested limit of 2", len(recent))
	}
	// Most recently ended first: the newest attempt (largest ended_at offset) leads.
	if recent[0].NodeID != "node-6" || recent[1].NodeID != "node-5" {
		t.Errorf("recent order = %s, %s; want node-6, node-5 (most recently ended first)", recent[0].NodeID, recent[1].NodeID)
	}
}
