// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// ClaimConfig are the dispatch tunables.
type ClaimConfig struct {
	// Queue this scheduler instance drains.
	Queue string
	// LivenessWindow is how long after its last heartbeat a worker is still
	// considered placeable. Shorter detects death faster but risks false
	// expiry under a network blip (ADR-0010).
	LivenessWindow time.Duration
	// LeaseTTL is the initial lease lifetime; the worker renews well inside it.
	LeaseTTL time.Duration
	// AgingRate is the effective-priority gain per second a PENDING attempt
	// accrues. Without it, a steady stream of small tasks starves a large one
	// forever — the classic failure of naive bin-packing schedulers (§7.3).
	AgingRate float64
	// AgingBlockThreshold is the effective-priority margin at which a blocked
	// attempt stops yielding to smaller work on the workers it is waiting for.
	// See reserveForBlocked.
	AgingBlockThreshold float64
	// LookAhead is how many PENDING attempts a cycle may skip past before
	// giving up, so one unplaceable task does not stall the whole queue while
	// aging accrues.
	LookAhead int
	// FairShare orders the push-path candidate scan by each namespace's
	// in-flight attempts divided by its weight BEFORE priority, so one
	// namespace flooding a shared queue cannot monopolise dispatch (ADR-0017).
	// Priority and aging still order work WITHIN a namespace.
	FairShare bool
	// FairShareMaxNamespaces bounds how many namespaces one cycle may give up
	// on (each after LookAhead blocked candidates) before returning. It is the
	// cost ceiling of a cycle in which every namespace is blocked:
	// LookAhead x FairShareMaxNamespaces candidate reads, never more.
	FairShareMaxNamespaces int
	// ClaimWindow is how many PENDING attempts the pull-path claim reads off
	// idx_attempts_dispatch_rank per window before joining and filtering them
	// (ADR-0018 Stage 1). It bounds a claim's cost by the window instead of
	// the backlog. Zero disables windowing and every claim scans the backlog.
	ClaimWindow int
	// ClaimWindowPages bounds how many consecutive windows one claim may read
	// before falling back to the exhaustive scan. A worker that can run
	// nothing near the top of the queue (a CPU worker behind a wall of GPU
	// tasks) would otherwise page through the whole backlog 64 rows at a
	// time, which is slower than scanning it once.
	ClaimWindowPages int
	// AttemptLockTimeout bounds how long a terminal transition driven by a
	// report (ApplyResult, run cancel) waits for another transaction holding
	// the attempt row -- usually the worker's own ReportStarted. Past it the
	// report fails with ErrAttemptBusy, which the RPC layer maps to a
	// retryable status so the agent resends from its WAL. Zero means
	// DefaultAttemptLockTimeout. See CONTRACT-FINDINGS F104.
	AttemptLockTimeout time.Duration
	// Quotas enforces namespace concurrent quotas in the claim (ADR-0021). On
	// by default: a namespace without quotas pays one indexed lookup and takes
	// no lock. Off (NEXUS_QUOTAS=false) is the rollback step before reverting
	// migration 0027: the claim then reads neither the view nor the usage.
	Quotas bool

	// PlacementPolicy is pack (best fit, the default) or spread, for the
	// worker choice on the push path, the deferral score on the pull path and
	// the shared GPU device inside a worker (ADR-0029 §4).
	PlacementPolicy domain.PlacementPolicy
	// PlacementWait bounds pull-path delay scheduling: a candidate with
	// something to prefer is left for a strictly better live worker for at
	// most this long after it became due. Zero disables deferral.
	PlacementWait time.Duration
	// Cost prices a worker for the placement score; nil ignores cost
	// (ADR-0030 §6).
	Cost domain.CostModel
	// HeadroomTTL is how old a cluster's published quota headroom may be and
	// still gate dispatch (ADR-0031 §3).
	HeadroomTTL time.Duration
	// MaxPreemptions is the platform cap on preemption retries for a task
	// that declares none (ADR-0030 §3).
	MaxPreemptions int32
	// PreemptionRetryDelay is the backoff before retrying a PREEMPTED attempt.
	PreemptionRetryDelay time.Duration
}

// DefaultClaimConfig returns the ADR-0010 recommended lease settings.
func DefaultClaimConfig(queue string) ClaimConfig {
	return ClaimConfig{
		Queue:          queue,
		LivenessWindow: 30 * time.Second,
		LeaseTTL:       30 * time.Second,
		// 1 priority point per second: a task blocked for 100s outranks a
		// fresh task 100 points above it, so the starvation window is bounded
		// in wall-clock terms an operator can reason about.
		AgingRate:           1.0,
		AgingBlockThreshold: 60,
		LookAhead:           8,
		// On by default: with a single namespace per queue it orders exactly
		// as before, and the case it exists for -- a flood from one tenant --
		// is the one nobody enables a flag for in advance.
		FairShare:              true,
		FairShareMaxNamespaces: 4,
		ClaimWindow:            64,
		ClaimWindowPages:       4,
		Quotas:                 true,
		PlacementPolicy:        domain.PolicyPack,
		PlacementWait:          3 * time.Second,
		Cost:                   domain.TierCostModel{},
		HeadroomTTL:            2 * time.Minute,
		MaxPreemptions:         domain.DefaultMaxPreemptions,
		PreemptionRetryDelay:   2 * time.Second,
	}
}

