// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testCluster = "11111111-1111-1111-1111-111111111111"
	testUID     = "kube-system-uid-1"
	workerNS    = "nexus-workers"
	workerDep   = "workers-cpu"
)

// fakeRM emulates the resource manager's ClusterControllerService, INCLUDING
// its server-side apply gate and its persistence of cooldown/idle state, so a
// "restarted" controller built over the same fakeRM sees what a real restart
// would.
type fakeRM struct {
	mu         sync.Mutex
	assignment *rmv1.ClusterAssignment
	demand     *rmv1.Demand
	pods       map[string]*rmv1.PodWorkers
	now        func() time.Time

	failAssignment error
	failPoolState  error

	drains  []string
	records []*rmv1.RecordScalingRequest
	reports []*rmv1.ReportObservationsRequest
	errs    []*rmv1.ReportReconcileErrorRequest
}

func newFakeRM(mode string) *fakeRM {
	return &fakeRM{
		assignment: &rmv1.ClusterAssignment{
			ClusterId: testCluster, ClusterName: "test", Namespace: "acme", Provider: "kubernetes",
			ExpectedClusterUid: testUID, ControlPlaneNamespace: "kgf-nexus", Mode: mode, DesiredRevision: 7,
			Bindings: []*rmv1.PoolBinding{{
				Namespace: "acme", KubernetesNamespace: workerNS, WorkerDeployment: workerDep, Pool: "cpu", Queue: "cpu",
				Enabled: true, MinWorkers: 1, MaxWorkers: 10, TasksPerWorker: 2, IdleSeconds: 60, MaxScaleUpStep: 3,
				MaxScaleDownStep: 1, ScaleUpCooldownSeconds: 30, ScaleDownCooldownSeconds: 120,
				ProfileCpuMillis: 1000, ProfileMemoryMib: 1024, PlatformRevision: 3,
			}},
		},
		demand: &rmv1.Demand{},
		pods:   map[string]*rmv1.PodWorkers{},
		now:    time.Now,
	}
}

func (f *fakeRM) binding() *rmv1.PoolBinding { return f.assignment.Bindings[0] }

// register marks a pod as having one ACTIVE worker with the given leases.
func (f *fakeRM) register(host string, leases int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pods[host] = &rmv1.PodWorkers{Hostname: host, Active: 1, LeaseCount: leases}
}

func (f *fakeRM) GetAssignment(ctx context.Context, in *rmv1.GetAssignmentRequest, _ ...grpc.CallOption) (*rmv1.ClusterAssignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAssignment != nil {
		return nil, f.failAssignment
	}
	return cloneAssignment(f.assignment), nil
}

func cloneAssignment(a *rmv1.ClusterAssignment) *rmv1.ClusterAssignment {
	return proto.Clone(a).(*rmv1.ClusterAssignment)
}

func (f *fakeRM) ReportObservations(ctx context.Context, in *rmv1.ReportObservationsRequest, _ ...grpc.CallOption) (*rmv1.ReportObservationsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, in)
	return &rmv1.ReportObservationsResponse{DesiredRevision: f.assignment.DesiredRevision}, nil
}

func (f *fakeRM) GetPoolState(ctx context.Context, in *rmv1.GetPoolStateRequest, _ ...grpc.CallOption) (*rmv1.PoolState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPoolState != nil {
		return nil, f.failPoolState
	}
	out := &rmv1.PoolState{Demand: f.demand}
	for _, h := range in.Hostnames {
		if p, ok := f.pods[h]; ok {
			out.Pods = append(out.Pods, proto.Clone(p).(*rmv1.PodWorkers))
		} else {
			out.Pods = append(out.Pods, &rmv1.PodWorkers{Hostname: h})
		}
	}
	return out, nil
}

func (f *fakeRM) DrainPod(ctx context.Context, in *rmv1.DrainPodRequest, _ ...grpc.CallOption) (*rmv1.DrainPodResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The server-side gate, as the real resource manager enforces it.
	if f.assignment.Mode != "apply" || in.ObservedClusterUid != f.assignment.ExpectedClusterUid {
		return nil, status.Error(codes.FailedPrecondition, "cluster is not enabled for apply")
	}
	p, ok := f.pods[in.Hostname]
	if !ok {
		p = &rmv1.PodWorkers{Hostname: in.Hostname}
		f.pods[in.Hostname] = p
	}
	n := p.Active + p.Quarantined
	p.Draining += n
	p.Active, p.Quarantined = 0, 0
	f.drains = append(f.drains, in.Hostname)
	return &rmv1.DrainPodResponse{Drained: n}, nil
}

