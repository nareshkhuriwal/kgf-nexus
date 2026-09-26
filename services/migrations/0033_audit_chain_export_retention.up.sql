-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- migrate: no-transaction
-- Tamper-evident, exportable, retained audit trail (ADR-0025 §2).
--
-- ADDITIVE (ADR-0011 rule 5): three nullable columns on audit_log, four new
-- tables, one function, two guard triggers and grants. An older gateway
-- replica keeps inserting audit rows exactly as before during a rolling
-- deploy: its rows simply carry no chain position (chain_seq NULL), which the
-- verifier reports as "unchained" rather than silently accepting.
--
-- `migrate: no-transaction` for CREATE INDEX CONCURRENTLY on audit_log, so
-- every statement is written to be safely re-run.
--
-- OWNERSHIP: api-gateway (ADR-0011), like audit_log itself.

-- ─── The chain, on audit_log ──────────────────────────────────────────────
-- Per NAMESPACE chains: a tenant's auditor verifies and exports their own
-- chain without seeing, or depending on, anyone else's rows.
--
--   chain_seq  1, 2, 3 ... per namespace, gapless while rows are retained
--   prev_hash  row_hash of chain_seq-1 (or the genesis/purge checkpoint)
--   row_hash   hex sha256 of canonical_v1(row), which includes prev_hash
--
-- canonical_v1 is computed by the GATEWAY (internal/auditchain), not by a
-- trigger: an offline verifier must be able to recompute it from an export,
-- and PostgreSQL's jsonb text form is not something an export consumer can
-- reproduce. See OPERATIONS §25.2 for the exact encoding.
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS chain_seq BIGINT;
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS prev_hash TEXT;
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS row_hash  TEXT;

