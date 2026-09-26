// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/workflow-manager/runs"
)

// ── The typed, generated half: nexus.v1.WorkflowManagerService ────────────

// RunServer implements the generated WorkflowManagerService.
//
// It is a transport adapter only. Every lifecycle rule — which transitions are
// legal, when a cancel fans out to attempts, when a pause may finalize — lives in
// runs.Service and is reached through the RunService interface, so the gRPC layer
// cannot develop a second opinion about the state machine in §5.2.
type RunServer struct {
	nexusv1.UnimplementedWorkflowManagerServiceServer

	runs RunService
	log  *slog.Logger
}

// CreateRun creates a run and materializes its DAG.
func (s *RunServer) CreateRun(ctx context.Context, req *nexusv1.CreateRunRequest) (*nexusv1.CreateRunResponse, error) {
	params := map[string]any{}
	for k, v := range req.GetParams() {
		params[k] = v
	}

	res, err := s.runs.Create(ctx, runs.CreateRequest{
		Namespace: req.GetNamespace(),
		// The proto carries only spec_id. A caller that has a NAME resolves it
		// before calling: pinning happens exactly once, at creation, and letting
		// the name be re-resolved deeper in the stack would reintroduce the race
		// ADR-0004 closes.
		SpecID:         req.GetSpecId(),
		IdempotencyKey: req.GetIdempotencyKey(),
		TriggerKind:    req.GetTriggerKind(),
		Params:         params,
		Priority:       req.GetPriority(),
	})
	if err != nil {
		return nil, runError("create run", err)
	}
	return &nexusv1.CreateRunResponse{
		RunId: res.Run.ID,
		// Deduplicated, not "failed": an idempotent replay returning the original
		// run is the whole point of the key.
		Deduplicated: !res.Created,
	}, nil
}

// CancelRun stops a run and its in-flight attempts.
func (s *RunServer) CancelRun(ctx context.Context, req *nexusv1.CancelRunRequest) (*nexusv1.RunActionResponse, error) {
	return s.action(ctx, "cancel run", func() (store.RunRow, error) { return s.runs.Cancel(ctx, req.GetRunId()) })
}

// PauseRun holds a run without killing in-flight work (§5.2).
func (s *RunServer) PauseRun(ctx context.Context, req *nexusv1.PauseRunRequest) (*nexusv1.RunActionResponse, error) {
	return s.action(ctx, "pause run", func() (store.RunRow, error) { return s.runs.Pause(ctx, req.GetRunId()) })
}

// ResumeRun returns a PAUSED run to RUNNING.
func (s *RunServer) ResumeRun(ctx context.Context, req *nexusv1.ResumeRunRequest) (*nexusv1.RunActionResponse, error) {
	return s.action(ctx, "resume run", func() (store.RunRow, error) { return s.runs.Resume(ctx, req.GetRunId()) })
}

// RetryRun restarts a failed run: from the named nodes when node_ids is set,
// otherwise from every node that did not succeed.
//
// only_failed is not consulted. Retry never re-runs succeeded work in either
// form, so the flag has no second meaning to select; it stays in the message
// only because removing a field is not an additive change.
func (s *RunServer) RetryRun(ctx context.Context, req *nexusv1.RetryRunRequest) (*nexusv1.RunActionResponse, error) {
	return s.action(ctx, "retry run", func() (store.RunRow, error) {
		res, err := s.runs.Retry(ctx, runs.RetryRequest{
			RunID: req.GetRunId(), NodeIDs: req.GetNodeIds(), Actor: req.GetActor(),
		})
		return res.Run, err
	})
}

// action is the common shape of the four lifecycle RPCs.
//
// A REFUSED transition comes back as accepted=false with a reason rather than as a
// gRPC error, because "you cannot pause an already-succeeded run" is a normal
// answer to a reasonable question, not a fault. Errors are reserved for faults.
func (s *RunServer) action(ctx context.Context, op string, fn func() (store.RunRow, error)) (*nexusv1.RunActionResponse, error) {
	row, err := fn()
	if err != nil {
		return nil, runError(op, err)
	}
	return &nexusv1.RunActionResponse{
		State:    runStateProto(row.State),
		Accepted: true,
	}, nil
}

