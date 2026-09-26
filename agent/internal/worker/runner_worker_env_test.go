// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"io"
	"testing"
)

// The runner hands every executor the worker's identity and advertised GPUs,
// which the executors export to the workload (executor.EnvWorkerID,
// EnvWorkerGPUKind, EnvWorkerGPUCount).
func TestExecutorSpecCarriesTheWorkersIdentityAndGPUs(t *testing.T) {
	r, _ := newSecretRunner(t, &progressRecorder{}, &leakyExecutor{}, io.Discard)
	r.WorkerID, r.GPUKind, r.GPUCount = "w-7", "nvidia-a10", 1
	spec, err := r.buildExecutorSpec(context.Background(), assignment("att-env", "true"))
	if err != nil {
		t.Fatal(err)
	}
	if spec.WorkerID != "w-7" || spec.WorkerGPUKind != "nvidia-a10" || spec.WorkerGPUCount != 1 {
		t.Fatalf("spec worker fields = %q %q %d", spec.WorkerID, spec.WorkerGPUKind, spec.WorkerGPUCount)
	}
}
