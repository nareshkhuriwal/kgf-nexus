// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"slices"
	"strings"
	"testing"
)

// The workload API reaches each substrate the way its filesystem allows
// (ADR-0032 §1): the host path for a subprocess, the mounted container path
// for docker, and nothing for kubernetes, whose pods cannot reach this host.
func TestTaskAPIIsExportedPerSubstrate(t *testing.T) {
	spec := TaskSpec{
		AttemptID: "a", AttemptNumber: 3,
		TaskAPISocket: "/tmp/nexus-task/nx-abc/api.sock", TaskAPIToken: "tok",
	}

	sub := buildEnv(spec, "/w", "/w/in", "/w/out.json", "/w/out")
	if v, _ := envValue(sub, EnvTaskAPI); v != "unix:///tmp/nexus-task/nx-abc/api.sock" {
		t.Errorf("subprocess %s = %q", EnvTaskAPI, v)
	}
	if v, _ := envValue(sub, EnvTaskAPIToken); v != "tok" {
		t.Errorf("subprocess %s = %q", EnvTaskAPIToken, v)
	}
	if v, _ := envValue(sub, EnvAttemptNumber); v != "3" {
		t.Errorf("subprocess %s = %q", EnvAttemptNumber, v)
	}

	dock := dockerEnv(spec, "/nexus")
	if v, _ := envValue(dock, EnvTaskAPI); v != "unix:///run/nexus-task/api.sock" {
		t.Errorf("docker %s = %q, want the container path", EnvTaskAPI, v)
	}
	if v, _ := envValue(dock, EnvTaskAPIToken); v != "tok" {
		t.Errorf("docker %s = %q", EnvTaskAPIToken, v)
	}
	if v, _ := envValue(dock, EnvAttemptNumber); v != "3" {
		t.Errorf("docker %s = %q", EnvAttemptNumber, v)
	}

	var sawNumber bool
	for _, e := range k8sEnv(spec) {
		switch e.Name {
		case EnvTaskAPI, EnvTaskAPIToken:
			t.Errorf("kubernetes must not export %s: the pod cannot reach the agent's socket (F149)", e.Name)
		case EnvAttemptNumber:
			sawNumber = e.Value == "3"
		}
	}
	if !sawNumber {
		t.Errorf("kubernetes: %s not exported", EnvAttemptNumber)
	}
}

func TestNoTaskAPIExportsNothing(t *testing.T) {
	for _, k := range []string{EnvTaskAPI, EnvTaskAPIToken, EnvAttemptNumber} {
		if v, ok := envValue(dockerEnv(TaskSpec{}, "/nexus"), k); ok {
			t.Errorf("docker exported %s=%q without a socket or attempt number", k, v)
		}
	}
}

// The docker executor mounts the socket's DIRECTORY read-write at
// /run/nexus-task, which keeps working under --network none.
func TestDockerRunArgsMountTheTaskAPIDirectory(t *testing.T) {
	spec := TaskSpec{AttemptID: "a1", TaskAPISocket: "/tmp/nexus-task/nx-a1/api.sock", TaskAPIToken: "tok"}
	st := &dockerState{
		image: "img:1", argv: []string{"python", "main.py"}, env: dockerEnv(spec, "/nexus"),
		workDir: "/work/a1", containerWD: "/nexus", name: "nexus-a1", taskAPIDir: taskAPIDirOf(spec),
	}
	args := dockerRunArgs(st, "a1", "none")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--volume /tmp/nexus-task/nx-a1:/run/nexus-task ") {
		t.Errorf("task API directory not mounted: %v", args)
	}
	if strings.Contains(joined, "/run/nexus-task:ro") {
		t.Error("the socket mount must be read-write: connecting is a write")
	}
	if !strings.Contains(joined, "--network none") || !strings.Contains(joined, "--volume /work/a1:/nexus") {
		t.Errorf("existing args changed: %v", args)
	}
	if !slices.Contains(args, "--env") || !strings.Contains(joined, EnvTaskAPI+"=unix:///run/nexus-task/api.sock") {
		t.Errorf("task API env missing: %v", args)
	}
	if args[len(args)-3] != "img:1" || args[len(args)-1] != "main.py" {
		t.Errorf("image and argv must come last: %v", args[len(args)-3:])
	}

	st.taskAPIDir, st.env = "", dockerEnv(TaskSpec{AttemptID: "a1"}, "/nexus")
	if strings.Contains(strings.Join(dockerRunArgs(st, "a1", "none"), " "), ContainerTaskAPIDir) {
		t.Error("no socket, yet the task API directory was mounted")
	}
}
