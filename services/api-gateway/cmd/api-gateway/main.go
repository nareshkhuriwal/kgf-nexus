// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

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
	"github.com/khuriwalgroup/kgf-nexus/internal/objstore"

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
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	nexuscfg "github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/kgfctx"
	"github.com/khuriwalgroup/kgf-nexus/internal/mail"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/secretbox"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/transport"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/auditexport"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/health"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/logs"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/rest"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/sessions"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/upstream"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/workergrpc"
)

// workerMaxConnectionAge is the longest an agent connection lives before the
// gateway asks it to redial, and so the longest a withdrawn certificate or CA
// can keep an already-connected agent attached. See the worker listener.
const workerMaxConnectionAge = 5 * time.Minute

func main() {
	if err := run(); err != nil {
		slog.Error("api-gateway exited with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	log := newLogger()
	slog.SetDefault(log)

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// Refuse to start rather than hand every worker a threshold that would push
	// multi-megabyte results through ReportResult and into task_attempts. The
	// agent clamps too; failing here is what makes the misconfiguration visible.
	if err := domain.ValidateInlineThreshold(cfg.InlineThresholdBytes); err != nil {
		return fmt.Errorf("NEXUS_INLINE_THRESHOLD_BYTES: %w", err)
	}

	// The shutdown context is cancelled on SIGTERM/SIGINT. Everything long-running
	// derives from it, so one signal unwinds the whole process in order.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Tracing (ADR-0026): off unless OTEL_* configures an exporter; flushed last.
	defer tracing.SetupOrExit(ctx, "api-gateway", version, log)()

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

	// Metrics. Served ONLY on the dedicated operations listener below, never on
	// the REST port: the REST port is the internet-facing ingress, and a
	// /metrics there would publish route inventories, error rates and build
	// details to anyone who can reach the console.
	reg := metrics.NewServiceRegistry("api-gateway", version)
	transport.RegisterMetrics(reg)
	store.RegisterPoolMetrics(reg, pool)
	httpMetrics := metrics.NewHTTPMetrics(reg)
	grpcMetrics := metrics.NewGRPCServerMetrics(reg)

	// Identity (human sessions and scoped tokens) is resolved by the SAME
	// authenticator that resolves API keys, so there is still exactly one place a
	// request is authenticated. API keys are unaffected: WithIdentity only adds
	// credential kinds, dispatched by prefix, and an unprefixed credential still
	// takes the original api_keys path.
	auth := authn.New(db).WithRBAC().
		WithIdentity(authn.StoreIdentity{DB: db}).
		WithLogger(log)

	// The gateway owns only api_keys and idempotency_keys (ADR-0011). Every other
	// mutation travels through the owning service. These clients are that boundary.
	wfmClient, err := upstream.DialWorkflowManager(ctx, cfg.WorkflowManagerAddr, db, log)
	if err != nil {
		return fmt.Errorf("connect to workflow-manager: %w", err)
	}
	defer wfmClient.Close()

	rmClient, err := upstream.DialResourceManager(ctx, cfg.ResourceManagerAddr, db, log)
	if err != nil {
		return fmt.Errorf("connect to resource-manager: %w", err)
	}
	defer rmClient.Close()

	schedClient, err := upstream.DialScheduler(ctx, cfg.SchedulerAddr, cfg.DAGEngineAddr, log)
	if err != nil {
		return fmt.Errorf("connect to scheduler: %w", err)
	}
	defer schedClient.Close()

	// Durable task logs (ADR-0020). The gateway reads log objects from the result
	// store, so it needs read credentials; without them the route answers 501
	// and agents are told not to ship, rather than filling a store nobody can
	// read back.
	logStreamer, logShipping := logBackend(ctx, cfg, db, reg, log)

	// Notifications, webhook triggers, sensors and lineage (ADR-0028). A
	// malformed secret key is fatal; an absent one is not -- the gateway then
	// refuses to store inline secrets (503) instead of storing them in clear.
	secretBox, err := secretbox.FromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("NEXUS_SECRET_KEY: %w", err)
	}
	if secretBox == nil {
		log.Warn("NEXUS_SECRET_KEY is not set: webhook triggers and inline channel secrets cannot be created")
	}
	// A trigger's hook URL is given to a third party, so a wrong base is worse
	// than none: a malformed value stops startup (F156).
	trustedProxies, err := rest.ParseTrustedProxies(os.Getenv("NEXUS_TRUSTED_PROXIES"))
	if err != nil {
		return err
	}
	eventsCfg := rest.EventsConfig{
		Secrets:             secretBox,
		TriggeredRuns:       wfmClient,
		AllowPrivateTargets: os.Getenv("NEXUS_NOTIFY_ALLOW_PRIVATE_TARGETS") == "true",
		PublicURL:           os.Getenv("NEXUS_PUBLIC_URL"),
		TrustedProxies:      trustedProxies,
	}
	if err := eventsCfg.Validate(); err != nil {
		return err
	}
	if eventsCfg.PublicURL == "" && len(trustedProxies) == 0 {
		log.Warn("neither NEXUS_PUBLIC_URL nor NEXUS_TRUSTED_PROXIES is set: webhook trigger URLs are reported as unknown and the console shows only their path")
	}

	// ── REST listener ─────────────────────────────────────────────────────
	api := rest.New(db, auth, wfmClient, rmClient, logStreamer, log).
		// The same client serves both: schedule writes go through workflow-manager
		// because it owns the table, and it already holds the connection.
		WithSchedules(wfmClient).
		WithBackfills(wfmClient).
		WithIdentity(rest.NewStoreIdentityAdmin(db)).
		// Reported with each replay assignment (ADR-0022), so a replayed attempt
		// inlines results exactly as the platform would (ADR-0016).
		WithInlineThreshold(cfg.InlineThresholdBytes).
		// Nothing else in the deployment aggregates per-service health: each
		// service serves its own /healthz and /readyz, but the gateway is the only
		// ingress, so a console has no route to them.
		WithFleetHealth(upstream.Fleet{WFM: wfmClient, RM: rmClient, Sched: schedClient}).
		// Memo invalidation rides the scheduler client's dag-engine connection,
		// the gateway's one existing route to the owner of memo_invalidations.
		WithCacheInvalidator(schedClient).
		// Rejections and bucket counts per limiter kind (docs/OPERATIONS.md §17);
		// limiters enabled below are instrumented as they are built.
		WithRateLimitMetrics(rest.NewRateLimitMetrics(reg)).
		WithEvents(eventsCfg)

	// Rate limiting is opt-in because a limit guessed wrong is itself an outage:
	// the right number depends on fleet size and the console's poll interval. Off
	// by default keeps every existing deployment behaving exactly as before, and
	// NEXUS_RATE_LIMIT_RPS turns it on for a deployment that has chosen a number.
	if limits, ok := rateLimitsFromEnv(); ok {
		api = api.WithRateLimits(limits)
		log.Info("rate limiting enabled (in memory, per replica)",
			"per_address_rps", limits.PerAddress.RequestsPerSecond, "per_address_burst", limits.PerAddress.Burst,
			"auth_failure_rps", limits.AuthFailures.RequestsPerSecond, "auth_failure_burst", limits.AuthFailures.Burst,
			"per_credential_rps", limits.PerCredential.RequestsPerSecond, "per_credential_burst", limits.PerCredential.Burst,
			"why", "the gateway is the only ingress, so an unlimited one has no defence against a retry loop or credential stuffing")
	}
	emailCfg, err := emailFlowConfig(log)
	if err != nil {
		return err
	}
	api = api.WithEmailFlows(emailCfg)
	mode, kgfAuth, kgfAttrib, kgfReady, err := kgfConfig(ctx, db, log)
	if err != nil {
		return err
	}
	api = api.WithKGF(mode, kgfAuth)

	// Audit export (ADR-0025): sink secrets are sealed under
	// NEXUS_AUDIT_SINK_KEY(_FILE); without it sinks are refused (503) but
	// listing, export, retention and legal hold all work. A malformed key or
	// destination policy is fatal, like any other security configuration.
	auditKeys, err := auditexport.KeyringFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	sinkPolicy, err := auditexport.PolicyFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	api = api.WithAuditExport(auditKeys, sinkPolicy)
	// KGF Cloud attribution in standalone mode (ADR-0025 with ADR-0024): with
	// KGF's context keys configured, a request that authenticates with a Nexus
	// credential and carries a verifying X-KGF-Context has its KGF
	// organization, scope, principal and request id recorded -- attribution
	// only. In kgf/both mode the context authenticates, and attribution comes
	// from that principal (kgfAttrib is nil there).
	api = api.WithKGFContext(kgfAttrib)
	rest.RegisterAuditMetrics(reg)
	deliverer := &auditexport.Deliverer{DB: db, Keys: auditKeys, Policy: sinkPolicy, Log: log}
	deliverer.RegisterMetrics(reg)
	purger := &auditexport.Purger{DB: db, Log: log}
	purger.RegisterMetrics(reg)

	checks := map[string]health.Check{
		// Readiness depends on the database because a gateway that cannot reach
		// Postgres can authenticate nothing and serve nothing useful.
		"postgres": func(ctx context.Context) error { return pool.Ping(ctx) },
	}
	if kgfReady != nil {
		// In KGF mode a gateway without the context verification keys can
		// authenticate no person, so it is not ready (ADR-0024 §2).
		checks["kgf_context_keys"] = kgfReady
	}
	hc := health.New(checks)

	restSrv := &http.Server{
		Addr:    cfg.RESTAddr,
		Handler: tracing.HTTPMiddleware(publicHandler(api.Routes(), hc, httpMetrics)),
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
	//
	// This is the one hop that leaves the control-plane network: agents connect
	// from the execution plane and every call carries an API key and a session
	// token, so it is the listener where transport encryption matters most.
	workerTLS, err := transport.ServerOptions()
	if err != nil {
		return fmt.Errorf("resolve TLS server options: %w", err)
	}
	// Metrics interceptors FIRST, so a call the auth interceptor rejects is still
	// counted (as Unauthenticated) rather than invisible.
	workerOpts := append(grpcMetrics.ServerOptions(),
		grpc.ChainUnaryInterceptor(workergrpc.AuthUnaryInterceptor(auth)),
		// Long polling holds connections open, so the keepalive policy must tolerate
		// a connection with no activity for the duration of a poll. Without this,
		// gRPC's default enforcement would kill exactly the connections the
		// long-poll design depends on.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		// MaxConnectionAge bounds how long a credential decision made at handshake
		// time can outlive a revocation: without it, an agent that connected
		// before its certificate (or the CA that signed it) was withdrawn kept its
		// connection until it happened to drop. After this age the server sends
		// GOAWAY and the agent redials, re-presenting its certificate to the
		// current trust bundle. Minutes rather than the control plane's 30s,
		// because this listener carries the whole fleet and each redial is a TLS
		// handshake per agent.
		//
		// The grace is derived from the long-poll cap, not picked separately: GOAWAY
		// lets in-flight RPCs finish, and a poll cut before it returns could strand
		// an attempt the scheduler has already claimed for this worker.
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:                  30 * time.Second,
			Timeout:               10 * time.Second,
			MaxConnectionAge:      workerMaxConnectionAge,
			MaxConnectionAgeGrace: workergrpc.DefaultConfig().MaxPollWait + 15*time.Second,
		}),
		// A chatty fleet sends small messages; capping receive size bounds the
		// memory a misbehaving agent can make the gateway allocate.
		grpc.MaxRecvMsgSize(4<<20),
	)
	workerSrv := grpc.NewServer(append(workerOpts, workerTLS...)...)

	wcfg := workergrpc.DefaultConfig()
	wcfg.ResultStore = &nexusv1.ResultStoreConfig{
		Endpoint:             cfg.ResultStoreEndpoint,
		Bucket:               cfg.ResultStoreBucket,
		Region:               cfg.ResultStoreRegion,
		InlineThresholdBytes: cfg.InlineThresholdBytes,
		// Agents speak the same API the gateway does unless their own
		// NEXUS_RESULT_STORE_BACKEND says otherwise (ADR-0027).
		Backend: cfg.Store.Backend,
	}
	wcfg.LogShipping = logShipping
	// The agent admission gate (docs/COMPATIBILITY.md §4.4). Both off by
	// default. A policy that cannot be applied (an unparsable minimum) stops
	// the gateway here instead of refusing every agent at Register.
	wcfg.AgentPolicy = agentPolicyFromEnv()
	if err := wcfg.AgentPolicy.Validate(); err != nil {
		return fmt.Errorf("NEXUS_MIN_AGENT_VERSION / NEXUS_REQUIRED_AGENT_CAPABILITIES: %w", err)
	}
	if wcfg.AgentPolicy.MinVersion != "" || len(wcfg.AgentPolicy.RequiredCapabilities) > 0 {
		log.Info("agent admission gate enabled",
			"min_agent_version", wcfg.AgentPolicy.MinVersion,
			"required_agent_capabilities", wcfg.AgentPolicy.RequiredCapabilities)
	}
	if cfg.WorkloadAPI {
		wcfg.WorkloadAPI = workergrpc.DefaultWorkloadAPI()
		wcfg.WorkloadAPI.MaxCheckpointBytes = cfg.MaxCheckpointBytes
	}
	nexusv1.RegisterWorkerServiceServer(workerSrv,
		workergrpc.NewServer(db, rmClient, schedClient, wcfg, log))

	grpcLis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen on %s for worker gRPC: %w", cfg.GRPCAddr, err)
	}

	// ── operations listener: /metrics, /healthz, /readyz ──────────────────
	// Bound before the goroutines start so a port conflict fails startup with
	// the address in the error. Health is served here TOO, so a probe or a
	// scraper never needs the public port.
	opsSrv, err := metrics.ListenOps(cfg.MetricsAddr, metrics.OpsMux(reg, hc.Routes), log)
	if err != nil {
		return fmt.Errorf("listen on %s for metrics: %w", cfg.MetricsAddr, err)
	}

	// The sweeper derives from the same shutdown context as the listeners, so the
	// signal that drains them ends it too and no goroutine outlives run().
	sweeper := sessions.New(db, log)
	sweeper.Interval = cfg.SessionSweepInterval
	sweeper.BatchLimit = cfg.SessionSweepLimit

	errCh := make(chan error, 6)
	go func() {
		if err := deliverer.Run(ctx); err != nil {
			errCh <- fmt.Errorf("audit sink deliverer: %w", err)
		}
	}()
	go func() {
		if err := purger.Run(ctx); err != nil {
			errCh <- fmt.Errorf("audit retention purger: %w", err)
		}
	}()
	go func() {
		if err := opsSrv.Serve(); err != nil {
			errCh <- fmt.Errorf("metrics listener: %w", err)
		}
	}()
	go func() {
		log.Info("expired-session sweeper started",
			"interval", sweeper.Interval, "limit", sweeper.BatchLimit)
		if err := sweeper.Run(ctx); err != nil {
			errCh <- fmt.Errorf("session sweeper: %w", err)
		}
	}()
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
	// After the listener: no new request can queue mail now, and anything already
	// queued (a registration's verification, a reset link) gets the rest of the
	// grace period to go out rather than being dropped with the process.
	if err := api.CloseEmailFlows(shutdownCtx); err != nil {
		log.Error("queued email was not fully delivered before shutdown", "error", err)
	}

	select {
	case <-stopped:
	case <-shutdownCtx.Done():
		// A long poll that will not end cannot be allowed to block the deploy
		// forever; force it after the grace period.
		log.Warn("worker gRPC did not drain within the grace period; forcing stop")
		workerSrv.Stop()
	}
	// Last, so the shutdown itself stays observable until the end.
	if err := opsSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("metrics listener shutdown did not complete cleanly", "error", err)
	}

	log.Info("api-gateway stopped")
	return nil
}

