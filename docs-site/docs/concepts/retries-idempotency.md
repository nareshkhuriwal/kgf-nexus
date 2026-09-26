# Retries and idempotency

## The guarantee

Nexus executes every dispatched task **at least once**. It does not promise
exactly once, because over a network nothing can. Suppose a worker commits a
side effect and dies before its report arrives. The control plane cannot tell
that apart from a worker that died before the effect. What Nexus does
guarantee ([architecture §6.1](../design/architecture.md#61-the-guarantee-stated-honestly)):

1. At most one attempt is **authorized** at any instant, through
   [lease fencing](leases.md).
2. A task that uses its idempotency key correctly produces **one logical
   effect**, however many times it physically runs.
3. Every state transition is durable before anyone can observe it.

## Retries

```yaml
retry:
  max_attempts: 4            # total, including the first; 1 = no retry
  strategy: exponential      # fixed | linear | exponential
  initial_ms: 10000
  max_ms: 600000
  multiplier: 2
  jitter: full               # none | full | equal (full is the default)
  retry_on: []               # empty = the platform default set
  no_retry_on: [application]
  retry_on_different_worker: true
```

The error class is decided by the worker, which sees the real failure. The
retry policy acts on that class:

| Class | Retried by default | Notes |
| --- | --- | --- |
| `transient`, `timeout`, `infrastructure`, `oom` | yes | |
| `worker_lost` | yes, subject to `idempotency` (below) | The outcome is unknown. |
| `application` | **no**, unless listed in `retry_on` | Your code raised. Retrying the same input usually fails the same way. |
| `validation`, `permission_denied`, `not_found` | **never**, even if listed | Running again cannot fix these. |

`no_retry_on` overrides everything else. A blown `schedule_to_close` budget
stops retries whatever attempts remain. `retry_on_different_worker` keeps a
poisoned host from using up the whole retry budget.

## Timeouts

Nexus uses Temporal's four timeouts. With four, "it timed out" says whether
to fix the cluster, the code or the network:

| Timeout | Measures | Usually means |
| --- | --- | --- |
| `schedule_to_start` | queued → started | not enough capacity |
| `start_to_close` | started → finished, one attempt | the task ran too long |
| `schedule_to_close` | first queued → finished, across **all** retries | the end-to-end budget was missed |
| `heartbeat` | silence between `ctx.heartbeat()` calls | the task is wedged but alive |

## Idempotency

The platform derives the key. You never invent one:

```text
idempotency_key = sha256(run_id ‖ node_id ‖ map_index ‖ task_version)
```

The attempt number is **deliberately excluded**, so every retry of the same
logical work gets the same key. Bumping `version` declares a new logical
effect. The key reaches a task as `ctx.idempotency_key` (SDK) or
`$NEXUS_IDEMPOTENCY_KEY` (any executor).

Use it through one of three mechanisms, listed from strongest to weakest:

1. **Downstream-native deduplication.** Pass the key to a system that stores
   it atomically with the effect, such as a payments API. There is no window.
2. **Conditional write.** Use `INSERT … ON CONFLICT (idem_key) DO NOTHING` in
   one transaction with the effect. There is no window. A `SELECT` followed by
   an `INSERT` is **not** equivalent. The `nexusctl init etl` template's
   `load` task shows the correct pattern.
3. **Effect journal.** `ctx.effect_recorded` / `ctx.record_effect`. The
   write-then-crash window still exists, but it shrinks from the whole task to
   one write.

`examples/idempotent_effect` demonstrates all three, including the window
that mechanism 3 leaves open.

### Lost attempts

`LOST` means the lease expired and nobody knows whether the task ran. The
task's `idempotency` declaration decides what happens next
([architecture §6.5](../design/architecture.md#65-handling-indeterminate-outcomes)):

| `idempotency` | On `LOST` |
| --- | --- |
| `required` (default) | retried automatically |
| `best_effort` | retried, with a `PossibleDuplicateExecution` warning event |
| `none` | **not** retried. The node fails with `indeterminate_outcome`, and an operator decides, for example with `nexusctl run retry --node`. |

A task declaring `none` cannot enable caching either.
