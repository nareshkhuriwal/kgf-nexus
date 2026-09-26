// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package store holds the pgx query layer. Each service owns its own files
// here (CONTRACT.md); this file and its gateway_*/wfm_* siblings belong to the
// api-gateway and workflow-manager services.
//
// There is no ORM by policy: every statement in this package is visible SQL,
// because the correctness of this platform lives in specific constraints and
// specific lock modes (FOR UPDATE SKIP LOCKED, partial unique indexes, ON
// CONFLICT clauses) that an ORM would abstract away exactly when we most need
// to reason about them.
//
// Exported names in these files carry a Gateway/WFM prefix. The package is
// shared by six independently developed services (ADR-0011), so an unprefixed
// `DB` or `Open` here would collide with another service's file in the same
// package and break a build the author of either file could not see coming.
package store

import (
	"github.com/khuriwalgroup/kgf-nexus/internal/tracing"

	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GWQuerier is the read/write surface shared by a pool and a transaction, so a
// query helper can run either standalone or inside a caller's transaction
// without being written twice.
type GWQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// GWDB adds transactions to GWQuerier. It is an interface rather than a
// concrete *pgxpool.Pool so tests can substitute a fake, and so it is obvious
// that nothing in the query layer depends on pool-specific behaviour.
type GWDB interface {
	GWQuerier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// GWOpen builds a connection pool.
//
// It deliberately does NOT run migrations: migrations are a dedicated job
// (ADR-0011 rule 4). Several service replicas racing to migrate one shared
// schema at startup is a corruption hazard, not a convenience.
func GWOpen(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.ConnConfig.Tracer = tracing.PgxTracer() // ADR-0026; parameters never recorded
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("new pool: %w", err)
	}
	// Ping at startup so a misconfigured DSN fails the process immediately
	// rather than surfacing as a confusing 500 on the first request.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}
