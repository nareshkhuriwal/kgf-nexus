-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- Enforce that a schedule's spec_name names a workflow that exists.
--
-- THE GAP. schedules.spec_name is plain TEXT with zero foreign keys (0006). The
-- consequence is not theoretical: POST /schedules accepts any string, returns 201
-- with next_fire_at armed, and produces a row that lists as enabled and healthy.
-- The fire loop then wakes on time, fails to resolve the name, and creates no
-- runs -- with nothing in the schedule row to say why. Of every failure this
-- schema can produce, that is the worst kind: it looks like success.
--
-- WHY NOT A PLAIN FOREIGN KEY, which is what anyone reads this file expecting.
-- workflow_specs is VERSIONED. Its primary key is id, and it is UNIQUE on
-- (namespace, name, version) and (namespace, name, spec_hash) -- but NOT on
-- (namespace, name). `ten-node-mixed` has three rows today. A foreign key needs a
-- unique target, so
--     FOREIGN KEY (namespace, spec_name) REFERENCES workflow_specs(namespace, name)
-- is not merely undesirable, it is rejected by Postgres outright. And pinning the
-- schedule to a spec ID instead -- the other obvious move -- would destroy the
-- design: a schedule deliberately tracks the LATEST version of a NAME (see 0006's
-- spec_name comment), which is what makes "the daily ETL" mean the current daily
-- ETL rather than whatever was current the day someone created the schedule.
--
-- WHY NOT A workflow_names PARENT TABLE, which is the structurally honest answer
-- and was seriously considered. (namespace, name) as its own table, workflow_specs
-- referencing it, schedules referencing it -- and the invariant becomes an ordinary
-- foreign key with no machinery at all. It was NOT taken, for reasons of blast
-- radius rather than taste:
--
--   * It is not additive. ADR-0011 rule 5 requires a migration to leave an old
--     replica working, and this one would add a NOT NULL reference to a table that
--     did not exist a moment ago. Every writer of workflow_specs -- the
--     workflow-manager's WFMPutSpec, and anything that inserts a spec during a
--     rolling upgrade -- must create the parent row first or be rejected. A replica
--     that predates the change inserts a spec and fails. That is a coordinated
--     deploy, not a migration.
--   * It requires a backfill of every distinct (namespace, name) in workflow_specs,
--     and a lifecycle nobody has specified: when the last version of a name is
--     deleted, does the name row remain? If it does, deleteSpec stops making the
--     name available for reuse in the way it does today; if it does not, deleting
--     it is another cascade to get right. WFMDeleteSpec's semantics would have to
--     be reopened.
--   * The thing it buys over the trigger below is real but narrow: a declarative
--     constraint instead of procedural code, and cheap referential integrity for
--     future tables that want to name a workflow. Today there is exactly one such
--     table.
--
-- It is the right model and should be revisited if a second table ever references a
-- workflow by name. It is too large a change to smuggle in behind a bug fix.
--
-- WHAT THIS DOES INSTEAD: a constraint trigger. It asserts, on INSERT and on any
-- UPDATE that changes the pair, that at least one workflow_specs row exists for
-- (namespace, spec_name). That is the invariant a foreign key would express, stated
-- the only way a versioned parent permits.
--
-- WHAT IT COSTS, said plainly, because a trigger is invisible machinery and the
-- worst thing about it is that nobody reading `\d schedules` learns it is there:
--
--   * It is NOT a foreign key and does not pretend to be. It fires on writes to
--     schedules only. Nothing here stops someone deleting the last version of a
--     workflow out from under a live schedule -- there is no trigger on
--     workflow_specs. That direction is guarded in application code
--     (WFMCountSpecReferences, called by WFMDeleteSpec, which refuses and names the
--     schedule), and this migration deliberately does not duplicate it: a second
--     enforcement point that could disagree with the first is worse than one that
--     is merely incomplete. The pair is "creation refuses an unknown name, deletion
--     refuses a name in use", and each half lives in exactly one place.
--   * It costs one indexed lookup per schedule write. Schedules are written by
--     hand, a few times a day at most, so this is free in practice -- but it is a
--     real cost and it is on the write path.
--   * A future bulk import of schedules that legitimately precedes its specs will
--     be rejected. That is the intended behaviour, but it is the kind of thing
--     discovered at 2am, so: the trigger can be suspended for a session with
--     ALTER TABLE schedules DISABLE TRIGGER trg_schedules_spec_exists.
--
-- The guard is a BELT to the gateway's BRACES, not a replacement for it. The
-- gateway rejects an unknown spec_name at submit time with a 400 naming the field
-- and the value (createSchedule, alongside the cron check it is modelled on),
-- because a database error surfacing as a 500 is a worse answer to a typo than a
-- 400 that says what to fix. This exists for the writer that is not the gateway:
-- a direct psql INSERT, a future service, a migration script.

