// Command api-gateway is the ONLY ingress to the Nexus control plane
// (architecture §3.2).
//
// It serves TWO listeners on TWO ports, which is a deliberate design choice rather
// than an accident of structure: worker traffic is chatty and latency-sensitive
// (long polls, batched lease renewals at seconds-scale intervals across the whole
// fleet), and a UI query that scans a run's event history must not make a worker's
// lease renewal queue behind it. Separate ports also mean the two can be given
// different connection limits, timeouts, and network exposure — the worker port
// need not be reachable from where humans are.
package main

import (
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
	"google.golang.org/grpc/keepalive"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	nexuscfg "github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/health"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/rest"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/upstream"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/workergrpc"
)

func main() {
	if err := run(); err != nil {
		slog.Error("api-gateway exited with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	log := newLogger()
	slog.SetDefault(log)

	cfg := loadConfig()

	// The shutdown context is cancelled on SIGTERM/SIGINT. Everything long-running
	// derives from it, so one signal unwinds the whole process in order.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// A bounded startup timeout: a gateway that hangs forever on an unreachable
	// database looks identical to one that is merely slow, and the orchestrator
	// cannot tell them apart.
	dbCtx, dbCancel := context.WithTimeout(ctx, 15*time.Second)
	defer dbCancel()
	pool, err := store.GWOpen(dbCtx, cfg.DatabaseURL, cfg.MaxConns)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()
	db := store.GWDB(pool)

	auth := authn.New(db)

	// The gateway owns only api_keys and idempotency_keys (ADR-0011). Every other
	// mutation travels through the owning service. These clients are that boundary.
	wfmClient, err := upstream.DialWorkflowManager(ctx, cfg.WorkflowManagerAddr, log)
	if err != nil {
		return fmt.Errorf("connect to workflow-manager: %w", err)
	}
	defer wfmClient.Close()

	rmClient, err := upstream.DialResourceManager(ctx, cfg.ResourceManagerAddr, db, log)
	if err != nil {
		return fmt.Errorf("connect to resource-manager: %w", err)
	}
	defer rmClient.Close()

	schedClient, err := upstream.DialScheduler(ctx, cfg.SchedulerAddr, log)
	if err != nil {
		return fmt.Errorf("connect to scheduler: %w", err)
	}
	defer schedClient.Close()

	// ── REST listener ─────────────────────────────────────────────────────
	api := rest.New(db, auth, wfmClient, rmClient, nil, log)
	mux := http.NewServeMux()
	mux.Handle("/v1/", api.Routes())

	hc := health.New(map[string]health.Check{
		// Readiness depends on the database because a gateway that cannot reach
		// Postgres can authenticate nothing and serve nothing useful.
		"postgres": func(ctx context.Context) error { return pool.Ping(ctx) },
	})
	hc.Routes(mux)

	restSrv := &http.Server{
		Addr:    cfg.RESTAddr,
		Handler: mux,
		// ReadHeaderTimeout, not ReadTimeout: a slow-header client is an attack, but
		// a legitimately slow BODY (a large spec over a poor link) is not.
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the SSE live tail holds a response open indefinitely by
		// design, and a write deadline would sever every event stream on a timer.
		// Stream lifetime is bounded by the client's own context instead.
		IdleTimeout: 120 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	// ── worker gRPC listener ──────────────────────────────────────────────
	workerSrv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(workergrpc.AuthUnaryInterceptor(auth)),
		// Long polling holds connections open, so the keepalive policy must tolerate
		// a connection with no activity for the duration of a poll. Without this,
		// gRPC's default enforcement would kill exactly the connections the
		// long-poll design depends on.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
		// A chatty fleet sends small messages; capping receive size bounds the
		// memory a misbehaving agent can make the gateway allocate.
		grpc.MaxRecvMsgSize(4<<20),
	)

	wcfg := workergrpc.DefaultConfig()
	wcfg.ResultStore = &nexusv1.ResultStoreConfig{
		Endpoint:             cfg.ResultStoreEndpoint,
		Bucket:               cfg.ResultStoreBucket,
		Region:               cfg.ResultStoreRegion,
		InlineThresholdBytes: cfg.InlineThresholdBytes,
	}
	nexusv1.RegisterWorkerServiceServer(workerSrv,
		workergrpc.NewServer(db, rmClient, schedClient, wcfg, log))

	grpcLis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen on %s for worker gRPC: %w", cfg.GRPCAddr, err)
	}

	errCh := make(chan error, 2)
	go func() {
		log.Info("REST listener started", "addr", cfg.RESTAddr)
		if err := restSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("rest listener: %w", err)
		}
	}()
	go func() {
		log.Info("worker gRPC listener started", "addr", cfg.GRPCAddr)
		if err := workerSrv.Serve(grpcLis); err != nil {
			errCh <- fmt.Errorf("grpc listener: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received; draining")
	}

	// Readiness fails FIRST, so the load balancer stops sending new requests before
	// anything is torn down. Shutting down while still advertising readiness is what
	// makes deploys drop connections.
	hc.BeginDraining()
	time.Sleep(cfg.DrainDelay)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	// GracefulStop lets in-flight worker RPCs finish. That matters more here than
	// for the REST side: killing a ReportResult mid-flight means the worker retries
	// against a gateway that may have already applied it, and while fencing and
	// idempotency make that safe, avoiding it is free.
	stopped := make(chan struct{})
	go func() {
		workerSrv.GracefulStop()
		close(stopped)
	}()

	if err := restSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("REST shutdown did not complete cleanly", "error", err)
	}

	select {
	case <-stopped:
	case <-shutdownCtx.Done():
		// A long poll that will not end cannot be allowed to block the deploy
		// forever; force it after the grace period.
		log.Warn("worker gRPC did not drain within the grace period; forcing stop")
		workerSrv.Stop()
	}

	log.Info("api-gateway stopped")
	return nil
}

