-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- migrate: no-transaction
--
-- Memoization (ARCHITECTURE §6.4) and worker quarantine (§5.5, §10).
--
-- NO-TRANSACTION, like 0015, and for the same reason: the index on
-- task_attempts below must be built CONCURRENTLY, because a plain CREATE INDEX
-- on the hottest table in the system holds a write lock for the whole build --
-- on a populated control plane that is an outage, not a migration. CONCURRENTLY
-- cannot run inside a transaction block. Every statement is therefore written to
-- be safe to re-run (IF NOT EXISTS, guarded DO blocks), so a failure part way
-- through is recovered by running the migration again; the ledger records it
-- only on success.
--
-- ADDITIVE (ADR-0011 rule 5): two new tables, one new NULLABLE column with no
-- default and no backfill (metadata-only on PG 11+), and one concurrent index.
-- Nothing is dropped, renamed or tightened, so a replica that predates this
-- migration keeps working: it never reads the new column or tables.

-- ─── Memoization ─────────────────────────────────────────────────────────────

-- cache_hit_of marks an attempt that was SATISFIED FROM THE CACHE rather than
-- executed, and names the attempt whose result it reused.
--
-- Why a column rather than an event alone: the memo lookup must be able to
-- exclude hits from the set of sources. If a hit could itself serve as a source,
-- every hit would restamp the entry with a fresh ended_at, and a result computed
-- once would never expire -- the TTL would be defeated by the very traffic it
-- exists to bound. Filtering on a column is also what lets the console mark a
-- node "cached" without replaying the event log.
--
-- No foreign key, deliberately: the source may belong to a run that is later
-- deleted (0011), and the hit's provenance should survive that as a dangling id
-- rather than force a cascade into another run's history.
ALTER TABLE task_attempts ADD COLUMN IF NOT EXISTS cache_hit_of UUID;

-- Operator invalidation. Append-only: an invalidation never deletes or edits an
-- attempt (invariant 9 -- terminal attempts are never mutated). It sets a
-- CUTOFF: any source that ended at or before invalidated_at is no longer
-- eligible. Scoped by namespace always, and then by the narrowest selector the
-- operator gave -- one exact cache_key, or a task_ref (optionally one version).
CREATE TABLE IF NOT EXISTS memo_invalidations (
    id             BIGSERIAL PRIMARY KEY,
    namespace      TEXT NOT NULL,
    tenant_id      UUID NOT NULL REFERENCES tenants(id),
    task_ref       TEXT,
    task_version   INT,
    cache_key      TEXT,
    reason         TEXT NOT NULL DEFAULT '',
    -- user:<sub> | token:<id> | ... -- every invalidation is attributable.
    actor          TEXT NOT NULL,
    invalidated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- An invalidation that selects nothing would silently do nothing; one that
    -- names a version without a task_ref would select across every task.
    CHECK (task_ref IS NOT NULL OR cache_key IS NOT NULL),
    CHECK (task_version IS NULL OR task_ref IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS memo_invalidations_ref_idx
    ON memo_invalidations (namespace, task_ref, invalidated_at DESC);
CREATE INDEX IF NOT EXISTS memo_invalidations_key_idx
    ON memo_invalidations (namespace, cache_key, invalidated_at DESC)
    WHERE cache_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS memo_invalidations_tenant_idx ON memo_invalidations (tenant_id);

DROP TRIGGER IF EXISTS tenant_assignment ON memo_invalidations;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON memo_invalidations
    FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();

-- ─── Worker quarantine ───────────────────────────────────────────────────────

-- One row per quarantine episode: the audit trail and the source of truth for
-- when it ends. workers.state = 'QUARANTINED' is the placement-facing
-- projection; THIS is why, since when, until when, and who released it.
--
-- hostname and namespace are copied onto the row because quarantine follows the
-- HOST, not the session. Worker identity is per-session (§5.5), so a poison host
-- that restarts re-registers under a new worker_id; keying quarantine on the id
-- alone would let a reboot launder a broken GPU driver straight back into the
-- pool. A new session from a host with an active quarantine inherits it.
CREATE TABLE IF NOT EXISTS worker_quarantines (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    worker_id       UUID NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    namespace       TEXT NOT NULL,
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    hostname        TEXT NOT NULL,
    -- auto:      the poison-worker detector (evidence says why)
    -- manual:    an operator, via the API
    -- inherited: a new session from a host already quarantined
    trigger_kind    TEXT NOT NULL CHECK (trigger_kind IN ('auto','manual','inherited')),
    reason          TEXT NOT NULL,
    evidence        JSONB NOT NULL DEFAULT '{}',
    actor           TEXT NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- NULL means indefinite: only an operator releases it.
    expires_at      TIMESTAMPTZ,
    released_at     TIMESTAMPTZ,
    released_by     TEXT,
    release_reason  TEXT,
    CHECK (expires_at IS NULL OR expires_at > started_at),
    CHECK ((released_at IS NULL) = (released_by IS NULL))
);
-- At most ONE open episode per worker. This is what makes two resource-manager
-- replicas detecting the same poison worker converge on one quarantine instead
-- of two: the loser's INSERT conflicts.
CREATE UNIQUE INDEX IF NOT EXISTS worker_quarantines_open_uq
    ON worker_quarantines (worker_id) WHERE released_at IS NULL;
-- Inheritance lookup on (re-)registration.
CREATE INDEX IF NOT EXISTS worker_quarantines_host_open_idx
    ON worker_quarantines (namespace, hostname) WHERE released_at IS NULL;
-- Expiry sweep.
CREATE INDEX IF NOT EXISTS worker_quarantines_expiry_idx
    ON worker_quarantines (expires_at) WHERE released_at IS NULL AND expires_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS worker_quarantines_tenant_idx ON worker_quarantines (tenant_id);

DROP TRIGGER IF EXISTS tenant_assignment ON worker_quarantines;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON worker_quarantines
    FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();

-- The detector's evidence scan: "this worker's terminal attempts in the last N
-- minutes". task_attempts has no index that answers it (idx_attempts_worker_active
-- covers only DISPATCHED/RUNNING), and without one each sweep is a sequential scan
-- of the largest table. Partial, so attempts that never reached a worker do not
-- bloat it. CONCURRENTLY -- see the header.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_attempts_worker_ended
    ON task_attempts (worker_id, ended_at)
    WHERE worker_id IS NOT NULL AND ended_at IS NOT NULL;

-- ─── Grants (0005 style) ─────────────────────────────────────────────────────
DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT ON memo_invalidations, worker_quarantines TO %I', r);
        END IF;
    END LOOP;
    -- memo_invalidations: owned by the dag-engine, which performs the lookup it
    -- gates. INSERT only: an invalidation is history.
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_dag_engine') THEN
        GRANT INSERT ON memo_invalidations TO nexus_dag_engine;
        GRANT USAGE ON SEQUENCE memo_invalidations_id_seq TO nexus_dag_engine;
    END IF;
    -- worker_quarantines: owned by the resource-manager, which owns workers and
    -- is therefore the only service that can move workers.state atomically with
    -- the episode row. UPDATE is needed to close an episode; DELETE is not
    -- granted -- the trail is not erasable by the service that writes it.
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_resource_manager') THEN
        GRANT INSERT, UPDATE ON worker_quarantines TO nexus_resource_manager;
    END IF;
END $$;
