package grpcapi

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/transport"
	"log/slog"

	"google.golang.org/grpc"
)

// The dag-engine's gRPC surface is a HAND-WRITTEN descriptor with a JSON codec
// (services/dag-engine/grpcapi.go), not generated protobuf stubs. Its wire types
// live in that service's `package main` and so cannot be imported, which is why
// they are redeclared here.
//
// They are deliberately kept minimal — only the fields this caller reads. The
// codec is plain encoding/json, so a field the server sends and we do not declare
// is simply dropped rather than an error, which is the same tolerance ADR-0011
// rule 5 asks for in the other direction.
type deMaterializeRunRequest struct {
	RunID string `json:"run_id"`
}

type deMaterializeRunResponse struct {
	RunID          string   `json:"run_id"`
	NodesInserted  int64    `json:"nodes_inserted"`
	EdgesInserted  int64    `json:"edges_inserted"`
	Frontier       []string `json:"frontier"`
	AttemptIDs     []string `json:"attempt_ids"`
	AlreadyExisted bool     `json:"already_existed"`
}

type deReconcileRunRequest struct {
	RunID  string `json:"run_id"`
	NodeID string `json:"node_id,omitempty"`
}

type deReconcileRunResponse struct {
	RunID          string   `json:"run_id"`
	Frontier       []string `json:"frontier"`
	AttemptIDs     []string `json:"attempt_ids"`
	RunState       string   `json:"run_state"`
	RunTerminal    bool     `json:"run_terminal"`
	Halted         bool     `json:"halted"`
	Skipped        []string `json:"skipped,omitempty"`
	UpstreamFailed []string `json:"upstream_failed,omitempty"`
}

// deJSONCodec mirrors the dag-engine's server-side codec.
//
// Registered PER CALL via grpc.CallContentSubtype rather than globally with
// encoding.RegisterCodec: the registry is process-wide, and a global codec named
// "json" would apply to every gRPC connection this process makes — including the
// protobuf ones — which is a much larger blast radius than one client needs.
type deJSONCodec struct{}

func (deJSONCodec) Marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal dag-engine message: %w", err)
	}
	return b, nil
}

func (deJSONCodec) Unmarshal(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("unmarshal dag-engine message: %w", err)
	}
	return nil
}

// Name must match the server's codec name exactly, or the content-subtype
// negotiation fails and every call errors on an unknown codec.
func (deJSONCodec) Name() string { return "json" }

// deConn lazily dials the dag-engine.
//
// grpc.NewClient does not connect eagerly, so this is cheap and the connection is
// established on the first call. That matters at startup: workflow-manager must
// come up and serve health even when dag-engine is still starting.
type deConn struct {
	conn *grpc.ClientConn
	log  *slog.Logger
}

func dialDAGEngine(addr string, log *slog.Logger) (*deConn, error) {
	// Insecure for Phase 1, matching every other inter-service client here.
	// ADR-0011 puts mTLS in the deployment layer.
	creds, terr := transport.DialOption()
	if terr != nil {
		return nil, fmt.Errorf("resolve TLS dial options: %w", terr)
	}
	conn, err := grpc.NewClient(config.PeerTarget(addr), append(config.PeerDialOptions(), creds)...)
	if err != nil {
		return nil, fmt.Errorf("dial dag-engine at %s: %w", addr, err)
	}
	return &deConn{conn: conn, log: log}, nil
}

func (d *deConn) invoke(ctx context.Context, method string, req, resp any) error {
	return d.conn.Invoke(ctx, method, req, resp, grpc.ForceCodec(deJSONCodec{}))
}

func (d *deConn) close() error { return d.conn.Close() }
