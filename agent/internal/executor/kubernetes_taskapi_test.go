// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"strings"
	"testing"
)

// The kubernetes executor's half of the network workload API (F148): the pod
// is told where the agent's TLS listener is and which CA to pin, and gets its
// per-attempt bearer token by REFERENCE to the attempt's own Secret -- never
// as a literal in the pod spec, which anyone who can `get pods` or read an
// etcd backup could lift.

const testCA = "-----BEGIN CERTIFICATE-----\nMIIBtest\n-----END CERTIFICATE-----\n"

func remoteSpec(id string) TaskSpec {
	spec := unitSpec(id)
	spec.Limit = cpuMemLimit()
	spec.TaskAPIURL = "https://10.1.2.3:9443"
	spec.TaskAPICA = []byte(testCA)
	spec.TaskAPIToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	return spec
}

func TestK8sPodGetsTheNetworkTaskAPIByReference(t *testing.T) {
	e := gpuExecutor(t)
	spec := remoteSpec("tapi-1")
	job, secret, err := e.buildJob(spec, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	task := pod.Containers[0]

	var url, ca string
	var tokenRef bool
	for _, ev := range task.Env {
		switch ev.Name {
		case EnvTaskAPI:
			url = ev.Value
		case EnvTaskAPICA:
			ca = ev.Value
		case EnvTaskAPIToken:
			if ev.Value != "" {
				t.Fatalf("%s is a literal in the pod spec", EnvTaskAPIToken)
			}
			if ev.ValueFrom == nil {
				t.Fatalf("%s has no value source", EnvTaskAPIToken)
			}
			ref := ev.ValueFrom.SecretKeyRef
			if ref == nil || ref.Name != k8sSecretName(job.Name) || ref.Key != k8sTaskAPITokenKey {
				t.Fatalf("token reference %+v", ev.ValueFrom)
			}
			tokenRef = true
		}
	}
	if url != "https://10.1.2.3:9443" || ca != testCA || !tokenRef {
		t.Fatalf("url=%q ca=%q tokenRef=%v", url, ca, tokenRef)
	}
	// The per-attempt Secret exists even with no inputs, holds the token,
	// and is immutable.
	if secret == nil || string(secret.Data[k8sTaskAPITokenKey]) != spec.TaskAPIToken {
		t.Fatalf("secret %+v", secret)
	}
	if secret.Immutable == nil || !*secret.Immutable {
		t.Fatal("the token Secret is mutable")
	}
	// The token appears nowhere else: not in an annotation, a label, the
	// collector, or any literal env value.
	blob := job.String()
	if strings.Contains(blob, spec.TaskAPIToken) {
		t.Fatal("the bearer token appears literally in the Job")
	}
	for _, ev := range pod.InitContainers[0].Env {
		if strings.HasPrefix(ev.Name, "NEXUS_TASK_API") {
			t.Fatalf("the output collector sidecar was given %s", ev.Name)
		}
	}
	// The token key cannot collide with an input (sanitised [A-Za-z0-9_-]) or
	// a task secret ("nexus-secret-" prefix), and it is not projected into the
	// inputs directory.
	for _, v := range pod.Volumes {
		if v.Secret == nil {
			continue
		}
		for _, it := range v.Secret.Items {
			if it.Key == k8sTaskAPITokenKey {
				t.Fatalf("volume %s projects the token into the filesystem", v.Name)
			}
		}
	}
}

func TestK8sTaskAPIListenerConfigFromEnv(t *testing.T) {
	env := map[string]string{"NEXUS_K8S_NAMESPACE": "tasks", "NEXUS_K8S_OWNER": "a"}
	get := func(k string) string { return env[k] }
	cfg, _, err := LoadKubernetesConfigFromEnv(get)
	if err != nil || cfg.TaskAPIAdvertise != "" || cfg.TaskAPIListen != "" {
		t.Fatalf("default: %+v %v (the listener is opt-in)", cfg, err)
	}
	env["NEXUS_K8S_TASK_API_ADVERTISE"] = "10.0.0.7:9443"
	env["NEXUS_K8S_TASK_API_EXTRA_NAMES"] = "agent.nexus.svc, agent"
	cfg, _, err = LoadKubernetesConfigFromEnv(get)
	if err != nil || cfg.TaskAPIListen != ":9443" || len(cfg.TaskAPIExtraNames) != 2 {
		t.Fatalf("%+v %v", cfg, err)
	}
	env["NEXUS_K8S_TASK_API_LISTEN"] = "0.0.0.0:55944"
	if cfg, _, _ = LoadKubernetesConfigFromEnv(get); cfg.TaskAPIListen != "0.0.0.0:55944" {
		t.Fatalf("explicit listen lost: %+v", cfg)
	}
	for _, bad := range []string{"https://10.0.0.7:9443", "10.0.0.7"} {
		env["NEXUS_K8S_TASK_API_ADVERTISE"] = bad
		if _, _, err := LoadKubernetesConfigFromEnv(get); err == nil {
			t.Errorf("advertise %q accepted", bad)
		}
	}
	delete(env, "NEXUS_K8S_TASK_API_ADVERTISE")
	if _, _, err := LoadKubernetesConfigFromEnv(get); err == nil {
		t.Error("a listen address without an advertised one was accepted")
	}
}

// A task cannot point its own SDK elsewhere: the platform's values win over
// runtime.env, as for every other NEXUS_* variable.
func TestK8sTaskEnvCannotOverrideTheTaskAPI(t *testing.T) {
	e := gpuExecutor(t)
	spec := remoteSpec("tapi-2")
	spec.Runtime.Env = map[string]string{EnvTaskAPI: "https://evil:1", EnvTaskAPICA: "x", EnvTaskAPIToken: "forged"}
	job, _, err := e.buildJob(spec, "w")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, ev := range job.Spec.Template.Spec.Containers[0].Env {
		seen[ev.Name]++
		switch ev.Name {
		case EnvTaskAPI:
			if ev.Value != spec.TaskAPIURL {
				t.Errorf("%s = %q", ev.Name, ev.Value)
			}
		case EnvTaskAPICA:
			if ev.Value != testCA {
				t.Errorf("%s overridden", ev.Name)
			}
		case EnvTaskAPIToken:
			if ev.ValueFrom == nil || ev.Value != "" {
				t.Errorf("%s overridden by the task: %+v", ev.Name, ev)
			}
		}
	}
	for _, k := range []string{EnvTaskAPI, EnvTaskAPICA, EnvTaskAPIToken} {
		if seen[k] != 1 {
			t.Errorf("%s appears %d times", k, seen[k])
		}
	}
}

// Without a network listener the pod gets nothing (the SDK's in-process
// fallback), and a local socket path is never exported to a remote pod.
func TestK8sWithoutANetworkListenerExportsNoTaskAPI(t *testing.T) {
	e := gpuExecutor(t)
	spec := unitSpec("tapi-3")
	spec.Limit = cpuMemLimit()
	spec.TaskAPISocket = "/tmp/nexus-task/nx-x/api.sock"
	spec.TaskAPIToken = "tok"
	job, secret, err := e.buildJob(spec, "w")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range job.Spec.Template.Spec.Containers[0].Env {
		if strings.HasPrefix(ev.Name, "NEXUS_TASK_API") {
			t.Errorf("exported %s without a network listener", ev.Name)
		}
	}
	if secret != nil {
		t.Errorf("a Secret was created for an attempt with no inputs, secrets or task API")
	}
}
