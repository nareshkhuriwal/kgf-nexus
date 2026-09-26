// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/eviction"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Eviction handling (ADR-0030 §2).
//
// One notice -- from whichever source fires first -- turns the agent into an
// eviction drain: stop polling, tell the control plane why (so the push path
// stops too), and send every running attempt its checkpoint signal with as
// much grace as the notice leaves, minus a margin to report the outcome
// before the node disappears.

const (
	// DefaultEvictionGrace is the checkpoint window when a notice carries no
	// deadline (the kubernetes and file sources). GCP and Azure give ~30 s.
	DefaultEvictionGrace = 25 * time.Second
	// DefaultEvictionMaxGrace caps the window even when the notice allows
	// more (AWS gives two minutes): a task that has not checkpointed in 90 s
	// will not, and the report must land before the instance does.
	DefaultEvictionMaxGrace = 90 * time.Second
	// evictionReportMargin is kept back from the deadline for the report.
	evictionReportMargin = 5 * time.Second
)

// EvictionGrace is how long a preempted attempt gets between SIGTERM and
// SIGKILL for a notice with this deadline.
func EvictionGrace(now time.Time, n eviction.Notice, def, max time.Duration) time.Duration {
	if def <= 0 {
		def = DefaultEvictionGrace
	}
	if max <= 0 {
		max = DefaultEvictionMaxGrace
	}
	g := def
	if !n.Deadline.IsZero() {
		g = n.Deadline.Sub(now) - evictionReportMargin
	}
	if g > max {
		g = max
	}
	if g < time.Second {
		// Too late to checkpoint properly; still send TERM before KILL.
		g = time.Second
	}
	return g
}

// watchEvictions runs the configured sources for this session.
func (a *Agent) watchEvictions(ctx context.Context) {
	if len(a.cfg.Eviction) == 0 {
		return
	}
	notices := eviction.Merge(ctx, a.log, a.cfg.Eviction...)
	go func() {
		select {
		case <-ctx.Done():
		case n := <-notices:
			a.onEviction(n)
		}
	}()
}

// onEviction starts the eviction drain. Idempotent.
func (a *Agent) onEviction(n eviction.Notice) {
	if !a.evicting.CompareAndSwap(false, true) {
		return
	}
	a.cfg.Metrics.EvictionNotice(n.Source)
	grace := EvictionGrace(time.Now(), n, a.cfg.EvictionGrace, a.cfg.EvictionMaxGrace)
	a.drainReason.Store("eviction:" + n.Source)
	running := a.runner.RunningAttempts()
	a.log.Warn("node eviction notice: draining and checkpoint-signalling every running attempt",
		"source", n.Source, "reason", n.Reason, "deadline", n.Deadline, "grace", grace, "running", len(running))
	a.draining.Store(true)
	a.registry.SetDraining(true)
	for _, id := range running {
		a.runner.Preempt(id, grace)
	}
}

// heartbeatExtra fills the ADR-0030 fields of every beat.
func (a *Agent) heartbeatExtra(req *nexusv1.HeartbeatRequest) {
	if r, ok := a.drainReason.Load().(string); ok {
		req.DrainReason = r
	}
	if a.cfg.Cache != nil {
		req.CachedArtifacts = a.cfg.Cache.Keys(maxAdvertisedArtifacts)
		req.CachedArtifactsReported = true
	}
}

// maxAdvertisedArtifacts bounds the keys a heartbeat carries; the control
// plane stores at most this many (ADR-0030 §4).
const maxAdvertisedArtifacts = 256
