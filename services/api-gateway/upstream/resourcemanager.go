package upstream

import (
	"context"
	"fmt"
	"github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/transport"
	"log/slog"

	"google.golang.org/grpc"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
)

// rmGRPC is the live resource-manager client, used when an address is configured.
//
// Split into its own file from the fallback shells so that the WIRED path and the
// not-yet-wired path are never confused for one another while the other services
// land: everything here makes a real RPC.
type rmGRPC struct {
	conn   *grpc.ClientConn
	client rmv1.ResourceManagerServiceClient
	log    *slog.Logger
}

func dialRM(ctx context.Context, addr string, log *slog.Logger) (*rmGRPC, error) {
	// Insecure for Phase 1. ADR-0011 names mTLS between services as a cost of the
	// decomposition; it belongs in the deployment layer (a sidecar or a service
	// mesh) rather than hard-coded here, so this is a deliberate deferral and not
	// an oversight.
	creds, terr := transport.DialOption()
	if terr != nil {
		return nil, fmt.Errorf("resolve TLS dial options: %w", terr)
	}
	conn, err := grpc.NewClient(config.PeerTarget(addr), append(config.PeerDialOptions(), creds)...)
	if err != nil {
		return nil, fmt.Errorf("dial resource-manager at %s: %w", addr, err)
	}
	return &rmGRPC{conn: conn, client: rmv1.NewResourceManagerServiceClient(conn), log: log}, nil
}

func (r *rmGRPC) close() error { return r.conn.Close() }

// registerWorker creates the per-session worker row.
func (r *rmGRPC) registerWorker(ctx context.Context, req *nexusv1.RegisterRequest, sessionTokenHash string) (string, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()

	resp, err := r.client.RegisterWorker(ctx, &rmv1.RegisterWorkerRequest{
		Namespace:    req.GetNamespace(),
		Hostname:     req.GetHostname(),
		Queues:       req.GetQueues(),
		Labels:       req.GetLabels(),
		Capacity:     req.GetCapacity(),
		Executors:    req.GetExecutors(),
		AgentVersion: req.GetAgentVersion(),
		// Only the HASH crosses this boundary. The plaintext session token exists in
		// exactly one place — the response the gateway hands the worker — so a
		// compromise of either the metadata store or this RPC leaks nothing usable.
		SessionTokenHash: sessionTokenHash,
	})
	if err != nil {
		return "", fmt.Errorf("register worker: %w", err)
	}
	return resp.GetWorkerId(), nil
}

// heartbeat records liveness AND performs the ledger reconciliation in ONE RPC.
//
// The resource-manager exposes these together deliberately, and that is the right
// shape: the heartbeat is the only moment the control plane holds GROUND TRUTH about
// what a worker is running (§3.2). Reconciling in a separate call would mean
// reconciling against a snapshot that has already moved, and splitting them across
// two RPCs would let one succeed and the other fail, leaving liveness updated and
// the leak still committed.
//
// Returned: whether the worker session is still known, which the gateway turns into a
// REREGISTER directive rather than an error, so a healthy host recovers itself.
func (r *rmGRPC) heartbeat(ctx context.Context, workerID string, runningAttemptIDs []string) (workerKnown bool, releasedCount int, err error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()

	resp, err := r.client.HeartbeatWorker(ctx, &rmv1.HeartbeatWorkerRequest{
		WorkerId:          workerID,
		RunningAttemptIds: runningAttemptIDs,
	})
	if err != nil {
		return false, 0, fmt.Errorf("heartbeat worker: %w", err)
	}
	return resp.GetWorkerKnown(), len(resp.GetReleased()), nil
}

// release returns one leaked reservation's capacity to the pool.
//
// Idempotent by the resource-manager's contract, which matters because release is
// driven by three independent at-least-once paths (completion reports, lease expiry,
// and heartbeat reconciliation) that will sometimes all fire for one attempt.
func (r *rmGRPC) release(ctx context.Context, attemptID, reason string) error {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()

	if _, err := r.client.Release(ctx, &rmv1.ReleaseRequest{AttemptId: attemptID}); err != nil {
		return fmt.Errorf("release reservation for attempt %s: %w", attemptID, err)
	}
	return nil
}

// setState transitions a worker's lifecycle state, e.g. to DRAINING.
func (r *rmGRPC) setState(ctx context.Context, workerID string, state rmv1.WorkerState, reason string) error {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()

	if _, err := r.client.SetWorkerState(ctx, &rmv1.SetWorkerStateRequest{
		WorkerId: workerID,
		State:    state,
		Reason:   reason,
	}); err != nil {
		return fmt.Errorf("set worker %s state to %s: %w", workerID, state, err)
	}
	return nil
}
