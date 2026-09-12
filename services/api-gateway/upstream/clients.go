// Package upstream holds the gateway's clients for the five other control-plane
// services.
//
// The gateway owns only api_keys and idempotency_keys (ADR-0011). Everything else
// it needs to CHANGE belongs to another service, and this package is where that
// boundary lives: a single place to see every cross-service call the gateway makes,
// each with a timeout, because a cross-service call that can hang forever turns one
// slow service into a gateway outage.
//
// Phase-1 status: the peer services' gRPC APIs are still being built by their
// owning agents. Until each lands, the clients here fall back to READ-ONLY queries
// against the shared schema for projections the gateway is permitted to read, and
// return an explicit "upstream not configured" error for every WRITE. Failing
// loudly rather than writing a table we do not own is the whole point — a silent
// cross-service write is exactly the violation ADR-0011's per-service DB roles
// exist to make impossible, and it must not be papered over here.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/rest"
	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
)

// ErrUpstreamNotConfigured means the owning service's address was not supplied.
//
// Surfaced as a 502 rather than silently writing the table ourselves: the gateway
// does not own it, and quietly crossing that boundary would work in development and
// then fail at the database in production, where per-service roles are enforced.
var ErrUpstreamNotConfigured = errors.New("upstream service is not configured")

// callTimeout bounds every cross-service call. ADR-0011 calls this out as a named
// cost of the decomposition: a call that was a function call is now a network hop
// with new failure modes, and every one needs a timeout and a retry budget.
const callTimeout = 5 * time.Second

// withCallTimeout derives the context for one cross-service call.
//
// Every outbound call routes through here so the timeout cannot be forgotten on a
// new method: an un-timed-out call to a hung peer holds a gateway goroutine and a
// client connection indefinitely, and enough of those is an outage caused entirely
// by someone else's slowness.
func withCallTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, callTimeout)
}

// ── workflow-manager ──────────────────────────────────────────────────────

// WorkflowManagerClient talks to workflow-manager, which owns workflow_specs,
// workflow_runs, and timers.
type WorkflowManagerClient struct {
	addr string
	db   store.GWQuerier
	log  *slog.Logger
	// grpc is non-nil once an address is configured. Mutations go through it;
	// reads the gateway is permitted to make stay local.
	grpc *wfmGRPC
}

// DialWorkflowManager creates the client.
//
// db is used for READ-BACK only: the run lifecycle RPCs return a state and an id
// rather than a full row, and re-reading the projection locally is cheaper and
// more honest than teaching the proto to carry a denormalized row that would then
// have two definitions.
func DialWorkflowManager(ctx context.Context, addr string, db store.GWQuerier, log *slog.Logger) (*WorkflowManagerClient, error) {
	c := &WorkflowManagerClient{addr: addr, db: db, log: log}
	if addr == "" {
		log.Warn("workflow-manager address is unset; spec and run MUTATIONS will fail with 502 rather than write tables the gateway does not own",
			"remedy", "set NEXUS_WORKFLOW_MANAGER_ADDR")
		return c, nil
	}
	g, err := dialWFM(ctx, addr, log)
	if err != nil {
		return nil, err
	}
	c.grpc = g
	return c, nil
}

// Close releases the connection.
func (c *WorkflowManagerClient) Close() error {
	if c.grpc != nil {
		return c.grpc.close()
	}
	return nil
}

// PutSpec stores a validated spec version through its owner.
func (c *WorkflowManagerClient) PutSpec(ctx context.Context, namespace, name, hash string, canonicalJSON []byte, createdBy string) (store.SpecRow, bool, error) {
	if c.grpc == nil {
		return store.SpecRow{}, false, fmt.Errorf("put spec: %w (workflow-manager owns workflow_specs)", ErrUpstreamNotConfigured)
	}
	return c.grpc.putSpec(ctx, namespace, name, hash, canonicalJSON, createdBy)
}

