//go:build integration

package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// TestConcurrentSchedulersNeverDoubleDispatch is the central correctness claim
// of §5.3 and ADR-0002: N scheduler replicas share one queue with no leader
// election and no coordination, and the database alone prevents double dispatch
// and capacity over-commit.
//
// It uses real goroutines against the real database because the guarantee is a
// property of Postgres row locking and constraint enforcement, not of Go code.
// A mocked store would test nothing that matters here.
func TestConcurrentSchedulersNeverDoubleDispatch(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	const (
		schedulers = 8
		workers    = 4
		slotsPer   = 5
		attempts   = 60
		cpuPerTask = 1000
	)
	// Capacity is deliberately SMALLER than demand: 4 workers x 5 slots = 20
	// dispatchable at once against 60 attempts. Over-subscription is what makes
	// the capacity race real; with slack capacity every claim would trivially
	// succeed and the test would prove nothing.
	var workerIDs []uuid.UUID
	for i := 0; i < workers; i++ {
		workerIDs = append(workerIDs, f.addWorker(
			vec{CPUMillis: cpuPerTask * slotsPer, MemoryBytes: int64(slotsPer) << 30}, nil, nil))
	}

	now := time.Now().UTC()
	var attemptIDs []uuid.UUID
	for i := 0; i < attempts; i++ {
		attemptIDs = append(attemptIDs, f.addAttempt(
			nodeName("t", i), 100,
			vec{CPUMillis: cpuPerTask, MemoryBytes: 1 << 30},
			nodeSpec{TaskRef: "test.task", Version: 1},
			now))
	}

	// Every replica hammers the same queue as fast as it can, with no
	// coordination whatsoever.
	var wg sync.WaitGroup
	dispatched := make([][]store.DispatchResult, schedulers)
	errs := make([]error, schedulers)
	deadline := time.Now().Add(20 * time.Second)

	for s := 0; s < schedulers; s++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			st := f.newStore(func(c *store.ClaimConfig) {
				c.LeaseTTL = 5 * time.Minute // long enough that nothing expires mid-test
				c.LookAhead = 4
			})
			idleStreak := 0
			for time.Now().Before(deadline) {
				rep, err := st.Claim(ctx, time.Now().UTC())
				if err != nil {
					errs[idx] = err
					return
				}
				switch rep.Outcome {
				case store.ClaimDispatched:
					idleStreak = 0
					dispatched[idx] = append(dispatched[idx], *rep.Dispatch)
				case store.ClaimBlocked, store.ClaimNothingPending:
					idleStreak++
					// Stop once the queue has clearly settled: every remaining
					// attempt is blocked on capacity that will not free up,
					// because nothing in this test ever completes.
					if idleStreak > 20 {
						return
					}
					time.Sleep(5 * time.Millisecond)
				case store.ClaimRaceLost:
					idleStreak = 0
				}
			}
		}(s)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("scheduler %d failed: %v", i, err)
		}
	}

	// ── Guarantee 1: no attempt was dispatched twice. ────────────────────
	// Checked three ways, because a single check could pass for the wrong
	// reason: what the replicas believe, what the leases table records, and
	// what the attempts table records must all agree.
	seen := map[uuid.UUID]int{}
	total := 0
	for _, ds := range dispatched {
		for _, d := range ds {
			seen[d.AttemptID]++
			total++
		}
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("attempt %s was dispatched %d times by the scheduler fleet", id, n)
		}
	}
	if total == 0 {
		t.Fatal("no attempts were dispatched at all; the test proved nothing")
	}
	t.Logf("dispatched %d attempts across %d schedulers", total, schedulers)

	var dupLeases int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM (
  SELECT attempt_id FROM leases l
   JOIN task_attempts a ON a.id = l.attempt_id
  WHERE a.run_id = $1
  GROUP BY attempt_id HAVING count(*) > 1) x`, f.runID).Scan(&dupLeases); err != nil {
		t.Fatalf("count duplicate leases: %v", err)
	}
	if dupLeases != 0 {
		t.Errorf("%d attempts hold more than one lease", dupLeases)
	}

	var leaseCount, dispatchedCount, reservationCount int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM leases l JOIN task_attempts a ON a.id=l.attempt_id WHERE a.run_id=$1),
       (SELECT count(*) FROM task_attempts WHERE run_id=$1 AND state='DISPATCHED'),
       (SELECT count(*) FROM reservations r JOIN task_attempts a ON a.id=r.attempt_id WHERE a.run_id=$1)`,
		f.runID).Scan(&leaseCount, &dispatchedCount, &reservationCount); err != nil {
		t.Fatalf("count dispatch facts: %v", err)
	}
	if leaseCount != total || dispatchedCount != total || reservationCount != total {
		t.Errorf("dispatch facts disagree: %d dispatched by fleet, %d leases, %d DISPATCHED rows, %d reservations",
			total, leaseCount, dispatchedCount, reservationCount)
	}

	// ── Guarantee 2: no capacity over-commit. ────────────────────────────
	// workers.committed must equal the sum of that worker's reservations AND
	// must never exceed capacity. The first catches ledger drift; the second
	// catches the actual over-commit bug FOR UPDATE exists to prevent.
	for _, wid := range workerIDs {
		committed := f.workerCommitted(wid)

		var resCPU, resMem int64
		var resCount int
		if err := pool.QueryRow(ctx, `
SELECT COALESCE(sum((resources->>'cpu_millis')::bigint),0),
       COALESCE(sum((resources->>'memory_bytes')::bigint),0),
       count(*)
  FROM reservations WHERE worker_id = $1`, wid).Scan(&resCPU, &resMem, &resCount); err != nil {
			t.Fatalf("sum reservations: %v", err)
		}
		if committed.CPUMillis != resCPU || committed.MemoryBytes != resMem {
			t.Errorf("worker %s ledger drift: committed cpu=%d mem=%d, reservations cpu=%d mem=%d",
				wid, committed.CPUMillis, committed.MemoryBytes, resCPU, resMem)
		}
		if committed.CPUMillis > cpuPerTask*slotsPer {
			t.Errorf("worker %s OVER-COMMITTED: committed cpu=%d > capacity %d",
				wid, committed.CPUMillis, cpuPerTask*slotsPer)
		}
		if resCount > slotsPer {
			t.Errorf("worker %s holds %d reservations, capacity allows %d", wid, resCount, slotsPer)
		}
	}

	// The cluster should be saturated: 4 workers x 5 slots. Anything less means
	// the schedulers gave up early and the over-subscription never materialized.
	if total != workers*slotsPer {
		t.Errorf("dispatched %d, expected the cluster to be saturated at %d",
			total, workers*slotsPer)
	}

	// Everything that did not fit must be PENDING with a structured reason, not
	// FAILED. Failing an attempt because the cluster is full would burn its
	// retry budget on an operations problem.
	var stillPending, failed int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE state='PENDING'),
       count(*) FILTER (WHERE state NOT IN ('PENDING','DISPATCHED'))
  FROM task_attempts WHERE run_id = $1`, f.runID).Scan(&stillPending, &failed); err != nil {
		t.Fatalf("count leftover attempts: %v", err)
	}
	if failed != 0 {
		t.Errorf("%d attempts left a non-PENDING, non-DISPATCHED state; capacity pressure must never fail an attempt", failed)
	}
	if stillPending != attempts-total {
		t.Errorf("PENDING=%d, want %d", stillPending, attempts-total)
	}

	var withReason int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM task_attempts
 WHERE run_id=$1 AND state='PENDING' AND pending_reason LIKE 'waiting_for_%'`,
		f.runID).Scan(&withReason); err != nil {
		t.Fatalf("count pending reasons: %v", err)
	}
	if withReason == 0 && stillPending > 0 {
		t.Error("blocked attempts carry no structured pending_reason; the UI can only show an unexplained stall")
	}
	_ = attemptIDs
}

