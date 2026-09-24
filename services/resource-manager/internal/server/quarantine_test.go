// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
)

// qfix is a namespace with a run whose attempts can be attributed to workers.
type qfix struct {
	t     *testing.T
	st    *store.RMStore
	ns    string
	runID string
	n     int
}

func newQFix(t *testing.T, s *Server, st *store.RMStore) *qfix {
	t.Helper()
	ctx := context.Background()
	ns := fmt.Sprintf("q-%d", time.Now().UnixNano())
	ensureTenant(t, st, ns)
	specID, runID := uuid.NewString(), uuid.NewString()
	if _, err := st.Pool().Exec(ctx, `
		INSERT INTO workflow_specs (id, namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1::uuid, $2, 'q', 1, $3, '{}', 'test')`, specID, ns, specID); err != nil {
		t.Fatalf("seed spec: %v", err)
	}
	if _, err := st.Pool().Exec(ctx, `
		INSERT INTO workflow_runs (id, spec_id, namespace, state, trigger_kind)
		VALUES ($1, $2, $3, 'RUNNING', 'manual')`, runID, specID, ns); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = st.Pool().Exec(ctx, `DELETE FROM workflow_runs WHERE id = $1`, runID)
		_, _ = st.Pool().Exec(ctx, `DELETE FROM workflow_specs WHERE id = $1`, specID)
	})
	return &qfix{t: t, st: st, ns: ns, runID: runID}
}

func (f *qfix) worker(s *Server, host string) string {
	f.t.Helper()
	resp, err := s.RegisterWorker(context.Background(), &rmv1.RegisterWorkerRequest{
		Namespace: f.ns, Hostname: host, Queues: []string{"default"}, Executors: []string{"subprocess"},
		Capacity: vec(4000, 8<<30, 0), AgentVersion: "test", SessionTokenHash: "h",
	})
	if err != nil {
		f.t.Fatalf("RegisterWorker: %v", err)
	}
	return resp.GetWorkerId()
}

// attempt records one terminal attempt of a distinct task on a worker.
func (f *qfix) attempt(workerID, taskRef, state, class string) {
	f.t.Helper()
	ctx := context.Background()
	f.n++
	node := fmt.Sprintf("n%d", f.n)
	if _, err := f.st.Pool().Exec(ctx, `
		INSERT INTO task_nodes (run_id, node_id, task_ref, spec_json) VALUES ($1, $2, $3, '{}')`,
		f.runID, node, taskRef); err != nil {
		f.t.Fatalf("seed node: %v", err)
	}
	var errJSON any
	if class != "" {
		errJSON = fmt.Sprintf(`{"class":%q}`, class)
	}
	if _, err := f.st.Pool().Exec(ctx, `
		INSERT INTO task_attempts (run_id, node_id, attempt_number, state, resource_request,
		                           idempotency_key, worker_id, error, ended_at)
		VALUES ($1, $2, 1, $3::attempt_state, '{}', $2, $4, $5::jsonb, now())`,
		f.runID, node, state, workerID, errJSON); err != nil {
		f.t.Fatalf("seed attempt: %v", err)
	}
}

