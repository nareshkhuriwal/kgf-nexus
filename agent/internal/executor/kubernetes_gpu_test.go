// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Pod-spec tests for fractional and whole GPUs on the kubernetes executor
// (ADR-0029 §2). The device plugin, not the agent, hands a pod its devices,
// so the pod spec's extended-resource request IS the allocation: it must
// follow what the agent's registry ADMITTED (the reservation), not the
// enforcement ceiling. An author who declares `limit: {cpu, memory}` next to
// `request: {mig: ...}` has a MIG reservation and must get a MIG pod.

func gpuExecutor(t *testing.T) *KubernetesExecutor {
	t.Helper()
	e, _ := newFakeExecutor(t)
	e.Config.MIGResourcePrefix = "nvidia.com/mig-"
	e.Config.SharedGPUResourceName = "nvidia.com/gpu.shared"
	return e
}

// cpuMemLimit is an enforcement ceiling that names no GPU at all.
func cpuMemLimit() *nexusv1.ResourceVector {
	return &nexusv1.ResourceVector{CpuMillis: 1000, MemoryBytes: 512 << 20}
}

func taskContainer(t *testing.T, e *KubernetesExecutor, spec TaskSpec) (corev1.Container, corev1.Container, map[string]string) {
	t.Helper()
	job, _, err := e.buildJob(spec, "worker-1")
	if err != nil {
		t.Fatalf("buildJob: %v", err)
	}
	pod := job.Spec.Template.Spec
	if len(pod.Containers) != 1 || len(pod.InitContainers) != 1 {
		t.Fatalf("containers %d init %d", len(pod.Containers), len(pod.InitContainers))
	}
	return pod.Containers[0], pod.InitContainers[0], job.Annotations
}

func quantity(c corev1.Container, name string) (req, lim int64) {
	q := c.Resources.Requests[corev1.ResourceName(name)]
	l := c.Resources.Limits[corev1.ResourceName(name)]
	return q.Value(), l.Value()
}

func podEnv(c corev1.Container) map[string]string {
	m := map[string]string{}
	for _, e := range c.Env {
		m[e.Name] = e.Value
	}
	return m
}

func TestK8sPodRequestsMIGProfilesFromTheAdmission(t *testing.T) {
	e := gpuExecutor(t)
	spec := unitSpec("mig-1")
	spec.Limit = cpuMemLimit()
	spec.GPU = GPUAssignment{MIG: []string{"MIG-sim-a", "MIG-sim-b", "MIG-sim-c"},
		MIGProfiles: map[string]int64{"1g.10gb": 2, "3g.40gb": 1}}

	task, collector, ann := taskContainer(t, e, spec)
	for profile, want := range map[string]int64{"1g.10gb": 2, "3g.40gb": 1} {
		name := "nvidia.com/mig-" + profile
		if req, lim := quantity(task, name); req != want || lim != want {
			t.Errorf("%s requests=%d limits=%d; want %d == %d (extended resources need requests == limits)",
				name, req, lim, want, want)
		}
		if req, lim := quantity(collector, name); req != 0 || lim != 0 {
			t.Errorf("the output collector sidecar requests %s", name)
		}
	}
	if req, _ := quantity(task, "nvidia.com/gpu"); req != 0 {
		t.Errorf("a MIG attempt also requests a whole GPU (%d)", req)
	}
	if !strings.Contains(ann[k8sAnnEnforced], "mig=device-plugin:nvidia.com/mig-1g.10gb") {
		t.Errorf("enforced annotation %q does not name the MIG resource", ann[k8sAnnEnforced])
	}
	env := podEnv(task)
	if env[EnvGPUMode] != "mig" {
		t.Errorf("%s = %q; want mig", EnvGPUMode, env[EnvGPUMode])
	}
	// The instance UUIDs the agent's ledger named are NOT the pod's: the
	// device plugin chooses and exports them. Exporting the agent's would
	// point CUDA at devices the container does not have.
	if v, ok := env["CUDA_VISIBLE_DEVICES"]; ok {
		t.Errorf("CUDA_VISIBLE_DEVICES=%q set on a device-plugin pod", v)
	}
	if v, ok := env["NVIDIA_VISIBLE_DEVICES"]; ok {
		t.Errorf("NVIDIA_VISIBLE_DEVICES=%q set on a device-plugin pod", v)
	}
}

