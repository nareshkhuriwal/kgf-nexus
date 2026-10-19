# ADR-0017: Weighted fair share across namespaces, priority + aging within one

- **Status:** Proposed — pending owner sign-off (implemented on the proposing branch)
- **Date:** 2026-09-24
- **Deciders:** platform team
- **Closes:** ARCHITECTURE.md §12 question 5

## Context

§12 asked for "single integer plus aging, or weighted fair-share per namespace
from the start", noting that retrofitting fair share is unpleasant. The claim
(§5.3, `internal/store/scheduler_claim.go`) already had integer priority plus
aging (1 point/s, in the ORDER BY) and a head-of-line guard. What it did not
have was any notion of *who* the work belongs to: the push-path candidate scan
ordered every PENDING attempt on a queue by effective priority, globally.

What is actually shared between namespaces had to be established from the code,
because it decides what fair share can mean here:

- **Worker capacity is not shared.** Since migrations 0013–0015 every worker
  and attempt carries `tenant_id` (1:1 with namespace), `selectWorker` and
  `ClaimForWorker` only pair an attempt with a worker of the same tenant. A
  flood from namespace A cannot occupy B's workers.
- **The claim path is shared.** The push loop (`Claim`) serves every
  namespace on a queue from one ORDER BY with an 8-deep look-ahead. Two failure
  shapes follow, both reproduced before the change:
  1. A floods at *higher priority*: every claim goes to A until A's backlog is
     gone (test: 300 A attempts at priority 1000, 5 B attempts at 100 → after 12
     claims, B had 0 dispatched).
  2. A floods with work that *cannot be placed* (A's workers full or absent): A
     has nothing in flight, stays first, and its blocked attempts consume every
     look-ahead slot of every cycle -- B is never examined (test: first claim
     returns `blocked` for A; B never dispatched).
- **The pull path** (`ClaimForWorker`, the default when no notifier is
  configured, F81) is already tenant-scoped by the polling worker, so
  cross-namespace fairness there is moot today. It becomes load-bearing if a
  shared (multi-tenant) worker pool is ever introduced.
- Postgres dispatch throughput is shared by everything (ADR-0018).

## Options

1. **Keep global priority + aging.** Rejected: both failure shapes above.
2. **Per-namespace queues (separate queue names per tenant).** Rejected: pushes
   tenancy into routing, multiplies scheduler replicas (one drains one queue),
   and still offers no weights.
3. **Weighted fair share by dispatch count history (virtual time).** Rejected
   for now: needs a persisted per-namespace counter updated in the claim
   transaction -- a hot row every replica contends on, i.e. a serialization
   point inside the one transaction whose whole design avoids them.
4. **Weighted fair share by in-flight share, computed in the claim.**
   *Chosen.* `share = in-flight attempts of the namespace on this queue /
   weight`; lowest share first; priority + aging order work within the
   namespace. Needs no new write, only a read the claim can index.

## Decision

- The push-path candidate query orders by `share ASC`, then a per-cycle salted
  hash of the tenant (rotates namespaces tied on share -- typically several at
  zero -- so a tie is not broken the same way forever, without scrambling order
  *inside* a namespace), then `effective_priority DESC, scheduled_at ASC`.
- A namespace that uses up its look-ahead (8) with blocked candidates is set
  aside for the rest of that cycle; at most `FairShareMaxNamespaces` (4) are set
  aside before the cycle returns. Worst-case cycle cost is therefore bounded:
  8 × 4 candidate reads.
- The in-flight count is a CTE over `idx_attempts_inflight_by_tenant`
  (`(queue, tenant_id) WHERE state IN ('DISPATCHED','RUNNING')`, migration
  0022, built CONCURRENTLY, re-runnable). Weights are read from the view
  `scheduler_namespace_weights`, which exposes
  `platform_config.config -> scheduler -> fair_share_weight` clamped to 1–100
  and degrades a malformed value to 1 instead of raising (an error there would
  stop dispatch for every tenant).
- **Unchanged:** READ COMMITTED, `FOR UPDATE OF a SKIP LOCKED` on the attempt
  only, the `state = 'PENDING'` CAS, `UNIQUE(attempt_id)` on leases, and the
  single transaction. The share only chooses which unlocked PENDING row is tried
  first; it is at worst one concurrent dispatch stale, which affects fairness by
  one dispatch and correctness not at all.
- **Configuration:** `fair_share_weight` (1–100, default 1) in the namespace's
  platform config, via the existing API and the Infrastructure → Scheduler
  tab. Changing the weight requires a **platform operator** (403 for a namespace
  admin): raising your own weight takes share from every other namespace.
  A client that omits the field keeps the default. `NEXUS_FAIR_SHARE=false`
  (scheduler) restores pure priority order and is the rollback step before
  reverting 0022.

## Evidence

Integration tests (`services/scheduler/fairshare_integration_test.go`, real
Postgres 16):

| Test | Result |
|---|---|
| Higher-priority flood (A 300 @1000, B 5 @100), 12 claims | all 5 B dispatched |
| Unplaceable flood (A 200, no A worker), 3 claims | 3 B dispatched |
| Weights 3:1, both backlogs 250, B older and higher priority, 200 claims | A=150, B=50 (75.0 %, tolerance ±3 pts) |
| Aging within a namespace: p10 aged 300 s vs fresh p200 vs p10 aged 60 s | aged p10 dispatched first; 60 s one still PENDING |
| 8 replicas, own pools, 3 namespaces, capacity < demand | 0 attempts with >1 dispatch event, 0 with >1 lease, committed = Σ reservations and ≤ capacity on every worker, every namespace filled its 8 slots |
| Platform config API (`platform_weight_integration_test.go`) | omitted → 1; unchanged save 200; raise by namespace admin 403; 101 → 400; view reads 1 |

Mutation checks (F89): with `FairShare=false` all four fair-share tests fail
(including the concurrency test, where one namespace got 0 of its 8 slots);
with the per-namespace exclusion removed the unplaceable-flood test fails;
with the operator check removed the API test fails (200 instead of 403).

Cost (ADR-0018 benchmark and `EXPLAIN ANALYZE`): with the CTE `MATERIALIZED`
the in-flight count is one index-only walk (0.03 ms at 10k pending) and the
weight lookup is memoized per tenant; single-replica push claims at a 1k
backlog measured 5.6 ms with fair share on and 5.9 ms with it off. The first cut did NOT
materialize the CTE, and a generic plan re-ran it per pending row (1.14 s per
claim at 8k pending / 2k in flight); the benchmark caught it before merge.
The dominant claim cost is the O(backlog) candidate sort (ADR-0018).

## Consequences

**Positive** -- no namespace can starve another through the push path; weights
are enforceable; priority can no longer be used to jump other tenants.

**Negative** -- the push path does one extra index walk per claim. Within a
single cycle a namespace may be tried up to 8 times, so the look-ahead budget
spent on blocked work grows from 8 to at most 32 reads. Share is "in flight",
not "consumed over time": a namespace of very short tasks gets more dispatches
per second than its weight suggests, because its in-flight count stays low;
weights govern concurrency share, not throughput share. Weight changes need an
operator session; there is no operator UI beyond the per-namespace field yet.

**Neutral** -- the pull path is unchanged. If shared multi-tenant pools are
introduced, the same ORDER BY must be added to `selectCandidateForWorker`
(the index and view already exist).
