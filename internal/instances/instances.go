// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package instances is the control-plane side of the service-instance registry
// (migration 0041, ADR-0037): every api-gateway, workflow-manager, dag-engine,
// scheduler, resource-manager and event-service process runs one Heartbeat,
// which upserts the process's own row every 10 seconds and marks it not ready
// when the process shuts down gracefully.
//
// The heartbeat is OFF the hot path by construction. It runs on its own
// goroutine, makes one short statement per beat on the service's existing
// pool under a 2-second deadline, and never returns an error to anyone: a
// failed beat is logged (once per outage, not once per beat) and counted in
// nexus_instance_heartbeat_failures_total. A process whose beats keep failing
// shows as stale after 30 seconds, which is exactly when an operator should
// look at it -- its pool is saturated or its database unreachable.
package instances

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/khuriwalgroup/kgf-nexus/internal/instances/identity"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// The control-plane components, by the names the registry and the API use.
const (
	APIGateway      = "api-gateway"
	WorkflowManager = "workflow-manager"
	DAGEngine       = "dag-engine"
	Scheduler       = "scheduler"
	ResourceManager = "resource-manager"
	EventService    = "event-service"
)

// Components lists the components in the order the API reports them
// (contract C3): request path first, then run lifecycle, then the ledger and
// the history.
func Components() []string {
	return []string{APIGateway, WorkflowManager, DAGEngine, Scheduler, ResourceManager, EventService}
}

// Registry timing (ADR-0037 §1).
const (
	// DefaultInterval is the beat period.
	DefaultInterval = 10 * time.Second
	// StaleAfter is how long a row may go unwritten before the API reports
	// the instance stale: three missed beats, so one slow write or one
	// dropped connection never flaps a healthy replica.
	StaleAfter = 30 * time.Second
	// Retention is how long an unwritten row is kept before it is pruned: a
	// day of deploy history, then gone.
	Retention = 24 * time.Hour
	// DefaultWriteTimeout bounds one beat's statement, connection acquisition
	// included. Short on purpose: a beat that waits longer than this for a
	// saturated pool would compete with the work the pool exists for.
	DefaultWriteTimeout = 2 * time.Second
	// DefaultProbeTimeout bounds the readiness evaluation that precedes a
	// beat. The services' own /readyz checks are bounded at 2-3 seconds.
	DefaultProbeTimeout = 3 * time.Second
	// DefaultPruneInterval is how often one process prunes. Every process
	// prunes, so the fleet prunes far more often than this; the statement is
	// a scan of a few pages.
	DefaultPruneInterval = 10 * time.Minute
)

// Detail keys every component writes (ADR-0037 §3). Component-specific keys
// are documented where each service builds them.
const (
	// DetailReadiness is "ok" when ready, else the reason the process's own
	// readiness check gave ("draining", "degraded: ...").
	DetailReadiness = "readiness"
	// DetailShuttingDown is true on the final write of a graceful shutdown.
	DetailShuttingDown = "shutting_down"
	// DetailPID is the process id, for matching a row to a log stream.
	DetailPID = "pid"
	// DetailIntervalSeconds is the beat period, so a reader can judge
	// staleness against the writer's own cadence.
	DetailIntervalSeconds = "heartbeat_interval_seconds"
	// DetailDBPool is the process's Postgres pool: max, total, acquired and
	// idle connections, and empty_acquires_total (the exhaustion signal).
	DetailDBPool = "db_pool"
)

// ReadyFunc reports whether the process is ready, with a short reason when it
// is not. It must answer the same question the process's /readyz answers;
// ProbeHandler adapts the /readyz handler itself.
type ReadyFunc func(ctx context.Context) (ready bool, reason string)

// DetailFunc returns the component-specific detail for the next beat. It is
// called on the heartbeat goroutine and must be cheap: read counters the
// process already keeps, never query anything.
type DetailFunc func() map[string]any

