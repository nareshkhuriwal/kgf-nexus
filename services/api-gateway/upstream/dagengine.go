package upstream

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// dagGRPC calls the dag-engine, which owns task_nodes and task_edges.
//
// The gateway calls exactly one of its RPCs: ReconcileRun, immediately after a
// terminal attempt report. That is deliberately the only one — validation and
// materialization happen on the submit and create paths, through
// workflow-manager, which is their owner.
//
// dag-engine serves a hand-written descriptor with a JSON codec rather than
// generated stubs, so the wire types are redeclared here.
type dagGRPC struct {
	conn *grpc.ClientConn
	log  *slog.Logger
}

func dialDAG(ctx context.Context, addr string, log *slog.Logger) (*dagGRPC, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial dag-engine at %s: %w", addr, err)
	}
	return &dagGRPC{conn: conn, log: log}, nil
}

func (d *dagGRPC) close() error { return d.conn.Close() }

type dagReconcileRequest struct {
	RunID string `json:"run_id"`
	// NodeID is advisory. The engine recomputes the whole run regardless, so a
	// lost or misaddressed notification cannot strand a run.
	NodeID string `json:"node_id,omitempty"`
}

type dagReconcileResponse struct {
	RunID       string   `json:"run_id"`
	Frontier    []string `json:"frontier"`
	AttemptIDs  []string `json:"attempt_ids"`
	RunState    string   `json:"run_state"`
	RunTerminal bool     `json:"run_terminal"`
}

// reconcileRun recomputes a run's frontier, creating the next attempts.
func (d *dagGRPC) reconcileRun(ctx context.Context, runID, nodeID string) error {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()

	var resp dagReconcileResponse
	if err := d.conn.Invoke(ctx, "/nexus.v1.DagEngineService/ReconcileRun",
		&dagReconcileRequest{RunID: runID, NodeID: nodeID}, &resp,
		grpc.ForceCodec(wfmJSONCodec{})); err != nil {
		return fmt.Errorf("reconcile run %s: %w", runID, err)
	}
	d.log.DebugContext(ctx, "run reconciled after attempt result",
		"run_id", runID, "node_id", nodeID, "run_state", resp.RunState,
		"frontier", resp.Frontier, "terminal", resp.RunTerminal)
	return nil
}
