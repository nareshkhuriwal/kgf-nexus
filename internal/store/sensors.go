// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

// Sensors (ADR-0028). Definitions are gateway-owned (sensors); runtime state
// and observations are workflow-manager-owned (sensor_state,
// sensor_observations). Every state write by the sensor loop is FENCED on the
// caller's lease, so a replica that lost its lease changes nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Sensor kinds.
const (
	SensorObjectStore  = "object_store"
	SensorRunSucceeded = "run_succeeded"
)

// Sensor is a definition, optionally joined with its state.
type Sensor struct {
	ID                  string
	Namespace           string
	Name                string
	Kind                string
	SpecName            string
	SpecVersion         *int32
	Config              map[string]any
	ParamsMapping       map[string]any
	PollIntervalSeconds int32
	Backfill            bool
	Priority            int32
	Enabled             bool
	CreatedBy           string
	CreatedAt           time.Time
	UpdatedAt           time.Time
	Revision            int64

	State *SensorState
}

// SensorState is the runtime view.
type SensorState struct {
	DefinitionRevision  int64
	Status              string
	BaselineDone        bool
	Cursor              string
	Watermark           *time.Time
	NextPollAt          time.Time
	LeaseOwner          string
	LeaseUntil          *time.Time
	LastPollAt          *time.Time
	LastSuccessAt       *time.Time
	LastError           string
	ConsecutiveFailures int32
	ObservedTotal       int64
	TriggeredTotal      int64
	PendingCount        int64
}

const sensorCols = `s.id::text, s.namespace, s.name, s.kind, s.spec_name, s.spec_version, s.config, s.params_mapping,
  s.poll_interval_seconds, s.backfill, s.priority, s.enabled, s.created_by, s.created_at, s.updated_at, s.revision`

const sensorStateCols = `st.sensor_id IS NOT NULL, COALESCE(st.definition_revision, 0), COALESCE(st.status, ''),
  COALESCE(st.baseline_done, false), COALESCE(st.cursor, ''), st.watermark, COALESCE(st.next_poll_at, now()),
  COALESCE(st.lease_owner, ''), st.lease_until, st.last_poll_at, st.last_success_at, COALESCE(st.last_error, ''),
  COALESCE(st.consecutive_failures, 0), COALESCE(st.observed_total, 0), COALESCE(st.triggered_total, 0),
  (SELECT count(*) FROM sensor_observations o WHERE o.sensor_id = s.id AND o.outcome = 'pending')`

func scanSensor(row pgx.Row, withState bool) (Sensor, error) {
	var s Sensor
	var cfg, pm []byte
	dest := []any{&s.ID, &s.Namespace, &s.Name, &s.Kind, &s.SpecName, &s.SpecVersion, &cfg, &pm,
		&s.PollIntervalSeconds, &s.Backfill, &s.Priority, &s.Enabled, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt, &s.Revision}
	var st SensorState
	var has bool
	if withState {
		dest = append(dest, &has, &st.DefinitionRevision, &st.Status, &st.BaselineDone, &st.Cursor, &st.Watermark,
			&st.NextPollAt, &st.LeaseOwner, &st.LeaseUntil, &st.LastPollAt, &st.LastSuccessAt, &st.LastError,
			&st.ConsecutiveFailures, &st.ObservedTotal, &st.TriggeredTotal, &st.PendingCount)
	}
	if err := row.Scan(dest...); err != nil {
		return s, err
	}
	s.Config, s.ParamsMapping = map[string]any{}, map[string]any{}
	_ = json.Unmarshal(cfg, &s.Config)
	_ = json.Unmarshal(pm, &s.ParamsMapping)
	if has {
		s.State = &st
	}
	return s, nil
}

// ── definitions (api-gateway) ───────────────────────────────────────────────

