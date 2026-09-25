// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
)

// RegisterPoolMetrics exposes a pgx pool's statistics as nexus_db_pool_*.
//
// Pool exhaustion is the most common way a healthy Postgres still produces a
// slow control plane: every request queues for a connection, latency climbs,
// and the database itself looks idle. acquire_duration and empty_acquire are
// the two numbers that distinguish that from a slow database.
//
// The values are read from pool.Stat() at scrape time, which takes the pool's
// internal lock briefly and never touches the database -- a scrape must not
// depend on the dependency it is reporting on.
func RegisterPoolMetrics(reg *metrics.Registry, pool *pgxpool.Pool) {
	if reg == nil || pool == nil {
		return
	}
	stat := func(f func(*pgxpool.Stat) float64) func() float64 {
		return func() float64 { return f(pool.Stat()) }
	}
	reg.NewGaugeFunc("nexus_db_pool_max_conns", "Maximum size of the Postgres connection pool.",
		stat(func(s *pgxpool.Stat) float64 { return float64(s.MaxConns()) }))
	reg.NewGaugeFunc("nexus_db_pool_total_conns", "Connections currently open (idle, acquired or being established).",
		stat(func(s *pgxpool.Stat) float64 { return float64(s.TotalConns()) }))
	reg.NewGaugeFunc("nexus_db_pool_acquired_conns", "Connections currently checked out by the service.",
		stat(func(s *pgxpool.Stat) float64 { return float64(s.AcquiredConns()) }))
	reg.NewGaugeFunc("nexus_db_pool_idle_conns", "Connections open and idle in the pool.",
		stat(func(s *pgxpool.Stat) float64 { return float64(s.IdleConns()) }))
	reg.NewCounterFunc("nexus_db_pool_acquires_total", "Successful connection acquisitions from the pool.",
		stat(func(s *pgxpool.Stat) float64 { return float64(s.AcquireCount()) }))
	reg.NewCounterFunc("nexus_db_pool_acquire_duration_seconds_total",
		"Cumulative time spent waiting to acquire a connection. rate() of this over rate(acquires) is the mean wait.",
		stat(func(s *pgxpool.Stat) float64 { return s.AcquireDuration().Seconds() }))
	reg.NewCounterFunc("nexus_db_pool_empty_acquires_total",
		"Acquisitions that had to wait because the pool was empty -- the pool-exhaustion signal.",
		stat(func(s *pgxpool.Stat) float64 { return float64(s.EmptyAcquireCount()) }))
	reg.NewCounterFunc("nexus_db_pool_canceled_acquires_total",
		"Acquisitions abandoned because the caller's context ended first.",
		stat(func(s *pgxpool.Stat) float64 { return float64(s.CanceledAcquireCount()) }))
}
