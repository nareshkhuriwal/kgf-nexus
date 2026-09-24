-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
--
-- Dynamic expansion (ARCHITECTURE §5.4).
--
-- A map TEMPLATE node fans out into one child task_nodes row per element of a
-- runtime collection. The children themselves need no new schema: task_nodes has
-- carried parent_node_id and map_index since 0001 precisely so that adding
-- expansion would not be a migration of the hot table.
--
-- What they DO need is a record of the expansion as a single fact:
--
--   * IDEMPOTENCY. The primary key (run_id, template_node_id) is what makes an
--     expansion happen at most once per template per run. Two DAG-engine replicas
--     reconciling the same run, a duplicate completion report for the upstream,
--     and a replay after a crash all converge on this row: the loser's INSERT
--     conflicts and its whole transaction -- children included -- rolls back.
--   * ATOMICITY. The row is written in the SAME transaction as the children and
--     the template's PENDING->RUNNING compare-and-set. A crash before commit
--     leaves no trace (the template is still PENDING and is re-expanded, from the
--     same immutable upstream output, to byte-identical children); a crash after
--     commit leaves a complete expansion. There is no partial state.
--   * HISTORY. §5.4 requires the materialized DAG of a historical run to be
--     reconstructible. The row names the exact upstream attempt whose output was
--     expanded and how many items it had, or why the expansion was refused.
--
-- OWNERSHIP: dag-engine (ADR-0011). It owns task_nodes and task_edges, and an
-- expansion is a change to exactly those tables plus this record of it.
--
-- ADDITIVE (ADR-0011 rule 5): one new table, nothing existing is altered. A
-- replica that predates this migration never writes a MapSpec-bearing spec (the
-- gateway that accepts one is the same release), so it never reads the table.

CREATE TABLE IF NOT EXISTS task_expansions (
    run_id            UUID NOT NULL,
    template_node_id  TEXT NOT NULL,
    -- Assigned by the tenant trigger below, exactly as every other run-scoped
    -- table's tenant_id is (0014). NOT NULL is safe on a new table: no replica
    -- that predates the column can be writing to it.
    tenant_id         UUID NOT NULL REFERENCES tenants(id),
    source_node_id    TEXT NOT NULL,
    source_output     TEXT NOT NULL,
    -- The upstream attempt whose output was expanded. Nullable because a refused
    -- expansion (collection_missing) may have no usable attempt to name.
    source_attempt_id UUID,
    item_count        INT  NOT NULL CHECK (item_count >= 0),
    max_fanout        INT  NOT NULL CHECK (max_fanout >= 0),
    -- EXPANDED: children materialized (possibly zero of them).
    -- FAILED:   refused deliberately; the template node is FAILED with `reason`.
    outcome           TEXT NOT NULL CHECK (outcome IN ('EXPANDED','FAILED')),
    reason            TEXT,
    detail            TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, template_node_id),
    -- Deleting a run cascades through task_nodes to here, so run deletion (0011)
    -- needs no new code to stay complete.
    FOREIGN KEY (run_id, template_node_id)
        REFERENCES task_nodes (run_id, node_id) ON DELETE CASCADE,
    CHECK (outcome = 'EXPANDED' OR reason IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS task_expansions_tenant_idx ON task_expansions (tenant_id);

DROP TRIGGER IF EXISTS tenant_assignment ON task_expansions;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON task_expansions
    FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('run_id', 'workflow_runs');

-- Grants, in the 0005 style: every service reads, only the owner writes.
-- INSERT only for the owner -- an expansion is a historical fact, never edited.
-- Guarded on role existence so a database without the per-service roles (a bare
-- development instance) still migrates.
DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT ON task_expansions TO %I', r);
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_dag_engine') THEN
        GRANT INSERT ON task_expansions TO nexus_dag_engine;
    END IF;
END $$;
