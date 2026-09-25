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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/rest"
)

// wfmGRPC is the live workflow-manager client.
//
// It speaks TWO protocols to one address, which is unusual enough to explain:
// run lifecycle uses the generated nexus.v1.WorkflowManagerService stubs, while
// spec submission uses a hand-written JSON-codec service. That is not a design
// preference — the frozen proto defines no PutSpec RPC, and the gateway may not
// write workflow_specs itself (ADR-0011), so the only honest option left is an
// RPC the proto does not yet describe. See the gap note in
// services/workflow-manager/grpcapi/wfm_server.go.
type wfmGRPC struct {
	conn *grpc.ClientConn
	runs nexusv1.WorkflowManagerServiceClient
	log  *slog.Logger
}

func dialWFM(ctx context.Context, addr string, log *slog.Logger) (*wfmGRPC, error) {
	creds, terr := transport.DialOption()
	if terr != nil {
		return nil, fmt.Errorf("resolve TLS dial options: %w", terr)
	}
	conn, err := grpc.NewClient(config.PeerTarget(addr), append(config.PeerDialOptions(), creds)...)
	if err != nil {
		return nil, fmt.Errorf("dial workflow-manager at %s: %w", addr, err)
	}
	return &wfmGRPC{conn: conn, runs: nexusv1.NewWorkflowManagerServiceClient(conn), log: log}, nil
}

func (w *wfmGRPC) close() error { return w.conn.Close() }

// wfmJSONCodec mirrors the workflow-manager's spec-service codec.
//
// Applied per call via grpc.ForceCodec rather than registered globally: the codec
// registry is process-wide, and a global "json" codec in the gateway would also
// apply to its protobuf connections to resource-manager and scheduler.
type wfmJSONCodec struct{}

func (wfmJSONCodec) Marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow-manager message: %w", err)
	}
	return b, nil
}

func (wfmJSONCodec) Unmarshal(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("unmarshal workflow-manager message: %w", err)
	}
	return nil
}

func (wfmJSONCodec) Name() string { return "json" }

// The spec service's wire types, redeclared because the server's live in another
// service's package. Only the fields this caller uses.
type wfmPutSpecRequest struct {
	Namespace     string          `json:"namespace"`
	Name          string          `json:"name"`
	SpecHash      string          `json:"spec_hash"`
	CanonicalJSON json.RawMessage `json:"canonical_json"`
	CreatedBy     string          `json:"created_by"`
}

type wfmPutSpecResponse struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Version   int32  `json:"version"`
	SpecHash  string `json:"spec_hash"`
	Created   bool   `json:"created"`
}

type wfmGetSpecRequest struct {
	Namespace string `json:"namespace"`
	SpecID    string `json:"spec_id,omitempty"`
	SpecName  string `json:"spec_name,omitempty"`
}

type wfmGetSpecResponse struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Version  int32  `json:"version"`
	SpecHash string `json:"spec_hash"`
}

// putSpec stores a validated, canonicalized spec version through its owner.
func (w *wfmGRPC) putSpec(ctx context.Context, namespace, name, hash string, canonicalJSON []byte, createdBy string) (store.SpecRow, bool, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()

	var resp wfmPutSpecResponse
	if err := w.conn.Invoke(ctx, "/nexus.v1.WorkflowSpecService/PutSpec", &wfmPutSpecRequest{
		Namespace: namespace, Name: name, SpecHash: hash,
		// The bytes the gateway canonicalized and hashed, sent verbatim. Anything
		// else would break the content address the caller was just handed.
		CanonicalJSON: canonicalJSON,
		CreatedBy:     createdBy,
	}, &resp, grpc.ForceCodec(wfmJSONCodec{})); err != nil {
		return store.SpecRow{}, false, fmt.Errorf("put spec %s/%s: %w", namespace, name, err)
	}
	return store.SpecRow{
		ID: resp.ID, Namespace: resp.Namespace, Name: resp.Name,
		Version: resp.Version, SpecHash: resp.SpecHash,
	}, resp.Created, nil
}

