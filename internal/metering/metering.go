// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package metering is the export hook of the usage ledger (ADR-0021): the seam
// a future billing or FinOps service plugs into. It builds no bill.
//
// Two ways out of the ledger, both at-least-once:
//
//   - push: every ledger row is announced on the bus as
//     nexus.usage.recorded.<namespace> (outbox topic "usage_recorded", stream
//     NEXUS_USAGE), written in the same transaction as the row;
//   - pull: Pump reads the ledger by id cursor and hands batches to an
//     Exporter.
//
// Either way a consumer must deduplicate on AttemptID, which the ledger keeps
// unique. The ledger, not the bus, is the record of truth.
package metering

import (
	"context"
	"fmt"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// Record is one attempt's metered usage, as written to the ledger.
type Record = store.UsageRecord

// Exporter receives ledger rows in id order. Export must be idempotent per
// AttemptID: a batch is re-delivered when the process stops between Export
// returning and the caller persisting the cursor.
type Exporter interface {
	Export(ctx context.Context, records []Record) error
}

// Cursor persists the id of the last exported row. A billing service keeps it
// in its own store; nothing in the control plane records export progress.
type Cursor interface {
	Load(ctx context.Context) (int64, error)
	Save(ctx context.Context, lastID int64) error
}

// Pump exports one batch: it reads up to batch rows after the saved cursor,
// hands them to exp, and advances the cursor only after Export succeeds.
// It returns how many rows were exported; zero means the ledger is drained up
// to the export lag (store.UsageExportLag).
func Pump(ctx context.Context, db store.GWQuerier, cur Cursor, exp Exporter, batch int) (int, error) {
	after, err := cur.Load(ctx)
	if err != nil {
		return 0, fmt.Errorf("metering: load cursor: %w", err)
	}
	recs, err := store.UsageRecordsAfter(ctx, db, after, batch)
	if err != nil {
		return 0, err
	}
	if len(recs) == 0 {
		return 0, nil
	}
	if err := exp.Export(ctx, recs); err != nil {
		return 0, fmt.Errorf("metering: export %d records after %d: %w", len(recs), after, err)
	}
	if err := cur.Save(ctx, recs[len(recs)-1].ID); err != nil {
		return 0, fmt.Errorf("metering: save cursor: %w", err)
	}
	return len(recs), nil
}
