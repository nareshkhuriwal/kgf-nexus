// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// WorkerClaimRequest is a polling worker's offer of capacity.
//
// FreeCapacity is the worker's own report and is treated as an UPPER BOUND that
// narrows placement, never as a source of truth that widens it: the authoritative
// ledger is workers.capacity minus workers.committed, held under the row lock in
// the claim transaction. A worker that over-reported free capacity therefore
// cannot talk the scheduler into over-committing it; it can only fail to be given
// work it could actually have run.
type WorkerClaimRequest struct {
	WorkerID uuid.UUID
	// Queues the worker is willing to serve on this poll. Intersected with the
	// queue this scheduler replica drains.
	Queues       []string
	FreeCapacity *nexusv1.ResourceVector
	MaxTasks     int32
}

// WorkerAssignment is one dispatched attempt as the worker needs to see it.
//
// It carries the idempotency key read from the attempt row rather than a
// recomputed one: the key is derived at attempt CREATION without attempt_number
// (invariant 1), and re-deriving it here would create a second source of truth
// for the value that all at-least-once safety rests on.
type WorkerAssignment struct {
	AttemptID      uuid.UUID
	RunID          uuid.UUID
	NodeID         string
	AttemptNumber  int32
	IdempotencyKey string
	LeaseToken     string
	// LeaseExpiresAt and StartToCloseDeadline are ABSOLUTE control-plane times
	// (invariant 7). The worker is handed instants, not durations, so a skewed
	// worker clock cannot extend its own budget.
	LeaseExpiresAt       time.Time
	StartToCloseDeadline time.Time
	// SpecJSON is task_nodes.spec_json verbatim. The scheduler does not reshape
	// it: the DAG engine owns that column and the agent parses it, so any
	// translation here would be a third dialect to keep in sync.
	SpecJSON []byte
	// InputRefsJSON is task_attempts.input_refs verbatim: the attempt's resolved
	// inputs as the DAG engine wrote them.
	InputRefsJSON []byte

	// claimedRequest is the resource request that was just committed, used only
	// to decrement the local capacity budget within one multi-task poll. Not
	// exported: the authoritative ledger is workers.committed, and exposing this
	// would invite a caller to do capacity arithmetic outside the transaction.
	claimedRequest *nexusv1.ResourceVector
}

// pollWorker is a row-locked polling worker, with the matching attributes the
// candidate query needs pushed alongside the capacity vectors.
//
// A local type rather than extra fields on WorkerCandidate: selectWorker's
// direction (one attempt, many workers) does its label and executor matching in
// SQL and never needs them in Go, and widening the shared struct for one caller
// would leave two zero-valued fields that mean "not loaded" everywhere else.
type pollWorker struct {
	WorkerCandidate
	Executors []string
	Labels    map[string]string
}

