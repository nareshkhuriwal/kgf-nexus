// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func start() *clock { return &clock{t: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)} }

func mustRun(t *testing.T, c *Controller) {
	t.Helper()
	if err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
}

func registerAll(fc *fakeCluster, rm *fakeRM) {
	for _, p := range fc.podNames() {
		if _, ok := rm.pods[p]; !ok {
			rm.register(p, 0)
		}
	}
}

func TestScaleOutOnQueuedDemand(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1), newFakeRM("apply")
	registerAll(fc, rm)
	// 10 pending at 2 per worker wants 5; the step limit of 3 caps this cycle at 1+3.
	rm.demand = &rmv1.Demand{PendingTasks: 10}
	c := newController(t, fc, rm, true, clk)

	mustRun(t, c)
	if got := fc.replicas(); got != 4 {
		t.Fatalf("replicas = %d, want 4 (1 + max step 3)", got)
	}
	if ev := rm.events(); !slices.Equal(ev, []string{"scale_out"}) {
		t.Fatalf("events = %v, want [scale_out]", ev)
	}
	last := rm.records[len(rm.records)-1]
	if !last.Applied || last.ScaledDirection != "out" || last.FromReplicas != 1 || last.ToReplicas != 4 {
		t.Fatalf("scale-out record = %+v", last)
	}

	// Within the scale-up cooldown nothing more happens even though demand remains.
	registerAll(fc, rm)
	clk.Advance(10 * time.Second)
	mustRun(t, c)
	if got := fc.replicas(); got != 4 {
		t.Fatalf("scaled again inside the cooldown: replicas = %d", got)
	}
	clk.Advance(30 * time.Second)
	mustRun(t, c)
	if got := fc.replicas(); got != 5 {
		t.Fatalf("after cooldown replicas = %d, want 5", got)
	}
}

func TestPlanModeChangesNothingAndDoesNotRepeatEvents(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1), newFakeRM("plan")
	registerAll(fc, rm)
	rm.demand = &rmv1.Demand{PendingTasks: 10}
	c := newController(t, fc, rm, true, clk)

	before := fc.mutations()
	for i := 0; i < 3; i++ {
		mustRun(t, c)
		clk.Advance(time.Minute)
	}
	if fc.mutations() != before {
		t.Fatalf("plan mode mutated the cluster: %v", fc.kube.Actions())
	}
	if ev := rm.events(); !slices.Equal(ev, []string{"plan"}) {
		t.Fatalf("events = %v, want exactly one plan event for an unchanged decision", ev)
	}
	rep := rm.reports[len(rm.reports)-1]
	if rep.EffectiveMode != "plan" || !strings.Contains(rep.EffectiveModeReason, "plan") {
		t.Fatalf("report mode = %q (%s)", rep.EffectiveMode, rep.EffectiveModeReason)
	}
	if rep.ObservedRevision != 7 {
		t.Fatalf("clean cycle observed revision = %d, want the desired 7", rep.ObservedRevision)
	}
}

func TestApplyNeedsProcessGateAndMatchingClusterIdentity(t *testing.T) {
	for name, tc := range map[string]struct {
		allow bool
		uid   string
		want  string
	}{
		"process not allowed": {false, testUID, "NEXUS_CONTROLLER_ALLOW_APPLY"},
		"wrong cluster":       {true, "some-other-cluster", "not the registered"},
		"no uid registered":   {true, "", "no expected cluster UID"},
	} {
		t.Run(name, func(t *testing.T) {
			clk := start()
			fc, rm := newFakeCluster(t, 1), newFakeRM("apply")
			rm.assignment.ExpectedClusterUid = tc.uid
			registerAll(fc, rm)
			rm.demand = &rmv1.Demand{PendingTasks: 10}
			c := newController(t, fc, rm, tc.allow, clk)
			before := fc.mutations()
			mustRun(t, c)
			if fc.mutations() != before {
				t.Fatalf("mutated without a full apply gate")
			}
			rep := rm.reports[len(rm.reports)-1]
			if rep.EffectiveMode != "plan" || !strings.Contains(rep.EffectiveModeReason, tc.want) {
				t.Fatalf("mode %q reason %q, want plan mentioning %q", rep.EffectiveMode, rep.EffectiveModeReason, tc.want)
			}
		})
	}
}

