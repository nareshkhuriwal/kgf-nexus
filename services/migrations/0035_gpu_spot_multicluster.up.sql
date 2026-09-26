-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Fractional GPUs, spot capacity, node-local caches and multi-cluster
-- placement (ADR-0029, ADR-0030, ADR-0031).
--
-- ADDITIVE (ADR-0011 rule 5). Three nullable-free columns with constant
-- defaults (metadata-only on PostgreSQL 11+: no table rewrite, no backfill) and
-- three on cluster_status. Nothing existing is renamed, retyped or dropped, so
-- replicas that predate this migration keep working: none of them reads or
-- writes these columns.
--
-- Fractional-GPU ledgers need NO schema change: mig, gpu_share_* and
-- gpu_shares are new keys inside the existing JSONB resource vectors
-- (workers.capacity/committed, task_attempts.resource_request,
-- reservations.resources), written only when non-zero.

-- ADR-0030 §3: the preemption retry budget, separate from attempts_used.
-- Owned by the dag-engine like the rest of task_nodes; the scheduler charges
-- it when it creates the retry of a PREEMPTED attempt, exactly as it already
-- charges attempts_used for every other retry.
ALTER TABLE task_nodes
    ADD COLUMN IF NOT EXISTS preemptions_used INT NOT NULL DEFAULT 0;

-- ADR-0030 §4: artifact keys in the worker's node-local cache, as advertised on
-- the heartbeat (at most 256). Owned by the resource manager with the row.
ALTER TABLE workers
    ADD COLUMN IF NOT EXISTS cached_artifacts TEXT[] NOT NULL DEFAULT '{}';

-- ADR-0031: the cluster-controller's published placement labels and quota
-- headroom, observed state owned by the resource manager (0018).
ALTER TABLE cluster_status
    ADD COLUMN IF NOT EXISTS labels JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS headroom JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS headroom_observed_at TIMESTAMPTZ;

-- Grants (the 0005 pattern, guarded on role existence).
--
-- The scheduler creates retry attempts (scheduler_timers.go) and has always
-- charged task_nodes.attempts_used in that transaction, but 0005 granted that
-- column only to the dag-engine: under per-service roles every retry timer
-- failed with "permission denied" (CONTRACT-FINDINGS F147). Granted here with
-- the new preemption counter, column-scoped as 0005 does for event_seq.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_scheduler') THEN
        GRANT UPDATE (attempts_used, preemptions_used) ON task_nodes TO nexus_scheduler;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_dag_engine') THEN
        GRANT UPDATE (preemptions_used) ON task_nodes TO nexus_dag_engine;
    END IF;
END $$;
