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

	"github.com/khuriwalgroup/kgf-nexus/internal/config"
	eventservice "github.com/khuriwalgroup/kgf-nexus/services/event-service"
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

	srv := &http.Server{
		Handler:           eventservice.NewServer(eventservice.NewStore(pool), log).Routes(),
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
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
