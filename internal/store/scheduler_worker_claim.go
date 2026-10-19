// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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
	// TraceContext is task_attempts.trace_context (migration 0028): the W3C
	// traceparent of whatever made the attempt ready, or "" when untraced.
	// The scheduler parents its dispatch span on it (ADR-0026).
	TraceContext string

	// claimedRequest is the resource request that was just committed, used only
	// to decrement the local capacity budget within one multi-task poll. Not
	// exported: the authoritative ledger is workers.committed, and exposing this
	// would invite a caller to do capacity arithmetic outside the transaction.
	claimedRequest *nexusv1.ResourceVector
	// Reserved is the vector committed for this attempt (the request plus the
	// shared GPU device it was placed on). Sent to the agent as
	// TaskAssignment.reserved, so it uses the same device (ADR-0029 §2).
	Reserved *nexusv1.ResourceVector
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
	// facts are the worker's placement facts: effective labels (own overlaid
	// with its cluster's), cached artifacts, and cluster quota headroom.
	facts workerFacts
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

	// The namespace quota lock comes before EVERYTHING, including the worker
	// row (ADR-0021): this is the one place the claim waits for it, and a
	// waiter that holds no other lock cannot be part of a deadlock. nil means
	// the namespace has no concurrent limits and nothing was locked.
	nq, err := s.lockQuotaForWorker(ctx, tx, workerID)
	if err != nil {
		return nil, err
	}
	var quotaBlocked []string
	// commitNoWork commits the reasons this pass recorded and returns "nothing
	// for this worker". Shared by the two ways a pass ends empty.
	commitNoWork := func(what string) (*WorkerAssignment, error) {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit %s: %w", what, err)
		}
		s.notifyQuotaBlocked(quotaBlocked)
		return nil, nil
	}

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
	// The deferral's peer read happens at most once per transaction.
	var peers peerSet
	for i := 0; i < lookAhead; i++ {
		worker.Placed = nil
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
			return commitNoWork("unschedulable explanation")
		}
		if err != nil {
			return nil, err
		}

		// Placement (ADR-0029 §4, ADR-0030, ADR-0031). The hard constraints are
		// already SQL pre-filter clauses; this Go check is the belt to those
		// braces, so the two can never disagree in the direction that places a
		// task where it must not run.
		if ok, _ := domain.Eligible(worker.facts.Labels, cand.Spec.Placement); !ok {
			skipped = append(skipped, cand.ID)
			continue
		}
		// A cluster known to be out of quota for this pod is skipped for this
		// worker, not waited on: another cluster may have room.
		if reason, err := s.headroomBlocks(ctx, tx, now, worker.ID, &worker.facts, cand,
			domain.EffectiveExecutor(cand.Spec.Executor, worker.Labels)); err != nil {
			return nil, err
		} else if reason != "" {
			if err := s.markBlocked(ctx, tx, cand.ID, reason); err != nil {
				return nil, err
			}
			skipped = append(skipped, cand.ID)
			continue
		}
		// Bounded delay scheduling: leave it for a strictly better live worker.
		if deferIt, err := s.shouldDefer(ctx, tx, now, worker, cand, &peers); err != nil {
			return nil, err
		} else if deferIt {
			s.notifyDeferred()
			skipped = append(skipped, cand.ID)
			continue
		}

		// §5.3 step 4: the named global semaphore, checked inside the same
		// transaction as the dispatch so two schedulers cannot both see limit-1
		// held and both admit.
		if key := cand.Spec.ConcurrencyKey; key != "" && cand.Spec.ConcurrencyLimit > 0 {
			ok, held, err := s.concurrencyAdmit(ctx, tx, key, cand.Spec.ConcurrencyLimit, cand.ID)
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

		// §5.3 step 3: the namespace quota, checked under the lock taken at the
		// top of this transaction, so no concurrent claim of this namespace can
		// pass the same check against the same usage.
		if nq != nil {
			v, err := s.quotaViolation(ctx, tx, nq, cand)
			if err != nil {
				return nil, err
			}
			if v != nil {
				reason := v.Reason()
				if err := s.markBlocked(ctx, tx, cand.ID, reason); err != nil {
					return nil, err
				}
				quotaBlocked = append(quotaBlocked, v.Dimension)
				if v.Exhausted {
					// No headroom at all on this dimension: every other attempt
					// of this worker's namespace that uses it would get the same
					// answer, so end the pass instead of spending the look-ahead.
					return commitNoWork("quota explanation")
				}
				// Headroom exists but not for this one; a smaller attempt may fit.
				skipped = append(skipped, cand.ID)
				continue
			}
		}

		// The placed vector: the request plus the shared GPU device chosen for
		// it, under this worker's row lock. finishWorkerCandidate's Fits
		// already passed, so a refusal here is impossible short of a ledger
		// bug; it is still handled as a shortfall, never as an over-commit.
		if short := s.placeOn(&worker.WorkerCandidate, cand.ResourceRequest); short != nil {
			if err := s.markBlocked(ctx, tx, cand.ID, domain.ShortfallReason(short)); err != nil {
				return nil, err
			}
			skipped = append(skipped, cand.ID)
			continue
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

		idemKey, specJSON, inputRefs, traceCtx, err := s.assignmentDetails(ctx, tx, cand)
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
		s.notifyQuotaBlocked(quotaBlocked)

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
			TraceContext:   traceCtx,
			claimedRequest: cand.ResourceRequest,
			Reserved:       worker.Placed,
		}
		// The start_to_close deadline is an absolute instant derived from
		// control-plane `now`, matching the timer row committed above, so the
		// worker and the timer agree on the same moment (invariant 7).
		if ms := cand.Spec.StartToCloseMs; ms > 0 {
			asg.StartToCloseDeadline = now.Add(time.Duration(ms) * time.Millisecond)
		}
		return asg, nil
	}
	// The look-ahead ran out with every candidate held back by the semaphore or
	// a quota. Commit so the reasons just recorded are kept (the deferred
	// Rollback would discard them and leave the attempts unexplained).
	return commitNoWork("look-ahead explanation")
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
	// The placement facts (effective labels, cached artifacts, cluster
	// headroom) ride on the same row read: correlated lookups by the cluster's
	// unique name, so a worker without a cluster label costs two index probes
	// that find nothing.
	q := `
SELECT w.id, w.hostname, w.capacity, w.committed, w.executors, w.labels, ` + s.placementCols() + `
  FROM workers w` + s.placementJoin() + `
 WHERE w.id = $1
   AND w.state = 'ACTIVE'
   AND $2 = ANY(w.queues)
   AND w.last_heartbeat_at > $3
 FOR UPDATE OF w SKIP LOCKED`

	var (
		w                           WorkerCandidate
		capJSON, comJSON            []byte
		labelsJSON, factLabels      []byte
		executors, cached           []string
		clusterLabels, headroomJSON []byte
		headroomAt                  *time.Time
	)
	err := tx.QueryRow(ctx, q, workerID, s.cfg.Queue, now.Add(-s.cfg.LivenessWindow)).
		Scan(&w.ID, &w.Hostname, &capJSON, &comJSON, &executors, &labelsJSON,
			&factLabels, &cached, &clusterLabels, &headroomJSON, &headroomAt)
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
	if s.placementMissing {
		factLabels = labelsJSON
	}
	out.facts = scanFacts(factLabels, cached, clusterLabels, headroomJSON, headroomAt)
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
	effJSON, err := json.Marshal(worker.facts.Labels)
	if err != nil {
		return nil, fmt.Errorf("marshal effective labels: %w", err)
	}

	// A task that names no executor runs on this worker's default; a
	// container default (docker, kubernetes) can run it only if it carries an
	// image, and kubernetes only without stdout_output (domain.UnnamedRefusal).
	def := domain.WorkerDefaultExecutor(worker.Labels)
	unnamedNeedsImage := domain.ExecutorRunsImage(def)
	unnamedNoStdout := domain.ExecutorRejectsStdoutOutput(def)

	args := []any{
		s.cfg.Queue, now, s.cfg.AgingRate, skip, executors, labelsJSON,
		budget.GetCpuMillis(), budget.GetMemoryBytes(),
		int64(budget.GetGpuCount()), budget.GetDiskBytes(), worker.ID,
		effJSON, domain.IsSpot(worker.facts.Labels), domain.CostTier(worker.facts.Labels),
		unnamedNeedsImage, unnamedNoStdout,
	}

	if s.windowed && s.cfg.ClaimWindow > 0 {
		cand, err, decided := s.selectCandidateWindowed(ctx, tx, now, worker, skip, args)
		if decided {
			return cand, err
		}
		// Every window this claim may read was full and held nothing this
		// worker can run -- say a CPU worker behind thousands of GPU tasks.
		// Paging on would read the backlog 64 rows at a time, strictly worse
		// than one scan, so fall through to the exhaustive query: the worst
		// case is today's cost plus a few cheap window reads, never more.
	}

	row := tx.QueryRow(ctx,
		workerCandidateSQL("", "effective_priority DESC, a.scheduled_at ASC"), args...)
	return s.finishWorkerCandidate(ctx, tx, worker, row)
}

