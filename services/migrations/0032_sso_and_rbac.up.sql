-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Tenant SSO (OIDC and SAML 2.0), IdP group mapping, service accounts, token
-- permission ceilings and cross-namespace owners (ADR-0024).
--
-- 0007 shaped `users` so that an external subject could bind to it later
-- (password_hash nullable). This is that binding, plus the RBAC pieces
-- RBAC-IMPLEMENTATION.md listed as missing.
--
-- ADDITIVE (ADR-0011 rule 5). New tables, and nullable or defaulted columns on
-- scoped_tokens, sessions and rbac_user_access. An old gateway replica never
-- reads the new columns: it keeps resolving scoped tokens by role and scopes.
-- A service-account token is written with an EMPTY scope list, so an old
-- replica that cannot interpret it grants it nothing; a ceilinged legacy token
-- is written with only the coarse scopes its ceiling implies, so an old replica
-- grants at most that coarse approximation (ADR-0024 §8, OPERATIONS §24.9). It
-- keeps writing sessions without an SSO provider. The one data change is bounded: custom
-- roles that hold the infrastructure grants gain the new cluster grants, so
-- splitting cluster permissions out of infrastructure takes nobody's access
-- away.
--
-- OWNERSHIP: api-gateway, like every identity table (0007).

-- ─── Identity providers, one row per tenant IdP ─────────────────────────────
CREATE TABLE IF NOT EXISTS sso_providers (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace     TEXT NOT NULL,
    tenant_id     UUID REFERENCES tenants(id),
    kind          TEXT NOT NULL CHECK (kind IN ('oidc', 'saml')),
    -- URL-safe and stable: it appears in audit rows and operator runbooks.
    slug          TEXT NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    display_name  TEXT NOT NULL CHECK (length(display_name) BETWEEN 1 AND 80),
    -- Disabled by default: a provider is configured, tested, then enabled.
    enabled       BOOLEAN NOT NULL DEFAULT false,

    -- OIDC. The client secret is a REFERENCE (file:<name> or env:<NAME>),
    -- never a value; see ADR-0024 §2 for why the reference is namespace-bound.
    issuer            TEXT NOT NULL DEFAULT '',
    client_id         TEXT NOT NULL DEFAULT '',
    client_secret_ref TEXT NOT NULL DEFAULT '',
    scopes            JSONB NOT NULL DEFAULT '["openid","email","profile"]'::jsonb
                      CHECK (jsonb_typeof(scopes) = 'array'),

    -- SAML. The metadata is pasted by an administrator; the gateway never
    -- fetches it from a tenant-supplied URL (no SSRF surface).
    idp_metadata_xml  TEXT NOT NULL DEFAULT '',
    idp_entity_id     TEXT NOT NULL DEFAULT '',

    -- Provisioning and linking policy.
    allowed_domains        JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(allowed_domains) = 'array'),
    jit_provisioning       BOOLEAN NOT NULL DEFAULT false,
    link_by_verified_email BOOLEAN NOT NULL DEFAULT false,
    groups_claim           TEXT NOT NULL DEFAULT 'groups' CHECK (length(groups_claim) BETWEEN 1 AND 128),
    revoke_sessions_on_role_loss BOOLEAN NOT NULL DEFAULT false,

    revision          BIGINT NOT NULL DEFAULT 1,
    mappings_revision BIGINT NOT NULL DEFAULT 0,
    created_by        UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (namespace, slug),
    CHECK (kind <> 'oidc' OR (issuer <> '' AND client_id <> '')),
    CHECK (kind <> 'saml' OR (idp_metadata_xml <> '' AND idp_entity_id <> ''))
);
CREATE INDEX IF NOT EXISTS sso_providers_namespace_idx ON sso_providers (namespace);
CREATE INDEX IF NOT EXISTS sso_providers_tenant_idx ON sso_providers (tenant_id);
-- One IdP application belongs to one tenant. Two tenants sharing an OIDC client
-- or a SAML entity would make "whose user is this" depend on which button was
-- pressed rather than on configuration.
CREATE UNIQUE INDEX IF NOT EXISTS sso_providers_oidc_client_uq
    ON sso_providers (issuer, client_id) WHERE kind = 'oidc';
CREATE UNIQUE INDEX IF NOT EXISTS sso_providers_saml_entity_uq
    ON sso_providers (idp_entity_id) WHERE kind = 'saml';

