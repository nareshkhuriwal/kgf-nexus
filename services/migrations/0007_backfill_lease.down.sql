-- Drops the lease columns. Safe only once no replica is claiming: without them the
-- driver cannot enforce its concurrency cap under concurrency, which is the defect
-- 0007 exists to fix.
ALTER TABLE schedule_backfills DROP COLUMN IF EXISTS claimed_at;
ALTER TABLE schedule_backfills DROP COLUMN IF EXISTS claimed_by;
