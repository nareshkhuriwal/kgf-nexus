-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- The durable run-reconcile queue (CONTRACT-FINDINGS F160).
--
-- WHY. A run advances only when the dag-engine reconciles it: the engine alone
-- owns the graph, so nothing else turns one finished task into the next one
-- starting. Until now the only thing that asked it to was a synchronous RPC
-- made AFTER the fact had committed -- the gateway's call after a terminal
-- attempt report, the workflow-manager's after resume, cancel and retry. Each
-- was logged and swallowed on failure, and nothing ever asked again. A 500-way
-- fan-out on the dev cluster lost 446 of them to DeadlineExceeded against a
-- dag-engine whose pool was capped at 3 connections, and stayed RUNNING
-- forever with every attempt SUCCEEDED. An engine restart, a network error or
-- a timeout did the same thing one run at a time.
--
-- WHAT. A row here says "this run must be reconciled", and it is written by
-- the DATABASE, in the same transaction as the fact that needs it:
--
--   * task_attempts: an attempt reaching a terminal state (SUCCEEDED, FAILED,
--     TIMED_OUT, LOST, CANCELLED), whoever wrote it -- the scheduler's report
--     path, lease expiry (the F152 case, which no report follows), the
--     deadline timers, cancellation -- or an attempt inserted terminal (a memo
--     hit). A trigger rather than an INSERT at each call site because the
--     call sites are many and the next one would be missed; the trigger cannot
--     be.
--   * workflow_runs: a state change that reopens or settles the graph and so
--     needs the engine -- into RUNNING from anything but PENDING (resume,
--     operator retry), into PAUSING and into CANCELLING. PENDING -> RUNNING is
--     the engine's own publish after materialize, which has just reconciled.
--
-- The request therefore exists if and only if the fact does. The dag-engine
-- consumes it (SKIP LOCKED claim with a lease, so replicas share the backlog
-- and a dead replica's claims come back) and deletes it only after a reconcile
-- that STARTED after the request committed has succeeded. The synchronous RPCs
-- remain, as a latency optimisation: a successful one consumes the requests it
-- covered, so the happy path does no second reconcile.
--
-- WHY APPEND-ONLY, one row per fact, rather than one dirty row per run. A
-- per-run row must be UPDATEd (a generation bump) on every fact, or a fact
-- landing between a reconcile's read and its acknowledgement is lost; that is
-- a row lock every terminal attempt of a fan-out would queue on. Appending
-- takes no lock on anything shared. The consumer coalesces: it reconciles each
-- run once per batch, however many rows the run has (a 500-way fan-out is one
-- or two reconciles, not 500).
--
-- WHY NOT THE OUTBOX (0016). The outbox is the event-service's publish queue;
-- its rows are marked published, not consumed by the engine, and giving the
-- engine write access there would let it suppress another service's
-- announcements. This is a different queue with a different owner.
--
-- OWNERSHIP (ADR-0011, and its F160 addendum). dag-engine owns this table: it
-- alone claims, releases and deletes rows. Writers never touch it directly --
-- the triggers run SECURITY DEFINER -- so no service needs INSERT, a missing
-- grant cannot fail a scheduler's report transaction, and a writer cannot
-- delete or lease another's request. This is the second deliberate case of a
-- write inside another service's transaction (the first is the dispatch
-- claim): the request must commit atomically with the fact, or it is exactly
-- the best-effort follow-up it replaces.
--
-- ADDITIVE (ADR-0011 rule 5): a new table, two trigger functions, two
-- triggers and a one-time backfill. No existing column or row changes. An old
-- dag-engine ignores the table (rows accumulate harmlessly until the new one
-- starts); an old scheduler's writes are enqueued by the trigger regardless of
-- its version.

CREATE TABLE IF NOT EXISTS run_reconcile_requests (
    id            BIGSERIAL PRIMARY KEY,
    -- Deliberately NOT a foreign key, for the outbox's reason: a deleted run
    -- cascades away its nodes and attempts, and an FK here would either block
    -- that or need its own cascade. The consumer drops a request whose run no
    -- longer exists. It also keeps the FK's KEY SHARE lock on workflow_runs
    -- off the report path.
    run_id        UUID NOT NULL,
    -- The node whose attempt went terminal, when there is one. Advisory, like
    -- ReconcileRun's node_id: the engine reconciles the whole run regardless.
    node_id       TEXT,
    -- attempt_terminal | run_state | upgrade_backfill. Plain text, so adding a
    -- reason is not a constraint swap.
    reason        TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Not before this instant: a request whose reconcile failed is backed off
    -- here rather than held in a replica's memory.
    available_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The lease. A claimed request is invisible to other replicas until
    -- claimed_until passes, so a replica that dies mid-reconcile returns its
    -- claims by expiry, with no sweeper.
    claimed_by    TEXT,
    claimed_until TIMESTAMPTZ,
    attempts      INT NOT NULL DEFAULT 0,
    last_error    TEXT
);

