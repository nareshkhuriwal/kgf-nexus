# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""TaskContext: what a handler is given at execution time, inside the worker.

This module runs on the WORKER, not in the control plane and not at compile time.
It is the other half of the SDK: `task.py`/`workflow.py` compile a document,
this file executes a handler against a dispatched attempt.

The most important thing here is the effect guard (architecture §6.2). Read
`TaskContext.record_effect` before writing a handler that touches anything
outside the result store.

The context is also the task's only channel to the control plane (ADR-0032):
heartbeats and progress, usage units, shared rate-limit permits and
checkpoints go through the agent's per-attempt workload API when there is one
(``NEXUS_TASK_API``), and fall back to in-process behaviour when there is not
(the local runner, unit tests, the kubernetes executor). Handlers call the same
methods either way.
"""

from __future__ import annotations

import base64
import dataclasses
import hashlib
import json
import logging
import os
import random
import sys
import threading
import time
from typing import Any, Callable, Mapping, Protocol, Sequence, TextIO

from ._taskapi import (
    Capabilities,
    CheckpointInfo,
    CheckpointTooLarge,
    StaleLease,
    TaskAPIClient,
    TaskAPIError,
    Unavailable,
    Unsupported,
)
from .canonical import canonical_bytes
from .ratelimit import PermitGrant, RateLimited, local_limiter
from .usage import InvalidUsage, MAX_KEYS, merge_units, token_units, validate_subject, validate_usage

__all__ = [
    "TaskContext",
    "ResultStore",
    "InMemoryResultStore",
    "FileResultStore",
    "StructuredLogger",
    "derive_idempotency_key",
    "EffectNotRecorded",
    "DEFAULT_INLINE_THRESHOLD_BYTES",
    "MAX_INLINE_THRESHOLD_BYTES",
    "resolve_inline_threshold",
    "CheckpointInfo",
    "MAX_CHECKPOINT_BYTES",
]


class EffectNotRecorded(LookupError):
    """Raised by `previous_result` for an effect that was never recorded."""


def derive_idempotency_key(
    run_id: str, node_id: str, map_index: int | str | None, task_version: int | str
) -> str:
    """Derive the platform idempotency key. THE PLATFORM OWNS THIS, not users.

        sha256(run_id ‖ node_id ‖ map_index|"-" ‖ task_version)

    `attempt_number` is deliberately EXCLUDED (CONTRACT.md invariant 1, §6.2).
    All retries of the same logical work therefore share one key -- that
    exclusion is the entire mechanism of at-least-once-plus-dedup. Including the
    attempt number would make every retry a fresh effect and turn a downstream
    system's deduplication into a no-op.

    This function exists in the SDK so a worker can verify the key the control
    plane sent, and so tests can construct a real one. It is NOT a hook for users
    to invent their own keys: hand-rolled keys are the most common source of
    idempotency bugs in practice, which is why the platform derives them.
    """
    if not run_id or not node_id:
        raise ValueError("idempotency key needs a non-empty run_id and node_id")
    mi = "-" if map_index is None or map_index == "" else str(map_index)
    # \x1f (unit separator), exactly as domain.IdempotencyKey joins the fields.
    # This used \x00, so every real assignment's key "differed" from the local
    # derivation and the worker logged a mismatch (CONTRACT-FINDINGS F116).
    payload = "\x1f".join((run_id, node_id, mi, str(task_version)))
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


# ── Result store ──────────────────────────────────────────────────────────────


class ResultStore(Protocol):
    """The result store interface. Payloads never transit the control plane.

    Workers read and write object storage directly (CONTRACT.md invariant 6,
    ADR-0008): the control plane holds a reference, never the bytes. That is what
    keeps a 40 GiB model artifact from flowing through the API gateway.
    """

    def put(self, value: Any, *, key: str | None = None) -> Mapping[str, Any]:
        """Store a value; return a ResultRef-shaped dict."""
        ...

    def get(self, ref: Mapping[str, Any] | str) -> Any:
        """Fetch a value by ResultRef or by key."""
        ...

    def exists(self, key: str) -> bool: ...


# Small results are inlined rather than stored as blobs, to avoid two network
# round-trips for `{"rows": 42}` (ADR-0008). The boundary is the CONTROL
# PLANE's (ADR-0016): configured on the gateway, clamped into [1 B, 1 MiB] by the
# agent, and exported to the workload as NEXUS_INLINE_THRESHOLD_BYTES. These
# mirror internal/domain/inline.go.
DEFAULT_INLINE_THRESHOLD_BYTES = 64 * 1024
MAX_INLINE_THRESHOLD_BYTES = 1 << 20
#: Deprecated alias of DEFAULT_INLINE_THRESHOLD_BYTES; stores now take the
#: configured value (see resolve_inline_threshold).
INLINE_MAX_BYTES = DEFAULT_INLINE_THRESHOLD_BYTES
ENV_INLINE_THRESHOLD = "NEXUS_INLINE_THRESHOLD_BYTES"


def clamp_inline_threshold(n: int) -> int:
    """domain.ClampInlineThreshold: unset/invalid -> default, too large -> max."""
    if n <= 0:
        return DEFAULT_INLINE_THRESHOLD_BYTES
    return min(n, MAX_INLINE_THRESHOLD_BYTES)


def resolve_inline_threshold(
    assignment: Mapping[str, Any] | None = None,
    environ: Mapping[str, str] | None = None,
) -> int:
    """The inline-versus-blob boundary this worker process should apply.

    In order: the assignment's ``result_store.inline_threshold_bytes`` (the
    replay endpoint carries the gateway's value), then
    ``NEXUS_INLINE_THRESHOLD_BYTES`` (exported by the agent to every workload),
    then the platform default. Whatever is found is clamped exactly as the agent
    clamps it, so a bad value can shrink or grow inlining only within the range
    the control plane accepts.
    """
    env = os.environ if environ is None else environ
    candidates: list[object] = []
    if assignment is not None:
        rs = assignment.get("result_store")
        if isinstance(rs, Mapping):
            candidates.append(rs.get("inline_threshold_bytes"))
    candidates.append(env.get(ENV_INLINE_THRESHOLD))
    for raw in candidates:
        if raw is None or raw == "" or isinstance(raw, bool):
            continue
        try:
            value = int(str(raw))
        except ValueError:
            continue
        return clamp_inline_threshold(value)
    return DEFAULT_INLINE_THRESHOLD_BYTES


def _encode(value: Any) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode("utf-8")


def decode_payload(data: bytes, content_type: str | None = None) -> Any:
    """Turn stored bytes back into a handler value.

    JSON is decoded; anything else (a CSV a shell task wrote, say) is returned
    as ``str`` when it is valid UTF-8 and as ``bytes`` otherwise. The platform
    never interprets an output's bytes, so neither does this beyond that.
    """
    if content_type in (None, "", "application/json"):
        try:
            return json.loads(data)
        except ValueError:
            pass
    try:
        return data.decode("utf-8")
    except UnicodeDecodeError:
        return data


def inline_ref(data: bytes, content_type: str | None = None) -> dict[str, Any]:
    """An inline ResultRef for raw bytes, in the SDK's native form.

    UTF-8 JSON rides as text under ``inline`` (what ``put`` has always
    produced); any other payload as base64 under ``inline_base64``, so a ref
    always says which of the two it is instead of leaving a reader to guess.
    """
    ct = content_type or ("application/json" if _is_json(data) else "application/octet-stream")
    if ct == "application/json":
        try:
            return {"inline": data.decode("utf-8"), "content_type": ct, "size_bytes": len(data)}
        except UnicodeDecodeError:
            pass
    return {
        "inline_base64": base64.b64encode(data).decode("ascii"),
        "content_type": ct,
        "size_bytes": len(data),
    }


def _is_json(data: bytes) -> bool:
    try:
        json.loads(data)
    except ValueError:
        return False
    return True


def ref_payload(ref: Mapping[str, Any]) -> bytes | None:
    """The bytes of an INLINE ref, or None for a blob ref."""
    if "inline" in ref:
        raw = ref["inline"]
        return raw.encode("utf-8") if isinstance(raw, str) else bytes(raw)
    if "inline_base64" in ref:
        return base64.b64decode(str(ref["inline_base64"]))
    return None


class InMemoryResultStore:
    """A result store backed by a dict. For unit tests and the subprocess executor.

    ``inline_max_bytes`` defaults to the configured threshold
    (:func:`resolve_inline_threshold`), not a compiled-in constant.
    """

    def __init__(self, *, inline_max_bytes: int | None = None) -> None:
        self._blobs: dict[str, bytes] = {}
        self.inline_max_bytes = (
            resolve_inline_threshold()
            if inline_max_bytes is None
            else clamp_inline_threshold(inline_max_bytes)
        )

    def put(self, value: Any, *, key: str | None = None) -> dict[str, Any]:
        data = _encode(value)
        digest = hashlib.sha256(data).hexdigest()
        if len(data) <= self.inline_max_bytes and key is None:
            return {
                "inline": data.decode("utf-8"),
                "content_type": "application/json",
                "size_bytes": len(data),
            }
        k = key or digest
        self._blobs[k] = data
        return {
            "blob": {"bucket": "memory", "key": k, "sha256": digest},
            "content_type": "application/json",
            "size_bytes": len(data),
        }

    def get(self, ref: Mapping[str, Any] | str) -> Any:
        if isinstance(ref, str):
            return json.loads(self._blobs[ref])
        inline = ref_payload(ref)
        if inline is not None:
            return decode_payload(inline, ref.get("content_type"))
        return decode_payload(self._blobs[ref["blob"]["key"]], ref.get("content_type"))

    def exists(self, key: str) -> bool:
        return key in self._blobs


class FileResultStore:
    """A result store backed by a local directory.

    Used by the subprocess executor and by the effect journal when no object
    store is configured. Writes are atomic (write-temp-then-rename) because the
    effect journal's value depends on a recorded effect being either fully there
    or absent -- a half-written journal entry would be worse than none.
    """

    def __init__(self, root: str, *, inline_max_bytes: int | None = None) -> None:
        self.root = os.path.abspath(root)
        os.makedirs(self.root, exist_ok=True)
        self.inline_max_bytes = (
            resolve_inline_threshold()
            if inline_max_bytes is None
            else clamp_inline_threshold(inline_max_bytes)
        )

    def _path(self, key: str) -> str:
        safe = key.replace("/", "_").replace("..", "_")
        return os.path.join(self.root, safe + ".json")

    def put(self, value: Any, *, key: str | None = None) -> dict[str, Any]:
        data = _encode(value)
        digest = hashlib.sha256(data).hexdigest()
        if len(data) <= self.inline_max_bytes and key is None:
            return {
                "inline": data.decode("utf-8"),
                "content_type": "application/json",
                "size_bytes": len(data),
            }
        k = key or digest
        self._write(k, data)
        return {
            "blob": {"bucket": "file", "key": k, "sha256": digest},
            "content_type": "application/json",
            "size_bytes": len(data),
        }

    def _write(self, key: str, data: bytes) -> None:
        path = self._path(key)
        # pid AND thread: the local runner writes from several threads of one
        # process, and two of them storing the same content would otherwise
        # share a temp file.
        tmp = path + f".tmp.{os.getpid()}.{threading.get_ident()}"
        with open(tmp, "wb") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)

    def put_bytes(self, data: bytes, content_type: str | None = None) -> dict[str, Any]:
        """Store raw bytes: inline at or under the threshold, else a
        content-addressed blob. The byte-level twin of ``put``, for outputs that
        are not JSON values (what a shell task writes to $NEXUS_OUTPUT_DIR)."""
        if len(data) <= self.inline_max_bytes:
            return inline_ref(data, content_type)
        digest = hashlib.sha256(data).hexdigest()
        self._write(digest, data)
        return {
            "blob": {"bucket": "file", "key": digest, "sha256": digest},
            "content_type": content_type
            or ("application/json" if _is_json(data) else "application/octet-stream"),
            "size_bytes": len(data),
        }

    def get_bytes(self, ref: Mapping[str, Any]) -> bytes:
        """The stored bytes of a ref, inline or blob."""
        inline = ref_payload(ref)
        if inline is not None:
            return inline
        with open(self._path(str(ref["blob"]["key"])), "rb") as f:
            return f.read()

    def get(self, ref: Mapping[str, Any] | str) -> Any:
        if isinstance(ref, str):
            with open(self._path(ref), "rb") as f:
                return json.loads(f.read())
        return decode_payload(self.get_bytes(ref), ref.get("content_type"))

    def exists(self, key: str) -> bool:
        return os.path.exists(self._path(key))


# ── Structured logging ────────────────────────────────────────────────────────


class StructuredLogger:
    """Structured JSON logging with run_id/attempt_id on every line.

    Correlation fields are attached by the context rather than by the handler,
    because a log line that cannot be tied back to an attempt is nearly useless
    when you are looking at one bad run out of ten thousand (architecture §9).
    """

    def __init__(self, fields: Mapping[str, Any], stream: TextIO | None = None) -> None:
        self._fields = dict(fields)
        self._stream = stream if stream is not None else sys.stderr

    def bind(self, **fields: Any) -> "StructuredLogger":
        return StructuredLogger({**self._fields, **fields}, self._stream)

    def _emit(self, level: str, msg: str, fields: Mapping[str, Any]) -> None:
        record = {
            "ts": time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime()) + "Z",
            "level": level,
            "msg": msg,
            **self._fields,
            **fields,
        }
        try:
            line = json.dumps(record, default=repr, sort_keys=False)
        except (TypeError, ValueError):  # pragma: no cover - default=repr covers most
            line = json.dumps({"level": level, "msg": msg, "log_encode_error": True})
        print(line, file=self._stream, flush=True)

    def debug(self, msg: str, **fields: Any) -> None:
        self._emit("DEBUG", msg, fields)

    def info(self, msg: str, **fields: Any) -> None:
        self._emit("INFO", msg, fields)

    def warning(self, msg: str, **fields: Any) -> None:
        self._emit("WARN", msg, fields)

    warn = warning

    def error(self, msg: str, **fields: Any) -> None:
        self._emit("ERROR", msg, fields)


# ── TaskContext ───────────────────────────────────────────────────────────────

HeartbeatFn = Callable[[str | None, float | None], None]

#: Heartbeats/progress closer together than this are coalesced (not sent).
PROGRESS_MIN_INTERVAL_S = 1.0
#: How long acquire_permits tolerates an unreachable permit service.
PERMIT_UNAVAILABLE_LIMIT_S = 60.0
#: The platform's default checkpoint limit (WorkloadApiConfig.max_checkpoint_bytes).
MAX_CHECKPOINT_BYTES = 64 << 20

_local_permit_warned = False
_local_permit_lock = threading.Lock()


def _warn_local_permits(log: "StructuredLogger") -> None:
    """Say ONCE per process that permits are not the platform's."""
    global _local_permit_warned
    with _local_permit_lock:
        if _local_permit_warned:
            return
        _local_permit_warned = True
    limiter = local_limiter()
    if limiter.limits:
        log.warning(
            "no workload API: rate limits from NEXUS_LOCAL_RATE_LIMITS are enforced in this "
            "process only and are NOT shared with any other worker",
            keys=sorted(limiter.limits),
        )
    else:
        log.warning(
            "no workload API: permits are granted without limit in this process "
            "(set NEXUS_LOCAL_RATE_LIMITS to try limits locally); nothing is shared"
        )


