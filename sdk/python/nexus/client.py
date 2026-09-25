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
    GET    /v1/namespaces/{ns}/runs/{run}/attempts/{id}/assignment   replay input
    GET    /v1/namespaces/{ns}/workers              fleet view
    GET    /v1/namespaces/{ns}/capacity             advertised vs committed
    GET    /v1/namespaces/{ns}/schedules            list; POST creates
    GET    /v1/namespaces/{ns}/schedules/preview    evaluate a cron expression
    GET    /v1/namespaces/{ns}/schedules/{name}     PUT replaces, DELETE removes
    POST   /v1/namespaces/{ns}/schedules/{name}/enable|disable
    POST   /v1/namespaces/{ns}/schedules/{name}/backfills   GET lists
    GET    /v1/namespaces/{ns}/backfills/{id}
    POST   /v1/namespaces/{ns}/backfills/{id}/pause|resume|cancel
"""

from __future__ import annotations

import datetime
import http.client
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Iterable, Iterator, Mapping, Sequence, cast

from .canonical import canonical_bytes
from .scheduling import (
    MAX_BACKFILL_ACTIVE,
    MAX_BACKFILL_RUNS,
    OVERLAP_POLICIES,
    Backfill,
    OverlapPolicy,
    Schedule,
    SchedulePreview,
)
from .workflow import CompiledWorkflow, WorkflowDefinition

__all__ = ["NexusClient", "NexusError", "NexusAPIError", "AuthError", "NotFound", "Conflict"]

USER_AGENT = "nexus-python-sdk/0.1"


class _Unset:
    """Sentinel for "argument not given" where None is a meaningful value."""

    def __repr__(self) -> str:
        return "<unset>"


_UNSET = _Unset()


def _q(segment: str) -> str:
    return urllib.parse.quote(segment, safe="")


def _json(payload: Mapping[str, Any]) -> bytes:
    return json.dumps(payload, sort_keys=True).encode("utf-8")


def _schedule_body(
    name: str,
    *,
    workflow: str,
    cron: str,
    timezone: str | None,
    params: Mapping[str, Any] | None,
    spec_version: int | None,
    priority: int | None,
    overlap_policy: str | None,
    catchup_limit: int | None,
    enabled: bool,
) -> dict[str, Any]:
    """The gateway's createScheduleRequest, validated where it can be locally."""
    if not name or not workflow or not cron:
        raise ValueError("name, workflow and cron are all required")
    if overlap_policy is not None and overlap_policy not in OVERLAP_POLICIES:
        raise ValueError(f"overlap_policy must be one of {', '.join(OVERLAP_POLICIES)}, got {overlap_policy!r}")
    if catchup_limit is not None and catchup_limit < 0:
        raise ValueError("catchup_limit must be zero or positive")
    body: dict[str, Any] = {"name": name, "spec_name": workflow, "cron": cron, "enabled": bool(enabled)}
    if timezone:
        body["timezone"] = timezone
    if params:
        body["params"] = dict(params)
    if spec_version is not None:
        body["spec_version"] = spec_version
    if priority:
        body["priority"] = priority
    if overlap_policy:
        body["overlap_policy"] = overlap_policy
    if catchup_limit:
        body["catchup_limit"] = catchup_limit
    return body


