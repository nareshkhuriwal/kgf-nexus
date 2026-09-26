// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/transport"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// schedGRPC is the live scheduler client, used when an address is configured.
//
// Split from the fallback shells in clients.go for the same reason
// resourcemanager.go is: everything in this file makes a real RPC, so the wired
// path and the not-yet-wired path cannot be mistaken for one another.
type schedGRPC struct {
	conn   *grpc.ClientConn
	client nexusv1.SchedulerServiceClient
	log    *slog.Logger
	// addr is kept for logging only: with several replicas each draining one
	// queue, a claim log that does not name the replica cannot say which queue
	// answered (F61).
	addr string
}

func dialScheduler(ctx context.Context, addr string, log *slog.Logger) (*schedGRPC, error) {
	// Insecure for Phase 1, matching resourcemanager.go. ADR-0011 names mTLS
	// between services as a cost of the decomposition and puts it in the
	// deployment layer (sidecar or mesh) rather than hard-coded here.
	creds, terr := transport.DialOption()
	if terr != nil {
		return nil, fmt.Errorf("resolve TLS dial options: %w", terr)
	}
	conn, err := grpc.NewClient(config.PeerTarget(addr), append(config.PeerDialOptions(), creds)...)
	if err != nil {
		return nil, fmt.Errorf("dial scheduler at %s: %w", addr, err)
	}
	return &schedGRPC{conn: conn, client: nexusv1.NewSchedulerServiceClient(conn), log: log, addr: addr}, nil
}

func (s *schedGRPC) close() error { return s.conn.Close() }

// claimForWorker runs the atomic dispatch claim inside the scheduler.
//
// The workerID passed here is the one the gateway's interceptor AUTHENTICATED,
// never the one the worker put in its request body. That substitution is the
// reason SchedulerService is internal: a worker able to call it directly could
// name someone else's worker_id and claim capacity it does not hold.
func (s *schedGRPC) claimForWorker(
	ctx context.Context, workerID string, free *nexusv1.ResourceVector,
	queues []string, maxTasks int32,
) ([]*nexusv1.TaskAssignment, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()

	resp, err := s.client.ClaimForWorker(ctx, &nexusv1.ClaimForWorkerRequest{
		WorkerId: workerID,
		Queues:   queues,
		// The worker's self-reported free capacity. The scheduler treats it as an
		// upper bound on placement and never as authority — the ledger under the
		// worker's row lock decides — so an over-reporting worker cannot cause an
		// over-commit.
		FreeCapacity: free,
		MaxTasks:     maxTasks,
	})
	if err != nil {
		return nil, fmt.Errorf("claim for worker %s: %w", workerID, err)
	}
	// An empty assignment list is the normal steady state, not a condition to
	// report: a healthy fleet polls continuously and most polls find nothing.
	return resp.GetAssignments(), nil
}

// pendingCancellations lists attempts this worker must stop.
func (s *schedGRPC) pendingCancellations(ctx context.Context, workerID string) ([]string, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()

	resp, err := s.client.PendingCancellations(ctx, &nexusv1.PendingCancellationsRequest{
		WorkerId: workerID,
	})
	if err != nil {
		return nil, fmt.Errorf("pending cancellations for worker %s: %w", workerID, err)
	}
	return resp.GetAttemptIds(), nil
}

// ── The attempt-report half ───────────────────────────────────────────────
//
// Served by the scheduler as a hand-written JSON-codec service, because
// nexus.v1.SchedulerService defines no RPC for ReportStarted / ReportProgress /
// ReportResult. See the gap note in services/scheduler/reports_grpc.go.

type schedJSONCodec struct{}

func (schedJSONCodec) Marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal scheduler message: %w", err)
	}
	return b, nil
}

func (schedJSONCodec) Unmarshal(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("unmarshal scheduler message: %w", err)
	}
	return nil
}

func (schedJSONCodec) Name() string { return "json" }

type schedMarkStartedRequest struct {
	AttemptID string `json:"attempt_id"`
}

// schedRecordProgressRequest mirrors services/scheduler's RecordProgressRequest.
// lease_token is additive: an older scheduler ignores it and keeps dropping the
// message, which is exactly its behaviour today.
type schedRecordProgressRequest struct {
	AttemptID  string  `json:"attempt_id"`
	LeaseToken string  `json:"lease_token,omitempty"`
	Message    string  `json:"message,omitempty"`
	Fraction   float64 `json:"fraction,omitempty"`
}

// schedRecordProgressResponse is additive over the Empty an older scheduler
// returns, which decodes here as "not stale".
type schedRecordProgressResponse struct {
	StaleLease bool `json:"stale_lease,omitempty"`
	Stored     bool `json:"stored,omitempty"`
}

type schedApplyResultRequest struct {
	ResultJSON json.RawMessage `json:"result_json"`
}

type schedApplyResultResponse struct {
	RunID   string `json:"run_id"`
	NodeID  string `json:"node_id"`
	State   string `json:"state"`
	Applied bool   `json:"applied"`
	Retry   bool   `json:"retry"`
	// StaleLease: the attempt had been reclaimed, so the result was not used
	// (F104). Absent from an older scheduler, which decodes as false.
	StaleLease bool `json:"stale_lease,omitempty"`
}

type schedEmpty struct{}

func (s *schedGRPC) markStarted(ctx context.Context, attemptID string) error {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	var resp schedEmpty
	if err := s.conn.Invoke(ctx, "/nexus.v1.AttemptReportService/MarkStarted",
		&schedMarkStartedRequest{AttemptID: attemptID}, &resp,
		grpc.ForceCodec(schedJSONCodec{})); err != nil {
		return fmt.Errorf("mark attempt %s started: %w", attemptID, err)
	}
	return nil
}

func (s *schedGRPC) recordProgress(ctx context.Context, attemptID, leaseToken, message string, fraction float64) (bool, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	var resp schedRecordProgressResponse
	if err := s.conn.Invoke(ctx, "/nexus.v1.AttemptReportService/RecordProgress",
		&schedRecordProgressRequest{AttemptID: attemptID, LeaseToken: leaseToken, Message: message, Fraction: fraction},
		&resp, grpc.ForceCodec(schedJSONCodec{})); err != nil {
		return false, fmt.Errorf("record progress for attempt %s: %w", attemptID, err)
	}
	return resp.StaleLease, nil
}

// applyResult forwards the worker's terminal report verbatim.
//
// The request is re-encoded as protojson rather than destructured, so the frozen
// message stays the single definition of what a result is: outcome, output refs,
// the error taxonomy and metrics all travel without this layer needing to know
// what any of them mean.
func (s *schedGRPC) applyResult(ctx context.Context, req *nexusv1.ReportResultRequest) (schedApplyResultResponse, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()

	raw, err := protojson.Marshal(req)
	if err != nil {
		return schedApplyResultResponse{}, fmt.Errorf("encode result for attempt %s: %w", req.GetAttemptId(), err)
	}
	var resp schedApplyResultResponse
	if err := s.conn.Invoke(ctx, "/nexus.v1.AttemptReportService/ApplyResult",
		&schedApplyResultRequest{ResultJSON: raw}, &resp,
		grpc.ForceCodec(schedJSONCodec{})); err != nil {
		return schedApplyResultResponse{}, fmt.Errorf("apply result for attempt %s: %w", req.GetAttemptId(), err)
	}
	return resp, nil
}
