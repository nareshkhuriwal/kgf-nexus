"""__WORKFLOW_NAME__ -- batch LLM inference that resumes mid-shard.

    plan_shards --> infer_shard (one child per shard, created at run time) --> aggregate

  * plan_shards  counts the prompts and cuts them into shards of `shard_size`.
                 Only shard DESCRIPTORS travel through the platform, never the
                 prompts.
  * infer_shard  a MAP over the shards. Each child sends its prompts to the
                 model through `nexus.llm.LLMClient`, which takes a permit from
                 the platform's SHARED rate limiter before every call (so the
                 whole fleet stays under the provider's limit), retries 429/503
                 in-task honouring Retry-After, and reports token usage under
                 the platform's usage keys. Every `checkpoint_every` answers the
                 child CHECKPOINTS its progress with `ctx.checkpoint`; a retry
                 (lost worker, provider outage, deadline) calls `ctx.resume()`
                 and carries on from the last checkpoint instead of paying for
                 every prompt again. The answers are written atomically to
                 `output_prefix/part-NNNNN.jsonl`.
  * aggregate    checks that every shard reported and every record was answered
                 exactly once, and totals tokens and an ESTIMATED cost.

The cost here uses `example_prices`, a small illustrative price table so the
summary has a number in it. It is NOT the platform's price book: the platform
records the usage units (tokens:<provider>/<model>:input, ...) in its usage
ledger, and prices them separately.

Input: JSON Lines, one record per line, `{"id": ..., "prompt": ...}` (optional
`"system"`, and `"expected"` to have the aggregate report accuracy), or
`synthetic://N` for N generated arithmetic prompts.

The defaults point at the mock model server (`python -m nexus.testing.mock_llm`)
at http://mock-llm:8089, so nothing here calls a paid API until you change
`base_url` and `api_key_env` (see the README).

    pytest
    nexusctl workflow compile dag.py --show
    nexusctl workflow submit dag.py
    nexusctl run create __WORKFLOW_NAME__ --param source=synthetic://500 --wait
"""

from __future__ import annotations

import hashlib
import json
import os
from typing import Any, Iterator

from nexus import Cache, Resources, Retry, Runtime, SecretRef, TaskContext, Timeouts, task, workflow
from nexus.llm import LLMClient

IMAGE = "__WORKFLOW_NAME__:0.1.0"

# The ceiling on shards. max_fanout is mandatory: an input far larger than
# expected fails the plan loudly instead of creating a million tasks.
MAX_SHARDS = 256

# Sent when `api_key_env` is empty: the mock accepts any non-empty key, and a
# real provider rejects this one (401 -> a terminal LLMAuthError), so an
# unconfigured key can never silently reach a paid API.
MOCK_API_KEY = "mock-key-not-a-secret"

# ILLUSTRATIVE prices in USD per million tokens, used only for the estimate in
# the summary. Not the platform's price book, not any provider's current list
# price: pass your own with --param 'example_prices={...}'.
EXAMPLE_PRICES = {"input_per_1m_tokens_usd": 0.15, "output_per_1m_tokens_usd": 0.60}

# The provider key, delivered as a SECRET REFERENCE the worker resolves at
# execution time into the environment variable `name` (pass that name as the
# api_key_env param). Empty for the mock. For example:
#   API_KEY_SECRETS = [SecretRef(name="OPENAI_API_KEY", ref="vault://kv/llm/openai")]
API_KEY_SECRETS: list[SecretRef] = []


def _task(name: str, **kwargs: Any):
    ref = f"__WORKFLOW_NAME__.{name}"
    return task(
        task_ref=ref,
        runtime=Runtime(
            executor="docker",
            image=IMAGE,
            command=["python", "-m", "nexus.entrypoint", "--module", "dag"],
            args=[ref],
            secrets=API_KEY_SECRETS or None,
        ),
        **kwargs,
    )


# ── input ────────────────────────────────────────────────────────────────────


def _synthetic_count(source: str) -> int | None:
    if not source.startswith("synthetic://"):
        return None
    n = int(source[len("synthetic://"):])
    if n <= 0:
        raise ValueError(f"{source}: the record count must be positive")
    return n


