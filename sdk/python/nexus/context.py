# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""TaskContext: what a handler is given at execution time, inside the worker.

This module runs on the WORKER, not in the control plane and not at compile time.
It is the other half of the SDK: `task.py`/`workflow.py` compile a document,
this file executes a handler against a dispatched attempt.

The most important thing here is the effect guard (architecture §6.2). Read
`TaskContext.record_effect` before writing a handler that touches anything
outside the result store.
"""

from __future__ import annotations

import base64
import dataclasses
import hashlib
import json
import logging
import os
import sys
import threading
import time
from typing import Any, Callable, Mapping, Protocol, TextIO

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
        """
        if self._heartbeat_fn is not None:
            self._heartbeat_fn(message, None)
        elif message:
            self.log.debug("heartbeat", detail=message)

    def progress(self, fraction: float, message: str | None = None) -> None:
        """Report fractional progress in [0.0, 1.0]. Also counts as a heartbeat."""
        if not 0.0 <= fraction <= 1.0:
            raise ValueError(f"progress fraction must be in [0,1], got {fraction}")
        if self._heartbeat_fn is not None:
            self._heartbeat_fn(message, fraction)
        else:
            self.log.debug("progress", fraction=fraction, detail=message)

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
