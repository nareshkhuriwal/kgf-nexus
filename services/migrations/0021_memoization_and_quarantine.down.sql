-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- migrate: no-transaction
--
-- Break-glass rollback of 0021. Release every quarantined worker FIRST
-- (POST .../workers/{id}/release): with the episode table gone nothing can
-- expire a quarantine, and a QUARANTINED worker would stay out of the pool until
-- an operator moved it by hand. Memo hits already recorded keep their
-- SUCCEEDED state; only the provenance column is dropped.
DROP INDEX CONCURRENTLY IF EXISTS idx_attempts_worker_ended;
DROP TABLE IF EXISTS worker_quarantines;
DROP TABLE IF EXISTS memo_invalidations;
ALTER TABLE task_attempts DROP COLUMN IF EXISTS cache_hit_of;