// workerCandidateSQL is the pinned claim's candidate query: §5.3 step 1
// narrowed to what one polling worker can run.
//
// ONE template serves both the exhaustive scan and the windowed lock step, so
// the eligibility rules -- run state, executor, labels, the resource
// pre-filter -- exist exactly once. Two copies of them would be two
// definitions of "dispatchable" that drift, and the windowed claim would then
// quietly pick a different attempt than the scan it replaces.
//
// Parameters, shared by both callers: $1 queue, $2 now, $3 aging rate,
// $4 skip ids, $5 executors, $6 worker labels, $7-$10 budget, $11 worker id,
// $12 effective labels (own + cluster), $13 worker is spot, $14 cost tier,
// $15 the worker's default executor needs an image, $16 it refuses
// stdout_output. The windowed caller adds $17, the window's ids.
//
// Aging is in the ORDER BY for the same reason as in selectCandidate: a
// background UPDATE-based ager would rewrite every PENDING row every tick, and
// it would destroy the author's declared priority as displayed data.
//
// r.state IN ('PENDING','RUNNING') is §5.2: a PAUSING/PAUSED/CANCELLING run
// receives no NEW dispatch.
func workerCandidateSQL(extraWhere, orderBy string) string {
	return `
SELECT a.id, a.run_id, a.node_id, a.attempt_number, a.queue, a.priority,
       a.resource_request, a.scheduled_at,
       a.priority + (EXTRACT(EPOCH FROM ($2::timestamptz - a.scheduled_at)) * $3) AS effective_priority,
       n.spec_json
  FROM task_attempts a
  JOIN task_nodes    n ON n.run_id = a.run_id AND n.node_id = a.node_id
  JOIN workflow_runs r ON r.id = a.run_id
 WHERE a.state = 'PENDING'
   AND a.queue = $1
   AND a.tenant_id = (SELECT tenant_id FROM workers WHERE id=$11)
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
   -- An absent executor means "this worker's default" (nexus.io/default-
   -- executor; subprocess for a worker that predates it), so the COALESCE is
   -- load-bearing: a spec that omits the field must match every worker whose
   -- default can run it rather than none (domain.UnnamedRefusal). A container
   -- default ($15) needs the task to carry an image; kubernetes ($16) also
   -- refuses stdout_output. Both read in every dialect, like the executor.
   AND ((COALESCE(n.spec_json #>> '{runtime,executor}',
                  n.spec_json #>> '{executor}', '') = ''
         AND (NOT $15::bool
              OR COALESCE(n.spec_json #>> '{runtime,image}',
                          n.spec_json #>> '{image}', '') <> '')
         AND (NOT $16::bool
              OR COALESCE(n.spec_json #>> '{runtime,stdout_output}',
                          n.spec_json #>> '{runtime,stdoutOutput}',
                          n.spec_json #>> '{stdout_output}', '') = ''))
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
   -- GPU model and per-device memory: what THIS worker's GPUs are, not what
   -- is free right now. A task the worker can never run must not be its
   -- candidate at all. Before this, a queue whose oldest task wanted an A10
   -- stopped every A100 worker's claim at that task (the fit check below
   -- treats a non-fit as "wait your turn"), so two A100s sat idle behind one
   -- busy A10 while A100 work waited. Waiting stays correct for a capacity
   -- shortage; it was never correct for a model this worker does not have.
   AND ((COALESCE((a.resource_request ->> 'gpu_count')::bigint, 0) = 0
         AND NOT (a.resource_request ?| ARRAY['mig','gpu_share_milli','gpu_share_memory_bytes']))
        OR COALESCE(a.resource_request ->> 'gpu_kind', '') = ''
        OR a.resource_request ->> 'gpu_kind' =
           (SELECT capacity ->> 'gpu_kind' FROM workers WHERE id = $11))
   AND COALESCE((a.resource_request ->> 'gpu_memory_bytes')::bigint, 0) <=
       COALESCE((SELECT (capacity ->> 'gpu_memory_bytes')::bigint FROM workers WHERE id = $11), 0)
   -- Fractional GPUs (ADR-0029 §5): a MIG profile this worker has too few
   -- instances of, or a shared slice larger than every one of its shared
   -- devices, can NEVER run here, so it must not be this worker's candidate
   -- (it would stop the poll at it, the A10/A100 defect above). Each clause
   -- is guarded by a key test; the keys are written only when non-zero, so a
   -- CPU-only attempt pays one '?' per clause.
   AND (NOT (a.resource_request ? 'mig') OR NOT EXISTS (
        SELECT 1 FROM jsonb_each_text(CASE WHEN jsonb_typeof(a.resource_request -> 'mig') = 'object'
                                           THEN a.resource_request -> 'mig' ELSE '{}'::jsonb END) m
         WHERE m.value::bigint > COALESCE(
               ((SELECT capacity FROM workers WHERE id = $11) -> 'mig' ->> m.key)::bigint, 0)))
   AND (NOT (a.resource_request ?| ARRAY['gpu_share_milli','gpu_share_memory_bytes']) OR EXISTS (
        SELECT 1 FROM jsonb_each(COALESCE((SELECT capacity -> 'gpu_shares' FROM workers WHERE id = $11), '{}'::jsonb)) d
         WHERE COALESCE((d.value ->> 'milli')::bigint, 0) >= COALESCE((a.resource_request ->> 'gpu_share_milli')::bigint, 0)
           AND COALESCE((d.value ->> 'memory_bytes')::bigint, 0) >= COALESCE((a.resource_request ->> 'gpu_share_memory_bytes')::bigint, 0)))
   -- Hard placement constraints (ADR-0030 §1, ADR-0031 §2), against the
   -- EFFECTIVE labels. Guarded by the placement key, which only specs that
   -- declare one carry.
   AND (NOT (n.spec_json ? 'placement') OR (
        $12::jsonb @> COALESCE(n.spec_json #> '{placement,required_labels}',
                               n.spec_json #> '{placement,requiredLabels}', '{}'::jsonb)
    AND (NOT $13::bool OR COALESCE(n.spec_json #>> '{placement,capacity}', '')
                          NOT IN ('CAPACITY_PREFERENCE_ON_DEMAND_ONLY', 'on_demand_only', '2'))
    AND (COALESCE(NULLIF(COALESCE(n.spec_json #>> '{placement,max_cost_tier}',
                                  n.spec_json #>> '{placement,maxCostTier}'), ''), '0')::int = 0
         OR $14::int <= COALESCE(n.spec_json #>> '{placement,max_cost_tier}',
                                 n.spec_json #>> '{placement,maxCostTier}')::int)))
   ` + extraWhere + `
 ORDER BY ` + orderBy + `
 LIMIT 1
 FOR UPDATE OF a SKIP LOCKED`
}