// SchedulerStore is the scheduler's data access. It owns task_attempts, leases
// and concurrency_slots, and touches reservations and workers.committed only
// inside Claim (ADR-0011's single documented exception).
type SchedulerStore struct {
	pool *pgxpool.Pool
	cfg  ClaimConfig
	// instanceID identifies this replica in timers.claimed_by, so a crashed
	// replica's claimed timers can be told apart from live ones.
	instanceID string
	// windowed selects the bounded pull-path claim. Set once, before the store
	// serves claims, by EnableWindowedClaim -- never flipped afterwards, so it
	// needs no synchronisation.
	windowed bool
	// observer receives quota and metering events after commit, for metrics.
	// Set once at startup by SetObserver; nil records nothing.
	observer QuotaObserver
	// quotasMissing and meteringMissing are set by DetectQuotaSchema when
	// migration 0027 is not applied yet, so a scheduler started ahead of the
	// migration keeps dispatching instead of failing every claim and every
	// terminal transition. Set once at startup, never flipped afterwards.
	quotasMissing   bool
	meteringMissing bool
	// placementMissing is set by DetectPlacementSchema when migration 0035 is
	// not applied yet; the claim then reads no placement facts.
	placementMissing bool
}

// NewSchedulerStore constructs the store. instanceID should be stable for the
// life of the process and unique across replicas (pod name, or a random UUID).
func NewSchedulerStore(pool *pgxpool.Pool, cfg ClaimConfig, instanceID string) *SchedulerStore {
	return &SchedulerStore{pool: pool, cfg: cfg, instanceID: instanceID}
}

// Config exposes the tunables for logging and for the reconcilers.
func (s *SchedulerStore) Config() ClaimConfig { return s.cfg }

// indexedAgingRate is the aging rate idx_attempts_dispatch_rank is built for
// (migration 0023). A scheduler configured with another rate still claims in
// the right order but cannot use the index.
const indexedAgingRate = 1.0

// WindowedClaimStatus reports how the pull-path claim will run, for the
// startup log line an operator reads when dispatch is slow.
type WindowedClaimStatus struct {
	// Enabled is true when claims read bounded windows.
	Enabled bool
	// Indexed is true when those windows are served by the index. False with
	// Enabled true means a non-default aging rate: correct order, unindexed.
	Indexed bool
	// Reason says why windowing is off, when it is.
	Reason string
}

// EnableWindowedClaim turns on the bounded pull-path claim if the database
// has migration 0023's rank function. Call it once at startup, before the
// store serves claims.
//
// Detected rather than assumed, because a scheduler can start before 0023 is
// applied during a rollout: using the windowed query then would fail EVERY
// pull claim, whereas the exhaustive scan it replaces keeps working. A
// scheduler that started without the function stays on the exhaustive scan
// until it restarts, which is the safe direction to be wrong in.
func (s *SchedulerStore) EnableWindowedClaim(ctx context.Context) (WindowedClaimStatus, error) {
	if s.cfg.ClaimWindow <= 0 {
		return WindowedClaimStatus{Reason: "claim window is 0 (disabled by configuration)"}, nil
	}
	var present bool
	if err := s.pool.QueryRow(ctx,
		`SELECT to_regprocedure('nexus_dispatch_rank(integer,timestamptz,double precision)') IS NOT NULL`,
	).Scan(&present); err != nil {
		return WindowedClaimStatus{}, fmt.Errorf("detect nexus_dispatch_rank: %w", err)
	}
	if !present {
		return WindowedClaimStatus{Reason: "migration 0023 not applied (nexus_dispatch_rank missing)"}, nil
	}
	s.windowed = true
	return WindowedClaimStatus{Enabled: true, Indexed: s.cfg.AgingRate == indexedAgingRate}, nil
}

// ErrNoCandidate signals an empty queue, distinguished from a real error so the
// dispatch loop can back off instead of logging a failure every tick.
var ErrNoCandidate = errors.New("no dispatchable attempt")

// Claim executes ONE dispatch cycle as ONE transaction, exactly as §5.3
// specifies: claim a PENDING attempt, find a fitting live worker, admit against
// the concurrency semaphore, then commit reservation + workers.committed +
// lease + attempt CAS + run_event + timers together.
//
// The transaction is the entire correctness argument for running N scheduler
// replicas with no leader election (ADR-0002):
//
//   - FOR UPDATE SKIP LOCKED on the attempt lets replicas work the same queue
//     without contending; a row another replica is examining is invisible here
//     rather than a blocking wait.
//   - FOR UPDATE on the worker row serializes capacity spending, which is what
//     makes over-commit impossible rather than merely unlikely.
//   - The CAS guard (WHERE state='PENDING') plus UNIQUE(attempt_id) on leases
//     makes double dispatch impossible even if a replica's row lock were
//     somehow lost.
//
// It deliberately performs NO network I/O: a claim transaction that waited on
// NATS would hold row locks across a network round trip and block vacuum
// (ADR-0002 negative consequences). The bus announcement is an outbox row
// written by the same statement as the AttemptDispatched event (appendEvent), and
// the event-service relay publishes it after commit (migration 0016).
func (s *SchedulerStore) Claim(ctx context.Context, now time.Time) (ClaimReport, error) {
	// READ COMMITTED, deliberately, and NOT the REPEATABLE READ that §5.3's
	// pseudocode shows. Under REPEATABLE READ, a locking read that reaches a row
	// some other transaction has already committed an update to does not skip it
	// — it aborts the whole transaction with a serialization failure (SQLSTATE
	// 40001). SKIP LOCKED only skips rows that are currently LOCKED, so with N
	// replicas working one queue the 40001s are constant and every claim becomes
	// a retry loop. READ COMMITTED re-reads the row at lock time, which is
	// exactly the semantics a SKIP LOCKED work queue needs.
	//
	// Nothing is lost: the correctness argument rests on the row locks
	// (FOR UPDATE serializes capacity spending), the CAS guard on
	// state = 'PENDING', and UNIQUE(attempt_id) on leases — none of which
	// depends on snapshot isolation. This divergence from the frozen document
	// is flagged in the report rather than silently absorbed.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ClaimReport{}, fmt.Errorf("begin claim tx: %w", err)
	}
	// Rollback is unconditional and idempotent: a committed tx makes it a
	// no-op, and any early return path must not leak the row locks.
	defer func() { _ = tx.Rollback(ctx) }()

	report, err := s.claimInTx(ctx, tx, now)
	if err != nil {
		return report, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ClaimReport{}, fmt.Errorf("commit claim tx: %w", err)
	}
	s.notifyQuotaBlocked(report.QuotaBlocked)
	return report, nil
}

