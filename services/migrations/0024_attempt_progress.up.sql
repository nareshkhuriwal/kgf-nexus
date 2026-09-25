-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
--
-- The latest ReportProgress of each attempt (CONTRACT-FINDINGS F96).
--
-- The Kubernetes executor reports pod phase changes ("pod Pending:
-- Unschedulable: 0/3 nodes are available: insufficient cpu") through
-- ReportProgress, which is ADR-0007's mitigation for nested scheduling. Until
-- now the scheduler kept only the heartbeat time and dropped the message and
-- the fraction, so the one explanation for "RUNNING but nothing is happening"
-- never reached an operator.
--
-- WHY A SIDE TABLE AND NOT COLUMNS ON task_attempts. Every terminal transition
-- (ApplyResult), cancellation and lease reclaim locks the attempt row with
-- `FOR UPDATE ... SKIP LOCKED`, and treats "no row" as "already handled". A
-- progress write that UPDATEd task_attempts would hold that row lock for the
-- length of its transaction, and a result arriving in the same instant -- the
-- executor's "pod Succeeded" and the worker's ReportResult are sent together --
-- would be SKIPPED and silently dropped, leaving the attempt to expire as LOST
-- and run again. A foreign key to task_attempts has the same flaw one level
-- down: its insert-time check takes FOR KEY SHARE on the attempt row, which
-- FOR UPDATE also conflicts with. So this table references the RUN (whose row
-- no path locks with SKIP LOCKED) and never locks the attempt at all. It also
-- keeps a chatty worker's updates off the hottest table in the system.
--
-- OWNERSHIP (ADR-0011): the scheduler writes it, like the rest of an attempt's
-- lifecycle; every service may read it. It is added to the scheduler's row of
-- the ownership matrix, not to the exceptions.
--
-- ADDITIVE (ADR-0011 rule 5): one new table, its grants and its tenant
-- trigger. Nothing existing is altered, so a scheduler or gateway built before
-- this migration never notices it: the old scheduler keeps dropping the
-- message, and the old gateway simply does not read the table.

CREATE TABLE IF NOT EXISTS attempt_progress (
    -- One row per attempt, keyed by its id. Deliberately NOT a foreign key to
    -- task_attempts; see the header.
    attempt_id          UUID PRIMARY KEY,
    -- Deleting a run cascades here, so run deletion (0011) stays complete with
    -- no new code.
    run_id              UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    -- Assigned by the tenant trigger below from the attempt, as leases' is.
    tenant_id           UUID NOT NULL REFERENCES tenants(id),
    -- The lease generation (leases.epoch) that wrote the row. A write carrying
    -- an older epoch than the stored one is refused, so a report from a
    -- superseded lease can never overwrite a newer one's.
    lease_epoch         INT NOT NULL,
    -- The latest non-empty message, already sanitized by the scheduler (one
    -- line, no control or format characters, capped at 512 bytes). The CHECK is
    -- the backstop, with headroom so the cap can be raised without a migration.
    message             TEXT CHECK (message IS NULL OR octet_length(message) <= 1024),
    -- The latest valid fraction. NULL means "never reported"; proto3 cannot
    -- distinguish an unset fraction from 0, so 0 is never stored as a report.
    -- NaN fails this CHECK (NaN <= 1 is false in Postgres).
    fraction            DOUBLE PRECISION CHECK (fraction IS NULL OR (fraction >= 0 AND fraction <= 1)),
    -- Control-plane time of the latest accepted report, message or not.
    reported_at         TIMESTAMPTZ NOT NULL,
    -- Control-plane time the message last CHANGED, so "pod Pending for 12
    -- minutes" is answerable without replaying events.
    message_changed_at  TIMESTAMPTZ,
    -- Rate limiting and deduplication of AttemptProgress run events. Kept
    -- here rather than derived from run_events so the decision is one row read
    -- under this row's lock, not a scan of an append-only log.
    last_event_at       TIMESTAMPTZ,
    event_count         INT NOT NULL DEFAULT 0 CHECK (event_count >= 0),
    -- Message changes that were stored on this row but not recorded as an
    -- event (rate limit or cap), reported in the next event's payload so the
    -- history is honest about what it skipped.
    events_suppressed   INT NOT NULL DEFAULT 0 CHECK (events_suppressed >= 0)
);

CREATE INDEX IF NOT EXISTS attempt_progress_run_idx ON attempt_progress (run_id);
CREATE INDEX IF NOT EXISTS attempt_progress_tenant_idx ON attempt_progress (tenant_id);

-- The tenant trigger reads the attempt with a plain SELECT (0014's function),
-- which takes no row lock, so it keeps the header's no-lock property.
DROP TRIGGER IF EXISTS tenant_assignment ON attempt_progress;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON attempt_progress
    FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('attempt_id', 'task_attempts');

-- Grants, guarded per role like 0020-0022, so a database without the service
-- roles (a developer's) still migrates. 0005's SELECT ON ALL TABLES does not
-- reach tables created after it ran.
DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT ON attempt_progress TO %I', r);
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_scheduler') THEN
        GRANT INSERT, UPDATE, DELETE ON attempt_progress TO nexus_scheduler;
    END IF;
END $$;