// createRun creates a run through its owner.
//
// A spec NAME is resolved to an id first, because the proto's CreateRunRequest
// carries only spec_id. Resolving through the owner rather than reading
// workflow_specs locally keeps the pinning decision (ADR-0004) with the table's
// owner, where it belongs.
func (w *wfmGRPC) createRun(ctx context.Context, req rest.CreateRunRequest) (store.RunRow, bool, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()

	specID := req.SpecID
	if specID == "" {
		var got wfmGetSpecResponse
		if err := w.conn.Invoke(ctx, "/nexus.v1.WorkflowSpecService/GetSpec", &wfmGetSpecRequest{
			Namespace: req.Namespace, SpecName: req.SpecName,
		}, &got, grpc.ForceCodec(wfmJSONCodec{})); err != nil {
			return store.RunRow{}, false, fmt.Errorf("resolve spec %q: %w", req.SpecName, err)
		}
		specID = got.ID
	}

	// Params are flattened to strings because the proto declares
	// map<string,string>. A structured value is carried as its JSON text so no
	// information is lost; the workflow-manager re-encodes the map into
	// workflow_runs.params.
	params := make(map[string]string, len(req.Params))
	for k, v := range req.Params {
		if s, ok := v.(string); ok {
			params[k] = s
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return store.RunRow{}, false, fmt.Errorf("encode run param %q: %w", k, err)
		}
		params[k] = string(b)
	}

	resp, err := w.runs.CreateRun(ctx, &nexusv1.CreateRunRequest{
		SpecId:         specID,
		Namespace:      req.Namespace,
		IdempotencyKey: req.IdempotencyKey,
		Params:         params,
		Priority:       req.Priority,
		TriggerKind:    req.TriggerKind,
	})
	if err != nil {
		// No "create run:" prefix here: the owner's runError already prefixes the
		// op, and reattachSentinel preserves that message, so wrapping again gives
		// the operator "create run: create run: ...".
		return store.RunRow{}, false, reattachSentinel(err)
	}
	// The response carries only the id and the dedup flag, so the full row is read
	// back as a projection. The gateway is permitted to READ every table
	// (ADR-0011); it is writes that must go through the owner, and this one just
	// did.
	return store.RunRow{ID: resp.GetRunId(), Namespace: req.Namespace}, !resp.GetDeduplicated(), nil
}

// reattachSentinel re-attaches the store sentinel a gRPC status code stands for.
//
// The owner maps its sentinels onto codes (runError) precisely so the gateway can
// answer with the right HTTP status, but a status travels the wire as a code and
// a string -- the sentinel does not survive it, so `errors.Is(err,
// ErrMaxActiveRuns)` on this side is always false and every one of those
// carefully mapped codes fell through to a blanket 500.
//
// The visible cost was a workflow at its max_active_runs cap answering
// "HTTP 500: this is a control-plane fault" when the documented limit was simply
// doing its job. An operator reads that as an outage and goes looking for one.
func reattachSentinel(err error) error {
	switch status.Code(err) {
	case codes.ResourceExhausted:
		// A namespace run quota (ADR-0021) travels as ResourceExhausted with an
		// ErrorInfo detail; without one it is the per-workflow cap.
		if q := store.RunQuotaFromStatus(status.Convert(err)); q != nil {
			return q
		}
		return fmt.Errorf("%w: %s", store.ErrMaxActiveRuns, status.Convert(err).Message())
	case codes.NotFound:
		return fmt.Errorf("%w: %s", store.ErrNotFound, status.Convert(err).Message())
	case codes.InvalidArgument:
		// The owner rejected the DOCUMENT -- today, a run omitting a parameter
		// its spec declares required. Same reasoning as the cases above: without
		// re-attaching a sentinel this arrives as a bare status and answers 500,
		// telling an operator the platform broke when their request was simply
		// incomplete.
		return fmt.Errorf("%w: %s", store.ErrInvalidRequest, status.Convert(err).Message())
	default:
		return err
	}
}