func (s *SchedulerStore) claimInTx(ctx context.Context, tx pgx.Tx, now time.Time) (ClaimReport, error) {
	// The look-ahead loop is the head-of-line-blocking avoidance from §7.3.
	// Pure head-of-line blocking would idle 3 GPUs waiting for a 4-GPU task;
	// skipping past a blocked attempt to a smaller one that fits keeps the
	// cluster busy. Aging (in the ORDER BY below) is what stops that skipping
	// from becoming permanent starvation.
	lookAhead := s.cfg.LookAhead
	if lookAhead < 1 {
		lookAhead = 1
	}

	var skipped []uuid.UUID
	var firstBlocked *ClaimReport

	// Fair share turns the flat look-ahead into a per-namespace one. Without
	// the exclusion, a namespace whose head is blocked -- its workers full --
	// sits at the lowest share (nothing of it is in flight) and would absorb
	// every look-ahead slot of every cycle, starving the namespaces behind it:
	// the very flood fair share exists to contain. So a namespace that has
	// used its LookAhead is set aside for the rest of THIS cycle only.
	budget := lookAhead
	maxNS := 1
	if s.cfg.FairShare {
		maxNS = s.cfg.FairShareMaxNamespaces
		if maxNS < 1 {
			maxNS = 1
		}
		budget = lookAhead * maxNS
	}
	var excluded []uuid.UUID
	blockedIn := map[uuid.UUID]int{}
	// The salt rotates namespaces that tie on share (typically several at
	// zero in flight) from cycle to cycle, so a tie is never broken the same
	// way forever. Per-namespace, not per-row: a per-row random() would
	// scramble priority order inside a namespace.
	salt := fairShareSalt(now)

	// Every attempt a namespace quota held back this cycle, by dimension, for
	// the metrics the caller emits after commit.
	var quotaBlocked []string
	exclude := func(tenant uuid.UUID) bool {
		excluded = append(excluded, tenant)
		return len(excluded) >= maxNS
	}

	for i := 0; i < budget; i++ {
		cand, err := s.selectCandidate(ctx, tx, now, skipped, excluded, salt)
		if errors.Is(err, ErrNoCandidate) {
			break
		}
		if err != nil {
			return ClaimReport{}, err
		}

		rep, err := s.placeCandidate(ctx, tx, now, cand)
		if err != nil {
			return ClaimReport{}, err
		}
		if rep.Outcome == ClaimDispatched || rep.Outcome == ClaimRaceLost {
			rep.QuotaBlocked = quotaBlocked
			return rep, nil
		}
		if rep.quotaContended {
			// Another replica is dispatching this namespace under its quota
			// lock right now (ADR-0021). Not blocked -- nothing is recorded on
			// the attempt -- only busy, so the namespace is set aside for the
			// rest of this cycle and the next cycle tries it again.
			skipped = append(skipped, cand.ID)
			if exclude(cand.TenantID) {
				break
			}
			continue
		}
		if rep.QuotaDimension != "" {
			quotaBlocked = append(quotaBlocked, rep.QuotaDimension)
		}
		// Blocked. Record the reason for the FIRST blocked attempt (the
		// highest effective priority one, i.e. the task an operator most wants
		// explained) and look past it for smaller work that fits.
		if firstBlocked == nil {
			r := rep
			firstBlocked = &r
		}
		skipped = append(skipped, cand.ID)
		blockedIn[cand.TenantID]++
		// A namespace with no headroom left on a quota dimension cannot place
		// ANY attempt that uses it, so looking further into its backlog would
		// only spend the look-ahead on the same answer (ADR-0021).
		if blockedIn[cand.TenantID] >= lookAhead || rep.quotaExhausted {
			if exclude(cand.TenantID) {
				break
			}
		}
	}

	if firstBlocked != nil {
		firstBlocked.QuotaBlocked = quotaBlocked
		return *firstBlocked, nil
	}
	return ClaimReport{Outcome: ClaimNothingPending, QuotaBlocked: quotaBlocked}, nil
}