// runStateProto maps the schema's run_state text onto the wire enum.
func runStateProto(s string) nexusv1.RunState {
	if v, ok := nexusv1.RunState_value["RUN_STATE_"+s]; ok {
		return nexusv1.RunState(v)
	}
	return nexusv1.RunState_RUN_STATE_UNSPECIFIED
}

// runError maps store sentinels onto gRPC codes so the gateway can turn them into
// the right HTTP status instead of a blanket 500.
func runError(op string, err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound), store.IsMalformedID(err):
		return status.Errorf(codes.NotFound, "%s: %v", op, err)
	case errors.Is(err, store.ErrRunQuotaExceeded):
		// Before ErrMaxActiveRuns, which shares the code: the gateway tells the
		// two apart by the ErrorInfo detail, and needs the dimension, the
		// numbers and the retry delay to answer with a useful 429 (ADR-0021).
		var q *store.RunQuotaError
		if errors.As(err, &q) {
			return store.RunQuotaStatus(op, q)
		}
		return status.Errorf(codes.ResourceExhausted, "%s: %v", op, err)
	case errors.Is(err, store.ErrMaxActiveRuns):
		return status.Errorf(codes.ResourceExhausted, "%s: %v", op, err)
	case errors.Is(err, runs.ErrMissingParams), errors.Is(err, runs.ErrRetryInvalidNodes):
		// InvalidArgument, not Internal: the caller's document is wrong, and no
		// retry of the same request will ever succeed.
		return status.Errorf(codes.InvalidArgument, "%s: %v", op, err)
	case errors.Is(err, store.ErrRunStateConflict), errors.Is(err, runs.ErrRetryRefused):
		// The state machine (or the retry planner) refusing a transition that is
		// illegal for the run as it is now. FailedPrecondition so the gateway can
		// answer 409 with the reason; as Internal it arrived as a 500 that told
		// an operator the platform had broken when it had said "no".
		return status.Errorf(codes.FailedPrecondition, "%s: %v", op, err)
	case errors.Is(err, runs.ErrRetryConflict):
		// A concurrent writer won; asking again after re-reading may succeed.
		return status.Errorf(codes.Aborted, "%s: %v", op, err)
	case errors.Is(err, runs.ErrRetryUnavailable):
		return status.Errorf(codes.Unimplemented, "%s: %v", op, err)
	case errors.Is(err, runs.ErrRetryIndeterminate):
		// The run is committed RUNNING and converging; the caller should re-read
		// it rather than assume either outcome.
		return status.Errorf(codes.Unavailable, "%s: %v", op, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", op, err)
	}
}

// ── The untyped half: spec submission ─────────────────────────────────────

// CONTRACT GAP (reported, not worked around): nexus.v1.WorkflowManagerService has
// CreateRun/Cancel/Pause/Resume/Retry but NO PutSpec, while ADR-0011 assigns
// workflow_specs to this service. With no RPC for it, the only ways to submit a
// spec are (a) the gateway writing a table it does not own, which is exactly what
// the ownership matrix exists to prevent, or (b) an RPC that the frozen proto does
// not define. CONTRACT.md forbids editing the proto, so this uses the same
// hand-written-descriptor-plus-JSON-codec shape the dag-engine already uses, and
// is a drop-in for a generated stub once PutSpec is added to services.proto.

// PutSpecRequest submits a canonicalized spec version.
type PutSpecRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// SpecHash is the content address computed by the SUBMITTER, not here. The
	// gateway canonicalizes and hashes as part of validating, and recomputing it
	// would give two components a vote on what a spec's identity is.
	SpecHash string `json:"spec_hash"`
	// CanonicalJSON is the canonical form whose sha256 is SpecHash.
	CanonicalJSON json.RawMessage `json:"canonical_json"`
	CreatedBy     string          `json:"created_by"`
}

