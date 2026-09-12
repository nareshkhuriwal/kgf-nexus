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
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/worker"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
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
		gpuCount = flag.Int("gpu-count", int(envInt("NEXUS_GPU_COUNT", 0)),
			"advertised GPU count")
		gpuKind = flag.String("gpu-kind", envOr("NEXUS_GPU_KIND", ""),
			"GPU model advertised, e.g. nvidia-a100")
		walDir = flag.String("wal-dir", envOr("NEXUS_WAL_DIR", "/var/lib/nexus-agent/wal"),
			"local write-ahead log directory; MUST be on durable storage")
		workDir = flag.String("work-dir", envOr("NEXUS_WORK_DIR", ""),
			"per-attempt scratch directory root (default: OS temp)")
		drainGrace = flag.Duration("drain-grace", envDuration("NEXUS_DRAIN_GRACE", 5*time.Minute),
			"how long a draining agent waits for in-flight work before giving up")
		allowUnenforceable = flag.Bool("allow-unenforceable-limits",
			os.Getenv("NEXUS_ALLOW_UNENFORCEABLE_LIMITS") == "true",
			"DEVELOPMENT ONLY: run tasks whose resource limits the subprocess executor cannot enforce")
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

	cfg := worker.Config{
		Namespace: *namespace,
		Queues:    splitCSV(*queues),
		Labels:    parseLabels(*labelsFlag),
		Capacity: &nexusv1.ResourceVector{
			CpuMillis:   *cpuMillis,
			MemoryBytes: *memoryBytes,
			GpuCount:    int32(*gpuCount),
			GpuKind:     *gpuKind,
		},
		AgentVersion:     version,
		WALDir:           *walDir,
		WorkDir:          *workDir,
		DrainGracePeriod: *drainGrace,
		ResultStore: worker.ResultStoreCredentials{
			AccessKeyID:     os.Getenv("NEXUS_S3_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("NEXUS_S3_SECRET_ACCESS_KEY"),
			SessionToken:    os.Getenv("NEXUS_S3_SESSION_TOKEN"),
			UseTLS:          os.Getenv("NEXUS_S3_USE_TLS") == "true",
		},
		AllowUnenforceableLimits: *allowUnenforceable,
	}

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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var n int64
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
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
