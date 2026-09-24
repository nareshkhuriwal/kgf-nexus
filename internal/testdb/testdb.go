// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package testdb holds the two things every Postgres integration test needs and
// that used to be re-implemented, slightly differently, in each package.
//
// It is imported only from _test.go files.
package testdb

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// EnvVar is the ONLY variable integration tests read their database from.
//
// There is deliberately no fallback DSN. A compiled-in default such as
// localhost:55432 silently points the suite at whatever Postgres happens to be
// listening there on a developer's machine -- an unrelated database, a stale
// schema -- and the suite then skips or passes against the wrong thing. Nor do
// the service spellings (DATABASE_URL, NEXUS_POSTGRES_DSN, ...) count: those
// name a service's database, and a test that truncates or seeds rows must never
// pick one up by accident.
const EnvVar = "NEXUS_TEST_DATABASE_URL"

// SkipMarker prefixes every skip this package issues because the database is
// missing. CI greps for it: a skipped integration test looks exactly like a
// passing one in `go test` output (F85), so the integration job fails if it
// appears.
const SkipMarker = "skipping integration test: no test database"

// DSN returns the test database DSN, or skips the test with SkipMarker when
// NEXUS_TEST_DATABASE_URL is unset.
func DSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv(EnvVar)
	if dsn == "" {
		t.Skipf("%s: %s is not set", SkipMarker, EnvVar)
	}
	return dsn
}

// SkipUnreachable skips the test with SkipMarker because the database named by
// NEXUS_TEST_DATABASE_URL could not be reached.
func SkipUnreachable(t testing.TB, err error) {
	t.Helper()
	t.Skipf("%s: %s is set but unreachable: %v", SkipMarker, EnvVar, err)
}

// Execer is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// DB is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx.
type DB interface {
	Execer
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// RegisterTenant makes ns a registered tenant.
//
// Since migrations 0014/0015, every namespace-owned row is assigned its
// tenant_id by trigger, and a namespace with no row in tenants is rejected
// (SQLSTATE 23503). Tests mint namespaces freely, so they must register them
// first, exactly as the product's registration flow does. Idempotent: ON
// CONFLICT DO NOTHING lets fixtures that share a fixed namespace, and repeated
// runs against one database, register it any number of times.
//
// The tenant row is intentionally left behind: tenant_id references it from
// every owned row, so deleting it would fail (or require deleting every row the
// test and the product wrote), and a stray tenant row is harmless to other tests
// because each mints its own namespace.
func RegisterTenant(t testing.TB, db Execer, ns string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.Exec(ctx,
		`INSERT INTO tenants (namespace, name) VALUES ($1, 'integration test') ON CONFLICT (namespace) DO NOTHING`,
		ns); err != nil {
		t.Fatalf("register tenant %q: %v", ns, err)
	}
}

// NewAttempt inserts the smallest graph a task attempt can belong to -- one
// spec, run and node in ns -- and returns the new attempt's id. ns is
// registered as a tenant first.
//
// Tests of the resource ledger used to reserve against random UUIDs and drop
// the reservations -> task_attempts foreign key to get away with it. Since 0014
// a reservation also inherits its tenant from its attempt by trigger, so a
// reservation needs a real attempt, and altering a shared table's constraints
// from a test was never safe anyway. The run is deleted on cleanup, which
// cascades to the node, the attempt and any reservation on it.
func NewAttempt(t testing.TB, db DB, ns string) string {
	t.Helper()
	RegisterTenant(t, db, ns)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var specID, runID, attemptID string
	if err := db.QueryRow(ctx, `
		INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1, 'attempt-fixture-' || gen_random_uuid()::text, 1, 'attempt-fixture', '{}'::jsonb, 'integration-test')
		RETURNING id::text`, ns).Scan(&specID); err != nil {
		t.Fatalf("attempt fixture spec: %v", err)
	}
	if err := db.QueryRow(ctx, `
		INSERT INTO workflow_runs (spec_id, namespace, trigger_kind)
		VALUES ($1, $2, 'test') RETURNING id::text`, specID, ns).Scan(&runID); err != nil {
		t.Fatalf("attempt fixture run: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = db.Exec(c, `DELETE FROM workflow_runs WHERE id = $1`, runID)
		_, _ = db.Exec(c, `DELETE FROM workflow_specs WHERE id = $1`, specID)
	})
	if _, err := db.Exec(ctx, `
		INSERT INTO task_nodes (run_id, node_id, task_ref, spec_json)
		VALUES ($1, 'node', 'test.task:v1', '{}'::jsonb)`, runID); err != nil {
		t.Fatalf("attempt fixture node: %v", err)
	}
	if err := db.QueryRow(ctx, `
		INSERT INTO task_attempts (run_id, node_id, attempt_number, resource_request, idempotency_key)
		VALUES ($1::uuid, 'node', 1, '{}'::jsonb, 'attempt-fixture-' || $1::text)
		RETURNING id::text`, runID).Scan(&attemptID); err != nil {
		t.Fatalf("attempt fixture attempt: %v", err)
	}
	return attemptID
}
