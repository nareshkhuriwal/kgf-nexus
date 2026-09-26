# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""nexus.tracing: joining the platform's W3C trace from a task process.

The worker agent exports TRACEPARENT/TRACESTATE to every workload. What matters:
a malformed value never fails a task (it is ignored); a valid one is exposed on
TaskContext; with OpenTelemetry installed the handler runs in a CHILD span of
the agent's attempt span (same trace id, parent = the traceparent's span id),
with failures recorded; without OpenTelemetry the handler runs unchanged.
"""

from __future__ import annotations

import logging
import sys
from typing import Any, Iterator

import pytest

from nexus import TaskContext, task
from nexus import local
from nexus.context import InMemoryResultStore, derive_idempotency_key
from nexus.entrypoint import EXIT_HANDLER_FAILED, run_from_environment
from nexus.task import registry
from nexus.tracing import (
    TraceContext,
    otel_available,
    parse_traceparent,
    propagation_headers,
    task_span,
    trace_context_from_environment,
)
from nexus.worker import build_context, execute_assignment

TRACE_ID = "4bf92f3577b34da6a3ce929d0e0e4736"
SPAN_ID = "00f067aa0ba902b7"
TP = f"00-{TRACE_ID}-{SPAN_ID}-01"


@pytest.fixture(autouse=True)
def _isolate_registry() -> Iterator[None]:
    registry.clear()
    yield
    registry.clear()


def _assignment(task_ref: str) -> dict[str, Any]:
    return {
        "attempt_id": "att-1",
        "run_id": "run-1",
        "node_id": "node-1",
        "attempt_number": 1,
        "spec": {"task_ref": task_ref, "version": 1, "idempotency": "IDEMPOTENCY_MODE_REQUIRED"},
        "inputs": {},
        "idempotency_key": derive_idempotency_key("run-1", "node-1", None, 1),
        "lease_token": "tok-1",
    }


def _hide_otel(monkeypatch: pytest.MonkeyPatch) -> None:
    """Make every opentelemetry import fail, as if the extra were not installed."""
    for name in [m for m in sys.modules if m == "opentelemetry" or m.startswith("opentelemetry.")]:
        monkeypatch.setitem(sys.modules, name, None)
    monkeypatch.setitem(sys.modules, "opentelemetry", None)


# ── parsing and validation ───────────────────────────────────────────────────


def test_a_valid_traceparent_parses() -> None:
    tc = parse_traceparent(TP, "vendor=abc")
    assert tc == TraceContext(TRACE_ID, SPAN_ID, "01", "vendor=abc")
    assert tc.traceparent == TP and tc.sampled
    assert tc.carrier() == {"traceparent": TP, "tracestate": "vendor=abc"}
    assert parse_traceparent(f"00-{TRACE_ID}-{SPAN_ID}-00").sampled is False  # type: ignore[union-attr]


def test_surrounding_whitespace_is_tolerated() -> None:
    assert parse_traceparent(f"  {TP}\n") is not None


@pytest.mark.parametrize(
    "bad",
    [
        "",
        "   ",
        "garbage",
        f"01-{TRACE_ID}-{SPAN_ID}-01",  # unknown version
        f"ff-{TRACE_ID}-{SPAN_ID}-01",  # forbidden version
        f"00-{'0' * 32}-{SPAN_ID}-01",  # all-zero trace id
        f"00-{TRACE_ID}-{'0' * 16}-01",  # all-zero span id
        f"00-{TRACE_ID.upper()}-{SPAN_ID}-01",  # uppercase hex
        f"00-{TRACE_ID[:-1]}-{SPAN_ID}-01",  # short trace id
        f"00-{TRACE_ID}-{SPAN_ID}0-01",  # long span id
        f"00-{TRACE_ID}-{SPAN_ID}-1",  # short flags
        f"00-{TRACE_ID}-{SPAN_ID}-0g",  # non-hex flags
        f"00-{TRACE_ID}-{SPAN_ID}-01-extra",  # trailing field
        f"00_{TRACE_ID}_{SPAN_ID}_01",  # wrong separator
    ],
)
def test_an_invalid_traceparent_is_ignored_with_a_debug_log(bad: str, caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.DEBUG, logger="nexus.tracing")
    assert parse_traceparent(bad) is None
    if bad.strip():
        assert any(r.name == "nexus.tracing" and r.levelno == logging.DEBUG for r in caplog.records)


def test_none_is_absent() -> None:
    assert parse_traceparent(None) is None


def test_an_invalid_tracestate_is_dropped_but_the_traceparent_kept() -> None:
    assert parse_traceparent(TP, "a=b\x00").tracestate is None  # type: ignore[union-attr]
    assert parse_traceparent(TP, "x" * 600).tracestate is None  # type: ignore[union-attr]
    assert parse_traceparent(TP, "  ").tracestate is None  # type: ignore[union-attr]


def test_trace_context_from_environment() -> None:
    assert trace_context_from_environment({}) is None
    assert trace_context_from_environment({"TRACEPARENT": "nope"}) is None
    tc = trace_context_from_environment({"TRACEPARENT": TP, "TRACESTATE": "k=v"})
    assert tc is not None and tc.trace_id == TRACE_ID and tc.tracestate == "k=v"


# ── exposure on TaskContext ──────────────────────────────────────────────────


def test_build_context_exposes_the_trace_context() -> None:
    ctx = build_context(_assignment("t.x"), store=InMemoryResultStore(), environ={"TRACEPARENT": TP})
    assert ctx.traceparent == TP
    assert ctx.trace_id == TRACE_ID
    assert ctx.tracestate is None
    assert ctx.trace_context is not None and ctx.trace_context.span_id == SPAN_ID


def test_trace_properties_are_none_without_a_context() -> None:
    for env in ({}, {"TRACEPARENT": "00-bad"}):
        ctx = build_context(_assignment("t.x"), store=InMemoryResultStore(), environ=env)
        assert ctx.traceparent is None and ctx.trace_id is None and ctx.trace_context is None
        assert ctx.trace_headers() == {}


def test_trace_properties_are_read_only() -> None:
    ctx = build_context(_assignment("t.x"), store=InMemoryResultStore(), environ={"TRACEPARENT": TP})
    with pytest.raises(AttributeError):
        ctx.traceparent = "x"  # type: ignore[misc]
    with pytest.raises(AttributeError):
        ctx.trace_id = "x"  # type: ignore[misc]


def test_the_worker_reads_traceparent_from_the_process_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("TRACEPARENT", TP)
    seen: list[str | None] = []

    @task(task_ref="t.env")
    def h(ctx: TaskContext) -> None:
        seen.append(ctx.trace_id)

    out = execute_assignment(_assignment("t.env"), store=InMemoryResultStore())
    assert out["outcome"] == "ATTEMPT_OUTCOME_SUCCEEDED"
    assert seen == [TRACE_ID]


def test_the_container_entrypoint_reads_traceparent(tmp_path: Any) -> None:
    mod = tmp_path / "handlers.py"
    mod.write_text(
        "from nexus import task\n"
        "@task(task_ref='t.ep')\n"
        "def ep(ctx):\n"
        "    return ctx.trace_id\n"
    )
    out_dir = tmp_path / "out"
    env = {
        "NEXUS_RUN_ID": "r",
        "NEXUS_ATTEMPT_ID": "a",
        "NEXUS_NODE_ID": "n",
        "NEXUS_INPUT_DIR": str(tmp_path / "in"),
        "NEXUS_OUTPUT_DIR": str(out_dir),
        "NEXUS_OUTPUT_NAMES": "result",
        "TRACEPARENT": TP,
    }
    (tmp_path / "in").mkdir()
    assert run_from_environment("t.ep", [str(mod)], env) == 0
    assert TRACE_ID in (out_dir / "result").read_text()


# ── without OpenTelemetry ────────────────────────────────────────────────────


def test_without_otel_the_handler_still_runs(monkeypatch: pytest.MonkeyPatch) -> None:
    _hide_otel(monkeypatch)
    assert otel_available() is False
    monkeypatch.setenv("TRACEPARENT", TP)
    ran: list[bool] = []

    @task(task_ref="t.nootel")
    def h(ctx: TaskContext) -> int:
        ran.append(True)
        assert ctx.trace_headers() == {"traceparent": TP}
        return 1

    out = execute_assignment(_assignment("t.nootel"), store=InMemoryResultStore())
    assert out["outcome"] == "ATTEMPT_OUTCOME_SUCCEEDED" and ran == [True]


def test_without_otel_task_span_yields_none_and_failures_propagate(monkeypatch: pytest.MonkeyPatch) -> None:
    _hide_otel(monkeypatch)
    ctx = build_context(_assignment("t.x"), store=InMemoryResultStore(), environ={"TRACEPARENT": TP})
    with task_span(ctx) as span:
        assert span is None
    with pytest.raises(ValueError, match="boom"):
        with task_span(ctx):
            raise ValueError("boom")
    assert propagation_headers(ctx) == {"traceparent": TP}


# ── local runner ─────────────────────────────────────────────────────────────


def test_local_runner_hands_a_valid_traceparent_to_tasks(tmp_path: Any, monkeypatch: pytest.MonkeyPatch) -> None:
    from nexus import Runtime, workflow

    monkeypatch.setenv("TRACEPARENT", TP)
    monkeypatch.setenv("TRACESTATE", "k=v")

    @task(task_ref="lt.sh", outputs={"result": {"type": "string"}},
          runtime=Runtime(executor="subprocess", command=["sh", "-c"],
                          args=['printf "%s|%s" "$TRACEPARENT" "$TRACESTATE" > "$NEXUS_OUTPUT_FILE"']))
    def sh(ctx: TaskContext) -> None:
        return None

    @task(task_ref="lt.py")
    def py(ctx: TaskContext) -> str | None:
        return ctx.trace_id

    @workflow(name="lt-trace")
    def wf() -> None:
        sh()
        py()

    res = local.run(wf, {}, run_dir=str(tmp_path), isolation="inprocess", out=None)
    assert res.state == "SUCCEEDED", res.summary()
    assert res.output("sh") == f"{TP}|k=v"
    assert res.output("py") == TRACE_ID


def test_local_runner_does_not_pass_on_an_invalid_traceparent(tmp_path: Any, monkeypatch: pytest.MonkeyPatch) -> None:
    from nexus import Runtime, workflow

    monkeypatch.setenv("TRACEPARENT", "00-not-valid")
    monkeypatch.setenv("TRACESTATE", "k=v")

    @task(task_ref="lt.sh2", outputs={"result": {"type": "string"}},
          runtime=Runtime(executor="subprocess", command=["sh", "-c"],
                          args=['printf "[%s|%s]" "${TRACEPARENT-unset}" "${TRACESTATE-unset}" > "$NEXUS_OUTPUT_FILE"']))
    def sh2(ctx: TaskContext) -> None:
        return None

    @workflow(name="lt-trace-bad")
    def wf() -> None:
        sh2()

    res = local.run(wf, {}, run_dir=str(tmp_path), out=None)
    assert res.state == "SUCCEEDED", res.summary()
    assert res.output("sh2") == "[unset|unset]"


# ── with OpenTelemetry ───────────────────────────────────────────────────────


@pytest.fixture(scope="module")
def exporter() -> Any:
    pytest.importorskip("opentelemetry.sdk")
    from opentelemetry import trace
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

    exp = InMemorySpanExporter()
    provider = trace.get_tracer_provider()
    if not isinstance(provider, TracerProvider):
        # The global provider can be set once per process; set it only if no
        # SDK provider is installed, otherwise attach to the one that is.
        provider = TracerProvider()
        trace.set_tracer_provider(provider)
        provider = trace.get_tracer_provider()
    assert isinstance(provider, TracerProvider)
    provider.add_span_processor(SimpleSpanProcessor(exp))
    return exp


@pytest.fixture
def spans(exporter: Any) -> Any:
    exporter.clear()
    yield exporter
    exporter.clear()


def test_the_handler_span_is_a_child_of_the_platform_span(spans: Any, monkeypatch: pytest.MonkeyPatch) -> None:
    from opentelemetry import trace

    monkeypatch.setenv("TRACEPARENT", TP)
    monkeypatch.setenv("TRACESTATE", "vendor=x")
    inner: list[Any] = []

    @task(task_ref="t.otel")
    def h(ctx: TaskContext) -> None:
        # A user span started in the handler joins the platform's trace.
        with trace.get_tracer("user").start_as_current_span("user.work") as s:
            inner.append(s.get_span_context())
            headers = ctx.trace_headers()
            assert headers["traceparent"].split("-")[1] == TRACE_ID
            assert headers["traceparent"].split("-")[2] == format(s.get_span_context().span_id, "016x")

    out = execute_assignment(_assignment("t.otel"), store=InMemoryResultStore())
    assert out["outcome"] == "ATTEMPT_OUTCOME_SUCCEEDED"

    finished = {s.name: s for s in spans.get_finished_spans()}
    task_span_ = finished["nexus.task node-1"]
    sc = task_span_.get_span_context()
    assert format(sc.trace_id, "032x") == TRACE_ID
    assert task_span_.parent is not None and format(task_span_.parent.span_id, "016x") == SPAN_ID
    assert task_span_.parent.is_remote
    assert str(sc.trace_state.to_header()) == "vendor=x"
    attrs = dict(task_span_.attributes or {})
    assert attrs["nexus.run_id"] == "run-1"
    assert attrs["nexus.attempt_id"] == "att-1"
    assert attrs["nexus.node_id"] == "node-1"
    assert task_span_.status.is_ok

    user = finished["user.work"]
    assert format(user.get_span_context().trace_id, "032x") == TRACE_ID
    assert user.parent is not None and user.parent.span_id == sc.span_id
    assert inner[0].trace_id == sc.trace_id


def test_a_failing_handler_records_the_exception(spans: Any, monkeypatch: pytest.MonkeyPatch) -> None:
    from opentelemetry.trace import StatusCode

    monkeypatch.setenv("TRACEPARENT", TP)

    @task(task_ref="t.otel.bad")
    def bad(ctx: TaskContext) -> None:
        raise RuntimeError("kaput")

    out = execute_assignment(_assignment("t.otel.bad"), store=InMemoryResultStore())
    assert out["outcome"] != "ATTEMPT_OUTCOME_SUCCEEDED"
    assert "kaput" in out["error"]["message"]

    (s,) = [s for s in spans.get_finished_spans() if s.name == "nexus.task node-1"]
    assert format(s.context.trace_id, "032x") == TRACE_ID
    assert s.status.status_code is StatusCode.ERROR
    assert "kaput" in (s.status.description or "")
    exc_events = [e for e in s.events if e.name == "exception"]
    assert exc_events and exc_events[0].attributes["exception.type"] == "RuntimeError"


def test_the_entrypoint_records_a_failure_on_the_span(spans: Any, tmp_path: Any) -> None:
    from opentelemetry.trace import StatusCode

    mod = tmp_path / "handlers.py"
    mod.write_text(
        "from nexus import task\n"
        "@task(task_ref='t.ep.bad')\n"
        "def epbad(ctx):\n"
        "    raise ValueError('nope')\n"
    )
    (tmp_path / "in").mkdir()
    env = {
        "NEXUS_RUN_ID": "r",
        "NEXUS_ATTEMPT_ID": "a",
        "NEXUS_NODE_ID": "epnode",
        "NEXUS_INPUT_DIR": str(tmp_path / "in"),
        "NEXUS_OUTPUT_DIR": str(tmp_path / "out"),
        "NEXUS_OUTPUT_NAMES": "result",
        "TRACEPARENT": TP,
    }
    assert run_from_environment("t.ep.bad", [str(mod)], env) == EXIT_HANDLER_FAILED
    (s,) = [s for s in spans.get_finished_spans() if s.name == "nexus.task epnode"]
    assert s.parent is not None and format(s.parent.span_id, "016x") == SPAN_ID
    assert s.status.status_code is StatusCode.ERROR
    assert any(e.name == "exception" for e in s.events)


def test_no_span_without_a_trace_context(spans: Any, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("TRACEPARENT", raising=False)

    @task(task_ref="t.otel.none")
    def h(ctx: TaskContext) -> None:
        return None

    out = execute_assignment(_assignment("t.otel.none"), store=InMemoryResultStore())
    assert out["outcome"] == "ATTEMPT_OUTCOME_SUCCEEDED"
    assert [s for s in spans.get_finished_spans() if s.name.startswith("nexus.task")] == []
