//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
)

// newTestPool connects to the live test database, skipping rather than failing
// when it is unreachable. A developer with no Postgres running should get a skip,
// not a wall of connection errors that hides the unit-test result they care about.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := store.NewPool(ctx, store.DefaultPoolConfig(dsn))
	if err != nil {
		testdb.SkipUnreachable(t, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fixture builds an isolated world for one test.
//
// Isolation is by a unique namespace and queue per test rather than by
// truncating tables, so tests can run in parallel against one shared database
// and a failed test leaves its rows behind for inspection.
type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	ns   string
	// queue is unique per test: it scopes every scheduler query, so two tests
	// running concurrently cannot steal each other's attempts.
	queue  string
	specID uuid.UUID
	runID  uuid.UUID
}

func newFixture(t *testing.T, pool *pgxpool.Pool) *fixture {
	t.Helper()
	return newFixtureOnQueue(t, pool, "")
}

// newFixtureOnQueue is newFixture with a caller-chosen queue, so several
// namespaces -- each its own fixture, tenant, run and workers -- can share ONE
// queue. That sharing is the whole subject of the fair-share tests: namespaces
// on different queues never compete for a claim. An empty queue gets a unique
// one, which is what every single-namespace test wants.
func newFixtureOnQueue(t *testing.T, pool *pgxpool.Pool, queue string) *fixture {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()[:8]
	if queue == "" {
		queue = "q-" + id
	}
	f := &fixture{
		t: t, pool: pool,
		ns:    "test-" + id,
		queue: queue,
	}
	testdb.RegisterTenant(t, pool, f.ns)

	// Migration 0015 makes tenant ownership mandatory: a namespace with no
	// tenants row is rejected by the ownership trigger. The fixture registers
	// its own throwaway tenant so the suite runs against a freshly migrated
	// database rather than only against one seeded before 0015 landed.
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (namespace, name) VALUES ($1,$1) ON CONFLICT (namespace) DO NOTHING`,
		f.ns); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
VALUES ($1,'wf',1,$2,'{}','test') RETURNING id`,
		f.ns, "hash-"+id).Scan(&f.specID); err != nil {
		t.Fatalf("insert spec: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO workflow_runs (spec_id, namespace, state, trigger_kind)
VALUES ($1,$2,'RUNNING','test') RETURNING id`,
		f.specID, f.ns).Scan(&f.runID); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	t.Cleanup(func() {
		// ON DELETE CASCADE from workflow_runs removes nodes, attempts, leases,
		// reservations, events and slots. Workers are namespaced separately.
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(cctx, `DELETE FROM timers WHERE subject_id IN
			(SELECT id FROM task_attempts WHERE run_id = $1)`, f.runID)
		_, _ = pool.Exec(cctx, `DELETE FROM workflow_runs WHERE id = $1`, f.runID)
		_, _ = pool.Exec(cctx, `DELETE FROM workflow_specs WHERE id = $1`, f.specID)
		_, _ = pool.Exec(cctx, `DELETE FROM workers WHERE namespace = $1`, f.ns)
		_, _ = pool.Exec(cctx, `DELETE FROM platform_config WHERE namespace = $1`, f.ns)
		_, _ = pool.Exec(cctx, `DELETE FROM tenants WHERE namespace = $1`, f.ns)
	})
	return f
}

type vec struct {
	CPUMillis      int64            `json:"cpu_millis"`
	MemoryBytes    int64            `json:"memory_bytes"`
	GPUCount       int32            `json:"gpu_count"`
	GPUKind        string           `json:"gpu_kind"`
	GPUMemoryBytes int64            `json:"gpu_memory_bytes"`
	DiskBytes      int64            `json:"disk_bytes"`
	Custom         map[string]int64 `json:"custom"`
	// Fractional GPUs (ADR-0029), omitted when zero exactly as the store
	// writes them.
	Mig                 map[string]int64      `json:"mig,omitempty"`
	GPUShareMilli       int32                 `json:"gpu_share_milli,omitempty"`
	GPUShareMemoryBytes int64                 `json:"gpu_share_memory_bytes,omitempty"`
	GPUShares           map[string]shareEntry `json:"gpu_shares,omitempty"`
}

type shareEntry struct {
	Milli       int64 `json:"milli"`
	MemoryBytes int64 `json:"memory_bytes"`
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// addWorker registers an ACTIVE worker with a fresh heartbeat.
func (f *fixture) addWorker(capacity vec, labels map[string]string, executors []string) uuid.UUID {
	f.t.Helper()
	if labels == nil {
		labels = map[string]string{}
	}
	if executors == nil {
		executors = []string{}
	}
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `
INSERT INTO workers (namespace, hostname, queues, executors, labels, capacity, committed,
                     state, agent_version, session_token_hash, last_heartbeat_at)
VALUES ($1,$2,ARRAY[$3],$4,$5,$6,'{}','ACTIVE','test','hash',now())
RETURNING id`,
		f.ns, "host-"+uuid.NewString()[:6], f.queue, executors,
		mustJSON(f.t, labels), mustJSON(f.t, capacity),
	).Scan(&id); err != nil {
		f.t.Fatalf("insert worker: %v", err)
	}
	return id
}

// nodeSpec is the subset of a task spec these tests exercise, written into
// task_nodes.spec_json in the same snake_case shape the scheduler reads.
type nodeSpec struct {
	TaskRef           string            `json:"task_ref"`
	Version           int32             `json:"version"`
	NodeSelector      map[string]string `json:"node_selector,omitempty"`
	Executor          string            `json:"executor,omitempty"`
	ScheduleToStartMs int64             `json:"schedule_to_start_ms,omitempty"`
	StartToCloseMs    int64             `json:"start_to_close_ms,omitempty"`
	ScheduleToCloseMs int64             `json:"schedule_to_close_ms,omitempty"`
	MaxAttempts       int32             `json:"max_attempts,omitempty"`
	Idempotency       string            `json:"idempotency,omitempty"`
	ConcurrencyKey    string            `json:"concurrency_key,omitempty"`
	ConcurrencyLimit  int32             `json:"concurrency_limit,omitempty"`
	Retry             map[string]any    `json:"retry,omitempty"`
	// ADR-0030/0031, in the protojson-with-proto-names shape the DAG engine
	// writes.
	Placement          map[string]any `json:"placement,omitempty"`
	PreferSameWorkerAs string         `json:"prefer_same_worker_as,omitempty"`
	// Image is runtime.image in the flat dialect; Runtime is the DAG
	// engine's nested runtime block (runtime.executor, runtime.image), the
	// dialect production specs arrive in.
	Image   string         `json:"image,omitempty"`
	Runtime map[string]any `json:"runtime,omitempty"`
}

// addAttempt creates a node and its first PENDING attempt.
func (f *fixture) addAttempt(nodeID string, priority int32, req vec, spec nodeSpec, scheduledAt time.Time) uuid.UUID {
	f.t.Helper()
	ctx := context.Background()
	maxAttempts := spec.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 3
	}
	if _, err := f.pool.Exec(ctx, `
INSERT INTO task_nodes (run_id, node_id, task_ref, state, max_attempts, spec_json)
VALUES ($1,$2,$3,'RUNNING',$4,$5)`,
		f.runID, nodeID, "test.task", maxAttempts, mustJSON(f.t, spec)); err != nil {
		f.t.Fatalf("insert node %s: %v", nodeID, err)
	}

	var id uuid.UUID
	if err := f.pool.QueryRow(ctx, `
INSERT INTO task_attempts (run_id, node_id, attempt_number, state, queue, priority,
                           resource_request, idempotency_key, scheduled_at)
VALUES ($1,$2,1,'PENDING',$3,$4,$5,$6,$7) RETURNING id`,
		f.runID, nodeID, f.queue, priority, mustJSON(f.t, req),
		fmt.Sprintf("idem-%s-%s", f.runID, nodeID), scheduledAt,
	).Scan(&id); err != nil {
		f.t.Fatalf("insert attempt %s: %v", nodeID, err)
	}
	return id
}

func (f *fixture) attemptState(id uuid.UUID) string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT state FROM task_attempts WHERE id = $1`, id).Scan(&s); err != nil {
		f.t.Fatalf("read attempt state: %v", err)
	}
	return s
}

func (f *fixture) pendingReason(id uuid.UUID) string {
	f.t.Helper()
	var s *string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT pending_reason FROM task_attempts WHERE id = $1`, id).Scan(&s); err != nil {
		f.t.Fatalf("read pending_reason: %v", err)
	}
	if s == nil {
		return ""
	}
	return *s
}

func (f *fixture) workerCommitted(id uuid.UUID) vec {
	f.t.Helper()
	var b []byte
	if err := f.pool.QueryRow(context.Background(),
		`SELECT committed FROM workers WHERE id = $1`, id).Scan(&b); err != nil {
		f.t.Fatalf("read worker committed: %v", err)
	}
	var v vec
	if err := json.Unmarshal(b, &v); err != nil {
		f.t.Fatalf("decode committed: %v", err)
	}
	return v
}

func (f *fixture) newStore(cfg func(*store.ClaimConfig)) *store.SchedulerStore {
	f.t.Helper()
	c := store.DefaultClaimConfig(f.queue)
	if cfg != nil {
		cfg(&c)
	}
	return store.NewSchedulerStore(f.pool, c, "test-"+uuid.NewString()[:8])
}

// occupy simulates an already-running attempt holding capacity on a worker: a
// RUNNING attempt, its reservation, and the matching increment to
// workers.committed, exactly as a real dispatch would have left them.
//
// Tests need this because the interesting scheduling scenarios (§7.3
// contention, best-fit packing) only exist on a PARTIALLY loaded cluster. An
// empty cluster places everything trivially and proves nothing.
func (f *fixture) occupy(workerID uuid.UUID, nodeID string, req vec) uuid.UUID {
	f.t.Helper()
	ctx := context.Background()
	attemptID := f.addAttempt(nodeID, 100, req, nodeSpec{TaskRef: "test.task", Version: 1}, time.Now().UTC())

	if _, err := f.pool.Exec(ctx,
		`UPDATE task_attempts SET state='RUNNING', worker_id=$2, dispatched_at=now(), started_at=now()
		 WHERE id=$1`, attemptID, workerID); err != nil {
		f.t.Fatalf("occupy: set running: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO reservations (worker_id, attempt_id, resources) VALUES ($1,$2,$3)`,
		workerID, attemptID, mustJSON(f.t, req)); err != nil {
		f.t.Fatalf("occupy: insert reservation: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
UPDATE workers SET committed = jsonb_build_object(
        'cpu_millis',        COALESCE((committed->>'cpu_millis')::bigint,0)        + $2::bigint,
        'memory_bytes',      COALESCE((committed->>'memory_bytes')::bigint,0)      + $3::bigint,
        'gpu_count',         COALESCE((committed->>'gpu_count')::int,0)            + $4::int,
        'disk_bytes',        COALESCE((committed->>'disk_bytes')::bigint,0)        + $5::bigint,
        'gpu_kind',          COALESCE(committed->>'gpu_kind',''),
        'gpu_memory_bytes',  COALESCE((committed->>'gpu_memory_bytes')::bigint,0),
        'custom',            COALESCE(committed->'custom','{}'::jsonb))
 WHERE id = $1`,
		workerID, req.CPUMillis, req.MemoryBytes, req.GPUCount, req.DiskBytes); err != nil {
		f.t.Fatalf("occupy: bump committed: %v", err)
	}
	return attemptID
}

// leaseFor returns the lease row for an attempt, if any.
func (f *fixture) leaseFor(attemptID uuid.UUID) (token string, expiresAt time.Time, ok bool) {
	f.t.Helper()
	err := f.pool.QueryRow(context.Background(),
		`SELECT token, expires_at FROM leases WHERE attempt_id = $1`, attemptID).
		Scan(&token, &expiresAt)
	if err != nil {
		return "", time.Time{}, false
	}
	return token, expiresAt, true
}

// expireLease backdates a lease so the reconciler sees it as expired, without
// making the test sleep for a real TTL.
func (f *fixture) expireLease(attemptID uuid.UUID) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE leases SET expires_at = now() - interval '1 second' WHERE attempt_id = $1`,
		attemptID); err != nil {
		f.t.Fatalf("expire lease: %v", err)
	}
}

// staleHeartbeat backdates a worker's heartbeat past the liveness window.
func (f *fixture) staleHeartbeat(workerID uuid.UUID, age time.Duration) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE workers SET last_heartbeat_at = now() - $2::interval WHERE id = $1`,
		workerID, age.String()); err != nil {
		f.t.Fatalf("stale heartbeat: %v", err)
	}
}

func (f *fixture) workerState(workerID uuid.UUID) string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT state FROM workers WHERE id = $1`, workerID).Scan(&s); err != nil {
		f.t.Fatalf("read worker state: %v", err)
	}
	return s
}