// PutSpecResponse reports the stored version.
type PutSpecResponse struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Version   int32  `json:"version"`
	SpecHash  string `json:"spec_hash"`
	// Created is false when a byte-identical spec already existed, which ADR-0004
	// makes a no-op returning the existing version rather than a new one.
	Created bool `json:"created"`
}

// GetSpecRequest resolves a spec by id, or by name to its latest version.
type GetSpecRequest struct {
	Namespace string `json:"namespace"`
	SpecID    string `json:"spec_id,omitempty"`
	SpecName  string `json:"spec_name,omitempty"`
}

// GetSpecResponse carries the resolved version.
type GetSpecResponse struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Version  int32  `json:"version"`
	SpecHash string `json:"spec_hash"`
}

// SetRunStateRequest is the DAG engine reporting a run-level transition it
// DECIDED but does not own.
//
// The engine computes the frontier and therefore knows first when a run has
// started or has gone terminal, but workflow_runs belongs to this service
// (ADR-0011), so the write comes back through here. Only two transitions are
// expressible, and that narrowness is the point: this is not a general
// "set the run to whatever you like" endpoint, which would let a peer service
// drive workflow_runs straight past its own state machine (§5.2).
type SetRunStateRequest struct {
	RunID string `json:"run_id"`
	// State is the engine's conclusion: RUNNING when the first attempts exist, or
	// one of the terminal states when every node is terminal. Anything else is
	// refused.
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// SetRunStateResponse reports the state actually recorded.
type SetRunStateResponse struct {
	State string `json:"state"`
	// Applied is false when the state machine legally refused the transition,
	// which is a normal answer and not an error.
	Applied bool `json:"applied"`
}

type specService interface {
	PutSpec(context.Context, *PutSpecRequest) (*PutSpecResponse, error)
	GetSpec(context.Context, *GetSpecRequest) (*GetSpecResponse, error)
	SetRunState(context.Context, *SetRunStateRequest) (*SetRunStateResponse, error)
	// DeleteSpec is a MUTATION of workflow_specs, so it comes here for the same
	// reason the schedule writes do: migration 0005's grants refuse a DELETE on
	// this table from any role but this service's. Routing it locally in the
	// gateway would fail at the database rather than at review.
	DeleteSpec(context.Context, *DeleteSpecRequest) (*DeleteSpecResponse, error)
	// Schedule mutations. Reads are served by the gateway directly; only writes
	// come here, because `schedules` is this service's table and migration 0005's
	// grants refuse a write from anyone else at the database.
	CreateSchedule(context.Context, *CreateScheduleRequest) (*ScheduleResponse, error)
	UpdateSchedule(context.Context, *CreateScheduleRequest) (*ScheduleResponse, error)
	SetScheduleEnabled(context.Context, *SetScheduleEnabledRequest) (*ScheduleResponse, error)
	DeleteSchedule(context.Context, *DeleteScheduleRequest) (*DeleteScheduleResponse, error)
	StartBackfill(context.Context, *StartBackfillRequest) (*BackfillResponse, error)
	BackfillAction(context.Context, *BackfillActionRequest) (*BackfillResponse, error)
}

// SpecServer implements the spec half plus the run-state callback.
type SpecServer struct {
	db   store.GWDB
	runs RunService
	log  *slog.Logger
	// backfills is nil until a deployment wires the schedule service. Nil answers
	// Unimplemented rather than panicking, so a workflow-manager built without it
	// still serves every other RPC on this service.
	backfills BackfillService
}

// WithBackfills attaches the backfill service, enabling those RPCs.
func (s *SpecServer) WithBackfills(b BackfillService) *SpecServer {
	s.backfills = b
	return s
}

// SetRunState applies a run-level transition the DAG engine detected.
//
// It routes to the lifecycle service's own methods rather than writing
// workflow_runs directly, so the §5.2 state machine still adjudicates every
// transition. A refused transition comes back as applied=false, because the
// engine is at-least-once: it will report the same conclusion again, and the
// second report legitimately has nothing to do.
func (s *SpecServer) SetRunState(ctx context.Context, req *SetRunStateRequest) (*SetRunStateResponse, error) {
	if req.RunID == "" {
		return nil, status.Error(codes.InvalidArgument, "run_id is required")
	}

	var (
		row store.RunRow
		err error
	)
	switch domain.RunState(req.State) {
	case domain.RunRunning:
		row, err = s.runs.Start(ctx, req.RunID)
	case domain.RunPaused:
		// The engine reports that a PAUSING run has finished draining; the run
		// service decides whether that is legal and fires TriggerDrained. Routed
		// through MaybeFinalizePause rather than a direct transition so the
		// in-flight recount happens on this side of the boundary, where
		// workflow_runs is owned.
		row, err = s.runs.MaybeFinalizePause(ctx, req.RunID)
	case domain.RunSucceeded, domain.RunFailed, domain.RunCancelled, domain.RunTimedOut:
		// The engine does not get to NAME the terminal state. It reports that
		// every node is terminal, and OnNodesTerminal re-derives which terminal
		// state that implies from the node counts — so §5.2's rules, including the
		// CANCELLED-without-failure case (CONTRACT-FINDINGS F3), live in exactly
		// one place.
		row, err = s.runs.OnNodesTerminal(ctx, req.RunID)
	default:
		return nil, status.Errorf(codes.InvalidArgument,
			"run state %q is not a transition this endpoint accepts", req.State)
	}
	if err != nil {
		// An illegal transition is the state machine working. Reported as
		// not-applied so a repeated at-least-once report is not an error storm.
		s.log.DebugContext(ctx, "run state transition refused",
			"run_id", req.RunID, "requested", req.State, "err", err)
		return &SetRunStateResponse{Applied: false}, nil
	}
	return &SetRunStateResponse{State: row.State, Applied: true}, nil
}

// PutSpec stores a new content-addressed version, or returns the existing one.
func (s *SpecServer) PutSpec(ctx context.Context, req *PutSpecRequest) (*PutSpecResponse, error) {
	if req.Namespace == "" || req.Name == "" || req.SpecHash == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace, name and spec_hash are required")
	}
	if len(req.CanonicalJSON) == 0 {
		return nil, status.Error(codes.InvalidArgument, "canonical_json is required")
	}
	row, created, err := store.WFMPutSpec(ctx, s.db,
		req.Namespace, req.Name, req.SpecHash, req.CanonicalJSON, req.CreatedBy)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "put spec: %v", err)
	}
	return &PutSpecResponse{
		ID: row.ID, Namespace: row.Namespace, Name: row.Name,
		Version: row.Version, SpecHash: row.SpecHash, Created: created,
	}, nil
}

