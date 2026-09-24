-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Email ownership verification and password recovery (docs/MULTITENANCY.md).
--
-- Self-service registration (0013) lets anyone create a customer with any email
-- address, and nothing proves the registrant controls that address. Password
-- recovery cannot exist without that proof: a reset link is only as safe as the
-- mailbox it is sent to. This migration adds the two facts both flows need -- WHEN
-- an address was proven, and single-use tokens that prove it.
--
-- OWNERSHIP: api-gateway, like users and sessions (0007). The gateway is the only
-- service that authenticates, so it is the only one that issues or consumes these.
--
-- ADDITIVE (ADR-0011 rule 5): one nullable column, one new table, one trigger. No
-- column is dropped, renamed or retyped, and no existing row is rewritten.

-- ─── users.email_verified_at ──────────────────────────────────────────────
--
-- EXISTING ACCOUNTS ARE TREATED AS VERIFIED, and the mechanism is deliberate.
--
-- ADD COLUMN with a non-volatile DEFAULT is a catalog-only change in Postgres 11+:
-- the default is evaluated ONCE, at this statement, and stored as the column's
-- "missing value" for every row that already exists. No row is rewritten and no
-- row lock is taken, which is why this is not a backfill. now() is STABLE, so it
-- qualifies, and every pre-existing account reads back the migration time.
--
-- The default is then dropped in the next statement. Dropping a default does NOT
-- clear the stored missing value for rows that predate it, so existing accounts
-- stay verified while every account inserted from here on starts UNVERIFIED --
-- including accounts inserted by an older gateway replica during a rolling deploy
-- that knows nothing about this column. That is the fail-safe direction: an old
-- replica can create an account that must still prove its address, never one that
-- skips the proof.
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified_at TIMESTAMPTZ DEFAULT now();
ALTER TABLE users ALTER COLUMN email_verified_at DROP DEFAULT;

-- Changing an account's address un-proves it. Enforced by the DATABASE rather than
-- by each handler that can edit an email, because there is more than one such path
-- (admin profile edit today, others later) and the invariant "a verified address is
-- one somebody proved" must not depend on every future path remembering it. A reset
-- link sent to an unproven address is an account-takeover primitive for whoever
-- owns that address.
CREATE OR REPLACE FUNCTION public.users_email_change_unverifies() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $$
BEGIN
 IF lower(NEW.email) IS DISTINCT FROM lower(OLD.email) THEN
  NEW.email_verified_at := NULL;
 END IF;
 RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS users_email_change_unverifies ON users;
CREATE TRIGGER users_email_change_unverifies BEFORE UPDATE OF email ON users
 FOR EACH ROW EXECUTE FUNCTION public.users_email_change_unverifies();

-- ─── email_tokens: single-use, expiring, purpose-scoped proofs ────────────
CREATE TABLE IF NOT EXISTS email_tokens (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Explicit tenant ownership like every customer-owned table since 0014. Assigned
    -- from the user by the same trigger sessions use, so no caller supplies it.
    tenant_id    UUID NOT NULL,
    user_id      UUID NOT NULL,

    -- One table, two purposes, and the purpose is part of every lookup. A token
    -- minted to verify an address must never reset a password: the verification
    -- link is the lower-value one (it is sent on every registration and resend),
    -- so letting it do the higher-value thing would make the weakest link the key.
    purpose      TEXT NOT NULL CHECK (purpose IN ('verify_email', 'reset_password')),

    -- sha256 of the token, never the token. The token is 32 random bytes, so there
    -- is no dictionary to attack and a password KDF would buy nothing -- the same
    -- reasoning as api_keys and sessions (store/gateway_apikeys.go). A leak of this
    -- table yields nothing that can be presented.
    token_hash   TEXT NOT NULL UNIQUE,

    -- The address the link was SENT to. Consumption requires it to still equal the
    -- account's address: a link mailed before an address change must not prove the
    -- new address, nor reset the password of an account that has moved on.
    email        TEXT NOT NULL,

    -- Judged by the database clock at consumption, never a gateway's, so a token is
    -- not alive on one replica and dead on another.
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Single use: set exactly once, by the conditional UPDATE that consumes it.
    consumed_at  TIMESTAMPTZ,
    -- Superseded by a newer token of the same purpose, or invalidated by a
    -- successful reset. Kept separate from consumed_at so a reviewer can tell "used"
    -- from "replaced".
    revoked_at   TIMESTAMPTZ,

    -- Weak evidence for an incident review ("who asked for this reset?"); never
    -- used for an authorization decision.
    requested_ip TEXT NOT NULL DEFAULT '',

    CONSTRAINT email_tokens_expiry_after_creation CHECK (expires_at > created_at),
    -- Composite, so a token can only ever belong to a user in its own tenant.
    CONSTRAINT email_tokens_user_tenant_fk FOREIGN KEY (tenant_id, user_id)
        REFERENCES users (tenant_id, id) ON DELETE CASCADE
);

-- Issuance throttling and supersession both ask "this user's recent tokens of this
-- purpose"; one index serves both.
CREATE INDEX IF NOT EXISTS idx_email_tokens_user_purpose
    ON email_tokens (user_id, purpose, created_at DESC);
-- The expiry sweep scans by deadline.
CREATE INDEX IF NOT EXISTS idx_email_tokens_expires ON email_tokens (expires_at);
CREATE INDEX IF NOT EXISTS email_tokens_tenant_idx ON email_tokens (tenant_id);

DROP TRIGGER IF EXISTS tenant_assignment ON email_tokens;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON email_tokens
 FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('user_id', 'users');

REVOKE ALL ON FUNCTION public.users_email_change_unverifies() FROM PUBLIC;

-- ─── Grants (the 0005 style; ADR-0011 rule 1) ─────────────────────────────
DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT ON email_tokens TO %I', r);
        END IF;
    END LOOP;
END $$;

-- Only the gateway writes. DELETE is for the expiry sweep; users already carries
-- UPDATE for the gateway from 0007, which covers email_verified_at.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_api_gateway') THEN
        EXECUTE 'GRANT INSERT, UPDATE, DELETE ON email_tokens TO nexus_api_gateway';
    END IF;
END $$;
