# ADR-0018: The Postgres dispatch ceiling -- measured, with a trigger and a migration path

- **Status:** Proposed — pending owner sign-off
- **Date:** 2026-09-24
- **Deciders:** platform team
- **Closes:** ARCHITECTURE.md §12 question 6
- **Refines:** ADR-0002

## Context

ADR-0002 made Postgres the dispatch queue and accepted "a throughput ceiling"
for atomic dispatch without distributed transactions. §12 asserted that
`SKIP LOCKED` "comfortably handles thousands of dispatches/second" and asked at
what *measured* throughput a dedicated path is needed. Nothing had measured it.

## Method

`make bench-pg && make bench-dispatch` (`tests/bench/dispatch_test.go`). It
drives the real claim code -- `ClaimForWorker` (pull: every worker poll, the
default deployment) and `Claim` (push: the notifier-driven loop) -- with R
"replicas", each a goroutine with its **own** connection pool (so they contend
on the database as separate processes do), against a backlog of B PENDING
attempts spread over B/100 runs (so `event_seq` bumps do not all hit one run
row) and 64 workers of effectively unbounded capacity (so the queue, not bin
packing, is measured). Each cell measures 2,000 dispatches or B/2, whichever is
smaller, capped at 90 s. Latency is one claim transaction, successful claims
only.

**Hardware -- read every number below as a laptop number.** Apple M4 Pro
(14 cores, 24 GB); Postgres 16.15 (`postgres:16-alpine`, `shared_buffers=512MB`,
`max_connections=300`, `fsync=on`, default `work_mem` 4 MB) in Docker Desktop
limited to 10 vCPU / 16 GB, on the same machine as the Go client
(GOMAXPROCS 14). No network hop, a VM disk, a shared CPU budget between client
and server. These locate the shape and the knee of the curve; they are **not**
production capacity figures.

## Results

Pull path (`ClaimForWorker`, unchanged query):

| backlog | replicas | claims/s | p50 | p99 |
|---:|---:|---:|---:|---:|
| 1,000 | 1 | 184 | 5.2 ms | 8.4 ms |
| 1,000 | 2 | 328 | 5.7 ms | 12.3 ms |
| 1,000 | 4 | 558 | 6.9 ms | 9.5 ms |
| 1,000 | 8 | 913 | 8.2 ms | 25.7 ms |
| 1,000 | 16 | 1,107 | 13.5 ms | 40.1 ms |
| 10,000 | 1 | 37 | 27.0 ms | 35.7 ms |
| 10,000 | 2 | 61 | 30.9 ms | 77.5 ms |
| 10,000 | 4 | 117 | 33.8 ms | 43.4 ms |
| 10,000 | 8 | 95 | 81.7 ms | 163.6 ms |
| 10,000 | 16 | 195 | 75.6 ms | 301.5 ms |
| 100,000 | 1 | 4 | 262 ms | 357 ms |
| 100,000 | 2 | 7 | 294 ms | 466 ms |
| 100,000 | 4 | 10 | 351 ms | 758 ms |
| 100,000 | 8 | 2 | 4.6 s | 6.3 s |
| 100,000 | 16 | 19 | 836 ms | 1.39 s |

