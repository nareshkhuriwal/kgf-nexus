-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- migrate: no-transaction
-- Make the pull-path claim cost bounded by a window, not by the backlog
-- (ADR-0018 Stage 1).
--
-- The worker claim ordered PENDING attempts by
--     priority + (now - scheduled_at) * aging_rate
-- which contains `now`, so no index can serve it: every claim evaluated and
-- sorted the queue's whole PENDING backlog (≈260 ms at 100k on a laptop, and a
-- 4.6 s plan collapse at 100k with 8 replicas). For a FIXED aging rate that
-- expression orders identically to
--     priority - epoch(scheduled_at) * aging_rate
-- because `now * rate` is the same for every row. That form has no `now`, so
-- an index can serve it and the claim can read a small window off the top.
--
-- Additive only (ADR-0011 rule 5): one pure function and two partial indexes.
-- No column, no backfill, no change to any existing object; an old scheduler
-- never uses them. CONCURRENTLY cannot run in a transaction, hence
-- `migrate: no-transaction`; every statement is safe to re-run.

-- The rank as an IMMUTABLE function, so it can appear in an index expression.
--
-- The epoch is taken from an INTERVAL (timestamptz - timestamptz), not with
-- EXTRACT(EPOCH FROM timestamptz): Postgres marks that form STABLE, because
-- extracting other fields from a timestamptz depends on the session TimeZone,
-- and an index may only use immutable functions. timestamptz subtraction and
-- extracting the epoch of an interval are both immutable, and the value is
-- identical, so the body is honestly immutable rather than merely declared so.
-- That also lets the planner inline it identically in the index and the query.
CREATE OR REPLACE FUNCTION nexus_dispatch_rank(priority integer, scheduled_at timestamptz, aging_rate double precision)
RETURNS double precision
LANGUAGE sql IMMUTABLE PARALLEL SAFE
AS $$
    SELECT priority::double precision
         - EXTRACT(EPOCH FROM (scheduled_at - TIMESTAMPTZ '1970-01-01 00:00:00+00')) * aging_rate
$$;

-- Leftover INVALID copies from a failed concurrent build would be kept by
-- IF NOT EXISTS and never used by the planner; drop them so a retry rebuilds.
DO $$
DECLARE ix text;
BEGIN
    FOREACH ix IN ARRAY ARRAY['idx_attempts_dispatch_rank', 'idx_attempts_unexplained'] LOOP
        IF EXISTS (
            SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
             WHERE c.relname = ix AND NOT i.indisvalid
        ) THEN
            EXECUTE format('DROP INDEX %I', ix);
        END IF;
    END LOOP;
END $$;

-- The claim window. Keyed exactly as the windowed query filters and orders:
-- queue and tenant as equality prefixes (a worker only ever claims its own
-- tenant's work on its replica's queue), then the rank DESC with scheduled_at
-- and id as deterministic tie-breaks, so paging by keyset is stable.
--
-- The rate is baked in as 1.0, the scheduler's default --aging-rate. A
-- scheduler run with another rate cannot use this index; it still claims in
-- the correct order (the query embeds its own rate) but pays the unindexed
-- sort, and logs that it is doing so at startup.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_attempts_dispatch_rank
    ON task_attempts (queue, tenant_id,
                      nexus_dispatch_rank(priority, scheduled_at, 1.0) DESC,
                      scheduled_at ASC, id ASC)
    WHERE state = 'PENDING';

-- explainUnschedulable runs on every claim and looks for the oldest PENDING
-- attempts that carry no pending_reason yet. Without this index, once most of
-- a large backlog has been explained, finding 20 unexplained rows means
-- walking past every explained one. Partial on exactly that predicate so the
-- walk starts at the first unexplained row.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_attempts_unexplained
    ON task_attempts (queue, tenant_id, scheduled_at)
    WHERE state = 'PENDING' AND (pending_reason IS NULL OR pending_reason = '');