func (f *fakeRM) CheckPodRemovable(ctx context.Context, in *rmv1.CheckPodRemovableRequest, _ ...grpc.CallOption) (*rmv1.CheckPodRemovableResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pods[in.Hostname]
	if p == nil {
		return &rmv1.CheckPodRemovableResponse{Removable: true, Reason: "no live worker"}, nil
	}
	switch {
	case p.Active > 0:
		return &rmv1.CheckPodRemovableResponse{Reason: "ACTIVE worker(s) still accept dispatch", Workers: p}, nil
	case p.LeaseCount > 0:
		return &rmv1.CheckPodRemovableResponse{Reason: fmt.Sprintf("holds %d live lease(s)", p.LeaseCount), Workers: p}, nil
	case p.ReservationCount > 0:
		return &rmv1.CheckPodRemovableResponse{Reason: "holds reservations", Workers: p}, nil
	}
	return &rmv1.CheckPodRemovableResponse{Removable: true, Reason: "drained", Workers: p}, nil
}

func (f *fakeRM) RecordScaling(ctx context.Context, in *rmv1.RecordScalingRequest, _ ...grpc.CallOption) (*rmv1.RecordScalingResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, in)
	b := f.binding()
	if in.ScaledDirection != "" {
		b.LastScaleAtUnixMs = f.now().UnixMilli()
		b.LastScaleDirection = in.ScaledDirection
	}
	switch in.IdleUpdate {
	case rmv1.RecordScalingRequest_IDLE_UPDATE_SET:
		if b.IdleSinceUnixMs == 0 {
			b.IdleSinceUnixMs = in.IdleSinceUnixMs
		}
	case rmv1.RecordScalingRequest_IDLE_UPDATE_CLEAR:
		b.IdleSinceUnixMs = 0
	}
	return &rmv1.RecordScalingResponse{}, nil
}

func (f *fakeRM) ReportReconcileError(ctx context.Context, in *rmv1.ReportReconcileErrorRequest, _ ...grpc.CallOption) (*rmv1.ReportReconcileErrorResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs = append(f.errs, in)
	return &rmv1.ReportReconcileErrorResponse{}, nil
}

// events returns recorded non-empty event actions.
func (f *fakeRM) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.records {
		if r.Action != "" {
			out = append(out, r.Action)
		}
	}
	return out
}

// ── fake cluster ──────────────────────────────────────────────────────────

var deploymentsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

type fakeCluster struct {
	t      *testing.T
	kube   *fake.Clientset
	mu     sync.Mutex
	serial int
	scales []int32
}

// newFakeCluster builds a cluster with the kube-system namespace, one worker
// Deployment with `replicas` Running+Ready pods, and a ReplicaSet emulation
// that honours pod-deletion-cost when scaling in.
func newFakeCluster(t *testing.T, replicas int32) *fakeCluster {
	t.Helper()
	fc := &fakeCluster{t: t}
	fc.kube = fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(testUID)}},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: workerDep, Namespace: workerNS, Generation: 1, ResourceVersion: "1"},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "agent", Image: "example/agent@sha256:abc",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU: mustQ("500m"), corev1.ResourceMemory: mustQ("512Mi"),
					}},
				}}}},
			},
			Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: replicas, UpdatedReplicas: replicas,
				ReadyReplicas: replicas, AvailableReplicas: replicas},
		},
	)
	for i := int32(0); i < replicas; i++ {
		fc.addPod()
	}
	fc.kube.PrependReactor("update", "deployments", fc.scaleReactor)
	return fc
}

func (fc *fakeCluster) addPod() string {
	fc.serial++
	name := fmt.Sprintf("%s-pod-%02d", workerDep, fc.serial)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: workerNS, Labels: map[string]string{"app": "worker"},
			CreationTimestamp: metav1.NewTime(time.Unix(1_700_000_000+int64(fc.serial), 0))},
		Spec: corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	if err := fc.kube.Tracker().Add(pod); err != nil {
		fc.t.Fatalf("add pod: %v", err)
	}
	return name
}

