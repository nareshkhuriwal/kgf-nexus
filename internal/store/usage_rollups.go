// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Read side of the usage ledger (ADR-0021): rollups for the API and page, and
// the keyset read a billing exporter pulls from. Every reader has SELECT on
// usage_ledger; only the scheduler writes it.

// UsageGroupKeys are the supported group-by dimensions and the SQL that
// computes each key. A fixed map, never interpolated from input: the key names
// are the whitelist.
//
// Attribution is by ended_at in UTC: an attempt counts in the day it ENDED.
var UsageGroupKeys = map[string]string{
	"day":      `to_char(date_trunc('day', ended_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD')`,
	"queue":    `queue`,
	"pool":     `COALESCE(pool, '')`,
	"worker":   `COALESCE(worker_hostname, worker_id::text, '')`,
	"executor": `COALESCE(executor, '')`,
	"outcome":  `outcome`,
	"none":     `'total'`,
}

// MaxUsageRange bounds a rollup's time range so one request cannot aggregate
// the whole history of a large tenant.
const MaxUsageRange = 400 * 24 * time.Hour

// UsageQuery is one rollup request.
type UsageQuery struct {
	From, To time.Time
	GroupBy  string
	// Cursor is the last key of the previous page; "" starts at the beginning.
	Cursor string
	Limit  int
}

// UsageBucket is one group's totals. Quantities are floats for display; the
// exact NUMERIC values are in the ledger rows and the export.
type UsageBucket struct {
	Key                string  `json:"key"`
	Attempts           int64   `json:"attempts"`
	WallSeconds        float64 `json:"wall_seconds"`
	ReservedSeconds    float64 `json:"reserved_seconds"`
	CPUCoreSeconds     float64 `json:"cpu_core_seconds"`
	MemoryGiBSeconds   float64 `json:"memory_gib_seconds"`
	GPUSeconds         float64 `json:"gpu_seconds"`
	MeasuredCPUSeconds float64 `json:"measured_cpu_seconds"`
	// Measured counts the attempts that carried worker metrics; the rest
	// (LOST, cancelled, older agents) contribute nothing to MeasuredCPUSeconds.
	Measured int64 `json:"measured_attempts"`
}

// UsagePage is one page of buckets. NextCursor is empty on the last page.
type UsagePage struct {
	Buckets    []UsageBucket `json:"buckets"`
	NextCursor string        `json:"next_cursor"`
}

// Validate normalizes the query and rejects what it cannot serve.
func (q *UsageQuery) Validate() error {
	if _, ok := UsageGroupKeys[q.GroupBy]; !ok {
		return fmt.Errorf("%w: group_by must be one of day, queue, pool, worker, executor, outcome, none", ErrInvalidRequest)
	}
	if !q.To.After(q.From) {
		return fmt.Errorf("%w: to must be after from", ErrInvalidRequest)
	}
	if q.To.Sub(q.From) > MaxUsageRange {
		return fmt.Errorf("%w: the time range may not exceed %d days", ErrInvalidRequest, int(MaxUsageRange.Hours()/24))
	}
	if q.Limit <= 0 {
		q.Limit = 100
	}
	if q.Limit > 500 {
		q.Limit = 500
	}
	return nil
}