func TestK8sPodMIGPrefixIsConfigurable(t *testing.T) {
	e := gpuExecutor(t)
	e.Config.MIGResourcePrefix = "example.com/mig-"
	spec := unitSpec("mig-2")
	spec.Limit = &nexusv1.ResourceVector{Mig: map[string]int64{"2g.20gb": 1}}
	task, _, _ := taskContainer(t, e, spec)
	if req, _ := quantity(task, "example.com/mig-2g.20gb"); req != 1 {
		t.Fatalf("resources %v", task.Resources.Limits)
	}
}

func TestK8sPodRequestsOneSharedReplicaFromTheAdmission(t *testing.T) {
	for _, mode := range []string{"mps", "timeslice"} {
		t.Run(mode, func(t *testing.T) {
			e := gpuExecutor(t)
			spec := unitSpec("share-" + mode)
			spec.Limit = cpuMemLimit()
			spec.GPU = GPUAssignment{SharedDevice: "4", SharedMode: mode, ShareMilli: 250, ShareMemoryBytes: 10 << 30}

			task, collector, ann := taskContainer(t, e, spec)
			if req, lim := quantity(task, "nvidia.com/gpu.shared"); req != 1 || lim != 1 {
				t.Fatalf("shared replica requests=%d limits=%d; want 1 == 1", req, lim)
			}
			if req, _ := quantity(collector, "nvidia.com/gpu.shared"); req != 0 {
				t.Fatal("the collector requests a shared GPU replica")
			}
			if !strings.Contains(ann[k8sAnnEnforced], "gpu-share=device-plugin-replica:nvidia.com/gpu.shared") {
				t.Errorf("enforced annotation %q", ann[k8sAnnEnforced])
			}
			env := podEnv(task)
			if env[EnvGPUMode] != "shared-"+mode || env[EnvGPUShareMilli] != "250" ||
				env[EnvGPUShareMemoryBytes] != "10737418240" {
				t.Errorf("slice env %v", env)
			}
			// The device plugin's MPS mode sets each replica's limits itself;
			// the agent's per-device values name a device index on ANOTHER
			// node's numbering and must not reach the pod.
			for _, k := range []string{"CUDA_MPS_PINNED_DEVICE_MEM_LIMIT", "CUDA_MPS_ACTIVE_THREAD_PERCENTAGE", "CUDA_MPS_PIPE_DIRECTORY", "CUDA_VISIBLE_DEVICES"} {
				if v, ok := env[k]; ok {
					t.Errorf("%s=%q set on a device-plugin pod", k, v)
				}
			}
		})
	}
}

func TestK8sPodRefusesASharedSliceWithoutAConfiguredResource(t *testing.T) {
	e := gpuExecutor(t)
	e.Config.SharedGPUResourceName = ""
	spec := unitSpec("share-none")
	spec.Limit = cpuMemLimit()
	spec.GPU = GPUAssignment{SharedDevice: "4", SharedMode: "mps", ShareMilli: 250}
	if _, _, err := e.buildJob(spec, "w"); !errors.Is(err, ErrLimitUnenforceable) {
		t.Fatalf("buildJob = %v; want ErrLimitUnenforceable (never a pod with no GPU at all)", err)
	}
}