// attemptsForNode returns every attempt row for a node ordered by attempt
// number, so a test can assert that a retry CREATED a new row instead of
// mutating the terminal one (invariant 9).
func (f *fixture) attemptsForNode(nodeID string) []struct {
	ID      uuid.UUID
	Number  int32
	State   string
	IdemKey string
} {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `
SELECT id, attempt_number, state, idempotency_key
  FROM task_attempts WHERE run_id = $1 AND node_id = $2
 ORDER BY attempt_number`, f.runID, nodeID)
	if err != nil {
		f.t.Fatalf("list attempts: %v", err)
	}
	defer rows.Close()
	var out []struct {
		ID      uuid.UUID
		Number  int32
		State   string
		IdemKey string
	}
	for rows.Next() {
		var r struct {
			ID      uuid.UUID
			Number  int32
			State   string
			IdemKey string
		}
		if err := rows.Scan(&r.ID, &r.Number, &r.State, &r.IdemKey); err != nil {
			f.t.Fatalf("scan attempt: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// eventKinds returns the run_events kinds recorded for an attempt, in order.
func (f *fixture) eventKinds(attemptID uuid.UUID) []string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT kind FROM run_events WHERE attempt_id = $1 ORDER BY seq`, attemptID)
	if err != nil {
		f.t.Fatalf("list events: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			f.t.Fatalf("scan event: %v", err)
		}
		out = append(out, k)
	}
	return out
}

// timersFor returns the kinds of timers pending for a subject.
func (f *fixture) timersFor(subjectID uuid.UUID) []string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT kind FROM timers WHERE subject_id = $1 ORDER BY kind`, subjectID)
	if err != nil {
		f.t.Fatalf("list timers: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			f.t.Fatalf("scan timer: %v", err)
		}
		out = append(out, k)
	}
	return out
}
