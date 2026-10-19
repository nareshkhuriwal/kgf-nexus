// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// The scheduler's service-instance detail (ADR-0037 §3). Every key is read
// from counters this replica already keeps in memory; building it queries
// nothing.
//
//	queue                       the queue this replica drains (-queue)
//	claims_total                claim transactions run since start, both paths, every outcome
//	claim_errors_total          of those, the ones that failed
//	dispatched_total            attempts leased to a worker since start
//	lost_total                  attempts this replica declared LOST since start
//	claims_recent               claims since the previous beat
//	dispatched_recent           dispatches since the previous beat
//	recent_window_seconds       the time the two *_recent values cover
//	pending_attempts            PENDING attempts on the queue at the latest backlog sample
//	oldest_pending_age_seconds  age of the oldest of them at that sample
//	backlog_sampled_at          when that sample was taken (RFC 3339); the three
//	                            backlog keys are absent until the first sample

// activity is the replica's running tally. Atomics, because claims are
// observed on the gRPC handlers' goroutines and the dispatch loop while the
// heartbeat reads.
type activity struct {
	claims      atomic.Int64
	claimErrors atomic.Int64
	dispatched  atomic.Int64
	lost        atomic.Int64

	backlogMu      sync.Mutex
	backlogAt      time.Time
	pending        int64
	oldestAgeSecs  float64
	backlogSampled bool
}

func (a *activity) claimed(outcome string, dispatched int) {
	a.claims.Add(1)
	if outcome == "error" {
		a.claimErrors.Add(1)
	}
	if dispatched > 0 {
		a.dispatched.Add(int64(dispatched))
	}
}

// sampled records the latest backlog of queue from a fresh sample. A queue
// absent from the sample has drained.
func (a *activity) sampled(queue string, sample []store.QueueBacklog) {
	var pending int64
	var oldest float64
	for _, b := range sample {
		if b.Queue == queue {
			pending += b.Pending
			oldest = math.Max(oldest, b.OldestAgeSeconds)
		}
	}
	a.backlogMu.Lock()
	a.pending, a.oldestAgeSecs, a.backlogAt, a.backlogSampled = pending, oldest, time.Now().UTC(), true
	a.backlogMu.Unlock()
}

// instanceDetail returns the heartbeat's DetailFunc for this replica. The
// *_recent values are the differences between consecutive calls, so they
// cover exactly one beat.
func (m *Metrics) instanceDetail() func() map[string]any {
	// The first beat's window runs from here, which is process start.
	var (
		mu                   sync.Mutex
		prevAt               = time.Now()
		prevClaims, prevDisp int64
	)
	return func() map[string]any {
		if m == nil {
			return nil
		}
		a := &m.activity
		claims, disp := a.claims.Load(), a.dispatched.Load()
		now := time.Now()

		mu.Lock()
		window := now.Sub(prevAt).Seconds()
		recentClaims, recentDisp := claims-prevClaims, disp-prevDisp
		prevAt, prevClaims, prevDisp = now, claims, disp
		mu.Unlock()

		d := map[string]any{
			"queue":                 m.queue,
			"claims_total":          claims,
			"claim_errors_total":    a.claimErrors.Load(),
			"dispatched_total":      disp,
			"lost_total":            a.lost.Load(),
			"claims_recent":         recentClaims,
			"dispatched_recent":     recentDisp,
			"recent_window_seconds": math.Round(window*10) / 10,
		}
		a.backlogMu.Lock()
		if a.backlogSampled {
			d["pending_attempts"] = a.pending
			d["oldest_pending_age_seconds"] = math.Round(a.oldestAgeSecs*10) / 10
			d["backlog_sampled_at"] = a.backlogAt.Format(time.RFC3339)
		}
		a.backlogMu.Unlock()
		return d
	}
}
