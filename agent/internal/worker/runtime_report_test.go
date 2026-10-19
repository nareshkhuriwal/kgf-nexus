// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/instances/identity"
)

// fakePlacementClient records ReportPlacement calls. fail answers the first
// fail calls with a transport error; answer, when set, decides the rest.
type fakePlacementClient struct {
	mu     sync.Mutex
	calls  []*nexusv1.ReportPlacementRequest
	fail   int
	answer func(*nexusv1.ReportPlacementRequest) (*nexusv1.Ack, error)
}

func (f *fakePlacementClient) ReportPlacement(_ context.Context, req *nexusv1.ReportPlacementRequest) (*nexusv1.Ack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.fail > 0 {
		f.fail--
		return nil, status.Error(codes.Unavailable, "gateway restarting")
	}
	if f.answer != nil {
		return f.answer(req)
	}
	return &nexusv1.Ack{Accepted: true}, nil
}

func (f *fakePlacementClient) snapshot() []*nexusv1.ReportPlacementRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*nexusv1.ReportPlacementRequest(nil), f.calls...)
}

func waitCalls(t *testing.T, f *fakePlacementClient, cond func([]*nexusv1.ReportPlacementRequest) bool) []*nexusv1.ReportPlacementRequest {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		calls := f.snapshot()
		if cond(calls) {
			return calls
		}
		if time.Now().After(deadline) {
			t.Fatalf("placement calls never matched: %v", calls)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var agentRuntime = identity.Runtime{Pod: "nexus-agent-0", PodNamespace: "nexus-workers", Node: "aks-node-1", Cluster: "aks-prod"}

// TestSubprocessAttemptReportsTheAgentsPlacement: the workload runs beside the
// agent, so its placement is the agent's own pod and node, fenced with the
// attempt's lease token.
func TestSubprocessAttemptReportsTheAgentsPlacement(t *testing.T) {
	r, reg, _ := newTestRunner(t, &fakeReportClient{})
	pc := &fakePlacementClient{}
	r.Placement, r.Runtime = pc, agentRuntime
	a := assignment("pl-sub", `sleep 0.5`)
	if ok, reason := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatal(reason)
	}
	r.Run(context.Background(), a)

	calls := pc.snapshot()
	if len(calls) != 1 {
		t.Fatalf("placement reports = %d, want 1: %v", len(calls), calls)
	}
	want := &nexusv1.AttemptPlacement{Executor: "subprocess", Cluster: "aks-prod",
		K8SNamespace: "nexus-workers", Pod: "nexus-agent-0", K8SNode: "aks-node-1"}
	if c := calls[0]; c.GetAttemptId() != "pl-sub" || c.GetLeaseToken() != "tok-pl-sub" || !proto.Equal(c.GetPlacement(), want) {
		t.Errorf("report = %v, want %v", c, want)
	}
}

// TestNoPlacementWithoutTheCapability: a control plane that did not advertise
// placement_reporting gets no call at all.
func TestNoPlacementWithoutTheCapability(t *testing.T) {
	r, reg, _ := newTestRunner(t, &fakeReportClient{})
	r.Runtime = agentRuntime // Placement left nil
	a := assignment("pl-off", `true`)
	if ok, reason := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatal(reason)
	}
	r.Run(context.Background(), a)
	if p := r.startPlacementReporter(context.Background(), a, testLog()); p != nil {
		t.Fatal("a reporter was started without a placement client")
	}
}

// podExecutor stands in for the kubernetes executor: a PlacementSource that
// reports its pod, then the pod's node, while it prepares.
type podExecutor struct{ outputExecutor }

func (e *podExecutor) ReportsPlacement() bool { return true }
func (e *podExecutor) Prepare(ctx context.Context, s executor.TaskSpec) (executor.Prepared, error) {
	s.OnPlacement(executor.PodPlacement{Namespace: "nexus-tasks", Pod: "nexus-pl-k8s-x7k2p"})
	s.OnPlacement(executor.PodPlacement{Namespace: "nexus-tasks", Pod: "nexus-pl-k8s-x7k2p", Node: "aks-gpu-3"})
	return e.outputExecutor.Prepare(ctx, s)
}
func (e *podExecutor) Wait(ctx context.Context, h executor.Handle) (executor.Outcome, error) {
	time.Sleep(300 * time.Millisecond) // long enough for the reports to land
	return e.outputExecutor.Wait(ctx, h)
}