Push path (`Claim`, after the plan fixes in commit "Keep the push-path claim on
a custom plan…"; fair share on):

| backlog | replicas | claims/s | p50 | p99 |
|---:|---:|---:|---:|---:|
| 1,000 | 1 | 174 | 5.6 ms | 8.9 ms |
| 1,000 | 4 | 226 | 16.3 ms | 36.8 ms |
| 1,000 | 16 | 230 | 28.9 ms | 75.2 ms |
| 10,000 | 1 | 36 | 27.7 ms | 35.0 ms |
| 10,000 | 4 | 74 | 36.5 ms | 165.8 ms |
| 10,000 | 16 | 95 | 116 ms | 392 ms |
| 100,000 | 1 | 4 | 271 ms | 352 ms |
| 100,000 | 4 | 11 | 284 ms | 1.17 s |
| 100,000 | 16 | 18 | 675 ms | 2.87 s |

Before those fixes the push path measured 22.7 ms / 243 ms / 3.62 s (p50,
1 replica) at 1k / 10k / 100k. With `NEXUS_FAIR_SHARE=false` the push path
measured 5.9 ms / 79.8 ms / 3.25 s -- i.e. the 100k collapse was pre-existing,
and fair share's own cost is noise once its CTE is materialized.

## Findings

1. **Claim cost is linear in the PENDING backlog of the queue, not in replica
   count.** ~2.6 µs per pending row per claim: 5 ms at 1k, 27 ms at 10k,
   260 ms at 100k for a single replica. `EXPLAIN ANALYZE` shows why: the
   candidate ORDER BY is an expression (`priority + age × aging_rate`), so every
   claim seq-scans the queue's pending rows, joins them to `task_nodes` and
   `workflow_runs`, and sorts them -- at 100k the sort spills to disk
   (external merge, 7–20 MB). `LIMIT 1` does not help; the sort must finish.
   `explainUnschedulable` adds another full scan (`ORDER BY scheduled_at`).
2. **Replicas scale throughput only while the backlog is small.** At 1k, 16
   replicas give 6× one replica (184 → 1,107/s). At 10k the gain is 5× (37 →
   195/s) with p99 at 300 ms; at 100k the database is CPU-bound on sorts and
   more replicas mostly add latency.
3. **The plan is unstable at large backlogs.** Two instances measured:
   (a) a cached *generic* plan for the push query nested-looped `workflow_runs`
   per pending row (3.3 s/claim; fixed by re-planning per call), and (b) the
   pull query's *custom* plan, reproducibly at 100k/8 replicas, chose a nested
   loop with a seq scan of `workflow_runs` per row after the in-flight set
   changed the statistics (4.6 s p50 -- 16 replicas was faster than 8). (b) is
   **not** fixed on this branch; wait-event sampling showed all 8 backends on
   CPU in the candidate query, not waiting on locks.
4. **§12's "thousands of dispatches/second" does not hold for this claim** on
   this hardware except with a sub-1k backlog and ≥ 16 replicas (~1.1k/s). The
   bottleneck is the candidate scan, not `SKIP LOCKED` contention: no cell
   showed lock waits, and the correctness properties (no double dispatch, no
   over-commit) held throughout (ADR-0017's concurrency test, F82).

## Options

1. **Keep Postgres as the queue; fix the scan first; define the trigger for a
   dedicated path.** *Chosen.*
2. **Move dispatch to a dedicated queue now (NATS JetStream work queue, Redis
   streams, an in-memory dispatcher).** Rejected for now: every such design
   still has to commit reservation + lease + attempt CAS + event + timers
   atomically in Postgres (invariant 4), so it removes the candidate scan but
   not the transaction; and the scan is fixable in SQL for a fraction of the
   cost (Stage 1 below), which should be measured before anything bigger.
3. **Accept the curve as is.** Rejected: a 10k backlog is an ordinary burst
   (one fan-out map of 10k items), and at 100k a claim takes ~0.3–4 s.

## Decision

Postgres remains the dispatch queue and the source of truth. The **trigger**
for building a dedicated dispatch path is any one of, sustained for 15 minutes
on production hardware, measured *after* Stage 1:

- claim p99 > 250 ms, or
- a queue's PENDING backlog routinely > 50,000, or
- required dispatch rate > 70 % of the measured claims/s at the observed
  backlog.

Neither metric exists yet: the claim path emits no latency histogram and no
PENDING-backlog gauge. Adding `nexus_claim_seconds` (by path, outcome) and
`nexus_pending_attempts` (by queue) is part of this decision and a
precondition for evaluating the trigger.

**Migration story, in order -- each stage keeps the §5.3 transaction exactly
as it is and is independently shippable:**

- **Stage 1 -- make the claim sub-linear in SQL.** Replace the expression sort
  with an index-servable order: for a fixed aging rate,
  `priority + (now − scheduled_at)·rate` orders identically to
  `priority − epoch(scheduled_at)·rate`, which an expression index on
  `(queue, tenant_id, …) WHERE state='PENDING'` can serve with `LIMIT`; select a
  bounded window (e.g. 64) by that index *before* joining `task_nodes` and
  `workflow_runs`; give `explainUnschedulable` an index on
  `(queue, scheduled_at) WHERE state='PENDING'` or rate-limit it; pin the pull
  query's plan the way the push query's was (re-plan per call plus a join
  shape that cannot degenerate into a per-row seq scan). Expected result: claim
  cost bounded by the window, not the backlog. Re-run `make bench-dispatch` as
  the acceptance test.
- **Stage 2 -- batch.** Claim up to `MaxTasks` for one polling worker in one
  scan (still one transaction per dispatch for the writes), and cache the
  bounded candidate window per replica for a few hundred ms.
- **Stage 3 -- dedicated dispatch path, only if the trigger fires after
  Stage 1.** Per-queue dispatcher processes (partitioned by queue hash, no
  leader election) hold the ready set in memory, fed from the outbox/NATS
  stream (ADR-0003) and reconciled from Postgres on start; they choose the
  candidate and then run the *unchanged* claim transaction keyed by attempt id
  (`WHERE id = $1 AND state = 'PENDING'`), so Postgres keeps deciding
  correctness. Roll out per queue behind a flag, run in shadow mode comparing
  choices with the SQL claim, and fall back to the SQL claim on any dispatcher
  failure. Nothing about workers, leases or fencing changes.

## Consequences

**Positive** -- the ceiling is now a measured curve with a reproducible
harness; two plan pathologies were found and one fixed (push path up to 12×
faster at 100k); the trigger is concrete and cheap to evaluate once the two
metrics exist.

**Negative** -- today, on this hardware, a queue with a 10k backlog dispatches
at roughly 40–200/s and a 100k backlog at single-digit to ~20/s; that is the
real current ceiling and it is far below §12's assumption. Finding 3(b) is an
open defect on the pull path. All numbers are laptop numbers (client and
database share one machine and a Docker VM); production capacity must be
re-measured on production-class hardware with the same harness.

**Neutral** -- no schema change is required by this ADR; Stage 1 will need an
additive, concurrently built index.
