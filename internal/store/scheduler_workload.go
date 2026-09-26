// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// The workload API's control-plane half (ADR-0032, migration 0036): usage
// units, rate-limit permits and checkpoint pointers, all written by the
// scheduler because it owns the attempt's lifecycle (ADR-0011).
//
// Every write here stores data a worker supplied, so, like RecordProgress, it
// re-checks the lease INSIDE its own transaction: the gateway's fence is a
// moment earlier, and the lease can be reclaimed in between.

// ErrWorkloadUnavailable means migration 0036 is not applied, so the workload
// API cannot be served. The RPC layer answers Unavailable; the agent retries
// or tells the workload 503.
var ErrWorkloadUnavailable = errors.New("workload API unavailable: migration 0036 is not applied")

// ErrInvalidWorkloadReport wraps a validation failure of worker-supplied data.
// Nothing was written. It is a protocol outcome, not a transport error: a
// report that failed validation will fail it again.
var ErrInvalidWorkloadReport = errors.New("invalid workload report")

// ReasonStaleCheckpoint is the Ack reason for a checkpoint older than the one
// already stored for the idempotency key.
const ReasonStaleCheckpoint = "stale_checkpoint"

// WorkloadSchemaStatus reports whether migration 0036 is present.
type WorkloadSchemaStatus struct{ Present bool }

// DetectWorkloadSchema records whether migration 0036 is applied. Call once at
// startup, before the store serves reports (the DetectQuotaSchema pattern): a
// scheduler rolled out ahead of the migration must keep ending attempts with
// the ledger statement the database can run.
func (s *SchedulerStore) DetectWorkloadSchema(ctx context.Context) (WorkloadSchemaStatus, error) {
	var present bool
	if err := s.pool.QueryRow(ctx, `
SELECT to_regclass('attempt_usage') IS NOT NULL
   AND to_regclass('task_checkpoints') IS NOT NULL
   AND to_regclass('namespace_rate_limits') IS NOT NULL
   AND EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_name = 'task_nodes' AND column_name = 'free_retries')`,
	).Scan(&present); err != nil {
		return WorkloadSchemaStatus{}, fmt.Errorf("detect migration 0036: %w", err)
	}
	s.workloadMissing = !present
	return WorkloadSchemaStatus{Present: present}, nil
}

// freeRetryCap is the effective cap on uncharged RATE_LIMITED retries.
func (s *SchedulerStore) freeRetryCap() int32 {
	switch n := s.cfg.RateLimitedFreeRetries; {
	case n < 0:
		return 0
	case n == 0:
		return domain.DefaultRateLimitedFreeRetries
	default:
		return n
	}
}

// workloadAttempt is what a fenced workload write needs about its attempt.
type workloadAttempt struct {
	RunID          uuid.UUID
	NodeID         string
	TenantID       uuid.UUID
	Namespace      string
	AttemptNumber  int32
	IdempotencyKey string
	Epoch          int32
}

// fenceWorkload loads the attempt, requires it in flight with a live lease
// matching token, and returns nil (stale) otherwise.
//
// With lock, the attempt row is taken FOR KEY SHARE, bounded by the attempt
// lock timeout, and the state and lease are read AFTER the lock. That is what
// orders this write against the attempt's terminal transition, which locks the
// row FOR UPDATE first (ADR-0032 §2): either this commits first and the
// terminal transaction sees it, or this waits and then finds the attempt ended.
// The statement is the transaction's first lock, so the wait cannot close a
// deadlock cycle. Without lock it is a plain read, for writes whose racing a
// terminal transition is harmless (a permit taken by an attempt that is ending
// is merely spent).
func (s *SchedulerStore) fenceWorkload(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, token string, lock bool) (*workloadAttempt, error) {
	q := `
SELECT a.run_id, a.node_id, a.tenant_id, r.namespace, a.attempt_number,
       a.idempotency_key, l.token, l.epoch, (l.expires_at <= now())
  FROM task_attempts a
  JOIN workflow_runs r ON r.id = a.run_id
  LEFT JOIN leases l ON l.attempt_id = a.id
 WHERE a.id = $1
   AND a.state IN ('DISPATCHED','RUNNING')`
	if lock {
		q += "\n FOR KEY SHARE OF a"
	}
	var (
		w       workloadAttempt
		tok     *string
		epoch   *int32
		expired *bool
	)
	read := func() error {
		return tx.QueryRow(ctx, q, attemptID).Scan(&w.RunID, &w.NodeID, &w.TenantID, &w.Namespace,
			&w.AttemptNumber, &w.IdempotencyKey, &tok, &epoch, &expired)
	}
	var err error
	if lock {
		err = s.lockAttemptForTransition(ctx, tx, read)
	} else {
		err = read()
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fence attempt %s: %w", attemptID, err)
	}
	// Same test as GatewayVerifyLease, in constant time: the token is a
	// credential, and an expired-but-matching token is stale.
	if tok == nil || (expired != nil && *expired) || token == "" ||
		subtle.ConstantTimeCompare([]byte(*tok), []byte(token)) != 1 {
		return nil, nil
	}
	w.Epoch = *epoch
	return &w, nil
}

