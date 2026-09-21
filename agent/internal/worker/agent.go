// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/heartbeat"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/lease"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/resultstore"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/wal"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Client is the full worker RPC surface, satisfied by the generated gRPC client
// and by test fakes. Declared here rather than imported so the agent depends on a
// behaviour, not on a transport.
type Client interface {
	Register(ctx context.Context, req *nexusv1.RegisterRequest) (*nexusv1.RegisterResponse, error)
	Heartbeat(ctx context.Context, req *nexusv1.HeartbeatRequest) (*nexusv1.HeartbeatResponse, error)
	PollTask(ctx context.Context, req *nexusv1.PollTaskRequest) (*nexusv1.PollTaskResponse, error)
	ReportStarted(ctx context.Context, req *nexusv1.ReportStartedRequest) (*nexusv1.Ack, error)
	ReportProgress(ctx context.Context, req *nexusv1.ReportProgressRequest) (*nexusv1.Ack, error)
	ReportResult(ctx context.Context, req *nexusv1.ReportResultRequest) (*nexusv1.Ack, error)
	ExtendLease(ctx context.Context, req *nexusv1.ExtendLeaseRequest) (*nexusv1.ExtendLeaseResponse, error)
}

// Config is the agent's static configuration. Anything the control plane owns
// (heartbeat interval, lease TTL, result-store location and inline threshold)
// arrives in RegisterResponse and is NOT configured here — a worker that
// disagreed with the control plane about the lease TTL would renew too late and
// lose its work.
type Config struct {
	Namespace    string
	Hostname     string
	Queues       []string
	Labels       map[string]string
	Capacity     *nexusv1.ResourceVector
	AgentVersion string

	WALDir  string
	WorkDir string

	ResultStore ResultStoreCredentials

	// DrainGracePeriod bounds how long a draining agent waits for in-flight work
	// before it stops waiting. A drain that never ends blocks a deploy.
	DrainGracePeriod time.Duration

	// AllowUnenforceableLimits relaxes the subprocess executor's refusal to run
	// tasks whose limits it cannot honour. Development only.
	AllowUnenforceableLimits bool
}

// ResultStoreCredentials are the parts of result-store access the agent holds
// locally. The endpoint, bucket and inline threshold come from the control plane;
// only the credentials are local, because credentials must never be in the
// metadata store (ADR-0008).
type ResultStoreCredentials struct {
	// Endpoint overrides the advertised address for workers outside its network.
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	UseTLS          bool
}

// Agent is the worker agent process.
type Agent struct {
	cfg    Config
	client Client
	log    *slog.Logger

	wal      *wal.WAL
	registry *Registry
	runner   *Runner
	keeper   *lease.Keeper
	hb       *heartbeat.Loop
	puller   *Puller
	store    *resultstore.Client

	workerID     string
	sessionToken string

	// reregister signals the supervising loop that this session is invalid and a
	// fresh registration (a NEW worker_id) is required.
	reregister chan struct{}
	// drainOnce keeps a repeated DRAIN directive — the control plane re-sends it
	// every beat until the worker is gone — from starting the drain repeatedly.
	drainOnce sync.Once
	draining  atomic.Bool
	stopAll   context.CancelFunc
}

// New builds an agent. The executors it supports are constructed here and
// advertised at registration so the scheduler can treat executor support as a
// placement constraint (ADR-0007).
func New(cfg Config, client Client, log *slog.Logger) (*Agent, error) {
	if cfg.Hostname == "" {
		h, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("agent: hostname: %w", err)
		}
		cfg.Hostname = h
	}
	if len(cfg.Queues) == 0 {
		cfg.Queues = []string{"default"}
	}
	if cfg.DrainGracePeriod <= 0 {
		cfg.DrainGracePeriod = 5 * time.Minute
	}

	w, err := wal.Open(cfg.WALDir)
	if err != nil {
		return nil, fmt.Errorf("agent: open WAL: %w", err)
	}
	return &Agent{
		cfg: cfg, client: client, log: log, wal: w,
		registry:   NewRegistry(cfg.Capacity),
		reregister: make(chan struct{}, 1),
	}, nil
}