func stateOf(t *testing.T, st *store.RMStore, id string) store.WorkerState {
	t.Helper()
	w, err := st.GetWorker(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return w.State
}

func testDetector(st *store.RMStore) *QuarantineDetector {
	return NewQuarantineDetector(st, testLog(), DefaultQuarantineConfig())
}

func TestDetectorQuarantinesAPoisonWorkerOnly(t *testing.T) {
	s, st := newTestServer(t)
	f := newQFix(t, s, st)
	poison, healthy, buggyTask, spare := f.worker(s, "poison"), f.worker(s, "healthy"), f.worker(s, "buggy"), f.worker(s, "spare")

	// Infrastructure failures across three different tasks: worker-correlated.
	f.attempt(poison, "etl.a", "FAILED", "ERROR_CLASS_INFRASTRUCTURE")
	f.attempt(poison, "etl.b", "FAILED", "ERROR_CLASS_INFRASTRUCTURE")
	f.attempt(poison, "etl.c", "LOST", "ERROR_CLASS_WORKER_LOST")
	f.attempt(poison, "etl.d", "SUCCEEDED", "")
	// Application errors are the task's fault, never the worker's.
	for _, ref := range []string{"etl.a", "etl.b", "etl.c"} {
		f.attempt(healthy, ref, "FAILED", "ERROR_CLASS_APPLICATION")
	}
	// A poison TASK: infrastructure failures, but all from one task_ref.
	for i := 0; i < 4; i++ {
		f.attempt(buggyTask, "etl.poison", "FAILED", "ERROR_CLASS_INFRASTRUCTURE")
	}
	_ = spare

	q, _, err := testDetector(st).PassOnce(context.Background())
	if err != nil {
		t.Fatalf("PassOnce: %v", err)
	}
	if stateOf(t, st, poison) != store.WorkerQuarantined {
		t.Fatalf("poison worker not quarantined (quarantined %d)", q)
	}
	if stateOf(t, st, healthy) != store.WorkerActive || stateOf(t, st, buggyTask) != store.WorkerActive {
		t.Fatal("a worker was quarantined for failures that were not worker-correlated")
	}
	eps, _ := st.ListQuarantines(context.Background(), poison)
	if len(eps) != 1 || eps[0].Trigger != store.QuarantineAuto || eps[0].ExpiresAt == nil ||
		eps[0].Evidence["distinct_tasks"] == nil {
		t.Fatalf("episode = %+v", eps)
	}

	// Idempotent across passes / replicas: no second episode.
	if _, _, err := testDetector(st).PassOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if eps, _ := st.ListQuarantines(context.Background(), poison); len(eps) != 1 {
		t.Fatalf("episodes after a second pass = %d", len(eps))
	}
}

// A shared-dependency outage makes every worker look poisoned. The fleet cap
// keeps most of the fleet serving.
func TestDetectorRespectsTheFleetCap(t *testing.T) {
	s, st := newTestServer(t)
	f := newQFix(t, s, st)
	var ws []string
	for i := 0; i < 4; i++ {
		w := f.worker(s, fmt.Sprintf("h%d", i))
		ws = append(ws, w)
		for _, ref := range []string{"x", "y", "z"} {
			f.attempt(w, ref, "FAILED", "ERROR_CLASS_INFRASTRUCTURE")
		}
	}
	if _, _, err := testDetector(st).PassOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var held int
	for _, w := range ws {
		if stateOf(t, st, w) == store.WorkerQuarantined {
			held++
		}
	}
	if held != 2 {
		t.Fatalf("quarantined %d of 4 workers, want exactly 2 (max_fleet_fraction 0.5)", held)
	}
}

func TestDetectorNeverQuarantinesTheLastWorker(t *testing.T) {
	s, st := newTestServer(t)
	f := newQFix(t, s, st)
	w := f.worker(s, "only")
	for _, ref := range []string{"x", "y", "z"} {
		f.attempt(w, ref, "FAILED", "ERROR_CLASS_INFRASTRUCTURE")
	}
	if _, _, err := testDetector(st).PassOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stateOf(t, st, w) != store.WorkerActive {
		t.Fatal("the namespace's only worker was quarantined automatically")
	}
}

// Expiry releases the worker; the placement predicate (state = 'ACTIVE') then
// admits it again. Placement exclusion itself is asserted against the real
// claim in TestQuarantinedWorkerGetsNoDispatch.
func TestQuarantineExpires(t *testing.T) {
	s, st := newTestServer(t)
	f := newQFix(t, s, st)
	w := f.worker(s, "h")
	past := time.Now().Add(-time.Minute)
	if ok, err := st.QuarantineWorker(context.Background(), w, store.QuarantineAuto, "test", "test", nil, nil); !ok || err != nil {
		t.Fatalf("quarantine: %v %v", ok, err)
	}
	if _, err := st.Pool().Exec(context.Background(),
		`UPDATE worker_quarantines SET started_at = $2::timestamptz - interval '1 minute', expires_at = $2 WHERE worker_id = $1`, w, past); err != nil {
		t.Fatal(err)
	}
	_, released, err := testDetector(st).PassOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if released != 1 || stateOf(t, st, w) != store.WorkerActive {
		t.Fatalf("released=%d state=%s, want 1/ACTIVE", released, stateOf(t, st, w))
	}
	eps, _ := st.ListQuarantines(context.Background(), w)
	if eps[0].ReleasedBy == nil || *eps[0].ReleasedBy != "system:quarantine_expiry" {
		t.Fatalf("expiry not recorded: %+v", eps[0])
	}
}

func TestDurationEscalatesPerHost(t *testing.T) {
	d := testDetector(nil)
	if d.duration(0) != 15*time.Minute || d.duration(1) != 30*time.Minute || d.duration(20) != 24*time.Hour {
		t.Fatalf("durations: %v %v %v", d.duration(0), d.duration(1), d.duration(20))
	}
}

// The quarantined worker is excluded by the REAL dispatch claim, and admitted
// again after release.
func TestQuarantinedWorkerGetsNoDispatch(t *testing.T) {
	s, st := newTestServer(t)
	f := newQFix(t, s, st)
	w := f.worker(s, "h")
	f.worker(s, "other") // keep the fleet >1 so nothing is special-cased
	ctx := context.Background()
	if _, err := st.Pool().Exec(ctx, `UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, w); err != nil {
		t.Fatal(err)
	}
	f.n++
	node := fmt.Sprintf("n%d", f.n)
	if _, err := st.Pool().Exec(ctx, `INSERT INTO task_nodes (run_id, node_id, task_ref, spec_json) VALUES ($1,$2,'t','{}')`, f.runID, node); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `
		INSERT INTO task_attempts (run_id, node_id, attempt_number, state, queue, resource_request, idempotency_key, scheduled_at)
		VALUES ($1, $2, 1, 'PENDING', 'default', '{"cpu_millis":100}', $2, now() - interval '1 second')`, f.runID, node); err != nil {
		t.Fatal(err)
	}
	sched := store.NewSchedulerStore(st.Pool(), store.DefaultClaimConfig("default"), "quarantine-test")
	if _, err := s.SetWorkerState(ctx, &rmv1.SetWorkerStateRequest{
		WorkerId: w, State: rmv1.WorkerState_WORKER_STATE_QUARANTINED, Reason: "test", Actor: "user:t",
	}); err != nil {
		t.Fatal(err)
	}
	asg, err := sched.ClaimForWorker(ctx, time.Now(), store.WorkerClaimRequest{WorkerID: uuid.MustParse(w), MaxTasks: 1})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(asg) != 0 {
		t.Fatalf("a QUARANTINED worker was dispatched %d attempt(s)", len(asg))
	}
	if _, err := s.SetWorkerState(ctx, &rmv1.SetWorkerStateRequest{
		WorkerId: w, State: rmv1.WorkerState_WORKER_STATE_ACTIVE, Actor: "user:t",
	}); err != nil {
		t.Fatal(err)
	}
	asg, err = sched.ClaimForWorker(ctx, time.Now(), store.WorkerClaimRequest{WorkerID: uuid.MustParse(w), MaxTasks: 1})
	if err != nil || len(asg) != 1 {
		t.Fatalf("released worker claim: %d %v, want 1 dispatch", len(asg), err)
	}
}
