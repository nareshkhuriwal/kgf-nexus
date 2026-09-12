-- Nexus MVP schema (architecture §4.2).
-- Postgres is the single source of truth, INCLUDING the dispatch queue
-- (ADR-0002). The constraints in this file ARE the design: they are what make
-- double-dispatch and capacity drift impossible rather than merely unlikely.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ─── Specs: immutable, content-addressed ──────────────────────────────────
CREATE TABLE workflow_specs (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace   TEXT NOT NULL,
    name        TEXT NOT NULL,
    version     INT  NOT NULL,
    spec_hash   TEXT NOT NULL,
    spec_json   JSONB NOT NULL,
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace, name, version),
    -- Resubmitting a byte-identical spec is a no-op returning the existing
    -- version, rather than creating a redundant one.
    UNIQUE (namespace, name, spec_hash)
);
-- Specs are NEVER updated. A change is a new version, so a historical run's
-- exact definition is recoverable forever (ADR-0004).

-- ─── Runs ─────────────────────────────────────────────────────────────────
CREATE TYPE run_state AS ENUM (
    'PENDING','RUNNING','PAUSING','PAUSED',
    'SUCCEEDED','FAILED','CANCELLING','CANCELLED','TIMED_OUT'
);

CREATE TABLE workflow_runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    spec_id         UUID NOT NULL REFERENCES workflow_specs(id),
    namespace       TEXT NOT NULL,
    state           run_state NOT NULL DEFAULT 'PENDING',
    priority        INT  NOT NULL DEFAULT 100,
    idempotency_key TEXT,
    trigger_kind    TEXT NOT NULL,
    trigger_detail  JSONB NOT NULL DEFAULT '{}',
    params          JSONB NOT NULL DEFAULT '{}',
    logical_date    TIMESTAMPTZ,
    deadline_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    ended_at        TIMESTAMPTZ,
    state_version   BIGINT NOT NULL DEFAULT 0
);

