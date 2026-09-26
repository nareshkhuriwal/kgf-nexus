// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package relay publishes the transactional outbox (migration 0016) to NATS
// JetStream.
//
// WHY THE EVENT-SERVICE OWNS IT. ADR-0011 gives the event-service run_events and
// ADR-0003 makes run-event fan-out the bus's second job, so the service that owns
// the history is the natural owner of its projection. It is also the one service
// that is NOT on the dispatch path: a broker outage, a slow publish or a relay bug
// here can delay a notification but can never hold a claim transaction's locks or
// stall a scheduler. Putting the relay in the scheduler would have coupled the
// one correctness-critical loop to the one component allowed to be down.
//
// THE CONTRACT, in the order a reviewer should check it:
//
//  1. At-least-once from the outbox's point of view: a row is marked published
//     only after the broker acknowledged it, in the same transaction that held
//     the row's lock. A crash anywhere before that COMMIT leaves the row
//     unpublished and it is sent again.
//  2. Effectively-once into the stream: every message carries a Nats-Msg-Id
//     derived from the row (bus.MsgID), so the re-send in (1) is dropped by
//     JetStream inside its duplicate window.
//  3. Safe with N replicas: rows are claimed FOR UPDATE SKIP LOCKED, so two
//     relays never publish the same row concurrently and never block on each
//     other.
//  4. Degrades, never blocks: with the broker down the relay backs off and the
//     backlog waits in Postgres; nothing upstream notices. Retention is bounded:
//     published rows are pruned after Retention, and rows that could not be
//     published within MaxAge are dropped (and counted) -- they announce facts
//     Postgres still holds, so dropping them costs a late or missing hint, never
//     data.
//
// ORDERING IS NOT GUARANTEED, and consumers must not assume it. Rows are claimed
// in id order, but ids are assigned at INSERT and committed in a different order,
// retries reorder, and two replicas interleave. Every message carries what a
// consumer needs to order or discard it: run events carry seq, run state carries
// state_version, and a dispatch hint carries nothing worth ordering.
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/khuriwalgroup/kgf-nexus/internal/bus"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// ErrBrokerUnavailable means the relay could not reach JetStream at all. It is
// the expected error during an outage and drives the relay-wide backoff, as
// opposed to a per-row failure, which backs off only that row.
var ErrBrokerUnavailable = errors.New("relay: nats broker unavailable")

// Config tunes the relay. Every field has a working default (DefaultConfig).
type Config struct {
	// BatchSize is how many rows one transaction claims and publishes. Large
	// enough to drain a post-outage backlog quickly, small enough that one batch's
	// row locks and publish wait stay short.
	BatchSize int
	// PollInterval is the pause after a batch that found less than a full batch.
	// It is the relay's contribution to end-to-end hint latency.
	PollInterval time.Duration
	// PublishTimeout bounds the wait for a batch's acks. The transaction holding
	// the batch's row locks stays open for at most this long.
	PublishTimeout time.Duration
	// MinBackoff and MaxBackoff bound the relay-wide backoff during an outage.
	// The maximum is also the worst-case delay between the broker returning and
	// the backlog starting to drain (a reconnect is detected sooner; see waitFor).
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// Retention keeps published rows for inspection ("did this event go out,
	// and when") before they are pruned.
	Retention time.Duration
	// MaxAge drops rows that could not be published in this long. Unbounded
	// retention of an unpublishable backlog would turn a long broker outage into
	// a disk-full Postgres outage, which is the one thing the bus must never
	// cause. Also what bounds the table when the bus is disabled entirely.
	MaxAge time.Duration
	// PruneInterval is how often the janitor runs.
	PruneInterval time.Duration
	// BacklogInterval is how often the backlog gauges are sampled.
	BacklogInterval time.Duration
	// HintInterval is how often the reconciler re-announces every queue that has
	// dispatchable PENDING work. Zero disables it.
	HintInterval time.Duration
	// MaxEventPayloadBytes caps the run-event payload copied into a message. A
	// larger payload is replaced by payload_truncated=true, and the consumer reads
	// the full event from the event-service API, which is authoritative anyway.
	MaxEventPayloadBytes int
}

