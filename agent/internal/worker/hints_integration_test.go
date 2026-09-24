//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

// Dispatch hints against a REAL NATS server and, for the claim, a REAL Postgres.
// Run through tests/nats/run-integration.sh.

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/bus"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

func natsURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("NEXUS_TEST_NATS_URL")
	if u == "" {
		t.Skip("skipping integration test: NEXUS_TEST_NATS_URL is not set (use tests/nats/run-integration.sh)")
	}
	return u
}

func publisher(t *testing.T) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(natsURL(t), nats.Timeout(3*time.Second))
	if err != nil {
		t.Skipf("skipping integration test: NATS unreachable: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func waitFor(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func subscribe(t *testing.T, ns string, queues []string, wake func()) *HintSubscription {
	t.Helper()
	h, err := SubscribeDispatchHints(bus.Config{URL: natsURL(t), Name: "agent-it"}, ns, queues, wake, testLog())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(h.Close)
	waitFor(t, "hint connection", 10*time.Second, h.Connected)
	if err := h.nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return h
}

// TestNATSHintsAreScopedToTheWorkersNamespaceAndQueues: a worker is woken by
// hints for its own queues in its own namespace and by nothing else. The positive
// hint at the end is the control that the subscription was live the whole time.
func TestNATSHintsAreScopedToTheWorkersNamespaceAndQueues(t *testing.T) {
	pub := publisher(t)
	ns := "hint-" + uuid.NewString()[:8]
	var wakes atomic.Int64
	subscribe(t, ns, []string{"default", "gpu"}, func() { wakes.Add(1) })

	for _, subj := range []string{
		bus.DispatchSubject(ns, "other"),               // a queue it does not serve
		bus.DispatchSubject(ns+"-x", "default"),        // another tenant, same queue
		bus.DispatchSubject("other-"+ns, "gpu"),        // another tenant
		bus.RunEventSubject(ns, uuid.NewString()),      // not a hint at all
		bus.DispatchSubject(ns+".default", "anything"), // dotted-namespace aliasing
	} {
		if err := pub.Publish(subj, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	_ = pub.Flush()
	time.Sleep(300 * time.Millisecond)
	if n := wakes.Load(); n != 0 {
		t.Fatalf("%d wakes from hints meant for other queues or tenants", n)
	}

	_ = pub.Publish(bus.DispatchSubject(ns, "gpu"), []byte(`{}`))
	_ = pub.Flush()
	waitFor(t, "a hint for the worker's own queue", 5*time.Second, func() bool { return wakes.Load() == 1 })
}

// TestNATSDuplicateAndStaleHintsCannotDoubleDispatch is the "hints are only
// hints" property with the real claim behind it. Two workers both subscribe; the
// same hint is delivered several times, plus a stale hint naming an attempt that
// does not exist, in no particular order. Each wake runs the real §5.3
// ClaimForWorker. Exactly one claim may win: the Postgres CAS, not the hint,
// decides.
func TestNATSDuplicateAndStaleHintsCannotDoubleDispatch(t *testing.T) {
	dsn := os.Getenv("NEXUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: NEXUS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		t.Skipf("skipping integration test: test database unreachable: %v", err)
	}
	defer pool.Close()

	id := uuid.NewString()[:8]
	ns, queue := "hintcas-"+id, "q-"+id
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO tenants (namespace, name) VALUES ($1,$1)`, ns)
	var specID, runID, attemptID string
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1,'wf',1,$2,'{}','t') RETURNING id::text`, ns, "h-"+id).Scan(&specID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_runs (spec_id, namespace, state, trigger_kind)
		VALUES ($1,$2,'RUNNING','t') RETURNING id::text`, specID, ns).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM timers WHERE subject_id IN (SELECT id FROM task_attempts WHERE run_id=$1::uuid)`, runID)
		_, _ = pool.Exec(c, `DELETE FROM outbox WHERE ref_id IN (SELECT id FROM run_events WHERE run_id=$1::uuid)`, runID)
		_, _ = pool.Exec(c, `DELETE FROM workflow_runs WHERE id=$1::uuid`, runID)
		_, _ = pool.Exec(c, `DELETE FROM workflow_specs WHERE id=$1::uuid`, specID)
		_, _ = pool.Exec(c, `DELETE FROM workers WHERE namespace=$1`, ns)
		_, _ = pool.Exec(c, `DELETE FROM tenants WHERE namespace=$1`, ns)
	}()
	mustExec(`INSERT INTO task_nodes (run_id, node_id, task_ref, state, spec_json) VALUES ($1::uuid,'n1','t','RUNNING','{}')`, runID)
	if err := pool.QueryRow(ctx, `INSERT INTO task_attempts (run_id, node_id, attempt_number, state, queue, resource_request, idempotency_key)
		VALUES ($1::uuid,'n1',1,'PENDING',$2,'{"cpu_millis":100}',$3) RETURNING id::text`, runID, queue, "k-"+id).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	var workers []uuid.UUID
	for i := 0; i < 2; i++ {
		var w uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO workers (namespace, hostname, queues, capacity, state, agent_version, session_token_hash, last_heartbeat_at)
			VALUES ($1,$2,ARRAY[$3],'{"cpu_millis":8000}','ACTIVE','t','h',now()) RETURNING id`,
			ns, "host-"+uuid.NewString()[:6], queue).Scan(&w); err != nil {
			t.Fatal(err)
		}
		workers = append(workers, w)
	}

	st := store.NewSchedulerStore(pool, store.DefaultClaimConfig(queue), "hint-it")
	var (
		mu       sync.Mutex
		claimed  []string
		claimErr error
		wakes    atomic.Int64
		wg       sync.WaitGroup
	)
	for _, w := range workers {
		w := w
		subscribe(t, ns, []string{queue}, func() {
			wakes.Add(1)
			wg.Add(1)
			// A hint only makes the worker ask; the claim decides.
			go func() {
				defer wg.Done()
				asg, err := st.ClaimForWorker(ctx, time.Now().UTC(), store.WorkerClaimRequest{
					WorkerID: w, Queues: []string{queue}, MaxTasks: 1,
					FreeCapacity: &nexusv1.ResourceVector{CpuMillis: 8000},
				})
				mu.Lock()
				defer mu.Unlock()
				if err != nil && claimErr == nil {
					claimErr = err
				}
				for _, a := range asg {
					claimed = append(claimed, a.AttemptID.String())
				}
			}()
		})
	}

	pub := publisher(t)
	subject := bus.DispatchSubject(ns, queue)
	stale := []byte(`{"attempt_id":"` + uuid.NewString() + `"}`)
	real := []byte(`{"attempt_id":"` + attemptID + `"}`)
	for _, body := range [][]byte{stale, real, real, stale, real, real, real} {
		if err := pub.Publish(subject, body); err != nil {
			t.Fatal(err)
		}
	}
	_ = pub.Flush()
	// Control: every hint reached both workers, so both really tried to claim.
	waitFor(t, "14 wakes (7 hints x 2 workers)", 10*time.Second, func() bool { return wakes.Load() == 14 })
	wg.Wait()

	if claimErr != nil {
		t.Fatalf("a claim errored: %v", claimErr)
	}
	if len(claimed) != 1 || claimed[0] != attemptID {
		t.Fatalf("claims = %v, want exactly one claim of %s across both workers and all hints", claimed, attemptID)
	}
	var leases int
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM leases WHERE attempt_id=$1::uuid`, attemptID).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if leases != 1 {
		t.Errorf("%d leases for the attempt, want 1", leases)
	}
}
