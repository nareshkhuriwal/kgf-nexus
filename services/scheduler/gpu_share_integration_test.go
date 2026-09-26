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
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
)

// ADR-0029/0030/0031 in the real claim, against real Postgres.
//
// The central test is TestFractionalGPURaceNeverOverAllocates: many pull and
// push replicas race for MIG instances and shared-device VRAM on a small fleet
// while a completer releases at random and a namespace quota binds. A sampler
// reads every reservation continuously; no sample may put any shared device,
// MIG profile or the quota over its limit.

const gib = int64(1) << 30

func (f *fixture) setWorkerFacts(id uuid.UUID, hostname string, cached []string) {
	f.t.Helper()
	if cached == nil {
		cached = []string{}
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE workers SET hostname = COALESCE(NULLIF($2,''), hostname), cached_artifacts = $3 WHERE id = $1`,
		id, hostname, cached); err != nil {
		f.t.Fatalf("set worker facts: %v", err)
	}
}

// addCluster registers a cluster of the fixture's namespace with published
// labels and (optionally) headroom observed at `at`.
func (f *fixture) addCluster(name string, labels map[string]string, headroom map[string]any, at time.Time) {
	f.t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := f.pool.QueryRow(ctx, `
INSERT INTO clusters (namespace, name, provider) VALUES ($1, $2, 'kubernetes') RETURNING id`,
		f.ns, name).Scan(&id); err != nil {
		f.t.Fatalf("insert cluster: %v", err)
	}
	if headroom == nil {
		headroom = map[string]any{}
	}
	if _, err := f.pool.Exec(ctx, `
INSERT INTO cluster_status (cluster_id, labels, headroom, headroom_observed_at) VALUES ($1,$2,$3,$4)`,
		id, mustJSON(f.t, labels), mustJSON(f.t, headroom), at); err != nil {
		f.t.Fatalf("insert cluster status: %v", err)
	}
	f.t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM clusters WHERE id = $1`, id) })
}

func sharedGPUWorker(devs int, mem int64) vec {
	v := vec{CPUMillis: 64000, MemoryBytes: 1 << 40, GPUKind: "nvidia-a100",
		Mig: map[string]int64{"1g.10gb": 3, "3g.40gb": 1}, GPUShares: map[string]shareEntry{}}
	for d := 0; d < devs; d++ {
		v.GPUShares[fmt.Sprint(4+d)] = shareEntry{Milli: 1000, MemoryBytes: mem}
	}
	return v
}

type ledgerSample struct {
	shared map[uuid.UUID]map[string]shareEntry
	mig    map[uuid.UUID]map[string]int64
}

// sampleReservations reads every reservation of the fixture's workers in one
// statement and sums them per worker, per shared device and per MIG profile.
func (f *fixture) sampleReservations(ctx context.Context) (ledgerSample, error) {
	out := ledgerSample{shared: map[uuid.UUID]map[string]shareEntry{}, mig: map[uuid.UUID]map[string]int64{}}
	rows, err := f.pool.Query(ctx, `
SELECT r.worker_id, r.resources FROM reservations r JOIN workers w ON w.id = r.worker_id
 WHERE w.namespace = $1`, f.ns)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var wid uuid.UUID
		var b []byte
		if err := rows.Scan(&wid, &b); err != nil {
			return out, err
		}
		var v vec
		if err := json.Unmarshal(b, &v); err != nil {
			return out, err
		}
		if out.shared[wid] == nil {
			out.shared[wid] = map[string]shareEntry{}
			out.mig[wid] = map[string]int64{}
		}
		if len(v.GPUShares) > 1 {
			return out, fmt.Errorf("reservation on worker %s names %d shared devices", wid, len(v.GPUShares))
		}
		if (v.GPUShareMilli > 0 || v.GPUShareMemoryBytes > 0) && len(v.GPUShares) != 1 {
			return out, fmt.Errorf("shared request reserved without a device on worker %s: %s", wid, b)
		}
		for d, s := range v.GPUShares {
			cur := out.shared[wid][d]
			cur.Milli += s.Milli
			cur.MemoryBytes += s.MemoryBytes
			out.shared[wid][d] = cur
		}
		for p, n := range v.Mig {
			out.mig[wid][p] += n
		}
	}
	return out, rows.Err()
}

