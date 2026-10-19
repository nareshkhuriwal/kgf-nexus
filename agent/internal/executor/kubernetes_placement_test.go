// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestK8sReportsThePodsPlacementAsItChanges is ADR-0037 for the kubernetes
// executor: OnPlacement hears the pod's name and namespace when the pod
// appears, its node once it is bound, and nothing more after that however
// many phase changes follow.
func TestK8sReportsThePodsPlacementAsItChanges(t *testing.T) {
	e, cs := newFakeExecutor(t)
	e.logStream = cannedLogs(k8sFrameComplete + "\n")
	spec := unitSpec("place-1")
	var mu sync.Mutex
	var seen []PodPlacement
	spec.OnPlacement = func(p PodPlacement) { mu.Lock(); seen = append(seen, p); mu.Unlock() }
	awaitPlacement := func(want PodPlacement) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := len(seen)
			last := PodPlacement{}
			if n > 0 {
				last = seen[n-1]
			}
			mu.Unlock()
			if last == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("OnPlacement never reported %+v; saw %+v", want, seen)
	}

	prepared := make(chan Prepared, 1)
	go func() {
		p, err := e.Prepare(context.Background(), spec)
		if err != nil {
			t.Errorf("Prepare: %v", err)
		}
		prepared <- p
	}()
	k := &kubelet{t: t, cs: cs, job: awaitJob(t, cs, k8sJobName("place-1"))}
	k.createPod()
	awaitPlacement(PodPlacement{Namespace: fakeNS, Pod: k.pod.Name})

	// The scheduler binds the pod.
	k.pod.Spec.NodeName = "aks-gpu-vmss000003"
	if _, err := cs.CoreV1().Pods(fakeNS).Update(context.Background(), k.pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	awaitPlacement(PodPlacement{Namespace: fakeNS, Pod: k.pod.Name, Node: "aks-gpu-vmss000003"})

	k.setStatus(runningStatus())
	h, err := e.Start(context.Background(), <-prepared)
	if err != nil {
		t.Fatal(err)
	}
	k.setStatus(doneStatus(0))
	if o, err := e.Wait(context.Background(), h); err != nil || !o.Succeeded() {
		t.Fatalf("Wait: %+v %v", o, err)
	}
	_ = e.Cleanup(context.Background(), h)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Errorf("placements reported = %+v; want exactly the pod, then the pod with its node", seen)
	}
}

func TestOnlyTheKubernetesExecutorIsAPlacementSource(t *testing.T) {
	var k Executor = NewKubernetesExecutor(KubernetesConfig{Namespace: fakeNS}, nil, t.TempDir(), nil)
	if src, ok := k.(PlacementSource); !ok || !src.ReportsPlacement() {
		t.Error("the kubernetes executor must report its pods' placement itself")
	}
	for _, e := range []Executor{&SubprocessExecutor{}, &DockerExecutor{}} {
		if _, ok := e.(PlacementSource); ok {
			t.Errorf("%T claims to report placement; its workload runs beside the agent", e)
		}
	}
}
