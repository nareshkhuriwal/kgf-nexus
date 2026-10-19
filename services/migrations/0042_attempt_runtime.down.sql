-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Reverses 0042. Roll the api-gateway back first, or restart it after this:
-- a gateway that found the table at startup offers ReportPlacement, and its
-- writes fail once the table is gone (each agent logs the failure and keeps
-- running its attempt; placement is advisory). The task-runner listing reads
-- the table and answers 500 without it. Dropping the table discards the
-- recorded placements and drops its grants with it.
DROP TRIGGER IF EXISTS tenant_assignment ON attempt_runtime;
DROP TABLE IF EXISTS attempt_runtime;