def _permit_keys(keys: str | Sequence[str]) -> list[str]:
    """Normalise permit keys: a list of distinct subjects, order kept."""
    raw = [keys] if isinstance(keys, str) else list(keys)
    if not raw:
        raise ValueError("acquire_permits needs at least one key, e.g. 'openai/gpt-4o-mini'")
    out: list[str] = []
    for k in raw:
        validate_subject(k)
        if k not in out:
            out.append(k)
    return out


def _check_count(name: str, value: int) -> None:
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise ValueError(f"{name} must be a non-negative int, got {value!r}")


@dataclasses.dataclass(slots=True)
class TaskContext:
    """Everything a handler is given about its own execution.

    Constructed by the worker agent from the TaskAssignment it polled. Handlers
    receive it as their first argument.
    """

    run_id: str
    node_id: str
    attempt_id: str
    attempt_number: int
    task_ref: str
    task_version: int
    idempotency_key: str
    idempotency_mode: str = "required"
    map_index: int | None = None
    namespace: str = "default"
    worker_id: str = ""
    lease_token: str = ""
    start_to_close_deadline_unix_ms: int = 0
    store: ResultStore = dataclasses.field(default_factory=InMemoryResultStore)
    log: StructuredLogger = dataclasses.field(default=None)  # type: ignore[assignment]
    inputs: Mapping[str, Any] = dataclasses.field(default_factory=dict)
    _heartbeat_fn: HeartbeatFn | None = None
    _effects: dict[str, Any] = dataclasses.field(default_factory=dict)
    _effects_loaded: bool = False
    # The attempt's workload API (ADR-0032 §1), or None: see the module doc.
    _task_api: TaskAPIClient | None = None
    _capabilities: Capabilities | None = None
    # Usage reported by this attempt: totals, and increments not yet delivered.
    _usage: dict[str, int] = dataclasses.field(default_factory=dict)
    _usage_pending: dict[str, int] = dataclasses.field(default_factory=dict)
    _checkpoint_seq: int = 0
    _last_progress_at: float = 0.0
    _lock: threading.Lock = dataclasses.field(
        default_factory=threading.Lock, repr=False, compare=False
    )

    def __post_init__(self) -> None:
        if self.log is None:
            self.log = StructuredLogger(
                {
                    "run_id": self.run_id,
                    "node_id": self.node_id,
                    "attempt_id": self.attempt_id,
                    "attempt": self.attempt_number,
                    "task_ref": self.task_ref,
                }
            )

    # ── progress signalling ─────────────────────────────────────────────────
    def heartbeat(self, message: str | None = None) -> None:
        """Report liveness. Silence longer than `timeouts.heartbeat` means retry.

        A long-running task that does not heartbeat is indistinguishable from a
        wedged one, so the platform assumes the worse of the two. Call this from
        inside your loops.

        Under a worker agent this reaches the control plane through the
        workload API (``POST /v1/progress``); before ADR-0032 it was a silent
        no-op there (F148). Calls closer together than
        ``PROGRESS_MIN_INTERVAL_S`` (1 s) are coalesced, so a tight loop may
        call it freely. Raises ``StaleLease`` if the platform has taken the
        attempt away -- stop working when it does. Any other delivery failure
        is logged, never raised: a heartbeat must not be what fails a task.
        """
        if self._heartbeat_fn is not None:
            self._heartbeat_fn(message, None)
        elif not self._forward_progress(message, None) and message:
            self.log.debug("heartbeat", detail=message)

    def progress(self, fraction: float, message: str | None = None) -> None:
        """Report fractional progress in [0.0, 1.0]. Also counts as a heartbeat.

        Delivered like :meth:`heartbeat`; a fraction of 1.0 is never coalesced.
        """
        if not 0.0 <= fraction <= 1.0:
            raise ValueError(f"progress fraction must be in [0,1], got {fraction}")
        if self._heartbeat_fn is not None:
            self._heartbeat_fn(message, fraction)
        elif not self._forward_progress(message, fraction):
            self.log.debug("progress", fraction=fraction, detail=message)

    # ── the workload API (ADR-0032) ─────────────────────────────────────────
    def _caps(self) -> Capabilities:
        """What the agent offers, asked once. No API -> nothing offered.

        An agent that cannot answer ``/v1/info`` is treated as offering
        nothing for the rest of the attempt (logged): the fallbacks keep the
        task running, and a local socket that fails once is not coming back.
        """
        api = self._task_api
        if api is None:
            return Capabilities.none()
        caps = self._capabilities
        if caps is None:
            try:
                caps = api.capabilities()
            except TaskAPIError as e:
                self.log.warning(
                    "workload API unreachable; usage, permits and checkpoints fall back "
                    "to in-process behaviour for this attempt",
                    error=str(e),
                )
                caps = Capabilities.none()
            self._capabilities = caps
        return caps

    def _disable(self, capability: str) -> None:
        """A 501 for a capability /v1/info advertised: stop asking for it."""
        self._capabilities = dataclasses.replace(self._caps(), **{capability: False})
        self.log.warning(
            "workload API does not offer this capability; using the in-process fallback",
            capability=capability,
        )

    def _forward_progress(self, message: str | None, fraction: float | None) -> bool:
        """Send progress to the agent. False when there is nowhere to send it."""
        api = self._task_api
        if api is None or not self._caps().progress:
            return False
        now = time.monotonic()
        with self._lock:
            if (
                fraction != 1.0
                and self._last_progress_at
                and now - self._last_progress_at < PROGRESS_MIN_INTERVAL_S
            ):
                return True
            self._last_progress_at = now
        try:
            api.progress(message, fraction)
        except StaleLease:
            raise
        except Unsupported:
            self._disable("progress")
            return False
        except TaskAPIError as e:
            self.log.warning("progress not delivered to the agent", error=str(e))
        return True

    # ── usage units (ADR-0032 §2) ───────────────────────────────────────────
    def report_usage(self, units: Mapping[str, int]) -> dict[str, int]:
        """Report usage INCREMENTS, e.g. ``{"images:openai/dall-e-3": 2}``.

        Keys follow the convention in :mod:`nexus.usage` (validated here, so a
        bad key fails at this call). Values add to the attempt's totals, which
        reach the usage ledger atomically with the attempt's end -- including
        for an attempt that later fails, because tokens spent were spent.
        Returns the attempt's totals so far.

        Without a workload API the totals accumulate on this context and are
        logged when the task ends. If the agent is briefly unreachable the
        increments are kept and resent with the next report (and once more
        when the task ends); they are never sent twice.
        """
        inc = {k: v for k, v in validate_usage(units).items() if v}
        if not inc:
            return self.usage_totals
        with self._lock:
            new_keys = set(inc) - set(self._usage)
            if len(self._usage) + len(new_keys) > MAX_KEYS:
                raise InvalidUsage(
                    f"usage would have {len(self._usage) + len(new_keys)} keys; "
                    f"at most {MAX_KEYS} per attempt"
                )
            merge_units(self._usage, inc)
            merge_units(self._usage_pending, inc)
        self._flush_usage()
        return self.usage_totals

    def report_tokens(
        self,
        provider: str,
        model: str,
        *,
        input_tokens: int = 0,
        output_tokens: int = 0,
        cached_tokens: int = 0,
        requests: int = 1,
    ) -> dict[str, int]:
        """Report one model call's tokens under the agreed key names.

        Shorthand for ``report_usage(nexus.usage.token_units(...))``;
        ``requests`` defaults to 1 because one call is the common case.
        """
        return self.report_usage(
            token_units(
                provider, model, input_tokens=input_tokens, output_tokens=output_tokens,
                cached_tokens=cached_tokens, requests=requests,
            )
        )

    @property
    def usage_totals(self) -> dict[str, int]:
        """This attempt's usage totals so far (a copy)."""
        with self._lock:
            return dict(self._usage)

    def _flush_usage(self) -> None:
        api = self._task_api
        if api is None or not self._caps().usage:
            with self._lock:
                self._usage_pending.clear()
            return
        with self._lock:
            pending, self._usage_pending = self._usage_pending, {}
        if not pending:
            return
        try:
            totals = api.report_usage(pending)
        except StaleLease:
            raise
        except Unsupported:
            self._disable("usage")
            return
        except Unavailable as e:
            with self._lock:
                merge_units(self._usage_pending, pending)
            self.log.warning("usage not delivered yet; will resend", error=str(e))
            return
        except TaskAPIError as e:
            raise InvalidUsage(f"the platform refused the usage report: {e}") from e
        with self._lock:
            # Max-merge, as the scheduler does: another process of this attempt
            # (a subprocess the handler started) may have reported too.
            for k, v in totals.items():
                if v > self._usage.get(k, 0):
                    self._usage[k] = v

    # ── shared rate limits (ADR-0032 §3) ────────────────────────────────────
    def acquire_permits(
        self,
        keys: str | Sequence[str],
        *,
        requests: int = 1,
        tokens: int = 0,
        timeout: float | None = None,
    ) -> PermitGrant:
        """Block until the platform grants ``requests`` and ``tokens`` on ``keys``.

        ``keys`` are subjects, e.g. ``["openai", "openai/gpt-4o-mini"]``; the
        grant is all keys or none, and keys with no configured limit are
        granted at once (``grant.unlimited_keys``). Limits are configured per
        namespace in the quotas document and shared by every worker and
        replica, so a 5,000-way fan-out stays under one provider limit.

        On a denial this sleeps the platform's ``retry_after_ms`` plus a little
        jitter and asks again -- nothing is held on the control plane while it
        waits. Raises :class:`nexus.RateLimited` when ``timeout`` seconds (or
        the task's start_to_close deadline, whichever is sooner) would pass
        before a grant: the attempt then fails as ``rate_limited`` and the
        platform retries it later without charging the retry budget. A control
        plane that stays unreachable for 60 s raises ``Unavailable``
        (TRANSIENT) rather than waiting forever.

        Ask for your ESTIMATED tokens (prompt + max output), then call
        :meth:`settle_permits` with ``actual - estimate`` once you know.

        Without a workload API, a process-local bucket stands in: unlimited
        unless ``NEXUS_LOCAL_RATE_LIMITS`` configures limits, and in either
        case not shared with any other process (logged once).
        """
        key_list = _permit_keys(keys)
        _check_count("requests", requests)
        _check_count("tokens", tokens)
        if timeout is not None and timeout < 0:
            raise ValueError(f"timeout must be >= 0, got {timeout}")
        started = time.monotonic()
        limits_at: list[float] = []
        if timeout is not None:
            limits_at.append(started + timeout)
        remaining = self.remaining_ms()
        if remaining is not None:
            limits_at.append(started + remaining / 1000.0)
        give_up_at = min(limits_at) if limits_at else None
        attempts = 0
        unavailable_since: float | None = None
        backoff = 0.25
        while True:
            attempts += 1
            platform = self._task_api is not None and self._caps().rate_limits
            try:
                if platform:
                    assert self._task_api is not None
                    d = self._task_api.acquire_permits(key_list, requests, tokens)
                    granted, retry_ms, unlimited = d.granted, d.retry_after_ms, d.unlimited_keys
                else:
                    _warn_local_permits(self.log)
                    granted, retry_ms, unlimited = local_limiter().acquire(key_list, requests, tokens)
                unavailable_since = None
            except Unsupported:
                self._disable("rate_limits")
                continue
            except Unavailable as e:
                now = time.monotonic()
                unavailable_since = unavailable_since or now
                if now - unavailable_since >= PERMIT_UNAVAILABLE_LIMIT_S:
                    raise
                self.log.warning("permit service unavailable; retrying", error=str(e))
                granted, retry_ms, unlimited = False, int(backoff * 1000), ()
                backoff = min(5.0, backoff * 2)
            now = time.monotonic()
            if granted:
                return PermitGrant(
                    keys=tuple(key_list), requests=requests, tokens=tokens,
                    unlimited_keys=tuple(unlimited), waited_s=now - started,
                    attempts=attempts, source="platform" if platform else "local",
                )
            wait = retry_ms / 1000.0
            wait += random.uniform(0.0, min(1.0, 0.1 * wait + 0.01))
            if give_up_at is not None and now + wait > give_up_at:
                raise RateLimited(
                    f"permits for {', '.join(key_list)} (requests={requests}, tokens={tokens}) "
                    f"not granted after {now - started:.1f}s and {attempts} asks",
                    retry_after_s=retry_ms / 1000.0,
                    provider=key_list[0].split("/", 1)[0],
                    model=next((k.split("/", 1)[1] for k in key_list if "/" in k), None),
                )
            time.sleep(wait)

    def settle_permits(self, keys: str | Sequence[str], tokens: int) -> None:
        """Settle an estimate: ``tokens = actual - estimate`` (debt or refund).

        Applied without a check, so tokens-per-minute is exact over time, not
        merely an upper bound. A settlement the agent cannot deliver is logged
        and dropped: the limit is then slightly off for a minute, which is not
        worth failing a task over.
        """
        key_list = _permit_keys(keys)
        if isinstance(tokens, bool) or not isinstance(tokens, int):
            raise TypeError(f"tokens must be an int, got {type(tokens).__name__}")
        if tokens == 0:
            return
        if self._task_api is not None and self._caps().rate_limits:
            try:
                self._task_api.settle_permits(key_list, tokens)
                return
            except StaleLease:
                raise
            except Unsupported:
                self._disable("rate_limits")
            except TaskAPIError as e:
                self.log.warning("permit settlement not delivered", error=str(e), tokens=tokens)
                return
        local_limiter().settle(key_list, tokens)

    # ── checkpoints (ADR-0032 §4) ───────────────────────────────────────────
    def checkpoint(self, state: Any) -> CheckpointInfo:
        """Durably save ``state`` so a retry of this work can :meth:`resume`.

        ``state`` must be JSON-encodable; it is stored as canonical JSON (at
        most 64 MiB). Checkpoints are keyed by the idempotency key, which every
        retry of this node shares, so attempt 2 resumes where attempt 1
        stopped. Under a worker agent each checkpoint is a new immutable object
        and the platform's pointer only moves forward: an attempt that has been
        superseded gets ``StaleLease``/``StaleCheckpoint`` instead of
        overwriting newer progress. Stop when you see either.

        A checkpoint is at-least-once STATE, not a transaction with your
        external effects: work done after the last checkpoint is redone by the
        retry, so the effect journal and idempotency keys still apply.

        Without a workload API it is written to ``ctx.store`` under
        ``checkpoints/<idempotency_key>``, which the local runner keeps across
        retries.
        """
        data = canonical_bytes(state)
        if len(data) > MAX_CHECKPOINT_BYTES:
            raise CheckpointTooLarge(
                f"checkpoint is {len(data)} bytes, over the {MAX_CHECKPOINT_BYTES}-byte limit; "
                "checkpoint a cursor into your data, not the data",
                status=413, code="too_large",
            )
        api = self._task_api
        if api is not None and self._caps().checkpoints:
            try:
                return api.put_checkpoint(data)
            except Unsupported:
                self._disable("checkpoints")
        digest = hashlib.sha256(data).hexdigest()
        with self._lock:
            self._checkpoint_seq += 1
            seq = self._checkpoint_seq
        key = self._checkpoint_key()
        self.store.put(
            {
                "idempotency_key": self.idempotency_key,
                "attempt_id": self.attempt_id,
                "attempt_number": self.attempt_number,
                "seq": seq,
                "saved_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "sha256": digest,
                "state": json.loads(data),
            },
            key=key,
        )
        return CheckpointInfo(seq=seq, key=key, sha256=digest, size_bytes=len(data))

    def resume(self) -> Any | None:
        """The state of the latest checkpoint of this work, or None.

        None when no attempt checkpointed yet, or when the stored checkpoint
        cannot be read (logged): starting from scratch is always correct, just
        slower. An agent that is unreachable raises ``Unavailable`` instead,
        so the platform retries rather than silently redoing hours of work.
        """
        api = self._task_api
        if api is not None and self._caps().checkpoints:
            try:
                stored = api.get_checkpoint()
            except Unsupported:
                self._disable("checkpoints")
            else:
                if stored is None:
                    return None
                try:
                    state = json.loads(stored.data)
                except ValueError as e:
                    self.log.warning("checkpoint unreadable; starting from scratch", error=str(e))
                    return None
                self.log.info(
                    "resuming from checkpoint", seq=stored.seq, written_by_attempt=stored.attempt,
                    saved_at=stored.saved_at,
                )
                return state
        key = self._checkpoint_key()
        try:
            if not self.store.exists(key):
                return None
            doc = self.store.get(key)
        except (OSError, ValueError, KeyError) as e:
            self.log.warning("checkpoint unreadable; starting from scratch", error=str(e))
            return None
        if not isinstance(doc, Mapping) or "state" not in doc:
            self.log.warning("checkpoint malformed; starting from scratch")
            return None
        self.log.info(
            "resuming from checkpoint", seq=doc.get("seq"),
            written_by_attempt=doc.get("attempt_number"), saved_at=doc.get("saved_at"),
        )
        return doc["state"]

    def _checkpoint_key(self) -> str:
        return f"checkpoints/{self.idempotency_key}"

    def _finish(self) -> None:
        """End-of-attempt housekeeping, called by the worker entrypoints.

        Delivers usage still pending (one last try) and logs the totals, so a
        run without a workload API still shows what it consumed.
        """
        try:
            self._flush_usage()
        except (TaskAPIError, InvalidUsage) as e:
            self.log.warning("final usage report not delivered", error=str(e))
        totals = self.usage_totals
        if totals:
            self.log.info("usage", units=totals)

    def remaining_ms(self) -> int | None:
        """Milliseconds left before start_to_close, per CONTROL-PLANE time.

        The deadline was computed by the control plane and shipped in the
        assignment; worker clocks are never authoritative (invariant 7). This
        still reads the local clock to measure the remaining interval, so treat
        the result as advisory -- use it to decide whether to start another chunk
        of work, not to decide whether a deadline has legally passed.
        """
        if not self.start_to_close_deadline_unix_ms:
            return None
        return max(0, self.start_to_close_deadline_unix_ms - int(time.time() * 1000))

    # ── the effect guard ────────────────────────────────────────────────────
    #
    # THE THREE IDEMPOTENCY MECHANISMS, IN DESCENDING ORDER OF STRENGTH.
    #
    # Nexus is at-least-once (ADR-0006): your handler CAN run more than once for
    # one logical unit of work. Which of these you use decides what guarantee you
    # actually have. The SDK states the ordering instead of hiding it, because a
    # user who believes they have mechanism 1 while using mechanism 3 has a
    # correctness bug they cannot see.
    #
    #   1. DOWNSTREAM-NATIVE DEDUP  (strongest)
    #      Pass ctx.idempotency_key to a system that deduplicates on it:
    #          payments.charge(..., idempotency_key=ctx.idempotency_key)
    #      The effect becomes exactly-once at the only boundary where that is
    #      achievable -- inside the system performing it. No window.
    #
    #   2. CONDITIONAL WRITE
    #          INSERT INTO t (idem_key, ...) VALUES (%s, ...)
    #          ON CONFLICT (idem_key) DO NOTHING
    #      Atomic in the target store, so also no window -- but it requires that
    #      you control the schema.
    #
    #   3. PLATFORM EFFECT JOURNAL  (weakest -- this API)
    #      effect_recorded / record_effect / previous_result. For downstream
    #      systems with no native dedup and no conditional write.
    #      THE WRITE-THEN-CRASH WINDOW STILL EXISTS: if the process dies after
    #      the downstream write but before record_effect returns, the retry
    #      repeats the effect. What the journal buys is that the window shrinks
    #      from the whole task to a single write. That is a real improvement and
    #      not a guarantee; do not describe it as one.

    def effect_recorded(self, name: str) -> bool:
        """True if this named effect already landed under this idempotency key.

        Guard the top of a side-effecting handler with this. Mechanism 3 above --
        read that comment block for what it does and does not promise.
        """
        self._load_effects()
        return name in self._effects

    def record_effect(self, name: str, value: Any = None) -> None:
        """Journal that a named effect landed, durably, keyed by idempotency_key.

        Call this IMMEDIATELY after the downstream write, never before: recording
        first and writing second would make a crash in between skip the effect
        forever, which is strictly worse than repeating it.
        """
        self._load_effects()
        self._effects[name] = value
        # Keyed by idempotency_key, which excludes attempt_number -- so attempt 2
        # reads what attempt 1 wrote. That is the point of the exclusion.
        self.store.put(
            {"idempotency_key": self.idempotency_key, "effects": self._effects},
            key=self._journal_key(),
        )

    def previous_result(self, name: str) -> Any:
        """The value recorded for a named effect by an earlier attempt."""
        self._load_effects()
        try:
            return self._effects[name]
        except KeyError:
            raise EffectNotRecorded(
                f"effect {name!r} was never recorded for idempotency key "
                f"{self.idempotency_key[:12]}...; guard with effect_recorded() first"
            ) from None

    def _journal_key(self) -> str:
        return f"effects/{self.idempotency_key}"

    def _load_effects(self) -> None:
        """Read the journal once per attempt; cache thereafter.

        Cached because a guarded handler calls effect_recorded on every effect and
        a store round-trip per call would be a needless cost on the hot path.
        """
        if self._effects_loaded:
            return
        self._effects_loaded = True
        key = self._journal_key()
        try:
            if self.store.exists(key):
                doc = self.store.get(key)
                if isinstance(doc, Mapping) and isinstance(doc.get("effects"), Mapping):
                    self._effects = dict(doc["effects"])
        except (OSError, ValueError, KeyError) as e:
            # A journal we cannot read means we do not know whether the effect
            # landed. Treating it as "not recorded" risks a duplicate; treating it
            # as "recorded" risks skipping the work entirely. We choose the
            # duplicate, because at-least-once is the platform's stated guarantee
            # and silently never doing the work is not a failure mode users can
            # detect.
            self.log.warning(
                "effect journal unreadable; proceeding as if no effect was recorded",
                error=str(e),
                idempotency_key_prefix=self.idempotency_key[:12],
            )

    def __repr__(self) -> str:
        return (
            f"TaskContext(run={self.run_id} node={self.node_id} "
            f"attempt={self.attempt_number} idem={self.idempotency_key[:12]}...)"
        )
