// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Memoization (§6.4), the DAG engine's I/O half.
//
// There is no cache TABLE. The cache is the set of SUCCEEDED attempts that
// carry a cache_key -- the rows already exist, already hold the output_ref the
// hit must reuse, and are already indexed for exactly this lookup
// (idx_attempts_cache, 0001). A separate table would be a second copy of the
// result reference that could disagree with the first, and the disagreement
// would surface as a downstream task reading the wrong input.
//
// What the attempts cannot express is an operator saying "stop trusting these",
// which is what memo_invalidations is for: an append-only cutoff.

// MemoQuery is one lookup.
type MemoQuery struct {
	Namespace string
	CacheKey  string
	TaskRef   string
	Version   int32
	TTL       time.Duration
	Now       time.Time
}

// MemoHit is a reusable prior result.
type MemoHit struct {
	SourceAttemptID string
	SourceRunID     string
	SourceNodeID    string
	OutputRef       []byte
	EndedAt         time.Time
}

// LookupMemo returns the newest eligible source for a cache key, or nil.
//
// Eligible means ALL of:
//   - SUCCEEDED. A failed, timed-out, lost or cancelled result is never reused:
//     memoizing a failure would turn one transient error into a permanent one.
//   - executed, not itself a hit (cache_hit_of IS NULL). Otherwise every hit
//     restamps the entry's age and nothing ever expires.
//   - ended within the TTL, measured from CONTROL-PLANE time (invariant 7).
//   - ended after the newest matching operator invalidation.
//   - in the same tenant as the requester. The namespace is already part of the
//     key; checking tenancy again in the predicate means a key collision -- or a
//     bug in the derivation -- still cannot hand one tenant another's output_ref,
//     whose blob lives under the other tenant's result-store prefix.
//
// Concurrency: this is a read, and correctness does not depend on it being
// serialized. Two identical runs that both miss both execute (at-least-once, and
// the task was declared pure to be memoizable at all); whichever finishes first
// becomes the source for later lookups. A lookup can never observe a source that
// is not yet SUCCEEDED, because the state and the output_ref commit together in
// the scheduler's ApplyResult transaction.
func (s *DAGEngineStore) LookupMemo(ctx context.Context, q MemoQuery) (*MemoHit, error) {
	if q.CacheKey == "" {
		return nil, nil
	}
	notBefore := q.Now.Add(-q.TTL)
	const sql = `
WITH cutoff AS (
  SELECT max(invalidated_at) AS at
    FROM memo_invalidations
   WHERE namespace = $1
     AND (cache_key = $2
          OR (task_ref = $3 AND (task_version IS NULL OR task_version = $4)))
)
SELECT a.id::text, a.run_id::text, a.node_id, a.output_ref, a.ended_at
  FROM task_attempts a
  JOIN workflow_runs r ON r.id = a.run_id
 WHERE a.cache_key = $2
   AND a.state = 'SUCCEEDED'
   AND a.cache_hit_of IS NULL
   AND a.ended_at IS NOT NULL
   AND a.ended_at > $5
   AND a.ended_at > COALESCE((SELECT at FROM cutoff), '-infinity'::timestamptz)
   AND r.namespace = $1
   AND a.tenant_id = (SELECT id FROM tenants WHERE namespace = $1)
 ORDER BY a.ended_at DESC
 LIMIT 1`
	var h MemoHit
	err := s.pool.QueryRow(ctx, sql, q.Namespace, q.CacheKey, q.TaskRef, q.Version, notBefore).
		Scan(&h.SourceAttemptID, &h.SourceRunID, &h.SourceNodeID, &h.OutputRef, &h.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memo lookup %s: %w", q.CacheKey, err)
	}
	return &h, nil
}

// MemoInvalidation is an operator's request to stop reusing results.
type MemoInvalidation struct {
	Namespace   string
	TaskRef     string
	TaskVersion *int32
	CacheKey    string
	Reason      string
	Actor       string
}

// InvalidateMemo records an invalidation cutoff and returns its id and time.
//
// It deletes and edits nothing: terminal attempts are never mutated (invariant
// 9), and the attempt that produced a now-distrusted result is still the true
// record of what ran. The cutoff simply stops it from being REUSED.
func (s *DAGEngineStore) InvalidateMemo(ctx context.Context, inv MemoInvalidation) (int64, time.Time, error) {
	if inv.TaskRef == "" && inv.CacheKey == "" {
		return 0, time.Time{}, errors.New("memo invalidation needs a task_ref or a cache_key")
	}
	if inv.TaskVersion != nil && inv.TaskRef == "" {
		return 0, time.Time{}, errors.New("memo invalidation by version needs a task_ref")
	}
	var ref, key *string
	if inv.TaskRef != "" {
		ref = &inv.TaskRef
	}
	if inv.CacheKey != "" {
		key = &inv.CacheKey
	}
	var id int64
	var at time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO memo_invalidations (namespace, task_ref, task_version, cache_key, reason, actor)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, invalidated_at`,
		inv.Namespace, ref, inv.TaskVersion, key, inv.Reason, inv.Actor).Scan(&id, &at)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("record memo invalidation in %s: %w", inv.Namespace, err)
	}
	return id, at, nil
}
