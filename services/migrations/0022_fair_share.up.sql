-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- migrate: no-transaction
-- Weighted fair share across namespaces in the dispatch claim (ADR-0017).
--
-- Two additive objects and nothing else: no column is added to, altered on or
-- backfilled into a hot table, so a mixed-version fleet is unaffected (ADR-0011
-- rule 5). An old scheduler simply never reads either object.
--
-- CREATE INDEX CONCURRENTLY cannot run inside a transaction block, so this file
-- declares `migrate: no-transaction` and every statement is safe to re-run: a
-- retry is how a partial application is recovered.

-- A CONCURRENTLY build that fails part way leaves an INVALID index behind, and
-- `IF NOT EXISTS` would then silently keep it -- an index the planner never
-- uses, so the claim would quietly fall back to a scan of every in-flight
-- attempt. Drop a leftover invalid copy first so the retry really rebuilds.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
         WHERE c.relname = 'idx_attempts_inflight_by_tenant' AND NOT i.indisvalid
    ) THEN
        EXECUTE 'DROP INDEX idx_attempts_inflight_by_tenant';
    END IF;
END $$;

-- The fair-share term counts each namespace's in-flight attempts on the queue
-- being claimed, once per claim. Without this index that count is a scan of
-- every DISPATCHED/RUNNING row on the queue inside the claim transaction --
-- exactly the lock-holding time §5.3 exists to keep short. Partial on the two
-- active states so terminal history (the overwhelming majority of rows) never
-- enters it, and keyed (queue, tenant_id) so the GROUP BY is an index-only walk.
-- tenant_id rather than namespace: tenants and namespaces are 1:1 (0013), and
-- tenant_id is already on task_attempts, so the count needs no join.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_attempts_inflight_by_tenant
    ON task_attempts (queue, tenant_id)
    WHERE state IN ('DISPATCHED', 'RUNNING');

-- Namespace weights are NOT a new table. They live in the namespace's existing
-- platform policy (platform_config.config -> scheduler -> fair_share_weight),
-- written through the audited, revisioned platform config API. A second table
-- would be a second source of truth for the same number.
--
-- The view exists so the scheduler reads a TYPED, CLAMPED integer instead of
-- parsing JSON inside the claim: a malformed stored value must degrade to the
-- neutral weight 1, never raise an error, because an error here would fail
-- every claim on the queue and stop dispatch for every tenant at once. The
-- clamp to [1,100] is applied to numeric BEFORE the int cast so that even an
-- absurd stored number cannot overflow.
CREATE OR REPLACE VIEW scheduler_namespace_weights AS
SELECT pc.tenant_id,
       pc.namespace,
       CASE WHEN jsonb_typeof(pc.config -> 'scheduler' -> 'fair_share_weight') = 'number'
            THEN LEAST(100, GREATEST(1,
                     floor((pc.config -> 'scheduler' ->> 'fair_share_weight')::numeric)))::int
            ELSE 1
       END AS weight
  FROM platform_config pc;

-- 0005 granted SELECT ON ALL TABLES at the time it ran; that does not reach
-- relations created afterwards, so the view needs its own grant. Every service
-- reads everything (ADR-0011), and the scheduler is the one that needs it.
-- Guarded per role, like 0005 and 0010, so a database without the service
-- roles (a developer's) still migrates.
DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT ON scheduler_namespace_weights TO %I', r);
        END IF;
    END LOOP;
END $$;
