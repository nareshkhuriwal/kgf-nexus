-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- migrate: no-transaction
--
-- Notifications, inbound webhook triggers, sensors and declared asset lineage
-- (ADR-0028).
--
-- WHY no-transaction. One statement needs CREATE INDEX CONCURRENTLY: the partial
-- index over the outbox's un-notified rows. The outbox is written by every
-- claim, and a plain CREATE INDEX would block those writers for the whole build.
-- Everything else here is safe to re-run -- IF NOT EXISTS on every table, column
-- and index, DROP/CREATE on every trigger, and idempotent grants -- so a failure
-- part way through is recovered by running the migration again, as 0015 is.
--
-- ADDITIVE (ADR-0011 rule 5). New tables, plus ONE nullable column on the
-- outbox. An old writer does not know the column and inserts NULL, which is
-- exactly "not yet notified". An old relay prunes published rows without
-- looking at the column. That can only drop a row an hour after it was
-- published, long after a live notifier has evaluated it.
--
-- OWNERSHIP (amends ADR-0011; see ADR-0028 for the matrix):
--   api-gateway      notification_channels, notification_rules,
--                    notification_rule_channels, webhook_triggers,
--                    webhook_receipts, sensors
--   event-service    notification_deliveries, notification_channel_windows,
--                    notification_incidents, assets, asset_lineage_edges,
--                    asset_materializations, and outbox.notified_at
--   workflow-manager sensor_state, sensor_observations
-- One documented cross-owner grant: the gateway may INSERT into
-- notification_deliveries (a channel test, a dead-letter redrive).
--
-- TENANCY. Every table carries namespace and tenant_id. tenant_id is assigned by
-- the 0014 trigger, so no writer has to know it.

-- ─── outbox: the notifier's marker ─────────────────────────────────────────

-- NULL until the notifier (event-service) has evaluated the row. That is also
-- the value for every row that exists when this runs. Those rows are evaluated
-- once and match nothing, because no rule can predate this migration.
ALTER TABLE outbox ADD COLUMN IF NOT EXISTS notified_at TIMESTAMPTZ;

-- The notifier's work queue: rows of the two topics it reads, not yet
-- evaluated. Partial, so a steady state of zero backlog is a probe of an empty
-- index, for the same reason as outbox_unpublished_idx.
CREATE INDEX CONCURRENTLY IF NOT EXISTS outbox_unnotified_idx
    ON outbox (id) WHERE notified_at IS NULL AND topic IN ('run_state', 'run_event');

-- ─── notification configuration (api-gateway) ──────────────────────────────

CREATE TABLE IF NOT EXISTS notification_channels (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace              TEXT NOT NULL,
    tenant_id              UUID REFERENCES tenants(id),
    name                   TEXT NOT NULL,
    -- Plain text with a CHECK, not an enum: adding a channel kind is a
    -- constraint swap in a later migration, not a type migration.
    kind                   TEXT NOT NULL CHECK (kind IN
                               ('slack_webhook', 'slack_bot', 'teams', 'pagerduty', 'webhook', 'email')),
    -- Non-secret settings only: a Slack channel id, email recipients, a
    -- PagerDuty severity, a generic webhook's URL. Secret material never lands
    -- here, so it can be returned by every read.
    config                 JSONB NOT NULL DEFAULT '{}',
    -- The secret (Slack/Teams webhook URL, bot token, PagerDuty routing key,
    -- webhook signing key) is EITHER sealed here (internal/secretbox, AES-GCM,
    -- AAD bound to this row) OR referenced (env:NAME / file:/path, resolved by
    -- the event-service when it sends). Never both.
    secret_ciphertext      BYTEA,
    secret_ref             TEXT,
    -- What a read may show: the last few characters, or a URL's host.
    secret_hint            TEXT NOT NULL DEFAULT '',
    rate_limit_per_minute  INT NOT NULL DEFAULT 30 CHECK (rate_limit_per_minute BETWEEN 1 AND 6000),
    enabled                BOOLEAN NOT NULL DEFAULT true,
    created_by             TEXT NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    revision               BIGINT NOT NULL DEFAULT 1,
    UNIQUE (namespace, name),
    CHECK (NOT (secret_ciphertext IS NOT NULL AND secret_ref IS NOT NULL))
);