type config struct {
	RESTAddr string
	GRPCAddr string
	// MetricsAddr is the operations listener (/metrics plus the probes). It must
	// NOT be exposed outside the cluster; "off" disables it.
	MetricsAddr         string
	DatabaseURL         string
	MaxConns            int32
	WorkflowManagerAddr string
	ResourceManagerAddr string
	SchedulerAddr       string
	// DAGEngineAddr is needed on the RESULT path: the engine recomputes the
	// frontier after an attempt goes terminal, which is what makes the next node
	// start.
	DAGEngineAddr        string
	ResultStoreEndpoint  string
	ResultStoreBucket    string
	ResultStoreRegion    string
	InlineThresholdBytes int64
	ShutdownTimeout      time.Duration
	DrainDelay           time.Duration
	// SessionSweepInterval and SessionSweepLimit tune the expired-session sweep.
	// Exposed because the right numbers depend on login volume, which only the
	// deployment knows; the defaults suit a single console rather than an SSO
	// front door minting thousands of sessions an hour.
	SessionSweepInterval time.Duration
	SessionSweepLimit    int

	// Store is the result-store configuration for serving logs (ADR-0020,
	// ADR-0027): the same NEXUS_RESULT_STORE_* / NEXUS_S3_* / NEXUS_AZURE_* /
	// NEXUS_GCS_* variables the agent reads, so compose needs no new settings.
	Store objstore.Config
	// LogShipping turns durable task logs on (default) or off.
	LogShipping        bool
	LogMaxBytes        int64
	LogChunkBytes      int64
	LogFlushIntervalMs int
	// WorkloadAPI offers tasks usage reporting, shared rate limits and
	// checkpoints through their agent (ADR-0032). On by default.
	WorkloadAPI        bool
	MaxCheckpointBytes int64
}

