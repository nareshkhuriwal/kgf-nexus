# AI workloads: usage, rate limits, checkpoints

Batch inference, evaluation and fine-tuning are ordinary DAGs. What they need
beyond that -- metering model calls, sharing a provider's rate limit across
the fleet, and resuming a long task mid-way after a retry -- is on the task
context ([ADR-0032](../design/adr/0032-ai-workloads-usage-rate-limits-checkpoints.md)).

```python
from nexus import Retry, TaskContext, task
from nexus.llm import LLMClient

@task(retry=Retry(max_attempts=3, retry_on=["transient", "rate_limited"]))
def label(ctx: TaskContext, prompts: list[str]) -> list[str]:
    llm = LLMClient(ctx, provider="openai", model="gpt-4o-mini",
                    base_url="https://api.openai.com", api_key_env="OPENAI_API_KEY")
    state = ctx.resume() or {"next": 0, "out": []}      # a retry picks up here
    for i in range(state["next"], len(prompts)):
        state["out"].append(llm.complete(prompts[i], max_tokens=64).text)
        state["next"] = i + 1
        if i % 100 == 99:
            ctx.checkpoint(state)
        ctx.progress(state["next"] / len(prompts))
    return state["out"]
```

Declare the provider key as a [secret](secrets.md), not a parameter.

## The workload API

Under a worker agent (subprocess and docker executors) each attempt gets a
private API on a Unix socket, `NEXUS_TASK_API`, with a per-attempt bearer
token in `NEXUS_TASK_API_TOKEN`. The agent adds the lease token and forwards
to the control plane, so the task never holds a platform credential, and a
task whose lease was taken away is refused. The context methods use it when it
is there; nothing in a handler needs to know. The kubernetes executor does not
offer it yet, and the SDK falls back to in-process behaviour there.

| Call | Effect |
| --- | --- |
| `ctx.report_usage({...})`, `ctx.report_tokens(...)`, `LLMClient` | cumulative usage units on the attempt, copied into the usage ledger when the attempt ends, including a failed one |
| `ctx.acquire_permits(keys, requests=, tokens=)`, `ctx.settle_permits(...)` | permits from the namespace's shared token buckets, so every worker respects one provider limit |
| `ctx.checkpoint(state)` / `ctx.resume()` | a durable, forward-only checkpoint of JSON state, readable by any later attempt of the same work |
| `ctx.heartbeat()` / `ctx.progress()` | liveness and progress that reach the control plane |
| raising `nexus.RateLimited` | a `rate_limited` failure with the provider's Retry-After |

## Rate limits and free retries

Shared limits are namespace policy, set in the quotas document next to the
[quotas](../concepts/quotas.md) (`rate_limits`, keyed by `<provider>` or
`<provider>/<model>`, requests and tokens per minute). A `rate_limited`
failure is retried after the provider's Retry-After **without charging the
task's retry budget**, up to a platform cap; past the cap it is charged like a
transient failure. The same node can also be retried after a spot eviction
against a separate preemption budget ([resources](../concepts/resources.md)),
and the three budgets never count against each other.

## Usage keys

Model calls are billed per token, not per CPU-second, so the units ride into
the usage ledger and its export, where KGF billing rates them
([KGF Cloud](../concepts/kgf-cloud.md)). The key convention is a contract:

```
tokens:<provider>/<model>:input      tokens:<provider>/<model>:output
tokens:<provider>/<model>:cached     requests:<provider>/<model>
<kind>:<subject>[:<qualifier>]
```

`nexus.testing.MockLLMServer` is an OpenAI- and Anthropic-compatible server
with deterministic answers and token counts and injectable 429s and 503s, so
the same handler runs end to end without a provider account. Examples: [llm_batch_inference](repo:examples/llm_batch_inference) and [llm_eval](repo:examples/llm_eval);
operations, limits and rollback:
[runbook §29](../operations/runbook.md#29-ai-workloads-usage-units-shared-rate-limits-and-checkpoints).