// TestKubernetesAttemptReportsItsOwnPod: a PlacementSource's pod and node are
// reported, the agent's own placement is not, and the newest report wins.
func TestKubernetesAttemptReportsItsOwnPod(t *testing.T) {
	r, reg, _ := newTestRunner(t, &fakeReportClient{})
	pc := &fakePlacementClient{}
	r.Placement, r.Runtime = pc, agentRuntime
	r.executors["kubernetes"] = &podExecutor{outputExecutor{kind: "kubernetes"}}
	a := assignment("pl-k8s", "unused")
	a.Spec.Runtime.Executor = "kubernetes"
	if ok, reason := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatal(reason)
	}
	r.Run(context.Background(), a)

	calls := pc.snapshot()
	if len(calls) == 0 || len(calls) > 2 {
		t.Fatalf("placement reports = %d, want the pod (perhaps coalesced) then the node: %v", len(calls), calls)
	}
	last := calls[len(calls)-1].GetPlacement()
	want := &nexusv1.AttemptPlacement{Executor: "kubernetes", Cluster: "aks-prod",
		K8SNamespace: "nexus-tasks", Pod: "nexus-pl-k8s-x7k2p", K8SNode: "aks-gpu-3"}
	if !proto.Equal(last, want) {
		t.Errorf("last report = %v, want %v", last, want)
	}
	for _, c := range calls {
		if c.GetPlacement().GetPod() == agentRuntime.Pod {
			t.Error("a kubernetes attempt reported the agent's own pod")
		}
	}
}

func newReporter(t *testing.T, pc *fakePlacementClient) (*Runner, *placementReporter, context.CancelFunc) {
	t.Helper()
	r, _, _ := newTestRunner(t, &fakeReportClient{})
	r.Placement = pc
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := r.startPlacementReporter(ctx, assignment("pl-rep", "unused"), testLog())
	p.backoff = time.Millisecond
	return r, p, cancel
}

// TestPlacementRetriesATransportFailure: a gateway blip costs a retry, not
// the placement.
func TestPlacementRetriesATransportFailure(t *testing.T) {
	pc := &fakePlacementClient{fail: 2}
	_, p, _ := newReporter(t, pc)
	p.set(&nexusv1.AttemptPlacement{Executor: "subprocess", Pod: "a"})
	calls := waitCalls(t, pc, func(c []*nexusv1.ReportPlacementRequest) bool { return len(c) == 3 })
	if calls[2].GetPlacement().GetPod() != "a" {
		t.Fatalf("calls = %v", calls)
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(pc.snapshot()); n != 3 {
		t.Errorf("calls = %d after delivery, want 3", n)
	}
}

// TestPlacementGivesUpAfterItsTries: an unreachable gateway is tried a
// bounded number of times; the next change is tried afresh.
func TestPlacementGivesUpAfterItsTries(t *testing.T) {
	pc := &fakePlacementClient{fail: 100}
	_, p, _ := newReporter(t, pc)
	p.set(&nexusv1.AttemptPlacement{Executor: "kubernetes", Pod: "a"})
	waitCalls(t, pc, func(c []*nexusv1.ReportPlacementRequest) bool { return len(c) == placementTries })
	time.Sleep(30 * time.Millisecond)
	if n := len(pc.snapshot()); n != placementTries {
		t.Fatalf("calls = %d, want %d", n, placementTries)
	}
	p.set(&nexusv1.AttemptPlacement{Executor: "kubernetes", Pod: "a", K8SNode: "n"})
	waitCalls(t, pc, func(c []*nexusv1.ReportPlacementRequest) bool { return len(c) == 2*placementTries })
}

// TestRefusedPlacementIsNotResent: an Ack refusal (stale lease, invalid) is
// final for that placement.
func TestRefusedPlacementIsNotResent(t *testing.T) {
	pc := &fakePlacementClient{answer: func(*nexusv1.ReportPlacementRequest) (*nexusv1.Ack, error) {
		return &nexusv1.Ack{Accepted: false, Reason: "stale_lease_token"}, nil
	}}
	_, p, _ := newReporter(t, pc)
	pl := &nexusv1.AttemptPlacement{Executor: "docker", Pod: "abc"}
	p.set(pl)
	waitCalls(t, pc, func(c []*nexusv1.ReportPlacementRequest) bool { return len(c) == 1 })
	p.set(proto.Clone(pl).(*nexusv1.AttemptPlacement))
	time.Sleep(30 * time.Millisecond)
	if n := len(pc.snapshot()); n != 1 {
		t.Fatalf("calls = %d, want the one refused report only", n)
	}
}

// TestUnimplementedStopsReportingForTheSession: a gateway that stopped
// offering the RPC is not asked again by this attempt or any later one.
func TestUnimplementedStopsReportingForTheSession(t *testing.T) {
	pc := &fakePlacementClient{answer: func(*nexusv1.ReportPlacementRequest) (*nexusv1.Ack, error) {
		return nil, errors.Join(errors.New("ReportPlacement"), status.Error(codes.Unimplemented, "unknown method"))
	}}
	r, p, _ := newReporter(t, pc)
	p.set(&nexusv1.AttemptPlacement{Executor: "subprocess"})
	waitCalls(t, pc, func(c []*nexusv1.ReportPlacementRequest) bool { return len(c) == 1 })
	deadline := time.Now().Add(5 * time.Second)
	for !r.placementOff.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Unimplemented did not turn reporting off")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if r.startPlacementReporter(context.Background(), assignment("later", "x"), testLog()) != nil {
		t.Fatal("a later attempt still reports placement")
	}
}

