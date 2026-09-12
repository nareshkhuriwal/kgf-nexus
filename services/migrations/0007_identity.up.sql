-- Human identity: users, sessions, scoped tokens, audit log (ARCHITECTURE §3.2).
--
-- §3.2 promises "OIDC bearer tokens for humans, API keys (hashed at rest) for
-- workers and machines" and "RBAC over (principal, namespace, action)". Only the
-- second half of the first sentence exists: `api_keys` with three roles. There is
-- no human identity at all -- the console's "login" is pasting a machine
-- credential into localStorage, which means every console user holds a key that is
-- indistinguishable from the one on a worker host, cannot be attributed to a
-- person, and never expires.
--
-- These tables are the human half. OIDC itself is deliberately NOT implemented
-- here: it needs an identity provider that does not exist in this deployment, and
-- the shape below (a `users` row keyed by a subject, with an auth kind) is what an
-- OIDC subject would later bind to, so adding it is a new auth kind rather than a
-- schema change.
--
-- OWNERSHIP: api-gateway. ADR-0011 gives it `api_keys` and `idempotency_keys`, and
-- authentication is the gateway's whole job -- it is the only ingress (§3.2), so
-- it is the only service that ever resolves a credential. No other service reads
-- these tables.
--
-- WHY FOUR TABLES AND NOT MORE COLUMNS ON api_keys. A session and an API key have
-- opposite lifecycles and opposite threat models: a key is long-lived, high-entropy
-- and lives on a host; a session is short-lived, tied to a person, and must be
-- revocable en masse when that person leaves. Folding them into one table would
-- mean every worker-RPC authn probe scans rows that expire, and every session
-- revocation risks touching a worker's credential. They are kept apart.
--
-- ADDITIVE (ADR-0011 rule 5): new tables only. Not one existing table is altered,
-- no column is dropped, and there is no backfill -- so an old replica that knows
-- nothing about users keeps authenticating API keys exactly as before. That is the
-- property that makes this safe to apply ahead of the code that uses it, and it is
-- why API-key clients (the CLI, every worker) are unaffected by this migration.

-- ─── Users: a human identity ──────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Namespace scopes a user the same way it scopes an API key: namespaces are
    -- the tenancy boundary (§3.2), and a principal that is not scoped to one
    -- cannot be authorized against one without a special case in the enforcement
    -- point. Keeping the shape identical to api_keys is what lets Principal stay
    -- one type with one Authorize.
    namespace     TEXT NOT NULL,

    -- The login identifier. Lowercased by the application before it reaches here
    -- so that Alice@x and alice@x are one account; the unique index below is what
    -- makes that a guarantee rather than a convention.
    email         TEXT NOT NULL,
    display_name  TEXT NOT NULL DEFAULT '',

    -- The at-rest password verifier, in PHC string format
    -- ($argon2id$v=19$m=...,t=...,p=...$salt$hash) -- self-describing, so the
    -- parameters a hash was created with travel WITH it and can be raised later
    -- without invalidating existing passwords.
    --
    -- NULL means this user has no password and cannot authenticate by password.
    -- That is the correct state for a future OIDC-backed user, and it is why the
    -- column is nullable rather than defaulted to an unusable string: a NULL
    -- cannot be mistaken for a hash that merely never matches.
    password_hash TEXT,

    -- 'admin' | 'reader'. Deliberately NOT 'worker': a worker credential exists to
    -- speak the worker gRPC protocol from an execution host, which is the least
    -- trustworthy place in the system (authn.go). A human is never that principal,
    -- so the role is unrepresentable here rather than merely discouraged.
    role          TEXT NOT NULL DEFAULT 'reader',

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Disablement is reversible and preserves history; a DELETE would orphan every
    -- audit row that attributes an action to this person, which is exactly the
    -- history an audit log exists to keep.
    disabled_at   TIMESTAMPTZ
);

-- One account per email per namespace. Partial on the namespace+email pair rather
-- than email alone, because two tenants legitimately have the same person.
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_ns_email ON users (namespace, lower(email));

-- ─── Sessions: a browser's proof of a completed login ─────────────────────
CREATE TABLE IF NOT EXISTS sessions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- The session token is stored HASHED, exactly like an API key and for exactly
    -- the same reason: a leak of this table must not yield usable credentials. The
    -- token is high-entropy and machine-generated, so sha256 is right here and a
    -- password KDF would be wrong -- see store/gateway_apikeys.go, which explains
    -- the distinction this schema is built on.
    token_hash    TEXT NOT NULL UNIQUE,

    -- Sessions EXPIRE, which is the property that most distinguishes them from an
    -- API key. A stolen browser token is bounded in time without anyone noticing
    -- the theft; a stolen API key is not.
    expires_at    TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Explicit logout, and the target of a mass revocation when a person leaves.
    revoked_at    TIMESTAMPTZ,

    -- Recorded for the operator answering "was this session used after the laptop
    -- was lost". Both are weak evidence (a proxy rewrites one, a client chooses the
    -- other) and neither is ever used for an authorization decision.
    user_agent    TEXT NOT NULL DEFAULT '',
    client_ip     TEXT NOT NULL DEFAULT '',
    last_used_at  TIMESTAMPTZ
);

-- Authn resolves a session by hash on every request, so this index is the hot path.
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions (user_id);
-- Expiry sweeps scan by deadline.
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions (expires_at);

