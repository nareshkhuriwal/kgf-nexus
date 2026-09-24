// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

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
}

func (f *fakeClusterStore) ClusterAssignmentByName(context.Context, string) (*store.ClusterAssignment, error) {
	return &store.ClusterAssignment{ID: cid, Name: "c", Bindings: []store.ClusterBinding{
		{Namespace: "acme", PoolName: "cpu", MaxReplicas: 4, Pool: platform.Pool{Name: "cpu", MaxWorkers: 10}},
		{Namespace: "acme", PoolName: "broken", ConfigError: "pool \"broken\" is not defined"},
	}}, nil
}
func (f *fakeClusterStore) ReportObservations(context.Context, store.ObservationReport) (int64, error) {
	return 0, fmt.Errorf("%w: unknown observation kind", store.ErrInvalidClusterReport)
}
func (f *fakeClusterStore) PodWorkerStates(context.Context, string, []string) (map[string]store.PodWorkers, error) {
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
