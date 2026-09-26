// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"errors"
	"strings"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	corev1 "k8s.io/api/core/v1"
)

func envMap(kv []string) map[string]string {
	m := map[string]string{}
	for _, e := range kv {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}
	return m
}

func TestMIGAssignmentEnvAndDockerArgs(t *testing.T) {
	spec := TaskSpec{
		Limit: &nexusv1.ResourceVector{Mig: map[string]int64{"1g.10gb": 2}},
		GPU:   GPUAssignment{MIG: []string{"MIG-a", "MIG-b"}, MIGProfiles: map[string]int64{"1g.10gb": 2}},
	}
	env := envMap(buildEnv(spec, "/w", "/w/in", "/w/out", "/w/outs"))
	if env["CUDA_VISIBLE_DEVICES"] != "MIG-a,MIG-b" || env[EnvGPUMode] != "mig" {
		t.Fatalf("env %v", env)
	}
	args := strings.Join(dockerGPUArgs(spec), " ")
	if args != `--gpus "device=MIG-a,MIG-b"` {
		t.Fatalf("docker args %q", args)
	}
}

func TestSharedMPSAssignmentCarriesClientLimits(t *testing.T) {
	spec := TaskSpec{
		Limit: &nexusv1.ResourceVector{GpuShareMilli: 250, GpuShareMemoryBytes: 10 << 30},
		GPU: GPUAssignment{SharedDevice: "4", SharedMode: "mps", ShareMilli: 250, ShareMemoryBytes: 10 << 30,
			MPSPipeDir: "/run/mps"},
	}
	host := envMap(buildEnv(spec, "/w", "/w/in", "/w/out", "/w/outs"))
	if host["CUDA_VISIBLE_DEVICES"] != "4" || host["CUDA_MPS_PINNED_DEVICE_MEM_LIMIT"] != "4=10240M" ||
		host["CUDA_MPS_ACTIVE_THREAD_PERCENTAGE"] != "25" || host[EnvGPUMode] != "shared-mps" ||
		host["CUDA_MPS_PIPE_DIRECTORY"] != "/run/mps" {
		t.Fatalf("host env %v", host)
	}
	// Inside the container the one exposed device is 0.
	ctr := envMap(dockerEnv(spec, "/nexus"))
	if ctr["CUDA_MPS_PINNED_DEVICE_MEM_LIMIT"] != "0=10240M" {
		t.Fatalf("container env %v", ctr)
	}
	args := strings.Join(dockerGPUArgs(spec), " ")
	if !strings.Contains(args, `device=4`) || !strings.Contains(args, "--ipc=host") || !strings.Contains(args, "/run/mps:/run/mps") {
		t.Fatalf("docker args %q", args)
	}
}

func TestTimeslicedAssignmentHasNoMPSLimits(t *testing.T) {
	spec := TaskSpec{
		Limit: &nexusv1.ResourceVector{GpuShareMemoryBytes: 1 << 30},
		GPU:   GPUAssignment{SharedDevice: "5", SharedMode: "timeslice", ShareMemoryBytes: 1 << 30},
	}
	env := envMap(buildEnv(spec, "/w", "/w/in", "/w/out", "/w/outs"))
	if _, ok := env["CUDA_MPS_PINNED_DEVICE_MEM_LIMIT"]; ok {
		t.Fatal("time-sliced device given an MPS limit it cannot enforce")
	}
	if env[EnvGPUShareMemoryBytes] != "1073741824" {
		t.Fatalf("env %v", env)
	}
	if strings.Contains(strings.Join(dockerGPUArgs(spec), " "), "--ipc") {
		t.Fatal("time-slicing does not need the host IPC namespace")
	}
}

func TestKubernetesFractionalResources(t *testing.T) {
	e := &KubernetesExecutor{Config: KubernetesConfig{MIGResourcePrefix: "nvidia.com/mig-",
		SharedGPUResourceName: "nvidia.com/gpu.shared"}}
	res, enforced, err := e.k8sResources(&nexusv1.ResourceVector{CpuMillis: 500, Mig: map[string]int64{"3g.40gb": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if q := res.Limits[corev1.ResourceName("nvidia.com/mig-3g.40gb")]; q.Value() != 1 {
		t.Fatalf("limits %v enforced %v", res.Limits, enforced)
	}
	res, _, err = e.k8sResources(&nexusv1.ResourceVector{GpuShareMemoryBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if q := res.Limits[corev1.ResourceName("nvidia.com/gpu.shared")]; q.Value() != 1 {
		t.Fatalf("shared limits %v", res.Limits)
	}
	none := &KubernetesExecutor{}
	if _, _, err := none.k8sResources(&nexusv1.ResourceVector{GpuShareMilli: 100}); !errors.Is(err, ErrLimitUnenforceable) {
		t.Fatalf("shared slice without a configured resource: %v", err)
	}
}

func TestSubprocessRefusesFractionalGPUWithoutIsolation(t *testing.T) {
	if SubprocessFidelity().GPU != "none" {
		t.Skip("this platform reports GPU fidelity")
	}
	if err := checkSubprocessLimitFidelity(&nexusv1.ResourceVector{Mig: map[string]int64{"1g.10gb": 1}}); !errors.Is(err, ErrLimitUnenforceable) {
		t.Fatalf("subprocess accepted a MIG request it cannot isolate: %v", err)
	}
}
