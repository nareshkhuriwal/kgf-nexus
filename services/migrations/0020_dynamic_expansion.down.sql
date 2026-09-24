-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
--
-- Drops the expansion record. Expansion CHILDREN are ordinary task_nodes rows
-- and are deliberately left alone: removing them would rewrite the history of
-- runs that already executed them. A dag-engine that predates 0020 must not be
-- running a spec with a MapSpec when this is applied.
DROP TABLE IF EXISTS fan_in_refusals;
DROP TABLE IF EXISTS task_expansions;