// CreateRun creates a run through its owner, which also materializes its DAG.
func (c *WorkflowManagerClient) CreateRun(ctx context.Context, req rest.CreateRunRequest) (store.RunRow, bool, error) {
	if c.grpc == nil {
		return store.RunRow{}, false, fmt.Errorf("create run: %w (workflow-manager owns workflow_runs)", ErrUpstreamNotConfigured)
	}
	row, created, err := c.grpc.createRun(ctx, req)
	if err != nil {
		return store.RunRow{}, false, err
	}
	// Read the full projection back so the REST response carries the same shape a
	// GET would. A failure here is not fatal: the run IS created, and answering
	// with the id alone beats reporting a failure for work that succeeded.
	if full, rerr := store.GatewayGetRun(ctx, c.db, req.Namespace, row.ID); rerr == nil {
		return full, created, nil
	}
	return row, created, nil
}

func (c *WorkflowManagerClient) CancelRun(ctx context.Context, namespace, runID string) (store.RunRow, error) {
	return c.lifecycle(ctx, namespace, "CancelRun", runID)
}

func (c *WorkflowManagerClient) PauseRun(ctx context.Context, namespace, runID string) (store.RunRow, error) {
	return c.lifecycle(ctx, namespace, "PauseRun", runID)
}

func (c *WorkflowManagerClient) ResumeRun(ctx context.Context, namespace, runID string) (store.RunRow, error) {
	return c.lifecycle(ctx, namespace, "ResumeRun", runID)
}

func (c *WorkflowManagerClient) RetryRun(ctx context.Context, namespace, runID string) (store.RunRow, error) {
	return c.lifecycle(ctx, namespace, "RetryRun", runID)
}

func (c *WorkflowManagerClient) lifecycle(ctx context.Context, namespace, op, runID string) (store.RunRow, error) {
	if c.grpc == nil {
		return store.RunRow{}, fmt.Errorf("%s: %w (workflow-manager owns workflow_runs)", op, ErrUpstreamNotConfigured)
	}
	if _, err := c.grpc.lifecycle(ctx, op, runID); err != nil {
		return store.RunRow{}, err
	}
	return store.GatewayGetRun(ctx, c.db, namespace, runID)
}

// ── resource-manager ──────────────────────────────────────────────────────

// ResourceManagerClient talks to resource-manager, which owns workers and
// reservations.
//
// It holds a database handle for the READ side only. Capacity is a projection the
// gateway is explicitly permitted to read (ADR-0011: the gateway reads all tables),
// and serving it locally avoids a network hop on a path the UI polls.
type ResourceManagerClient struct {
	addr string
	db   store.GWQuerier
	log  *slog.Logger
	// grpc is non-nil once an address is configured. Mutations go through it; reads
	// that the gateway is permitted to make stay local.
	grpc *rmGRPC
}

// DialResourceManager creates the client.
func DialResourceManager(ctx context.Context, addr string, db store.GWQuerier, log *slog.Logger) (*ResourceManagerClient, error) {
	c := &ResourceManagerClient{addr: addr, db: db, log: log}
	if addr == "" {
		log.Warn("resource-manager address is unset; worker registration, drain, and reservation release will fail rather than write tables the gateway does not own",
			"remedy", "set NEXUS_RESOURCE_MANAGER_ADDR")
		return c, nil
	}
	g, err := dialRM(ctx, addr, log)
	if err != nil {
		return nil, err
	}
	c.grpc = g
	return c, nil
}

// Close releases the connection.
func (c *ResourceManagerClient) Close() error {
	if c.grpc != nil {
		return c.grpc.close()
	}
	return nil
}

// RegisterWorker creates the per-session worker row.
func (c *ResourceManagerClient) RegisterWorker(ctx context.Context, req *nexusv1.RegisterRequest, sessionTokenHash string) (string, error) {
	if c.grpc == nil {
		return "", fmt.Errorf("register worker: %w (resource-manager owns workers)", ErrUpstreamNotConfigured)
	}
	return c.grpc.registerWorker(ctx, req, sessionTokenHash)
}