-- ─── KGF Cloud attribution ────────────────────────────────────────────────
-- Nexus runs as a service of KGF Cloud. When a request carries a VERIFIED
-- KGF context (the X-KGF-Context token KGF's gateway signs), the row records
-- the KGF organization, subscription, resource group, principal and the
-- person a KGF service acted for, so KGF's audit pipeline can attribute the
-- action without trusting Nexus's own principal names. request_id is the
-- correlation id (KGF's, from the verified context, else the caller's
-- X-Request-ID). Empty strings, not NULL: every value is hashed, and one
-- representation of "absent" keeps canonical_v1 unambiguous.
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS kgf_organization   TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS kgf_subscription   TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS kgf_resource_group TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS kgf_principal      TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS kgf_on_behalf_of   TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS request_id         TEXT NOT NULL DEFAULT '';

-- One position per namespace; the export streams in this order.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS audit_log_chain_idx
    ON audit_log (namespace, chain_seq) WHERE chain_seq IS NOT NULL;
-- Keyset pagination of the list view (occurred_at DESC, id DESC) is served by
-- 0007's idx_audit_ns_time; the id tiebreak is cheap.

-- ─── Chain heads: the lock that keeps concurrent replicas from forking ────
-- The append takes SELECT ... FOR UPDATE on its namespace's head row in the
-- same transaction as the INSERT, so two gateway replicas serialise per
-- namespace and can never both extend seq N.
CREATE TABLE IF NOT EXISTS audit_chain_heads (
    namespace  TEXT PRIMARY KEY,
    tenant_id  UUID REFERENCES tenants(id),
    last_seq   BIGINT NOT NULL DEFAULT 0,
    last_hash  TEXT   NOT NULL,
    -- When the chain started. Rows older than this with chain_seq NULL are
    -- pre-chain history (written before 0033), reported as such.
    started_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ─── Checkpoints: anchors the verifier starts from ────────────────────────
-- 'genesis' when a namespace's chain starts; 'purge' when retention removed
-- a prefix of the chain (seq and hash of the LAST purged row, so the first
-- retained row's prev_hash is checkable). Written only by the chain code and
-- by audit_purge(); never updated.
CREATE TABLE IF NOT EXISTS audit_chain_checkpoints (
    namespace   TEXT   NOT NULL,
    tenant_id   UUID REFERENCES tenants(id),
    seq         BIGINT NOT NULL,
    hash        TEXT   NOT NULL,
    kind        TEXT   NOT NULL CHECK (kind IN ('genesis', 'purge')),
    rows_purged BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, seq, kind)
);

-- ─── Retention policy and legal hold ──────────────────────────────────────
-- retention_days NULL = keep forever (the default: deleting evidence is the
-- decision that needs making, not keeping it). A legal hold suspends ALL
-- purging for the namespace regardless of retention.
CREATE TABLE IF NOT EXISTS audit_policies (
    namespace         TEXT PRIMARY KEY,
    tenant_id         UUID REFERENCES tenants(id),
    retention_days    INTEGER CHECK (retention_days IS NULL OR retention_days BETWEEN 1 AND 36500),
    legal_hold        BOOLEAN NOT NULL DEFAULT false,
    legal_hold_reason TEXT    NOT NULL DEFAULT '',
    legal_hold_by     TEXT    NOT NULL DEFAULT '',
    legal_hold_at     TIMESTAMPTZ,
    revision          BIGINT  NOT NULL DEFAULT 1,
    updated_by        TEXT    NOT NULL DEFAULT '',
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_purge_at     TIMESTAMPTZ
);

-- ─── Sinks: continuous delivery to a SIEM ─────────────────────────────────
-- The audit table IS the outbox (ADR-0025): each sink keeps a durable cursor
-- (the last chain_seq it acknowledged) and delivery is at-least-once from it.
-- The HMAC signing secret is stored ENCRYPTED (AES-256-GCM under the gateway's
-- NEXUS_AUDIT_SINK_KEY); key_id names that key so a rotation is detectable,
-- and the API never returns the secret after creation.
CREATE TABLE IF NOT EXISTS audit_sinks (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace            TEXT NOT NULL,
    tenant_id            UUID REFERENCES tenants(id),
    name                 TEXT NOT NULL,
    kind                 TEXT NOT NULL CHECK (kind IN ('webhook')),
    -- 'nexus': the Nexus export record; 'kgf': KGF Cloud's audit event shape
    -- (OPERATIONS §25.2), for a KGF-side collector.
    format               TEXT NOT NULL DEFAULT 'nexus' CHECK (format IN ('nexus', 'kgf')),
    url                  TEXT NOT NULL,
    enabled              BOOLEAN NOT NULL DEFAULT true,
    secret_ciphertext    BYTEA NOT NULL,
    secret_nonce         BYTEA NOT NULL,
    key_id               TEXT  NOT NULL,
    batch_size           INTEGER NOT NULL DEFAULT 100 CHECK (batch_size BETWEEN 1 AND 1000),
    cursor_seq           BIGINT NOT NULL DEFAULT 0,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    next_attempt_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_attempt_at      TIMESTAMPTZ,
    last_success_at      TIMESTAMPTZ,
    last_error           TEXT NOT NULL DEFAULT '',
    created_by           TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    revision             BIGINT NOT NULL DEFAULT 1,
    UNIQUE (namespace, name)
);
CREATE INDEX IF NOT EXISTS audit_sinks_due_idx ON audit_sinks (next_attempt_at) WHERE enabled;

-- Tenant ownership, the 0014 way: derived from namespace by trigger.
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['audit_chain_heads', 'audit_chain_checkpoints', 'audit_policies', 'audit_sinks'] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'tenant_assignment' AND tgrelid = t::regclass) THEN
            EXECUTE format('CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON %I '
                           'FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant()', t);
        END IF;
    END LOOP;
END $$;

-- ─── audit_log is append-only for EVERY role, not only by grant ───────────
-- 0007 grants the gateway INSERT only. These triggers extend that to the
-- table owner: an UPDATE always fails, and a DELETE fails unless it runs
-- inside audit_purge() (which sets a transaction-local flag). A superuser can
-- still disable a trigger -- which is exactly the case the hash chain and the
-- externally delivered copy exist to expose.
CREATE OR REPLACE FUNCTION audit_log_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        -- 0014's tenant backfill is the only historical UPDATE; it has run.
        RAISE EXCEPTION 'audit_log is append-only' USING ERRCODE = '42501';
    END IF;
    IF current_setting('nexus.audit_purge', true) IS DISTINCT FROM 'on' THEN
        RAISE EXCEPTION 'audit_log rows are removed only by audit_purge()' USING ERRCODE = '42501';
    END IF;
    RETURN OLD;
END $$;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'audit_log_no_update' AND tgrelid = 'audit_log'::regclass) THEN
        CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log
            FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'audit_log_no_delete' AND tgrelid = 'audit_log'::regclass) THEN
        CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log
            FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();
    END IF;
END $$;