def synthetic_record(i: int) -> dict:
    """Record i of `synthetic://N`: an arithmetic question with its answer, so
    the aggregate can report accuracy. Pure in i, so any shard can regenerate
    exactly its own slice."""
    a, b = (i * 7919 + 3) % 97, (i * 104729 + 11) % 89
    return {"id": f"q-{i:08d}", "prompt": f"What is {a} + {b}?", "expected": str(a + b)}


def _path(source: str) -> str:
    return source[len("file://"):] if source.startswith("file://") else source


def _records(source: str, start: int, stop: int) -> Iterator[dict]:
    """Records [start, stop) of the input, streamed rather than loaded whole."""
    if _synthetic_count(source) is not None:
        for i in range(start, stop):
            yield synthetic_record(i)
        return
    with open(_path(source)) as f:
        for i, line in enumerate(f):
            if i >= stop:
                break
            if i >= start:
                rec = json.loads(line)
                if not isinstance(rec, dict) or not rec.get("id") or not isinstance(rec.get("prompt"), str):
                    raise ValueError(f"{source} line {i + 1}: expected {{\"id\": ..., \"prompt\": \"...\"}}")
                yield rec


# ── the model ────────────────────────────────────────────────────────────────


def make_client(ctx: TaskContext, provider: str, model: str, base_url: str, api_key_env: str) -> LLMClient:
    """An LLMClient for this task. The key comes from the environment variable
    `api_key_env` names (supply it as a secret), never from a parameter."""
    if api_key_env:
        return LLMClient(ctx, provider=provider, model=model, base_url=base_url, api_key_env=api_key_env)
    return LLMClient(ctx, provider=provider, model=model, base_url=base_url, api_key=MOCK_API_KEY)


def estimated_cost_usd(input_tokens: int, output_tokens: int, prices: dict) -> float:
    return round(
        input_tokens / 1e6 * float(prices.get("input_per_1m_tokens_usd", 0))
        + output_tokens / 1e6 * float(prices.get("output_per_1m_tokens_usd", 0)),
        6,
    )


# ── tasks ────────────────────────────────────────────────────────────────────


@_task(
    "plan_shards",
    resources=Resources(cpu="250m", memory="256Mi"),
    timeouts=Timeouts(start_to_close="10m", heartbeat="2m"),
    retry=Retry(max_attempts=3, retry_on=["transient", "infrastructure"]),
)
def plan_shards(ctx: TaskContext, source: str, shard_size: int) -> list:
    """Cut the input into shard descriptors [{shard, source, start, stop}]."""
    if shard_size <= 0:
        raise ValueError("shard_size must be positive")
    total = _synthetic_count(source)
    if total is None:
        with open(_path(source)) as f:
            total = sum(1 for _ in f)
    if total == 0:
        raise ValueError(f"{source} has no records")
    shards = [
        {"shard": i, "source": source, "start": start, "stop": min(start + shard_size, total)}
        for i, start in enumerate(range(0, total, shard_size))
    ]
    if len(shards) > MAX_SHARDS:
        raise ValueError(
            f"{total} records at shard_size={shard_size} is {len(shards)} shards, over MAX_SHARDS={MAX_SHARDS}; "
            "raise shard_size"
        )
    ctx.log.info("planned", records=total, shards=len(shards), shard_size=shard_size)
    return shards


def _fresh_state(shard: dict, model: str) -> dict:
    return {"shard": shard["shard"], "start": shard["start"], "model": model, "next": 0,
            "answers": [], "input_tokens": 0, "output_tokens": 0, "requests": 0}


