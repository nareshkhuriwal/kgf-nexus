// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package client

// Operator endpoints that the gateway already served but nexusctl did not reach:
// run pause/resume, schedules, backfills, worker drain/quarantine/release,
// workflow delete and cache invalidation. Each method maps to exactly one route
// in services/api-gateway/rest; the request bodies mirror the handlers' decode
// structs field for field, so a renamed field there is a compile-visible diff
// here rather than a silently ignored key.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// post sends a JSON body. A nil payload sends "{}" -- the lifecycle routes
// accept an empty object, and some proxies reject a POST with no body at all.
func (c *Client) post(ctx context.Context, path string, payload any) (any, error) {
	body := []byte("{}")
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encoding request: %w", err)
		}
		body = raw
	}
	return c.do(ctx, http.MethodPost, c.urlFor(path, nil), body, nil)
}

func esc(s string) string { return url.PathEscape(s) }

// ── runs ─────────────────────────────────────────────────────────────────────

// PauseRun stops new dispatch for a run; attempts already running finish.
func (c *Client) PauseRun(ctx context.Context, id string) (any, error) {
	return c.post(ctx, "/runs/"+esc(id)+"/pause", nil)
}

// ResumeRun re-enables dispatch for a PAUSED run.
func (c *Client) ResumeRun(ctx context.Context, id string) (any, error) {
	return c.post(ctx, "/runs/"+esc(id)+"/resume", nil)
}

// ── workflows ────────────────────────────────────────────────────────────────

// DeleteWorkflow removes every version of a workflow. The gateway refuses with
// 409 (and names the referencing runs and schedules) while anything uses it.
func (c *Client) DeleteWorkflow(ctx context.Context, name string) (any, error) {
	return c.do(ctx, http.MethodDelete, c.urlFor("/specs/"+esc(name), nil), nil, nil)
}

// ── schedules ────────────────────────────────────────────────────────────────

// Schedule is the body of POST /schedules and PUT /schedules/{name}, matching
// the gateway's createScheduleRequest.
type Schedule struct {
	Name          string         `json:"name"`
	SpecName      string         `json:"spec_name"`
	SpecVersion   *int32         `json:"spec_version,omitempty"`
	Cron          string         `json:"cron"`
	Timezone      string         `json:"timezone,omitempty"`
	Params        map[string]any `json:"params,omitempty"`
	Priority      int32          `json:"priority,omitempty"`
	OverlapPolicy string         `json:"overlap_policy,omitempty"`
	CatchupLimit  int32          `json:"catchup_limit,omitempty"`
	// Enabled is a pointer because the gateway distinguishes absent (create:
	// defaults to true) from false, and REQUIRES it on update.
	Enabled *bool `json:"enabled,omitempty"`
}

// ListSchedules lists the namespace's schedules.
func (c *Client) ListSchedules(ctx context.Context, limit int) (any, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return c.do(ctx, http.MethodGet, c.urlFor("/schedules", q), nil, nil)
}

// GetSchedule reads one schedule with its next `count` fire times.
func (c *Client) GetSchedule(ctx context.Context, name string, count int) (any, error) {
	q := url.Values{}
	if count > 0 {
		q.Set("count", strconv.Itoa(count))
	}
	return c.do(ctx, http.MethodGet, c.urlFor("/schedules/"+esc(name), q), nil, nil)
}

// CreateSchedule registers a schedule. The gateway validates the cron, the
// timezone and that spec_name names a workflow before storing anything.
func (c *Client) CreateSchedule(ctx context.Context, s Schedule) (any, error) {
	return c.post(ctx, "/schedules", s)
}

// UpdateSchedule replaces a schedule. PUT is a full replacement on the
// gateway, so callers must send every field they want kept.
func (c *Client) UpdateSchedule(ctx context.Context, s Schedule) (any, error) {
	body, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encoding schedule: %w", err)
	}
	return c.do(ctx, http.MethodPut, c.urlFor("/schedules/"+esc(s.Name), nil), body, nil)
}

// DeleteSchedule removes a schedule and its tick history (204 on success).
func (c *Client) DeleteSchedule(ctx context.Context, name string) (any, error) {
	return c.do(ctx, http.MethodDelete, c.urlFor("/schedules/"+esc(name), nil), nil, nil)
}