// Config describes the process.
type Config struct {
	// Component is one of Components().
	Component string
	// Version is the build's stamped version.
	Version string
	// Runtime is where the process runs (identity.FromEnv). An empty Pod
	// falls back to the host name.
	Runtime identity.Runtime
	// StartedAt is the process start; zero means the time New is called.
	StartedAt time.Time

	// Ready reports readiness. Nil means "ready while the process runs".
	Ready ReadyFunc
	// Detail adds component-specific keys. Optional.
	Detail DetailFunc
	// Pool, when set, is reported under DetailDBPool.
	Pool *pgxpool.Pool

	// Zero values take the defaults above.
	Interval      time.Duration
	WriteTimeout  time.Duration
	ProbeTimeout  time.Duration
	PruneInterval time.Duration
	Retention     time.Duration
}

// Heartbeat writes one process's registry row.
type Heartbeat struct {
	cfg     Config
	db      store.InstanceExecer
	log     *slog.Logger
	metrics *Metrics
	row     store.ServiceInstance

	mu        sync.Mutex
	failing   bool
	lastPrune time.Time
	now       func() time.Time
}

// New builds a heartbeat for the process described by cfg, writing through db
// (the service's pool). reg may be nil (no metrics).
func New(db store.InstanceExecer, cfg Config, reg *metrics.Registry, log *slog.Logger) (*Heartbeat, error) {
	if db == nil {
		return nil, errors.New("instances: a database is required")
	}
	known := false
	for _, c := range Components() {
		known = known || c == cfg.Component
	}
	if !known {
		return nil, fmt.Errorf("instances: unknown component %q", cfg.Component)
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = DefaultProbeTimeout
	}
	if cfg.PruneInterval <= 0 {
		cfg.PruneInterval = DefaultPruneInterval
	}
	if cfg.Retention <= 0 {
		cfg.Retention = Retention
	}
	if cfg.StartedAt.IsZero() {
		cfg.StartedAt = time.Now()
	}
	instance := cfg.Runtime.Pod
	if instance == "" {
		instance = hostname()
	}
	h := &Heartbeat{
		cfg: cfg, db: db, metrics: NewMetrics(reg), now: time.Now,
		log: log.With("component", cfg.Component, "instance", instance),
		row: store.ServiceInstance{
			Component:    cfg.Component,
			Instance:     instance,
			PodNamespace: cfg.Runtime.PodNamespace,
			Node:         cfg.Runtime.Node,
			Cluster:      cfg.Runtime.Cluster,
			Version:      truncate(cfg.Version, 128),
			StartedAt:    cfg.StartedAt,
		},
	}
	return h, nil
}

// Instance is the identity the row is keyed by.
func (h *Heartbeat) Instance() string { return h.row.Instance }

// Start runs the heartbeat on its own goroutine until ctx ends or stop is
// called, whichever is first, and then writes the not-ready row. stop returns
// once that final write is done (it is bounded by the write timeout), so a
// service defers it AFTER deferring its pool's Close: deferred calls run in
// reverse, and the last write must not find the pool closed. Calling stop
// more than once is safe.
func Start(ctx context.Context, h *Heartbeat) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// Run beats immediately, then every Interval, until ctx ends; then it writes
// one last row with ready = false. It never returns early on a failure.
func (h *Heartbeat) Run(ctx context.Context) {
	h.log.Info("service instance heartbeat started", "interval", h.cfg.Interval,
		"pod_namespace", h.row.PodNamespace, "node", h.row.Node, "cluster", h.row.Cluster)
	t := time.NewTicker(h.cfg.Interval)
	defer t.Stop()
	for {
		h.Beat(ctx)
		select {
		case <-ctx.Done():
			h.Shutdown(context.WithoutCancel(ctx))
			return
		case <-t.C:
		}
	}
}

