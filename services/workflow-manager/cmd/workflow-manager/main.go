// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Command workflow-manager owns the WorkflowRun lifecycle, the workflow_specs and
// workflow_runs tables, and the durable timer table (ADR-0011).
//
// Its most operationally important component is the timer reconciler: EVERY
// deadline in the system is a row in `timers`, and this process is what makes those
// rows fire. Using the database rather than in-memory timers means a restart of this
// service loses nothing — the failure mode of in-memory timer wheels, where a
// rolling deploy silently drops every pending deadline (§3.2).
package main

import (
	"github.com/khuriwalgroup/kgf-nexus/internal/tracing"

	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"

	nexuscfg "github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/instances"
	"github.com/khuriwalgroup/kgf-nexus/internal/instances/identity"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/transport"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/health"
	"github.com/khuriwalgroup/kgf-nexus/services/workflow-manager/grpcapi"
	"github.com/khuriwalgroup/kgf-nexus/services/workflow-manager/runs"
	"github.com/khuriwalgroup/kgf-nexus/services/workflow-manager/schedules"
	"github.com/khuriwalgroup/kgf-nexus/services/workflow-manager/sensors"
	"github.com/khuriwalgroup/kgf-nexus/services/workflow-manager/timers"
)

func main() {
	if err := run(); err != nil {
		slog.Error("workflow-manager exited with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	log := newLogger()
	slog.SetDefault(log)

	cfg := loadConfig()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Tracing (ADR-0026): off unless OTEL_* configures an exporter; flushed last.
	defer tracing.SetupOrExit(ctx, "workflow-manager", version, log)()

	dbCtx, dbCancel := context.WithTimeout(ctx, 15*time.Second)
	defer dbCancel()
	pool, err := store.GWOpen(dbCtx, cfg.DatabaseURL, cfg.MaxConns)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()
	db := store.GWDB(pool)

	// Peer clients. The workflow-manager owns runs but not nodes or attempts, so
	// materializing a DAG and cancelling attempts are calls to their owners.
	dagClient := grpcapi.NewDAGEngineClient(cfg.DAGEngineAddr, log)
	schedClient := grpcapi.NewSchedulerClient(cfg.SchedulerAddr, log)
	eventClient := grpcapi.NewEventClient(cfg.EventServiceAddr, log)

	runSvc := runs.New(db, eventClient, dagClient, schedClient, log)

	// ── timer reconciler ──────────────────────────────────────────────────
	// The claimant identifies THIS replica in timers.claimed_by. Using the
	// hostname plus the pid means a restarted pod on the same host does not look
	// like its predecessor, so the stale-claim sweeper correctly reclaims what the
	// dead process was holding.
	claimant := fmt.Sprintf("%s/%d", hostname(), os.Getpid())
	// One registry per process, served on the health listener below. The
	// reconciler's instruments are what REPLACED the timer_backlog readiness
	// check: see the health block for why that check was an availability hazard.
	reg := metrics.NewServiceRegistry("workflow-manager", version)
	store.RegisterPoolMetrics(reg, pool)
	grpcMetrics := metrics.NewGRPCServerMetrics(reg)
	// Certificate expiry and reload outcomes (docs/TLS.md §5.4). Safe to call
	// before the credentials below are resolved: values are replayed into the
	// registry whenever they appear.
	transport.RegisterMetrics(reg)

	rec := timers.New(db, timers.Config{
		Interval:        cfg.TimerInterval,
		BatchSize:       cfg.TimerBatchSize,
		ClaimTimeout:    cfg.TimerClaimTimeout,
		Claimant:        claimant,
		BacklogInterval: cfg.TimerBacklogInterval,
		BacklogAlert:    int64(cfg.TimerBacklogAlert),
		// The stalled-run backstop. See Reconciler.sweepStalledRuns: it should
		// never find anything, and its counter is an alarm rather than a metric.
		StalledRunAge:      cfg.StalledRunAge,
		StalledRunInterval: cfg.StalledRunInterval,
		// Re-issues a CANCELLING run's attempt cancellation (F109).
		CancelSweepInterval: cfg.CancelSweepInterval,
		Metrics:             timers.NewMetrics(reg),
	}, log)

	registerTimerHandlers(rec, runSvc, schedClient, log)

	// The stalled-run sweep's recovery action. Wired here rather than inside the
	// timers package because only this file knows how to reach the run service,
	// which is the same reason the timer handlers are injected.
	rec.OnStalledRun(runSvc.ReconcileStalled)
	rec.OnUnmaterializedRun(runSvc.EnsureMaterialized)
	rec.OnCancellingRun(runSvc.ReconcileCancelling)

	// ── gRPC API ──────────────────────────────────────────────────────────
	// The JSON codec must be registered before the server starts: the spec half of
	// this service carries hand-written types rather than protobufs, for the
	// contract-gap reason documented on SpecServiceDesc.
	encoding.RegisterCodec(grpcapi.JSONCodec{})

	// PeerServerOptions bounds how long one connection lives. Without it a client
	// that connected before an address swap keeps using a healthy connection to
	// whatever now answers at the old address, and nothing re-resolves it.
	sopts, terr := transport.ServerOptions()
	if terr != nil {
		// Refusing to start is the point: a deployment that configured TLS
		// incompletely must not quietly serve plaintext instead.
		return fmt.Errorf("resolve TLS server options: %w", terr)
	}
	grpcOpts := append(grpcMetrics.ServerOptions(), nexuscfg.PeerServerOptions()...)
	grpcSrv := grpc.NewServer(append(grpcOpts, sopts...)...)
	grpcapi.Register(grpcSrv, runSvc, log)
	// Built before RegisterTyped so the backfill RPCs have a service to call. Its
	// loop is started further down, after the listeners.
	schedSvc := schedules.New(db, schedules.NewRunsAdapter(runSvc), schedules.Config{
		Interval:             cfg.ScheduleInterval,
		BatchSize:            cfg.ScheduleBatchSize,
		StalledSweepInterval: cfg.ScheduleSweepInterval,
		MaxCatchupPerPass:    cfg.ScheduleMaxCatchup,
		BackfillLease:        cfg.BackfillLease,
	}, log)

	grpcapi.RegisterTyped(grpcSrv, runSvc, db, log, schedules.NewBackfillAdapter(schedSvc))

	grpcLis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.GRPCAddr, err)
	}

	// ── health ────────────────────────────────────────────────────────────
	// Readiness checks ONLY what this replica needs to serve. The timer backlog is
	// deliberately not here, though it used to be.
	//
	// The backlog is a property of the SHARED timers table, so every replica
	// computes the same number and they would all fail readiness together --
	// emptying the whole tier out of the load balancer precisely when deadlines
	// were already firing late, and removing the very replicas whose job is to
	// drain the backlog. A global condition evaluated per-replica cannot shed load
	// the way the old comment claimed; it can only cause an outage.
	//
	// The signal is not lost: the reconciler samples it into nexus_timer_backlog
	// and logs above NEXUS_TIMER_BACKLOG_ALERT. That threshold is now an alerting
	// threshold rather than a traffic gate, which is what it should always have
	// been -- an operator who set it low was previously configuring an outage.
	hc := health.New(map[string]health.Check{
		"postgres": func(ctx context.Context) error { return pool.Ping(ctx) },
	})
	mux := http.NewServeMux()
	hc.Routes(mux)
	// Scraped on the health address; no extra port and no new scrape target.
	reg.Routes(mux)
	healthSrv := &http.Server{
		Addr:              cfg.HealthAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	// The service-instance registry (ADR-0037): this replica's row, written
	// every 10 s with the answer its /readyz gives, and marked not ready as
	// soon as shutdown begins. Deferred after the pool's Close, so it runs
	// before it.
	hb, err := instances.New(pool, instances.Config{
		Component: instances.WorkflowManager,
		Version:   version,
		Runtime:   identity.FromEnv(nil),
		Pool:      pool,
		Ready:     instances.ProbeHandler(mux, "/readyz"),
	}, reg, log)
	if err != nil {
		return err
	}
	stopHeartbeat := instances.Start(ctx, hb)
	defer stopHeartbeat()

	errCh := make(chan error, 4)
	go func() {
		log.Info("gRPC listener started", "addr", cfg.GRPCAddr)
		if err := grpcSrv.Serve(grpcLis); err != nil {
			errCh <- fmt.Errorf("grpc listener: %w", err)
		}
	}()
	go func() {
		log.Info("health listener started", "addr", cfg.HealthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("health listener: %w", err)
		}
	}()

	// The reconciler runs in THIS process alongside the API rather than as a
	// separate deployment. Several replicas may run it concurrently and correctness
	// does not depend on there being one: FOR UPDATE SKIP LOCKED means two replicas
	// never claim the same timer, so no leader election is needed (§3.2).
	recDone := make(chan struct{})
	go func() {
		defer close(recDone)
		if err := rec.Run(ctx); err != nil {
			errCh <- fmt.Errorf("timer reconciler: %w", err)
		}
	}()

	// The schedule fire loop, in this process for the same reason as the
	// reconciler: several replicas may run it and correctness does not depend on
	// there being one, because claiming a due schedule is FOR UPDATE SKIP LOCKED
	// and a tick's identity is a unique constraint. No leader election.
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		if err := schedSvc.Run(ctx); err != nil {
			errCh <- fmt.Errorf("schedule fire loop: %w", err)
		}
	}()

	// The sensor loop (ADR-0028), for the same reasons as the fire loop: each
	// sensor is leased with SKIP LOCKED and every observation is a unique row,
	// so any number of replicas may run it without a leader.
	listers := sensors.NewRegistry()
	if err := sensors.RegisterFromEnv(listers, os.Getenv); err != nil {
		return fmt.Errorf("object-store sensor configuration: %w", err)
	}
	if len(listers.Schemes()) == 0 {
		log.Info("no object store configured for sensors (NEXUS_SENSOR_S3_*, NEXUS_SENSOR_AZURE_*, NEXUS_SENSOR_GCS_*); object-store sensors will report an error")
	}
	sensorCfg := sensors.DefaultConfig()
	if v := os.Getenv("NEXUS_SENSOR_INTERVAL"); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil && d > 0 {
			sensorCfg.Interval = d
		}
	}
	sensorSvc := sensors.New(db, sensors.NewRunsAdapter(runSvc), listers, sensorCfg, log)
	sensorDone := make(chan struct{})
	go func() {
		defer close(sensorDone)
		if err := sensorSvc.Run(ctx); err != nil {
			errCh <- fmt.Errorf("sensor loop: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received; draining")
	}

	hc.BeginDraining()
	stopHeartbeat()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	// The reconciler stops first, so no new timer effects start while the API is
	// winding down. Any batch it had claimed is left claimed and is reclaimed by the
	// stale-claim sweeper on another replica — losing nothing, which is the point of
	// claiming rather than deleting.
	select {
	case <-recDone:
	case <-shutdownCtx.Done():
		log.Warn("timer reconciler did not stop within the grace period")
	}

	stopped := make(chan struct{})
	go func() {
		grpcSrv.GracefulStop()
		close(stopped)
	}()
	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("health shutdown did not complete cleanly", "error", err)
	}
	select {
	case <-stopped:
	case <-shutdownCtx.Done():
		grpcSrv.Stop()
	}

	log.Info("workflow-manager stopped")
	return nil
}

