# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Client for the agent-local workload API (ADR-0032 §1).

The worker agent serves each attempt a small HTTP/1.1 JSON API on a Unix
domain socket it creates for that attempt, and exports:

    NEXUS_TASK_API        unix:///abs/path/api.sock (as the workload sees it)
    NEXUS_TASK_API_TOKEN  sent as ``Authorization: Bearer <token>``

The agent is the only thing that talks to the control plane: it adds the lease
token and forwards over its own authenticated connection. The workload never
holds the lease token or the worker credential, and one socket per attempt
means a request can only ever act as that attempt.

This module is private (``nexus._taskapi``): handlers use the ``TaskContext``
methods, which choose between this client and the in-process fallback. It is
deliberately small and deliberately bounded:

  * one connection per request, closed afterwards -- no pool to go stale when
    the agent restarts a listener;
  * a short timeout on every request (10 s; 120 s for checkpoint bodies), so a
    wedged agent turns into an ``Unavailable`` error rather than a task that
    hangs until its start_to_close deadline;
  * no automatic retries: usage reports are increments, and resending one whose
    response was lost would double-count it. Callers that can retry safely
    (permits) do so themselves.

Status codes are the workload's contract: 400 invalid request, 401 bad token,
409 ``stale_lease`` (the attempt no longer owns its lease: stop), 413
checkpoint too large, 501 capability not offered, 503 control plane
unreachable (retry).
"""

from __future__ import annotations

import dataclasses
import http.client
import json
import os
import socket
from typing import Any, Mapping, Sequence

__all__ = [
    "TaskAPIClient",
    "TaskAPIError",
    "StaleLease",
    "StaleCheckpoint",
    "Unsupported",
    "Unavailable",
    "CheckpointTooLarge",
    "Capabilities",
    "PermitDecision",
    "CheckpointInfo",
    "StoredCheckpoint",
    "from_environment",
    "parse_address",
    "ENV_TASK_API",
    "ENV_TASK_API_TOKEN",
    "DEFAULT_TIMEOUT_S",
    "CHECKPOINT_TIMEOUT_S",
]

ENV_TASK_API = "NEXUS_TASK_API"
ENV_TASK_API_TOKEN = "NEXUS_TASK_API_TOKEN"
DEFAULT_TIMEOUT_S = 10.0
CHECKPOINT_TIMEOUT_S = 120.0
# Error bodies are for humans; never let one grow without bound in a message.
_MAX_ERROR_TEXT = 500


class TaskAPIError(Exception):
    """A request to the workload API failed. ``status`` is the HTTP status
    (0 when no response arrived) and ``code`` the API's error code."""

    def __init__(self, message: str, *, status: int = 0, code: str = "") -> None:
        super().__init__(message)
        self.status = status
        self.code = code


class StaleLease(TaskAPIError):
    """409 ``stale_lease``: this attempt no longer owns its lease.

    The platform has reclaimed or superseded the attempt; another attempt may
    already be running the same work. Stop: nothing this attempt reports from
    now on will be recorded, and its result will be refused.
    """


class StaleCheckpoint(StaleLease):
    """409 ``stale_checkpoint``: a newer checkpoint exists for this work.

    Only a newer attempt can have written it, so this attempt has been
    superseded -- the same conclusion as a stale lease, hence the subclass.
    """


class Unsupported(TaskAPIError):
    """501: this control plane does not offer the capability.

    ``TaskContext`` falls back to its in-process behaviour when it sees this.
    """


class Unavailable(TaskAPIError, ConnectionError):
    """503, or the agent could not be reached at all. Retry later.

    A ``ConnectionError``, so a task that lets it escape is classified
    TRANSIENT (retryable) by the worker, which is what it is.
    """


class CheckpointTooLarge(TaskAPIError, ValueError):
    """413: the checkpoint is larger than the platform accepts (64 MiB by
    default). A ``ValueError``: retrying the same state cannot succeed."""


@dataclasses.dataclass(frozen=True, slots=True)
class Capabilities:
    """What the agent (and the control plane behind it) offers this attempt."""

    usage: bool = False
    rate_limits: bool = False
    checkpoints: bool = False
    progress: bool = False

    @classmethod
    def none(cls) -> "Capabilities":
        return cls()


@dataclasses.dataclass(frozen=True, slots=True)
class PermitDecision:
    """The answer to one ``POST /v1/permits``: never waits server-side."""

    granted: bool
    retry_after_ms: int = 0
    unlimited_keys: tuple[str, ...] = ()


@dataclasses.dataclass(frozen=True, slots=True)
class CheckpointInfo:
    """A checkpoint as registered: its per-attempt sequence number, the object
    key it was stored under, and its digest and size."""

    seq: int
    key: str
    sha256: str
    size_bytes: int


