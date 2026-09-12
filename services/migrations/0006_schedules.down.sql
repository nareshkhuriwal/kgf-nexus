-- Drops the schedule tables. Destructive: every schedule definition and every
-- backfill's progress is in these rows, and nothing else records them.
--
-- schedule_runs and schedule_backfills cascade from schedules, but they are dropped
-- explicitly and in dependency order so the intent is visible rather than implied.
DROP TABLE IF EXISTS schedule_runs;
DROP TABLE IF EXISTS schedule_backfills;
DROP TABLE IF EXISTS schedules;
