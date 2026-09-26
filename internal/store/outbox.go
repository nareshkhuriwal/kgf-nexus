// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"

	"github.com/khuriwalgroup/kgf-nexus/internal/tracing"
)

// Transactional outbox writers (migration 0016, ADR-0003).
//
// Every fact worth announcing on the bus writes its outbox row IN THE SAME
// STATEMENT as the fact, as a data-modifying CTE. Same transaction is the
// requirement -- the row must exist if and only if the fact committed -- and same
// statement is the implementation, for two reasons:
//
//   - It adds no round trip. One of these rides inside the §5.3 dispatch claim,
//     where every extra network hop is held under the worker's row lock.
//   - It cannot be forgotten. A separate "and now insert the outbox row" call is
//     exactly the line a later refactor drops, and nothing would notice: the bus
//     is a hint, so losing it degrades latency silently.
//
// The CTEs below never change what the fact's own statement does or returns: an
// INSERT ... ON CONFLICT DO NOTHING that inserts nothing produces no outbox row,
// and a CAS UPDATE that matches nothing produces none either.
//
// The event-service relay turns these rows into NATS messages. Nothing here knows
// or cares whether NATS is configured: with the bus disabled the relay's janitor
// prunes the rows instead of publishing them.

// Outbox topics. Mirrored by the relay, which is the only reader.
const (
	OutboxTopicRunEvent  = "run_event"
	OutboxTopicRunState  = "run_state"
	OutboxTopicTaskReady = "task_ready"
)

// outboxTaskReadyCTE announces a newly PENDING attempt as a dispatch hint.
//
// It is appended to a `WITH a AS (INSERT INTO task_attempts ... RETURNING id,
// run_id, queue, scheduled_at, trace_context, ...)` and emits one row per
// attempt actually inserted. The hint carries the attempt's trace context
// (migration 0028), so the published message joins the run's trace. available_at is the attempt's scheduled_at, so a hint for work that
// is not yet claimable (a retry scheduled into the future) is published when it
// becomes claimable rather than waking the fleet early for nothing.
const outboxTaskReadyCTE = `
ob AS (
  INSERT INTO outbox (topic, namespace, routing_key, payload, available_at, trace_context)
  SELECT 'task_ready', r.namespace, a.queue,
         jsonb_build_object('attempt_id', a.id, 'run_id', a.run_id, 'queue', a.queue),
         GREATEST(a.scheduled_at, now()), a.trace_context
    FROM a JOIN workflow_runs r ON r.id = a.run_id
)`

// retryTrace is the trace context a retry attempt records: the caller's when
// it has one, else the failed attempt's. The retry timer loop runs outside any
// request, so without the fallback every retry would fall out of the run's
// trace exactly when an operator most wants to follow it.
func retryTrace(ctx context.Context, prev *string) *string {
	if tp := tracing.NullableTraceparent(ctx); tp != nil {
		return tp
	}
	return prev
}
