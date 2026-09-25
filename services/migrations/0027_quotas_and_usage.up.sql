-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- migrate: no-transaction
-- Namespace quotas and the usage ledger (ADR-0021).
--
-- Additive only (ADR-0011 rule 5): one function, one view, one partial index on
-- task_attempts, one new table with its indexes, trigger and grants. No existing
-- column, row or object changes, so a mixed-version fleet is unaffected: an old
-- scheduler never reads the view or writes the ledger, and an old relay leaves
-- `usage_recorded` outbox rows for a new replica (0016's unknown-topic rule).
--
-- `migrate: no-transaction` because of CREATE INDEX CONCURRENTLY on the hottest
-- table. Every statement is therefore written to be safely re-run: a retry is
-- how a partial application is recovered.

-- ─── Quota limits ────────────────────────────────────────────────────────────
-- Quotas live in the namespace's existing platform policy
-- (platform_config.config -> quotas), written through the audited, revisioned
-- API -- not a second table that could disagree with it.
--
-- nexus_quota_limit turns one stored value into a limit the claim can compare
-- against: a JSON number is floored and clamped to [0, 2^62]; anything else
-- (absent, null, a string, an object) is 0, which means UNLIMITED. It never
-- raises, for ADR-0017's reason: an error inside the claim would stop dispatch
-- for every tenant, and a malformed value can only come from hand-edited SQL
-- because the API validates what it writes. Failing open to "unlimited" is the
-- direction that keeps the platform running; it is the same as not having set
-- the quota.
CREATE OR REPLACE FUNCTION nexus_quota_limit(v jsonb)
RETURNS bigint
LANGUAGE sql IMMUTABLE PARALLEL SAFE
AS $$
    SELECT CASE WHEN jsonb_typeof(v) = 'number'
                THEN LEAST(4611686018427387904::numeric,
                           GREATEST(0::numeric, floor(v::numeric)))::bigint
                ELSE 0::bigint
           END
$$;

-- One row per namespace that has a platform policy. Every limit column is 0 for
-- "unlimited"; max_custom holds only the positive custom limits, so '{}' means
-- none. The scheduler filters for rows with at least one concurrent limit, so a
-- namespace without quotas costs the claim one indexed lookup returning no row.
CREATE OR REPLACE VIEW namespace_quota_limits AS
SELECT pc.tenant_id,
       pc.namespace,
       nexus_quota_limit(pc.config -> 'quotas' -> 'max_inflight_attempts') AS max_inflight_attempts,
       nexus_quota_limit(pc.config -> 'quotas' -> 'max_cpu_millis')        AS max_cpu_millis,
       nexus_quota_limit(pc.config -> 'quotas' -> 'max_memory_bytes')      AS max_memory_bytes,
       nexus_quota_limit(pc.config -> 'quotas' -> 'max_gpu_count')         AS max_gpu_count,
       COALESCE((
           SELECT jsonb_object_agg(e.key, nexus_quota_limit(e.value))
             FROM jsonb_each(CASE WHEN jsonb_typeof(pc.config -> 'quotas' -> 'max_custom') = 'object'
                                  THEN pc.config -> 'quotas' -> 'max_custom'
                                  ELSE '{}'::jsonb END) e
            WHERE nexus_quota_limit(e.value) > 0
       ), '{}'::jsonb)                                                      AS max_custom,
       nexus_quota_limit(pc.config -> 'quotas' -> 'max_active_runs')       AS max_active_runs,
       nexus_quota_limit(pc.config -> 'quotas' -> 'max_runs_per_hour')     AS max_runs_per_hour,
       nexus_quota_limit(pc.config -> 'quotas' -> 'storage_soft_limit_bytes') AS storage_soft_limit_bytes
  FROM platform_config pc;

-- The pool that serves each queue in a namespace's policy, for the usage
-- ledger's pool attribution. A view rather than a read of platform_config in
-- the scheduler's statement, because platform_config is granted to the gateway
-- alone (0010) and a view runs with its owner's rights -- the same arrangement
-- as 0022's weights. Pool queues are unique within a policy (the API enforces
-- it); min() keeps the view total should hand-edited SQL break that.
CREATE OR REPLACE VIEW namespace_queue_pools AS
SELECT pc.tenant_id,
       p ->> 'queue'    AS queue,
       min(p ->> 'name') AS pool
  FROM platform_config pc,
       jsonb_array_elements(CASE WHEN jsonb_typeof(pc.config -> 'pools') = 'array'
                                 THEN pc.config -> 'pools' ELSE '[]'::jsonb END) p
 WHERE p ->> 'queue' IS NOT NULL
 GROUP BY pc.tenant_id, p ->> 'queue';

-- ─── In-flight usage per namespace ───────────────────────────────────────────
-- The claim sums a namespace's DISPATCHED/RUNNING attempts across EVERY queue.
-- ADR-0017's idx_attempts_inflight_by_tenant is keyed (queue, tenant_id), and
-- PostgreSQL 16 cannot skip-scan its leading column, so it would read the
-- in-flight rows of every tenant. Same partial predicate, tenant leading: the
-- read is O(this namespace's in-flight attempts).
--
-- A failed CONCURRENTLY build leaves an INVALID index that IF NOT EXISTS would
-- keep and the planner would never use; drop it so the retry really rebuilds.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
         WHERE c.relname = 'idx_attempts_inflight_tenant' AND NOT i.indisvalid
    ) THEN
        EXECUTE 'DROP INDEX idx_attempts_inflight_tenant';
    END IF;
