-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- KGF.Nexus workspaces (ADR-0034): one Nexus tenant namespace per KGF
-- workspace resource (/subscriptions/{sub}/resourceGroups/{rg}/providers/
-- KGF.Nexus/workspaces/{ws}), provisioned explicitly by the KGF.Nexus
-- provider front (PUT /v1/kgf/workspace) and never on first use.
--
-- A new table rather than a column on kgf_scopes: kgf_scopes is unique per
-- (organization, subscription, resource_group) -- the v1 mapping, one
-- namespace per resource group -- and several workspaces live in one resource
-- group. Keeping the two mappings in two tables leaves every v1 row, its
-- uniqueness and its readers exactly as they were.
--
-- ADDITIVE (ADR-0011 rule 5): one new table. An old gateway replica never
-- reads it; it refuses workspace contexts (unknown `res` issuer) and so never
-- touches a workspace namespace.
--
-- OWNERSHIP: api-gateway, like kgf_scopes (0032).
CREATE TABLE IF NOT EXISTS kgf_workspaces (
    namespace      TEXT PRIMARY KEY REFERENCES tenants(namespace),
    tenant_id      UUID REFERENCES tenants(id),
    organization   TEXT NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    subscription   TEXT NOT NULL CHECK (length(subscription) BETWEEN 1 AND 128 AND subscription = lower(subscription)),
    resource_group TEXT NOT NULL CHECK (length(resource_group) BETWEEN 1 AND 128 AND resource_group = lower(resource_group)),
    workspace      TEXT NOT NULL CHECK (workspace ~ '^[a-z][a-z0-9-]{2,47}$'),
    -- The normalised (lower-case) resource id, hashed into the namespace.
    resource_id    TEXT NOT NULL CHECK (length(resource_id) BETWEEN 1 AND 512 AND resource_id = lower(resource_id)),
    state          TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'deleted')),
    created_by     TEXT NOT NULL DEFAULT '' CHECK (length(created_by) <= 256),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_by     TEXT NOT NULL DEFAULT '' CHECK (length(deleted_by) <= 256),
    deleted_at     TIMESTAMPTZ,
    CHECK ((state = 'deleted') = (deleted_at IS NOT NULL)),
    -- One resource id, one namespace, per organization. A deleted row keeps
    -- its id: re-creating a deleted workspace is refused (workspace_deleted).
    UNIQUE (organization, resource_id)
);
CREATE INDEX IF NOT EXISTS kgf_workspaces_scope_idx ON kgf_workspaces (organization, subscription, resource_group);
DROP TRIGGER IF EXISTS tenant_assignment ON kgf_workspaces;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON kgf_workspaces
    FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();

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
            EXECUTE format('GRANT SELECT ON kgf_workspaces TO %I', r);
        END IF;
    END LOOP;
    -- The gateway provisions (INSERT) and deletes (UPDATE of the state
    -- columns only; a workspace row is never removed or re-keyed).
    r := nexus_role('api_gateway');
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
        EXECUTE format('GRANT INSERT ON kgf_workspaces TO %I', r);
        EXECUTE format('GRANT UPDATE (state, deleted_at, deleted_by) ON kgf_workspaces TO %I', r);
    END IF;
END $$;
