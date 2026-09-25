"""Tests for the task functions. They run the real handlers, no platform needed."""

from __future__ import annotations

import sqlite3

import pytest

from dag import extract, load, pipeline, transform, validate

CSV = (
    "order_id,customer_id,order_date,amount,currency\n"
    "o-1,c-1,2026-09-01,10.50,usd\n"
    "o-2,c-2,2026-09-01,4.25,USD\n"
    "o-3,c-1,2026-09-02,100,eur\n"
)


@pytest.fixture
def source(tmp_path):
    p = tmp_path / "orders.csv"
    p.write_text(CSV)
    return str(p)


def test_extract_reads_rows_and_fingerprints_the_file(make_ctx, source):
    out = extract(make_ctx("extract"), source=source)
    assert [r["order_id"] for r in out["rows"]] == ["o-1", "o-2", "o-3"]
    assert len(out["sha256"]) == 64
    # file:// URLs are accepted too.
    assert extract(make_ctx("extract"), source="file://" + source)["sha256"] == out["sha256"]


def test_extract_rejects_a_file_missing_required_columns(make_ctx, tmp_path):
    p = tmp_path / "bad.csv"
    p.write_text("order_id,amount\n1,2\n")
    with pytest.raises(ValueError, match="missing required columns: customer_id"):
        extract(make_ctx("extract"), source=str(p))


def test_validate_splits_valid_and_rejected_rows(make_ctx):
    extracted = {
        "source": "s",
        "sha256": "x",
        "rows": [
            {"order_id": "1", "customer_id": "c", "order_date": "2026-09-01", "amount": "1", "currency": "USD"},
            {"order_id": "2", "customer_id": "c", "order_date": "09/01/2026", "amount": "1", "currency": "USD"},
            {"order_id": "3", "customer_id": "c", "order_date": "2026-09-01", "amount": "-5", "currency": "USD"},
            {"order_id": "1", "customer_id": "c", "order_date": "2026-09-01", "amount": "1", "currency": "USD"},
        ],
    }
    out = validate(make_ctx("validate"), extracted=extracted, max_reject_ratio=1.0)
    assert [r["order_id"] for r in out["valid"]] == ["1"]
    reasons = {r["line"]: r["reason"] for r in out["rejected"]}
    assert "not YYYY-MM-DD" in reasons[3]
    assert "negative" in reasons[4]
    assert "duplicate" in reasons[5]


def test_validate_fails_the_task_over_the_reject_ratio(make_ctx):
    rows = [{"order_id": str(i), "customer_id": "", "order_date": "x", "amount": "1", "currency": "USD"} for i in range(3)]
    with pytest.raises(ValueError, match="over max_reject_ratio"):
        validate(make_ctx("validate"), extracted={"source": "s", "sha256": "x", "rows": rows}, max_reject_ratio=0.05)


def test_transform_normalizes_and_totals_per_day(make_ctx, source):
    validated = validate(make_ctx("validate"), extracted=extract(make_ctx("extract"), source=source))
    out = transform(make_ctx("transform"), validated=validated)
    assert out["rows"][0] == {
        "order_id": "o-1", "customer_id": "c-1", "order_date": "2026-09-01",
        "amount_cents": 1050, "currency": "USD",
    }
    assert out["daily_totals"] == [
        {"order_date": "2026-09-01", "currency": "USD", "amount_cents": 1475},
        {"order_date": "2026-09-02", "currency": "EUR", "amount_cents": 10000},
    ]
    # Pure: the same input gives the same output, which is what makes cache=True valid.
    assert transform(make_ctx("transform"), validated=validated) == out


def _transformed(make_ctx, source):
    validated = validate(make_ctx("validate"), extracted=extract(make_ctx("extract"), source=source))
    return transform(make_ctx("transform"), validated=validated)


def _count(db, table):
    with sqlite3.connect(db) as c:
        return c.execute(f"SELECT COUNT(*) FROM {table}").fetchone()[0]


def test_load_is_idempotent_across_retries_of_the_same_attempt(make_ctx, source, tmp_path):
    db = str(tmp_path / "wh.db")
    data = _transformed(make_ctx, source)
    first = load(make_ctx("load", attempt_number=1), transformed=data, target_db=db)
    assert first == {"rows_loaded": 3, "loaded_at": first["loaded_at"], "already_loaded": False}
    # A retry of the same node carries the same idempotency key: the batch is
    # found and nothing is written twice.
    retry = load(make_ctx("load", attempt_number=2), transformed=data, target_db=db)
    assert retry["already_loaded"] is True
    assert _count(db, "orders") == 3
    assert _count(db, "load_batches") == 1


def test_reloading_the_same_file_in_a_new_run_does_not_double_count(make_ctx, source, tmp_path):
    db = str(tmp_path / "wh.db")
    data = _transformed(make_ctx, source)
    load(make_ctx("load", run_id="run-a"), transformed=data, target_db=db)
    load(make_ctx("load", run_id="run-b"), transformed=data, target_db=db)
    assert _count(db, "orders") == 3
    with sqlite3.connect(db) as c:
        total = c.execute(
            "SELECT amount_cents FROM daily_totals WHERE order_date='2026-09-01' AND currency='USD'"
        ).fetchone()[0]
    assert total == 1475


def test_workflow_compiles_to_a_linear_pipeline():
    spec = pipeline.compile().spec
    edges = {(e["from_node_id"], e["to_node_id"]) for e in spec["edges"]}
    assert edges == {("extract", "validate"), ("validate", "transform"), ("transform", "load")}
