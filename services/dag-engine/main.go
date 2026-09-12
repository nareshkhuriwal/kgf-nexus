package main

import (
	"context"
	"encoding/json"
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

	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"

	"github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/dag"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

func main() {
	if err := run(); err != nil {
		// Covers failures that happen before slog is configured (bad flags,
		// unreachable DB); everything after that logs structurally.
		fmt.Fprintf(os.Stderr, "dag-engine: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dsn      = flag.String("dsn", config.DSN("postgres://postgres:test@localhost:55432/nexus"), "Postgres DSN")
		grpcAddr = flag.String("grpc-addr", envOr("NEXUS_GRPC_ADDR", ":8092"), "gRPC listen address")
		httpAddr = flag.String("http-addr", envOr("NEXUS_HTTP_ADDR", ":8082"), "address for /healthz and /readyz")
		logLevel = flag.String("log-level", envOr("NEXUS_LOG_LEVEL", "info"), "debug|info|warn|error")
		wfmAddr  = flag.String("workflow-manager-addr", envOr("NEXUS_WORKFLOW_MANAGER_ADDR", envOr("WORKFLOW_MANAGER_ADDR", "")), "workflow-manager gRPC address, for publishing run state")
		// Phase 1 ships all_success and all_done (architecture §11). The pure
		// engine implements all six rules, so enabling the rest is a policy flag
		// rather than a rebuild -- which is what makes the Phase 2 rollout a
		// configuration change.
		allowAllRules = flag.Bool("allow-all-trigger-rules", envBool("NEXUS_ALLOW_ALL_TRIGGER_RULES"), "accept the Phase-2 trigger rules at submit time")
	)
	flag.Parse()

	// JSON to stderr: these logs are scraped, not read in a terminal. Every
	// reconcile log carries run_id and node_id (CONTRACT.md logging convention)
	// so one run can be reconstructed by filtering on it.
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))
	slog.SetDefault(log)
	log = log.With("service", "dag-engine")

	// Signals are trapped before anything is opened, so Ctrl-C during startup
	// still exits cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.NewPool(ctx, store.DefaultPoolConfig(*dsn))
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	st := store.NewDAGEngineStore(pool)
	// Without a run-state writer the engine runs headless: it maintains
	// task_nodes/task_edges and creates attempts, but a run never leaves PENDING
	// and never reaches SUCCEEDED, because nothing else in the system decides
	// those transitions. That is fine for tests and wrong for a deployment, so it
	// is warned about loudly rather than defaulted into silently.
	var runStates RunStateWriter
	if *wfmAddr == "" {
		log.Warn("workflow-manager address is unset; runs will stay PENDING forever because nothing will publish their state",
			"remedy", "set NEXUS_WORKFLOW_MANAGER_ADDR")
	} else {
		c, derr := dialWFMRunState(*wfmAddr, log)
		if derr != nil {
			return fmt.Errorf("connect to workflow-manager: %w", derr)
		}
		defer func() { _ = c.close() }()
		runStates = c
	}

	engine := NewEngine(st, runStates, nil, log)
	if *allowAllRules {
		opts := dag.DefaultValidationOptions()
		opts.AllowDeferredTriggerRules = true
		engine.SetValidationOptions(opts)
		log.Warn("accepting Phase-2 trigger rules at submit time")
	}

	// The JSON codec must be registered before the server starts, since the
	// descriptor in grpcapi.go carries hand-written types rather than protobufs.
	encoding.RegisterCodec(jsonCodec{})

	grpcSrv := grpc.NewServer()
	RegisterDagEngineService(grpcSrv, NewGRPCServer(engine, log))

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *grpcAddr, err)
	}

	// Readiness flips only once the gRPC server is actually accepting, so a load
	// balancer never routes to a replica that cannot serve. Liveness is weaker on
	// purpose: a process that is up but not yet ready must not be killed.
	var ready atomic.Bool
	httpSrv := &http.Server{
		Addr:              *httpAddr,
		Handler:           healthMux(pool, &ready),
		ReadHeaderTimeout: 5 * time.Second,
	}

	httpErr := make(chan error, 1)
	go func() {
		log.Info("health endpoints listening", "addr", *httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- err
			return
		}
		httpErr <- nil
	}()

	grpcErr := make(chan error, 1)
	go func() {
		log.Info("grpc listening", "addr", *grpcAddr)
		ready.Store(true)
		grpcErr <- grpcSrv.Serve(lis)
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-grpcErr:
		if err != nil {
			return fmt.Errorf("grpc server: %w", err)
		}
	case err := <-httpErr:
		if err != nil {
			return fmt.Errorf("health server: %w", err)
		}
	}

	// Stop advertising readiness BEFORE draining, so the load balancer stops
	// sending new work while in-flight reconciles finish.
	ready.Store(false)
	grpcSrv.GracefulStop()

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		log.Warn("health server shutdown", "error", err)
	}
	return nil
}

// healthMux serves the two endpoints ADR-0011 requires of every service.
//
// /healthz and /readyz answer DIFFERENT questions and must not be aliased.
// healthz asks "is this process alive" -- a false answer gets the container
// killed. readyz asks "can it serve right now", and a false answer only removes
// it from the load balancer. A database blip should drain traffic, not trigger a
// restart loop that makes the outage worse.
func healthMux(pinger interface{ Ping(context.Context) error }, ready *atomic.Bool) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Deliberately does NOT touch the database: the process is alive.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "starting"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pinger.Ping(ctx); err != nil {
			// Unready, not unhealthy: see the comment above.
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "degraded", "error": "postgres unreachable",
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	return mux
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
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

// errorsIs wraps errors.Is so grpcapi.go does not need the import; it exists
// only to keep the status mapping readable.
func errorsIs(err, target error) bool { return errors.Is(err, target) }

// isValidationError reports whether an error came from spec validation, so it
// maps to INVALID_ARGUMENT rather than INTERNAL. It matches on the marker the
// engine wraps validation failures with, because dag.ValidationErrors is
// collapsed to a plain error at the API boundary.
func isValidationError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "validation error")
}
