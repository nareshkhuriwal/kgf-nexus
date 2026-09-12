// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"math"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func rv(cpu, mem int64, gpu int32, kind string, gpuMem, disk int64, custom map[string]int64) *nexusv1.ResourceVector {
	return &nexusv1.ResourceVector{
		CpuMillis: cpu, MemoryBytes: mem, GpuCount: gpu,
		GpuKind: kind, GpuMemoryBytes: gpuMem, DiskBytes: disk, Custom: custom,
	}
}

func TestAddVec(t *testing.T) {
	tests := []struct {
		name    string
		a, b    *nexusv1.ResourceVector
		want    *nexusv1.ResourceVector
		wantCus map[string]int64
	}{
		{
			name: "nil operands are the identity",
			a:    nil, b: nil,
			want: rv(0, 0, 0, "", 0, 0, nil),
		},
		{
			name: "quantities sum",
			a:    rv(1000, 1<<30, 1, "", 0, 100, nil),
			b:    rv(2000, 2<<30, 3, "", 0, 200, nil),
			want: rv(3000, 3<<30, 4, "", 0, 300, nil),
		},
		{
			name: "gpu kind is a constraint and is not concatenated",
			a:    rv(0, 0, 1, "nvidia-a100", 40<<30, 0, nil),
			b:    rv(0, 0, 1, "nvidia-a100", 40<<30, 0, nil),
			want: rv(0, 0, 2, "nvidia-a100", 40<<30, 0, nil),
		},
		{
			name: "left kind wins; right fills only when left is empty",
			a:    rv(0, 0, 1, "", 0, 0, nil),
			b:    rv(0, 0, 1, "nvidia-h100", 80<<30, 0, nil),
			want: rv(0, 0, 2, "nvidia-h100", 80<<30, 0, nil),
		},
		{
			name:    "custom dimensions sum per key",
			a:       rv(0, 0, 0, "", 0, 0, map[string]int64{"license": 1, "nic": 2}),
			b:       rv(0, 0, 0, "", 0, 0, map[string]int64{"license": 2}),
			want:    rv(0, 0, 0, "", 0, 0, nil),
			wantCus: map[string]int64{"license": 3, "nic": 2},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := AddVec(tc.a, tc.b)
			assertScalars(t, got, tc.want)
			assertCustom(t, got.GetCustom(), tc.wantCus)
		})
	}
}

func TestSubVecClampsAtZero(t *testing.T) {
	tests := []struct {
		name    string
		a, b    *nexusv1.ResourceVector
		want    *nexusv1.ResourceVector
		wantCus map[string]int64
	}{
		{
			name: "normal release",
			a:    rv(4000, 8<<30, 4, "nvidia-a100", 40<<30, 500, nil),
			b:    rv(1000, 2<<30, 1, "", 0, 100, nil),
			want: rv(3000, 6<<30, 3, "nvidia-a100", 40<<30, 400, nil),
		},
		{
			// A double release by two racing reconcilers must not drive
			// committed negative: that would inflate apparent free capacity
			// forever and permanently over-commit the worker.
			name: "over-release clamps rather than going negative",
			a:    rv(1000, 1<<30, 1, "", 0, 10, nil),
			b:    rv(5000, 9<<30, 9, "", 0, 90, nil),
			want: rv(0, 0, 0, "", 0, 0, nil),
		},
		{
			name:    "custom over-release clamps",
			a:       rv(0, 0, 0, "", 0, 0, map[string]int64{"license": 1}),
			b:       rv(0, 0, 0, "", 0, 0, map[string]int64{"license": 5}),
			want:    rv(0, 0, 0, "", 0, 0, nil),
			wantCus: map[string]int64{"license": 0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SubVec(tc.a, tc.b)
			assertScalars(t, got, tc.want)
			assertCustom(t, got.GetCustom(), tc.wantCus)
		})
	}
}

func TestSubVecDoesNotMutateOperands(t *testing.T) {
	// The scheduler probes many candidate workers with the same capacity
	// vector; mutation here would corrupt the placement scan.
	a := rv(1000, 0, 0, "", 0, 0, map[string]int64{"license": 2})
	b := rv(400, 0, 0, "", 0, 0, map[string]int64{"license": 1})
	_ = SubVec(a, b)
	if a.GetCpuMillis() != 1000 || a.GetCustom()["license"] != 2 {
		t.Fatalf("SubVec mutated its left operand: %v", a)
	}
	if b.GetCpuMillis() != 400 || b.GetCustom()["license"] != 1 {
		t.Fatalf("SubVec mutated its right operand: %v", b)
	}
}