func loadConfig() (config, error) {
	store, err := objstore.ConfigFromEnv(nil)
	if err != nil {
		return config{}, err
	}
	c := config{
		// Separate ports, per §3.2: worker traffic must not contend with UI queries.
		RESTAddr:            env("NEXUS_REST_ADDR", ":8080"),
		GRPCAddr:            env("NEXUS_GRPC_ADDR", ":9090"),
		MetricsAddr:         env("NEXUS_METRICS_ADDR", ":8088"),
		DatabaseURL:         nexuscfg.DSN(),
		MaxConns:            int32(envInt("NEXUS_DB_MAX_CONNS", 20)),
		WorkflowManagerAddr: env("NEXUS_WORKFLOW_MANAGER_ADDR", ""),
		ResourceManagerAddr: env("NEXUS_RESOURCE_MANAGER_ADDR", ""),
		SchedulerAddr:       env("NEXUS_SCHEDULER_ADDR", ""),
		DAGEngineAddr:       env("NEXUS_DAG_ENGINE_ADDR", env("DAG_ENGINE_ADDR", "")),
		ResultStoreEndpoint: env("NEXUS_RESULT_STORE_ENDPOINT", defaultEndpoint(store.Backend)),
		ResultStoreBucket:   env("NEXUS_RESULT_STORE_BUCKET", "nexus-results"),
		ResultStoreRegion:   env("NEXUS_RESULT_STORE_REGION", "us-east-1"),
		// Results at or below this size come back inline, avoiding two round-trips
		// for a trivial output like {"rows": 42} (ADR-0008).
		InlineThresholdBytes: int64(envInt("NEXUS_INLINE_THRESHOLD_BYTES", int(domain.DefaultInlineThresholdBytes))),
		ShutdownTimeout:      time.Duration(envInt("NEXUS_SHUTDOWN_TIMEOUT_SEC", 30)) * time.Second,
		// A brief pause after failing readiness, so load balancers observe the
		// change before connections start closing.
		DrainDelay: time.Duration(envInt("NEXUS_DRAIN_DELAY_MS", 500)) * time.Millisecond,
		SessionSweepInterval: time.Duration(envInt("NEXUS_SESSION_SWEEP_INTERVAL_SEC",
			int(sessions.DefaultInterval/time.Second))) * time.Second,
		SessionSweepLimit: envInt("NEXUS_SESSION_SWEEP_LIMIT", sessions.DefaultBatchLimit),

		Store:              store,
		LogShipping:        envBool("NEXUS_LOG_SHIPPING", true),
		LogMaxBytes:        int64(envInt("NEXUS_LOG_MAX_BYTES_PER_ATTEMPT", 64<<20)),
		LogChunkBytes:      int64(envInt("NEXUS_LOG_CHUNK_BYTES", 256<<10)),
		LogFlushIntervalMs: envInt("NEXUS_LOG_FLUSH_INTERVAL_MS", 2000),
		WorkloadAPI:        envBool("NEXUS_WORKLOAD_API", true),
		MaxCheckpointBytes: int64(envInt("NEXUS_MAX_CHECKPOINT_BYTES", workergrpc.DefaultMaxCheckpointBytes)),
	}
	c.Store.Endpoint, c.Store.Bucket, c.Store.Region = c.ResultStoreEndpoint, c.ResultStoreBucket, c.ResultStoreRegion
	return c, nil
}