// idleCluster is a 3-replica pool that has been idle past its cooldown.
func idleCluster(t *testing.T) (*fakeCluster, *fakeRM, *Controller, *clock) {
	clk := start()
	fc, rm := newFakeCluster(t, 3), newFakeRM("apply")
	registerAll(fc, rm)
	rm.binding().IdleSinceUnixMs = clk.Now().Add(-10 * time.Minute).UnixMilli()
	return fc, rm, newController(t, fc, rm, true, clk), clk
}

func TestScaleInDrainsBeforeRemovingAndRemovesTheDrainedPod(t *testing.T) {
	fc, rm, c, clk := idleCluster(t)
	pods := fc.podNames()

	// Cycle 1: choose and drain ONE worker (step 1). Replicas unchanged.
	mustRun(t, c)
	if fc.replicas() != 3 {
		t.Fatalf("replicas changed before any drain completed: %d", fc.replicas())
	}
	if len(rm.drains) != 1 {
		t.Fatalf("drains = %v, want one", rm.drains)
	}
	victim := rm.drains[0]
	// Newest pod among equals is drained first.
	if victim != pods[2] {
		t.Fatalf("drained %s, want the newest pod %s", victim, pods[2])
	}

	// Cycle 2: the drained pod holds nothing, so it is removed -- and it is
	// THAT pod the ReplicaSet deletes, not a busy one.
	clk.Advance(15 * time.Second)
	mustRun(t, c)
	if fc.replicas() != 2 {
		t.Fatalf("replicas = %d, want 2 after removing the drained pod", fc.replicas())
	}
	if slices.Contains(fc.podNames(), victim) {
		t.Fatalf("the drained pod %s survived; a different pod was removed: %v", victim, fc.podNames())
	}
	if ev := rm.events(); !slices.Contains(ev, "scale_in") {
		t.Fatalf("events = %v, want a scale_in", ev)
	}
}

func TestWorkerHoldingLeasesIsNeverRemoved(t *testing.T) {
	fc, rm, c, clk := idleCluster(t)
	pods := fc.podNames()
	// The newest pod still holds a lease; the drain must pick a lease-free one.
	rm.pods[pods[2]].LeaseCount = 2
	mustRun(t, c)
	if len(rm.drains) != 1 || rm.drains[0] == pods[2] {
		t.Fatalf("drained %v; must prefer a worker holding no leases over %s", rm.drains, pods[2])
	}

	// Now make the drained worker acquire a lease before removal (a claim that
	// committed just before the drain). It must be kept, cycle after cycle.
	victim := rm.drains[0]
	rm.pods[victim].LeaseCount = 1
	for i := 0; i < 5; i++ {
		clk.Advance(time.Minute)
		mustRun(t, c)
		if !slices.Contains(fc.podNames(), victim) || fc.replicas() != 3 {
			t.Fatalf("cycle %d removed capacity while %s held a lease (pods %v, replicas %d)", i, victim, fc.podNames(), fc.replicas())
		}
	}
	last := rm.records[len(rm.records)-1]
	if !strings.Contains(last.Reason, "holds 1 live lease") {
		t.Fatalf("hold reason %q does not say why", last.Reason)
	}

	// Lease released: removed on the next cycle.
	rm.pods[victim].LeaseCount = 0
	clk.Advance(time.Minute)
	mustRun(t, c)
	if slices.Contains(fc.podNames(), victim) {
		t.Fatalf("drained lease-free pod %s not removed", victim)
	}
}