func TestFits(t *testing.T) {
	cap4GPU := rv(16000, 64<<30, 4, "nvidia-a100", 40<<30, 1000, map[string]int64{"license": 2})

	tests := []struct {
		name             string
		capacity         *nexusv1.ResourceVector
		committed        *nexusv1.ResourceVector
		request          *nexusv1.ResourceVector
		wantOK           bool
		wantDims         []string
		wantReasonSubstr string
	}{
		{
			name: "empty request fits an empty worker", capacity: rv(0, 0, 0, "", 0, 0, nil),
			committed: nil, request: nil, wantOK: true,
		},
		{
			name:     "fits exactly at the boundary",
			capacity: cap4GPU, committed: rv(12000, 56<<30, 3, "", 0, 900, map[string]int64{"license": 1}),
			request: rv(4000, 8<<30, 1, "", 0, 100, map[string]int64{"license": 1}),
			wantOK:  true,
		},
		{
			// The §7.3 worked example: 4 GPUs total, 1 committed, 3 free,
			// T1 needs 4.
			name:     "gpu shortfall reports need and free",
			capacity: cap4GPU, committed: rv(0, 0, 1, "", 0, 0, nil),
			request:          rv(0, 0, 4, "", 0, 0, nil),
			wantOK:           false,
			wantDims:         []string{DimGPU},
			wantReasonSubstr: "waiting_for_resources: gpu (need 4, free 3)",
		},
		{
			name:     "all failing dimensions are reported, not just the first",
			capacity: cap4GPU, committed: rv(15000, 63<<30, 4, "", 0, 999, nil),
			request:  rv(4000, 8<<30, 2, "", 0, 500, nil),
			wantOK:   false,
			wantDims: []string{DimCPU, DimDisk, DimGPU, DimMemory},
		},
		{
			name:     "empty requested gpu kind matches any worker gpu",
			capacity: cap4GPU, committed: nil,
			request: rv(0, 0, 1, "", 0, 0, nil),
			wantOK:  true,
		},
		{
			name:     "non-empty gpu kind must match exactly",
			capacity: cap4GPU, committed: nil,
			request:  rv(0, 0, 1, "nvidia-h100", 0, 0, nil),
			wantOK:   false,
			wantDims: []string{DimGPUKind},
		},
		{
			name:     "matching gpu kind is accepted",
			capacity: cap4GPU, committed: nil,
			request: rv(0, 0, 1, "nvidia-a100", 0, 0, nil),
			wantOK:  true,
		},
		{
			// A CPU-only task must not be rejected for a GPU model mismatch on
			// a GPU worker, nor for lack of VRAM.
			name:     "gpu constraints are ignored when no gpu is requested",
			capacity: rv(16000, 64<<30, 0, "", 0, 0, nil), committed: nil,
			request: rv(1000, 1<<30, 0, "nvidia-a100", 80<<30, 0, nil),
			wantOK:  true,
		},
		{
			name:     "vram floor is compared against capacity not free",
			capacity: cap4GPU, committed: rv(0, 0, 3, "", 0, 0, nil),
			request: rv(0, 0, 1, "", 40<<30, 0, nil),
			wantOK:  true,
		},
		{
			name:     "vram floor above the device model is rejected",
			capacity: cap4GPU, committed: nil,
			request:  rv(0, 0, 1, "", 80<<30, 0, nil),
			wantOK:   false,
			wantDims: []string{DimGPUMemory},
		},
		{
			name:     "custom dimension exhausted",
			capacity: cap4GPU, committed: rv(0, 0, 0, "", 0, 0, map[string]int64{"license": 2}),
			request:  rv(0, 0, 0, "", 0, 0, map[string]int64{"license": 1}),
			wantOK:   false,
			wantDims: []string{"custom:license"},
		},
		{
			// An unknown custom key means zero available, never
			// "unconstrained": placing a task on a worker that cannot honour
			// its requirement is worse than leaving it PENDING.
			name:     "unknown custom key on the worker means unavailable",
			capacity: rv(16000, 64<<30, 0, "", 0, 0, nil), committed: nil,
			request:  rv(0, 0, 0, "", 0, 0, map[string]int64{"fpga": 1}),
			wantOK:   false,
			wantDims: []string{"custom:fpga"},
		},
		{
			name:     "zero-valued custom request is not a constraint",
			capacity: rv(16000, 0, 0, "", 0, 0, nil), committed: nil,
			request: rv(0, 0, 0, "", 0, 0, map[string]int64{"fpga": 0}),
			wantOK:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, short := Fits(tc.capacity, tc.committed, tc.request)
			if ok != tc.wantOK {
				t.Fatalf("Fits = %v (shortfall %v), want %v", ok, short, tc.wantOK)
			}
			if tc.wantDims != nil {
				if len(short) != len(tc.wantDims) {
					t.Fatalf("shortfall dims = %v, want %v", dims(short), tc.wantDims)
				}
				for i, d := range tc.wantDims {
					if short[i].Dim != d {
						t.Fatalf("shortfall dims = %v, want %v", dims(short), tc.wantDims)
					}
				}
			}
			if tc.wantReasonSubstr != "" {
				if got := ShortfallReason(short); got != tc.wantReasonSubstr {
					t.Fatalf("ShortfallReason = %q, want %q", got, tc.wantReasonSubstr)
				}
			}
		})
	}
}

