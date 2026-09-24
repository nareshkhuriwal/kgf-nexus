//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// These tests pin the WRITER half of the transactional outbox (migration 0016):
// every announced fact writes its outbox row in the same transaction, and only
// then. They need Postgres with 0016 applied and no NATS; the relay half is
// tested in services/event-service/relay against a real broker.
//
// Named TestNATS* so tests/nats/run-integration.sh can select them on a fresh
// database: the older fixtures in this package predate tenant registration
// (0014) and fail there for reasons unrelated to the outbox.

// outboxWorld is a tenant, spec and RUNNING run, built with real inserts so the
// tenant triggers and foreign keys are exercised rather than bypassed.
type outboxWorld struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	ns    string
	queue string
	runID uuid.UUID
}

func newOutboxWorld(t *testing.T) *outboxWorld {
	t.Helper()
	ctx := context.Background()
	pool := testPool(t)
	var one int
	if err := pool.QueryRow(ctx,
		`SELECT count(*)::int FROM information_schema.tables WHERE table_name = 'outbox'`).Scan(&one); err != nil || one != 1 {
		// Fatal, not Skip: a reachable database without the migration would
		// otherwise let every assertion below pass vacuously or fail confusingly.
		t.Fatalf("outbox table missing (migration 0016 not applied): count=%d err=%v", one, err)
	}
	id := uuid.NewString()[:8]
	w := &outboxWorld{t: t, ctx: ctx, pool: pool, ns: "ob-" + id, queue: "q-" + id}
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (namespace, name) VALUES ($1, $1)`, w.ns); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	var specID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
VALUES ($1,'wf',1,$2,'{}','test') RETURNING id`, w.ns, "hash-"+id).Scan(&specID); err != nil {
		t.Fatalf("insert spec: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO workflow_runs (spec_id, namespace, state, trigger_kind)
VALUES ($1,$2,'RUNNING','test') RETURNING id`, specID, w.ns).Scan(&w.runID); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM timers WHERE subject_id IN (SELECT id FROM task_attempts WHERE run_id = $1)`, w.runID)
		_, _ = pool.Exec(c, `DELETE FROM outbox WHERE namespace = $1 OR routing_key = $2
		                        OR ref_id IN (SELECT id FROM run_events WHERE run_id = $3)`, w.ns, w.runID.String(), w.runID)
		_, _ = pool.Exec(c, `DELETE FROM workflow_runs WHERE id = $1`, w.runID)
		_, _ = pool.Exec(c, `DELETE FROM workflow_specs WHERE id = $1`, specID)
		_, _ = pool.Exec(c, `DELETE FROM workers WHERE namespace = $1`, w.ns)
		_, _ = pool.Exec(c, `DELETE FROM tenants WHERE namespace = $1`, w.ns)
	})
	return w
}

func (w *outboxWorld) node(nodeID, state string) {
	w.t.Helper()
	if _, err := w.pool.Exec(w.ctx, `
INSERT INTO task_nodes (run_id, node_id, task_ref, state, spec_json)
VALUES ($1,$2,'test.task',$3,'{}')`, w.runID, nodeID, state); err != nil {
		w.t.Fatalf("insert node %s: %v", nodeID, err)
	}
}

func (w *outboxWorld) attempt(nodeID string) uuid.UUID {
	w.t.Helper()
	w.node(nodeID, "RUNNING")
	var id uuid.UUID
	if err := w.pool.QueryRow(w.ctx, `
INSERT INTO task_attempts (run_id, node_id, attempt_number, state, queue, resource_request, idempotency_key)
VALUES ($1,$2,1,'PENDING',$3,'{"cpu_millis":100}',$4) RETURNING id`,
		w.runID, nodeID, w.queue, "idem-"+nodeID+"-"+w.runID.String()).Scan(&id); err != nil {
		w.t.Fatalf("insert attempt: %v", err)
	}
	return id
}

func (w *outboxWorld) worker() uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.pool.QueryRow(w.ctx, `
INSERT INTO workers (namespace, hostname, queues, capacity, state, agent_version, session_token_hash, last_heartbeat_at)
VALUES ($1,$2,ARRAY[$3],'{"cpu_millis":8000,"memory_bytes":8589934592}','ACTIVE','test','hash',now())
RETURNING id`, w.ns, "host-"+uuid.NewString()[:6], w.queue).Scan(&id); err != nil {
		w.t.Fatalf("insert worker: %v", err)
	}
	return id
}

