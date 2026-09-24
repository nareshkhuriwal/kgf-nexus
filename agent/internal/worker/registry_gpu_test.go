// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"fmt"
	"sync"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func gpuWorker(gpus int32) *Registry {
	return NewRegistry(&nexusv1.ResourceVector{
		CpuMillis: 64000, MemoryBytes: 256 << 30, GpuCount: gpus,
	})
}

func oneGPU() *nexusv1.ResourceVector {
	return &nexusv1.ResourceVector{CpuMillis: 1000, MemoryBytes: 1 << 30, GpuCount: 1}
}

// The bug this pins: two 1-GPU attempts running at once on a 2-GPU worker were
// both told CUDA_VISIBLE_DEVICES=0, because devices were numbered 0..n-1 per
// attempt instead of allocated. They must get DIFFERENT devices.
func TestConcurrentGPUAttemptsGetDistinctDevices(t *testing.T) {
	r := gpuWorker(2)
	if ok, why := r.Admit("a", oneGPU()); !ok {
		t.Fatalf("admit a: %s", why)
	}
	if ok, why := r.Admit("b", oneGPU()); !ok {
		t.Fatalf("admit b: %s", why)
	}
	a, b := r.GPUDevices("a"), r.GPUDevices("b")
	if len(a) != 1 || len(b) != 1 || a[0] == b[0] {
		t.Fatalf("devices a=%v b=%v; concurrent attempts must hold different devices", a, b)
	}
	// And the ledger still refuses a third: the worker has two.
	if ok, _ := r.Admit("c", oneGPU()); ok {
		t.Fatal("a third 1-GPU attempt was admitted on a 2-GPU worker")
	}
}

func TestReleasedDevicesAreReused(t *testing.T) {
	r := gpuWorker(2)
	r.Admit("a", oneGPU())
	r.Admit("b", oneGPU())
	freed := r.GPUDevices("a")[0]
	r.Release("a")
	if got := r.GPUDevices("a"); got != nil {
		t.Fatalf("released attempt still holds %v", got)
	}
	if ok, why := r.Admit("c", oneGPU()); !ok {
		t.Fatalf("admit after release: %s", why)
	}
	if got := r.GPUDevices("c"); len(got) != 1 || got[0] != freed {
		t.Fatalf("c got %v; want the freed device %d", got, freed)
	}
	// Release is idempotent and must not free a device another attempt holds.
	r.Release("a")
	if got := r.GPUDevices("c"); len(got) != 1 || got[0] != freed {
		t.Fatalf("a second release of a disturbed c's device: %v", got)
	}
}

func TestMultiGPUAttemptGetsEveryDevice(t *testing.T) {
	r := gpuWorker(2)
	two := &nexusv1.ResourceVector{CpuMillis: 1000, MemoryBytes: 1 << 30, GpuCount: 2}
	if ok, why := r.Admit("dist", two); !ok {
		t.Fatalf("admit: %s", why)
	}
	if got := r.GPUDevices("dist"); fmt.Sprint(got) != "[0 1]" {
		t.Fatalf("2-GPU attempt got %v, want [0 1]", got)
	}
	if got := r.GPUDevices("cpu-only"); got != nil {
		t.Fatalf("an attempt that asked for no GPU holds %v", got)
	}
}

// Many admissions racing: every admitted attempt must hold a device no other
// running attempt holds. Run under -race.
func TestConcurrentAdmissionsNeverShareADevice(t *testing.T) {
	const gpus = 8
	r := gpuWorker(gpus)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.Admit(fmt.Sprintf("att-%d", i), oneGPU())
		}(i)
	}
	wg.Wait()
	seen := map[int]string{}
	admitted := 0
	for _, id := range r.RunningAttemptIDs() {
		for _, d := range r.GPUDevices(id) {
			if other, dup := seen[d]; dup {
				t.Fatalf("device %d held by both %s and %s", d, other, id)
			}
			seen[d] = id
		}
		admitted++
	}
	if admitted != gpus {
		t.Fatalf("admitted %d attempts on an %d-GPU worker", admitted, gpus)
	}
}
