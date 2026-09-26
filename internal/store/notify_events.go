// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

// The event-service side of ADR-0028: the notifier's outbox consumer, rule
// matching, the delivery queue, PagerDuty incidents, the SLA sweep and lineage
// recording. Everything here is written by nexus_event_service (migration 0034).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// NotifyFact is one outbox row the notifier evaluates.
type NotifyFact struct {
	OutboxID  int64
	Topic     string
	Namespace string
	CreatedAt time.Time
	// run_state
	RunID        string
	State        string
	StateVersion int64
	// run_event (EventID is 0 when the event is gone)
	EventID    int64
	EventKind  string
	NodeID     string
	AttemptID  string
	EventBody  map[string]any
	OccurredAt time.Time
}

// NotifyClaimFacts locks up to limit un-notified run_state/run_event rows.
//
// FOR UPDATE OF o SKIP LOCKED, like the relay's claim, so N notifiers never
// evaluate one row concurrently and never wait on the relay, which locks the
// same rows on a different predicate.
func NotifyClaimFacts(ctx context.Context, tx pgx.Tx, limit int) ([]NotifyFact, error) {
	rows, err := tx.Query(ctx, `
SELECT o.id, o.topic, COALESCE(o.namespace, wr.namespace, ''), o.created_at, o.payload,
       e.id, e.run_id::text, e.node_id, e.attempt_id::text, e.kind, e.payload, e.occurred_at
  FROM outbox o
  LEFT JOIN run_events e     ON o.topic = 'run_event' AND e.id = o.ref_id
  LEFT JOIN workflow_runs wr ON wr.id = e.run_id
 WHERE o.notified_at IS NULL
   AND o.topic IN ('run_state', 'run_event')
 ORDER BY o.id
 LIMIT $1
 FOR UPDATE OF o SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("notify: claim outbox rows: %w", err)
	}
	defer rows.Close()
	var out []NotifyFact
	for rows.Next() {
		var (
			f                          NotifyFact
			payload, evPayload         []byte
			evID                       *int64
			evRun, evNode, evAtt, kind *string
			occurred                   *time.Time
		)
		if err := rows.Scan(&f.OutboxID, &f.Topic, &f.Namespace, &f.CreatedAt, &payload,
			&evID, &evRun, &evNode, &evAtt, &kind, &evPayload, &occurred); err != nil {
			return nil, fmt.Errorf("notify: scan outbox row: %w", err)
		}
		f.OccurredAt = f.CreatedAt
		switch f.Topic {
		case OutboxTopicRunState:
			var p struct {
				RunID        string `json:"run_id"`
				Namespace    string `json:"namespace"`
				State        string `json:"state"`
				StateVersion int64  `json:"state_version"`
			}
			_ = json.Unmarshal(payload, &p)
			f.RunID, f.State, f.StateVersion = p.RunID, p.State, p.StateVersion
			if f.Namespace == "" {
				f.Namespace = p.Namespace
			}
		case OutboxTopicRunEvent:
			if evID != nil {
				f.EventID = *evID
				f.RunID = deref(evRun)
				f.NodeID = deref(evNode)
				f.AttemptID = deref(evAtt)
				f.EventKind = deref(kind)
				f.EventBody = map[string]any{}
				_ = json.Unmarshal(evPayload, &f.EventBody)
				if occurred != nil {
					f.OccurredAt = *occurred
				}
			}
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// NotifyMarkFacts marks rows evaluated. Same transaction as their outputs.
func NotifyMarkFacts(ctx context.Context, tx pgx.Tx, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE outbox SET notified_at = clock_timestamp() WHERE id = ANY($1)`, ids); err != nil {
		return fmt.Errorf("notify: mark outbox rows: %w", err)
	}
	return nil
}

// RunContext is what rule matching and rendering need about a run.
type RunContext struct {
	RunID     string
	Namespace string
	Workflow  string
	SpecID    string
	Labels    map[string]string
	State     string
	CreatedAt time.Time
	Trigger   string
}

// ErrRunGone means the run was deleted before its fact was evaluated.
var ErrRunGone = errors.New("notify: run no longer exists")

