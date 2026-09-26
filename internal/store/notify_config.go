// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

// The api-gateway side of ADR-0028's notifications: channel and rule
// configuration (gateway-owned) and read projections of the delivery queue.
// Secret columns are read ONLY by NotifyChannelSecret; every other read leaves
// them out, so no listing can leak one by accident.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNotifyNotFound is returned when a channel, rule or delivery does not exist
// in the namespace.
var ErrNotifyNotFound = errors.New("not found")

// ErrRevisionConflict means an update named a stale revision.
var ErrRevisionConflict = errors.New("revision conflict")

// NotificationChannel is a channel WITHOUT its secret.
type NotificationChannel struct {
	ID                 string
	Namespace          string
	Name               string
	Kind               string
	Config             map[string]any
	SecretSource       string // "sealed" | "ref" | ""
	SecretRef          string
	SecretHint         string
	RateLimitPerMinute int32
	Enabled            bool
	CreatedBy          string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	Revision           int64
}

// ChannelSecretUpdate replaces (or keeps) a channel's secret.
type ChannelSecretUpdate struct {
	Keep       bool
	Ciphertext []byte
	Ref        string
	Hint       string
}

const channelCols = `id::text, namespace, name, kind, config,
  CASE WHEN secret_ciphertext IS NOT NULL THEN 'sealed' WHEN secret_ref IS NOT NULL THEN 'ref' ELSE '' END,
  COALESCE(secret_ref, ''), secret_hint, rate_limit_per_minute, enabled, created_by, created_at, updated_at, revision`

func scanChannel(row pgx.Row) (NotificationChannel, error) {
	var c NotificationChannel
	var cfg []byte
	err := row.Scan(&c.ID, &c.Namespace, &c.Name, &c.Kind, &cfg, &c.SecretSource, &c.SecretRef, &c.SecretHint,
		&c.RateLimitPerMinute, &c.Enabled, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &c.Revision)
	if err != nil {
		return c, err
	}
	c.Config = map[string]any{}
	_ = json.Unmarshal(cfg, &c.Config)
	return c, nil
}

func jsonb(v any) (string, error) {
	if v == nil {
		return "{}", nil
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// NotifyCreateChannel inserts a channel. The id is chosen by the caller, because
// a sealed secret's associated data names it and must be sealed before insert.
func NotifyCreateChannel(ctx context.Context, q GWQuerier, c NotificationChannel, sec ChannelSecretUpdate) (NotificationChannel, error) {
	cfg, err := jsonb(c.Config)
	if err != nil {
		return c, err
	}
	var ref any
	if sec.Ref != "" {
		ref = sec.Ref
	}
	row := q.QueryRow(ctx, `
INSERT INTO notification_channels (id, namespace, name, kind, config, secret_ciphertext, secret_ref, secret_hint,
                                   rate_limit_per_minute, enabled, created_by)
VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, $10, $11)
RETURNING `+channelCols, c.ID, c.Namespace, c.Name, c.Kind, cfg, sec.Ciphertext, ref, sec.Hint,
		c.RateLimitPerMinute, c.Enabled, c.CreatedBy)
	return scanChannel(row)
}

// NotifyUpdateChannel updates a channel by name. revision 0 skips the check.
func NotifyUpdateChannel(ctx context.Context, q GWQuerier, c NotificationChannel, sec ChannelSecretUpdate, revision int64) (NotificationChannel, error) {
	cfg, err := jsonb(c.Config)
	if err != nil {
		return c, err
	}
	var ref any
	if sec.Ref != "" {
		ref = sec.Ref
	}
	row := q.QueryRow(ctx, `
UPDATE notification_channels
   SET config = $3::jsonb,
       secret_ciphertext = CASE WHEN $4 THEN secret_ciphertext ELSE $5 END,
       secret_ref        = CASE WHEN $4 THEN secret_ref ELSE $6 END,
       secret_hint       = CASE WHEN $4 THEN secret_hint ELSE $7 END,
       rate_limit_per_minute = $8, enabled = $9,
       updated_at = now(), revision = revision + 1
 WHERE namespace = $1 AND name = $2 AND ($10 = 0 OR revision = $10)
RETURNING `+channelCols, c.Namespace, c.Name, cfg, sec.Keep, sec.Ciphertext, ref, sec.Hint,
		c.RateLimitPerMinute, c.Enabled, revision)
	out, err := scanChannel(row)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := NotifyGetChannel(ctx, q, c.Namespace, c.Name); gerr == nil {
			return out, ErrRevisionConflict
		}
		return out, ErrNotifyNotFound
	}
	return out, err
}

// NotifyGetChannel reads one channel by name or id.
func NotifyGetChannel(ctx context.Context, q GWQuerier, ns, nameOrID string) (NotificationChannel, error) {
	c, err := scanChannel(q.QueryRow(ctx, `SELECT `+channelCols+` FROM notification_channels
 WHERE namespace = $1 AND (name = $2 OR id::text = $2)`, ns, nameOrID))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotifyNotFound
	}
	return c, err
}

