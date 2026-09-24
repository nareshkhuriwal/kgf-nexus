-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Cluster registry, observed Kubernetes state, and the audit trail of the
-- worker-scaling controller (docs/CLUSTER-SCALING.md, "Controller phase").
--
-- DESIRED AND OBSERVED ARE DIFFERENT TABLES WITH DIFFERENT OWNERS, on purpose.
-- What an operator asked for and what a cluster reports are two independent
-- facts that change at different times and are written by different
-- principals. Folding them into one row would let a controller report
-- overwrite an operator edit (or the reverse) and would make "has the cluster
-- caught up with revision N?" unanswerable.
--
--   DESIRED  (api-gateway owns; operator intent, like platform_config in 0010):
--     clusters, cluster_namespace_bindings
--   OBSERVED (resource-manager owns; it is the capacity/fleet owner, ADR-0011):
--     cluster_status, cluster_observations, cluster_scaling_state,
--     cluster_reconcile_errors, cluster_scaling_events
--
-- The in-cluster controller holds NO database credential. It reads desired
-- state and writes observed state exclusively through the resource-manager's
-- gRPC API, so a compromised controller pod cannot touch any table directly.
--
-- TENANCY: a cluster belongs to the namespace that registered it (and so to
-- that namespace's tenant, via the 0014 trigger). Sharing one physical cluster
-- between tenants needs platform-operator authorization that does not exist
-- yet; see docs/CLUSTER-SCALING.md.
--
-- ADDITIVE (ADR-0011 rule 5): new tables only. No existing table, column or row
-- is touched, so replicas that predate this migration keep working unchanged.

-- ── desired state (api-gateway) ─────────────────────────────────────────────

CREATE TABLE clusters (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The namespace that registered, and therefore administers, this cluster.
    namespace            TEXT NOT NULL,
    tenant_id            UUID NOT NULL REFERENCES tenants(id),
    -- The name the controller is started with (NEXUS_CLUSTER_NAME). Globally
    -- unique because a controller finds its assignment by name alone.
    name                 TEXT NOT NULL UNIQUE
                         CHECK (name ~ '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$' AND length(name) <= 63),
    provider             TEXT NOT NULL CHECK (provider IN ('aks','eks','kubernetes')),
    -- e.g. /subscriptions/<id>/resourceGroups/<rg>/providers/Microsoft.ContainerService/managedClusters/<name>
    -- Informational: Nexus never calls the cloud provider with it.
    provider_resource_id TEXT NOT NULL DEFAULT '' CHECK (length(provider_resource_id) <= 1024),
    -- The kube-system namespace UID of the cluster the operator INTENDS. A
    -- controller whose observed UID differs is pointed at the wrong cluster
    -- (a copied kubeconfig, a restored backup), and apply is refused.
    expected_cluster_uid TEXT NOT NULL DEFAULT '' CHECK (length(expected_cluster_uid) <= 64),
    -- Where the scheduler Deployment and the controller itself run.
    control_plane_namespace TEXT NOT NULL DEFAULT 'kgf-nexus'
                         CHECK (control_plane_namespace ~ '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'),
    -- 'plan' is the default and only reports what it would do. 'apply' is an
    -- explicit per-cluster opt-in, and additionally needs the controller's own
    -- NEXUS_CONTROLLER_ALLOW_APPLY and the apply RoleBinding (belt and braces:
    -- no single database edit can turn a read-only controller into a mutating one).
    mode                 TEXT NOT NULL DEFAULT 'plan' CHECK (mode IN ('plan','apply')),
    -- Bumped on every change to this row or its bindings. The controller echoes
    -- back the revision it last reconciled into cluster_status.observed_revision.
    desired_revision     BIGINT NOT NULL DEFAULT 1 CHECK (desired_revision > 0),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by           TEXT NOT NULL DEFAULT '',
    -- Target of the bindings' composite foreign key below.
    UNIQUE (id, namespace)
);
CREATE INDEX idx_clusters_namespace ON clusters (namespace);
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON clusters
    FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();

-- One row per worker Deployment the controller may scale. The two UNIQUE
-- constraints are the "no competing controllers on one target" rule made
-- structural: a Deployment has at most one binding, and a Nexus pool is scaled
-- by at most one cluster.
CREATE TABLE cluster_namespace_bindings (
    cluster_id           UUID NOT NULL,
    -- The Nexus namespace whose queue demand drives this Deployment.
    namespace            TEXT NOT NULL,
    tenant_id            UUID NOT NULL REFERENCES tenants(id),
    kubernetes_namespace TEXT NOT NULL
                         CHECK (kubernetes_namespace ~ '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$' AND length(kubernetes_namespace) <= 63),
    worker_deployment    TEXT NOT NULL
                         CHECK (worker_deployment ~ '^[a-z0-9]([-.a-z0-9]*[a-z0-9])?$' AND length(worker_deployment) <= 253),
    -- A pool name from platform_config.config->pools; its policy (min/max,
    -- slots, idle cooldown, step) is read from there, not duplicated here.
    pool                 TEXT NOT NULL,
    -- Registry-level ceiling ON TOP of the pool policy. The smaller wins, so a
    -- pool policy edit can never exceed what the cluster owner allowed.
    max_replicas         INT NOT NULL DEFAULT 10 CHECK (max_replicas BETWEEN 0 AND 10000),
    max_scale_down_step  INT NOT NULL DEFAULT 1 CHECK (max_scale_down_step BETWEEN 1 AND 100),
    scale_up_cooldown_seconds   INT NOT NULL DEFAULT 30  CHECK (scale_up_cooldown_seconds BETWEEN 0 AND 3600),
    scale_down_cooldown_seconds INT NOT NULL DEFAULT 300 CHECK (scale_down_cooldown_seconds BETWEEN 60 AND 86400),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster_id, kubernetes_namespace, worker_deployment),
    UNIQUE (namespace, pool),
    -- A binding's Nexus namespace MUST be the cluster's owning namespace. As a
    -- composite key rather than application code, so no gateway bug can let one
    -- tenant's queue demand drive another tenant's cluster.
    FOREIGN KEY (cluster_id, namespace) REFERENCES clusters(id, namespace) ON DELETE CASCADE
);
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON cluster_namespace_bindings
    FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();

