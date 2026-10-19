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
	cases := []struct {
		def      string
		hasImage bool
		want     bool
	}{
		{ExecutorSubprocess, false, true},
		{ExecutorSubprocess, true, true},
		{ExecutorKubernetes, true, true},
		{ExecutorKubernetes, false, false},
		{ExecutorK8sAlias, false, false},
		{ExecutorDocker, false, false},
		{ExecutorDocker, true, true},
	}
	for _, c := range cases {
		if got := CanRunUnnamed(c.def, c.hasImage); got != c.want {
			t.Errorf("CanRunUnnamed(%q, image=%v) = %v, want %v", c.def, c.hasImage, got, c.want)
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
