// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	"github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/transport"
)

func main() {
	if err := run(); err != nil {
		// slog is already configured by the time most failures happen; this
		// covers the pre-logger ones (bad flags, unreachable DB).
		fmt.Fprintf(os.Stderr, "scheduler: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dsn       = flag.String("dsn", config.DSN("postgres://postgres:test@localhost:55432/nexus"), "Postgres DSN")
		queue     = flag.String("queue", envOr("NEXUS_QUEUE", "default"), "queue this replica drains")
		httpAddr  = flag.String("http-addr", envOr("NEXUS_HTTP_ADDR", ":8081"), "address for /healthz and /readyz")
		grpcAddr  = flag.String("grpc-addr", envOr("NEXUS_GRPC_ADDR", envOr("GRPC_ADDR", ":9090")), "address for nexus.v1.SchedulerService")
		logLevel  = flag.String("log-level", envOr("NEXUS_LOG_LEVEL", "info"), "debug|info|warn|error")
		leaseTTL  = flag.Duration("lease-ttl", 30*time.Second, "initial lease lifetime (ADR-0010 default)")
		liveness  = flag.Duration("liveness-window", 30*time.Second, "heartbeat staleness before a worker is DEAD")
		agingRate = flag.Float64("aging-rate", 1.0, "effective-priority gain per second a PENDING attempt accrues (§7.3)")
		// An escape hatch, not a tuning knob: fair share is on by default, and
		// turning it off is how an operator rolls back to pure priority order
		// before reverting migration 0022 (ADR-0017).
		fairShare = flag.Bool("fair-share", envOr("NEXUS_FAIR_SHARE", "true") != "false",
			"order the dispatch scan by namespace in-flight/weight before priority (ADR-0017)")
	)
	flag.Parse()

	// JSON to stderr because these logs are scraped, not read in a terminal.
	// Every dispatch/lost log carries run_id, attempt_id and worker_id
	// (CONTRACT.md logging convention) so an incident can be reconstructed by
	// filtering on one run.
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))
	slog.SetDefault(log)

	// A per-process identity, not a per-host one. It lands in timers.claimed_by,
	// and reusing a hostname across restarts would make a crashed replica's
	// claimed timers indistinguishable from a live replica's (the same reasoning
	// ADR-0010 applies to worker identity).
	instanceID := envOr("HOSTNAME", "") + "/" + uuid.NewString()
	log = log.With("service", "scheduler", "instance", instanceID, "queue", *queue)

	// Shutdown context. Signals are trapped before anything is opened so a
	// Ctrl-C during startup still exits cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.NewPool(ctx, store.DefaultPoolConfig(*dsn))
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	claimCfg := store.DefaultClaimConfig(*queue)
	claimCfg.LeaseTTL = *leaseTTL
	claimCfg.LivenessWindow = *liveness
	claimCfg.AgingRate = *agingRate
	claimCfg.FairShare = *fairShare

	st := store.NewSchedulerStore(pool, claimCfg, instanceID)
	sched := NewSchedulerWithLogger(st, log)

	// The scheduler is live once it can reach the DB. Readiness is stricter:
	// see readyHandler.
	var ready atomic.Bool
	srv := &http.Server{
		Addr:              *httpAddr,
		Handler:           healthMux(st, sched, &ready),
		ReadHeaderTimeout: 5 * time.Second,
	}
	srvErr := make(chan error, 1)
	go func() {
		log.Info("health endpoints listening", "addr", *httpAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
			return
		}
		srvErr <- nil
	}()

	// The gRPC API is how a POLLING worker reaches the claim. The background
	// dispatch loop above places work on its own initiative, but a pull-only
	// fleet (ADR-0005) has no way to be told about it, so the gateway calls
	// ClaimForWorker on the worker's behalf. Both paths run the SAME store
	// transaction; only the worker-selection step differs.
	//
	// The listener is bound BEFORE readiness flips, so a load balancer never
	// routes a poll to a replica whose port is not yet accepting.
	grpcLis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		return fmt.Errorf("listen for grpc on %s: %w", *grpcAddr, err)
	}
	// PeerServerOptions bounds how long one connection lives. Without it a client
	// that connected before an address swap keeps using a healthy connection to
	// whatever now answers at the old address, and nothing re-resolves it.
	sopts, terr := transport.ServerOptions()
	if terr != nil {
		// Refusing to start is the point: a deployment that configured TLS
		// incompletely must not quietly serve plaintext instead.
		return fmt.Errorf("resolve TLS server options: %w", terr)
	}
	grpcSrv := grpc.NewServer(append(config.PeerServerOptions(), sopts...)...)
	Register(grpcSrv, NewGRPCServer(st, log))
	RegisterReports(grpcSrv, NewReportServer(st, log))

	grpcErr := make(chan error, 1)
	go func() {
		log.Info("grpc listening", "addr", *grpcAddr, "service", "nexus.v1.SchedulerService")
		grpcErr <- grpcSrv.Serve(grpcLis)
	}()

	ready.Store(true)
	log.Info("scheduler starting",
		"lease_ttl", claimCfg.LeaseTTL, "liveness_window", claimCfg.LivenessWindow,
		"aging_rate", claimCfg.AgingRate, "look_ahead", claimCfg.LookAhead,
		"fair_share", claimCfg.FairShare)

	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	select {
	case err := <-srvErr:
		if err != nil {
			return fmt.Errorf("health server: %w", err)
		}
	case err := <-grpcErr:
		if err != nil {
			return fmt.Errorf("grpc server: %w", err)
		}
	case <-ctx.Done():
		log.Info("shutdown signal received; draining")
	}

	// Fail readiness first so the load balancer stops sending traffic before the
	// loops stop, then let in-flight claim transactions finish. A claim
	// interrupted mid-transaction is safe — it rolls back and a peer re-claims —
	// but finishing it avoids a needless PENDING round trip.
	ready.Store(false)
	stop()
	select {
	case <-done:
		log.Info("loops drained")
	case <-time.After(15 * time.Second):
		// Bounded: a loop wedged on a slow query must not block the pod's
		// termination past the orchestrator's grace period.
		log.Warn("loops did not drain within the grace period; exiting anyway")
	}

	// GracefulStop rather than Stop: an in-flight ClaimForWorker has a
	// transaction open, and killing the connection mid-claim would roll back a
	// dispatch the worker may already have been told about.
	grpcSrv.GracefulStop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("health server shutdown", "err", err)
	}
	log.Info("scheduler stopped")
	return nil
}

