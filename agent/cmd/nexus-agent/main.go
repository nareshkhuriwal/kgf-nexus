// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Command nexus-agent is the Nexus worker agent (architecture §3.3).
//
// It is deliberately small: register, prove liveness, obtain leased work,
// execute it under limits, report the outcome durably. Every scheduling,
// retry and policy decision lives in the control plane, which is what lets a
// worker be a static binary with no runtime dependency that can run behind NAT,
// in another VPC, or on a laptop.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/agentmetrics"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/worker"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/bus"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
)

// version is stamped at build time (-ldflags "-X main.version=..."). It is
// advertised at registration so an operator can correlate a behavioural change
// with a specific agent build across a partially-upgraded fleet.
var version = "dev"

func main() {
	var (
		controlPlane = flag.String("control-plane", envOr("NEXUS_CONTROL_PLANE", "localhost:50051"),
			"worker gRPC endpoint of the control plane")
		namespace = flag.String("namespace", envOr("NEXUS_NAMESPACE", "default"),
			"namespace this worker serves; also the result-store key prefix and tenancy boundary")
		queues = flag.String("queues", envOr("NEXUS_QUEUES", "default"),
			"comma-separated queues to serve")
		labelsFlag = flag.String("labels", envOr("NEXUS_LABELS", ""),
			"comma-separated k=v worker labels used as hard placement constraints")
		cpuMillis = flag.Int64("cpu-millis", envInt("NEXUS_CPU_MILLIS", int64(runtime.NumCPU())*1000),
			"advertised CPU capacity in millicores")
		memoryBytes = flag.Int64("memory-bytes", envInt("NEXUS_MEMORY_BYTES", 2<<30),
			"advertised memory capacity in bytes")
		diskBytes = flag.Int64("disk-bytes", envInt("NEXUS_DISK_BYTES", 0),
			"advertised scratch-disk capacity in bytes; 0 means probe the work directory's filesystem")
		gpuCount = flag.Int("gpu-count", int(envInt("NEXUS_GPU_COUNT", 0)),
			"advertised GPU count")
		gpuKind = flag.String("gpu-kind", envOr("NEXUS_GPU_KIND", ""),
			"GPU model advertised, e.g. nvidia-a100")
		// Per-device VRAM. The scheduler's fit check enforces a task's
		// gpu_memory_bytes floor against this, so leaving it unset advertises 0
		// and makes every task that names a VRAM requirement permanently
		// unschedulable -- correctly refused, but for a reason no operator would
		// guess from a worker that plainly has GPUs.
		gpuMemoryBytes = flag.Int64("gpu-memory-bytes", envInt("NEXUS_GPU_MEMORY_BYTES", 0),
			"advertised per-device GPU memory in bytes; required for tasks that set gpu_memory_bytes")
		// Countable pooled resources the platform knows nothing about by name --
		// license tokens, NIC slots. The scheduler treats them exactly like CPU
		// (§5.3 fit check, decremented on dispatch), so a task requesting one it
		// cannot get stays PENDING with a structured reason.
		customFlag = flag.String("custom-resources", envOr("NEXUS_CUSTOM_RESOURCES", ""),
			"comma-separated k=n countable custom capacity, e.g. license_token=2")
		walDir = flag.String("wal-dir", envOr("NEXUS_WAL_DIR", "/var/lib/nexus-agent/wal"),
			"local write-ahead log directory; MUST be on durable storage")
		workDir = flag.String("work-dir", envOr("NEXUS_WORK_DIR", ""),
			"per-attempt scratch directory root (default: OS temp)")
		drainGrace = flag.Duration("drain-grace", envDuration("NEXUS_DRAIN_GRACE", 5*time.Minute),
			"how long a draining agent waits for in-flight work before giving up")
		maxUninterruptible = flag.Duration("max-uninterruptible",
			envDuration("NEXUS_MAX_UNINTERRUPTIBLE", worker.DefaultUninterruptibleCeiling),
			"platform ceiling on how long cancelling an uninterruptible task waits before SIGKILL (ADR-0015)")
		allowUnenforceable = flag.Bool("allow-unenforceable-limits",
			os.Getenv("NEXUS_ALLOW_UNENFORCEABLE_LIMITS") == "true",
			"DEVELOPMENT ONLY: run tasks whose resource limits the subprocess executor cannot enforce")
		// Loopback by default: an agent on a laptop or a shared host should not
		// publish its metrics to the LAN unasked. Deployments that scrape it set
		// NEXUS_METRICS_ADDR=:8089 (compose, Helm). "off" disables it.
		metricsAddr = flag.String("metrics-addr", envOr("NEXUS_METRICS_ADDR", "127.0.0.1:8089"),
			"address for /metrics and /healthz; \"off\" disables")
		logLevel  = flag.String("log-level", envOr("NEXUS_LOG_LEVEL", "info"), "debug|info|warn|error")
		showVer   = flag.Bool("version", false, "print the agent version and exit")
		logFormat = flag.String("log-format", envOr("NEXUS_LOG_FORMAT", "json"), "json|text")
	)
	flag.Parse()

	if *showVer {
		fmt.Println(version)
		return
	}

	log := newLogger(*logLevel, *logFormat)
	slog.SetDefault(log)

	// SIGTERM is the deploy signal, so it must start a DRAIN, not a kill: an
	// agent that dropped its tasks on SIGTERM would turn every routine deploy
	// into a burst of LOST attempts and duplicated work.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// NEXUS_NATS_*: empty URL means poll-only, which is a complete, correct
	// configuration (ADR-0003). A configuration the bus REJECTS -- credentials
	// set but unreadable, none where NEXUS_NATS_REQUIRE_AUTH or NEXUS_ENV=
	// production demands them -- is fatal, like a malformed NEXUS_K8S_*: an
	// agent quietly running without hints would hide the deployment bug.
	busCfg, err := bus.ConfigFromEnv("nexus-agent")
	if err != nil {
		log.Error("invalid NATS configuration", "error", err)
		os.Exit(1)
	}
	if bus.RequireAuthOverridden() {
		log.Warn("NEXUS_NATS_REQUIRE_AUTH=false overrides NEXUS_ENV: dispatch hints may be received without credentials")
	}

	cfg := worker.Config{
		Namespace: *namespace,
		Queues:    splitCSV(*queues),
		Labels:    parseLabels(*labelsFlag),
		Capacity: &nexusv1.ResourceVector{
			CpuMillis:      *cpuMillis,
			MemoryBytes:    *memoryBytes,
			DiskBytes:      advertisedDisk(*diskBytes, *workDir, log),
			GpuCount:       int32(*gpuCount),
			GpuKind:        *gpuKind,
			GpuMemoryBytes: *gpuMemoryBytes,
			Custom:         parseCustom(*customFlag, log),
		},
		AgentVersion:     version,
		WALDir:           *walDir,
		WorkDir:          *workDir,
		DrainGracePeriod: *drainGrace,
		ResultStore: worker.ResultStoreCredentials{
			Endpoint:        os.Getenv("NEXUS_RESULT_STORE_ENDPOINT"),
			AccessKeyID:     os.Getenv("NEXUS_S3_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("NEXUS_S3_SECRET_ACCESS_KEY"),
			SessionToken:    os.Getenv("NEXUS_S3_SESSION_TOKEN"),
			UseTLS:          os.Getenv("NEXUS_S3_USE_TLS") == "true",
		},
		AllowUnenforceableLimits: *allowUnenforceable,
		Bus:                      busCfg,
		MaxUninterruptible:       *maxUninterruptible,
	}

	// The kubernetes executor is opt-in: it exists only when NEXUS_K8S_NAMESPACE
	// names a target namespace. It is built ONCE here, outside the session loop,
	// because it owns informers that must not be re-created on every REREGISTER.
	kcfg, kEnabled, err := executor.LoadKubernetesConfigFromEnv(os.Getenv)
	if err != nil {
		// A malformed NEXUS_K8S_* is a deployment bug; failing fast beats an
		// agent that silently advertises less than its operator intended.
		log.Error("invalid kubernetes executor configuration", "error", err)
		os.Exit(1)
	}
	if kEnabled {
		client, cerr := executor.NewKubernetesClient(kcfg)
		if cerr != nil {
			// Not fatal: the agent still serves subprocess/docker work, and
			// buildExecutors will simply not advertise kubernetes.
			log.Error("could not build the kubernetes client; the kubernetes executor is disabled", "error", cerr)
		}
		ke := executor.NewKubernetesExecutor(kcfg, client, *workDir, log)
		defer ke.Close()
		cfg.Kubernetes = ke
		log.Info("kubernetes executor configured", "namespace", kcfg.Namespace,
			"owner", ke.Config.Owner, "helper_image", ke.Config.HelperImage)
	}

	// A drain that gives up before the uninterruptible ceiling would kill, at
	// deploy time, exactly the critical sections the ceiling promises to let
	// finish. Not fatal -- an operator may accept that -- but never silent.
	if *drainGrace < *maxUninterruptible {
		log.Warn("drain grace is shorter than the uninterruptible ceiling: a deploy can SIGKILL an "+
			"uninterruptible task before its declared bound (ADR-0015)",
			"drain_grace", *drainGrace, "max_uninterruptible", *maxUninterruptible)
	}

	// Process-wide metrics: built once, outside the session loop, so counters
	// survive a REREGISTER (see agentmetrics).
	reg := metrics.NewServiceRegistry("agent", version)
	cfg.Metrics = agentmetrics.New(reg)
	stopMetrics := serveMetrics(*metricsAddr, reg, log)
	defer stopMetrics()

	if err := run(ctx, cfg, *controlPlane, log); err != nil {
		log.Error("agent exiting with an error", "error", err)
		os.Exit(1)
	}
	log.Info("agent exited cleanly")
}

// run supervises agent sessions.
//
// A REREGISTER directive ends the session and starts a NEW one with a NEW
// worker_id. It does not re-register in place: identity is per-session, not
// per-host, so a fresh session can never reclaim the previous session's leases
// (§5.5). Systems that key worker identity on hostname get this wrong and let a
// restarted agent resurrect stale leases.
func run(ctx context.Context, cfg worker.Config, target string, log *slog.Logger) error {
	for {
		client, err := worker.DialControlPlane(ctx, target)
		if err != nil {
			return err
		}
		// Bootstrap credential for Register, which cannot present a session token
		// because obtaining one is the purpose of the call.
		client.WithAPIKey(envOr("NEXUS_API_KEY", ""))

		a, err := worker.New(cfg, client, log)
		if err != nil {
			client.Close()
			return err
		}

		log.Info("starting agent session", "control_plane", target,
			"agent_version", version, "wal_dir", cfg.WALDir)
		err = a.Run(ctx)
		client.Close()

		if errors.Is(err, worker.ErrReregister) && ctx.Err() == nil {
			log.Warn("session invalidated; re-registering with a new identity")
			// Brief pause: an immediate reconnect against a control plane that
			// is rejecting sessions would hot-loop the whole fleet against it.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
			continue
		}
		return err
	}
}

// serveMetrics starts the agent's metrics listener. A failure to bind is a
// WARNING, not fatal: refusing to run work because the metrics port is taken
// would shrink the fleet over an observability problem.
func serveMetrics(addr string, reg *metrics.Registry, log *slog.Logger) func() {
	ops, err := metrics.ListenOps(addr, metrics.OpsMux(reg, func(mux *http.ServeMux) {
		// Liveness only: the agent has no dependency whose loss should restart
		// it (a control-plane outage is waited out, never restarted through).
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok"))
		})
	}), log)
	if err != nil {
		log.Warn("metrics listener disabled: could not bind", "addr", addr, "error", err)
		return func() {}
	}
	go func() {
		if err := ops.Serve(); err != nil {
			log.Warn("metrics listener stopped", "error", err)
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = ops.Shutdown(ctx)
	}
}