// DefaultConfig returns the production defaults.
func DefaultConfig() Config {
	return Config{
		BatchSize:            256,
		PollInterval:         200 * time.Millisecond,
		PublishTimeout:       5 * time.Second,
		MinBackoff:           250 * time.Millisecond,
		MaxBackoff:           10 * time.Second,
		Retention:            time.Hour,
		MaxAge:               24 * time.Hour,
		PruneInterval:        30 * time.Second,
		BacklogInterval:      5 * time.Second,
		HintInterval:         10 * time.Second,
		MaxEventPayloadBytes: 64 << 10,
	}
}

// ConfigFromEnv overlays NEXUS_OUTBOX_* variables on the defaults. A value that
// does not parse is ignored with the default kept, and reported, rather than
// failing startup: a typo in a tuning knob must not take the event history
// offline.
func ConfigFromEnv(log *slog.Logger) Config {
	c := DefaultConfig()
	envInt(log, "NEXUS_OUTBOX_BATCH_SIZE", &c.BatchSize)
	envDur(log, "NEXUS_OUTBOX_POLL_INTERVAL", &c.PollInterval)
	envDur(log, "NEXUS_OUTBOX_PUBLISH_TIMEOUT", &c.PublishTimeout)
	envDur(log, "NEXUS_OUTBOX_MAX_BACKOFF", &c.MaxBackoff)
	envDur(log, "NEXUS_OUTBOX_RETENTION", &c.Retention)
	envDur(log, "NEXUS_OUTBOX_MAX_AGE", &c.MaxAge)
	envDur(log, "NEXUS_OUTBOX_HINT_INTERVAL", &c.HintInterval)
	return c
}

func envInt(log *slog.Logger, key string, dst *int) {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			log.Warn("ignoring invalid setting", "key", key, "value", v)
			return
		}
		*dst = n
	}
}

func envDur(log *slog.Logger, key string, dst *time.Duration) {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			log.Warn("ignoring invalid setting", "key", key, "value", v)
			return
		}
		*dst = d
	}
}

// Metrics are the relay's instruments. Label values are drawn from small fixed
// sets (topic, failure kind, prune reason) -- never ids -- so cardinality stays
// constant however many runs flow through.
type Metrics struct {
	Backlog        *metrics.Gauge
	OldestAge      *metrics.Gauge
	Connected      *metrics.Gauge
	Enabled        *metrics.Gauge
	Lag            *metrics.HistogramVec
	Published      *metrics.CounterVec
	Duplicates     *metrics.Counter
	Failures       *metrics.CounterVec
	Pruned         *metrics.CounterVec
	HintsPublished *metrics.Counter
}

// NewMetrics registers the relay's metrics on reg.
func NewMetrics(reg *metrics.Registry) *Metrics {
	return &Metrics{
		Backlog: reg.NewGauge("nexus_outbox_backlog_rows",
			"Outbox rows available for publishing but not yet published.").Gauge(),
		OldestAge: reg.NewGauge("nexus_outbox_oldest_unpublished_seconds",
			"Age of the oldest available, unpublished outbox row; the publish lag an operator should alert on.").Gauge(),
		Connected: reg.NewGauge("nexus_outbox_broker_connected",
			"1 when this relay's NATS connection is up, else 0.").Gauge(),
		Enabled: reg.NewGauge("nexus_outbox_relay_enabled",
			"1 when NATS is configured and this relay publishes; 0 when it only prunes.").Gauge(),
		Lag: reg.NewHistogram("nexus_outbox_publish_lag_seconds",
			"Time from outbox row creation to broker acknowledgement.",
			[]float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300, 1800}, "topic"),
		Published: reg.NewCounter("nexus_outbox_published_total",
			"Outbox rows acknowledged by JetStream and marked published.", "topic"),
		Duplicates: reg.NewCounter("nexus_outbox_duplicate_acks_total",
			"Publishes JetStream reported as duplicates (a re-send after a crash or timeout, dropped by Nats-Msg-Id).").Counter(),
		Failures: reg.NewCounter("nexus_outbox_publish_failures_total",
			"Failed publish attempts; kind=broker for outage-wide failures, kind=row for a single row.", "kind"),
		Pruned: reg.NewCounter("nexus_outbox_pruned_total",
			"Outbox rows deleted; reason=published (retention) or reason=expired (never published within max age).", "reason"),
		HintsPublished: reg.NewCounter("nexus_outbox_reconcile_hints_total",
			"Dispatch hints published by the periodic reconciler.").Counter(),
	}
}

