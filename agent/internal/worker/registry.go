// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package worker wires the agent's parts together: registration, the task
// registry that is the heartbeat's ground truth, the long-polling puller, the
// runner that drives an executor under a WAL, and the reporter.
package worker

import (
	"sort"
	"sync"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/gpu"
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

	// gpus maps attempt_id to the device indices it holds, and gpuHeld marks
	// which indices are taken. The ledger above only COUNTS GPUs; without
	// naming them, every GPU attempt was told to use devices 0..n-1, so two
	// concurrent 1-GPU attempts on a 2-GPU worker both ran on device 0 while
	// device 1 sat idle.
	gpus    map[string][]int
	gpuHeld map[int]bool

	// Fractional GPUs (ADR-0029 §2). inv names the MIG instances and shared
	// devices; migHeld marks instances taken and migOf maps an attempt to the
	// instances it holds. The shared-device ledger is committed.GpuShares:
	// an attempt's active vector is its PLACED vector, naming its device.
	inv     gpu.Inventory
	migHeld map[string]bool
	migOf   map[string][]string
	// MPSPipeDir is handed to executors for MPS-shared devices.
	MPSPipeDir string
}

// NewRegistry builds a registry over the capacity advertised at registration.
func NewRegistry(capacity *nexusv1.ResourceVector) *Registry {
	return &Registry{
		capacity:  capacity,
		committed: &nexusv1.ResourceVector{},
		active:    make(map[string]*nexusv1.ResourceVector),
		gpus:      make(map[string][]int),
		gpuHeld:   make(map[int]bool),
		migHeld:   make(map[string]bool),
		migOf:     make(map[string][]string),
	}
}

// NewRegistryWithGPUs is NewRegistry with a fractional-GPU inventory; the
// capacity must already include it (gpu.Inventory.ApplyTo).
func NewRegistryWithGPUs(capacity *nexusv1.ResourceVector, inv gpu.Inventory) *Registry {
	r := NewRegistry(capacity)
	r.inv = inv
	return r
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
	return r.AdmitReserved(attemptID, request, nil)
}

// AdmitReserved is Admit honouring the control plane's placement: reserved is
// TaskAssignment.reserved, which names the shared GPU device the control
// plane's ledger charged. The agent uses THAT device or refuses -- placing the
// slice elsewhere would make the control plane's per-device ledger and the
// real usage diverge, which is the over-allocation per-device accounting
// exists to prevent (ADR-0029 §2). A nil reserved (an older scheduler) places
// locally with the same rule the control plane uses.
func (r *Registry) AdmitReserved(attemptID string, request, reserved *nexusv1.ResourceVector) (bool, string) {
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
	placed, why := r.placeShared(request, reserved)
	if why != "" {
		return false, why
	}
	migs, why := r.pickMIG(request)
	if why != "" {
		return false, why
	}
	// Name the devices in the same critical section as the count, so two
	// concurrent admissions can never pick the same free device.
	var devices []int
	if n := int(request.GetGpuCount()); n > 0 {
		for d := 0; d < int(r.capacity.GetGpuCount()) && len(devices) < n; d++ {
			if !r.gpuHeld[d] {
				devices = append(devices, d)
			}
		}
		if len(devices) < n {
			// Fits passed on the count, so the held set disagrees with the
			// ledger. Refuse rather than hand out a device someone holds.
			return false, "gpu_devices_unavailable"
		}
		for _, d := range devices {
			r.gpuHeld[d] = true
		}
		r.gpus[attemptID] = devices
	}
	for _, u := range migs {
		r.migHeld[u] = true
	}
	if len(migs) > 0 {
		r.migOf[attemptID] = migs
	}
	r.active[attemptID] = placed
	r.committed = domain.AddVec(r.committed, placed)
	return true, ""
}

// placeShared resolves the shared-device slice. Called with r.mu held.
func (r *Registry) placeShared(request, reserved *nexusv1.ResourceVector) (*nexusv1.ResourceVector, string) {
	if !domain.UsesSharedGPU(request) {
		out := domain.CloneVec(request)
		out.GpuShares = nil
		return out, ""
	}
	dev, named := domain.SharedDevice(reserved)
	if !named {
		p, ok, short := domain.Place(r.capacity, r.committed, request, domain.PolicyPack)
		if !ok {
			return nil, domain.ShortfallReason(short)
		}
		return p, ""
	}
	// The named device must exist here and hold the slice by THIS ledger.
	c, ok := r.capacity.GetGpuShares()[dev]
	if !ok {
		return nil, "gpu_share_device_unknown: " + dev
	}
	only := domain.CloneVec(r.capacity)
	only.GpuShares = map[string]*nexusv1.GPUShare{dev: c}
	if fits, short := domain.Fits(only, r.committed, request); !fits {
		return nil, "gpu_share_device_full: " + dev + " " + domain.ShortfallReason(short)
	}
	out := domain.CloneVec(request)
	out.GpuShares = map[string]*nexusv1.GPUShare{dev: {
		Milli: int64(request.GetGpuShareMilli()), MemoryBytes: request.GetGpuShareMemoryBytes()}}
	return out, ""
}

// pickMIG names free MIG instances for the request. Called with r.mu held.
func (r *Registry) pickMIG(request *nexusv1.ResourceVector) ([]string, string) {
	var out []string
	profiles := make([]string, 0, len(request.GetMig()))
	for p := range request.GetMig() {
		profiles = append(profiles, p)
	}
	sort.Strings(profiles)
	for _, p := range profiles {
		need := request.GetMig()[p]
		for _, u := range r.inv.MIG[p] {
			if need == 0 {
				break
			}
			if !r.migHeld[u] {
				out = append(out, u)
				need--
			}
		}
		if need > 0 {
			// Fits passed on the count, so the named set disagrees with the
			// ledger. Refuse rather than hand out an instance someone holds.
			return nil, "mig_instances_unavailable: " + p
		}
	}
	return out, ""
}

// GPUAssignment is the fractional-GPU part of an admitted attempt, for the
// executor.
func (r *Registry) GPUAssignment(attemptID string) executor.GPUAssignment {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := executor.GPUAssignment{MIG: append([]string(nil), r.migOf[attemptID]...), MPSPipeDir: r.MPSPipeDir}
	placed := r.active[attemptID]
	if len(placed.GetMig()) > 0 {
		g.MIGProfiles = map[string]int64{}
		for p, n := range placed.GetMig() {
			g.MIGProfiles[p] = n
		}
	}
	if dev, ok := domain.SharedDevice(placed); ok {
		g.SharedDevice = dev
		g.SharedMode = r.inv.Shared[dev].Mode
		g.ShareMilli = int64(placed.GetGpuShareMilli())
		g.ShareMemoryBytes = placed.GetGpuShareMemoryBytes()
	}
	return g
}

// GPUDevices is the device indices an admitted attempt holds, lowest first, or
// nil if it holds none.
func (r *Registry) GPUDevices(attemptID string) []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.gpus[attemptID]...)
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
	for _, d := range r.gpus[attemptID] {
		delete(r.gpuHeld, d)
	}
	delete(r.gpus, attemptID)
	for _, u := range r.migOf[attemptID] {
		delete(r.migHeld, u)
	}
	delete(r.migOf, attemptID)
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