@dataclasses.dataclass(frozen=True, slots=True)
class StoredCheckpoint:
    """A checkpoint read back: the bytes plus which attempt wrote them, when."""

    data: bytes
    seq: int = 0
    attempt: str = ""
    saved_at: str = ""


class _UnixHTTPConnection(http.client.HTTPConnection):
    """``http.client`` over ``AF_UNIX``. The Host header is cosmetic."""

    def __init__(self, path: str, timeout: float) -> None:
        super().__init__("localhost", timeout=timeout)
        self._path = path

    def connect(self) -> None:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(self.timeout)
        try:
            sock.connect(self._path)
        except BaseException:
            sock.close()
            raise
        self.sock = sock


@dataclasses.dataclass(frozen=True, slots=True)
class _Response:
    status: int
    body: bytes
    headers: Mapping[str, str]

    def json(self) -> Any:
        if not self.body:
            return {}
        return json.loads(self.body)


class TaskAPIClient:
    """A client of one attempt's workload API socket.

    Thread-safe: it holds no connection between requests, so handlers that fan
    calls out over a thread pool can share the context's client.
    """

    def __init__(self, socket_path: str, token: str, *, timeout_s: float = DEFAULT_TIMEOUT_S) -> None:
        if not socket_path:
            raise ValueError("workload API socket path is empty")
        self.socket_path = socket_path
        self._token = token
        self.timeout_s = timeout_s
        self._info: dict[str, Any] | None = None

    def __repr__(self) -> str:
        # Never the token.
        return f"TaskAPIClient(socket={self.socket_path!r})"

    # ── transport ────────────────────────────────────────────────────────────
    def _request(
        self,
        method: str,
        path: str,
        *,
        body: bytes | None = None,
        content_type: str = "application/json",
        timeout_s: float | None = None,
    ) -> _Response:
        conn = _UnixHTTPConnection(self.socket_path, timeout_s or self.timeout_s)
        headers = {"Authorization": f"Bearer {self._token}", "Accept": "application/json"}
        if body is not None:
            headers["Content-Type"] = content_type
        try:
            conn.request(method, path, body=body, headers=headers)
            resp = conn.getresponse()
            data = resp.read()
            hdrs = {k.lower(): v for k, v in resp.getheaders()}
            return _Response(resp.status, data, hdrs)
        except (OSError, http.client.HTTPException) as e:
            # socket.timeout is an OSError. Whatever it was, no answer arrived,
            # which from the task's point of view is "the agent is unavailable".
            raise Unavailable(
                f"workload API {method} {path} failed: {type(e).__name__}: {e}"
            ) from e
        finally:
            conn.close()

    def _call(
        self,
        method: str,
        path: str,
        payload: Any = None,
        *,
        ok: Sequence[int] = (200, 204),
    ) -> _Response:
        body = None if payload is None else json.dumps(payload, separators=(",", ":")).encode("utf-8")
        resp = self._request(method, path, body=body)
        if resp.status in ok:
            return resp
        raise _error_for(method, path, resp)

    # ── endpoints ────────────────────────────────────────────────────────────
    def info(self) -> dict[str, Any]:
        """``GET /v1/info``: attempt id and capabilities. Cached after success."""
        if self._info is None:
            doc = self._call("GET", "/v1/info").json()
            self._info = dict(doc) if isinstance(doc, Mapping) else {}
        return dict(self._info)

    def capabilities(self) -> Capabilities:
        caps = self.info().get("capabilities")
        if not isinstance(caps, Mapping):
            return Capabilities.none()
        return Capabilities(
            usage=bool(caps.get("usage")),
            rate_limits=bool(caps.get("rate_limits")),
            checkpoints=bool(caps.get("checkpoints")),
            progress=bool(caps.get("progress")),
        )

    def report_usage(self, units: Mapping[str, int]) -> dict[str, int]:
        """``POST /v1/usage`` with INCREMENTS; returns the attempt's totals."""
        doc = self._call("POST", "/v1/usage", {"units": dict(units)}).json()
        totals = doc.get("totals") if isinstance(doc, Mapping) else None
        if not isinstance(totals, Mapping):
            return {}
        return {str(k): int(v) for k, v in totals.items()}

    def progress(self, message: str | None, fraction: float | None) -> None:
        """``POST /v1/progress``: also a heartbeat."""
        self._call("POST", "/v1/progress", {"message": message or "", "fraction": fraction})

    def acquire_permits(self, keys: Sequence[str], requests: int, tokens: int) -> PermitDecision:
        """``POST /v1/permits``: one all-or-none attempt. Never waits server-side."""
        doc = self._call(
            "POST", "/v1/permits", {"keys": list(keys), "requests": requests, "tokens": tokens}
        ).json()
        if not isinstance(doc, Mapping):
            raise TaskAPIError("workload API returned a malformed permit decision", status=200)
        return PermitDecision(
            granted=bool(doc.get("granted")),
            retry_after_ms=max(0, int(doc.get("retry_after_ms") or 0)),
            unlimited_keys=tuple(str(k) for k in (doc.get("unlimited_keys") or ())),
        )

    def settle_permits(self, keys: Sequence[str], tokens: int) -> None:
        """``POST /v1/permits/settle``: debt (positive) or refund (negative)."""
        self._call("POST", "/v1/permits/settle", {"keys": list(keys), "tokens": tokens})

    def put_checkpoint(self, data: bytes) -> CheckpointInfo:
        """``PUT /v1/checkpoint`` with the raw state bytes."""
        resp = self._request(
            "PUT", "/v1/checkpoint", body=data, content_type="application/octet-stream",
            timeout_s=max(self.timeout_s, CHECKPOINT_TIMEOUT_S),
        )
        if resp.status != 200:
            raise _error_for("PUT", "/v1/checkpoint", resp)
        doc = resp.json()
        return CheckpointInfo(
            seq=int(doc.get("seq") or 0),
            key=str(doc.get("key") or ""),
            sha256=str(doc.get("sha256") or ""),
            size_bytes=int(doc.get("size_bytes") or len(data)),
        )

    def get_checkpoint(self) -> StoredCheckpoint | None:
        """``GET /v1/checkpoint``: the latest checkpoint for this work, or None."""
        resp = self._request("GET", "/v1/checkpoint", timeout_s=max(self.timeout_s, CHECKPOINT_TIMEOUT_S))
        if resp.status == 404:
            return None
        if resp.status != 200:
            raise _error_for("GET", "/v1/checkpoint", resp)
        seq_raw = resp.headers.get("x-nexus-checkpoint-seq") or "0"
        try:
            seq = int(seq_raw)
        except ValueError:
            seq = 0
        return StoredCheckpoint(
            data=resp.body,
            seq=seq,
            attempt=resp.headers.get("x-nexus-checkpoint-attempt", ""),
            saved_at=resp.headers.get("x-nexus-checkpoint-saved-at", ""),
        )

    def report_error(
        self, error_class: str, error_type: str, message: str, retry_after_ms: int | None = None
    ) -> None:
        """``POST /v1/error``: declare how the failure about to happen should be classified."""
        payload: dict[str, Any] = {
            "class": error_class,
            "type": error_type,
            "message": message[:2000],
        }
        if retry_after_ms is not None:
            payload["retry_after_ms"] = max(0, int(retry_after_ms))
        self._call("POST", "/v1/error", payload)


