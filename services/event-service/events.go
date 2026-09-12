// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package eventservice owns the run_events table (ADR-0011) and serves the
// append-only run history for the UI, the CLI, and external subscribers.
//
// Two properties define this service:
//
//  1. run_events is APPEND-ONLY. Nothing here ever UPDATEs or DELETEs a row
//     within retention. The current-state tables are a projection of this log;
//     on any disagreement the log wins, and state can be rebuilt from it.
//
//  2. It is NOT on the dispatch path. The scheduler writes its dispatch event
//     inside the claim transaction (the documented ADR-0011 exception) because
//     those five facts must commit atomically. This service serves reads and
//     accepts events from writers that are NOT inside such a transaction.
//     Putting it on the dispatch path would mean a cross-service call inside the
//     claim, which would hold row locks across a network hop -- exactly what
//     keeps claim transactions short and vacuum healthy.
package eventservice

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Event is one durable entry in a run's history.
type Event struct {
	ID         int64             `json:"id"`
	RunID      string            `json:"run_id"`
	Seq        int32             `json:"seq"`
	NodeID     *string           `json:"node_id,omitempty"`
	AttemptID  *string           `json:"attempt_id,omitempty"`
	Kind       string            `json:"kind"`
	Payload    map[string]any    `json:"payload"`
	Actor      string            `json:"actor"`
	OccurredAt time.Time         `json:"occurred_at"`
}

// Store is the data access surface for run_events.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// ErrRunNotFound distinguishes "this run has no events yet" (legitimate, empty
// history) from "this run does not exist" (a client error worth a 404). Callers
// that cannot tell these apart end up rendering an empty timeline for a typo'd
// run id, which sends operators hunting a non-existent problem.
var ErrRunNotFound = errors.New("run not found")

// Append writes one event, allocating seq atomically.
//
// Sequence allocation uses the workflow_runs.event_seq counter (migration 0002)
// rather than max(seq)+1, which races: two concurrent writers for the same run
// both read the same max and collide on UNIQUE(run_id, seq). The counter UPDATE
// contends only on that run's own row, so unrelated runs never serialize.
func (s *Store) Append(ctx context.Context, e Event) (Event, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Event{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var seq int32
	err = tx.QueryRow(ctx,
		`UPDATE workflow_runs SET event_seq = event_seq + 1
		  WHERE id = $1
		 RETURNING event_seq`, e.RunID).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrRunNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("allocate seq: %w", err)
	}

	payload := e.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	var id int64
	var occurred time.Time
	err = tx.QueryRow(ctx,
		`INSERT INTO run_events (run_id, seq, node_id, attempt_id, kind, payload, actor)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 RETURNING id, occurred_at`,
		e.RunID, seq, e.NodeID, e.AttemptID, e.Kind, payload, e.Actor).Scan(&id, &occurred)
	if err != nil {
		return Event{}, fmt.Errorf("insert event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Event{}, fmt.Errorf("commit: %w", err)
	}

	e.ID, e.Seq, e.OccurredAt, e.Payload = id, seq, occurred, payload
	return e, nil
}

// ListOpts pages a run's history. AfterSeq drives live tailing: a client polls
// with the highest seq it has seen and receives only what is new, which is why
// seq is monotonic per run rather than a global counter.
type ListOpts struct {
	AfterSeq int32
	Limit    int
	Kind     string
	Actor    string
}

// List returns events for a run in seq order.
func (s *Store) List(ctx context.Context, runID string, opt ListOpts) ([]Event, error) {
	if opt.Limit <= 0 || opt.Limit > 1000 {
		opt.Limit = 500
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, seq, node_id, attempt_id, kind, payload, actor, occurred_at
		   FROM run_events
		  WHERE run_id = $1
		    AND seq > $2
		    AND ($3 = '' OR kind  = $3)
		    AND ($4 = '' OR actor = $4)
		  ORDER BY seq ASC
		  LIMIT $5`,
		runID, opt.AfterSeq, opt.Kind, opt.Actor, opt.Limit)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	out := make([]Event, 0, opt.Limit)
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.RunID, &e.Seq, &e.NodeID, &e.AttemptID,
			&e.Kind, &e.Payload, &e.Actor, &e.OccurredAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
