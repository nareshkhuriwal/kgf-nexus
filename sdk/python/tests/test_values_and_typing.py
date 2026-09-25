# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""max_bytes, stdout_output, the configured inline threshold, source maps and
the typed usage example.

The spec fields are asserted against the wire shape the gateway's schema
accepts (schemas/workflow-spec-v1.json: ValueSpec.max_bytes,
RuntimeSpec.stdout_output), so the SDK cannot emit a field the server rejects.
"""

from __future__ import annotations

import importlib.util
import json
import os
import pathlib

import pytest

from nexus import (
    FileResultStore,
    InMemoryResultStore,
    Runtime,
    SpecError,
    TaskContext,
    ValueSpec,
    task,
    workflow,
)
from nexus.context import (
    DEFAULT_INLINE_THRESHOLD_BYTES,
    MAX_INLINE_THRESHOLD_BYTES,
    resolve_inline_threshold,
)
from nexus.task import registry
from nexus.worker import build_context

HERE = pathlib.Path(__file__).resolve().parent
SCHEMA = HERE.parents[2] / "schemas" / "workflow-spec-v1.json"


@pytest.fixture(autouse=True)
def _isolate_registry():
    registry.clear()
    yield
    registry.clear()


def _task_spec(compiled, node_id):
    return next(t for t in compiled.spec["tasks"] if t["id"] == node_id)


# ── max_bytes ────────────────────────────────────────────────────────────────


def test_max_bytes_is_emitted_on_declared_values():
    @task(
        task_ref="mb.a",
        inputs={"x": {"type": "json", "max_bytes": "2Ki"}},
        outputs={"out": ValueSpec(type="json", max_bytes=128)},
    )
    def a(ctx, x):
        return x

    @workflow(name="mb")
    def wf(x):
        a(x)

    spec = _task_spec(wf.compile(), "a")
    assert spec["inputs"]["x"]["max_bytes"] == 2048
    assert spec["outputs"]["out"] == {"type": "json", "required": True, "max_bytes": 128}


def test_max_bytes_absent_keeps_the_content_address():
    """An author who never mentions max_bytes compiles the same bytes as before."""

    @task(task_ref="mb.plain", outputs={"out": {"type": "json"}})
    def plain(ctx):
        return 1

    @workflow(name="mbp")
    def wf():
        plain()

    assert "max_bytes" not in wf.compile().json


@pytest.mark.parametrize("bad", [0, -1, True, 1.5])
def test_max_bytes_must_be_a_positive_integer(bad):
    with pytest.raises(SpecError, match="max_bytes"):

        @task(task_ref=f"mb.bad{bad!r}", outputs={"out": {"type": "json", "max_bytes": bad}})
        def bad_task(ctx):
            return None


def test_unknown_value_option_is_rejected():
    with pytest.raises(SpecError, match="unknown value option"):

        @task(task_ref="mb.typo", outputs={"out": {"type": "json", "max_byte": 10}})
        def typo(ctx):
            return None


def test_fan_in_over_the_platform_ceiling_fails_at_compile():
    @task(task_ref="fi.list", outputs={"ids": {"type": "json"}})
    def list_ids(ctx):
        return []

    @task(task_ref="fi.child", outputs={"r": {"type": "json", "max_bytes": 1 << 20}})
    def child(ctx, item):
        return item

    @task(task_ref="fi.join")
    def join(ctx, rs):
        return rs

    @workflow(name="fi")
    def wf():
        join(rs=child.map(list_ids()["ids"], item="item", max_fanout=1000)["r"])

    with pytest.raises(SpecError, match="fan-in"):
        wf.compile()


# ── stdout_output ────────────────────────────────────────────────────────────


def test_stdout_output_is_emitted_when_declared():
    @task(
        task_ref="so.a",
        outputs={"report": {"type": "string"}},
        runtime=Runtime(executor="subprocess", command=["sh", "-c"], args=["echo hi"], stdout_output="report"),
    )
    def a(ctx):
        return None

    @workflow(name="so")
    def wf():
        a()

    assert _task_spec(wf.compile(), "a")["runtime"]["stdout_output"] == "report"


def test_stdout_output_may_name_the_implicit_result():
    @task(task_ref="so.r", runtime={"command": ["echo", "x"], "stdout_output": "result"})
    def r(ctx):
        return None

    assert r.declaration.runtime is not None


def test_stdout_output_must_name_a_declared_output():
    with pytest.raises(SpecError, match="not a declared output"):

        @task(
            task_ref="so.bad",
            outputs={"report": {"type": "string"}},
            runtime=Runtime(command=["echo"], stdout_output="reprot"),
        )
        def bad(ctx):
            return None


def test_stdout_output_is_refused_on_kubernetes():
    with pytest.raises(SpecError, match="kubernetes"):

        @task(
            task_ref="so.k8s",
            runtime=Runtime(executor="kubernetes", image="img@sha256:0", stdout_output="result"),
        )
        def k8s(ctx):
            return None


def test_emitted_fields_are_in_the_schema():
    """The fields this module emits exist in the schema the gateway enforces."""
    schema = json.loads(SCHEMA.read_text())
    defs = schema["$defs"]
    assert "max_bytes" in defs["ValueSpec"]["properties"]
    assert "stdout_output" in defs["RuntimeSpec"]["properties"]


# ── inline threshold (ADR-0016) ──────────────────────────────────────────────


def test_inline_threshold_comes_from_the_environment():
    assert resolve_inline_threshold(environ={}) == DEFAULT_INLINE_THRESHOLD_BYTES
    assert resolve_inline_threshold(environ={"NEXUS_INLINE_THRESHOLD_BYTES": "16"}) == 16


def test_inline_threshold_assignment_beats_environment():
    got = resolve_inline_threshold(
        {"result_store": {"inline_threshold_bytes": 32}}, environ={"NEXUS_INLINE_THRESHOLD_BYTES": "16"}
    )
    assert got == 32


@pytest.mark.parametrize(
    "raw,want",
    [("0", DEFAULT_INLINE_THRESHOLD_BYTES), ("-5", DEFAULT_INLINE_THRESHOLD_BYTES),
     (str(50 << 20), MAX_INLINE_THRESHOLD_BYTES), ("junk", DEFAULT_INLINE_THRESHOLD_BYTES)],
)
def test_inline_threshold_is_clamped_like_the_agent(raw, want):
    assert resolve_inline_threshold(environ={"NEXUS_INLINE_THRESHOLD_BYTES": raw}) == want


def test_a_lowered_threshold_sends_a_small_result_to_a_blob(tmp_path, monkeypatch):
    """The mutation this guards against: reverting to the 64 KiB constant."""
    value = {"payload": "x" * 40}
    store = FileResultStore(str(tmp_path), inline_max_bytes=16)
    ref = store.put(value)
    assert "blob" in ref and store.get(ref) == value

    monkeypatch.setenv("NEXUS_INLINE_THRESHOLD_BYTES", "16")
    assert "blob" in InMemoryResultStore().put(value)
    ctx = build_context({"run_id": "r", "node_id": "n", "spec": {"task_ref": "t"}})
    assert "blob" in ctx.store.put(value)


# ── source maps (.py validation line mapping) ────────────────────────────────


def test_compiled_workflow_maps_problem_paths_to_python_lines():
    @task(task_ref="sm.a")
    def a(ctx, x):
        return x

    @task(task_ref="sm.b")
    def b(ctx, y):
        return y

    @workflow(name="sm")
    def wf(x):
        first = a(x)  # line marker: call-a
        b(first)

    compiled = wf.compile()
    here = os.path.abspath(__file__)
    src = pathlib.Path(here).read_text().splitlines()
    a_def = next(i for i, l in enumerate(src, 1) if l.strip() == '@task(task_ref="sm.a")')
    call_a = next(i for i, l in enumerate(src, 1) if "line marker: call-a" in l)

    loc = compiled.locate("/tasks/a/resources/request")
    assert loc is not None and os.path.abspath(loc.file) == here and loc.line == a_def
    # Schema paths carry the task's INDEX in the canonical (id-sorted) list.
    assert compiled.locate("/tasks/0/queue") == loc
    # Wiring problems point at the call in the workflow body.
    wiring = compiled.locate("/tasks/a/input_params")
    assert wiring is not None and wiring.line == call_a
    assert compiled.locate("/name") is None
    assert compiled.locate("/tasks/nope/queue") is None
    # Beside the document, never in it: the hash ignores line numbers.
    assert "line" not in compiled.json
    assert set(compiled.source_map_json()) == {"a", "b"}


# ── the typed usage example also runs ────────────────────────────────────────


def test_typed_usage_example_compiles_and_runs():
    path = HERE.parent / "typecheck" / "typed_usage.py"
    spec = importlib.util.spec_from_file_location("typed_usage", path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    compiled = module.compiled
    assert {t["id"] for t in compiled.spec["tasks"]} == {"extract", "list_ids", "score", "total", "shell"}
    score = next(t for t in compiled.spec["tasks"] if t["id"] == "score")
    assert score["map"]["max_fanout"] == 100


def test_direct_call_with_context_runs_the_handler():
    @task(task_ref="dc.a")
    def a(ctx: TaskContext, n: int) -> int:
        return n + 1

    ctx = TaskContext(run_id="r", node_id="n", attempt_id="x", attempt_number=1,
                      task_ref="dc.a", task_version=1, idempotency_key="k")
    assert a(ctx, 1) == 2
    assert a.map([1, 2], ctx, item="n", max_fanout=2) == [2, 3]
    with pytest.raises(SpecError, match="concrete list"):
        a.map("12", ctx, item="n", max_fanout=2)