END $$;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_attempts_inflight_tenant
    ON task_attempts (tenant_id)
    WHERE state IN ('DISPATCHED', 'RUNNING');

-- ─── The usage ledger ────────────────────────────────────────────────────────
-- One row per attempt that held a reservation, written by the scheduler in the
-- SAME transaction as the attempt's terminal transition (ApplyResult,
-- lease/worker-death expiry, cancel). UNIQUE(attempt_id) is the idempotency
-- key: a second insert for the same attempt is ON CONFLICT DO NOTHING.
--
-- Deliberately NO foreign key to task_attempts or workflow_runs: deleting a run
-- (0011) cascades its attempts, and what the run consumed must outlive it. The
-- tenant FK stays, because a tenant is never deleted while its rows exist.
--
-- Quantities are NUMERIC, not float: a billing system sums millions of these,
-- and every input (milliseconds, millicores, bytes) is an exact integer.
CREATE TABLE IF NOT EXISTS usage_ledger (
    id                   BIGSERIAL PRIMARY KEY,
    attempt_id           UUID NOT NULL UNIQUE,
    tenant_id            UUID NOT NULL REFERENCES tenants(id),
    namespace            TEXT NOT NULL,
    run_id               UUID NOT NULL,
    node_id              TEXT NOT NULL,
    attempt_number       INT  NOT NULL,
    -- Terminal attempt state: SUCCEEDED, FAILED, TIMED_OUT, LOST, CANCELLED.
    outcome              TEXT NOT NULL,
    queue                TEXT NOT NULL,
    -- The namespace policy's pool serving this queue, when one is configured.
    pool                 TEXT,
    worker_id            UUID,
    worker_hostname      TEXT,
    executor             TEXT,
    -- The reserved vector (the attempt's resource_request, which is exactly
    -- what the claim reserved).
    cpu_millis           BIGINT NOT NULL DEFAULT 0,
    memory_bytes         BIGINT NOT NULL DEFAULT 0,
    gpu_count            INT    NOT NULL DEFAULT 0,
    gpu_kind             TEXT,
    custom               JSONB  NOT NULL DEFAULT '{}',
    dispatched_at        TIMESTAMPTZ NOT NULL,
    started_at           TIMESTAMPTZ,
    ended_at             TIMESTAMPTZ NOT NULL,
    -- ended - started (or - dispatched when the attempt never started).
    wall_seconds         NUMERIC NOT NULL,
    -- ended - dispatched: how long the reservation held capacity.
    reserved_seconds     NUMERIC NOT NULL,
    cpu_core_seconds     NUMERIC NOT NULL,
    memory_gib_seconds   NUMERIC NOT NULL,
    gpu_seconds          NUMERIC NOT NULL,
    -- The worker's measured ExecutionMetrics, when the attempt ended by a
    -- worker report. NULL (not 0) for LOST and CANCELLED: unknown is not zero.
    measured_cpu_seconds       DOUBLE PRECISION,
    measured_peak_memory_bytes BIGINT,
    measured_wall_ms           BIGINT,
    measured_oom_killed        BOOLEAN,
    measured_exit_code         INT,
    recorded_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (ended_at >= dispatched_at),
    CHECK (wall_seconds >= 0 AND reserved_seconds >= 0)
);

-- Rollups read one tenant over a time range, attributed by end time.
CREATE INDEX IF NOT EXISTS usage_ledger_tenant_ended_idx ON usage_ledger (tenant_id, ended_at);
-- The platform-wide rollup and retention read by time alone.
CREATE INDEX IF NOT EXISTS usage_ledger_ended_idx ON usage_ledger (ended_at);
CREATE INDEX IF NOT EXISTS usage_ledger_run_idx ON usage_ledger (run_id);

-- Append-only. The grants below already withhold UPDATE and DELETE from every
-- service; this refuses UPDATE even for the table owner, because a corrected
-- bill is a new record, never a rewritten one. DELETE is left to the database
-- owner for retention (OPERATIONS §21).
CREATE OR REPLACE FUNCTION usage_ledger_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'usage_ledger is append-only: rows are never updated (ADR-0021)'
        USING ERRCODE = 'insufficient_privilege';
END $$;
DROP TRIGGER IF EXISTS usage_ledger_no_update ON usage_ledger;
CREATE TRIGGER usage_ledger_no_update BEFORE UPDATE ON usage_ledger
    FOR EACH ROW EXECUTE FUNCTION usage_ledger_append_only();

-- ─── Grants (0005 style, guarded per role so a developer database migrates) ──
-- Owned by the SCHEDULER (ADR-0021 amends ADR-0011's matrix): every write is
-- part of a terminal transition the scheduler already owns. INSERT only.
DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT ON namespace_quota_limits, namespace_queue_pools, usage_ledger TO %I', r);
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_scheduler') THEN
        GRANT INSERT ON usage_ledger TO nexus_scheduler;
        GRANT USAGE ON SEQUENCE usage_ledger_id_seq TO nexus_scheduler;
    END IF;
END $$;