// selectCandidate is §5.3 step 1: claim the highest effective-priority PENDING
// attempt with FOR UPDATE SKIP LOCKED.
//
// The ORDER BY adds the aging term to the stored priority. Aging lives in the
// ORDER BY rather than in a background UPDATE of the priority column for two
// reasons: an UPDATE-based ager would rewrite every PENDING row every tick
// (table bloat on the hottest table in the system, ADR-0002), and it would make
// the stored priority no longer the author's declared priority, so the UI could
// not show what the user actually asked for.
//
// It also filters on the RUN's state, because a PAUSING/PAUSED/CANCELLING run
// must receive no NEW dispatch (§5.2). The scheduler READS workflow_runs; it
// never writes it (ADR-0011).
//
// With FairShare on, the namespace's share -- in-flight attempts on this queue
// divided by its configured weight -- is the FIRST sort key, ahead of priority
// (ADR-0017). Two consequences are deliberate. Priority is compared only
// within a namespace, so a tenant cannot jump the whole queue by declaring
// priority 1000 on everything. And the share is computed inside this same READ
// COMMITTED transaction from committed rows, so it is at worst one concurrent
// dispatch stale; nothing about the SKIP LOCKED + CAS argument changes, because
// the share only decides which unlocked PENDING row is tried first.
func (s *SchedulerStore) selectCandidate(
	ctx context.Context, tx pgx.Tx, now time.Time, skip, excludeTenants []uuid.UUID, salt string,
) (*Candidate, error) {
	// The in-flight count is a CTE, not a join-and-aggregate in the main query:
	// FOR UPDATE is not allowed alongside GROUP BY, and it must lock only the
	// attempt row. idx_attempts_inflight_by_tenant (migration 0022) makes the
	// CTE an index-only walk over the active rows of this queue.
	//
	// MATERIALIZED is load-bearing, measured (ADR-0018): pgx prepares this
	// statement, and after five executions Postgres may switch to a generic
	// plan that inlines the CTE as a correlated subquery re-run once PER
	// PENDING ROW. With 8,000 pending and 2,000 in flight that took a claim
	// from ~25 ms to ~1,140 ms. Materializing evaluates the count once.
	const q = `
WITH inflight AS MATERIALIZED (
  SELECT tenant_id, count(*) AS n
    FROM task_attempts
   WHERE queue = $1 AND state IN ('DISPATCHED','RUNNING')
   GROUP BY tenant_id
)
SELECT a.id, a.run_id, a.node_id, a.attempt_number, a.queue, a.priority,
       a.resource_request, a.scheduled_at,
       a.priority + (EXTRACT(EPOCH FROM ($2::timestamptz - a.scheduled_at)) * $3) AS effective_priority,
       n.spec_json, a.tenant_id,
       CASE WHEN $6::bool
            THEN COALESCE(i.n, 0)::float8 / COALESCE(w.weight, 1)
            ELSE 0 END AS share
  FROM task_attempts a
  JOIN task_nodes    n ON n.run_id = a.run_id AND n.node_id = a.node_id
  JOIN workflow_runs r ON r.id = a.run_id
  LEFT JOIN inflight i ON i.tenant_id = a.tenant_id
  LEFT JOIN scheduler_namespace_weights w ON w.tenant_id = a.tenant_id
 WHERE a.state = 'PENDING'
   AND a.queue = $1
   AND a.scheduled_at <= $2
   AND r.state IN ('PENDING','RUNNING')
   AND NOT (a.id = ANY($4::uuid[]))
   AND NOT (a.tenant_id = ANY($5::uuid[]))
 ORDER BY share ASC,
          CASE WHEN $6::bool THEN hashtext(a.tenant_id::text || $7) ELSE 0 END,
          effective_priority DESC, a.scheduled_at ASC
 LIMIT 1
 FOR UPDATE OF a SKIP LOCKED`

	if skip == nil {
		skip = []uuid.UUID{}
	}
	if excludeTenants == nil {
		excludeTenants = []uuid.UUID{}
	}
	// QueryExecModeDescribeExec (an unnamed statement, re-planned every call) instead
	// of pgx's default cached prepared statement, measured (ADR-0018): after
	// five executions Postgres may adopt a GENERIC plan for this query, and the
	// one it picks nested-loops a scan of workflow_runs per pending row. At a
	// 100k backlog that took one claim from ~270 ms (custom plan) to ~3.3 s.
	// Planning costs ~1-3 ms per claim; a plan that is wrong by 10x costs more.
	row := tx.QueryRow(ctx, q, pgx.QueryExecModeDescribeExec, s.cfg.Queue, now, s.cfg.AgingRate, skip,
		excludeTenants, s.cfg.FairShare, salt)

	var c Candidate
	var reqJSON, specJSON []byte
	if err := row.Scan(
		&c.ID, &c.RunID, &c.NodeID, &c.AttemptNumber, &c.Queue, &c.Priority,
		&reqJSON, &c.ScheduledAt, &c.EffectivePriority, &specJSON,
		&c.TenantID, &c.FairShare,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoCandidate
		}
		return nil, fmt.Errorf("select pending attempt: %w", err)
	}

	req, err := decodeVector(reqJSON)
	if err != nil {
		return nil, fmt.Errorf("decode resource_request for attempt %s: %w", c.ID, err)
	}
	c.ResourceRequest = req

	var spec TaskSpecView
	// A spec we cannot decode must not stop the scheduler: the attempt stays
	// PENDING with platform defaults rather than wedging the whole queue.
	if len(specJSON) > 0 {
		_ = json.Unmarshal(specJSON, &spec)
	}
	c.Spec = &spec
	return &c, nil
}