CREATE TABLE IF NOT EXISTS notification_rules (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace    TEXT NOT NULL,
    tenant_id    UUID REFERENCES tenants(id),
    name         TEXT NOT NULL,
    -- run.failed | run.succeeded | run.timed_out | run.sla_exceeded | attempt.lost.
    -- Validated by the gateway, not a CHECK, so a new event type is a code change.
    events       TEXT[] NOT NULL,
    -- Empty means every workflow in the namespace.
    workflows    TEXT[] NOT NULL DEFAULT '{}',
    -- Every key/value here must equal the workflow's spec labels.
    labels       JSONB NOT NULL DEFAULT '{}',
    -- Required when events contains run.sla_exceeded: a run still active this
    -- long after it was created is overdue.
    sla_seconds  INT CHECK (sla_seconds IS NULL OR sla_seconds BETWEEN 60 AND 2592000),
    enabled      BOOLEAN NOT NULL DEFAULT true,
    created_by   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    revision     BIGINT NOT NULL DEFAULT 1,
    UNIQUE (namespace, name),
    CHECK (cardinality(events) > 0)
);

-- A join table rather than a UUID[] on the rule, so deleting a channel removes
-- it from every rule by FK instead of leaving dangling ids for the evaluator.
CREATE TABLE IF NOT EXISTS notification_rule_channels (
    rule_id     UUID NOT NULL REFERENCES notification_rules(id) ON DELETE CASCADE,
    channel_id  UUID NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    namespace   TEXT NOT NULL,
    tenant_id   UUID REFERENCES tenants(id),
    PRIMARY KEY (rule_id, channel_id)
);
CREATE INDEX IF NOT EXISTS notification_rule_channels_channel_idx ON notification_rule_channels (channel_id);
CREATE INDEX IF NOT EXISTS notification_rules_ns_idx ON notification_rules (namespace) WHERE enabled;

-- ─── notification delivery (event-service) ─────────────────────────────────

CREATE TABLE IF NOT EXISTS notification_deliveries (
    id               BIGSERIAL PRIMARY KEY,
    namespace        TEXT NOT NULL,
    tenant_id        UUID REFERENCES tenants(id),
    channel_id       UUID NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    rule_id          UUID REFERENCES notification_rules(id) ON DELETE SET NULL,
    -- THE dedup key: derived from the fact (run_state:<run>:<version>,
    -- run_event:<id>, sla:<rule>:<run>, ...). Two rules matching one event on
    -- one channel, or one outbox row evaluated twice, insert one row.
    event_key        TEXT NOT NULL,
    -- What the receiver sees as the idempotency key. Equal to a hash of
    -- (channel, event_key) except on a redrive, which keeps the original's so the
    -- receiver can still de-duplicate.
    dedup_key        TEXT NOT NULL,
    event_type       TEXT NOT NULL,
    -- trigger | resolve (PagerDuty recovery) | test
    action           TEXT NOT NULL DEFAULT 'trigger' CHECK (action IN ('trigger', 'resolve', 'test')),
    run_id           UUID,
    payload          JSONB NOT NULL DEFAULT '{}',
    state            TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'delivered', 'dead')),
    attempts         INT NOT NULL DEFAULT 0,
    max_attempts     INT NOT NULL DEFAULT 8 CHECK (max_attempts BETWEEN 1 AND 50),
    next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A dispatcher's claim. Sending happens outside any transaction; the lease
    -- is what keeps a second replica off the row meanwhile, and its expiry is
    -- what re-sends a row whose dispatcher died mid-send.
    lease_owner      TEXT,
    lease_until      TIMESTAMPTZ,
    last_error       TEXT,
    last_status      INT,
    redrive_of       BIGINT REFERENCES notification_deliveries(id) ON DELETE SET NULL,
    created_by       TEXT NOT NULL DEFAULT 'notifier',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at     TIMESTAMPTZ,
    dead_at          TIMESTAMPTZ,
    UNIQUE (channel_id, event_key)
);
CREATE INDEX IF NOT EXISTS notification_deliveries_due_idx
    ON notification_deliveries (next_attempt_at) WHERE state = 'pending';
CREATE INDEX IF NOT EXISTS notification_deliveries_ns_idx
    ON notification_deliveries (namespace, created_at DESC);
CREATE INDEX IF NOT EXISTS notification_deliveries_dead_idx
    ON notification_deliveries (namespace, dead_at DESC) WHERE state = 'dead';

-- Per-channel fixed one-minute window, taken with a conditional UPDATE so the
-- limit is exact across replicas. Separate from the channel row because the
-- channel belongs to the gateway and this changes on every send.
CREATE TABLE IF NOT EXISTS notification_channel_windows (
    channel_id    UUID PRIMARY KEY REFERENCES notification_channels(id) ON DELETE CASCADE,
    namespace     TEXT NOT NULL,
    tenant_id     UUID REFERENCES tenants(id),
    window_start  TIMESTAMPTZ NOT NULL,
    sent          INT NOT NULL DEFAULT 0
);

