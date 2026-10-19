//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// A task that names no runtime.executor runs on the claiming worker's DEFAULT
// executor, which the agent advertises as the nexus.io/default-executor label
// (NEXUS_DEFAULT_EXECUTOR). Before the label, every worker ran such a task as a
// subprocess, so "any worker" was the right claim. Once a pool can be
// kubernetes-only, the claim must only hand an unnamed task to a worker whose
// default can run it: a container default needs the task to carry an image.

// kubernetesPool is a kubernetes-only worker defaulting to kubernetes, as the
// chart renders agents with executors: kubernetes.
func kubernetesPool(f *fixture, labels map[string]string) uuid.UUID {
	l := map[string]string{domain.LabelDefaultExecutor: domain.ExecutorKubernetes}
	for k, v := range labels {
		l[k] = v
	}
	return f.addWorker(vec{CPUMillis: 64000, MemoryBytes: 1 << 36}, l, []string{"k8s", "kubernetes"})
}

func workerOf(t *testing.T, f *fixture, attemptID uuid.UUID) uuid.UUID {
	t.Helper()
	var w *uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT worker_id FROM task_attempts WHERE id = $1`, attemptID).Scan(&w); err != nil {
		t.Fatalf("read worker_id: %v", err)
	}
	if w == nil {
		return uuid.Nil
	}
	return *w
}

// TestUnnamedExecutorPushClaimUsesTheWorkersDefault: the background dispatch
// loop (attempt -> workers).
func TestUnnamedExecutorPushClaimUsesTheWorkersDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t, newTestPool(t))
	ctx := context.Background()
	st := f.newStore(nil)
	pool := kubernetesPool(f, nil)
	now := time.Now().UTC()

	// An unnamed task WITH an image, in the DAG engine's dialect: it becomes a
	// pod on the kubernetes pool.
	pod := f.addAttempt("pod", 300, vec{CPUMillis: 1000},
		nodeSpec{TaskRef: "t", Version: 1, Runtime: map[string]any{"image": "reg/train:1", "command": []string{"python"}}}, now)
	rep, err := st.Claim(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != store.ClaimDispatched || rep.Attempt.ID != pod {
		t.Fatalf("outcome %s for %v, want the image-carrying unnamed task dispatched", rep.Outcome, rep.Attempt)
	}
	if got := workerOf(t, f, pod); got != pool {
		t.Fatalf("dispatched to %s, want the kubernetes pool %s", got, pool)
	}

	// An unnamed task WITHOUT an image cannot run on a kubernetes default: it
	// waits, and says why.
	bare := f.addAttempt("bare", 200, vec{CPUMillis: 1000},
		nodeSpec{TaskRef: "t", Version: 1, Runtime: map[string]any{"command": []string{"echo", "hi"}}}, now)
	rep, err = st.Claim(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != store.ClaimBlocked || rep.Attempt.ID != bare {
		t.Fatalf("outcome %s for %v, want the image-less task blocked", rep.Outcome, rep.Attempt)
	}
	r := f.pendingReason(bare)
	for _, want := range []string{"waiting_for_worker:", "kubernetes needs runtime.image", domain.LabelDefaultExecutor} {
		if !strings.Contains(r, want) {
			t.Errorf("pending_reason %q does not contain %q", r, want)
		}
	}

	// With an image but runtime.stdout_output, which the kubernetes executor
	// refuses: the same, for that reason. (Above bare's priority, so this
	// claim examines it.)
	stdout := f.addAttempt("stdout", 250, vec{CPUMillis: 1000},
		nodeSpec{TaskRef: "t", Version: 1, Runtime: map[string]any{"image": "reg/x:1", "stdout_output": "result"}}, now)
	rep, err = st.Claim(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != store.ClaimBlocked || rep.Attempt.ID != stdout {
		t.Fatalf("outcome %s for %v, want the stdout_output task blocked", rep.Outcome, rep.Attempt)
	}
	if r := f.pendingReason(stdout); !strings.Contains(r, "kubernetes does not support runtime.stdout_output") {
		t.Errorf("pending_reason %q does not name stdout_output", r)
	}

	// A task that NAMES subprocess is never given to a kubernetes-only pool
	// (the pre-existing executor constraint, unchanged).
	named := f.addAttempt("named", 100, vec{CPUMillis: 1000},
		nodeSpec{TaskRef: "t", Version: 1, Runtime: map[string]any{"executor": "subprocess", "command": []string{"true"}}}, now)
	for i := 0; i < 4; i++ {
		if _, err := st.Claim(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []uuid.UUID{bare, stdout, named} {
		if s := f.attemptState(id); s != "PENDING" {
			t.Fatalf("attempt %s is %s, want PENDING with only the kubernetes pool", id, s)
		}
	}

	// A worker that predates the label defaults to subprocess and takes all three.
	legacy := f.addWorker(vec{CPUMillis: 8000}, nil, []string{"subprocess"})
	pending := func() bool {
		return f.attemptState(bare) == "PENDING" || f.attemptState(stdout) == "PENDING" || f.attemptState(named) == "PENDING"
	}
	for i := 0; i < 9 && pending(); i++ {
		if _, err := st.Claim(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []uuid.UUID{bare, stdout, named} {
		if got := workerOf(t, f, id); got != legacy {
			t.Errorf("attempt %s went to %s, want the subprocess worker %s", id, got, legacy)
		}
	}
}

// TestUnnamedExecutorPullClaimUsesTheWorkersDefault: a polling worker
// (worker -> attempts), where the rule is a SQL predicate.
func TestUnnamedExecutorPullClaimUsesTheWorkersDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t, newTestPool(t))
	ctx := context.Background()
	st := f.newStore(nil)
	pool := kubernetesPool(f, nil)
	legacy := f.addWorker(vec{CPUMillis: 8000}, nil, []string{"subprocess"})
	explicitDefault := f.addWorker(vec{CPUMillis: 8000},
		map[string]string{domain.LabelDefaultExecutor: "subprocess"}, []string{"subprocess", "kubernetes"})
	old := time.Now().UTC().Add(-time.Minute) // past the placement wait

	// The image-less task is the OLDEST and highest priority: a pool that
	// stopped at it would starve the work it can run behind it.
	bare := f.addAttempt("bare", 900, vec{CPUMillis: 100},
		nodeSpec{TaskRef: "t", Version: 1, Runtime: map[string]any{"command": []string{"echo"}}}, old.Add(-time.Minute))
	named := f.addAttempt("named", 800, vec{CPUMillis: 100},
		nodeSpec{TaskRef: "t", Version: 1, Executor: "subprocess"}, old)
	flat := f.addAttempt("flat-image", 700, vec{CPUMillis: 100},
		nodeSpec{TaskRef: "t", Version: 1, Image: "reg/a:1"}, old)
	nested := f.addAttempt("nested-image", 600, vec{CPUMillis: 100},
		nodeSpec{TaskRef: "t", Version: 1, Runtime: map[string]any{"image": "reg/b:1"}}, old)
	// An image, but stdout_output, which the kubernetes executor refuses.
	stdout := f.addAttempt("stdout", 500, vec{CPUMillis: 100},
		nodeSpec{TaskRef: "t", Version: 1, Runtime: map[string]any{"image": "reg/c:1", "stdout_output": "o"}}, old)

	got, err := st.ClaimForWorker(ctx, time.Now().UTC(), store.WorkerClaimRequest{WorkerID: pool, MaxTasks: 8})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[uuid.UUID]bool{}
	for _, a := range got {
		ids[a.AttemptID] = true
	}
	if len(got) != 2 || !ids[flat] || !ids[nested] {
		t.Fatalf("kubernetes pool claimed %v, want exactly the two image-carrying unnamed tasks", got)
	}

	got, err = st.ClaimForWorker(ctx, time.Now().UTC(), store.WorkerClaimRequest{WorkerID: legacy, MaxTasks: 1})
	if err != nil || len(got) != 1 || got[0].AttemptID != bare {
		t.Fatalf("legacy worker claimed %v (%v), want the image-less unnamed task", got, err)
	}
	if f.attemptState(stdout) != "PENDING" {
		t.Fatal("the kubernetes pool claimed a stdout_output task")
	}
	got, err = st.ClaimForWorker(ctx, time.Now().UTC(), store.WorkerClaimRequest{WorkerID: explicitDefault, MaxTasks: 1})
	if err != nil || len(got) != 1 || got[0].AttemptID != named {
		t.Fatalf("subprocess-default worker claimed %v (%v), want the task naming subprocess", got, err)
	}
}

// TestUnnamedTaskOnAKubernetesDefaultIsGatedByClusterHeadroom: a task that
// names no executor becomes a pod on a kubernetes-default worker, so the
// cluster's quota headroom gates it exactly like one naming kubernetes.
func TestUnnamedTaskOnAKubernetesDefaultIsGatedByClusterHeadroom(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	f.addCluster("full-"+f.ns, nil, map[string]any{"tasks": map[string]any{"cpu_millis": 1000, "memory_bytes": -1,
		"gpu_count": -1, "pods": 10}}, time.Now().UTC())
	full := kubernetesPool(f, map[string]string{domain.LabelCluster: "full-" + f.ns, domain.LabelK8sTaskNamespace: "tasks"})
	id := f.addAttempt("k", 100, vec{CPUMillis: 2000},
		nodeSpec{TaskRef: "t", Version: 1, Runtime: map[string]any{"image": "reg/x:1"}}, time.Now().UTC().Add(-time.Minute))
	st := f.newStore(nil)
	if got, _ := st.ClaimForWorker(context.Background(), time.Now().UTC(),
		store.WorkerClaimRequest{WorkerID: full, MaxTasks: 1}); len(got) != 0 {
		t.Fatal("an unnamed task became a pod in a cluster whose headroom is 1000m for 2000m")
	}
	if r := f.pendingReason(id); !strings.Contains(r, "waiting_for_cluster_quota") {
		t.Fatalf("pending_reason %q", r)
	}
}

// TestUsageLedgerAttributesAnUnnamedTaskToTheWorkersDefault: metering records
// the executor that ran the attempt, not an empty one, when the task named
// none; a worker without the label keeps recording NULL as before.
func TestUsageLedgerAttributesAnUnnamedTaskToTheWorkersDefault(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	st := f.newStore(nil)
	k8s := kubernetesPool(f, nil)
	id := f.dispatchOne(st, k8s, "pod", vec{CPUMillis: 1000},
		nodeSpec{TaskRef: "t", Version: 1, Runtime: map[string]any{"image": "reg/x:1"}})
	succeed(t, st, id, nil)
	if r := f.ledger(id); r.Executor == nil || *r.Executor != "kubernetes" {
		t.Errorf("ledger executor = %v, want kubernetes (the worker's default)", r.Executor)
	}

	f2 := newFixture(t, pool)
	st2 := f2.newStore(nil)
	legacy := f2.addWorker(vec{CPUMillis: 8000}, nil, []string{"subprocess"})
	id2 := f2.dispatchOne(st2, legacy, "sub", vec{CPUMillis: 1000}, nodeSpec{TaskRef: "t", Version: 1})
	succeed(t, st2, id2, nil)
	if r := f2.ledger(id2); r.Executor != nil {
		t.Errorf("ledger executor = %q for a worker without the label, want NULL as before", *r.Executor)
	}
}
