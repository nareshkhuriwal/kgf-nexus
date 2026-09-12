-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Intentionally a no-op. Reverting to camelCase would reintroduce the silent
-- NULL-key bug, and the readers tolerate both dialects, so rolling the code
-- back does not require rolling the data back.
SELECT 1;
