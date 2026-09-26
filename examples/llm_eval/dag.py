# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""llm_eval -- evaluate a model with three graders; the quality gate fails the run.

    load --> answer (one child per batch) --> grade --> verdict

  * load     the built-in evaluation set: 40 arithmetic questions and 10
             sentiment reviews, each with a reference answer, cut into batches.
  * answer   a MAP over the batches: the model under test answers every item
             through `nexus.llm.LLMClient` (a platform permit per call on the
             keys `mock` and `mock/<model>`, in-task 429 retries, token usage
             reported as `tokens:mock/<model>:...`).
  * grade    three graders per answer: exact match, a regex, and LLM-as-judge
             (the judge model is shown the reference and the candidate under a
             "You are a strict grader" system prompt and answers PASS/FAIL).
  * verdict  accuracy per grader and per category, then the QUALITY GATE:
             FAILS THE RUN when any grader's accuracy is below `min_accuracy`,
             or when the graders disagree about an item's arithmetic answer
             (a judge that rubber-stamps would show up here).

With the default `model=mock-large` every grader scores 1.0 and the run
succeeds; `--param model=mock-small` (which gets ~30% of arithmetic wrong)
fails at the gate. Needs Python 3 and the SDK on the agent, the mock at
http://mock-llm:8089, and this directory mounted at /opt/nexus/examples/llm_eval.
Nothing calls a real provider.

    python3 examples/llm_eval/check_local.py      # both runs, locally, against an in-process mock
