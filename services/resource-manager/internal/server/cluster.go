// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ClusterStore is the persistence the cluster API needs. An interface so the
// API's validation and error mapping are testable without Postgres.
type ClusterStore interface {
	ClusterAssignmentByName(ctx context.Context, name string) (*store.ClusterAssignment, error)
	ReportObservations(ctx context.Context, r store.ObservationReport) (int64, error)
	PodWorkerStates(ctx context.Context, namespace string, hostnames []string) (map[string]store.PodWorkers, error)
	PoolDemand(ctx context.Context, b store.ClusterBinding) (platform.Demand, error)
	DrainPod(ctx context.Context, clusterID, namespace, k8sNamespace, deployment, hostname, observedUID, reason, actor string) (int, int, error)
	CheckPodRemovable(ctx context.Context, clusterID, namespace, hostname string) (store.PodWorkers, bool, string, error)
	RecordScaling(ctx context.Context, r store.ScalingRecord) error
	ReportReconcileError(ctx context.Context, clusterID, target, message, actor string) error
	ClusterNameByID(ctx context.Context, id string) (string, error)
}

// ClusterServer implements ClusterControllerService (docs/CLUSTER-SCALING.md).
//
// It is the resource manager's surface for the in-cluster controller. The
// controller has no database credential, so everything it learns and records
// passes through here -- which is what lets the ownership rules and the apply
// gate be enforced in one place, server-side.
type ClusterServer struct {
	rmv1.UnimplementedClusterControllerServiceServer
	store ClusterStore
	log   *slog.Logger
}

// NewClusterServer builds the service.
func NewClusterServer(s ClusterStore, log *slog.Logger) *ClusterServer {
	return &ClusterServer{store: s, log: log}
}

// maxObjectsPerReport bounds one report. A cluster with more Jobs than this in
// the bound namespaces is truncated by the controller, which says so.
const maxObjectsPerReport = 5000

// maxHostnamesPerQuery bounds GetPoolState; it is also far above any sane
// worker Deployment size.
const maxHostnamesPerQuery = 10000

