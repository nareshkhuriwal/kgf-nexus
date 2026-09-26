// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package notifier evaluates announced facts against notification rules, keeps
// the delivery queue moving, and records declared asset lineage (ADR-0028).
//
// It runs in the event-service beside the outbox relay and shares nothing with
// it except the outbox rows, which the two claim on different predicates with
// SKIP LOCKED. Three independent loops:
//
//   - evaluate: claim un-notified run_state/run_event rows, write deliveries,
//     incidents and lineage, mark the rows -- one transaction per batch, so the
//     outputs of a fact exist exactly when the fact is marked.
//   - sla: enqueue run.sla_exceeded for active runs past a rule's SLA.
//   - dispatch: lease due deliveries, send them outside any transaction, record
//     the outcome under a fence on the lease.
//
// None needs NATS, and none needs a leader: every claim is SKIP LOCKED and every
// output is keyed by a unique constraint.
package notifier

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/notify"
	"github.com/khuriwalgroup/kgf-nexus/internal/secretbox"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// Config tunes the loops.
type Config struct {
	EvalBatch        int
	EvalInterval     time.Duration
	SLAInterval      time.Duration
	DispatchBatch    int
	DispatchInterval time.Duration
	Concurrency      int
	Lease            time.Duration
	BaseBackoff      time.Duration
	MaxBackoff       time.Duration
	Retention        time.Duration
	PruneInterval    time.Duration
	// ConsoleURL, when set, links each notification to its run.
	ConsoleURL string
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		EvalBatch: 200, EvalInterval: time.Second, SLAInterval: 30 * time.Second,
		DispatchBatch: 50, DispatchInterval: time.Second, Concurrency: 8,
		Lease: 60 * time.Second, BaseBackoff: 30 * time.Second, MaxBackoff: time.Hour,
		Retention: 30 * 24 * time.Hour, PruneInterval: 10 * time.Minute,
	}
}

// ConfigFromEnv overlays NEXUS_NOTIFY_* variables. Invalid values are ignored
// with a warning, as the relay does.
func ConfigFromEnv(log *slog.Logger) Config {
	c := DefaultConfig()
	dur := func(k string, dst *time.Duration) {
		if v := os.Getenv(k); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				*dst = d
			} else {
				log.Warn("ignoring invalid setting", "key", k, "value", v)
			}
		}
	}
	num := func(k string, dst *int) {
		if v := os.Getenv(k); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				*dst = n
			} else {
				log.Warn("ignoring invalid setting", "key", k, "value", v)
			}
		}
	}
	num("NEXUS_NOTIFY_EVAL_BATCH", &c.EvalBatch)
	dur("NEXUS_NOTIFY_EVAL_INTERVAL", &c.EvalInterval)
	dur("NEXUS_NOTIFY_SLA_INTERVAL", &c.SLAInterval)
	num("NEXUS_NOTIFY_DISPATCH_BATCH", &c.DispatchBatch)
	dur("NEXUS_NOTIFY_DISPATCH_INTERVAL", &c.DispatchInterval)
	num("NEXUS_NOTIFY_CONCURRENCY", &c.Concurrency)
	dur("NEXUS_NOTIFY_BASE_BACKOFF", &c.BaseBackoff)
	dur("NEXUS_NOTIFY_MAX_BACKOFF", &c.MaxBackoff)
	dur("NEXUS_NOTIFY_RETENTION", &c.Retention)
	c.ConsoleURL = strings.TrimRight(os.Getenv("NEXUS_CONSOLE_URL"), "/")
	return c
}

// Metrics are the notifier's instruments. Labels come from fixed sets.
type Metrics struct {
	Evaluated   *metrics.Counter
	Enqueued    *metrics.CounterVec
	Sent        *metrics.CounterVec
	DeadLetters *metrics.CounterVec
	RateLimited *metrics.Counter
	Backlog     *metrics.Gauge
	DeadRecent  *metrics.Gauge
	OldestAge   *metrics.Gauge
	Lineage     *metrics.CounterVec
	EvalErrors  *metrics.Counter
}

