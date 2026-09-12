// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package worker wires the agent's parts together: registration, the task
// registry that is the heartbeat's ground truth, the long-polling puller, the
// runner that drives an executor under a WAL, and the reporter.
package worker

import (
	"sort"
	"sync"

	"github.com/khuriwalgroup/kgf-nexus/internal/domain"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Registry tracks what this worker is actually running.
//
// It is the single source of two things the rest of the agent depends on: the
// running_attempt_ids the heartbeat reports as GROUND TRUTH (which is what lets
// the control plane detect and release leaked reservations), and the local free
// capacity used for admission.
//
// Every method takes the lock for a bounded, non-blocking critical section. That
// is a hard requirement, not a style preference: the heartbeat goroutine reads
// through here every 5s, and a lock held across task execution would let a wedged
// task delay the beat and get the worker wrongly declared dead.
type Registry struct {
	mu        sync.Mutex
	capacity  *nexusv1.ResourceVector
	committed *nexusv1.ResourceVector
	// active maps attempt_id to the resources it reserved locally. Keyed by
	// attempt_id (never by node) because two attempts of the same node can
	// legitimately be distinct rows.
	active map[string]*nexusv1.ResourceVector

	// draining stops admission without touching what is already running: a
	// DRAIN means "finish current work, accept none", not "abandon".
	draining bool
}

// NewRegistry builds a registry over the capacity advertised at registration.
func NewRegistry(capacity *nexusv1.ResourceVector) *Registry {
	return &Registry{
		capacity:  capacity,
		committed: &nexusv1.ResourceVector{},
		active:    make(map[string]*nexusv1.ResourceVector),
	}
}

// Admit performs the LOCAL admission check and commits the resources on success.
//
// Both the control plane and the worker verify capacity, which is redundant on
// purpose: the control plane's ledger can be momentarily stale (a completion it
// has not yet processed, a reconciliation mid-flight), and accepting work the
// node cannot actually hold is how a node OOMs and takes every sibling attempt
// with it. The check and the commit are one critical section so two concurrent
// admissions cannot both see the same free capacity and both succeed.
func (r *Registry) Admit(attemptID string, request *nexusv1.ResourceVector) (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.draining {
		return false, "worker_draining"
	}
	if _, dup := r.active[attemptID]; dup {
		// Already ours. Re-admitting would double-count the reservation and
		// permanently shrink this worker's apparent capacity.
		return false, "already_running"
	}
	ok, short := domain.Fits(r.capacity, r.committed, request)
	if !ok {
		return false, domain.ShortfallReason(short)
	}
	r.active[attemptID] = request
	r.committed = domain.AddVec(r.committed, request)
	return true, ""
}

// Release frees an attempt's locally committed resources. Idempotent: release is
// called on every terminal path, and a second call must not drive committed
// negative (domain.SubVec clamps, but not double-subtracting is better).
func (r *Registry) Release(attemptID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	req, ok := r.active[attemptID]
	if !ok {
		return
	}
	delete(r.active, attemptID)
	r.committed = domain.SubVec(r.committed, req)
}

// RunningAttemptIDs is the heartbeat's ground truth. Sorted so the wire payload
// and test assertions are deterministic.
func (r *Registry) RunningAttemptIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.active))
	for id := range r.active {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// FreeCapacity reports capacity minus local commitments.
func (r *Registry) FreeCapacity() *nexusv1.ResourceVector {
	r.mu.Lock()
	defer r.mu.Unlock()
	return domain.SubVec(r.capacity, r.committed)
}

// Utilization reports observed load as a fraction of advertised capacity.
//
// Derived from committed reservations rather than sampled from the OS: the
// control plane uses this for placement hints, and a reservation-based figure is
// the one that matches its own ledger. A sampled figure would disagree with the
// scheduler's model and make placement decisions harder to explain.
func (r *Registry) Utilization() (cpu, mem float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.capacity.GetCpuMillis(); c > 0 {
		cpu = float64(r.committed.GetCpuMillis()) / float64(c)
	}
	if m := r.capacity.GetMemoryBytes(); m > 0 {
		mem = float64(r.committed.GetMemoryBytes()) / float64(m)
	}
	return cpu, mem
}

// SetDraining stops admission. Existing work continues to completion, per the
// DRAINING state in §5.5.
func (r *Registry) SetDraining(d bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.draining = d
}

// Draining reports whether admission is closed.
func (r *Registry) Draining() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.draining
}

// Count reports the number of active attempts.
func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.active)
}

// Idle reports whether a draining worker may now exit.
func (r *Registry) Idle() bool { return r.Count() == 0 }