def _error_for(method: str, path: str, resp: _Response) -> TaskAPIError:
    code = ""
    detail = ""
    try:
        doc = resp.json()
        if isinstance(doc, Mapping):
            code = str(doc.get("error") or "")
            detail = str(doc.get("message") or "")
    except ValueError:
        detail = resp.body[:_MAX_ERROR_TEXT].decode("utf-8", "replace")
    text = f"workload API {method} {path}: HTTP {resp.status}"
    if code:
        text += f" {code}"
    if detail:
        text += f": {detail[:_MAX_ERROR_TEXT]}"
    if resp.status == 409 and code == "stale_checkpoint":
        return StaleCheckpoint(text, status=409, code=code)
    if resp.status == 409:
        return StaleLease(text, status=409, code=code or "stale_lease")
    if resp.status == 413:
        return CheckpointTooLarge(text, status=413, code=code or "too_large")
    if resp.status == 501:
        return Unsupported(text, status=501, code=code or "unsupported")
    if resp.status in (502, 503, 504):
        return Unavailable(text, status=resp.status, code=code or "unavailable")
    return TaskAPIError(text, status=resp.status, code=code)


def parse_address(address: str) -> str | None:
    """The socket path of a ``unix://`` address, or None for anything else."""
    if not address.startswith("unix://"):
        return None
    path = address[len("unix://"):]
    return path or None


def from_environment(env: Mapping[str, str] | None = None) -> TaskAPIClient | None:
    """The attempt's workload API client, or None when there is none.

    None under the local runner, in unit tests, and under the kubernetes
    executor (F149), which do not export ``NEXUS_TASK_API``. An address that is
    not ``unix://`` is ignored rather than guessed at.
    """
    e = os.environ if env is None else env
    raw = (e.get(ENV_TASK_API) or "").strip()
    if not raw:
        return None
    path = parse_address(raw)
    if path is None:
        return None
    return TaskAPIClient(path, e.get(ENV_TASK_API_TOKEN) or "")