func TestScaleInHeldWhilePodsAreNotReady(t *testing.T) {
	fc, rm, c, _ := idleCluster(t)
	p := fc.pods()[0]
	p.Status.Conditions[0].Status = corev1.ConditionFalse
	if _, err := fc.kube.CoreV1().Pods(workerNS).UpdateStatus(context.Background(), &p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	mustRun(t, c)
	if len(rm.drains) != 0 {
		t.Fatalf("drained while a pod was not Ready: the ReplicaSet would prefer deleting the unready pod")
	}
	if !strings.Contains(rm.records[len(rm.records)-1].Reason, "not Running and Ready") {
		t.Fatalf("reason %q", rm.records[len(rm.records)-1].Reason)
	}
}

func TestBoundsAreEnforced(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 8), newFakeRM("apply")
	registerAll(fc, rm)
	rm.binding().MaxWorkers = 9
	rm.demand = &rmv1.Demand{PendingTasks: 1000}
	c := newController(t, fc, rm, true, clk)
	mustRun(t, c)
	if fc.replicas() != 9 {
		t.Fatalf("replicas = %d, want the max of 9", fc.replicas())
	}

	// Minimum: an idle pool never drains below min_workers.
	fc2, rm2, c2, clk2 := idleCluster(t)
	rm2.binding().MinWorkers = 3
	mustRun(t, c2)
	clk2.Advance(time.Hour)
	mustRun(t, c2)
	if len(rm2.drains) != 0 || fc2.replicas() != 3 {
		t.Fatalf("scaled below the minimum: drains %v replicas %d", rm2.drains, fc2.replicas())
	}
}

func TestNamespaceQuotaBoundsScaleOut(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1), newFakeRM("apply")
	registerAll(fc, rm)
	rm.demand = &rmv1.Demand{PendingTasks: 10}
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "workers", Namespace: workerNS},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{"requests.cpu": mustQ("1500m"), "pods": mustQ("10")}},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{"requests.cpu": mustQ("1500m"), "pods": mustQ("10")},
			Used: corev1.ResourceList{"requests.cpu": mustQ("500m"), "pods": mustQ("1")},
		},
	}
	if err := fc.kube.Tracker().Add(quota); err != nil {
		t.Fatal(err)
	}
	c := newController(t, fc, rm, true, clk)
	mustRun(t, c)
	// 1000m of CPU headroom at 500m per worker admits exactly 2 more.
	if fc.replicas() != 3 {
		t.Fatalf("replicas = %d, want 3 (quota headroom of 2)", fc.replicas())
	}
	if r := rm.records[len(rm.records)-1].Reason; !strings.Contains(r, "namespace quota") {
		t.Fatalf("reason %q does not name the quota", r)
	}
}

func TestResourceManagerFailureChangesNothing(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 3), newFakeRM("apply")
	registerAll(fc, rm)
	c := newController(t, fc, rm, true, clk)

	rm.failPoolState = status.Error(codes.Unavailable, "resource manager restarting")
	before := fc.mutations()
	if err := c.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the cycle to fail")
	}
	if fc.mutations() != before {
		t.Fatalf("mutated the cluster without a pool-state answer")
	}
	if len(rm.errs) != 1 || !strings.Contains(rm.errs[0].Message, "resource manager restarting") {
		t.Fatalf("reconcile error not reported: %+v", rm.errs)
	}
	if rep := rm.reports[len(rm.reports)-1]; rep.ObservedRevision != 0 || rep.LastError == "" {
		t.Fatalf("a failed cycle acknowledged the revision: %+v", rep)
	}

	rm.failPoolState = nil
	rm.failAssignment = status.Error(codes.Unavailable, "down")
	reports := len(rm.reports)
	if err := c.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the cycle to fail without an assignment")
	}
	if fc.mutations() != before || len(rm.reports) != reports {
		t.Fatalf("acted without an assignment")
	}
}

func TestKubernetesAPIFailureChangesNothing(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1), newFakeRM("apply")
	registerAll(fc, rm)
	rm.demand = &rmv1.Demand{PendingTasks: 10}
	fc.kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver overloaded")
	})
	c := newController(t, fc, rm, true, clk)
	before := fc.mutations()
	if err := c.RunOnce(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if fc.mutations() != before {
		t.Fatalf("scaled without seeing the pods")
	}
}

