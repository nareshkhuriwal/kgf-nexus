//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
)

// These tests pin ADR-0021's claim-side guarantee: a namespace's concurrent
// quota holds under any number of scheduler replicas on either claim path, a
// held-back attempt says why, and a namespace without quotas is unaffected.
//
// Mutation check (F89), done by hand and recorded in the ADR: with the advisory
// lock removed from lockQuotaForWorker and tryLockQuota (usage read without it),
// TestQuotaHoldsUnderConcurrentReplicas exceeds the in-flight quota on every
// run. A quota test that passes without the lock would prove nothing.

// setQuotas writes the namespace's quotas exactly where the quotas API stores
// them, so the tests exercise the namespace_quota_limits view.
func (f *fixture) setQuotas(q map[string]any) {
	f.t.Helper()
	b, err := json.Marshal(q)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `
INSERT INTO platform_config (namespace, revision, config)
VALUES ($1, 1, jsonb_build_object('quotas', $2::jsonb))
ON CONFLICT (namespace) DO UPDATE SET config = jsonb_set(platform_config.config, '{quotas}', $2::jsonb)`,
		f.ns, b); err != nil {
		f.t.Fatalf("set quotas: %v", err)
	}
}

// inflight reads the namespace's in-flight count and CPU sum in ONE statement,
// i.e. from one snapshot, so a sample can never mix two moments.
func (f *fixture) inflight() (n, cpu int64) {
	f.t.Helper()
	if err := f.pool.QueryRow(context.Background(), `
SELECT count(*), COALESCE(sum((a.resource_request->>'cpu_millis')::bigint), 0)
  FROM task_attempts a JOIN workflow_runs r ON r.id = a.run_id
 WHERE r.namespace = $1 AND a.state IN ('DISPATCHED','RUNNING')`, f.ns).Scan(&n, &cpu); err != nil {
		f.t.Fatalf("read in-flight: %v", err)
	}
	return n, cpu
}

func (f *fixture) ledgerRows(attemptID uuid.UUID) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM usage_ledger WHERE attempt_id = $1`, attemptID).Scan(&n); err != nil {
		f.t.Fatalf("count ledger rows: %v", err)
	}
	return n
}

func succeed(t *testing.T, st *store.SchedulerStore, id uuid.UUID, m *nexusv1.ExecutionMetrics) store.AppliedResult {
	t.Helper()
	res, err := st.ApplyResult(context.Background(), time.Now().UTC(), &nexusv1.ReportResultRequest{
		AttemptId: id.String(),
		Outcome:   nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
		Metrics:   m,
	})
	if err != nil {
		t.Fatalf("ApplyResult %s: %v", id, err)
	}
	return res
}

// TestQuotaHoldsUnderConcurrentReplicas is the atomicity proof. 4 push-path and
// 4 pull-path replicas, each with its own connection pool, race to dispatch 200
// attempts of one namespace across 16 roomy workers while a completer finishes
// dispatched work at random, so the namespace keeps hitting its limits from
// below and above. The quota is 5 in flight and 2,000 CPU millis with mixed
// 300/700-millicore requests, so both dimensions bind. A sampler reads the
// in-flight totals continuously; neither may ever exceed its limit.
func TestQuotaHoldsUnderConcurrentReplicas(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	f.setQuotas(map[string]any{"max_inflight_attempts": 5, "max_cpu_millis": 2000})

	var workers []uuid.UUID
	for i := 0; i < 16; i++ {
		workers = append(workers, f.addWorker(vec{CPUMillis: 64000, MemoryBytes: 1 << 40}, nil, nil))
	}
	now := time.Now().UTC().Add(-time.Minute)
	var all []uuid.UUID
	for i := 0; i < 200; i++ {
		cpu := int64(300)
		if i%3 == 0 {
			cpu = 700
		}
		all = append(all, f.addAttempt(nodeName("q", i), 100, vec{CPUMillis: cpu, MemoryBytes: 1 << 20},
			nodeSpec{TaskRef: "test.task", Version: 1}, now))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var (
		wg         sync.WaitGroup
		violations atomic.Int64
		maxN       atomic.Int64
		maxCPU     atomic.Int64
		completed  atomic.Int64
		errs       = make(chan error, 64)
	)
	stop := make(chan struct{})
	var blocked countingObserver

	// Sampler.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, cpu := f.inflight()
			if n > 5 || cpu > 2000 {
				if violations.Add(1) <= 5 {
					t.Errorf("quota exceeded: %d in flight, %d CPU millis", n, cpu)
				}
			}
			for cur := maxN.Load(); n > cur && !maxN.CompareAndSwap(cur, n); cur = maxN.Load() {
			}
			for cur := maxCPU.Load(); cpu > cur && !maxCPU.CompareAndSwap(cur, cpu); cur = maxCPU.Load() {
			}
		}
	}()

	replica := func(r int, pull bool) {
		defer wg.Done()
		pc := store.DefaultPoolConfig(testdb.DSN(t))
		pc.MaxConns, pc.MinConns = 2, 1
		p, err := store.NewPool(ctx, pc)
		if err != nil {
			errs <- err
			return
		}
		defer p.Close()
		cfg := store.DefaultClaimConfig(f.queue)
		st := store.NewSchedulerStore(p, cfg, fmt.Sprintf("quota-%d", r))
		st.SetObserver(&blocked)
		rng := rand.New(rand.NewSource(int64(r)))
		for completed.Load() < int64(len(all)) && ctx.Err() == nil {
			var err error
			if pull {
				_, err = st.ClaimForWorker(ctx, time.Now().UTC(), store.WorkerClaimRequest{
					WorkerID: workers[rng.Intn(len(workers))], MaxTasks: 2})
			} else {
				_, err = st.Claim(ctx, time.Now().UTC())
			}
			if err != nil {
				errs <- fmt.Errorf("replica %d: %w", r, err)
				return
			}
		}
	}
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go replica(r, r%2 == 0)
	}

	// Completer: finishes dispatched attempts, one at a time, at random.
	wg.Add(1)
	go func() {
		defer wg.Done()
		st := f.newStore(nil)
		for completed.Load() < int64(len(all)) && ctx.Err() == nil {
			rows, err := pool.Query(ctx, `