// RecordHeartbeat updates liveness and free capacity.
func (c *ResourceManagerClient) RecordHeartbeat(ctx context.Context, workerID string, free *nexusv1.ResourceVector, cpuUtil, memUtil float64) error {
	if c.grpc == nil {
		return fmt.Errorf("record heartbeat: %w (resource-manager owns workers)", ErrUpstreamNotConfigured)
	}
	// running_attempt_ids is passed EMPTY here on purpose. The gateway's heartbeat
	// handler performs its own reconciliation pass and calls ReleaseReservation per
	// leak, so handing the same ground truth to the resource-manager's own
	// reconciler would make two components release the same reservation. Release is
	// idempotent, so it would be harmless — but it would also double-log every leak
	// and make the audit trail lie about how many were found.
	if _, _, err := c.grpc.heartbeat(ctx, workerID, nil); err != nil {
		return err
	}
	return nil
}

// ReleaseReservation returns leaked capacity to the pool.
//
// This is the call heartbeat reconciliation depends on (§3.2), and it goes through
// the owner deliberately: releasing a reservation must delete the row AND decrement
// workers.committed atomically, and only the owner can do both correctly.
func (c *ResourceManagerClient) ReleaseReservation(ctx context.Context, workerID, attemptID, reason string) error {
	if c.grpc == nil {
		return fmt.Errorf("release reservation: %w (resource-manager owns reservations)", ErrUpstreamNotConfigured)
	}
	return c.grpc.release(ctx, attemptID, reason)
}