func newLogger(level, format string) *slog.Logger {
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
	opts := &slog.HandlerOptions{Level: lv}
	if strings.EqualFold(format, "text") {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

// advertisedDisk resolves the scratch-disk capacity this agent advertises.
//
// An explicit --disk-bytes wins. Otherwise the work directory's filesystem is
// probed, because advertising ZERO disk is not the neutral default it looks
// like: the scheduler's fit check is a per-dimension comparison, so a worker
// with disk_bytes=0 can never satisfy a task that declares ANY disk, and the
// attempt stays PENDING for the life of the run. That failure is silent and
// reads exactly like a capacity shortage, which is the hardest kind of stall to
// diagnose from the console.
//
// Probing reports the filesystem's TOTAL size rather than its free space
// deliberately. Capacity advertised for scheduling is a statement about how big
// this worker is, not about how full it happens to be right now; free space
// moves with every task and would make placement decisions oscillate. Space
// actually available at run time is the executor's concern (ADR-0007), and the
// gap between advertised and enforced is documented as such.
func advertisedDisk(explicit int64, workDir string, log *slog.Logger) int64 {
	if explicit > 0 {
		return explicit
	}
	probe := workDir
	if probe == "" {
		probe = os.TempDir()
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(probe, &st); err != nil {
		// Not fatal: a worker with no disk dimension is still useful for every
		// task that declares none. Logged at WARN because the consequence is a
		// whole class of task that silently cannot be placed here.
		log.Warn("could not probe disk capacity; advertising no disk, so tasks declaring disk cannot be placed on this worker",
			"path", probe, "error", err,
			"remedy", "set NEXUS_DISK_BYTES")
		return 0
	}
	total := int64(st.Blocks) * int64(st.Bsize)
	log.Info("advertising probed disk capacity",
		"path", probe, "disk_bytes", total,
		"why", "advertising zero would make every task that declares disk unschedulable here")
	return total
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseLabels(s string) map[string]string {
	out := make(map[string]string)
	for _, kv := range splitCSV(s) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// parseCustom reads "k=n,k2=n2" into the countable custom capacity map.
//
// A malformed entry is logged and skipped rather than fatal: dropping one
// mis-typed dimension leaves the agent serving every other kind of work, while
// refusing to start takes a whole worker out of the fleet over a typo.
func parseCustom(s string, log *slog.Logger) map[string]int64 {
	out := make(map[string]int64)
	for _, kv := range splitCSV(s) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			log.Warn("ignoring malformed custom resource; want k=n", "entry", kv)
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			log.Warn("ignoring custom resource with a non-numeric quantity",
				"entry", kv, "error", err)
			continue
		}
		out[strings.TrimSpace(k)] = n
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt reads a whole-number setting, and REFUSES to start on a malformed one.
//
// These are capacity figures: a value the agent cannot read must stop it, not
// be guessed at. The previous fmt.Sscanf("%d") read "1.7179869184e+10" -- a
// byte count a templating layer had rendered in float notation -- as 1 and
// reported no error, so the agent registered 1 byte of memory and the whole
// fleet sat idle with every task "waiting_for_resources". A malformed value
// that silently became the default would hide the same mistake just as well.
func envInt(key string, def int64) int64 {
	n, err := parseEnvInt(key, os.Getenv(key), def)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nexus-agent: %v\n", err)
		os.Exit(2)
	}
	return n
}

// parseEnvInt is envInt's decision, separated so it can be tested without
// exiting: empty means the default; anything else must be a whole number.
func parseEnvInt(key, raw string, def int64) (int64, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a whole number (write plain digits, e.g. 17179869184, not 1.7179869184e+10)", key, raw)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