@_task(
    "infer_shard",
    resources=Resources(cpu="500m", memory="512Mi"),
    timeouts=Timeouts(start_to_close="2h", heartbeat="10m"),
    # rate_limited: a provider throttle that outlasted the in-task retries. The
    # platform retries it after the provider's Retry-After without charging the
    # retry budget (up to a cap), and the retry resumes from the checkpoint.
    retry=Retry(max_attempts=4, initial="30s", max="10m",
                retry_on=["rate_limited", "transient", "worker_lost", "infrastructure", "timeout"],
                retry_on_different_worker=True),
    # A shard answered once with this model and these prompts is not paid for
    # again for 30 days (memoization; the checkpoint covers the retry case).
    cache=Cache(enabled=True, ttl="30d"),
    outputs={"summary": {"type": "json", "required": True, "max_bytes": 2048}},
)
def infer_shard(ctx: TaskContext, shard: dict, provider: str, model: str, base_url: str,
                api_key_env: str, checkpoint_every: int, max_tokens: int, output_prefix: str,
                example_prices: dict) -> dict:
    """Answer one shard's prompts, checkpointing as it goes; return a small summary."""
    if checkpoint_every <= 0:
        raise ValueError("checkpoint_every must be positive")
    expected = shard["stop"] - shard["start"]
    llm = make_client(ctx, provider, model, base_url, api_key_env)

    state = ctx.resume()
    if state is not None and (state.get("shard") != shard["shard"] or state.get("start") != shard["start"]
                              or state.get("model") != model):
        # Not this work's checkpoint (the plan or the model changed under the
        # same key): starting over is always correct.
        ctx.log.warning("checkpoint does not match this shard; starting over")
        state = None
    state = state or _fresh_state(shard, model)
    resumed_from = int(state["next"])
    if resumed_from:
        ctx.log.info("resuming", shard=shard["shard"], answered=resumed_from, of=expected)

    ids: list[str] = []
    asked = 0
    for offset, rec in enumerate(_records(shard["source"], shard["start"], shard["stop"])):
        ids.append(str(rec["id"]))
        if offset < state["next"]:
            continue  # answered by an earlier attempt: never asked again
        c = llm.complete(rec["prompt"], max_tokens=max_tokens, system=rec.get("system"))
        asked += 1
        answer = {"id": str(rec["id"]), "answer": c.text.strip(), "input_tokens": c.input_tokens,
                  "output_tokens": c.output_tokens, "finish_reason": c.finish_reason}
        if "expected" in rec:
            answer["correct"] = answer["answer"] == str(rec["expected"]).strip()
        state["answers"].append(answer)
        state["input_tokens"] += c.input_tokens
        state["output_tokens"] += c.output_tokens
        state["requests"] += 1
        state["next"] = offset + 1
        if state["next"] % checkpoint_every == 0 and state["next"] < expected:
            ctx.checkpoint(state)
        ctx.progress(state["next"] / expected)

    answered = [a["id"] for a in state["answers"]]
    if answered != ids or len(ids) != expected:
        raise ValueError(
            f"shard {shard['shard']}: answered {len(answered)} records, expected the {expected} "
            f"records [{shard['start']}, {shard['stop']}) exactly once; did the input change?"
        )

    # Write to a temporary name and rename: a crash never leaves a truncated
    # part file that a reader could mistake for complete.
    os.makedirs(output_prefix, exist_ok=True)
    final = os.path.join(output_prefix, f"part-{shard['shard']:05d}.jsonl")
    tmp = f"{final}.{ctx.attempt_id or 'local'}.tmp"
    digest = hashlib.sha256()
    with open(tmp, "w") as fh:
        for a in state["answers"]:
            line = json.dumps(a, sort_keys=True) + "\n"
            fh.write(line)
            digest.update(line.encode())
    os.replace(tmp, final)

    graded = [a["correct"] for a in state["answers"] if "correct" in a]
    return {
        "shard": shard["shard"],
        "records": expected,
        "resumed_from": resumed_from,
        "asked_this_attempt": asked,
        "requests": state["requests"],
        "input_tokens": state["input_tokens"],
        "output_tokens": state["output_tokens"],
        "graded": len(graded),
        "correct": sum(graded),
        "estimated_cost_usd": estimated_cost_usd(state["input_tokens"], state["output_tokens"], example_prices),
        "output": final,
        "sha256": digest.hexdigest(),
        "model": model,
    }