// NewMetrics registers the notifier metrics.
func NewMetrics(reg *metrics.Registry) *Metrics {
	return &Metrics{
		Evaluated: reg.NewCounter("nexus_notify_facts_evaluated_total",
			"Outbox facts (run state changes and run events) evaluated against notification rules.").Counter(),
		Enqueued: reg.NewCounter("nexus_notify_deliveries_enqueued_total",
			"Notification deliveries enqueued, by event type.", "event"),
		Sent: reg.NewCounter("nexus_notify_send_attempts_total",
			"Notification send attempts, by channel kind and outcome (delivered, retry, dead).", "kind", "outcome"),
		DeadLetters: reg.NewCounter("nexus_notify_dead_letters_total",
			"Deliveries moved to the dead-letter state, by channel kind.", "kind"),
		RateLimited: reg.NewCounter("nexus_notify_rate_limited_total",
			"Sends deferred because the channel's per-minute limit was reached.").Counter(),
		Backlog: reg.NewGauge("nexus_notify_pending_deliveries",
			"Deliveries waiting to be sent (including those backing off).").Gauge(),
		DeadRecent: reg.NewGauge("nexus_notify_dead_deliveries_24h",
			"Deliveries that became dead letters in the last 24 hours.").Gauge(),
		OldestAge: reg.NewGauge("nexus_notify_oldest_due_delivery_seconds",
			"Age of the oldest delivery that is due and not yet sent.").Gauge(),
		Lineage: reg.NewCounter("nexus_lineage_records_total",
			"Lineage rows recorded, by kind (edge, materialization).", "kind"),
		EvalErrors: reg.NewCounter("nexus_notify_eval_errors_total",
			"Facts whose evaluation failed and will be retried.").Counter(),
	}
}

// Notifier is the three loops.
type Notifier struct {
	pool   *pgxpool.Pool
	box    *secretbox.Box
	sender *notify.Sender
	cfg    Config
	log    *slog.Logger
	m      *Metrics
	owner  string
	now    func() time.Time
	getenv func(string) string
}

// New builds a notifier. box may be nil (sealed channel secrets then fail
// permanently with a clear error); sender must not be nil.
func New(pool *pgxpool.Pool, box *secretbox.Box, sender *notify.Sender, cfg Config, log *slog.Logger, m *Metrics) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	if m == nil {
		m = NewMetrics(metrics.NewRegistry())
	}
	d := DefaultConfig()
	if cfg.EvalBatch <= 0 {
		cfg.EvalBatch = d.EvalBatch
	}
	if cfg.DispatchBatch <= 0 {
		cfg.DispatchBatch = d.DispatchBatch
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = d.Concurrency
	}
	if cfg.Lease <= 0 {
		cfg.Lease = d.Lease
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = d.BaseBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = d.MaxBackoff
	}
	if cfg.Retention <= 0 {
		cfg.Retention = d.Retention
	}
	host, _ := os.Hostname()
	return &Notifier{pool: pool, box: box, sender: sender, cfg: cfg, log: log.With("component", "notifier"), m: m,
		owner: fmt.Sprintf("%s/%s", host, uuid.NewString()[:8]), now: time.Now, getenv: os.Getenv}
}

// Run drives every loop until ctx is cancelled.
func (n *Notifier) Run(ctx context.Context) {
	var wg sync.WaitGroup
	loop := func(name string, every time.Duration, fn func(context.Context) (int, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				did, err := fn(ctx)
				if err != nil && ctx.Err() == nil {
					n.log.Error("notifier loop pass failed; retrying", "loop", name, "error", err)
				}
				if did > 0 && err == nil {
					continue // a backlog: keep draining
				}
				select {
				case <-ctx.Done():
				case <-time.After(every):
				}
			}
		}()
	}
	loop("evaluate", n.cfg.EvalInterval, n.EvaluateBatch)
	loop("sla", n.cfg.SLAInterval, func(ctx context.Context) (int, error) { _, err := n.SweepSLA(ctx); return 0, err })
	loop("dispatch", n.cfg.DispatchInterval, n.DispatchOnce)
	loop("maintenance", n.cfg.PruneInterval, func(ctx context.Context) (int, error) { return 0, n.maintain(ctx) })
	wg.Wait()
}

