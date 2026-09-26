-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Reverses 0035. Run only after every service is back on a release that does
-- not read these columns (ADR-0011 rule 5). Fractional-GPU keys inside the
-- JSONB vectors are left in place: older readers ignore them.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_scheduler') THEN
        REVOKE UPDATE (attempts_used, preemptions_used) ON task_nodes FROM nexus_scheduler;
    END IF;
END $$;
ALTER TABLE cluster_status
    DROP COLUMN IF EXISTS headroom_observed_at,
    DROP COLUMN IF EXISTS headroom,
    DROP COLUMN IF EXISTS labels;
ALTER TABLE workers DROP COLUMN IF EXISTS cached_artifacts;
ALTER TABLE task_nodes DROP COLUMN IF EXISTS preemptions_used;
