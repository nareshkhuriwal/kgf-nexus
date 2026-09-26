// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"sort"
	"strconv"
	"strings"
)

// GPUAssignment is the fractional-GPU part of what the worker's registry
// assigned an attempt (ADR-0029 §2): MIG instances by UUID and/or one slice of
// a shared device. Whole devices stay in TaskSpec.GPUDevices.
type GPUAssignment struct {
	// MIG are the instance UUIDs (MIG-...) the attempt holds.
	MIG []string
	// MIGProfiles counts instances per profile, for the kubernetes executor,
	// which requests nvidia.com/mig-<profile> rather than naming instances.
	MIGProfiles map[string]int64
	// SharedDevice is the shared GPU's device index, "" for none.
	SharedDevice string
	// SharedMode is "mps" or "timeslice".
	SharedMode string
	// ShareMilli and ShareMemoryBytes are the declared slice.
	ShareMilli       int64
	ShareMemoryBytes int64
	// MPSPipeDir is the host's MPS pipe directory, mounted into containers
	// that use an MPS-shared device. Empty means /tmp/nvidia-mps.
	MPSPipeDir string
}

// Any reports whether a fractional GPU was assigned.
func (g GPUAssignment) Any() bool { return len(g.MIG) > 0 || g.SharedDevice != "" }

// Environment variables describing the GPU slice to the workload, so a
// framework can size itself (vLLM gpu_memory_utilization, PyTorch's caching
// allocator) instead of assuming it owns the whole device.
const (
	EnvGPUMode             = "NEXUS_GPU_MODE"
	EnvGPUShareMilli       = "NEXUS_GPU_SHARE_MILLI"
	EnvGPUShareMemoryBytes = "NEXUS_GPU_SHARE_MEMORY_BYTES"
)

// gpuMode names the attempt's GPU mode for EnvGPUMode.
func gpuMode(spec TaskSpec) string {
	switch {
	case spec.GPU.SharedDevice != "":
		return "shared-" + spec.GPU.SharedMode
	case len(spec.GPU.MIG) > 0:
		return "mig"
	case spec.Limit.GetGpuCount() > 0 || len(spec.GPUDevices) > 0:
		return "whole"
	}
	return ""
}

// hostGPUList is every device the attempt may use, as the HOST names them:
// whole indices, MIG UUIDs, the shared device index. It is what
// CUDA_VISIBLE_DEVICES holds for a process on the host and what docker's
// --gpus device= list takes.
func hostGPUList(spec TaskSpec) string {
	var ids []string
	if spec.Limit.GetGpuCount() > 0 || len(spec.GPUDevices) > 0 {
		if w := gpuDeviceList(spec); w != "" {
			ids = append(ids, w)
		}
	}
	ids = append(ids, spec.GPU.MIG...)
	if spec.GPU.SharedDevice != "" {
		ids = append(ids, spec.GPU.SharedDevice)
	}
	return strings.Join(ids, ",")
}

// gpuSliceEnv is the slice description, plus the MPS client limits when the
// shared device runs an MPS server. deviceInView is how the workload itself
// addresses the shared device: its host index for a process on the host, 0
// inside a container that was given exactly that one device.
//
// The MPS limits are what makes a VRAM declaration enforceable on MPS
// (Volta+): the MPS server refuses allocations past the pinned limit. On a
// time-sliced device nothing enforces it -- ADR-0029 §3.
func gpuSliceEnv(spec TaskSpec, deviceInView string) []string {
	mode := gpuMode(spec)
	if mode == "" {
		return nil
	}
	env := []string{EnvGPUMode + "=" + mode}
	g := spec.GPU
	if g.SharedDevice == "" {
		return env
	}
	env = append(env,
		EnvGPUShareMilli+"="+strconv.FormatInt(g.ShareMilli, 10),
		EnvGPUShareMemoryBytes+"="+strconv.FormatInt(g.ShareMemoryBytes, 10))
	if g.SharedMode == "mps" {
		if g.ShareMilli > 0 {
			pct := (g.ShareMilli + 9) / 10
			if pct > 100 {
				pct = 100
			}
			env = append(env, "CUDA_MPS_ACTIVE_THREAD_PERCENTAGE="+strconv.FormatInt(pct, 10))
		}
		if g.ShareMemoryBytes > 0 {
			mib := (g.ShareMemoryBytes + (1<<20 - 1)) >> 20
			env = append(env, "CUDA_MPS_PINNED_DEVICE_MEM_LIMIT="+deviceInView+"="+strconv.FormatInt(mib, 10)+"M")
		}
		env = append(env, "CUDA_MPS_PIPE_DIRECTORY="+mpsPipeDir(g))
	}
	return env
}

func mpsPipeDir(g GPUAssignment) string {
	if g.MPSPipeDir != "" {
		return g.MPSPipeDir
	}
	return "/tmp/nvidia-mps"
}

// dockerGPUArgs exposes the assigned devices to a container. Requires the
// NVIDIA container toolkit; without it `docker run` fails, which is the legible
// failure (see dockerLimitArgs). An MPS client additionally needs the server's
// pipe directory and the host IPC namespace -- a real isolation cost that
// ADR-0029 §3 records.
func dockerGPUArgs(spec TaskSpec) []string {
	if !spec.GPU.Any() {
		return nil
	}
	args := []string{"--gpus", `"device=` + hostGPUList(spec) + `"`}
	if spec.GPU.SharedDevice != "" && spec.GPU.SharedMode == "mps" {
		dir := mpsPipeDir(spec.GPU)
		args = append(args, "--ipc=host", "-v", dir+":"+dir)
	}
	return args
}

// Artifact environment (ADR-0030 §4).
const (
	EnvArtifactPrefix = "NEXUS_ARTIFACT_"
	EnvArtifactDir    = "NEXUS_ARTIFACT_DIR"
	// ContainerArtifactDir is where the docker executor mounts artifacts.
	ContainerArtifactDir = "/nexus/artifacts"
)

// ArtifactEnvName maps an artifact name onto its variable: upper-cased, and
// anything outside [A-Z0-9_] becomes '_', so an author's "llama-3.1" cannot
// produce an invalid or shell-hostile variable name.
func ArtifactEnvName(name string) string {
	b := []byte(strings.ToUpper(name))
	for i, c := range b {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			b[i] = '_'
		}
	}
	return EnvArtifactPrefix + string(b)
}

// artifactEnv renders NEXUS_ARTIFACT_<NAME> for each artifact. inContainer
// switches the paths to the docker mount points.
func artifactEnv(spec TaskSpec, inContainer bool) []string {
	if len(spec.Artifacts) == 0 {
		return nil
	}
	var env []string
	for _, n := range sortedArtifactNames(spec.Artifacts) {
		p := spec.Artifacts[n]
		if inContainer {
			p = ContainerArtifactDir + "/" + sanitizeID(n)
		}
		env = append(env, ArtifactEnvName(n)+"="+p)
	}
	if inContainer {
		env = append(env, EnvArtifactDir+"="+ContainerArtifactDir)
	}
	return env
}

// dockerArtifactArgs mounts each cached artifact read-only: the cache is
// shared by every attempt on the node, and a task that could write to it could
// poison the next task's model.
func dockerArtifactArgs(artifacts map[string]string) []string {
	var args []string
	for _, n := range sortedArtifactNames(artifacts) {
		args = append(args, "--volume", artifacts[n]+":"+ContainerArtifactDir+"/"+sanitizeID(n)+":ro")
	}
	return args
}

func sortedArtifactNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
