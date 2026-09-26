# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""llm_batch_inference -- LLM calls under shared rate limits, resumed mid-shard, checked exactly.

    plan --> answer_shard (one child per shard, created at run time) --> verify

  * plan          cuts `records` arithmetic prompts into shards of `shard_size`
                  and emits shard DESCRIPTORS. Prompt i is "What is a + b?" with
                  a and b derived from i, so every task can regenerate its slice
                  and the verifier knows every right answer.
  * answer_shard  a MAP over the shards. Each child asks the model every prompt
                  of its slice through `nexus.llm.LLMClient` (a platform permit
                  per call on the keys `mock` and `mock/<model>`, in-task 429
                  retries, token usage reported as `tokens:mock/<model>:...`).
                  Every `checkpoint_every` answers it calls `ctx.checkpoint`.
                  Shard `crash_shard` DELIBERATELY FAILS once, right after its
                  first checkpoint at or past `crash_after` answers, on the
                  attempt that started from scratch; the platform retries it and
                  the retry calls `ctx.resume()` and asks only the prompts not
                  yet answered.
  * verify        FAILS THE RUN unless: every shard reported once; every record
                  was answered exactly once and correctly (the mock is
                  deterministic); the shards' token totals equal the per-answer
                  counts and the mock's documented token rule; the crashed shard
                  resumed (resumed_from > 0); and no shard asked any prompt twice.

It runs on a stack whose agent has Python 3 and the Nexus SDK, with the mock
model server (`python -m nexus.testing.mock_llm --host 0.0.0.0 --port 8089`)
reachable as http://mock-llm:8089, and this directory mounted at
/opt/nexus/examples/llm_batch_inference. Nothing calls a real provider.

    python3 examples/llm_batch_inference/check_local.py      # the same run, locally, against an in-process mock