// TestAgingPreventsStarvationOfLargeTask reproduces the §7.3 failure mode.
//
// A 4-GPU task cannot run while a stream of 1-GPU tasks keeps consuming the
// capacity it needs. The test deliberately gives the large task NO static
// priority advantage — same priority as the small ones — because that isolates
// the variable under test: if the large task only wins when an operator has
// already hand-tuned its priority above the others, the scheduler has not solved
// starvation at all, it has just deferred the problem to the user.
//
// The only thing that differs between the two halves of this test is the aging
// rate. With aging off, the small tasks walk past the large one and consume the
// capacity it is waiting for, forever. With aging on, the large task's effective
// priority climbs with its wait and the scheduler stops feeding that worker
// smaller work.
func TestAgingPreventsStarvationOfLargeTask(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()

	// scenario builds an identical world for each half, so the only independent
	// variable is agingRate.
	type result struct {
		smallDispatched int
		bigState        string
		bigReason       string
		heldForBig      int
		committedGPU    int32
		reservedGPU     int64
	}
	scenario := func(t *testing.T, agingRate float64) result {
		t.Helper()
		f := newFixture(t, pool)

		// §7.3 verbatim: one worker, 4 GPUs, 3 free. The occupied GPU belongs to
		// a long-running attempt that never finishes during this test, so the
		// 4-GPU request genuinely cannot be satisfied right now.
		workerID := f.addWorker(vec{CPUMillis: 16000, MemoryBytes: 64 << 30, GPUCount: 4}, nil, nil)
		f.occupy(workerID, "incumbent", vec{GPUCount: 1})

		now := time.Now().UTC()
		spec := nodeSpec{TaskRef: "test.task", Version: 1}

		// T1: the large task. SAME priority as the small tasks, but it has been
		// waiting 300s. Only the aging term can distinguish it.
		big := f.addAttempt("big", 100, vec{GPUCount: 4}, spec, now.Add(-300*time.Second))

		// The stream of small tasks, all freshly scheduled so they carry no
		// aging credit. This is the "steady stream of small tasks" of §7.3.
		var small []uuid.UUID
		for i := 0; i < 6; i++ {
			small = append(small, f.addAttempt(nodeName("small", i), 100, vec{GPUCount: 1}, spec, now))
		}

		st := f.newStore(func(c *store.ClaimConfig) {
			c.LeaseTTL = 5 * time.Minute
			c.AgingRate = agingRate
			c.AgingBlockThreshold = 60
			c.LookAhead = 8
		})

		var res result
		for i := 0; i < 40; i++ {
			rep, err := st.Claim(ctx, time.Now().UTC())
			if err != nil {
				t.Fatalf("claim (aging=%v): %v", agingRate, err)
			}
			if rep.Outcome != store.ClaimDispatched {
				break
			}
			if rep.Dispatch.AttemptID == big {
				t.Fatalf("the 4-GPU task cannot fit in 3 free GPUs; dispatching it is a capacity bug")
			}
			res.smallDispatched++
		}

		res.bigState = f.attemptState(big)
		res.bigReason = f.pendingReason(big)
		for _, id := range small {
			if strings.Contains(f.pendingReason(id), "head_of_line") {
				res.heldForBig++
			}
		}
		res.committedGPU = f.workerCommitted(workerID).GPUCount
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(sum((resources->>'gpu_count')::bigint),0) FROM reservations WHERE worker_id=$1`,
			workerID).Scan(&res.reservedGPU); err != nil {
			t.Fatalf("sum gpu reservations: %v", err)
		}
		return res
	}

	// ── Control: aging disabled. The documented failure mode. ────────────
	without := scenario(t, 0)
	if without.smallDispatched == 0 {
		t.Fatal("without aging no small task overtook the large one; the test is not exercising the starvation path it claims to")
	}

	// ── Treatment: aging enabled. ────────────────────────────────────────
	with := scenario(t, 1.0)
	if with.smallDispatched != 0 {
		t.Errorf("%d small tasks were dispatched past a starving large task; aging is not preventing starvation",
			with.smallDispatched)
	}
	if with.smallDispatched >= without.smallDispatched {
		t.Errorf("aging made no difference: %d small tasks jumped the queue with aging, %d without",
			with.smallDispatched, without.smallDispatched)
	}
	t.Logf("small tasks that jumped the queue — without aging: %d, with aging: %d",
		without.smallDispatched, with.smallDispatched)

	// The large task must be left PENDING with an actionable reason, never
	// failed: capacity pressure is an operations problem, and failing the
	// attempt would burn its retry budget on it.
	if with.bigState != "PENDING" {
		t.Errorf("large task state = %s, want PENDING", with.bigState)
	}
	if !strings.HasPrefix(with.bigReason, "waiting_for_resources:") || !strings.Contains(with.bigReason, "gpu") {
		t.Errorf("large task pending_reason = %q, want a structured gpu shortfall", with.bigReason)
	}
	// The held-back small tasks must say WHY, naming the head-of-line guard
	// rather than reporting a resource shortfall that does not exist for them.
	if with.heldForBig == 0 {
		t.Error("no small task reports being held for the starving task; an operator would see an unexplained stall")
	}

	// The ledger must still be exact in both halves.
	for name, r := range map[string]result{"without aging": without, "with aging": with} {
		if int64(r.committedGPU) != r.reservedGPU {
			t.Errorf("%s: gpu ledger drift: committed=%d reservations=%d", name, r.committedGPU, r.reservedGPU)
		}
		if r.committedGPU > 4 {
			t.Errorf("%s: gpu over-commit: committed=%d capacity=4", name, r.committedGPU)
		}
	}
}

func nodeName(prefix string, i int) string {
	return prefix + "-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
}

// TestUnschedulableAttemptIsExplainedOnAnIdleQueue pins the commit that F61
// exposed: an attempt no worker can ever fit must end up PENDING with a
// structured pending_reason even when the poll dispatches NOTHING.
//
// The distinction from the existing coverage is the whole point. Every other
// test that asserts on pending_reason has some OTHER attempt dispatch in the
// same pass, so the claim transaction reaches its commit and the explanation
// rides along with it. On an idle queue there is nothing to dispatch, the claim
// returns early, and the explanation was being written into a transaction that
// the deferred Rollback then discarded -- leaving the operator with a stalled
// run, a healthy-looking fleet, and no field connecting the two, which is
// exactly the unexplained stall §3.2 says the column exists to prevent.
//
// So the queue here holds ONE attempt and it is unplaceable. A regression makes
// pending_reason empty again, not merely differently worded.
func TestUnschedulableAttemptIsExplainedOnAnIdleQueue(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	// 50 GiB of disk on the only worker, against a 100 GiB request: the
	// shortfall is on a scalar dimension the SQL pre-filter discards, so the
	// attempt never reaches domain.Fits through the candidate query and can only
	// be explained by explainUnschedulable.
	const (
		workerDisk = 50 << 30
		taskDisk   = 100 << 30
	)
	workerID := f.addWorker(vec{CPUMillis: 8000, MemoryBytes: 16 << 30, DiskBytes: workerDisk}, nil, nil)
	attemptID := f.addAttempt("ingest", 0,
		vec{CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: taskDisk},
		nodeSpec{TaskRef: "ingest", Version: 1}, time.Now().UTC().Add(-time.Minute))

	st := f.newStore(nil)
	asg, err := st.ClaimForWorker(ctx, time.Now().UTC(), store.WorkerClaimRequest{
		WorkerID:     workerID,
		Queues:       []string{f.queue},
		FreeCapacity: &nexusv1.ResourceVector{CpuMillis: 8000, MemoryBytes: 16 << 30, DiskBytes: workerDisk},
		MaxTasks:     1,
	})
	if err != nil {
		t.Fatalf("claim for worker: %v", err)
	}
	// An empty poll is the correct outcome here; dispatching a 100 GiB request
	// onto a 50 GiB worker would be the far worse bug.
	if len(asg) != 0 {
		t.Fatalf("dispatched %d assignments; a 100 GiB disk request must not be placed on a 50 GiB worker", len(asg))
	}

	// The assertion that matters: the reason SURVIVED the transaction.
	if state := f.attemptState(attemptID); state != "PENDING" {
		t.Errorf("attempt state = %s, want PENDING; capacity pressure must never fail an attempt", state)
	}
	reason := f.pendingReason(attemptID)
	if reason == "" {
		t.Fatal("pending_reason is empty after an idle-queue poll: the explanation was rolled back, " +
			"so an operator sees a stalled run beside an idle fleet with nothing connecting the two")
	}
	if !strings.HasPrefix(reason, "waiting_for_resources:") || !strings.Contains(reason, "disk") {
		t.Errorf("pending_reason = %q, want a structured disk shortfall", reason)
	}
	t.Logf("idle-queue poll explained the stall: %s", reason)
}
