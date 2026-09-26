// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/gpu"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

func fractionalRegistry(t *testing.T) *Registry {
	t.Helper()
	inv, err := gpu.Parse("1g.10gb=MIG-a,MIG-b,MIG-c", "4:mps:40Gi,5:timeslice:40Gi", 0, "h")
	if err != nil {
		t.Fatal(err)
	}
	c := &nexusv1.ResourceVector{CpuMillis: 64000, MemoryBytes: 1 << 40}
	inv.ApplyTo(c)
	return NewRegistryWithGPUs(c, inv)
}

func TestRegistryNamesDistinctMIGInstances(t *testing.T) {
	r := fractionalRegistry(t)
	req := &nexusv1.ResourceVector{Mig: map[string]int64{"1g.10gb": 1}}
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		id := fmt.Sprint("a", i)
		if ok, why := r.Admit(id, req); !ok {
			t.Fatalf("admit %d: %s", i, why)
		}
		g := r.GPUAssignment(id)
		if len(g.MIG) != 1 || seen[g.MIG[0]] {
			t.Fatalf("attempt %d got %v (seen %v)", i, g.MIG, seen)
		}
		seen[g.MIG[0]] = true
	}
	if ok, _ := r.Admit("a3", req); ok {
		t.Fatal("a fourth instance admitted on three")
	}
	r.Release("a1")
	if ok, _ := r.Admit("a4", req); !ok || r.GPUAssignment("a4").MIG[0] != "MIG-b" {
		t.Fatalf("released instance not reused: %v", r.GPUAssignment("a4").MIG)
	}
}

// The agent uses the device the control plane's ledger charged, or refuses:
// re-placing it would desynchronise the two ledgers.
func TestRegistryHonoursTheReservedSharedDevice(t *testing.T) {
	r := fractionalRegistry(t)
	req := &nexusv1.ResourceVector{GpuShareMemoryBytes: 30 << 30, GpuShareMilli: 300}
	onFive := domain.CloneVec(req)
	onFive.GpuShares = map[string]*nexusv1.GPUShare{"5": {Milli: 300, MemoryBytes: 30 << 30}}
	if ok, why := r.AdmitReserved("x", req, onFive); !ok {
		t.Fatal(why)
	}
	g := r.GPUAssignment("x")
	if g.SharedDevice != "5" || g.SharedMode != gpu.ModeTimeslice || g.ShareMemoryBytes != 30<<30 {
		t.Fatalf("assignment %+v", g)
	}
	// Device 5 has 10 GiB left; the control plane naming it for 30 GiB more
	// means the ledgers disagree, so refuse -- even though device 4 is empty.
	if ok, why := r.AdmitReserved("y", req, onFive); ok || !strings.Contains(why, "gpu_share_device_full") {
		t.Fatalf("admitted onto a full device (%s)", why)
	}
	unknown := domain.CloneVec(req)
	unknown.GpuShares = map[string]*nexusv1.GPUShare{"9": {}}
	if ok, _ := r.AdmitReserved("z", req, unknown); ok {
		t.Fatal("admitted onto a device this worker does not have")
	}
	// No reservation (older scheduler): placed locally, on the empty device.
	if ok, why := r.AdmitReserved("w", req, nil); !ok || r.GPUAssignment("w").SharedDevice != "4" {
		t.Fatalf("local placement: %v %s %+v", ok, why, r.GPUAssignment("w"))
	}
	r.Release("x")
	r.Release("w")
	if free := r.FreeCapacity(); len(free.GetGpuShares()) != 2 || free.GetGpuShares()["5"].GetMemoryBytes() != 40<<30 {
		t.Fatalf("release did not restore both devices: %v", free.GetGpuShares())
	}
}

func TestRegistryConcurrentSharedAdmissionsNeverOverfill(t *testing.T) {
	r := fractionalRegistry(t)
	req := &nexusv1.ResourceVector{GpuShareMemoryBytes: 10 << 30}
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if ok, _ := r.AdmitReserved(fmt.Sprint("c", i), req, nil); ok {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if admitted != 8 {
		t.Fatalf("admitted %d x 10 GiB onto 2 x 40 GiB, want exactly 8", admitted)
	}
}
