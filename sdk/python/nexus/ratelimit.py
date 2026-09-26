# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Rate limits: the RateLimited error, permit grants, and the local bucket.

Provider rate limits are enforced by the CONTROL PLANE (ADR-0032 §3): a
5,000-way fan-out spread over 40 workers must stay under one provider's
requests-per-second, and a per-process limiter would admit ``workers x limit``.
``TaskContext.acquire_permits`` asks the platform through the agent's workload
API, which grants all keys or none and never waits server-side; the SDK sleeps
``retry_after_ms`` (plus jitter) and asks again.

When there is no workload API -- the local runner, unit tests, a kubernetes
agent with no listener configured -- :class:`LocalRateLimiter` stands in: the same token-bucket
semantics, but process-local. It is UNLIMITED unless limits are configured in
``NEXUS_LOCAL_RATE_LIMITS``, and it says once, loudly, that its limits are not
shared with any other process.

Configure the platform's limits per namespace in the quotas document
(``PUT /v1/namespaces/{ns}/quotas``, key ``rate_limits``; OPERATIONS §29).
"""

from __future__ import annotations

import dataclasses
import json
import math
import os
import threading
import time
from typing import Any, Callable, Literal, Mapping, Sequence

__all__ = [
    "RateLimited",
    "PermitGrant",
    "RateLimit",
    "LocalRateLimiter",
    "parse_rate_limits",
    "local_limiter",
    "ENV_LOCAL_RATE_LIMITS",
]

ENV_LOCAL_RATE_LIMITS = "NEXUS_LOCAL_RATE_LIMITS"


class RateLimited(Exception):
    """A provider (or the platform's permit service) kept throttling past the
    in-task bound.

    Raise it -- or let it escape from ``nexus.llm.LLMClient`` or
    ``ctx.acquire_permits`` -- and the attempt fails with error class
    ``rate_limited``. The scheduler retries a RATE_LIMITED failure after
    ``retry_after_s`` (clamped to [1 s, 15 min]) WITHOUT charging the retry
    budget, up to a per-node cap (ADR-0032 §3): a throttle is the platform's
    concern, not the author's error.

    ``retry_after_s`` is the provider's ``Retry-After`` when it sent one,
    ``provider``/``model`` say which endpoint throttled (for the log and the
    error message; they are not sent upstream separately).
    """

    def __init__(
        self,
        message: str = "rate limited",
        *,
        retry_after_s: float | None = None,
        provider: str | None = None,
        model: str | None = None,
    ) -> None:
        super().__init__(message)
        self.retry_after_s = retry_after_s
        self.provider = provider
        self.model = model

    @property
    def retry_after_ms(self) -> int | None:
        """``retry_after_s`` in whole milliseconds (rounded up), or None."""
        if self.retry_after_s is None:
            return None
        return max(0, int(math.ceil(self.retry_after_s * 1000)))


@dataclasses.dataclass(frozen=True, slots=True)
class PermitGrant:
    """What ``ctx.acquire_permits`` returns once permits were granted.

    ``unlimited_keys`` lists keys with no configured limit (granted at no
    cost); ``source`` says who granted: ``"platform"`` (shared by the whole
    fleet) or ``"local"`` (this process only). ``waited_s`` is the time spent
    waiting for the grant and ``attempts`` the number of asks it took.
    """

    keys: tuple[str, ...]
    requests: int
    tokens: int
    unlimited_keys: tuple[str, ...] = ()
    waited_s: float = 0.0
    attempts: int = 1
    source: Literal["platform", "local"] = "platform"


@dataclasses.dataclass(frozen=True, slots=True)
class RateLimit:
    """One key's limit: requests per second and/or tokens per minute.

    Bursts default as the platform's do: one second of requests (at least 1)
    and one minute of tokens.
    """

    requests_per_second: float | None = None
    tokens_per_minute: int | None = None
    burst_requests: float | None = None
    burst_tokens: float | None = None

    def request_burst(self) -> float:
        if self.burst_requests is not None:
            return float(self.burst_requests)
        return max(1.0, float(self.requests_per_second or 0))

    def token_burst(self) -> float:
        if self.burst_tokens is not None:
            return float(self.burst_tokens)
        return float(self.tokens_per_minute or 0)


def parse_rate_limits(raw: str | Mapping[str, Any]) -> dict[str, RateLimit]:
    """Parse ``{"key": {"requests_per_second": .., "tokens_per_minute": ..}}``.

    The same shape as a namespace's ``rate_limits`` in the quotas document,
    so a limit can be tried locally before it is configured for real.
    """
    doc: Any = json.loads(raw) if isinstance(raw, str) else raw
    if not isinstance(doc, Mapping):
        raise ValueError("rate limits must be a JSON object of key -> limit")
    out: dict[str, RateLimit] = {}
    for key, spec in doc.items():
        if not isinstance(spec, Mapping):
            raise ValueError(f"rate limit {key!r} must be an object")
        rps = spec.get("requests_per_second")
        tpm = spec.get("tokens_per_minute")
        if rps is None and tpm is None:
            raise ValueError(f"rate limit {key!r} needs requests_per_second or tokens_per_minute")
        if rps is not None and (isinstance(rps, bool) or float(rps) <= 0):
            raise ValueError(f"rate limit {key!r}: requests_per_second must be > 0")
        if tpm is not None and (isinstance(tpm, bool) or int(tpm) <= 0):
            raise ValueError(f"rate limit {key!r}: tokens_per_minute must be > 0")
        br = spec.get("burst_requests")
        bt = spec.get("burst_tokens")
        out[str(key)] = RateLimit(
            requests_per_second=None if rps is None else float(rps),
            tokens_per_minute=None if tpm is None else int(tpm),
            burst_requests=None if br is None else float(br),
            burst_tokens=None if bt is None else float(bt),
        )
    return out


@dataclasses.dataclass(slots=True)
class _Bucket:
    requests: float
    tokens: float
    refreshed: float


class LocalRateLimiter:
    """A process-local, all-or-none token bucket with the platform's semantics.

    * Unconfigured keys are granted at once and returned as unlimited.
    * A denial returns the exact time until the slowest missing level refills.
    * A request larger than a burst is granted only from a full bucket and
      drives it negative (bounded at -burst): possible, but paid for by the
      calls after it.
    * ``settle`` applies a token debt or refund without a check, clamped to
      [-burst, burst].

    Thread-safe. NOT shared between processes: two workers each admit the full
    limit. That is why the platform's permit service exists.
    """

    def __init__(
        self, limits: Mapping[str, RateLimit] | None = None, *, clock: Callable[[], float] = time.monotonic
    ) -> None:
        self.limits: dict[str, RateLimit] = dict(limits or {})
        self._clock = clock
        self._buckets: dict[str, _Bucket] = {}
        self._lock = threading.Lock()

    def _bucket(self, key: str, limit: RateLimit, now: float) -> _Bucket:
        b = self._buckets.get(key)
        if b is None:
            b = _Bucket(limit.request_burst(), limit.token_burst(), now)
            self._buckets[key] = b
            return b
        elapsed = max(0.0, now - b.refreshed)
        if limit.requests_per_second:
            b.requests = min(limit.request_burst(), b.requests + elapsed * limit.requests_per_second)
        if limit.tokens_per_minute:
            b.tokens = min(limit.token_burst(), b.tokens + elapsed * limit.tokens_per_minute / 60.0)
        b.refreshed = now
        return b

    def acquire(self, keys: Sequence[str], requests: int, tokens: int) -> tuple[bool, int, tuple[str, ...]]:
        """One all-or-none ask: ``(granted, retry_after_ms, unlimited_keys)``."""
        with self._lock:
            now = self._clock()
            unlimited = tuple(k for k in keys if k not in self.limits)
            limited = sorted(k for k in set(keys) if k in self.limits)
            wait_s = 0.0
            for key in limited:
                limit = self.limits[key]
                b = self._bucket(key, limit, now)
                if limit.requests_per_second and requests > 0:
                    burst = limit.request_burst()
                    need = min(float(requests), burst)  # oversized: needs a full bucket
                    if b.requests < need:
                        wait_s = max(wait_s, (need - b.requests) / limit.requests_per_second)
                if limit.tokens_per_minute and tokens > 0:
                    burst = limit.token_burst()
                    need = min(float(tokens), burst)
                    if b.tokens < need:
                        wait_s = max(wait_s, (need - b.tokens) / (limit.tokens_per_minute / 60.0))
            if wait_s > 0:
                return False, max(1, int(math.ceil(wait_s * 1000))), unlimited
            for key in limited:
                limit = self.limits[key]
                b = self._buckets[key]
                if limit.requests_per_second:
                    b.requests = max(-limit.request_burst(), b.requests - requests)
                if limit.tokens_per_minute:
                    b.tokens = max(-limit.token_burst(), b.tokens - tokens)
            return True, 0, unlimited

    def settle(self, keys: Sequence[str], tokens: int) -> None:
        """Apply ``tokens`` of debt (positive) or refund (negative)."""
        with self._lock:
            now = self._clock()
            for key in sorted(set(keys)):
                limit = self.limits.get(key)
                if limit is None or not limit.tokens_per_minute:
                    continue
                b = self._bucket(key, limit, now)
                burst = limit.token_burst()
                b.tokens = min(burst, max(-burst, b.tokens - tokens))


_local_lock = threading.Lock()
_local: tuple[str, LocalRateLimiter] | None = None


def local_limiter(env: Mapping[str, str] | None = None) -> LocalRateLimiter:
    """The process's shared fallback limiter, configured from the environment.

    One instance per process (per distinct ``NEXUS_LOCAL_RATE_LIMITS`` value),
    so every context in the process -- the local runner's in-process attempts,
    a handler's threads -- draws from the same buckets. An unparseable value
    is an error, not silently "unlimited": a limit someone configured and the
    SDK ignored would be the worst of both.
    """
    global _local
    e = os.environ if env is None else env
    raw = e.get(ENV_LOCAL_RATE_LIMITS, "").strip()
    with _local_lock:
        if _local is not None and _local[0] == raw:
            return _local[1]
        try:
            limits = parse_rate_limits(raw) if raw else {}
        except (ValueError, TypeError) as err:
            raise ValueError(f"{ENV_LOCAL_RATE_LIMITS} is invalid: {err}") from err
        limiter = LocalRateLimiter(limits)
        _local = (raw, limiter)
        return limiter
