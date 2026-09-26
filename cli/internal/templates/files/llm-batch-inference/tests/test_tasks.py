"""Tests for the task functions. They run the real handlers against the SDK's
mock model server (nexus.testing.MockLLMServer): no platform, no paid API, no key."""

from __future__ import annotations

import json

import pytest

from dag import (
    EXAMPLE_PRICES,
    MAX_SHARDS,
    aggregate,
    infer_shard,
    llm_batch_inference,
    plan_shards,
    synthetic_record,
)
from nexus import InMemoryResultStore, TaskContext, derive_idempotency_key
from nexus.testing import MockLLMServer
from nexus.testing.mock_llm import count_tokens


@pytest.fixture
def mock():
    with MockLLMServer() as server:
        yield server


def _infer(ctx, shard, base_url, out, **kw):
    args = dict(provider="openai", model="mock-large", base_url=base_url, api_key_env="",
                checkpoint_every=5, max_tokens=16, output_prefix=str(out), example_prices=EXAMPLE_PRICES)
    args.update(kw)
    return infer_shard(ctx, shard=shard, **args)


def _expected_tokens(start, stop):
    recs = [synthetic_record(i) for i in range(start, stop)]
    # The mock counts " <prompt>" words in and one word ("19") out.
    return sum(count_tokens(" " + r["prompt"]) for r in recs), sum(count_tokens(r["expected"]) for r in recs)


def test_plan_covers_every_record_exactly_once(make_ctx):
    shards = plan_shards(make_ctx("plan_shards"), source="synthetic://105", shard_size=50)
    assert [(s["start"], s["stop"]) for s in shards] == [(0, 50), (50, 100), (100, 105)]


def test_plan_refuses_more_shards_than_the_fanout_allows(make_ctx):
    with pytest.raises(ValueError, match="MAX_SHARDS"):
        plan_shards(make_ctx("plan_shards"), source=f"synthetic://{MAX_SHARDS + 1}", shard_size=1)


def test_infer_answers_every_prompt_and_reports_tokens(make_ctx, mock, tmp_path):
    ctx = make_ctx("infer_shard")
    shard = {"shard": 2, "source": "synthetic://100", "start": 40, "stop": 60}
    out = _infer(ctx, shard, mock.base_url, tmp_path)

    assert out["records"] == 20 and out["correct"] == 20 and out["resumed_from"] == 0
    tin, tout = _expected_tokens(40, 60)
    assert (out["input_tokens"], out["output_tokens"]) == (tin, tout)
    stats = mock.stats
    assert stats.completions == 20 and (stats.input_tokens, stats.output_tokens) == (tin, tout)
    # Usage units under the platform's key convention, for the usage ledger.
    assert ctx.usage_totals["tokens:openai/mock-large:input"] == tin
    assert ctx.usage_totals["requests:openai/mock-large"] == 20
    lines = [json.loads(x) for x in (tmp_path / "part-00002.jsonl").read_text().splitlines()]
    assert [x["id"] for x in lines] == [f"q-{i:08d}" for i in range(40, 60)]
    assert sorted(p.name for p in tmp_path.iterdir()) == ["part-00002.jsonl"]  # no .tmp left
    assert len(json.dumps(out)) < 2048  # within the declared max_bytes


def test_infer_rides_out_429s(make_ctx, tmp_path):
    with MockLLMServer(rate_limit_every=4, retry_after_s=0.01) as mock:
        shard = {"shard": 0, "source": "synthetic://30", "start": 0, "stop": 30}
        out = _infer(make_ctx("infer_shard"), shard, mock.base_url, tmp_path)
        stats = mock.stats
    assert out["correct"] == 30 and out["requests"] == 30
    # Throttled calls were retried in-task, and are neither answers nor tokens.
    assert stats.throttled > 0 and stats.completions == 30


class _CrashAfterCheckpoints(TaskContext):
    """A context whose process 'dies' right after its Nth checkpoint."""

    def checkpoint(self, state):
        info = super().checkpoint(state)
        self.saved += 1
        if self.saved == self.crash_at:
            raise ConnectionError("simulated worker loss after a checkpoint")
        return info


def _attempt(cls, store, number, **extra):
    ctx = cls(run_id="run-1", node_id="infer_shard[0]", attempt_id=f"a{number}", attempt_number=number,
              task_ref="__WORKFLOW_NAME__.infer_shard", task_version=1,
              idempotency_key=derive_idempotency_key("run-1", "infer_shard[0]", None, 1), store=store)
    for k, v in extra.items():
        setattr(ctx, k, v)
    return ctx