-- The consumer's per-run lookups: the snapshot a reconcile acknowledges, and
-- "is anything queued for this run" (the stalled-run sweep's skip).
CREATE INDEX IF NOT EXISTS run_reconcile_requests_run_idx
    ON run_reconcile_requests (run_id, id);

-- ── the triggers ────────────────────────────────────────────────────────────

-- SECURITY DEFINER with a pinned search_path, as audit_purge (0033): the
-- function inserts as the table's owner, so the scheduler and the
-- workflow-manager need no privilege on the queue at all.
CREATE OR REPLACE FUNCTION run_reconcile_on_attempt_terminal() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
BEGIN
    INSERT INTO public.run_reconcile_requests (run_id, node_id, reason)
    VALUES (NEW.run_id, NEW.node_id, 'attempt_terminal');
    RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION run_reconcile_on_run_state() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
BEGIN
    INSERT INTO public.run_reconcile_requests (run_id, reason)
    VALUES (NEW.id, 'run_state');
    RETURN NULL;
END $$;

REVOKE ALL ON FUNCTION run_reconcile_on_attempt_terminal() FROM PUBLIC;
REVOKE ALL ON FUNCTION run_reconcile_on_run_state() FROM PUBLIC;

-- AFTER and row-level, with the condition in WHEN so a non-terminal update
-- (dispatch, start, progress) never even calls the function. OF state: an
-- update that does not name the column cannot change it.
DROP TRIGGER IF EXISTS run_reconcile_on_attempt_terminal ON task_attempts;
CREATE TRIGGER run_reconcile_on_attempt_terminal
    AFTER UPDATE OF state ON task_attempts
    FOR EACH ROW
    WHEN (NEW.state IN ('SUCCEEDED','FAILED','TIMED_OUT','LOST','CANCELLED')
          AND OLD.state IS DISTINCT FROM NEW.state)
    EXECUTE FUNCTION run_reconcile_on_attempt_terminal();

DROP TRIGGER IF EXISTS run_reconcile_on_attempt_born_terminal ON task_attempts;
CREATE TRIGGER run_reconcile_on_attempt_born_terminal
    AFTER INSERT ON task_attempts
    FOR EACH ROW
    WHEN (NEW.state IN ('SUCCEEDED','FAILED','TIMED_OUT','LOST','CANCELLED'))
    EXECUTE FUNCTION run_reconcile_on_attempt_terminal();

DROP TRIGGER IF EXISTS run_reconcile_on_run_state ON workflow_runs;
CREATE TRIGGER run_reconcile_on_run_state
    AFTER UPDATE OF state ON workflow_runs
    FOR EACH ROW
    WHEN (OLD.state IS DISTINCT FROM NEW.state
          AND NEW.state IN ('RUNNING','PAUSING','CANCELLING')
          AND NOT (OLD.state = 'PENDING' AND NEW.state = 'RUNNING'))
    EXECUTE FUNCTION run_reconcile_on_run_state();

-- ── the upgrade repair ──────────────────────────────────────────────────────

-- Runs stranded BEFORE this migration have no request, and never will: the
-- fact that should have produced one is already in the past. One request per
-- live, materialized run lets the new engine look at each once. Harmless for
-- a healthy run (a reconcile is idempotent and changes nothing it need not),
-- and bounded by the number of live runs, not by history. PAUSED runs are
-- left alone: resume reconciles them.
INSERT INTO run_reconcile_requests (run_id, reason)
SELECT r.id, 'upgrade_backfill'
  FROM workflow_runs r
 WHERE r.state IN ('PENDING','RUNNING','PAUSING','CANCELLING')
   AND EXISTS (SELECT 1 FROM task_nodes n WHERE n.run_id = r.id);

-- ── grants, through nexus_role (0038) ──────────────────────────────────────

DO $$
DECLARE
    svc TEXT;
    r   TEXT;
BEGIN
    -- Every service reads everything (ADR-0011's matrix).
    FOREACH svc IN ARRAY nexus_service_roles() LOOP
        r := nexus_role(svc);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT ON run_reconcile_requests TO %I', r);
        END IF;
    END LOOP;
    -- The owner claims (UPDATE), releases (UPDATE) and consumes (DELETE). It
    -- also INSERTs: the engine's own tests and an operator's manual "look at
    -- this run again" go through the same table.
    r := nexus_role('dag_engine');
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
        EXECUTE format('GRANT INSERT, UPDATE, DELETE ON run_reconcile_requests TO %I', r);
        EXECUTE format('GRANT USAGE ON SEQUENCE run_reconcile_requests_id_seq TO %I', r);
    END IF;
END $$;