// SetScheduleEnabled enables or disables a schedule. Enabling recomputes the
// next fire time from now rather than replaying missed ticks.
func (c *Client) SetScheduleEnabled(ctx context.Context, name string, enabled bool) (any, error) {
	verb := "disable"
	if enabled {
		verb = "enable"
	}
	return c.post(ctx, "/schedules/"+esc(name)+"/"+verb, nil)
}

// PreviewSchedule evaluates a cron expression without creating anything. An
// invalid expression is a 200 with valid:false, not an error.
func (c *Client) PreviewSchedule(ctx context.Context, cron, timezone string, count int) (any, error) {
	q := url.Values{"cron": []string{cron}}
	if timezone != "" {
		q.Set("timezone", timezone)
	}
	if count > 0 {
		q.Set("count", strconv.Itoa(count))
	}
	return c.do(ctx, http.MethodGet, c.urlFor("/schedules/preview", q), nil, nil)
}

// ── backfills ────────────────────────────────────────────────────────────────

// BackfillOptions is the body of POST /schedules/{name}/backfills.
type BackfillOptions struct {
	From      string `json:"from"`
	To        string `json:"to"`
	MaxActive int32  `json:"max_active,omitempty"`
	MaxRuns   int32  `json:"max_runs,omitempty"`
}

// StartBackfill starts a backfill of a schedule over [from, to].
func (c *Client) StartBackfill(ctx context.Context, schedule string, o BackfillOptions) (any, error) {
	return c.post(ctx, "/schedules/"+esc(schedule)+"/backfills", o)
}

// ListBackfills lists a schedule's backfills.
func (c *Client) ListBackfills(ctx context.Context, schedule string, limit int) (any, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return c.do(ctx, http.MethodGet, c.urlFor("/schedules/"+esc(schedule)+"/backfills", q), nil, nil)
}

// GetBackfill reads one backfill by id.
func (c *Client) GetBackfill(ctx context.Context, id string) (any, error) {
	return c.do(ctx, http.MethodGet, c.urlFor("/backfills/"+esc(id), nil), nil, nil)
}

// BackfillAction applies pause, resume or cancel to a backfill.
func (c *Client) BackfillAction(ctx context.Context, id, action string) (any, error) {
	switch action {
	case "pause", "resume", "cancel":
	default:
		return nil, fmt.Errorf("unknown backfill action %q", action)
	}
	return c.post(ctx, "/backfills/"+esc(id)+"/"+action, nil)
}

// ── workers ──────────────────────────────────────────────────────────────────

// DrainWorker marks a worker DRAINING (202): it takes no new work and exits
// after finishing what it holds. Delivered on its next heartbeat response.
func (c *Client) DrainWorker(ctx context.Context, id string) (any, error) {
	return c.post(ctx, "/workers/"+esc(id)+"/drain", nil)
}

// QuarantineWorker removes a worker from placement. durationMs 0 = indefinite.
func (c *Client) QuarantineWorker(ctx context.Context, id, reason string, durationMs int64) (any, error) {
	return c.post(ctx, "/workers/"+esc(id)+"/quarantine",
		map[string]any{"reason": reason, "duration_ms": durationMs})
}

// ReleaseWorker returns a QUARANTINED worker to the pool.
func (c *Client) ReleaseWorker(ctx context.Context, id, reason string) (any, error) {
	return c.post(ctx, "/workers/"+esc(id)+"/release", map[string]any{"reason": reason})
}

// ── cache ────────────────────────────────────────────────────────────────────

// CacheInvalidation selects memoized results to stop reusing. At least one of
// TaskRef or CacheKey is required; TaskVersion needs TaskRef.
type CacheInvalidation struct {
	TaskRef     string `json:"task_ref,omitempty"`
	TaskVersion *int32 `json:"task_version,omitempty"`
	CacheKey    string `json:"cache_key,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// InvalidateCache records a memo invalidation. Nothing is deleted: results
// produced before the cutoff simply stop being eligible for reuse.
func (c *Client) InvalidateCache(ctx context.Context, inv CacheInvalidation) (any, error) {
	return c.post(ctx, "/cache/invalidations", inv)
}