// WorkerState reads the worker's lifecycle state.
//
// A local READ, not a call: this is on the heartbeat path, which every worker hits
// every few seconds, and the gateway is permitted to read the table. A network hop
// per heartbeat per worker would add load proportional to fleet size for
// information already one index probe away.
func (c *ResourceManagerClient) WorkerState(ctx context.Context, workerID string) (string, bool, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()

	var state string
	err := c.db.QueryRow(ctx, `SELECT state::text FROM workers WHERE id = $1`, workerID).Scan(&state)
	if err != nil {
		if isNoRows(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read worker state: %w", err)
	}
	return state, true, nil
}

// DrainWorker marks the worker DRAINING so the DRAIN directive rides its next
// heartbeat response.
func (c *ResourceManagerClient) DrainWorker(ctx context.Context, namespace, workerID string) error {
	if c.grpc == nil {
		return fmt.Errorf("drain worker: %w (resource-manager owns workers)", ErrUpstreamNotConfigured)
	}
	// Marking the worker DRAINING is the whole action. The command itself reaches the
	// worker on its next heartbeat RESPONSE, because the control plane never dials a
	// worker (ADR-0005) — which is why the REST handler answers 202, not 200.
	return c.grpc.setState(ctx, workerID, rmv1.WorkerState_WORKER_STATE_DRAINING, "operator drain via api-gateway")
}

// Capacity reports advertised versus committed capacity for a namespace.
//
// A read-only projection, aggregated in SQL rather than by summing in Go: the sum
// belongs where the rows are, and pulling every worker row across the wire to add
// up integers would scale with fleet size for no reason.
func (c *ResourceManagerClient) Capacity(ctx context.Context, namespace string) (rest.CapacityReport, error) {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()

	var (
		workers                 int
		advCPU, advMem, advDisk int64
		comCPU, comMem, comDisk int64
		advGPU, comGPU          int64
		pending                 int
	)
	err := c.db.QueryRow(ctx, `
		SELECT count(*)::int,
		       COALESCE(SUM((capacity ->> 'cpu_millis')::bigint), 0),
		       COALESCE(SUM((capacity ->> 'memory_bytes')::bigint), 0),
		       COALESCE(SUM((capacity ->> 'disk_bytes')::bigint), 0),
		       COALESCE(SUM((capacity ->> 'gpu_count')::bigint), 0),
		       COALESCE(SUM((committed ->> 'cpu_millis')::bigint), 0),
		       COALESCE(SUM((committed ->> 'memory_bytes')::bigint), 0),
		       COALESCE(SUM((committed ->> 'disk_bytes')::bigint), 0),
		       COALESCE(SUM((committed ->> 'gpu_count')::bigint), 0)
		  FROM workers
		 WHERE namespace = $1 AND state IN ('ACTIVE','DRAINING')`,
		namespace,
	).Scan(&workers, &advCPU, &advMem, &advDisk, &advGPU, &comCPU, &comMem, &comDisk, &comGPU)
	if err != nil {
		return rest.CapacityReport{}, fmt.Errorf("aggregate capacity: %w", err)
	}

	// The pending count is what turns an unexplained stall into an explained one:
	// "0 running, 40 pending" plus each attempt's pending_reason tells an operator
	// whether to add capacity or fix a spec.
	if err := c.db.QueryRow(ctx, `
		SELECT count(*)::int
		  FROM task_attempts a
		  JOIN workflow_runs r ON r.id = a.run_id
		 WHERE r.namespace = $1 AND a.state = 'PENDING'`,
		namespace,
	).Scan(&pending); err != nil {
		return rest.CapacityReport{}, fmt.Errorf("count pending attempts: %w", err)
	}

	return rest.CapacityReport{
		Namespace: namespace,
		Advertised: map[string]any{
			"cpu_millis": advCPU, "memory_bytes": advMem,
			"disk_bytes": advDisk, "gpu_count": advGPU,
		},
		Committed: map[string]any{
			"cpu_millis": comCPU, "memory_bytes": comMem,
			"disk_bytes": comDisk, "gpu_count": comGPU,
		},
		Workers:         workers,
		PendingAttempts: pending,
	}, nil
}

// ── scheduler ─────────────────────────────────────────────────────────────

// SchedulerClient talks to scheduler, which owns task_attempts, leases, and
// concurrency_slots.
//
// Every method here is a CALL rather than a local write, and PollTask in particular
// must be: the dispatch claim commits five facts in ONE transaction (invariant 4),
// and that transaction has to live inside its owner. Splitting it across a service
// boundary would replace a transaction with a saga on the one path where a
// compensation bug means a double-dispatched task.
type SchedulerClient struct {
	addr string
	log  *slog.Logger
	// grpc is non-nil once an address is configured.
	grpc *schedGRPC
	// dag is the DAG engine, called after a terminal attempt report so the run's
	// frontier advances. It lives on this client rather than in the REST layer
	// because the two calls are one causal step: a result that is applied without
	// a reconcile leaves the run stalled with no error anywhere.
	dag *dagGRPC
}

// DialScheduler creates the client.
func DialScheduler(ctx context.Context, addr, dagAddr string, log *slog.Logger) (*SchedulerClient, error) {
	c := &SchedulerClient{addr: addr, log: log}
	if addr == "" {
		log.Warn("scheduler address is unset; PollTask will return no work and attempt reports will fail",
			"remedy", "set NEXUS_SCHEDULER_ADDR")
		return c, nil
	}
	g, err := dialScheduler(ctx, addr, log)
	if err != nil {
		return nil, err
	}
	c.grpc = g

	if dagAddr == "" {
		log.Warn("dag-engine address is unset; runs will not advance past their first node",
			"remedy", "set NEXUS_DAG_ENGINE_ADDR")
		return c, nil
	}
	d, err := dialDAG(ctx, dagAddr, log)
	if err != nil {
		return nil, err
	}
	c.dag = d
	return c, nil
}

// Close releases the connections.
func (c *SchedulerClient) Close() error {
	var firstErr error
	if c.grpc != nil {
		if err := c.grpc.close(); err != nil {
			firstErr = err
		}
	}
	if c.dag != nil {
		if err := c.dag.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ClaimForWorker performs the atomic dispatch claim inside the scheduler.
//
// workerID is the id the gateway's authn interceptor established for this
// session, not one read from the worker's request. The scheduler takes it as
// given, so substituting it here is what stops a worker claiming another
// worker's capacity.
func (c *SchedulerClient) ClaimForWorker(ctx context.Context, workerID string, free *nexusv1.ResourceVector, queues []string, maxTasks int32) ([]*nexusv1.TaskAssignment, error) {
	if c.grpc == nil {
		// Returning NO WORK rather than an error is correct here and nowhere else
		// in this file: an empty poll is a normal protocol outcome, so an
		// unconfigured scheduler degrades to an idle fleet rather than a fleet
		// spinning on errors.
		return nil, nil
	}
	return c.grpc.claimForWorker(ctx, workerID, free, queues, maxTasks)
}

// MarkStarted transitions DISPATCHED -> RUNNING.
//
// startedAt is the worker's claim and is deliberately not forwarded: the
// scheduler stamps the transition with CONTROL-PLANE time, so a worker with a
// skewed clock cannot move its own deadlines (invariant 7).
func (c *SchedulerClient) MarkStarted(ctx context.Context, attemptID string, startedAt time.Time) error {
	if c.grpc == nil {
		return fmt.Errorf("mark started: %w (scheduler owns task_attempts)", ErrUpstreamNotConfigured)
	}
	return c.grpc.markStarted(ctx, attemptID)
}

// RecordProgress resets the task heartbeat deadline.
func (c *SchedulerClient) RecordProgress(ctx context.Context, attemptID, message string, fraction float64) error {
	if c.grpc == nil {
		return fmt.Errorf("record progress: %w (scheduler owns task_attempts)", ErrUpstreamNotConfigured)
	}
	return c.grpc.recordProgress(ctx, attemptID, message, fraction)
}

// ApplyResult applies a terminal outcome, then asks the DAG engine to recompute
// the run's frontier.
//
// The reconcile is what turns one finished task into the next one starting.
// Nothing else in the system computes the frontier — only the engine owns the
// graph — so without this call a run's first node succeeds and the run then sits
// forever.
//
// It runs AFTER the terminal transition has committed, and its failure does not
// fail the RPC. The attempt's outcome is already durable; refusing the worker's
// report because the engine was briefly unavailable would make the worker retry a
// report that already landed, and the engine's own reconcile is idempotent and
// re-runnable, so a missed call costs latency rather than correctness.
func (c *SchedulerClient) ApplyResult(ctx context.Context, req *nexusv1.ReportResultRequest) error {
	if c.grpc == nil {
		return fmt.Errorf("apply result: %w (scheduler owns task_attempts)", ErrUpstreamNotConfigured)
	}
	applied, err := c.grpc.applyResult(ctx, req)
	if err != nil {
		return err
	}
	if !applied.Applied || c.dag == nil {
		return nil
	}
	if err := c.dag.reconcileRun(ctx, applied.RunID, applied.NodeID); err != nil {
		c.log.ErrorContext(ctx, "attempt result committed but the run reconcile failed; the run will not advance until a later reconcile",
			"run_id", applied.RunID, "node_id", applied.NodeID,
			"attempt_id", req.GetAttemptId(), "err", err)
	}
	return nil
}

func (c *SchedulerClient) PendingCancellations(ctx context.Context, workerID string) ([]string, error) {
	if c.grpc == nil {
		// No directives rather than an error: a heartbeat must still deliver DRAIN
		// and complete its capacity reconciliation even when the scheduler is
		// unreachable.
		return nil, nil
	}
	return c.grpc.pendingCancellations(ctx, workerID)
}

// ── helpers ───────────────────────────────────────────────────────────────

// errNotWired marks a method whose peer gRPC service definition does not exist yet.
//
// A distinct, loud error rather than a silent no-op: a no-op here would look like
// success to the caller and quietly drop an operator's cancel or a worker's result.
func errNotWired(service, method string) error {
	return fmt.Errorf("%s.%s is not wired yet; the %s gRPC service definition is still pending", service, method, service)
}

// isNoRows distinguishes "this worker session does not exist" from a real fault.
//
// Compared against pgx's sentinel rather than the error string: a string match
// would break silently on a driver upgrade, and here a false negative would make
// the gateway treat a live worker as an unknown session and order it to re-register
// on every heartbeat.
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
