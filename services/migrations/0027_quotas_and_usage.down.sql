-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- migrate: no-transaction
-- Undo 0027. Roll the fleet back first: a scheduler built with ADR-0021 reads
-- namespace_quota_limits in every claim and writes usage_ledger in every
-- terminal transition, so dropping them under it fails those transactions.
--
-- THE LEDGER IS BILLING DATA. Dropping it destroys the only record of what was
-- consumed. Export it first (OPERATIONS §21.6). The DROP TABLE is here so the
-- migration is reversible, not as a recommendation.
--
-- Quotas stored in platform_config are untouched -- they are policy data, and
-- re-applying 0027 makes them effective again.
DROP VIEW IF EXISTS namespace_quota_limits;
DROP VIEW IF EXISTS namespace_queue_pools;
DROP FUNCTION IF EXISTS nexus_quota_limit(jsonb);
DROP TABLE IF EXISTS usage_ledger;
DROP FUNCTION IF EXISTS usage_ledger_append_only();
DROP INDEX CONCURRENTLY IF EXISTS idx_attempts_inflight_tenant;
