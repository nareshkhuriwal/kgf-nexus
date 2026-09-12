"""The worker-side entrypoint: executing a registered handler by task_ref.

This runs inside the task container. The assertions that matter are that a
handler failure comes back as a REPORTABLE OUTCOME rather than an exception (the
agent needs the outcome to report it), and that error classification is
conservative -- a wrongly-retryable classification wastes cluster capacity.
"""

from __future__ import annotations

import json

import pytest

from nexus import task
from nexus.context import InMemoryResultStore, derive_idempotency_key
from nexus.task import registry
from nexus.worker import build_context, execute_assignment, main


@pytest.fixture(autouse=True)
def _isolate_registry():
    registry.clear()
    yield
    registry.clear()


def _assignment(task_ref: str, **overrides) -> dict:
    a = {
        "attempt_id": "att-1",
        "run_id": "run-1",
        "node_id": "node-1",
        "attempt_number": 1,
        "spec": {"task_ref": task_ref, "version": 1, "idempotency": "IDEMPOTENCY_MODE_REQUIRED"},
        "inputs": {},
        "idempotency_key": derive_idempotency_key("run-1", "node-1", None, 1),
        "lease_token": "tok-1",
    }
    a.update(overrides)
    return a


def test_successful_handler_reports_succeeded_with_an_output_ref() -> None:
    @task(task_ref="w.ok")
    def ok(ctx, n):
        return {"doubled": n * 2}

    store = InMemoryResultStore()
    out = execute_assignment(
        _assignment("w.ok", input_literals={"n": "21"}), store=store
    )
    assert out["outcome"] == "ATTEMPT_OUTCOME_SUCCEEDED"
    assert store.get(out["output_ref"]) == {"doubled": 42}
    assert out["metrics"]["exit_code"] == 0


def test_handler_receives_the_platform_derived_key() -> None:
    seen: list[str] = []

    @task(task_ref="w.key")
    def k(ctx):
        seen.append(ctx.idempotency_key)
        return None

    a = _assignment("w.key")
    execute_assignment(a, store=InMemoryResultStore())
    assert seen == [a["idempotency_key"]]


def test_control_plane_key_wins_over_local_derivation(capsys) -> None:
    """The control plane's key is authoritative; a mismatch is loud but not fatal.

    A version skew between agent and control plane is a legitimate reason for a
    difference (N-1 agent compatibility), but a silent difference would break dedup
    in exactly the situation it exists for.
    """
    seen: list[str] = []

    @task(task_ref="w.skew")
    def skew(ctx):
        seen.append(ctx.idempotency_key)
        return None

    execute_assignment(
        _assignment("w.skew", idempotency_key="f" * 64), store=InMemoryResultStore()
    )
    assert seen == ["f" * 64]
    assert "differs from locally derived" in capsys.readouterr().err


def test_handler_exception_becomes_a_reportable_failure_not_a_raise() -> None:
    @task(task_ref="w.boom")
    def boom(ctx):
        raise RuntimeError("downstream exploded")

    out = execute_assignment(_assignment("w.boom"), store=InMemoryResultStore())
    assert out["outcome"] == "ATTEMPT_OUTCOME_FAILED"
    assert out["error"]["type"] == "RuntimeError"
    assert "downstream exploded" in out["error"]["message"]
    # An unrecognized exception is APPLICATION: terminal unless the author
    # explicitly listed it in retry_on. Guessing "retryable" would burn capacity
    # re-running deterministic bugs.
    assert out["error"]["class"] == "ERROR_CLASS_APPLICATION"
    assert out["error"]["retryable"] is False


@pytest.mark.parametrize(
    "exc,expected_class,retryable",
    [
        (TimeoutError("slow"), "ERROR_CLASS_TIMEOUT", True),
        (ConnectionError("reset"), "ERROR_CLASS_TRANSIENT", True),
        (PermissionError("nope"), "ERROR_CLASS_PERMISSION_DENIED", False),
        (FileNotFoundError("missing"), "ERROR_CLASS_NOT_FOUND", False),
        (ValueError("bad input"), "ERROR_CLASS_VALIDATION", False),
        (MemoryError(), "ERROR_CLASS_OOM", True),
    ],
)
def test_error_classification(exc, expected_class, retryable) -> None:
    @task(task_ref="w.classify")
    def classify(ctx):
        raise exc

    out = execute_assignment(_assignment("w.classify"), store=InMemoryResultStore())
    assert out["error"]["class"] == expected_class
    assert out["error"]["retryable"] is retryable


