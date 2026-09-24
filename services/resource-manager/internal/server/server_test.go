// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testDSN() string {
	if v := os.Getenv("NEXUS_TEST_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://postgres:test@localhost:55432/nexus"
}

// newTestServer connects, or SKIPS cleanly when no database is reachable.
func newTestServer(t *testing.T) (*Server, *store.RMStore) {
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

	st := store.NewRMStore(pool)
	s := New(st, testLog())
	// A short grace so tests do not have to wait out the production window.
	s.ReconcileGrace = 10 * time.Millisecond
	return s, st
}

func vec(cpu, mem int64, gpu int32) *nexusv1.ResourceVector {
	return &nexusv1.ResourceVector{CpuMillis: cpu, MemoryBytes: mem, GpuCount: gpu}
}

func registerWorker(t *testing.T, s *Server, st *store.RMStore, capacity *nexusv1.ResourceVector) string {
	t.Helper()
	ns := fmt.Sprintf("rmsrv-%s-%d", t.Name(), time.Now().UnixNano())
	ensureTenant(t, st, ns)
	resp, err := s.RegisterWorker(context.Background(), &rmv1.RegisterWorkerRequest{
		Namespace: ns, Hostname: "host-1", Queues: []string{"default"},
		Executors: []string{"subprocess", "docker"},
		Labels:    map[string]string{"zone": "a"},
		Capacity:  capacity, AgentVersion: "test", SessionTokenHash: "hash",
	})
	if err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	id := resp.GetWorkerId()
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM workers WHERE id = $1`, id)
	})
	return id
}

// ensureTenant registers a namespace's tenant. Since 0014 every workers row must
// belong to a registered tenant, and these tests mint a namespace per test.
func ensureTenant(t *testing.T, st *store.RMStore, ns string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO tenants (namespace, name) VALUES ($1, $1) ON CONFLICT (namespace) DO NOTHING`, ns); err != nil {
		t.Fatalf("register tenant %s: %v", ns, err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM workers WHERE namespace = $1`, ns)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM tenants WHERE namespace = $1`, ns)
	})
}

// dropAttemptFK lets ledger tests write reservations without a full DAG. Skips if
// the database user cannot alter the table.
func dropAttemptFK(t *testing.T, st *store.RMStore) {
	t.Helper()
	ctx := context.Background()
	var name string
	if err := st.Pool().QueryRow(ctx, `
		SELECT conname FROM pg_constraint
		 WHERE conrelid = 'reservations'::regclass AND contype = 'f'
		   AND confrelid = 'task_attempts'::regclass`).Scan(&name); err != nil {
		t.Skipf("cannot inspect the reservations FK (%v); skipping", err)
	}
	if _, err := st.Pool().Exec(ctx,
		fmt.Sprintf(`ALTER TABLE reservations DROP CONSTRAINT %q`, name)); err != nil {
		t.Skipf("cannot drop the reservations FK (%v); skipping", err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), fmt.Sprintf(
			`ALTER TABLE reservations ADD CONSTRAINT %q FOREIGN KEY (attempt_id)
			 REFERENCES task_attempts(id) ON DELETE CASCADE`, name))
	})
}

func newUUID(t *testing.T, st *store.RMStore) string {
	t.Helper()
	var id string
	if err := st.Pool().QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatalf("gen_random_uuid: %v", err)
	}
	return id
}

// TestRegisterCreatesANewSessionIdentityEveryTime: identity is per-SESSION, not
// per-host (§5.5). A restarted agent that reused its predecessor's id could present
// a stale lease token that still matched and resurrect ownership of work already
// re-dispatched.
func TestRegisterCreatesANewSessionIdentityEveryTime(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	ns := fmt.Sprintf("rmsrv-session-%d", time.Now().UnixNano())

	req := &rmv1.RegisterWorkerRequest{
		Namespace: ns, Hostname: "same-host", Queues: []string{"default"},
		Capacity: vec(1000, 1<<30, 0), AgentVersion: "v1", SessionTokenHash: "h1",
	}
	first, err := s.RegisterWorker(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RegisterWorker(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(),
			`DELETE FROM workers WHERE id = ANY($1::uuid[])`,
			[]string{first.GetWorkerId(), second.GetWorkerId()})
	})

	if first.GetWorkerId() == second.GetWorkerId() {
		t.Fatal("the same host reusing a worker_id would let a restarted agent inherit stale leases")
	}
}

