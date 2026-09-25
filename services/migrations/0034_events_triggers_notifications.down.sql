-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- migrate: no-transaction
-- Undo 0034. Roll the fleet back first: an event-service built with ADR-0028
-- claims outbox rows by notified_at and writes the delivery tables, and a
-- workflow-manager built with it leases sensor_state.
--
-- This destroys notification configuration, delivery history (including dead
-- letters), webhook triggers and their secrets, sensor definitions and state,
-- and recorded lineage. Export what you need first (OPERATIONS §27).
DROP TABLE IF EXISTS asset_materializations;
DROP TABLE IF EXISTS asset_lineage_edges;
DROP TABLE IF EXISTS assets;
DROP TABLE IF EXISTS sensor_observations;
DROP TABLE IF EXISTS sensor_state;
DROP TABLE IF EXISTS sensors;
DROP TABLE IF EXISTS webhook_receipts;
DROP TABLE IF EXISTS webhook_triggers;
DROP TABLE IF EXISTS notification_incidents;
DROP TABLE IF EXISTS notification_channel_windows;
DROP TABLE IF EXISTS notification_deliveries;
DROP TABLE IF EXISTS notification_rule_channels;
DROP TABLE IF EXISTS notification_rules;
DROP TABLE IF EXISTS notification_channels;
DROP INDEX CONCURRENTLY IF EXISTS outbox_unnotified_idx;
ALTER TABLE outbox DROP COLUMN IF EXISTS notified_at;
