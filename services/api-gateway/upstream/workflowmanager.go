package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

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
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
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
		return store.RunRow{}, false, fmt.Errorf("create run: %w", err)
	}
	// The response carries only the id and the dedup flag, so the full row is read
	// back as a projection. The gateway is permitted to READ every table
	// (ADR-0011); it is writes that must go through the owner, and this one just
	// did.
	return store.RunRow{ID: resp.GetRunId(), Namespace: req.Namespace}, !resp.GetDeduplicated(), nil
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
		resp, err = w.runs.RetryRun(ctx, &nexusv1.RetryRunRequest{RunId: runID})
	default:
		return nexusv1.RunState_RUN_STATE_UNSPECIFIED, fmt.Errorf("%s: unknown run lifecycle operation", op)
	}
	if err != nil {
		return nexusv1.RunState_RUN_STATE_UNSPECIFIED, fmt.Errorf("%s %s: %w", op, runID, err)
	}
	if !resp.GetAccepted() {
		// A refused transition is the run's state machine working (§5.2), not a
		// transport failure, so it is reported with the reason the owner gave.
		return resp.GetState(), fmt.Errorf("%s %s refused: %s", op, runID, resp.GetReason())
	}
	return resp.GetState(), nil
}