// windowCursor is the keyset position after the last row of a claim window, in
// the window's own order: rank DESC, scheduled_at ASC, id ASC.
type windowCursor struct {
	rank        float64
	scheduledAt time.Time
	id          uuid.UUID
}

// selectCandidateWindowed is the bounded form of the candidate scan
// (ADR-0018 Stage 1).
//
// It reads the top ClaimWindow PENDING ids for this worker's queue and tenant
// straight off idx_attempts_dispatch_rank -- no join, no sort -- and only then
// joins and filters THOSE rows with the shared eligibility template, locking
// the best one with SKIP LOCKED exactly as the exhaustive scan does. A claim
// therefore costs a window, not the backlog, and the join cannot degenerate
// into a per-row scan of workflow_runs because it only ever sees the window.
//
// decided=false means "every page was full and nothing in it was eligible":
// the caller falls back to the exhaustive scan. decided=true returns the same
// answer the exhaustive scan would give (a candidate, ErrNoCandidate, or an
// error), because the window order is the scan's order.
func (s *SchedulerStore) selectCandidateWindowed(
	ctx context.Context, tx pgx.Tx, now time.Time,
	worker *pollWorker, skip []uuid.UUID, args []any,
) (*Candidate, error, bool) {
	pages := s.cfg.ClaimWindowPages
	if pages < 1 {
		pages = 1
	}
	var cursor *windowCursor
	for page := 0; page < pages; page++ {
		ids, last, err := s.claimWindow(ctx, tx, now, worker, skip, cursor)
		if err != nil {
			return nil, err, true
		}
		if len(ids) == 0 {
			// Nothing PENDING and due at or after the cursor: the exhaustive
			// scan would find nothing either.
			return nil, ErrNoCandidate, true
		}

		row := tx.QueryRow(ctx, workerCandidateSQL(
			"AND a.id = ANY($17::uuid[])",
			// The same order the window was read in, with the configured rate
			// passed as a parameter: this sorts at most ClaimWindow rows, so it
			// needs no index, and it keeps the pick identical to the window's.
			"nexus_dispatch_rank(a.priority, a.scheduled_at, $3) DESC, a.scheduled_at ASC, a.id ASC",
		), append(args, ids)...)
		cand, err := s.finishWorkerCandidate(ctx, tx, worker, row)
		if !errors.Is(err, ErrNoCandidate) || errors.Is(err, errCandidateBlocked) {
			// A candidate, a real error, or the best eligible attempt found but
			// not fitting this worker (its reason is already recorded). The
			// exhaustive scan stops at that same attempt, so stop here too.
			return cand, err, true
		}
		if len(ids) < s.cfg.ClaimWindow {
			// A short window is the end of the backlog.
			return nil, ErrNoCandidate, true
		}
		cursor = last
	}
	return nil, nil, false
}