func TestShortfallReasonEmptyWhenFitting(t *testing.T) {
	if got := ShortfallReason(nil); got != "" {
		t.Fatalf("ShortfallReason(nil) = %q, want empty", got)
	}
}

func TestGPUKindMatches(t *testing.T) {
	tests := []struct {
		worker, request string
		want            bool
	}{
		{"nvidia-a100", "", true},
		{"", "", true},
		{"nvidia-a100", "nvidia-a100", true},
		{"NVIDIA-A100", "nvidia-a100", true},
		// Exact, not prefix: a100 and a100-80gb are different hardware with
		// different performance, and a prefix match places jobs on the wrong one.
		{"nvidia-a100-80gb", "nvidia-a100", false},
		{"nvidia-a100", "nvidia-h100", false},
		{"", "nvidia-a100", false},
	}
	for _, tc := range tests {
		if got := GPUKindMatches(tc.worker, tc.request); got != tc.want {
			t.Errorf("GPUKindMatches(%q,%q) = %v, want %v", tc.worker, tc.request, got, tc.want)
		}
	}
}

func TestBestFitScore(t *testing.T) {
	req1GPU := rv(1000, 1<<30, 1, "", 0, 0, nil)

	t.Run("unfittable worker scores +Inf so it can never win", func(t *testing.T) {
		capacity := rv(1000, 1<<30, 1, "", 0, 0, nil)
		got := BestFitScore(capacity, rv(0, 0, 1, "", 0, 0, nil), req1GPU)
		if !math.IsInf(got, 1) {
			t.Fatalf("BestFitScore = %v, want +Inf", got)
		}
	})

	t.Run("tighter worker scores lower", func(t *testing.T) {
		// Two workers that both fit a 1-GPU task: one already has 3 of 4 GPUs
		// committed (tight), one is empty (loose). Best-fit must prefer the
		// tight one so the empty worker stays available for a 4-GPU task.
		tight := BestFitScore(
			rv(16000, 64<<30, 4, "", 0, 0, nil), rv(12000, 48<<30, 3, "", 0, 0, nil), req1GPU)
		loose := BestFitScore(
			rv(16000, 64<<30, 4, "", 0, 0, nil), nil, req1GPU)
		if !(tight < loose) {
			t.Fatalf("tight=%v loose=%v; best-fit must rank the tighter worker lower", tight, loose)
		}
	})

	t.Run("exact fit scores zero", func(t *testing.T) {
		got := BestFitScore(rv(1000, 1<<30, 1, "", 0, 0, nil), nil, req1GPU)
		if got != 0 {
			t.Fatalf("BestFitScore for an exact fit = %v, want 0", got)
		}
	})

	t.Run("gpu fragmentation outweighs cpu slack", func(t *testing.T) {
		// Worker A: exactly enough GPU, lots of spare CPU.
		// Worker B: lots of spare GPU, exactly enough CPU.
		// GPUs are the scarce indivisible dimension, so A must win.
		a := BestFitScore(rv(64000, 64<<30, 1, "", 0, 0, nil), nil, req1GPU)
		b := BestFitScore(rv(1000, 1<<30, 8, "", 0, 0, nil), nil, req1GPU)
		if !(a < b) {
			t.Fatalf("a=%v b=%v; GPU tightness must dominate CPU tightness", a, b)
		}
	})

	t.Run("empty request is total and finite", func(t *testing.T) {
		got := BestFitScore(rv(1000, 0, 0, "", 0, 0, nil), nil, nil)
		if math.IsNaN(got) || math.IsInf(got, 0) {
			t.Fatalf("BestFitScore for an empty request = %v, want a finite value", got)
		}
	})

	t.Run("custom dimension participates", func(t *testing.T) {
		req := rv(0, 0, 0, "", 0, 0, map[string]int64{"license": 1})
		tight := BestFitScore(rv(0, 0, 0, "", 0, 0, map[string]int64{"license": 1}), nil, req)
		loose := BestFitScore(rv(0, 0, 0, "", 0, 0, map[string]int64{"license": 8}), nil, req)
		if !(tight < loose) {
			t.Fatalf("tight=%v loose=%v", tight, loose)
		}
	})
}