def test_traceback_goes_to_the_result_store_not_the_outcome() -> None:
    """Unbounded text in the metadata store is how a metadata store dies."""

    @task(task_ref="w.trace")
    def trace(ctx):
        raise RuntimeError("x" * 5000)

    store = InMemoryResultStore()
    out = execute_assignment(_assignment("w.trace"), store=store)
    assert len(out["error"]["message"]) <= 2000
    assert out["error"]["detail_ref"]
    assert "RuntimeError" in store.get(out["error"]["detail_ref"])["traceback"]


def test_missing_handler_is_terminal_not_found() -> None:
    out = execute_assignment(_assignment("w.absent"), store=InMemoryResultStore())
    assert out["error"]["class"] == "ERROR_CLASS_NOT_FOUND"
    # Retrying cannot make a missing handler appear; burning attempts on it only
    # delays the real error reaching a human.
    assert out["error"]["retryable"] is False


def test_inputs_the_handler_does_not_accept_are_dropped_not_passed() -> None:
    """Adding an input to a spec must not TypeError a not-yet-redeployed handler."""

    @task(task_ref="w.narrow")
    def narrow(ctx, wanted):
        return wanted

    out = execute_assignment(
        _assignment("w.narrow", input_literals={"wanted": '"a"', "unwanted": '"b"'}),
        store=InMemoryResultStore(),
    )
    assert out["outcome"] == "ATTEMPT_OUTCOME_SUCCEEDED"


def test_blob_inputs_are_materialized_from_the_result_store() -> None:
    """Payloads never transit the control plane (invariant 6)."""
    store = InMemoryResultStore()
    ref = store.put({"rows": list(range(20_000))})
    assert "blob" in ref

    @task(task_ref="w.blob")
    def consume(ctx, dataset):
        return {"count": len(dataset["rows"])}

    out = execute_assignment(
        _assignment("w.blob", inputs={"dataset": ref}), store=store
    )
    assert store.get(out["output_ref"]) == {"count": 20_000}


def test_none_return_produces_no_output_ref() -> None:
    @task(task_ref="w.void")
    def void(ctx):
        return None

    out = execute_assignment(_assignment("w.void"), store=InMemoryResultStore())
    assert out["outcome"] == "ATTEMPT_OUTCOME_SUCCEEDED"
    assert "output_ref" not in out


def test_build_context_maps_the_idempotency_mode() -> None:
    ctx = build_context(
        _assignment("w.mode", spec={"task_ref": "w.mode", "version": 3, "idempotency": "IDEMPOTENCY_MODE_NONE"})
    )
    assert ctx.idempotency_mode == "none"
    assert ctx.task_version == 3


def test_cli_entrypoint_reads_an_assignment_and_writes_an_outcome(tmp_path, capsys) -> None:
    module = tmp_path / "handlers.py"
    module.write_text(
        "from nexus import task\n"
        "@task(task_ref='cli.echo')\n"
        "def echo(ctx, msg):\n"
        "    ctx.heartbeat('working')\n"
        "    return {'echoed': msg}\n"
    )
    assignment = tmp_path / "assignment.json"
    assignment.write_text(json.dumps(_assignment("cli.echo", input_literals={"msg": '"hi"'})))

    rc = main(
        [
            "--assignment",
            str(assignment),
            "--module",
            str(module),
            "--result-dir",
            str(tmp_path / "results"),
        ]
    )
    assert rc == 0
    outcome = json.loads(capsys.readouterr().out)
    assert outcome["outcome"] == "ATTEMPT_OUTCOME_SUCCEEDED"


def test_cli_rejects_an_unreadable_assignment(tmp_path, capsys) -> None:
    # Exit non-zero means "could not produce an outcome at all", which the agent
    # treats as an executor/infrastructure failure rather than a task failure.
    assert main(["--assignment", str(tmp_path / "nope.json")]) == 2
    assert "cannot read assignment" in capsys.readouterr().err
