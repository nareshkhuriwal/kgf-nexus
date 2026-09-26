// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CheckpointView is the stored checkpoint pointer for an attempt's idempotency
// key (task_checkpoints, migration 0036, ADR-0032 §4).
type CheckpointView struct {
	Key           string
	SHA256        string
	SizeBytes     int64
	Seq           int64
	AttemptNumber int32
	SavedAt       time.Time
}

// GatewayGetCheckpoint returns the latest checkpoint for the idempotency key of
// attemptID, or ErrNotFound when none was saved.
//
// A read: the caller has already fenced on the attempt's lease. The pointer is
// keyed by the idempotency key, not the attempt, so a retry reads what the
// attempt before it saved -- the whole point of the key excluding the attempt
// number (invariant 1).
func GatewayGetCheckpoint(ctx context.Context, q GWQuerier, attemptID string) (CheckpointView, error) {
	var c CheckpointView
	err := q.QueryRow(ctx, `
		SELECT c.object_key, c.sha256, c.size_bytes, c.seq, c.attempt_number, c.saved_at
		  FROM task_attempts a
		  JOIN task_checkpoints c ON c.idempotency_key = a.idempotency_key
		 WHERE a.id = $1`, attemptID,
	).Scan(&c.Key, &c.SHA256, &c.SizeBytes, &c.Seq, &c.AttemptNumber, &c.SavedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CheckpointView{}, ErrNotFound
	}
	if err != nil {
		return CheckpointView{}, fmt.Errorf("get checkpoint for attempt %s: %w", attemptID, err)
	}
	return c, nil
}