def test_a_retry_resumes_mid_shard_and_never_re_asks(mock, tmp_path):
    store = InMemoryResultStore()  # the durable checkpoint store both attempts share
    shard = {"shard": 0, "source": "synthetic://23", "start": 0, "stop": 23}

    first = _attempt(_CrashAfterCheckpoints, store, 1, saved=0, crash_at=2)
    with pytest.raises(ConnectionError):
        _infer(first, shard, mock.base_url, tmp_path)
    assert mock.stats.completions == 10  # two checkpoints of 5, then the crash
    assert not list(tmp_path.iterdir())  # nothing half-written

    second = _attempt(TaskContext, store, 2)
    out = _infer(second, shard, mock.base_url, tmp_path)
    assert out["resumed_from"] == 10 and out["asked_this_attempt"] == 13
    # The mock saw each prompt exactly once across both attempts.
    assert mock.stats.completions == 23
    assert out["requests"] == 23 and out["correct"] == 23
    assert (out["input_tokens"], out["output_tokens"]) == _expected_tokens(0, 23)
    # The second attempt's usage is only its own calls: the first already reported its 10.
    assert second.usage_totals["requests:openai/mock-large"] == 13
    ids = [json.loads(x)["id"] for x in (tmp_path / "part-00000.jsonl").read_text().splitlines()]
    assert ids == [f"q-{i:08d}" for i in range(23)]


def test_a_checkpoint_for_other_work_is_ignored(mock, tmp_path):
    store = InMemoryResultStore()
    ctx = _attempt(TaskContext, store, 1)
    ctx.checkpoint({"shard": 9, "start": 0, "model": "other", "next": 3, "answers": []})
    shard = {"shard": 0, "source": "synthetic://6", "start": 0, "stop": 6}
    out = _infer(_attempt(TaskContext, store, 2), shard, mock.base_url, tmp_path)
    assert out["resumed_from"] == 0 and mock.stats.completions == 6


def test_jsonl_input(make_ctx, mock, tmp_path):
    src = tmp_path / "in.jsonl"
    src.write_text("".join(json.dumps({"id": f"r{i}", "prompt": f"What is {i} + 1?"}) + "\n" for i in range(4)))
    shards = plan_shards(make_ctx("plan_shards"), source=str(src), shard_size=3)
    out = _infer(make_ctx("infer_shard"), shards[0], mock.base_url, tmp_path / "out")
    assert out["records"] == 3 and out["graded"] == 0
    answers = [json.loads(x)["answer"] for x in (tmp_path / "out" / "part-00000.jsonl").read_text().splitlines()]
    assert answers == ["1", "2", "3"]


def test_end_to_end_without_the_platform(make_ctx, mock, tmp_path):
    shards = plan_shards(make_ctx("plan_shards"), source="synthetic://45", shard_size=20)
    summaries = infer_shard.map(
        shards, item="shard", max_fanout=MAX_SHARDS, ctx=make_ctx("infer_shard"), provider="openai",
        model="mock-large", base_url=mock.base_url, api_key_env="", checkpoint_every=5, max_tokens=16,
        output_prefix=str(tmp_path), example_prices=EXAMPLE_PRICES,
    )
    report = aggregate(make_ctx("aggregate"), summaries=summaries, shards=shards, example_prices=EXAMPLE_PRICES)
    assert report["records"] == 45 and report["shards"] == 3 and report["accuracy"] == 1.0
    assert report["input_tokens"] == mock.stats.input_tokens
    assert report["output_tokens"] == mock.stats.output_tokens
    assert report["estimated_cost_usd"] > 0


def test_aggregate_names_the_missing_shards(make_ctx):
    shards = [{"shard": i, "source": "s", "start": i, "stop": i + 1} for i in range(3)]
    ok = {"shard": 0, "records": 1, "input_tokens": 1, "output_tokens": 1, "requests": 1,
          "graded": 0, "correct": 0, "resumed_from": 0, "output": "o"}
    with pytest.raises(ValueError, match=r"2 of 3 shards have no result \(shards \[1, 2\]\)"):
        aggregate(make_ctx("aggregate"), summaries=[ok, None, None], shards=shards, example_prices=EXAMPLE_PRICES)


def test_an_unset_key_variable_fails_before_any_call(make_ctx, mock, tmp_path, monkeypatch):
    monkeypatch.delenv("NEXUS_TEMPLATE_TEST_KEY", raising=False)
    shard = {"shard": 0, "source": "synthetic://2", "start": 0, "stop": 2}
    with pytest.raises(PermissionError, match="NEXUS_TEMPLATE_TEST_KEY"):
        _infer(make_ctx("infer_shard"), shard, mock.base_url, tmp_path, api_key_env="NEXUS_TEMPLATE_TEST_KEY")
    assert mock.stats.completions == 0


def test_workflow_shape():
    spec = llm_batch_inference.compile().spec
    node = next(t for t in spec["tasks"] if t["id"] == "infer_shard")
    assert node["map"]["max_fanout"] == MAX_SHARDS
    assert "ERROR_CLASS_RATE_LIMITED" in node["retry"]["retry_on"]
    assert spec["params"]["base_url"]["default_json"] == '"http://mock-llm:8089"'
