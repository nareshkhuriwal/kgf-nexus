# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Typed usage of the AI-workload API (ADR-0032), checked by mypy --strict.

Only type-checked, never executed: the handler below is not called, so no
request is made to anything.
"""

from __future__ import annotations

from typing import Any, assert_type

from nexus import CheckpointInfo, PermitGrant, RateLimited, Retry, TaskContext, task
from nexus.llm import Completion, LLMClient
from nexus.testing import MockLLMServer, MockStats
from nexus.usage import token_units


@task(task_ref="typed.llm_shard", retry=Retry(max_attempts=3, retry_on=["transient", "rate_limited"]))
def llm_shard(ctx: TaskContext, prompts: list[str], base_url: str) -> list[str]:
    llm = LLMClient(ctx, provider="openai", model="gpt-4o-mini", base_url=base_url,
                    api_key_env="OPENAI_API_KEY")
    state = ctx.resume()
    assert_type(state, Any | None)
    start: int = state["next"] if state else 0
    out: list[str] = []
    for i in range(start, len(prompts)):
        c = llm.complete(prompts[i], max_tokens=64, system="Be brief.")
        assert_type(c, Completion)
        assert_type(c.text, str)
        out.append(c.text)
        info = ctx.checkpoint({"next": i + 1})
        assert_type(info, CheckpointInfo)
        ctx.progress((i + 1) / len(prompts))
    grant = ctx.acquire_permits(["openai", "openai/gpt-4o-mini"], requests=1, tokens=100, timeout=30.0)
    assert_type(grant, PermitGrant)
    ctx.settle_permits(grant.keys, -20)
    assert_type(ctx.report_usage(token_units("openai", "gpt-4o-mini", input_tokens=1)), dict[str, int])
    assert_type(ctx.report_tokens("openai", "gpt-4o-mini", input_tokens=1, output_tokens=2), dict[str, int])
    assert_type(ctx.usage_totals, dict[str, int])
    if not out:
        raise RateLimited("nothing done", retry_after_s=5.0, provider="openai", model="gpt-4o-mini")
    return out


def _mock_types() -> None:
    with MockLLMServer(rate_limit_every=5) as mock:
        assert_type(mock.base_url, str)
        assert_type(mock.stats, MockStats)
        assert_type(mock.stats.requests["/v1/chat/completions"], int)