SELECT a.id FROM task_attempts a JOIN workflow_runs r ON r.id = a.run_id
 WHERE r.namespace = $1 AND a.state = 'DISPATCHED' ORDER BY random() LIMIT 1`, f.ns)
			if err != nil {
				errs <- err
				return
			}
			var ids []uuid.UUID
			for rows.Next() {
				var id uuid.UUID
				_ = rows.Scan(&id)
				ids = append(ids, id)
			}
			rows.Close()
			for _, id := range ids {
				if res := succeed(t, st, id, nil); res.Applied {
					completed.Add(1)
				}
			}
			// Slower than eight claimers on purpose: the namespace must sit at
			// its limit most of the time, or the test proves nothing.
			time.Sleep(3 * time.Millisecond)
		}
	}()

	// Stop the sampler once every attempt is done (or the deadline passes).
	for completed.Load() < int64(len(all)) && ctx.Err() == nil {
		time.Sleep(20 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatalf("timed out with %d of %d attempts completed", completed.Load(), len(all))
	}
	if violations.Load() > 0 {
		t.Fatalf("%d samples exceeded the quota", violations.Load())
	}
	// The quota was actually reached, so the test exercised the boundary.
	if maxN.Load() < 3 {
		t.Errorf("peak in flight was %d; the test never pressed against the quota", maxN.Load())
	}
	if blocked.quota.Load() == 0 {
		t.Error("no claim was ever held back by the quota; the test never pressed against it")
	}
	t.Logf("peak in flight %d (quota 5), peak CPU %d (quota 2000), %d completed, %d quota holds",
		maxN.Load(), maxCPU.Load(), completed.Load(), blocked.quota.Load())

	// Every completed attempt was metered exactly once.
	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM usage_ledger WHERE namespace = $1`, f.ns).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != len(all) {
		t.Errorf("usage ledger has %d rows, want %d (one per completed attempt)", rows, len(all))
	}
}

