# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Test helpers that ship with the SDK.

:class:`MockLLMServer` is an OpenAI- and Anthropic-compatible HTTP server with
deterministic answers and token counts, injectable 429s and 503s, and an
OpenAI-style fine-tuning job lifecycle. Point ``nexus.llm.LLMClient`` (or any
OpenAI/Anthropic client) at its ``base_url`` to exercise an AI workload end to
end without a provider account, a key, or a network. See
:mod:`nexus.testing.mock_llm`.
"""

from __future__ import annotations

from .mock_llm import MockLLMServer, MockStats, respond

__all__ = ["MockLLMServer", "MockStats", "respond"]
