-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Deliberately a no-op.
--
-- Rolling this back would mean moving event_seq BACKWARD, below events that
-- already exist, which is precisely the broken state 0004 repaired: the next
-- append would collide on UNIQUE(run_id, seq). A counter that correctly reflects
-- the rows present is safe for every reader and for both allocators, so there is
-- nothing to undo. Same reasoning as 0003's down migration.
SELECT 1;
