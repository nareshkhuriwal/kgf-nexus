-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Cron schedules and bounded backfill (ARCHITECTURE §3.2, §4; Phase 2).
--
-- The entity diagram in §4 names the fields (cron, tz, catchup, next_fire_at) but
-- gives no DDL, and §3.2's "Metadata Store" list claims a `schedules` table that
-- has never existed. This creates it.
--
-- WHY A TABLE AND NOT JUST TIMERS. The `timers` wheel already does durable,
-- multi-replica, at-least-once delivery with SKIP LOCKED, and a schedule fire is
-- one more timer kind. But a timer is a single-shot fact: it fires and is gone. A
-- schedule is a standing intention with state an operator edits and inspects --
-- paused or not, what it will do next, what it did last, why a tick was skipped.
-- Keeping that in the timer row would mean reconstructing a schedule's identity
-- from a fired-and-deleted row, so the schedule owns its state and the timer is
-- only its next wakeup.
--
-- OWNERSHIP: workflow-manager. It already owns workflow_specs, workflow_runs and
-- timers, and a schedule's whole job is creating runs from a spec on a timer --
-- putting it anywhere else would make run creation a cross-service call on the
-- one path that must stay simple (ADR-0011).
--
-- ADDITIVE (ADR-0011 rule 5): new tables only. Nothing existing is altered, so an
-- old replica that knows nothing about schedules keeps working -- it simply never
-- fires one. That is what makes this safe to deploy before the code that uses it.

CREATE TABLE IF NOT EXISTS schedules (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace       TEXT NOT NULL,
    -- The human-facing name, unique per namespace so an operator can refer to a
    -- schedule without knowing its UUID (and so a retried create is idempotent).
    name            TEXT NOT NULL,

    -- Schedules point at a spec NAME, not a spec id, deliberately.
    --
    -- A spec id is content-addressed and immutable, so binding to one would freeze
    -- a daily schedule to the definition that existed the day it was created and
    -- silently keep running last month's pipeline after every edit. Resolving the
    -- name at fire time is what makes "the daily ETL" mean the current daily ETL.
    -- The run still pins the exact version it used (workflow_runs.spec_id), so
    -- history stays reproducible.
    spec_name       TEXT NOT NULL,
    -- Pin a version to opt out of that: a schedule that must keep running one
    -- reviewed definition sets this and stops tracking edits.
    spec_version    INT,

    -- Standard five-field cron (minute hour dom month dow).
    cron_expr       TEXT NOT NULL,
    -- IANA timezone name. Present and UNUSED in this release: every expression is
    -- evaluated in UTC for now. The column exists so adding local-time semantics
    -- later is a code change rather than a migration of live schedules, and it
    -- defaults to UTC so the stored value never lies about what actually happened.
    timezone        TEXT NOT NULL DEFAULT 'UTC',

    -- Parameters handed to every run this schedule creates.
    params          JSONB NOT NULL DEFAULT '{}',
    priority        INT NOT NULL DEFAULT 100,

    -- What to do when the previous run of THIS schedule is still active:
    --   skip      -- do not fire; record why. The default, because an overlapping
    --               pipeline that double-writes is worse than a visible missed tick.
    --   allow     -- fire anyway; concurrency bounded only by max_active_runs.
    --   queue_one -- hold at most one pending fire and run it when the active run
    --               ends, so a slow run delays rather than drops a tick.
    overlap_policy  TEXT NOT NULL DEFAULT 'skip'
                    CHECK (overlap_policy IN ('skip', 'allow', 'queue_one')),

    -- Bounded catch-up for ticks missed while paused or while the control plane was
    -- down. 0 means fire only the current due tick and jump forward.
    --
    -- A limit is not optional: a schedule paused for a month with a five-minute
    -- cron has ~8,600 missed ticks, and firing them all would stampede the fleet
    -- with work nobody asked for at the moment an operator un-pauses.
    catchup_limit   INT NOT NULL DEFAULT 0 CHECK (catchup_limit >= 0),

    enabled         BOOLEAN NOT NULL DEFAULT TRUE,

    -- The next instant this schedule is due, in UTC. Also the schedule's claim on
    -- the timer wheel: a NULL means "not scheduled" (disabled, or being recomputed).
    next_fire_at    TIMESTAMPTZ,
    -- The logical time of the last tick actually fired. Distinct from a wall-clock
    -- "last run at": catch-up and backfill fire ticks whose logical time is in the
    -- past, and it is the LOGICAL time that identifies a tick.
    last_fire_at    TIMESTAMPTZ,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by      TEXT NOT NULL,

    UNIQUE (namespace, name)
);