-- PagerDuty incidents this platform opened and has not yet resolved. A recovery
-- sends a resolve only when a row here is open, so a success with nothing open
-- is silent.
CREATE TABLE IF NOT EXISTS notification_incidents (
    rule_id        UUID NOT NULL REFERENCES notification_rules(id) ON DELETE CASCADE,
    channel_id     UUID NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    workflow       TEXT NOT NULL,
    namespace      TEXT NOT NULL,
    tenant_id      UUID REFERENCES tenants(id),
    dedup_key      TEXT NOT NULL,
    opened_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    opened_by_run  UUID,
    resolved_at    TIMESTAMPTZ,
    resolved_by_run UUID,
    PRIMARY KEY (rule_id, channel_id, workflow)
);

-- ─── inbound webhook triggers (api-gateway) ────────────────────────────────

CREATE TABLE IF NOT EXISTS webhook_triggers (
    id                          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace                   TEXT NOT NULL,
    tenant_id                   UUID REFERENCES tenants(id),
    name                        TEXT NOT NULL,
    spec_name                   TEXT NOT NULL,
    spec_version                INT,
    -- param name -> "$.json.path" | "text with {{ $.path }}" | literal
    params_mapping              JSONB NOT NULL DEFAULT '{}',
    -- JSONPath into the payload whose value keys the run (webhook:<id>:<value>).
    idempotency_key_path        TEXT,
    priority                    INT NOT NULL DEFAULT 100,
    tolerance_seconds           INT NOT NULL DEFAULT 300 CHECK (tolerance_seconds BETWEEN 30 AND 3600),
    -- Sealed (internal/secretbox), AAD bound to the trigger id. Verification
    -- needs the secret itself, so it cannot be hashed like an API key.
    secret_ciphertext           BYTEA NOT NULL,
    previous_secret_ciphertext  BYTEA,
    previous_secret_expires_at  TIMESTAMPTZ,
    secret_rotated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    enabled                     BOOLEAN NOT NULL DEFAULT true,
    created_by                  TEXT NOT NULL,
    created_by_id               UUID,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_fired_at               TIMESTAMPTZ,
    fire_count                  BIGINT NOT NULL DEFAULT 0,
    revision                    BIGINT NOT NULL DEFAULT 1,
    UNIQUE (namespace, name)
);

-- Replay protection and the delivery log. The primary key IS the replay
-- guard: a second delivery carrying the same signature conflicts.
CREATE TABLE IF NOT EXISTS webhook_receipts (
    trigger_id        UUID NOT NULL REFERENCES webhook_triggers(id) ON DELETE CASCADE,
    signature_digest  TEXT NOT NULL,
    namespace         TEXT NOT NULL,
    tenant_id         UUID REFERENCES tenants(id),
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- accepted | deduplicated | rejected
    outcome           TEXT NOT NULL DEFAULT 'accepted',
    http_status       INT NOT NULL DEFAULT 202,
    run_id            UUID,
    idempotency_key   TEXT,
    detail            TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (trigger_id, signature_digest)
);
CREATE INDEX IF NOT EXISTS webhook_receipts_time_idx ON webhook_receipts (trigger_id, received_at DESC);

-- ─── sensors ───────────────────────────────────────────────────────────────

-- Definitions (api-gateway): configuration an operator edits.
CREATE TABLE IF NOT EXISTS sensors (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace              TEXT NOT NULL,
    tenant_id              UUID REFERENCES tenants(id),
    name                   TEXT NOT NULL,
    kind                   TEXT NOT NULL CHECK (kind IN ('object_store', 'run_succeeded')),
    spec_name              TEXT NOT NULL,
    spec_version           INT,
    -- object_store:  {"uri": "s3://bucket/prefix/", "pattern": "*.parquet"}
    -- run_succeeded: {"upstream_workflow": "name"}
    config                 JSONB NOT NULL DEFAULT '{}',
    params_mapping         JSONB NOT NULL DEFAULT '{}',
    poll_interval_seconds  INT NOT NULL DEFAULT 30 CHECK (poll_interval_seconds BETWEEN 5 AND 86400),
    -- false: record what already exists as a baseline and trigger only on new
    -- arrivals. true: trigger for everything present too.
    backfill               BOOLEAN NOT NULL DEFAULT false,
    priority               INT NOT NULL DEFAULT 100,
    enabled                BOOLEAN NOT NULL DEFAULT true,
    created_by             TEXT NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    revision               BIGINT NOT NULL DEFAULT 1,
    UNIQUE (namespace, name)
);