// ── Usage units ──────────────────────────────────────────────────────────────

// UsageOutcome says what RecordUsage did.
type UsageOutcome struct {
	// StaleLease: the token is not the attempt's live lease, or the attempt
	// has ended. Nothing was written; the worker should stop reporting.
	StaleLease bool
	// Changed: at least one total grew.
	Changed bool
	// Totals are the attempt's stored totals after the merge.
	Totals map[string]int64
	// Growth is how much the totals grew, summed by domain.UsageKind (for
	// metrics: kinds are a closed set, keys are not).
	Growth map[string]int64
}

// RecordUsage merges one cumulative usage snapshot into the attempt's totals
// (per-key max, domain.MergeUsage). Invalid input returns an error wrapping
// ErrInvalidWorkloadReport and writes nothing.
func (s *SchedulerStore) RecordUsage(ctx context.Context, attemptID uuid.UUID, token string,
	units map[string]int64, now time.Time) (UsageOutcome, error) {
	if s.workloadMissing {
		return UsageOutcome{}, ErrWorkloadUnavailable
	}
	if err := domain.ValidateUsage(units); err != nil {
		return UsageOutcome{}, fmt.Errorf("%w: %v", ErrInvalidWorkloadReport, err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return UsageOutcome{}, fmt.Errorf("begin usage tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w, err := s.fenceWorkload(ctx, tx, attemptID, token, true)
	if err != nil {
		return UsageOutcome{}, err
	}
	if w == nil {
		return UsageOutcome{StaleLease: true}, nil
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO attempt_usage (attempt_id, run_id, lease_epoch, reported_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (attempt_id) DO NOTHING`, attemptID, w.RunID, w.Epoch, now); err != nil {
		return UsageOutcome{}, fmt.Errorf("create usage row for %s: %w", attemptID, err)
	}
	var (
		raw         []byte
		storedEpoch int32
	)
	if err := tx.QueryRow(ctx,
		`SELECT units, lease_epoch FROM attempt_usage WHERE attempt_id = $1 FOR UPDATE`,
		attemptID).Scan(&raw, &storedEpoch); err != nil {
		return UsageOutcome{}, fmt.Errorf("lock usage row for %s: %w", attemptID, err)
	}
	if storedEpoch > w.Epoch {
		// A newer lease generation of this attempt already reported.
		return UsageOutcome{StaleLease: true}, nil
	}
	stored := map[string]int64{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &stored); err != nil {
			return UsageOutcome{}, fmt.Errorf("decode stored usage for %s: %w", attemptID, err)
		}
	}
	merged, changed, err := domain.MergeUsage(stored, units)
	if err != nil {
		return UsageOutcome{}, fmt.Errorf("%w: %v", ErrInvalidWorkloadReport, err)
	}
	enc, err := json.Marshal(merged)
	if err != nil {
		return UsageOutcome{}, fmt.Errorf("encode usage: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE attempt_usage
   SET units = $2, lease_epoch = GREATEST(lease_epoch, $3), reports = reports + 1,
       reported_at = $4
 WHERE attempt_id = $1`, attemptID, enc, w.Epoch, now); err != nil {
		return UsageOutcome{}, fmt.Errorf("store usage for %s: %w", attemptID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return UsageOutcome{}, fmt.Errorf("commit usage tx: %w", err)
	}
	growth := map[string]int64{}
	for k, v := range merged {
		if d := v - stored[k]; d > 0 {
			growth[domain.UsageKind(k)] += d
		}
	}
	return UsageOutcome{Changed: changed, Totals: merged, Growth: growth}, nil
}

// ── Rate-limit permits ───────────────────────────────────────────────────────

// Permit request bounds (ADR-0032 §3).
const (
	MaxPermitKeys     = 8
	MaxPermitRequests = 1_000_000
	MaxPermitTokens   = 1_000_000_000
)

// PermitRequest is one AcquirePermits call.
type PermitRequest struct {
	AttemptID uuid.UUID
	Token     string
	Keys      []string
	Requests  int64
	Tokens    int64
	// Settle applies Tokens as a signed correction without a check.
	Settle bool
}

// PermitOutcome says what AcquirePermits did.
type PermitOutcome struct {
	StaleLease bool
	Granted    bool
	// RetryAfter is set when not granted: how long until the request would be.
	RetryAfter time.Duration
	// Unlimited lists the requested keys with no configured limit.
	Unlimited []string
	// Limited is how many keys had a configured limit (for metrics).
	Limited int
}

// normalizePermitRequest validates and canonicalizes (dedupe, sort) the keys.
func normalizePermitRequest(r *PermitRequest) error {
	if len(r.Keys) == 0 || len(r.Keys) > MaxPermitKeys {
		return fmt.Errorf("%w: between 1 and %d rate-limit keys are required", ErrInvalidWorkloadReport, MaxPermitKeys)
	}
	seen := map[string]bool{}
	keys := make([]string, 0, len(r.Keys))
	for _, k := range r.Keys {
		if !domain.ValidRateLimitKey(k) {
			return fmt.Errorf("%w: rate-limit key %q is not <provider> or <provider>/<model>", ErrInvalidWorkloadReport, truncate(k, 64))
		}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	// Sorted: every transaction locks bucket rows in the same order, so two
	// multi-key acquisitions can never deadlock.
	sort.Strings(keys)
	r.Keys = keys
	if r.Settle {
		if r.Requests != 0 || r.Tokens < -MaxPermitTokens || r.Tokens > MaxPermitTokens {
			return fmt.Errorf("%w: a settlement carries tokens only, within ±%d", ErrInvalidWorkloadReport, int64(MaxPermitTokens))
		}
		return nil
	}
	if r.Requests < 0 || r.Requests > MaxPermitRequests || r.Tokens < 0 || r.Tokens > MaxPermitTokens {
		return fmt.Errorf("%w: requests must be in [0, %d] and tokens in [0, %d]", ErrInvalidWorkloadReport,
			int64(MaxPermitRequests), int64(MaxPermitTokens))
	}
	if r.Requests == 0 && r.Tokens == 0 {
		return fmt.Errorf("%w: ask for at least one request or token", ErrInvalidWorkloadReport)
	}
	return nil
}

// AcquirePermits takes (or settles) permits from the namespace's token buckets
// for every requested key, all or none, in one transaction (ADR-0032 §3). It
// never waits for capacity: a denial returns how long to wait, computed from
// the database clock.
func (s *SchedulerStore) AcquirePermits(ctx context.Context, req PermitRequest) (PermitOutcome, error) {
	if s.workloadMissing {
		return PermitOutcome{}, ErrWorkloadUnavailable
	}
	if err := normalizePermitRequest(&req); err != nil {
		return PermitOutcome{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return PermitOutcome{}, fmt.Errorf("begin permit tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w, err := s.fenceWorkload(ctx, tx, req.AttemptID, req.Token, false)
	if err != nil {
		return PermitOutcome{}, err
	}
	if w == nil {
		return PermitOutcome{StaleLease: true}, nil
	}

	limits := map[string]domain.BucketLimit{}
	rows, err := tx.Query(ctx, `
SELECT key, requests_per_second, tokens_per_minute, burst_requests, burst_tokens
  FROM namespace_rate_limits
 WHERE tenant_id = $1 AND key = ANY($2)`, w.TenantID, req.Keys)
	if err != nil {
		return PermitOutcome{}, fmt.Errorf("read rate limits: %w", err)
	}
	for rows.Next() {
		var (
			k string
			l domain.BucketLimit
		)
		if err := rows.Scan(&k, &l.RequestsPerSecond, &l.TokensPerMinute, &l.BurstRequests, &l.BurstTokens); err != nil {
			rows.Close()
			return PermitOutcome{}, fmt.Errorf("scan rate limit: %w", err)
		}
		if l.Limited() {
			limits[k] = l
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PermitOutcome{}, fmt.Errorf("read rate limits: %w", err)
	}

	out := PermitOutcome{Granted: true}
	type locked struct {
		key   string
		limit domain.BucketLimit
		state domain.BucketState
	}
	var held []locked
	var now time.Time
	for _, k := range req.Keys { // sorted
		l, ok := limits[k]
		if !ok {
			out.Unlimited = append(out.Unlimited, k)
			continue
		}
		full := l.FullBucket()
		// The no-op DO UPDATE locks the row and returns its current levels in
		// one statement, creating it full on first use. clock_timestamp() in
		// RETURNING is evaluated after the lock is held, so the refill is
		// measured to the moment this transaction owns the bucket.
		var (
			st        domain.BucketState
			refreshed time.Time
		)
		if err := tx.QueryRow(ctx, `
INSERT INTO rate_limit_buckets (tenant_id, key, request_level, token_level, refreshed_at)
VALUES ($1, $2, $3, $4, clock_timestamp())
ON CONFLICT (tenant_id, key) DO UPDATE SET key = EXCLUDED.key
RETURNING request_level, token_level, refreshed_at, clock_timestamp()`,
			w.TenantID, k, full.Requests, full.Tokens).Scan(&st.Requests, &st.Tokens, &refreshed, &now); err != nil {
			return PermitOutcome{}, fmt.Errorf("lock rate-limit bucket %q: %w", k, err)
		}
		st = l.Refill(st, now.Sub(refreshed).Seconds())
		held = append(held, locked{k, l, st})
	}
	out.Limited = len(held)
	if len(held) == 0 {
		// Nothing is limited; no row was touched.
		return out, tx.Commit(ctx)
	}

	var wait float64
	next := make([]domain.BucketState, len(held))
	for i, h := range held {
		if req.Settle {
			next[i] = h.limit.Settle(h.state, req.Tokens)
			continue
		}
		n, ok, wt := h.limit.Take(h.state, req.Requests, req.Tokens)
		next[i] = n
		if !ok {
			out.Granted = false
			wait = math.Max(wait, wt)
		}
	}
	for i, h := range held {
		if !out.Granted {
			// Store the refilled levels (so the next reader's elapsed time is
			// short) and count the denial; take nothing.
			next[i] = h.state
		}
		grantedReq, grantedTok := int64(0), int64(0)
		if out.Granted && !req.Settle {
			grantedReq, grantedTok = req.Requests, req.Tokens
		}
		denied := 0
		if !out.Granted {
			denied = 1
		}
		if _, err := tx.Exec(ctx, `
UPDATE rate_limit_buckets
   SET request_level = $3, token_level = $4, refreshed_at = GREATEST(refreshed_at, $5),
       granted_requests = granted_requests + $6, granted_tokens = granted_tokens + $7,
       denied = denied + $8
 WHERE tenant_id = $1 AND key = $2`,
			w.TenantID, h.key, next[i].Requests, next[i].Tokens, now,
			grantedReq, grantedTok, denied); err != nil {
			return PermitOutcome{}, fmt.Errorf("update rate-limit bucket %q: %w", h.key, err)
		}
	}
	if !out.Granted {
		// Round up: waking a millisecond early only buys another denial.
		out.RetryAfter = time.Duration(math.Ceil(wait*1000)) * time.Millisecond
		if out.RetryAfter < time.Millisecond {
			out.RetryAfter = time.Millisecond
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return PermitOutcome{}, fmt.Errorf("commit permit tx: %w", err)
	}
	return out, nil
}

// ── Checkpoints ──────────────────────────────────────────────────────────────

// CheckpointSave is one SaveCheckpoint call.
type CheckpointSave struct {
	AttemptID uuid.UUID
	Token     string
	Seq       int64
	Key       string
	SHA256    string
	SizeBytes int64
}

// CheckpointOutcome says what SaveCheckpoint did.
type CheckpointOutcome struct {
	StaleLease bool
	// Superseded: a checkpoint at least as new is already stored.
	Superseded bool
}

var checkpointSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// CheckpointPrefix is the only prefix an attempt may register a checkpoint
// under: its namespace, its idempotency key and its own id, so a key can never
// name another attempt's (or tenant's) object.
func CheckpointPrefix(namespace, idempotencyKey string, attemptID uuid.UUID) string {
	return namespace + "/checkpoints/" + idempotencyKey + "/" + attemptID.String() + "/"
}

// SaveCheckpoint moves the idempotency key's checkpoint pointer to an object
// the agent has already written, if and only if the attempt still owns its
// lease and the checkpoint is newer (attempt number, then seq) than the stored
// one (ADR-0032 §4).
func (s *SchedulerStore) SaveCheckpoint(ctx context.Context, c CheckpointSave, now time.Time) (CheckpointOutcome, error) {
	if s.workloadMissing {
		return CheckpointOutcome{}, ErrWorkloadUnavailable
	}
	if c.Seq <= 0 || c.SizeBytes < 0 || !checkpointSHA256.MatchString(c.SHA256) || len(c.Key) > 1024 ||
		strings.Contains(c.Key, "..") {
		return CheckpointOutcome{}, fmt.Errorf("%w: checkpoint needs seq > 0, size >= 0, a lower-case hex sha256 and a key under 1 KiB",
			ErrInvalidWorkloadReport)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return CheckpointOutcome{}, fmt.Errorf("begin checkpoint tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w, err := s.fenceWorkload(ctx, tx, c.AttemptID, c.Token, true)
	if err != nil {
		return CheckpointOutcome{}, err
	}
	if w == nil {
		return CheckpointOutcome{StaleLease: true}, nil
	}
	prefix := CheckpointPrefix(w.Namespace, w.IdempotencyKey, c.AttemptID)
	if !strings.HasPrefix(c.Key, prefix) || len(c.Key) == len(prefix) {
		return CheckpointOutcome{}, fmt.Errorf("%w: checkpoint key must be under %s", ErrInvalidWorkloadReport, prefix)
	}

	// Forward-only: the conditional DO UPDATE refuses a pointer that is not
	// strictly newer, so a zombie (older attempt) or a reordered save cannot
	// move it back. No row returned = superseded.
	var ok int
	err = tx.QueryRow(ctx, `
INSERT INTO task_checkpoints (idempotency_key, run_id, node_id, attempt_id, attempt_number,
                              lease_epoch, seq, object_key, sha256, size_bytes, saved_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (idempotency_key) DO UPDATE
   SET node_id = EXCLUDED.node_id, attempt_id = EXCLUDED.attempt_id,
       attempt_number = EXCLUDED.attempt_number, lease_epoch = EXCLUDED.lease_epoch,
       seq = EXCLUDED.seq, object_key = EXCLUDED.object_key, sha256 = EXCLUDED.sha256,
       size_bytes = EXCLUDED.size_bytes, saved_at = EXCLUDED.saved_at,
       saves = task_checkpoints.saves + 1
 WHERE (task_checkpoints.attempt_number, task_checkpoints.seq)
     < (EXCLUDED.attempt_number, EXCLUDED.seq)
RETURNING 1`,
		w.IdempotencyKey, w.RunID, w.NodeID, c.AttemptID, w.AttemptNumber, w.Epoch,
		c.Seq, c.Key, c.SHA256, c.SizeBytes, now).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return CheckpointOutcome{Superseded: true}, nil
	}
	if err != nil {
		return CheckpointOutcome{}, fmt.Errorf("store checkpoint for %s: %w", c.AttemptID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CheckpointOutcome{}, fmt.Errorf("commit checkpoint tx: %w", err)
	}
	return CheckpointOutcome{}, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
