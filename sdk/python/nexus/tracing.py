# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""W3C trace context for task processes: join the platform's trace.

The worker agent exports the attempt's trace context to every workload it
launches (subprocess, docker and kubernetes executors alike) as environment
variables, following OpenTelemetry's "environment variables as context
propagation carriers" convention:

    TRACEPARENT=00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
    TRACESTATE=vendor=value            (only when non-empty)

This module reads them, validates ``TRACEPARENT`` strictly, and exposes the
result on ``TaskContext`` (``ctx.traceparent``, ``ctx.trace_id``, ...).

OpenTelemetry is OPTIONAL (``pip install 'nexus-sdk[opentelemetry]'``). When
``opentelemetry-api`` is importable the handler runs inside a span named
``nexus.task <node>`` that is a CHILD of the platform's attempt span, so any span
the handler creates joins the platform's trace. The SDK never configures a
TracerProvider or an exporter: with none configured the OTel API is a no-op and
this costs nothing. Configure it yourself (``opentelemetry-instrument`` or your
own ``TracerProvider`` driven by the ``OTEL_*`` environment).

A missing or malformed trace context never fails a task: an invalid value is
ignored with a DEBUG log, and a broken OpenTelemetry installation degrades to
running the handler without a span.
"""

from __future__ import annotations

import contextlib
import dataclasses
import logging
import os
import re
from typing import TYPE_CHECKING, Any, Iterator, Mapping

if TYPE_CHECKING:
    from .context import TaskContext

__all__ = [
    "TRACEPARENT_ENV",
    "TRACESTATE_ENV",
    "TraceContext",
    "parse_traceparent",
    "trace_context_from_environment",
    "otel_available",
    "task_span",
    "propagation_headers",
]

TRACEPARENT_ENV = "TRACEPARENT"
TRACESTATE_ENV = "TRACESTATE"

# W3C Trace Context level 1, version 00: exactly four lowercase-hex fields. A
# future version may append fields, but this SDK only claims to understand 00,
# and "strict" here means a value we do not fully understand is not propagated.
_TRACEPARENT_RE = re.compile(r"^00-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$")
_ZERO_TRACE_ID = "0" * 32
_ZERO_SPAN_ID = "0" * 16
# W3C: a tracestate header is at most 32 list-members; 512 characters is the
# size vendors are required to propagate. Anything beyond is dropped rather than
# truncated, since truncation could cut a member in half.
_TRACESTATE_MAX_LEN = 512
_TRACESTATE_BAD_CHARS = re.compile(r"[^\x20-\x7e]")

_log = logging.getLogger("nexus.tracing")


@dataclasses.dataclass(frozen=True, slots=True)
class TraceContext:
    """A validated W3C trace context: the platform attempt span's identity.

    ``span_id`` is the PARENT of the handler's span (the agent's attempt span),
    not a span this process created.
    """

    trace_id: str
    span_id: str
    trace_flags: str
    tracestate: str | None = None

    @property
    def traceparent(self) -> str:
        """The ``traceparent`` header value, re-serialized in canonical form."""
        return f"00-{self.trace_id}-{self.span_id}-{self.trace_flags}"

    @property
    def sampled(self) -> bool:
        """The W3C ``sampled`` flag (bit 0 of trace-flags)."""
        return bool(int(self.trace_flags, 16) & 0x01)

    def carrier(self) -> dict[str, str]:
        """A W3C header carrier: ``{"traceparent": ..., "tracestate": ...}``.

        ``tracestate`` is present only when non-empty.
        """
        out = {"traceparent": self.traceparent}
        if self.tracestate:
            out["tracestate"] = self.tracestate
        return out


def parse_traceparent(value: str | None, tracestate: str | None = None) -> TraceContext | None:
    """Parse and strictly validate a ``traceparent`` value; None if absent or invalid.

    Rules (W3C Trace Context, version 00): version ``00``; a 32-hex trace id that
    is not all zeros; a 16-hex parent span id that is not all zeros; 2-hex flags;
    lowercase hex only; nothing else. Surrounding whitespace is tolerated (an env
    var written with a trailing newline). An invalid ``tracestate`` is dropped
    without invalidating the ``traceparent``.
    """
    if value is None:
        return None
    raw = value.strip()
    if not raw:
        return None
    m = _TRACEPARENT_RE.match(raw)
    if m is None:
        _log.debug("ignoring malformed traceparent", extra={"traceparent": raw[:128]})
        return None
    trace_id, span_id, flags = m.groups()
    if trace_id == _ZERO_TRACE_ID or span_id == _ZERO_SPAN_ID:
        _log.debug("ignoring traceparent with an all-zero id", extra={"traceparent": raw})
        return None
    return TraceContext(trace_id, span_id, flags, _clean_tracestate(tracestate))


def _clean_tracestate(value: str | None) -> str | None:
    if value is None:
        return None
    raw = value.strip()
    if not raw:
        return None
    if len(raw) > _TRACESTATE_MAX_LEN or _TRACESTATE_BAD_CHARS.search(raw):
        _log.debug("ignoring invalid tracestate", extra={"tracestate_len": len(raw)})
        return None
    return raw


def trace_context_from_environment(environ: Mapping[str, str] | None = None) -> TraceContext | None:
    """The trace context the worker agent exported, from ``TRACEPARENT``/``TRACESTATE``."""
    env = os.environ if environ is None else environ
    return parse_traceparent(env.get(TRACEPARENT_ENV), env.get(TRACESTATE_ENV))


def otel_available() -> bool:
    """True when ``opentelemetry-api`` can be imported in this process."""
    try:
        import opentelemetry.trace  # noqa: F401
    except ImportError:
        return False
    return True


def _span_attributes(ctx: "TaskContext") -> dict[str, str | int]:
    attrs: dict[str, str | int] = {}
    for key, value in (
        ("nexus.run_id", ctx.run_id),
        ("nexus.attempt_id", ctx.attempt_id),
        ("nexus.node_id", ctx.node_id),
        ("nexus.task_ref", ctx.task_ref),
        ("nexus.namespace", ctx.namespace),
    ):
        if value:
            attrs[key] = value
    if ctx.attempt_number:
        attrs["nexus.attempt_number"] = ctx.attempt_number
    if ctx.map_index is not None:
        attrs["nexus.map_index"] = ctx.map_index
    return attrs


def span_name(ctx: "TaskContext") -> str:
    return f"nexus.task {ctx.node_id or ctx.task_ref or 'unknown'}"


@contextlib.contextmanager
def task_span(ctx: "TaskContext") -> Iterator[Any]:
    """Run the enclosed handler call inside a child span of the platform's trace.

    Yields the OpenTelemetry span, or None when there is no trace context or
    OpenTelemetry is not installed (the handler then runs exactly as it would
    without this module). An exception escaping the block is recorded on the
    span, the span's status is set to ERROR, and the exception propagates
    unchanged -- classification and reporting stay the worker's job.
    """
    trace_ctx = ctx.trace_context
    started = _start_span(ctx, trace_ctx) if trace_ctx is not None else None
    if started is None:
        yield None
        return

    span, token = started
    from opentelemetry import context as otel_context
    from opentelemetry.trace import Status, StatusCode

    try:
        yield span
    except BaseException as e:
        try:
            span.record_exception(e)
            span.set_status(Status(StatusCode.ERROR, f"{type(e).__name__}: {e}"[:512]))
        except Exception:  # pragma: no cover - telemetry must not mask the error
            pass
        raise
    finally:
        try:
            span.end()
        finally:
            otel_context.detach(token)


def _start_span(ctx: "TaskContext", trace_ctx: TraceContext) -> tuple[Any, Any] | None:
    """Start the handler span and make it current; None if OTel is unusable."""
    try:
        from opentelemetry import context as otel_context
        from opentelemetry import trace
        from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator
    except ImportError:
        return None
    try:
        from . import __version__

        parent = TraceContextTextMapPropagator().extract(carrier=trace_ctx.carrier())
        tracer = trace.get_tracer("nexus.sdk", __version__)
        span = tracer.start_span(
            span_name(ctx),
            context=parent,
            kind=trace.SpanKind.INTERNAL,
            attributes=_span_attributes(ctx),
        )
        token = otel_context.attach(trace.set_span_in_context(span, parent))
        return span, token
    except Exception as e:  # a broken telemetry setup must never fail a task
        _log.debug("cannot start task span; running handler without one", exc_info=e)
        return None


def propagation_headers(ctx: "TaskContext | None" = None) -> dict[str, str]:
    """Headers that carry the CURRENT trace context to a downstream service.

    With OpenTelemetry installed and a span active (the handler span, or a span
    the handler started), the active context is injected with the W3C
    propagator, so the downstream service becomes a child of that span. Otherwise
    the platform's own ``traceparent``/``tracestate`` are returned as-is (from
    ``ctx``, or from the environment when ``ctx`` is None), so propagation still
    works without OpenTelemetry. Empty when there is no trace context at all.
    """
    try:
        from opentelemetry import trace
        from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator
    except ImportError:
        pass
    else:
        try:
            if trace.get_current_span().get_span_context().is_valid:
                carrier: dict[str, str] = {}
                TraceContextTextMapPropagator().inject(carrier)
                if carrier:
                    return carrier
        except Exception as e:  # pragma: no cover - fall back to the raw context
            _log.debug("cannot inject trace context", exc_info=e)
    trace_ctx = ctx.trace_context if ctx is not None else trace_context_from_environment()
    return trace_ctx.carrier() if trace_ctx is not None else {}