// TestQuotaBlockedAttemptExplainsItself: on both paths, an attempt held back by
// a quota stays PENDING with the documented reason, and runs once usage drops.
func TestQuotaBlockedAttemptExplainsItself(t *testing.T) {
	pool := newTestPool(t)
	for _, path := range []string{"pull", "push"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture(t, pool)
			f.setQuotas(map[string]any{"max_inflight_attempts": 1})
			w := f.addWorker(vec{CPUMillis: 8000, MemoryBytes: 1 << 34}, nil, nil)
			now := time.Now().UTC().Add(-time.Minute)
			a := f.addAttempt("a", 200, vec{CPUMillis: 100}, nodeSpec{TaskRef: "t", Version: 1}, now)
			b := f.addAttempt("b", 100, vec{CPUMillis: 100}, nodeSpec{TaskRef: "t", Version: 1}, now)
			st := f.newStore(nil)
			claim := func() {
				t.Helper()
				var err error
				if path == "pull" {
					_, err = st.ClaimForWorker(context.Background(), time.Now().UTC(),
						store.WorkerClaimRequest{WorkerID: w, MaxTasks: 4})
				} else {
					for i := 0; i < 2; i++ {
						if _, err = st.Claim(context.Background(), time.Now().UTC()); err != nil {
							break
						}
					}
				}
				if err != nil {
					t.Fatalf("claim: %v", err)
				}
			}
			claim()
			if got := f.attemptState(a); got != "DISPATCHED" {
				t.Fatalf("first attempt %s, want DISPATCHED", got)
			}
			if got := f.attemptState(b); got != "PENDING" {
				t.Fatalf("second attempt %s, want PENDING (quota 1)", got)
			}
			if got, want := f.pendingReason(b), "quota_exceeded: inflight_attempts (used 1 of 1)"; got != want {
				t.Fatalf("pending_reason = %q, want %q", got, want)
			}
			succeed(t, st, a, nil)
			claim()
			if got := f.attemptState(b); got != "DISPATCHED" {
				t.Fatalf("after the first finished, second is %s, want DISPATCHED", got)
			}
		})
	}
}