// scaleReactor emulates the scale subresource, including the resourceVersion
// precondition, and then the ReplicaSet reacting to it.
func (fc *fakeCluster) scaleReactor(action k8stesting.Action) (bool, runtime.Object, error) {
	ua, ok := action.(k8stesting.UpdateAction)
	if !ok || action.GetSubresource() != "scale" {
		return false, nil, nil
	}
	scale := ua.GetObject().(*autoscalingv1.Scale)
	obj, err := fc.kube.Tracker().Get(deploymentsGVR, scale.Namespace, scale.Name)
	if err != nil {
		return true, nil, err
	}
	dep := obj.(*appsv1.Deployment).DeepCopy()
	if scale.ResourceVersion != "" && scale.ResourceVersion != dep.ResourceVersion {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, dep.Name,
			fmt.Errorf("resourceVersion %s != %s", scale.ResourceVersion, dep.ResourceVersion))
	}
	rv, _ := strconv.Atoi(dep.ResourceVersion)
	n := scale.Spec.Replicas
	dep.Spec.Replicas = &n
	dep.ResourceVersion = strconv.Itoa(rv + 1)
	dep.Generation++
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: n, UpdatedReplicas: n, ReadyReplicas: n, AvailableReplicas: n}
	if err := fc.kube.Tracker().Update(deploymentsGVR, dep, dep.Namespace); err != nil {
		return true, nil, err
	}
	fc.mu.Lock()
	fc.scales = append(fc.scales, n)
	fc.mu.Unlock()
	fc.reconcileReplicaSet(n)
	return true, &autoscalingv1.Scale{ObjectMeta: scale.ObjectMeta, Spec: scale.Spec}, nil
}

// reconcileReplicaSet deletes the lowest-deletion-cost pods (ties: newest) or
// adds pods, like the ReplicaSet controller does for equally-ranked pods.
func (fc *fakeCluster) reconcileReplicaSet(want int32) {
	// Runs inside a reactor, which holds the fake's lock: only the tracker may
	// be used here, never the clientset.
	pods := fc.trackerPods()
	for int32(len(pods)) < want {
		fc.addPod()
		pods = fc.trackerPods()
	}
	if int32(len(pods)) > want {
		sort.SliceStable(pods, func(i, j int) bool {
			ci, cj := deletionCost(&pods[i]), deletionCost(&pods[j])
			if ci != cj {
				return ci < cj
			}
			return pods[j].CreationTimestamp.Before(&pods[i].CreationTimestamp)
		})
		for _, p := range pods[:int32(len(pods))-want] {
			_ = fc.kube.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, workerNS, p.Name)
		}
	}
}

func (fc *fakeCluster) trackerPods() []corev1.Pod {
	obj, err := fc.kube.Tracker().List(schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, workerNS)
	if err != nil {
		fc.t.Fatalf("tracker list pods: %v", err)
	}
	return obj.(*corev1.PodList).Items
}

func (fc *fakeCluster) pods() []corev1.Pod {
	list, err := fc.kube.CoreV1().Pods(workerNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		fc.t.Fatalf("list pods: %v", err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items
}

func (fc *fakeCluster) podNames() []string {
	var out []string
	for _, p := range fc.pods() {
		out = append(out, p.Name)
	}
	return out
}

func (fc *fakeCluster) replicas() int32 {
	d, err := fc.kube.AppsV1().Deployments(workerNS).Get(context.Background(), workerDep, metav1.GetOptions{})
	if err != nil {
		fc.t.Fatalf("get deployment: %v", err)
	}
	return *d.Spec.Replicas
}

func (fc *fakeCluster) mutations() int {
	n := 0
	for _, a := range fc.kube.Actions() {
		switch a.GetVerb() {
		case "update", "patch", "create", "delete":
			if a.GetResource().Resource != "leases" {
				n++
			}
		}
	}
	return n
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newController(t *testing.T, fc *fakeCluster, rm *fakeRM, allowApply bool, clk *clock) *Controller {
	t.Helper()
	rm.now = clk.Now
	c, err := New(Config{ClusterName: "test", Identity: "controller-0", AllowApply: allowApply, Now: clk.Now,
		Interval: time.Second}, fc.kube, rm, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustQ(s string) resource.Quantity { return resource.MustParse(s) }
