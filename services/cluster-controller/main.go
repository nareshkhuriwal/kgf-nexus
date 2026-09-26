// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Command cluster-controller is the narrowly scoped in-cluster controller that
// reports Kubernetes state to Nexus and scales worker Deployments from queued
// demand (docs/CLUSTER-SCALING.md).
//
// It has NO database credential: everything goes through the resource
// manager's ClusterControllerService. It runs in plan (dry-run) mode unless the
// cluster's registry entry says apply AND this process was started with
// NEXUS_CONTROLLER_ALLOW_APPLY=true AND the observed cluster identity matches
// the registered one. Replicas elect a leader through a Kubernetes Lease; only
// the leader reconciles.
//
//	docker build -f deploy/Dockerfile.service --build-arg SERVICE=cluster-controller .
package main

import (
	"github.com/khuriwalgroup/kgf-nexus/internal/tracing"

	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/transport"
	"github.com/khuriwalgroup/kgf-nexus/services/cluster-controller/internal/controller"
	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var version = "dev"

func main() {
	var (
		clusterName = flag.String("cluster", os.Getenv("NEXUS_CLUSTER_NAME"), "registry name of this cluster (clusters.name)")
		rmAddr      = flag.String("resource-manager", config.Addr("resource-manager:50052", "NEXUS_RM_ADDR"), "resource-manager gRPC address")
		httpAddr    = flag.String("http-addr", envOr("CONTROLLER_HTTP_ADDR", ":8090"), "health endpoint listen address")
		identity    = flag.String("identity", envOr("POD_NAME", hostname()), "leader-election identity (the pod name in-cluster)")
		leaseNS     = flag.String("lease-namespace", envOr("POD_NAMESPACE", "kgf-nexus"), "namespace holding the leader-election Lease")
		allowApply  = flag.Bool("allow-apply", envBool("NEXUS_CONTROLLER_ALLOW_APPLY"), "process-level permission to mutate the cluster; the registry must ALSO say apply")
		interval    = flag.Duration("interval", envDuration("NEXUS_CONTROLLER_INTERVAL", 15*time.Second), "reconcile period")
		jobSelector = flag.String("job-selector", envOr("NEXUS_JOB_LABEL_SELECTOR", "nexus.io/attempt-id"), "label selector for task Jobs to report")
		kubeconfig  = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "kubeconfig path for out-of-cluster runs; in-cluster config otherwise")
		logLevel    = flag.String("log-level", envOr("CONTROLLER_LOG_LEVEL", "info"), "debug|info|warn|error")
		showVer     = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}
	log := newLogger(*logLevel).With("service", "cluster-controller", "version", version)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Tracing (ADR-0026): off unless OTEL_* configures an exporter; flushed last.
	defer tracing.SetupOrExit(ctx, "cluster-controller", version, log)()

	err := run(ctx, log, runConfig{
		cluster: *clusterName, rmAddr: *rmAddr, httpAddr: *httpAddr, identity: *identity, leaseNS: *leaseNS,
		allowApply: *allowApply, interval: *interval, jobSelector: *jobSelector, kubeconfig: *kubeconfig,
	})
	if err != nil {
		log.Error("cluster-controller exiting with an error", "error", err)
		os.Exit(1)
	}
	log.Info("cluster-controller exited cleanly")
}

type runConfig struct {
	cluster, rmAddr, httpAddr, identity, leaseNS, jobSelector, kubeconfig string
	allowApply                                                            bool
	interval                                                              time.Duration
}

func run(ctx context.Context, log *slog.Logger, cfg runConfig) error {
	if cfg.cluster == "" {
		return errors.New("NEXUS_CLUSTER_NAME (or -cluster) is required")
	}
	restCfg, err := kubeConfig(cfg.kubeconfig)
	if err != nil {
		return err
	}
	// Modest client-side rate limits: this controller makes a few calls per
	// cycle and must never be the reason an API server throttles others.
	restCfg.QPS, restCfg.Burst = 10, 20
	restCfg.UserAgent = "kgf-nexus-cluster-controller/" + version
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("build kubernetes client: %w", err)
	}

	creds, err := transport.DialOption()
	if err != nil {
		return fmt.Errorf("resolve TLS dial options: %w", err)
	}
	conn, err := grpc.NewClient(config.PeerTarget(cfg.rmAddr), append(config.PeerDialOptions(), creds)...)
	if err != nil {
		return fmt.Errorf("dial resource manager %s: %w", cfg.rmAddr, err)
	}
	defer conn.Close()

	ctl, err := controller.New(controller.Config{
		ClusterName: cfg.cluster, Identity: cfg.identity, AllowApply: cfg.allowApply,
		Interval: cfg.interval, JobLabelSelector: cfg.jobSelector,
	}, kube, rmv1.NewClusterControllerServiceClient(conn), log)
	if err != nil {
		return err
	}
	if cfg.allowApply {
		log.Warn("apply permitted by this process; the registry entry must also be in apply mode with a verified cluster UID before anything is changed")
	} else {
		log.Info("plan-only process: nothing in the cluster will be changed regardless of the registry mode")
	}

	var leading controller.Leading
	// Metrics, served on the health listener (docs/OPERATIONS.md).
	reg := metrics.NewServiceRegistry("cluster-controller", version)
	transport.RegisterMetrics(reg)
	ctl.SetMetrics(controller.NewMetrics(reg))
	reg.NewGaugeFunc("nexus_controller_leader",
		"1 while this replica holds the leader-election Lease and reconciles; 0 on a standby.",
		func() float64 {
			if leading.Is() {
				return 1
			}
			return 0
		})
	httpSrv := healthServer(cfg.httpAddr, ctl, &leading, cfg.interval, reg)
	errs := make(chan error, 2)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("http serve: %w", err)
		}
	}()
	go func() {
		errs <- controller.RunLeaderElected(ctx, kube, controller.LeaderConfig{
			Namespace: cfg.leaseNS, LeaseName: "nexus-cluster-controller-" + cfg.cluster, Identity: cfg.identity,
		}, &leading, log, ctl.Run)
	}()

	var runErr error
	select {
	case <-ctx.Done():
		// Wait for the elector to release the Lease so the standby takes over
		// promptly instead of after a full lease duration.
		runErr = <-errs
	case runErr = <-errs:
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
	return runErr
}

func kubeConfig(path string) (*rest.Config, error) {
	if path != "" {
		c, err := clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig %s: %w", path, err)
		}
		return c, nil
	}
	c, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster kubernetes config (set KUBECONFIG to run outside a cluster): %w", err)
	}
	return c, nil
}

// healthServer: /healthz is liveness and never depends on a peer (a resource
// manager outage must not restart every controller replica). /readyz is true
// for a standby, and for the leader only while cycles are succeeding, so an
// alert on readiness means "the leader cannot reconcile".
func healthServer(addr string, ctl *controller.Controller, leading *controller.Leading, interval time.Duration,
	reg *metrics.Registry) *http.Server {
	start := time.Now()
	mux := http.NewServeMux()
	reg.Routes(mux)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !leading.Is() {
			_, _ = w.Write([]byte("standby"))
			return
		}
		last := ctl.LastSuccess()
		grace := 4 * interval
		if last.IsZero() && time.Since(start) < grace {
			_, _ = w.Write([]byte("leading; first cycle pending"))
			return
		}
		if time.Since(last) > grace {
			http.Error(w, "leading, but no successful reconcile in "+grace.String(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("leading"))
	})
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
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
	return slog.New(tracing.LogHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lv})))
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	v, _ := strconv.ParseBool(os.Getenv(key))
	return v
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