// Relay moves outbox rows to JetStream.
type Relay struct {
	pool *pgxpool.Pool
	nc   *nats.Conn // nil when the bus is disabled: prune only
	js   jetstream.JetStream
	cfg  Config
	log  *slog.Logger
	m    *Metrics

	streamsReady   atomic.Bool
	seenReconnects atomic.Uint64

	// beforeMark is a test seam called after the broker acknowledged a batch and
	// before the rows are marked. An error aborts the batch exactly as a crash at
	// that instant would: the transaction rolls back and every row stays
	// unpublished although the broker already has the messages.
	beforeMark func(ids []int64) error
}

// New builds a relay. nc may be nil, in which case the relay never publishes and
// only runs the janitor, so the outbox stays bounded with the bus disabled.
func New(pool *pgxpool.Pool, nc *nats.Conn, cfg Config, log *slog.Logger, m *Metrics) (*Relay, error) {
	if log == nil {
		log = slog.Default()
	}
	if m == nil {
		m = NewMetrics(metrics.NewRegistry())
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultConfig().BatchSize
	}
	r := &Relay{pool: pool, nc: nc, cfg: cfg, log: log.With("component", "outbox-relay"), m: m}
	if nc != nil {
		js, err := jetstream.New(nc,
			jetstream.WithPublishAsyncMaxPending(cfg.BatchSize*2),
			jetstream.WithPublishAsyncTimeout(cfg.PublishTimeout))
		if err != nil {
			return nil, fmt.Errorf("relay: jetstream context: %w", err)
		}
		r.js = js
		m.Enabled.Set(1)
	} else {
		m.Enabled.Set(0)
	}
	return r, nil
}

// Run drives the publish loop, the janitor, the backlog sampler and the hint
// reconciler until ctx is cancelled. Each is independent: a stuck publish must
// not stop pruning, and a slow prune must not add hint latency.
func (r *Relay) Run(ctx context.Context) {
	done := make(chan struct{}, 4)
	loops := 0
	start := func(fn func(context.Context)) {
		loops++
		go func() { fn(ctx); done <- struct{}{} }()
	}
	start(r.pruneLoop)
	start(r.backlogLoop)
	if r.nc != nil {
		start(r.publishLoop)
		if r.cfg.HintInterval > 0 {
			start(r.hintLoop)
		}
	} else {
		r.log.Info("NATS disabled: outbox relay prunes only; the system runs on polling alone")
	}
	for i := 0; i < loops; i++ {
		<-done
	}
}

func (r *Relay) connected() bool {
	return r.nc != nil && r.nc.Status() == nats.CONNECTED
}

// publishLoop publishes batches back to back while there is a backlog, idles
// between empty polls, and backs off exponentially while the broker is down.
func (r *Relay) publishLoop(ctx context.Context) {
	backoff := time.Duration(0)
	for ctx.Err() == nil {
		res, err := r.PublishBatch(ctx)
		r.m.Connected.Set(boolGauge(r.connected()))
		switch {
		case err == nil:
			backoff = 0
			if res.Claimed >= r.cfg.BatchSize {
				continue // a backlog: keep draining without sleeping
			}
			r.waitFor(ctx, r.cfg.PollInterval, true)
		case ctx.Err() != nil:
			return
		default:
			backoff = nextBackoff(backoff, r.cfg.MinBackoff, r.cfg.MaxBackoff)
			if errors.Is(err, ErrBrokerUnavailable) {
				r.log.Warn("outbox publish deferred: broker unavailable", "error", err, "retry_in", backoff)
			} else {
				r.log.Error("outbox publish failed", "error", err, "retry_in", backoff)
			}
			r.waitFor(ctx, backoff, r.connected())
		}
	}
}

// waitFor sleeps d, but returns early the moment a broker that was down comes
// back. Without that, the worst-case delay before a backlog starts draining after
// an outage would be a full MaxBackoff on top of NATS's own reconnect time.
func (r *Relay) waitFor(ctx context.Context, d time.Duration, wasConnected bool) {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-tick.C:
			if !wasConnected && r.connected() {
				return
			}
		}
	}
}

