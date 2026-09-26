// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// F160: reconciles of one run are coalesced, and coalescing must never answer
// a caller with a pass that began before it asked.

// gatedDo is a reconcile that blocks until released, recording each pass's
// start sequence number.
type gatedDo struct {
	mu       sync.Mutex
	started  []int64 // value of stamp when each pass began
	release  chan struct{}
	stamp    atomic.Int64
	inFlight atomic.Int32
	maxSeen  atomic.Int32
}

func (g *gatedDo) do(ctx context.Context, runID string) (*ReconcileResult, error) {
	n := g.inFlight.Add(1)
	for {
		m := g.maxSeen.Load()
		if n <= m || g.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	defer g.inFlight.Add(-1)
	g.mu.Lock()
	g.started = append(g.started, g.stamp.Load())
	g.mu.Unlock()
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &ReconcileResult{RunID: runID}, nil
}

func (g *gatedDo) passes() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.started)
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCoalescerCollapsesABurstIntoTwoPasses(t *testing.T) {
	g := &gatedDo{release: make(chan struct{})}
	f := newReconcileFlights(g.do, 4, time.Minute)

	// The first caller starts a pass and blocks inside it.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = f.reconcile(context.Background(), "run") }()
	waitUntil(t, func() bool { return g.passes() == 1 })

	// 500 more arrive while it runs. None may be answered by the running pass
	// (it may have read the run before their facts committed); all of them
	// share ONE next pass.
	g.stamp.Store(1)
	for i := 0; i < 500; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.reconcile(context.Background(), "run"); err != nil {
				t.Errorf("reconcile: %v", err)
			}
		}()
	}
	waitUntil(t, func() bool { return f.callers.Load() == 501 })
	close(g.release)
	wg.Wait()

	if n := g.passes(); n != 2 {
		t.Fatalf("501 calls ran %d passes, want 2 (the running one and one shared follow-up)", n)
	}
	if g.started[1] != 1 {
		t.Error("the follow-up pass started before the burst arrived")
	}
	f.mu.Lock()
	left := len(f.runs)
	f.mu.Unlock()
	if left != 0 {
		t.Errorf("%d flights left after every pass finished", left)
	}
}

func TestCoalescerKeepsRunsIndependentAndBoundsConcurrency(t *testing.T) {
	g := &gatedDo{release: make(chan struct{})}
	f := newReconcileFlights(g.do, 3, time.Minute)
	var wg sync.WaitGroup
	for _, run := range []string{"a", "b", "c", "d", "e", "f"} {
		wg.Add(1)
		go func(run string) { defer wg.Done(); _, _ = f.reconcile(context.Background(), run) }(run)
	}
	waitUntil(t, func() bool { return g.passes() == 3 })
	time.Sleep(20 * time.Millisecond)
	if n := g.passes(); n != 3 {
		t.Fatalf("%d passes started with a concurrency of 3", n)
	}
	close(g.release)
	wg.Wait()
	if n := g.passes(); n != 6 {
		t.Fatalf("six runs ran %d passes, want one each", n)
	}
	if m := g.maxSeen.Load(); m > 3 {
		t.Errorf("%d passes ran at once, bound is 3", m)
	}
}

// A caller whose deadline expires stops waiting; the pass it started must not
// be cancelled with it, because other callers (and the durable queue) are
// waiting on the same pass.
func TestCoalescerPassOutlivesAnImpatientCaller(t *testing.T) {
	g := &gatedDo{release: make(chan struct{})}
	f := newReconcileFlights(g.do, 1, time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := f.reconcile(ctx, "run"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("impatient caller got %v, want DeadlineExceeded", err)
	}
	// A patient caller arriving now joins the next pass; release both.
	done := make(chan error, 1)
	go func() { _, err := f.reconcile(context.Background(), "run"); done <- err }()
	waitUntil(t, func() bool { return f.callers.Load() == 2 })
	close(g.release)
	if err := <-done; err != nil {
		t.Fatalf("patient caller: %v (the first pass was cancelled with its caller)", err)
	}
}

// A pass is bounded by the flight's own timeout, not by any caller.
func TestCoalescerBoundsAPassByItsOwnTimeout(t *testing.T) {
	g := &gatedDo{release: make(chan struct{})} // never released
	f := newReconcileFlights(g.do, 1, 30*time.Millisecond)
	_, err := f.reconcile(context.Background(), "run")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a wedged pass returned %v, want its own DeadlineExceeded", err)
	}
}

func TestReconcileQueueConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	cfg, conc, timeout, err := reconcileQueueConfigFromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if conc != defaultReconcileConcurrency || timeout != defaultReconcileTimeout ||
		cfg.PollInterval != time.Second || cfg.BatchSize != 500 || cfg.Lease != 30*time.Second {
		t.Errorf("defaults: conc=%d timeout=%s cfg=%+v", conc, timeout, cfg)
	}

	cfg, conc, timeout, err = reconcileQueueConfigFromEnv(env(map[string]string{
		"NEXUS_DAG_RECONCILE_CONCURRENCY": "2", "NEXUS_DAG_RECONCILE_TIMEOUT_SEC": "30",
		"NEXUS_DAG_RECONCILE_QUEUE_POLL_MS": "250", "NEXUS_DAG_RECONCILE_QUEUE_BATCH": "50",
		"NEXUS_DAG_RECONCILE_QUEUE_LEASE_SEC": "10",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if conc != 2 || timeout != 30*time.Second || cfg.PollInterval != 250*time.Millisecond ||
		cfg.BatchSize != 50 || cfg.Lease != 10*time.Second {
		t.Errorf("overrides: conc=%d timeout=%s cfg=%+v", conc, timeout, cfg)
	}

	for _, bad := range []string{"0", "-1", "ten", "1.5"} {
		if _, _, _, err := reconcileQueueConfigFromEnv(env(map[string]string{"NEXUS_DAG_RECONCILE_QUEUE_POLL_MS": bad})); err == nil {
			t.Errorf("NEXUS_DAG_RECONCILE_QUEUE_POLL_MS=%q was accepted", bad)
		}
	}
}

func TestReconcileQueueBackoffIsBoundedExponential(t *testing.T) {
	q := &ReconcileQueue{cfg: ReconcileQueueConfig{BackoffBase: time.Second, BackoffMax: time.Minute}.withDefaults()}
	for failures, want := range map[int]time.Duration{0: time.Second, 1: 2 * time.Second, 3: 8 * time.Second, 6: time.Minute, 60: time.Minute} {
		if got := q.backoff(failures); got != want {
			t.Errorf("backoff(%d) = %s, want %s", failures, got, want)
		}
	}
}