func usageRollup(ctx context.Context, q GWQuerier, keyExpr, where string, args []any, uq UsageQuery) (UsagePage, error) {
	n := len(args)
	sql := fmt.Sprintf(`
SELECT key, count(*),
       COALESCE(sum(wall_seconds), 0)::float8, COALESCE(sum(reserved_seconds), 0)::float8,
       COALESCE(sum(cpu_core_seconds), 0)::float8, COALESCE(sum(memory_gib_seconds), 0)::float8,
       COALESCE(sum(gpu_seconds), 0)::float8,
       COALESCE(sum(measured_cpu_seconds), 0)::float8, count(measured_cpu_seconds)
  FROM (SELECT %s AS key, * FROM usage_ledger
         WHERE %s AND ended_at >= $%d AND ended_at < $%d) x
 WHERE ($%d = '' OR key > $%d)
 GROUP BY key
 ORDER BY key
 LIMIT $%d`, keyExpr, where, n+1, n+2, n+3, n+3, n+4)
	args = append(args, uq.From, uq.To, uq.Cursor, uq.Limit+1)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return UsagePage{}, fmt.Errorf("usage rollup: %w", err)
	}
	defer rows.Close()
	page := UsagePage{Buckets: []UsageBucket{}}
	for rows.Next() {
		var b UsageBucket
		if err := rows.Scan(&b.Key, &b.Attempts, &b.WallSeconds, &b.ReservedSeconds, &b.CPUCoreSeconds,
			&b.MemoryGiBSeconds, &b.GPUSeconds, &b.MeasuredCPUSeconds, &b.Measured); err != nil {
			return UsagePage{}, fmt.Errorf("scan usage bucket: %w", err)
		}
		page.Buckets = append(page.Buckets, b)
	}
	if err := rows.Err(); err != nil {
		return UsagePage{}, fmt.Errorf("usage rollup rows: %w", err)
	}
	// One extra row was requested to learn whether another page exists without
	// a second count query.
	if len(page.Buckets) > uq.Limit {
		page.Buckets = page.Buckets[:uq.Limit]
		page.NextCursor = page.Buckets[len(page.Buckets)-1].Key
	}
	return page, nil
}

// GatewayUsageRollup aggregates ONE namespace's ledger. The namespace is the
// principal's; there is no parameter by which a caller could name another.
func GatewayUsageRollup(ctx context.Context, q GWQuerier, ns string, uq UsageQuery) (UsagePage, error) {
	if err := uq.Validate(); err != nil {
		return UsagePage{}, err
	}
	return usageRollup(ctx, q, UsageGroupKeys[uq.GroupBy],
		// By tenant id, so the (tenant_id, ended_at) index serves the range.
		"tenant_id = (SELECT id FROM tenants WHERE namespace = $1)", []any{ns}, uq)
}

// GatewayPlatformUsageRollup aggregates every namespace, one bucket per
// namespace, for a platform operator.
func GatewayPlatformUsageRollup(ctx context.Context, q GWQuerier, uq UsageQuery) (UsagePage, error) {
	uq.GroupBy = "none"
	if err := uq.Validate(); err != nil {
		return UsagePage{}, err
	}
	return usageRollup(ctx, q, "namespace", "true", nil, uq)
}

// UsageRecord is one ledger row as an exporter sees it.
type UsageRecord struct {
	ID                      int64            `json:"ledger_id"`
	AttemptID               uuid.UUID        `json:"attempt_id"`
	TenantID                uuid.UUID        `json:"tenant_id"`
	Namespace               string           `json:"namespace"`
	RunID                   uuid.UUID        `json:"run_id"`
	NodeID                  string           `json:"node_id"`
	AttemptNumber           int32            `json:"attempt_number"`
	Outcome                 string           `json:"outcome"`
	Queue                   string           `json:"queue"`
	Pool                    *string          `json:"pool"`
	WorkerID                *uuid.UUID       `json:"worker_id"`
	WorkerHostname          *string          `json:"worker_hostname"`
	Executor                *string          `json:"executor"`
	CPUMillis               int64            `json:"cpu_millis"`
	MemoryBytes             int64            `json:"memory_bytes"`
	GPUCount                int32            `json:"gpu_count"`
	GPUKind                 *string          `json:"gpu_kind"`
	Custom                  map[string]int64 `json:"custom"`
	DispatchedAt            time.Time        `json:"dispatched_at"`
	StartedAt               *time.Time       `json:"started_at"`
	EndedAt                 time.Time        `json:"ended_at"`
	WallSeconds             string           `json:"wall_seconds"`
	ReservedSeconds         string           `json:"reserved_seconds"`
	CPUCoreSeconds          string           `json:"cpu_core_seconds"`
	MemoryGiBSeconds        string           `json:"memory_gib_seconds"`
	GPUSeconds              string           `json:"gpu_seconds"`
	MeasuredCPUSeconds      *float64         `json:"measured_cpu_seconds"`
	MeasuredPeakMemoryBytes *int64           `json:"measured_peak_memory_bytes"`
	MeasuredWallMs          *int64           `json:"measured_wall_ms"`
	MeasuredOOMKilled       *bool            `json:"measured_oom_killed"`
	MeasuredExitCode        *int32           `json:"measured_exit_code"`
	RecordedAt              time.Time        `json:"recorded_at"`
}

