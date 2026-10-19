// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import "testing"

func TestWorkerDefaultExecutorAbsentMeansSubprocess(t *testing.T) {
	cases := []struct {
		labels map[string]string
		want   string
	}{
		{nil, ExecutorSubprocess},
		{map[string]string{"zone": "a"}, ExecutorSubprocess},
		{map[string]string{LabelDefaultExecutor: ""}, ExecutorSubprocess},
		{map[string]string{LabelDefaultExecutor: "kubernetes"}, ExecutorKubernetes},
		{map[string]string{LabelDefaultExecutor: "k8s"}, ExecutorKubernetes},
		{map[string]string{LabelDefaultExecutor: "docker"}, ExecutorDocker},
	}
	for _, c := range cases {
		if got := WorkerDefaultExecutor(c.labels); got != c.want {
			t.Errorf("WorkerDefaultExecutor(%v) = %q, want %q", c.labels, got, c.want)
		}
	}
}

func TestEffectiveExecutorPrefersTheNamedOne(t *testing.T) {
	k8sWorker := map[string]string{LabelDefaultExecutor: ExecutorKubernetes}
	if got := EffectiveExecutor("", k8sWorker); got != ExecutorKubernetes {
		t.Errorf("unnamed on a kubernetes-default worker = %q", got)
	}
	if got := EffectiveExecutor("subprocess", k8sWorker); got != ExecutorSubprocess {
		t.Errorf("named subprocess = %q", got)
	}
	if got := EffectiveExecutor("k8s", nil); got != ExecutorKubernetes {
		t.Errorf("named k8s = %q, want the canonical spelling", got)
	}
	if got := EffectiveExecutor("", nil); got != ExecutorSubprocess {
		t.Errorf("unnamed on a pre-label worker = %q", got)
	}
}

func TestCanRunUnnamed(t *testing.T) {
	image := UnnamedTask{HasImage: true}
	bare := UnnamedTask{}
	stdout := UnnamedTask{HasImage: true, StdoutOutput: true}
	cases := []struct {
		def  string
		task UnnamedTask
		want string
	}{
		{ExecutorSubprocess, bare, ""},
		{ExecutorSubprocess, stdout, ""},
		{"", bare, ""},
		{ExecutorKubernetes, image, ""},
		{ExecutorKubernetes, bare, "kubernetes needs runtime.image"},
		{ExecutorK8sAlias, bare, "kubernetes needs runtime.image"},
		{ExecutorKubernetes, stdout, "kubernetes does not support runtime.stdout_output"},
		{ExecutorDocker, bare, "docker needs runtime.image"},
		{ExecutorDocker, stdout, ""},
	}
	for _, c := range cases {
		if got := UnnamedRefusal(c.def, c.task); got != c.want {
			t.Errorf("UnnamedRefusal(%q, %+v) = %q, want %q", c.def, c.task, got, c.want)
		}
		if got := CanRunUnnamed(c.def, c.task); got != (c.want == "") {
			t.Errorf("CanRunUnnamed(%q, %+v) = %v", c.def, c.task, got)
		}
	}
}

func TestKnownExecutors(t *testing.T) {
	for _, k := range []string{"subprocess", "docker", "kubernetes", "k8s"} {
		if !IsKnownExecutor(k) {
			t.Errorf("%q should be known", k)
		}
	}
	for _, k := range []string{"", "podman", "Kubernetes", "wasm"} {
		if IsKnownExecutor(k) {
			t.Errorf("%q should not be known", k)
		}
	}
}
