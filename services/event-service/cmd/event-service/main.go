// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Command event-service serves the append-only run history (ADR-0011).
package main

import (
	"github.com/khuriwalgroup/kgf-nexus/internal/tracing"

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
	"github.com/khuriwalgroup/kgf-nexus/internal/mail"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/notify"
	"github.com/khuriwalgroup/kgf-nexus/internal/secretbox"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	eventservice "github.com/khuriwalgroup/kgf-nexus/services/event-service"
	"github.com/khuriwalgroup/kgf-nexus/services/event-service/notifier"
	"github.com/khuriwalgroup/kgf-nexus/services/event-service/relay"
)

func main() {
	log := slog.New(tracing.LogHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	dsn := config.DSN()
	addr := env("HTTP_ADDR", ":8085")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Tracing (ADR-0026): off unless OTEL_* configures an exporter; flushed last.
	defer tracing.SetupOrExit(ctx, "event-service", version, log)()

	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		log.Error("could not parse database url", "error", err)
		os.Exit(1)
	}
	pcfg.ConnConfig.Tracer = tracing.PgxTracer() // ADR-0026; parameters never recorded
	if err := store.ApplyPoolEnv(pcfg); err != nil {
		log.Error("invalid pool setting", "error", err)
		os.Exit(1)
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
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
	// A configuration the bus rejects (credentials set but unreadable, none at
	// all where NEXUS_NATS_REQUIRE_AUTH or NEXUS_ENV=production demands them, a
	// password over plaintext) is fatal: continuing without the bus would hide
	// the mistake, and connecting without credentials is exactly what the
	// broker's authorization exists to prevent. An UNREACHABLE broker is not an
	// error here and never stops startup.
	busCfg, err := bus.ConfigFromEnv("nexus-event-service")
	if err != nil {
		log.Error("invalid NATS configuration", "error", err)
		os.Exit(1)
	}
	if bus.RequireAuthOverridden() {
		log.Warn("NEXUS_NATS_REQUIRE_AUTH=false overrides NEXUS_ENV: the event bus may be used without credentials")
	}
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

	// ── notifications and lineage (ADR-0028) ───────────────────────────────
	// Independent of NATS: the notifier reads the outbox rows directly. A
	// malformed secret key or mail configuration is fatal, like a malformed
	// NATS credential: running without it would turn every sealed channel or
	// email rule into a dead letter at the worst moment. An ABSENT key is
	// allowed; sealed secrets then fail with a message saying so.
	box, err := secretbox.FromEnv(os.Getenv)
	if err != nil {
		log.Error("invalid secret key configuration", "error", err)
		os.Exit(1)
	}
	if box == nil {
		log.Warn("NEXUS_SECRET_KEY is not set: channels with sealed secrets cannot be delivered; secret references still work")
	}
	mcfg, err := mail.ConfigFromEnv(os.Getenv)
	if err != nil {
		log.Error("invalid SMTP configuration", "error", err)
		os.Exit(1)
	}
	mailer, err := mail.New(mcfg)
	if err != nil {
		log.Error("invalid SMTP configuration", "error", err)
		os.Exit(1)
	}
	allowPrivate := os.Getenv("NEXUS_NOTIFY_ALLOW_PRIVATE_TARGETS") == "true"
	if allowPrivate {
		log.Warn("NEXUS_NOTIFY_ALLOW_PRIVATE_TARGETS=true: notifications may reach private and loopback addresses")
	}
	sender := &notify.Sender{HTTP: notify.NewHTTPClient(allowPrivate, 10*time.Second), UserAgent: "kgf-nexus-notifier/" + version}
	if !mail.IsDisabled(mailer) {
		sender.Mail = notifier.MailAdapter{S: mailer}
	}
	nt := notifier.New(pool, box, sender, notifier.ConfigFromEnv(log), log, notifier.NewMetrics(reg))
	notifierDone := make(chan struct{})
	go func() { nt.Run(ctx); close(notifierDone) }()

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
	// A send in flight when the context ends is recorded by nobody; its lease
	// expires and a peer (or the next start) sends it again.
	select {
	case <-notifierDone:
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
