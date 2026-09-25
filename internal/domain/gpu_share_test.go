// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

const gib = int64(1) << 30

func sharedCap(devs map[string]int64) map[string]*nexusv1.GPUShare {
	out := map[string]*nexusv1.GPUShare{}
	for k, mem := range devs {
		out[k] = &nexusv1.GPUShare{Milli: 1000, MemoryBytes: mem}
	}
	return out
}

func TestPlaceSharedPacksOntoOneDeviceAndReleasesExactly(t *testing.T) {
	capacity := &nexusv1.ResourceVector{CpuMillis: 64000, GpuKind: "nvidia-h100",
		GpuShares: sharedCap(map[string]int64{"0": 80 * gib, "1": 80 * gib})}
	committed := &nexusv1.ResourceVector{}
	req := &nexusv1.ResourceVector{CpuMillis: 1000, GpuShareMemoryBytes: 20 * gib, GpuShareMilli: 250}

	var placed []*nexusv1.ResourceVector
	for i := 0; i < 8; i++ {
		p, ok, short := Place(capacity, committed, req, PolicyPack)
		if !ok {
			t.Fatalf("placement %d refused: %v", i, short)
		}
		placed = append(placed, p)
		committed = AddVec(committed, p)
	}
	// pack: the first device fills (4 x 20 GiB) before the second is touched.
	for i, p := range placed {
		dev, _ := SharedDevice(p)
		want := "0"
		if i >= 4 {
			want = "1"
		}
		if dev != want {
			t.Fatalf("placement %d on device %s, want %s (pack fills one device first)", i, dev, want)
		}
	}
	if ok, short := Fits(capacity, committed, req); ok {
		t.Fatal("a ninth 20 GiB slice fit on two full 80 GiB devices")
	} else if !strings.Contains(ShortfallReason(short), "gpu_share") {
		t.Fatalf("reason does not name the shared dimension: %s", ShortfallReason(short))
	}
	for _, p := range placed {
		committed = SubVec(committed, p)
	}
	if !IsZeroVec(committed) || len(committed.GetGpuShares()) != 0 {
		t.Fatalf("releasing everything left residue: %+v", committed)
	}
}

func TestSpreadUsesTheEmptierDevice(t *testing.T) {
	capacity := &nexusv1.ResourceVector{GpuShares: sharedCap(map[string]int64{"0": 80 * gib, "1": 80 * gib})}
	req := &nexusv1.ResourceVector{GpuShareMemoryBytes: 10 * gib}
	committed := &nexusv1.ResourceVector{}
	p1, _, _ := Place(capacity, committed, req, PolicySpread)
	committed = AddVec(committed, p1)
	p2, _, _ := Place(capacity, committed, req, PolicySpread)
	d1, _ := SharedDevice(p1)
	d2, _ := SharedDevice(p2)
	if d1 == d2 {
		t.Fatalf("spread put both slices on device %s", d1)
	}
}

func TestSharedRequestMustFitOnOneDevice(t *testing.T) {
	// 30 GiB free in total but only 15 on each device: a pooled model would
	// admit a 20 GiB request; the per-device ledger must not.
	capacity := &nexusv1.ResourceVector{GpuShares: sharedCap(map[string]int64{"0": 40 * gib, "1": 40 * gib})}
	committed := &nexusv1.ResourceVector{GpuShares: map[string]*nexusv1.GPUShare{
		"0": {MemoryBytes: 25 * gib}, "1": {MemoryBytes: 25 * gib}}}
	if ok, _ := Fits(capacity, committed, &nexusv1.ResourceVector{GpuShareMemoryBytes: 20 * gib}); ok {
		t.Fatal("a 20 GiB slice fit although no device has more than 15 GiB free")
	}
	if ok, _ := Fits(capacity, committed, &nexusv1.ResourceVector{GpuShareMemoryBytes: 15 * gib}); !ok {
		t.Fatal("a 15 GiB slice did not fit on a device with exactly 15 GiB free")
	}
}

func TestComputeFractionIsSubtractedToo(t *testing.T) {
	capacity := &nexusv1.ResourceVector{GpuShares: sharedCap(map[string]int64{"0": 80 * gib})}
	committed := &nexusv1.ResourceVector{}
	req := &nexusv1.ResourceVector{GpuShareMilli: 400}
	for i := 0; i < 2; i++ {
		p, ok, _ := Place(capacity, committed, req, PolicyPack)
		if !ok {
			t.Fatalf("slice %d refused", i)
		}
		committed = AddVec(committed, p)
	}
	if ok, _ := Fits(capacity, committed, req); ok {
		t.Fatal("1200 milli fit on one device")
	}
}

func TestMIGProfilesArePooledPerProfile(t *testing.T) {
	capacity := &nexusv1.ResourceVector{GpuKind: "nvidia-a100", Mig: map[string]int64{"1g.10gb": 3, "3g.40gb": 1}}
	committed := &nexusv1.ResourceVector{}
	small := &nexusv1.ResourceVector{Mig: map[string]int64{"1g.10gb": 1}}
	for i := 0; i < 3; i++ {
		p, ok, short := Place(capacity, committed, small, PolicyPack)
		if !ok {
			t.Fatalf("1g.10gb #%d refused: %v", i, short)
		}
		committed = AddVec(committed, p)
	}
	ok, short := Fits(capacity, committed, small)
	if ok || ShortfallReason(short) != "waiting_for_resources: mig:1g.10gb (need 1, free 0)" {
		t.Fatalf("fourth 1g.10gb: ok=%v reason=%q", ok, ShortfallReason(short))
	}
	// Another profile is unaffected.
	if ok, _ := Fits(capacity, committed, &nexusv1.ResourceVector{Mig: map[string]int64{"3g.40gb": 1}}); !ok {
		t.Fatal("3g.40gb refused although its instance is free")
	}
	// A profile the worker does not have never fits.
	if ok, _ := Fits(capacity, &nexusv1.ResourceVector{}, &nexusv1.ResourceVector{Mig: map[string]int64{"2g.20gb": 1}}); ok {
		t.Fatal("a missing profile fit")
	}
	// And the model constraint applies to MIG.
	if ok, _ := Fits(capacity, &nexusv1.ResourceVector{}, &nexusv1.ResourceVector{GpuKind: "nvidia-h100",
		Mig: map[string]int64{"3g.40gb": 1}}); ok {
		t.Fatal("an H100 MIG request fit on an A100")
	}
}

