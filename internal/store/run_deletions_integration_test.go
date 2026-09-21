// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package store

import (
	"context"
	"github.com/jackc/pgx/v5"
	"testing"
)

func TestIntegrationDeletedRunHiddenButRetained(t *testing.T) {
	pool := testPool(t)
	inAttentionTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		ns := "run-delete-test"
		id := seedRun(t, tx, ns, "deletion-spec", "SUCCEEDED")
		if _, err := tx.Exec(ctx, `INSERT INTO run_deletions(run_id,namespace) VALUES($1,$2)`, id, ns); err != nil {
			t.Fatal(err)
		}
		rows, err := GatewayListRuns(ctx, tx, ns, RunFilter{Limit: 25})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Fatal("deleted run remains listed")
		}
		if _, err := GatewayGetRun(ctx, tx, ns, id); err != nil {
			t.Fatalf("execution record lost: %v", err)
		}
	})
}
