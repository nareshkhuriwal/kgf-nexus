# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""A minimal OpenAI- and Anthropic-compatible chat client for tasks.

    from nexus.llm import LLMClient

    @task(retry=Retry(max_attempts=3))
    def summarize(ctx: TaskContext, docs: list[str]) -> list[str]:
        llm = LLMClient(ctx, provider="openai", model="gpt-4o-mini",
                        base_url="https://api.openai.com", api_key_env="OPENAI_API_KEY")
        return [llm.complete(f"Summarize: {d}", max_tokens=128).text for d in docs]

What it does that a bare HTTP call does not, per call (ADR-0032 §6):

  1. ESTIMATE tokens: ``ceil(chars / 4) + max_tokens``.
  2. ACQUIRE permits for ``requests=1, tokens=estimate`` on the rate-limit keys
     (default ``[provider, provider/model]``) through ``ctx.acquire_permits``,
     so the whole fleet stays under the provider's limit.
  3. CALL the provider (stdlib ``urllib``; no new dependency).
  4. On 429 or 503, REFUND the estimate, honour ``Retry-After`` (seconds or
     an HTTP date), back off exponentially with jitter, capped at
     ``max_retry_wait_s``, and try again up to ``max_retries`` times; then
     raise :class:`nexus.RateLimited`, which the platform retries later
     without charging the retry budget. A ``Retry-After`` longer than
     ``max_retry_wait_s`` raises at once: sleeping that long inside a task
     holds a worker slot for nothing.
  5. On success, SETTLE ``actual - estimate`` tokens and REPORT usage under
     ``tokens:<provider>/<model>:{input,output,cached}`` and
     ``requests:<provider>/<model>`` (successful calls; a throttled call is
     not billed by providers and is not counted).

Other failures raise :class:`LLMError` subclasses classified for the
platform's retry policy: 401/403 -> ``LLMAuthError`` (a ``PermissionError``:
permission_denied, terminal); other 4xx -> ``LLMRequestError`` (a
``ValueError``: validation, terminal); 5xx and network errors that outlast the
in-task retries -> ``LLMUnavailable`` (a ``ConnectionError``: transient).