// WorkerID reports the per-session identity assigned at registration.
func (a *Agent) WorkerID() string { return a.workerID }

// Registry exposes the task registry, for tests and for a status endpoint.
func (a *Agent) Registry() *Registry { return a.registry }

// Run registers, replays the WAL, then runs the heartbeat, lease keeper and
// puller concurrently until ctx is cancelled or a REREGISTER directive arrives.
//
// On REREGISTER it returns ErrReregister rather than re-registering in place: a
// new session means a NEW worker_id, and the old session's leases can never be
// reclaimed. Identity is per-session, not per-host, precisely so a restarted
// agent cannot resurrect stale leases (§5.5) — a genuinely nasty class of bug in
// systems that key worker identity on hostname.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.register(ctx); err != nil {
		return err
	}

	// ── WAL replay, BEFORE any new work ───────────────────────────────────────
	// Ordering matters: a COMPLETED entry re-reported before we start polling
	// lets the control plane finalise the attempt and release its reservation
	// before we ask it for more work, so our own restart does not look like a
	// capacity leak.
	res := wal.Replay(ctx, a.wal, reportFunc(a.client.ReportResult), a.log)
	if n := len(res.Reported); n > 0 {
		a.log.Info("WAL replay complete", "reported", n,
			"indeterminate", len(res.Indeterminate), "rejected", len(res.Rejected),
			"retained", len(res.Retained))
	}

	// Reap containers a previous session left behind. Process-group kills and
	// pdeathsig cover a dying agent; an agent that was SIGKILLed leaves
	// containers the daemon happily keeps running, permanently leaking node
	// capacity the ledger believes is free.
	a.reapOrphans(ctx)

	runCtx, cancel := context.WithCancel(ctx)
	a.stopAll = cancel
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, 3)

	// The heartbeat gets its OWN goroutine and is started FIRST. It must be
	// beating before any task can run: a blocked task must never delay a beat,
	// or the control plane wrongly declares the worker dead and duplicates work.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := a.hb.Run(runCtx); err != nil {
			errs <- fmt.Errorf("heartbeat loop: %w", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := a.keeper.Run(runCtx); err != nil {
			errs <- fmt.Errorf("lease keeper: %w", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := a.puller.Run(runCtx); err != nil {
			errs <- fmt.Errorf("task puller: %w", err)
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
		a.log.Info("shutdown requested; draining")
		a.startDrain(runCtx)
	case <-a.reregister:
		runErr = ErrReregister
		a.log.Warn("re-registration required; ending this session")
	case err := <-errs:
		runErr = err
	}

	a.waitForDrain(runCtx)
	cancel()
	a.keeper.Stop()
	wg.Wait()

	// Last chance to deliver anything the WAL still holds before the process
	// exits, so a clean shutdown never leaves a completed result undelivered.
	final, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	wal.Replay(final, a.wal, reportFunc(a.client.ReportResult), a.log)
	fcancel()

	return runErr
}

// ErrReregister signals that the session is invalid and the supervisor should
// build a fresh agent with a new identity.
var ErrReregister = errors.New("agent: control plane requested re-registration")

// register advertises capacity, queues, labels, supported executors and the
// agent version, and receives the per-session worker_id and session_token.
func (a *Agent) register(ctx context.Context) error {
	execs := a.buildExecutors()
	kinds := make([]string, 0, len(execs))
	for k := range execs {
		kinds = append(kinds, k)
	}

	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	resp, err := a.client.Register(rctx, &nexusv1.RegisterRequest{
		Namespace:    a.cfg.Namespace,
		Hostname:     a.cfg.Hostname,
		Queues:       a.cfg.Queues,
		Labels:       a.cfg.Labels,
		Capacity:     a.cfg.Capacity,
		Executors:    kinds,
		AgentVersion: a.cfg.AgentVersion,
	})
	cancel()
	if err != nil {
		return fmt.Errorf("agent: register: %w", err)
	}
	if resp.GetWorkerId() == "" {
		return errors.New("agent: control plane returned an empty worker_id")
	}
	a.workerID, a.sessionToken = resp.GetWorkerId(), resp.GetSessionToken()
	a.log = a.log.With("worker_id", a.workerID)
	a.log.Info("registered with the control plane",
		"namespace", a.cfg.Namespace, "queues", a.cfg.Queues, "executors", kinds,
		"heartbeat_interval_ms", resp.GetHeartbeatIntervalMs(), "lease_ttl_ms", resp.GetLeaseTtlMs())

	a.initResultStore(resp.GetResultStore())

	// The control plane owns both cadences. A worker that picked its own lease
	// renewal period could renew too late and lose work it is actively running.
	hbInterval := time.Duration(resp.GetHeartbeatIntervalMs()) * time.Millisecond
	ttl := time.Duration(resp.GetLeaseTtlMs()) * time.Millisecond

	reporter := NewReporter(a.client, a.wal, a.log)
	a.keeper = lease.New(a.workerID, a.client, nil, ttl, a.log)
	a.runner = NewRunner(execs, a.registry, a.keeper, reporter, a.wal, a.store, a.log)
	// The keeper revokes into the runner and the runner renews through the
	// keeper, so one of the two edges must be wired after construction.
	a.keeper.SetHandler(a.runner)

	a.hb = heartbeat.New(a.workerID, a.client, a.registry, a, hbInterval, a.log)
	a.puller = NewPuller(a.workerID, a.cfg.Queues, a.client, a.registry, a.keeper, a.runner, a.log)
	return nil
}

// buildExecutors constructs the substrates this worker supports.
//
// Docker is advertised only when a daemon is actually reachable: advertising an
// executor we cannot run turns every task routed here into an INFRASTRUCTURE
// failure, and the scheduler would keep routing them here because we said we
// supported it.
func (a *Agent) buildExecutors() map[string]executor.Executor {
	out := map[string]executor.Executor{
		"subprocess": &executor.SubprocessExecutor{
			Root: a.cfg.WorkDir, Log: a.log,
			AllowUnenforceableLimits: a.cfg.AllowUnenforceableLimits,
		},
	}
	probe, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if executor.DockerAvailable(probe, "") {
		out["docker"] = &executor.DockerExecutor{Root: a.cfg.WorkDir, Log: a.log}
	} else {
		a.log.Warn("docker daemon unreachable; not advertising the docker executor")
	}
	return out
}

func (a *Agent) initResultStore(cfg *nexusv1.ResultStoreConfig) {
	endpoint := a.cfg.ResultStore.Endpoint
	if endpoint == "" {
		endpoint = cfg.GetEndpoint()
	}
	if endpoint == "" {
		a.log.Warn("control plane supplied no result store; outputs will be inlined only")
		return
	}
	c, err := resultstore.New(resultstore.Config{
		Endpoint:        endpoint,
		Bucket:          cfg.GetBucket(),
		Region:          cfg.GetRegion(),
		Namespace:       a.cfg.Namespace,
		AccessKeyID:     a.cfg.ResultStore.AccessKeyID,
		SecretAccessKey: a.cfg.ResultStore.SecretAccessKey,
		SessionToken:    a.cfg.ResultStore.SessionToken,
		UseTLS:          a.cfg.ResultStore.UseTLS,
		InlineThreshold: cfg.GetInlineThresholdBytes(),
		Log:             a.log,
	})
	if err != nil {
		// Not fatal. A worker that refuses to boot because object storage is
		// briefly unavailable silently shrinks the fleet; a worker that boots and
		// fails individual tasks with INFRASTRUCTURE is visible and recoverable.
		a.log.Error("could not build the result store client; tasks needing blobs will fail", "error", err)
		return
	}
	a.store = c
	if err := c.Reachable(context.Background()); err != nil {
		a.log.Warn("result store is not currently reachable", "error", err)
	}
}

func (a *Agent) reapOrphans(ctx context.Context) {
	d, ok := a.buildExecutors()["docker"]
	if !ok {
		return
	}
	de, ok := d.(*executor.DockerExecutor)
	if !ok {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if n, err := de.ReapOrphans(rctx); err != nil {
		a.log.Warn("could not reap orphaned containers", "error", err)
	} else if n > 0 {
		a.log.Warn("reaped orphaned containers from a previous session", "count", n)
	}
}

// ── heartbeat.DirectiveHandler ───────────────────────────────────────────────
// These MUST return promptly: they are called from the heartbeat goroutine, and
// blocking one would delay the next beat and risk a false death declaration.

// OnDrain implements the DRAINING transition from §5.5: stop accepting new work,
// let current work finish, then exit.
func (a *Agent) OnDrain(ctx context.Context) {
	a.drainOnce.Do(func() {
		a.log.Info("entering DRAINING: no new work will be accepted; current work will finish")
		a.draining.Store(true)
		a.registry.SetDraining(true)
		go a.startDrain(ctx)
	})
}

// OnCancelAttempt tears one attempt down on the SIGTERM → grace → SIGKILL
// contract. Returns immediately; the teardown is asynchronous.
func (a *Agent) OnCancelAttempt(ctx context.Context, attemptID string, grace time.Duration) {
	if grace <= 0 {
		grace = executor.DefaultGraceAfterTerm
	}
	a.runner.Cancel(attemptID, grace, "control_plane_cancel")
}

// OnReregister ends this session. Non-blocking: the channel is buffered and a
// repeated directive is dropped rather than queued.
func (a *Agent) OnReregister(ctx context.Context) {
	select {
	case a.reregister <- struct{}{}:
	default:
	}
}

func (a *Agent) startDrain(ctx context.Context) {
	a.draining.Store(true)
	a.registry.SetDraining(true)
}

// waitForDrain blocks until in-flight work finishes or the grace period expires.
//
// The grace period is a bound, not a promise: a drain that waits forever blocks a
// deploy, and the tasks that survive it become LOST via lease expiry and are
// retried, which is a cost the deploy can afford. Waiting some bounded time first
// is what makes a routine deploy NOT produce a burst of LOST attempts.
func (a *Agent) waitForDrain(ctx context.Context) {
	if a.registry.Idle() {
		return
	}
	deadline := time.Now().Add(a.cfg.DrainGracePeriod)
	a.log.Info("waiting for in-flight attempts to finish",
		"in_flight", a.registry.Count(), "grace_period", a.cfg.DrainGracePeriod)

	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if a.registry.Idle() {
				a.log.Info("all in-flight attempts finished; drain complete")
				return
			}
			if time.Now().After(deadline) {
				a.log.Warn("drain grace period expired; remaining attempts will become LOST via lease expiry",
					"still_running", a.registry.Count())
				return
			}
		case <-ctx.Done():
			// The parent context is already cancelled during shutdown, so a
			// cancelled context here means a hard stop, not a drain.
			if ctx.Err() != nil && a.registry.Count() > 0 {
				a.log.Warn("hard stop with attempts still running",
					"still_running", a.registry.Count())
			}
			return
		}
	}
}

// reportFunc adapts a bare method value to wal.Reporter.
type reportFunc func(ctx context.Context, req *nexusv1.ReportResultRequest) (*nexusv1.Ack, error)

func (f reportFunc) ReportResult(ctx context.Context, req *nexusv1.ReportResultRequest) (*nexusv1.Ack, error) {
	return f(ctx, req)
}
