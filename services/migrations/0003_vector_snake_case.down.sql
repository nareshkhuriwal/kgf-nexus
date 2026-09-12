-- Intentionally a no-op. Reverting to camelCase would reintroduce the silent
-- NULL-key bug, and the readers tolerate both dialects, so rolling the code
-- back does not require rolling the data back.
SELECT 1;