// placeCandidate runs §5.3 steps 2-5 for one already-locked candidate.
func (s *SchedulerStore) placeCandidate(
	ctx context.Context, tx pgx.Tx, now time.Time, cand *Candidate,
) (ClaimReport, error) {
	// §5.3 step 2: find a fitting, live, label- and executor-compatible worker,
	// row-locked so two schedulers cannot spend the same capacity.
	worker, short, err := s.selectWorker(ctx, tx, now, cand)
	if err != nil {
		return ClaimReport{}, err
	}
	if worker == nil {
		// §5.3 step 3: record WHY and leave it PENDING. Emphatically not a
		// failure — the cluster is merely busy, and failing the attempt here
		// would burn its retry budget on a capacity problem.
		reason := domain.ShortfallReason(short)
		if reason == "" {
			reason = "waiting_for_worker: no ACTIVE worker matches queue/labels/executor"
		}
		if err := s.markBlocked(ctx, tx, cand.ID, reason); err != nil {
			return ClaimReport{}, err
		}
		return ClaimReport{Outcome: ClaimBlocked, Attempt: cand, Reason: reason}, nil
	}

	// §5.3 step 4: concurrency semaphore. Checked after placement because a
	// task that cannot fit anywhere should report the resource shortfall, which
	// is the actionable problem, rather than a semaphore it never reached.
	if key := cand.Spec.ConcurrencyKey; key != "" && cand.Spec.ConcurrencyLimit > 0 {
		ok, held, err := s.concurrencyAdmit(ctx, tx, key, cand.Spec.ConcurrencyLimit, cand.ID)
		if err != nil {
			return ClaimReport{}, err
		}
		if !ok {
			reason := fmt.Sprintf("waiting_for_concurrency: %s (held %d, limit %d)",
				key, held, cand.Spec.ConcurrencyLimit)
			if err := s.markBlocked(ctx, tx, cand.ID, reason); err != nil {
				return ClaimReport{}, err
			}
			return ClaimReport{Outcome: ClaimBlocked, Attempt: cand, Reason: reason}, nil
		}
	}

	// §5.3 step 3 as specified, after placement and the semaphore for the same
	// reason the semaphore follows placement: the resource shortfall is the
	// more actionable explanation. See scheduler_quota.go for why the lock is
	// only TRIED here.
	nq, locked, err := s.tryLockQuota(ctx, tx, cand.TenantID)
	if err != nil {
		return ClaimReport{}, err
	}
	if nq != nil {
		if !locked {
			return ClaimReport{Outcome: ClaimBlocked, Attempt: cand, quotaContended: true}, nil
		}
		v, err := s.quotaViolation(ctx, tx, nq, cand)
		if err != nil {
			return ClaimReport{}, err
		}
		if v != nil {
			reason := v.Reason()
			if err := s.markBlocked(ctx, tx, cand.ID, reason); err != nil {
				return ClaimReport{}, err
			}
			return ClaimReport{Outcome: ClaimBlocked, Attempt: cand, Reason: reason,
				QuotaDimension: v.Dimension, quotaExhausted: v.Exhausted}, nil
		}
	}

	// §5.3 step 5: commit everything atomically.
	dispatch, ok, err := s.commitDispatch(ctx, tx, now, cand, worker)
	if err != nil {
		return ClaimReport{}, err
	}
	if !ok {
		// The CAS guard fired: another replica dispatched this attempt between
		// our SELECT and our UPDATE. Impossible with the row lock held, kept as
		// a belt-and-braces check because a silent double dispatch is the worst
		// bug this system could have.
		return ClaimReport{Outcome: ClaimRaceLost, Attempt: cand}, nil
	}
	return ClaimReport{Outcome: ClaimDispatched, Attempt: cand, Dispatch: dispatch}, nil
}