-- Runtime state (workflow-manager): the lease, the cursor and the health.
CREATE TABLE IF NOT EXISTS sensor_state (
    sensor_id             UUID PRIMARY KEY REFERENCES sensors(id) ON DELETE CASCADE,
    namespace             TEXT NOT NULL,
    tenant_id             UUID REFERENCES tenants(id),
    -- The sensors.revision this state was built for. A changed definition
    -- restarts the listing and, without backfill, re-baselines.
    definition_revision   BIGINT NOT NULL DEFAULT 0,
    status                TEXT NOT NULL DEFAULT 'initializing'
                              CHECK (status IN ('initializing', 'ok', 'error')),
    baseline_done         BOOLEAN NOT NULL DEFAULT false,
    -- object_store: the last key listed (S3 StartAfter); '' restarts the sweep.
    cursor                TEXT NOT NULL DEFAULT '',
    -- run_succeeded: ended_at of the newest upstream run seen.
    watermark             TIMESTAMPTZ,
    next_poll_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_owner           TEXT,
    lease_until           TIMESTAMPTZ,
    last_poll_at          TIMESTAMPTZ,
    last_success_at       TIMESTAMPTZ,
    last_error            TEXT,
    consecutive_failures  INT NOT NULL DEFAULT 0,
    observed_total        BIGINT NOT NULL DEFAULT 0,
    triggered_total       BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS sensor_state_due_idx ON sensor_state (next_poll_at);

-- One row per observed object or upstream run. The primary key is the
-- at-most-one-trigger guarantee.
CREATE TABLE IF NOT EXISTS sensor_observations (
    sensor_id        UUID NOT NULL REFERENCES sensors(id) ON DELETE CASCADE,
    item_key         TEXT NOT NULL,
    namespace        TEXT NOT NULL,
    tenant_id        UUID REFERENCES tenants(id),
    item             JSONB NOT NULL DEFAULT '{}',
    -- pending:   claimed, run not yet created (a crash here is retried under
    --            the same idempotency key)
    -- triggered: run created (run_id set)
    -- baseline:  existed before the sensor started; never triggers
    -- failed:    run creation refused permanently (e.g. the spec is gone)
    outcome          TEXT NOT NULL DEFAULT 'pending'
                         CHECK (outcome IN ('pending', 'triggered', 'baseline', 'failed')),
    idempotency_key  TEXT NOT NULL,
    run_id           UUID,
    attempts         INT NOT NULL DEFAULT 0,
    last_error       TEXT,
    observed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    triggered_at     TIMESTAMPTZ,
    PRIMARY KEY (sensor_id, item_key)
);
CREATE INDEX IF NOT EXISTS sensor_observations_pending_idx
    ON sensor_observations (sensor_id) WHERE outcome = 'pending';
CREATE INDEX IF NOT EXISTS sensor_observations_recent_idx
    ON sensor_observations (sensor_id, observed_at DESC);

-- ─── asset lineage (event-service) ─────────────────────────────────────────

CREATE TABLE IF NOT EXISTS assets (
    namespace                 TEXT NOT NULL,
    tenant_id                 UUID REFERENCES tenants(id),
    uri                       TEXT NOT NULL,
    name                      TEXT NOT NULL DEFAULT '',
    first_seen_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_declared_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_materialized_at      TIMESTAMPTZ,
    last_materialized_run_id  UUID REFERENCES workflow_runs(id) ON DELETE SET NULL,
    PRIMARY KEY (namespace, uri)
);

-- What each run's tasks DECLARED they produce and consume. Declared lineage,
-- recorded once per run; it states what a task said, not what it touched.
CREATE TABLE IF NOT EXISTS asset_lineage_edges (
    run_id       UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    node_id      TEXT NOT NULL,
    direction    TEXT NOT NULL CHECK (direction IN ('produces', 'consumes')),
    asset_uri    TEXT NOT NULL,
    asset_name   TEXT NOT NULL DEFAULT '',
    namespace    TEXT NOT NULL,
    tenant_id    UUID REFERENCES tenants(id),
    spec_name    TEXT NOT NULL,
    recorded_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, node_id, direction, asset_uri)
);
CREATE INDEX IF NOT EXISTS asset_lineage_edges_asset_idx
    ON asset_lineage_edges (namespace, asset_uri, direction, recorded_at DESC);