func TestFractionalGPURaceNeverOverAllocates(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	const quota = 14
	f.setQuotas(map[string]any{"max_inflight_attempts": quota})

	capacity := sharedGPUWorker(2, 40*gib)
	var workers []uuid.UUID
	for i := 0; i < 4; i++ {
		workers = append(workers, f.addWorker(capacity, nil, nil))
	}
	rng := rand.New(rand.NewSource(7))
	now := time.Now().UTC().Add(-time.Minute)
	const total = 240
	for i := 0; i < total; i++ {
		req := vec{CPUMillis: 100, MemoryBytes: 1 << 20}
		switch i % 3 {
		case 0:
			req.GPUShareMemoryBytes = gib * int64(5+rng.Intn(26))
			req.GPUShareMilli = int32(100 * (1 + rng.Intn(5)))
		case 1:
			req.Mig = map[string]int64{"1g.10gb": int64(1 + rng.Intn(2))}
		default:
			req.Mig = map[string]int64{"3g.40gb": 1}
		}
		f.addAttempt(nodeName("g", i), 100, req, nodeSpec{TaskRef: "test.task", Version: 1}, now)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	var (
		wg         sync.WaitGroup
		violations atomic.Int64
		samples    atomic.Int64
		peakShared atomic.Int64
		completed  atomic.Int64
		errs       = make(chan error, 64)
	)
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s, err := f.sampleReservations(ctx)
			if err != nil {
				if ctx.Err() == nil {
					errs <- err
				}
				return
			}
			samples.Add(1)
			for wid, devs := range s.shared {
				for d, used := range devs {
					c := capacity.GPUShares[d]
					if used.MemoryBytes > c.MemoryBytes || used.Milli > c.Milli {
						if violations.Add(1) <= 5 {
							t.Errorf("worker %s device %s over-allocated: %+v of %+v", wid, d, used, c)
						}
					}
					for cur := peakShared.Load(); used.MemoryBytes > cur && !peakShared.CompareAndSwap(cur, used.MemoryBytes); cur = peakShared.Load() {
					}
				}
				for p, n := range s.mig[wid] {
					if n > capacity.Mig[p] {
						if violations.Add(1) <= 5 {
							t.Errorf("worker %s MIG %s over-allocated: %d of %d", wid, p, n, capacity.Mig[p])
						}
					}
				}
			}
			if n, _ := f.inflight(); n > quota {
				if violations.Add(1) <= 5 {
					t.Errorf("namespace quota exceeded: %d in flight > %d", n, quota)
				}
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
		if r%4 == 1 {
			cfg.PlacementPolicy = domain.PolicySpread
		}
		st := store.NewSchedulerStore(p, cfg, fmt.Sprintf("gpu-%d", r))
		rr := rand.New(rand.NewSource(int64(r)))
		for completed.Load() < total && ctx.Err() == nil {
			var err error
			if pull {
				_, err = st.ClaimForWorker(ctx, time.Now().UTC(), store.WorkerClaimRequest{
					WorkerID: workers[rr.Intn(len(workers))], MaxTasks: 3})
			} else {
				_, err = st.Claim(ctx, time.Now().UTC())
			}
			if err != nil {
				if ctx.Err() == nil {
					errs <- fmt.Errorf("replica %d: %w", r, err)
				}
				return
			}
		}
	}
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go replica(r, r%2 == 0)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		st := f.newStore(nil)
		for completed.Load() < total && ctx.Err() == nil {
			var id uuid.UUID
			err := pool.QueryRow(ctx, `
SELECT a.id FROM task_attempts a JOIN workflow_runs r ON r.id = a.run_id
 WHERE r.namespace = $1 AND a.state = 'DISPATCHED' ORDER BY random() LIMIT 1`, f.ns).Scan(&id)
			if err != nil {
				time.Sleep(2 * time.Millisecond)
				continue
			}
			res, err := st.ApplyResult(ctx, time.Now().UTC(), &nexusv1.ReportResultRequest{
				AttemptId: id.String(), Outcome: nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED})
			if err != nil {
				if ctx.Err() == nil {
					errs <- err
				}
				return
			}
			if res.Applied {
				completed.Add(1)
			}
		}
	}()

	for completed.Load() < total && ctx.Err() == nil {
		time.Sleep(50 * time.Millisecond)
	}
	close(stop)
	cancel()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := completed.Load(); got != total {
		t.Fatalf("completed %d of %d attempts before the deadline", got, total)
	}
	if violations.Load() > 0 {
		t.Fatalf("%d over-allocation samples", violations.Load())
	}
	t.Logf("%d samples, peak VRAM reserved on one device %d GiB of %d", samples.Load(),
		peakShared.Load()/gib, capacity.GPUShares["4"].MemoryBytes/gib)
	if peakShared.Load() < 25*gib {
		t.Errorf("peak per-device VRAM %d GiB: the fleet was never packed, so the test proved little", peakShared.Load()/gib)
	}
	// Every release returned exactly what was taken: the ledger is empty.
	for _, w := range workers {
		c := f.workerCommitted(w)
		if c.CPUMillis != 0 || c.GPUShareMemoryBytes != 0 || len(c.GPUShares) != 0 || sumMap(c.Mig) != 0 {
			t.Errorf("worker %s committed not zero after every attempt ended: %+v", w, c)
		}
	}
}

