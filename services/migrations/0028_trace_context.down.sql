-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
-- Reverse 0028.
--
-- Roll the scheduler, dag-engine, workflow-manager and event-service back
-- BEFORE running this: they write and read the columns. Nothing but traces is
-- lost -- runs in flight simply stop being connected to their creating trace.
ALTER TABLE task_attempts DROP CONSTRAINT IF EXISTS task_attempts_trace_context_len;
ALTER TABLE outbox        DROP CONSTRAINT IF EXISTS outbox_trace_context_len;
ALTER TABLE task_attempts DROP COLUMN IF EXISTS trace_context;
ALTER TABLE outbox        DROP COLUMN IF EXISTS trace_context;