-- EXISTING ROWS. Checked before anything is created rather than discovered when
-- the trigger is validated: a migration that fails on live data is worse than no
-- migration, because it leaves the deployment stuck rather than merely unguarded.
--
-- The behaviour is TOLERANT BY CONSTRUCTION, and that is a deliberate choice over
-- the two alternatives:
--
--   * CLEAN UP (delete or disable the offending schedules) was rejected outright.
--     A migration must not destroy or silently disarm an operator's schedule. A
--     schedule pointing at a missing workflow is broken, but it is also the only
--     record of an intention -- the workflow may have been deleted by mistake and
--     be about to be restored -- and the row's own spec_name is the evidence
--     needed to fix it. Deleting it discards that evidence.
--   * REFUSE (abort the migration) was rejected because the trigger is validated
--     only on write. An existing bad row is not touched by this migration and does
--     not have to be, so refusing would block a deployment over data the change
--     does not disturb.
--
-- So: any pre-existing violation is REPORTED as a loud WARNING and left exactly
-- where it is. It stops new ones being created, which is the whole point; the old
-- ones stay visible and stay the operator's to fix. Note the asymmetry this
-- creates, since it is the kind of thing that surprises people later: such a row
-- survives, but any UPDATE that touches its namespace or spec_name will now be
-- rejected until the workflow exists or the name is corrected to one that does.
DO $$
DECLARE
    bad_count INT;
    bad_list  TEXT;
BEGIN
    SELECT count(*), string_agg(format('%s/%s -> %s', s.namespace, s.name, s.spec_name), ', ')
      INTO bad_count, bad_list
      FROM schedules s
     WHERE NOT EXISTS (
           SELECT 1 FROM workflow_specs w
            WHERE w.namespace = s.namespace AND w.name = s.spec_name);

    IF bad_count > 0 THEN
        RAISE WARNING 'schedules referencing a workflow that does not exist: % (%). These rows are LEFT AS THEY ARE -- the guard applies to new writes only. Each one will arm, fire on time and create no runs until its workflow is restored or its spec_name corrected.',
            bad_count, bad_list;
    END IF;
END $$;

-- The check itself.
--
-- SECURITY INVOKER (the default, stated by omission and noted here so the absence
-- is not read as an oversight): every role that writes schedules already has SELECT
-- on workflow_specs, so there is nothing to elevate, and a SECURITY DEFINER
-- function is a privilege escalation surface to be justified rather than assumed.
--
-- The message NAMES the namespace and the name, for the same reason the gateway's
-- 400 does: whoever hits this is looking at a string they believed was right, and
-- an error that does not quote it back sends them to diff it by eye. The HINT says
-- the one thing that is genuinely non-obvious -- that the name needs only to
-- EXIST, not to be a particular version -- because someone reading "does not
-- exist" about a workflow they can see in the catalog will otherwise suspect the
-- version pin.
CREATE OR REPLACE FUNCTION schedules_require_existing_spec() RETURNS TRIGGER AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM workflow_specs
         WHERE namespace = NEW.namespace AND name = NEW.spec_name
    ) THEN
        RAISE EXCEPTION 'spec_name % does not name a workflow in namespace %',
                        quote_literal(NEW.spec_name), quote_literal(NEW.namespace)
              USING ERRCODE = 'foreign_key_violation',
                    HINT = 'A schedule resolves this name every time it fires, so one that matches nothing would arm, fire on time and create no runs. Any version of the workflow satisfies this; the schedule is not pinned to one.';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ERRCODE 23503 (foreign_key_violation) is chosen ON PURPOSE rather than left as
-- the default P0001. This IS a foreign key violation in everything but the
-- mechanism, callers already classify 23503 as a referential refusal
-- (WFMDeleteSpec maps it to ErrSpecReferenced rather than to an internal error),
-- and a client that cannot tell this from a generic exception would report a
-- correct refusal as a 500.

-- DROP-then-CREATE rather than CREATE OR REPLACE, because Postgres before 14 has
-- no CREATE OR REPLACE TRIGGER and this must re-run cleanly regardless. Every
-- other object here is idempotent (IF NOT EXISTS / OR REPLACE), matching the
-- established pattern -- 0008's header notes that re-application has to be
-- harmless because a renumbered migration can genuinely be applied twice.
DROP TRIGGER IF EXISTS trg_schedules_spec_exists ON schedules;

-- BEFORE, not AFTER: the row should never reach the table. A CONSTRAINT TRIGGER
-- (deferrable, AFTER) was the alternative and is wrong here -- deferral exists so
-- a transaction can pass through an inconsistent intermediate state, and there is
-- no ordering in which a schedule legitimately precedes its workflow within one
-- transaction. Rejecting at the statement puts the error where the mistake is.
--
-- The UPDATE arm is narrowed with WHEN so an ordinary write -- the fire loop
-- stamping next_fire_at and last_fire_at, which is by far the most frequent UPDATE
-- this table sees -- does not pay for a lookup of a pair it did not touch. Without
-- it the guard would run on every tick of every schedule forever, to re-verify
-- something that cannot have changed.
CREATE TRIGGER trg_schedules_spec_exists
    BEFORE INSERT OR UPDATE OF namespace, spec_name ON schedules
    FOR EACH ROW
    WHEN (pg_trigger_depth() = 0)
    EXECUTE FUNCTION schedules_require_existing_spec();

-- Supports the lookup the trigger makes, and the identical one the gateway makes
-- on every schedule creation and the delete path makes on every spec deletion.
-- workflow_specs is UNIQUE on (namespace, name, version), whose leading columns
-- already serve this -- so this index is NOT created. Noted rather than omitted
-- silently, so the next reader does not add a redundant one.