func TestRegisterValidatesInput(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	for name, req := range map[string]*rmv1.RegisterWorkerRequest{
		"no hostname":  {Namespace: "ns", Capacity: vec(1, 1, 0)},
		"no namespace": {Hostname: "h", Capacity: vec(1, 1, 0)},
		"no capacity":  {Namespace: "ns", Hostname: "h"},
	} {
		if _, err := s.RegisterWorker(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: err = %v, want InvalidArgument", name, err)
		}
	}
}

// ── THE LEAK CASE, through the API ───────────────────────────────────────────

// TestHeartbeatReconcilesLeakedCapacity is the §3.2 behaviour end-to-end: the
// worker's reported running_attempt_ids are ground truth, and any reservation the
// ledger holds that the worker does not acknowledge is released.
func TestHeartbeatReconcilesLeakedCapacity(t *testing.T) {
	s, st := newTestServer(t)
	dropAttemptFK(t, st)
	ctx := context.Background()

	workerID := registerWorker(t, s, st, vec(4000, 4<<30, 0))
	kept := newUUID(t, st)
	leaked1 := newUUID(t, st)
	leaked2 := newUUID(t, st)
	for _, id := range []string{kept, leaked1, leaked2} {
		if _, err := st.Reserve(ctx, workerID, id, vec(1000, 1<<30, 0)); err != nil {
			t.Fatalf("Reserve %s: %v", id, err)
		}
	}
	// Let the grace window pass so these are eligible for reconciliation.
	time.Sleep(30 * time.Millisecond)

	resp, err := s.HeartbeatWorker(ctx, &rmv1.HeartbeatWorkerRequest{
		WorkerId:          workerID,
		RunningAttemptIds: []string{kept},
		ReconcileGraceMs:  1,
	})
	if err != nil {
		t.Fatalf("HeartbeatWorker: %v", err)
	}
	if !resp.GetWorkerKnown() {
		t.Fatal("a live worker was reported as unknown")
	}
	if got := len(resp.GetReleased()); got != 2 {
		t.Fatalf("released %d reservations, want 2", got)
	}
	for _, r := range resp.GetReleased() {
		if r.GetAttemptId() == kept {
			t.Error("released a reservation the worker acknowledged; that would kill live work")
		}
	}
	if got := resp.GetCommitted().GetCpuMillis(); got != 1000 {
		t.Errorf("committed cpu = %d, want 1000 (only the acknowledged attempt)", got)
	}
}

// TestHeartbeatFromUnknownWorkerIsNotAnError: the correct response is a REREGISTER
// directive, and returning NOT_FOUND would make a routine post-sweep heartbeat look
// like a fault in every dashboard.
func TestHeartbeatFromUnknownWorkerIsNotAnError(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	resp, err := s.HeartbeatWorker(ctx, &rmv1.HeartbeatWorkerRequest{
		WorkerId: newUUID(t, st),
	})
	if err != nil {
		t.Fatalf("err = %v, want a clean worker_known=false response", err)
	}
	if resp.GetWorkerKnown() {
		t.Error("worker_known = true for a worker that does not exist")
	}
}