// ClaimForWorker runs the §5.3 atomic claim PINNED to one worker, for a worker
// that is polling for work.
//
// It differs from Claim in exactly one respect: worker selection is fixed rather
// than searched. Everything else — the candidate scan with FOR UPDATE SKIP
// LOCKED, the fit check against the authoritative ledger under the worker's row
// lock, the concurrency semaphore, and the single transaction committing
// reservation + workers.committed + lease + attempt CAS + run_event + timers —
// is the SAME code path as the background dispatch loop, because two
// implementations of the no-double-dispatch guarantee is one too many.
//
// An empty slice with a nil error is the normal steady state, not a fault: most
// polls from a healthy fleet find nothing to do.
//
// Each assignment is its own transaction. Batching N dispatches into one
// transaction would hold this worker's row lock across N candidate scans and
// stall every other replica trying to place work on the same worker, and a
// failure on the Nth would roll back N-1 perfectly good dispatches.
func (s *SchedulerStore) ClaimForWorker(
	ctx context.Context, now time.Time, req WorkerClaimRequest,
) ([]WorkerAssignment, error) {
	if req.WorkerID == uuid.Nil {
		return nil, fmt.Errorf("claim for worker: worker_id is required")
	}
	// A worker that does not serve this replica's queue gets nothing rather than
	// an error: with several replicas each draining one queue, "not my queue" is
	// a routine answer that every poll gets from most replicas.
	if len(req.Queues) > 0 && !containsString(req.Queues, s.cfg.Queue) {
		return nil, nil
	}

	maxTasks := int(req.MaxTasks)
	if maxTasks <= 0 {
		maxTasks = 1
	}

	// Free capacity is tracked locally across the batch so a second assignment
	// in the same poll cannot be handed capacity the first one just consumed.
	// The ledger in Postgres enforces this too; doing it here as well avoids
	// burning a transaction on a candidate we already know will not fit.
	remaining := req.FreeCapacity

	var out []WorkerAssignment
	for len(out) < maxTasks {
		asg, err := s.claimOneForWorker(ctx, now, req.WorkerID, remaining)
		if err != nil {
			// Assignments already committed are returned alongside the error:
			// they are real dispatches with real leases, and dropping them on
			// the floor would leak capacity until the lease expired.
			return out, err
		}
		if asg == nil {
			break
		}
		out = append(out, *asg)
		if remaining != nil {
			remaining = domain.SubVec(remaining, asg.claimedRequest)
		}
	}
	return out, nil
}