DROP TRIGGER IF EXISTS tenant_assignment ON sso_providers;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON sso_providers
    FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();

-- ─── External identities linked to users ────────────────────────────────────
--
-- Unique per PROVIDER and subject, not per issuer: several tenants legitimately
-- configure https://accounts.google.com, and the same Google account may be a
-- member of two of them as two distinct Nexus users.
CREATE TABLE IF NOT EXISTS user_identities (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider_id   UUID NOT NULL REFERENCES sso_providers(id) ON DELETE CASCADE,
    namespace     TEXT NOT NULL,
    tenant_id     UUID REFERENCES tenants(id),
    subject       TEXT NOT NULL CHECK (length(subject) BETWEEN 1 AND 512),
    email         TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ,
    UNIQUE (provider_id, subject),
    UNIQUE (provider_id, user_id)
);
CREATE INDEX IF NOT EXISTS user_identities_user_idx ON user_identities (user_id);
CREATE INDEX IF NOT EXISTS user_identities_tenant_idx ON user_identities (tenant_id);

-- The identity, its user and its provider must share one namespace. This is
-- the database half of "a tenant A IdP can never produce a tenant B user".
CREATE OR REPLACE FUNCTION public.sso_identity_same_namespace() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM public.users u WHERE u.id = NEW.user_id AND u.namespace = NEW.namespace) THEN
        RAISE EXCEPTION 'identity user is not in namespace %', NEW.namespace USING ERRCODE = '23514';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.sso_providers p WHERE p.id = NEW.provider_id AND p.namespace = NEW.namespace) THEN
        RAISE EXCEPTION 'identity provider is not in namespace %', NEW.namespace USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS same_namespace ON user_identities;
CREATE TRIGGER same_namespace BEFORE INSERT OR UPDATE ON user_identities
    FOR EACH ROW EXECUTE FUNCTION public.sso_identity_same_namespace();
DROP TRIGGER IF EXISTS tenant_assignment ON user_identities;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON user_identities
    FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();

