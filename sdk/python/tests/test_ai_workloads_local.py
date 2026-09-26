# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""ADR-0032 without a workload API: in-process usage, the local permit
bucket, checkpoints in ctx.store (and across local-runner retries), plus the
rate_limited error class through the worker, the entrypoint and the spec."""

from __future__ import annotations

import io
import json
import os
import textwrap

import pytest

import nexus.context as context_mod
import nexus.ratelimit as ratelimit_mod
from fake_taskapi import FakeTaskAPI
from nexus import (
    FileResultStore,
    InMemoryResultStore,
    RateLimited,
    Retry,
    StructuredLogger,
    TaskContext,
    local,
    spec_hash,
    task,
    workflow,
)
from nexus._dag import should_retry
from nexus.entrypoint import EXIT_HANDLER_FAILED, run_from_environment
from nexus.ratelimit import LocalRateLimiter, RateLimit, local_limiter, parse_rate_limits
from nexus.task import registry
from nexus.usage import InvalidUsage, token_units, usage_subject, validate_usage
from nexus.worker import build_context, execute_assignment


@pytest.fixture(autouse=True)
def _fresh(monkeypatch):
    registry.clear()
    monkeypatch.setattr(ratelimit_mod, "_local", None)
    monkeypatch.setattr(context_mod, "_local_permit_warned", False)
    monkeypatch.delenv("NEXUS_LOCAL_RATE_LIMITS", raising=False)
    monkeypatch.delenv("NEXUS_TASK_API", raising=False)
    yield
    registry.clear()


def make_ctx(store=None, *, key="idem-L", attempt=1, buf=None) -> TaskContext:
    return TaskContext(
        run_id="r", node_id="n", attempt_id=f"a{attempt}", attempt_number=attempt, task_ref="t",
        task_version=1, idempotency_key=key, store=store or InMemoryResultStore(),
        log=StructuredLogger({}, buf if buf is not None else io.StringIO()),
    )


# ── usage keys ────────────────────────────────────────────────────────────────

def test_token_units_convention():
    assert token_units("OpenAI", "gpt-4o-mini", input_tokens=12, output_tokens=3, requests=1) == {
        "tokens:openai/gpt-4o-mini:input": 12,
        "tokens:openai/gpt-4o-mini:output": 3,
        "requests:openai/gpt-4o-mini": 1,
    }
    assert token_units("anthropic", "m") == {}
    # A fine-tuned id's ':' is mapped, deterministically, into the subject alphabet.
    assert usage_subject("openai", "ft:gpt-4o-mini:org::abc") == "openai/ft_gpt-4o-mini_org__abc"
    assert usage_subject("bedrock") == "bedrock"
    with pytest.raises(InvalidUsage):
        usage_subject(" ")
    with pytest.raises(InvalidUsage):
        validate_usage({"k:" + "x" * 199: 1})
    with pytest.raises(InvalidUsage):
        validate_usage({"units:x": 10**15 + 1})
    with pytest.raises(InvalidUsage):
        validate_usage([("units:x", 1)])  # type: ignore[arg-type]
    with pytest.raises(InvalidUsage):
        validate_usage({1: 1})  # type: ignore[dict-item]
    assert validate_usage({"images:openai/dall-e-3": 0, "units:embeddings": 5}) == {
        "images:openai/dall-e-3": 0, "units:embeddings": 5}


def test_usage_accumulates_in_process_and_is_logged():
    buf = io.StringIO()
    ctx = make_ctx(buf=buf)
    ctx.report_tokens("mock", "mock-large", input_tokens=4, output_tokens=2)
    ctx.report_tokens("mock", "mock-large", input_tokens=1, output_tokens=1)
    assert ctx.usage_totals["tokens:mock/mock-large:input"] == 5
    assert ctx.usage_totals["requests:mock/mock-large"] == 2
    ctx._finish()
    line = [json.loads(x) for x in buf.getvalue().splitlines() if '"usage"' in x][-1]
    assert line["units"]["tokens:mock/mock-large:output"] == 3


# ── local permits ─────────────────────────────────────────────────────────────

class Clock:
    def __init__(self) -> None:
        self.t = 100.0

    def __call__(self) -> float:
        return self.t


def test_local_limiter_semantics():
    clock = Clock()
    lim = LocalRateLimiter({"p": RateLimit(requests_per_second=2, tokens_per_minute=600)}, clock=clock)
    assert lim.acquire(["p", "free"], 1, 0) == (True, 0, ("free",))
    assert lim.acquire(["p"], 1, 0)[0]
    granted, retry_ms, _ = lim.acquire(["p"], 1, 0)
    assert not granted and retry_ms == 500
    clock.t += 0.5
    assert lim.acquire(["p"], 1, 0)[0]
    # Tokens: 600/min = 10/s, burst 600. An oversized ask needs a full bucket,
    # then drives it negative.
    clock.t += 10
    assert lim.acquire(["p"], 0, 1000)[0]
    granted, retry_ms, _ = lim.acquire(["p"], 0, 10)
    assert not granted and retry_ms == pytest.approx(41000, abs=10)
    # A refund (negative settle) is applied without a check, clamped at the burst.
    lim.settle(["p", "free"], -10_000)
    assert lim.acquire(["p"], 0, 600)[0]
    lim.settle(["p"], 10_000)
    assert not lim.acquire(["p"], 0, 1)[0]


def test_parse_rate_limits():
    got = parse_rate_limits('{"openai": {"requests_per_second": 5, "burst_requests": 1},'
                            ' "a/b": {"tokens_per_minute": 100}}')
    assert got["openai"].request_burst() == 1.0 and got["a/b"].token_burst() == 100.0
    assert got["a/b"].request_burst() == 1.0
    for bad in ("[]", '{"k": 1}', '{"k": {}}', '{"k": {"requests_per_second": 0}}',
                '{"k": {"tokens_per_minute": -1}}'):
        with pytest.raises(ValueError):
            parse_rate_limits(bad)


def test_local_permits_unlimited_by_default_and_warn_once():
    buf = io.StringIO()
    ctx = make_ctx(buf=buf)
    for _ in range(100):
        g = ctx.acquire_permits(["openai", "openai/gpt-4o-mini"], tokens=10_000)
    assert g.source == "local" and g.unlimited_keys == ("openai", "openai/gpt-4o-mini")
    assert buf.getvalue().count("granted without limit") == 1
    ctx.settle_permits("openai", 5)  # no limit: a no-op


def test_local_permits_from_env(monkeypatch):
    monkeypatch.setenv("NEXUS_LOCAL_RATE_LIMITS", json.dumps({"openai": {"requests_per_second": 20, "burst_requests": 1}}))
    buf = io.StringIO()
    ctx = make_ctx(buf=buf)
    ctx.acquire_permits("openai")
    assert "NOT shared" in buf.getvalue()
    with pytest.raises(RateLimited) as ei:
        ctx.acquire_permits("openai", timeout=0)
    assert ei.value.retry_after_s == pytest.approx(0.05, abs=0.01)
    g = ctx.acquire_permits("openai", timeout=1.0)
    assert g.attempts >= 2 and g.waited_s > 0
    assert local_limiter() is local_limiter()
    monkeypatch.setenv("NEXUS_LOCAL_RATE_LIMITS", "{nope")
    with pytest.raises(ValueError, match="NEXUS_LOCAL_RATE_LIMITS"):
        local_limiter()


# ── local checkpoints ─────────────────────────────────────────────────────────

def test_checkpoint_in_store_resumed_by_next_attempt(tmp_path):
    store = FileResultStore(str(tmp_path))
    first = make_ctx(store)
    assert first.resume() is None
    first.checkpoint({"i": 1})
    info = first.checkpoint({"i": 2})
    assert info.seq == 2 and info.key == "checkpoints/idem-L" and len(info.sha256) == 64
    assert make_ctx(store, attempt=2).resume() == {"i": 2}
    assert make_ctx(store, key="other").resume() is None


def test_malformed_local_checkpoint_is_ignored():
    store = InMemoryResultStore()
    store.put({"no": "state"}, key="checkpoints/idem-L")
    buf = io.StringIO()
    assert make_ctx(store, buf=buf).resume() is None
    assert "malformed" in buf.getvalue()

    class Broken(InMemoryResultStore):
        def exists(self, key: str) -> bool:
            raise OSError("disk gone")

    buf = io.StringIO()
    assert make_ctx(Broken(), buf=buf).resume() is None
    assert "unreadable" in buf.getvalue()


def test_local_runner_resumes_a_retry_from_its_checkpoint(tmp_path):
    seen: list[object] = []

    @task(task_ref="ai.shard", retry=Retry(max_attempts=2, initial="1s"))
    def shard(ctx: TaskContext) -> int:
        state = ctx.resume()
        seen.append(state)
        start = state["next"] if state else 0
        for i in range(start, 10):
            if i == 6 and ctx.attempt_number == 1:
                raise ConnectionError("provider went away")
            if i % 3 == 0:
                ctx.checkpoint({"next": i})
        return start

    @workflow(name="ai-resume")
    def wf():
        shard()

    res = local.run(wf, run_dir=str(tmp_path), isolation="inprocess", backoff_scale=0, out=None)
    assert res.state == "SUCCEEDED", res.summary()
    assert seen == [None, {"next": 3}]
    assert res.output("shard") == 3


# ── rate_limited through the worker, the entrypoint and the spec ─────────────

def test_worker_reports_rate_limited_and_declares_it():
    @task(task_ref="ai.throttled")
    def throttled(ctx: TaskContext) -> None:
        ctx.report_usage({"requests:openai/m": 1})
        raise RateLimited("429s", retry_after_s=2.5, provider="openai", model="m")

    with FakeTaskAPI() as fake:
        out = execute_assignment(
            {"run_id": "r", "node_id": "n", "attempt_id": "a", "spec": {"task_ref": "ai.throttled"}},
            task_api=fake.client(),
        )
        assert fake.errors == [{"class": "rate_limited", "type": "RateLimited", "message": "429s",
                                "retry_after_ms": 2500}]
        assert fake.usage == {"requests:openai/m": 1}
    err = out["error"]
    assert err["class"] == "ERROR_CLASS_RATE_LIMITED" and err["retry_after_ms"] == 2500
    assert err["retryable"] is True


def test_worker_declares_only_the_allowed_classes():
    @task(task_ref="ai.bad")
    def bad(ctx: TaskContext) -> None:
        raise ValueError("bad input")

    @task(task_ref="ai.slow")
    def slow(ctx: TaskContext) -> None:
        raise TimeoutError("took too long")

    with FakeTaskAPI() as fake:
        for ref in ("ai.bad", "ai.slow"):
            execute_assignment({"run_id": "r", "node_id": "n", "spec": {"task_ref": ref}},
                               task_api=fake.client())
        assert [e["class"] for e in fake.errors] == ["validation"], "timeout is the agent's to claim"
        fake.force_status("/v1/error", 503)
        out = execute_assignment({"run_id": "r", "node_id": "n", "spec": {"task_ref": "ai.bad"}},
                                 task_api=fake.client())
        assert out["error"]["class"] == "ERROR_CLASS_VALIDATION"


def test_build_context_reads_the_environment(monkeypatch):
    with FakeTaskAPI() as fake:
        for k, v in fake.env.items():
            monkeypatch.setenv(k, v)
        ctx = build_context({"run_id": "r", "node_id": "n"})
        assert ctx._task_api is not None and ctx._task_api.socket_path == fake.path
        assert build_context({"run_id": "r", "node_id": "n"}, task_api=None)._task_api is None


HANDLERS = textwrap.dedent(
    """
    from nexus import task, RateLimited

    @task(task_ref="ep.throttled")
    def throttled(ctx):
        ctx.heartbeat("working")
        ctx.report_tokens("mock", "mock-small", input_tokens=3, output_tokens=1)
        raise RateLimited("provider 429", retry_after_s=1.25)

    @task(task_ref="ep.ok")
    def ok(ctx):
        ctx.checkpoint({"n": 1})
        return {"resumed": ctx.resume()}
    """
)


def test_entrypoint_posts_rate_limited_and_forwards_progress(tmp_path):
    (tmp_path / "ep_handlers.py").write_text(HANDLERS)
    out_dir = tmp_path / "out"
    with FakeTaskAPI() as fake:
        env = {**fake.env, "NEXUS_OUTPUT_DIR": str(out_dir), "NEXUS_RUN_ID": "r", "NEXUS_NODE_ID": "n"}
        code = run_from_environment("ep.throttled", [str(tmp_path / "ep_handlers.py")], env)
        assert code == EXIT_HANDLER_FAILED
        assert fake.errors == [{"class": "rate_limited", "type": "RateLimited", "message": "provider 429",
                                "retry_after_ms": 1250}]
        assert fake.progress == [("working", None)]
        assert fake.usage["tokens:mock/mock-small:input"] == 3
        registry.clear()
        assert run_from_environment("ep.ok", [str(tmp_path / "ep_handlers.py")], env) == 0
        assert json.loads((out_dir / "result").read_text()) == {"resumed": {"n": 1}}
        assert fake.checkpoints == [(1, b'{"n":1}')]


def test_rate_limited_error_class_in_specs_and_retry_policy():
    @task(task_ref="ai.spec", retry=Retry(max_attempts=4, retry_on=["transient", "rate_limited"],
                                          no_retry_on=["validation"]))
    def t(ctx: TaskContext) -> None: ...

    @workflow(name="ai-spec")
    def wf():
        t()

    compiled = wf.compile()
    retry = compiled.spec["tasks"][0]["retry"]
    assert "ERROR_CLASS_RATE_LIMITED" in retry["retry_on"]
    assert spec_hash(compiled.spec) == compiled.hash
    # Locally, a throttle retries even when retry_on omits it, unless vetoed.
    assert should_retry("ERROR_CLASS_RATE_LIMITED", "IDEMPOTENCY_MODE_REQUIRED", 1,
                        {"retry_on": ["ERROR_CLASS_TRANSIENT"]})[0]
    assert not should_retry("ERROR_CLASS_RATE_LIMITED", "IDEMPOTENCY_MODE_REQUIRED", 1,
                            {"no_retry_on": ["ERROR_CLASS_RATE_LIMITED"]})[0]


def test_local_runner_strips_a_parent_task_api(monkeypatch, tmp_path):
    monkeypatch.setenv("NEXUS_TASK_API", "unix:///tmp/parent.sock")
    monkeypatch.setenv("NEXUS_TASK_API_TOKEN", "parent")

    @task(task_ref="ai.env")
    def env_task(ctx: TaskContext) -> dict[str, object]:
        ctx.report_usage({"units:x": 1})
        return {"api": os.environ.get("NEXUS_TASK_API"), "has_api": ctx._task_api is not None}

    @workflow(name="ai-env")
    def wf():
        env_task()

    res = local.run(wf, run_dir=str(tmp_path), isolation="inprocess", backoff_scale=0, out=None)
    assert res.output("env_task")["has_api"] is False