func clusterErr(op string, err error) error {
	switch {
	case errors.Is(err, store.ErrClusterNotFound):
		return status.Errorf(codes.NotFound, "%s: %v", op, err)
	case errors.Is(err, store.ErrClusterNotApplying):
		return status.Errorf(codes.FailedPrecondition, "%s: %v", op, err)
	case errors.Is(err, store.ErrInvalidClusterReport):
		return status.Errorf(codes.InvalidArgument, "%s: %v", op, err)
	case errors.Is(err, store.ErrBindingNotFound):
		return status.Errorf(codes.PermissionDenied, "%s: %v", op, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	}
	return status.Errorf(codes.Internal, "%s: %v", op, err)
}

func required(fields map[string]string) error {
	for name, v := range fields {
		if strings.TrimSpace(v) == "" {
			return status.Errorf(codes.InvalidArgument, "%s is required", name)
		}
		// A malformed id is a caller bug and must read as one, not as the
		// Internal error Postgres would produce for an invalid uuid literal.
		if name == "cluster_id" {
			if _, err := uuid.Parse(v); err != nil {
				return status.Errorf(codes.InvalidArgument, "cluster_id %q is not a uuid", v)
			}
		}
	}
	return nil
}

func unixMs(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UnixMilli()
}

// GetAssignment returns the cluster's desired state.
func (s *ClusterServer) GetAssignment(ctx context.Context, req *rmv1.GetAssignmentRequest) (*rmv1.ClusterAssignment, error) {
	if err := required(map[string]string{"cluster_name": req.GetClusterName()}); err != nil {
		return nil, err
	}
	a, err := s.store.ClusterAssignmentByName(ctx, req.GetClusterName())
	if err != nil {
		return nil, clusterErr("get assignment", err)
	}
	out := &rmv1.ClusterAssignment{
		ClusterId: a.ID, ClusterName: a.Name, Namespace: a.Namespace, Provider: a.Provider,
		ProviderResourceId: a.ProviderResourceID, ExpectedClusterUid: a.ExpectedClusterUID,
		ControlPlaneNamespace: a.ControlPlaneNamespace, Mode: a.Mode, DesiredRevision: a.DesiredRevision,
	}
	for _, b := range a.Bindings {
		out.Bindings = append(out.Bindings, &rmv1.PoolBinding{
			Namespace: b.Namespace, KubernetesNamespace: b.KubernetesNamespace, WorkerDeployment: b.WorkerDeployment,
			Pool: b.PoolName, Queue: b.Pool.Queue, Enabled: b.Pool.Enabled,
			MinWorkers: int32(b.Pool.MinWorkers), MaxWorkers: int32(b.EffectiveMax()),
			TasksPerWorker: int32(b.Pool.TasksPerWorker), IdleSeconds: int32(b.Pool.IdleSeconds),
			MaxScaleUpStep: int32(b.Pool.MaxScaleUpStep), MaxScaleDownStep: int32(b.MaxScaleDownStep),
			ScaleUpCooldownSeconds:   int32(b.ScaleUpCooldown / time.Second),
			ScaleDownCooldownSeconds: int32(b.ScaleDownCooldown / time.Second),
			ProfileCpuMillis:         b.Profile.CPU, ProfileMemoryMib: b.Profile.Memory, ProfileGpuCount: int32(b.Profile.GPU),
			PlatformRevision: b.PlatformRevision, LastScaleAtUnixMs: unixMs(b.LastScaleAt),
			LastScaleDirection: b.LastScaleDirection, IdleSinceUnixMs: unixMs(b.IdleSince), ConfigError: b.ConfigError,
		})
	}
	return out, nil
}

// ReportObservations stores a snapshot.
func (s *ClusterServer) ReportObservations(ctx context.Context, req *rmv1.ReportObservationsRequest) (*rmv1.ReportObservationsResponse, error) {
	if err := required(map[string]string{"cluster_id": req.GetClusterId(), "controller_identity": req.GetControllerIdentity()}); err != nil {
		return nil, err
	}
	if len(req.GetObjects()) > maxObjectsPerReport {
		return nil, status.Errorf(codes.InvalidArgument, "report carries %d objects; the bound is %d", len(req.GetObjects()), maxObjectsPerReport)
	}
	r := store.ObservationReport{
		ClusterID: req.GetClusterId(), ControllerIdentity: req.GetControllerIdentity(),
		ObservedClusterUID: req.GetObservedClusterUid(), KubernetesVersion: req.GetKubernetesVersion(),
		ObservedRevision: req.GetObservedRevision(), EffectiveMode: req.GetEffectiveMode(),
		EffectiveModeReason: req.GetEffectiveModeReason(), CompleteKinds: req.GetCompleteKinds(),
		LastError: truncate(req.GetLastError(), 4096),
	}
	for _, o := range req.GetObjects() {
		obj := store.ObservedObject{
			Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName(), UID: o.GetUid(),
			Generation: o.GetGeneration(), ObservedGeneration: o.GetObservedGeneration(), Ready: o.GetReady(),
			ResourceVersion: o.GetResourceVersion(), SummaryJSON: []byte(o.GetSummaryJson()),
		}
		if o.GetSpecReplicas() >= 0 {
			v := o.GetSpecReplicas()
			obj.SpecReplicas = &v
		}
		if o.GetReadyReplicas() >= 0 {
			v := o.GetReadyReplicas()
			obj.ReadyReplicas = &v
		}
		if o.GetCreatedAtUnixMs() > 0 {
			t := time.UnixMilli(o.GetCreatedAtUnixMs())
			obj.CreatedAt = &t
		}
		r.Objects = append(r.Objects, obj)
	}
	desired, err := s.store.ReportObservations(ctx, r)
	if err != nil {
		return nil, clusterErr("report observations", err)
	}
	return &rmv1.ReportObservationsResponse{DesiredRevision: desired}, nil
}

// GetPoolState returns demand and per-pod worker/lease state for one binding.
func (s *ClusterServer) GetPoolState(ctx context.Context, req *rmv1.GetPoolStateRequest) (*rmv1.PoolState, error) {
	if err := required(map[string]string{"cluster_id": req.GetClusterId(), "namespace": req.GetNamespace(), "pool": req.GetPool()}); err != nil {
		return nil, err
	}
	if len(req.GetHostnames()) > maxHostnamesPerQuery {
		return nil, status.Errorf(codes.InvalidArgument, "at most %d hostnames per query", maxHostnamesPerQuery)
	}
	// The binding is resolved from the registry rather than taken from the
	// request, so the controller cannot ask about a pool it was not assigned.
	b, err := s.bindingFor(ctx, req.GetClusterId(), req.GetNamespace(), req.GetPool())
	if err != nil {
		return nil, err
	}
	if b.ConfigError != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "pool state: %s", b.ConfigError)
	}
	d, err := s.store.PoolDemand(ctx, *b)
	if err != nil {
		return nil, clusterErr("pool demand", err)
	}
	pods, err := s.store.PodWorkerStates(ctx, req.GetNamespace(), req.GetHostnames())
	if err != nil {
		return nil, clusterErr("pod worker states", err)
	}
	out := &rmv1.PoolState{Demand: &rmv1.Demand{
		PendingTasks: int32(d.Pending), RunningTasks: int32(d.Running), CpuMillis: d.CPU, MemoryMib: d.Memory,
		GpuCount: int32(d.GPU), UnschedulableTasks: int32(d.Unschedulable), CurrentWorkers: int32(d.CurrentWorkers),
	}}
	for _, h := range req.GetHostnames() {
		out.Pods = append(out.Pods, podWorkersPB(pods[h]))
	}
	return out, nil
}

