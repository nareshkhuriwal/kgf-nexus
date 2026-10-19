-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Where each attempt's workload runs (ADR-0037): the executor, the registered
-- cluster and, on Kubernetes, the pod, its namespace and its node, as the
-- agent reports them through WorkerService.ReportPlacement.
--
-- WHY. The control plane knows which WORKER holds an attempt (task_attempts.
-- worker_id) and that worker's host name, and nothing more. For the
-- kubernetes executor the workload is a separate pod the cluster schedules
-- onto any node (ADR-0007), so "which node is this task on, and which pod do
-- I `kubectl logs`" had no answer short of searching the cluster by the
-- attempt label. The console's live task-runner view needs it per attempt.
--
-- WHY A SIDE TABLE AND NOT COLUMNS ON task_attempts: 0024's reason exactly.
-- Every terminal transition locks the attempt row FOR UPDATE ... SKIP LOCKED
-- and treats "no row" as "already handled"; a placement write that UPDATEd
-- task_attempts -- or held a foreign-key KEY SHARE lock on it -- could make a
-- result arriving in the same instant be skipped. This table references the
-- RUN, never locks the attempt, and keeps an agent's reports off the hottest
-- table in the system.
--
-- FENCED. The gateway writes a row only for the exact lease that passed the
-- token check (leases.id), while that lease is unexpired, and never lets a
-- report from an older lease epoch overwrite a newer one's. A zombie agent
-- cannot relocate an attempt it no longer owns.
--
-- OWNERSHIP (ADR-0011): the api-gateway, which receives the report, like
-- attempt_logs (0025). Every service may read it.
--
-- TENANCY: tenant_id, assigned from the attempt by 0014's trigger like every
-- attempt side table, so it is a customer-owned row that dies with its run.
--
-- ADDITIVE (ADR-0011 rule 5): one new table, its indexes, trigger and grants.
-- An old gateway never offers ReportPlacement (agents then report nothing); a
-- new gateway detects the table at startup and offers it only when present.

CREATE TABLE IF NOT EXISTS attempt_runtime (
    -- One row per attempt. Deliberately NOT a foreign key to task_attempts;
    -- see the header.
    attempt_id        UUID PRIMARY KEY,
    -- Run deletion (0011) cascades here, like attempt_progress.
    run_id            UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    tenant_id         UUID NOT NULL REFERENCES tenants(id),
    -- leases.epoch of the lease that wrote the row. A report carrying an older
    -- epoch than the stored one is ignored.
    lease_epoch       INT NOT NULL,
    -- kubernetes | docker | subprocess, or empty when the agent did not say.
    -- A format CHECK, not a closed list: executors are pluggable (ADR-0007).
    executor          TEXT NOT NULL DEFAULT '' CHECK (executor ~ '^[a-z0-9-]{0,32}$'),
    -- clusters.name of the agent's cluster (its NEXUS_CLUSTER_NAME).
    cluster           TEXT NOT NULL DEFAULT '' CHECK (octet_length(cluster) <= 253),
    -- Kubernetes names: a namespace is a DNS label, pods and nodes DNS
    -- subdomains. Empty when not known (a pod not yet scheduled has no node).
    k8s_namespace     TEXT NOT NULL DEFAULT '' CHECK (octet_length(k8s_namespace) <= 63),
    pod               TEXT NOT NULL DEFAULT '' CHECK (octet_length(pod) <= 253),
    k8s_node          TEXT NOT NULL DEFAULT '' CHECK (octet_length(k8s_node) <= 253),
    -- Control-plane time of the first and of the latest accepted report.
    first_reported_at TIMESTAMPTZ NOT NULL,
    reported_at       TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS attempt_runtime_run_idx ON attempt_runtime (run_id);
CREATE INDEX IF NOT EXISTS attempt_runtime_tenant_idx ON attempt_runtime (tenant_id);

-- The tenant trigger reads the attempt with a plain SELECT (0014's function),
-- which takes no row lock, so it keeps the header's no-lock property.
DROP TRIGGER IF EXISTS tenant_assignment ON attempt_runtime;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON attempt_runtime
    FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('attempt_id', 'task_attempts');

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
            EXECUTE format('GRANT SELECT ON attempt_runtime TO %I', r);
        END IF;
    END LOOP;
    -- The owner records placements (INSERT ... ON CONFLICT DO UPDATE). DELETE
    -- is the owner's too, so a manual repair needs no new grant; run deletion
    -- removes rows through the cascade.
    r := nexus_role('api_gateway');
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
        EXECUTE format('GRANT INSERT, UPDATE, DELETE ON attempt_runtime TO %I', r);
    END IF;
END $$;
