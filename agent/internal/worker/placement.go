// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/artifactcache"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Agent-side halves of ADR-0030: the node-local artifact cache as a task input,
// and preemption as its own outcome.

// acquireArtifacts ensures every declared artifact is in the node-local cache
// and pins it for the attempt's lifetime. The returned release unpins them all;
// it is safe to call when acquisition failed part way.
//
// Only for executors whose workload runs on THIS node. The kubernetes
// executor's pods run elsewhere, where a path in this agent's cache is
// meaningless, so their artifacts are the task's own business (ADR-0030 §4).
func (r *Runner) acquireArtifacts(ctx context.Context, a *nexusv1.TaskAssignment, kind string,
	log *slog.Logger) (map[string]string, func(), error) {
	arts := a.GetSpec().GetPlacement().GetArtifacts()
	noop := func() {}
	if len(arts) == 0 || r.Cache == nil || kind == executor.KubernetesKind || kind == executor.KubernetesKindAlias {
		return nil, noop, nil
	}
	var leases []*artifactcache.Lease
	release := func() {
		for _, l := range leases {
			l.Release()
		}
	}
	paths := make(map[string]string, len(arts))
	for _, art := range arts {
		start := time.Now()
		l, err := r.Cache.Acquire(ctx, artifactcache.Artifact{
			Name: art.GetName(), Key: art.GetKey(), URI: art.GetUri(), SizeBytes: art.GetSizeBytes(),
		})
		if err != nil {
			release()
			return nil, noop, fmt.Errorf("artifact %q: %w", art.GetName(), err)
		}
		leases = append(leases, l)
		paths[art.GetName()] = l.Path
		log.Info("artifact ready", "artifact", art.GetName(), "key", l.Key, "cache_hit", l.Hit,
			"took", time.Since(start).Round(time.Millisecond))
	}
	return paths, release, nil
}

// classifyArtifactError maps a cache failure onto the retry taxonomy. A digest
// that does not match what the author declared is VALIDATION: fetching the same
// URI again returns the same wrong bytes. A cache too small or too full is an
// INFRASTRUCTURE fault of this node, and another worker may well have room.
// Anything else (the origin was unreachable) is TRANSIENT.
func classifyArtifactError(err error) executor.Classified {
	c := executor.Classified{Type: "ArtifactUnavailable", Message: err.Error()}
	switch {
	case errors.Is(err, artifactcache.ErrIntegrity), errors.Is(err, artifactcache.ErrInvalidArtifact),
		errors.Is(err, artifactcache.ErrUnsupportedScheme):
		c.Class, c.Type = nexusv1.ErrorClass_ERROR_CLASS_VALIDATION, "ArtifactInvalid"
	case errors.Is(err, artifactcache.ErrTooLarge), errors.Is(err, artifactcache.ErrNoSpace):
		c.Class, c.Retryable = nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE, true
	default:
		c.Class, c.Retryable = nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT, true
	}
	return c
}

// Preempt stops an attempt because this node is being reclaimed (ADR-0030 §2):
// the task gets SIGTERM -- its checkpoint signal -- and grace, then SIGKILL, and
// its outcome is reported as PREEMPTED unless it exits 0. An uninterruptible
// task (ADR-0015) is not signalled; it is waited on for at most the same grace,
// because the node will be gone after it regardless of what the task declared.
func (r *Runner) Preempt(attemptID string, grace time.Duration) {
	r.mu.Lock()
	rt, ok := r.running[attemptID]
	r.mu.Unlock()
	if !ok {
		return
	}
	if !rt.preempted.CompareAndSwap(false, true) {
		return // already being preempted
	}
	r.Metrics.TaskPreempted()
	log := r.log.With("attempt_id", attemptID, "grace", grace)
	if !rt.policy.GetUninterruptible() {
		log.Warn("preempting attempt: node is being reclaimed")
		r.Cancel(attemptID, grace, "preempted")
		return
	}
	log.Warn("node is being reclaimed; uninterruptible task gets until the eviction deadline, then SIGKILL")
	go func() {
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-rt.done:
			return
		case <-t.C:
		}
		rt.handleMu.Lock()
		h, exec := rt.handle, rt.exec
		rt.handleMu.Unlock()
		if h != nil && exec != nil {
			_ = exec.Signal(context.Background(), *h, executor.SignalKill)
		}
		rt.cancel()
	}()
}

// RunningAttempts lists what is running, for an eviction drain.
func (r *Runner) RunningAttempts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.running))
	for id := range r.running {
		out = append(out, id)
	}
	return out
}

// preempted reports whether the attempt was stopped by Preempt.
func (r *Runner) preempted(attemptID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rt, ok := r.running[attemptID]
	return ok && rt.preempted.Load()
}

// asPreempted rewrites a non-successful report of a preempted attempt into the
// PREEMPTED class, which the control plane retries against its own budget.
func asPreempted(req *nexusv1.ReportResultRequest) {
	req.Outcome = nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED
	msg := "node reclaimed (spot/preemptible eviction); the task was sent its checkpoint signal"
	if prev := req.GetError().GetMessage(); prev != "" {
		msg += ": " + prev
	}
	req.Error = &nexusv1.TaskError{
		Class: nexusv1.ErrorClass_ERROR_CLASS_PREEMPTED, Type: "Preempted", Message: msg, Retryable: true,
	}
}
