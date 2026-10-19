// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package workergrpc

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/instances/identity"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// ReasonInvalidPlacement is the Ack reason for a placement report the gateway
// refuses to store. The agent stops resending that report.
const ReasonInvalidPlacement = "invalid_placement"

// ReportPlacement records where an attempt's workload runs (ADR-0037): the
// executor, the cluster and, on Kubernetes, the pod, its namespace and node.
//
// Fenced like every attempt RPC, and then AGAIN inside the write: the row is
// stored only through the exact lease the token check found, while it is
// still live by the database clock, and never over a newer lease epoch's
// report. A zombie agent cannot relocate an attempt it no longer owns.
//
// The gateway writes attempt_runtime itself rather than forwarding to the
// scheduler: like the log index (ReportLogs), placement is metadata ABOUT an
// attempt that changes nothing in its lifecycle, so it needs neither the
// scheduler's transaction nor a lock on the attempt row.
//
// Advisory throughout. A database failure answers Unavailable, which the
// agent retries a few times and then gives up on; the attempt itself is never
// affected by whether its placement was recorded.
func (s *Server) ReportPlacement(ctx context.Context, req *nexusv1.ReportPlacementRequest) (*nexusv1.Ack, error) {
	if _, err := s.requirePrincipal(ctx, ""); err != nil {
		return nil, err
	}
	if !s.cfg.PlacementReporting {
		// The same answer a gateway that predates the RPC gives, so the agent
		// has one code path for "not recorded here".
		return nil, status.Error(codes.Unimplemented, "this control plane does not record attempt placement")
	}
	lease, ack := s.fenceLease(ctx, req.GetAttemptId(), req.GetLeaseToken(), "ReportPlacement")
	if ack != nil {
		return ack, nil
	}
	p := placementFromProto(req.GetPlacement())
	recorded, err := store.GatewayRecordAttemptPlacement(ctx, s.db, lease, p)
	switch {
	case errors.Is(err, store.ErrInvalidPlacement):
		s.log.WarnContext(ctx, "placement report refused", "attempt_id", req.GetAttemptId(), "error", err)
		return &nexusv1.Ack{Accepted: false, Reason: ReasonInvalidPlacement}, nil
	case err != nil:
		return nil, status.Errorf(codes.Unavailable, "record placement: %v", err)
	case !recorded:
		// The lease ended, or was superseded by a newer epoch, between the
		// fence above and the write: the same answer the fence would now give.
		s.log.WarnContext(ctx, "rejected attempt RPC on stale lease token",
			"rpc", "ReportPlacement", "attempt_id", req.GetAttemptId())
		return &nexusv1.Ack{Accepted: false, Reason: store.ReasonStaleLeaseToken}, nil
	}
	s.log.DebugContext(ctx, "attempt placement recorded", "attempt_id", req.GetAttemptId(),
		"executor", p.Executor, "cluster", p.Cluster, "k8s_namespace", p.K8sNamespace,
		"pod", p.Pod, "k8s_node", p.K8sNode)
	return &nexusv1.Ack{Accepted: true}, nil
}

// placementFromProto normalises a report: names are trimmed and stripped of
// control characters (they reach the console and the logs verbatim), and the
// executor's "k8s" alias -- which agents advertise beside "kubernetes"
// (executor.KubernetesKindAlias) -- is recorded under the one name the
// listing groups by.
func placementFromProto(p *nexusv1.AttemptPlacement) store.AttemptPlacement {
	executor := strings.ToLower(identity.Clean(p.GetExecutor()))
	if executor == "k8s" {
		executor = "kubernetes"
	}
	return store.AttemptPlacement{
		Executor:     executor,
		Cluster:      identity.Clean(p.GetCluster()),
		K8sNamespace: identity.Clean(p.GetK8SNamespace()),
		Pod:          identity.Clean(p.GetPod()),
		K8sNode:      identity.Clean(p.GetK8SNode()),
	}
}