-- How "submit the same run twice" becomes safe at the outermost layer: a
-- duplicate INSERT conflicts and we return the existing run. Partial, so
-- unkeyed runs are unconstrained.
CREATE UNIQUE INDEX uq_runs_idem ON workflow_runs (namespace, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- Partial index: the reconciler only ever scans NON-TERMINAL runs, and that
-- set stays small even when history grows to billions of rows.
CREATE INDEX idx_runs_active ON workflow_runs (namespace, state)
    WHERE state IN ('PENDING','RUNNING','PAUSING','CANCELLING');
CREATE INDEX idx_runs_listing ON workflow_runs (namespace, created_at DESC);

-- ─── Nodes ────────────────────────────────────────────────────────────────
CREATE TYPE node_state AS ENUM (
    'PENDING','READY','RUNNING','SUCCEEDED','FAILED',
    'SKIPPED','UPSTREAM_FAILED','CANCELLED'
);

CREATE TABLE task_nodes (
    run_id         UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    node_id        TEXT NOT NULL,
    task_ref       TEXT NOT NULL,
    state          node_state NOT NULL DEFAULT 'PENDING',
    trigger_rule   TEXT NOT NULL DEFAULT 'all_success',
    max_attempts   INT  NOT NULL DEFAULT 3,
    attempts_used  INT  NOT NULL DEFAULT 0,
    -- Non-null only for dynamic expansion children (Phase 3; columns exist now
    -- so adding it is not a migration of the hot table).
    parent_node_id TEXT,
    map_index      INT,
    spec_json      JSONB NOT NULL,
    PRIMARY KEY (run_id, node_id)
);
CREATE INDEX idx_nodes_run_state ON task_nodes (run_id, state);

CREATE TABLE task_edges (
    run_id          UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    from_node_id    TEXT NOT NULL,
    to_node_id      TEXT NOT NULL,
    output_to_input JSONB NOT NULL DEFAULT '{}',
    PRIMARY KEY (run_id, from_node_id, to_node_id)
);
CREATE INDEX idx_edges_to ON task_edges (run_id, to_node_id);

-- ─── Workers ──────────────────────────────────────────────────────────────
CREATE TYPE worker_state AS ENUM ('ACTIVE','DRAINING','QUARANTINED','DEAD');

CREATE TABLE workers (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace          TEXT NOT NULL,
    hostname           TEXT NOT NULL,
    queues             TEXT[] NOT NULL,
    executors          TEXT[] NOT NULL DEFAULT '{}',
    labels             JSONB NOT NULL DEFAULT '{}',
    -- Advertised at registration.
    capacity           JSONB NOT NULL,
    -- Sum of active reservations. Maintained transactionally with dispatch and
    -- reconciled against heartbeat ground truth (§3.2).
    committed          JSONB NOT NULL DEFAULT '{}',
    state              worker_state NOT NULL DEFAULT 'ACTIVE',
    agent_version      TEXT NOT NULL,
    session_token_hash TEXT NOT NULL,
    registered_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_heartbeat_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Drives liveness sweeps; partial so dead workers do not bloat it.
CREATE INDEX idx_workers_live ON workers (last_heartbeat_at)
    WHERE state IN ('ACTIVE','DRAINING');
CREATE INDEX idx_workers_ns ON workers (namespace, state);

-- ─── Attempts: the execution unit ─────────────────────────────────────────
CREATE TYPE attempt_state AS ENUM (
    'PENDING','DISPATCHED','RUNNING','SUCCEEDED','FAILED',
    'TIMED_OUT','LOST','CANCELLED'
);

CREATE TABLE task_attempts (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id           UUID NOT NULL,
    node_id          TEXT NOT NULL,
    attempt_number   INT  NOT NULL,
    state            attempt_state NOT NULL DEFAULT 'PENDING',
    queue            TEXT NOT NULL DEFAULT 'default',
    priority         INT  NOT NULL DEFAULT 100,
    resource_request JSONB NOT NULL,
    -- Stable ACROSS retries: derived WITHOUT attempt_number, so every retry of
    -- the same logical work shares one key. That exclusion is the entire
    -- mechanism behind at-least-once + idempotency (§6.2).
    idempotency_key  TEXT NOT NULL,
    cache_key        TEXT,
    worker_id        UUID REFERENCES workers(id) ON DELETE SET NULL,
    lease_id         UUID,
    input_refs       JSONB NOT NULL DEFAULT '{}',
    output_ref       JSONB,
    error            JSONB,
    metrics          JSONB,
    -- Structured explanation for why a PENDING attempt has not dispatched,
    -- e.g. 'waiting_for_resources: gpu (need 4, free 3)'. Without this the UI
    -- can only show an unexplained stall.
    pending_reason   TEXT,
    scheduled_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    dispatched_at    TIMESTAMPTZ,
    started_at       TIMESTAMPTZ,
    ended_at         TIMESTAMPTZ,
    state_version    BIGINT NOT NULL DEFAULT 0,
    FOREIGN KEY (run_id, node_id) REFERENCES task_nodes(run_id, node_id)
        ON DELETE CASCADE,
    UNIQUE (run_id, node_id, attempt_number)
);

-- THE dispatch hot path. Partial index keeps it tiny regardless of how much
-- history accumulates, which is what makes Postgres-as-queue viable.
CREATE INDEX idx_attempts_dispatchable
    ON task_attempts (queue, priority DESC, scheduled_at ASC)
    WHERE state = 'PENDING';
CREATE INDEX idx_attempts_run ON task_attempts (run_id, node_id);
CREATE INDEX idx_attempts_worker_active ON task_attempts (worker_id)
    WHERE state IN ('DISPATCHED','RUNNING');
CREATE INDEX idx_attempts_cache ON task_attempts (cache_key)
    WHERE state = 'SUCCEEDED' AND cache_key IS NOT NULL;

-- ─── Leases: fencing (ADR-0010) ───────────────────────────────────────────
CREATE TABLE leases (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- At most ONE worker legitimately holds an attempt at a time.
    attempt_id UUID NOT NULL UNIQUE REFERENCES task_attempts(id) ON DELETE CASCADE,
    worker_id  UUID NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    -- Must be presented on every attempt RPC. A stale token is REJECTED: this
    -- cannot stop a zombie worker from executing, but it guarantees its result
    -- cannot corrupt our state.
    token      TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    renewed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    epoch      INT  NOT NULL DEFAULT 0
);
CREATE INDEX idx_leases_expiry ON leases (expires_at);

-- ─── Reservations: the capacity ledger ────────────────────────────────────
CREATE TABLE reservations (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    worker_id  UUID NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    attempt_id UUID NOT NULL UNIQUE REFERENCES task_attempts(id) ON DELETE CASCADE,
    resources  JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Created in the SAME transaction as the lease, so the ledger cannot drift
-- from dispatch reality (§5.3).
CREATE INDEX idx_reservations_worker ON reservations (worker_id);

-- ─── Events: append-only history ──────────────────────────────────────────
CREATE TABLE run_events (
    id          BIGSERIAL PRIMARY KEY,
    run_id      UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    seq         INT  NOT NULL,
    node_id     TEXT,
    attempt_id  UUID,
    kind        TEXT NOT NULL,
    payload     JSONB NOT NULL DEFAULT '{}',
    -- scheduler | worker:<id> | user:<sub>. Every transition is attributable.
    actor       TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, seq)
);
-- Append-only; never UPDATEd or DELETEd within retention. The current-state
-- tables above are a projection of this log, and on any disagreement the log
-- wins -- state can be rebuilt from it.
CREATE INDEX idx_events_run_seq ON run_events (run_id, seq);

-- ─── Timers: durable deadlines ────────────────────────────────────────────
CREATE TABLE timers (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind       TEXT NOT NULL,
    subject_id UUID NOT NULL,
    fire_at    TIMESTAMPTZ NOT NULL,
    payload    JSONB NOT NULL DEFAULT '{}',
    claimed_by TEXT,
    claimed_at TIMESTAMPTZ
);
-- EVERY deadline in the system is a row here, so a control-plane restart loses
-- no timers -- the failure mode of in-memory timer wheels (§3.2).
CREATE INDEX idx_timers_due ON timers (fire_at) WHERE claimed_by IS NULL;
CREATE INDEX idx_timers_subject ON timers (subject_id, kind);

-- ─── Idempotency for external submissions ─────────────────────────────────
CREATE TABLE idempotency_keys (
    namespace    TEXT NOT NULL,
    key          TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    response     JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, key)
);
-- Same key + same body -> cached response. Same key + DIFFERENT body -> 409,
-- which catches client bugs instead of silently doing the wrong thing.

-- ─── API keys (MVP authn: static keys, hashed at rest) ────────────────────
CREATE TABLE api_keys (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace  TEXT NOT NULL,
    name       TEXT NOT NULL,
    key_hash   TEXT NOT NULL UNIQUE,
    -- 'admin' | 'worker' | 'reader'
    role       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);

-- ─── Concurrency semaphores ───────────────────────────────────────────────
CREATE TABLE concurrency_slots (
    key        TEXT NOT NULL,
    attempt_id UUID NOT NULL REFERENCES task_attempts(id) ON DELETE CASCADE,
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (key, attempt_id)
);
CREATE INDEX idx_concurrency_key ON concurrency_slots (key);