// TestQuotaDimensionsAndOversizeRequest: CPU, GPU and custom limits each block
// with their own dimension, and a single request larger than the quota says it
// can never run as configured.
func TestQuotaDimensionsAndOversizeRequest(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	f.setQuotas(map[string]any{
		"max_cpu_millis": 1000, "max_gpu_count": 1, "max_custom": map[string]any{"license": 1},
	})
	w := f.addWorker(vec{CPUMillis: 64000, MemoryBytes: 1 << 36, GPUCount: 8, Custom: map[string]int64{"license": 10}}, nil, nil)
	now := time.Now().UTC().Add(-time.Minute)
	spec := nodeSpec{TaskRef: "t", Version: 1}
	big := f.addAttempt("big", 500, vec{CPUMillis: 4000}, spec, now)
	gpu1 := f.addAttempt("gpu1", 400, vec{CPUMillis: 100, GPUCount: 1}, spec, now)
	gpu2 := f.addAttempt("gpu2", 300, vec{CPUMillis: 100, GPUCount: 1}, spec, now)
	lic1 := f.addAttempt("lic1", 200, vec{CPUMillis: 100, Custom: map[string]int64{"license": 1}}, spec, now)
	lic2 := f.addAttempt("lic2", 100, vec{CPUMillis: 100, Custom: map[string]int64{"license": 1}}, spec, now)

	st := f.newStore(nil)
	if _, err := st.ClaimForWorker(context.Background(), time.Now().UTC(),
		store.WorkerClaimRequest{WorkerID: w, MaxTasks: 8}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[uuid.UUID]string{gpu1: "DISPATCHED", lic1: "DISPATCHED", big: "PENDING", gpu2: "PENDING", lic2: "PENDING"} {
		if got := f.attemptState(id); got != want {
			t.Errorf("attempt %s: %s, want %s", id, got, want)
		}
	}
	if r := f.pendingReason(big); r != "quota_exceeded: cpu_millis (used 0 of 1000, request 4000 exceeds the quota)" &&
		!strings.HasPrefix(r, "quota_exceeded: cpu_millis") {
		t.Errorf("oversize reason %q", r)
	} else if !strings.Contains(r, "request 4000 exceeds the quota") {
		t.Errorf("oversize reason does not say it can never run: %q", r)
	}
	if r := f.pendingReason(gpu2); r != "quota_exceeded: gpu_count (used 1 of 1)" {
		t.Errorf("gpu reason %q", r)
	}
	if r := f.pendingReason(lic2); r != "quota_exceeded: custom:license (used 1 of 1)" {
		t.Errorf("custom reason %q", r)
	}
}

// TestNoQuotasMeansUnlimited: a namespace with no quota configured, and one
// whose policy has quotas all zero, dispatch exactly as before ADR-0021.
func TestNoQuotasMeansUnlimited(t *testing.T) {
	pool := newTestPool(t)
	for _, set := range []bool{false, true} {
		f := newFixture(t, pool)
		if set {
			f.setQuotas(map[string]any{"max_inflight_attempts": 0, "max_cpu_millis": 0, "max_active_runs": 0})
		}
		w := f.addWorker(vec{CPUMillis: 64000, MemoryBytes: 1 << 36}, nil, nil)
		now := time.Now().UTC().Add(-time.Minute)
		var ids []uuid.UUID
		for i := 0; i < 6; i++ {
			ids = append(ids, f.addAttempt(nodeName("u", i), 100, vec{CPUMillis: 100}, nodeSpec{TaskRef: "t", Version: 1}, now))
		}
		asg, err := f.newStore(nil).ClaimForWorker(context.Background(), time.Now().UTC(),
			store.WorkerClaimRequest{WorkerID: w, MaxTasks: 6})
		if err != nil {
			t.Fatal(err)
		}
		if len(asg) != 6 {
			t.Errorf("quotas set=%v: dispatched %d of 6", set, len(asg))
		}
	}
}

// ── Metering ──────────────────────────────────────────────────────────────

type ledgerRow struct {
	Outcome                        string
	WorkerID                       *uuid.UUID
	CPUMillis                      int64
	Reserved, CPUCore, MemGiB, GPU float64
	MeasuredCPU                    *float64
	MeasuredPeak                   *int64
	Pool                           *string
	Queue                          string
	Executor                       *string
}

func (f *fixture) ledger(attemptID uuid.UUID) ledgerRow {
	f.t.Helper()
	var r ledgerRow
	if err := f.pool.QueryRow(context.Background(), `
SELECT outcome, worker_id, cpu_millis, reserved_seconds::float8, cpu_core_seconds::float8,
       memory_gib_seconds::float8, gpu_seconds::float8, measured_cpu_seconds,
       measured_peak_memory_bytes, pool, queue, executor
  FROM usage_ledger WHERE attempt_id = $1`, attemptID).Scan(&r.Outcome, &r.WorkerID, &r.CPUMillis,
		&r.Reserved, &r.CPUCore, &r.MemGiB, &r.GPU, &r.MeasuredCPU, &r.MeasuredPeak, &r.Pool, &r.Queue, &r.Executor); err != nil {
		f.t.Fatalf("read ledger row for %s: %v", attemptID, err)
	}
	return r
}

// dispatchOne dispatches a single attempt through the pull path and backdates
// its dispatch so the reserved interval is a known 10 seconds.
func (f *fixture) dispatchOne(st *store.SchedulerStore, w uuid.UUID, node string, req vec, spec nodeSpec) uuid.UUID {
	f.t.Helper()
	id := f.addAttempt(node, 100, req, spec, time.Now().UTC().Add(-time.Minute))
	asg, err := st.ClaimForWorker(context.Background(), time.Now().UTC(), store.WorkerClaimRequest{WorkerID: w, MaxTasks: 1})
	if err != nil || len(asg) != 1 || asg[0].AttemptID != id {
		f.t.Fatalf("dispatch %s: %v %+v", node, err, asg)
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE task_attempts SET dispatched_at = now() - interval '10 seconds' WHERE id = $1`, id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// TestUsageLedgerSuccessIsMeteredOnceWithMetrics: one row per attempt, exact
// derived quantities, the measured metrics, pool and executor attribution, an
// outbox announcement -- and a duplicate report writes nothing more.
func TestUsageLedgerSuccessIsMeteredOnceWithMetrics(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	// A pool in the namespace policy serving this queue, for attribution.
	if _, err := pool.Exec(context.Background(), `
INSERT INTO platform_config (namespace, revision, config)
VALUES ($1, 1, jsonb_build_object('pools', jsonb_build_array(jsonb_build_object('name','cpu-pool','queue',$2::text))))`,
		f.ns, f.queue); err != nil {
		t.Fatal(err)
	}
	w := f.addWorker(vec{CPUMillis: 8000, MemoryBytes: 8 << 30, GPUCount: 2}, nil, []string{"docker"})
	st := f.newStore(nil)
	id := f.dispatchOne(st, w, "m", vec{CPUMillis: 2000, MemoryBytes: 2 << 30, GPUCount: 1},
		nodeSpec{TaskRef: "t", Version: 1, Executor: "docker"})

	m := &nexusv1.ExecutionMetrics{CpuSeconds: 7.5, PeakMemoryBytes: 123456, WallMs: 9000, ExitCode: 0}
	if res := succeed(t, st, id, m); !res.UsageRecorded {
		t.Fatal("ApplyResult did not report a usage record")
	}
	if res := succeed(t, st, id, m); res.Applied || res.UsageRecorded {
		t.Fatalf("duplicate report applied=%v usage=%v", res.Applied, res.UsageRecorded)
	}
	if n := f.ledgerRows(id); n != 1 {
		t.Fatalf("%d ledger rows, want exactly 1", n)
	}
	r := f.ledger(id)
	if r.Outcome != "SUCCEEDED" || r.WorkerID == nil || *r.WorkerID != w || r.CPUMillis != 2000 {
		t.Errorf("row identity: %+v", r)
	}
	// reserved ≈ 10 s (backdated dispatch); the derived columns are exact
	// multiples of it.
	if r.Reserved < 10 || r.Reserved > 15 {
		t.Errorf("reserved_seconds = %v, want ~10", r.Reserved)
	}
	if d := r.CPUCore - 2*r.Reserved; d > 1e-9 || d < -1e-9 {
		t.Errorf("cpu_core_seconds %v != 2 cores x %v s", r.CPUCore, r.Reserved)
	}
	if d := r.MemGiB - 2*r.Reserved; d > 1e-9 || d < -1e-9 {
		t.Errorf("memory_gib_seconds %v != 2 GiB x %v s", r.MemGiB, r.Reserved)
	}
	if d := r.GPU - r.Reserved; d > 1e-9 || d < -1e-9 {
		t.Errorf("gpu_seconds %v != 1 GPU x %v s", r.GPU, r.Reserved)
	}
	if r.MeasuredCPU == nil || *r.MeasuredCPU != 7.5 || r.MeasuredPeak == nil || *r.MeasuredPeak != 123456 {
		t.Errorf("measured metrics not recorded: cpu=%v peak=%v", r.MeasuredCPU, r.MeasuredPeak)
	}
	if r.Pool == nil || *r.Pool != "cpu-pool" || r.Executor == nil || *r.Executor != "docker" {
		t.Errorf("attribution: pool=%v executor=%v", r.Pool, r.Executor)
	}
	// F122: the metrics are on the attempt too.
	var metrics []byte
	if err := pool.QueryRow(context.Background(), `SELECT metrics FROM task_attempts WHERE id=$1`, id).Scan(&metrics); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(metrics), `"peak_memory_bytes": 123456`) && !strings.Contains(string(metrics), `"peak_memory_bytes":123456`) {
		t.Errorf("task_attempts.metrics = %s", metrics)
	}
	// The export hook: one outbox row, same transaction.
	var ob int
	var payload []byte
	if err := pool.QueryRow(context.Background(), `
SELECT count(*), max(payload::text)::jsonb FROM outbox WHERE topic = 'usage_recorded' AND payload->>'attempt_id' = $1`,
		id.String()).Scan(&ob, &payload); err != nil {
		t.Fatal(err)
	}
	if ob != 1 || !strings.Contains(string(payload), `"outcome": "SUCCEEDED"`) {
		t.Errorf("usage_recorded outbox rows = %d, payload %s", ob, payload)
	}
}

// TestUsageLedgerLostCancelAndPending: lease expiry and worker death are
// metered as LOST without metrics; cancelling a dispatched attempt is metered
// as CANCELLED; cancelling a PENDING one writes nothing; a late report after
// LOST writes nothing.
func TestUsageLedgerLostCancelAndPending(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	w := f.addWorker(vec{CPUMillis: 8000, MemoryBytes: 8 << 30}, nil, nil)
	st := f.newStore(nil)
	spec := nodeSpec{TaskRef: "t", Version: 1, MaxAttempts: 1}
	ctx := context.Background()

	lost := f.dispatchOne(st, w, "lost", vec{CPUMillis: 1000}, spec)
	f.expireLease(lost)
	if l, err := st.ReconcileExpiredLeases(ctx, time.Now().UTC(), 10); err != nil || len(l) != 1 || !l[0].UsageRecorded {
		t.Fatalf("expiry: %v %+v", err, l)
	}
	if res := succeed(t, st, lost, &nexusv1.ExecutionMetrics{CpuSeconds: 1}); res.Applied {
		t.Fatal("a late report was applied after LOST")
	}
	if r := f.ledger(lost); r.Outcome != "LOST" || r.MeasuredCPU != nil {
		t.Errorf("lost row: outcome %s measured %v (want LOST, NULL)", r.Outcome, r.MeasuredCPU)
	}
	if n := f.ledgerRows(lost); n != 1 {
		t.Errorf("lost attempt has %d ledger rows", n)
	}

	cancelled := f.dispatchOne(st, w, "cancelled", vec{CPUMillis: 1000}, spec)
	pending := f.addAttempt("pending", 100, vec{CPUMillis: 1000}, spec, time.Now().UTC().Add(time.Hour))
	if n, err := st.CancelRunAttempts(ctx, f.runID, time.Now().UTC()); err != nil || n != 2 {
		t.Fatalf("cancel: n=%d err=%v", n, err)
	}
	if r := f.ledger(cancelled); r.Outcome != "CANCELLED" || r.Reserved < 10 {
		t.Errorf("cancelled row: %+v", r)
	}
	if n := f.ledgerRows(pending); n != 0 {
		t.Errorf("a never-dispatched attempt was metered (%d rows)", n)
	}

	// Worker death.
	w2 := f.addWorker(vec{CPUMillis: 8000, MemoryBytes: 8 << 30}, nil, nil)
	f.staleHeartbeat(w, time.Hour)
	dead := f.dispatchOne(st, w2, "dead", vec{CPUMillis: 1000}, spec)
	f.staleHeartbeat(w2, time.Hour)
	if _, err := st.SweepDeadWorkers(ctx, time.Now().UTC(), 10); err != nil {
		t.Fatal(err)
	}
	if r := f.ledger(dead); r.Outcome != "LOST" {
		t.Errorf("worker-death row outcome %s", r.Outcome)
	}
}

// TestUsageLedgerReportRacingExpiry: a worker's report and the lease reconciler
// race for the same attempt from separate connections, many times. Exactly one
// terminal transition wins each time, and exactly one ledger row exists.
func TestUsageLedgerReportRacingExpiry(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	w := f.addWorker(vec{CPUMillis: 64000, MemoryBytes: 64 << 30}, nil, nil)
	st := f.newStore(nil)
	other := f.newStore(nil)
	spec := nodeSpec{TaskRef: "t", Version: 1, MaxAttempts: 1}
	for i := 0; i < 25; i++ {
		id := f.dispatchOne(st, w, nodeName("race", i), vec{CPUMillis: 10}, spec)
		f.expireLease(id)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = st.ApplyResult(context.Background(), time.Now().UTC(), &nexusv1.ReportResultRequest{
				AttemptId: id.String(), Outcome: nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED})
		}()
		go func() {
			defer wg.Done()
			_, _ = other.ReconcileExpiredLeases(context.Background(), time.Now().UTC(), 10)
		}()
		wg.Wait()
		// SKIP LOCKED means the loser may have skipped rather than waited; a
		// second pass settles an attempt neither finished.
		_, _ = other.ReconcileExpiredLeases(context.Background(), time.Now().UTC(), 10)
		if n := f.ledgerRows(id); n != 1 {
			t.Fatalf("iteration %d: %d ledger rows, want 1 (state %s)", i, n, f.attemptState(id))
		}
	}
}

// TestUsageLedgerIsAppendOnly: an UPDATE is refused even for the table owner.
func TestUsageLedgerIsAppendOnly(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	w := f.addWorker(vec{CPUMillis: 8000, MemoryBytes: 8 << 30}, nil, nil)
	st := f.newStore(nil)
	id := f.dispatchOne(st, w, "ao", vec{CPUMillis: 1000}, nodeSpec{TaskRef: "t", Version: 1})
	succeed(t, st, id, nil)
	_, err := pool.Exec(context.Background(), `UPDATE usage_ledger SET cpu_millis = 0 WHERE attempt_id = $1`, id)
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("UPDATE of the ledger: err = %v, want the append-only refusal", err)
	}
}

// countingObserver counts quota holds and ledger writes across replicas.
type countingObserver struct{ quota, usage atomic.Int64 }

func (c *countingObserver) QuotaBlocked(string)  { c.quota.Add(1) }
func (c *countingObserver) UsageRecorded(string) { c.usage.Add(1) }
