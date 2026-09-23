-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- migrate: no-transaction
-- Undo 0022. The view goes first: a scheduler built with fair share reads it,
-- and roll back the fleet (or set NEXUS_FAIR_SHARE=false) before this runs.
-- Weights stored in platform_config are untouched -- they are policy data, and
-- re-applying 0022 makes them effective again.
DROP VIEW IF EXISTS scheduler_namespace_weights;
DROP INDEX CONCURRENTLY IF EXISTS idx_attempts_inflight_by_tenant;
