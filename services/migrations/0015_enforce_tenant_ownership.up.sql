-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- migrate: no-transaction
-- Enforce tenant ownership, one release after 0014 added and backfilled it.
--
-- 0014 added tenant_id, backfilled it, and installed the triggers that keep it
-- correct. It deliberately stopped there. This migration is the second half,
-- and it is a SEPARATE migration rather than the tail of that one because of
-- ADR-0011 rule 5: a rolling deploy runs mixed versions against one schema.
--
-- SET NOT NULL rejects an INSERT from any replica that predates the column, so
-- applying it while old replicas still serve takes them down mid-deploy. It is
-- safe only once every replica writes tenant_id -- which the 0014 triggers
-- guarantee, since they assign it on INSERT and UPDATE regardless of whether
-- the writing code knows the column exists.
--
-- Do not apply this in the same release as 0014.
--
-- CREATE INDEX CONCURRENTLY cannot run inside a transaction block, so this
-- migration declares `migrate: no-transaction` below and migrate.sh applies it
-- without --single-transaction.
--
-- That trades atomicity away, so every statement here is written to be safe to
-- re-run: IF NOT EXISTS on each index, and SET NOT NULL is idempotent by
-- nature. A failure part way through is recovered by running the migration
-- again -- the ledger records it only on success. A CONCURRENTLY build that
-- fails leaves an INVALID index, which the retry drops and rebuilds rather
-- than silently using.

-- Enforce the column now that every replica populates it.
ALTER TABLE workflow_specs ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE workflow_runs ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE workers ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE idempotency_keys ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE api_keys ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE schedules ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE schedule_backfills ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE users ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE scoped_tokens ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE audit_log ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE platform_config ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE rbac_roles ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE rbac_user_access ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE run_deletions ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE task_nodes ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE task_edges ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE task_attempts ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE leases ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE reservations ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE run_events ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE concurrency_slots ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE schedule_runs ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE sessions ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE timers ALTER COLUMN tenant_id SET NOT NULL;
-- Built CONCURRENTLY: a plain CREATE INDEX holds a write lock for the whole
-- build, which on a populated table is an outage rather than a migration.
CREATE INDEX CONCURRENTLY IF NOT EXISTS workflow_specs_tenant_idx ON workflow_specs(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS workflow_runs_tenant_idx ON workflow_runs(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS workers_tenant_idx ON workers(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idempotency_keys_tenant_idx ON idempotency_keys(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS api_keys_tenant_idx ON api_keys(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS schedules_tenant_idx ON schedules(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS schedule_backfills_tenant_idx ON schedule_backfills(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS users_tenant_idx ON users(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS scoped_tokens_tenant_idx ON scoped_tokens(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS audit_log_tenant_idx ON audit_log(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS platform_config_tenant_idx ON platform_config(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS rbac_roles_tenant_idx ON rbac_roles(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS rbac_user_access_tenant_idx ON rbac_user_access(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS run_deletions_tenant_idx ON run_deletions(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS task_nodes_tenant_idx ON task_nodes(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS task_edges_tenant_idx ON task_edges(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS task_attempts_tenant_idx ON task_attempts(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS leases_tenant_idx ON leases(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS reservations_tenant_idx ON reservations(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS run_events_tenant_idx ON run_events(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS concurrency_slots_tenant_idx ON concurrency_slots(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS schedule_runs_tenant_idx ON schedule_runs(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS sessions_tenant_idx ON sessions(tenant_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS timers_tenant_idx ON timers(tenant_id);