// NotifyRunContext loads a run's workflow name and spec labels.
func NotifyRunContext(ctx context.Context, q GWQuerier, runID string) (RunContext, error) {
	var (
		rc     RunContext
		labels []byte
	)
	err := q.QueryRow(ctx, `
SELECT r.id::text, r.namespace, s.name, s.id::text, COALESCE(s.spec_json->'labels', '{}'::jsonb),
       r.state::text, r.created_at, r.trigger_kind
  FROM workflow_runs r JOIN workflow_specs s ON s.id = r.spec_id
 WHERE r.id = $1`, runID).Scan(&rc.RunID, &rc.Namespace, &rc.Workflow, &rc.SpecID, &labels, &rc.State, &rc.CreatedAt, &rc.Trigger)
	if errors.Is(err, pgx.ErrNoRows) {
		return rc, ErrRunGone
	}
	if err != nil {
		return rc, fmt.Errorf("notify: load run %s: %w", runID, err)
	}
	rc.Labels = map[string]string{}
	var raw map[string]any
	_ = json.Unmarshal(labels, &raw)
	for k, v := range raw {
		if s, ok := v.(string); ok {
			rc.Labels[k] = s
		}
	}
	return rc, nil
}

// NotifyFailedNodes lists up to limit FAILED node ids, for a failure summary.
func NotifyFailedNodes(ctx context.Context, q GWQuerier, runID string, limit int) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT node_id FROM task_nodes WHERE run_id = $1 AND state = 'FAILED' ORDER BY node_id LIMIT $2`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RuleTarget is one (rule, channel) pair a fact matched.
type RuleTarget struct {
	RuleID      string
	RuleName    string
	ChannelID   string
	ChannelKind string
}

// NotifyMatchRules finds enabled (rule, channel) pairs subscribing to eventType
// for this run. A rule only matches facts at or after its creation, so a
// notifier catching up on a backlog never pages about history that predates it.
func NotifyMatchRules(ctx context.Context, q GWQuerier, rc RunContext, eventType string, factAt time.Time) ([]RuleTarget, error) {
	labels, _ := json.Marshal(rc.Labels)
	rows, err := q.Query(ctx, `
SELECT r.id::text, r.name, c.id::text, c.kind
  FROM notification_rules r
  JOIN notification_rule_channels rc ON rc.rule_id = r.id
  JOIN notification_channels c       ON c.id = rc.channel_id AND c.enabled
 WHERE r.namespace = $1
   AND r.enabled
   AND $2 = ANY(r.events)
   AND r.created_at <= $3
   AND (cardinality(r.workflows) = 0 OR $4 = ANY(r.workflows))
   AND r.labels <@ $5::jsonb
 ORDER BY r.id, c.id`, rc.Namespace, eventType, factAt, rc.Workflow, string(labels))
	if err != nil {
		return nil, fmt.Errorf("notify: match rules: %w", err)
	}
	defer rows.Close()
	var out []RuleTarget
	for rows.Next() {
		var t RuleTarget
		if err := rows.Scan(&t.RuleID, &t.RuleName, &t.ChannelID, &t.ChannelKind); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeliveryInsert is one delivery to enqueue.
type DeliveryInsert struct {
	Namespace string
	ChannelID string
	RuleID    string // may be empty
	EventKey  string
	DedupKey  string // defaults to DedupKeyFor(channel, event key)
	EventType string
	Action    string
	RunID     string
	Payload   map[string]any
	CreatedBy string
	RedriveOf *int64
	// MaxAttempts 0 keeps the column default.
	MaxAttempts int32
}

// DedupKeyFor derives the receiver-facing idempotency key of (channel, event).
func DedupKeyFor(channelID, eventKey string) string {
	sum := sha256.Sum256([]byte(channelID + "\x00" + eventKey))
	return "nexus-" + hex.EncodeToString(sum[:16])
}

// NotifyEnqueue inserts a delivery unless (channel, event key) already exists.
// Reports whether a row was inserted.
func NotifyEnqueue(ctx context.Context, q GWQuerier, d DeliveryInsert) (int64, bool, error) {
	if d.DedupKey == "" {
		d.DedupKey = DedupKeyFor(d.ChannelID, d.EventKey)
	}
	if d.Action == "" {
		d.Action = "trigger"
	}
	if d.CreatedBy == "" {
		d.CreatedBy = "notifier"
	}
	payload, err := json.Marshal(d.Payload)
	if err != nil {
		return 0, false, err
	}
	var ruleID, runID any
	if d.RuleID != "" {
		ruleID = d.RuleID
	}
	if d.RunID != "" {
		runID = d.RunID
	}
	var maxAttempts any
	if d.MaxAttempts > 0 {
		maxAttempts = d.MaxAttempts
	}
	var id int64
	err = q.QueryRow(ctx, `