"""

from __future__ import annotations

from typing import Any

from nexus import Resources, Retry, Runtime, TaskContext, Timeouts, task, workflow
from nexus.llm import LLMClient
from nexus.usage import usage_subject

MODULE = "/opt/nexus/examples/llm_batch_inference/dag.py"
MAX_SHARDS = 32
MAX_SHARD_SIZE = 100
# The mock accepts any non-empty key. This example never talks to anything else.
MOCK_API_KEY = "mock-key-not-a-secret"
_SMALL = Resources(cpu="100m", memory="128Mi")


def _runtime(ref: str) -> Runtime:
    return Runtime(executor="subprocess",
                   command=["python3", "-m", "nexus.entrypoint", "--module", MODULE], args=[ref])


def record(i: int) -> dict:
    a, b = (i * 7919 + 3) % 97, (i * 104729 + 11) % 89
    return {"id": f"q-{i:06d}", "prompt": f"What is {a} + {b}?", "expected": str(a + b)}


def _crash_point(size: int, checkpoint_every: int, crash_after: int) -> int:
    """The answer count at which the crash shard fails: its first checkpoint at
    or past crash_after (0 if it has none before its last record)."""
    point = -(-max(crash_after, 1) // checkpoint_every) * checkpoint_every
    return point if point < size else 0


class SimulatedCrash(ConnectionError):
    """The deliberate once-only failure. A ConnectionError, so it is classified
    `transient` and retried like a lost connection would be."""


@task(
    task_ref="llm_batch.plan",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="5m", heartbeat="5m"),
    retry=Retry(max_attempts=2, initial="1s", retry_on=["transient", "infrastructure"]),
    runtime=_runtime("llm_batch.plan"),
)
def plan(ctx: TaskContext, records: int, shard_size: int, checkpoint_every: int, crash_shard: int,
         crash_after: int) -> list:
    """Shard descriptors covering [0, records)."""
    if records < 1 or not 1 <= shard_size <= MAX_SHARD_SIZE or checkpoint_every < 1:
        raise ValueError(f"need records >= 1, 1 <= shard_size <= {MAX_SHARD_SIZE}, checkpoint_every >= 1")
    shards = [{"shard": s, "start": start, "stop": min(start + shard_size, records)}
              for s, start in enumerate(range(0, records, shard_size))]
    if len(shards) > MAX_SHARDS:
        raise ValueError(f"{len(shards)} shards is over MAX_SHARDS={MAX_SHARDS}; raise shard_size")
    if 0 <= crash_shard < len(shards):
        s = shards[crash_shard]
        if not _crash_point(s["stop"] - s["start"], checkpoint_every, crash_after):
            raise ValueError(f"shard {crash_shard} has no checkpoint at or past crash_after={crash_after} "
                             "before its last record, so it cannot crash and resume; lower crash_after")
    return shards


@task(
    task_ref="llm_batch.answer_shard",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="10m", heartbeat="2m"),
    # The deliberate crash is `transient`; a provider throttle past the in-task
    # retries is `rate_limited`, retried without charging the budget.
    retry=Retry(max_attempts=3, initial="1s", max="10s",
                retry_on=["transient", "rate_limited", "worker_lost", "infrastructure"]),
    outputs={"summary": {"type": "json", "required": True, "max_bytes": 16384}},
    runtime=_runtime("llm_batch.answer_shard"),
)
def answer_shard(ctx: TaskContext, shard: dict, model: str, base_url: str, checkpoint_every: int,
                 max_tokens: int, crash_shard: int, crash_after: int) -> dict:
    """Answer shard [start, stop), checkpointing; crash once if told to."""
    llm = LLMClient(ctx, provider="openai", model=model, base_url=base_url, api_key=MOCK_API_KEY,
                    usage_provider="mock", rate_limit_keys=[usage_subject("mock"), usage_subject("mock", model)],
                    max_retry_wait_s=10.0)
    size = shard["stop"] - shard["start"]
    state = ctx.resume()
    resumed = state is not None and state.get("shard") == shard["shard"] and state.get("model") == model
    if not resumed:
        state = {"shard": shard["shard"], "model": model, "next": 0, "answers": [], "asked": 0,
                 "input_tokens": 0, "output_tokens": 0}
    assert state is not None
    resumed_from = state["next"]
    crash_at = _crash_point(size, checkpoint_every, crash_after) if shard["shard"] == crash_shard else 0

    asked_now = 0
    for offset in range(state["next"], size):
        rec = record(shard["start"] + offset)
        c = llm.complete(rec["prompt"], max_tokens=max_tokens)
        asked_now += 1
        state["asked"] += 1
        state["answers"].append([rec["id"], c.text.strip(), c.input_tokens, c.output_tokens])
        state["input_tokens"] += c.input_tokens
        state["output_tokens"] += c.output_tokens
        state["next"] = offset + 1
        if state["next"] % checkpoint_every == 0 and state["next"] < size:
            ctx.checkpoint(state)
            if crash_at and not resumed and state["next"] == crash_at:
                ctx.log.warning("crashing deliberately after a checkpoint", shard=shard["shard"],
                                answered=state["next"])
                raise SimulatedCrash(f"shard {shard['shard']}: deliberate crash after {state['next']} answers "
                                     "(crash_shard/crash_after); the retry must resume from the checkpoint")
        ctx.progress(state["next"] / size)

    return {
        "shard": shard["shard"],
        "attempt": ctx.attempt_number,
        "resumed_from": resumed_from,
        "asked_this_attempt": asked_now,
        # Prompts asked by every attempt whose work reached a checkpoint, plus
        # this one: more than `size` would mean a prompt was asked twice.
        "asked_total": state["asked"],
        "input_tokens": state["input_tokens"],
        "output_tokens": state["output_tokens"],
        "answers": state["answers"],
    }


@task(
    task_ref="llm_batch.verify",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="5m", heartbeat="5m"),
    retry=Retry(max_attempts=1),  # a failed check is a verdict, not a glitch
    trigger_rule="all_done",  # run even when shards failed, and say which
    outputs={"verdict": {"type": "json", "required": True}},
    runtime=_runtime("llm_batch.verify"),
)
def verify(ctx: TaskContext, summaries: list, shards: list, records: int, crash_shard: int) -> dict:
    """The verdict: raise (failing the run) on any discrepancy."""
    from nexus.testing.mock_llm import count_tokens  # the mock's documented token rule

    problems: list[str] = []
    missing = [s["shard"] for s, r in zip(shards, summaries) if r is None]
    if missing or len(summaries) != len(shards):
        raise AssertionError(f"shards {missing} did not report ({len(summaries)} results for {len(shards)} shards)")
    if sorted(r["shard"] for r in summaries) != list(range(len(shards))):
        problems.append("shard results are not exactly one per shard")

    answers = [a for r in summaries for a in r["answers"]]
    want = [record(i) for i in range(records)]
    if [a[0] for a in answers] != [w["id"] for w in want]:
        problems.append(f"answered ids are not every record exactly once, in order ({len(answers)} vs {records})")
    wrong = [a[0] for a, w in zip(answers, want) if a[1] != w["expected"]]
    if wrong:
        problems.append(f"{len(wrong)} wrong answers, e.g. {wrong[:5]}")

    tin = sum(r["input_tokens"] for r in summaries)
    tout = sum(r["output_tokens"] for r in summaries)
    per_answer = (sum(a[2] for a in answers), sum(a[3] for a in answers))
    by_rule = (sum(count_tokens(" " + w["prompt"]) for w in want), sum(count_tokens(w["expected"]) for w in want))
    if (tin, tout) != per_answer or per_answer != by_rule:
        problems.append(f"token totals disagree: shards {(tin, tout)}, answers {per_answer}, mock rule {by_rule}")

    re_asked = {r["shard"]: r["asked_total"] - (s["stop"] - s["start"]) for s, r in zip(shards, summaries)
                if r["asked_total"] != s["stop"] - s["start"]}
    if re_asked:
        problems.append(f"prompts asked more than once, per shard: {re_asked}")
    crashed = next((r for r in summaries if r["shard"] == crash_shard), None)
    # resumed_from > 0 can only come from ctx.resume() returning an earlier
    # attempt's checkpoint.
    if crashed is not None and not crashed["resumed_from"] > 0:
        problems.append(f"shard {crash_shard} was told to crash once and resume, but it finished with "
                        f"resumed_from={crashed['resumed_from']} (attempt {crashed['attempt']})")

    verdict: dict[str, Any] = {
        "ok": not problems,
        "records": records,
        "shards": len(shards),
        "answered": len(answers),
        "correct": len(answers) - len(wrong),
        "input_tokens": tin,
        "output_tokens": tout,
        "crash_shard": crash_shard,
        "resumed_from": crashed["resumed_from"] if crashed else None,
        "crash_shard_attempt": crashed["attempt"] if crashed else None,
        "re_asked_prompts": sum(re_asked.values()),
        "problems": problems,
    }
    ctx.log.info("verdict", **{k: v for k, v in verdict.items() if k != "problems"})
    if problems:
        raise AssertionError("llm batch inference check failed: " + "; ".join(problems))
    return verdict


@workflow(
    name="llm-batch-inference",
    description="LLM calls over sharded prompts with shared rate limits; a shard crashes once and resumes "
                "from its checkpoint; the verdict checks every answer, every token and zero re-asked prompts.",
    params={
        "records": {"type": "int", "required": False, "default": 120},
        "shard_size": {"type": "int", "required": False, "default": 30},
        "model": {"type": "string", "required": False, "default": "mock-large"},
        "base_url": {"type": "string", "required": False, "default": "http://mock-llm:8089"},
        "checkpoint_every": {"type": "int", "required": False, "default": 5},
        "max_tokens": {"type": "int", "required": False, "default": 16},
        # -1 disables the deliberate crash.
        "crash_shard": {"type": "int", "required": False, "default": 1},
        "crash_after": {"type": "int", "required": False, "default": 10},
    },
    run_timeout="30m",
)
def llm_batch_inference(records: int, shard_size: int, model: str, base_url: str, checkpoint_every: int,
                        max_tokens: int, crash_shard: int, crash_after: int):
    shards = plan(records=records, shard_size=shard_size, checkpoint_every=checkpoint_every,
                  crash_shard=crash_shard, crash_after=crash_after)
    summaries = answer_shard.map(
        shards, item="shard", max_fanout=MAX_SHARDS, model=model, base_url=base_url,
        checkpoint_every=checkpoint_every, max_tokens=max_tokens, crash_shard=crash_shard, crash_after=crash_after,
    )
    return verify(summaries=summaries, shards=shards, records=records, crash_shard=crash_shard)


if __name__ == "__main__":
    print(llm_batch_inference.compile().to_yaml())