// nextBackoff doubles with full jitter, so N relay replicas recovering from the
// same outage do not retry in lockstep.
func nextBackoff(prev, min, max time.Duration) time.Duration {
	if min <= 0 {
		min = 100 * time.Millisecond
	}
	if max < min {
		max = min
	}
	next := prev * 2
	if next < min {
		next = min
	}
	if next > max {
		next = max
	}
	// Jitter within [next/2, next]: never zero, so an outage cannot hot-loop.
	half := next / 2
	return half + time.Duration(rand.Int64N(int64(next-half)+1))
}

// BatchResult reports what one PublishBatch did.
type BatchResult struct {
	Claimed    int // rows locked by this batch
	Published  int // acknowledged and marked published
	Duplicates int // acknowledged as duplicates (already in the stream)
	Failed     int // publish attempted and failed
	Gone       int // referenced run event no longer exists; marked done, nothing sent
	Deferred   int // topic this relay version does not know; left for a newer replica
}

type outboxRow struct {
	id         int64
	topic      string
	namespace  *string
	routingKey *string
	payload    []byte
	createdAt  time.Time
	attempts   int32

	// Joined from run_events / workflow_runs for topic = run_event.
	evID        *int64
	evRunID     *string
	evSeq       *int32
	evNodeID    *string
	evAttemptID *string
	evKind      *string
	evPayload   []byte
	evActor     *string
	evOccurred  *time.Time
	evNamespace *string
}