// TestNewerPlacementSupersedesOneBeingRetried: the control plane ends with
// the newest placement, not a stale one delivered late.
func TestNewerPlacementSupersedesOneBeingRetried(t *testing.T) {
	pc := &fakePlacementClient{fail: 1}
	_, p, _ := newReporter(t, pc)
	p.backoff = 50 * time.Millisecond
	p.set(&nexusv1.AttemptPlacement{Executor: "kubernetes", Pod: "p"})
	waitCalls(t, pc, func(c []*nexusv1.ReportPlacementRequest) bool { return len(c) == 1 })
	p.set(&nexusv1.AttemptPlacement{Executor: "kubernetes", Pod: "p", K8SNode: "node-9"})
	calls := waitCalls(t, pc, func(c []*nexusv1.ReportPlacementRequest) bool {
		return len(c) > 0 && c[len(c)-1].GetPlacement().GetK8SNode() == "node-9"
	})
	time.Sleep(80 * time.Millisecond)
	if last := pc.snapshot(); last[len(last)-1].GetPlacement().GetK8SNode() != "node-9" {
		t.Fatalf("an older placement was delivered after the newer one: %v", calls)
	}
}

func TestLocalPlacement(t *testing.T) {
	r := &Runner{}
	id := strings.Repeat("ab", 32)
	cases := []struct {
		name    string
		runtime identity.Runtime
		kind    string
		want    *nexusv1.AttemptPlacement
	}{
		{"subprocess outside Kubernetes", identity.Runtime{}, "subprocess",
			&nexusv1.AttemptPlacement{Executor: "subprocess"}},
		{"docker outside Kubernetes: the container", identity.Runtime{}, "docker",
			&nexusv1.AttemptPlacement{Executor: "docker", Pod: id[:12]}},
		{"docker in a pod: the agent's pod", agentRuntime, "docker",
			&nexusv1.AttemptPlacement{Executor: "docker", Cluster: "aks-prod", K8SNamespace: "nexus-workers",
				Pod: "nexus-agent-0", K8SNode: "aks-node-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r.Runtime = tc.runtime
			if got := r.localPlacement(tc.kind, executor.Handle{ID: id}); !proto.Equal(got, tc.want) {
				t.Errorf("localPlacement = %v, want %v", got, tc.want)
			}
		})
	}
	if shortContainerID("abc") != "abc" {
		t.Error("a short id must be kept whole")
	}
}

// TestRegisterEnablesPlacementOnlyWhenAdvertised: the session reports
// placement exactly when the gateway said it records it, with the agent's own
// runtime identity either way.
func TestRegisterEnablesPlacementOnlyWhenAdvertised(t *testing.T) {
	for _, offered := range []bool{false, true} {
		g := &scriptedGateway{placement: offered}
		a, _ := testAgent(t, dialFake(t, g), fastBackoff)
		a.cfg.Runtime = agentRuntime
		if err := a.register(context.Background()); err != nil {
			t.Fatalf("register: %v", err)
		}
		if got := a.runner.Placement != nil; got != offered {
			t.Errorf("advertised=%v: placement reporting on = %v", offered, got)
		}
		if a.runner.Runtime != agentRuntime {
			t.Errorf("runner runtime = %+v", a.runner.Runtime)
		}
	}
}
