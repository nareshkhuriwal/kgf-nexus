-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0
--
-- Durable task logs: the control-plane INDEX over log chunks that agents write
-- directly to the result store (ADR-0020).
--
-- The bytes are NOT here. Each chunk is an NDJSON object at
-- <namespace>/logs/<attempt_id>/<seq>.ndjson, written by the agent with its
-- existing object-store credentials; the agent then reports the chunk's
-- metadata through WorkerService.ReportLogs, which is lease-fenced. These rows
-- are what makes a chunk part of an attempt's log: an object no row names --
-- written by a stale attempt, or superseded by compaction -- is an orphan the
-- artifact-gc sweep collects after its grace period.
--
-- ADDITIVE (ADR-0011 rule 5): two new tables, their indexes and grants. Nothing
-- existing is altered, so a replica that predates this migration is unaffected:
-- it never reads these tables, and an old gateway never advertises log shipping,
-- so no agent writes objects that would need them.
--
-- Transactional (no `migrate: no-transaction`): the tables are new and empty,
-- so their indexes are built in the same transaction without locking anyone.

-- One row per attempt that has shipped anything. Created by the first
-- ReportLogs; the counters advance only by chunk rows actually inserted, in the
-- same transaction, so a redelivered chunk cannot double-count.
CREATE TABLE IF NOT EXISTS attempt_logs (
    attempt_id       UUID PRIMARY KEY REFERENCES task_attempts(id) ON DELETE CASCADE,
    -- Copied from the attempt's run so the tenant trigger and the namespace
    -- check on keys need no join. Immutable, like every tenant assignment.
    namespace        TEXT NOT NULL,
    tenant_id        UUID REFERENCES tenants(id),
    -- open:     chunks may still arrive.
    -- complete: the agent sent its final report; the totals are authoritative.
    -- An attempt that ended while its log was still 'open' is PARTIAL -- that
    -- is derived at read time from the attempt state, never stored, so an agent
    -- that finalizes late still wins.
    state            TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'complete')),
    chunk_count      BIGINT NOT NULL DEFAULT 0,
    -- Highest line number indexed (lines are contiguous from 1).
    line_count       BIGINT NOT NULL DEFAULT 0,
    byte_count       BIGINT NOT NULL DEFAULT 0,
    -- Omitted by the per-attempt cap, and lost to shipping backpressure. Both
    -- are reported so the console can say what is missing rather than imply a
    -- log is whole.
    truncated_lines  BIGINT NOT NULL DEFAULT 0,
    truncated_bytes  BIGINT NOT NULL DEFAULT 0,
    dropped_lines    BIGINT NOT NULL DEFAULT 0,
    -- The single object the chunks were concatenated into at finalization, in
    -- seq order, so attempt_log_chunks.byte_offset addresses a range inside it.
    compacted_key    TEXT,
    compacted_sha256 TEXT,
    compacted_bytes  BIGINT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_chunk_at    TIMESTAMPTZ,
    finalized_at     TIMESTAMPTZ,
    CHECK (chunk_count >= 0 AND line_count >= 0 AND byte_count >= 0),
    CHECK (truncated_lines >= 0 AND truncated_bytes >= 0 AND dropped_lines >= 0),
    CHECK ((state = 'complete') = (finalized_at IS NOT NULL))
);

CREATE TABLE IF NOT EXISTS attempt_log_chunks (
    attempt_id       UUID NOT NULL REFERENCES attempt_logs(attempt_id) ON DELETE CASCADE,
    seq              BIGINT NOT NULL CHECK (seq >= 0),
    tenant_id        UUID REFERENCES tenants(id),
    object_key       TEXT NOT NULL,
    first_line       BIGINT NOT NULL CHECK (first_line >= 1),
    line_count       BIGINT NOT NULL CHECK (line_count >= 0),
    byte_offset      BIGINT NOT NULL CHECK (byte_offset >= 0),
    size_bytes       BIGINT NOT NULL CHECK (size_bytes >= 0),
    sha256           TEXT NOT NULL,
    first_ts         TIMESTAMPTZ,
    last_ts          TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- THE idempotency key: a redelivered chunk conflicts and is ignored.
    PRIMARY KEY (attempt_id, seq)
);

-- Every read locates chunks by line: "the chunks whose last line is at or after
-- K", in seq order. (first_line + line_count) is the exclusive end.
CREATE INDEX IF NOT EXISTS attempt_log_chunks_line_idx
    ON attempt_log_chunks (attempt_id, (first_line + line_count));
-- Retention scan in artifact-gc.
CREATE INDEX IF NOT EXISTS attempt_logs_finalized_idx ON attempt_logs (finalized_at);
CREATE INDEX IF NOT EXISTS attempt_logs_tenant_idx ON attempt_logs (tenant_id);
CREATE INDEX IF NOT EXISTS attempt_log_chunks_tenant_idx ON attempt_log_chunks (tenant_id);

DROP TRIGGER IF EXISTS tenant_assignment ON attempt_logs;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON attempt_logs
    FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('attempt_id', 'task_attempts');
DROP TRIGGER IF EXISTS tenant_assignment ON attempt_log_chunks;
CREATE TRIGGER tenant_assignment BEFORE INSERT OR UPDATE ON attempt_log_chunks
    FOR EACH ROW EXECUTE FUNCTION public.assign_parent_tenant('attempt_id', 'task_attempts');

-- ─── Grants (0005 style) ─────────────────────────────────────────────────────
-- Owned by the api-gateway (ADR-0020 amends ADR-0011's matrix): it terminates
-- the worker RPC that writes the index and serves the API that reads it, and the
-- rows share no transaction with any other service's table.
DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_workflow_manager', 'nexus_dag_engine',
        'nexus_scheduler', 'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT ON attempt_logs, attempt_log_chunks TO %I', r);
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_api_gateway') THEN
        GRANT INSERT, UPDATE, DELETE ON attempt_logs, attempt_log_chunks TO nexus_api_gateway;
    END IF;
END $$;
