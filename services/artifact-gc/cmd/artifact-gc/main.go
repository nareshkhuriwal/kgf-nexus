// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Command artifact-gc is the operator-run reachability-keyed garbage collector
// for the result store (ADR-0008).
//
// It reports, and by default does not delete:
//
//	# what the default does: examine, report, change nothing
//	artifact-gc
//
//	# the same, scoped to one namespace prefix, with a longer grace period
//	artifact-gc -prefix default/ -grace 72h
//
//	# actually delete. Requires BOTH flags; neither alone is enough.
//	artifact-gc -delete -i-understand-this-deletes-data
//
// The double flag is not theatre. ADR-0008 calls out that "a bug in an inline
// delete path destroys data", and the same is true of a fat-fingered command.
// `-delete` alone is the kind of flag that ends up in a shell history and then in
// a cron entry; pairing it with a flag nobody types by accident means the
// destructive mode cannot be reached without having read this paragraph.
//
// There is deliberately NO daemon mode here. artifactgc.Sweeper has a Run loop
// for the day ADR-0008's background reconciler is turned on, but nothing in this
// repository starts it, and this command runs exactly one pass and exits.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khuriwalgroup/kgf-nexus/internal/artifactgc"
	"github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

var version = "dev"

func main() {
	var (
		dsn = flag.String("database-url",
			config.DSN("database-url"),
			"Postgres DSN (the reference set is derived from here)")
		endpoint = flag.String("endpoint",
			firstEnv("RESULT_STORE_ENDPOINT", "NEXUS_RESULT_STORE_ENDPOINT", "NEXUS_S3_ENDPOINT"),
			"result-store endpoint, e.g. http://localhost:9010")
		bucket = flag.String("bucket",
			envOr("RESULT_STORE_BUCKET", envOr("NEXUS_RESULT_BUCKET", "nexus-results")),
			"result-store bucket")
		region = flag.String("region", envOr("RESULT_STORE_REGION", "us-east-1"), "result-store region")
		access = flag.String("access-key",
			envOr("RESULT_STORE_ACCESS_KEY", envOr("NEXUS_S3_ACCESS_KEY_ID", envOr("MINIO_ROOT_USER", "nexusadmin"))),
			"result-store access key")
		secret = flag.String("secret-key",
			envOr("RESULT_STORE_SECRET_KEY", envOr("NEXUS_S3_SECRET_ACCESS_KEY", envOr("MINIO_ROOT_PASSWORD", "nexus-dev-password"))),
			"result-store secret key")
		useTLS = flag.Bool("tls", envBool("NEXUS_S3_USE_TLS", false), "use TLS to reach the result store")

		prefix = flag.String("prefix", "", "only sweep keys under this prefix, e.g. \"default/\"")
		grace  = flag.Duration("grace", envDuration("ARTIFACT_GC_GRACE", artifactgc.DefaultGracePeriod),
			"minimum object age before it may be collected; covers objects written by an attempt whose row has not committed")
		maxObjects = flag.Int("max-objects", envInt("ARTIFACT_GC_MAX_OBJECTS", artifactgc.DefaultMaxObjects),
			"maximum objects examined in this pass")

		doDelete = flag.Bool("delete", false,
			"actually delete orphans (requires -i-understand-this-deletes-data); the default reports only")
		confirm = flag.Bool("i-understand-this-deletes-data", false,
			"second confirmation required alongside -delete")

		// The OPPOSITE direction from the sweep, and read-only by construction:
		// rows naming an object the store does not have. It shares none of the
		// delete guards because it has nothing to guard -- see dangling.go for
		// why there is no repair mode.
		checkRefs = flag.Bool("check-refs", false,
			"instead of sweeping, report rows whose object is MISSING from the store (read-only)")
		maxRefs = flag.Int("max-refs", envInt("ARTIFACT_GC_MAX_REFS", artifactgc.DefaultMaxRefs),
			"maximum distinct object keys checked by -check-refs")

		// Batch-job metrics (see metrics.go): push at the end, and/or serve
		// while the pass runs. Both off by default.
		metricsAddr = flag.String("metrics-addr", envOr("ARTIFACT_GC_METRICS_ADDR", ""),
			"serve /metrics on this address while the pass runs (e.g. 127.0.0.1:9469); empty = off")
		pushURL = flag.String("metrics-pushgateway", envOr("ARTIFACT_GC_PUSHGATEWAY_URL", ""),
			"push final metrics to this Prometheus Pushgateway URL under job=artifact-gc; empty = off")

		logLevel = flag.String("log-level", envOr("ARTIFACT_GC_LOG_LEVEL", "info"), "debug|info|warn|error")
		showVer  = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}

	log := newLogger(*logLevel).With("command", "artifact-gc", "version", version)
	slog.SetDefault(log)

	// Dry run is the default and stays the default unless BOTH flags are given.
	dryRun := true
	switch {
	case *doDelete && *confirm:
		dryRun = false
	case *doDelete && !*confirm:
		fmt.Fprintln(os.Stderr,
			"artifact-gc: -delete also requires -i-understand-this-deletes-data.\n"+
				"artifact-gc: this permanently removes objects from the result store.\n"+
				"artifact-gc: run without -delete first and read the report.")
		os.Exit(2)
	case !*doDelete && *confirm:
		log.Warn("-i-understand-this-deletes-data was given without -delete; staying in dry run")
	}

	if *endpoint == "" {
		fmt.Fprintln(os.Stderr,
			"artifact-gc: no result-store endpoint. Set RESULT_STORE_ENDPOINT or pass -endpoint\n"+
				"artifact-gc: (the compose stack publishes MinIO on http://localhost:9010).")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	gm := newGCMetrics()
	stopMetrics, err := gm.serve(*metricsAddr, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "artifact-gc: %v\n", err)
		os.Exit(2)
	}
	runErr := run(ctx, log, runConfig{
		dsn: *dsn, prefix: *prefix, grace: *grace, maxObjects: *maxObjects, dryRun: dryRun,
		checkRefs: *checkRefs, maxRefs: *maxRefs,
		minio: artifactgc.MinIOConfig{
			Endpoint: *endpoint, Bucket: *bucket, Region: *region,
			AccessKeyID: *access, SecretAccessKey: *secret, UseTLS: *useTLS,
		},
		metrics: gm,
	})
	gm.finish(runErr)
	gm.push(*pushURL, log)
	stopMetrics()
	if runErr != nil {
		log.Error("artifact-gc failed", "error", runErr)
		os.Exit(1)
	}
}