type config struct {
	RESTAddr             string
	GRPCAddr             string
	DatabaseURL          string
	MaxConns             int32
	WorkflowManagerAddr  string
	ResourceManagerAddr  string
	SchedulerAddr        string
	ResultStoreEndpoint  string
	ResultStoreBucket    string
	ResultStoreRegion    string
	InlineThresholdBytes int64
	ShutdownTimeout      time.Duration
	DrainDelay           time.Duration
}

func loadConfig() config {
	return config{
		// Separate ports, per §3.2: worker traffic must not contend with UI queries.
		RESTAddr:            env("NEXUS_REST_ADDR", ":8080"),
		GRPCAddr:            env("NEXUS_GRPC_ADDR", ":9090"),
		DatabaseURL:         nexuscfg.DSN("postgres://postgres:test@localhost:55432/nexus"),
		MaxConns:            int32(envInt("NEXUS_DB_MAX_CONNS", 20)),
		WorkflowManagerAddr: env("NEXUS_WORKFLOW_MANAGER_ADDR", ""),
		ResourceManagerAddr: env("NEXUS_RESOURCE_MANAGER_ADDR", ""),
		SchedulerAddr:       env("NEXUS_SCHEDULER_ADDR", ""),
		ResultStoreEndpoint: env("NEXUS_RESULT_STORE_ENDPOINT", "http://localhost:9000"),
		ResultStoreBucket:   env("NEXUS_RESULT_STORE_BUCKET", "nexus-results"),
		ResultStoreRegion:   env("NEXUS_RESULT_STORE_REGION", "us-east-1"),
		// Results at or below this size come back inline, avoiding two round-trips
		// for a trivial output like {"rows": 42} (ADR-0008).
		InlineThresholdBytes: int64(envInt("NEXUS_INLINE_THRESHOLD_BYTES", 65536)),
		ShutdownTimeout:      time.Duration(envInt("NEXUS_SHUTDOWN_TIMEOUT_SEC", 30)) * time.Second,
		// A brief pause after failing readiness, so load balancers observe the
		// change before connections start closing.
		DrainDelay: time.Duration(envInt("NEXUS_DRAIN_DELAY_MS", 500)) * time.Millisecond,
	}
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if v := os.Getenv("NEXUS_LOG_LEVEL"); v != "" {
		_ = level.UnmarshalText([]byte(v))
	}
	// JSON, because these logs are aggregated and queried by run_id rather than
	// read by eye.
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})).
		With("service", "api-gateway")
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