// claimOneForWorker is one pinned claim transaction. A nil assignment with a nil
// error means nothing dispatchable fits this worker right now.
func (s *SchedulerStore) claimOneForWorker(
	ctx context.Context, now time.Time, workerID uuid.UUID, free *nexusv1.ResourceVector,
) (*WorkerAssignment, error) {
	// READ COMMITTED for the same reason Claim uses it (see the comment there):
	// SKIP LOCKED must skip rows a peer already moved, not abort the transaction
	// with SQLSTATE 40001. Safety rests on the row locks, the state='PENDING'
	// CAS and UNIQUE(attempt_id) on leases, none of which need snapshot
	// isolation.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin worker claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the worker row FIRST, before scanning candidates. Locking in a fixed
	// order (worker, then attempt) across every pinned claim is what keeps two
	// workers polling concurrently from deadlocking against each other.
	worker, err := s.lockWorkerForClaim(ctx, tx, now, workerID)
	if err != nil {
		return nil, err
	}
	if worker == nil {
		// Not ACTIVE, not on this queue, heartbeat stale, or being placed by a
		// peer right now. No work, no error: a DRAINING worker polling is
		// expected behaviour, not a fault.
		return nil, nil
	}

	// The authoritative free capacity is the ledger's, intersected with what the
	// worker says it has free. MinVec is the intersection: whichever side is
	// more pessimistic wins, so neither an over-reporting worker nor a stale
	// ledger can cause an over-commit.
	budget := domain.SubVec(worker.Capacity, worker.Committed)
	if free != nil {
		budget = minVec(budget, free)
	}

	lookAhead := s.cfg.LookAhead
	if lookAhead < 1 {
		lookAhead = 1
	}

	var skipped []uuid.UUID
	for i := 0; i < lookAhead; i++ {
		cand, err := s.selectCandidateForWorker(ctx, tx, now, worker, budget, skipped)
		if errors.Is(err, ErrNoCandidate) {
			// Nothing this worker can take. Before giving up, explain any attempt
			// the SQL pre-filter silently excluded.
			//
			// The pre-filter is only an optimisation, but it runs BEFORE the
			// domain.Fits call that records a reason -- so an attempt that can
			// never fit the fleet (a 100GiB disk request against 50GiB workers)
			// is skipped without explanation and sits PENDING forever with an
			// empty pending_reason. That is precisely the unexplained stall §3.2
			// says this field exists to prevent: the operator sees a stuck run,
			// an idle fleet, and no reason connecting the two.
			if xerr := s.explainUnschedulable(ctx, tx, worker, skipped); xerr != nil {
				return nil, xerr
			}
			// COMMIT, even though nothing was dispatched. explainUnschedulable
			// writes pending_reason, and the deferred Rollback would discard
			// every reason it just recorded -- which is exactly the idle-fleet
			// case the field exists for. The success path below commits for the
			// same reason; this path was the one that did not, so a stalled queue
			// with no claims to make stayed permanently unexplained.
			if err := tx.Commit(ctx); err != nil {
				return nil, fmt.Errorf("commit unschedulable explanation: %w", err)
			}
			return nil, nil
		}
		if err != nil {
			return nil, err
		}

		// §5.3 step 4: the named global semaphore, checked inside the same
		// transaction as the dispatch so two schedulers cannot both see limit-1
		// held and both admit.
		if key := cand.Spec.ConcurrencyKey; key != "" && cand.Spec.ConcurrencyLimit > 0 {
			ok, held, err := s.concurrencyAdmit(ctx, tx, key, cand.Spec.ConcurrencyLimit)
			if err != nil {
				return nil, err
			}
			if !ok {
				reason := fmt.Sprintf("waiting_for_concurrency: %s (held %d, limit %d)",
					key, held, cand.Spec.ConcurrencyLimit)
				if err := s.markBlocked(ctx, tx, cand.ID, reason); err != nil {
					return nil, err
				}
				// Look past it: another attempt on a different key may fit.
				skipped = append(skipped, cand.ID)
				continue
			}
		}

		// §5.3 step 5, unchanged and shared with the background loop: this is
		// THE claim transaction.
		dispatch, ok, err := s.commitDispatch(ctx, tx, now, cand, &worker.WorkerCandidate)
		if err != nil {
			return nil, err
		}
		if !ok {
			// The CAS guard fired: a peer replica dispatched this attempt
			// between our SELECT and our UPDATE. Not an error — the guard
			// working — so look past it within this transaction's look-ahead.
			skipped = append(skipped, cand.ID)
			continue
		}

		idemKey, specJSON, inputRefs, err := s.assignmentDetails(ctx, tx, cand)
		if err != nil {
			return nil, err
		}

		// Explain the leftovers before committing.
		//
		// This must happen on the SUCCESS path too, not only when nothing could
		// be claimed. On a busy queue a poll almost always finds something, so an
		// attempt the SQL pre-filter permanently excludes -- a 100GiB disk request
		// against 50GiB workers -- would otherwise never be reached and would sit
		// PENDING forever with an empty pending_reason while work flowed around
		// it. An unschedulable attempt on a busy queue is exactly the case an
		// operator most needs explained, because the fleet looks healthy.
		if xerr := s.explainUnschedulable(ctx, tx, worker, append(skipped, cand.ID)); xerr != nil {
			return nil, xerr
		}

		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit worker claim tx: %w", err)
		}

		asg := &WorkerAssignment{
			AttemptID:      dispatch.AttemptID,
			RunID:          dispatch.RunID,
			NodeID:         dispatch.NodeID,
			AttemptNumber:  cand.AttemptNumber,
			IdempotencyKey: idemKey,
			LeaseToken:     dispatch.LeaseToken,
			LeaseExpiresAt: dispatch.ExpiresAt,
			SpecJSON:       specJSON,
			InputRefsJSON:  inputRefs,
			claimedRequest: cand.ResourceRequest,
		}
		// The start_to_close deadline is an absolute instant derived from
		// control-plane `now`, matching the timer row committed above, so the
		// worker and the timer agree on the same moment (invariant 7).
		if ms := cand.Spec.StartToCloseMs; ms > 0 {
			asg.StartToCloseDeadline = now.Add(time.Duration(ms) * time.Millisecond)
		}
		return asg, nil
	}
	return nil, nil
}