-- ── observed state (resource-manager) ───────────────────────────────────────

-- One row per cluster: the controller's latest self-report.
CREATE TABLE cluster_status (
    cluster_id           UUID PRIMARY KEY REFERENCES clusters(id) ON DELETE CASCADE,
    tenant_id            UUID NOT NULL REFERENCES tenants(id),
    -- The desired_revision the controller last fully reconciled. Lagging
    -- desired_revision means "not caught up", which the console shows as such.
    observed_revision    BIGINT NOT NULL DEFAULT 0,
    observed_cluster_uid TEXT NOT NULL DEFAULT '',
    kubernetes_version   TEXT NOT NULL DEFAULT '',
    -- The pod name of the leader that sent the report.
    controller_identity  TEXT NOT NULL DEFAULT '',
    -- The mode the controller actually ran in, which can be narrower than the
    -- registry's (apply requested, but the controller does not allow it).
    effective_mode       TEXT NOT NULL DEFAULT 'plan' CHECK (effective_mode IN ('plan','apply')),
    effective_mode_reason TEXT NOT NULL DEFAULT '',
    last_report_at       TIMESTAMPTZ,
    last_error           TEXT NOT NULL DEFAULT '',
    last_error_at        TIMESTAMPTZ
);
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON cluster_status
    FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('cluster_id','clusters');

-- The latest snapshot of each reported Kubernetes object. Replaced per kind on
-- every complete report, so a deleted Deployment disappears instead of lingering
-- as a ghost that looks live.
CREATE TABLE cluster_observations (
    cluster_id           UUID NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    tenant_id            UUID NOT NULL REFERENCES tenants(id),
    kind                 TEXT NOT NULL CHECK (kind IN ('SchedulerDeployment','WorkerDeployment','Deployment','Node','Job','HorizontalPodAutoscaler')),
    kubernetes_namespace TEXT NOT NULL DEFAULT '',
    name                 TEXT NOT NULL,
    uid                  TEXT NOT NULL DEFAULT '',
    generation           BIGINT NOT NULL DEFAULT 0,
    observed_generation  BIGINT NOT NULL DEFAULT 0,
    spec_replicas        INT,
    ready_replicas       INT,
    ready                BOOLEAN NOT NULL DEFAULT false,
    resource_version     TEXT NOT NULL DEFAULT '',
    -- Bounded, kind-specific summary (conditions, capacity, labels). Never a
    -- full object: secrets referenced by env, annotations and managedFields do
    -- not belong in the metadata store.
    summary              JSONB NOT NULL DEFAULT '{}',
    object_created_at    TIMESTAMPTZ,
    observed_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster_id, kind, kubernetes_namespace, name)
);
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON cluster_observations
    FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('cluster_id','clusters');