// PublishBatch claims up to BatchSize available rows, publishes them, and marks
// the acknowledged ones, all inside one transaction.
//
// The transaction deliberately spans the publish. That is what makes the claim
// exclusive (the row locks are what SKIP LOCKED skips) and what makes a crash
// safe (no COMMIT, no mark). It is bounded by PublishTimeout, and it holds locks
// only on outbox rows, which no writer ever updates -- writers only INSERT new
// rows -- so it cannot delay the facts being announced.
func (r *Relay) PublishBatch(ctx context.Context) (BatchResult, error) {
	var res BatchResult
	if r.nc == nil {
		return res, errors.New("relay: publishing is disabled (no NATS connection)")
	}
	if !r.connected() {
		// Do not even open a transaction: during an outage every batch would
		// lock rows only to fail them, which is churn with no chance of progress.
		return res, fmt.Errorf("%w: connection status %v", ErrBrokerUnavailable, r.nc.Status())
	}
	if err := r.ensureStreams(ctx); err != nil {
		return res, fmt.Errorf("%w: %v", ErrBrokerUnavailable, err)
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return res, fmt.Errorf("relay: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := r.claim(ctx, tx)
	if err != nil {
		return res, err
	}
	res.Claimed = len(rows)
	if len(rows) == 0 {
		return res, nil
	}

	type pending struct {
		row outboxRow
		fut jetstream.PubAckFuture
	}
	var (
		inflight []pending
		acked    []outboxRow
		gone     []int64
		failed   = map[int64]string{}
		// immediate counts publishes the client refused before they reached the
		// wire (not connected, too many pending). They are real attempts, and
		// they say as much about the broker as a failed ack does.
		immediate int
	)
	for _, row := range rows {
		msg, known, isGone := r.buildMessage(row)
		switch {
		case isGone:
			gone = append(gone, row.id)
			continue
		case !known:
			res.Deferred++
			failed[row.id] = "unknown topic " + strconv.Quote(row.topic) + " for this relay version"
			continue
		}
		fut, err := r.js.PublishMsgAsync(msg, jetstream.WithMsgID(bus.MsgID(row.id, row.createdAt)))
		if err != nil {
			failed[row.id] = err.Error()
			res.Failed++
			immediate++
			continue
		}
		inflight = append(inflight, pending{row: row, fut: fut})
	}

	waitCtx, cancel := context.WithTimeout(ctx, r.cfg.PublishTimeout)
	defer cancel()
	for _, p := range inflight {
		select {
		case ack := <-p.fut.Ok():
			if ack != nil && ack.Duplicate {
				res.Duplicates++
				r.m.Duplicates.Inc()
			}
			acked = append(acked, p.row)
		case perr := <-p.fut.Err():
			failed[p.row.id] = perr.Error()
			res.Failed++
			if errors.Is(perr, jetstream.ErrNoStreamResponse) || errors.Is(perr, nats.ErrNoResponders) {
				// The stream vanished (a broker that lost its storage). Recreate
				// it on the next batch rather than failing forever.
				r.streamsReady.Store(false)
			}
		case <-waitCtx.Done():
			failed[p.row.id] = "timed out waiting for publish ack"
			res.Failed++
		}
	}

	attempted := len(inflight) + immediate
	if len(acked) == 0 && attempted > 0 && res.Failed == attempted {
		// Everything we tried failed: the broker is the problem, not the rows.
		// Record the error but do NOT push the rows' available_at out, or a long
		// outage would leave the backlog scheduled minutes into the future after
		// the broker returned.
		r.m.Failures.With(metrics.Labels{"kind": "broker"}).Inc()
		if err := r.recordFailures(ctx, tx, failed, false); err != nil {
			return res, err
		}
		if err := tx.Commit(ctx); err != nil {
			return res, fmt.Errorf("relay: commit failure record: %w", err)
		}
		return res, fmt.Errorf("%w: all %d publishes in the batch failed (first: %s)",
			ErrBrokerUnavailable, attempted, firstErr(failed))
	}

	ackedIDs := make([]int64, 0, len(acked))
	for _, a := range acked {
		ackedIDs = append(ackedIDs, a.id)
	}
	if r.beforeMark != nil {
		if err := r.beforeMark(ackedIDs); err != nil {
			return res, fmt.Errorf("relay: aborted before marking %d published rows: %w", len(ackedIDs), err)
		}
	}

	lags, err := r.markPublished(ctx, tx, ackedIDs, gone)
	if err != nil {
		return res, err
	}
	if len(failed) > 0 {
		// A partial failure: the broker is up and these rows specifically did not
		// go. Back them off individually so one poison row cannot monopolise the
		// head of every batch.
		r.m.Failures.With(metrics.Labels{"kind": "row"}).Add(float64(res.Failed))
		if err := r.recordFailures(ctx, tx, failed, true); err != nil {
			return res, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("relay: commit published marks: %w", err)
	}

	res.Published = len(acked)
	res.Gone = len(gone)
	for _, a := range acked {
		labels := metrics.Labels{"topic": a.topic}
		r.m.Published.With(labels).Inc()
		if lag, ok := lags[a.id]; ok {
			r.m.Lag.With(labels).Observe(lag)
		}
	}
	return res, nil
}

func firstErr(failed map[int64]string) string {
	for _, e := range failed {
		return e
	}
	return ""
}

func (r *Relay) ensureStreams(ctx context.Context) error {
	// A reconnect may mean a different (or wiped) server, so the streams are
	// re-verified after every reconnect, not only once per process.
	if rc := r.nc.Stats().Reconnects; rc != r.seenReconnects.Load() {
		r.seenReconnects.Store(rc)
		r.streamsReady.Store(false)
	}
	if r.streamsReady.Load() {
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, r.cfg.PublishTimeout)
	defer cancel()
	if err := bus.EnsureStreams(sctx, r.js); err != nil {
		return err
	}
	r.streamsReady.Store(true)
	return nil
}

// claim locks the next batch of available rows, skipping rows another relay
// replica holds.
func (r *Relay) claim(ctx context.Context, tx pgx.Tx) ([]outboxRow, error) {
	// FOR UPDATE OF o: only the outbox row is locked. run_events and
	// workflow_runs are read for the message body and must NOT be locked -- the
	// run row is the event_seq allocator, and locking it here would serialise the
	// relay against every event append on that run.
	const q = `
SELECT o.id, o.topic, o.namespace, o.routing_key, o.payload, o.created_at, o.attempts,
       e.id, e.run_id::text, e.seq, e.node_id, e.attempt_id::text, e.kind, e.payload,
       e.actor, e.occurred_at, wr.namespace
  FROM outbox o
  LEFT JOIN run_events e     ON o.topic = 'run_event' AND e.id = o.ref_id
  LEFT JOIN workflow_runs wr ON wr.id = e.run_id
 WHERE o.published_at IS NULL
   AND o.available_at <= now()
 ORDER BY o.id
 LIMIT $1
 FOR UPDATE OF o SKIP LOCKED`
	rows, err := tx.Query(ctx, q, r.cfg.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("relay: claim outbox batch: %w", err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var o outboxRow
		if err := rows.Scan(&o.id, &o.topic, &o.namespace, &o.routingKey, &o.payload, &o.createdAt,
			&o.attempts, &o.evID, &o.evRunID, &o.evSeq, &o.evNodeID, &o.evAttemptID, &o.evKind,
			&o.evPayload, &o.evActor, &o.evOccurred, &o.evNamespace); err != nil {
			return nil, fmt.Errorf("relay: scan outbox row: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("relay: iterate outbox batch: %w", err)
	}
	return out, nil
}

// buildMessage renders one row. known=false means this relay version does not
// understand the topic; gone=true means there is nothing left to announce.
func (r *Relay) buildMessage(o outboxRow) (msg *nats.Msg, known, gone bool) {
	var (
		subject string
		body    map[string]any
	)
	switch o.topic {
	case store.OutboxTopicRunEvent:
		if o.evID == nil || o.evRunID == nil || o.evNamespace == nil {
			// The run (and with it, by cascade, the event) was deleted before we
			// got to it. The fact no longer exists, so there is nothing to project.
			return nil, true, true
		}
		subject = bus.RunEventSubject(*o.evNamespace, *o.evRunID)
		body = map[string]any{
			"type": "run.event", "id": *o.evID, "run_id": *o.evRunID,
			"namespace": *o.evNamespace, "seq": deref(o.evSeq), "kind": deref(o.evKind),
			"actor": deref(o.evActor),
		}
		if o.evNodeID != nil {
			body["node_id"] = *o.evNodeID
		}
		if o.evAttemptID != nil {
			body["attempt_id"] = *o.evAttemptID
		}
		if o.evOccurred != nil {
			body["occurred_at"] = o.evOccurred.UTC().Format(time.RFC3339Nano)
		}
		if len(o.evPayload) > r.cfg.MaxEventPayloadBytes {
			body["payload_truncated"] = true
		} else if len(o.evPayload) > 0 {
			body["payload"] = json.RawMessage(o.evPayload)
		}
	case store.OutboxTopicRunState:
		subject = bus.RunStateSubject(deref(o.namespace), deref(o.routingKey))
		body = decodeObject(o.payload)
		body["type"] = "run.state"
	case store.OutboxTopicTaskReady:
		subject = bus.DispatchSubject(deref(o.namespace), deref(o.routingKey))
		body = decodeObject(o.payload)
		body["type"] = "task.ready"
		body["namespace"] = deref(o.namespace)
	case store.OutboxTopicUsageRecorded:
		// The metering export hook (ADR-0021). The payload is the ledger row's
		// identity and quantities, written by the same statement as the row.
		subject = bus.UsageSubject(deref(o.namespace))
		body = decodeObject(o.payload)
		body["type"] = "nexus.usage.recorded"
	default:
		return nil, false, false
	}
	body["outbox_id"] = o.id
	data, err := json.Marshal(body)
	if err != nil {
		// Every value above is JSON-derived; this cannot fail short of a bug.
		// Treat as an unknown shape so the row is retried by a fixed version.
		r.log.Error("could not encode outbox message", "outbox_id", o.id, "topic", o.topic, "error", err)
		return nil, false, false
	}
	msg = nats.NewMsg(subject)
	msg.Data = data
	msg.Header.Set("Nexus-Outbox-Topic", o.topic)
	return msg, true, false
}

func decodeObject(b []byte) map[string]any {
	m := map[string]any{}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &m)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// markPublished marks acknowledged rows and closes out rows whose fact is gone.
// It returns each acked row's publish lag, computed from the DATABASE clock on
// both ends so relay-host clock skew cannot distort the metric.
func (r *Relay) markPublished(ctx context.Context, tx pgx.Tx, acked, gone []int64) (map[int64]float64, error) {
	lags := make(map[int64]float64, len(acked))
	if len(acked) > 0 {
		rows, err := tx.Query(ctx, `
UPDATE outbox
   SET published_at = clock_timestamp(), attempts = attempts + 1, last_error = NULL
 WHERE id = ANY($1)
RETURNING id, EXTRACT(EPOCH FROM clock_timestamp() - created_at)::float8`, acked)
		if err != nil {
			return nil, fmt.Errorf("relay: mark published: %w", err)
		}
		for rows.Next() {
			var id int64
			var lag float64
			if err := rows.Scan(&id, &lag); err != nil {
				rows.Close()
				return nil, fmt.Errorf("relay: scan publish lag: %w", err)
			}
			lags[id] = lag
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("relay: mark published: %w", err)
		}
	}
	if len(gone) > 0 {
		if _, err := tx.Exec(ctx, `
UPDATE outbox
   SET published_at = clock_timestamp(), last_error = 'announced run event no longer exists; nothing published'
 WHERE id = ANY($1)`, gone); err != nil {
			return nil, fmt.Errorf("relay: close out rows with no fact: %w", err)
		}
	}
	return lags, nil
}

// recordFailures notes the error on each failed row. perRow pushes the row's
// available_at out exponentially in its own attempt count (capped at 5 minutes);
// broker-wide failures do not, because the rows are not at fault.
func (r *Relay) recordFailures(ctx context.Context, tx pgx.Tx, failed map[int64]string, perRow bool) error {
	for id, msg := range failed {
		if len(msg) > 500 {
			msg = msg[:500]
		}
		var err error
		if perRow {
			_, err = tx.Exec(ctx, `
UPDATE outbox
   SET attempts = attempts + 1, last_error = $2,
       available_at = clock_timestamp() + make_interval(secs => LEAST(300, power(2, LEAST(attempts, 9))))
 WHERE id = $1`, id, msg)
		} else {
			_, err = tx.Exec(ctx,
				`UPDATE outbox SET attempts = attempts + 1, last_error = $2 WHERE id = $1`, id, msg)
		}
		if err != nil {
			return fmt.Errorf("relay: record failure on outbox row %d: %w", id, err)
		}
	}
	return nil
}

// ── janitor ────────────────────────────────────────────────────────────────

func (r *Relay) pruneLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.PruneInterval)
	defer t.Stop()
	for {
		if _, _, err := r.Prune(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("outbox prune failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Prune deletes published rows past Retention and unpublished rows past MaxAge,
// in bounded chunks so one janitor pass never holds a long transaction or takes
// a burst of row locks across the table.
func (r *Relay) Prune(ctx context.Context) (published, expired int64, err error) {
	const chunk = 5000
	const maxChunks = 20
	for i := 0; i < maxChunks; i++ {
		tag, err := r.pool.Exec(ctx, `
DELETE FROM outbox o USING (
  SELECT id FROM outbox
   WHERE published_at IS NOT NULL AND published_at < now() - make_interval(secs => $1)
     -- ADR-0028: a row the notifier has not evaluated yet is kept, up to
     -- MaxAge, so a notifier that is behind never loses a fact to retention.
     AND (notified_at IS NOT NULL OR topic NOT IN ('run_state', 'run_event')
          OR created_at < now() - make_interval(secs => $3))
   ORDER BY published_at LIMIT $2
) d WHERE o.id = d.id`, r.cfg.Retention.Seconds(), chunk, r.cfg.MaxAge.Seconds())
		if err != nil {
			return published, expired, fmt.Errorf("relay: prune published rows: %w", err)
		}
		published += tag.RowsAffected()
		if tag.RowsAffected() < chunk {
			break
		}
	}
	for i := 0; i < maxChunks; i++ {
		// SKIP LOCKED: a row a relay is publishing right now is not expired out
		// from under it.
		tag, err := r.pool.Exec(ctx, `
DELETE FROM outbox o USING (
  SELECT id FROM outbox
   WHERE published_at IS NULL AND created_at < now() - make_interval(secs => $1)
   ORDER BY id LIMIT $2
   FOR UPDATE SKIP LOCKED
) d WHERE o.id = d.id`, r.cfg.MaxAge.Seconds(), chunk)
		if err != nil {
			return published, expired, fmt.Errorf("relay: expire unpublished rows: %w", err)
		}
		expired += tag.RowsAffected()
		if tag.RowsAffected() < chunk {
			break
		}
	}
	if published > 0 {
		r.m.Pruned.With(metrics.Labels{"reason": "published"}).Add(float64(published))
	}
	if expired > 0 {
		r.m.Pruned.With(metrics.Labels{"reason": "expired"}).Add(float64(expired))
		if r.nc != nil {
			// With the bus enabled, expiry means rows sat unpublishable for the
			// whole MaxAge: a real outage or a poison row, worth a warning. With
			// the bus disabled it is the normal way the table stays bounded.
			r.log.Warn("dropped outbox rows that were never published within max age",
				"rows", expired, "max_age", r.cfg.MaxAge)
		}
	}
	return published, expired, nil
}

// ── backlog gauges ─────────────────────────────────────────────────────────

func (r *Relay) backlogLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.BacklogInterval)
	defer t.Stop()
	for {
		if err := r.SampleBacklog(ctx); err != nil && ctx.Err() == nil {
			r.log.Warn("outbox backlog sample failed", "error", err)
		}
		r.m.Connected.Set(boolGauge(r.connected()))
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SampleBacklog refreshes the backlog gauges. The count is capped so the sample
// itself stays cheap during exactly the outage it is meant to show.
func (r *Relay) SampleBacklog(ctx context.Context) error {
	var n int64
	var oldest float64
	if err := r.pool.QueryRow(ctx, `
SELECT count(*), COALESCE(EXTRACT(EPOCH FROM clock_timestamp() - min(created_at)), 0)::float8
  FROM (SELECT created_at FROM outbox
         WHERE published_at IS NULL AND available_at <= now()
         LIMIT 1000000) b`).Scan(&n, &oldest); err != nil {
		return fmt.Errorf("relay: sample backlog: %w", err)
	}
	r.m.Backlog.Set(float64(n))
	r.m.OldestAge.Set(oldest)
	return nil
}

// ── reconciliation hints ───────────────────────────────────────────────────

func (r *Relay) hintLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.HintInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !r.connected() {
			continue
		}
		if _, err := r.PublishReadyHints(ctx); err != nil && ctx.Err() == nil {
			r.log.Warn("reconcile hints failed", "error", err)
		}
	}
}

// PublishReadyHints announces every (namespace, queue) that currently holds
// dispatchable PENDING work.
//
// This is the periodic reconciliation that makes the hint stream self-healing.
// Outbox rows cover the transitions that CREATE pending work, but not every
// transition that makes existing work claimable again: a paused run resuming, a
// worker freeing capacity, a hint lost while an agent was disconnected or the
// broker was down. Rather than enumerate those, this re-derives the answer from
// Postgres every HintInterval, so after any outage or missed message the fleet is
// re-woken within one interval of the broker being back. Agents also poll on
// their own regardless, so this bounds hint staleness, not correctness.
//
// Published as core NATS (no ack, no Msg-Id): a periodic, idempotent summary has
// nothing to de-duplicate and nothing worth retrying.
func (r *Relay) PublishReadyHints(ctx context.Context) (int, error) {
	rows, err := r.pool.Query(ctx, `
SELECT wr.namespace, a.queue, count(*)::int
  FROM task_attempts a
  JOIN workflow_runs wr ON wr.id = a.run_id
 WHERE a.state = 'PENDING'
   AND a.scheduled_at <= now()
   AND wr.state IN ('PENDING','RUNNING')
 GROUP BY wr.namespace, a.queue
 ORDER BY wr.namespace, a.queue
 LIMIT 1000`)
	if err != nil {
		return 0, fmt.Errorf("relay: find queues with pending work: %w", err)
	}
	type nq struct {
		ns, queue string
		pending   int
	}
	var list []nq
	for rows.Next() {
		var x nq
		if err := rows.Scan(&x.ns, &x.queue, &x.pending); err != nil {
			rows.Close()
			return 0, fmt.Errorf("relay: scan pending queue: %w", err)
		}
		list = append(list, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("relay: iterate pending queues: %w", err)
	}
	sent := 0
	for _, x := range list {
		data, _ := json.Marshal(map[string]any{
			"type": "task.ready", "reason": "reconcile",
			"namespace": x.ns, "queue": x.queue, "pending": x.pending,
		})
		if err := r.nc.Publish(bus.DispatchSubject(x.ns, x.queue), data); err != nil {
			return sent, fmt.Errorf("relay: publish reconcile hint for %s/%s: %w", x.ns, x.queue, err)
		}
		sent++
	}
	r.m.HintsPublished.Add(float64(sent))
	return sent, nil
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
