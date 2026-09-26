"""__WORKFLOW_NAME__ -- evaluate an LLM against a reference set, with a quality gate.

    load_dataset --> answer (one child per batch) --> grade --> compute_metrics --> quality_gate

  * load_dataset     reads the evaluation items (JSON Lines, or the built-in
                     deterministic set of arithmetic and sentiment items) and
                     cuts them into batches.
  * answer           a MAP over the batches: the model under test answers every
                     item through `nexus.llm.LLMClient` (shared rate-limit
                     permits, in-task 429 retries, token usage reported).
  * grade            scores every answer with three graders:
                       exact  -- normalised string equality with the reference;
                       regex  -- the item's `pattern`, or the reference as a
                                 whole word when it has none;
                       judge  -- LLM-as-judge: a judge model is shown the
                                 reference and the candidate and answers
                                 PASS or FAIL.
  * compute_metrics  accuracy per grader, per grader and category, and tokens.
  * quality_gate     FAILS THE RUN when any gated grader's accuracy is below
                     `min_accuracy`, so a regression cannot ship silently.

The defaults point at the mock model server (`python -m nexus.testing.mock_llm`)
at http://mock-llm:8089. `mock-large` passes the gate; `mock-small` gets about
30% of arithmetic wrong and fails it:

    pytest
    nexusctl workflow submit dag.py
    nexusctl run create __WORKFLOW_NAME__ --wait                        # passes
    nexusctl run create __WORKFLOW_NAME__ --param model=mock-small --wait  # the gate fails the run
"""

from __future__ import annotations

import json
import re
from typing import Any

from nexus import Cache, Resources, Retry, Runtime, SecretRef, TaskContext, Timeouts, task, workflow
from nexus.llm import LLMClient

IMAGE = "__WORKFLOW_NAME__:0.1.0"

MAX_BATCHES = 64
MAX_BATCH_SIZE = 50
# Answers are stored truncated to this many characters, which bounds each
# batch's output (and so the fan-in) by construction.
MAX_ANSWER_CHARS = 1000
GRADERS = ("exact", "regex", "judge")

# Sent when `api_key_env` is empty: the mock accepts any non-empty key, and a
# real provider rejects this one (401 -> a terminal LLMAuthError).
MOCK_API_KEY = "mock-key-not-a-secret"

# The provider key as a SECRET REFERENCE, resolved by the worker into the
# environment variable `name` (pass that name as api_key_env). Empty for the mock.
#   API_KEY_SECRETS = [SecretRef(name="OPENAI_API_KEY", ref="vault://kv/llm/openai")]
API_KEY_SECRETS: list[SecretRef] = []

JUDGE_SYSTEM = (
    "You are a strict grader. Compare the candidate answer with the reference answer. "
    "Reply with exactly PASS if they mean the same thing, otherwise FAIL."
)


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


# ── the dataset ──────────────────────────────────────────────────────────────

_REVIEWS = [
    ("I love this product, it is excellent.", "positive"),
    ("Terrible service and the food was awful.", "negative"),
    ("The package arrived on Tuesday.", "neutral"),
    ("Great value, would recommend to friends.", "positive"),
    ("Broken on arrival, I want a refund.", "negative"),
    ("It is a chair with four legs.", "neutral"),
    ("Best purchase this year, amazing quality.", "positive"),
    ("Slow shipping and rude support.", "negative"),
    ("The manual is printed in English.", "neutral"),
    ("Wonderful staff, happy with everything.", "positive"),
]


def builtin_items() -> list[dict]:
    """40 arithmetic and 10 sentiment items, with references. Deterministic."""
    items = []
    for i in range(40):
        a, b, op = (i * 37 + 12) % 90 + 2, (i * 53 + 7) % 40 + 1, "+-*"[i % 3]
        answer = a + b if op == "+" else a - b if op == "-" else a * b
        items.append({"id": f"arith-{i:03d}", "category": "arithmetic",
                      "prompt": f"What is {a} {op} {b}? Answer with the number only.", "reference": str(answer)})
    for i, (review, label) in enumerate(_REVIEWS):
        items.append({
            "id": f"sent-{i:03d}", "category": "sentiment",
            "prompt": ("Classify the sentiment of this review as positive, negative or neutral. "
                       f"Answer with one word. Review: {review}"),
            "reference": label, "pattern": rf"(?i)\s*{label}\s*\.?\s*",
        })
    return items


def _load(dataset: str) -> list[dict]:
    if dataset == "builtin://default":
        return builtin_items()
    path = dataset[len("file://"):] if dataset.startswith("file://") else dataset
    items = []
    with open(path) as f:
        for n, line in enumerate(f, 1):
            if not line.strip():
                continue
            item = json.loads(line)
            if not isinstance(item, dict) or not item.get("id") or not isinstance(item.get("prompt"), str) \
                    or not isinstance(item.get("reference"), str):
                raise ValueError(f"{dataset} line {n}: expected {{\"id\", \"prompt\", \"reference\"}}")
            item.setdefault("category", "default")
            items.append(item)
    return items