The API key is read from ``api_key`` or the environment variable named by
``api_key_env`` -- supply it through the platform's secret mechanism, never in
a spec -- and it is never logged, never put in an exception message and never
shown by ``repr``. Every test and example in this repository points
``base_url`` at :class:`nexus.testing.MockLLMServer`; nothing here calls a
real provider unless you give it a real ``base_url`` and key.
"""

from __future__ import annotations

import dataclasses
import email.utils
import json
import math
import os
import random
import socket
import time
import urllib.error
import urllib.request
from typing import TYPE_CHECKING, Any, Literal, Mapping, Sequence

from .ratelimit import RateLimited
from .usage import usage_subject

if TYPE_CHECKING:
    from .context import TaskContext

__all__ = [
    "LLMClient",
    "Completion",
    "LLMError",
    "LLMAuthError",
    "LLMRequestError",
    "LLMUnavailable",
    "estimate_tokens",
    "parse_retry_after",
    "ANTHROPIC_VERSION",
]

ANTHROPIC_VERSION = "2023-06-01"
Provider = Literal["openai", "anthropic"]
# A provider error body is quoted in the exception, truncated to this.
_MAX_BODY_IN_ERROR = 500
_RETRYABLE_STATUS = frozenset({429, 500, 502, 503, 504})
_THROTTLE_STATUS = frozenset({429, 503})


class LLMError(Exception):
    """A model call failed. ``status`` is the HTTP status (0 if none)."""

    def __init__(self, message: str, *, status: int = 0, provider: str = "", model: str = "") -> None:
        super().__init__(message)
        self.status = status
        self.provider = provider
        self.model = model


class LLMAuthError(LLMError, PermissionError):
    """401/403: the key is missing, wrong or not allowed. Terminal."""


class LLMRequestError(LLMError, ValueError):
    """Another 4xx: the request itself is wrong. Terminal."""


class LLMUnavailable(LLMError, ConnectionError):
    """5xx or a network failure that outlasted the in-task retries. Transient."""


@dataclasses.dataclass(frozen=True, slots=True)
class Completion:
    """One model response: the text, token counts as the provider reported
    them, the model that answered, why it stopped, and the raw JSON."""

    text: str
    input_tokens: int
    output_tokens: int
    model: str
    finish_reason: str
    raw: Mapping[str, Any]
    cached_tokens: int = 0


def estimate_tokens(chars: int, max_tokens: int) -> int:
    """The permit estimate: ``ceil(chars / 4) + max_tokens``.

    Four characters per token is the usual rule of thumb for English; the
    estimate only has to be close, because it is settled against the real
    count afterwards.
    """
    return int(math.ceil(max(0, chars) / 4)) + max(0, max_tokens)


def parse_retry_after(value: str | None, *, now: float | None = None) -> float | None:
    """``Retry-After`` in seconds: delta-seconds or an HTTP date. None if absent
    or unparseable. Never negative."""
    if value is None:
        return None
    v = value.strip()
    if not v:
        return None
    try:
        return max(0.0, float(v))
    except ValueError:
        pass
    try:
        when = email.utils.parsedate_to_datetime(v)
    except (TypeError, ValueError, IndexError):
        return None
    if when is None:  # pragma: no cover - older Pythons returned None
        return None
    ref = time.time() if now is None else now
    return max(0.0, when.timestamp() - ref)


class LLMClient:
    """A chat-completions client bound to one task context, provider and model.

    Thread-safe: share one instance across a handler's worker threads. See
    the module docstring for the per-call flow.
    """

    def __init__(
        self,
        ctx: "TaskContext",
        *,
        provider: Provider,
        model: str,
        base_url: str,
        api_key: str | None = None,
        api_key_env: str | None = None,
        rate_limit_keys: Sequence[str] | None = None,
        usage_provider: str | None = None,
        max_retries: int = 5,
        max_retry_wait_s: float = 60.0,
        timeout_s: float = 60.0,
    ) -> None:
        if provider not in ("openai", "anthropic"):
            raise ValueError(f"provider must be 'openai' or 'anthropic', got {provider!r}")
        if not model:
            raise ValueError("model must be non-empty")
        if not base_url:
            raise ValueError("base_url must be non-empty, e.g. https://api.openai.com")
        if max_retries < 0 or max_retry_wait_s < 0 or timeout_s <= 0:
            raise ValueError("max_retries and max_retry_wait_s must be >= 0 and timeout_s > 0")
        key = api_key
        if key is None and api_key_env:
            key = os.environ.get(api_key_env)
            if not key:
                raise LLMAuthError(
                    f"environment variable {api_key_env} is not set; supply the key through the "
                    "platform's secret mechanism",
                    provider=provider, model=model,
                )
        self.ctx = ctx
        self.provider: Provider = provider
        self.model = model
        self.base_url = base_url.rstrip("/")
        self._api_key = key
        self.usage_provider = usage_provider or provider
        self.rate_limit_keys: tuple[str, ...] = tuple(
            rate_limit_keys
            if rate_limit_keys is not None
            else (usage_subject(provider), usage_subject(provider, model))
        )
        self.max_retries = max_retries
        self.max_retry_wait_s = max_retry_wait_s
        self.timeout_s = timeout_s

    def __repr__(self) -> str:
        # Never the key.
        return (
            f"LLMClient(provider={self.provider!r}, model={self.model!r}, "
            f"base_url={self.base_url!r}, api_key={'<set>' if self._api_key else None})"
        )

    # ── public ───────────────────────────────────────────────────────────────
    def complete(
        self,
        prompt: str | Sequence[Mapping[str, str]],
        *,
        max_tokens: int = 256,
        temperature: float = 0.0,
        system: str | None = None,
    ) -> Completion:
        """One chat completion. ``prompt`` is a user message, or a list of
        ``{"role", "content"}`` messages (a ``system`` role message is moved to
        Anthropic's top-level ``system`` field)."""
        if max_tokens <= 0:
            raise ValueError(f"max_tokens must be > 0, got {max_tokens}")
        messages = _messages(prompt)
        chars = sum(len(m["content"]) for m in messages) + len(system or "")
        estimate = estimate_tokens(chars, max_tokens)
        url, body, headers = self._request(messages, max_tokens, temperature, system)

        attempt = 0
        last_wait: float | None = None
        while True:
            self.ctx.acquire_permits(self.rate_limit_keys, requests=1, tokens=estimate)
            try:
                status, resp_headers, payload = self._post(url, body, headers)
            except (urllib.error.URLError, socket.timeout, TimeoutError, ConnectionError, OSError) as e:
                self._refund(estimate)
                if attempt >= self.max_retries:
                    raise LLMUnavailable(
                        f"{self.provider} {self.model}: request failed after {attempt + 1} tries: "
                        f"{type(e).__name__}: {_reason(e)}",
                        provider=self.provider, model=self.model,
                    ) from None
                last_wait = self._backoff(attempt, None)
                self.ctx.log.warning(
                    "llm request failed; retrying", provider=self.provider, model=self.model,
                    error=type(e).__name__, retry_in_s=round(last_wait, 3),
                )
                time.sleep(last_wait)
                attempt += 1
                continue

            if status < 300:
                completion = self._parse(payload)
                self._settle(estimate, completion)
                return completion

            self._refund(estimate)
            retry_after = _retry_after(resp_headers)
            if status in _RETRYABLE_STATUS:
                throttled = status in _THROTTLE_STATUS
                if retry_after is not None and retry_after > self.max_retry_wait_s and throttled:
                    raise RateLimited(
                        f"{self.provider} {self.model}: HTTP {status} with Retry-After "
                        f"{retry_after:.1f}s, over max_retry_wait_s={self.max_retry_wait_s}",
                        retry_after_s=retry_after, provider=self.provider, model=self.model,
                    )
                if attempt >= self.max_retries:
                    if throttled:
                        raise RateLimited(
                            f"{self.provider} {self.model}: still HTTP {status} after "
                            f"{attempt + 1} tries",
                            retry_after_s=retry_after if retry_after is not None else last_wait,
                            provider=self.provider, model=self.model,
                        )
                    raise LLMUnavailable(
                        f"{self.provider} {self.model}: HTTP {status} after {attempt + 1} tries: "
                        f"{_body_text(payload)}",
                        status=status, provider=self.provider, model=self.model,
                    )
                last_wait = self._backoff(attempt, retry_after)
                self.ctx.log.warning(
                    "llm throttled; retrying" if throttled else "llm server error; retrying",
                    provider=self.provider, model=self.model, status=status,
                    retry_in_s=round(last_wait, 3), attempt=attempt + 1,
                )
                time.sleep(last_wait)
                attempt += 1
                continue
            text = f"{self.provider} {self.model}: HTTP {status}: {_body_text(payload)}"
            if status in (401, 403):
                raise LLMAuthError(text, status=status, provider=self.provider, model=self.model)
            raise LLMRequestError(text, status=status, provider=self.provider, model=self.model)

    # ── internals ────────────────────────────────────────────────────────────
    def _request(
        self, messages: list[dict[str, str]], max_tokens: int, temperature: float, system: str | None
    ) -> tuple[str, bytes, dict[str, str]]:
        headers = {"Content-Type": "application/json", "Accept": "application/json"}
        if self.provider == "openai":
            msgs = ([{"role": "system", "content": system}] if system else []) + messages
            doc: dict[str, Any] = {
                "model": self.model, "messages": msgs, "max_tokens": max_tokens,
                "temperature": temperature,
            }
            if self._api_key:
                headers["Authorization"] = f"Bearer {self._api_key}"
            url = f"{self.base_url}/v1/chat/completions"
        else:
            systems = [m["content"] for m in messages if m["role"] == "system"]
            if system:
                systems.insert(0, system)
            doc = {
                "model": self.model,
                "messages": [m for m in messages if m["role"] != "system"],
                "max_tokens": max_tokens,
                "temperature": temperature,
            }
            if systems:
                doc["system"] = "\n\n".join(systems)
            if self._api_key:
                headers["x-api-key"] = self._api_key
            headers["anthropic-version"] = ANTHROPIC_VERSION
            url = f"{self.base_url}/v1/messages"
        return url, json.dumps(doc).encode("utf-8"), headers

    def _post(self, url: str, body: bytes, headers: Mapping[str, str]) -> tuple[int, Mapping[str, str], bytes]:
        req = urllib.request.Request(url, data=body, headers=dict(headers), method="POST")
        try:
            with urllib.request.urlopen(req, timeout=self.timeout_s) as resp:
                return int(resp.status), {k.lower(): v for k, v in resp.headers.items()}, resp.read()
        except urllib.error.HTTPError as e:
            data = e.read() if e.fp is not None else b""
            hdrs = {k.lower(): v for k, v in (e.headers or {}).items()}
            return int(e.code), hdrs, data

    def _parse(self, payload: bytes) -> Completion:
        try:
            doc = json.loads(payload)
        except ValueError as e:
            raise LLMUnavailable(
                f"{self.provider} {self.model}: response is not JSON: {e}",
                provider=self.provider, model=self.model,
            ) from None
        if not isinstance(doc, Mapping):
            raise LLMUnavailable(f"{self.provider} {self.model}: response is not an object")
        usage = doc.get("usage") if isinstance(doc.get("usage"), Mapping) else {}
        assert isinstance(usage, Mapping)
        if self.provider == "openai":
            choices = doc.get("choices") or [{}]
            first = choices[0] if isinstance(choices, list) and choices else {}
            message = first.get("message") or {}
            text = str(message.get("content") or "")
            finish = str(first.get("finish_reason") or "")
            input_tokens = int(usage.get("prompt_tokens") or 0)
            output_tokens = int(usage.get("completion_tokens") or 0)
            details = usage.get("prompt_tokens_details")
            cached = int(details.get("cached_tokens") or 0) if isinstance(details, Mapping) else 0
        else:
            blocks = doc.get("content") or []
            text = "".join(
                str(b.get("text") or "") for b in blocks if isinstance(b, Mapping) and b.get("type") == "text"
            )
            finish = str(doc.get("stop_reason") or "")
            input_tokens = int(usage.get("input_tokens") or 0)
            output_tokens = int(usage.get("output_tokens") or 0)
            cached = int(usage.get("cache_read_input_tokens") or 0)
        return Completion(
            text=text, input_tokens=input_tokens, output_tokens=output_tokens,
            model=str(doc.get("model") or self.model), finish_reason=finish, raw=doc,
            cached_tokens=cached,
        )

    def _settle(self, estimate: int, c: Completion) -> None:
        actual = c.input_tokens + c.output_tokens
        self.ctx.settle_permits(self.rate_limit_keys, actual - estimate)
        self.ctx.report_tokens(
            self.usage_provider, self.model, input_tokens=c.input_tokens,
            output_tokens=c.output_tokens, cached_tokens=c.cached_tokens, requests=1,
        )

    def _refund(self, estimate: int) -> None:
        """The provider refused the call: give the tokens back (the request
        permit stays spent -- it WAS a request against the limit)."""
        self.ctx.settle_permits(self.rate_limit_keys, -estimate)

    def _backoff(self, attempt: int, retry_after: float | None) -> float:
        """Retry-After when given, else exponential (0.5 s base) with full
        jitter; capped at max_retry_wait_s either way."""
        if retry_after is not None:
            wait = retry_after + random.uniform(0.0, min(1.0, 0.1 * retry_after + 0.01))
        else:
            wait = random.uniform(0.0, 0.5 * (2 ** attempt))
        return min(wait, self.max_retry_wait_s)


def _messages(prompt: str | Sequence[Mapping[str, str]]) -> list[dict[str, str]]:
    if isinstance(prompt, str):
        return [{"role": "user", "content": prompt}]
    out: list[dict[str, str]] = []
    for m in prompt:
        role = str(m.get("role") or "")
        if role not in ("system", "user", "assistant"):
            raise ValueError(f"message role must be system, user or assistant, got {role!r}")
        out.append({"role": role, "content": str(m.get("content") or "")})
    if not any(m["role"] != "system" for m in out):
        raise ValueError("prompt needs at least one user or assistant message")
    return out


def _retry_after(headers: Mapping[str, str]) -> float | None:
    ms = headers.get("retry-after-ms")
    if ms:
        try:
            return max(0.0, float(ms) / 1000.0)
        except ValueError:
            pass
    return parse_retry_after(headers.get("retry-after"))


def _body_text(payload: bytes) -> str:
    return payload[:_MAX_BODY_IN_ERROR].decode("utf-8", "replace")


def _reason(e: BaseException) -> str:
    if isinstance(e, urllib.error.URLError):
        return str(e.reason)
    return str(e)