type runConfig struct {
	dsn        string
	prefix     string
	grace      time.Duration
	maxObjects int
	dryRun     bool
	checkRefs  bool
	maxRefs    int
	minio      artifactgc.MinIOConfig
	metrics    *gcMetrics
}

func run(ctx context.Context, log *slog.Logger, cfg runConfig) error {
	pool, err := store.NewPool(ctx, store.PoolConfig{
		DSN: cfg.dsn, MaxConns: 4, MinConns: 1,
		MaxConnLifetime: 5 * time.Minute, MaxConnIdleTime: time.Minute,
	})
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	objects, err := artifactgc.NewMinIOStore(ctx, cfg.minio)
	if err != nil {
		return err
	}

	if cfg.checkRefs {
		return checkDangling(ctx, pool, objects, log, cfg)
	}

	sw := artifactgc.NewSweeper(pool, objects, log, artifactgc.Config{
		DryRun:      cfg.dryRun,
		GracePeriod: cfg.grace,
		MaxObjects:  cfg.maxObjects,
		Prefix:      cfg.prefix,
	})

	mode := "DRY RUN (nothing will be deleted)"
	if !cfg.dryRun {
		mode = "DELETE (orphans will be permanently removed)"
	}
	log.Info("artifact GC starting",
		"mode", mode,
		"endpoint", cfg.minio.Endpoint,
		"bucket", objects.Bucket(),
		"prefix", cfg.prefix,
		"grace_period", sw.Config().GracePeriod,
		"max_objects", sw.Config().MaxObjects,
		"reference_sources", artifactgc.RefSources())

	st, err := sw.Once(ctx)
	if err != nil {
		return err
	}
	cfg.metrics.sweep(st)

	// A human-readable summary on stdout as well as the structured log: this is a
	// command an operator reads, not a service whose logs get shipped.
	fmt.Printf("\nartifact-gc summary (%s)\n", mode)
	fmt.Printf("  objects examined      %d\n", st.Listed)
	fmt.Printf("  live references       %d  (from %v)\n", st.LiveRefs, artifactgc.RefSources())
	fmt.Printf("  reachable (kept)      %d\n", st.Reachable)
	fmt.Printf("  too young (kept)      %d  (grace %s)\n", st.TooYoung, sw.Config().GracePeriod)
	fmt.Printf("  collectable orphans   %d  (%d bytes)\n", st.Orphans, st.OrphanBytes)
	fmt.Printf("  deleted               %d\n", st.Deleted)
	if st.DeleteErrors > 0 {
		fmt.Printf("  delete errors         %d\n", st.DeleteErrors)
	}
	if st.DryRun && st.Orphans > 0 {
		fmt.Printf("\n  Nothing was deleted. To collect these, re-run with:\n")
		fmt.Printf("    -delete -i-understand-this-deletes-data\n")
	}
	return nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// checkDangling reports rows whose object is missing from the store.
//
// Exits NONZERO when any are found, so this is usable as a monitoring check.
// The backup ordering (OPERATIONS §7.2) is designed to produce orphans and
// never dangling refs, so a nonzero result is a real fault — a restore run out
// of order, an out-of-band delete, or a GC sweep that removed something it
// should not have — and not a condition to be tolerated on a schedule.
func checkDangling(
	ctx context.Context, pool *pgxpool.Pool, objects *artifactgc.MinIOStore,
	log *slog.Logger, cfg runConfig,
) error {
	log.Info("checking for dangling references (READ-ONLY; nothing will be modified)",
		"endpoint", cfg.minio.Endpoint, "bucket", objects.Bucket(),
		"max_refs", cfg.maxRefs, "reference_sources", artifactgc.RefSources())

	rep, err := artifactgc.CheckDanglingRefs(ctx, pool, objects, log, cfg.maxRefs)
	if err != nil {
		return err
	}
	cfg.metrics.check(rep)

	fmt.Printf("\nartifact-gc dangling-reference check (READ-ONLY)\n")
	fmt.Printf("  rows scanned          %d  (from %v)\n", rep.RowsScanned, artifactgc.RefSources())
	fmt.Printf("  distinct keys checked %d\n", rep.RefsChecked)
	fmt.Printf("  missing objects       %d\n", rep.MissingKeys)
	fmt.Printf("  damaged rows          %d\n", len(rep.Dangling))
	if rep.Truncated {
		fmt.Printf("\n  NOTE: stopped at -max-refs=%d; more references exist and were NOT checked.\n",
			cfg.maxRefs)
	}
	if len(rep.Dangling) == 0 {
		fmt.Printf("\n  Every reference resolves to an object that exists.\n")
		return nil
	}

	fmt.Printf("\n  These rows name an object the store does not have. The result is\n")
	fmt.Printf("  UNREADABLE, and any downstream task mapping it as an input will fail:\n\n")
	for _, d := range rep.Dangling {
		fmt.Printf("    run %s node %-16s %s.%s\n      key %s\n",
			d.RunID, d.NodeID, d.Table, d.Column, d.Key)
	}
	fmt.Printf("\n  Nothing was changed. There is no automatic repair: the object is gone,\n")
	fmt.Printf("  and rewriting the row would destroy the evidence of what happened.\n")

	return fmt.Errorf("%d dangling reference(s) across %d missing object(s)",
		len(rep.Dangling), rep.MissingKeys)
}