// selectWorker implements §5.3 step 2. Fit and best-fit scoring are evaluated
// in Go rather than SQL because the arithmetic (GPU kind matching, per-device
// VRAM floors, the custom map, the fragmentation weighting) lives in
// internal/domain and must be identical here and in every unit test. A
// duplicate SQL implementation would be a second source of truth for the
// scheduling policy.
//
// The SQL narrows to plausible candidates (state, queue, liveness, labels,
// executor) so the Go loop scans a bounded set, and locks the rows it returns so
// the capacity we then spend cannot be spent concurrently.
func (s *SchedulerStore) selectWorker(
	ctx context.Context, tx pgx.Tx, now time.Time, cand *Candidate,
) (*WorkerCandidate, []domain.Shortfall, error) {
	selector := cand.Spec.NodeSelector
	if selector == nil {
		selector = map[string]string{}
	}
	selJSON, err := json.Marshal(selector)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal node_selector: %w", err)
	}

	// DRAINING workers are excluded: they accept no new work, which is what
	// makes a rolling deploy not cost hours of in-flight compute (§5.5).
	//
	// The placement facts ride on the same rows (ADR-0031): effective labels
	// for eligibility, cached artifacts and locality for the score.
	q := `
SELECT w.id, w.hostname, w.capacity, w.committed, ` + s.placementCols() + `
  FROM workers w
 WHERE w.state = 'ACTIVE'
   AND $1 = ANY(w.queues)
   AND w.last_heartbeat_at > $2
   AND w.labels @> $3::jsonb
   AND ($4 = '' OR $4 = ANY(w.executors))
   AND w.tenant_id = (SELECT tenant_id FROM task_attempts WHERE id=$5)
 ORDER BY w.id
 FOR UPDATE OF w SKIP LOCKED`

	if err := s.resolveSameHost(ctx, tx, cand); err != nil {
		return nil, nil, err
	}

	rows, err := tx.Query(ctx, q,
		s.cfg.Queue, now.Add(-s.cfg.LivenessWindow), selJSON, cand.Spec.Executor, cand.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("select candidate workers: %w", err)
	}

	type scanned struct {
		w     WorkerCandidate
		facts workerFacts
	}
	var locked []scanned
	for rows.Next() {
		var sc scanned
		var capJSON, comJSON, labelsJSON, clLabels, headroom []byte
		var cached []string
		var at *time.Time
		if err := rows.Scan(&sc.w.ID, &sc.w.Hostname, &capJSON, &comJSON,
			&labelsJSON, &cached, &clLabels, &headroom, &at); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("scan worker row: %w", err)
		}
		if sc.w.Capacity, err = decodeVector(capJSON); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("decode capacity for worker %s: %w", sc.w.ID, err)
		}
		if sc.w.Committed, err = decodeVector(comJSON); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("decode committed for worker %s: %w", sc.w.ID, err)
		}
		sc.facts = scanFacts(labelsJSON, cached, clLabels, headroom, at)
		locked = append(locked, sc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate worker rows: %w", err)
	}

	var (
		best      *WorkerCandidate
		bestShort []domain.Shortfall
	)
	cfg := s.scoreConfig()
	for i := range locked {
		w, facts := locked[i].w, locked[i].facts
		if ok, why := domain.Eligible(facts.Labels, cand.Spec.Placement); !ok {
			if bestShort == nil {
				bestShort = []domain.Shortfall{{Dim: "placement", Detail: why}}
			}
			continue
		}
		if reason, err := s.headroomBlocks(ctx, tx, now, w.ID, &facts, cand); err != nil {
			return nil, nil, err
		} else if reason != "" {
			if bestShort == nil {
				bestShort = []domain.Shortfall{{Dim: "cluster_quota", Detail: reason}}
			}
			continue
		}
		if short := s.placeOn(&w, cand.ResourceRequest); short != nil {
			// Keep the shortfall from the CLOSEST worker (fewest blocking
			// dimensions) so pending_reason names the bottleneck an operator can
			// actually fix, not an arbitrary worker's unrelated limits.
			if bestShort == nil || len(short) < len(bestShort) {
				bestShort = short
			}
			continue
		}
		// HIGHER is better (domain.PlacementScore): packing per the policy plus
		// the soft preferences. Ties keep the lowest worker id, as before.
		w.Score = domain.PlacementScore(domain.WorkerView{
			Hostname: w.Hostname, Labels: facts.Labels, Capacity: w.Capacity,
			Committed: w.Committed, Cached: facts.Cached,
		}, cand.ResourceRequest, cand.Spec.Placement, cfg)
		if best == nil || w.Score > best.Score {
			cp := w
			best = &cp
		}
	}

	if best == nil {
		return nil, bestShort, nil
	}

	// Head-of-line-blocking guard (§7.3): if a HIGHER effective-priority
	// attempt is already blocked waiting for this very worker's capacity, and
	// its aging has pushed it past the threshold, we stop handing that worker's
	// capacity to smaller tasks and let it drain so the large task can be
	// placed. Without this, aging alone never helps: the large task's priority
	// climbs forever while small tasks keep consuming the capacity it needs.
	reserved, err := s.reservedForBlocked(ctx, tx, now, cand, best.ID)
	if err != nil {
		return nil, nil, err
	}
	if reserved != "" {
		return nil, []domain.Shortfall{{Dim: "head_of_line", Detail: reserved}}, nil
	}
	return best, nil, nil
}