func (n *Notifier) maintain(ctx context.Context) error {
	pending, dead, oldest, err := store.NotifyBacklog(ctx, n.pool)
	if err != nil {
		return err
	}
	n.m.Backlog.Set(float64(pending))
	n.m.DeadRecent.Set(float64(dead))
	n.m.OldestAge.Set(oldest)
	pruned, err := store.NotifyPrune(ctx, n.pool, n.cfg.Retention)
	if pruned > 0 {
		n.log.Info("pruned old notification deliveries", "rows", pruned, "retention", n.cfg.Retention)
	}
	return err
}

// ── evaluate ───────────────────────────────────────────────────────────────

var stateEvents = map[string]string{
	"FAILED":    notify.EventRunFailed,
	"SUCCEEDED": notify.EventRunSucceeded,
	"TIMED_OUT": notify.EventRunTimedOut,
}

// EvaluateBatch evaluates one batch of facts. Returns how many were claimed.
func (n *Notifier) EvaluateBatch(ctx context.Context) (int, error) {
	tx, err := n.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	facts, err := store.NotifyClaimFacts(ctx, tx, n.cfg.EvalBatch)
	if err != nil || len(facts) == 0 {
		return 0, err
	}
	done := make([]int64, 0, len(facts))
	for _, f := range facts {
		// A savepoint per fact: one fact that fails (a bug, a constraint
		// surprise) is rolled back alone and retried next pass, instead of
		// wedging the batch behind it. Its row stays unmarked, so it is
		// re-evaluated; the relay's max-age expiry bounds a truly poisoned row.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return 0, err
		}
		if perr := n.evaluate(ctx, sp, f); perr != nil {
			_ = sp.Rollback(ctx)
			n.m.EvalErrors.Inc()
			n.log.Error("could not evaluate a fact; it will be retried",
				"outbox_id", f.OutboxID, "topic", f.Topic, "run_id", f.RunID, "error", perr)
			continue
		}
		if err := sp.Commit(ctx); err != nil {
			return 0, err
		}
		done = append(done, f.OutboxID)
	}
	if err := store.NotifyMarkFacts(ctx, tx, done); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	n.m.Evaluated.Add(float64(len(done)))
	return len(facts), nil
}

func (n *Notifier) evaluate(ctx context.Context, tx pgx.Tx, f store.NotifyFact) error {
	if f.RunID == "" {
		return nil // a deleted run's event, or a malformed row: nothing to do
	}
	switch f.Topic {
	case store.OutboxTopicRunState:
		// Lineage first: the run's declared edges are recorded the first time
		// any of its state changes is seen.
		if k, err := store.LineageRecordRun(ctx, tx, f.RunID); err != nil {
			return err
		} else if k > 0 {
			n.m.Lineage.With(metrics.Labels{"kind": "edge"}).Add(float64(k))
		}
		ev, ok := stateEvents[f.State]
		if !ok {
			return nil
		}
		rc, err := store.NotifyRunContext(ctx, tx, f.RunID)
		if errors.Is(err, store.ErrRunGone) {
			return nil
		}
		if err != nil {
			return err
		}
		key := fmt.Sprintf("run_state:%s:%d", f.RunID, f.StateVersion)
		payload := n.runPayload(ctx, tx, rc, f.State, f.OccurredAt)
		if err := n.fanOut(ctx, tx, rc, ev, key, f.OccurredAt, payload); err != nil {
			return err
		}
		if ev == notify.EventRunSucceeded {
			return n.resolveIncidents(ctx, tx, rc, key, payload)
		}
		return nil
	case store.OutboxTopicRunEvent:
		switch f.EventKind {
		case domain.EventAttemptLost:
			rc, err := store.NotifyRunContext(ctx, tx, f.RunID)
			if errors.Is(err, store.ErrRunGone) {
				return nil
			}
			if err != nil {
				return err
			}
			payload := map[string]any{
				"workflow": rc.Workflow, "run_id": rc.RunID, "node_id": f.NodeID, "attempt_id": f.AttemptID,
				"state": "LOST", "occurred_at": f.OccurredAt.UTC().Format(time.RFC3339Nano),
				"summary": fmt.Sprintf("Attempt of node %s was LOST (outcome unknown): %v", f.NodeID, f.EventBody["reason"]),
				"details": map[string]any{"reason": f.EventBody["reason"], "worker_id": f.EventBody["worker_id"]},
			}
			return n.fanOut(ctx, tx, rc, notify.EventAttemptLost, fmt.Sprintf("run_event:%d", f.EventID), f.OccurredAt, payload)
		case "AttemptSucceeded", "NodeStateChanged":
			if f.EventKind == "NodeStateChanged" && f.EventBody["state"] != "SUCCEEDED" {
				return nil
			}
			// A map child ("load[3]") materializes nothing on its own; its
			// template's settle event does.
			if strings.Contains(f.NodeID, "[") || f.NodeID == "" {
				return nil
			}
			if _, err := store.LineageRecordRun(ctx, tx, f.RunID); err != nil {
				return err
			}
			k, err := store.LineageRecordMaterialization(ctx, tx, f.RunID, f.NodeID, f.OccurredAt)
			if k > 0 {
				n.m.Lineage.With(metrics.Labels{"kind": "materialization"}).Add(float64(k))
			}
			return err
		}
	}
	return nil
}

