-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- migrate: no-transaction

-- Nexus inside KGF Cloud (ADR-0024): the KGF scope each tenant namespace was
-- provisioned for, request ids on audit rows, and service accounts with
-- permission-ceilinged tokens for agents and CI.
--
-- `migrate: no-transaction` because the request-id index on audit_log, a
-- populated append-only table, is built CONCURRENTLY. Every statement is
-- therefore written to be re-run safely (IF NOT EXISTS, CREATE OR REPLACE,
-- DROP ... IF EXISTS before CREATE TRIGGER); a partial application is
-- recovered by running the migration again.
--
-- ADDITIVE (ADR-0011 rule 5): new tables, and nullable or defaulted columns on
-- audit_log and scoped_tokens. An old gateway replica never reads them. A
-- service-account token is written with an EMPTY scope list, so an old replica
-- that cannot interpret it grants it nothing; a ceilinged legacy token is
-- written with only the coarse scopes its ceiling implies (OPERATIONS §24.9).
--
-- OWNERSHIP: api-gateway, like every identity table (0007).

-- ─── KGF scope of a provisioned namespace ──────────────────────────────────
--
-- One row per Nexus tenant namespace created for a KGF (organization,
-- subscription, resource group). The namespace is derived from the triple
-- (ADR-0024 §3); this row is what makes it readable, and what refuses a
-- namespace being claimed for a second triple.
CREATE TABLE IF NOT EXISTS kgf_scopes (
    namespace      TEXT PRIMARY KEY REFERENCES tenants(namespace),
    tenant_id      UUID REFERENCES tenants(id),
    organization   TEXT NOT NULL CHECK (length(organization) BETWEEN 1 AND 128),
    subscription   TEXT NOT NULL CHECK (length(subscription) BETWEEN 1 AND 128),
    resource_group TEXT NOT NULL CHECK (length(resource_group) BETWEEN 1 AND 128),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (organization, subscription, resource_group)
);
CREATE INDEX IF NOT EXISTS kgf_scopes_organization_idx ON kgf_scopes (organization);
DROP TRIGGER IF EXISTS tenant_assignment ON kgf_scopes;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON kgf_scopes
    FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();

-- ─── Request ids on audit rows ─────────────────────────────────────────────
--
-- The KGF request id travels inside the context; recording it makes a KGF
-- support reference resolve to Nexus audit rows. Defaulted, so existing rows
-- and old replicas are unaffected.
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS request_id TEXT NOT NULL DEFAULT '';
-- A CONCURRENTLY build that failed leaves an INVALID index that IF NOT EXISTS
-- would silently keep; drop it so the retry rebuilds it.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
                WHERE c.relname = 'audit_log_request_id_idx' AND NOT i.indisvalid) THEN
        EXECUTE 'DROP INDEX audit_log_request_id_idx';
    END IF;
END $$;
CREATE INDEX CONCURRENTLY IF NOT EXISTS audit_log_request_id_idx
    ON audit_log (request_id) WHERE request_id <> '';

-- ─── Service accounts ───────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS service_accounts (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace   TEXT NOT NULL,
    tenant_id   UUID REFERENCES tenants(id),
    name        TEXT NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{1,62}$'),
    description TEXT NOT NULL DEFAULT '' CHECK (length(description) <= 500),
    disabled_at TIMESTAMPTZ,
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revision    BIGINT NOT NULL DEFAULT 1,
    UNIQUE (namespace, name)
);
CREATE INDEX IF NOT EXISTS service_accounts_tenant_idx ON service_accounts (tenant_id);
DROP TRIGGER IF EXISTS tenant_assignment ON service_accounts;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON service_accounts
    FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();

CREATE TABLE IF NOT EXISTS service_account_roles (
    service_account_id UUID PRIMARY KEY REFERENCES service_accounts(id) ON DELETE CASCADE,
    namespace          TEXT NOT NULL,
    tenant_id          UUID REFERENCES tenants(id),
    roles              JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(roles) = 'array'),
    revision           BIGINT NOT NULL DEFAULT 1
);
DROP TRIGGER IF EXISTS tenant_assignment ON service_account_roles;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON service_account_roles
    FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();

-- A token may belong to a service account and may carry an explicit
-- permission ceiling. Both nullable: NULL is exactly the pre-0032 token.
ALTER TABLE scoped_tokens ADD COLUMN IF NOT EXISTS service_account_id UUID REFERENCES service_accounts(id) ON DELETE CASCADE;
ALTER TABLE scoped_tokens ADD COLUMN IF NOT EXISTS permissions JSONB
    CHECK (permissions IS NULL OR jsonb_typeof(permissions) = 'array');

CREATE OR REPLACE FUNCTION public.scoped_token_account_namespace() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
BEGIN
    IF NEW.service_account_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM public.service_accounts s
         WHERE s.id = NEW.service_account_id AND s.namespace = NEW.namespace) THEN
        RAISE EXCEPTION 'service account is not in namespace %', NEW.namespace USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS service_account_namespace ON scoped_tokens;
CREATE TRIGGER service_account_namespace BEFORE INSERT OR UPDATE ON scoped_tokens
    FOR EACH ROW EXECUTE FUNCTION public.scoped_token_account_namespace();

-- ─── Grants (0005 style) ────────────────────────────────────────────────────
DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT ON kgf_scopes, service_accounts, service_account_roles TO %I', r);
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_api_gateway') THEN
        -- The gateway provisions KGF namespaces on first use, which is a
        -- tenant registration: it already holds INSERT on tenants (0013).
        GRANT INSERT ON kgf_scopes TO nexus_api_gateway;
        GRANT INSERT, UPDATE, DELETE ON service_accounts, service_account_roles TO nexus_api_gateway;
    END IF;
END $$;