// lockWorkerForClaim row-locks the polling worker and returns it only if it is
// actually placeable. Returns nil when it is not.
//
// The FOR UPDATE is the over-commit guard: capacity spending for one worker is
// serialized on its row, so the background dispatch loop and a concurrent poll
// cannot both spend the same GPU.
func (s *SchedulerStore) lockWorkerForClaim(
	ctx context.Context, tx pgx.Tx, now time.Time, workerID uuid.UUID,
) (*pollWorker, error) {
	// DRAINING and QUARANTINED are excluded here exactly as in selectWorker: a
	// draining worker finishes what it holds and is given nothing new (§5.5).
	//
	// The heartbeat-liveness predicate is kept even though the worker is
	// evidently alive enough to be polling, because liveness is what the rest of
	// the system uses to decide whether this worker's leases are trustworthy. A
	// worker whose heartbeats are not landing may be about to have its leases
	// expired and its work retried elsewhere, and handing it MORE work in that
	// state manufactures the duplicate execution the lease design exists to
	// prevent.
	const q = `
SELECT id, hostname, capacity, committed, executors, labels
  FROM workers
 WHERE id = $1
   AND state = 'ACTIVE'
   AND $2 = ANY(queues)
   AND last_heartbeat_at > $3
 FOR UPDATE SKIP LOCKED`

	var (
		w                WorkerCandidate
		capJSON, comJSON []byte
		labelsJSON       []byte
		executors        []string
	)
	err := tx.QueryRow(ctx, q, workerID, s.cfg.Queue, now.Add(-s.cfg.LivenessWindow)).
		Scan(&w.ID, &w.Hostname, &capJSON, &comJSON, &executors, &labelsJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock worker %s for claim: %w", workerID, err)
	}

	if w.Capacity, err = decodeVector(capJSON); err != nil {
		return nil, fmt.Errorf("decode capacity for worker %s: %w", workerID, err)
	}
	if w.Committed, err = decodeVector(comJSON); err != nil {
		return nil, fmt.Errorf("decode committed for worker %s: %w", workerID, err)
	}
	out := &pollWorker{WorkerCandidate: w, Executors: executors, Labels: map[string]string{}}
	if len(labelsJSON) > 0 {
		// A worker whose labels we cannot parse is treated as having none, which
		// makes it eligible only for work with no node_selector. Failing the
		// poll instead would take the worker out of service over a cosmetic
		// column problem.
		_ = json.Unmarshal(labelsJSON, &out.Labels)
	}
	return out, nil
}

