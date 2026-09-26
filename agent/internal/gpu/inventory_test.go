// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"strings"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func TestParseRealAndSimulated(t *testing.T) {
	inv, err := Parse("1g.10gb=MIG-a,MIG-b;3g.40gb=MIG-c", "4:mps:80Gi,5:timeslice:24Gi", 2, "h")
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.MIG["1g.10gb"]) != 2 || inv.Simulated || inv.Shared["4"].Mode != ModeMPS || inv.Shared["5"].MemoryBytes != 24<<30 {
		t.Fatalf("%+v", inv)
	}
	c := &nexusv1.ResourceVector{GpuCount: 2}
	inv.ApplyTo(c)
	if c.Mig["3g.40gb"] != 1 || c.GpuShares["4"].GetMilli() != 1000 || c.GpuShares["4"].GetMemoryBytes() != 80<<30 {
		t.Fatalf("capacity %v", c)
	}
	sim, err := Parse("1g.10gb=3", "", 0, "host1")
	if err != nil || !sim.Simulated || len(sim.MIG["1g.10gb"]) != 3 || !strings.HasPrefix(sim.MIG["1g.10gb"][0], "MIG-sim-host1") {
		t.Fatalf("simulated %+v %v", sim, err)
	}
	if sim.Labels()["simulated"] != "true" {
		t.Fatal("simulated MIG not labelled simulated")
	}
}

func TestParseRejectsDoubleCountedDevices(t *testing.T) {
	for _, tc := range []struct{ mig, shared string }{
		{"", "1:mps:80Gi"},                  // index 1 is a whole GPU (count 2)
		{"", "4:mps:80Gi,4:timeslice:8Gi"},  // listed twice
		{"1g.10gb=MIG-a;2g.20gb=MIG-a", ""}, // instance under two profiles
		{"banana=1", ""},
		{"", "4:magic:8Gi"},
		{"", "4:mps:0"},
		{"1g.10gb=gpu-0", ""},
	} {
		if _, err := Parse(tc.mig, tc.shared, 2, "h"); err == nil {
			t.Errorf("accepted mig=%q shared=%q", tc.mig, tc.shared)
		}
	}
}

// Captured shape of `nvidia-smi -L` on an A100 node with GPU 1 partitioned.
const smiA100 = `GPU 0: NVIDIA A100-SXM4-80GB (UUID: GPU-5ad9f4e2-6c4b-7d1a-9e3f-0a1b2c3d4e5f)
GPU 1: NVIDIA A100-SXM4-80GB (UUID: GPU-6be0a5f3-7d5c-8e2b-af40-1b2c3d4e5f60)
  MIG 3g.40gb     Device  0: (UUID: MIG-2f6b1d44-0c55-5f3a-9e7d-8b6a5c4d3e2f)
  MIG 1g.10gb     Device  1: (UUID: MIG-3a7c2e55-1d66-6a4b-af8e-9c7b6d5e4f30)
  MIG 1g.10gb     Device  2: (UUID: MIG-4b8d3f66-2e77-7b5c-b09f-ad8c7e6f5041)
`

func TestDiscoverNvidiaSMI(t *testing.T) {
	whole, mig, err := DiscoverNvidiaSMI(smiA100)
	if err != nil {
		t.Fatal(err)
	}
	if whole != 1 || len(mig["1g.10gb"]) != 2 || len(mig["3g.40gb"]) != 1 {
		t.Fatalf("whole=%d mig=%v", whole, mig)
	}
	bad := "GPU 0: A (UUID: GPU-aa)\n  MIG 1g.10gb Device 0: (UUID: MIG-bb)\nGPU 1: B (UUID: GPU-cc)\n"
	if _, _, err := DiscoverNvidiaSMI(bad); err == nil {
		t.Fatal("a whole GPU above a MIG GPU accepted (CUDA_VISIBLE_DEVICES 0..n-1 would be wrong)")
	}
}
