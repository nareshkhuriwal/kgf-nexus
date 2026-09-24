-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- migrate: no-transaction
-- Reverse 0023.
--
-- Roll the schedulers back (or restart them) BEFORE running this. A scheduler
-- decides at startup whether nexus_dispatch_rank exists and uses the windowed
-- claim only if it does; one started while 0023 was applied would otherwise
-- fail every pull claim once the function is gone. A restart re-detects it and
-- falls back to the exhaustive query.
DROP INDEX CONCURRENTLY IF EXISTS idx_attempts_unexplained;
DROP INDEX CONCURRENTLY IF EXISTS idx_attempts_dispatch_rank;
DROP FUNCTION IF EXISTS nexus_dispatch_rank(integer, timestamptz, double precision);