// NewSchedulerWithLogger builds a Scheduler with default cadences.
func NewSchedulerWithLogger(st *store.SchedulerStore, log *slog.Logger) *Scheduler {
	// The scheduler holds no NATS connection, deliberately. Its bus traffic is
	// written to the transactional outbox inside the claim and published by the
	// event-service relay (ADR-0003), so the correctness-critical process has no
	// broker dependency at all. The nil notifier also keeps the PUSH dispatch loop
	// off, which is required: workers are pull-only, and a pushed dispatch would
	// reach no one (see dispatchLoop). Workers are woken by dispatch HINTS
	// instead, and still claim through PollTask.
	return NewScheduler(st, log, DefaultLoopConfig(), nil)
}

func healthMux(st *store.SchedulerStore, sched *Scheduler, ready *atomic.Bool) *http.ServeMux {
	mux := http.NewServeMux()

	// /healthz is LIVENESS: is the process itself functioning? It deliberately
	// does NOT touch Postgres. A DB outage must not make every scheduler replica
	// fail its liveness probe and get killed — restarting them fixes nothing and
	// guarantees an empty fleet the moment the database returns.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// /readyz is READINESS: can this replica actually do its job right now? It
	// does query the database, because a replica that cannot claim should be
	// taken out of rotation rather than silently accepting no work.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"draining"}`))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := st.Ready(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, `{"status":"unready","err":%q}`, err.Error())
			return
		}
		if err := sched.LastError(); err != nil {
			// A loop that is erroring is a real degradation even when the probe
			// query succeeds, so it is surfaced rather than hidden.
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, `{"status":"degraded","err":%q}`, err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	return mux
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
