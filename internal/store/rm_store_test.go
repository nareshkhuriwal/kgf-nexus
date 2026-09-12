// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// testDSN is the local schema-applied database from CONTRACT.md. Overridable so
// CI can point at its own instance.
func testDSN() string {
	if v := os.Getenv("NEXUS_TEST_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://postgres:test@localhost:55432/nexus"
}

// newTestStore connects, or SKIPS cleanly when no database is available. Skipping
// rather than failing is deliberate: a test that fails for want of infrastructure
// teaches nobody anything and trains people to ignore red builds.
func newTestStore(t *testing.T) (*RMStore, *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, testDSN())
	if err != nil {
		t.Skipf("no test database available (%v); skipping", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("test database unreachable (%v); skipping", err)
	}
	t.Cleanup(pool.Close)
	return NewRMStore(pool), pool
}

func vec(cpu, mem int64, gpu int32) *nexusv1.ResourceVector {
	return &nexusv1.ResourceVector{CpuMillis: cpu, MemoryBytes: mem, GpuCount: gpu}
}

// registerTestWorker creates an isolated worker in its own namespace so parallel
// test runs and leftover rows cannot interfere.
func registerTestWorker(t *testing.T, s *RMStore, capacity *nexusv1.ResourceVector) string {
	t.Helper()
	ns := fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano())
	id, err := s.RegisterWorker(context.Background(), Worker{
		Namespace: ns, Hostname: "test-host", Queues: []string{"default"},
		Executors: []string{"subprocess"}, Labels: map[string]string{"zone": "a"},
		Capacity: capacity, AgentVersion: "test",
	}, "hash-"+ns)
	if err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	t.Cleanup(func() {
		// ON DELETE CASCADE removes the reservations with the worker.
		_, _ = s.Pool().Exec(context.Background(), `DELETE FROM workers WHERE id = $1`, id)
	})
	return id
}