func TestHeartbeatFromDeadWorkerRequiresReregistration(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	workerID := registerWorker(t, s, st, vec(1000, 1<<30, 0))

	if _, err := s.SetWorkerState(ctx, &rmv1.SetWorkerStateRequest{
		WorkerId: workerID, State: rmv1.WorkerState_WORKER_STATE_DEAD, Reason: "test",
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := s.HeartbeatWorker(ctx, &rmv1.HeartbeatWorkerRequest{WorkerId: workerID})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if resp.GetWorkerKnown() {
		t.Error("a DEAD worker's heartbeat was accepted; it must be told to re-register")
	}
}

// ── Reserve / Release through the API ────────────────────────────────────────

// TestReserveRefusalCarriesAShortfall: "your task has been PENDING for 40 minutes"
// is an unactionable support ticket, while "waiting_for_resources: gpu (need 4,
// free 3)" tells the operator whether to add capacity or shrink the request.
func TestReserveRefusalCarriesAShortfall(t *testing.T) {
	s, st := newTestServer(t)
	dropAttemptFK(t, st)
	ctx := context.Background()

	workerID := registerWorker(t, s, st, vec(1000, 1<<30, 1))
	resp, err := s.Reserve(ctx, &rmv1.ReserveRequest{
		WorkerId: workerID, AttemptId: newUUID(t, st), Request: vec(4000, 8<<30, 4),
	})
	if err != nil {
		t.Fatalf("a refusal is a normal outcome, not an error: %v", err)
	}
	if resp.GetGranted() {
		t.Fatal("granted a reservation that cannot fit")
	}
	if resp.GetShortfallReason() == "" {
		t.Error("no shortfall reason; the UI can only show an unexplained stall")
	}
	if resp.GetFree() == nil {
		t.Error("no free-capacity snapshot returned with the refusal")
	}
}

// TestReleaseIsIdempotentThroughTheAPI: release is driven by completion reports,
// lease expiry and reconciliation — three independent at-least-once paths.
func TestReleaseIsIdempotentThroughTheAPI(t *testing.T) {
	s, st := newTestServer(t)
	dropAttemptFK(t, st)
	ctx := context.Background()

	workerID := registerWorker(t, s, st, vec(2000, 2<<30, 0))
	attemptID := newUUID(t, st)
	if _, err := s.Reserve(ctx, &rmv1.ReserveRequest{
		WorkerId: workerID, AttemptId: attemptID, Request: vec(1000, 1<<30, 0),
	}); err != nil {
		t.Fatal(err)
	}

	first, err := s.Release(ctx, &rmv1.ReleaseRequest{AttemptId: attemptID})
	if err != nil || !first.GetReleased() {
		t.Fatalf("first release: released=%v err=%v", first.GetReleased(), err)
	}
	second, err := s.Release(ctx, &rmv1.ReleaseRequest{AttemptId: attemptID})
	if err != nil {
		t.Fatalf("a duplicate release must be SUCCESS, not an error: %v", err)
	}
	if second.GetReleased() {
		t.Error("the second release claimed to have released something")
	}
	w, err := s.GetWorker(ctx, &rmv1.GetWorkerRequest{WorkerId: workerID})
	if err != nil {
		t.Fatal(err)
	}
	if got := w.GetWorker().GetCommitted().GetCpuMillis(); got != 0 {
		t.Errorf("committed cpu = %d after a double release, want 0", got)
	}
}

// ── Worker lifecycle ─────────────────────────────────────────────────────────

// TestDeadWorkerReleasesAllCapacity is the §5.5 DEAD path: all reservations
// released, capacity returns to the pool.
func TestDeadWorkerReleasesAllCapacity(t *testing.T) {
	s, st := newTestServer(t)
	dropAttemptFK(t, st)
	ctx := context.Background()

	workerID := registerWorker(t, s, st, vec(4000, 4<<30, 0))
	for i := 0; i < 3; i++ {
		if _, err := st.Reserve(ctx, workerID, newUUID(t, st), vec(1000, 1<<30, 0)); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := s.SetWorkerState(ctx, &rmv1.SetWorkerStateRequest{
		WorkerId: workerID, State: rmv1.WorkerState_WORKER_STATE_DEAD, Reason: "liveness",
	})
	if err != nil {
		t.Fatalf("SetWorkerState: %v", err)
	}
	if !resp.GetApplied() {
		t.Fatal("ACTIVE -> DEAD was not applied")
	}
	if got := len(resp.GetReleased()); got != 3 {
		t.Errorf("released %d reservations, want 3 (a dead worker runs nothing)", got)
	}
	if got := resp.GetCurrent(); got != rmv1.WorkerState_WORKER_STATE_DEAD {
		t.Errorf("current state = %v, want DEAD", got)
	}
}

// Manual quarantine is recorded, excludes the worker from placement (state is
// no longer ACTIVE), keeps its heartbeat accepted, and release restores it.
func TestManualQuarantineAndRelease(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	workerID := registerWorker(t, s, st, vec(1000, 1<<30, 0))

	if _, err := s.SetWorkerState(ctx, &rmv1.SetWorkerStateRequest{
		WorkerId: workerID, State: rmv1.WorkerState_WORKER_STATE_QUARANTINED,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a quarantine without a reason: err = %v, want InvalidArgument", err)
	}
	resp, err := s.SetWorkerState(ctx, &rmv1.SetWorkerStateRequest{
		WorkerId: workerID, State: rmv1.WorkerState_WORKER_STATE_QUARANTINED,
		Reason: "bad GPU driver", Actor: "user:ops", QuarantineMs: 60_000,
	})
	if err != nil || !resp.GetApplied() || resp.GetCurrent() != rmv1.WorkerState_WORKER_STATE_QUARANTINED {
		t.Fatalf("quarantine: resp=%v err=%v", resp, err)
	}
	if err := st.TouchHeartbeat(ctx, workerID); err != nil {
		t.Fatalf("a quarantined worker's heartbeat was refused (it would be told to re-register): %v", err)
	}
	eps, err := st.ListQuarantines(ctx, workerID)
	if err != nil || len(eps) != 1 || eps[0].Trigger != store.QuarantineManual || eps[0].Actor != "user:ops" || eps[0].ExpiresAt == nil {
		t.Fatalf("episode = %+v err=%v", eps, err)
	}
	// A second quarantine is not applied: one open episode per worker.
	resp, err = s.SetWorkerState(ctx, &rmv1.SetWorkerStateRequest{
		WorkerId: workerID, State: rmv1.WorkerState_WORKER_STATE_QUARANTINED, Reason: "again", Actor: "user:ops",
	})
	if err != nil || resp.GetApplied() {
		t.Fatalf("double quarantine applied: %v %v", resp, err)
	}

	resp, err = s.SetWorkerState(ctx, &rmv1.SetWorkerStateRequest{
		WorkerId: workerID, State: rmv1.WorkerState_WORKER_STATE_ACTIVE, Actor: "user:ops", Reason: "driver fixed",
	})
	if err != nil || !resp.GetApplied() || resp.GetCurrent() != rmv1.WorkerState_WORKER_STATE_ACTIVE {
		t.Fatalf("release: resp=%v err=%v", resp, err)
	}
	eps, _ = st.ListQuarantines(ctx, workerID)
	if eps[0].ReleasedAt == nil || eps[0].ReleasedBy == nil || *eps[0].ReleasedBy != "user:ops" {
		t.Fatalf("release not recorded: %+v", eps[0])
	}
}

// Quarantine follows the host: a restarted agent's new session inherits it.
func TestQuarantineIsInheritedByANewSessionOfTheSameHost(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	first := registerWorker(t, s, st, vec(1000, 1<<30, 0))
	var ns string
	_ = st.Pool().QueryRow(ctx, `SELECT namespace FROM workers WHERE id=$1`, first).Scan(&ns)
	if _, err := s.SetWorkerState(ctx, &rmv1.SetWorkerStateRequest{
		WorkerId: first, State: rmv1.WorkerState_WORKER_STATE_QUARANTINED, Reason: "poison", Actor: "user:ops",
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := s.RegisterWorker(ctx, &rmv1.RegisterWorkerRequest{
		Namespace: ns, Hostname: "host-1", Queues: []string{"default"}, Capacity: vec(1000, 1<<30, 0),
		AgentVersion: "test", SessionTokenHash: "h2",
	})
	if err != nil {
		t.Fatal(err)
	}
	w, err := st.GetWorker(ctx, resp.GetWorkerId())
	if err != nil || w.State != store.WorkerQuarantined {
		t.Fatalf("new session state = %v err=%v, want QUARANTINED (inherited)", w, err)
	}
}

func TestIllegalTransitionIsReportedNotErrored(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	workerID := registerWorker(t, s, st, vec(1000, 1<<30, 0))

	if _, err := s.SetWorkerState(ctx, &rmv1.SetWorkerStateRequest{
		WorkerId: workerID, State: rmv1.WorkerState_WORKER_STATE_DEAD,
	}); err != nil {
		t.Fatal(err)
	}
	// DEAD -> ACTIVE is illegal. A losing race with a concurrent sweep is normal,
	// so this must be reported as applied=false, not as an error.
	resp, err := s.SetWorkerState(ctx, &rmv1.SetWorkerStateRequest{
		WorkerId: workerID, State: rmv1.WorkerState_WORKER_STATE_ACTIVE,
	})
	if err != nil {
		t.Fatalf("an illegal transition must not be an error: %v", err)
	}
	if resp.GetApplied() {
		t.Error("DEAD -> ACTIVE was applied; a dead worker must never be resurrected")
	}
	if resp.GetCurrent() != rmv1.WorkerState_WORKER_STATE_DEAD {
		t.Errorf("current = %v, want DEAD", resp.GetCurrent())
	}
}

func TestGetWorkerReportsFreeCapacity(t *testing.T) {
	s, st := newTestServer(t)
	dropAttemptFK(t, st)
	ctx := context.Background()

	workerID := registerWorker(t, s, st, vec(4000, 4<<30, 2))
	if _, err := st.Reserve(ctx, workerID, newUUID(t, st), vec(1000, 1<<30, 1)); err != nil {
		t.Fatal(err)
	}
	resp, err := s.GetWorker(ctx, &rmv1.GetWorkerRequest{WorkerId: workerID})
	if err != nil {
		t.Fatal(err)
	}
	w := resp.GetWorker()
	// Free is computed server-side so every caller agrees on it rather than each
	// re-deriving it and drifting.
	if w.GetFree().GetCpuMillis() != 3000 || w.GetFree().GetGpuCount() != 1 {
		t.Errorf("free = %+v, want cpu 3000 / gpu 1", w.GetFree())
	}
	if w.GetState() != rmv1.WorkerState_WORKER_STATE_ACTIVE {
		t.Errorf("state = %v, want ACTIVE", w.GetState())
	}
	if len(w.GetExecutors()) != 2 {
		t.Errorf("executors = %v, want both advertised kinds", w.GetExecutors())
	}
}

func TestGetWorkerNotFound(t *testing.T) {
	s, st := newTestServer(t)
	_, err := s.GetWorker(context.Background(), &rmv1.GetWorkerRequest{WorkerId: newUUID(t, st)})
	if status.Code(err) != codes.NotFound {
		t.Errorf("err = %v, want NotFound", err)
	}
}

func TestListWorkersExcludesDead(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()

	ns := fmt.Sprintf("rmsrv-list-%d", time.Now().UnixNano())
	mk := func() string {
		resp, err := s.RegisterWorker(ctx, &rmv1.RegisterWorkerRequest{
			Namespace: ns, Hostname: "h", Queues: []string{"default"},
			Capacity: vec(1000, 1<<30, 0), AgentVersion: "t", SessionTokenHash: "h",
		})
		if err != nil {
			t.Fatal(err)
		}
		id := resp.GetWorkerId()
		t.Cleanup(func() { _, _ = st.Pool().Exec(context.Background(), `DELETE FROM workers WHERE id=$1`, id) })
		return id
	}
	alive, dead := mk(), mk()
	if _, err := s.SetWorkerState(ctx, &rmv1.SetWorkerStateRequest{
		WorkerId: dead, State: rmv1.WorkerState_WORKER_STATE_DEAD,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := s.ListWorkers(ctx, &rmv1.ListWorkersRequest{Namespace: ns, Queue: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetWorkers()) != 1 || resp.GetWorkers()[0].GetId() != alive {
		t.Errorf("listed %d workers; a DEAD worker must not be placeable", len(resp.GetWorkers()))
	}
}

// ── Sweeper ──────────────────────────────────────────────────────────────────

// TestSweeperDeclaresSilentWorkersDeadAndReclaimsCapacity closes the path a
// heartbeat cannot: a worker that has stopped entirely never reports again, so
// without the sweep its reservations stay committed forever.
func TestSweeperDeclaresSilentWorkersDeadAndReclaimsCapacity(t *testing.T) {
	s, st := newTestServer(t)
	dropAttemptFK(t, st)
	ctx := context.Background()

	workerID := registerWorker(t, s, st, vec(4000, 4<<30, 0))
	if _, err := st.Reserve(ctx, workerID, newUUID(t, st), vec(2000, 2<<30, 0)); err != nil {
		t.Fatal(err)
	}
	// Backdate the heartbeat past the liveness window.
	if _, err := st.Pool().Exec(ctx,
		`UPDATE workers SET last_heartbeat_at = now() - interval '5 minutes' WHERE id = $1`,
		workerID); err != nil {
		t.Fatal(err)
	}

	sw := NewSweeper(st, testLog())
	sw.LivenessWindow = 30 * time.Second
	dead, released := sw.SweepOnce(ctx)

	if dead < 1 {
		t.Fatalf("declared %d workers dead, want at least 1", dead)
	}
	if released < 1 {
		t.Errorf("reclaimed %d reservations, want at least 1", released)
	}
	w, err := st.GetWorker(ctx, workerID)
	if err != nil {
		t.Fatal(err)
	}
	if w.State != store.WorkerDead {
		t.Errorf("state = %s, want DEAD", w.State)
	}
	if w.Committed.GetCpuMillis() != 0 {
		t.Errorf("committed cpu = %d after the sweep, want 0", w.Committed.GetCpuMillis())
	}
}

// TestSweeperLeavesLiveWorkersAlone: a false-positive sweep would expire a live
// worker's leases and duplicate its work.
func TestSweeperLeavesLiveWorkersAlone(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	workerID := registerWorker(t, s, st, vec(1000, 1<<30, 0))

	sw := NewSweeper(st, testLog())
	sw.LivenessWindow = time.Hour
	if _, _, err := func() (int, int, error) {
		d, r := sw.SweepOnce(ctx)
		return d, r, nil
	}(); err != nil {
		t.Fatal(err)
	}
	w, err := st.GetWorker(ctx, workerID)
	if err != nil {
		t.Fatal(err)
	}
	if w.State != store.WorkerActive {
		t.Errorf("a freshly-heartbeating worker was swept to %s", w.State)
	}
}

// TestSweeperIsSafeToRunConcurrently: multiple RM replicas sweep with no leader
// election. Correctness comes from the conditional UPDATE, not coordination.
func TestSweeperIsSafeToRunConcurrently(t *testing.T) {
	s, st := newTestServer(t)
	dropAttemptFK(t, st)
	ctx := context.Background()

	workerID := registerWorker(t, s, st, vec(4000, 4<<30, 0))
	if _, err := st.Reserve(ctx, workerID, newUUID(t, st), vec(1000, 1<<30, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx,
		`UPDATE workers SET last_heartbeat_at = now() - interval '5 minutes' WHERE id = $1`,
		workerID); err != nil {
		t.Fatal(err)
	}

	sw := NewSweeper(st, testLog())
	sw.LivenessWindow = 30 * time.Second

	type result struct{ dead, released int }
	results := make(chan result, 4)
	for i := 0; i < 4; i++ {
		go func() {
			d, r := sw.SweepOnce(ctx)
			results <- result{d, r}
		}()
	}
	totalDead, totalReleased := 0, 0
	for i := 0; i < 4; i++ {
		r := <-results
		totalDead += r.dead
		totalReleased += r.released
	}
	// Exactly one replica may win the transition, and therefore exactly one
	// performs the release.
	if totalDead != 1 {
		t.Errorf("total DEAD transitions = %d across 4 replicas, want exactly 1", totalDead)
	}
	if totalReleased != 1 {
		t.Errorf("total releases = %d, want exactly 1", totalReleased)
	}
	w, err := st.GetWorker(ctx, workerID)
	if err != nil {
		t.Fatal(err)
	}
	if w.Committed.GetCpuMillis() != 0 {
		t.Errorf("committed cpu = %d, want 0", w.Committed.GetCpuMillis())
	}
}

// TestDefaultsMatchTheArchitecture guards the §5.5 numbers against a careless edit.
func TestDefaultsMatchTheArchitecture(t *testing.T) {
	if DefaultLivenessWindow != 30*time.Second {
		t.Errorf("liveness window = %v, want 30s per §5.5", DefaultLivenessWindow)
	}
	// The reconcile grace must exceed a long poll plus a heartbeat, or
	// reconciliation races dispatch and cancels freshly scheduled work.
	if DefaultReconcileGrace <= 25*time.Second {
		t.Errorf("reconcile grace = %v; it must comfortably exceed one long poll (20s) "+
			"plus one heartbeat (5s), or the leak detector becomes a leak generator",
			DefaultReconcileGrace)
	}
}
