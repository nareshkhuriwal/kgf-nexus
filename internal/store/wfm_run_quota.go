// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
)

// ErrRunQuotaExceeded is the sentinel for a namespace's run-level quota
// (ADR-0021). The concrete error is *RunQuotaError.
var ErrRunQuotaExceeded = errors.New("namespace run quota exceeded")

// runQuotaViewPresent caches that migration 0027's view exists.
var runQuotaViewPresent atomic.Bool

// RunQuotaError says which run-level quota refused a run creation.
type RunQuotaError struct {
	Namespace string
	// Dimension is platform.DimActiveRuns or platform.DimRunsPerHour.
	Dimension string
	Used      int64
	Limit     int64
	// RetryAfter is set for the hourly rate: how long until enough of the
	// window ages out for one more run. Zero for active runs, which free up
	// when runs finish, not on a clock.
	RetryAfter time.Duration
}

func (e *RunQuotaError) Error() string {
	return fmt.Sprintf("namespace_quota_exceeded: %s (used %d of %d)", e.Dimension, e.Used, e.Limit)
}

// Is makes errors.Is(err, ErrRunQuotaExceeded) true.
func (e *RunQuotaError) Is(target error) bool { return target == ErrRunQuotaExceeded }

// wfmCheckRunQuota enforces max_active_runs and max_runs_per_hour for the
// namespace, inside the creating transaction (workflow_runs is owned by the
// workflow-manager, ADR-0011).
//
// When either limit is set it takes a per-tenant advisory lock first, so two
// concurrent creations cannot both count N-1 and both insert; the lock is held
// until the caller commits the insert. It is taken only here, after the
// per-workflow max_active_runs lock in WFMCreateRun, so the two locks are
// always acquired in one order and cannot deadlock. A namespace with neither
// limit takes no lock and pays one indexed lookup that returns no row.
func wfmCheckRunQuota(ctx context.Context, tx pgx.Tx, namespace string) error {
	if !runQuotaViewPresent.Load() {
		// A workflow-manager may start before migration 0027 during a rollout
		// (ADR-0011 rule 5: one schema version of skew). Referencing the view
		// then would abort every run creation. Checked until it is seen once,
		// then never again: a migration is not reverted under a running fleet.
		var present bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass('namespace_quota_limits') IS NOT NULL`).Scan(&present); err != nil {
			return fmt.Errorf("detect migration 0027: %w", err)
		}
		if !present {
			return nil
		}
		runQuotaViewPresent.Store(true)
	}
	var maxActive, maxPerHour int64
	err := tx.QueryRow(ctx, `
SELECT q.max_active_runs, q.max_runs_per_hour
  FROM namespace_quota_limits q
 CROSS JOIN LATERAL (SELECT pg_advisory_xact_lock($2, hashtext(q.tenant_id::text))) l
 WHERE q.namespace = $1
   AND (q.max_active_runs > 0 OR q.max_runs_per_hour > 0)`,
		namespace, quotaLockClassRunCreate).Scan(&maxActive, &maxPerHour)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read run quotas: %w", err)
	}
	if maxActive > 0 {
		var active int64
		if err := tx.QueryRow(ctx, `
SELECT count(*) FROM workflow_runs
 WHERE namespace = $1 AND state IN ('PENDING','RUNNING','PAUSING','PAUSED','CANCELLING')`,
			namespace).Scan(&active); err != nil {
			return fmt.Errorf("count active runs of namespace: %w", err)
		}
		if active >= maxActive {
			return &RunQuotaError{Namespace: namespace, Dimension: platform.DimActiveRuns, Used: active, Limit: maxActive}
		}
	}
	if maxPerHour > 0 {
		// Database time throughout, so every replica sees one window
		// (invariant 7).
		var created int64
		if err := tx.QueryRow(ctx, `
SELECT count(*) FROM workflow_runs
 WHERE namespace = $1 AND created_at > now() - interval '1 hour'`,
			namespace).Scan(&created); err != nil {
			return fmt.Errorf("count runs created in the last hour: %w", err)
		}
		if created >= maxPerHour {
			// One more run fits once (created - limit + 1) of the window's runs
			// have aged out, i.e. when the (created - limit + 1)th oldest does.
			var wait float64
			if err := tx.QueryRow(ctx, `
SELECT GREATEST(1, ceil(EXTRACT(EPOCH FROM (created_at + interval '1 hour' - now()))))::float8
  FROM workflow_runs
 WHERE namespace = $1 AND created_at > now() - interval '1 hour'
 ORDER BY created_at
 OFFSET $2 LIMIT 1`, namespace, created-maxPerHour).Scan(&wait); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("compute run quota retry-after: %w", err)
			}
			return &RunQuotaError{Namespace: namespace, Dimension: platform.DimRunsPerHour, Used: created,
				Limit: maxPerHour, RetryAfter: time.Duration(wait) * time.Second}
		}
	}
	return nil
}

// runQuotaErrorReason identifies a run-quota refusal in a gRPC status's
// ErrorInfo detail, so it survives the workflow-manager -> gateway hop that
// otherwise reduces every error to a code and a string.
const runQuotaErrorReason = "NAMESPACE_QUOTA_EXCEEDED"

// RunQuotaStatus renders a RunQuotaError as a gRPC status carrying the
// dimension and numbers (ErrorInfo) and, for the hourly rate, the delay
// (RetryInfo).
func RunQuotaStatus(op string, q *RunQuotaError) error {
	st := status.Newf(codes.ResourceExhausted, "%s: %v", op, q)
	details := []protoadapt.MessageV1{&errdetails.ErrorInfo{
		Reason: runQuotaErrorReason,
		Domain: "nexus",
		Metadata: map[string]string{
			"namespace": q.Namespace, "dimension": q.Dimension,
			"used": strconv.FormatInt(q.Used, 10), "limit": strconv.FormatInt(q.Limit, 10),
		},
	}}
	if q.RetryAfter > 0 {
		details = append(details, &errdetails.RetryInfo{RetryDelay: durationpb.New(q.RetryAfter)})
	}
	if withDetails, err := st.WithDetails(details...); err == nil {
		st = withDetails
	}
	return st.Err()
}

// RunQuotaFromStatus recovers a RunQuotaError from a status built by
// RunQuotaStatus, or returns nil when the status is not one.
func RunQuotaFromStatus(st *status.Status) *RunQuotaError {
	var q *RunQuotaError
	for _, d := range st.Details() {
		switch v := d.(type) {
		case *errdetails.ErrorInfo:
			if v.GetReason() != runQuotaErrorReason {
				continue
			}
			md := v.GetMetadata()
			used, _ := strconv.ParseInt(md["used"], 10, 64)
			limit, _ := strconv.ParseInt(md["limit"], 10, 64)
			if q == nil {
				q = &RunQuotaError{}
			}
			q.Namespace, q.Dimension, q.Used, q.Limit = md["namespace"], md["dimension"], used, limit
		case *errdetails.RetryInfo:
			if q == nil {
				q = &RunQuotaError{}
			}
			q.RetryAfter = v.GetRetryDelay().AsDuration()
		}
	}
	if q == nil || q.Dimension == "" {
		return nil
	}
	return q
}
