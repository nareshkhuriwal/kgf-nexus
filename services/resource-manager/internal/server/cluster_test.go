// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const cid = "11111111-1111-1111-1111-111111111111"

type fakeClusterStore struct {
	drainErr  error
	demandHit bool

	hpaErr     error
	hpaReports []store.SchedulerAutoscalingReport
}

var lastApplied = time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC)

func (f *fakeClusterStore) ClusterAssignmentByName(context.Context, string) (*store.ClusterAssignment, error) {
	return &store.ClusterAssignment{ID: cid, Name: "c", Bindings: []store.ClusterBinding{
		{Namespace: "acme", PoolName: "cpu", MaxReplicas: 4, Pool: platform.Pool{Name: "cpu", MaxWorkers: 10}},
		{Namespace: "acme", PoolName: "broken", ConfigError: "pool \"broken\" is not defined"},
	}, SchedulerAutoscaling: store.SchedulerAutoscalingAssignment{
		Desired:        platform.HPASettings{MinReplicas: 2, MaxReplicas: 8, TargetCPUPercent: 70, ScaleDownStabilizationSeconds: 600},
		ConfigRevision: 5,
		HPAs: []store.SchedulerHPAMemory{
			{KubernetesNamespace: "kgf-nexus", HPAName: "scheduler", LastAppliedAt: &lastApplied},
			{KubernetesNamespace: "kgf-nexus", HPAName: "scheduler-gpu"},
		},
	}}, nil
}
func (f *fakeClusterStore) RecordSchedulerAutoscaling(_ context.Context, r store.SchedulerAutoscalingReport) error {
	if f.hpaErr != nil {
		return f.hpaErr
	}
	f.hpaReports = append(f.hpaReports, r)
	return nil
}
func (f *fakeClusterStore) ReportObservations(context.Context, store.ObservationReport) (int64, error) {
	return 0, fmt.Errorf("%w: unknown observation kind", store.ErrInvalidClusterReport)
}
func (f *fakeClusterStore) PodWorkerStates(context.Context, string, string, []string) (map[string]store.PodWorkers, error) {
	return map[string]store.PodWorkers{}, nil
}
func (f *fakeClusterStore) PoolDemand(context.Context, store.ClusterBinding) (platform.Demand, error) {
	f.demandHit = true
	return platform.Demand{Pending: 3}, nil
}
func (f *fakeClusterStore) DrainPod(context.Context, string, string, string, string, string, string, string, string) (int, int, error) {
	return 0, 0, f.drainErr
}
func (f *fakeClusterStore) CheckPodRemovable(context.Context, string, string, string) (store.PodWorkers, bool, string, error) {
	return store.PodWorkers{}, false, "", store.ErrClusterNotFound
}
func (f *fakeClusterStore) RecordScaling(context.Context, store.ScalingRecord) error { return nil }
func (f *fakeClusterStore) ReportReconcileError(context.Context, string, string, string, string) error {
	return nil
}
func (f *fakeClusterStore) ClusterNameByID(context.Context, string) (string, error) { return "c", nil }

func code(err error) codes.Code { return status.Code(err) }