func (n *Notifier) runPayload(ctx context.Context, tx pgx.Tx, rc store.RunContext, state string, at time.Time) map[string]any {
	p := map[string]any{
		"workflow": rc.Workflow, "run_id": rc.RunID, "state": state, "trigger": rc.Trigger,
		"occurred_at": at.UTC().Format(time.RFC3339Nano),
	}
	switch state {
	case "FAILED":
		if nodes, err := store.NotifyFailedNodes(ctx, tx, rc.RunID, 5); err == nil && len(nodes) > 0 {
			p["summary"] = "Failed nodes: " + strings.Join(nodes, ", ")
			p["details"] = map[string]any{"failed_nodes": nodes}
		} else {
			p["summary"] = "The run failed."
		}
	case "TIMED_OUT":
		p["summary"] = "The run exceeded its run_timeout and was stopped."
	case "SUCCEEDED":
		p["summary"] = "The run succeeded."
	}
	return p
}

// fanOut enqueues one delivery per matched (rule, channel).
func (n *Notifier) fanOut(ctx context.Context, tx pgx.Tx, rc store.RunContext, ev, key string, at time.Time, payload map[string]any) error {
	targets, err := store.NotifyMatchRules(ctx, tx, rc, ev, at)
	if err != nil {
		return err
	}
	for _, t := range targets {
		d := store.DeliveryInsert{Namespace: rc.Namespace, ChannelID: t.ChannelID, RuleID: t.RuleID,
			EventKey: key, EventType: ev, Action: notify.ActionTrigger, RunID: rc.RunID, Payload: payload}
		if t.ChannelKind == notify.KindPagerDuty {
			if ev == notify.EventRunSucceeded {
				// A success is not an incident. PagerDuty hears about it only as
				// the resolve of an incident this platform opened.
				continue
			}
			dk, err := store.NotifyOpenIncident(ctx, tx, rc.Namespace, t.RuleID, t.ChannelID, rc.Workflow, key, rc.RunID)
			if err != nil {
				return err
			}
			d.DedupKey = dk
		}
		if _, inserted, err := store.NotifyEnqueue(ctx, tx, d); err != nil {
			return err
		} else if inserted {
			n.m.Enqueued.With(metrics.Labels{"event": ev}).Inc()
		}
	}
	return nil
}

func (n *Notifier) resolveIncidents(ctx context.Context, tx pgx.Tx, rc store.RunContext, key string, payload map[string]any) error {
	open, err := store.NotifyResolveIncidents(ctx, tx, rc.Namespace, rc.Workflow, rc.RunID)
	if err != nil {
		return err
	}
	for _, inc := range open {
		if _, _, err := store.NotifyEnqueue(ctx, tx, store.DeliveryInsert{
			Namespace: rc.Namespace, ChannelID: inc.ChannelID, RuleID: inc.RuleID,
			EventKey: "resolve:" + key, DedupKey: inc.DedupKey, EventType: notify.EventRunSucceeded,
			Action: notify.ActionResolve, RunID: rc.RunID, Payload: payload,
		}); err != nil {
			return err
		}
		n.m.Enqueued.With(metrics.Labels{"event": "resolve"}).Inc()
	}
	return nil
}