// SensorCreate inserts a definition.
func SensorCreate(ctx context.Context, q GWQuerier, s Sensor) (Sensor, error) {
	cfg, err := jsonb(s.Config)
	if err != nil {
		return s, err
	}
	pm, err := jsonb(s.ParamsMapping)
	if err != nil {
		return s, err
	}
	var id string
	if err := q.QueryRow(ctx, `
INSERT INTO sensors (namespace, name, kind, spec_name, spec_version, config, params_mapping, poll_interval_seconds,
                     backfill, priority, enabled, created_by)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb, $8, $9, $10, $11, $12) RETURNING id::text`,
		s.Namespace, s.Name, s.Kind, s.SpecName, s.SpecVersion, cfg, pm, s.PollIntervalSeconds, s.Backfill,
		s.Priority, s.Enabled, s.CreatedBy).Scan(&id); err != nil {
		return s, err
	}
	return SensorGet(ctx, q, s.Namespace, id)
}

// SensorUpdate changes a definition. The kind is immutable.
func SensorUpdate(ctx context.Context, q GWQuerier, s Sensor, revision int64) (Sensor, error) {
	cfg, err := jsonb(s.Config)
	if err != nil {
		return s, err
	}
	pm, err := jsonb(s.ParamsMapping)
	if err != nil {
		return s, err
	}
	var id string
	err = q.QueryRow(ctx, `
UPDATE sensors
   SET spec_name = $3, spec_version = $4, config = $5::jsonb, params_mapping = $6::jsonb,
       poll_interval_seconds = $7, backfill = $8, priority = $9, enabled = $10,
       updated_at = now(), revision = revision + 1
 WHERE namespace = $1 AND name = $2 AND ($11 = 0 OR revision = $11)
RETURNING id::text`, s.Namespace, s.Name, s.SpecName, s.SpecVersion, cfg, pm, s.PollIntervalSeconds,
		s.Backfill, s.Priority, s.Enabled, revision).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := SensorGet(ctx, q, s.Namespace, s.Name); gerr == nil {
			return s, ErrRevisionConflict
		}
		return s, ErrNotifyNotFound
	}
	if err != nil {
		return s, err
	}
	return SensorGet(ctx, q, s.Namespace, id)
}

// SensorGet reads a definition with its state.
func SensorGet(ctx context.Context, q GWQuerier, ns, nameOrID string) (Sensor, error) {
	s, err := scanSensor(q.QueryRow(ctx, `SELECT `+sensorCols+`, `+sensorStateCols+`
  FROM sensors s LEFT JOIN sensor_state st ON st.sensor_id = s.id
 WHERE s.namespace = $1 AND (s.name = $2 OR s.id::text = $2)`, ns, nameOrID), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrNotifyNotFound
	}
	return s, err
}