func TestClusterAPIMapsRefusalsToLegibleCodes(t *testing.T) {
	f := &fakeClusterStore{drainErr: fmt.Errorf("%w: registry mode is plan", store.ErrClusterNotApplying)}
	s := NewClusterServer(f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	if _, err := s.DrainPod(ctx, &rmv1.DrainPodRequest{ClusterId: "not-a-uuid", Namespace: "acme", Hostname: "p", Actor: "a",
		KubernetesNamespace: "w", WorkerDeployment: "d"}); code(err) != codes.InvalidArgument {
		t.Errorf("malformed cluster id: %v", err)
	}
	if _, err := s.DrainPod(ctx, &rmv1.DrainPodRequest{ClusterId: cid, Namespace: "acme", Hostname: "p", Actor: "a",
		KubernetesNamespace: "w", WorkerDeployment: "d"}); code(err) != codes.FailedPrecondition {
		t.Errorf("drain in plan mode: %v, want FailedPrecondition", err)
	}
	if _, err := s.DrainPod(ctx, &rmv1.DrainPodRequest{ClusterId: cid, Namespace: "acme", Hostname: "p"}); code(err) != codes.InvalidArgument {
		t.Errorf("drain without actor/deployment: %v", err)
	}
	if _, err := s.GetPoolState(ctx, &rmv1.GetPoolStateRequest{ClusterId: cid, Namespace: "other", Pool: "cpu"}); code(err) != codes.PermissionDenied {
		t.Errorf("pool of another namespace: %v, want PermissionDenied", err)
	}
	if _, err := s.GetPoolState(ctx, &rmv1.GetPoolStateRequest{ClusterId: cid, Namespace: "acme", Pool: "broken"}); code(err) != codes.FailedPrecondition || f.demandHit {
		t.Errorf("misconfigured binding: %v (demand observed: %v)", err, f.demandHit)
	}
	ps, err := s.GetPoolState(ctx, &rmv1.GetPoolStateRequest{ClusterId: cid, Namespace: "acme", Pool: "cpu", Hostnames: []string{"p1"}})
	if err != nil || ps.GetDemand().GetPendingTasks() != 3 || len(ps.GetPods()) != 1 {
		t.Errorf("pool state %+v %v", ps, err)
	}
	if _, err := s.ReportObservations(ctx, &rmv1.ReportObservationsRequest{ClusterId: cid, ControllerIdentity: "c"}); code(err) != codes.InvalidArgument {
		t.Errorf("invalid report: %v", err)
	}
	if _, err := s.CheckPodRemovable(ctx, &rmv1.CheckPodRemovableRequest{ClusterId: cid, Namespace: "acme", Hostname: "p"}); code(err) != codes.NotFound {
		t.Errorf("unknown cluster: %v", err)
	}
	if _, err := s.RecordScaling(ctx, &rmv1.RecordScalingRequest{ClusterId: cid, KubernetesNamespace: "w", WorkerDeployment: "d", Action: "delete_everything"}); code(err) != codes.InvalidArgument {
		t.Errorf("unknown action accepted: %v", err)
	}
	a, err := s.GetAssignment(ctx, &rmv1.GetAssignmentRequest{ClusterName: "c"})
	if err != nil || a.GetBindings()[0].GetMaxWorkers() != 4 {
		t.Errorf("assignment must carry the smaller of pool max and registry ceiling: %+v %v", a, err)
	}
}

func TestAssignmentCarriesSchedulerAutoscaling(t *testing.T) {
	s := NewClusterServer(&fakeClusterStore{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a, err := s.GetAssignment(context.Background(), &rmv1.GetAssignmentRequest{ClusterName: "c"})
	if err != nil {
		t.Fatal(err)
	}
	sa := a.GetSchedulerAutoscaling()
	d := sa.GetDesired()
	if sa.GetConfigRevision() != 5 || d.GetMinReplicas() != 2 || d.GetMaxReplicas() != 8 || d.GetTargetCpuPercent() != 70 ||
		d.GetScaleDownStabilizationSeconds() != 600 {
		t.Fatalf("scheduler autoscaling %+v", sa)
	}
	if len(sa.GetHpas()) != 2 || sa.GetHpas()[0].GetLastAppliedAtUnixMs() != lastApplied.UnixMilli() ||
		sa.GetHpas()[1].GetLastAppliedAtUnixMs() != 0 || sa.GetHpas()[1].GetHpaName() != "scheduler-gpu" {
		t.Fatalf("persisted HPA memory %+v", sa.GetHpas())
	}
}

func TestRecordSchedulerAutoscalingValidatesAndMaps(t *testing.T) {
	f := &fakeClusterStore{}
	s := NewClusterServer(f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	target := func() *rmv1.SchedulerHPAReconcile {
		return &rmv1.SchedulerHPAReconcile{KubernetesNamespace: "kgf-nexus", HpaName: "scheduler", Deployment: "scheduler",
			HpaUid: "u-1", Managed: true, Result: "applying", Reason: "min 2→4", ConfigRevision: 5, Action: "hpa_update", Applied: true,
			Observed: &rmv1.SchedulerAutoscalingSettings{MinReplicas: 4, MaxReplicas: 8, TargetCpuPercent: 70, ScaleDownStabilizationSeconds: 600},
			From:     &rmv1.SchedulerAutoscalingSettings{MinReplicas: 2, MaxReplicas: 8, TargetCpuPercent: 70, ScaleDownStabilizationSeconds: 600},
			To:       &rmv1.SchedulerAutoscalingSettings{MinReplicas: 4, MaxReplicas: 8, TargetCpuPercent: 70, ScaleDownStabilizationSeconds: 600}}
	}

	if _, err := s.RecordSchedulerAutoscaling(ctx, &rmv1.RecordSchedulerAutoscalingRequest{ClusterId: cid}); code(err) != codes.InvalidArgument {
		t.Errorf("no actor: %v", err)
	}
	noName := target()
	noName.HpaName = ""
	if _, err := s.RecordSchedulerAutoscaling(ctx, &rmv1.RecordSchedulerAutoscalingRequest{ClusterId: cid, Actor: "c-0",
		Targets: []*rmv1.SchedulerHPAReconcile{noName}}); code(err) != codes.InvalidArgument {
		t.Errorf("target without a name: %v", err)
	}
	f.hpaErr = fmt.Errorf("%w: HPA other/scheduler is outside the control-plane namespace", store.ErrBindingNotFound)
	if _, err := s.RecordSchedulerAutoscaling(ctx, &rmv1.RecordSchedulerAutoscalingRequest{ClusterId: cid, Actor: "c-0",
		Targets: []*rmv1.SchedulerHPAReconcile{target()}}); code(err) != codes.PermissionDenied {
		t.Errorf("HPA outside the control plane: %v, want PermissionDenied", err)
	}
	f.hpaErr = fmt.Errorf("%w: unknown scheduler HPA result", store.ErrInvalidClusterReport)
	if _, err := s.RecordSchedulerAutoscaling(ctx, &rmv1.RecordSchedulerAutoscalingRequest{ClusterId: cid, Actor: "c-0",
		Targets: []*rmv1.SchedulerHPAReconcile{target()}}); code(err) != codes.InvalidArgument {
		t.Errorf("invalid report: %v, want InvalidArgument", err)
	}
	f.hpaErr = nil

	stateOnly := target()
	stateOnly.HpaName, stateOnly.Action, stateOnly.Applied, stateOnly.From, stateOnly.To, stateOnly.Observed = "scheduler-gpu", "", false, nil, nil, nil
	if _, err := s.RecordSchedulerAutoscaling(ctx, &rmv1.RecordSchedulerAutoscalingRequest{ClusterId: cid, Actor: "c-0", Mode: "apply",
		Complete: true, Targets: []*rmv1.SchedulerHPAReconcile{target(), stateOnly}}); err != nil {
		t.Fatal(err)
	}
	r := f.hpaReports[0]
	if r.ClusterID != cid || r.Actor != "c-0" || r.Mode != "apply" || !r.Complete || len(r.Targets) != 2 {
		t.Fatalf("report %+v", r)
	}
	got := r.Targets[0]
	if got.Status.Result != "applying" || got.Status.UID != "u-1" || !got.Status.Managed || got.Status.ConfigRevision != 5 ||
		got.Status.Observed.MinReplicas != 4 || got.From == nil || got.From.MinReplicas != 2 || got.To.MinReplicas != 4 ||
		!got.Applied || got.Action != "hpa_update" || got.Reason != "min 2→4" {
		t.Fatalf("target %+v", got)
	}
	if st := r.Targets[1]; st.From != nil || st.To != nil || st.Action != "" || st.Status.Observed != (platform.HPASettings{}) {
		t.Fatalf("state-only target %+v", st)
	}
}
