-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
--
-- Reverts 0039 (F160). Roll the dag-engine back first: an engine that
-- consumes the queue fails its claims once the table is gone. Dropping the
-- table discards any queued requests; the stalled-run sweep in the
-- workflow-manager is the backstop that remains for the runs they named.
-- Dropping the table drops its grants with it.
DROP TRIGGER IF EXISTS run_reconcile_on_attempt_terminal ON task_attempts;
DROP TRIGGER IF EXISTS run_reconcile_on_attempt_born_terminal ON task_attempts;
DROP TRIGGER IF EXISTS run_reconcile_on_run_state ON workflow_runs;
DROP FUNCTION IF EXISTS run_reconcile_on_attempt_terminal();
DROP FUNCTION IF EXISTS run_reconcile_on_run_state();
DROP TABLE IF EXISTS run_reconcile_requests;
