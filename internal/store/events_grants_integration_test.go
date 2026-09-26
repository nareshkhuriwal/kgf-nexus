//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
)

// TestADR0028EventServiceTablesAreOwned pins migration 0034's grants for the
// event-service-owned tables, which the ownedTables matrix above does not
// cover (the event-service's pre-existing cross-table writes -- the run_events
// seq counter on workflow_runs -- predate that matrix).
func TestADR0028EventServiceTablesAreOwned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := testPool(t)
	owned := []string{"notification_deliveries", "notification_channel_windows", "notification_incidents",
		"assets", "asset_lineage_edges", "asset_materializations"}
	roles := []string{"nexus_api_gateway", "nexus_workflow_manager", "nexus_dag_engine", "nexus_scheduler",
		"nexus_resource_manager", "nexus_event_service"}
	allowed := map[string]bool{"nexus_api_gateway/notification_deliveries/INSERT": true}
	for _, table := range owned {
		for _, role := range roles {
			for _, priv := range []string{"INSERT", "UPDATE", "DELETE", "SELECT"} {
				var ok bool
				if err := pool.QueryRow(ctx, `SELECT has_table_privilege($1, $2, $3)`, role, table, priv).Scan(&ok); err != nil {
					t.Fatal(err)
				}
				want := priv == "SELECT" || role == "nexus_event_service" || allowed[role+"/"+table+"/"+priv]
				if ok != want {
					t.Errorf("%s %s on %s = %v, want %v", role, priv, table, ok, want)
				}
			}
		}
	}
	// The notifier's marker: only the outbox owner may set it.
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT has_column_privilege('nexus_scheduler', 'outbox', 'notified_at', 'UPDATE')`).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a writer may mark outbox rows notified, which would suppress their notifications")
	}
}
