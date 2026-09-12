-- Repair workflow_runs.event_seq where it drifted below the events that exist.
--
-- Two allocators wrote run_events concurrently: event-service allocated from the
-- event_seq counter (migration 0002, and the rule CONTRACT.md states), while the
-- shared appendEvent helper used `max(seq)+1` under an advisory lock. The lock
-- serialized only its own callers, so the two collided -- and because max(seq)+1
-- never advanced the counter, every run written by the helper left event_seq
-- stale. 31 runs had drifted on the development fleet, and on each of them the
-- NEXT event-service append was guaranteed to fail with
--
--   duplicate key value violates unique constraint "run_events_run_id_seq_key"
--
-- because it would hand out a seq that already existed.
--
-- appendEvent now allocates from the counter too, which stops new drift. This
-- repairs the rows already written, without which those runs stay permanently
-- unable to record another event.
--
-- ADDITIVE AND IDEMPOTENT (ADR-0011 rule 5): it writes no DDL and only ever moves
-- a counter FORWARD to match rows that already exist, so re-running it is a no-op
-- and a mixed-version deploy is unaffected -- both allocators tolerate a counter
-- that is already correct. It deliberately does not touch run_events itself:
-- that table is append-only (invariant 8) and its seq values are the history.
UPDATE workflow_runs r
   SET event_seq = sub.max_seq
  FROM (
        SELECT run_id, max(seq) AS max_seq
          FROM run_events
      GROUP BY run_id
       ) AS sub
 WHERE r.id = sub.run_id
   AND r.event_seq < sub.max_seq;
