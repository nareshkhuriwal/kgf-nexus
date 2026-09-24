# ADR-0015: Bounded `uninterruptible` tasks

- **Status:** Proposed — pending owner sign-off (implemented on the proposing branch)
- **Date:** 2026-09-24
- **Deciders:** platform team
- **Closes:** ARCHITECTURE.md §12 question 3

## Context

Every external cancellation of a running attempt -- an operator or run cancel
(`CANCEL_ATTEMPT` on the heartbeat), a revoked lease, an agent drain -- is
implemented in the agent as SIGTERM, a grace period (control-plane
`CancelGrace` = 30 s, agent fallback 10 s), then SIGKILL (`Runner.Cancel`,
executor `Wait`). For most tasks that is right. For a task whose body is a
non-idempotent critical section -- capture a payment, apply one schema step,
publish to a system with no idempotency key (`idempotency: none`) -- a SIGTERM
part way through is the one outcome worse than either finishing or never
starting: the side effect may be half-applied, and because the attempt then
reports FAILED/LOST the platform cannot tell. §12 asked whether a task may opt
out, and what bounds it.

Two facts from the code shaped the answer:

- The control plane releases the capacity ledger the moment it marks an
  attempt CANCELLED (`cancelOneAttempt` → `releaseCapacity`), before the worker
  has stopped anything. The agent's own registry keeps the attempt's capacity
  until `Run` returns, and the pull path intersects the ledger with the worker's
  self-reported free capacity (`minVec` in `ClaimForWorker`), so a worker that is
  still finishing a cancelled task is not handed work into that capacity.
- `CANCEL_ATTEMPT` is re-sent on every heartbeat while the worker reports the
  attempt, and a revoked lease cancels again. Any deferral must be idempotent.

## Options

1. **No opt-out** (status quo). Rejected: leaves the half-applied-side-effect
   case with no tool but "make it idempotent", which is not always possible
   (third parties without idempotency keys).
2. **Unbounded opt-out.** Rejected: a task that cannot be stopped is a worker
   the platform cannot reclaim, a deploy that cannot finish, and a cancel button
   that lies.
3. **Opt-out bounded by the spec only.** Rejected: the spec is authored by the
   tenant; the bound has to be something the operator controls.
4. **Opt-out bounded by min(spec bound, platform ceiling), enforced on the
   agent.** *Chosen.*
5. **Extend the SIGTERM grace instead of skipping SIGTERM.** Rejected: the
   declaration exists precisely because the task cannot honour a TERM; sending
   one anyway just moves the half-applied window.

## Decision

A task may declare

```yaml
cancellation:
  uninterruptible: true
  max_uninterruptible_ms: 300000   # optional; absent/0 = the platform ceiling
```

(proto `TaskSpec.cancellation = 50`, `CancellationPolicy`; both JSON Schema
copies; SDK `Cancellation(max_wait="5m")` or `cancellation=True`.)

On any external cancellation the agent sends **no SIGTERM**. It waits for the
workload to finish on its own for `min(max_uninterruptible_ms, ceiling)`, then
**SIGKILLs** it. The first cancellation starts the clock; repeats (heartbeat
re-sends, lease revocation) do not restart or duplicate it. A cancel that
arrives before the workload has started simply abandons Prepare/Start.

**Bounds.** The schema rejects a declared bound above **1 hour** and a bound
without `uninterruptible: true`. The **platform ceiling** is agent
configuration, `--max-uninterruptible` / `NEXUS_MAX_UNINTERRUPTIBLE`, default
**10 minutes**; the clamp lives only on the agent (the enforcing side), so there
is no second copy to drift. The control plane's `grace_ms` is ignored for these
tasks: it is one number for every attempt on a heartbeat, and the task's own
clamped declaration is more specific.

**Lease and timeout interplay.**

| Situation | Behaviour |
|---|---|
| Operator / run cancel | Attempt is CANCELLED in the DB immediately (unchanged). Agent defers as above; lease renewals keep going until `Run` returns; the eventual report is fenced (attempt already terminal). |
| Lease revoked (renewal rejected, e.g. partition) | Same bounded deferral. The control plane may already have marked the attempt LOST; with `idempotency: none` LOST is **not** auto-retried (invariant 2), so no second copy starts. With `required`/`best_effort` a retry may start elsewhere while this one finishes -- the ordinary at-least-once overlap the idempotency key exists for. |
| Agent drain (deploy) | The drain waits `--drain-grace` (default 5 m). If it is shorter than the ceiling the agent logs a WARN at startup: a deploy can SIGKILL an uninterruptible task before its bound. Operators should set drain grace ≥ ceiling on pools that run such tasks. |
| `start_to_close` deadline | **Not extended.** It is the author's own maximum run time and still ends the attempt TIMED_OUT via SIGTERM → grace → SIGKILL. Uninterruptible concerns external cancellation only. |
| Capacity | Ledger released at cancel (unchanged); the worker keeps the capacity in its local registry until the process exits, and the pull path honours that via the free-capacity intersection. The **push** loop (`Claim`) does not see worker self-reports, so while a deferral is running the push path can over-place onto that worker for up to the bound. Push dispatch is off unless a notifier is configured (F81); recorded as a known gap. |
| UI | The workflow page's task detail states "uninterruptible: cancel waits up to X (capped by the worker ceiling), then SIGKILL". |

## Evidence

Tests (`agent/internal/worker/uninterruptible_test.go`, real subprocesses via
the subprocess executor):

- a cancelled uninterruptible task that traps TERM never sees it and reports
  SUCCEEDED after finishing on its own;
- a task ignoring TERM with a 1 s bound dies after ≥ 0.9 s and well under 8 s;
- a 60 s declaration on an agent with a 1 s ceiling is clamped to ~1 s;
- repeated cancels + lease revocations every 300 ms: killed on the first
  cancel's clock and exactly one deferral started;
- a bound table test for `EffectiveUninterruptibleBound`.

Mutation-checked (F89): routing uninterruptible tasks through the normal TERM
path fails the first test; removing the SIGKILL at the bound makes the second
and third take ~11 s (TERM ignored, then the executor's 10 s grace) and fail;
removing the `deferring` guard starts 13 deferrals in the repeat test and fails
it. Spec tests (`services/api-gateway/spec/cancellation_test.go`) cover
acceptance, conversion to the wire proto, and five rejection shapes; SDK tests
(`sdk/python/tests/test_cancellation.py`) cover compilation and the rule that an
interruptible task compiles to unchanged bytes (unchanged content address).

## Consequences

**Positive** -- a tenant can protect a genuinely non-idempotent critical section;
the operator keeps a hard, agent-enforced upper bound; old agents ignore the
additive field and keep the plain contract; old specs hash identically.

**Negative** -- a cancel no longer means "stops within 30 s" for these tasks;
the UI says so but CLI output does not yet. The push-path over-placement window
above. Docker and future k8s executors inherit the behaviour through the
executor `Signal` interface; only the subprocess executor was exercised in
tests. An agent older than this change silently ignores the declaration (it will
SIGTERM): pools running such tasks must run upgraded agents first.

**Neutral** -- the default ceiling (10 min) is a judgement, not a measurement;
operators should size it to their longest legitimate critical section.