-- ─── audit_purge: the ONLY way rows leave audit_log ───────────────────────
-- SECURITY DEFINER so the gateway role stays INSERT-only on audit_log. The
-- cutoff comes from the stored policy, never from the caller, so a
-- compromised gateway cannot purge more than the policy allows. It:
--   * does nothing without a retention policy, and refuses under legal hold;
--   * purges a PREFIX of the chain only (chain_seq <= X), never a hole;
--   * never purges a row an enabled sink has not yet acknowledged;
--   * records a 'purge' checkpoint (seq and hash of the last purged row) in
--     the same transaction, so the retained remainder still verifies;
--   * also removes pre-chain (chain_seq NULL) rows past the cutoff.
-- Returns the number of rows removed.
CREATE OR REPLACE FUNCTION audit_purge(p_namespace TEXT) RETURNS BIGINT
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE
    pol       audit_policies%ROWTYPE;
    cutoff    TIMESTAMPTZ;
    sink_min  BIGINT;
    upto      BIGINT;
    upto_hash TEXT;
    n_chain   BIGINT := 0;
    n_legacy  BIGINT := 0;
BEGIN
    SELECT * INTO pol FROM audit_policies WHERE namespace = p_namespace FOR UPDATE;
    IF NOT FOUND OR pol.retention_days IS NULL THEN
        RETURN 0;
    END IF;
    IF pol.legal_hold THEN
        RAISE EXCEPTION 'audit purge refused: namespace % is under legal hold', p_namespace
            USING ERRCODE = '55006';
    END IF;
    cutoff := now() - make_interval(days => pol.retention_days);

    -- Serialise with appends: the head lock is what appends take.
    PERFORM 1 FROM audit_chain_heads WHERE namespace = p_namespace FOR UPDATE;

    SELECT min(cursor_seq) INTO sink_min FROM audit_sinks WHERE namespace = p_namespace AND enabled;

    SELECT max(chain_seq) INTO upto FROM audit_log
     WHERE namespace = p_namespace AND chain_seq IS NOT NULL AND occurred_at < cutoff;
    IF upto IS NOT NULL AND sink_min IS NOT NULL AND upto > sink_min THEN
        upto := sink_min;  -- undelivered rows stay until every enabled sink has them
    END IF;

    PERFORM set_config('nexus.audit_purge', 'on', true);
    IF upto IS NOT NULL AND upto > 0 THEN
        SELECT row_hash INTO upto_hash FROM audit_log WHERE namespace = p_namespace AND chain_seq = upto;
        IF upto_hash IS NOT NULL THEN
            DELETE FROM audit_log WHERE namespace = p_namespace AND chain_seq IS NOT NULL AND chain_seq <= upto;
            GET DIAGNOSTICS n_chain = ROW_COUNT;
            INSERT INTO audit_chain_checkpoints (namespace, seq, hash, kind, rows_purged)
            VALUES (p_namespace, upto, upto_hash, 'purge', n_chain)
            ON CONFLICT (namespace, seq, kind) DO NOTHING;
        END IF;
    END IF;
    DELETE FROM audit_log WHERE namespace = p_namespace AND chain_seq IS NULL AND occurred_at < cutoff;
    GET DIAGNOSTICS n_legacy = ROW_COUNT;
    PERFORM set_config('nexus.audit_purge', 'off', true);

    UPDATE audit_policies SET last_purge_at = now() WHERE namespace = p_namespace;
    RETURN n_chain + n_legacy;
END $$;
REVOKE ALL ON FUNCTION audit_purge(TEXT) FROM PUBLIC;

-- ─── Grants (0005/0007 style) ─────────────────────────────────────────────
DO $$
DECLARE r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT ON audit_chain_heads, audit_chain_checkpoints, audit_policies TO %I', r);
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_api_gateway') THEN
        -- Heads: the gateway extends its chains. Checkpoints: genesis only
        -- (INSERT); purge checkpoints come from audit_purge().
        EXECUTE 'GRANT INSERT, UPDATE ON audit_chain_heads TO nexus_api_gateway';
        EXECUTE 'GRANT INSERT ON audit_chain_checkpoints TO nexus_api_gateway';
        EXECUTE 'GRANT INSERT, UPDATE ON audit_policies TO nexus_api_gateway';
        -- Sinks hold encrypted secrets: the gateway only, not every service.
        EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON audit_sinks TO nexus_api_gateway';
        EXECUTE 'GRANT EXECUTE ON FUNCTION audit_purge(TEXT) TO nexus_api_gateway';
    END IF;
END $$;