// defaultEndpoint keeps the historical local default for S3 only: Azure and
// GCS derive their endpoints from the account and the SDK, and a localhost
// default would point them at nothing.
func defaultEndpoint(backend string) string {
	if backend == objstore.BackendS3 {
		return "http://localhost:9000"
	}
	return ""
}

// version is stamped at build time (-ldflags "-X main.version=...").
var version = "dev"

// publicHandler is the REST listener's handler: the API mux under /v1/, the
// probes, and route-template RED metrics around all of it.
//
// The API mux is mounted DIRECTLY (no wrapper between the two muxes) because
// the metrics middleware reads the matched pattern from the request after the
// handler returns, and net/http's ServeMux records it on the *Request it was
// given. A wrapper that called r.WithContext between them would hide the inner
// pattern and collapse every route into "/v1/". TestPublicHandlerRouteTemplates
// pins this.
func publicHandler(api http.Handler, hc *health.Handler, m *metrics.HTTPMetrics) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v1/", api)
	hc.Routes(mux)
	return m.Middleware(mux)
}

// logBackend builds the log reader and the policy advertised to agents. Both
// are off together: an agent is told to ship only when this gateway can serve
// what it ships.
func logBackend(ctx context.Context, cfg config, db store.GWDB, reg *metrics.Registry, log *slog.Logger) (rest.LogStreamer, *nexusv1.LogShippingConfig) {
	if !cfg.LogShipping {
		log.Warn("durable task logs are DISABLED (NEXUS_LOG_SHIPPING=false); agents will not ship logs and the logs route answers 501")
		return nil, nil
	}
	store, err := objstore.Open(ctx, cfg.Store)
	if err != nil {
		log.Warn("durable task logs are unavailable: the gateway cannot read the result store; agents will not ship logs and the logs route answers 501",
			"backend", cfg.Store.Backend, "error", err,
			"fix", "give the gateway read access to */logs/*: static S3 keys, a cloud workload identity (NEXUS_S3_CREDENTIALS, NEXUS_AZURE_STORAGE_AUTH, NEXUS_GCS_AUTH), see OPERATIONS §26")
		return nil, nil
	}
	reader := logs.NewStoreReader(objstore.Instrument(store, objstore.NewMetrics(reg)))
	policy := &nexusv1.LogShippingConfig{
		Enabled:            true,
		MaxBytesPerAttempt: cfg.LogMaxBytes,
		ChunkBytes:         cfg.LogChunkBytes,
		FlushIntervalMs:    int32(cfg.LogFlushIntervalMs),
	}
	log.Info("durable task logs enabled", "backend", cfg.Store.Backend, "bucket", cfg.ResultStoreBucket,
		"max_bytes_per_attempt", cfg.LogMaxBytes, "chunk_bytes", cfg.LogChunkBytes,
		"flush_interval_ms", cfg.LogFlushIntervalMs)
	return logs.NewReader(logs.StoreIndex{DB: db}, reader, logs.Limits{}), policy
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if v := os.Getenv("NEXUS_LOG_LEVEL"); v != "" {
		_ = level.UnmarshalText([]byte(v))
	}
	// JSON, because these logs are aggregated and queried by run_id rather than
	// read by eye.
	return slog.New(tracing.LogHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))).
		With("service", "api-gateway")
}