func (w *outboxWorld) count(q string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.pool.QueryRow(w.ctx, q, args...).Scan(&n); err != nil {
		w.t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// TestNATSOutboxClaimAnnouncesDispatchAtomically: the §5.3 claim commits exactly
// one outbox row, and it references exactly the AttemptDispatched event the claim
// wrote. This is the "only add the outbox insert" change to the claim, checked
// against the claim itself rather than a reimplementation of it.
func TestNATSOutboxClaimAnnouncesDispatchAtomically(t *testing.T) {
	w := newOutboxWorld(t)
	attemptID := w.attempt("n1")
	workerID := w.worker()

	st := NewSchedulerStore(w.pool, DefaultClaimConfig(w.queue), "outbox-it")
	asg, err := st.ClaimForWorker(w.ctx, time.Now().UTC(), WorkerClaimRequest{
		WorkerID: workerID, Queues: []string{w.queue}, MaxTasks: 1,
		FreeCapacity: &nexusv1.ResourceVector{CpuMillis: 8000, MemoryBytes: 8 << 30},
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(asg) != 1 || asg[0].AttemptID != attemptID {
		t.Fatalf("claim returned %+v, want exactly attempt %s", asg, attemptID)
	}

	n := w.count(`
SELECT count(*)::int FROM outbox o JOIN run_events e ON e.id = o.ref_id
 WHERE o.topic = 'run_event' AND e.attempt_id = $1 AND e.kind = $2`, attemptID, domain.EventAttemptDispatched)
	if n != 1 {
		t.Errorf("found %d outbox rows for the AttemptDispatched event, want exactly 1", n)
	}
	// And nothing the claim wrote is unannounced: every run event of this run
	// has exactly one outbox row.
	orphans := w.count(`
SELECT count(*)::int FROM run_events e
 WHERE e.run_id = $1
   AND (SELECT count(*) FROM outbox o WHERE o.topic = 'run_event' AND o.ref_id = e.id) <> 1`, w.runID)
	if orphans != 0 {
		t.Errorf("%d run events of the run do not have exactly one outbox row", orphans)
	}
}

// TestNATSOutboxRolledBackEventLeavesNoRow is the atomicity property itself: the
// row is visible inside the transaction that wrote the event, and gone when that
// transaction rolls back. The in-transaction read is the control -- without it,
// "zero rows afterwards" would also be the result of never writing one.
func TestNATSOutboxRolledBackEventLeavesNoRow(t *testing.T) {
	w := newOutboxWorld(t)
	tx, err := w.pool.Begin(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendEvent(w.ctx, tx, w.runID, "n1", nil, "TestEvent", "outbox-it", map[string]any{"x": 1}); err != nil {
		t.Fatalf("append: %v", err)
	}
	var outboxID int64
	if err := tx.QueryRow(w.ctx, `
SELECT o.id FROM outbox o JOIN run_events e ON e.id = o.ref_id WHERE e.run_id = $1`, w.runID).Scan(&outboxID); err != nil {
		t.Fatalf("control failed: the outbox row is not visible inside its own transaction: %v", err)
	}
	if err := tx.Rollback(w.ctx); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*)::int FROM outbox WHERE id = $1`, outboxID); n != 0 {
		t.Errorf("outbox row %d survived the rollback of the event it announces", outboxID)
	}
}

// TestNATSOutboxAttemptCreationAnnouncesTaskReady: the DAG engine's attempt insert
// writes a dispatch hint with the routing the agent subscribes on, and an insert
// that loses the ON CONFLICT race writes none.
func TestNATSOutboxAttemptCreationAnnouncesTaskReady(t *testing.T) {
	w := newOutboxWorld(t)
	w.node("n1", "PENDING")
	de := NewDAGEngineStore(w.pool)
	attempts := []NewAttempt{{
		RunID: w.runID.String(), NodeID: "n1", Queue: w.queue,
		Resources: &nexusv1.ResourceVector{CpuMillis: 100}, IdempotencyKey: "idem-" + w.runID.String(),
	}}
	res, err := de.ApplyNodeStateAndCreateAttempts(w.ctx, w.runID.String(),
		[]NodeTransition{{NodeID: "n1", From: domain.NodePending, To: domain.NodeReady}}, attempts)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.AttemptsCreated) != 1 {
		t.Fatalf("created %d attempts, want 1", len(res.AttemptsCreated))
	}
	created := res.AttemptsCreated[0].AttemptID

	var ns, key string
	var payload []byte
	if err := w.pool.QueryRow(w.ctx, `
SELECT namespace, routing_key, payload FROM outbox
 WHERE topic = 'task_ready' AND payload->>'attempt_id' = $1`, created).Scan(&ns, &key, &payload); err != nil {
		t.Fatalf("no task_ready row for the created attempt: %v", err)
	}
	if ns != w.ns || key != w.queue {
		t.Errorf("task_ready routed to %s/%s, want %s/%s", ns, key, w.ns, w.queue)
	}
	var body map[string]any
	_ = json.Unmarshal(payload, &body)
	if body["run_id"] != w.runID.String() {
		t.Errorf("payload = %s", payload)
	}

	// Replaying the same transition loses the node CAS and creates nothing, so it
	// must announce nothing.
	if _, err := de.ApplyNodeStateAndCreateAttempts(w.ctx, w.runID.String(),
		[]NodeTransition{{NodeID: "n1", From: domain.NodePending, To: domain.NodeReady}}, attempts); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n := w.count(`SELECT count(*)::int FROM outbox WHERE topic = 'task_ready' AND namespace = $1`, w.ns); n != 1 {
		t.Errorf("%d task_ready rows after a no-op replay, want 1", n)
	}
}

// TestNATSOutboxRunTransitionAnnouncesState: a run state change writes a run_state
// row carrying state_version, and a CAS miss writes nothing.
func TestNATSOutboxRunTransitionAnnouncesState(t *testing.T) {
	w := newOutboxWorld(t)
	if _, err := WFMTransitionRun(w.ctx, w.pool, w.runID.String(), []string{"RUNNING"}, "PAUSING"); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if _, err := WFMTransitionRun(w.ctx, w.pool, w.runID.String(), []string{"RUNNING"}, "PAUSING"); err == nil {
		t.Fatal("a stale CAS was accepted")
	}
	var payload []byte
	n := w.count(`SELECT count(*)::int FROM outbox WHERE topic = 'run_state' AND routing_key = $1`, w.runID.String())
	if n != 1 {
		t.Fatalf("%d run_state rows, want exactly 1 (the CAS miss must write none)", n)
	}
	if err := w.pool.QueryRow(w.ctx,
		`SELECT payload FROM outbox WHERE topic = 'run_state' AND routing_key = $1`, w.runID.String()).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(payload, &body)
	if body["state"] != "PAUSING" || body["state_version"] == nil || body["namespace"] != w.ns {
		t.Errorf("run_state payload = %s", payload)
	}
}

// TestNATSOutboxGrants pins 0016's grants in the shape of 0005's tests: the owner
// may read, mark and prune; writers may only INSERT; nobody else may write.
func TestNATSOutboxGrants(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	want := map[string]map[string]bool{
		"nexus_event_service":    {"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true},
		"nexus_scheduler":        {"SELECT": true, "INSERT": true},
		"nexus_dag_engine":       {"SELECT": true, "INSERT": true},
		"nexus_workflow_manager": {"SELECT": true, "INSERT": true},
		"nexus_resource_manager": {"SELECT": true},
		"nexus_api_gateway":      {"SELECT": true},
	}
	for role, privs := range want {
		for _, p := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
			var ok bool
			if err := pool.QueryRow(ctx, `SELECT has_table_privilege($1, 'outbox', $2)`, role, p).Scan(&ok); err != nil {
				t.Fatalf("check %s %s: %v", role, p, err)
			}
			if ok != privs[p] {
				t.Errorf("%s %s on outbox = %v, want %v", role, p, ok, privs[p])
			}
		}
		var seq bool
		if err := pool.QueryRow(ctx, `SELECT has_sequence_privilege($1, 'outbox_id_seq', 'USAGE')`, role).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		if seq != privs["INSERT"] {
			t.Errorf("%s USAGE on outbox_id_seq = %v, want %v (an INSERT without it fails at runtime)", role, seq, privs["INSERT"])
		}
	}
}

// TestNATSOutboxWritersCanActuallyInsertAsTheirRole: a grant check can pass while
// the statement still fails (a missing sequence grant, a CTE needing SELECT on a
// joined table). This runs the scheduler's real append as nexus_scheduler.
func TestNATSOutboxWritersCanActuallyInsertAsTheirRole(t *testing.T) {
	w := newOutboxWorld(t)
	tx, err := w.pool.Begin(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(w.ctx) }()
	if _, err := tx.Exec(w.ctx, `SET LOCAL ROLE nexus_scheduler`); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if err := appendEvent(w.ctx, tx, w.runID, "n1", nil, "TestEvent", "outbox-it", nil); err != nil {
		t.Fatalf("appendEvent as nexus_scheduler: %v", err)
	}
	if _, err := tx.Exec(w.ctx, `UPDATE outbox SET published_at = now()`); err == nil {
		t.Error("nexus_scheduler could mark outbox rows published; only the relay may")
	}
}