INSERT INTO notification_deliveries
  (namespace, channel_id, rule_id, event_key, dedup_key, event_type, action, run_id, payload,
   created_by, redrive_of, max_attempts)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, COALESCE($12::int, 8))
ON CONFLICT (channel_id, event_key) DO NOTHING
RETURNING id`, d.Namespace, d.ChannelID, ruleID, d.EventKey, d.DedupKey, d.EventType, d.Action,
		runID, string(payload), d.CreatedBy, d.RedriveOf, maxAttempts).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("notify: enqueue delivery: %w", err)
	}
	return id, true, nil
}

// ── PagerDuty incidents ─────────────────────────────────────────────────────

// NotifyOpenIncident returns the dedup key of the open incident for (rule,
// channel, workflow), opening one keyed on openingKey when none is open. The
// same key is reused while an incident is open, so repeated failures group
// into one PagerDuty incident until a success resolves it.
func NotifyOpenIncident(ctx context.Context, tx pgx.Tx, ns, ruleID, channelID, workflow, openingKey, runID string) (string, error) {
	fresh := "nexus-inc-" + hashHex(ruleID, channelID, workflow, openingKey)
	var key string
	err := tx.QueryRow(ctx, `
INSERT INTO notification_incidents (rule_id, channel_id, workflow, namespace, dedup_key, opened_by_run)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (rule_id, channel_id, workflow) DO UPDATE
   SET dedup_key       = CASE WHEN notification_incidents.resolved_at IS NULL
                              THEN notification_incidents.dedup_key ELSE EXCLUDED.dedup_key END,
       opened_at       = CASE WHEN notification_incidents.resolved_at IS NULL
                              THEN notification_incidents.opened_at ELSE now() END,
       opened_by_run   = CASE WHEN notification_incidents.resolved_at IS NULL
                              THEN notification_incidents.opened_by_run ELSE EXCLUDED.opened_by_run END,
       resolved_at     = NULL,
       resolved_by_run = NULL
RETURNING dedup_key`, ruleID, channelID, workflow, ns, fresh, nullUUID(runID)).Scan(&key)
	if err != nil {
		return "", fmt.Errorf("notify: open incident: %w", err)
	}
	return key, nil
}

// OpenIncident is an unresolved incident a recovery may close.
type OpenIncident struct {
	RuleID    string
	ChannelID string
	DedupKey  string
}

// NotifyResolveIncidents closes every open incident for workflow in namespace
// whose rule and channel are still enabled, returning them so the caller can
// enqueue the resolves in the same transaction.
func NotifyResolveIncidents(ctx context.Context, tx pgx.Tx, ns, workflow, runID string) ([]OpenIncident, error) {
	rows, err := tx.Query(ctx, `
UPDATE notification_incidents i
   SET resolved_at = now(), resolved_by_run = $3
  FROM notification_rules r, notification_channels c
 WHERE i.namespace = $1 AND i.workflow = $2 AND i.resolved_at IS NULL
   AND r.id = i.rule_id AND r.enabled
   AND c.id = i.channel_id AND c.enabled
RETURNING i.rule_id::text, i.channel_id::text, i.dedup_key`, ns, workflow, nullUUID(runID))
	if err != nil {
		return nil, fmt.Errorf("notify: resolve incidents: %w", err)
	}
	defer rows.Close()
	var out []OpenIncident
	for rows.Next() {
		var o OpenIncident
		if err := rows.Scan(&o.RuleID, &o.ChannelID, &o.DedupKey); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func hashHex(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ── SLA ────────────────────────────────────────────────────────────────────

// SLAOverdue is one active run past a rule's SLA on one channel, not yet notified.
type SLAOverdue struct {
	RuleTarget
	RunContext
	SLASeconds int32
}

// NotifyFindOverdue lists active runs past the SLA of an enabled rule
// subscribing to run.sla_exceeded, per channel, that have no delivery yet.
func NotifyFindOverdue(ctx context.Context, q GWQuerier, limit int) ([]SLAOverdue, error) {
	rows, err := q.Query(ctx, `