CREATE TABLE IF NOT EXISTS asset_materializations (
    id               BIGSERIAL PRIMARY KEY,
    namespace        TEXT NOT NULL,
    tenant_id        UUID REFERENCES tenants(id),
    asset_uri        TEXT NOT NULL,
    asset_name       TEXT NOT NULL DEFAULT '',
    run_id           UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    node_id          TEXT NOT NULL,
    spec_name        TEXT NOT NULL,
    materialized_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, node_id, asset_uri)
);
CREATE INDEX IF NOT EXISTS asset_materializations_asset_idx
    ON asset_materializations (namespace, asset_uri, materialized_at DESC);

-- ─── tenant assignment (the 0014 trigger) ──────────────────────────────────

DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'notification_channels', 'notification_rules', 'notification_rule_channels',
        'notification_deliveries', 'notification_channel_windows', 'notification_incidents',
        'webhook_triggers', 'webhook_receipts', 'sensors', 'sensor_state',
        'sensor_observations', 'assets', 'asset_lineage_edges', 'asset_materializations'
    ] LOOP
        -- Created only when absent (a re-run is a no-op); these tables are
        -- new in this migration, so there is never an old trigger to replace.
        IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'tenant_assignment' AND tgrelid = t::regclass) THEN
            EXECUTE format('CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON %I '
                           'FOR EACH ROW EXECUTE FUNCTION public.assign_namespace_tenant()', t);
        END IF;
    END LOOP;
END $$;

CREATE INDEX IF NOT EXISTS notification_channels_tenant_idx ON notification_channels (tenant_id);
CREATE INDEX IF NOT EXISTS notification_rules_tenant_idx ON notification_rules (tenant_id);
CREATE INDEX IF NOT EXISTS notification_rule_channels_tenant_idx ON notification_rule_channels (tenant_id);
CREATE INDEX IF NOT EXISTS notification_deliveries_tenant_idx ON notification_deliveries (tenant_id);
CREATE INDEX IF NOT EXISTS notification_channel_windows_tenant_idx ON notification_channel_windows (tenant_id);
CREATE INDEX IF NOT EXISTS notification_incidents_tenant_idx ON notification_incidents (tenant_id);
CREATE INDEX IF NOT EXISTS webhook_triggers_tenant_idx ON webhook_triggers (tenant_id);
CREATE INDEX IF NOT EXISTS webhook_receipts_tenant_idx ON webhook_receipts (tenant_id);
CREATE INDEX IF NOT EXISTS sensors_tenant_idx ON sensors (tenant_id);
CREATE INDEX IF NOT EXISTS sensor_state_tenant_idx ON sensor_state (tenant_id);
CREATE INDEX IF NOT EXISTS sensor_observations_tenant_idx ON sensor_observations (tenant_id);
CREATE INDEX IF NOT EXISTS assets_tenant_idx ON assets (tenant_id);
CREATE INDEX IF NOT EXISTS asset_lineage_edges_tenant_idx ON asset_lineage_edges (tenant_id);
CREATE INDEX IF NOT EXISTS asset_materializations_tenant_idx ON asset_materializations (tenant_id);

-- ─── grants (0005 style; ADR-0011 rule 1) ──────────────────────────────────

DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT ON notification_channels, notification_rules, '
                'notification_rule_channels, notification_deliveries, notification_channel_windows, '
                'notification_incidents, webhook_triggers, webhook_receipts, sensors, sensor_state, '
                'sensor_observations, assets, asset_lineage_edges, asset_materializations TO %I', r);
        END IF;
    END LOOP;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_api_gateway') THEN
        GRANT INSERT, UPDATE, DELETE ON notification_channels, notification_rules,
            notification_rule_channels, webhook_triggers, webhook_receipts, sensors
            TO nexus_api_gateway;
        -- The documented exception: a channel test and a dead-letter redrive are
        -- NEW delivery rows. INSERT only, so the gateway can never lease, mark
        -- or delete a delivery.
        GRANT INSERT ON notification_deliveries TO nexus_api_gateway;
        GRANT USAGE ON SEQUENCE notification_deliveries_id_seq TO nexus_api_gateway;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_event_service') THEN
        GRANT INSERT, UPDATE, DELETE ON notification_deliveries, notification_channel_windows,
            notification_incidents, assets, asset_lineage_edges, asset_materializations
            TO nexus_event_service;
        GRANT USAGE ON SEQUENCE notification_deliveries_id_seq, asset_materializations_id_seq
            TO nexus_event_service;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_workflow_manager') THEN
        GRANT INSERT, UPDATE, DELETE ON sensor_state, sensor_observations TO nexus_workflow_manager;
    END IF;
END $$;