// bindingFor finds a binding by cluster id. The registry is small (a handful of
// bindings per cluster), so loading the assignment is cheaper than a second
// query shape to keep in sync.
func (s *ClusterServer) bindingFor(ctx context.Context, clusterID, namespace, pool string) (*store.ClusterBinding, error) {
	name, err := s.clusterName(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	a, err := s.store.ClusterAssignmentByName(ctx, name)
	if err != nil {
		return nil, clusterErr("load assignment", err)
	}
	for i := range a.Bindings {
		if a.Bindings[i].Namespace == namespace && a.Bindings[i].PoolName == pool {
			return &a.Bindings[i], nil
		}
	}
	return nil, status.Errorf(codes.PermissionDenied, "pool %s/%s is not bound to cluster %s", namespace, pool, clusterID)
}

func (s *ClusterServer) clusterName(ctx context.Context, clusterID string) (string, error) {
	name, err := s.store.ClusterNameByID(ctx, clusterID)
	if err != nil {
		return "", clusterErr("resolve cluster", err)
	}
	return name, nil
}

func podWorkersPB(p store.PodWorkers) *rmv1.PodWorkers {
	return &rmv1.PodWorkers{
		Hostname: p.Hostname, Active: int32(p.Active), Draining: int32(p.Draining), Quarantined: int32(p.Quarantined),
		LeaseCount: int32(p.LeaseCount), ReservationCount: int32(p.ReservationCount), LastHeartbeatUnixMs: unixMs(p.LastHeartbeat),
	}
}

// DrainPod drains every worker registered from a pod.
func (s *ClusterServer) DrainPod(ctx context.Context, req *rmv1.DrainPodRequest) (*rmv1.DrainPodResponse, error) {
	if err := required(map[string]string{
		"cluster_id": req.GetClusterId(), "namespace": req.GetNamespace(), "hostname": req.GetHostname(),
		"actor": req.GetActor(), "kubernetes_namespace": req.GetKubernetesNamespace(), "worker_deployment": req.GetWorkerDeployment(),
	}); err != nil {
		return nil, err
	}
	drained, already, err := s.store.DrainPod(ctx, req.GetClusterId(), req.GetNamespace(), req.GetKubernetesNamespace(),
		req.GetWorkerDeployment(), req.GetHostname(), req.GetObservedClusterUid(), truncate(req.GetReason(), 1024), req.GetActor())
	if err != nil {
		return nil, clusterErr("drain pod", err)
	}
	s.log.InfoContext(ctx, "drained workers for scale-in", "cluster_id", req.GetClusterId(), "namespace", req.GetNamespace(),
		"pod", req.GetHostname(), "drained", drained, "already_draining", already, "actor", req.GetActor())
	return &rmv1.DrainPodResponse{Drained: int32(drained), AlreadyDraining: int32(already)}, nil
}

// CheckPodRemovable is the fresh pre-removal check.
func (s *ClusterServer) CheckPodRemovable(ctx context.Context, req *rmv1.CheckPodRemovableRequest) (*rmv1.CheckPodRemovableResponse, error) {
	if err := required(map[string]string{"cluster_id": req.GetClusterId(), "namespace": req.GetNamespace(), "hostname": req.GetHostname()}); err != nil {
		return nil, err
	}
	p, ok, why, err := s.store.CheckPodRemovable(ctx, req.GetClusterId(), req.GetNamespace(), req.GetHostname())
	if err != nil {
		return nil, clusterErr("check pod removable", err)
	}
	return &rmv1.CheckPodRemovableResponse{Removable: ok, Reason: why, Workers: podWorkersPB(p)}, nil
}

var scalingActions = map[string]bool{"": true, "plan": true, "scale_out": true, "drain": true, "scale_in": true, "hold": true, "refused": true}

// RecordScaling persists a decision and the controller's cooldown/idle memory.
func (s *ClusterServer) RecordScaling(ctx context.Context, req *rmv1.RecordScalingRequest) (*rmv1.RecordScalingResponse, error) {
	if err := required(map[string]string{"cluster_id": req.GetClusterId(), "kubernetes_namespace": req.GetKubernetesNamespace(), "worker_deployment": req.GetWorkerDeployment()}); err != nil {
		return nil, err
	}
	if !scalingActions[req.GetAction()] {
		return nil, status.Errorf(codes.InvalidArgument, "unknown scaling action %q", req.GetAction())
	}
	r := store.ScalingRecord{
		ClusterID: req.GetClusterId(), KubernetesNamespace: req.GetKubernetesNamespace(), WorkerDeployment: req.GetWorkerDeployment(),
		Action: req.GetAction(), Mode: req.GetMode(), Applied: req.GetApplied(), Reason: truncate(req.GetReason(), 1024),
		Actor: req.GetActor(), Detail: []byte(req.GetDetailJson()), ScaledDirection: req.GetScaledDirection(),
		ObservedPlatformRevision: req.GetObservedPlatformRevision(), Decision: []byte(req.GetDecisionJson()),
	}
	if req.GetAction() != "" {
		from, to := req.GetFromReplicas(), req.GetToReplicas()
		r.FromReplicas, r.ToReplicas = &from, &to
	}
	switch req.GetIdleUpdate() {
	case rmv1.RecordScalingRequest_IDLE_UPDATE_SET:
		r.IdleUpdate, r.IdleSince = "set", time.UnixMilli(req.GetIdleSinceUnixMs())
	case rmv1.RecordScalingRequest_IDLE_UPDATE_CLEAR:
		r.IdleUpdate = "clear"
	}
	if err := s.store.RecordScaling(ctx, r); err != nil {
		return nil, clusterErr("record scaling", err)
	}
	return &rmv1.RecordScalingResponse{}, nil
}

// ReportReconcileError records a controller failure for the console.
func (s *ClusterServer) ReportReconcileError(ctx context.Context, req *rmv1.ReportReconcileErrorRequest) (*rmv1.ReportReconcileErrorResponse, error) {
	if err := required(map[string]string{"cluster_id": req.GetClusterId(), "message": req.GetMessage()}); err != nil {
		return nil, err
	}
	if err := s.store.ReportReconcileError(ctx, req.GetClusterId(), truncate(req.GetTarget(), 512), req.GetMessage(), req.GetActor()); err != nil {
		return nil, clusterErr("report reconcile error", err)
	}
	return &rmv1.ReportReconcileErrorResponse{}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