// GetSpec resolves a spec reference to a pinned version.
//
// Resolution lives here rather than in the gateway because pinning a NAME to a
// version is a decision about what a run will execute forever (ADR-0004), and that
// belongs with the table's owner.
func (s *SpecServer) GetSpec(ctx context.Context, req *GetSpecRequest) (*GetSpecResponse, error) {
	var (
		row store.SpecRow
		err error
	)
	switch {
	case req.SpecID != "":
		row, err = store.WFMGetSpecByID(ctx, s.db, req.SpecID)
	case req.SpecName != "":
		row, err = store.WFMGetLatestSpec(ctx, s.db, req.Namespace, req.SpecName)
	default:
		return nil, status.Error(codes.InvalidArgument, "either spec_id or spec_name is required")
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || store.IsMalformedID(err) {
			return nil, status.Errorf(codes.NotFound, "get spec: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "get spec: %v", err)
	}
	if row.Namespace != req.Namespace {
		// Namespaces are the tenancy boundary: knowing a UUID must not grant
		// cross-tenant read.
		return nil, status.Error(codes.NotFound, "get spec: not found in this namespace")
	}
	return &GetSpecResponse{ID: row.ID, Name: row.Name, Version: row.Version, SpecHash: row.SpecHash}, nil
}

// SpecServiceDesc is the hand-written descriptor. Mirrors the dag-engine's shape
// so that replacing both with generated stubs is one mechanical change.
var SpecServiceDesc = grpc.ServiceDesc{
	ServiceName: "nexus.v1.WorkflowSpecService",
	HandlerType: (*specService)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "PutSpec", Handler: putSpecHandler},
		{MethodName: "GetSpec", Handler: getSpecHandler},
		{MethodName: "SetRunState", Handler: setRunStateHandler},
		{MethodName: "DeleteSpec", Handler: deleteSpecHandler},
		{MethodName: "CreateSchedule", Handler: createScheduleHandler},
		{MethodName: "UpdateSchedule", Handler: updateScheduleHandler},
		{MethodName: "SetScheduleEnabled", Handler: setScheduleEnabledHandler},
		{MethodName: "DeleteSchedule", Handler: deleteScheduleHandler},
		{MethodName: "StartBackfill", Handler: startBackfillHandler},
		{MethodName: "BackfillAction", Handler: backfillActionHandler},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "services/workflow-manager/grpcapi/wfm_server.go",
}

func putSpecHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(PutSpecRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(specService).PutSpec(ctx, in)
	}
	return interceptor(ctx, in, &grpc.UnaryServerInfo{
		Server: srv, FullMethod: "/nexus.v1.WorkflowSpecService/PutSpec",
	}, func(ctx context.Context, req any) (any, error) {
		return srv.(specService).PutSpec(ctx, req.(*PutSpecRequest))
	})
}

func getSpecHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(GetSpecRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(specService).GetSpec(ctx, in)
	}
	return interceptor(ctx, in, &grpc.UnaryServerInfo{
		Server: srv, FullMethod: "/nexus.v1.WorkflowSpecService/GetSpec",
	}, func(ctx context.Context, req any) (any, error) {
		return srv.(specService).GetSpec(ctx, req.(*GetSpecRequest))
	})
}

func setRunStateHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(SetRunStateRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(specService).SetRunState(ctx, in)
	}
	return interceptor(ctx, in, &grpc.UnaryServerInfo{
		Server: srv, FullMethod: "/nexus.v1.WorkflowSpecService/SetRunState",
	}, func(ctx context.Context, req any) (any, error) {
		return srv.(specService).SetRunState(ctx, req.(*SetRunStateRequest))
	})
}

// RegisterTyped wires both halves onto a gRPC server.
//
// db is the workflow-manager's own pool: it is the OWNER of workflow_specs, so
// writing the table here is the ownership rule being followed, not bent.
// backfills may be nil; those RPCs then answer Unimplemented.
func RegisterTyped(srv *grpc.Server, runSvc RunService, db store.GWDB, log *slog.Logger, backfills BackfillService) {
	if log == nil {
		log = slog.Default()
	}
	nexusv1.RegisterWorkflowManagerServiceServer(srv, &RunServer{runs: runSvc, log: log})
	srv.RegisterService(&SpecServiceDesc, &SpecServer{db: db, runs: runSvc, log: log, backfills: backfills})
	log.Info("workflow-manager gRPC registered",
		"services", []string{"nexus.v1.WorkflowManagerService", "nexus.v1.WorkflowSpecService"})
}

// JSONCodec is the server-side codec for the hand-written spec service.
//
// Exported so main.go can register it: encoding.RegisterCodec is process-global,
// and registering it from the transport package rather than at the call site keeps
// the one place that needs it honest about the cost.
type JSONCodec struct{}

func (JSONCodec) Marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal grpc message: %v", err)
	}
	return b, nil
}

func (JSONCodec) Unmarshal(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return status.Errorf(codes.InvalidArgument, "unmarshal grpc message: %v", err)
	}
	return nil
}

// Name must match what the dag-engine registers, because both are "json" in one
// process-wide registry. They are byte-compatible (both plain encoding/json), so a
// collision is harmless — but it is a collision, and worth stating.
func (JSONCodec) Name() string { return "json" }
