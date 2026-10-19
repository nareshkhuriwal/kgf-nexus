-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- The service-instance registry (ADR-0037): one row per running control-plane
-- process -- every api-gateway, workflow-manager, dag-engine, scheduler,
-- resource-manager and event-service replica -- written by that process
-- itself every 10 seconds.
--
-- WHY. Nothing in the metadata store says which control-plane processes exist.
-- Workers register (ADR-0005) and the cluster controller reports (0018), but a
-- scheduler replica is visible only to whoever can reach its /readyz, and the
-- gateway's fleet view (GET /v1/health/fleet) is its own gRPC connectivity to
-- one address per service -- a load-balanced address, behind which a dead
-- replica is indistinguishable from a live one. The console's Infrastructure
-- page needs every replica: where it runs, which version, whether it is ready
-- and when it was last heard from.
--
-- WHAT. A row is keyed by (component, cluster, pod_namespace, instance), the
-- process's runtime identity (NEXUS_POD_NAME, NEXUS_POD_NAMESPACE,
-- NEXUS_CLUSTER_NAME; the host name when there is no pod). A restarted pod of
-- a StatefulSet reuses its row; a Deployment's replacement pod adds one.
-- `ready` is the process's own readiness -- the answer its /readyz gives --
-- and `last_seen_at` is the DATABASE clock at the latest beat, so "stale" (no
-- beat for 30 s) is judged against one clock whatever the hosts' clocks say
-- (invariant 7). A graceful shutdown writes ready = false before the process
-- exits. Rows not seen for 24 hours are pruned by the processes themselves.
--
-- PLATFORM-SCOPED: no tenant_id. These rows describe the installation, not a
-- customer, like `tenants` and `platform_operators` (docs/MULTITENANCY.md,
-- "shared registries"). The gateway exposes them to a namespace's
-- infrastructure.view holders as component health; placement identity is
-- shown only as ADR-0037 §4 decides.
--
-- OWNERSHIP (ADR-0011, ADR-0037 §2). Every service writes, and only ever
-- writes, its OWN row: one writer per row, the process the row describes. The
-- grants are therefore INSERT/UPDATE/DELETE for every service role rather than
-- one owner. DELETE is the 24-hour prune, which any replica may run. The
-- table carries no authoritative state -- nothing reads it to make a
-- scheduling, fencing or tenancy decision -- so a forged row can mislead a
-- dashboard and nothing else, and a service able to forge one already holds
-- write access to tables that matter far more.
--
-- ADDITIVE (ADR-0011 rule 5): one new table and its grants. A process built
-- before this migration never writes it; a process built after it against a
-- database without it logs and meters every failed beat
-- (nexus_instance_heartbeat_failures_total) and runs normally.

CREATE TABLE IF NOT EXISTS service_instances (
    -- api-gateway | workflow-manager | dag-engine | scheduler |
    -- resource-manager | event-service. A format CHECK rather than a closed
    -- list, so a future component (the cluster controller) is not a
    -- constraint swap; the gateway lists the six it knows.
    component     TEXT NOT NULL CHECK (component ~ '^[a-z][a-z0-9-]{1,62}$'),
    -- The pod name, else the host name: never empty.
    instance      TEXT NOT NULL CHECK (octet_length(instance) BETWEEN 1 AND 253),
    -- Empty when unknown (outside Kubernetes, or the downward API not wired).
    pod_namespace TEXT NOT NULL DEFAULT '' CHECK (octet_length(pod_namespace) <= 253),
    node          TEXT NOT NULL DEFAULT '' CHECK (octet_length(node) <= 253),
    cluster       TEXT NOT NULL DEFAULT '' CHECK (octet_length(cluster) <= 253),
    -- The build's stamped version ("dev" for an unstamped build).
    version       TEXT NOT NULL DEFAULT '' CHECK (octet_length(version) <= 128),
    -- The PROCESS start, from the process's own clock: informational.
    started_at    TIMESTAMPTZ NOT NULL,
    -- The latest beat, from the database clock (the writer passes none).
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    ready         BOOLEAN NOT NULL DEFAULT false,
    -- Component-specific, documented keys (ADR-0037 §3), bounded so a bug in
    -- one writer cannot grow the row without limit.
    detail        JSONB NOT NULL DEFAULT '{}'
                  CHECK (jsonb_typeof(detail) = 'object' AND octet_length(detail::text) <= 16384),
    PRIMARY KEY (component, cluster, pod_namespace, instance)
);

-- No further index: the table holds one row per process seen in the last
-- 24 hours (tens, or a few hundred across rolling deploys), so the listing and
-- the prune are scans of a handful of pages.

-- ── grants, through nexus_role (0038) ──────────────────────────────────────
DO $$
DECLARE
    svc TEXT;
    r   TEXT;
BEGIN
    -- Every service reads it (ADR-0011's matrix) and writes its own row.
    FOREACH svc IN ARRAY nexus_service_roles() LOOP
        r := nexus_role(svc);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON service_instances TO %I', r);
        END IF;
    END LOOP;
END $$;
