// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package store holds the pgx data access layer. There is no ORM (CONTRACT.md):
// the dispatch claim is a hand-written transaction whose exact lock ordering and
// SKIP LOCKED semantics ARE the correctness argument (§5.3), and no ORM
// expresses that faithfully.
//
// Each service owns its own file here and writes only the tables ADR-0011
// assigns it. Shared plumbing (this file) is common to all of them.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig are the knobs that actually matter for a control-plane service.
type PoolConfig struct {
	DSN string
	// MaxConns bounds Postgres connection pressure. Every scheduler replica
	// multiplies this, and Postgres connections are expensive, so it is set
	// explicitly rather than left to the driver's NumCPU-derived default.
	MaxConns int32
	MinConns int32
	// MaxConnLifetime recycles connections so a long-lived pool eventually
	// rebalances across Postgres instances after a failover.
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

// DefaultPoolConfig returns sane defaults for a scheduler replica.
func DefaultPoolConfig(dsn string) PoolConfig {
	return PoolConfig{
		DSN:             dsn,
		MaxConns:        16,
		MinConns:        2,
		MaxConnLifetime: 30 * time.Minute,
		MaxConnIdleTime: 5 * time.Minute,
	}
}

// NewPool opens and verifies a connection pool.
//
// It pings before returning so a bad DSN or an unreachable database fails at
// startup rather than on the first dispatch cycle. A service that starts
// "successfully" and then fails every claim is much harder to diagnose than one
// that refuses to start.
func NewPool(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		pc.MinConns = cfg.MinConns
	}
	pc.MaxConnLifetime = cfg.MaxConnLifetime
	pc.MaxConnIdleTime = cfg.MaxConnIdleTime

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}
