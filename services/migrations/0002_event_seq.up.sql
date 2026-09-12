-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- F7: run_events.seq had no safe allocation mechanism.
-- UNIQUE(run_id, seq) with a plain INT and no counter forces max(seq)+1, which
-- races: concurrent event writers for one run collide on the unique constraint.
-- An in-row counter makes allocation atomic
-- (UPDATE ... SET event_seq = event_seq + 1 RETURNING event_seq), contending
-- only on that run's own row, with no advisory lock and no serialization of
-- unrelated runs.
ALTER TABLE workflow_runs ADD COLUMN event_seq INT NOT NULL DEFAULT 0;

-- Backfill so existing runs continue from their current high-water mark rather
-- than restarting at 1 and colliding with history.
UPDATE workflow_runs r
   SET event_seq = COALESCE((SELECT max(e.seq) FROM run_events e WHERE e.run_id = r.id), 0);
