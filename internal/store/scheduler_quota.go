// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
)

// Namespace quota enforcement in the claim (ADR-0021).
//
// The correctness argument in one paragraph. Nothing the claim already locks is
// per-namespace: two replicas dispatching two attempts of one namespace to two
// different workers share no row, so a usage check alone would let both pass
// and together exceed the quota. A namespace WITH concurrent limits therefore
// takes a transaction-scoped advisory lock keyed on its tenant before reading
// its usage. Every dispatch of that namespace holds the lock from before its
// usage read until its COMMIT, and the lock is released by that same commit, so
// the next holder's usage read -- a fresh READ COMMITTED statement taken after
// acquiring the lock -- already sees the previous dispatch. Releases do not take
// the lock: they only lower usage, and a usage read that is a moment too high is
// conservative. A namespace WITHOUT concurrent limits takes no lock and reads no
// usage.
//
// Deadlock freedom comes from WHERE the lock is taken:
//
//   - The pull path knows the tenant (the polling worker's) before it locks
//     anything, so it waits for the lock as its FIRST statement. A waiter holds
//     no lock at all, so no wait cycle can pass through this one.
//   - The push path learns the tenant only after it has locked the candidate
//     attempt and the worker rows, so it only ever TRIES the lock. A namespace
//     another replica is dispatching right now is skipped for this cycle.

// Advisory lock classes: the first key of the two-key form. Distinct from each
// other and from the single-key locks elsewhere (Postgres keeps the one- and
// two-key spaces apart), so a claim never waits behind a run creation.
const (
	quotaLockClassDispatch  int32 = 0x4E585131 // "NXQ1": concurrent quotas, the claim
	quotaLockClassRunCreate int32 = 0x4E585132 // "NXQ2": run-level quotas, run creation
)

// quotaLimitsCols is the select list of namespace_quota_limits the claim needs.
const quotaLimitsCols = `q.tenant_id, q.max_inflight_attempts, q.max_cpu_millis,
       q.max_memory_bytes, q.max_gpu_count, q.max_custom`

// quotaHasConcurrent filters namespace_quota_limits to namespaces with at least
// one limit the claim enforces, so an unlimited namespace yields no row.
const quotaHasConcurrent = `(q.max_inflight_attempts > 0 OR q.max_cpu_millis > 0 OR
        q.max_memory_bytes > 0 OR q.max_gpu_count > 0 OR q.max_custom <> '{}'::jsonb)`

// namespaceQuota is a namespace's concurrent limits, read under its lock.
type namespaceQuota struct {
	tenantID uuid.UUID
	limits   platform.Quotas
}

func scanNamespaceQuota(row pgx.Row, extra ...any) (*namespaceQuota, error) {
	var nq namespaceQuota
	var custom []byte
	dest := append([]any{&nq.tenantID, &nq.limits.MaxInflightAttempts, &nq.limits.MaxCPUMillis,
		&nq.limits.MaxMemoryBytes, &nq.limits.MaxGPUCount, &custom}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	if len(custom) > 0 {
		// The view emits only positive integers here; a decode failure would be
		// a view bug, and failing open (no custom limits) is the documented
		// direction for a malformed quota.
		_ = json.Unmarshal(custom, &nq.limits.MaxCustom)
	}
	return &nq, nil
}

// lockQuotaForWorker is the pull path's acquisition: it WAITS for the polling
// worker's namespace lock, and must be the claim transaction's first statement
// (see the file comment). Returns nil when the namespace has no concurrent
// limits, in which case no lock was taken.
//
// The lock is taken in a LATERAL over the filtered view, so it is acquired only
// for a namespace that has limits: the unlimited namespace pays one indexed
// lookup that returns no row.
func (s *SchedulerStore) lockQuotaForWorker(ctx context.Context, tx pgx.Tx, workerID uuid.UUID) (*namespaceQuota, error) {
	if !s.cfg.Quotas || s.quotasMissing {
		return nil, nil
	}
	nq, err := scanNamespaceQuota(tx.QueryRow(ctx, `
SELECT `+quotaLimitsCols+`
  FROM namespace_quota_limits q
 CROSS JOIN LATERAL (SELECT pg_advisory_xact_lock($2, hashtext(q.tenant_id::text))) l
 WHERE q.tenant_id = (SELECT tenant_id FROM workers WHERE id = $1)
   AND `+quotaHasConcurrent,
		workerID, quotaLockClassDispatch))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock namespace quota for worker %s: %w", workerID, err)
	}
	return nq, nil
}

// tryLockQuota is the push path's acquisition: it never waits. Returns
// (nil, true) for an unlimited namespace, (quota, true) when the lock is held,
// and (quota, false) when another transaction holds it right now.
func (s *SchedulerStore) tryLockQuota(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (*namespaceQuota, bool, error) {
	if !s.cfg.Quotas || s.quotasMissing {
		return nil, true, nil
	}
	var got bool
	nq, err := scanNamespaceQuota(tx.QueryRow(ctx, `
SELECT `+quotaLimitsCols+`, pg_try_advisory_xact_lock($2, hashtext(q.tenant_id::text))
  FROM namespace_quota_limits q
 WHERE q.tenant_id = $1
   AND `+quotaHasConcurrent,
		tenantID, quotaLockClassDispatch), &got)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("try namespace quota lock for tenant %s: %w", tenantID, err)
	}
	return nq, got, nil
}

