// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Command event-service serves the append-only run history (ADR-0011).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/khuriwalgroup/kgf-nexus/internal/bus"
	"github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	eventservice "github.com/khuriwalgroup/kgf-nexus/services/event-service"
	"github.com/khuriwalgroup/kgf-nexus/services/event-service/relay"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	dsn := config.DSN("postgres://postgres:test@localhost:55432/nexus")
	addr := env("HTTP_ADDR", ":8085")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Error("could not create connection pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	// ── NATS (optional, ADR-0003) ──────────────────────────────────────────
	// The outbox relay lives here because this service owns run_events and is
	// not on the dispatch path (see package relay). An empty NEXUS_NATS_URL is a
	// supported configuration: the relay then only prunes the outbox, SSE tails
	// poll, and agents long-poll -- the Phase 1 behaviour, fully correct.
	reg := metrics.NewServiceRegistry("event-service", version)
	store.RegisterPoolMetrics(reg, pool)
	httpMetrics := metrics.NewHTTPMetrics(reg)
	busCfg := bus.ConfigFromEnv("nexus-event-service")
	var nc *nats.Conn
	waker := eventservice.NewRunWaker()
	if busCfg.Enabled() {
		// Connect never blocks on the broker being up (RetryOnFailedConnect), so a
		// NATS outage at deploy time cannot keep the event history offline.
		nc, err = bus.Connect(busCfg, log)
		if err != nil {
			log.Error("nats configuration rejected; continuing without the bus", "error", err)
			nc = nil
		} else {
			defer nc.Close()
			if _, err := waker.Attach(nc); err != nil {
				log.Warn("could not subscribe SSE waker; tails will poll", "error", err)
			}
		}
	} else {
		log.Info("NEXUS_NATS_URL is empty: event bus disabled; polling only")
	}
	rl, err := relay.New(pool, nc, relay.ConfigFromEnv(log), log, relay.NewMetrics(reg))
	if err != nil {
		log.Error("could not build outbox relay", "error", err)
		os.Exit(1)
	}
	relayDone := make(chan struct{})
	go func() { rl.Run(ctx); close(relayDone) }()

	srv := &http.Server{
		// Route-template RED metrics; the mux is passed directly so the
		// middleware sees the matched pattern (metrics.HTTPMetrics).
		Handler: httpMetrics.Middleware(eventservice.NewServer(eventservice.NewStore(pool), log).
			WithWaker(waker).
			WithRoutes(reg.Routes).
			Routes()),
		Addr:              addr,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE streams are long-lived by design and a write
		// deadline would sever every event tail on a fixed interval.
	}

	go func() {
		log.Info("event-service listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	// Bounded drain: in-flight reads finish, but a client holding an idle SSE
	// stream must not delay the deploy indefinitely.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown incomplete", "error", err)
	}
	// The relay stops on ctx; an in-flight batch rolls back and its rows are
	// re-sent by a peer or the next start, de-duplicated by Nats-Msg-Id.
	select {
	case <-relayDone:
	case <-shutdownCtx.Done():
	}
}

// version is stamped at build time (-ldflags "-X main.version=...").
var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