func sumMap(m map[string]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}

func TestPlacementHardConstraintsBothPaths(t *testing.T) {
	pool := newTestPool(t)
	for _, pull := range []bool{true, false} {
		t.Run(fmt.Sprintf("pull=%v", pull), func(t *testing.T) {
			f := newFixture(t, pool)
			cap := vec{CPUMillis: 8000, MemoryBytes: 1 << 34}
			f.addCluster("eu-"+f.ns, map[string]string{domain.LabelRegion: "eu-west-1", domain.LabelCostTier: "3"}, nil, time.Now())
			f.addCluster("us-"+f.ns, map[string]string{domain.LabelRegion: "us-east-1", domain.LabelCostTier: "1"}, nil, time.Now())
			spotEU := f.addWorker(cap, map[string]string{domain.LabelCluster: "eu-" + f.ns, domain.LabelCapacityType: "spot",
				// A worker CLAIMING a region its cluster contradicts: the cluster wins.
				domain.LabelRegion: "us-east-1"}, nil)
			odUS := f.addWorker(cap, map[string]string{domain.LabelCluster: "us-" + f.ns}, nil)
			at := time.Now().UTC().Add(-time.Minute)
			onDemand := f.addAttempt("od", 100, vec{CPUMillis: 100}, nodeSpec{TaskRef: "t", Version: 1,
				Placement: map[string]any{"capacity": "CAPACITY_PREFERENCE_ON_DEMAND_ONLY"}}, at)
			euOnly := f.addAttempt("eu", 100, vec{CPUMillis: 100}, nodeSpec{TaskRef: "t", Version: 1,
				Placement: map[string]any{"required_labels": map[string]string{domain.LabelRegion: "eu-west-1"}}}, at)
			cheap := f.addAttempt("cheap", 100, vec{CPUMillis: 100}, nodeSpec{TaskRef: "t", Version: 1,
				Placement: map[string]any{"max_cost_tier": 2}}, at)
			st := f.newStore(func(c *store.ClaimConfig) { c.PlacementWait = 0 })
			dispatch := func() {
				for i := 0; i < 6; i++ {
					if pull {
						for _, w := range []uuid.UUID{spotEU, odUS} {
							if _, err := st.ClaimForWorker(context.Background(), time.Now().UTC(),
								store.WorkerClaimRequest{WorkerID: w, MaxTasks: 4}); err != nil {
								t.Fatal(err)
							}
						}
					} else if _, err := st.Claim(context.Background(), time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
				}
			}
			dispatch()
			workerOf := func(id uuid.UUID) uuid.UUID {
				var w *uuid.UUID
				_ = pool.QueryRow(context.Background(), `SELECT worker_id FROM task_attempts WHERE id=$1`, id).Scan(&w)
				if w == nil {
					t.Fatalf("attempt %s was not dispatched (%s)", id, f.pendingReason(id))
				}
				return *w
			}
			if w := workerOf(onDemand); w != odUS {
				t.Errorf("on_demand_only ran on %s, want the on-demand worker", w)
			}
			if w := workerOf(euOnly); w != spotEU {
				t.Errorf("eu-only ran on %s, want the EU cluster's worker (cluster labels must override the worker's claim)", w)
			}
			if w := workerOf(cheap); w != odUS {
				t.Errorf("max_cost_tier 2 ran on %s, want the tier-1 cluster", w)
			}
		})
	}
}

func TestPullDeferralPrefersCachedWorkerWithinTheWait(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	cap := vec{CPUMillis: 8000, MemoryBytes: 1 << 34}
	cold := f.addWorker(cap, nil, nil)
	warm := f.addWorker(cap, nil, nil)
	f.setWorkerFacts(warm, "", []string{"sha256:model"})
	spec := nodeSpec{TaskRef: "t", Version: 1, Placement: map[string]any{"artifacts": []map[string]any{
		{"name": "model", "key": "sha256:model", "size_bytes": "42949672960"}}}}
	fresh := f.addAttempt("fresh", 100, vec{CPUMillis: 100}, spec, time.Now().UTC())
	st := f.newStore(func(c *store.ClaimConfig) { c.PlacementWait = time.Minute })
	var obs deferralCounter
	st.SetObserver(&obs)

	got, err := st.ClaimForWorker(context.Background(), time.Now().UTC(), store.WorkerClaimRequest{WorkerID: cold, MaxTasks: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || f.attemptState(fresh) != "PENDING" || obs.n.Load() == 0 {
		t.Fatalf("cold worker took an attempt a cached worker should get (deferrals=%d)", obs.n.Load())
	}
	got, err = st.ClaimForWorker(context.Background(), time.Now().UTC(), store.WorkerClaimRequest{WorkerID: warm, MaxTasks: 1})
	if err != nil || len(got) != 1 || got[0].AttemptID != fresh {
		t.Fatalf("warm worker did not get it: %v %v", got, err)
	}

	// Past the wait, anyone who fits takes it: deferral is bounded.
	old := f.addAttempt("old", 100, vec{CPUMillis: 100}, spec, time.Now().UTC().Add(-2*time.Minute))
	got, err = st.ClaimForWorker(context.Background(), time.Now().UTC(), store.WorkerClaimRequest{WorkerID: cold, MaxTasks: 1})
	if err != nil || len(got) != 1 || got[0].AttemptID != old {
		t.Fatalf("an attempt past the placement wait was still deferred: %v %v", got, err)
	}
}

type deferralCounter struct{ n atomic.Int64 }

func (d *deferralCounter) QuotaBlocked(string)  {}
func (d *deferralCounter) UsageRecorded(string) {}
func (d *deferralCounter) PlacementDeferred()   { d.n.Add(1) }

func TestPreferSameWorkerAsIsRead(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	cap := vec{CPUMillis: 8000, MemoryBytes: 1 << 34}
	a := f.addWorker(cap, nil, nil)
	b := f.addWorker(cap, nil, nil)
	f.setWorkerFacts(b, "host-b-"+f.ns, nil)
	// The upstream node succeeded on b.
	up := f.occupy(b, "upstream", vec{CPUMillis: 1})
	st := f.newStore(func(c *store.ClaimConfig) { c.PlacementWait = time.Minute })
	succeed(t, st, up, nil)
	down := f.addAttempt("down", 100, vec{CPUMillis: 100},
		nodeSpec{TaskRef: "t", Version: 1, PreferSameWorkerAs: "upstream"}, time.Now().UTC())
	if got, _ := st.ClaimForWorker(context.Background(), time.Now().UTC(), store.WorkerClaimRequest{WorkerID: a, MaxTasks: 1}); len(got) != 0 {
		t.Fatal("worker a took a task that prefers the host that ran its upstream")
	}
	if got, _ := st.ClaimForWorker(context.Background(), time.Now().UTC(), store.WorkerClaimRequest{WorkerID: b, MaxTasks: 1}); len(got) != 1 || got[0].AttemptID != down {
		t.Fatal("the preferred host did not get the task")
	}
}

func TestSharedGPUPlacementIsReservedAndReleasedExactly(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	w := f.addWorker(sharedGPUWorker(2, 40*gib), nil, nil)
	st := f.newStore(func(c *store.ClaimConfig) { c.PlacementWait = 0 })
	var ids []uuid.UUID
	for i := 0; i < 4; i++ {
		ids = append(ids, f.addAttempt(nodeName("s", i), 100,
			vec{CPUMillis: 100, GPUShareMemoryBytes: 20 * gib, GPUShareMilli: 250}, nodeSpec{TaskRef: "t", Version: 1},
			time.Now().UTC().Add(-time.Minute)))
	}
	got, err := st.ClaimForWorker(context.Background(), time.Now().UTC(), store.WorkerClaimRequest{WorkerID: w, MaxTasks: 8})
	if err != nil || len(got) != 4 {
		t.Fatalf("dispatched %d of 4 slices (err %v); 4 x 20 GiB fits 2 x 40 GiB exactly", len(got), err)
	}
	devices := map[string]int{}
	for _, a := range got {
		d, ok := domain.SharedDevice(a.Reserved)
		if !ok {
			t.Fatalf("assignment %s carries no shared device", a.AttemptID)
		}
		devices[d]++
	}
	if devices["4"] != 2 || devices["5"] != 2 {
		t.Fatalf("devices %v, want two slices on each", devices)
	}
	c := f.workerCommitted(w)
	if c.GPUShares["4"].MemoryBytes != 40*gib || c.GPUShares["5"].MemoryBytes != 40*gib {
		t.Fatalf("committed %+v", c.GPUShares)
	}
	extra := f.addAttempt("extra", 100, vec{CPUMillis: 100, GPUShareMemoryBytes: gib}, nodeSpec{TaskRef: "t", Version: 1},
		time.Now().UTC().Add(-time.Minute))
	if got, _ := st.ClaimForWorker(context.Background(), time.Now().UTC(), store.WorkerClaimRequest{WorkerID: w, MaxTasks: 1}); len(got) != 0 {
		t.Fatal("a slice fit on two full devices")
	}
	if r := f.pendingReason(extra); !strings.Contains(r, "gpu_share") {
		t.Fatalf("pending_reason %q does not name the shared dimension", r)
	}
	for _, id := range ids {
		succeed(t, st, id, nil)
	}
	c = f.workerCommitted(w)
	if len(c.GPUShares) != 0 || c.GPUShareMemoryBytes != 0 {
		t.Fatalf("release left residue: %+v", c)
	}
}

func TestMIGRequestNeverBlocksAWorkerWithoutTheProfile(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	plain := f.addWorker(vec{CPUMillis: 8000, MemoryBytes: 1 << 34}, nil, nil)
	at := time.Now().UTC().Add(-time.Hour)
	mig := f.addAttempt("mig", 1000, vec{Mig: map[string]int64{"1g.10gb": 1}}, nodeSpec{TaskRef: "t", Version: 1}, at)
	cpu := f.addAttempt("cpu", 1, vec{CPUMillis: 100}, nodeSpec{TaskRef: "t", Version: 1}, time.Now().UTC())
	st := f.newStore(nil)
	got, err := st.ClaimForWorker(context.Background(), time.Now().UTC(), store.WorkerClaimRequest{WorkerID: plain, MaxTasks: 1})
	if err != nil || len(got) != 1 || got[0].AttemptID != cpu {
		t.Fatalf("a CPU worker was blocked behind a MIG task it can never run: %v %v", got, err)
	}
	if f.attemptState(mig) != "PENDING" {
		t.Fatal("MIG task dispatched to a worker without MIG")
	}
}

func TestClusterHeadroomGatesKubernetesDispatch(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	cap := vec{CPUMillis: 64000, MemoryBytes: 1 << 36}
	f.addCluster("full-"+f.ns, nil, map[string]any{"tasks": map[string]any{"cpu_millis": 1000, "memory_bytes": -1,
		"gpu_count": -1, "pods": 10}}, time.Now().UTC())
	f.addCluster("roomy-"+f.ns, nil, map[string]any{"tasks": map[string]any{"cpu_millis": 100000, "memory_bytes": -1,
		"gpu_count": -1, "pods": 10}}, time.Now().UTC())
	full := f.addWorker(cap, map[string]string{domain.LabelCluster: "full-" + f.ns, domain.LabelK8sTaskNamespace: "tasks"},
		[]string{"kubernetes"})
	roomy := f.addWorker(cap, map[string]string{domain.LabelCluster: "roomy-" + f.ns, domain.LabelK8sTaskNamespace: "tasks"},
		[]string{"kubernetes"})
	id := f.addAttempt("k", 100, vec{CPUMillis: 2000}, nodeSpec{TaskRef: "t", Version: 1, Executor: "kubernetes"},
		time.Now().UTC().Add(-time.Minute))
	st := f.newStore(nil)
	if got, _ := st.ClaimForWorker(context.Background(), time.Now().UTC(), store.WorkerClaimRequest{WorkerID: full, MaxTasks: 1}); len(got) != 0 {
		t.Fatal("dispatched into a cluster whose quota headroom is 1000m for a 2000m pod")
	}
	if r := f.pendingReason(id); !strings.Contains(r, "waiting_for_cluster_quota") {
		t.Fatalf("pending_reason %q", r)
	}
	if got, _ := st.ClaimForWorker(context.Background(), time.Now().UTC(), store.WorkerClaimRequest{WorkerID: roomy, MaxTasks: 1}); len(got) != 1 {
		t.Fatal("the cluster with headroom did not get the pod")
	}
	// A stale observation gates nothing.
	f2 := newFixture(t, pool)
	f2.addCluster("stale-"+f2.ns, nil, map[string]any{"tasks": map[string]any{"cpu_millis": 0, "memory_bytes": -1,
		"gpu_count": -1, "pods": 0}}, time.Now().UTC().Add(-time.Hour))
	w := f2.addWorker(cap, map[string]string{domain.LabelCluster: "stale-" + f2.ns, domain.LabelK8sTaskNamespace: "tasks"},
		[]string{"kubernetes"})
	f2.addAttempt("k", 100, vec{CPUMillis: 2000}, nodeSpec{TaskRef: "t", Version: 1, Executor: "kubernetes"},
		time.Now().UTC().Add(-time.Minute))
	if got, _ := f2.newStore(nil).ClaimForWorker(context.Background(), time.Now().UTC(), store.WorkerClaimRequest{WorkerID: w, MaxTasks: 1}); len(got) != 1 {
		t.Fatal("a stale headroom observation blocked dispatch")
	}
}

func TestPreemptionUsesItsOwnRetryBudget(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	// One worker per preemption: a preempted worker is drained in the same
	// transaction, so each retry must land somewhere else.
	var workers []uuid.UUID
	for i := 0; i < 3; i++ {
		workers = append(workers, f.addWorker(vec{CPUMillis: 8000, MemoryBytes: 1 << 34}, nil, nil))
	}
	next := 0
	st := f.newStore(func(c *store.ClaimConfig) { c.PreemptionRetryDelay = 0 })
	f.addAttempt("p", 100, vec{CPUMillis: 100}, nodeSpec{TaskRef: "t", Version: 1, MaxAttempts: 1, Idempotency: "required",
		Placement: map[string]any{"max_preemptions": 2}}, time.Now().UTC().Add(-time.Minute))
	ctx := context.Background()
	budget := func() (attempts, preemptions int) {
		if err := pool.QueryRow(ctx, `SELECT attempts_used, preemptions_used FROM task_nodes WHERE run_id=$1 AND node_id='p'`,
			f.runID).Scan(&attempts, &preemptions); err != nil {
			t.Fatal(err)
		}
		return
	}
	preemptOnce := func() store.AppliedResult {
		w := workers[next]
		next++
		got, err := st.ClaimForWorker(ctx, time.Now().UTC(), store.WorkerClaimRequest{WorkerID: w, MaxTasks: 1})
		if err != nil || len(got) != 1 {
			t.Fatalf("dispatch: %v %v", got, err)
		}
		res, err := st.ApplyResult(ctx, time.Now().UTC(), &nexusv1.ReportResultRequest{
			AttemptId: got[0].AttemptID.String(), Outcome: nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED,
			Error: &nexusv1.TaskError{Class: nexusv1.ErrorClass_ERROR_CLASS_PREEMPTED, Message: "spot interruption"}})
		if err != nil {
			t.Fatal(err)
		}
		// The reporting worker's node is going away: it is drained in the same
		// transaction, so neither a parked long poll nor the push loop can hand
		// it the retry.
		if s := f.workerState(w); s != "DRAINING" {
			t.Fatalf("preempted worker is %s, want DRAINING", s)
		}
		if again, _ := st.ClaimForWorker(ctx, time.Now().UTC(), store.WorkerClaimRequest{WorkerID: w, MaxTasks: 1}); len(again) != 0 {
			t.Fatal("the drained, preempted worker was handed work")
		}
		if res.Retry {
			fired, err := st.ClaimDueTimers(ctx, time.Now().UTC().Add(time.Minute), []string{domain.TimerRetryBackoff}, 10)
			if err != nil || len(fired) != 1 {
				t.Fatalf("fire retry timer: %d %v", len(fired), err)
			}
			if _, err := st.CreatePendingRetry(ctx, time.Now().UTC(), fired[0]); err != nil {
				t.Fatal(err)
			}
		}
		return res
	}
	// max_attempts is 1, so without a separate budget the FIRST preemption
	// would end the task.
	for i := 1; i <= 2; i++ {
		if res := preemptOnce(); !res.Retry {
			t.Fatalf("preemption %d not retried", i)
		}
		if a, p := budget(); a != 0 || p != i {
			t.Fatalf("after preemption %d: attempts_used=%d preemptions_used=%d, want 0 and %d", i, a, p, i)
		}
	}
	// Preemption budget gone; the ordinary budget (max_attempts 1, and the
	// dag-engine did not charge the first attempt in this fixture) decides.
	res := preemptOnce()
	a, p := budget()
	t.Logf("third preemption: retry=%v attempts_used=%d preemptions_used=%d", res.Retry, a, p)
	if p != 2 {
		t.Fatalf("preemptions_used moved past its cap: %d", p)
	}
	if res.Retry && a != 1 {
		t.Fatalf("a retry past the preemption cap was not charged to attempts_used (%d)", a)
	}
}
