-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
--
-- The workload API (ADR-0032): usage units reported by a running task, shared
-- token-bucket rate limits, fenced task checkpoints, and free retries for
-- provider throttling (ERROR_CLASS_RATE_LIMITED).
--
-- ADDITIVE ONLY (ADR-0011 rule 5): three new tables, one view, one column with
-- a constant default on task_nodes (metadata-only on PostgreSQL 11+, no table
-- rewrite), triggers and grants. Nothing existing is altered, so a mixed-version
-- fleet is unaffected: an older scheduler never reads these objects and keeps
-- its previous ledger statement; an older gateway never serves the new RPCs.
-- A scheduler newer than this migration detects its absence at startup and
-- behaves as before it (DetectWorkloadSchema).
--
-- Every statement is written to be safely re-run.

-- ─── Usage units per attempt ─────────────────────────────────────────────────
-- The attempt's cumulative usage units (tokens:<provider>/<model>:input, ...),
-- merged per key with max by the scheduler, and copied into the usage ledger's
-- `custom` object at the terminal transition.
--
-- A SIDE TABLE, for 0024's reason: no report path may UPDATE task_attempts,
-- whose row every terminal transition locks. Deliberately NOT a foreign key to
-- task_attempts: the FK check's FOR KEY SHARE would be taken on every insert.
-- (The usage report does take FOR NO KEY UPDATE on the attempt, on purpose and
-- bounded, to serialize against the terminal transition; see ADR-0032 §2.)
CREATE TABLE IF NOT EXISTS attempt_usage (
    attempt_id   UUID PRIMARY KEY,
    -- Run deletion (0011) cascades here, like attempt_progress.
    run_id       UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    tenant_id    UUID NOT NULL REFERENCES tenants(id),
    -- {key: non-negative integer}. Validated by the scheduler (grammar, at most
    -- 64 keys, values <= 10^15); the CHECKs are the backstop.
    units        JSONB NOT NULL DEFAULT '{}'
                 CHECK (jsonb_typeof(units) = 'object'),
    lease_epoch  INT NOT NULL,
    reports      INT NOT NULL DEFAULT 0 CHECK (reports >= 0),
    reported_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS attempt_usage_run_idx ON attempt_usage (run_id);
CREATE INDEX IF NOT EXISTS attempt_usage_tenant_idx ON attempt_usage (tenant_id);

DROP TRIGGER IF EXISTS tenant_assignment ON attempt_usage;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON attempt_usage
    FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('attempt_id', 'task_attempts');

-- ─── Rate-limit buckets ──────────────────────────────────────────────────────
-- One row per (namespace, key): the two token-bucket levels as of refreshed_at.
-- A permit is consumed, never returned, so there is no release path that could
-- drift (ADR-0032 §3, contrast ADR-0021 alternative 2). The row is locked for
-- each read-modify-write; the levels are refilled from the DATABASE clock.
-- Levels may be negative (debt from an oversized request or a settlement),
-- bounded at -burst by the scheduler.
CREATE TABLE IF NOT EXISTS rate_limit_buckets (
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    key             TEXT NOT NULL CHECK (octet_length(key) BETWEEN 1 AND 200),
    request_level   DOUBLE PRECISION NOT NULL,
    token_level     DOUBLE PRECISION NOT NULL,
    refreshed_at    TIMESTAMPTZ NOT NULL,
    granted_requests BIGINT NOT NULL DEFAULT 0,
    granted_tokens   BIGINT NOT NULL DEFAULT 0,
    denied          BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, key)
);

