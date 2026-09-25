-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- Reverse 0026.
--
-- Roll the dag-engine and scheduler back BEFORE running this: both read the
-- column. A run retried by node while 0026 was applied loses its record of which
-- attempts were superseded, so an older engine derives those nodes from their
-- latest attempt again -- the behaviour before 0026, not a corruption.
ALTER TABLE task_nodes DROP COLUMN IF EXISTS retry_floor;
