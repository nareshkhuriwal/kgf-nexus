//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// ownedTables is ADR-0011's matrix, as the grants must express it. Kept here as
// data rather than prose so a drift between the ADR and the database fails a test
// instead of being noticed in review, which is the failure mode migration 0005
// exists to end: the ADR claimed grants enforced ownership while none existed.
var ownedTables = map[string][]string{
	"nexus_api_gateway": {"api_keys", "attempt_log_chunks", "attempt_logs", "idempotency_keys",
		// ADR-0028: notification, trigger and sensor configuration.
		"notification_channels", "notification_rule_channels", "notification_rules", "sensors",
		"webhook_receipts", "webhook_triggers"},
	// sensor_state and sensor_observations (ADR-0028): the sensor loop's lease,
	// cursor and at-most-once observation claims.
	"nexus_workflow_manager": {"sensor_observations", "sensor_state", "timers", "workflow_runs", "workflow_specs"},
	"nexus_dag_engine":       {"task_edges", "task_nodes"},
	// attempt_progress (0024) is the latest ReportProgress of each attempt: part
	// of the attempt's lifecycle, so the scheduler's, kept out of task_attempts
	// only so a progress write never holds the attempt row (F96).
	"nexus_scheduler":        {"attempt_progress", "concurrency_slots", "leases", "task_attempts"},
	"nexus_resource_manager": {"reservations", "workers"},
}

// TestServiceRolesExist fails loudly if 0005 has not been applied, so the rest of
// the assertions below cannot pass vacuously.
func TestServiceRolesExist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := testPool(t)

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*)::int FROM pg_roles WHERE rolname LIKE 'nexus\_%'`).Scan(&n); err != nil {
		t.Fatalf("count roles: %v", err)
	}
	if n != 6 {
		t.Fatalf("found %d nexus_* roles, want 6; migration 0005 is not applied, so "+
			"table ownership is convention-only and every assertion here is vacuous", n)
	}
}

// TestOwnersCanWriteTheirOwnTables is the other half of enforcement: grants that
// are too tight break the service they protect, which is worse than no grants at
// all because it fails at runtime in production.
func TestOwnersCanWriteTheirOwnTables(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := testPool(t)

	for role, tables := range ownedTables {
		for _, table := range tables {
			for _, priv := range []string{"INSERT", "UPDATE", "DELETE"} {
				var ok bool
				if err := pool.QueryRow(ctx,
					`SELECT has_table_privilege($1, $2, $3)`, role, table, priv).Scan(&ok); err != nil {
					t.Fatalf("check %s %s on %s: %v", role, priv, table, err)
				}
				if !ok {
					t.Errorf("%s cannot %s its own table %s; the owner is locked out of the "+
						"table ADR-0011 assigns it", role, priv, table)
				}
			}
		}
	}
}

// TestNonOwnersCannotWriteOthersTables is the guarantee itself. A service that can
// write another's tables can corrupt state its owner believes only it controls,
// and the damage surfaces far from the code that caused it.
func TestNonOwnersCannotWriteOthersTables(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := testPool(t)

	// The documented exceptions (ADR-0011 rule 2): the dispatch claim commits five
	// facts in one transaction, three of them in tables the scheduler does not own,
	// because atomicity outweighs ownership purity on the no-double-dispatch path.
	// Listed explicitly so an UNdocumented cross-owner grant still fails.
	allowed := map[string]bool{
		"nexus_scheduler/reservations/INSERT": true,
		"nexus_scheduler/reservations/DELETE": true,
		"nexus_scheduler/timers/INSERT":       true,
		"nexus_scheduler/timers/DELETE":       true,
		"nexus_scheduler/run_events/INSERT":   true,
		// The DAG engine creates a node's first attempt while materializing the
		// frontier; every transition after that belongs to the scheduler.
		"nexus_dag_engine/task_attempts/INSERT": true,
		// ADR-0028: a channel test and a dead-letter redrive are NEW delivery
		// rows the gateway inserts; it can never lease, mark or delete one.
		"nexus_api_gateway/notification_deliveries/INSERT": true,
	}

	owner := map[string]string{}
	for role, tables := range ownedTables {
		for _, table := range tables {
			owner[table] = role
		}
	}

	for role := range ownedTables {
		for table, ownedBy := range owner {
			if ownedBy == role {
				continue
			}
			for _, priv := range []string{"INSERT", "UPDATE", "DELETE"} {
				key := strings.Join([]string{role, table, priv}, "/")
				var ok bool
				if err := pool.QueryRow(ctx,
					`SELECT has_table_privilege($1, $2, $3)`, role, table, priv).Scan(&ok); err != nil {
					t.Fatalf("check %s %s on %s: %v", role, priv, table, err)
				}
				if ok && !allowed[key] {
					t.Errorf("%s may %s %s, which %s owns, and that is not one of "+
						"ADR-0011's documented exceptions", role, priv, table, ownedBy)
				}
			}
		}
	}
}

// TestRunEventsIsAppendOnlyAtTheDatabase turns invariant 8 from a convention into
// something the database refuses to break. An UPDATE or DELETE here would rewrite
// history the platform reports as an audit trail.
func TestRunEventsIsAppendOnlyAtTheDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := testPool(t)

	for _, role := range []string{"nexus_event_service", "nexus_scheduler"} {
		for _, priv := range []string{"UPDATE", "DELETE"} {
			var ok bool
			if err := pool.QueryRow(ctx,
				`SELECT has_table_privilege($1, 'run_events', $2)`, role, priv).Scan(&ok); err != nil {
				t.Fatalf("check %s %s: %v", role, priv, err)
			}
			if ok {
				t.Errorf("%s may %s run_events; invariant 8 says the log is append-only, "+
					"so no role may rewrite it", role, priv)
			}
		}
		// It must still be able to APPEND, or no history is recorded at all.
		var canInsert bool
		if err := pool.QueryRow(ctx,
			`SELECT has_table_privilege($1, 'run_events', 'INSERT')`, role).Scan(&canInsert); err != nil {
			t.Fatalf("check %s INSERT: %v", role, err)
		}
		if !canInsert {
			t.Errorf("%s cannot append to run_events, so its events are lost", role)
		}
	}
}

// TestCrossOwnerWritesAreColumnScoped keeps the exceptions narrow. The scheduler
// adjusts the capacity ledger and marks a worker DEAD, but must not be able to
// rewrite a worker's identity, advertised capacity or queues — which a table-level
// grant would allow.
func TestCrossOwnerWritesAreColumnScoped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := testPool(t)

	granted := map[string]bool{"committed": true, "state": true}
	rows, err := pool.Query(ctx, `
		SELECT column_name FROM information_schema.column_privileges
		 WHERE grantee = 'nexus_scheduler' AND table_name = 'workers'
		   AND privilege_type = 'UPDATE'`)
	if err != nil {
		t.Fatalf("read column privileges: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, col)
		if !granted[col] {
			t.Errorf("nexus_scheduler may UPDATE workers.%s; only committed and state "+
				"are documented exceptions, and a wider grant lets the scheduler rewrite "+
				"a worker's advertised capacity", col)
		}
	}
	sort.Strings(got)
	if len(got) != len(granted) {
		t.Errorf("scheduler's UPDATE columns on workers = %v, want exactly committed and state", got)
	}
}