// rateLimitsFromEnv reads the gateway limiter settings (docs/OPERATIONS.md §16).
//
// NEXUS_RATE_LIMIT_RPS / _BURST keep their original meaning — the per-credential
// allowance — and remain the one switch: unset, nothing is limited. The address
// and auth-failure buckets default from them (rest.GatewayRateLimitsFor) and each
// can be overridden. Overriding a rate without its burst sets the burst to the
// rate, the same rule NEXUS_RATE_LIMIT_BURST has always followed.
func rateLimitsFromEnv() (rest.GatewayRateLimits, bool) {
	rps := envFloat("NEXUS_RATE_LIMIT_RPS", 0)
	if rps <= 0 {
		return rest.GatewayRateLimits{}, false
	}
	limits := rest.GatewayRateLimitsFor(rest.RateLimitConfig{
		RequestsPerSecond: rps,
		Burst:             envFloat("NEXUS_RATE_LIMIT_BURST", 0),
		MaxEntries:        envInt("NEXUS_RATE_LIMIT_MAX_KEYS", 0),
	})
	override := func(c *rest.RateLimitConfig, rpsKey, burstKey string) {
		if v := envFloat(rpsKey, 0); v > 0 {
			c.RequestsPerSecond = v
			c.Burst = envFloat(burstKey, 0)
		} else if b := envFloat(burstKey, 0); b > 0 {
			c.Burst = b
		}
	}
	override(&limits.PerAddress, "NEXUS_RATE_LIMIT_IP_RPS", "NEXUS_RATE_LIMIT_IP_BURST")
	override(&limits.AuthFailures, "NEXUS_RATE_LIMIT_AUTH_FAIL_RPS", "NEXUS_RATE_LIMIT_AUTH_FAIL_BURST")
	return limits, true
}

