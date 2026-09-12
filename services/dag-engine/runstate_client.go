package main

import (
	"context"
	"fmt"
	"github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/transport"
	"log/slog"
	"time"

	"google.golang.org/grpc"

	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// wfmRunStateClient publishes a run-level transition the engine detected to the
// workflow-manager, which owns workflow_runs (ADR-0011).
//
// The engine is the component that knows FIRST: it computes the frontier, so it
// sees the moment a run's first attempts exist and the moment every node is
// terminal. It must not write the table itself, so the conclusion travels back to
// the owner, whose own §5.2 state machine adjudicates it.
type wfmRunStateClient struct {
	conn *grpc.ClientConn
	log  *slog.Logger
}

func dialWFMRunState(addr string, log *slog.Logger) (*wfmRunStateClient, error) {
	creds, terr := transport.DialOption()
	if terr != nil {
		return nil, fmt.Errorf("resolve TLS dial options: %w", terr)
	}
	conn, err := grpc.NewClient(config.PeerTarget(addr), append(config.PeerDialOptions(), creds)...)
	if err != nil {
		return nil, fmt.Errorf("dial workflow-manager at %s: %w", addr, err)
	}
	return &wfmRunStateClient{conn: conn, log: log}, nil
}

func (c *wfmRunStateClient) close() error { return c.conn.Close() }

type setRunStateRequest struct {
	RunID  string `json:"run_id"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type setRunStateResponse struct {
	State   string `json:"state"`
	Applied bool   `json:"applied"`
}

// SetRunState implements RunStateWriter.
func (c *wfmRunStateClient) SetRunState(ctx context.Context, runID string, state domain.RunState, reason string) error {
	// A short timeout of its own: this call sits on the reconcile path, and a
	// slow workflow-manager must not hold the engine's reconcile open, since the
	// node state it has already written is durable and correct regardless.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var resp setRunStateResponse
	if err := c.conn.Invoke(ctx, "/nexus.v1.WorkflowSpecService/SetRunState",
		&setRunStateRequest{RunID: runID, State: string(state), Reason: reason},
		&resp, grpc.ForceCodec(jsonCodec{})); err != nil {
		return fmt.Errorf("set run %s state to %s: %w", runID, state, err)
	}
	if !resp.Applied {
		// The owner's state machine refused it. Not an error here: the engine
		// reports at-least-once, so a repeat of a transition already made is the
		// expected steady state, not a fault.
		c.log.DebugContext(ctx, "run state transition not applied by its owner",
			"run_id", runID, "requested", string(state))
	}
	return nil
}
