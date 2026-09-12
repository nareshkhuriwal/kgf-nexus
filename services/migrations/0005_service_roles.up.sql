-- Per-service Postgres roles with table-level grants (ADR-0011 rule 1).
--
-- The ADR states the rule is "enforced by per-service Postgres roles with
-- table-level grants, so a violation fails at the database, not at code review."
-- That enforcement did not exist: there was not one GRANT or CREATE ROLE anywhere
-- in the repository, every service connected as the owning superuser, and table
-- ownership was a convention held up entirely by review. This migration makes the
-- claim true.
--
-- WHY IT MATTERS beyond tidiness: the ownership boundary is what makes the shared
-- schema safe to share. A service that can write another's tables can corrupt
-- state its owner believes only it controls -- and the failure surfaces as
-- inexplicable data, far from the code that caused it. A grant turns that class of
-- bug into an immediate, attributable error.
--
-- READS ARE UNRESTRICTED, deliberately. ADR-0011's matrix gives every service
-- read access to everything ("Reads: all") because the projections services build
-- for the console and for placement decisions legitimately span tables. Only
-- WRITES are constrained, which is where the ownership guarantee lives.
--
-- ADDITIVE (ADR-0011 rule 5): creates roles and grants only. It adds no column,
-- drops nothing, and changes no existing row, so a mixed-version rolling deploy is
-- unaffected. Services that still connect as the superuser keep working
-- unchanged -- adopting a role is a deployment change (a different DSN per
-- service), not a schema change, which is why this is safe to apply ahead of it.
--
-- IDEMPOTENT: every statement guards on existence, so re-running is a no-op.

-- Roles are NOLOGIN group roles. A deployment grants one to the login user it
-- creates per service, so credential rotation never touches this migration.
DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('CREATE ROLE %I NOLOGIN', r);
        END IF;
    END LOOP;
END $$;

-- Every service reads everything (ADR-0011's matrix) and uses the schema.
DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        EXECUTE format('GRANT USAGE ON SCHEMA public TO %I', r);
        EXECUTE format('GRANT SELECT ON ALL TABLES IN SCHEMA public TO %I', r);
        -- Sequences back the bigserial keys on run_events and api_keys; a role
        -- that may INSERT must be able to draw from them.
        EXECUTE format('GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO %I', r);
    END LOOP;
END $$;

-- ── owned tables: full write access ──────────────────────────────────────────

GRANT INSERT, UPDATE, DELETE ON api_keys, idempotency_keys TO nexus_api_gateway;
GRANT INSERT, UPDATE, DELETE ON workflow_specs, workflow_runs, timers TO nexus_workflow_manager;
GRANT INSERT, UPDATE, DELETE ON task_nodes, task_edges TO nexus_dag_engine;
GRANT INSERT, UPDATE, DELETE ON task_attempts, leases, concurrency_slots TO nexus_scheduler;
GRANT INSERT, UPDATE, DELETE ON workers, reservations TO nexus_resource_manager;

-- run_events is append-only (invariant 8): its owner may INSERT and must NOT be
-- able to UPDATE or DELETE, so the history cannot be rewritten even by the
-- service that writes it. The invariant stops being a convention here.
GRANT INSERT ON run_events TO nexus_event_service;

-- ── the documented exceptions ───────────────────────────────────────────────

-- The dispatch claim commits five facts in ONE transaction, three of them in
-- tables the scheduler does not own (ADR-0011 rule 2). Splitting it would replace
-- a transaction with a saga on the one path where a compensation bug means a
-- double-dispatched task, so atomicity wins over ownership and these grants make
-- the exception explicit rather than implicit.
GRANT INSERT, DELETE ON reservations TO nexus_scheduler;   -- capacity reservation
GRANT INSERT ON run_events TO nexus_scheduler;             -- the dispatch event
GRANT INSERT, DELETE ON timers TO nexus_scheduler;         -- deadline timers

-- workers.committed only: the scheduler adjusts the capacity ledger inside the
-- claim and when reclaiming it, but must not be able to rewrite a worker's
-- identity, capacity or queues. Column-level grants are what make "one documented
-- exception" narrower than "write access to the table".
GRANT UPDATE (committed) ON workers TO nexus_scheduler;

-- The worker-DEAD transition writes workers.state from the scheduler's liveness
-- sweeper, because marking a worker DEAD must be atomic with expiring its leases:
-- a DEAD worker with live leases is dispatched-to by no one and recovered by no
-- one, so work stalls silently. Recorded as still-open in ADR-0011 rule 2 and
-- CONTRACT-FINDINGS F9; granting the single column keeps the blast radius at the
-- column rather than the table while that is resolved.
GRANT UPDATE (state) ON workers TO nexus_scheduler;

-- run_events.seq is allocated from workflow_runs.event_seq (migration 0002, and
-- CONTRACT.md's EVENT SEQ rule). Every writer of run_events must therefore be able
-- to advance that ONE column of a table it does not own -- which is why the grant
-- is column-scoped. Without it the allocator fails and no event can be recorded.
GRANT UPDATE (event_seq) ON workflow_runs TO nexus_scheduler;
GRANT UPDATE (event_seq) ON workflow_runs TO nexus_event_service;

-- The DAG engine creates the first attempt of a node as part of materializing the
-- frontier, which is the scheduler's table. INSERT only, never UPDATE or DELETE:
-- creating an attempt is the engine's job, and every state transition after that
-- belongs to the scheduler. This is the split dagengine_writes.go describes.
GRANT INSERT ON task_attempts TO nexus_dag_engine;
-- attempts_used is the retry budget, incremented with the attempt row so the two
-- can never disagree.
GRANT UPDATE (attempts_used) ON task_nodes TO nexus_dag_engine;

-- New tables must not silently become writable by everyone, so nothing is granted
-- by default; a future migration grants explicitly, the same way this one does.
