"""Tests for the task functions. They run the real handlers, no platform needed."""

from __future__ import annotations

import pytest

from nexus import TaskContext
from dag import MAX_TRIALS, evaluate, load_dataset, make_grid, quality_gate, select_best, train, training


@pytest.fixture(scope="module")
def dataset():
    # Module-scoped (built once), so it cannot use the function-scoped make_ctx.
    ctx = TaskContext(run_id="r", node_id="load_dataset", attempt_id="a", attempt_number=1,
                      task_ref="t", task_version=1, idempotency_key="k")
    return load_dataset(ctx, source="synthetic", rows=600, seed=3)


def test_dataset_split_is_deterministic_per_seed(make_ctx):
    a = load_dataset(make_ctx("load_dataset"), source="synthetic", rows=200, seed=1)
    b = load_dataset(make_ctx("load_dataset"), source="synthetic", rows=200, seed=1)
    c = load_dataset(make_ctx("load_dataset"), source="synthetic", rows=200, seed=2)
    assert a == b and a != c
    assert (len(a["train"]), len(a["test"])) == (160, 40)


def test_dataset_reads_a_csv_with_the_target_last(make_ctx, tmp_path):
    p = tmp_path / "d.csv"
    p.write_text("x1,x2,y\n" + "".join(f"{i},{2 * i},{3 * i + 1}\n" for i in range(50)))
    out = load_dataset(make_ctx("load_dataset"), source=str(p), rows=0, seed=0)
    assert len(out["train"]) + len(out["test"]) == 50
    assert all(len(r) == 3 for r in out["train"])


def test_too_small_a_dataset_is_rejected(make_ctx):
    with pytest.raises(ValueError, match="at least 20 rows"):
        load_dataset(make_ctx("load_dataset"), source="synthetic", rows=10, seed=0)


def test_grid_is_the_cartesian_product_and_is_bounded(make_ctx):
    trials = make_grid(make_ctx("make_grid"), learning_rates=[0.1, 0.2], l2=[0, 0.01], epochs=5)
    assert trials == [
        {"lr": 0.1, "l2": 0.0, "epochs": 5}, {"lr": 0.1, "l2": 0.01, "epochs": 5},
        {"lr": 0.2, "l2": 0.0, "epochs": 5}, {"lr": 0.2, "l2": 0.01, "epochs": 5},
    ]
    with pytest.raises(ValueError, match="MAX_TRIALS"):
        make_grid(make_ctx("make_grid"), learning_rates=list(range(MAX_TRIALS + 1)), l2=[0], epochs=1)
    with pytest.raises(ValueError, match="empty"):
        make_grid(make_ctx("make_grid"), learning_rates=[], l2=[0], epochs=1)


def test_training_learns_the_synthetic_relationship(make_ctx, dataset):
    out = train(make_ctx("train"), hparams={"lr": 0.1, "l2": 0.0, "epochs": 150}, dataset=dataset)
    assert out["val_r2"] > 0.95


def test_the_map_runs_one_trial_per_grid_point(make_ctx, dataset):
    trials = make_grid(make_ctx("make_grid"), learning_rates=[0.05, 0.1], l2=[0.0], epochs=60)
    # Outside a workflow, .map runs the handler per element: the same code path
    # as the platform's children, minus the platform.
    results = train.map(trials, item="hparams", max_fanout=MAX_TRIALS, ctx=make_ctx("train"), dataset=dataset)
    assert [r["hparams"] for r in results] == trials


def test_select_best_ignores_failed_trials(make_ctx):
    results = [None, {"hparams": {"lr": 1}, "val_r2": 0.5}, {"hparams": {"lr": 2}, "val_r2": 0.9}]
    out = select_best(make_ctx("select_best"), results=results)
    assert out["best"]["hparams"] == {"lr": 2}
    assert out["failed_trials"] == 1
    with pytest.raises(ValueError, match="all 2 trials failed"):
        select_best(make_ctx("select_best"), results=[None, None])


def test_quality_gate_fails_below_the_bar(make_ctx, dataset):
    sel = {"best": {"hparams": {"lr": 0.1, "l2": 0.0, "epochs": 150}, "val_r2": 0.97}, "trials": 1, "failed_trials": 0}
    ev = evaluate(make_ctx("evaluate"), selection=sel, dataset=dataset)
    card = quality_gate(make_ctx("quality_gate"), evaluation=ev, min_r2=0.9)
    assert card["passed"] and card["test_r2"] > 0.9
    with pytest.raises(ValueError, match="below min_r2"):
        quality_gate(make_ctx("quality_gate"), evaluation=ev, min_r2=0.9999)


def test_workflow_fans_out_over_the_grid():
    spec = training.compile().spec
    train_node = next(t for t in spec["tasks"] if t["id"] == "train")
    assert train_node["map"]["max_fanout"] == MAX_TRIALS
    assert train_node["outputs"]["result"]["max_bytes"] == 4096
