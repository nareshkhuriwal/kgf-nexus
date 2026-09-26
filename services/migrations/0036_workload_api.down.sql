-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
--
-- Reverts 0036 (ADR-0032). DESTRUCTIVE: drops reported usage units, bucket
-- state and checkpoint pointers. The usage ledger keeps the units already copied
-- into its rows. Checkpoint objects become orphans for artifact-gc.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_scheduler') THEN
        -- attempts_used stays: 0035 granted it, and 0035's rollback revokes it.
        REVOKE UPDATE (free_retries) ON task_nodes FROM nexus_scheduler;
    END IF;
END $$;
ALTER TABLE task_nodes DROP COLUMN IF EXISTS free_retries;
DROP TABLE IF EXISTS task_checkpoints;
DROP VIEW IF EXISTS namespace_rate_limits;
DROP TABLE IF EXISTS rate_limit_buckets;
DROP TABLE IF EXISTS attempt_usage;