// reservedForBlocked reports whether the chosen worker's capacity is being held
// for a starving higher-priority attempt. Returns a non-empty explanation when
// this candidate must yield.
//
// The comparison is on EFFECTIVE priority (with aging), and requires the blocked
// attempt to lead by AgingBlockThreshold. The margin exists so that a
// momentarily-blocked task does not instantly freeze the queue: small tasks keep
// flowing (utilization) until the large task has genuinely been starved for
// threshold/aging_rate seconds, at which point correctness of the queue matters
// more than squeezing in one more small task.
func (s *SchedulerStore) reservedForBlocked(
	ctx context.Context, tx pgx.Tx, now time.Time, cand *Candidate, workerID uuid.UUID,
) (string, error) {
	// Only attempts that are already known-blocked on resources can reserve
	// capacity. An attempt that has never been examined has no claim on a
	// worker, and letting it reserve would let a brand-new high-priority task
	// idle the cluster before we even know it does not fit.
	const q = `
SELECT a.id, a.resource_request,
       a.priority + (EXTRACT(EPOCH FROM ($1::timestamptz - a.scheduled_at)) * $2) AS effective_priority
  FROM task_attempts a
  JOIN workflow_runs r ON r.id = a.run_id
 WHERE a.state = 'PENDING'
   AND a.queue = $3
   AND a.id <> $4
   AND a.tenant_id = (SELECT tenant_id FROM task_attempts WHERE id=$4)
   AND a.pending_reason LIKE 'waiting_for_resources:%'
   AND r.state IN ('PENDING','RUNNING')
   AND a.priority + (EXTRACT(EPOCH FROM ($1::timestamptz - a.scheduled_at)) * $2) >= $5
 ORDER BY effective_priority DESC
 LIMIT 8`

	rows, err := tx.Query(ctx, q,
		now, s.cfg.AgingRate, s.cfg.Queue, cand.ID,
		cand.EffectivePriority+s.cfg.AgingBlockThreshold)
	if err != nil {
		return "", fmt.Errorf("scan starving attempts: %w", err)
	}
	defer rows.Close()

	type starving struct {
		id  uuid.UUID
		req *nexusv1.ResourceVector
		eff float64
	}
	var list []starving
	for rows.Next() {
		var st starving
		var reqJSON []byte
		if err := rows.Scan(&st.id, &reqJSON, &st.eff); err != nil {
			return "", fmt.Errorf("scan starving attempt: %w", err)
		}
		v, err := decodeVector(reqJSON)
		if err != nil {
			return "", fmt.Errorf("decode starving resource_request: %w", err)
		}
		st.req = v
		list = append(list, st)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate starving attempts: %w", err)
	}
	if len(list) == 0 {
		return "", nil
	}

	// Reserve only if the starving attempt could EVENTUALLY run on this worker
	// (its total capacity suffices) but cannot right now. A task that will never
	// fit here must not be allowed to idle this worker forever — that trades
	// starvation of one task for starvation of the whole queue.
	var capJSON []byte
	if err := tx.QueryRow(ctx,
		`SELECT capacity FROM workers WHERE id = $1`, workerID).Scan(&capJSON); err != nil {
		return "", fmt.Errorf("load worker capacity: %w", err)
	}
	capacity, err := decodeVector(capJSON)
	if err != nil {
		return "", fmt.Errorf("decode worker capacity: %w", err)
	}

	for _, st := range list {
		if fitsEmpty, _ := domain.Fits(capacity, nil, st.req); !fitsEmpty {
			continue
		}
		return fmt.Sprintf(
			"capacity on worker %s reserved for starving attempt %s (effective priority %.0f > %.0f)",
			workerID, st.id, st.eff, cand.EffectivePriority), nil
	}
	return "", nil
}

// concurrencyAdmit is §5.3 step 4: the named global semaphore.
//
// Slots are counted under the same transaction as the dispatch, so two
// schedulers cannot both see limit-1 held and both admit. The row-level lock on
// the attempt is not enough for this — the contending attempts are different
// rows — so the count and the insert must share a transaction, which they do.
func (s *SchedulerStore) concurrencyAdmit(
	ctx context.Context, tx pgx.Tx, key string, limit int32, attemptID uuid.UUID,
) (bool, int32, error) {
	// Only slots belonging to attempts that are genuinely still active count.
	// A slot whose attempt is terminal is garbage the release path missed (a
	// crashed replica); counting it would deadlock the semaphore permanently.
	const q = `
SELECT count(*)
  FROM concurrency_slots c
  JOIN task_attempts a ON a.id = c.attempt_id
 WHERE c.key = $1
   AND c.tenant_id = (SELECT tenant_id FROM task_attempts WHERE id=$2)
   AND a.state IN ('DISPATCHED','RUNNING')`
	var held int32
	if err := tx.QueryRow(ctx, q, key, attemptID).Scan(&held); err != nil {
		return false, 0, fmt.Errorf("count concurrency slots for %q: %w", key, err)
	}
	return held < limit, held, nil
}