func TestConflictingDeploymentUpdateIsNotOverwritten(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1), newFakeRM("apply")
	registerAll(fc, rm)
	rm.demand = &rmv1.Demand{PendingTasks: 10}
	// A human changes the Deployment between the controller's read and write.
	var once sync.Once
	fc.kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		once.Do(func() {
			obj, _ := fc.kube.Tracker().Get(deploymentsGVR, workerNS, workerDep)
			d := obj.(interface{ DeepCopyObject() runtime.Object }).DeepCopyObject()
			dep := d.(metav1.Object)
			dep.SetResourceVersion("999")
			_ = fc.kube.Tracker().Update(deploymentsGVR, d, workerNS)
		})
		return false, nil, nil
	})
	c := newController(t, fc, rm, true, clk)
	err := c.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "re-planning next cycle") {
		t.Fatalf("err = %v, want a conflict", err)
	}
	if fc.replicas() != 1 {
		t.Fatalf("overwrote a concurrent change")
	}
	if ev := rm.events(); !slices.Contains(ev, "refused") {
		t.Fatalf("events = %v, want the refusal recorded", ev)
	}
}

func TestRestartKeepsCooldownAndIdleClock(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1), newFakeRM("apply")
	registerAll(fc, rm)
	rm.demand = &rmv1.Demand{PendingTasks: 10}
	mustRun(t, newController(t, fc, rm, true, clk))
	if fc.replicas() != 4 {
		t.Fatalf("replicas = %d", fc.replicas())
	}

	// A new process (restart / leader change) inside the cooldown must not
	// scale again: the cooldown lives in the resource manager, not in memory.
	registerAll(fc, rm)
	clk.Advance(5 * time.Second)
	mustRun(t, newController(t, fc, rm, true, clk))
	if fc.replicas() != 4 {
		t.Fatalf("restarted controller ignored the persisted cooldown: replicas %d", fc.replicas())
	}

	// Idle clock: demand stops; a restart must not reset "idle since".
	rm.demand = &rmv1.Demand{}
	clk.Advance(time.Minute)
	mustRun(t, newController(t, fc, rm, true, clk))
	idleSince := rm.binding().IdleSinceUnixMs
	if idleSince == 0 {
		t.Fatal("idle clock not persisted")
	}
	clk.Advance(30 * time.Second)
	mustRun(t, newController(t, fc, rm, true, clk))
	if rm.binding().IdleSinceUnixMs != idleSince {
		t.Fatalf("restart reset the idle clock")
	}
}

func TestObservationsAreReportedPerKind(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1), newFakeRM("plan")
	registerAll(fc, rm)
	two := int32(2)
	sixty := int32(60)
	objs := []runtime.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "aks-pool-1", Labels: map[string]string{"kubernetes.azure.com/agentpool": "tasks", "secret-ish": "x"}},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
				Allocatable: corev1.ResourceList{corev1.ResourceCPU: mustQ("4")}}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "nexus-task-abc", Namespace: workerNS, Labels: map[string]string{"nexus.io/attempt-id": "a-1"}},
			Status: batchv1.JobStatus{Active: 1}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: workerNS}},
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "scheduler", Namespace: "kgf-nexus"},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: &two, MaxReplicas: 6, Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU, Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: &sixty}}}}}},
	}
	for _, o := range objs {
		if err := fc.kube.Tracker().Add(o); err != nil {
			t.Fatal(err)
		}
	}
	mustRun(t, newController(t, fc, rm, false, clk))
	rep := rm.reports[len(rm.reports)-1]
	for _, k := range []string{"WorkerDeployment", "SchedulerDeployment", "HorizontalPodAutoscaler", "Node", "Job"} {
		if !slices.Contains(rep.CompleteKinds, k) {
			t.Errorf("kind %s not complete: %v", k, rep.CompleteKinds)
		}
	}
	kinds := map[string][]*rmv1.ObservedObject{}
	for _, o := range rep.Objects {
		kinds[o.Kind] = append(kinds[o.Kind], o)
	}
	if len(kinds["Job"]) != 1 || !strings.Contains(kinds["Job"][0].SummaryJson, `"attempt_id":"a-1"`) {
		t.Errorf("jobs = %+v, want only the labelled task job", kinds["Job"])
	}
	if len(kinds["Node"]) != 1 || strings.Contains(kinds["Node"][0].SummaryJson, "secret-ish") {
		t.Errorf("node summary leaks arbitrary labels: %+v", kinds["Node"])
	}
	if len(kinds["WorkerDeployment"]) != 1 || kinds["WorkerDeployment"][0].ObservedGeneration != 1 {
		t.Errorf("worker deployment observation missing generation: %+v", kinds["WorkerDeployment"])
	}
	if rep.KubernetesVersion == "" && rep.ObservedClusterUid != testUID {
		t.Errorf("identity not reported: %+v", rep)
	}

	// A forbidden node list leaves Node out of complete_kinds rather than
	// reporting zero nodes.
	fc.kube.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("rbac"))
	})
	_ = newController(t, fc, rm, false, clk).RunOnce(context.Background())
	rep = rm.reports[len(rm.reports)-1]
	if slices.Contains(rep.CompleteKinds, "Node") {
		t.Fatalf("Node reported complete after a failed list")
	}
	if rep.ObservedRevision != 0 {
		t.Fatalf("partial observation acknowledged the revision")
	}
}