-- ─── In-flight login flows ──────────────────────────────────────────────────
--
-- Server-side rather than a signed cookie, so that consumption is exactly once
-- across replicas (UPDATE ... WHERE consumed_at IS NULL) and a stolen state is
-- useless after its first use. The state and the browser binding are stored as
-- sha256; the nonce and PKCE verifier are stored as they are, for at most ten
-- minutes, because the gateway must present the verifier to the IdP.
CREATE TABLE IF NOT EXISTS sso_login_states (
    state_hash      TEXT PRIMARY KEY,
    provider_id     UUID NOT NULL REFERENCES sso_providers(id) ON DELETE CASCADE,
    namespace       TEXT NOT NULL,
    tenant_id       UUID REFERENCES tenants(id),
    nonce           TEXT NOT NULL DEFAULT '',
    code_verifier   TEXT NOT NULL DEFAULT '',
    saml_request_id TEXT NOT NULL DEFAULT '',
    return_to       TEXT NOT NULL DEFAULT '/' CHECK (length(return_to) <= 512),
    binding_hash    TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    consumed_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS sso_login_states_expires_idx ON sso_login_states (expires_at);
DROP TRIGGER IF EXISTS tenant_assignment ON sso_login_states;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON sso_login_states
    FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();

-- One-time handoff from the IdP callback to the console, bound to the same
-- browser as the login state (ADR-0024 §3).
CREATE TABLE IF NOT EXISTS sso_handoffs (
    code_hash         TEXT PRIMARY KEY,
    user_id           UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider_id       UUID NOT NULL REFERENCES sso_providers(id) ON DELETE CASCADE,
    namespace         TEXT NOT NULL,
    tenant_id         UUID REFERENCES tenants(id),
    binding_hash      TEXT NOT NULL,
    return_to         TEXT NOT NULL DEFAULT '/',
    sso_name_id       TEXT NOT NULL DEFAULT '',
    sso_session_index TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ NOT NULL,
    consumed_at       TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS sso_handoffs_expires_idx ON sso_handoffs (expires_at);
DROP TRIGGER IF EXISTS tenant_assignment ON sso_handoffs;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON sso_handoffs
    FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();

-- SAML assertion IDs already accepted, kept until they could no longer be
-- valid anyway (NotOnOrAfter plus skew). The primary key IS the replay check.
CREATE TABLE IF NOT EXISTS sso_assertion_replay (
    provider_id  UUID NOT NULL REFERENCES sso_providers(id) ON DELETE CASCADE,
    assertion_id TEXT NOT NULL CHECK (length(assertion_id) BETWEEN 1 AND 256),
    tenant_id    UUID REFERENCES tenants(id),
    expires_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (provider_id, assertion_id)
);
CREATE INDEX IF NOT EXISTS sso_assertion_replay_expires_idx ON sso_assertion_replay (expires_at);
DROP TRIGGER IF EXISTS tenant_assignment ON sso_assertion_replay;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON sso_assertion_replay
    FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('provider_id', 'sso_providers');

-- ─── IdP group → Nexus role mapping ─────────────────────────────────────────
CREATE TABLE IF NOT EXISTS sso_group_mappings (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id UUID NOT NULL REFERENCES sso_providers(id) ON DELETE CASCADE,
    namespace   TEXT NOT NULL,
    tenant_id   UUID REFERENCES tenants(id),
    idp_group   TEXT NOT NULL CHECK (length(idp_group) BETWEEN 1 AND 256),
    role_name   TEXT NOT NULL CHECK (length(role_name) BETWEEN 1 AND 64),
    created_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_id, idp_group, role_name)
);
CREATE INDEX IF NOT EXISTS sso_group_mappings_namespace_idx ON sso_group_mappings (namespace);
DROP TRIGGER IF EXISTS tenant_assignment ON sso_group_mappings;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON sso_group_mappings
    FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();

-- Roles an IdP granted at the last login, kept apart from administrator-assigned
-- `roles` so a group removal revokes exactly what the group granted.
ALTER TABLE rbac_user_access ADD COLUMN IF NOT EXISTS idp_roles JSONB NOT NULL DEFAULT '[]'::jsonb;

-- Which IdP minted a session, so logout can offer RP-initiated / SAML logout.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS sso_provider_id UUID REFERENCES sso_providers(id) ON DELETE SET NULL;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS sso_name_id TEXT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS sso_session_index TEXT;

-- ─── Service accounts ───────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS service_accounts (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace   TEXT NOT NULL,
    tenant_id   UUID REFERENCES tenants(id),
    name        TEXT NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{1,62}$'),
    description TEXT NOT NULL DEFAULT '' CHECK (length(description) <= 500),
    disabled_at TIMESTAMPTZ,
    created_by  UUID REFERENCES users(id) ON DELETE SET NULL,
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

-- A token may belong to a service account, and may carry an explicit
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

-- ─── Cross-namespace owners ─────────────────────────────────────────────────
--
-- A user from any namespace granted Owner of THIS namespace (tenant_id is the
-- target's). Deliberately cross-tenant: the target's administrator decides.
-- There is no wildcard row: every namespace an owner may act in is one row.
CREATE TABLE IF NOT EXISTS namespace_owner_grants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace   TEXT NOT NULL,
    tenant_id   UUID REFERENCES tenants(id),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    granted_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS namespace_owner_grants_active_uq
    ON namespace_owner_grants (namespace, user_id) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS namespace_owner_grants_user_idx ON namespace_owner_grants (user_id);
DROP TRIGGER IF EXISTS tenant_assignment ON namespace_owner_grants;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON namespace_owner_grants
    FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant();

-- ─── Cluster permissions split from infrastructure ─────────────────────────
-- Bounded by the WHERE: only custom roles holding the old grant change, and
-- each gains exactly the grant it used to reach clusters through.
UPDATE rbac_roles
   SET permissions = permissions || '["clusters.view"]'::jsonb, revision = revision + 1
 WHERE permissions ? 'infrastructure.view' AND NOT permissions ? 'clusters.view';
UPDATE rbac_roles
   SET permissions = permissions || '["clusters.configure"]'::jsonb, revision = revision + 1
 WHERE permissions ? 'infrastructure.configure' AND NOT permissions ? 'clusters.configure';

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
            EXECUTE format('GRANT SELECT ON sso_providers, user_identities, sso_login_states, sso_handoffs, '
                           'sso_assertion_replay, sso_group_mappings, service_accounts, service_account_roles, '
                           'namespace_owner_grants TO %I', r);
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_api_gateway') THEN
        GRANT INSERT, UPDATE, DELETE ON sso_providers, user_identities, sso_login_states, sso_handoffs,
            sso_assertion_replay, sso_group_mappings, service_accounts, service_account_roles,
            namespace_owner_grants TO nexus_api_gateway;
    END IF;
END $$;
