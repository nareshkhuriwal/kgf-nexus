// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package platform

import (
	"strings"
	"testing"
)

func valid() Config {
	c := Default()
	c.Profiles = []PodProfile{{Name: "cpu", Cloud: "aws", KubernetesNamespace: "nexus", ServiceAccount: "tasks", Image: "example/tasks@sha256:" + strings.Repeat("a", 64), CPU: 2000, Memory: 4096, EphemeralStorage: 1024, DeadlineSeconds: 3600, GraceSeconds: 30}}
	c.Pools = []Pool{{Name: "cpu", Queue: "cpu", Profile: "cpu", MaxWorkers: 20, TasksPerWorker: 2, IdleSeconds: 300, MaxScaleUpStep: 5, Enabled: true}}
	return c
}
func TestValidation(t *testing.T) {
	c := valid()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Profiles[0].Image = "image:latest"
	if c.Validate() == nil {
		t.Fatal("accepted mutable image")
	}
	c = valid()
	c.Pools[0].MaxWorkers = 10001
	if c.Validate() == nil {
		t.Fatal("accepted unbounded pool")
	}
	c = valid()
	c.Pools = append(c.Pools, c.Pools[0])
	if c.Validate() == nil {
		t.Fatal("accepted duplicate queues")
	}
}
func TestScalingBoundsAndScaleIn(t *testing.T) {
	c := valid()
	p := c.Pools[0]
	profile := c.Profiles[0]
	d := Demand{Pending: 100, CPU: 100000, CurrentWorkers: 2}
	r := Plan(p, profile, d)
	if r.Desired != 7 || !r.Limited {
		t.Fatalf("unbounded surge: %+v", r)
	}
	d = Demand{CurrentWorkers: 10, Running: 1, IdleSeconds: 999}
	if Plan(p, profile, d).Desired != 10 {
		t.Fatal("removed busy capacity")
	}
	d.Running = 0
	if Plan(p, profile, d).Desired != 0 {
		t.Fatal("idle pool did not scale to zero")
	}
	p.Enabled = false
	if Plan(p, profile, d).Desired != 10 {
		t.Fatal("disabled policy changed capacity")
	}
}
func TestPodIsolationAndRetryOwnership(t *testing.T) {
	p := valid().Profiles[0]
	p.Cloud = "azure"
	job := JobTemplate(p)
	spec := job["spec"].(map[string]any)
	if spec["backoffLimit"] != 0 {
		t.Fatal("Kubernetes must not own Nexus retries")
	}
	template := spec["template"].(map[string]any)
	labels := template["metadata"].(map[string]any)["labels"].(map[string]string)
	if labels["azure.workload.identity/use"] != "true" {
		t.Fatal("missing workload identity label")
	}
	pod := template["spec"].(map[string]any)
	if pod["automountServiceAccountToken"] != false {
		t.Fatal("default API credential leaked")
	}
}

// TestFairShareWeightBounds pins ADR-0017's range: 0 would take a namespace
// out of dispatch entirely and an unbounded weight would make one namespace
// effectively exclusive, so both ends are rejected; the default is neutral.
func TestFairShareWeightBounds(t *testing.T) {
	if Default().Scheduler.FairShareWeight != DefaultFairShareWeight {
		t.Fatalf("default weight = %d, want %d", Default().Scheduler.FairShareWeight, DefaultFairShareWeight)
	}
	for _, tc := range []struct {
		w  int
		ok bool
	}{{0, false}, {1, true}, {100, true}, {101, false}, {-3, false}} {
		c := valid()
		c.Scheduler.FairShareWeight = tc.w
		if err := c.Validate(); (err == nil) != tc.ok {
			t.Errorf("weight %d: err=%v, want ok=%v", tc.w, err, tc.ok)
		}
	}
}
