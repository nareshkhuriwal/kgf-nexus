// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"sort"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Fractional GPUs (ADR-0029).
//
// Three device modes share one vector:
//
//   - whole devices: gpu_count, unchanged;
//   - MIG instances: mig, a count per profile, pooled per profile because every
//     instance of a profile is interchangeable;
//   - shared devices (MPS or time-slicing): gpu_shares, one entry per device,
//     with compute in thousandths and VRAM in bytes SUBTRACTED PER DEVICE.
//
// The per-device arithmetic is the point. A pooled VRAM total would admit two
// 30 GB jobs onto three devices with 20 GB free each; a per-device ledger
// cannot. Place returns the request PLUS the device it chose, and that placed
// vector is what the claim writes to reservations.resources and adds to
// workers.committed -- so the existing release paths, which subtract the
// reservation row, return exactly what was taken.

// Dimension names for fractional-GPU shortfalls, as they appear in
// pending_reason.
const (
	// DimMIGPrefix prefixes a MIG profile shortfall: "mig:1g.10gb".
	DimMIGPrefix = "mig:"
	// DimGPUShare is a shared-device shortfall: no single shared device has
	// both the compute and the memory the request needs.
	DimGPUShare = "gpu_share"
)

// PlacementPolicy chooses between packing and spreading, for both the worker
// (push path score, pull path deferral) and the shared device inside a worker.
type PlacementPolicy string

const (
	// PolicyPack is best-fit / least waste: fill partly used workers and
	// devices first, keeping whole ones free for large requests. The default,
	// and what BestFitScore always did on the push path.
	PolicyPack PlacementPolicy = "pack"
	// PolicySpread is worst-fit: balance load, trading fragmentation for less
	// contention between co-located jobs.
	PolicySpread PlacementPolicy = "spread"
)

// ParsePlacementPolicy reads a policy name; anything unrecognised is pack, the
// historical behaviour.
func ParsePlacementPolicy(s string) PlacementPolicy {
	if s == string(PolicySpread) {
		return PolicySpread
	}
	return PolicyPack
}

// UsesGPU reports whether a request asks for any form of GPU.
func UsesGPU(req *nexusv1.ResourceVector) bool {
	return req.GetGpuCount() > 0 || UsesSharedGPU(req) || usesMIG(req)
}

// UsesSharedGPU reports whether a request asks for a slice of a shared device.
func UsesSharedGPU(req *nexusv1.ResourceVector) bool {
	return req.GetGpuShareMilli() > 0 || req.GetGpuShareMemoryBytes() > 0
}

func usesMIG(req *nexusv1.ResourceVector) bool {
	for _, n := range req.GetMig() {
		if n > 0 {
			return true
		}
	}
	return false
}

// fractionalShortfalls is the MIG and shared-device half of Fits.
func fractionalShortfalls(capacity, committed, request *nexusv1.ResourceVector) []Shortfall {
	var short []Shortfall
	for profile, need := range request.GetMig() {
		if need <= 0 {
			continue
		}
		free := capacity.GetMig()[profile] - committed.GetMig()[profile]
		if free < 0 {
			free = 0
		}
		if need > free {
			short = append(short, Shortfall{Dim: DimMIGPrefix + profile, Need: need, Free: free})
		}
	}
	if UsesSharedGPU(request) {
		if _, ok := chooseSharedDevice(capacity, committed, request, PolicyPack); !ok {
			short = append(short, sharedShortfall(capacity, committed, request))
		}
	}
	return short
}

// sharedShortfall explains why no shared device fits, naming the device that
// came closest so an operator can tell "all full right now" from "too big for
// any device here".
func sharedShortfall(capacity, committed, request *nexusv1.ResourceVector) Shortfall {
	needM, needB := int64(request.GetGpuShareMilli()), request.GetGpuShareMemoryBytes()
	bestKey, bestM, bestB := "", int64(-1), int64(-1)
	for _, k := range sortedShareKeys(capacity.GetGpuShares()) {
		m, b := shareFree(capacity, committed, k)
		if b > bestB || (b == bestB && m > bestM) {
			bestKey, bestM, bestB = k, m, b
		}
	}
	if bestKey == "" {
		return Shortfall{Dim: DimGPUShare,
			Detail: fmt.Sprintf("need %d milli and %d bytes on one shared GPU, worker has no shared GPUs", needM, needB)}
	}
	return Shortfall{Dim: DimGPUShare, Need: needB, Free: bestB,
		Detail: fmt.Sprintf("need %d milli and %d bytes on one shared GPU; most free is device %s with %d milli and %d bytes",
			needM, needB, bestKey, bestM, bestB)}
}

// shareFree is one shared device's free compute and memory.
func shareFree(capacity, committed *nexusv1.ResourceVector, key string) (milli, bytes int64) {
	c := capacity.GetGpuShares()[key]
	u := committed.GetGpuShares()[key]
	milli = c.GetMilli() - u.GetMilli()
	bytes = c.GetMemoryBytes() - u.GetMemoryBytes()
	if milli < 0 {
		milli = 0
	}
	if bytes < 0 {
		bytes = 0
	}
	return milli, bytes
}

// chooseSharedDevice picks the shared device a request is placed on, or
// reports that none fits. Deterministic: ties break on the device key, so the
// same ledger always yields the same choice.
//
// pack picks the device left with the LEAST memory (then compute) after
// placement -- best fit, which keeps whole devices free for big requests.
// spread picks the one left with the most.
func chooseSharedDevice(capacity, committed, request *nexusv1.ResourceVector, policy PlacementPolicy) (string, bool) {
	needM, needB := int64(request.GetGpuShareMilli()), request.GetGpuShareMemoryBytes()
	best, found := "", false
	var bestM, bestB int64
	for _, k := range sortedShareKeys(capacity.GetGpuShares()) {
		m, b := shareFree(capacity, committed, k)
		if needM > m || needB > b {
			continue
		}
		leftM, leftB := m-needM, b-needB
		better := !found
		if found {
			switch policy {
			case PolicySpread:
				better = leftB > bestB || (leftB == bestB && leftM > bestM)
			default:
				better = leftB < bestB || (leftB == bestB && leftM < bestM)
			}
		}
		if better {
			best, bestM, bestB, found = k, leftM, leftB, true
		}
	}
	return best, found
}

