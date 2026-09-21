-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- migrate: no-transaction
-- Undo 0015: drop the tenant indexes and relax the NOT NULL enforcement.
--
-- The column, its data and the triggers stay: those are 0014's, and dropping
-- them here would discard the backfill rather than undo this migration.
-- Reversing enforcement is enough to let an older fleet serve again.

-- DROP INDEX CONCURRENTLY also cannot run inside a transaction block, so this
-- file declares `migrate: no-transaction` too. IF EXISTS on every drop keeps a
-- partial rollback safe to re-run.
DROP INDEX CONCURRENTLY IF EXISTS workflow_specs_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS workflow_runs_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS workers_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS idempotency_keys_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS api_keys_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS schedules_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS schedule_backfills_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS users_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS scoped_tokens_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS audit_log_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS platform_config_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS rbac_roles_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS rbac_user_access_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS run_deletions_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS task_nodes_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS task_edges_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS task_attempts_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS leases_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS reservations_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS run_events_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS concurrency_slots_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS schedule_runs_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS sessions_tenant_idx;
DROP INDEX CONCURRENTLY IF EXISTS timers_tenant_idx;

-- Relaxing NOT NULL is always safe: it only widens what a write may contain.
ALTER TABLE workflow_specs ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE workflow_runs ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE workers ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE idempotency_keys ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE api_keys ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE schedules ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE schedule_backfills ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE users ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE scoped_tokens ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE audit_log ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE platform_config ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE rbac_roles ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE rbac_user_access ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE run_deletions ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE task_nodes ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE task_edges ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE task_attempts ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE leases ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE reservations ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE run_events ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE concurrency_slots ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE schedule_runs ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE sessions ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE timers ALTER COLUMN tenant_id DROP NOT NULL;