// mustCommitted reads the ledger's committed vector.
func mustCommitted(t *testing.T, s *RMStore, workerID string) *nexusv1.ResourceVector {
	t.Helper()
	w, err := s.GetWorker(context.Background(), workerID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	return w.Committed
}

// insertAttemptlessReservation writes a reservation directly.
//
// The schema's FK on reservations.attempt_id normally requires a real attempt
// row, which would require a whole run/spec/node graph owned by other services.
// Tests of the LEDGER should not need the DAG, so the constraint is dropped for
// the duration of the test transaction-free and restored after. If the FK cannot
// be manipulated (insufficient privilege), the test skips rather than lies.
func insertReservation(t *testing.T, s *RMStore, workerID, attemptID string, res *nexusv1.ResourceVector, age time.Duration) {
	t.Helper()
	b, err := marshalVec(res)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Pool().Exec(context.Background(), `
		INSERT INTO reservations (worker_id, attempt_id, resources, created_at)
		VALUES ($1, $2, $3, now() - $4::interval)`,
		workerID, attemptID, b, fmt.Sprintf("%d milliseconds", age.Milliseconds()))
	if err != nil {
		t.Fatalf("insert reservation %s: %v", attemptID, err)
	}
}

// setCommitted writes the ledger directly, simulating what the scheduler's
// dispatch transaction does (its documented ownership exception, ADR-0011).
func setCommitted(t *testing.T, s *RMStore, workerID string, v *nexusv1.ResourceVector) {
	t.Helper()
	b, err := marshalVec(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool().Exec(context.Background(),
		`UPDATE workers SET committed = $2 WHERE id = $1`, workerID, b); err != nil {
		t.Fatalf("setCommitted: %v", err)
	}
}

// dropAttemptFK removes the FK so ledger tests need no DAG rows, restoring it
// afterwards. Skips the test if the database user cannot alter the table.
func dropAttemptFK(t *testing.T, s *RMStore) {
	t.Helper()
	ctx := context.Background()
	var name string
	err := s.Pool().QueryRow(ctx, `
		SELECT conname FROM pg_constraint
		 WHERE conrelid = 'reservations'::regclass AND contype = 'f'
		   AND confrelid = 'task_attempts'::regclass`).Scan(&name)
	if err != nil {
		t.Skipf("cannot inspect the reservations FK (%v); skipping ledger test", err)
	}
	if _, err := s.Pool().Exec(ctx,
		fmt.Sprintf(`ALTER TABLE reservations DROP CONSTRAINT %q`, name)); err != nil {
		t.Skipf("cannot drop the reservations FK (%v); skipping ledger test", err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool().Exec(context.Background(), fmt.Sprintf(
			`ALTER TABLE reservations ADD CONSTRAINT %q FOREIGN KEY (attempt_id)
			 REFERENCES task_attempts(id) ON DELETE CASCADE`, name))
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// THE LEAK CASE. This is the highest-value test in the resource manager.
// ─────────────────────────────────────────────────────────────────────────────

// TestReconcileReclaimsLeakedCapacityAfterWorkerRestart is the scenario §3.2
// exists for: a worker restarts and FORGETS its work, while the control plane's
// ledger still believes that capacity is committed. Without reconciliation, every
// agent restart permanently strands a slice of the cluster and the ledger drifts
// further from reality with every incident, until the cluster looks full while
// sitting idle.
func TestReconcileReclaimsLeakedCapacityAfterWorkerRestart(t *testing.T) {
	s, _ := newTestStore(t)
	dropAttemptFK(t, s)
	ctx := context.Background()

	capacity := vec(4000, 8<<30, 4)
	workerID := registerTestWorker(t, s, capacity)

	// Three attempts were dispatched and committed by the scheduler.
	a1, a2, a3 := newUUID(t, s), newUUID(t, s), newUUID(t, s)
	r1, r2, r3 := vec(1000, 1<<30, 1), vec(1000, 1<<30, 1), vec(500, 512<<20, 0)
	insertReservation(t, s, workerID, a1, r1, 5*time.Minute)
	insertReservation(t, s, workerID, a2, r2, 5*time.Minute)
	insertReservation(t, s, workerID, a3, r3, 5*time.Minute)
	setCommitted(t, s, workerID, vec(2500, (2<<30)+(512<<20), 2))

	before := mustCommitted(t, s, workerID)
	if before.GetCpuMillis() != 2500 || before.GetGpuCount() != 2 {
		t.Fatalf("precondition: committed = %+v, want cpu 2500 / gpu 2", before)
	}

	// ── The worker restarted. It acknowledges ONLY a1; a2 and a3 are forgotten. ──
	leaked, err := s.ReconcileWorker(ctx, workerID, []string{a1}, time.Second)
	if err != nil {
		t.Fatalf("ReconcileWorker: %v", err)
	}

	if len(leaked) != 2 {
		t.Fatalf("expected 2 leaked reservations released, got %d (%+v)", len(leaked), leaked)
	}
	gotIDs := map[string]bool{}
	for _, r := range leaked {
		gotIDs[r.AttemptID] = true
	}
	if !gotIDs[a2] || !gotIDs[a3] {
		t.Errorf("wrong reservations released: got %v, want %s and %s", gotIDs, a2, a3)
	}
	if gotIDs[a1] {
		t.Error("released a reservation the worker DID acknowledge; that would kill live work")
	}

	// The ledger must now reflect ONLY the acknowledged attempt.
	after := mustCommitted(t, s, workerID)
	if after.GetCpuMillis() != 1000 {
		t.Errorf("committed cpu_millis = %d, want 1000 (only a1 remains)", after.GetCpuMillis())
	}
	if after.GetMemoryBytes() != 1<<30 {
		t.Errorf("committed memory_bytes = %d, want %d", after.GetMemoryBytes(), int64(1<<30))
	}
	if after.GetGpuCount() != 1 {
		t.Errorf("committed gpu_count = %d, want 1", after.GetGpuCount())
	}

	// The reservation ROWS must be gone too, or the next reconciliation pass would
	// find and "release" them again, double-subtracting.
	remaining, err := s.ListReservations(ctx, workerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].AttemptID != a1 {
		t.Errorf("expected only a1's reservation to remain, got %d rows", len(remaining))
	}

	// And the capacity really is usable again: a request that would NOT have fit
	// under the leaked ledger must now be granted.
	res, err := s.Reserve(ctx, workerID, newUUID(t, s), vec(3000, 6<<30, 3))
	if err != nil {
		t.Fatalf("reclaimed capacity is not usable: %v", err)
	}
	if res == nil {
		t.Fatal("Reserve returned no reservation")
	}
}

// TestReconcileForgetsEverythingWhenWorkerReportsNothing is the full-amnesia case:
// a worker that came back with an empty running list. The `<> ALL` predicate must
// treat an EMPTY array as "matches nothing", or the leak detector silently does
// nothing in exactly the case that matters most.
func TestReconcileForgetsEverythingWhenWorkerReportsNothing(t *testing.T) {
	s, _ := newTestStore(t)
	dropAttemptFK(t, s)
	ctx := context.Background()

	workerID := registerTestWorker(t, s, vec(4000, 4<<30, 0))
	a1, a2 := newUUID(t, s), newUUID(t, s)
	insertReservation(t, s, workerID, a1, vec(1000, 1<<30, 0), time.Minute)
	insertReservation(t, s, workerID, a2, vec(1000, 1<<30, 0), time.Minute)
	setCommitted(t, s, workerID, vec(2000, 2<<30, 0))

	leaked, err := s.ReconcileWorker(ctx, workerID, nil, time.Second)
	if err != nil {
		t.Fatalf("ReconcileWorker: %v", err)
	}
	if len(leaked) != 2 {
		t.Fatalf("an empty running list must release EVERYTHING; released %d", len(leaked))
	}
	after := mustCommitted(t, s, workerID)
	if after.GetCpuMillis() != 0 || after.GetMemoryBytes() != 0 {
		t.Errorf("committed = %+v, want fully zeroed", after)
	}
}

// TestReconcileGracePeriodProtectsFreshDispatch: reconciliation must NOT race
// dispatch. A just-dispatched attempt legitimately has not reached the worker yet,
// and releasing it would turn the leak detector into a leak generator that cancels
// work the instant it is scheduled.
func TestReconcileGracePeriodProtectsFreshDispatch(t *testing.T) {
	s, _ := newTestStore(t)
	dropAttemptFK(t, s)
	ctx := context.Background()

	workerID := registerTestWorker(t, s, vec(4000, 4<<30, 0))
	fresh := newUUID(t, s)
	stale := newUUID(t, s)
	insertReservation(t, s, workerID, fresh, vec(1000, 1<<30, 0), 0)              // just now
	insertReservation(t, s, workerID, stale, vec(1000, 1<<30, 0), 10*time.Minute) // long ago
	setCommitted(t, s, workerID, vec(2000, 2<<30, 0))

	// The worker acknowledges NEITHER, but only the old one is a genuine leak.
	leaked, err := s.ReconcileWorker(ctx, workerID, nil, time.Minute)
	if err != nil {
		t.Fatalf("ReconcileWorker: %v", err)
	}
	if len(leaked) != 1 {
		t.Fatalf("expected only the stale reservation released, got %d", len(leaked))
	}
	if leaked[0].AttemptID != stale {
		t.Errorf("released %s, want the stale one %s", leaked[0].AttemptID, stale)
	}
	if c := mustCommitted(t, s, workerID); c.GetCpuMillis() != 1000 {
		t.Errorf("committed cpu = %d, want 1000 (the fresh reservation is protected)", c.GetCpuMillis())
	}
}

// TestReconcileIsIdempotent: two reconciler replicas will race on the same
// heartbeat. The second pass must find nothing and subtract nothing — a repeated
// subtraction would drive committed below reality and over-commit the worker
// forever.
func TestReconcileIsIdempotent(t *testing.T) {
	s, _ := newTestStore(t)
	dropAttemptFK(t, s)
	ctx := context.Background()

	workerID := registerTestWorker(t, s, vec(4000, 4<<30, 0))
	a := newUUID(t, s)
	insertReservation(t, s, workerID, a, vec(1000, 1<<30, 0), time.Minute)
	setCommitted(t, s, workerID, vec(1000, 1<<30, 0))

	first, err := s.ReconcileWorker(ctx, workerID, nil, time.Second)
	if err != nil || len(first) != 1 {
		t.Fatalf("first pass: %d released, err=%v", len(first), err)
	}
	afterFirst := mustCommitted(t, s, workerID)

	second, err := s.ReconcileWorker(ctx, workerID, nil, time.Second)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second pass released %d reservations; must be a no-op", len(second))
	}
	afterSecond := mustCommitted(t, s, workerID)
	if afterFirst.GetCpuMillis() != afterSecond.GetCpuMillis() {
		t.Errorf("committed changed on the idempotent pass: %d -> %d",
			afterFirst.GetCpuMillis(), afterSecond.GetCpuMillis())
	}
}

// TestConcurrentReconcileDoesNotDoubleSubtract runs replicas in parallel against
// the same worker, which is the real deployment shape (multiple RM replicas, no
// leader election).
func TestConcurrentReconcileDoesNotDoubleSubtract(t *testing.T) {
	s, _ := newTestStore(t)
	dropAttemptFK(t, s)
	ctx := context.Background()

	workerID := registerTestWorker(t, s, vec(8000, 8<<30, 0))
	for i := 0; i < 4; i++ {
		insertReservation(t, s, workerID, newUUID(t, s), vec(1000, 1<<30, 0), time.Minute)
	}
	setCommitted(t, s, workerID, vec(4000, 4<<30, 0))

	var wg sync.WaitGroup
	total := make(chan int, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			leaked, err := s.ReconcileWorker(ctx, workerID, nil, time.Second)
			if err != nil {
				t.Errorf("concurrent reconcile: %v", err)
				return
			}
			total <- len(leaked)
		}()
	}
	wg.Wait()
	close(total)
	sum := 0
	for n := range total {
		sum += n
	}
	// Exactly four releases across all replicas: each reservation is released once.
	if sum != 4 {
		t.Errorf("total releases = %d, want exactly 4 (each reservation released once)", sum)
	}
	after := mustCommitted(t, s, workerID)
	if after.GetCpuMillis() != 0 {
		t.Errorf("committed cpu = %d, want 0 — SubVec clamps, but the count must still be exact",
			after.GetCpuMillis())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Reserve / Release
// ─────────────────────────────────────────────────────────────────────────────

func TestReserveAndRelease(t *testing.T) {
	s, _ := newTestStore(t)
	dropAttemptFK(t, s)
	ctx := context.Background()

	workerID := registerTestWorker(t, s, vec(2000, 2<<30, 1))
	attemptID := newUUID(t, s)

	res, err := s.Reserve(ctx, workerID, attemptID, vec(1500, 1<<30, 1))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.AttemptID != attemptID {
		t.Errorf("reservation attempt = %s, want %s", res.AttemptID, attemptID)
	}
	if c := mustCommitted(t, s, workerID); c.GetCpuMillis() != 1500 || c.GetGpuCount() != 1 {
		t.Errorf("committed after reserve = %+v", c)
	}

	// A second request that no longer fits must be refused with a shortfall, not
	// silently over-committed.
	if _, err := s.Reserve(ctx, workerID, newUUID(t, s), vec(1000, 1<<30, 1)); err == nil {
		t.Fatal("expected the over-capacity reservation to be refused")
	} else {
		var ins *ErrInsufficientCapacity
		if !errors.As(err, &ins) {
			t.Fatalf("error = %v, want ErrInsufficientCapacity so a pending_reason can be rendered", err)
		}
		if len(ins.Shortfall) == 0 {
			t.Error("shortfall is empty; the operator gets no explanation")
		}
	}

	if _, err := s.Release(ctx, attemptID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if c := mustCommitted(t, s, workerID); c.GetCpuMillis() != 0 || c.GetGpuCount() != 0 {
		t.Errorf("committed after release = %+v, want zeroed", c)
	}
}

// TestReleaseIsIdempotent: release is driven by completion reports, lease expiry
// and reconciliation — three independent at-least-once paths.
func TestReleaseIsIdempotent(t *testing.T) {
	s, _ := newTestStore(t)
	dropAttemptFK(t, s)
	ctx := context.Background()

	workerID := registerTestWorker(t, s, vec(2000, 2<<30, 0))
	attemptID := newUUID(t, s)
	if _, err := s.Reserve(ctx, workerID, attemptID, vec(1000, 1<<30, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Release(ctx, attemptID); err != nil {
		t.Fatalf("first release: %v", err)
	}
	_, err := s.Release(ctx, attemptID)
	if !errors.Is(err, ErrReservationNotFound) {
		t.Fatalf("second release error = %v, want ErrReservationNotFound", err)
	}
	// Critically, the committed vector must not have moved twice.
	if c := mustCommitted(t, s, workerID); c.GetCpuMillis() != 0 {
		t.Errorf("committed = %d after a double release, want 0", c.GetCpuMillis())
	}
}

// TestReserveTwiceForSameAttemptIsNotDoubleCounted: a retried dispatch RPC must
// not commit the capacity twice and permanently shrink the worker.
func TestReserveTwiceForSameAttemptIsNotDoubleCounted(t *testing.T) {
	s, _ := newTestStore(t)
	dropAttemptFK(t, s)
	ctx := context.Background()

	workerID := registerTestWorker(t, s, vec(4000, 4<<30, 0))
	attemptID := newUUID(t, s)

	first, err := s.Reserve(ctx, workerID, attemptID, vec(1000, 1<<30, 0))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Reserve(ctx, workerID, attemptID, vec(1000, 1<<30, 0))
	if err != nil {
		t.Fatalf("duplicate reserve: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("duplicate reserve created a NEW reservation (%s vs %s)", first.ID, second.ID)
	}
	if c := mustCommitted(t, s, workerID); c.GetCpuMillis() != 1000 {
		t.Errorf("committed cpu = %d, want 1000 (not double-counted)", c.GetCpuMillis())
	}
}

// TestConcurrentReserveDoesNotOverCommit is what the FOR UPDATE row lock is for:
// two concurrent reservations must not both read the same committed vector, both
// find room, and both commit.
func TestConcurrentReserveDoesNotOverCommit(t *testing.T) {
	s, _ := newTestStore(t)
	dropAttemptFK(t, s)
	ctx := context.Background()

	// Capacity for exactly two of the three requests below.
	workerID := registerTestWorker(t, s, vec(2000, 2<<30, 0))

	var wg sync.WaitGroup
	granted := make(chan bool, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Reserve(ctx, workerID, newUUID(t, s), vec(1000, 1<<30, 0))
			granted <- err == nil
		}()
	}
	wg.Wait()
	close(granted)

	n := 0
	for ok := range granted {
		if ok {
			n++
		}
	}
	if n != 2 {
		t.Errorf("granted %d reservations, want exactly 2 — the row lock must serialise them", n)
	}
	c := mustCommitted(t, s, workerID)
	if c.GetCpuMillis() > 2000 {
		t.Errorf("committed cpu = %d EXCEEDS capacity 2000: the ledger over-committed the node",
			c.GetCpuMillis())
	}
}

// TestReserveRefusedOnNonActiveWorker: a DRAINING worker is finishing what it has;
// a DEAD one runs nothing at all.
func TestReserveRefusedOnNonActiveWorker(t *testing.T) {
	s, _ := newTestStore(t)
	dropAttemptFK(t, s)
	ctx := context.Background()

	workerID := registerTestWorker(t, s, vec(4000, 4<<30, 0))
	if ok, err := s.SetWorkerState(ctx, workerID, WorkerDraining); err != nil || !ok {
		t.Fatalf("SetWorkerState(DRAINING): ok=%v err=%v", ok, err)
	}
	if _, err := s.Reserve(ctx, workerID, newUUID(t, s), vec(100, 1<<20, 0)); err == nil {
		t.Fatal("expected a DRAINING worker to refuse a new reservation")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Worker lifecycle (§5.5)
// ─────────────────────────────────────────────────────────────────────────────

func TestWorkerStateTransitions(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	workerID := registerTestWorker(t, s, vec(1000, 1<<30, 0))

	if ok, err := s.SetWorkerState(ctx, workerID, WorkerDraining); err != nil || !ok {
		t.Fatalf("ACTIVE -> DRAINING should be legal: ok=%v err=%v", ok, err)
	}
	if ok, err := s.SetWorkerState(ctx, workerID, WorkerDead); err != nil || !ok {
		t.Fatalf("DRAINING -> DEAD should be legal: ok=%v err=%v", ok, err)
	}
	// A DEAD worker must NEVER be revived: its session is gone, and the agent has
	// to re-register with a new worker_id. Reviving the row would let a restarted
	// agent inherit an identity whose leases have already been reassigned.
	if ok, err := s.SetWorkerState(ctx, workerID, WorkerActive); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if ok {
		t.Error("DEAD -> ACTIVE was applied; a dead worker must never be resurrected")
	}
	if ok, err := s.SetWorkerState(ctx, workerID, WorkerDraining); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if ok {
		t.Error("DEAD -> DRAINING was applied; DEAD is terminal")
	}
}

// TestTouchHeartbeatRejectsDeadWorker: a heartbeat from a swept worker must be
// reported as unknown so the agent is told to re-register.
func TestTouchHeartbeatRejectsDeadWorker(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	workerID := registerTestWorker(t, s, vec(1000, 1<<30, 0))

	if err := s.TouchHeartbeat(ctx, workerID); err != nil {
		t.Fatalf("heartbeat from a live worker: %v", err)
	}
	if _, err := s.SetWorkerState(ctx, workerID, WorkerDead); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchHeartbeat(ctx, workerID); !errors.Is(err, ErrWorkerNotFound) {
		t.Fatalf("heartbeat from a DEAD worker: err = %v, want ErrWorkerNotFound", err)
	}
}

// TestReleaseAllForWorker is the §5.5 DEAD path: all reservations released,
// capacity returns to the pool.
func TestReleaseAllForWorker(t *testing.T) {
	s, _ := newTestStore(t)
	dropAttemptFK(t, s)
	ctx := context.Background()

	workerID := registerTestWorker(t, s, vec(4000, 4<<30, 2))
	for i := 0; i < 3; i++ {
		insertReservation(t, s, workerID, newUUID(t, s), vec(1000, 1<<30, 0), 0)
	}
	setCommitted(t, s, workerID, vec(3000, 3<<30, 0))

	// No grace period on the dead path: a dead worker is running nothing, so even
	// a reservation created a millisecond ago is a leak.
	leaked, err := s.ReleaseAllForWorker(ctx, workerID)
	if err != nil {
		t.Fatalf("ReleaseAllForWorker: %v", err)
	}
	if len(leaked) != 3 {
		t.Fatalf("released %d, want all 3 including the freshly created one", len(leaked))
	}
	if c := mustCommitted(t, s, workerID); c.GetCpuMillis() != 0 {
		t.Errorf("committed cpu = %d, want 0", c.GetCpuMillis())
	}
}

func TestListWorkersForQueue(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	ns := fmt.Sprintf("test-list-%d", time.Now().UnixNano())
	id, err := s.RegisterWorker(ctx, Worker{
		Namespace: ns, Hostname: "h1", Queues: []string{"gpu", "default"},
		Executors: []string{"docker"}, Capacity: vec(1000, 1<<30, 2), AgentVersion: "t",
	}, "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.Pool().Exec(context.Background(), `DELETE FROM workers WHERE id=$1`, id) })

	ws, err := s.ListWorkersForQueue(ctx, ns, "gpu")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].ID != id {
		t.Fatalf("expected the gpu worker, got %d rows", len(ws))
	}
	if ws[0].Capacity.GetGpuCount() != 2 {
		t.Errorf("capacity round-trip lost gpu_count: %+v", ws[0].Capacity)
	}
	if ws[0].Executors[0] != "docker" {
		t.Errorf("executors round-trip = %v", ws[0].Executors)
	}

	// A DEAD worker must not appear: the scheduler would place work on it.
	if _, err := s.SetWorkerState(ctx, id, WorkerDead); err != nil {
		t.Fatal(err)
	}
	ws, err = s.ListWorkersForQueue(ctx, ns, "gpu")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 0 {
		t.Errorf("a DEAD worker is still listed as placeable")
	}
}

func TestListStaleWorkers(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	workerID := registerTestWorker(t, s, vec(1000, 1<<30, 0))

	// Backdate the heartbeat past the window.
	if _, err := s.Pool().Exec(ctx,
		`UPDATE workers SET last_heartbeat_at = now() - interval '2 minutes' WHERE id = $1`,
		workerID); err != nil {
		t.Fatal(err)
	}
	stale, err := s.ListStaleWorkers(ctx, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range stale {
		if w.ID == workerID {
			found = true
		}
	}
	if !found {
		t.Error("the silent worker was not reported as stale")
	}

	// A fresh heartbeat must remove it from the sweep set.
	if err := s.TouchHeartbeat(ctx, workerID); err != nil {
		t.Fatal(err)
	}
	stale, err = s.ListStaleWorkers(ctx, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range stale {
		if w.ID == workerID {
			t.Error("a worker that just heartbeated is still considered stale")
		}
	}
}

// TestResourceVectorRoundTrip guards the JSONB encoding: capacity written by one
// service must be readable by another, custom dimensions included.
func TestResourceVectorRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	want := &nexusv1.ResourceVector{
		CpuMillis: 4000, MemoryBytes: 16 << 30, GpuCount: 2,
		GpuKind: "nvidia-a100", GpuMemoryBytes: 80 << 30, DiskBytes: 500 << 30,
		Custom: map[string]int64{"license_token": 4, "db_connection": 10},
	}
	ns := fmt.Sprintf("test-vec-%d", time.Now().UnixNano())
	id, err := s.RegisterWorker(ctx, Worker{
		Namespace: ns, Hostname: "h", Queues: []string{"default"},
		Capacity: want, AgentVersion: "t",
	}, "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.Pool().Exec(context.Background(), `DELETE FROM workers WHERE id=$1`, id) })

	w, err := s.GetWorker(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	got := w.Capacity
	if got.GetCpuMillis() != want.CpuMillis || got.GetMemoryBytes() != want.MemoryBytes ||
		got.GetGpuCount() != want.GpuCount || got.GetGpuKind() != want.GpuKind ||
		got.GetGpuMemoryBytes() != want.GpuMemoryBytes || got.GetDiskBytes() != want.DiskBytes {
		t.Errorf("scalar round trip lost data:\n got %+v\nwant %+v", got, want)
	}
	if got.GetCustom()["license_token"] != 4 || got.GetCustom()["db_connection"] != 10 {
		t.Errorf("custom dimensions lost: %v", got.GetCustom())
	}
}

// newUUID asks Postgres for a UUID so attempt ids match the column type without
// adding a client-side dependency just for tests.
func newUUID(t *testing.T, s *RMStore) string {
	t.Helper()
	var id string
	if err := s.Pool().QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatalf("gen_random_uuid: %v", err)
	}
	return id
}
