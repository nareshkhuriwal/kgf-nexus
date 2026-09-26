// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"strings"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func envOf(list []string) map[string]string {
	m := map[string]string{}
	for _, kv := range list {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// A workload can tell which worker it runs on and what GPUs that worker
// advertises, on every substrate. examples/gpu_scheduling audits placement
// with these; it used to infer the GPU model from Kubernetes pod-name
// hostnames, which compose containers do not have (F150).
func TestWorkloadsSeeTheirWorkersIdentityAndAdvertisedGPUs(t *testing.T) {
	spec := TaskSpec{AttemptID: "a", WorkerID: "w-1", WorkerGPUKind: "nvidia-a100", WorkerGPUCount: 2}
	k8s := map[string]string{}
	for _, e := range k8sEnv(spec) {
		k8s[e.Name] = e.Value
	}
	for name, env := range map[string]map[string]string{
		"subprocess": envOf(buildEnv(spec, "/w", "/w/in", "/w/out.json", "/w/out")),
		"docker":     envOf(dockerEnv(spec, "/nexus")),
		"kubernetes": k8s,
	} {
		if env[EnvWorkerID] != "w-1" || env[EnvWorkerGPUKind] != "nvidia-a100" || env[EnvWorkerGPUCount] != "2" {
			t.Errorf("%s: worker id %q, gpu kind %q, gpu count %q", name, env[EnvWorkerID], env[EnvWorkerGPUKind], env[EnvWorkerGPUCount])
		}
	}

	// A worker with no GPUs says so by omission, not with an empty kind.
	plain := envOf(buildEnv(TaskSpec{AttemptID: "a", WorkerID: "w-2"}, "/w", "/w/in", "/w/out.json", "/w/out"))
	if _, ok := plain[EnvWorkerGPUKind]; ok {
		t.Error("a GPU-less worker exported a GPU kind")
	}
	if _, ok := plain[EnvWorkerGPUCount]; ok {
		t.Error("a GPU-less worker exported a GPU count")
	}
	// The platform's values win over a task env of the same name: a task
	// cannot make an audit believe it ran on hardware it did not.
	spoof := TaskSpec{AttemptID: "a", WorkerID: "w-1", WorkerGPUKind: "nvidia-a10", WorkerGPUCount: 1,
		Runtime: &nexusv1.RuntimeSpec{Env: map[string]string{EnvWorkerGPUKind: "nvidia-h100", EnvWorkerID: "other"}}}
	for name, env := range map[string]map[string]string{
		// Later entries win in an exec environment, as they do in envOf.
		"subprocess": envOf(buildEnv(spoof, "/w", "/w/in", "/w/out.json", "/w/out")),
		"docker":     envOf(dockerEnv(spoof, "/nexus")),
	} {
		if env[EnvWorkerGPUKind] != "nvidia-a10" || env[EnvWorkerID] != "w-1" {
			t.Errorf("%s: a task env overrode the platform's: kind %q id %q", name, env[EnvWorkerGPUKind], env[EnvWorkerID])
		}
	}
}