// SweepSLA enqueues run.sla_exceeded for overdue runs. Idempotent: the event
// key is (rule, run), unique per channel.
func (n *Notifier) SweepSLA(ctx context.Context) (int, error) {
	tx, err := n.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	overdue, err := store.NotifyFindOverdue(ctx, tx, 500)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, o := range overdue {
		key := fmt.Sprintf("sla:%s:%s", o.RuleID, o.RunID)
		age := n.now().Sub(o.CreatedAt).Round(time.Second)
		payload := map[string]any{
			"workflow": o.Workflow, "run_id": o.RunID, "state": o.State, "trigger": o.Trigger,
			"occurred_at": n.now().UTC().Format(time.RFC3339Nano),
			"summary":     fmt.Sprintf("The run has been active for %s, past the %ds SLA of rule %s.", age, o.SLASeconds, o.RuleName),
			"details":     map[string]any{"sla_seconds": o.SLASeconds, "age_seconds": int64(age.Seconds()), "rule": o.RuleName},
		}
		d := store.DeliveryInsert{Namespace: o.Namespace, ChannelID: o.ChannelID, RuleID: o.RuleID, EventKey: key,
			EventType: notify.EventRunSLAExceeded, Action: notify.ActionTrigger, RunID: o.RunID, Payload: payload}
		if o.ChannelKind == notify.KindPagerDuty {
			dk, err := store.NotifyOpenIncident(ctx, tx, o.Namespace, o.RuleID, o.ChannelID, o.Workflow, key, o.RunID)
			if err != nil {
				return count, err
			}
			d.DedupKey = dk
		}
		if _, ins, err := store.NotifyEnqueue(ctx, tx, d); err != nil {
			return count, err
		} else if ins {
			count++
			n.m.Enqueued.With(metrics.Labels{"event": notify.EventRunSLAExceeded}).Inc()
		}
	}
	return count, tx.Commit(ctx)
}

// ── dispatch ───────────────────────────────────────────────────────────────

// DispatchOnce leases and sends one batch. Returns how many were claimed.
func (n *Notifier) DispatchOnce(ctx context.Context) (int, error) {
	batch, err := store.NotifyClaimDeliveries(ctx, n.pool, n.owner, n.cfg.DispatchBatch, n.cfg.Lease)
	if err != nil || len(batch) == 0 {
		return 0, err
	}
	sem := make(chan struct{}, n.cfg.Concurrency)
	var wg sync.WaitGroup
	for _, d := range batch {
		wg.Add(1)
		sem <- struct{}{}
		go func(d store.ClaimedDelivery) {
			defer wg.Done()
			defer func() { <-sem }()
			n.deliver(ctx, d)
		}(d)
	}
	wg.Wait()
	return len(batch), nil
}