// envFloat reads a float setting, treating an unparseable value as absent rather
// than as zero: silently disabling a limit the operator asked for would be worse
// than ignoring a typo, because nothing would report it.
func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// agentPolicyFromEnv reads the agent admission gate: NEXUS_MIN_AGENT_VERSION
// (a semantic version) and NEXUS_REQUIRED_AGENT_CAPABILITIES (comma-separated).
func agentPolicyFromEnv() domain.AgentPolicy {
	p := domain.AgentPolicy{MinVersion: strings.TrimSpace(os.Getenv("NEXUS_MIN_AGENT_VERSION"))}
	for _, c := range strings.Split(os.Getenv("NEXUS_REQUIRED_AGENT_CAPABILITIES"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			p.RequiredCapabilities = append(p.RequiredCapabilities, c)
		}
	}
	return p
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

// emailFlowConfig reads mail delivery and verification policy from the
// environment and says, loudly, which mode the gateway is in.
//
// A misconfiguration (bad port, TLS mode, a From with no address, credentials
// over plain text, SMTP without a public URL to build links from) stops startup:
// discovering it from the first person whose reset email never arrived is worse.
// NO configuration at all is valid and yields the disabled sender, which refuses
// every send -- and the WARN below is the operator-facing record of that choice.
func emailFlowConfig(log *slog.Logger) (rest.EmailFlowConfig, error) {
	mcfg, err := mail.ConfigFromEnv(os.Getenv)
	if err != nil {
		return rest.EmailFlowConfig{}, fmt.Errorf("email delivery configuration: %w", err)
	}
	sender, err := mail.New(mcfg)
	if err != nil {
		return rest.EmailFlowConfig{}, fmt.Errorf("email delivery configuration: %w", err)
	}
	cfg := rest.DefaultEmailFlowConfig()
	cfg.Mailer = sender
	cfg.PublicURL = env("NEXUS_PUBLIC_URL", "")
	cfg.VerifyTTL = time.Duration(envInt("NEXUS_EMAIL_VERIFY_TTL_SEC", int(cfg.VerifyTTL/time.Second))) * time.Second
	cfg.ResetTTL = time.Duration(envInt("NEXUS_PASSWORD_RESET_TTL_SEC", int(cfg.ResetTTL/time.Second))) * time.Second
	cfg.RequireVerified = envBool("NEXUS_EMAIL_VERIFICATION_REQUIRED", true)
	if err := cfg.Validate(); err != nil {
		return rest.EmailFlowConfig{}, fmt.Errorf("email flows: %w", err)
	}

	if !mcfg.Enabled() {
		effect := "verification and password-reset requests answer 503 email_unavailable"
		if cfg.RequireVerified {
			effect += "; accounts created from now on cannot confirm their address and therefore cannot sign in"
		}
		log.Warn("email delivery is DISABLED: NEXUS_SMTP_HOST is not set",
			"verification_required", cfg.RequireVerified, "effect", effect)
		return cfg, nil
	}
	log.Info("email delivery configured",
		"smtp_host", mcfg.Host, "smtp_port", mcfg.Port, "smtp_tls", string(mcfg.TLS),
		"smtp_auth", mcfg.Username != "", "public_url", cfg.PublicURL,
		"verify_ttl", cfg.VerifyTTL.String(), "reset_ttl", cfg.ResetTTL.String(),
		"verification_required", cfg.RequireVerified)
	if !cfg.RequireVerified {
		log.Warn("NEXUS_EMAIL_VERIFICATION_REQUIRED=false: accounts may sign in before proving their email address")
	}
	// Probed in the background so a slow mail server never delays startup, but
	// reported at ERROR so a wrong host, certificate or password is in the log
	// from the first minute rather than discovered by a user.
	if s, ok := sender.(*mail.SMTP); ok {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), mcfg.Timeout+5*time.Second)
			defer cancel()
			if err := s.Probe(ctx); err != nil {
				log.Error("SMTP server probe failed; verification and reset emails will not be delivered until this is fixed",
					"smtp_host", mcfg.Host, "smtp_port", mcfg.Port, "error", err)
				return
			}
			log.Info("SMTP server probe succeeded", "smtp_host", mcfg.Host, "smtp_port", mcfg.Port)
		}()
	}
	return cfg, nil
}

