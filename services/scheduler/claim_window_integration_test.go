// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package main

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// The windowed pull claim (ADR-0018 Stage 1) is a performance change, so its
// whole correctness claim is: it dispatches exactly what the exhaustive scan
// would have, in exactly the same order. These tests hold it to that, against
// an ORACLE computed in Go from the rows themselves -- not against the
// exhaustive scan alone, which would let a bug shared by both paths pass.

// backlogRow is one attempt as the oracle sees it.
type backlogRow struct {
	node        string
	priority    int32
	scheduledAt time.Time
	eligible    bool
}

// seedBacklog writes the same randomized backlog into f and returns the rows.
//
// It is built to be hard for a window, not just big:
//   - a WALL of `wall` ineligible attempts (wrong executor) holding the very
//     highest priorities, so the first windows contain nothing this worker can
//     run and the claim must page and then fall back;
//   - scattered attempts that are ineligible for every other reason the
//     eligibility template checks: node_selector, the resource pre-filter,
//     and a scheduled_at still in the future;
//   - many priority ties, so the scheduled_at and id tie-breaks matter.
func seedBacklog(f *fixture, seed int64, n, wall int, now time.Time) []backlogRow {
	f.t.Helper()
	rng := rand.New(rand.NewSource(seed))
	small := vec{CPUMillis: 100, MemoryBytes: 64 << 20}
	var rows []backlogRow
	for i := 0; i < n; i++ {
		node := fmt.Sprintf("n%03d", i)
		pri := int32(rng.Intn(4)) // few values: plenty of ties
		sched := now.Add(-time.Duration(rng.Intn(3600)) * time.Second)
		spec := nodeSpec{TaskRef: node, Version: 1}
		req := small
		eligible := true
		switch {
		case i < wall:
			pri = 1000 // outranks everything else by far
			spec.Executor = "docker"
			eligible = false
		case i%11 == 0:
			spec.NodeSelector = map[string]string{"gpu": "a100"}
			eligible = false
		case i%13 == 0:
			req = vec{CPUMillis: 1 << 40} // cannot fit any worker here
			eligible = false
		case i%17 == 0:
			sched = now.Add(time.Hour) // not yet due
			eligible = false
		}
		f.addAttempt(node, pri, req, spec, sched)
		rows = append(rows, backlogRow{node: node, priority: pri, scheduledAt: sched, eligible: eligible})
	}
	return rows
}

// oracleOrder is the dispatch order §5.3 prescribes: eligible rows only, by
// effective priority (priority + age * rate) DESC, then oldest first.
func oracleOrder(rows []backlogRow, now time.Time, rate float64) []string {
	var el []backlogRow
	for _, r := range rows {
		if r.eligible {
			el = append(el, r)
		}
	}
	eff := func(r backlogRow) float64 {
		return float64(r.priority) + now.Sub(r.scheduledAt).Seconds()*rate
	}
	sort.SliceStable(el, func(i, j int) bool {
		ei, ej := eff(el[i]), eff(el[j])
		if ei != ej {
			return ei > ej
		}
		return el[i].scheduledAt.Before(el[j].scheduledAt)
	})
	out := make([]string, len(el))
	for i, r := range el {
		out[i] = r.node
	}
	return out
}

// drain claims one attempt at a time until the worker gets nothing, returning
// the node ids in dispatch order.
func drain(t *testing.T, st *store.SchedulerStore, f *fixture, worker uuid.UUID, now time.Time) []string {
	t.Helper()
	ctx := context.Background()
	var order []string
	for i := 0; i < 1000; i++ {
		asg, err := st.ClaimForWorker(ctx, now, store.WorkerClaimRequest{
			WorkerID: worker, Queues: []string{f.queue}, MaxTasks: 1,
			FreeCapacity: &nexusv1.ResourceVector{CpuMillis: 1 << 30, MemoryBytes: 1 << 50},
		})
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if len(asg) == 0 {
			return order
		}
		order = append(order, asg[0].NodeID)
	}
	t.Fatal("drain did not terminate: the claim keeps returning work")
	return nil
}

func bigWorker(f *fixture) uuid.UUID {
	return f.addWorker(vec{CPUMillis: 1 << 30, MemoryBytes: 1 << 50, DiskBytes: 1 << 50},
		map[string]string{"zone": "a"}, []string{"subprocess"})
}

