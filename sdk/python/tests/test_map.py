# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Dynamic expansion (§5.4) in the SDK: `.map(...)` compiles to a MapSpec."""

from __future__ import annotations

import pytest

from nexus import SpecError, TaskContext, task, workflow
from nexus.task import registry


@pytest.fixture(autouse=True)
def _clean_registry():
    registry.clear()
    yield
    registry.clear()


def _tasks():
    @task(task_ref="t.list", outputs={"items": {"type": "json"}})
    def lister(ctx: TaskContext) -> list:
        return [1, 2, 3]

    @task(task_ref="t.proc")
    def proc(ctx: TaskContext, item: int, scale: int) -> int:
        return item * scale

    @task(task_ref="t.sum", trigger_rule="all_done")
    def total(ctx: TaskContext, values: list) -> int:
        return sum(v for v in values if v is not None)

    return lister, proc, total


def test_map_compiles_to_template_with_edge_and_item_input():
    lister, proc, total = _tasks()

    @workflow(name="mapped")
    def wf():
        xs = lister()
        ys = proc.map(xs["items"], item="item", max_fanout=25, scale=3)
        return total(ys)

    spec = wf.compile().spec
    by_id = {t["id"]: t for t in spec["tasks"]}
    assert by_id["proc"]["map"] == {
        "item_input": "item",
        "max_fanout": 25,
        "over_node_id": "lister",
        "over_output": "items",
    }
    # The item input is declared (the server requires it) but bound by no edge.
    assert "item" in by_id["proc"]["inputs"]
    assert by_id["proc"]["input_literals"] == {"scale": "3"}
    edges = {(e["from_node_id"], e["to_node_id"]): e["output_to_input"] for e in spec["edges"]}
    assert edges[("lister", "proc")] == {}
    assert edges[("proc", "total")] == {"values": "result"}


def test_map_requires_a_positive_max_fanout():
    lister, proc, _ = _tasks()

    for bad in (0, -1, None, True, "10"):

        @workflow(name="bad")
        def wf():
            proc.map(lister()["items"], item="item", max_fanout=bad, scale=1)

        with pytest.raises(SpecError, match="max_fanout"):
            wf.compile()


def test_map_rejects_unknown_item_and_double_binding():
    lister, proc, _ = _tasks()

    @workflow(name="bad_item")
    def wf1():
        proc.map(lister()["items"], item="nope", max_fanout=5, scale=1)

    with pytest.raises(SpecError, match="not a handler parameter"):
        wf1.compile()

    # Binding the item positionally as a shared arg is impossible: positional
    # args skip the item parameter, so a stray extra one is rejected.
    @workflow(name="too_many")
    def wf2():
        proc.map(lister()["items"], 1, 2, item="item", max_fanout=5)

    with pytest.raises(SpecError, match="positional"):
        wf2.compile()


def test_map_over_a_literal_is_rejected():
    _, proc, _ = _tasks()

    @workflow(name="lit")
    def wf():
        proc.map([1, 2], item="item", max_fanout=5, scale=1)

    with pytest.raises(SpecError, match="upstream task output"):
        wf.compile()


def test_map_outside_a_workflow_runs_per_element():
    _, proc, _ = _tasks()
    assert proc.map([1, 2, 3], item="item", max_fanout=5, ctx=None, scale=2) == [2, 4, 6]
    with pytest.raises(SpecError, match="max_fanout"):
        proc.map([1, 2, 3], item="item", max_fanout=2, ctx=None, scale=2)


def test_example_compiles_to_the_committed_document():
    import importlib.util
    import pathlib

    import yaml

    root = pathlib.Path(__file__).resolve().parents[3]
    spec_path = root / "examples" / "fan_out_map" / "dag.py"
    mod_spec = importlib.util.spec_from_file_location("fan_out_map_dag", spec_path)
    mod = importlib.util.module_from_spec(mod_spec)
    mod_spec.loader.exec_module(mod)
    committed = yaml.safe_load((root / "examples" / "fan_out_map" / "compiled.yaml").read_text())
    assert mod.fan_out_map.compile().spec == committed
