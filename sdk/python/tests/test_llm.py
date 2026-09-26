# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""nexus.llm.LLMClient against nexus.testing.MockLLMServer. No real provider,
no real key: every call here goes to 127.0.0.1."""

from __future__ import annotations

import io
import time

import pytest

import nexus.context as context_mod
import nexus.ratelimit as ratelimit_mod
from fake_taskapi import FakeTaskAPI
from nexus import RateLimited, StructuredLogger, TaskContext
from nexus.llm import (
    LLMAuthError,
    LLMClient,
    LLMRequestError,
    LLMUnavailable,
    estimate_tokens,
    parse_retry_after,
)
from nexus.testing import MockLLMServer

SECRET = "sk-test-DO-NOT-LOG-0123456789"


@pytest.fixture(autouse=True)
def _fresh(monkeypatch):
    monkeypatch.setattr(ratelimit_mod, "_local", None)
    monkeypatch.setattr(context_mod, "_local_permit_warned", False)
    monkeypatch.delenv("NEXUS_LOCAL_RATE_LIMITS", raising=False)


def make_ctx(api=None, buf=None) -> TaskContext:
    return TaskContext(
        run_id="r", node_id="n", attempt_id="a", attempt_number=1, task_ref="t", task_version=1,
        idempotency_key="k", log=StructuredLogger({}, buf if buf is not None else io.StringIO()),
        _task_api=api,
    )


@pytest.mark.parametrize("provider", ["openai", "anthropic"])
def test_complete_reports_usage_and_settles(provider):
    with MockLLMServer() as mock, FakeTaskAPI() as agent:
        ctx = make_ctx(agent.client())
        llm = LLMClient(ctx, provider=provider, model="mock-large", base_url=mock.base_url,
                        api_key=SECRET, usage_provider="mock")
        c = llm.complete("What is 12 + 7?", max_tokens=16, system="Answer with a number.")
        assert c.text == "19" and c.model == "mock-large"
        assert c.finish_reason in ("stop", "end_turn")
        # Word counts: "Answer with a number. What is 12 + 7?" = 9 words; "19" = 1.
        assert (c.input_tokens, c.output_tokens) == (9, 1)
        assert agent.usage == {
            "tokens:mock/mock-large:input": 9,
            "tokens:mock/mock-large:output": 1,
            "requests:mock/mock-large": 1,
        }
        estimate = estimate_tokens(len("What is 12 + 7?") + len("Answer with a number."), 16)
        assert agent.permit_asks == [
            {"keys": [provider, f"{provider}/mock-large"], "requests": 1, "tokens": estimate}
        ]
        assert agent.settles == [{"keys": [provider, f"{provider}/mock-large"], "tokens": 10 - estimate}]
        path = "/v1/chat/completions" if provider == "openai" else "/v1/messages"
        assert mock.stats.requests[path] == 1 and mock.stats.total_tokens == 10


def test_messages_list_and_anthropic_system_role():
    with MockLLMServer() as mock:
        ctx = make_ctx()
        llm = LLMClient(ctx, provider="anthropic", model="mock-large", base_url=mock.base_url + "/",
                        api_key=SECRET)
        c = llm.complete([
            {"role": "system", "content": "You are a strict grader."},
            {"role": "user", "content": "Reference: Paris\nCandidate: paris."},
        ])
        assert c.text == "PASS"
        assert ctx.usage_totals["requests:anthropic/mock-large"] == 1
        with pytest.raises(ValueError):
            llm.complete([{"role": "system", "content": "only"}])
        with pytest.raises(ValueError):
            llm.complete([{"role": "tool", "content": "x"}])
        with pytest.raises(ValueError):
            llm.complete("x", max_tokens=0)


def test_max_tokens_truncates():
    with MockLLMServer() as mock:
        llm = LLMClient(make_ctx(), provider="openai", model="mock-large", base_url=mock.base_url,
                        api_key=SECRET)
        c = llm.complete("one two three four five six seven eight nine", max_tokens=3)
        assert c.text == "Summary: one two" and c.finish_reason == "length" and c.output_tokens == 3


def test_429_retry_after_is_honoured():
    with MockLLMServer(rate_limit_every=2, retry_after_s=0.2) as mock, FakeTaskAPI() as agent:
        ctx = make_ctx(agent.client())
        llm = LLMClient(ctx, provider="openai", model="mock-small", base_url=mock.base_url,
                        api_key=SECRET)
        llm.complete("hello there")
        t0 = time.monotonic()
        c = llm.complete("hello again")  # 2nd call: 429, then success
        assert time.monotonic() - t0 >= 0.2
        assert c.text == "Summary: hello again"
        assert mock.stats.throttled == 1 and mock.stats.completions == 2
        est = estimate_tokens(len("hello again"), 256)
        # The refused call's tokens were refunded before the retry re-asked.
        assert {"keys": ["openai", "openai/mock-small"], "tokens": -est} in agent.settles
        assert len(agent.permit_asks) == 3
        assert agent.usage["requests:openai/mock-small"] == 2, "throttled calls are not billed"


def test_persistent_503_raises_rate_limited():
    with MockLLMServer(fail_first=10, retry_after_s=0.05) as mock:
        buf = io.StringIO()
        ctx = make_ctx(buf=buf)
        llm = LLMClient(ctx, provider="anthropic", model="mock-large", base_url=mock.base_url,
                        api_key=SECRET, max_retries=2)
        with pytest.raises(RateLimited) as ei:
            llm.complete("hi")
        assert ei.value.retry_after_s == pytest.approx(0.05)
        assert ei.value.provider == "anthropic" and ei.value.model == "mock-large"
        assert mock.stats.unavailable == 3
        assert SECRET not in buf.getvalue() and SECRET not in str(ei.value)


def test_retry_after_over_the_bound_raises_at_once():
    with MockLLMServer(fail_first=1, retry_after_s=120) as mock:
        llm = LLMClient(make_ctx(), provider="openai", model="m", base_url=mock.base_url,
                        api_key=SECRET, max_retry_wait_s=5)
        t0 = time.monotonic()
        with pytest.raises(RateLimited) as ei:
            llm.complete("hi")
        assert time.monotonic() - t0 < 2 and ei.value.retry_after_s == 120
        assert ei.value.retry_after_ms == 120_000


def test_auth_and_request_errors(monkeypatch):
    with MockLLMServer() as mock:
        buf = io.StringIO()
        ctx = make_ctx(buf=buf)
        with pytest.raises(LLMAuthError, match="401"):
            LLMClient(ctx, provider="openai", model="m", base_url=mock.base_url).complete("x")
        monkeypatch.delenv("NEXUS_TEST_KEY", raising=False)
        with pytest.raises(LLMAuthError, match="NEXUS_TEST_KEY"):
            LLMClient(ctx, provider="openai", model="m", base_url=mock.base_url, api_key_env="NEXUS_TEST_KEY")
        monkeypatch.setenv("NEXUS_TEST_KEY", SECRET)
        llm = LLMClient(ctx, provider="openai", model="m", base_url=mock.base_url + "/nope",
                        api_key_env="NEXUS_TEST_KEY")
        assert SECRET not in repr(llm) and "<set>" in repr(llm)
        with pytest.raises(LLMRequestError) as ei:
            llm.complete("x")
        assert ei.value.status == 404 and isinstance(ei.value, ValueError)
        assert SECRET not in str(ei.value) and SECRET not in buf.getvalue()
    for bad in ({"provider": "cohere"}, {"model": ""}, {"base_url": ""}, {"max_retries": -1}):
        kw = {"provider": "openai", "model": "m", "base_url": "http://x", **bad}
        with pytest.raises(ValueError):
            LLMClient(make_ctx(), **kw)  # type: ignore[arg-type]


def test_network_error_is_unavailable():
    with MockLLMServer() as mock:
        url = mock.base_url
    buf = io.StringIO()
    llm = LLMClient(make_ctx(buf=buf), provider="openai", model="m", base_url=url, api_key=SECRET,
                    max_retries=1, timeout_s=2)
    with pytest.raises(LLMUnavailable) as ei:
        llm.complete("x")
    assert isinstance(ei.value, ConnectionError)
    assert "llm request failed; retrying" in buf.getvalue() and SECRET not in buf.getvalue()


def test_permits_rate_limited_propagates():
    with MockLLMServer() as mock, FakeTaskAPI() as agent:
        agent.permit_script = [{"granted": False, "retry_after_ms": 30_000}]
        ctx = make_ctx(agent.client())
        ctx.start_to_close_deadline_unix_ms = int(time.time() * 1000) + 1000
        llm = LLMClient(ctx, provider="openai", model="m", base_url=mock.base_url, api_key=SECRET,
                        rate_limit_keys=["openai"])
        with pytest.raises(RateLimited):
            llm.complete("x")
        assert mock.stats.completions == 0


def test_fine_tuned_model_ids_map_into_usage_keys():
    with MockLLMServer() as mock:
        ctx = make_ctx()
        llm = LLMClient(ctx, provider="openai", model="ft:mock-small:mock:ftjob-mock1",
                        base_url=mock.base_url, api_key=SECRET)
        assert llm.rate_limit_keys == ("openai", "openai/ft_mock-small_mock_ftjob-mock1")
        llm.complete("What is 1 + 1?")
        assert "requests:openai/ft_mock-small_mock_ftjob-mock1" in ctx.usage_totals


def test_parse_retry_after():
    assert parse_retry_after(None) is None
    assert parse_retry_after("") is None
    assert parse_retry_after("2.5") == 2.5
    assert parse_retry_after("-3") == 0.0
    assert parse_retry_after("soon") is None
    now = 1_700_000_000.0
    assert parse_retry_after("Tue, 14 Nov 2023 22:13:30 GMT", now=now) == pytest.approx(10.0)
    assert parse_retry_after("Tue, 14 Nov 2023 22:13:00 GMT", now=now) == 0.0
    assert estimate_tokens(9, 10) == 13