// claimWindow reads one window of candidate ids in rank order, starting after
// cursor (or at the top when cursor is nil).
//
// No join and no row lock: this is a cheap, index-ordered read. Locking and
// every eligibility rule happen in the second query, over these ids only.
//
// The rank expression embeds the configured aging rate as a literal, because
// that is what lets the planner match idx_attempts_dispatch_rank (built for the
// default rate 1.0). A different rate still orders correctly; it just cannot
// use the index, which EnableWindowedClaim warns about at startup.
func (s *SchedulerStore) claimWindow(
	ctx context.Context, tx pgx.Tx, now time.Time,
	worker *pollWorker, skip []uuid.UUID, cursor *windowCursor,
) ([]uuid.UUID, *windowCursor, error) {
	args := []any{s.cfg.Queue, worker.ID, now, skip, s.cfg.ClaimWindow}
	if cursor != nil {
		args = append(args, cursor.rank, cursor.scheduledAt, cursor.id)
	}
	rows, err := tx.Query(ctx, s.claimWindowSQL(cursor != nil), args...)
	if err != nil {
		return nil, nil, fmt.Errorf("read claim window for worker %s: %w", worker.ID, err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	var last windowCursor
	for rows.Next() {
		if err := rows.Scan(&last.id, &last.rank, &last.scheduledAt); err != nil {
			return nil, nil, fmt.Errorf("scan claim window: %w", err)
		}
		ids = append(ids, last.id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("claim window rows: %w", err)
	}
	return ids, &last, nil
}

// errCandidateBlocked is ErrNoCandidate for "the best eligible attempt was
// found but does not fit this worker". It wraps ErrNoCandidate so every existing
// caller treats it exactly as before; only the windowed scan distinguishes it,
// because it must stop paging there rather than read the next window.
var errCandidateBlocked = fmt.Errorf("%w: best candidate does not fit the polling worker", ErrNoCandidate)

// claimWindowSQL is the window query, separate so a test can EXPLAIN the exact
// text the claim runs rather than a copy of it that could drift.
//
// Parameters: $1 queue, $2 worker id, $3 now, $4 skip ids, $5 window size, and
// with a cursor $6 rank, $7 scheduled_at, $8 id.
func (s *SchedulerStore) claimWindowSQL(withCursor bool) string {
	rank := "nexus_dispatch_rank(a.priority, a.scheduled_at, " +
		strconv.FormatFloat(s.cfg.AgingRate, 'g', -1, 64) + "::double precision)"
	q := `
SELECT a.id, ` + rank + ` AS rank, a.scheduled_at
  FROM task_attempts a
 WHERE a.state = 'PENDING'
   AND a.queue = $1
   AND a.tenant_id = (SELECT tenant_id FROM workers WHERE id = $2)
   AND a.scheduled_at <= $3
   AND NOT (a.id = ANY($4::uuid[]))`
	if withCursor {
		// Keyset paging in the index's mixed order. The leading "rank <= $6"
		// is redundant with the OR below but is what lets the scan START at
		// the cursor instead of re-reading every earlier page.
		q += `
   AND ` + rank + ` <= $6
   AND (` + rank + ` < $6
        OR (a.scheduled_at > $7 OR (a.scheduled_at = $7 AND a.id > $8)))`
	}
	return q + `
 ORDER BY ` + rank + ` DESC, a.scheduled_at ASC, a.id ASC
 LIMIT $5`
}

// finishWorkerCandidate scans the candidate row and applies the authoritative
// fit check. Shared by the exhaustive and windowed paths so both decide fit
// the same way.
func (s *SchedulerStore) finishWorkerCandidate(
	ctx context.Context, tx pgx.Tx, worker *pollWorker, row pgx.Row,
) (*Candidate, error) {
	var err error
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
		return nil, errCandidateBlocked
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
) (idempotencyKey string, specJSON, inputRefs []byte, traceCtx string, err error) {
	var tc *string
	err = tx.QueryRow(ctx, `
SELECT a.idempotency_key, n.spec_json::text, a.input_refs::text, a.trace_context
  FROM task_attempts a
  JOIN task_nodes n ON n.run_id = a.run_id AND n.node_id = a.node_id
 WHERE a.id = $1`, cand.ID).Scan(&idempotencyKey, &specJSON, &inputRefs, &tc)
	if err != nil {
		return "", nil, nil, "", fmt.Errorf("load assignment details for attempt %s: %w", cand.ID, err)
	}
	if tc != nil {
		traceCtx = *tc
	}
	return idempotencyKey, specJSON, inputRefs, traceCtx, nil
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
//
// TIMED_OUT is included (CONTRACT-FINDINGS F160): a run deadline kills the
// run's work exactly as cancel does. The workflow-manager marks the run
// TIMED_OUT and then asks for the attempts to be cancelled, and that second
// call was best-effort with no retry -- when it was lost, the run was over and
// its attempts ran on, holding their reservations, with no channel left to
// stop them. Deriving the directive from the run's durable state makes the
// lost call harmless, as it already was for CANCELLING.
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
            AND r.state IN ('CANCELLING','CANCELLED','TIMED_OUT')))
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
   AND a.tenant_id = (SELECT tenant_id FROM workers WHERE id=$3)
   AND (a.pending_reason IS NULL OR a.pending_reason = '')
   AND NOT (a.id = ANY($2::uuid[]))
 ORDER BY a.scheduled_at
 LIMIT 20`, s.cfg.Queue, skip, worker.ID)
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