// Beat evaluates readiness and writes the row once, then prunes when a prune
// is due. Exported for tests and for callers that drive their own loop.
func (h *Heartbeat) Beat(ctx context.Context) {
	ready, reason := h.readiness(ctx)
	if ctx.Err() != nil {
		// Shutting down mid-beat: the final write follows.
		return
	}
	h.write(ctx, "beat", ready, reason, false)
	h.maybePrune(ctx)
}

// Shutdown writes the final row: not ready, shutting_down. ctx should not be
// the (already cancelled) run context; the write is bounded by WriteTimeout.
func (h *Heartbeat) Shutdown(ctx context.Context) {
	h.write(ctx, "shutdown", false, "shutting down", true)
	h.log.Info("service instance marked not ready for shutdown")
}

func (h *Heartbeat) readiness(ctx context.Context) (bool, string) {
	if h.cfg.Ready == nil {
		return true, "ok"
	}
	pctx, cancel := context.WithTimeout(ctx, h.cfg.ProbeTimeout)
	defer cancel()
	ready, reason := h.cfg.Ready(pctx)
	if ready {
		return true, "ok"
	}
	if reason == "" {
		reason = "not ready"
	}
	return false, truncate(reason, 200)
}

func (h *Heartbeat) write(ctx context.Context, op string, ready bool, reason string, shuttingDown bool) {
	row := h.row
	row.Ready = ready
	row.Detail = h.detail(reason, shuttingDown)

	wctx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
	defer cancel()
	err := store.UpsertServiceInstance(wctx, h.db, row)
	if err != nil {
		h.metrics.failed(op)
		h.mu.Lock()
		first := !h.failing
		h.failing = true
		h.mu.Unlock()
		// Once per outage at WARN: a beat every 10 s against a database that
		// is down would otherwise be the loudest line in the log.
		if first || op == "shutdown" {
			h.log.Warn("service instance heartbeat failed; the instance will show as stale until a beat succeeds",
				"op", op, "error", err)
		} else {
			h.log.Debug("service instance heartbeat failed again", "op", op, "error", err)
		}
		return
	}
	h.metrics.succeeded(h.now())
	h.mu.Lock()
	recovered := h.failing
	h.failing = false
	h.mu.Unlock()
	if recovered {
		h.log.Info("service instance heartbeat recovered")
	}
}

func (h *Heartbeat) detail(reason string, shuttingDown bool) map[string]any {
	d := map[string]any{}
	if h.cfg.Detail != nil {
		for k, v := range h.cfg.Detail() {
			d[k] = v
		}
	}
	// The common keys win over a component's, so their meaning is the same
	// on every row.
	d[DetailReadiness] = reason
	d[DetailPID] = os.Getpid()
	d[DetailIntervalSeconds] = h.cfg.Interval.Seconds()
	if shuttingDown {
		d[DetailShuttingDown] = true
	}
	if p := h.cfg.Pool; p != nil {
		s := p.Stat()
		d[DetailDBPool] = map[string]any{
			"max": s.MaxConns(), "total": s.TotalConns(), "acquired": s.AcquiredConns(),
			"idle": s.IdleConns(), "empty_acquires_total": s.EmptyAcquireCount(),
		}
	}
	return d
}

func (h *Heartbeat) maybePrune(ctx context.Context) {
	now := h.now()
	h.mu.Lock()
	due := h.lastPrune.IsZero() || now.Sub(h.lastPrune) >= h.cfg.PruneInterval
	if due {
		h.lastPrune = now
	}
	h.mu.Unlock()
	if !due {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
	defer cancel()
	n, err := store.PruneServiceInstances(pctx, h.db, h.cfg.Retention)
	if err != nil {
		h.metrics.failed("prune")
		h.log.Debug("service instance prune failed; the next one retries", "error", err)
		return
	}
	if n > 0 {
		h.log.Info("pruned service instances not seen within the retention", "rows", n, "retention", h.cfg.Retention)
	}
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return fmt.Sprintf("unknown-host-%d", os.Getpid())
	}
	return identity.Clean(h)
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