// registerTimerHandlers wires every timer kind this service dispatches.
//
// A kind with no handler is left claimed and then re-pooled rather than discarded
// (see the reconciler), so a missing registration degrades to a stalled deadline
// that shows up in the backlog metric — loud rather than silent. Registering all of
// them here, in one place, is what makes it possible to see at a glance that none is
// missing.
func registerTimerHandlers(rec *timers.Reconciler, runSvc *runs.Service, sched *grpcapi.SchedulerClient, log *slog.Logger) {
	// run_deadline: the run's own wall-clock budget. Owned here because
	// workflow_runs is this service's table.
	rec.Handle(timers.KindRunDeadline, func(ctx context.Context, t store.Timer) error {
		return runSvc.OnRunDeadline(ctx, t.SubjectID)
	})

	// The four attempt-scoped timeouts and the retry backoff all act on
	// task_attempts and leases, which the SCHEDULER owns. The reconciler lives here
	// because `timers` is this service's table, but the EFFECT is applied by the
	// owner — the alternative would be this service writing another's table, which
	// ADR-0011's per-service DB roles make fail at the database anyway.
	//
	// Splitting the timer store from the timer effect is the one awkward seam the
	// decomposition creates, and it is preferable to the alternative: a per-service
	// timer table would mean several reconcilers, several sweepers, and no single
	// place to answer "what deadlines are overdue right now".
	forward := func(kind string, fn func(context.Context, string) error) {
		rec.Handle(kind, func(ctx context.Context, t store.Timer) error {
			if err := fn(ctx, t.SubjectID); err != nil {
				return fmt.Errorf("dispatch %s for %s: %w", kind, t.SubjectID, err)
			}
			return nil
		})
	}
	// NOT registered here: retry_backoff and schedule_to_start. The scheduler's own
	// timer loop claims and applies those two natively, in the service that owns
	// task_attempts, so registering them here would put two claimants on the same
	// rows — and this one can only fail, because the forwarding RPCs below do not
	// exist on the scheduler yet. A retry_backoff claimed here therefore stalled
	// until the stale-claim sweeper released it, and the retry never materialized.
	// Leave the owner's kinds to the owner until the forwarding RPCs are wired.
	forward(timers.KindStartToClose, sched.OnStartToClose)
	forward(timers.KindScheduleToClose, sched.OnScheduleToClose)
	forward(timers.KindHeartbeat, sched.OnHeartbeatTimeout)
	forward(timers.KindLeaseExpiry, sched.OnLeaseExpiry)
}

