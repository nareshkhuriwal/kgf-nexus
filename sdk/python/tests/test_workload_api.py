# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""TaskContext against the agent's workload API (ADR-0032), end to end over a
real Unix socket served by an in-memory fake of the agent."""

from __future__ import annotations

import io
import json
import time

import pytest

import nexus.context as context_mod
import nexus.ratelimit as ratelimit_mod
from fake_taskapi import FakeTaskAPI, make_ca_and_cert, openssl_available
from nexus import RateLimited, StaleCheckpoint, StaleLease, StructuredLogger, TaskContext
from nexus._taskapi import (
    CheckpointTooLarge,
    TaskAPIClient,
    TaskAPIError,
    Unavailable,
    from_environment,
    parse_address,
)
from nexus.usage import InvalidUsage


@pytest.fixture(autouse=True)
def _fresh_process_state(monkeypatch):
    monkeypatch.setattr(ratelimit_mod, "_local", None)
    monkeypatch.setattr(context_mod, "_local_permit_warned", False)
    monkeypatch.delenv("NEXUS_LOCAL_RATE_LIMITS", raising=False)


@pytest.fixture
def api():
    with FakeTaskAPI() as fake:
        yield fake


def make_ctx(client: TaskAPIClient | None, *, key: str = "idem-1", attempt: int = 1,
             buf: io.StringIO | None = None) -> TaskContext:
    return TaskContext(
        run_id="run-1", node_id="node", attempt_id=f"att-{attempt}", attempt_number=attempt,
        task_ref="t.ref", task_version=1, idempotency_key=key,
        log=StructuredLogger({"t": 1}, buf if buf is not None else io.StringIO()),
        _task_api=client,
    )


# ── the client ────────────────────────────────────────────────────────────────

def test_from_environment_parses_only_unix_addresses(api):
    assert from_environment({}) is None
    assert from_environment({"NEXUS_TASK_API": "http://127.0.0.1:1"}) is None
    assert from_environment({"NEXUS_TASK_API": "unix://"}) is None
    c = from_environment(api.env)
    assert c is not None and c.socket_path == api.path
    assert "test-token" not in repr(c)
    assert parse_address("unix:///x/y.sock") == "/x/y.sock"
    with pytest.raises(ValueError):
        TaskAPIClient("", "t")


# ── the network transport (kubernetes pods, F148) ────────────────────────────

needs_openssl = pytest.mark.skipif(not openssl_available(), reason="needs the openssl CLI to mint a test CA")


@pytest.fixture
def tls_api():
    with FakeTaskAPI(tls=True) as fake:
        yield fake


@needs_openssl
def test_https_address_with_pinned_ca_round_trips(tls_api):
    c = from_environment(tls_api.env)
    assert c is not None and c.socket_path == "" and c.url == tls_api.url
    assert "test-token" not in repr(c) and "BEGIN CERTIFICATE" not in repr(c)
    ctx = make_ctx(c)
    ctx.report_tokens("mock", "m", input_tokens=4, output_tokens=2)
    assert tls_api.usage["tokens:mock/m:input"] == 4
    ctx.checkpoint({"done": 3})
    assert make_ctx(c).resume() == {"done": 3}
    ctx.heartbeat("working")
    assert tls_api.progress[-1][0] == "working"


@needs_openssl
def test_https_never_trusts_an_unpinned_or_wrong_ca(tls_api, tmp_path):
    # No CA: the system roots, which do not know the agent's private CA.
    env = dict(tls_api.env)
    del env["NEXUS_TASK_API_CA"]
    with pytest.raises(Unavailable):
        from_environment(env).info()
    # Another agent's CA: refused, never "verify=False".
    other_ca, _, _ = make_ca_and_cert(str(tmp_path))
    env["NEXUS_TASK_API_CA"] = other_ca
    with pytest.raises(Unavailable):
        from_environment(env).info()
    # A malformed CA is a configuration error surfaced at once.
    env["NEXUS_TASK_API_CA"] = "not a certificate"
    with pytest.raises(ValueError):
        from_environment(env)


@needs_openssl
def test_https_bad_token_is_unauthorized(tls_api):
    bad = TaskAPIClient.https(tls_api.url, "wrong", ca_pem=tls_api.ca_pem)
    with pytest.raises(TaskAPIError) as ei:
        bad.info()
    assert ei.value.status == 401


def test_plain_http_is_never_used():
    # A bearer token must not cross a pod network in clear.
    assert from_environment({"NEXUS_TASK_API": "http://10.0.0.1:9443", "NEXUS_TASK_API_TOKEN": "t"}) is None
    with pytest.raises(ValueError):
        TaskAPIClient.https("http://10.0.0.1:9443", "t")
    for bad in ("https://", "https://host", "https://host:0", "https://host:99999", "https://host:1/path"):
        with pytest.raises(ValueError):
            TaskAPIClient.https(bad, "t")


def test_info_and_bad_token(api):
    c = api.client()
    info = c.info()
    assert info["attempt_id"] == "att-1"
    caps = c.capabilities()
    assert caps.usage and caps.rate_limits and caps.checkpoints and caps.progress
    bad = TaskAPIClient(api.path, "wrong")
    with pytest.raises(TaskAPIError) as ei:
        bad.info()
    assert ei.value.status == 401 and ei.value.code == "unauthorized"


def test_unreachable_agent_is_unavailable_and_context_falls_back(tmp_path):
    c = TaskAPIClient("/tmp/nexus-no-such-socket.sock", "t", timeout_s=0.5)
    with pytest.raises(Unavailable):
        c.info()
    buf = io.StringIO()
    ctx = make_ctx(c, buf=buf)
    ctx.report_usage({"units:x": 2})
    assert ctx.usage_totals == {"units:x": 2}
    assert "fall back" in buf.getvalue()
    assert ctx.checkpoint({"i": 1}).key == "checkpoints/idem-1"
    assert ctx.resume() == {"i": 1}


# ── usage ─────────────────────────────────────────────────────────────────────

def test_report_usage_and_tokens_reach_the_agent(api):
    ctx = make_ctx(api.client())
    ctx.report_tokens("openai", "gpt-4o-mini", input_tokens=10, output_tokens=3)
    totals = ctx.report_tokens("openai", "gpt-4o-mini", input_tokens=5, output_tokens=1, cached_tokens=2)
    expected = {
        "tokens:openai/gpt-4o-mini:input": 15,
        "tokens:openai/gpt-4o-mini:output": 4,
        "tokens:openai/gpt-4o-mini:cached": 2,
        "requests:openai/gpt-4o-mini": 2,
    }
    assert totals == expected
    assert api.usage == expected
    assert ctx.usage_totals == expected
    # Zero-only reports send nothing.
    before = len(api.calls)
    ctx.report_usage({"units:x": 0})
    assert len(api.calls) == before


def test_invalid_usage_is_refused_before_sending(api):
    ctx = make_ctx(api.client())
    for bad in ({"no-colon": 1}, {"units:x": -1}, {"units:x": 1.5}, {"units:x": True},
                {"Units:x": 1}, {"units:x:Bad": 1}):
        with pytest.raises(InvalidUsage):
            ctx.report_usage(bad)  # type: ignore[arg-type]
    assert api.usage == {}
    with pytest.raises(InvalidUsage):
        ctx.report_usage({f"units:k{i}": 1 for i in range(65)})
    ctx.report_usage({f"units:k{i}": 1 for i in range(64)})
    with pytest.raises(InvalidUsage, match="at most 64"):
        ctx.report_usage({"units:one_more": 1})


def test_usage_unavailable_is_resent_once_not_twice(api):
    ctx = make_ctx(api.client())
    api.force_status("/v1/usage", 503)
    buf = io.StringIO()
    ctx.log = StructuredLogger({}, buf)
    ctx.report_usage({"units:a": 3})
    assert api.usage == {} and "will resend" in buf.getvalue()
    ctx.report_usage({"units:a": 1})
    assert api.usage == {"units:a": 4}
    ctx._finish()
    assert api.usage == {"units:a": 4}, "nothing pending, nothing resent"
    assert '"units": {"units:a": 4}' in buf.getvalue()


def test_usage_refused_by_platform_raises(api):
    ctx = make_ctx(api.client())
    api.force_status("/v1/usage", 400)
    with pytest.raises(InvalidUsage, match="refused"):
        ctx.report_usage({"units:a": 1})


def test_usage_unsupported_falls_back_in_process(api):
    ctx = make_ctx(api.client())
    api.force_status("/v1/usage", 501)
    ctx.report_usage({"units:a": 1})
    ctx.report_usage({"units:a": 1})
    assert ctx.usage_totals == {"units:a": 2}
    assert api.usage == {}
    assert [c for c in api.calls if c[1] == "/v1/usage"] == [("POST", "/v1/usage")], "asked once"


def test_stale_lease_on_usage_raises(api):
    ctx = make_ctx(api.client())
    api.stale = True
    with pytest.raises(StaleLease):
        ctx.report_usage({"units:a": 1})


def test_capability_off_uses_fallback():
    with FakeTaskAPI(capabilities={"usage": False, "progress": False, "rate_limits": False,
                                   "checkpoints": False}) as fake:
        ctx = make_ctx(fake.client())
        ctx.report_usage({"units:a": 1})
        ctx.heartbeat("x")
        ctx.acquire_permits("openai")
        ctx.checkpoint([1])
        assert ctx.resume() == [1]
        assert [p for _, p in fake.calls] == ["/v1/info"]


# ── progress (F148) ───────────────────────────────────────────────────────────

def test_heartbeat_and_progress_are_forwarded(api, monkeypatch):
    monkeypatch.setattr(context_mod, "PROGRESS_MIN_INTERVAL_S", 0.0)
    ctx = make_ctx(api.client())
    ctx.heartbeat("connecting")
    ctx.progress(0.5, "half")
    ctx.heartbeat()
    assert api.progress == [("connecting", None), ("half", 0.5), ("", None)]


def test_progress_is_coalesced_but_completion_always_sent(api):
    ctx = make_ctx(api.client())
    for i in range(50):
        ctx.progress(i / 100)
    ctx.progress(1.0, "done")
    assert api.progress[0] == ("", 0.0)
    assert api.progress[-1] == ("done", 1.0)
    assert len(api.progress) == 2


def test_heartbeat_failures(api, monkeypatch):
    monkeypatch.setattr(context_mod, "PROGRESS_MIN_INTERVAL_S", 0.0)
    buf = io.StringIO()
    ctx = make_ctx(api.client(), buf=buf)
    api.force_status("/v1/progress", 503)
    ctx.heartbeat("a")  # logged, not raised
    assert "progress not delivered" in buf.getvalue()
    api.force_status("/v1/progress", 501)
    ctx.heartbeat("b")  # falls back to the debug log, and stops asking
    ctx.heartbeat("c")
    assert sum(1 for _, p in api.calls if p == "/v1/progress") == 2
    api2 = make_ctx(api.client())
    api.stale = True
    with pytest.raises(StaleLease):
        api2.heartbeat("zombie")


def test_heartbeat_fn_still_wins():
    seen = []
    ctx = make_ctx(None)
    ctx._heartbeat_fn = lambda m, f: seen.append((m, f))
    ctx.heartbeat("x")
    ctx.progress(0.25)
    assert seen == [("x", None), (None, 0.25)]


# ── permits ───────────────────────────────────────────────────────────────────

def test_permit_denials_sleep_retry_after_then_grant(api):
    api.permit_script = [
        {"granted": False, "retry_after_ms": 60},
        {"granted": False, "retry_after_ms": 60},
        {"granted": True, "unlimited_keys": ["openai"]},
    ]
    ctx = make_ctx(api.client())
    t0 = time.monotonic()
    g = ctx.acquire_permits(["openai", "openai/gpt-4o-mini", "openai"], tokens=100)
    assert time.monotonic() - t0 >= 0.12
    assert g.attempts == 3 and g.source == "platform"
    assert g.keys == ("openai", "openai/gpt-4o-mini")
    assert g.unlimited_keys == ("openai",)
    assert api.permit_asks[0] == {"keys": ["openai", "openai/gpt-4o-mini"], "requests": 1, "tokens": 100}


def test_permit_timeout_raises_rate_limited(api):
    api.permit_script = [{"granted": False, "retry_after_ms": 5000}]
    ctx = make_ctx(api.client())
    with pytest.raises(RateLimited) as ei:
        ctx.acquire_permits(["anthropic/claude-x"], timeout=0.5)
    assert ei.value.retry_after_s == 5.0 and ei.value.retry_after_ms == 5000
    assert ei.value.provider == "anthropic" and ei.value.model == "claude-x"


def test_permit_wait_bounded_by_task_deadline(api):
    api.permit_script = [{"granted": False, "retry_after_ms": 10_000}]
    ctx = make_ctx(api.client())
    ctx.start_to_close_deadline_unix_ms = int(time.time() * 1000) + 2000
    with pytest.raises(RateLimited):
        ctx.acquire_permits("openai")


def test_permit_unavailable_retries_then_unsupported_falls_back(api, monkeypatch):
    ctx = make_ctx(api.client())
    api.force_status("/v1/permits", 503)
    g = ctx.acquire_permits("openai")
    assert g.attempts == 2 and g.source == "platform"
    api.force_status("/v1/permits", 501)
    g = ctx.acquire_permits("openai")
    assert g.source == "local"
    monkeypatch.setattr(context_mod, "PERMIT_UNAVAILABLE_LIMIT_S", 0.0)
    ctx2 = make_ctx(api.client())
    api.force_status("/v1/permits", 503, times=5)
    with pytest.raises(Unavailable):
        ctx2.acquire_permits("openai")


def test_permit_argument_validation(api):
    ctx = make_ctx(api.client())
    for bad_keys in ([], ["has space"], ["openai:bad"]):
        with pytest.raises(ValueError):
            ctx.acquire_permits(bad_keys)
    with pytest.raises(ValueError):
        ctx.acquire_permits("openai", requests=-1)
    with pytest.raises(ValueError):
        ctx.acquire_permits("openai", tokens=True)  # type: ignore[arg-type]
    with pytest.raises(ValueError):
        ctx.acquire_permits("openai", timeout=-1)
    with pytest.raises(TypeError):
        ctx.settle_permits("openai", 1.5)  # type: ignore[arg-type]


def test_settle_permits(api):
    buf = io.StringIO()
    ctx = make_ctx(api.client(), buf=buf)
    ctx.settle_permits(["openai", "openai/m"], -40)
    ctx.settle_permits("openai", 0)  # nothing to send
    assert api.settles == [{"keys": ["openai", "openai/m"], "tokens": -40}]
    api.force_status("/v1/permits/settle", 503)
    ctx.settle_permits("openai", 5)
    assert "settlement not delivered" in buf.getvalue()
    api.stale = True
    with pytest.raises(StaleLease):
        ctx.settle_permits("openai", 5)
    api.stale = False
    api.force_status("/v1/permits/settle", 501)
    ctx.settle_permits("openai", 5)  # local fallback, no error


# ── checkpoints ───────────────────────────────────────────────────────────────

def test_checkpoint_resume_round_trip_through_the_agent(api):
    first = make_ctx(api.client())
    assert first.resume() is None
    info1 = first.checkpoint({"cursor": 10, "done": ["a"]})
    info2 = first.checkpoint({"cursor": 20, "done": ["a", "b"]})
    assert (info1.seq, info2.seq) == (1, 2)
    assert info2.size_bytes == len(b'{"cursor":20,"done":["a","b"]}')
    # Canonical JSON bytes: sorted keys, no whitespace.
    assert api.checkpoints[-1][1] == b'{"cursor":20,"done":["a","b"]}'
    retry = make_ctx(api.client(), attempt=2)
    assert retry.resume() == {"cursor": 20, "done": ["a", "b"]}


def test_checkpoint_errors(api):
    ctx = make_ctx(api.client())
    api.max_checkpoint_bytes = 10
    with pytest.raises(CheckpointTooLarge):
        ctx.checkpoint({"a": "x" * 100})
    api.force_status("/v1/checkpoint", 409)
    with pytest.raises(StaleLease):
        ctx.checkpoint({"a": 1})
    with pytest.raises(ValueError):
        ctx.checkpoint({"a": float("nan")})


def test_stale_checkpoint_code(api):
    ctx = make_ctx(api.client())
    with api.lock:
        api.force["/v1/checkpoint"] = (409, 1)
    # A forced 409 answers stale_lease; emulate the checkpoint-specific code.
    from nexus._taskapi import _error_for, _Response

    err = _error_for("PUT", "/v1/checkpoint",
                     _Response(409, json.dumps({"error": "stale_checkpoint"}).encode(), {}))
    assert isinstance(err, StaleCheckpoint) and isinstance(err, StaleLease)
    with pytest.raises(StaleLease):
        ctx.checkpoint({"a": 1})


def test_local_size_limit(monkeypatch):
    monkeypatch.setattr(context_mod, "MAX_CHECKPOINT_BYTES", 8)
    with pytest.raises(CheckpointTooLarge):
        make_ctx(None).checkpoint("0123456789")


def test_unreadable_checkpoint_from_agent_resumes_from_scratch(api):
    api.current = (1, "att-0", b"not json")
    buf = io.StringIO()
    assert make_ctx(api.client(), buf=buf).resume() is None
    assert "starting from scratch" in buf.getvalue()


def test_checkpoint_unsupported_falls_back_to_store(api):
    ctx = make_ctx(api.client())
    api.force_status("/v1/checkpoint", 501)
    info = ctx.checkpoint({"n": 1})
    assert info.key == "checkpoints/idem-1"
    assert ctx.resume() == {"n": 1}


# ── declared errors ───────────────────────────────────────────────────────────

def test_report_error_payload(api):
    c = api.client()
    c.report_error("rate_limited", "RateLimited", "slow down", 1500)
    c.report_error("application", "RuntimeError", "x" * 3000)
    assert api.errors[0] == {"class": "rate_limited", "type": "RateLimited", "message": "slow down",
                             "retry_after_ms": 1500}
    assert "retry_after_ms" not in api.errors[1] and len(api.errors[1]["message"]) == 2000
