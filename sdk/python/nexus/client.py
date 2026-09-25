# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""HTTP client for the public REST API (architecture §8.2).

Deliberately built on `urllib` from the standard library rather than `requests`
or `httpx`. The SDK gets installed into every task image; each dependency is a
version conflict waiting to happen with the user's own requirements, and an
orchestrator SDK that is hard to install is an orchestrator nobody adopts.

Endpoints implemented (all namespace-scoped, per §8.2):
    POST   /v1/namespaces/{ns}/specs                submit or validate a spec
    GET    /v1/namespaces/{ns}/specs/{name}/versions
    POST   /v1/namespaces/{ns}/runs                 create a run
    GET    /v1/namespaces/{ns}/runs                 list
    GET    /v1/namespaces/{ns}/runs/{id}            detail + node states
    GET    /v1/namespaces/{ns}/runs/{id}/events     history, SSE for live tail
    POST   /v1/namespaces/{ns}/runs/{id}/cancel
    POST   /v1/namespaces/{ns}/runs/{id}/retry
    GET    /v1/namespaces/{ns}/attempts/{id}/logs   stream
    GET    /v1/namespaces/{ns}/workers              fleet view
    GET    /v1/namespaces/{ns}/capacity             advertised vs committed
"""

from __future__ import annotations

import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Iterator, Mapping

from .canonical import canonical_bytes
from .workflow import CompiledWorkflow, WorkflowDefinition

__all__ = ["NexusClient", "NexusError", "NexusAPIError", "AuthError", "NotFound", "Conflict"]

USER_AGENT = "nexus-python-sdk/0.1"


class NexusError(Exception):
    """Base class for every client-side failure."""


class NexusAPIError(NexusError):
    """A non-2xx response, carrying enough detail to act on.

    An orchestrator client that raises a bare "HTTP 500" wastes the operator's
    time, so the server's error body is surfaced verbatim when it has one.
    """

    def __init__(self, status: int, message: str, *, body: Any = None, request_id: str = "") -> None:
        self.status = status
        self.message = message
        self.body = body
        self.request_id = request_id
        detail = f"HTTP {status}: {message}"
        if request_id:
            detail += f" (request_id={request_id})"
        super().__init__(detail)


class AuthError(NexusAPIError):
    """401/403. Almost always a missing or wrong NEXUS_API_KEY."""


class NotFound(NexusAPIError):
    """404."""


class Conflict(NexusAPIError):
    """409. For run creation this usually means the Idempotency-Key was reused."""


class NexusClient:
    """A client for one endpoint and namespace.

    Not thread-safe by design, and cheap to construct: make one per thread rather
    than sharing, which avoids having to reason about urllib opener state.
    """

    def __init__(
        self,
        endpoint: str | None = None,
        *,
        api_key: str | None = None,
        namespace: str | None = None,
        timeout: float = 30.0,
        max_retries: int = 3,
    ) -> None:
        self.endpoint = (endpoint or os.environ.get("NEXUS_ENDPOINT") or "http://localhost:8090").rstrip("/")
        self._api_key = api_key or os.environ.get("NEXUS_API_KEY") or ""
        # `namespace` defaults to None rather than "default" so that an explicit
        # argument, the environment, and the fallback are distinguishable; a
        # truthy default would silently shadow NEXUS_NAMESPACE.
        self.namespace = namespace or os.environ.get("NEXUS_NAMESPACE") or "default"
        self.timeout = timeout
        self.max_retries = max_retries

    def __repr__(self) -> str:
        # Never render the key, not even truncated: reprs end up in logs and
        # exception tracebacks, which end up in tickets.
        return f"NexusClient(endpoint={self.endpoint!r}, namespace={self.namespace!r})"

    # ── plumbing ────────────────────────────────────────────────────────────
    def _url(self, path: str, query: Mapping[str, Any] | None = None) -> str:
        ns = urllib.parse.quote(self.namespace, safe="")
        url = f"{self.endpoint}/v1/namespaces/{ns}{path}"
        if query:
            pairs = [(k, str(v)) for k, v in sorted(query.items()) if v is not None and v != ""]
            if pairs:
                url += "?" + urllib.parse.urlencode(pairs)
        return url

    def _request(
        self,
        method: str,
        url: str,
        *,
        body: bytes | None = None,
        headers: Mapping[str, str] | None = None,
        stream: bool = False,
        timeout: float | None = None,
    ) -> Any:
        hdrs: dict[str, str] = {"User-Agent": USER_AGENT, "Accept": "application/json"}
        if self._api_key:
            hdrs["Authorization"] = f"Bearer {self._api_key}"
        if body is not None:
            hdrs["Content-Type"] = "application/json"
        if headers:
            hdrs.update(headers)

        attempt = 0
        while True:
            attempt += 1
            req = urllib.request.Request(url, data=body, headers=hdrs, method=method)
            try:
                resp = urllib.request.urlopen(req, timeout=timeout or self.timeout)
            except urllib.error.HTTPError as e:
                raw = e.read()
                # 5xx and 429 are worth retrying; a 4xx will not change its mind.
                # Retrying here is safe for mutating calls too, because run
                # creation is keyed by Idempotency-Key and spec submission is
                # content-addressed and idempotent by construction (ADR-0004).
                if e.code in (429, 500, 502, 503, 504) and attempt <= self.max_retries:
                    time.sleep(min(2 ** (attempt - 1) * 0.25, 4.0))
                    continue
                raise _api_error(e.code, raw, dict(e.headers or {})) from None
            except urllib.error.URLError as e:
                if attempt <= self.max_retries:
                    time.sleep(min(2 ** (attempt - 1) * 0.25, 4.0))
                    continue
                raise NexusError(
                    f"cannot reach Nexus at {self.endpoint}: {e.reason}. Check "
                    "NEXUS_ENDPOINT and that the control plane is running."
                ) from None

            if stream:
                return resp
            with resp:
                payload = resp.read()
            if not payload:
                return {}
            try:
                return json.loads(payload)
            except ValueError:
                raise NexusError(
                    f"{method} {url} returned a non-JSON body "
                    f"({payload[:200]!r}); is NEXUS_ENDPOINT pointing at the API gateway?"
                ) from None

    # ── specs ───────────────────────────────────────────────────────────────
    def submit_spec(
        self,
        spec: CompiledWorkflow | WorkflowDefinition | Mapping[str, Any],
        *,
        dry_run: bool = False,
    ) -> dict[str, Any]:
        """Submit a spec, or validate it only when `dry_run`.

        The body is the CANONICAL bytes, not a re-serialization: the server
        content-addresses what it receives, so re-encoding here would risk a hash
        that differs from the one we told the user about.
        """
        compiled = _as_compiled(spec)
        return self._request(
            "POST",
            self._url("/specs", {"dry_run": "true"} if dry_run else None),
            body=canonical_bytes(compiled.spec),
        )

    def validate_spec(self, spec: Any) -> dict[str, Any]:
        """Validate without registering. `workflow submit --dry-run`."""
        return self.submit_spec(spec, dry_run=True)

    def list_spec_versions(self, name: str) -> dict[str, Any]:
        return self._request("GET", self._url(f"/specs/{urllib.parse.quote(name, safe='')}/versions"))

    # ── runs ────────────────────────────────────────────────────────────────
    def create_run(
        self,
        spec_name: str,
        *,
        params: Mapping[str, Any] | None = None,
        idempotency_key: str | None = None,
        spec_version: int | None = None,
        priority: int | None = None,
        trigger_kind: str = "manual",
    ) -> dict[str, Any]:
        """Create a run. `idempotency_key` goes in the header, per §8.2.

        Supplying it makes "submit this run twice" safe at the outermost layer: a
        duplicate conflicts on the unique index and the existing run is returned
        rather than a second one being created. This is the ONE place a user-chosen
        key is correct -- it identifies a user's INTENT to start a run. Per-task
        effect keys are always platform-derived (§6.2); do not confuse the two.
        """
        payload: dict[str, Any] = {"spec_name": spec_name, "trigger_kind": trigger_kind}
        if params:
            payload["params"] = dict(params)
        if spec_version is not None:
            payload["spec_version"] = spec_version
        if priority is not None:
            payload["priority"] = priority
        headers = {"Idempotency-Key": idempotency_key} if idempotency_key else None
        return self._request(
            "POST",
            self._url("/runs"),
            body=json.dumps(payload, sort_keys=True).encode("utf-8"),
            headers=headers,
        )

    def get_run(self, run_id: str) -> dict[str, Any]:
        """Run detail including per-node states."""
        return self._request("GET", self._url(f"/runs/{urllib.parse.quote(run_id, safe='')}"))

    def list_runs(
        self,
        *,
        state: str | None = None,
        spec: str | None = None,
        limit: int = 50,
        cursor: str | None = None,
    ) -> dict[str, Any]:
        return self._request(
            "GET", self._url("/runs", {"state": state, "spec": spec, "limit": limit, "cursor": cursor})
        )

    def cancel_run(self, run_id: str, *, reason: str = "") -> dict[str, Any]:
        """Cancel: stop dispatching AND signal running attempts.

        Distinct from pause, which stops new dispatch but lets in-flight work
        finish (§5.2). Conflating them makes pause useless for its actual job.
        """
        return self._request(
            "POST",
            self._url(f"/runs/{urllib.parse.quote(run_id, safe='')}/cancel"),
            body=json.dumps({"reason": reason}).encode("utf-8"),
        )

    def retry_run(self, run_id: str, *, nodes: list[str] | None = None) -> dict[str, Any]:
        """Retry from failed nodes. Successful upstream nodes are not re-run."""
        payload: dict[str, Any] = {}
        if nodes:
            payload["nodes"] = list(nodes)
        return self._request(
            "POST",
            self._url(f"/runs/{urllib.parse.quote(run_id, safe='')}/retry"),
            body=json.dumps(payload, sort_keys=True).encode("utf-8"),
        )

    def pause_run(self, run_id: str) -> dict[str, Any]:
        return self._request("POST", self._url(f"/runs/{urllib.parse.quote(run_id, safe='')}/pause"), body=b"{}")

    def resume_run(self, run_id: str) -> dict[str, Any]:
        return self._request("POST", self._url(f"/runs/{urllib.parse.quote(run_id, safe='')}/resume"), body=b"{}")

    # ── events and logs ─────────────────────────────────────────────────────
    def get_events(self, run_id: str, *, after_seq: int | None = None) -> dict[str, Any]:
        """Event history as a single response. `run_events` is append-only."""
        return self._request(
            "GET",
            self._url(f"/runs/{urllib.parse.quote(run_id, safe='')}/events", {"after_seq": after_seq}),
        )

    def stream_events(
        self, run_id: str, *, after_seq: int | None = None, timeout: float = 3600.0
    ) -> Iterator[dict[str, Any]]:
        """Tail run events over SSE, yielding decoded event dicts.

        Events carry a monotonic sequence number; a caller that reconnects should
        pass the last seq it saw as `after_seq` so nothing is missed in the gap.
        """
        url = self._url(
            f"/runs/{urllib.parse.quote(run_id, safe='')}/events",
            {"watch": "true", "after_seq": after_seq},
        )
        resp = self._request(
            "GET", url, headers={"Accept": "text/event-stream"}, stream=True, timeout=timeout
        )
        with resp:
            yield from _parse_sse(resp)

    def get_logs(
        self,
        attempt_id: str,
        *,
        follow: bool = False,
        tail: int | None = None,
        stream: str | None = None,
        timeout: float = 3600.0,
    ) -> Iterator[str]:
        """Stream an attempt's log lines as the task printed them (ADR-0020).

        With ``follow=True`` the stream stays open until the attempt has ended
        and its log is complete. ``tail`` starts at the last N lines; ``stream``
        is ``"stdout"`` or ``"stderr"``. Lines beginning ``[nexus]`` are the
        platform's own: output omitted by the per-attempt cap, lines dropped
        while shipping fell behind, or a range that could not be read.
        """
        params: dict[str, Any] = {}
        if follow:
            params["follow"] = "true"
        if tail:
            params["tail"] = tail
        if stream:
            params["stream_name"] = stream
        url = self._url(
            f"/attempts/{urllib.parse.quote(attempt_id, safe='')}/logs", params or None
        )
        resp = self._request("GET", url, headers={"Accept": "text/plain"}, stream=True, timeout=timeout)
        with resp:
            for raw in resp:
                yield raw.decode("utf-8", errors="replace").rstrip("\n")

    def get_log_page(
        self,
        attempt_id: str,
        *,
        tail: int | None = None,
        from_line: int | None = None,
        limit: int | None = None,
        stream: str | None = None,
    ) -> dict[str, Any]:
        """One page of structured log lines plus the log's state.

        Returns ``{"log": {"state": ..., "detail": ...}, "lines": [{"n", "ts",
        "stream", "message"}], "next_line": ..., "has_more": ...}``. ``state`` is
        one of none, live, complete, partial (the attempt ended without its log
        being finalized) or expired (removed by retention). Pass ``next_line``
        back as ``from_line`` to continue.
        """
        return self._request(
            "GET",
            self._url(
                f"/attempts/{urllib.parse.quote(attempt_id, safe='')}/logs",
                {"tail": tail, "from_line": from_line, "limit": limit, "stream_name": stream},
            ),
        )

    def search_logs(
        self,
        attempt_id: str,
        query: str,
        *,
        regex: bool = False,
        case_insensitive: bool = False,
        stream: str | None = None,
        from_line: int | None = None,
        limit: int | None = None,
    ) -> dict[str, Any]:
        """Bounded server-side search of an attempt's log.

        Returns ``{"log": {...}, "search": {"matches": [...], "next_line": ...,
        "complete": bool, "stopped_by": "matches"|"bytes"|"time"|None}}``. When
        ``complete`` is false, call again with ``from_line=next_line``. Regular
        expressions use RE2 syntax (no backreferences or lookaround).
        """
        return self._request(
            "GET",
            self._url(
                f"/attempts/{urllib.parse.quote(attempt_id, safe='')}/logs",
                {
                    "q": query,
                    "regex": "true" if regex else None,
                    "case": "insensitive" if case_insensitive else None,
                    "stream_name": stream,
                    "from_line": from_line,
                    "limit": limit,
                },
            ),
        )

    # ── fleet ───────────────────────────────────────────────────────────────
    def list_workers(self, *, state: str | None = None) -> dict[str, Any]:
        return self._request("GET", self._url("/workers", {"state": state}))

    def drain_worker(self, worker_id: str) -> dict[str, Any]:
        return self._request(
            "POST", self._url(f"/workers/{urllib.parse.quote(worker_id, safe='')}/drain"), body=b"{}"
        )

    def capacity(self) -> dict[str, Any]:
        """Advertised vs committed capacity -- the ledger, as the platform sees it."""
        return self._request("GET", self._url("/capacity"))

    # ── convenience ─────────────────────────────────────────────────────────
    def submit_and_run(
        self,
        workflow: WorkflowDefinition,
        *,
        params: Mapping[str, Any] | None = None,
        idempotency_key: str | None = None,
    ) -> dict[str, Any]:
        """Compile, submit, and create a run. The common script-level path."""
        compiled = workflow.compile()
        self.submit_spec(compiled)
        return self.create_run(
            compiled.spec["name"], params=params, idempotency_key=idempotency_key
        )

    def wait_for_run(
        self, run_id: str, *, poll_interval: float = 2.0, timeout: float = 3600.0
    ) -> dict[str, Any]:
        """Poll until the run reaches a terminal state. For scripts and tests."""
        terminal = {"SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT"}
        deadline = time.monotonic() + timeout
        while True:
            run = self.get_run(run_id)
            state = str(run.get("state", "")).upper()
            if state in terminal:
                return run
            if time.monotonic() > deadline:
                raise NexusError(
                    f"run {run_id} still {state or 'UNKNOWN'} after {timeout}s"
                )
            time.sleep(poll_interval)


def _as_compiled(spec: Any) -> CompiledWorkflow:
    if isinstance(spec, CompiledWorkflow):
        return spec
    if isinstance(spec, WorkflowDefinition):
        return spec.compile()
    if isinstance(spec, Mapping):
        from .canonical import canonical_json, spec_hash

        return CompiledWorkflow(spec=spec, json=canonical_json(spec), hash=spec_hash(spec))
    raise TypeError(
        f"expected a CompiledWorkflow, a @workflow, or a spec mapping, got "
        f"{type(spec).__name__}"
    )


def _api_error(status: int, raw: bytes, headers: Mapping[str, str]) -> NexusAPIError:
    """Turn an HTTP failure into the most specific, most actionable exception."""
    body: Any = None
    message = raw.decode("utf-8", errors="replace")[:500] or f"HTTP {status}"
    try:
        body = json.loads(raw)
        if isinstance(body, Mapping):
            message = str(body.get("message") or body.get("error") or message)
            details = body.get("details")
            if details:
                message += f" | {json.dumps(details, sort_keys=True)[:400]}"
    except ValueError:
        pass
    request_id = str(headers.get("X-Request-Id") or headers.get("x-request-id") or "")

    if status in (401, 403):
        return AuthError(
            status,
            f"{message}. Set NEXUS_API_KEY, or `api_key` in ~/.nexus/config.yaml.",
            body=body,
            request_id=request_id,
        )
    if status == 404:
        return NotFound(status, message, body=body, request_id=request_id)
    if status == 409:
        return Conflict(status, message, body=body, request_id=request_id)
    return NexusAPIError(status, message, body=body, request_id=request_id)


def _parse_sse(stream: Any) -> Iterator[dict[str, Any]]:
    """Minimal SSE reader: accumulate `data:` lines until a blank line.

    Only the `data` field is used. Comment lines (`:`) are keep-alives and are
    dropped, which matters for a long-idle tail: without them a proxy would close
    the connection during a quiet stretch of a slow pipeline.
    """
    data_lines: list[str] = []
    for raw in stream:
        line = raw.decode("utf-8", errors="replace").rstrip("\r\n")
        if not line:
            if data_lines:
                payload = "\n".join(data_lines)
                data_lines = []
                try:
                    parsed = json.loads(payload)
                except ValueError:
                    continue
                if isinstance(parsed, dict):
                    yield parsed
            continue
        if line.startswith(":"):
            continue
        field, _, value = line.partition(":")
        if field == "data":
            data_lines.append(value[1:] if value.startswith(" ") else value)