// SensorList lists a namespace's sensors with their state.
func SensorList(ctx context.Context, q GWQuerier, ns string) ([]Sensor, error) {
	rows, err := q.Query(ctx, `SELECT `+sensorCols+`, `+sensorStateCols+`
  FROM sensors s LEFT JOIN sensor_state st ON st.sensor_id = s.id
 WHERE s.namespace = $1 ORDER BY s.name LIMIT 1000`, ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sensor
	for rows.Next() {
		s, err := scanSensor(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SensorDelete removes a definition and, by cascade, its state and observations.
func SensorDelete(ctx context.Context, q GWQuerier, ns, name string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `DELETE FROM sensors WHERE namespace = $1 AND name = $2 RETURNING id::text`, ns, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotifyNotFound
	}
	return id, err
}

// SensorObservation is one observed item.
type SensorObservation struct {
	SensorID       string
	ItemKey        string
	Item           map[string]any
	Outcome        string
	IdempotencyKey string
	RunID          string
	Attempts       int32
	LastError      string
	ObservedAt     time.Time
	TriggeredAt    *time.Time
}

func scanObservation(row pgx.Row) (SensorObservation, error) {
	var o SensorObservation
	var item []byte
	err := row.Scan(&o.SensorID, &o.ItemKey, &item, &o.Outcome, &o.IdempotencyKey, &o.RunID, &o.Attempts,
		&o.LastError, &o.ObservedAt, &o.TriggeredAt)
	if err == nil {
		o.Item = map[string]any{}
		_ = json.Unmarshal(item, &o.Item)
	}
	return o, err
}

const observationCols = `sensor_id::text, item_key, item, outcome, idempotency_key, COALESCE(run_id::text, ''),
  attempts, COALESCE(last_error, ''), observed_at, triggered_at`

// SensorListObservations returns a sensor's most recent observations,
// optionally excluding baseline rows.
func SensorListObservations(ctx context.Context, q GWQuerier, sensorID string, includeBaseline bool, limit int) ([]SensorObservation, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := q.Query(ctx, `SELECT `+observationCols+` FROM sensor_observations
 WHERE sensor_id = $1 AND ($2 OR outcome <> 'baseline') ORDER BY observed_at DESC, item_key LIMIT $3`, sensorID, includeBaseline, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SensorObservation
	for rows.Next() {
		o, err := scanObservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ── runtime (workflow-manager) ──────────────────────────────────────────────

// SensorEnsureStates creates missing state rows for enabled sensors. Cheap and
// idempotent; runs every pass so a new definition starts polling promptly.
func SensorEnsureStates(ctx context.Context, q GWQuerier) error {
	_, err := q.Exec(ctx, `
INSERT INTO sensor_state (sensor_id, namespace, definition_revision)
SELECT s.id, s.namespace, s.revision FROM sensors s
 WHERE s.enabled AND NOT EXISTS (SELECT 1 FROM sensor_state st WHERE st.sensor_id = s.id)
ON CONFLICT DO NOTHING`)
	return err
}

// SensorClaimDue leases up to limit due, enabled sensors for owner.
func SensorClaimDue(ctx context.Context, q GWQuerier, owner string, limit int, lease time.Duration) ([]Sensor, error) {
	rows, err := q.Query(ctx, `
WITH c AS (
  SELECT st.sensor_id FROM sensor_state st JOIN sensors s ON s.id = st.sensor_id
   WHERE s.enabled AND st.next_poll_at <= now()
     AND (st.lease_until IS NULL OR st.lease_until < now())
   ORDER BY st.next_poll_at
   LIMIT $2
   FOR UPDATE OF st SKIP LOCKED
), upd AS (
  UPDATE sensor_state st
     SET lease_owner = $1, lease_until = now() + make_interval(secs => $3)
    FROM c WHERE st.sensor_id = c.sensor_id
  RETURNING st.*
)
SELECT `+sensorCols+`, `+sensorStateCols+`
  FROM upd st JOIN sensors s ON s.id = st.sensor_id`, owner, limit, lease.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sensor
	for rows.Next() {
		s, err := scanSensor(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ErrSensorLeaseLost means another replica owns the sensor now.
var ErrSensorLeaseLost = errors.New("sensor lease lost")

// SensorReset restarts a sensor whose definition changed: cursor and
// watermark cleared, baseline redone unless backfill, fenced on the lease.
func SensorReset(ctx context.Context, q GWQuerier, sensorID, owner string, revision int64) error {
	tag, err := q.Exec(ctx, `
UPDATE sensor_state SET definition_revision = $3, cursor = '', watermark = NULL, baseline_done = false, status = 'initializing'
 WHERE sensor_id = $1 AND lease_owner = $2 AND lease_until > now()`, sensorID, owner, revision)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrSensorLeaseLost
	}
	return nil
}

// SensorProgress is what a pass wants persisted.
type SensorProgress struct {
	Cursor       string
	Watermark    *time.Time
	BaselineDone bool
	Observed     int64
	Triggered    int64
	Err          string
	NextPollAt   time.Time
}

// SensorSaveProgress records a pass's outcome and releases the lease, fenced.
func SensorSaveProgress(ctx context.Context, q GWQuerier, sensorID, owner string, p SensorProgress) error {
	tag, err := q.Exec(ctx, `
UPDATE sensor_state
   SET cursor = $3, watermark = $4, baseline_done = $5,
       observed_total = observed_total + $6, triggered_total = triggered_total + $7,
       last_poll_at = now(),
       last_success_at = CASE WHEN $8 = '' THEN now() ELSE last_success_at END,
       last_error = NULLIF($8, ''),
       consecutive_failures = CASE WHEN $8 = '' THEN 0 ELSE consecutive_failures + 1 END,
       status = CASE WHEN $8 = '' THEN 'ok' ELSE 'error' END,
       next_poll_at = $9, lease_owner = NULL, lease_until = NULL
 WHERE sensor_id = $1 AND lease_owner = $2 AND lease_until > now()`,
		sensorID, owner, p.Cursor, p.Watermark, p.BaselineDone, p.Observed, p.Triggered, p.Err, p.NextPollAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrSensorLeaseLost
	}
	return nil
}

// SensorObserve claims an item. outcome is 'pending' (will trigger) or
// 'baseline'. false means the item was already observed: at most one trigger
// per item, whichever replica and pass saw it first.
func SensorObserve(ctx context.Context, q GWQuerier, sensorID, ns, itemKey string, item map[string]any, outcome, idemKey string) (bool, error) {
	b, err := jsonb(item)
	if err != nil {
		return false, err
	}
	tag, err := q.Exec(ctx, `
INSERT INTO sensor_observations (sensor_id, item_key, namespace, item, outcome, idempotency_key)
VALUES ($1, $2, $3, $4::jsonb, $5, $6) ON CONFLICT DO NOTHING`, sensorID, itemKey, ns, b, outcome, idemKey)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SensorPending returns up to limit observations still waiting for their run.
func SensorPending(ctx context.Context, q GWQuerier, sensorID string, limit int) ([]SensorObservation, error) {
	rows, err := q.Query(ctx, `SELECT `+observationCols+` FROM sensor_observations
 WHERE sensor_id = $1 AND outcome = 'pending' ORDER BY observed_at, item_key LIMIT $2`, sensorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SensorObservation
	for rows.Next() {
		o, err := scanObservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SensorMarkTriggered records the run created for an observation.
func SensorMarkTriggered(ctx context.Context, q GWQuerier, sensorID, itemKey, runID string) error {
	_, err := q.Exec(ctx, `
UPDATE sensor_observations SET outcome = 'triggered', run_id = $3, triggered_at = now(), attempts = attempts + 1, last_error = NULL
 WHERE sensor_id = $1 AND item_key = $2 AND outcome = 'pending'`, sensorID, itemKey, runID)
	return err
}

// SensorMarkAttempt records a failed run creation; permanent moves it to failed.
func SensorMarkAttempt(ctx context.Context, q GWQuerier, sensorID, itemKey, msg string, permanent bool) error {
	_, err := q.Exec(ctx, `
UPDATE sensor_observations
   SET attempts = attempts + 1, last_error = $3,
       outcome = CASE WHEN $4 OR attempts + 1 >= 20 THEN 'failed' ELSE outcome END
 WHERE sensor_id = $1 AND item_key = $2 AND outcome = 'pending'`, sensorID, itemKey, truncateErr(msg), permanent)
	return err
}

// UpstreamRun is a succeeded run a run_succeeded sensor observes.
type UpstreamRun struct {
	RunID       string
	Workflow    string
	Params      map[string]any
	LogicalDate *time.Time
	EndedAt     time.Time
	TriggerKind string
}

// SensorSucceededRuns lists runs of workflow in ns that SUCCEEDED with ended_at
// after `after`, oldest first. The caller passes a watermark minus a lookback,
// because ended_at is set before COMMIT and commits land out of order.
func SensorSucceededRuns(ctx context.Context, q GWQuerier, ns, workflow string, after time.Time, limit int) ([]UpstreamRun, error) {
	rows, err := q.Query(ctx, `
SELECT r.id::text, s.name, r.params, r.logical_date, r.ended_at, r.trigger_kind
  FROM workflow_runs r JOIN workflow_specs s ON s.id = r.spec_id
 WHERE r.namespace = $1 AND s.name = $2 AND r.state = 'SUCCEEDED' AND r.ended_at > $3
 ORDER BY r.ended_at, r.id
 LIMIT $4`, ns, workflow, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpstreamRun
	for rows.Next() {
		var u UpstreamRun
		var params []byte
		if err := rows.Scan(&u.RunID, &u.Workflow, &params, &u.LogicalDate, &u.EndedAt, &u.TriggerKind); err != nil {
			return nil, err
		}
		u.Params = map[string]any{}
		_ = json.Unmarshal(params, &u.Params)
		out = append(out, u)
	}
	return out, rows.Err()
}