// selectCandidateForWorker is §5.3 step 1 narrowed to what THIS worker can run.
//
// The executor, node_selector and resource predicates are pushed into SQL here
// rather than filtered in Go as selectWorker does, because the direction of the
// join is reversed: selectWorker has one attempt and scans workers, so the Go
// loop is bounded by fleet size, while this has one worker and scans attempts,
// where the queue may be arbitrarily long. Filtering in Go would mean paging
// through thousands of unplaceable PENDING rows on every poll.
//
// The resource predicate is a conservative PRE-FILTER, not the fit decision:
// domain.Fits remains the single authority on fit (GPU-kind matching, per-device
// VRAM floors, the custom map), and it is applied in Go below on the narrowed
// set. SQL only discards rows that obviously cannot fit.
func (s *SchedulerStore) selectCandidateForWorker(
	ctx context.Context, tx pgx.Tx, now time.Time,
	worker *pollWorker, budget *nexusv1.ResourceVector, skip []uuid.UUID,
) (*Candidate, error) {
	if skip == nil {
		skip = []uuid.UUID{}
	}
	executors := worker.Executors
	if executors == nil {
		executors = []string{}
	}
	labelsJSON, err := json.Marshal(worker.Labels)
	if err != nil {
		return nil, fmt.Errorf("marshal worker labels: %w", err)
	}

	// Aging is in the ORDER BY for the same reason as in selectCandidate: a
	// background UPDATE-based ager would rewrite every PENDING row every tick,
	// and it would destroy the author's declared priority as displayed data.
	//
	// r.state IN ('PENDING','RUNNING') is §5.2: a PAUSING/PAUSED/CANCELLING run
	// receives no NEW dispatch.
	const q = `
SELECT a.id, a.run_id, a.node_id, a.attempt_number, a.queue, a.priority,
       a.resource_request, a.scheduled_at,
       a.priority + (EXTRACT(EPOCH FROM ($2::timestamptz - a.scheduled_at)) * $3) AS effective_priority,
       n.spec_json
  FROM task_attempts a
  JOIN task_nodes    n ON n.run_id = a.run_id AND n.node_id = a.node_id
  JOIN workflow_runs r ON r.id = a.run_id
 WHERE a.state = 'PENDING'
   AND a.queue = $1
   AND a.scheduled_at <= $2
   AND r.state IN ('PENDING','RUNNING')
   AND NOT (a.id = ANY($4::uuid[]))
   -- Executor compatibility. Both spellings of the column are checked because
   -- task_nodes.spec_json has two producers: the DAG engine writes protojson
   -- (nested, lowerCamelCase) while the scheduler's own fixtures write a flat
   -- snake_case form. Checking one dialect would silently match every worker
   -- for documents in the other, which is worse than not filtering at all --
   -- it would place a docker task on a subprocess-only worker.
   --
   -- An absent executor means "any", so the COALESCE is load-bearing: a spec
   -- that omits the field must match every worker rather than none.
   AND (COALESCE(n.spec_json #>> '{runtime,executor}',
                 n.spec_json #>> '{executor}', '') = ''
        OR COALESCE(n.spec_json #>> '{runtime,executor}',
                    n.spec_json #>> '{executor}') = ANY($5::text[]))
   -- Hard label constraints (§5.3 step 2), as jsonb containment in the same
   -- direction as selectWorker: the WORKER's labels must contain the spec's
   -- node_selector. Both dialects again.
   AND ($6::jsonb @> COALESCE(n.spec_json -> 'nodeSelector',
                              n.spec_json -> 'node_selector', '{}'::jsonb))
   -- Conservative resource pre-filter on the scalar dimensions. domain.Fits
   -- below is the authority; this only avoids paging through rows that cannot
   -- possibly fit.
   AND COALESCE((a.resource_request ->> 'cpu_millis')::bigint, 0)    <= $7
   AND COALESCE((a.resource_request ->> 'memory_bytes')::bigint, 0)  <= $8
   AND COALESCE((a.resource_request ->> 'gpu_count')::bigint, 0)     <= $9
   AND COALESCE((a.resource_request ->> 'disk_bytes')::bigint, 0)    <= $10
 ORDER BY effective_priority DESC, a.scheduled_at ASC
 LIMIT 1
 FOR UPDATE OF a SKIP LOCKED`

	row := tx.QueryRow(ctx, q,
		s.cfg.Queue, now, s.cfg.AgingRate, skip, executors, labelsJSON,
		budget.GetCpuMillis(), budget.GetMemoryBytes(),
		int64(budget.GetGpuCount()), budget.GetDiskBytes())

	var c Candidate
	var reqJSON, specJSON []byte
	if err := row.Scan(
		&c.ID, &c.RunID, &c.NodeID, &c.AttemptNumber, &c.Queue, &c.Priority,
		&reqJSON, &c.ScheduledAt, &c.EffectivePriority, &specJSON,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoCandidate
		}
		return nil, fmt.Errorf("select pending attempt for worker %s: %w", worker.ID, err)
	}

	if c.ResourceRequest, err = decodeVector(reqJSON); err != nil {
		return nil, fmt.Errorf("decode resource_request for attempt %s: %w", c.ID, err)
	}

	var spec TaskSpecView
	if len(specJSON) > 0 {
		// A spec we cannot decode leaves the attempt PENDING with platform
		// defaults rather than wedging this worker's polls.
		_ = json.Unmarshal(specJSON, &spec)
	}
	c.Spec = &spec

	// The authoritative fit check, against the LEDGER rather than the worker's
	// self-report, using the same domain.Fits every unit test uses. The SQL
	// pre-filter above is an optimisation; this is the decision.
	if fits, short := domain.Fits(worker.Capacity, worker.Committed, c.ResourceRequest); !fits {
		reason := domain.ShortfallReason(short)
		if reason == "" {
			reason = "waiting_for_resources: does not fit the polling worker"
		}
		if err := s.markBlocked(ctx, tx, c.ID, reason); err != nil {
			return nil, err
		}
		return nil, ErrNoCandidate
	}
	return &c, nil
}