-- The due-schedule query: enabled, scheduled, and due. Partial so the index holds
-- only rows that can actually fire, which on a fleet with many disabled or
-- far-future schedules is most of them.
CREATE INDEX IF NOT EXISTS idx_schedules_due
    ON schedules (next_fire_at)
    WHERE enabled AND next_fire_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_schedules_namespace
    ON schedules (namespace, name);

-- Backfill jobs: a bounded set of runs over a historical interval.
--
-- SEPARATE FROM THE SCHEDULE because §3.2 requires a backfill to have "its own
-- concurrency cap, so a backfill cannot starve live traffic". That cap has to
-- belong to the backfill rather than the schedule: the point is that catching up
-- three months of history runs slower than the live schedule, on purpose.
--
-- It is also a long-lived, resumable job. Generating every run up front would
-- create thousands of PENDING rows that an operator then cannot stop, so a backfill
-- advances a cursor and only ever has `max_active` runs outstanding.
CREATE TABLE IF NOT EXISTS schedule_backfills (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    schedule_id     UUID NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
    namespace       TEXT NOT NULL,

    -- Half-open [from_time, to_time): the interval of LOGICAL times to generate.
    from_time       TIMESTAMPTZ NOT NULL,
    to_time         TIMESTAMPTZ NOT NULL,
    -- The next logical tick to generate. NULL once the interval is exhausted.
    cursor_at       TIMESTAMPTZ,

    -- How many runs this backfill may have in flight at once, independent of the
    -- namespace's own limits. This is the starvation guard.
    max_active      INT NOT NULL DEFAULT 1 CHECK (max_active >= 1),
    -- A hard ceiling on total runs, so a wrong interval or a one-minute cron
    -- cannot generate an unbounded job.
    max_runs        INT NOT NULL DEFAULT 1000 CHECK (max_runs >= 1),
    runs_created    INT NOT NULL DEFAULT 0,

    state           TEXT NOT NULL DEFAULT 'RUNNING'
                    CHECK (state IN ('RUNNING', 'PAUSED', 'COMPLETED', 'CANCELLED', 'FAILED')),
    -- Why a backfill stopped, for the terminal states that need explaining.
    detail          JSONB NOT NULL DEFAULT '{}',

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at        TIMESTAMPTZ,
    created_by      TEXT NOT NULL,

    CHECK (to_time > from_time)
);

CREATE INDEX IF NOT EXISTS idx_backfills_active
    ON schedule_backfills (schedule_id)
    WHERE state = 'RUNNING';

-- Ties a run back to the schedule and the LOGICAL tick that produced it.
--
-- workflow_runs already carries logical_date, trigger_kind and trigger_detail, but
-- none of them can be constrained the way this needs: the guarantee that matters is
-- that one (schedule, logical tick) produces at most ONE run, however many times a
-- timer is delivered or a backfill is resumed. A UNIQUE index is the only honest
-- way to state that, and putting it here rather than on workflow_runs keeps a
-- schedule-shaped constraint out of the table every other trigger kind shares.
CREATE TABLE IF NOT EXISTS schedule_runs (
    schedule_id     UUID NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
    -- The tick's logical time, not when it ran.
    logical_at      TIMESTAMPTZ NOT NULL,
    run_id          UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    -- Which backfill produced it, if any. NULL for a live tick.
    backfill_id     UUID REFERENCES schedule_backfills(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (schedule_id, logical_at)
);

CREATE INDEX IF NOT EXISTS idx_schedule_runs_run ON schedule_runs (run_id);
CREATE INDEX IF NOT EXISTS idx_schedule_runs_backfill ON schedule_runs (backfill_id)
    WHERE backfill_id IS NOT NULL;

-- workflow-manager owns all three (ADR-0011 rule 1, migration 0005). The gateway
-- reads them to serve the REST API, like every other projection it serves.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_workflow_manager') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE
           ON schedules, schedule_backfills, schedule_runs
           TO nexus_workflow_manager;
    END IF;
END $$;

DO $$
DECLARE
    r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'nexus_api_gateway', 'nexus_dag_engine', 'nexus_scheduler',
        'nexus_resource_manager', 'nexus_event_service'
    ] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT SELECT ON schedules, schedule_backfills, schedule_runs TO %I', r);
        END IF;
    END LOOP;
END $$;