-- Controller memory that must survive a restart or a leader change. Without it
-- a restarted controller forgets its cooldown and its idle clock, and a crash
-- loop becomes a scaling loop.
CREATE TABLE cluster_scaling_state (
    cluster_id           UUID NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    tenant_id            UUID NOT NULL REFERENCES tenants(id),
    kubernetes_namespace TEXT NOT NULL,
    worker_deployment    TEXT NOT NULL,
    last_scale_at        TIMESTAMPTZ,
    last_scale_direction TEXT NOT NULL DEFAULT '' CHECK (last_scale_direction IN ('','out','in')),
    idle_since           TIMESTAMPTZ,
    observed_platform_revision BIGINT NOT NULL DEFAULT 0,
    last_decision        JSONB NOT NULL DEFAULT '{}',
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster_id, kubernetes_namespace, worker_deployment)
);
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON cluster_scaling_state
    FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('cluster_id','clusters');

-- Reconciliation failures, retained for the console. Pruned by age by its
-- owner, which is why the owner may DELETE (unlike the event table below).
CREATE TABLE cluster_reconcile_errors (
    id                   BIGSERIAL PRIMARY KEY,
    cluster_id           UUID NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    tenant_id            UUID NOT NULL REFERENCES tenants(id),
    target               TEXT NOT NULL DEFAULT '',
    message              TEXT NOT NULL CHECK (length(message) <= 4096),
    actor                TEXT NOT NULL DEFAULT '',
    occurred_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_cluster_reconcile_errors_recent ON cluster_reconcile_errors (cluster_id, occurred_at DESC);
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON cluster_reconcile_errors
    FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('cluster_id','clusters');

-- Every scaling decision that changed, and every action taken or withheld.
-- APPEND-ONLY, enforced by an INSERT-only grant (the run_events pattern): the
-- record of what an autoscaler did must not be editable by the autoscaler.
CREATE TABLE cluster_scaling_events (
    id                   BIGSERIAL PRIMARY KEY,
    cluster_id           UUID NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    tenant_id            UUID NOT NULL REFERENCES tenants(id),
    kubernetes_namespace TEXT NOT NULL DEFAULT '',
    worker_deployment    TEXT NOT NULL DEFAULT '',
    -- plan | scale_out | drain | scale_in | hold | refused
    action               TEXT NOT NULL,
    mode                 TEXT NOT NULL CHECK (mode IN ('plan','apply')),
    applied              BOOLEAN NOT NULL DEFAULT false,
    from_replicas        INT,
    to_replicas          INT,
    reason               TEXT NOT NULL DEFAULT '',
    actor                TEXT NOT NULL DEFAULT '',
    detail               JSONB NOT NULL DEFAULT '{}',
    occurred_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_cluster_scaling_events_recent ON cluster_scaling_events (cluster_id, occurred_at DESC);
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON cluster_scaling_events
    FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('cluster_id','clusters');

-- ── grants (the 0005 pattern) ───────────────────────────────────────────────
-- Reads are unrestricted per ADR-0011's matrix; writes follow ownership above.
-- Guarded on role existence so a database without 0005's roles still migrates.
DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT ON clusters, cluster_namespace_bindings, cluster_status, '
                'cluster_observations, cluster_scaling_state, cluster_reconcile_errors, '
                'cluster_scaling_events TO %I', r);
        END IF;
    END LOOP;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_api_gateway') THEN
        GRANT INSERT, UPDATE, DELETE ON clusters, cluster_namespace_bindings TO nexus_api_gateway;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_resource_manager') THEN
        GRANT INSERT, UPDATE, DELETE ON cluster_status, cluster_observations,
            cluster_scaling_state, cluster_reconcile_errors TO nexus_resource_manager;
        GRANT INSERT ON cluster_scaling_events TO nexus_resource_manager;
        GRANT USAGE ON SEQUENCE cluster_reconcile_errors_id_seq, cluster_scaling_events_id_seq
            TO nexus_resource_manager;
        -- Pool policy (min/max, slots, profile) lives in platform_config, and
        -- the resource manager resolves each binding against it. 0010 granted
        -- that table to the gateway only; this is a READ, which ADR-0011's
        -- matrix already allows every service.
        GRANT SELECT ON platform_config TO nexus_resource_manager;
    END IF;
END $$;