# ── graders ──────────────────────────────────────────────────────────────────


def normalize(s: str) -> str:
    return " ".join(s.strip().lower().rstrip(".!?;:,").split())


def grade_exact(answer: str, item: dict) -> bool:
    return normalize(answer) == normalize(item["reference"])


def grade_regex(answer: str, item: dict) -> bool:
    pattern = item.get("pattern")
    if pattern:
        return re.fullmatch(pattern, answer) is not None
    # The reference as a whole token: "19" matches "is 19." but not "119" or "19.5".
    ref = re.escape(item["reference"].strip())
    return re.search(rf"(?<![\w.-]){ref}(?!\w|\.\d)", answer) is not None


def judge_prompt(item: dict, answer: str) -> str:
    return f"Question: {item['prompt']}\nReference: {item['reference']}\nCandidate: {answer}"


def make_client(ctx: TaskContext, provider: str, model: str, base_url: str, api_key_env: str) -> LLMClient:
    """An LLMClient for this task; the key comes from `api_key_env` (a secret)."""
    if api_key_env:
        return LLMClient(ctx, provider=provider, model=model, base_url=base_url, api_key_env=api_key_env)
    return LLMClient(ctx, provider=provider, model=model, base_url=base_url, api_key=MOCK_API_KEY)


_LLM_RETRY = Retry(max_attempts=4, initial="30s", max="10m",
                   retry_on=["rate_limited", "transient", "worker_lost", "infrastructure", "timeout"])


# ── tasks ────────────────────────────────────────────────────────────────────


@_task(
    "load_dataset",
    resources=Resources(cpu="250m", memory="256Mi"),
    timeouts=Timeouts(start_to_close="10m", heartbeat="2m"),
    retry=Retry(max_attempts=3, retry_on=["transient", "infrastructure"]),
)
def load_dataset(ctx: TaskContext, dataset: str, batch_size: int) -> list:
    """The items, as batches [{batch, items: [...]}]."""
    if not 0 < batch_size <= MAX_BATCH_SIZE:
        raise ValueError(f"batch_size must be in [1, {MAX_BATCH_SIZE}]")
    items = _load(dataset)
    if not items:
        raise ValueError(f"{dataset} has no items")
    ids = [it["id"] for it in items]
    if len(set(ids)) != len(ids):
        raise ValueError(f"{dataset}: item ids are not unique")
    batches = [{"batch": n, "items": items[i:i + batch_size]} for n, i in enumerate(range(0, len(items), batch_size))]
    if len(batches) > MAX_BATCHES:
        raise ValueError(f"{len(items)} items at batch_size={batch_size} is over MAX_BATCHES={MAX_BATCHES}")
    ctx.log.info("loaded", items=len(items), batches=len(batches))
    return batches


@_task(
    "answer",
    resources=Resources(cpu="250m", memory="256Mi"),
    timeouts=Timeouts(start_to_close="1h", heartbeat="10m"),
    retry=_LLM_RETRY,
    outputs={"answers": {"type": "json", "required": True, "max_bytes": 65536}},
)
def answer(ctx: TaskContext, batch: dict, provider: str, model: str, base_url: str, api_key_env: str,
           max_tokens: int) -> dict:
    """The model under test answers one batch."""
    llm = make_client(ctx, provider, model, base_url, api_key_env)
    out = []
    for n, item in enumerate(batch["items"], 1):
        c = llm.complete(item["prompt"], max_tokens=max_tokens, system=item.get("system"))
        out.append({"id": item["id"], "answer": c.text.strip()[:MAX_ANSWER_CHARS],
                    "input_tokens": c.input_tokens, "output_tokens": c.output_tokens})
        ctx.progress(n / len(batch["items"]))
    return {"batch": batch["batch"], "model": model, "answers": out}


@_task(
    "grade",
    resources=Resources(cpu="250m", memory="256Mi"),
    timeouts=Timeouts(start_to_close="1h", heartbeat="10m"),
    retry=_LLM_RETRY,
    # Graded once per (answers, judge): a re-run of an unchanged model is free.
    cache=Cache(enabled=True, ttl="7d"),
)
def grade(ctx: TaskContext, batches: list, answered: list, provider: str, judge_model: str, base_url: str,
          api_key_env: str) -> dict:
    """Every answer through the exact, regex and LLM-as-judge graders."""
    missing = [b["batch"] for b, a in zip(batches, answered) if a is None]
    if missing or len(answered) != len(batches):
        raise ValueError(f"batches {missing[:20]} have no answers; retry the run")
    judge = make_client(ctx, provider, judge_model, base_url, api_key_env)
    results = []
    total = sum(len(b["items"]) for b in batches)
    for b, a in zip(batches, answered):
        by_id = {x["id"]: x for x in a["answers"]}
        for item in b["items"]:
            got = by_id.get(item["id"])
            if got is None:
                raise ValueError(f"item {item['id']} was not answered")
            verdict = judge.complete(judge_prompt(item, got["answer"]), max_tokens=4, system=JUDGE_SYSTEM)
            results.append({
                "id": item["id"], "category": item.get("category", "default"),
                "exact": grade_exact(got["answer"], item),
                "regex": grade_regex(got["answer"], item),
                "judge": verdict.text.strip().upper().startswith("PASS"),
            })
            ctx.progress(len(results) / total)
    return {"results": results, "judge_model": judge_model, "judge_usage": ctx.usage_totals}