// UsageExportLag is how old a row must be before UsageRecordsAfter returns it.
//
// Ledger ids are drawn at INSERT and become visible at COMMIT, which is not the
// same order (the reason 0016 marks outbox rows individually). A cursor over id
// that read a just-committed high id could skip a lower id still in flight. The
// terminal transitions that write the ledger are short, so holding back the
// newest rows by a margin closes that gap in practice; an exporter must still
// treat delivery as at-least-once and deduplicate on attempt_id, which is
// unique.
const UsageExportLag = 30 * time.Second

// UsageRecordsAfter returns up to limit ledger rows with id > afterID, in id
// order, older than UsageExportLag. The caller persists the last id it
// exported and passes it back.
func UsageRecordsAfter(ctx context.Context, q GWQuerier, afterID int64, limit int) ([]UsageRecord, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := q.Query(ctx, `
SELECT id, attempt_id, tenant_id, namespace, run_id, node_id, attempt_number, outcome, queue, pool,
       worker_id, worker_hostname, executor, cpu_millis, memory_bytes, gpu_count, gpu_kind, custom,
       dispatched_at, started_at, ended_at,
       wall_seconds::text, reserved_seconds::text, cpu_core_seconds::text, memory_gib_seconds::text,
       gpu_seconds::text, measured_cpu_seconds, measured_peak_memory_bytes, measured_wall_ms,
       measured_oom_killed, measured_exit_code, recorded_at
  FROM usage_ledger
 WHERE id > $1 AND recorded_at < now() - make_interval(secs => $3)
 ORDER BY id
 LIMIT $2`, afterID, limit, UsageExportLag.Seconds())
	if err != nil {
		return nil, fmt.Errorf("read usage records: %w", err)
	}
	defer rows.Close()
	var out []UsageRecord
	for rows.Next() {
		var r UsageRecord
		var custom []byte
		if err := rows.Scan(&r.ID, &r.AttemptID, &r.TenantID, &r.Namespace, &r.RunID, &r.NodeID,
			&r.AttemptNumber, &r.Outcome, &r.Queue, &r.Pool, &r.WorkerID, &r.WorkerHostname, &r.Executor,
			&r.CPUMillis, &r.MemoryBytes, &r.GPUCount, &r.GPUKind, &custom,
			&r.DispatchedAt, &r.StartedAt, &r.EndedAt,
			&r.WallSeconds, &r.ReservedSeconds, &r.CPUCoreSeconds, &r.MemoryGiBSeconds, &r.GPUSeconds,
			&r.MeasuredCPUSeconds, &r.MeasuredPeakMemoryBytes, &r.MeasuredWallMs,
			&r.MeasuredOOMKilled, &r.MeasuredExitCode, &r.RecordedAt); err != nil {
			return nil, fmt.Errorf("scan usage record: %w", err)
		}
		r.Custom = map[string]int64{}
		if len(custom) > 0 {
			// Values were written from the attempt's resource_request; a
			// protojson-written custom map spells numbers as strings.
			var loose map[string]json.Number
			if err := json.Unmarshal(custom, &loose); err == nil {
				for k, v := range loose {
					n, _ := v.Int64()
					r.Custom[k] = n
				}
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