-- The namespace's configured limits, from its platform policy
-- (config -> quotas -> rate_limits, written through the audited quotas API).
-- Parsed with 0027's never-raising nexus_quota_limit for the integer fields;
-- requests_per_second may be fractional, so it is read as a clamped numeric.
-- A malformed entry yields zeros, and a key with both rates zero is dropped:
-- failing open to "unlimited" is ADR-0017's direction, and the API validates
-- what it writes.
CREATE OR REPLACE VIEW namespace_rate_limits AS
SELECT pc.tenant_id,
       pc.namespace,
       e.key,
       CASE WHEN jsonb_typeof(e.value -> 'requests_per_second') = 'number'
            THEN LEAST(1000000::numeric, GREATEST(0::numeric, (e.value ->> 'requests_per_second')::numeric))::double precision
            ELSE 0::double precision END                                 AS requests_per_second,
       nexus_quota_limit(e.value -> 'tokens_per_minute')                 AS tokens_per_minute,
       nexus_quota_limit(e.value -> 'burst_requests')                    AS burst_requests,
       nexus_quota_limit(e.value -> 'burst_tokens')                      AS burst_tokens
  FROM platform_config pc,
       jsonb_each(CASE WHEN jsonb_typeof(pc.config -> 'quotas' -> 'rate_limits') = 'object'
                       THEN pc.config -> 'quotas' -> 'rate_limits' ELSE '{}'::jsonb END) e
 WHERE jsonb_typeof(e.value) = 'object';

-- ─── Task checkpoints ────────────────────────────────────────────────────────
-- One POINTER per idempotency key (stable across every retry of the logical
-- work, invariant 1) to an immutable result-store object
-- <namespace>/checkpoints/<idempotency_key>/<attempt_id>/<seq>. The pointer only
-- moves forward (attempt_number, seq); a stale attempt cannot move it back.
-- artifact-gc treats object_key as a reference source while the node has not
-- SUCCEEDED (ADR-0032 §4).
CREATE TABLE IF NOT EXISTS task_checkpoints (
    idempotency_key TEXT PRIMARY KEY,
    run_id          UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    node_id         TEXT NOT NULL,
    attempt_id      UUID NOT NULL,
    attempt_number  INT  NOT NULL,
    lease_epoch     INT  NOT NULL,
    seq             BIGINT NOT NULL CHECK (seq > 0),
    object_key      TEXT NOT NULL CHECK (octet_length(object_key) <= 1024),
    sha256          TEXT NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    size_bytes      BIGINT NOT NULL CHECK (size_bytes >= 0),
    saves           INT NOT NULL DEFAULT 1,
    saved_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS task_checkpoints_run_idx ON task_checkpoints (run_id, node_id);
CREATE INDEX IF NOT EXISTS task_checkpoints_tenant_idx ON task_checkpoints (tenant_id);

DROP TRIGGER IF EXISTS tenant_assignment ON task_checkpoints;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON task_checkpoints
    FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('run_id', 'workflow_runs');

-- ─── Free retries for provider throttling ────────────────────────────────────
-- RATE_LIMITED retries that were NOT charged to the retry budget. Both budget
-- checks subtract it (domain.ShouldRetry, the DAG engine's exhaustion test).
-- Reset to 0 by an operator retry, with attempts_used.
ALTER TABLE task_nodes ADD COLUMN IF NOT EXISTS free_retries INT NOT NULL DEFAULT 0;

-- ─── Grants (guarded per role, 0020-0027 style) ──────────────────────────────
DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT ON attempt_usage, rate_limit_buckets, task_checkpoints, namespace_rate_limits TO %I', r);
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_scheduler') THEN
        GRANT INSERT, UPDATE, DELETE ON attempt_usage, rate_limit_buckets, task_checkpoints TO nexus_scheduler;
        -- The scheduler records a free RATE_LIMITED retry in the terminal
        -- transaction (ADR-0032 §3). Column-scoped, like every other
        -- cross-owner grant. UPDATE (attempts_used), which the retry timer
        -- needs to charge the ordinary budget, is 0035's (F147; F149 is the
        -- same gap), granted once there rather than twice.
        GRANT UPDATE (free_retries) ON task_nodes TO nexus_scheduler;
    END IF;
END $$;