func TestQuotaHeadroom(t *testing.T) {
	tmpl := corev1.PodSpec{Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: mustQ("250m"), corev1.ResourceMemory: mustQ("1Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: mustQ("2Gi")},
	}}}}
	q := func(hard, used corev1.ResourceList) corev1.ResourceQuota {
		return corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "q"}, Spec: corev1.ResourceQuotaSpec{Hard: hard},
			Status: corev1.ResourceQuotaStatus{Used: used}}
	}
	cases := []struct {
		name  string
		quota corev1.ResourceQuota
		want  int
	}{
		{"cpu", q(corev1.ResourceList{"requests.cpu": mustQ("1")}, corev1.ResourceList{"requests.cpu": mustQ("0")}), 4},
		{"memory limits", q(corev1.ResourceList{"limits.memory": mustQ("5Gi")}, corev1.ResourceList{"limits.memory": mustQ("1Gi")}), 2},
		{"pods", q(corev1.ResourceList{"pods": mustQ("3")}, corev1.ResourceList{"pods": mustQ("3")}), 0},
		{"undeclared gpu", q(corev1.ResourceList{"requests.nvidia.com/gpu": mustQ("4")}, corev1.ResourceList{"requests.nvidia.com/gpu": mustQ("0")}), 0},
		{"usage unknown", q(corev1.ResourceList{"pods": mustQ("10")}, nil), 0},
		{"over-used", q(corev1.ResourceList{"requests.cpu": mustQ("1")}, corev1.ResourceList{"requests.cpu": mustQ("2")}), 0},
		{"unrelated", q(corev1.ResourceList{"services": mustQ("1")}, corev1.ResourceList{"services": mustQ("1")}), 1<<31 - 1},
	}
	for _, tc := range cases {
		if got, why := QuotaHeadroom([]corev1.ResourceQuota{tc.quota}, tmpl); got != tc.want {
			t.Errorf("%s: headroom %d (%s), want %d", tc.name, got, why, tc.want)
		}
	}
}

func TestLeaderElectionRunsOneReplicaAtATime(t *testing.T) {
	kube := fake.NewClientset()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var active, maxActive, runs atomic.Int32
	work := func(ctx context.Context) error {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			if m := maxActive.Load(); n > m {
				maxActive.CompareAndSwap(m, n)
			}
			runs.Add(1)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	cfg := func(id string) LeaderConfig {
		return LeaderConfig{Namespace: "kgf-nexus", LeaseName: "test", Identity: id,
			LeaseDuration: 1500 * time.Millisecond, RenewDeadline: time.Second, RetryPeriod: 100 * time.Millisecond}
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	var la, lb Leading
	doneA := make(chan error, 1)
	go func() { doneA <- RunLeaderElected(ctxA, kube, cfg("a"), &la, log, work) }()
	waitFor(t, "a leads", func() bool { return la.Is() })
	go func() { _ = RunLeaderElected(ctxB, kube, cfg("b"), &lb, log, work) }()
	time.Sleep(700 * time.Millisecond)
	if lb.Is() {
		t.Fatal("b leads while a holds the lease")
	}
	cancelA()
	if err := <-doneA; err != nil {
		t.Fatalf("a exited with %v", err)
	}
	waitFor(t, "b takes over", func() bool { return lb.Is() })
	if maxActive.Load() != 1 {
		t.Fatalf("%d replicas worked at once", maxActive.Load())
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