// kgfConfig reads KGF Cloud context authentication (ADR-0024,
// docs/OPERATIONS.md §24) and attribution (ADR-0025, §25). One kgfctx
// verifier -- keys, audience, user issuers, replay guard -- serves both.
//
//   - NEXUS_AUTH_MODE=kgf or both: a key source is mandatory (a gateway that
//     could verify no context would refuse every KGF request, so a
//     misconfiguration stops startup) and the context AUTHENTICATES; the
//     returned authenticator wraps the verifier.
//   - standalone (the default): Nexus credentials authenticate. With a key
//     source configured, the verifier is returned for audit ATTRIBUTION only;
//     without one, nil, and an X-KGF-Context is refused rather than ignored.
func kgfConfig(ctx context.Context, db store.GWDB, log *slog.Logger) (rest.AuthMode, *authn.KGFAuthenticator, *kgfctx.Verifier, health.Check, error) {
	mode, err := rest.ParseAuthMode(os.Getenv("NEXUS_AUTH_MODE"))
	if err != nil {
		return "", nil, nil, nil, err
	}
	var keys kgfctx.KeySource
	var ready health.Check
	// Key sources are MERGED (ADR-0034): static keys plus one key document per
	// URL in the comma-separated NEXUS_KGF_CONTEXT_KEYS_URL -- kgf-gateway's
	// and each provider front's /.well-known/kgf-context.json.
	static, url := env("NEXUS_KGF_CONTEXT_PUBLIC_KEYS", ""), env("NEXUS_KGF_CONTEXT_KEYS_URL", "")
	urls := kgfctx.ParseKeyURLs(url)
	var sources kgfctx.MultiKeys
	if static != "" {
		sk, err := kgfctx.ParseStaticKeys(static)
		if err != nil || len(sk) == 0 {
			return "", nil, nil, nil, fmt.Errorf("NEXUS_KGF_CONTEXT_PUBLIC_KEYS: %v", err)
		}
		sources = append(sources, sk)
	}
	for _, u := range urls {
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return "", nil, nil, nil, fmt.Errorf("NEXUS_KGF_CONTEXT_KEYS_URL: %q is not an http(s) URL", u)
		}
		rk := kgfctx.NewRemoteKeys(u, &http.Client{Timeout: 5 * time.Second}, log)
		rk.Refresh = time.Duration(envInt("NEXUS_KGF_CONTEXT_KEYS_REFRESH_SEC", 300)) * time.Second
		if err := rk.Ready(ctx); err != nil {
			// Not fatal: KGF's gateway and fronts may start after Nexus.
			// /readyz reports it in kgf/both mode.
			log.Warn("KGF context keys are not available yet; contexts they sign are refused until they are", "url", u, "error", err)
		}
		sources = append(sources, rk)
	}
	switch {
	case len(sources) == 0 && mode == rest.AuthStandalone:
		log.Info("authentication mode: standalone (Nexus credentials only; no KGF attribution keys configured)")
		return mode, nil, nil, nil, nil
	case len(sources) == 0:
		return "", nil, nil, nil, errors.New("NEXUS_AUTH_MODE=" + string(mode) + " needs NEXUS_KGF_CONTEXT_KEYS_URL or NEXUS_KGF_CONTEXT_PUBLIC_KEYS")
	case len(sources) == 1 && static != "":
		keys = sources[0]
	default:
		keys, ready = sources, sources.Ready
	}
	var issuers []string
	for _, i := range strings.Split(env("NEXUS_KGF_USER_ISSUERS", "kgf-gateway"), ",") {
		if i = strings.TrimSpace(i); i != "" {
			issuers = append(issuers, i)
		}
	}
	// Provider fronts (ADR-0034): the only issuers that may name a workspace
	// (`res`). An issuer may not be both a user issuer and a front: a user
	// issuer's contexts must never carry `res`, and a front's must.
	var fronts []string
	for _, i := range strings.Split(env("NEXUS_KGF_FRONT_ISSUERS", ""), ",") {
		if i = strings.TrimSpace(i); i != "" {
			for _, u := range issuers {
				if u == i {
					return "", nil, nil, nil, fmt.Errorf("NEXUS_KGF_FRONT_ISSUERS: %q is also in NEXUS_KGF_USER_ISSUERS; an issuer is one or the other", i)
				}
			}
			fronts = append(fronts, i)
		}
	}
	// NEXUS_KGF_AUDIENCE: one audience, or a comma-separated list when the
	// gateway's contexts ("kgf-nexus") and the KGF.Nexus front's
	// ("kgf-nexus-engine") both reach this deployment.
	var audiences []string
	for _, a := range strings.Split(env("NEXUS_KGF_AUDIENCE", "kgf-nexus"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			if a == kgfctx.AnyAudience {
				return "", nil, nil, nil, errors.New("NEXUS_KGF_AUDIENCE: \"*\" is not an audience; see NEXUS_KGF_ACCEPT_ANY_AUDIENCE")
			}
			audiences = append(audiences, a)
		}
	}
	if len(audiences) == 0 {
		audiences = []string{"kgf-nexus"}
	}
	verifier := kgfctx.Verifier{
		Keys: keys, Audience: audiences[0], Audiences: audiences[1:],
		AcceptAnyAudience: envBool("NEXUS_KGF_ACCEPT_ANY_AUDIENCE", false),
		UserIssuers:       issuers, FrontIssuers: fronts, Seen: kgfctx.NewMemoryReplayGuard(),
	}
	if verifier.AcceptAnyAudience {
		log.Warn("NEXUS_KGF_ACCEPT_ANY_AUDIENCE=true: contexts minted for every KGF service are accepted here")
	}
	if mode == rest.AuthStandalone {
		log.Info("authentication mode: standalone; X-KGF-Context is verified for audit attribution only",
			"audiences", audiences, "user_issuers", issuers, "front_issuers", fronts, "keys_urls", urls, "static_keys", static != "")
		return mode, nil, &verifier, nil, nil
	}
	services, err := authn.ParseServicePrincipals(env("NEXUS_KGF_SERVICE_PRINCIPALS", ""))
	if err != nil {
		return "", nil, nil, nil, fmt.Errorf("NEXUS_KGF_SERVICE_PRINCIPALS: %w", err)
	}
	k := &authn.KGFAuthenticator{
		Verifier:          verifier,
		Provisioner:       authn.StoreProvisioner{DB: db},
		Workspaces:        authn.StoreProvisioner{DB: db},
		ServicePrincipals: services,
	}
	log.Info("authentication mode: KGF context", "mode", string(mode), "audiences", audiences,
		"user_issuers", issuers, "front_issuers", fronts, "keys_urls", urls, "static_keys", static != "",
		"service_principals", len(services))
	return mode, k, nil, ready, nil
}

// envBool reads a boolean, keeping the default on an unparseable value for the
// same reason envFloat does.
func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