func TestIsZeroVec(t *testing.T) {
	tests := []struct {
		name string
		v    *nexusv1.ResourceVector
		want bool
	}{
		{"nil", nil, true},
		{"empty", rv(0, 0, 0, "", 0, 0, nil), true},
		{"kind only is still zero cost", rv(0, 0, 0, "nvidia-a100", 40<<30, 0, nil), true},
		{"zero custom values", rv(0, 0, 0, "", 0, 0, map[string]int64{"x": 0}), true},
		{"cpu", rv(1, 0, 0, "", 0, 0, nil), false},
		{"gpu", rv(0, 0, 1, "", 0, 0, nil), false},
		{"custom", rv(0, 0, 0, "", 0, 0, map[string]int64{"x": 1}), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsZeroVec(tc.v); got != tc.want {
				t.Fatalf("IsZeroVec = %v, want %v", got, tc.want)
			}
		})
	}
}

func dims(s []Shortfall) []string {
	out := make([]string, 0, len(s))
	for _, x := range s {
		out = append(out, x.Dim)
	}
	return out
}

func assertScalars(t *testing.T, got, want *nexusv1.ResourceVector) {
	t.Helper()
	if got.GetCpuMillis() != want.GetCpuMillis() ||
		got.GetMemoryBytes() != want.GetMemoryBytes() ||
		got.GetGpuCount() != want.GetGpuCount() ||
		got.GetDiskBytes() != want.GetDiskBytes() ||
		got.GetGpuKind() != want.GetGpuKind() ||
		got.GetGpuMemoryBytes() != want.GetGpuMemoryBytes() {
		t.Fatalf("scalars = %v, want %v", got, want)
	}
}

func assertCustom(t *testing.T, got, want map[string]int64) {
	t.Helper()
	if len(want) == 0 {
		return
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("custom[%q] = %d, want %d (full: %v)", k, got[k], v, got)
		}
	}
}

// Releasing everything a worker held must return the ledger to zero on EVERY
// dimension. AddVec latches gpu_kind and gpu_memory_bytes onto an empty
// committed vector, so SubVec has to un-latch them or the pair is not
// reversible and the constraint is committed forever.
//
// The live symptom this pins: both GPU workers sat at 100% of capacity
// committed with ZERO reservation rows behind it, and no reconciler could
// correct it — every repair path works by finding reservations to release, and
// there were none left to find.
func TestSubVecReturnsTheLedgerToZero(t *testing.T) {
	req := rv(1000, 1<<30, 1, "nvidia-a100", 42949672960, 0, nil)

	got := SubVec(AddVec(&nexusv1.ResourceVector{}, req), req)

	assertScalars(t, got, &nexusv1.ResourceVector{})
}

// The constraint is released only when the operands MATCH. A release carrying a
// different kind, or no kind at all, must not clear a constraint that some
// other live reservation still depends on.
func TestSubVecKeepsAConstraintItDidNotCommit(t *testing.T) {
	committed := rv(2000, 0, 2, "nvidia-a100", 42949672960, 0, nil)

	// A release that names no GPU constraint at all leaves both in place.
	cpuOnly := SubVec(committed, rv(1000, 0, 0, "", 0, 0, nil))
	if cpuOnly.GetGpuKind() != "nvidia-a100" || cpuOnly.GetGpuMemoryBytes() != 42949672960 {
		t.Errorf("a CPU-only release dropped a GPU constraint: %v", cpuOnly)
	}

	// A release naming a DIFFERENT kind leaves the committed one alone.
	otherKind := SubVec(committed, rv(0, 0, 1, "nvidia-a10", 25769803776, 0, nil))
	if otherKind.GetGpuKind() != "nvidia-a100" || otherKind.GetGpuMemoryBytes() != 42949672960 {
		t.Errorf("a mismatched release dropped the committed constraint: %v", otherKind)
	}
}

// Fits computes free as SubVec(capacity, committed), so un-latching must not
// make an occupied worker stop matching VRAM requirements: per-device VRAM is a
// floor on the hardware, compared against capacity and never against free.
func TestGPUWorkerStillTakesASecondTaskAfterTheFix(t *testing.T) {
	capacity := rv(4000, 34359738368, 2, "nvidia-a100", 42949672960, 0, nil)
	one := rv(1000, 1<<30, 1, "nvidia-a100", 42949672960, 0, nil)

	committed := AddVec(&nexusv1.ResourceVector{}, one)

	if ok, short := Fits(capacity, committed, one); !ok {
		t.Errorf("a second GPU task no longer fits an idle-enough worker: %v", dims(short))
	}
}