func sortedShareKeys(m map[string]*nexusv1.GPUShare) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Place is Fits plus the device choice: it returns the vector to RESERVE, which
// is the request with the chosen shared device recorded in gpu_shares.
//
// The claim must reserve the placed vector, not the request: release subtracts
// reservations.resources, and only the placed vector names the device whose
// ledger entry the release must decrement.
func Place(capacity, committed, request *nexusv1.ResourceVector, policy PlacementPolicy) (*nexusv1.ResourceVector, bool, []Shortfall) {
	ok, short := Fits(capacity, committed, request)
	if !ok {
		return nil, false, short
	}
	placed := CloneVec(request)
	placed.GpuShares = nil
	if UsesSharedGPU(request) {
		dev, found := chooseSharedDevice(capacity, committed, request, policy)
		if !found {
			// Fits already proved a device exists; reaching here means the
			// ledger changed under us, which it cannot under the row lock.
			return nil, false, []Shortfall{sharedShortfall(capacity, committed, request)}
		}
		placed.GpuShares = map[string]*nexusv1.GPUShare{dev: {
			Milli:       int64(request.GetGpuShareMilli()),
			MemoryBytes: request.GetGpuShareMemoryBytes(),
		}}
	}
	return placed, true, nil
}

// SharedDevice returns the one shared device a placed vector names, if any.
func SharedDevice(placed *nexusv1.ResourceVector) (string, bool) {
	for k := range placed.GetGpuShares() {
		return k, true
	}
	return "", false
}

// CloneVec deep-copies a vector so callers can mutate the copy.
func CloneVec(v *nexusv1.ResourceVector) *nexusv1.ResourceVector {
	v = orZero(v)
	out := &nexusv1.ResourceVector{
		CpuMillis:           v.GetCpuMillis(),
		MemoryBytes:         v.GetMemoryBytes(),
		GpuCount:            v.GetGpuCount(),
		GpuKind:             v.GetGpuKind(),
		GpuMemoryBytes:      v.GetGpuMemoryBytes(),
		DiskBytes:           v.GetDiskBytes(),
		GpuShareMilli:       v.GetGpuShareMilli(),
		GpuShareMemoryBytes: v.GetGpuShareMemoryBytes(),
	}
	if len(v.GetCustom()) > 0 {
		out.Custom = make(map[string]int64, len(v.GetCustom()))
		for k, n := range v.GetCustom() {
			out.Custom[k] = n
		}
	}
	if len(v.GetMig()) > 0 {
		out.Mig = make(map[string]int64, len(v.GetMig()))
		for k, n := range v.GetMig() {
			out.Mig[k] = n
		}
	}
	if len(v.GetGpuShares()) > 0 {
		out.GpuShares = make(map[string]*nexusv1.GPUShare, len(v.GetGpuShares()))
		for k, s := range v.GetGpuShares() {
			out.GpuShares[k] = &nexusv1.GPUShare{Milli: s.GetMilli(), MemoryBytes: s.GetMemoryBytes()}
		}
	}
	return out
}

// mergeShares folds b's per-device shares into a with the given sign, clamping
// each device at zero (the SubVec double-release argument, per device). On
// subtraction a device of `a` that reaches zero is dropped, so releasing every
// reservation leaves no residue in workers.committed.
func mergeShares(a, b map[string]*nexusv1.GPUShare, sign int64) map[string]*nexusv1.GPUShare {
	if len(a) == 0 && (len(b) == 0 || sign < 0) {
		return nil
	}
	out := make(map[string]*nexusv1.GPUShare, len(a)+len(b))
	for k, s := range a {
		out[k] = &nexusv1.GPUShare{Milli: s.GetMilli(), MemoryBytes: s.GetMemoryBytes()}
	}
	for k, s := range b {
		cur := out[k]
		if cur == nil {
			if sign < 0 {
				// Subtracting a device the left side does not have: nothing to
				// take from, and creating a zero entry would be noise.
				continue
			}
			cur = &nexusv1.GPUShare{}
		}
		cur.Milli = max64(cur.GetMilli()+sign*s.GetMilli(), 0)
		cur.MemoryBytes = max64(cur.GetMemoryBytes()+sign*s.GetMemoryBytes(), 0)
		out[k] = cur
		if sign < 0 && cur.Milli == 0 && cur.MemoryBytes == 0 {
			delete(out, k)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// fractionalFitFraction adds the best-fit contribution of the fractional GPU
// dimensions: the fraction of each that would remain free after placement.
func fractionalFitFraction(capacity, committed, request *nexusv1.ResourceVector, policy PlacementPolicy,
	add func(need, avail int64, w float64)) {
	const gpuWeight = 4.0
	for profile, need := range request.GetMig() {
		free := capacity.GetMig()[profile] - committed.GetMig()[profile]
		add(need, free, gpuWeight)
	}
	if UsesSharedGPU(request) {
		if dev, ok := chooseSharedDevice(capacity, committed, request, policy); ok {
			m, b := shareFree(capacity, committed, dev)
			add(request.GetGpuShareMemoryBytes(), b, gpuWeight)
			add(int64(request.GetGpuShareMilli()), m, gpuWeight)
		}
	}
}