// lifecycle performs one run state transition through its owner.
func (w *wfmGRPC) lifecycle(ctx context.Context, op, runID string) (nexusv1.RunState, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()

	var (
		resp *nexusv1.RunActionResponse
		err  error
	)
	switch op {
	case "CancelRun":
		resp, err = w.runs.CancelRun(ctx, &nexusv1.CancelRunRequest{RunId: runID})
	case "PauseRun":
		resp, err = w.runs.PauseRun(ctx, &nexusv1.PauseRunRequest{RunId: runID})
	case "ResumeRun":
		resp, err = w.runs.ResumeRun(ctx, &nexusv1.ResumeRunRequest{RunId: runID})
	case "RetryRun":
		// A whole-run retry. A retry naming nodes goes through retry(), which
		// carries the selection this generic path has no argument for.
		resp, err = w.runs.RetryRun(ctx, &nexusv1.RetryRunRequest{RunId: runID})
	default:
		return nexusv1.RunState_RUN_STATE_UNSPECIFIED, fmt.Errorf("%s: unknown run lifecycle operation", op)
	}
	if err != nil {
		return nexusv1.RunState_RUN_STATE_UNSPECIFIED, fmt.Errorf("%s %s: %w", op, runID, reattachLifecycleSentinel(err))
	}
	if !resp.GetAccepted() {
		// A refused transition is the run's state machine working (§5.2), not a
		// transport failure, so it is reported with the reason the owner gave.
		return resp.GetState(), fmt.Errorf("%s %s refused: %s", op, runID, resp.GetReason())
	}
	return resp.GetState(), nil
}

// retry performs an operator retry through the owner, carrying the node
// selection and the acting principal.
//
// The status is returned WRAPPED rather than reduced to a sentinel: the gateway
// answers a refused selection, a lost race and an unavailable engine
// differently, and only the code tells them apart. The run-state conflict is
// re-attached as well, so a caller matching on ErrRunStateConflict still can.
func (w *wfmGRPC) retry(ctx context.Context, runID string, nodes []string, actor string) (nexusv1.RunState, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	resp, err := w.runs.RetryRun(ctx, &nexusv1.RetryRunRequest{RunId: runID, NodeIds: nodes, Actor: actor})
	if err != nil {
		if status.Code(err) == codes.FailedPrecondition {
			return nexusv1.RunState_RUN_STATE_UNSPECIFIED,
				fmt.Errorf("%w: %s", store.ErrRunStateConflict, status.Convert(err).Message())
		}
		switch status.Code(err) {
		case codes.NotFound, codes.InvalidArgument:
			return nexusv1.RunState_RUN_STATE_UNSPECIFIED,
				fmt.Errorf("RetryRun %s: %w", runID, reattachSentinel(err))
		}
		return nexusv1.RunState_RUN_STATE_UNSPECIFIED, fmt.Errorf("RetryRun %s: %w", runID, err)
	}
	if !resp.GetAccepted() {
		return resp.GetState(), fmt.Errorf("RetryRun %s refused: %w: %s", runID, store.ErrRunStateConflict, resp.GetReason())
	}
	return resp.GetState(), nil
}

// reattachLifecycleSentinel maps the owner's "illegal for the run's current
// state" answer back onto store.ErrRunStateConflict.
//
// The owner reports it as FailedPrecondition; before this the lifecycle path
// returned the bare status, the REST layer's errors.Is(ErrRunStateConflict)
// never matched, and pausing a run that had just finished answered 500 -- "the
// platform broke" -- instead of 409 with the reason.
func reattachLifecycleSentinel(err error) error {
	if status.Code(err) == codes.FailedPrecondition {
		return fmt.Errorf("%w: %s", store.ErrRunStateConflict, status.Convert(err).Message())
	}
	return reattachSentinel(err)
}