type config struct {
	GRPCAddr             string
	HealthAddr           string
	DatabaseURL          string
	MaxConns             int32
	DAGEngineAddr        string
	SchedulerAddr        string
	EventServiceAddr     string
	TimerInterval        time.Duration
	TimerBatchSize       int
	TimerClaimTimeout    time.Duration
	TimerBacklogAlert    int
	TimerBacklogInterval time.Duration
	StalledRunAge        time.Duration
	StalledRunInterval   time.Duration
	CancelSweepInterval  time.Duration

	ScheduleInterval      time.Duration
	ScheduleBatchSize     int
	ScheduleSweepInterval time.Duration
	ScheduleMaxCatchup    int
	BackfillLease         time.Duration
	ShutdownTimeout       time.Duration
}

func loadConfig() config {
	return config{
		GRPCAddr:         env("NEXUS_WFM_GRPC_ADDR", ":9091"),
		HealthAddr:       env("NEXUS_WFM_HEALTH_ADDR", ":8081"),
		DatabaseURL:      nexuscfg.DSN(),
		MaxConns:         int32(envInt("NEXUS_DB_MAX_CONNS", 20)),
		DAGEngineAddr:    env("NEXUS_DAG_ENGINE_ADDR", ""),
		SchedulerAddr:    env("NEXUS_SCHEDULER_ADDR", ""),
		EventServiceAddr: env("NEXUS_EVENT_SERVICE_ADDR", ""),
		// One second bounds how LATE a deadline can fire. It is therefore
		// correctness-adjacent, not merely a performance knob: it must stay well
		// inside the smallest deadline the platform enforces.
		TimerInterval:     time.Duration(envInt("NEXUS_TIMER_INTERVAL_MS", 1000)) * time.Millisecond,
		TimerBatchSize:    envInt("NEXUS_TIMER_BATCH_SIZE", 100),
		TimerClaimTimeout: time.Duration(envInt("NEXUS_TIMER_CLAIM_TIMEOUT_SEC", 120)) * time.Second,
		TimerBacklogAlert: envInt("NEXUS_TIMER_BACKLOG_ALERT", 1000),
		// Much coarser than TimerInterval on purpose: the backlog count aggregates
		// the whole timers table, so sampling it per poll would add a full scan of
		// shared state every second for a number that moves slowly.
		TimerBacklogInterval: time.Duration(envInt("NEXUS_TIMER_BACKLOG_INTERVAL_SEC", 15)) * time.Second,

		// How long a run must show no activity before the stalled-run sweep will
		// touch it. This is a RACE GUARD, not a tuning knob: a healthy run shows
		// every node terminal for the moment between its last attempt reporting
		// and the engine publishing the outcome, and sweeping that window would
		// replace a correct conclusion with a guessed one. Two minutes is far
		// longer than that window and far shorter than a human noticing.
		StalledRunAge: time.Duration(envInt("NEXUS_STALLED_RUN_AGE_SEC", 120)) * time.Second,
		// Coarse: this is a backstop for something that should never happen, so it
		// costs a query a minute rather than a query a second.
		StalledRunInterval: time.Duration(envInt("NEXUS_STALLED_RUN_INTERVAL_SEC", 60)) * time.Second,
		// Short: the cancel sweep races the killed task's late result (F109).
		CancelSweepInterval: time.Duration(envInt("NEXUS_CANCEL_SWEEP_INTERVAL_SEC", 5)) * time.Second,

		// Cron's resolution is a minute, so polling faster than this buys nothing;
		// this bounds how late a tick can be to roughly one interval.
		ScheduleInterval:      time.Duration(envInt("NEXUS_SCHEDULE_INTERVAL_SEC", 15)) * time.Second,
		ScheduleBatchSize:     envInt("NEXUS_SCHEDULE_BATCH_SIZE", 20),
		ScheduleSweepInterval: time.Duration(envInt("NEXUS_SCHEDULE_SWEEP_SEC", 300)) * time.Second,
		ScheduleMaxCatchup:    envInt("NEXUS_SCHEDULE_MAX_CATCHUP", 10),
		// Bounds a CRASH mid-backfill, not normal pacing: a pass releases its claim
		// when done, so this only matters when a replica dies holding one.
		BackfillLease:   time.Duration(envInt("NEXUS_BACKFILL_LEASE_SEC", 120)) * time.Second,
		ShutdownTimeout: time.Duration(envInt("NEXUS_SHUTDOWN_TIMEOUT_SEC", 30)) * time.Second,
	}
}

// version is stamped at build time (-ldflags "-X main.version=...").
var version = "dev"

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if v := os.Getenv("NEXUS_LOG_LEVEL"); v != "" {
		_ = level.UnmarshalText([]byte(v))
	}
	return slog.New(tracing.LogHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))).
		With("service", "workflow-manager")
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown-host"
	}
	return h
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