@_task(
    "aggregate",
    resources=Resources(cpu="250m", memory="256Mi"),
    timeouts=Timeouts(start_to_close="10m", heartbeat="2m"),
    retry=Retry(max_attempts=1),
    # Run even if shards failed, so the error names every missing shard at once.
    trigger_rule="all_done",
)
def aggregate(ctx: TaskContext, summaries: list, shards: list, example_prices: dict) -> dict:
    """Verify every shard and record was answered exactly once; total the tokens."""
    missing = [s["shard"] for s, r in zip(shards, summaries) if r is None]
    if missing or len(summaries) != len(shards):
        raise ValueError(
            f"{len(missing)} of {len(shards)} shards have no result (shards {missing[:20]}); "
            "retry the run: finished shards are reused and interrupted ones resume from their checkpoints"
        )
    seen = sorted(r["shard"] for r in summaries)
    if seen != list(range(len(shards))):
        raise ValueError(f"shard results are not exactly one per shard: {seen[:20]}")
    for s, r in zip(shards, summaries):
        if r["records"] != s["stop"] - s["start"] or r["shard"] != s["shard"]:
            raise ValueError(f"shard {s['shard']} reported {r['records']} records, expected {s['stop'] - s['start']}")
    covered = sorted((s["start"], s["stop"]) for s in shards)
    if covered[0][0] != 0 or any(a[1] != b[0] for a, b in zip(covered, covered[1:])):
        raise ValueError("the shards do not tile the input: records are missing or duplicated")

    records = sum(r["records"] for r in summaries)
    input_tokens = sum(r["input_tokens"] for r in summaries)
    output_tokens = sum(r["output_tokens"] for r in summaries)
    graded = sum(r["graded"] for r in summaries)
    report = {
        "records": records,
        "shards": len(summaries),
        "requests": sum(r["requests"] for r in summaries),
        "input_tokens": input_tokens,
        "output_tokens": output_tokens,
        "resumed_shards": sum(1 for r in summaries if r["resumed_from"] > 0),
        "accuracy": round(sum(r["correct"] for r in summaries) / graded, 6) if graded else None,
        # An ESTIMATE at the illustrative example_prices; the platform's
        # usage ledger and price book are the source of truth for cost.
        "estimated_cost_usd": estimated_cost_usd(input_tokens, output_tokens, example_prices),
        "example_prices": example_prices,
        "outputs": [r["output"] for r in summaries],
    }
    ctx.log.info("aggregated", **{k: v for k, v in report.items() if k != "outputs"})
    return report


# ── workflow ─────────────────────────────────────────────────────────────────


@workflow(
    name="__WORKFLOW_NAME__",
    namespace="__NAMESPACE__",
    description="Shard prompts, answer them with an LLM under shared rate limits, resuming mid-shard; aggregate.",
    params={
        "source": {"type": "string", "required": False, "default": "synthetic://200"},
        "shard_size": {"type": "int", "required": False, "default": 50},
        "provider": {"type": "string", "required": False, "default": "openai"},
        "model": {"type": "string", "required": False, "default": "mock-large"},
        "base_url": {"type": "string", "required": False, "default": "http://mock-llm:8089"},
        # The NAME of the environment variable holding the key (a secret), never
        # the key. Empty for the mock.
        "api_key_env": {"type": "string", "required": False, "default": ""},
        "checkpoint_every": {"type": "int", "required": False, "default": 10},
        "max_tokens": {"type": "int", "required": False, "default": 64},
        "output_prefix": {"type": "string", "required": False, "default": "/data/llm-outputs"},
        "example_prices": {"type": "json", "required": False, "default": EXAMPLE_PRICES},
    },
    run_timeout="12h",
)
def llm_batch_inference(source: str, shard_size: int, provider: str, model: str, base_url: str,
                        api_key_env: str, checkpoint_every: int, max_tokens: int, output_prefix: str,
                        example_prices: dict):
    shards = plan_shards(source=source, shard_size=shard_size)
    summaries = infer_shard.map(
        shards, item="shard", max_fanout=MAX_SHARDS,
        provider=provider, model=model, base_url=base_url, api_key_env=api_key_env,
        checkpoint_every=checkpoint_every, max_tokens=max_tokens, output_prefix=output_prefix,
        example_prices=example_prices,
    )
    return aggregate(summaries=summaries, shards=shards, example_prices=example_prices)


if __name__ == "__main__":
    compiled = llm_batch_inference.compile()
    print(compiled.to_json(indent=2))
    print(f"\nspec hash: {compiled.hash}")