func TestWindowedClaimDispatchesInExactlyTheScanOrder(t *testing.T) {
	pool := newTestPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	for _, tc := range []struct {
		name          string
		window, pages int
		rate          float64
	}{
		// Window 8 x 2 pages against a 30-row ineligible wall: both windows
		// come back empty, so this exercises paging AND the fallback.
		{"small window, forced fallback", 8, 2, 1.0},
		// Window 64 x 4: the wall fits in the first window, so the pick
		// comes from the window itself.
		{"default window", 64, 4, 1.0},
		// A non-default rate: windows are unindexed but must order by the
		// configured rate, not the index's.
		{"non-default aging rate", 16, 4, 0.25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const n, wall = 120, 30

			// Windowed store on one fixture...
			fw := newFixture(t, pool)
			rows := seedBacklog(fw, 42, n, wall, now)
			ww := bigWorker(fw)
			sw := fw.newStore(func(c *store.ClaimConfig) {
				c.ClaimWindow, c.ClaimWindowPages, c.AgingRate = tc.window, tc.pages, tc.rate
			})
			status, err := sw.EnableWindowedClaim(context.Background())
			if err != nil {
				t.Fatalf("enable windowed claim: %v", err)
			}
			if !status.Enabled {
				t.Fatalf("windowed claim not enabled (%s); is migration 0023 applied to the test database?", status.Reason)
			}

			// ...and the exhaustive scan on an identical copy.
			fe := newFixture(t, pool)
			seedBacklog(fe, 42, n, wall, now)
			we := bigWorker(fe)
			se := fe.newStore(func(c *store.ClaimConfig) { c.AgingRate = tc.rate })

			want := oracleOrder(rows, now, tc.rate)
			gotWindowed := drain(t, sw, fw, ww, now)
			gotScan := drain(t, se, fe, we, now)

			if len(want) == 0 {
				t.Fatal("oracle expects nothing: the seeded backlog has no eligible rows, so this test proves nothing")
			}
			if fmt.Sprint(gotWindowed) != fmt.Sprint(want) {
				t.Fatalf("windowed claim order differs from the oracle\n got:  %v\n want: %v", gotWindowed, want)
			}
			if fmt.Sprint(gotScan) != fmt.Sprint(want) {
				t.Fatalf("exhaustive scan order differs from the oracle (the reference itself is wrong)\n got:  %v\n want: %v", gotScan, want)
			}
		})
	}
}

// A worker that can run nothing near the top must still get the eligible work
// further down -- the fallback, not an early "no candidate", has to answer.
// This is the liveness half of the change: a window that gave up after its
// pages would strand eligible work behind an ineligible wall forever.
func TestWindowedClaimReachesWorkBehindAnIneligibleWall(t *testing.T) {
	pool := newTestPool(t)
	now := time.Now().UTC()
	f := newFixture(t, pool)
	for i := 0; i < 50; i++ {
		f.addAttempt(fmt.Sprintf("gpu%02d", i), 1000,
			vec{CPUMillis: 100}, nodeSpec{TaskRef: "g", Version: 1, Executor: "docker"},
			now.Add(-time.Minute))
	}
	target := f.addAttempt("cpu-last", 0, vec{CPUMillis: 100},
		nodeSpec{TaskRef: "c", Version: 1}, now.Add(-time.Second))
	w := bigWorker(f)
	st := f.newStore(func(c *store.ClaimConfig) { c.ClaimWindow, c.ClaimWindowPages = 4, 2 })
	if s, err := st.EnableWindowedClaim(context.Background()); err != nil || !s.Enabled {
		t.Fatalf("enable windowed claim: %+v %v", s, err)
	}
	got := drain(t, st, f, w, now)
	if len(got) != 1 || got[0] != "cpu-last" {
		t.Fatalf("dispatched %v; want exactly [cpu-last] from behind 50 ineligible attempts", got)
	}
	if s := f.attemptState(target); s != "DISPATCHED" {
		t.Fatalf("target state %s, want DISPATCHED", s)
	}
}

func TestEnableWindowedClaimRespectsAZeroWindow(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	st := f.newStore(func(c *store.ClaimConfig) { c.ClaimWindow = 0 })
	s, err := st.EnableWindowedClaim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Enabled || s.Reason == "" {
		t.Fatalf("status %+v; a zero window must stay disabled and say why", s)
	}
}
