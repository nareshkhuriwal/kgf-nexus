# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""nexus.entrypoint: a decorated handler run under the executor FILE contract."""

from __future__ import annotations

import json
import os
import subprocess
import sys
import textwrap

import pytest

from nexus import SpecError, ValueSpec, task
from nexus.entrypoint import EXIT_HANDLER_FAILED, EXIT_SETUP_FAILED, run_from_environment
from nexus.task import registry

SDK_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

MODULE = textwrap.dedent(
    """
    from nexus import task

    @task(task_ref="t.add")
    def add(ctx, a, b):
        return {"sum": a + b, "key": ctx.idempotency_key, "run": ctx.run_id}

    @task(task_ref="t.split", outputs={"evens": "json", "odds": "json"})
    def split(ctx, xs):
        return {"evens": [x for x in xs if x % 2 == 0], "odds": [x for x in xs if x % 2]}

    @task(task_ref="t.big", outputs={"result": {"type": "json", "max_bytes": 16}})
    def big(ctx):
        return "x" * 100

    @task(task_ref="t.boom")
    def boom(ctx):
        raise RuntimeError("kaboom")

    @task(task_ref="t.text")
    def text(ctx, name):
        return "hello " + name
    """
)


@pytest.fixture(autouse=True)
def _isolate_registry():
    registry.clear()
    yield
    registry.clear()


@pytest.fixture
def workspace(tmp_path):
    (tmp_path / "handlers.py").write_text(MODULE)
    (tmp_path / "in").mkdir()
    (tmp_path / "out").mkdir()
    return tmp_path


def _env(ws, **extra):
    env = {
        "NEXUS_INPUT_DIR": str(ws / "in"),
        "NEXUS_OUTPUT_DIR": str(ws / "out"),
        "NEXUS_RUN_ID": "run-1",
        "NEXUS_NODE_ID": "add",
        "NEXUS_ATTEMPT_ID": "att-1",
        "NEXUS_IDEMPOTENCY_KEY": "idem-123",
    }
    env.update(extra)
    return env


def test_inputs_are_decoded_and_the_output_written(workspace) -> None:
    (workspace / "in" / "a").write_text("2")
    (workspace / "in" / "b").write_text("40")
    (workspace / "in" / "ignored").write_text("1")  # not a handler parameter
    code = run_from_environment("t.add", [str(workspace / "handlers.py")], _env(workspace))
    assert code == 0
    out = json.loads((workspace / "out" / "result").read_text())
    # The platform's key and run id reach the handler through the context.
    assert out == {"sum": 42, "key": "idem-123", "run": "run-1"}


def test_several_outputs_are_split_by_name(workspace) -> None:
    (workspace / "in" / "xs").write_text("[1,2,3,4]")
    assert run_from_environment("t.split", [str(workspace / "handlers.py")], _env(workspace)) == 0
    assert json.loads((workspace / "out" / "evens").read_text()) == [2, 4]
    assert json.loads((workspace / "out" / "odds").read_text()) == [1, 3]


def test_non_json_input_arrives_as_text(workspace) -> None:
    (workspace / "in" / "name").write_text("world")
    assert run_from_environment("t.text", [str(workspace / "handlers.py")], _env(workspace)) == 0
    assert json.loads((workspace / "out" / "result").read_text()) == "hello world"


def test_declared_max_bytes_is_enforced(workspace, capsys) -> None:
    code = run_from_environment("t.big", [str(workspace / "handlers.py")], _env(workspace))
    assert code == EXIT_HANDLER_FAILED
    assert "over its declared max_bytes=16" in capsys.readouterr().err
    # Nothing is written: the executor must not collect an oversized output.
    assert os.listdir(workspace / "out") == []


def test_a_raising_handler_fails_with_its_traceback(workspace, capsys) -> None:
    assert run_from_environment("t.boom", [str(workspace / "handlers.py")], _env(workspace)) == EXIT_HANDLER_FAILED
    assert "RuntimeError: kaboom" in capsys.readouterr().err


def test_setup_failures_are_distinguished(workspace, capsys) -> None:
    mod = [str(workspace / "handlers.py")]
    assert run_from_environment("t.nope", mod, _env(workspace)) == EXIT_SETUP_FAILED
    assert "no handler registered" in capsys.readouterr().err
    registry.clear()
    env = _env(workspace)
    del env["NEXUS_OUTPUT_DIR"]
    assert run_from_environment("t.add", mod, env) == EXIT_SETUP_FAILED
    assert "NEXUS_OUTPUT_DIR" in capsys.readouterr().err


def test_runs_as_a_module_the_way_a_container_does(workspace) -> None:
    (workspace / "in" / "a").write_text("1")
    (workspace / "in" / "b").write_text("2")
    env = {**os.environ, **_env(workspace), "PYTHONPATH": SDK_ROOT}
    proc = subprocess.run(
        [sys.executable, "-m", "nexus.entrypoint", "--module", "handlers", "t.add"],
        cwd=workspace, env=env, capture_output=True, text=True, timeout=60,
    )
    assert proc.returncode == 0, proc.stderr
    assert json.loads((workspace / "out" / "result").read_text())["sum"] == 3


# ── ValueSpec.max_bytes ─────────────────────────────────────────────────────


def test_max_bytes_reaches_the_wire() -> None:
    @task(task_ref="mb.one", outputs={"result": {"type": "json", "required": True, "max_bytes": 512}})
    def one(ctx):
        return 1

    spec = one.to_task_spec("one", {})
    assert spec["outputs"]["result"] == {"type": "json", "required": True, "max_bytes": 512}
    assert ValueSpec(type="json").to_wire() == {"type": "json", "required": True}


@pytest.mark.parametrize("bad", [-1, 1.5, True, "10"])
def test_max_bytes_must_be_a_non_negative_int(bad) -> None:
    with pytest.raises(SpecError, match="max_bytes"):
        ValueSpec(type="json", max_bytes=bad).to_wire()