// assignmentDetails reads the per-attempt facts the worker needs that the claim
// transaction does not itself produce.
//
// The idempotency key is READ, never derived here. It is written once at attempt
// creation from sha256(run_id ‖ node_id ‖ map_index ‖ task_version) with
// attempt_number deliberately excluded (invariant 1); recomputing it in the
// dispatch path would give the platform two places that must agree about the
// value every at-least-once guarantee depends on.
func (s *SchedulerStore) assignmentDetails(
	ctx context.Context, tx pgx.Tx, cand *Candidate,
) (idempotencyKey string, specJSON, inputRefs []byte, err error) {
	err = tx.QueryRow(ctx, `
SELECT a.idempotency_key, n.spec_json::text, a.input_refs::text
  FROM task_attempts a
  JOIN task_nodes n ON n.run_id = a.run_id AND n.node_id = a.node_id
 WHERE a.id = $1`, cand.ID).Scan(&idempotencyKey, &specJSON, &inputRefs)
	if err != nil {
		return "", nil, nil, fmt.Errorf("load assignment details for attempt %s: %w", cand.ID, err)
	}
	return idempotencyKey, specJSON, inputRefs, nil
}

// PendingCancellations lists the attempts this worker holds that the control
// plane wants stopped, for delivery as heartbeat directives (ADR-0005).
//
// Two sources, unioned, because the cancel signal arrives by two different
// routes and a worker that only heard one of them would keep burning CPU:
//
//   - the ATTEMPT is already CANCELLED — the scheduler moved it terminal (an
//     operator cancelled the node, or the run's cancel fanned out), and the work
//     on the worker is now orphaned; and
//   - the RUN is CANCELLING or PAUSING per §5.2 — the run-level transition has
//     been recorded but the in-flight attempts have not yet been individually
//     moved, which is precisely the window in which the directive must be sent.
//
// PAUSING is deliberately NOT included, and that is the whole point of pause.
//
// It used to be, on the reasoning that a paused run's in-flight attempts must
// drain and CANCEL_ATTEMPT is the only channel a pull-only fleet has. But the
// gateway turns every id in this list into a CANCEL_ATTEMPT directive
// unconditionally, and the agent implements that directive as SIGTERM, grace,
// SIGKILL. So pausing a run KILLED the very work §5.2 promises to preserve: the
// attempt came back FAILED with "workload terminated by signal terminated", the
// retry budget was charged for a failure the task never caused, and an operator
// who paused a run to investigate destroyed exactly the in-flight compute they
// were trying to protect. Verified against a live run before this was changed.
//
// Draining needs no directive at all. PAUSING already closes the dispatch gate
// (the claim query below admits only runs in PENDING/RUNNING), so no NEW attempt
// starts; the in-flight ones finish on their own and MaybeFinalizePause advances
// PAUSING -> PAUSED when the last of them reports. "Drain" here means "stop
// feeding it", not "stop it" — and cancel remains the operation that stops work.
func (s *SchedulerStore) PendingCancellations(
	ctx context.Context, workerID uuid.UUID,
) ([]uuid.UUID, error) {
	const q = `
SELECT a.id
  FROM task_attempts a
  JOIN workflow_runs r ON r.id = a.run_id
 WHERE a.worker_id = $1
   AND (a.state = 'CANCELLED'
        OR (a.state IN ('DISPATCHED','RUNNING')
            AND r.state IN ('CANCELLING','CANCELLED')))
 ORDER BY a.dispatched_at NULLS LAST`
	rows, err := s.pool.Query(ctx, q, workerID)
	if err != nil {
		return nil, fmt.Errorf("select pending cancellations for worker %s: %w", workerID, err)
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan pending cancellation: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// minVec is the pessimistic intersection of two capacity vectors.
//
// Used to reconcile the worker's self-reported free capacity with the ledger's:
// taking the minimum per dimension means neither an over-reporting worker nor a
// stale ledger entry can produce an over-commit, and the worst outcome is that
// the worker is briefly given less work than it could have run.
func minVec(a, b *nexusv1.ResourceVector) *nexusv1.ResourceVector {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := &nexusv1.ResourceVector{
		CpuMillis:      minI64(a.GetCpuMillis(), b.GetCpuMillis()),
		MemoryBytes:    minI64(a.GetMemoryBytes(), b.GetMemoryBytes()),
		GpuCount:       int32(minI64(int64(a.GetGpuCount()), int64(b.GetGpuCount()))),
		GpuMemoryBytes: minI64(a.GetGpuMemoryBytes(), b.GetGpuMemoryBytes()),
		DiskBytes:      minI64(a.GetDiskBytes(), b.GetDiskBytes()),
		// GPU kind is an identity, not a quantity, so it is taken from the
		// LEDGER side: what a worker actually has is a hardware fact the worker
		// does not get to renegotiate per poll.
		GpuKind: a.GetGpuKind(),
	}
	if len(a.GetCustom()) > 0 || len(b.GetCustom()) > 0 {
		out.Custom = map[string]int64{}
		for k, v := range a.GetCustom() {
			out.Custom[k] = v
		}
		for k, v := range b.GetCustom() {
			if cur, ok := out.Custom[k]; !ok || v < cur {
				out.Custom[k] = v
			}
		}
	}
	return out
}

func minI64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// explainUnschedulable records why PENDING attempts on this queue could not be
// placed on this worker, for attempts the SQL pre-filter excluded before the
// authoritative fit check could record a reason.
//
// Deliberately best-effort and bounded: it is diagnostics, not scheduling, so it
// must never fail a claim or scan the whole backlog. An attempt that already
// carries a reason is left alone -- the first explanation is usually the most
// specific, and rewriting it on every poll would churn the row for no gain.
func (s *SchedulerStore) explainUnschedulable(
	ctx context.Context, tx pgx.Tx, worker *pollWorker, skip []uuid.UUID,
) error {
	// A nil slice reaches Postgres as SQL NULL, and `x <> ALL(NULL)` /
	// `NOT (x = ANY(NULL))` evaluate to NULL rather than true -- so every row is
	// filtered out and nothing is ever explained. Forcing a non-nil empty slice
	// is the fix. (The resource manager's leak detector had the identical bug;
	// it is an easy one to write twice.)
	if skip == nil {
		skip = []uuid.UUID{}
	}
	rows, err := tx.Query(ctx, `
SELECT a.id, a.resource_request
  FROM task_attempts a
 WHERE a.state = 'PENDING'
   AND a.queue = $1
   AND (a.pending_reason IS NULL OR a.pending_reason = '')
   AND NOT (a.id = ANY($2::uuid[]))
 ORDER BY a.scheduled_at
 LIMIT 20`, s.cfg.Queue, skip)
	if err != nil {
		return fmt.Errorf("explain unschedulable: %w", err)
	}
	type pend struct {
		id  uuid.UUID
		req *nexusv1.ResourceVector
	}
	var pending []pend
	for rows.Next() {
		var id uuid.UUID
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return fmt.Errorf("explain unschedulable scan: %w", err)
		}
		vec, derr := decodeVector(raw)
		if derr != nil {
			continue
		}
		pending = append(pending, pend{id: id, req: vec})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("explain unschedulable rows: %w", err)
	}

	for _, pnd := range pending {
		fits, short := domain.Fits(worker.Capacity, worker.Committed, pnd.req)
		if fits {
			// It fits this worker but was not claimed this pass -- look-ahead or
			// a concurrency skip. Not a shortfall, so say nothing.
			continue
		}
		reason := domain.ShortfallReason(short)
		if reason == "" {
			continue
		}
		if err := s.markBlocked(ctx, tx, pnd.id, reason); err != nil {
			return err
		}
	}
	return nil
}
