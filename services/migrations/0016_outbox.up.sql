-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Transactional outbox for the NATS event bus (ADR-0003).
--
-- WHY AN OUTBOX. The bus carries hints and a projection of run history; Postgres
-- is the truth. Publishing from the writer AFTER commit loses the message
-- whenever the process dies between COMMIT and publish, and publishing BEFORE or
-- INSIDE the transaction announces facts that may still roll back. A row in this
-- table is written in the SAME transaction as the fact it announces, so it exists
-- if and only if the fact does; the relay (event-service) publishes it afterwards
-- and may retry as often as it likes, because JetStream drops re-publishes that
-- carry the same Nats-Msg-Id.
--
-- WHY A published_at FLAG rather than a high-water mark over id. BIGSERIAL ids are
-- handed out at INSERT but become visible at COMMIT, which is not the same order:
-- a relay that tracked "everything up to id N is published" would permanently
-- skip a row with id < N whose transaction committed late. Marking each row
-- individually has no such gap.
--
-- WHAT A ROW HOLDS. Only enough to build the message later. A run_event row
-- stores the run_events.id it announces (ref_id) instead of a copy of the event:
-- the dispatch claim writes one of these, and copying the event body would double
-- the bytes written on the hottest transaction in the system. The subject is not
-- stored either -- the relay derives it in one Go function, so the subject scheme
-- can change without a migration.
--
-- OWNERSHIP (ADR-0011). event-service owns this table: it runs the relay, so it
-- alone reads, marks and deletes rows. Every service that commits a fact worth
-- announcing needs INSERT, and ONLY insert -- the same reasoning as run_events in
-- the dispatch exception: the announcement must commit atomically with the fact,
-- so it cannot go through the owner's API in a second transaction.
--
-- ADDITIVE (ADR-0011 rule 5): one new table, its indexes and grants. No existing
-- table, column or row changes, so a mixed-version fleet is unaffected; an old
-- replica simply writes no outbox rows, which costs latency, never correctness.
CREATE TABLE IF NOT EXISTS outbox (
    id           BIGSERIAL PRIMARY KEY,
    -- run_event | run_state | task_ready. Plain text with no CHECK and no enum,
    -- so adding a topic is a code change rather than a constraint swap. During a
    -- rolling deploy an OLD relay that meets a topic it does not know leaves the
    -- row for a new replica instead of discarding it.
    topic        TEXT NOT NULL,
    -- run_events.id for topic = run_event; NULL otherwise. Deliberately NOT a
    -- foreign key: run_events rows are removed by ON DELETE CASCADE when a run is
    -- deleted, and an FK here would either block that delete or need its own
    -- cascade. A dangling ref is handled by the relay (nothing to publish).
    ref_id       BIGINT,
    -- Routing inputs for the subject, where the topic needs them (namespace and
    -- queue for task_ready, namespace and run id for run_state).
    namespace    TEXT,
    routing_key  TEXT,
    payload      JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Not before this instant. Lets a writer schedule a hint for when the fact
    -- becomes actionable, and lets the relay back a failing row off without
    -- holding it in memory.
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    attempts     INT NOT NULL DEFAULT 0,
    last_error   TEXT
);

-- The relay's work queue: unpublished rows in id order, which is (nearly) the
-- order the facts happened in, so one run's events tend to go out in sequence.
-- Partial, so it stays the size of the backlog rather than of the table, and a
-- steady state of zero backlog makes the relay's poll a probe of an empty index.
-- available_at is filtered while scanning; rows backed off into the future are
-- rare and few.
CREATE INDEX IF NOT EXISTS outbox_unpublished_idx
    ON outbox (id) WHERE published_at IS NULL;

-- Retention: the janitor deletes published rows older than a cutoff.
CREATE INDEX IF NOT EXISTS outbox_published_idx
    ON outbox (published_at) WHERE published_at IS NOT NULL;

-- ── grants, in the 0005 style ───────────────────────────────────────────────

-- Every service reads everything (ADR-0011's matrix), including this table.
GRANT SELECT ON outbox TO nexus_api_gateway, nexus_workflow_manager, nexus_dag_engine,
    nexus_scheduler, nexus_resource_manager, nexus_event_service;

-- The owner: reads the backlog, marks rows published, and prunes them.
GRANT INSERT, UPDATE, DELETE ON outbox TO nexus_event_service;

-- Writers of announced facts: INSERT only, so no writer can mark another's row
-- published (which would silently suppress it) or delete the backlog.
--   scheduler        -- AttemptDispatched and every other scheduler run_event,
--                       and retry attempts becoming ready
--   dag-engine       -- first attempts of newly READY nodes
--   workflow-manager -- run state transitions
GRANT INSERT ON outbox TO nexus_scheduler, nexus_dag_engine, nexus_workflow_manager;

-- 0005 granted USAGE on the sequences that existed then; this one is new, and a
-- role that may INSERT must be able to draw ids from it.
GRANT USAGE ON SEQUENCE outbox_id_seq TO nexus_event_service, nexus_scheduler,
    nexus_dag_engine, nexus_workflow_manager;