func (n *Notifier) deliver(ctx context.Context, d store.ClaimedDelivery) {
	log := n.log.With("delivery_id", d.ID, "channel", d.ChannelName, "kind", d.ChannelKind, "event", d.EventType)
	fail := func(status int, msg string, permanent bool, retryAfter time.Duration) {
		retryAt := n.now().Add(n.backoff(d.Attempts, retryAfter))
		dead, err := store.NotifyRecordFailure(ctx, n.pool, d.ID, n.owner, status, msg, permanent, retryAt)
		switch {
		case errors.Is(err, store.ErrLeaseLost):
			log.Warn("delivery lease lost before its outcome was recorded; another dispatcher owns it")
		case err != nil:
			log.Error("could not record a delivery failure; the lease will expire and it will be retried", "error", err)
		case dead:
			n.m.Sent.With(metrics.Labels{"kind": d.ChannelKind, "outcome": "dead"}).Inc()
			n.m.DeadLetters.With(metrics.Labels{"kind": d.ChannelKind}).Inc()
			log.Warn("notification moved to dead letters", "attempts", d.Attempts+1, "permanent", permanent, "error", msg)
		default:
			n.m.Sent.With(metrics.Labels{"kind": d.ChannelKind, "outcome": "retry"}).Inc()
			log.Info("notification send failed; will retry", "attempt", d.Attempts+1, "retry_at", retryAt, "error", msg)
		}
	}
	if !d.ChannelEnabled {
		fail(0, "the channel is disabled", true, 0)
		return
	}
	secret, err := n.secret(d)
	if err != nil {
		fail(0, err.Error(), true, 0)
		return
	}
	if ok, reset, err := store.NotifyTakeRateSlot(ctx, n.pool, d.Namespace, d.ChannelID, d.RateLimitPerMin); err != nil {
		fail(0, "rate limiter unavailable: "+err.Error(), false, 0)
		return
	} else if !ok {
		n.m.RateLimited.Inc()
		if err := store.NotifyDefer(ctx, n.pool, d.ID, n.owner, reset,
			fmt.Sprintf("deferred: channel limit of %d/minute reached", d.RateLimitPerMin)); err != nil && !errors.Is(err, store.ErrLeaseLost) {
			log.Error("could not defer a rate-limited delivery", "error", err)
		}
		return
	}
	ch := notify.Channel{ID: d.ChannelID, Name: d.ChannelName, Kind: d.ChannelKind, Config: d.ChannelConfig, Secret: secret}
	out := n.sender.Send(ctx, ch, n.message(d))
	if out.OK() {
		if err := store.NotifyRecordSuccess(ctx, n.pool, d.ID, n.owner, out.Status); err != nil {
			log.Error("sent, but could not record it; the delivery may be sent again", "error", err)
			return
		}
		n.m.Sent.With(metrics.Labels{"kind": d.ChannelKind, "outcome": "delivered"}).Inc()
		return
	}
	fail(out.Status, out.Err.Error(), out.Permanent, out.RetryAfter)
}

func (n *Notifier) secret(d store.ClaimedDelivery) ([]byte, error) {
	switch {
	case len(d.SecretCiphertext) > 0:
		if n.box == nil {
			return nil, errors.New("the channel secret is sealed, but NEXUS_SECRET_KEY is not configured on the event-service")
		}
		pt, err := n.box.Open(d.SecretCiphertext, secretbox.Context("notification_channels", d.Namespace, d.ChannelID))
		if err != nil {
			return nil, fmt.Errorf("the channel secret could not be opened (wrong or retired NEXUS_SECRET_KEY?): %w", err)
		}
		return pt, nil
	case d.SecretRef != "":
		return secretbox.ResolveRef(d.SecretRef, n.getenv)
	}
	return nil, nil
}

// backoff is exponential in the attempts already made, with full jitter in
// [d/2, d], floored at the receiver's Retry-After.
func (n *Notifier) backoff(attempts int32, retryAfter time.Duration) time.Duration {
	d := n.cfg.BaseBackoff
	for i := int32(0); i < attempts && d < n.cfg.MaxBackoff; i++ {
		d *= 2
	}
	if d > n.cfg.MaxBackoff {
		d = n.cfg.MaxBackoff
	}
	half := d / 2
	d = half + time.Duration(rand.Int64N(int64(d-half)+1))
	if retryAfter > d {
		d = retryAfter
	}
	return d
}

func (n *Notifier) message(d store.ClaimedDelivery) notify.Message {
	s := func(k string) string {
		v, _ := d.Payload[k].(string)
		return v
	}
	m := notify.Message{
		DeliveryID: d.ID, DedupKey: d.DedupKey, EventType: d.EventType, Action: d.Action,
		Namespace: d.Namespace, Workflow: s("workflow"), RunID: d.RunID, NodeID: s("node_id"),
		AttemptID: s("attempt_id"), State: s("state"), Summary: s("summary"),
	}
	if t, err := time.Parse(time.RFC3339Nano, s("occurred_at")); err == nil {
		m.OccurredAt = t
	} else {
		m.OccurredAt = d.CreatedAt
	}
	if det, ok := d.Payload["details"].(map[string]any); ok {
		m.Details = det
	}
	if n.cfg.ConsoleURL != "" && d.RunID != "" {
		m.Link = n.cfg.ConsoleURL + "/runs/" + url.PathEscape(d.RunID)
	}
	return m
}