// namespaceInflightUsage sums a namespace's DISPATCHED/RUNNING attempts across
// every queue, over idx_attempts_inflight_tenant (migration 0027). The resource
// request is the reservation: commitDispatch reserves exactly it.
//
// JSON values are read through ->> so both writers' spellings (numbers from
// this package, strings from protojson) sum correctly, and a custom map that is
// JSON null rather than an object contributes nothing instead of raising.
const namespaceInflightUsageSQL = `
WITH f AS MATERIALIZED (
  SELECT resource_request AS r
    FROM task_attempts
   WHERE tenant_id = $1 AND state IN ('DISPATCHED','RUNNING')
)
SELECT (SELECT count(*) FROM f),
       (SELECT COALESCE(sum(COALESCE((r ->> 'cpu_millis')::bigint, 0)), 0)::bigint FROM f),
       (SELECT COALESCE(sum(COALESCE((r ->> 'memory_bytes')::bigint, 0)), 0)::bigint FROM f),
       (SELECT COALESCE(sum(COALESCE((r ->> 'gpu_count')::bigint, 0)), 0)::bigint FROM f),
       (SELECT COALESCE(jsonb_object_agg(k, t), '{}'::jsonb)
          FROM (SELECT e.key AS k, sum(e.value::numeric)::bigint AS t
                  FROM f, jsonb_each_text(CASE WHEN jsonb_typeof(f.r -> 'custom') = 'object'
                                               THEN f.r -> 'custom' ELSE '{}'::jsonb END) e
                 GROUP BY e.key) c)`

// NamespaceInflightUsage is the current concurrent usage of a namespace. Read by
// the claim under the quota lock and by the API without it.
func NamespaceInflightUsage(ctx context.Context, q GWQuerier, tenantID uuid.UUID) (platform.QuotaUsage, error) {
	var u platform.QuotaUsage
	var custom []byte
	if err := q.QueryRow(ctx, namespaceInflightUsageSQL, tenantID).Scan(
		&u.InflightAttempts, &u.CPUMillis, &u.MemoryBytes, &u.GPUCount, &custom); err != nil {
		return u, fmt.Errorf("read in-flight usage of tenant %s: %w", tenantID, err)
	}
	u.Custom = map[string]int64{}
	if len(custom) > 0 {
		if err := json.Unmarshal(custom, &u.Custom); err != nil {
			return u, fmt.Errorf("decode custom usage: %w", err)
		}
	}
	return u, nil
}

// quotaViolation checks one candidate against its namespace's limits. It must
// be called with the namespace's quota lock held. nil means it may dispatch.
func (s *SchedulerStore) quotaViolation(
	ctx context.Context, tx pgx.Tx, nq *namespaceQuota, cand *Candidate,
) (*platform.QuotaViolation, error) {
	used, err := NamespaceInflightUsage(ctx, tx, nq.tenantID)
	if err != nil {
		return nil, err
	}
	req := cand.ResourceRequest
	v, ok := nq.limits.CheckConcurrent(used, platform.QuotaRequest{
		CPUMillis:   req.GetCpuMillis(),
		MemoryBytes: req.GetMemoryBytes(),
		GPUCount:    int64(req.GetGpuCount()),
		Custom:      req.GetCustom(),
	})
	if ok {
		return nil, nil
	}
	return &v, nil
}

// QuotaSchemaStatus reports whether migration 0027's objects exist, for the
// startup log line.
type QuotaSchemaStatus struct {
	Quotas   bool // namespace_quota_limits exists and ClaimConfig.Quotas is on
	Metering bool // usage_ledger exists
}

// DetectQuotaSchema checks for migration 0027 and turns off what it cannot
// find. Call once at startup, before the store serves claims.
//
// Detected rather than assumed for EnableWindowedClaim's reason: a scheduler
// can start before the migration job during a rollout, and assuming the objects
// exist would fail every claim (the view) and every completion (the ledger) --
// an outage -- whereas running without them only defers enforcement and
// metering until the restart after the migration. The metering gap is real
// (attempts ending in that window have no ledger row) and is logged.
func (s *SchedulerStore) DetectQuotaSchema(ctx context.Context) (QuotaSchemaStatus, error) {
	var view, ledger bool
	if err := s.pool.QueryRow(ctx,
		`SELECT to_regclass('namespace_quota_limits') IS NOT NULL, to_regclass('usage_ledger') IS NOT NULL`,
	).Scan(&view, &ledger); err != nil {
		return QuotaSchemaStatus{}, fmt.Errorf("detect migration 0027: %w", err)
	}
	s.quotasMissing = !view
	s.meteringMissing = !ledger
	return QuotaSchemaStatus{Quotas: view && s.cfg.Quotas, Metering: ledger}, nil
}

// QuotaObserver receives the scheduler's quota and metering events, AFTER the
// transaction that produced them committed, for metrics. Implementations must
// be cheap and must not block: they run on the claim and report paths.
type QuotaObserver interface {
	// QuotaBlocked is one attempt left PENDING by a concurrent quota.
	QuotaBlocked(dimension string)
	// UsageRecorded is one usage-ledger row written, by attempt outcome.
	UsageRecorded(outcome string)
}

// SetObserver installs the metrics observer. Call once at startup, before the
// store serves claims.
func (s *SchedulerStore) SetObserver(o QuotaObserver) { s.observer = o }

func (s *SchedulerStore) notifyQuotaBlocked(dims []string) {
	if s.observer == nil {
		return
	}
	for _, d := range dims {
		s.observer.QuotaBlocked(d)
	}
}

// PlacementObserver is an optional extension of the observer for placement
// events (ADR-0029 §4). Checked by type assertion so existing observers keep
// compiling.
type PlacementObserver interface {
	// PlacementDeferred is one pull-path candidate left for a better worker.
	PlacementDeferred()
}

func (s *SchedulerStore) notifyDeferred() {
	if o, ok := s.observer.(PlacementObserver); ok {
		o.PlacementDeferred()
	}
}

func (s *SchedulerStore) notifyUsage(outcome string) {
	if s.observer != nil && outcome != "" {
		s.observer.UsageRecorded(outcome)
	}
}
