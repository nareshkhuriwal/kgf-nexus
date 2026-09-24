// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// A worker must never stall behind a task that asks for a GPU model it does
// not have. Reproduces the stall seen on the dev cluster: the gpu queue's
// OLDEST task wanted an A10, so every A100 worker's claim stopped at it and
// returned nothing -- two A100s idle while six A100 tasks waited behind one
// busy A10. Each worker must instead claim the work it can actually run.
func TestWorkersSkipTasksForAGPUModelTheyDoNotHave(t *testing.T) {
	for _, windowed := range []bool{true, false} {
		name := "exhaustive scan"
		if windowed {
			name = "windowed claim"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			f := newFixture(t, pool)
			now := time.Now().UTC()

			a100 := f.addWorker(vec{CPUMillis: 8000, MemoryBytes: 32 << 30, GPUCount: 2,
				GPUKind: "nvidia-a100", GPUMemoryBytes: 40 << 30}, nil, nil)
			a10 := f.addWorker(vec{CPUMillis: 8000, MemoryBytes: 16 << 30, GPUCount: 1,
				GPUKind: "nvidia-a10", GPUMemoryBytes: 24 << 30}, nil, nil)

			// The A10 task is the oldest, so it is every claim's first candidate.
			enhanceID := f.addAttempt("enhance", 0, vec{CPUMillis: 1000, MemoryBytes: 1 << 30, GPUCount: 1,
				GPUKind: "nvidia-a10", GPUMemoryBytes: 20 << 30}, nodeSpec{TaskRef: "e", Version: 1}, now.Add(-10*time.Minute))
			f.addAttempt("detect", 0, vec{CPUMillis: 1000, MemoryBytes: 1 << 30, GPUCount: 1,
				GPUKind: "nvidia-a100", GPUMemoryBytes: 40 << 30}, nodeSpec{TaskRef: "d", Version: 1}, now.Add(-5*time.Minute))
			// A GPU-memory floor above what the A10 has: only the A100 can run it.
			f.addAttempt("big_mem", 0, vec{CPUMillis: 1000, MemoryBytes: 1 << 30, GPUCount: 1,
				GPUMemoryBytes: 32 << 30}, nodeSpec{TaskRef: "b", Version: 1}, now.Add(-4*time.Minute))

			st := f.newStore(nil)
			if windowed {
				if s, err := st.EnableWindowedClaim(ctx); err != nil || !s.Enabled {
					t.Fatalf("enable windowed claim: %+v %v", s, err)
				}
			}
			claim := func(worker uuid.UUID, free vec) []string {
				t.Helper()
				asg, err := st.ClaimForWorker(ctx, now, store.WorkerClaimRequest{
					WorkerID: worker, Queues: []string{f.queue}, MaxTasks: 4,
					FreeCapacity: &nexusv1.ResourceVector{CpuMillis: free.CPUMillis, MemoryBytes: free.MemoryBytes,
						GpuCount: free.GPUCount, GpuMemoryBytes: free.GPUMemoryBytes},
				})
				if err != nil {
					t.Fatalf("claim: %v", err)
				}
				var got []string
				for _, a := range asg {
					got = append(got, a.NodeID)
				}
				return got
			}

			// The A100 worker polls first. It must NOT stall behind "enhance".
			got := claim(a100, vec{CPUMillis: 8000, MemoryBytes: 32 << 30, GPUCount: 2, GPUMemoryBytes: 40 << 30})
			if len(got) != 2 || !contains(got, "detect") || !contains(got, "big_mem") {
				t.Fatalf("A100 worker claimed %v; want [detect big_mem] -- it must skip the A10-only task, not stall behind it", got)
			}
			if s := f.attemptState(enhanceID); s != "PENDING" {
				t.Fatalf("the A10 task was %s; an A100 worker must never take it", s)
			}

			// And the A10 worker gets the A10 task.
			got = claim(a10, vec{CPUMillis: 8000, MemoryBytes: 16 << 30, GPUCount: 1, GPUMemoryBytes: 24 << 30})
			if len(got) != 1 || got[0] != "enhance" {
				t.Fatalf("A10 worker claimed %v; want [enhance]", got)
			}
		})
	}
}
