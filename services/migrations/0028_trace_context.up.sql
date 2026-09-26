-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- Trace context across the asynchronous hops (ADR-0026).
--
-- A run's work crosses two boundaries that no request carries a trace through:
--
--   * the transactional outbox (0016). A fact commits in one service; the
--     event-service relay publishes it to NATS later, in its own loop. Without
--     the writer's context the published message starts a new trace.
--   * the attempt row. The dag-engine (or the retry timer) inserts a PENDING
--     attempt inside the trace of whatever made the node ready; a worker's poll
--     claims it minutes later inside the POLL's trace. Without the inserter's
--     context the dispatch, the execution and the result are cut off from the
--     run that caused them.
--
-- Each table gains one nullable TEXT column holding a W3C traceparent
-- ("00-<32 hex trace id>-<16 hex span id>-<2 hex flags>"). Only traceparent:
-- tracestate and baggage are request-scoped vendor data whose meaning does not
-- survive a queue. NULL means "not traced", which is also what every existing
-- row and every row written by a pre-0028 replica says.
--
-- Additive only (ADR-0011 rule 5): nullable columns without a default are
-- metadata-only on PostgreSQL 11+ (no rewrite of task_attempts, the hottest
-- table). No new grant: every writer holds table-level INSERT on both tables
-- (0005, 0016), and every role's table-level SELECT covers a new column.
-- Bounded length, so a peer cannot park kilobytes of junk in the dispatch row.
ALTER TABLE outbox        ADD COLUMN IF NOT EXISTS trace_context TEXT;
ALTER TABLE task_attempts ADD COLUMN IF NOT EXISTS trace_context TEXT;

ALTER TABLE outbox DROP CONSTRAINT IF EXISTS outbox_trace_context_len;
ALTER TABLE outbox ADD CONSTRAINT outbox_trace_context_len
    CHECK (trace_context IS NULL OR length(trace_context) <= 128) NOT VALID;
ALTER TABLE task_attempts DROP CONSTRAINT IF EXISTS task_attempts_trace_context_len;
ALTER TABLE task_attempts ADD CONSTRAINT task_attempts_trace_context_len
    CHECK (trace_context IS NULL OR length(trace_context) <= 128) NOT VALID;