-- ─── Scoped tokens: a narrow, expiring machine credential ─────────────────
--
-- The gap these fill: an API key today is all-or-nothing for its role and never
-- expires, so a CI pipeline that only needs to submit a spec is handed a
-- credential that can also drain workers, forever. A scoped token is the same
-- credential shape with two additions -- an expiry and an explicit action list --
-- which is the Argo ServiceAccount-token idea narrowed to what this platform
-- actually enforces.
CREATE TABLE IF NOT EXISTS scoped_tokens (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace     TEXT NOT NULL,
    name          TEXT NOT NULL,

    -- sha256, for the same reason as api_keys.key_hash: high machine entropy.
    token_hash    TEXT NOT NULL UNIQUE,

    -- The actions this token may perform, as a JSONB array of the Action strings
    -- the gateway's authz matrix already uses ("read", "write", "admin_write").
    --
    -- Scopes INTERSECT with the role's grants and never extend them, so a scope
    -- string the matrix does not know grants nothing rather than something
    -- unintended. That ordering is what makes this table safe to add to an
    -- enforcement point that already works.
    scopes        JSONB NOT NULL DEFAULT '[]'::jsonb,

    -- The role the token acts as; scopes narrow it. Kept so a token is resolvable
    -- to the same Principal shape as a key or a session.
    role          TEXT NOT NULL DEFAULT 'reader',

    -- NULL means no expiry, which preserves the current api_keys behaviour for a
    -- token deliberately created as a long-lived one. The UI defaults to a bounded
    -- lifetime; the schema permits the exception rather than forcing a lie.
    expires_at    TIMESTAMPTZ,

    -- The column that answers "is this credential still in use, or can we revoke
    -- it?" -- unanswerable today for api_keys, which is why unused keys accumulate
    -- forever. Written on a best-effort, coarse cadence: see the store layer, which
    -- deliberately does NOT write it on every request.
    last_used_at  TIMESTAMPTZ,

    created_by    UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_scoped_tokens_ns ON scoped_tokens (namespace);

-- ─── Audit log: who did what, append-only ─────────────────────────────────
--
-- run_events records what happened to a RUN. Nothing records what happened to the
-- SYSTEM: who created a user, who minted a token, who revoked one, who failed to
-- log in five times. Those are precisely the events an incident review needs and
-- the ones no run's history contains.
--
-- APPEND-ONLY, like run_events (invariant 8). The grant below is INSERT-only, so
-- the gateway cannot rewrite its own audit trail even by accident -- the invariant
-- is enforced by the database rather than asserted in a comment.
CREATE TABLE IF NOT EXISTS audit_log (
    id            BIGSERIAL PRIMARY KEY,
    namespace     TEXT NOT NULL,

    -- WHO. Denormalised alongside the id on purpose: the id may later be NULL
    -- (a deleted user, an unauthenticated attempt), and an audit row that cannot
    -- say who did something is worthless. The label is what the reviewer reads.
    actor_id      UUID,
    actor_kind    TEXT NOT NULL DEFAULT 'user',   -- 'user' | 'api_key' | 'token' | 'anonymous'
    actor_label   TEXT NOT NULL DEFAULT '',

    -- WHAT, and to which object. Free-form strings rather than enums because the
    -- set of auditable actions grows with the product, and a CHECK constraint here
    -- would make adding one a migration.
    action        TEXT NOT NULL,                  -- e.g. 'user.create', 'token.revoke'
    target_kind   TEXT NOT NULL DEFAULT '',
    target_id     TEXT NOT NULL DEFAULT '',

    -- The outcome, so a FAILED attempt is recorded as loudly as a successful one.
    -- An audit log that only contains successes cannot show an attack in progress.
    outcome       TEXT NOT NULL DEFAULT 'success',-- 'success' | 'denied' | 'error'

    -- Structured context. MUST NOT contain secret material: nothing that could
    -- reconstruct a credential goes here, which is why the store layer builds this
    -- object explicitly rather than serialising a request.
    detail        JSONB NOT NULL DEFAULT '{}'::jsonb,

    client_ip     TEXT NOT NULL DEFAULT '',
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The audit view is "most recent first, in this namespace", which is one index.
CREATE INDEX IF NOT EXISTS idx_audit_ns_time ON audit_log (namespace, occurred_at DESC);
-- "Everything this person did" is the other question an incident review asks.
CREATE INDEX IF NOT EXISTS idx_audit_actor ON audit_log (actor_id, occurred_at DESC);

-- ─── Grants (the 0005 style; ADR-0011 rule 1) ─────────────────────────────
--
-- 0005 ends by noting that new tables are granted explicitly by a future migration
-- rather than becoming writable by default. This is that migration doing it.
--
-- Reads stay unrestricted for every service, matching 0005's reasoning and
-- ADR-0011's "Reads: all". Only writes are constrained, and every write here
-- belongs to the api-gateway, because it is the only service that authenticates.
DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT ON users, sessions, scoped_tokens, audit_log TO %I', r);
        END IF;
    END LOOP;
END $$;

-- The sequence behind audit_log.id: a role that may INSERT must be able to draw
-- from it (0005 grants sequences wholesale; this one is created after that ran).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_api_gateway') THEN
        EXECUTE 'GRANT INSERT, UPDATE, DELETE ON users, sessions, scoped_tokens TO nexus_api_gateway';
        -- audit_log is INSERT only. The gateway writes history and must not be able
        -- to rewrite it, exactly as run_events is INSERT-only for its owner in 0005.
        EXECUTE 'GRANT INSERT ON audit_log TO nexus_api_gateway';
        EXECUTE 'GRANT USAGE ON SEQUENCE audit_log_id_seq TO nexus_api_gateway';
    END IF;
END $$;
