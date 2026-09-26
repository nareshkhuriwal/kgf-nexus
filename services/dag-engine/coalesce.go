// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// reconcileFlights coalesces reconciles of the same run (CONTRACT-FINDINGS
// F160).
//
// A reconcile recomputes the WHOLE run from the attempt rows, so one pass that
// starts after a set of attempts committed covers all of them. Before this,
// every terminal attempt got a pass of its own: a 500-way fan-out finishing
// together sent ~500 concurrent reconciles of one run at the engine, each
// loading every node and attempt of that run through a 3-connection pool.
// They queued on the pool, the gateway's deadline expired on 446 of them, and
// with nothing durable behind the calls the run stranded.
//
// The rule, per run:
//
//   - no pass running: start one; the caller waits for it.
//   - a pass running: the caller cannot be served by it (it may already have
//     read the attempts before the caller's fact committed), so it waits for
//     the NEXT pass, which starts when the current one ends. Every caller that
//     arrives while a pass runs shares that one next pass.
//
// So a burst of N reports of one run costs at most two passes at a time
// instead of N, and a caller is only ever answered by a pass that began after
// it asked -- the property that makes coalescing safe rather than lossy.
//
// Passes run DETACHED from the callers' contexts, with a timeout of their own:
// a pass serves many callers, and one caller's deadline expiring must not
// abort the work the others (and the durable queue) are waiting on. A caller
// whose context ends stops waiting; the pass carries on.
//
// A global semaphore bounds how many runs reconcile at once, so a burst across
// MANY runs queues here, holding goroutines, rather than on the pool, holding
// nothing useful and timing everyone out together.
type reconcileFlights struct {
	do      func(ctx context.Context, runID string) (*ReconcileResult, error)
	timeout time.Duration
	sem     chan struct{}

	mu   sync.Mutex
	runs map[string]*flight

	// passes counts passes executed; callers counts requests served. Their
	// ratio is the coalescing factor (and what the burst test asserts on).
	passes  atomic.Int64
	callers atomic.Int64
	metrics *Metrics
}

type flight struct {
	next *pass // the pass queued behind the running one, shared by its waiters
}

type pass struct {
	ctx  context.Context // values (trace, logger) only; never its cancellation
	done chan struct{}
	res  *ReconcileResult
	err  error
}

// Defaults: generous enough for a very large run's reconcile, bounded so a
// wedged database cannot pin a run's flight forever.
const (
	defaultReconcileConcurrency = 8
	defaultReconcileTimeout     = 2 * time.Minute
)

func newReconcileFlights(do func(context.Context, string) (*ReconcileResult, error), concurrency int, timeout time.Duration) *reconcileFlights {
	if concurrency <= 0 {
		concurrency = defaultReconcileConcurrency
	}
	if timeout <= 0 {
		timeout = defaultReconcileTimeout
	}
	return &reconcileFlights{
		do: do, timeout: timeout,
		sem:  make(chan struct{}, concurrency),
		runs: make(map[string]*flight),
	}
}

// reconcile returns the result of a pass over runID that began after this call.
func (f *reconcileFlights) reconcile(ctx context.Context, runID string) (*ReconcileResult, error) {
	f.callers.Add(1)
	f.mu.Lock()
	fl, running := f.runs[runID]
	var p *pass
	switch {
	case !running:
		p = &pass{ctx: context.WithoutCancel(ctx), done: make(chan struct{})}
		fl = &flight{}
		f.runs[runID] = fl
		go f.fly(runID, fl, p)
	case fl.next == nil:
		p = &pass{ctx: context.WithoutCancel(ctx), done: make(chan struct{})}
		fl.next = p
	default:
		p = fl.next
		f.metrics.observeCoalesced()
	}
	f.mu.Unlock()

	select {
	case <-p.done:
		return p.res, p.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fly runs passes for one run until none is queued, then retires the flight.
func (f *reconcileFlights) fly(runID string, fl *flight, p *pass) {
	for {
		p.res, p.err = f.runPass(p.ctx, runID)
		close(p.done)

		f.mu.Lock()
		if fl.next == nil {
			delete(f.runs, runID)
			f.mu.Unlock()
			return
		}
		p, fl.next = fl.next, nil
		f.mu.Unlock()
	}
}

func (f *reconcileFlights) runPass(ctx context.Context, runID string) (*ReconcileResult, error) {
	f.sem <- struct{}{}
	defer func() { <-f.sem }()
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	f.passes.Add(1)
	return f.do(ctx, runID)
}