func TestCPUOnlyRequestIgnoresGPUDimensions(t *testing.T) {
	capacity := &nexusv1.ResourceVector{CpuMillis: 1000}
	if ok, short := Fits(capacity, nil, &nexusv1.ResourceVector{CpuMillis: 500, GpuKind: "nvidia-a100"}); !ok {
		t.Fatalf("CPU-only request with a stray gpu_kind refused: %v", short)
	}
}

// TestPropertyNeverOverAllocate is the ADR-0029 invariant under random fleets
// and random interleavings of placement and release: no device, MIG profile or
// scalar dimension is ever committed beyond capacity, and releasing every
// placement returns the ledger to zero.
func TestPropertyNeverOverAllocate(t *testing.T) {
	profiles := []string{"1g.10gb", "2g.20gb", "3g.40gb"}
	for seed := int64(1); seed <= 300; seed++ {
		rng := rand.New(rand.NewSource(seed))
		capacity := &nexusv1.ResourceVector{
			CpuMillis: 1000 * int64(1+rng.Intn(32)), MemoryBytes: gib * int64(1+rng.Intn(256)),
			GpuCount: int32(rng.Intn(4)), GpuKind: "nvidia-a100", Mig: map[string]int64{},
			GpuShares: map[string]*nexusv1.GPUShare{},
		}
		for _, p := range profiles {
			capacity.Mig[p] = int64(rng.Intn(4))
		}
		for d := 0; d < rng.Intn(4); d++ {
			capacity.GpuShares[fmt.Sprint(4+d)] = &nexusv1.GPUShare{Milli: 1000, MemoryBytes: gib * int64(8+rng.Intn(73))}
		}
		policy := PolicyPack
		if seed%2 == 0 {
			policy = PolicySpread
		}
		committed := &nexusv1.ResourceVector{}
		var held []*nexusv1.ResourceVector
		for step := 0; step < 200; step++ {
			if len(held) > 0 && rng.Intn(3) == 0 {
				i := rng.Intn(len(held))
				committed = SubVec(committed, held[i])
				held = append(held[:i], held[i+1:]...)
			} else {
				req := &nexusv1.ResourceVector{CpuMillis: int64(rng.Intn(4000)), MemoryBytes: gib * int64(rng.Intn(16))}
				switch rng.Intn(4) {
				case 0:
					req.GpuCount = int32(rng.Intn(3))
				case 1:
					req.Mig = map[string]int64{profiles[rng.Intn(len(profiles))]: int64(1 + rng.Intn(2))}
				case 2:
					req.GpuShareMilli = int32(rng.Intn(700))
					req.GpuShareMemoryBytes = gib * int64(rng.Intn(40))
				}
				placed, ok, _ := Place(capacity, committed, req, policy)
				if ok {
					committed = AddVec(committed, placed)
					held = append(held, placed)
				}
			}
			assertWithinCapacity(t, seed, step, capacity, committed)
		}
		for _, p := range held {
			committed = SubVec(committed, p)
		}
		if !IsZeroVec(committed) || len(committed.GetGpuShares()) != 0 {
			t.Fatalf("seed %d: ledger not zero after releasing everything: %+v", seed, committed)
		}
		// Double release is a no-op, never negative.
		for _, p := range held {
			committed = SubVec(committed, p)
		}
		assertWithinCapacity(t, seed, -1, capacity, committed)
	}
}

func assertWithinCapacity(t *testing.T, seed int64, step int, capacity, committed *nexusv1.ResourceVector) {
	t.Helper()
	fail := func(what string, used, limit int64) {
		t.Fatalf("seed %d step %d: %s committed %d > capacity %d", seed, step, what, used, limit)
	}
	if committed.GetCpuMillis() > capacity.GetCpuMillis() {
		fail("cpu", committed.GetCpuMillis(), capacity.GetCpuMillis())
	}
	if committed.GetMemoryBytes() > capacity.GetMemoryBytes() {
		fail("memory", committed.GetMemoryBytes(), capacity.GetMemoryBytes())
	}
	if committed.GetGpuCount() > capacity.GetGpuCount() {
		fail("gpu", int64(committed.GetGpuCount()), int64(capacity.GetGpuCount()))
	}
	for p, n := range committed.GetMig() {
		if n > capacity.GetMig()[p] || n < 0 {
			fail("mig:"+p, n, capacity.GetMig()[p])
		}
	}
	for d, s := range committed.GetGpuShares() {
		c, ok := capacity.GetGpuShares()[d]
		if !ok {
			t.Fatalf("seed %d step %d: committed names device %s the worker does not have", seed, step, d)
		}
		if s.GetMilli() > c.GetMilli() || s.GetMilli() < 0 {
			fail("share milli dev "+d, s.GetMilli(), c.GetMilli())
		}
		if s.GetMemoryBytes() > c.GetMemoryBytes() || s.GetMemoryBytes() < 0 {
			fail("share memory dev "+d, s.GetMemoryBytes(), c.GetMemoryBytes())
		}
	}
}