// commitDispatch is §5.3 step 5: reservation, capacity, lease, attempt CAS,
// event and timers, all in the caller's transaction.
//
// Returns ok=false when the CAS guard rejected the state change, which the
// caller must treat as "someone else won", never as an error.
func (s *SchedulerStore) commitDispatch(
	ctx context.Context, tx pgx.Tx, now time.Time, cand *Candidate, worker *WorkerCandidate,
) (*DispatchResult, bool, error) {
	token, err := newLeaseToken()
	if err != nil {
		return nil, false, err
	}
	expiresAt := now.Add(s.cfg.LeaseTTL)

	// ── ADR-0011 EXCEPTION STARTS ────────────────────────────────────────
	// The next two statements write `reservations` and `workers.committed`,
	// which are RESOURCE-MANAGER tables. This is THE ONE deliberate exception
	// to per-service table ownership (ADR-0011 rule 2, CONTRACT.md).
	//
	// The reason is invariant 4: the capacity reservation, the lease, the
	// attempt CAS, the event and the timers must commit or fail together.
	// Routing the capacity half through the resource manager's gRPC API would
	// split that into two transactions across two processes, which needs a saga
	// with compensating transactions — and a compensation bug there means
	// either a permanently leaked GPU reservation or a double-dispatched task.
	// Atomicity here is worth more than ownership purity. Nowhere else does the
	// scheduler write these tables; every other capacity operation calls the
	// resource manager's API.
	//
	// The reservation is the PLACED vector (ADR-0029): the request plus the
	// shared GPU device chosen for it. Every release path subtracts the
	// reservation row, so recording the device here is what makes release
	// return capacity to the device it was taken from.
	reserved := worker.Placed
	if reserved == nil {
		reserved = cand.ResourceRequest
	}
	reqJSON, err := encodeVector(reserved)
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO reservations (worker_id, attempt_id, resources) VALUES ($1,$2,$3)`,
		worker.ID, cand.ID, reqJSON,
	); err != nil {
		return nil, false, fmt.Errorf("insert reservation: %w", err)
	}

	newCommitted, err := encodeVector(domain.AddVec(worker.Committed, reserved))
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE workers SET committed = $2 WHERE id = $1`, worker.ID, newCommitted,
	); err != nil {
		return nil, false, fmt.Errorf("update worker committed: %w", err)
	}
	// ── ADR-0011 EXCEPTION ENDS ──────────────────────────────────────────

	var leaseID uuid.UUID
	if err := tx.QueryRow(ctx, `
INSERT INTO leases (attempt_id, worker_id, token, expires_at, renewed_at, epoch)
VALUES ($1,$2,$3,$4,$5,
        COALESCE((SELECT max(epoch)+1 FROM leases WHERE attempt_id = $1), 0))
RETURNING id`,
		cand.ID, worker.ID, token, expiresAt, now,
	).Scan(&leaseID); err != nil {
		// UNIQUE(attempt_id) on leases is the hard backstop against double
		// dispatch. If it ever fires we want the error, not a silent retry.
		return nil, false, fmt.Errorf("insert lease for attempt %s: %w", cand.ID, err)
	}

	// The CAS guard. `AND state = 'PENDING'` is what makes double dispatch
	// impossible: the second writer's UPDATE matches zero rows.
	tag, err := tx.Exec(ctx, `
UPDATE task_attempts
   SET state = 'DISPATCHED', worker_id = $2, lease_id = $3,
       dispatched_at = $4, pending_reason = NULL,
       state_version = state_version + 1
 WHERE id = $1 AND state = 'PENDING'`,
		cand.ID, worker.ID, leaseID, now)
	if err != nil {
		return nil, false, fmt.Errorf("cas attempt to DISPATCHED: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, false, nil
	}

	if key := cand.Spec.ConcurrencyKey; key != "" && cand.Spec.ConcurrencyLimit > 0 {
		if _, err := tx.Exec(ctx,
			`INSERT INTO concurrency_slots (key, attempt_id, acquired_at) VALUES ($1,$2,$3)
			 ON CONFLICT (key, attempt_id) DO NOTHING`,
			key, cand.ID, now,
		); err != nil {
			return nil, false, fmt.Errorf("acquire concurrency slot: %w", err)
		}
	}

	if err := appendEvent(ctx, tx, cand.RunID, cand.NodeID, &cand.ID,
		domain.EventAttemptDispatched, domain.ActorScheduler, map[string]any{
			"worker_id":        worker.ID.String(),
			"worker_hostname":  worker.Hostname,
			"lease_id":         leaseID.String(),
			"lease_expires_at": expiresAt.UTC().Format(time.RFC3339Nano),
			"attempt_number":   cand.AttemptNumber,
			// Keeps its historical meaning (lower is tighter); the combined
			// ADR-0029 score, higher is better, is placement_score.
			"best_fit_score":    domain.BestFitScore(worker.Capacity, worker.Committed, reserved),
			"placement_score":   worker.Score,
			"shared_gpu_device": sharedDeviceOf(reserved),
		}); err != nil {
		return nil, false, err
	}

	// Deadlines are computed from CONTROL-PLANE time (invariant 7): worker
	// clocks are never authoritative, so a worker with a skewed clock cannot
	// extend or shorten its own deadline.
	if ms := cand.Spec.ScheduleToStartMs; ms > 0 {
		if err := insertTimer(ctx, tx, domain.TimerScheduleToStart, cand.ID,
			now.Add(time.Duration(ms)*time.Millisecond), nil); err != nil {
			return nil, false, err
		}
	}
	if ms := cand.Spec.StartToCloseMs; ms > 0 {
		if err := insertTimer(ctx, tx, domain.TimerStartToClose, cand.ID,
			now.Add(time.Duration(ms)*time.Millisecond), nil); err != nil {
			return nil, false, err
		}
	}

	return &DispatchResult{
		AttemptID: cand.ID, RunID: cand.RunID, NodeID: cand.NodeID,
		WorkerID: worker.ID, LeaseToken: token, LeaseID: leaseID, ExpiresAt: expiresAt,
	}, true, nil
}

// markBlocked records the structured pending_reason and leaves the attempt
// PENDING (§5.3 step 3).
//
// The state is untouched on purpose: failing an attempt because the cluster is
// momentarily full would spend its retry budget on a capacity problem and then
// report a task failure for what is actually an operations issue.
func (s *SchedulerStore) markBlocked(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, reason string) error {
	// state_version is NOT bumped: pending_reason is an annotation, not a state
	// transition, and bumping it would make optimistic-concurrency readers
	// elsewhere think the attempt had moved.
	if _, err := tx.Exec(ctx,
		`UPDATE task_attempts SET pending_reason = $2 WHERE id = $1 AND state = 'PENDING'`,
		attemptID, reason,
	); err != nil {
		return fmt.Errorf("record pending_reason: %w", err)
	}
	return nil
}

// newLeaseToken mints the opaque fencing token.
//
// crypto/rand, not math/rand: the token is a capability. A worker that could
// predict another attempt's token could forge reports for work it was never
// assigned, defeating the fencing that ADR-0010 exists to provide.
func newLeaseToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate lease token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// sharedDeviceOf names the shared GPU device a reservation holds, "" for none.
func sharedDeviceOf(v *nexusv1.ResourceVector) string {
	d, _ := domain.SharedDevice(v)
	return d
}