@_task(
    "compute_metrics",
    resources=Resources(cpu="250m", memory="256Mi"),
    timeouts=Timeouts(start_to_close="10m"),
    retry=Retry(max_attempts=1),
)
def compute_metrics(ctx: TaskContext, graded: dict, answered: list, model: str) -> dict:
    """Accuracy per grader and per (grader, category); tokens spent."""
    results = graded["results"]
    categories = sorted({r["category"] for r in results})
    metrics = {
        "model": model,
        "judge_model": graded["judge_model"],
        "items": len(results),
        "accuracy": {g: round(sum(r[g] for r in results) / len(results), 6) for g in GRADERS},
        "by_category": {
            c: {g: round(sum(r[g] for r in rs) / len(rs), 6) for g in GRADERS}
            for c in categories for rs in [[r for r in results if r["category"] == c]]
        },
        "failures": {g: [r["id"] for r in results if not r[g]][:20] for g in GRADERS},
        "model_tokens": {
            "input": sum(x["input_tokens"] for a in answered for x in a["answers"]),
            "output": sum(x["output_tokens"] for a in answered for x in a["answers"]),
        },
        "judge_usage": graded["judge_usage"],
    }
    ctx.log.info("metrics", accuracy=metrics["accuracy"], items=metrics["items"])
    return metrics


class QualityGateFailed(ValueError):
    """The model under test is below the bar. Terminal: a retry scores the same."""


@_task(
    "quality_gate",
    resources=Resources(cpu="100m", memory="128Mi"),
    timeouts=Timeouts(start_to_close="5m"),
    retry=Retry(max_attempts=1),  # a failed gate is a verdict, not a glitch
)
def quality_gate(ctx: TaskContext, metrics: dict, min_accuracy: float, gate_graders: list) -> dict:
    """Fail the run if any gated grader's accuracy is below min_accuracy."""
    unknown = sorted(set(gate_graders) - set(GRADERS))
    if unknown or not gate_graders:
        raise ValueError(f"gate_graders must be a non-empty subset of {list(GRADERS)}, got {gate_graders}")
    below = {g: metrics["accuracy"][g] for g in gate_graders if metrics["accuracy"][g] < min_accuracy}
    if below:
        raise QualityGateFailed(
            f"{metrics['model']} is below min_accuracy={min_accuracy}: "
            + ", ".join(f"{g}={v:.3f} (fails e.g. {metrics['failures'][g][:5]})" for g, v in sorted(below.items()))
        )
    ctx.log.info("quality gate passed", model=metrics["model"], accuracy=metrics["accuracy"])
    return {"passed": True, "model": metrics["model"], "min_accuracy": min_accuracy,
            "accuracy": metrics["accuracy"], "by_category": metrics["by_category"]}


# ── workflow ─────────────────────────────────────────────────────────────────


@workflow(
    name="__WORKFLOW_NAME__",
    namespace="__NAMESPACE__",
    description="Evaluate an LLM with exact, regex and LLM-as-judge graders; fail the run below min_accuracy.",
    params={
        "dataset": {"type": "string", "required": False, "default": "builtin://default"},
        "batch_size": {"type": "int", "required": False, "default": 10},
        "provider": {"type": "string", "required": False, "default": "openai"},
        "model": {"type": "string", "required": False, "default": "mock-large"},
        "judge_model": {"type": "string", "required": False, "default": "mock-large"},
        "base_url": {"type": "string", "required": False, "default": "http://mock-llm:8089"},
        "api_key_env": {"type": "string", "required": False, "default": ""},
        "max_tokens": {"type": "int", "required": False, "default": 32},
        "min_accuracy": {"type": "float", "required": False, "default": 0.9},
        "gate_graders": {"type": "json", "required": False, "default": list(GRADERS)},
    },
    run_timeout="6h",
)
def llm_eval(dataset: str, batch_size: int, provider: str, model: str, judge_model: str, base_url: str,
             api_key_env: str, max_tokens: int, min_accuracy: float, gate_graders: list):
    batches = load_dataset(dataset=dataset, batch_size=batch_size)
    answered = answer.map(
        batches, item="batch", max_fanout=MAX_BATCHES,
        provider=provider, model=model, base_url=base_url, api_key_env=api_key_env, max_tokens=max_tokens,
    )
    graded = grade(batches=batches, answered=answered, provider=provider, judge_model=judge_model,
                   base_url=base_url, api_key_env=api_key_env)
    metrics = compute_metrics(graded=graded, answered=answered, model=model)
    return quality_gate(metrics=metrics, min_accuracy=min_accuracy, gate_graders=gate_graders)


if __name__ == "__main__":
    compiled = llm_eval.compile()
    print(compiled.to_json(indent=2))
    print(f"\nspec hash: {compiled.hash}")
