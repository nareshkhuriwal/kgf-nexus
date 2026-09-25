"""Tests for the task functions. They run the real handlers, no platform needed."""

from __future__ import annotations

import json

import pytest

from dag import DEFAULT_MODEL, MAX_SHARDS, aggregate, batch_inference, infer, plan_shards


def test_plan_covers_every_record_exactly_once(make_ctx):
    shards = plan_shards(make_ctx("plan_shards"), source="synthetic://1050", shard_size=500)
    assert [(s["start"], s["stop"]) for s in shards] == [(0, 500), (500, 1000), (1000, 1050)]


def test_plan_counts_lines_of_a_jsonl_file(make_ctx, tmp_path):
    p = tmp_path / "in.jsonl"
    p.write_text("".join(json.dumps({"id": i, "features": [0, 0, 0, 0]}) + "\n" for i in range(7)))
    shards = plan_shards(make_ctx("plan_shards"), source=str(p), shard_size=3)
    assert [s["stop"] - s["start"] for s in shards] == [3, 3, 1]


def test_plan_refuses_more_shards_than_the_fanout_allows(make_ctx):
    with pytest.raises(ValueError, match="MAX_SHARDS"):
        plan_shards(make_ctx("plan_shards"), source=f"synthetic://{MAX_SHARDS + 1}", shard_size=1)


def test_infer_writes_the_shard_and_returns_a_small_checkpoint(make_ctx, tmp_path):
    shard = {"shard": 3, "source": "synthetic://100", "start": 40, "stop": 60}
    out = infer(make_ctx("infer"), shard=shard, model=DEFAULT_MODEL, output_prefix=str(tmp_path), batch_size=7)
    assert out["rows"] == 20
    lines = (tmp_path / "part-00003.jsonl").read_text().splitlines()
    assert len(lines) == 20 and json.loads(lines[0])["id"] == "rec-00000040"
    assert out["positives"] == sum(json.loads(line)["label"] for line in lines)
    # Small enough for its declared max_bytes, which is what bounds the fan-in.
    assert len(json.dumps(out)) < 1024
    # No temporary file left behind.
    assert sorted(p.name for p in tmp_path.iterdir()) == ["part-00003.jsonl"]


def test_infer_is_deterministic_so_the_checkpoint_is_valid(make_ctx, tmp_path):
    shard = {"shard": 0, "source": "synthetic://50", "start": 0, "stop": 50}
    a = infer(make_ctx("infer"), shard=shard, model=DEFAULT_MODEL, output_prefix=str(tmp_path / "a"))
    b = infer(make_ctx("infer"), shard=shard, model=DEFAULT_MODEL, output_prefix=str(tmp_path / "b"))
    assert a["sha256"] == b["sha256"]


def test_infer_rejects_records_the_model_cannot_score(make_ctx, tmp_path):
    p = tmp_path / "in.jsonl"
    p.write_text(json.dumps({"id": "x", "features": [1, 2]}) + "\n")
    shard = {"shard": 0, "source": str(p), "start": 0, "stop": 1}
    with pytest.raises(ValueError, match="expects 4"):
        infer(make_ctx("infer"), shard=shard, model=DEFAULT_MODEL, output_prefix=str(tmp_path / "out"))


def test_end_to_end_without_the_platform(make_ctx, tmp_path):
    shards = plan_shards(make_ctx("plan_shards"), source="synthetic://230", shard_size=100)
    summaries = infer.map(shards, item="shard", max_fanout=MAX_SHARDS, ctx=make_ctx("infer"),
                          model=DEFAULT_MODEL, output_prefix=str(tmp_path))
    report = aggregate(make_ctx("aggregate"), summaries=summaries, shards=shards)
    assert report["records"] == 230 and report["shards"] == 3
    assert 0 < report["positive_rate"] < 1


def test_aggregate_names_the_missing_shards(make_ctx):
    shards = [{"shard": i, "source": "s", "start": i, "stop": i + 1} for i in range(3)]
    summaries = [{"rows": 1, "positives": 0, "score_sum": 0.1, "output": "o"}, None, None]
    with pytest.raises(ValueError, match=r"2 of 3 shards have no result \(shards \[1, 2\]\)"):
        aggregate(make_ctx("aggregate"), summaries=summaries, shards=shards)


def test_workflow_maps_over_the_shard_plan():
    spec = batch_inference.compile().spec
    node = next(t for t in spec["tasks"] if t["id"] == "infer")
    assert node["map"]["max_fanout"] == MAX_SHARDS
    assert node["cache"]["enabled"] is True
