// Command resource-manager is the Nexus capacity ledger service (§3.2).
//
// It owns the `workers` and `reservations` tables and is their only writer, with
// one documented exception: the scheduler writes both inside its dispatch claim
// transaction, because atomicity with the lease and the attempt CAS outweighs
// ownership purity (ADR-0011, invariant 4).
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
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
	"github.com/khuriwalgroup/kgf-nexus/services/resource-manager/internal/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

var version = "dev"

func main() {
	var (
		grpcAddr = flag.String("grpc-addr", envOr("RM_GRPC_ADDR", ":50052"), "gRPC listen address")
		httpAddr = flag.String("http-addr", envOr("RM_HTTP_ADDR", ":8082"), "health endpoint listen address")
		dsn      = flag.String("database-url",
			config.DSN("postgres://postgres:test@localhost:55432/nexus"),
			"Postgres DSN")
		livenessWindow = flag.Duration("liveness-window",
			envDuration("RM_LIVENESS_WINDOW", server.DefaultLivenessWindow),
			"silence after which a worker is declared DEAD (§5.5)")
		sweepInterval = flag.Duration("sweep-interval",
			envDuration("RM_SWEEP_INTERVAL", 10*time.Second), "liveness sweep period")
		reconcileGrace = flag.Duration("reconcile-grace",
			envDuration("RM_RECONCILE_GRACE", server.DefaultReconcileGrace),
			"how long a reservation is protected from being treated as a leak")
		logLevel = flag.String("log-level", envOr("RM_LOG_LEVEL", "info"), "debug|info|warn|error")
		showVer  = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}

	log := newLogger(*logLevel).With("service", "resource-manager", "version", version)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, log, runConfig{
		grpcAddr: *grpcAddr, httpAddr: *httpAddr, dsn: *dsn,
		livenessWindow: *livenessWindow, sweepInterval: *sweepInterval,
		reconcileGrace: *reconcileGrace,
	}); err != nil {
		log.Error("resource-manager exiting with an error", "error", err)
		os.Exit(1)
	}
	log.Info("resource-manager exited cleanly")
}

type runConfig struct {
	grpcAddr, httpAddr, dsn                       string
	livenessWindow, sweepInterval, reconcileGrace time.Duration
}

func run(ctx context.Context, log *slog.Logger, cfg runConfig) error {
	pcfg, err := pgxpool.ParseConfig(cfg.dsn)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	// The ledger's hot path is short transactions under a worker row lock, so a
	// modest pool with a health check beats a large one: more connections would
	// just queue longer on the same locks while consuming Postgres backends.
	pcfg.MaxConns = 16
	pcfg.MaxConnLifetime = 30 * time.Minute
	pcfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	// Migrations are applied by a DEDICATED JOB, never by a service at startup
	// (ADR-0011). A service that migrates on boot turns a rolling deploy of N
	// replicas into N racing schema changes.
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping postgres: %w", err)
	}
	log.Info("connected to the metadata store")

	st := store.NewRMStore(pool)
	svc := server.New(st, log)
	svc.ReconcileGrace = cfg.reconcileGrace

	sweeper := server.NewSweeper(st, log)
	sweeper.LivenessWindow = cfg.livenessWindow
	sweeper.Interval = cfg.sweepInterval

	// PeerServerOptions bounds how long one connection lives. Without it a client
	// that connected before an address swap keeps using a healthy connection to
	// whatever now answers at the old address, and nothing re-resolves it.
	gs := grpc.NewServer(config.PeerServerOptions()...)
	rmv1.RegisterResourceManagerServiceServer(gs, svc)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(gs, hs)
	// Reflection makes grpcurl work against a running instance, which is the
	// difference between debugging an incident in seconds and writing a client.
	reflection.Register(gs)

	lis, err := net.Listen("tcp", cfg.grpcAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.grpcAddr, err)
	}

	// ready is flipped only once the gRPC listener is actually serving, so a load
	// balancer cannot route traffic to a replica that is still starting.
	var ready atomic.Bool
	httpSrv := healthServer(cfg.httpAddr, pool, &ready)

	errs := make(chan error, 3)
	go func() {
		log.Info("gRPC serving", "addr", cfg.grpcAddr)
		ready.Store(true)
		hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
		if err := gs.Serve(lis); err != nil {
			errs <- fmt.Errorf("grpc serve: %w", err)
		}
	}()
	go func() {
		log.Info("health endpoints serving", "addr", cfg.httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("http serve: %w", err)
		}
	}()
	go func() {
		log.Info("liveness sweeper starting",
			"liveness_window", cfg.livenessWindow, "interval", cfg.sweepInterval)
		if err := sweeper.Run(ctx); err != nil {
			errs <- fmt.Errorf("sweeper: %w", err)
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case runErr = <-errs:
	}

	// Fail readiness first so the load balancer drains us BEFORE we stop
	// accepting calls; otherwise in-flight requests are refused rather than
	// completed.
	ready.Store(false)
	hs.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)

	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)

	// GracefulStop lets in-flight reservations and releases commit. Killing them
	// mid-transaction would roll back a dispatch's reservation while the
	// scheduler believed it succeeded.
	done := make(chan struct{})
	go func() { gs.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-shutCtx.Done():
		log.Warn("graceful stop timed out; forcing")
		gs.Stop()
	}
	return runErr
}

// healthServer exposes /healthz and /readyz.
//
// The two are deliberately different. /healthz answers "is this process alive" —
// it must NOT touch the database, because a database blip would otherwise make
// Kubernetes restart every replica and turn a recoverable outage into a
// cold-start stampede. /readyz answers "should traffic come here" and DOES check
// the database, because a replica that cannot reach Postgres can serve nothing.
func healthServer(addr string, pool *pgxpool.Pool, ready *atomic.Bool) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, "not serving yet", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			http.Error(w, "metadata store unreachable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