def _instant(label: str, value: datetime.datetime | str) -> str:
    """An RFC 3339 instant from an aware datetime or a string."""
    if isinstance(value, str):
        try:
            parsed = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
        except ValueError:
            raise ValueError(f"{label} {value!r} is not an RFC 3339 timestamp") from None
    else:
        parsed = value
    if parsed.tzinfo is None:
        raise ValueError(
            f"{label} {value!r} has no timezone; pass an aware datetime (e.g. tzinfo=timezone.utc) "
            "or an RFC 3339 string with an offset"
        )
    return parsed.isoformat()


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
        #: For a rejected spec (422): the gateway's problems, each a dict with
        #: path, message, stage and -- for a Python workflow -- "location", the
        #: "file:line" of the @task or workflow-body call it concerns.
        self.problems: list[dict[str, Any]] = []
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

    def _call(
        self,
        method: str,
        url: str,
        *,
        body: bytes | None = None,
        headers: Mapping[str, str] | None = None,
        timeout: float | None = None,
    ) -> dict[str, Any]:
        """A JSON request whose response is a JSON object (every endpoint here)."""
        resp = self._open(method, url, body=body, headers=headers, timeout=timeout)
        with resp:
            payload = resp.read()
        if not payload:
            return {}
        try:
            decoded = json.loads(payload)
        except ValueError:
            raise NexusError(
                f"{method} {url} returned a non-JSON body "
                f"({payload[:200]!r}); is NEXUS_ENDPOINT pointing at the API gateway?"
            ) from None
        if not isinstance(decoded, dict):
            raise NexusError(
                f"{method} {url} returned JSON {type(decoded).__name__}, expected an object"
            )
        return decoded

    def _open(
        self,
        method: str,
        url: str,
        *,
        body: bytes | None = None,
        headers: Mapping[str, str] | None = None,
        timeout: float | None = None,
    ) -> http.client.HTTPResponse:
        """Send a request with retries and return the open response."""
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

            # urlopen is annotated as returning Any-ish in typeshed for http(s)
            # URLs; the object IS an HTTPResponse for every scheme this client
            # accepts, which the cast states rather than hides.
            return cast(http.client.HTTPResponse, resp)

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
        try:
            return self._call(
                "POST",
                self._url("/specs", {"dry_run": "true"} if dry_run else None),
                body=canonical_bytes(compiled.spec),
            )
        except NexusAPIError as e:
            if e.status == 422:
                _locate_problems(e, compiled)
            raise

    def validate_spec(
        self, spec: CompiledWorkflow | WorkflowDefinition | Mapping[str, Any]
    ) -> dict[str, Any]:
        """Validate without registering. `workflow submit --dry-run`."""
        return self.submit_spec(spec, dry_run=True)

    def list_spec_versions(self, name: str) -> dict[str, Any]:
        return self._call("GET", self._url(f"/specs/{urllib.parse.quote(name, safe='')}/versions"))

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
        return self._call(
            "POST",
            self._url("/runs"),
            body=json.dumps(payload, sort_keys=True).encode("utf-8"),
            headers=headers,
        )

    def get_run(self, run_id: str) -> dict[str, Any]:
        """Run detail including per-node states."""
        return self._call("GET", self._url(f"/runs/{urllib.parse.quote(run_id, safe='')}"))

    def list_runs(
        self,
        *,
        state: str | None = None,
        spec: str | None = None,
        limit: int = 50,
        cursor: str | None = None,
    ) -> dict[str, Any]:
        return self._call(
            "GET", self._url("/runs", {"state": state, "spec": spec, "limit": limit, "cursor": cursor})
        )

    def cancel_run(self, run_id: str, *, reason: str = "") -> dict[str, Any]:
        """Cancel: stop dispatching AND signal running attempts.

        Distinct from pause, which stops new dispatch but lets in-flight work
        finish (§5.2). Conflating them makes pause useless for its actual job.
        """
        return self._call(
            "POST",
            self._url(f"/runs/{urllib.parse.quote(run_id, safe='')}/cancel"),
            body=json.dumps({"reason": reason}).encode("utf-8"),
        )

    def retry_run(self, run_id: str, *, nodes: Sequence[str] | None = None) -> dict[str, Any]:
        """Retry a FAILED or TIMED_OUT run, whole or from selected nodes.

        With ``nodes``, exactly those nodes and every downstream node that did not
        succeed are retried; upstream work is left as it is. Each named node must
        be FAILED, UPSTREAM_FAILED or CANCELLED. Without ``nodes`` (or with an
        empty sequence) every node that did not succeed is retried. Succeeded
        nodes are never re-run either way.

        The request is all-or-nothing. The gateway refuses it -- and retries
        nothing -- when a node is unknown (HTTP 422), is not in a retryable state
        or would fail again at once because an upstream failure it depends on is
        not also selected (409, raised as :class:`Conflict`; the message names
        the node to add), or when the run itself is not FAILED/TIMED_OUT (409).

        Each retried node gets a new attempt with a fresh retry budget and the
        same idempotency key as its earlier attempts, which stay in the run's
        history unchanged. Returns the run, now RUNNING, with ``retried_nodes``.
        """
        if isinstance(nodes, (str, bytes)):
            # A bare string is a Sequence[str] too, and list("load") would send
            # ["l", "o", "a", "d"]: four unknown nodes instead of one.
            raise TypeError("nodes must be a sequence of node ids, not a single string; use nodes=[...]")
        payload: dict[str, Any] = {}
        if nodes:
            payload["nodes"] = list(nodes)
        return self._call(
            "POST",
            self._url(f"/runs/{urllib.parse.quote(run_id, safe='')}/retry"),
            body=json.dumps(payload, sort_keys=True).encode("utf-8"),
        )

    def pause_run(self, run_id: str) -> dict[str, Any]:
        return self._call("POST", self._url(f"/runs/{urllib.parse.quote(run_id, safe='')}/pause"), body=b"{}")

    def resume_run(self, run_id: str) -> dict[str, Any]:
        return self._call("POST", self._url(f"/runs/{urllib.parse.quote(run_id, safe='')}/resume"), body=b"{}")

    # ── events and logs ─────────────────────────────────────────────────────
    def get_events(self, run_id: str, *, after_seq: int | None = None) -> dict[str, Any]:
        """Event history as a single response. `run_events` is append-only."""
        return self._call(
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
        resp = self._open(
            "GET", url, headers={"Accept": "text/event-stream"}, timeout=timeout
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
        resp = self._open("GET", url, headers={"Accept": "text/plain"}, timeout=timeout)
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
        return self._call(
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
        return self._call(
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

    def get_assignment(self, attempt_id: str, *, run_id: str = "-") -> dict[str, Any]:
        """An attempt's resolved inputs and TaskSpec, for replaying it locally.

        ``run_id`` narrows the lookup; the default ``"-"`` finds the attempt in
        any run of the namespace. The response never contains the lease token
        or a secret value -- SecretRefs stay references -- and blob inputs stay
        references into the object store. Pass it to ``nexus.replay.replay``.
        Needs the runs.view, tasks.view and tasks.results.view permissions.
        """
        return self._call(
            "GET",
            self._url(
                f"/runs/{urllib.parse.quote(run_id, safe='')}/attempts/"
                f"{urllib.parse.quote(attempt_id, safe='')}/assignment"
            ),
        )

    # ── fleet ───────────────────────────────────────────────────────────────
    def list_workers(self, *, state: str | None = None) -> dict[str, Any]:
        return self._call("GET", self._url("/workers", {"state": state}))

    def drain_worker(self, worker_id: str) -> dict[str, Any]:
        return self._call(
            "POST", self._url(f"/workers/{urllib.parse.quote(worker_id, safe='')}/drain"), body=b"{}"
        )

    def capacity(self) -> dict[str, Any]:
        """Advertised vs committed capacity -- the ledger, as the platform sees it."""
        return self._call("GET", self._url("/capacity"))

    # ── schedules ───────────────────────────────────────────────────────────
    def list_schedules(self, *, limit: int | None = None) -> list[Schedule]:
        """Every schedule in the namespace."""
        out = self._call("GET", self._url("/schedules", {"limit": limit}))
        return [cast(Schedule, x) for x in out.get("schedules") or []]

    def get_schedule(self, name: str, *, preview_count: int | None = None) -> Schedule:
        """One schedule, with its next ``preview_count`` fire times (default 5, max 20)."""
        return cast(Schedule, self._call("GET", self._url(f"/schedules/{_q(name)}", {"count": preview_count})))

    def create_schedule(
        self,
        name: str,
        *,
        workflow: str,
        cron: str,
        timezone: str | None = None,
        params: Mapping[str, Any] | None = None,
        spec_version: int | None = None,
        priority: int | None = None,
        overlap_policy: OverlapPolicy | None = None,
        catchup_limit: int | None = None,
        enabled: bool = True,
    ) -> Schedule:
        """Create a schedule that starts runs of ``workflow``.

        The gateway validates the cron expression, the timezone and that
        ``workflow`` names a registered workflow before storing anything, so a
        typo fails here rather than as a schedule that never fires. Raises
        :class:`Conflict` if the name is taken. The response carries
        ``next_fires``: check it.
        """
        body = _schedule_body(
            name, workflow=workflow, cron=cron, timezone=timezone, params=params,
            spec_version=spec_version, priority=priority, overlap_policy=overlap_policy,
            catchup_limit=catchup_limit, enabled=enabled,
        )
        return cast(Schedule, self._call("POST", self._url("/schedules"), body=_json(body)))

    def update_schedule(
        self,
        name: str,
        *,
        workflow: str | None = None,
        cron: str | None = None,
        timezone: str | None = None,
        params: Mapping[str, Any] | None = None,
        spec_version: int | None | _Unset = _UNSET,
        priority: int | None = None,
        overlap_policy: OverlapPolicy | None = None,
        catchup_limit: int | None = None,
    ) -> Schedule:
        """Change some fields of a schedule, keeping the rest.

        The gateway's PUT replaces a schedule whole, so this reads it, applies
        the given changes and writes it back; ``enabled`` is preserved (use
        :meth:`enable_schedule` / :meth:`disable_schedule`). ``params`` REPLACES
        the bound parameters. ``spec_version=None`` unpins the schedule so it
        follows the latest version; leave it out to keep the current pin.

        Read-modify-write is not atomic: a concurrent update between the read
        and the write is overwritten.
        """
        current = self.get_schedule(name)
        body = _schedule_body(
            name,
            workflow=workflow if workflow is not None else current.get("spec_name", ""),
            cron=cron if cron is not None else current.get("cron", ""),
            timezone=timezone if timezone is not None else current.get("timezone"),
            params=params if params is not None else current.get("params"),
            spec_version=current.get("spec_version") if isinstance(spec_version, _Unset) else spec_version,
            priority=priority if priority is not None else current.get("priority"),
            overlap_policy=overlap_policy if overlap_policy is not None else current.get("overlap_policy"),
            catchup_limit=catchup_limit if catchup_limit is not None else current.get("catchup_limit"),
            enabled=bool(current.get("enabled", True)),
        )
        return cast(Schedule, self._call("PUT", self._url(f"/schedules/{_q(name)}"), body=_json(body)))

    def delete_schedule(self, name: str) -> None:
        """Delete a schedule and its tick history. Runs it created are kept."""
        self._call("DELETE", self._url(f"/schedules/{_q(name)}"))

    def enable_schedule(self, name: str) -> Schedule:
        """Re-enable; the next fire time is computed from now (missed ticks are not replayed)."""
        return cast(Schedule, self._call("POST", self._url(f"/schedules/{_q(name)}/enable"), body=b"{}"))

    def disable_schedule(self, name: str) -> Schedule:
        return cast(Schedule, self._call("POST", self._url(f"/schedules/{_q(name)}/disable"), body=b"{}"))

    def preview_schedule(
        self, cron: str, *, timezone: str | None = None, count: int | None = None
    ) -> SchedulePreview:
        """Evaluate a cron expression without creating anything.

        An invalid expression is a normal answer (``valid`` False, ``error``
        set), not an exception: this call exists to ask the question.
        """
        return cast(SchedulePreview, self._call(
            "GET", self._url("/schedules/preview", {"cron": cron, "timezone": timezone, "count": count})
        ))

    # ── backfills ───────────────────────────────────────────────────────────
    def start_backfill(
        self,
        schedule: str,
        *,
        start: datetime.datetime | str,
        end: datetime.datetime | str,
        max_active: int | None = None,
        max_runs: int | None = None,
    ) -> Backfill:
        """Run ``schedule`` for every tick in the half-open interval [start, end).

        ``start``/``end`` are timezone-aware datetimes or RFC 3339 strings; a
        naive datetime is refused, because whether it means UTC or local time
        is exactly the ambiguity that backfills the wrong week. ``max_active``
        (default 1, at most 50) bounds runs in flight; ``max_runs`` (default
        1000, at most 100000) bounds the total.
        """
        if max_active is not None and not 1 <= max_active <= MAX_BACKFILL_ACTIVE:
            raise ValueError(f"max_active must be between 1 and {MAX_BACKFILL_ACTIVE}, got {max_active}")
        if max_runs is not None and not 1 <= max_runs <= MAX_BACKFILL_RUNS:
            raise ValueError(f"max_runs must be between 1 and {MAX_BACKFILL_RUNS}, got {max_runs}")
        lo, hi = _instant("start", start), _instant("end", end)
        if datetime.datetime.fromisoformat(lo) >= datetime.datetime.fromisoformat(hi):
            raise ValueError(f"start ({lo}) must be before end ({hi})")
        body: dict[str, Any] = {"from": lo, "to": hi}
        if max_active is not None:
            body["max_active"] = max_active
        if max_runs is not None:
            body["max_runs"] = max_runs
        return cast(Backfill, self._call("POST", self._url(f"/schedules/{_q(schedule)}/backfills"), body=_json(body)))

    def list_backfills(self, schedule: str, *, limit: int | None = None) -> list[Backfill]:
        out = self._call("GET", self._url(f"/schedules/{_q(schedule)}/backfills", {"limit": limit}))
        return [cast(Backfill, x) for x in out.get("backfills") or []]

    def get_backfill(self, backfill_id: str) -> Backfill:
        return cast(Backfill, self._call("GET", self._url(f"/backfills/{_q(backfill_id)}")))

    def pause_backfill(self, backfill_id: str) -> Backfill:
        """Stop generating runs; runs already created continue. Resume later."""
        return self._backfill_action(backfill_id, "pause", ("RUNNING",))

    def resume_backfill(self, backfill_id: str) -> Backfill:
        return self._backfill_action(backfill_id, "resume", ("PAUSED",))

    def cancel_backfill(self, backfill_id: str) -> Backfill:
        """Stop generating runs for good. Runs already created are NOT cancelled."""
        return self._backfill_action(backfill_id, "cancel", ("RUNNING", "PAUSED"))

    def _backfill_action(self, backfill_id: str, action: str, allowed: tuple[str, ...]) -> Backfill:
        # The server applies these without checking the current state
        # (CONTRACT-FINDINGS F119): resuming a COMPLETED or CANCELLED backfill
        # would revive it. Check first; racy, but it stops the common mistake.
        state = self.get_backfill(backfill_id).get("state")
        if state not in allowed:
            raise Conflict(
                409,
                f"cannot {action} a backfill that is {state} (only {' or '.join(allowed)})",
                body={"state": state},
            )
        return cast(Backfill, self._call("POST", self._url(f"/backfills/{_q(backfill_id)}/{action}"), body=b"{}"))

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


def _as_compiled(spec: object) -> CompiledWorkflow:
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


def _locate_problems(e: NexusAPIError, compiled: CompiledWorkflow) -> None:
    """Attach the gateway's problems to a 422, placed in the author's Python.

    The gateway's own line numbers refer to the canonical JSON this client sent
    -- one line -- so the useful position is the one only this process knows:
    the source map the compiler recorded while tracing.
    """
    body = e.body if isinstance(e.body, Mapping) else {}
    raw = body.get("problems")
    if not isinstance(raw, list):
        return
    lines = []
    for p in raw:
        if not isinstance(p, Mapping):
            continue
        problem = dict(p)
        loc = compiled.locate(str(problem.get("path", "")))
        if loc is not None:
            problem["location"] = str(loc)
        e.problems.append(problem)
        where = problem.get("location") or "(no source line)"
        lines.append(f"  {where}: {problem.get('message', '')} ({problem.get('path', '')})")
    if lines:
        e.args = (f"{e.args[0]}\n" + "\n".join(lines),)


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


def _parse_sse(stream: Iterable[bytes]) -> Iterator[dict[str, Any]]:
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