"""

from __future__ import annotations

import re

from nexus import Resources, Retry, Runtime, TaskContext, Timeouts, task, workflow
from nexus.llm import LLMClient
from nexus.usage import usage_subject

MODULE = "/opt/nexus/examples/llm_eval/dag.py"
MAX_BATCHES = 16
MOCK_API_KEY = "mock-key-not-a-secret"
GRADERS = ("exact", "regex", "judge")
JUDGE_SYSTEM = ("You are a strict grader. Compare the candidate answer with the reference answer. "
                "Reply with exactly PASS if they mean the same thing, otherwise FAIL.")
_SMALL = Resources(cpu="100m", memory="128Mi")
_LLM_RETRY = Retry(max_attempts=3, initial="1s", max="10s",
                   retry_on=["transient", "rate_limited", "worker_lost", "infrastructure"])

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


def _runtime(ref: str) -> Runtime:
    return Runtime(executor="subprocess",
                   command=["python3", "-m", "nexus.entrypoint", "--module", MODULE], args=[ref])


def _client(ctx: TaskContext, model: str, base_url: str) -> LLMClient:
    return LLMClient(ctx, provider="openai", model=model, base_url=base_url, api_key=MOCK_API_KEY,
                     usage_provider="mock", rate_limit_keys=[usage_subject("mock"), usage_subject("mock", model)],
                     max_retry_wait_s=10.0)


def items() -> list[dict]:
    out = []
    for i in range(40):
        a, b, op = (i * 37 + 12) % 90 + 2, (i * 53 + 7) % 40 + 1, "+-*"[i % 3]
        ref = a + b if op == "+" else a - b if op == "-" else a * b
        out.append({"id": f"arith-{i:03d}", "category": "arithmetic",
                    "prompt": f"What is {a} {op} {b}? Answer with the number only.",
                    "reference": str(ref), "pattern": rf"\s*{ref}\s*\.?\s*"})
    for i, (review, label) in enumerate(_REVIEWS):
        out.append({"id": f"sent-{i:03d}", "category": "sentiment",
                    "prompt": ("Classify the sentiment of this review as positive, negative or neutral. "
                               f"Answer with one word. Review: {review}"),
                    "reference": label, "pattern": rf"(?i)\s*{label}\s*\.?\s*"})
    return out


def _norm(s: str) -> str:
    return " ".join(s.strip().lower().rstrip(".!?;:,").split())


@task(task_ref="llm_eval.load", resources=_SMALL, timeouts=Timeouts(start_to_close="5m", heartbeat="5m"),
      retry=Retry(max_attempts=2, initial="1s", retry_on=["transient", "infrastructure"]),
      runtime=_runtime("llm_eval.load"))
def load(ctx: TaskContext, batch_size: int) -> list:
    """The evaluation set, as batches of item ids (the items are regenerated where needed)."""
    if not 1 <= batch_size <= 50:
        raise ValueError("batch_size must be in [1, 50]")
    ids = [it["id"] for it in items()]
    batches = [{"batch": n, "ids": ids[i:i + batch_size]} for n, i in enumerate(range(0, len(ids), batch_size))]
    if len(batches) > MAX_BATCHES:
        raise ValueError(f"{len(batches)} batches is over MAX_BATCHES={MAX_BATCHES}")
    return batches


@task(task_ref="llm_eval.answer", resources=_SMALL, timeouts=Timeouts(start_to_close="10m", heartbeat="2m"),
      retry=_LLM_RETRY, outputs={"answers": {"type": "json", "required": True, "max_bytes": 16384}},
      runtime=_runtime("llm_eval.answer"))
def answer(ctx: TaskContext, batch: dict, model: str, base_url: str) -> dict:
    """The model under test answers one batch."""
    by_id = {it["id"]: it for it in items()}
    llm = _client(ctx, model, base_url)
    out = []
    for n, item_id in enumerate(batch["ids"], 1):
        c = llm.complete(by_id[item_id]["prompt"], max_tokens=16)
        out.append([item_id, c.text.strip()[:200], c.input_tokens, c.output_tokens])
        ctx.progress(n / len(batch["ids"]))
    return {"batch": batch["batch"], "answers": out}


@task(task_ref="llm_eval.grade", resources=_SMALL, timeouts=Timeouts(start_to_close="10m", heartbeat="2m"),
      retry=_LLM_RETRY, runtime=_runtime("llm_eval.grade"))
def grade(ctx: TaskContext, answered: list, judge_model: str, base_url: str) -> list:
    """[id, category, exact, regex, judge] per item."""
    missing = [n for n, a in enumerate(answered) if a is None]
    if missing:
        raise ValueError(f"batches {missing} have no answers")
    by_id = {it["id"]: it for it in items()}
    judge = _client(ctx, judge_model, base_url)
    rows = []
    for a in answered:
        for item_id, text, _, _ in a["answers"]:
            it = by_id[item_id]
            verdict = judge.complete(f"Question: {it['prompt']}\nReference: {it['reference']}\nCandidate: {text}",
                                     max_tokens=4, system=JUDGE_SYSTEM)
            rows.append([item_id, it["category"], _norm(text) == _norm(it["reference"]),
                         re.fullmatch(it["pattern"], text) is not None,
                         verdict.text.strip().upper().startswith("PASS")])
    return rows


@task(task_ref="llm_eval.verdict", resources=_SMALL, timeouts=Timeouts(start_to_close="5m", heartbeat="5m"),
      retry=Retry(max_attempts=1),  # a failed gate is a verdict, not a glitch
      outputs={"verdict": {"type": "json", "required": True}}, runtime=_runtime("llm_eval.verdict"))
def verdict(ctx: TaskContext, graded: list, answered: list, model: str, min_accuracy: float) -> dict:
    """Metrics, then the quality gate: raise (failing the run) below min_accuracy."""
    n = len(graded)
    expected = [it["id"] for it in items()]
    problems = []
    if sorted(r[0] for r in graded) != sorted(expected):
        problems.append(f"graded {n} items, expected each of the {len(expected)} exactly once")
    accuracy = {g: round(sum(r[i] for r in graded) / n, 4) for i, g in enumerate(GRADERS, start=2)}
    by_category = {c: {g: round(sum(r[i] for r in rs) / len(rs), 4) for i, g in enumerate(GRADERS, start=2)}
                   for c in sorted({r[1] for r in graded}) for rs in [[r for r in graded if r[1] == c]]}
    disagree = [r[0] for r in graded if len({r[2], r[3], r[4]}) > 1]
    if disagree:
        problems.append(f"graders disagree on {disagree[:5]}")
    below = {g: v for g, v in accuracy.items() if v < min_accuracy}
    if below:
        problems.append(f"{model} is below min_accuracy={min_accuracy}: {below}")
    report = {
        "ok": not problems, "model": model, "items": n, "min_accuracy": min_accuracy,
        "accuracy": accuracy, "by_category": by_category,
        "tokens": {"input": sum(x[2] for a in answered for x in a["answers"]),
                   "output": sum(x[3] for a in answered for x in a["answers"])},
        "failed_items": [r[0] for r in graded if not r[2]][:20],
        "problems": problems,
    }
    ctx.log.info("eval verdict", ok=report["ok"], accuracy=accuracy)
    if problems:
        raise AssertionError("llm eval quality gate failed: " + "; ".join(problems))
    return report


@workflow(
    name="llm-eval",
    description="Evaluate a model with exact, regex and LLM-as-judge graders; the quality gate fails the run "
                "below min_accuracy.",
    params={
        "model": {"type": "string", "required": False, "default": "mock-large"},
        "judge_model": {"type": "string", "required": False, "default": "mock-large"},
        "base_url": {"type": "string", "required": False, "default": "http://mock-llm:8089"},
        "batch_size": {"type": "int", "required": False, "default": 10},
        "min_accuracy": {"type": "float", "required": False, "default": 0.9},
    },
    run_timeout="30m",
)
def llm_eval(model: str, judge_model: str, base_url: str, batch_size: int, min_accuracy: float):
    batches = load(batch_size=batch_size)
    answered = answer.map(batches, item="batch", max_fanout=MAX_BATCHES, model=model, base_url=base_url)
    graded = grade(answered=answered, judge_model=judge_model, base_url=base_url)
    return verdict(graded=graded, answered=answered, model=model, min_accuracy=min_accuracy)


if __name__ == "__main__":
    print(llm_eval.compile().to_yaml())