SELECT r.id::text, r.name, c.id::text, c.kind, r.sla_seconds,
       ru.id::text, ru.namespace, s.name, s.id::text, ru.state::text, ru.created_at, ru.trigger_kind
  FROM notification_rules r
  JOIN notification_rule_channels rc ON rc.rule_id = r.id
  JOIN notification_channels c       ON c.id = rc.channel_id AND c.enabled
  JOIN workflow_runs ru ON ru.namespace = r.namespace
                       AND ru.state IN ('PENDING','RUNNING','PAUSING','CANCELLING')
                       AND ru.created_at < now() - make_interval(secs => r.sla_seconds)
  JOIN workflow_specs s ON s.id = ru.spec_id
 WHERE r.enabled
   AND r.sla_seconds IS NOT NULL
   AND 'run.sla_exceeded' = ANY(r.events)
   AND (cardinality(r.workflows) = 0 OR s.name = ANY(r.workflows))
   AND r.labels <@ COALESCE(s.spec_json->'labels', '{}'::jsonb)
   AND NOT EXISTS (SELECT 1 FROM notification_deliveries d
                    WHERE d.channel_id = c.id AND d.event_key = 'sla:' || r.id::text || ':' || ru.id::text)
 ORDER BY ru.created_at
 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("notify: find overdue runs: %w", err)
	}
	defer rows.Close()
	var out []SLAOverdue
	for rows.Next() {
		var o SLAOverdue
		if err := rows.Scan(&o.RuleID, &o.RuleName, &o.ChannelID, &o.ChannelKind, &o.SLASeconds,
			&o.RunID, &o.Namespace, &o.Workflow, &o.SpecID, &o.State, &o.CreatedAt, &o.Trigger); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ── dispatcher ─────────────────────────────────────────────────────────────

// ClaimedDelivery is a leased delivery joined with its channel.
type ClaimedDelivery struct {
	ID          int64
	Namespace   string
	ChannelID   string
	RuleID      string
	EventKey    string
	DedupKey    string
	EventType   string
	Action      string
	RunID       string
	Payload     map[string]any
	Attempts    int32
	MaxAttempts int32
	CreatedAt   time.Time

	ChannelName       string
	ChannelKind       string
	ChannelConfig     map[string]any
	ChannelEnabled    bool
	SecretCiphertext  []byte
	SecretRef         string
	RateLimitPerMin   int32
}

// NotifyClaimDeliveries leases up to limit due deliveries for owner.
func NotifyClaimDeliveries(ctx context.Context, q GWQuerier, owner string, limit int, lease time.Duration) ([]ClaimedDelivery, error) {
	rows, err := q.Query(ctx, `
WITH c AS (
  SELECT id FROM notification_deliveries
   WHERE state = 'pending' AND next_attempt_at <= now()
     AND (lease_until IS NULL OR lease_until < now())
   ORDER BY next_attempt_at, id
   LIMIT $2
   FOR UPDATE SKIP LOCKED
)
UPDATE notification_deliveries d
   SET lease_owner = $1, lease_until = now() + make_interval(secs => $3)
  FROM c, notification_channels ch
 WHERE d.id = c.id AND ch.id = d.channel_id
RETURNING d.id, d.namespace, d.channel_id::text, COALESCE(d.rule_id::text, ''), d.event_key, d.dedup_key,
          d.event_type, d.action, COALESCE(d.run_id::text, ''), d.payload, d.attempts, d.max_attempts, d.created_at,
          ch.name, ch.kind, ch.config, ch.enabled, ch.secret_ciphertext, COALESCE(ch.secret_ref, ''),
          ch.rate_limit_per_minute`, owner, limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("notify: claim deliveries: %w", err)
	}
	defer rows.Close()
	var out []ClaimedDelivery
	for rows.Next() {
		var (
			d            ClaimedDelivery
			payload, cfg []byte
		)
		if err := rows.Scan(&d.ID, &d.Namespace, &d.ChannelID, &d.RuleID, &d.EventKey, &d.DedupKey,
			&d.EventType, &d.Action, &d.RunID, &payload, &d.Attempts, &d.MaxAttempts, &d.CreatedAt,
			&d.ChannelName, &d.ChannelKind, &cfg, &d.ChannelEnabled, &d.SecretCiphertext, &d.SecretRef,
			&d.RateLimitPerMin); err != nil {
			return nil, err
		}
		d.Payload = map[string]any{}
		_ = json.Unmarshal(payload, &d.Payload)
		d.ChannelConfig = map[string]any{}
		_ = json.Unmarshal(cfg, &d.ChannelConfig)
		out = append(out, d)
	}
	return out, rows.Err()
}

// NotifyTakeRateSlot takes one send from the channel's current one-minute
// window. false means the window is full; the returned time is when it resets.
func NotifyTakeRateSlot(ctx context.Context, q GWQuerier, ns, channelID string, limit int32) (bool, time.Time, error) {
	var sent int32
	var start time.Time
	err := q.QueryRow(ctx, `
INSERT INTO notification_channel_windows AS w (channel_id, namespace, window_start, sent)
VALUES ($1, $2, date_trunc('minute', now()), 1)
ON CONFLICT (channel_id) DO UPDATE
   SET window_start = CASE WHEN w.window_start < date_trunc('minute', now())
                           THEN date_trunc('minute', now()) ELSE w.window_start END,
       sent         = CASE WHEN w.window_start < date_trunc('minute', now())
                           THEN 1 ELSE w.sent + 1 END
 WHERE w.window_start < date_trunc('minute', now()) OR w.sent < $3
RETURNING sent, window_start`, channelID, ns, limit).Scan(&sent, &start)
	if errors.Is(err, pgx.ErrNoRows) {
		var reset time.Time
		if err := q.QueryRow(ctx, `SELECT date_trunc('minute', now()) + interval '1 minute'`).Scan(&reset); err != nil {
			return false, time.Time{}, err
		}
		return false, reset, nil
	}
	if err != nil {
		return false, time.Time{}, fmt.Errorf("notify: rate window: %w", err)
	}
	return true, start, nil
}

// ErrLeaseLost means another dispatcher owns the delivery now; the outcome of
// this send is not recorded (the other owner's is).
var ErrLeaseLost = errors.New("notify: delivery lease lost")

// NotifyDefer releases a delivery until at without spending an attempt.
func NotifyDefer(ctx context.Context, q GWQuerier, id int64, owner string, at time.Time, reason string) error {
	tag, err := q.Exec(ctx, `
UPDATE notification_deliveries
   SET next_attempt_at = $3, lease_owner = NULL, lease_until = NULL, last_error = $4
 WHERE id = $1 AND lease_owner = $2 AND state = 'pending'`, id, owner, at, truncateErr(reason))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// NotifyRecordSuccess marks a delivery delivered.
func NotifyRecordSuccess(ctx context.Context, q GWQuerier, id int64, owner string, status int) error {
	tag, err := q.Exec(ctx, `
UPDATE notification_deliveries
   SET state = 'delivered', delivered_at = now(), attempts = attempts + 1, last_status = $3,
       last_error = NULL, lease_owner = NULL, lease_until = NULL
 WHERE id = $1 AND lease_owner = $2 AND state = 'pending'`, id, owner, nullInt(status))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// NotifyRecordFailure records a failed attempt: dead when dead is true or the
// attempt budget is spent, otherwise pending again at retryAt. Reports whether
// the delivery is now dead.
func NotifyRecordFailure(ctx context.Context, q GWQuerier, id int64, owner string, status int, msg string, dead bool, retryAt time.Time) (bool, error) {
	var state string
	err := q.QueryRow(ctx, `
UPDATE notification_deliveries
   SET attempts = attempts + 1, last_status = $3, last_error = $4,
       lease_owner = NULL, lease_until = NULL,
       state = CASE WHEN $5 OR attempts + 1 >= max_attempts THEN 'dead' ELSE 'pending' END,
       dead_at = CASE WHEN $5 OR attempts + 1 >= max_attempts THEN now() ELSE NULL END,
       next_attempt_at = $6
 WHERE id = $1 AND lease_owner = $2 AND state = 'pending'
RETURNING state`, id, owner, nullInt(status), truncateErr(msg), dead, retryAt).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrLeaseLost
	}
	if err != nil {
		return false, err
	}
	return state == "dead", nil
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func truncateErr(s string) string {
	if len(s) > 1000 {
		return s[:1000]
	}
	return s
}

// NotifyBacklog reports pending deliveries and the age of the oldest due one.
func NotifyBacklog(ctx context.Context, q GWQuerier) (pending, dead int64, oldest float64, err error) {
	err = q.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE state = 'pending'),
       count(*) FILTER (WHERE state = 'dead' AND dead_at > now() - interval '24 hours'),
       COALESCE(EXTRACT(EPOCH FROM now() - min(created_at) FILTER (WHERE state = 'pending' AND next_attempt_at <= now())), 0)::float8
  FROM notification_deliveries`).Scan(&pending, &dead, &oldest)
	return
}

// NotifyPrune deletes delivered and dead deliveries older than retention, in
// bounded chunks.
func NotifyPrune(ctx context.Context, q GWQuerier, retention time.Duration) (int64, error) {
	var total int64
	for i := 0; i < 20; i++ {
		tag, err := q.Exec(ctx, `
DELETE FROM notification_deliveries d USING (
  SELECT id FROM notification_deliveries
   WHERE state IN ('delivered', 'dead') AND created_at < now() - make_interval(secs => $1)
   ORDER BY id LIMIT 5000
) x WHERE d.id = x.id`, retention.Seconds())
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < 5000 {
			break
		}
	}
	return total, nil
}

// ── lineage ────────────────────────────────────────────────────────────────

// LineageRecordRun records the run's declared asset edges from its spec, once.
// Idempotent: edges conflict on their primary key, and the asset catalogue is
// only touched for edges actually inserted.
func LineageRecordRun(ctx context.Context, q GWQuerier, runID string) (int64, error) {
	tag, err := q.Exec(ctx, `
WITH decl AS (
  SELECT r.id AS run_id, t->>'id' AS node_id, d.dir, a->>'uri' AS uri,
         COALESCE(a->>'name', '') AS name, r.namespace, s.name AS spec_name
    FROM workflow_runs r
    JOIN workflow_specs s ON s.id = r.spec_id
    CROSS JOIN LATERAL jsonb_array_elements(COALESCE(s.spec_json->'tasks', '[]'::jsonb)) t
    CROSS JOIN LATERAL (VALUES ('produces'), ('consumes')) d(dir)
    CROSS JOIN LATERAL jsonb_array_elements(
        CASE WHEN jsonb_typeof(t->'assets'->d.dir) = 'array' THEN t->'assets'->d.dir ELSE '[]'::jsonb END) a
   WHERE r.id = $1 AND COALESCE(a->>'uri', '') <> ''
), ins AS (
  INSERT INTO asset_lineage_edges (run_id, node_id, direction, asset_uri, asset_name, namespace, spec_name)
  SELECT run_id, node_id, dir, uri, name, namespace, spec_name FROM decl
  ON CONFLICT DO NOTHING
  RETURNING namespace, asset_uri, asset_name
)
INSERT INTO assets AS a (namespace, uri, name)
SELECT DISTINCT ON (namespace, asset_uri) namespace, asset_uri, asset_name FROM ins
 ORDER BY namespace, asset_uri, asset_name DESC
ON CONFLICT (namespace, uri) DO UPDATE
   SET last_declared_at = now(),
       name = CASE WHEN EXCLUDED.name <> '' THEN EXCLUDED.name ELSE a.name END`, runID)
	if err != nil {
		return 0, fmt.Errorf("lineage: record run %s: %w", runID, err)
	}
	return tag.RowsAffected(), nil
}

// LineageRecordMaterialization records that nodeID's declared products were
// materialized by this run, once per (run, node, asset).
func LineageRecordMaterialization(ctx context.Context, q GWQuerier, runID, nodeID string, at time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `
WITH ins AS (
  INSERT INTO asset_materializations (namespace, asset_uri, asset_name, run_id, node_id, spec_name, materialized_at)
  SELECT namespace, asset_uri, asset_name, run_id, node_id, spec_name, $3
    FROM asset_lineage_edges
   WHERE run_id = $1 AND node_id = $2 AND direction = 'produces'
  ON CONFLICT (run_id, node_id, asset_uri) DO NOTHING
  RETURNING namespace, asset_uri, run_id, materialized_at
)
UPDATE assets a
   SET last_materialized_at = ins.materialized_at, last_materialized_run_id = ins.run_id
  FROM ins
 WHERE a.namespace = ins.namespace AND a.uri = ins.asset_uri
   AND (a.last_materialized_at IS NULL OR a.last_materialized_at <= ins.materialized_at)`, runID, nodeID, at)
	if err != nil {
		return 0, fmt.Errorf("lineage: record materialization %s/%s: %w", runID, nodeID, err)
	}
	return tag.RowsAffected(), nil
}