// NotifyChannelSecret reads a channel's sealed secret and reference. The only
// read that returns secret material; the gateway uses it to re-seal on rotation.
func NotifyChannelSecret(ctx context.Context, q GWQuerier, ns, id string) ([]byte, string, error) {
	var ct []byte
	var ref *string
	err := q.QueryRow(ctx, `SELECT secret_ciphertext, secret_ref FROM notification_channels WHERE namespace = $1 AND id::text = $2`, ns, id).Scan(&ct, &ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotifyNotFound
	}
	return ct, deref(ref), err
}

// NotifyListChannels lists a namespace's channels.
func NotifyListChannels(ctx context.Context, q GWQuerier, ns string) ([]NotificationChannel, error) {
	rows, err := q.Query(ctx, `SELECT `+channelCols+` FROM notification_channels WHERE namespace = $1 ORDER BY name LIMIT 1000`, ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotificationChannel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotifyDeleteChannel removes a channel (and, by cascade, its rule links and
// delivery history). Returns the deleted id.
func NotifyDeleteChannel(ctx context.Context, q GWQuerier, ns, name string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `DELETE FROM notification_channels WHERE namespace = $1 AND name = $2 RETURNING id::text`, ns, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotifyNotFound
	}
	return id, err
}

// ── rules ──────────────────────────────────────────────────────────────────

// NotificationRule is a rule and the channels it targets.
type NotificationRule struct {
	ID         string
	Namespace  string
	Name       string
	Events     []string
	Workflows  []string
	Labels     map[string]string
	SLASeconds *int32
	Enabled    bool
	Channels   []string // channel names
	ChannelIDs []string
	CreatedBy  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Revision   int64
}

const ruleCols = `r.id::text, r.namespace, r.name, r.events, r.workflows, r.labels, r.sla_seconds, r.enabled,
  r.created_by, r.created_at, r.updated_at, r.revision,
  COALESCE((SELECT array_agg(c.name ORDER BY c.name) FROM notification_rule_channels rc
             JOIN notification_channels c ON c.id = rc.channel_id WHERE rc.rule_id = r.id), '{}'),
  COALESCE((SELECT array_agg(c.id::text ORDER BY c.name) FROM notification_rule_channels rc
             JOIN notification_channels c ON c.id = rc.channel_id WHERE rc.rule_id = r.id), '{}')`

func scanRule(row pgx.Row) (NotificationRule, error) {
	var r NotificationRule
	var labels []byte
	err := row.Scan(&r.ID, &r.Namespace, &r.Name, &r.Events, &r.Workflows, &labels, &r.SLASeconds, &r.Enabled,
		&r.CreatedBy, &r.CreatedAt, &r.UpdatedAt, &r.Revision, &r.Channels, &r.ChannelIDs)
	if err != nil {
		return r, err
	}
	r.Labels = map[string]string{}
	_ = json.Unmarshal(labels, &r.Labels)
	return r, nil
}

// NotifyResolveChannelIDs maps channel names to ids in a namespace, failing on
// any name that does not exist.
func NotifyResolveChannelIDs(ctx context.Context, q GWQuerier, ns string, names []string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT name, id::text FROM notification_channels WHERE namespace = $1 AND name = ANY($2)`, ns, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byName := map[string]string{}
	for rows.Next() {
		var n, id string
		if err := rows.Scan(&n, &id); err != nil {
			return nil, err
		}
		byName[n] = id
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		id, ok := byName[n]
		if !ok {
			return nil, fmt.Errorf("%w: channel %q", ErrNotifyNotFound, n)
		}
		out = append(out, id)
	}
	return out, nil
}

// NotifyUpsertRule creates (revision < 0) or updates a rule and replaces its
// channel links, in the caller's transaction.
func NotifyUpsertRule(ctx context.Context, tx pgx.Tx, r NotificationRule, create bool, revision int64) (NotificationRule, error) {
	labels, err := jsonb(r.Labels)
	if err != nil {
		return r, err
	}
	if r.Workflows == nil {
		r.Workflows = []string{}
	}
	var id string
	if create {
		err = tx.QueryRow(ctx, `
INSERT INTO notification_rules (namespace, name, events, workflows, labels, sla_seconds, enabled, created_by)
VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8) RETURNING id::text`,
			r.Namespace, r.Name, r.Events, r.Workflows, labels, r.SLASeconds, r.Enabled, r.CreatedBy).Scan(&id)
	} else {
		err = tx.QueryRow(ctx, `
UPDATE notification_rules
   SET events = $3, workflows = $4, labels = $5::jsonb, sla_seconds = $6, enabled = $7,
       updated_at = now(), revision = revision + 1
 WHERE namespace = $1 AND name = $2 AND ($8 = 0 OR revision = $8)
RETURNING id::text`, r.Namespace, r.Name, r.Events, r.Workflows, labels, r.SLASeconds, r.Enabled, revision).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			if _, gerr := NotifyGetRule(ctx, tx, r.Namespace, r.Name); gerr == nil {
				return r, ErrRevisionConflict
			}
			return r, ErrNotifyNotFound
		}
	}
	if err != nil {
		return r, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM notification_rule_channels WHERE rule_id = $1`, id); err != nil {
		return r, err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO notification_rule_channels (rule_id, channel_id, namespace)
SELECT $1, c, $2 FROM unnest($3::uuid[]) c ON CONFLICT DO NOTHING`, id, r.Namespace, r.ChannelIDs); err != nil {
		return r, err
	}
	return NotifyGetRule(ctx, tx, r.Namespace, r.Name)
}

// NotifyGetRule reads one rule by name or id.
func NotifyGetRule(ctx context.Context, q GWQuerier, ns, nameOrID string) (NotificationRule, error) {
	r, err := scanRule(q.QueryRow(ctx, `SELECT `+ruleCols+` FROM notification_rules r WHERE r.namespace = $1 AND (r.name = $2 OR r.id::text = $2)`, ns, nameOrID))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotifyNotFound
	}
	return r, err
}

// NotifyListRules lists a namespace's rules.
func NotifyListRules(ctx context.Context, q GWQuerier, ns string) ([]NotificationRule, error) {
	rows, err := q.Query(ctx, `SELECT `+ruleCols+` FROM notification_rules r WHERE r.namespace = $1 ORDER BY r.name LIMIT 1000`, ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotificationRule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// NotifyDeleteRule deletes a rule by name, returning its id.
func NotifyDeleteRule(ctx context.Context, q GWQuerier, ns, name string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `DELETE FROM notification_rules WHERE namespace = $1 AND name = $2 RETURNING id::text`, ns, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotifyNotFound
	}
	return id, err
}

// ── deliveries (read + insert-only) ─────────────────────────────────────────

// NotificationDelivery is a delivery's read projection.
type NotificationDelivery struct {
	ID            int64
	Namespace     string
	ChannelID     string
	ChannelName   string
	ChannelKind   string
	RuleID        string
	RuleName      string
	EventKey      string
	DedupKey      string
	EventType     string
	Action        string
	RunID         string
	Payload       map[string]any
	State         string
	Attempts      int32
	MaxAttempts   int32
	NextAttemptAt time.Time
	LastError     string
	LastStatus    *int32
	RedriveOf     *int64
	CreatedBy     string
	CreatedAt     time.Time
	DeliveredAt   *time.Time
	DeadAt        *time.Time
}

// DeliveryFilter narrows a delivery listing.
type DeliveryFilter struct {
	State     string
	ChannelID string
	RunID     string
	BeforeID  int64
	Limit     int
}

const deliveryCols = `d.id, d.namespace, d.channel_id::text, c.name, c.kind, COALESCE(d.rule_id::text, ''),
  COALESCE(r.name, ''), d.event_key, d.dedup_key, d.event_type, d.action, COALESCE(d.run_id::text, ''), d.payload,
  d.state, d.attempts, d.max_attempts, d.next_attempt_at, COALESCE(d.last_error, ''), d.last_status, d.redrive_of,
  d.created_by, d.created_at, d.delivered_at, d.dead_at`

func scanDelivery(row pgx.Row) (NotificationDelivery, error) {
	var d NotificationDelivery
	var payload []byte
	err := row.Scan(&d.ID, &d.Namespace, &d.ChannelID, &d.ChannelName, &d.ChannelKind, &d.RuleID, &d.RuleName,
		&d.EventKey, &d.DedupKey, &d.EventType, &d.Action, &d.RunID, &payload, &d.State, &d.Attempts, &d.MaxAttempts,
		&d.NextAttemptAt, &d.LastError, &d.LastStatus, &d.RedriveOf, &d.CreatedBy, &d.CreatedAt, &d.DeliveredAt, &d.DeadAt)
	if err != nil {
		return d, err
	}
	d.Payload = map[string]any{}
	_ = json.Unmarshal(payload, &d.Payload)
	return d, nil
}

// NotifyListDeliveries pages a namespace's deliveries, newest first.
func NotifyListDeliveries(ctx context.Context, q GWQuerier, ns string, f DeliveryFilter) ([]NotificationDelivery, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	rows, err := q.Query(ctx, `SELECT `+deliveryCols+`
  FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  LEFT JOIN notification_rules r ON r.id = d.rule_id
 WHERE d.namespace = $1
   AND ($2 = '' OR d.state = $2)
   AND ($3 = '' OR d.channel_id::text = $3)
   AND ($4 = '' OR d.run_id::text = $4)
   AND ($5 = 0 OR d.id < $5)
 ORDER BY d.id DESC
 LIMIT $6`, ns, f.State, f.ChannelID, f.RunID, f.BeforeID, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotificationDelivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// NotifyGetDelivery reads one delivery.
func NotifyGetDelivery(ctx context.Context, q GWQuerier, ns string, id int64) (NotificationDelivery, error) {
	d, err := scanDelivery(q.QueryRow(ctx, `SELECT `+deliveryCols+`
  FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  LEFT JOIN notification_rules r ON r.id = d.rule_id
 WHERE d.namespace = $1 AND d.id = $2`, ns, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotifyNotFound
	}
	return d, err
}

// NotifyRedriveCount counts earlier redrives of a delivery, to name the next.
func NotifyRedriveCount(ctx context.Context, q GWQuerier, id int64) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*)::int FROM notification_deliveries WHERE redrive_of = $1`, id).Scan(&n)
	return n, err
}