func TestK8sPodRequestsTheAdmittedWholeGPUs(t *testing.T) {
	e := gpuExecutor(t)
	spec := unitSpec("whole-1")
	spec.Limit = cpuMemLimit()
	spec.GPUDevices = []int{0, 1}
	task, _, _ := taskContainer(t, e, spec)
	if req, lim := quantity(task, "nvidia.com/gpu"); req != 2 || lim != 2 {
		t.Fatalf("whole GPUs requests=%d limits=%d; want the 2 the registry admitted", req, lim)
	}
	if podEnv(task)[EnvGPUMode] != "whole" {
		t.Errorf("env %v", podEnv(task))
	}
	// And a CPU-only attempt requests no GPU resource and gets no GPU mode.
	cpu := unitSpec("cpu-1")
	cpu.Limit = cpuMemLimit()
	task, _, _ = taskContainer(t, e, cpu)
	for name := range task.Resources.Limits {
		if strings.Contains(string(name), "nvidia.com") {
			t.Errorf("a CPU-only attempt requests %s", name)
		}
	}
	if _, ok := podEnv(task)[EnvGPUMode]; ok {
		t.Error("a CPU-only pod carries NEXUS_GPU_MODE")
	}
}

func TestK8sGPUResourceNamesFromEnv(t *testing.T) {
	env := map[string]string{
		"NEXUS_K8S_NAMESPACE":            "tasks",
		"NEXUS_K8S_GPU_RESOURCE":         "nvidia.com/gpu",
		"NEXUS_K8S_MIG_RESOURCE_PREFIX":  "nvidia.com/mig-",
		"NEXUS_K8S_GPU_SHARED_RESOURCE":  "nvidia.com/gpu.shared",
		"NEXUS_K8S_GPU_KIND_LABEL":       "nvidia.com/gpu.product",
		"NEXUS_K8S_TASK_API_ADVERTISE":   "",
		"NEXUS_K8S_IMAGE_PULL_POLICY":    "",
		"NEXUS_K8S_TTL_AFTER_FINISHED":   "",
		"NEXUS_K8S_START_TIMEOUT":        "",
		"NEXUS_K8S_IMAGE_PULL_TIMEOUT":   "",
		"NEXUS_K8S_WRITABLE_ROOT_FS":     "",
		"NEXUS_K8S_RUN_AS_USER":          "",
		"NEXUS_K8S_NODE_SELECTOR":        "",
		"NEXUS_K8S_TOLERATIONS":          "",
		"NEXUS_K8S_IMAGE_PULL_SECRETS":   "",
		"NEXUS_K8S_SERVICE_ACCOUNT":      "",
		"NEXUS_K8S_OWNER_POD_NAMESPACE":  "",
		"NEXUS_K8S_OWNER_POD_NAME":       "",
		"NEXUS_K8S_OWNER_POD_UID":        "",
		"NEXUS_K8S_HELPER_IMAGE":         "",
		"NEXUS_K8S_KUBECONFIG":           "",
		"NEXUS_K8S_CONTEXT":              "",
		"NEXUS_K8S_OWNER":                "agent-0",
		"NEXUS_K8S_TASK_API_LISTEN":      "",
		"NEXUS_K8S_TASK_API_EXTRA_NAMES": "",
	}
	cfg, ok, err := LoadKubernetesConfigFromEnv(func(k string) string { return env[k] })
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if cfg.GPUResourceName != "nvidia.com/gpu" || cfg.MIGResourcePrefix != "nvidia.com/mig-" ||
		cfg.SharedGPUResourceName != "nvidia.com/gpu.shared" || cfg.GPUKindLabel != "nvidia.com/gpu.product" {
		t.Fatalf("%+v", cfg)
	}
	// A MIG prefix must end in a separator the device plugin uses: "mig-"
	// without one would name nvidia.com/mig1g.10gb, a resource no plugin
	// advertises, and every MIG pod would sit Pending forever.
	env["NEXUS_K8S_MIG_RESOURCE_PREFIX"] = "nvidia.com/mig"
	if _, _, err := LoadKubernetesConfigFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Fatal("a MIG prefix without a trailing '-' was accepted")
	}
	env["NEXUS_K8S_MIG_RESOURCE_PREFIX"] = "nvidia.com/mig-"
	env["NEXUS_K8S_GPU_SHARED_RESOURCE"] = "not a resource"
	if _, _, err := LoadKubernetesConfigFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Fatal("a malformed shared resource name was accepted")
	}
}
