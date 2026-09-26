"""Tests for the task functions. They run the real handlers against the SDK's
mock model server (nexus.testing.MockLLMServer): no platform, no paid API, no key."""

from __future__ import annotations

import json

import pytest

from dag import (
    GRADERS,
    MAX_BATCHES,
    QualityGateFailed,
    answer,
    builtin_items,
    compute_metrics,
    grade,
    grade_exact,
    grade_regex,
    llm_eval,
    load_dataset,
    quality_gate,
)
from nexus.testing import MockLLMServer


@pytest.fixture(scope="module")
def mock():
    with MockLLMServer() as server:
        yield server


def evaluate(make_ctx, base_url, *, model, judge_model="mock-large", dataset="builtin://default",
             min_accuracy=0.9, gate_graders=GRADERS):
    """The workflow's body, run on the real handlers."""
    batches = load_dataset(make_ctx("load_dataset"), dataset=dataset, batch_size=10)
    answered = answer.map(batches, item="batch", max_fanout=MAX_BATCHES, ctx=make_ctx("answer"),
                          provider="openai", model=model, base_url=base_url, api_key_env="", max_tokens=32)
    graded = grade(make_ctx("grade"), batches=batches, answered=answered, provider="openai",
                   judge_model=judge_model, base_url=base_url, api_key_env="")
    metrics = compute_metrics(make_ctx("compute_metrics"), graded=graded, answered=answered, model=model)
    return metrics, lambda: quality_gate(make_ctx("quality_gate"), metrics=metrics, min_accuracy=min_accuracy,
                                         gate_graders=list(gate_graders))


def test_the_builtin_set_is_deterministic_and_unique():
    items = builtin_items()
    assert items == builtin_items()
    assert len({i["id"] for i in items}) == len(items) == 50
    assert {i["category"] for i in items} == {"arithmetic", "sentiment"}


def test_graders():
    item = {"reference": "19"}
    assert grade_exact(" 19. ", item) and not grade_exact("20", item)
    assert grade_regex("The answer is 19.", item) and not grade_regex("119", item) and not grade_regex("1.19", item)
    assert grade_regex("Positive", {"reference": "positive", "pattern": r"(?i)\s*positive\s*\.?\s*"})
    assert not grade_regex("negative", {"reference": "positive", "pattern": r"(?i)\s*positive\s*\.?\s*"})


def test_a_good_model_passes_the_gate(make_ctx, mock):
    metrics, gate = evaluate(make_ctx, mock.base_url, model="mock-large")
    assert metrics["accuracy"] == {"exact": 1.0, "regex": 1.0, "judge": 1.0}
    assert set(metrics["by_category"]) == {"arithmetic", "sentiment"}
    assert metrics["model_tokens"]["input"] > 0 and metrics["model_tokens"]["output"] >= metrics["items"]
    # The judge's calls are its own usage, under its own model's keys.
    assert metrics["judge_usage"]["requests:openai/mock-large"] == 50
    assert gate()["passed"] is True


def test_a_weak_model_fails_the_gate(make_ctx, mock):
    metrics, gate = evaluate(make_ctx, mock.base_url, model="mock-small")
    # mock-small gets a deterministic ~30% of arithmetic wrong; sentiment is fine.
    assert metrics["by_category"]["sentiment"]["exact"] == 1.0
    assert metrics["by_category"]["arithmetic"]["exact"] < 0.8
    # The three graders agree on this data: the judge is not a rubber stamp.
    assert metrics["accuracy"]["exact"] == metrics["accuracy"]["regex"] == metrics["accuracy"]["judge"] < 0.9
    with pytest.raises(QualityGateFailed, match=r"mock-small is below min_accuracy=0.9: exact="):
        gate()


def test_the_gate_threshold_and_graders_are_parameters(make_ctx, mock):
    _, gate = evaluate(make_ctx, mock.base_url, model="mock-small", min_accuracy=0.5, gate_graders=["judge"])
    assert gate()["passed"] is True
    with pytest.raises(ValueError, match="gate_graders"):
        quality_gate(make_ctx("quality_gate"), metrics={"accuracy": {}}, min_accuracy=0.5, gate_graders=["vibes"])


def test_throttling_is_absorbed(make_ctx):
    with MockLLMServer(rate_limit_every=5, retry_after_s=0.01) as throttled:
        metrics, gate = evaluate(make_ctx, throttled.base_url, model="mock-large")
        stats = throttled.stats
    assert stats.throttled > 0 and gate()["passed"] is True
    # Every item answered once and judged once, despite the 429s.
    assert stats.completions == 2 * metrics["items"]


def test_a_jsonl_dataset(make_ctx, mock, tmp_path):
    src = tmp_path / "eval.jsonl"
    src.write_text("".join(json.dumps(x) + "\n" for x in [
        {"id": "a", "prompt": "What is 2 + 2?", "reference": "4", "category": "math"},
        {"id": "b", "prompt": "Classify the sentiment: I love it", "reference": "positive"},
    ]))
    metrics, gate = evaluate(make_ctx, mock.base_url, model="mock-large", dataset=str(src))
    assert metrics["items"] == 2 and set(metrics["by_category"]) == {"math", "default"}
    assert gate()["passed"] is True


def test_duplicate_ids_are_refused(make_ctx, tmp_path):
    src = tmp_path / "dup.jsonl"
    src.write_text(json.dumps({"id": "x", "prompt": "p", "reference": "r"}) + "\n"
                   + json.dumps({"id": "x", "prompt": "q", "reference": "r"}) + "\n")
    with pytest.raises(ValueError, match="not unique"):
        load_dataset(make_ctx("load_dataset"), dataset=str(src), batch_size=10)


def test_workflow_shape():
    spec = llm_eval.compile().spec
    node = next(t for t in spec["tasks"] if t["id"] == "answer")
    assert node["map"]["max_fanout"] == MAX_BATCHES
    assert "ERROR_CLASS_RATE_LIMITED" in node["retry"]["retry_on"]
    assert [t["id"] for t in spec["tasks"]][-1] == "quality_gate"
