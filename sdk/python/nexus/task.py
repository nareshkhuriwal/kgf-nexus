# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""The `@task` decorator: declares a task spec and registers its handler.

Two things happen here and it is worth being precise about which process each one
runs in:

  * The DECLARATION (resources, timeouts, retry, ...) is compiled into a TaskSpec
    fragment in the AUTHOR's process at import time, and ends up inside the
    WorkflowSpec document submitted over HTTP.
  * The HANDLER function is registered in a process-local registry used only by
    the worker-side entrypoint (`nexus.worker`), inside the task container.

The control plane sees the first and never the second. It does not import this
module, does not import the user's module, and cannot be destabilized by either
(ADR-0004). Nothing in this file may assume otherwise.
"""

from __future__ import annotations

import dataclasses
import inspect
from typing import TYPE_CHECKING, Any, Callable, Generic, Mapping, Sequence, TypeVar, overload

from .sourcemap import SourceLocation
from .types import (
    Cache,
    Cancellation,
    Concurrency,
    IdempotencyMode,
    Resources,
    Retry,
    Runtime,
    SpecError,
    Timeouts,
    TriggerRule,
    ValueSpec,
    ValueSpecLike,
    coerce_idempotency,
    coerce_trigger_rule,
)

if TYPE_CHECKING:
    from .context import TaskContext
    from .workflow import OutputRef, TaskInvocation

__all__ = ["task", "TaskDefinition", "registry", "HandlerRegistry"]

R = TypeVar("R")
R2 = TypeVar("R2")

# Platform defaults. These are chosen so that a task declaring nothing but a
# handler is still safe to run unattended: bounded attempt time, a real
# end-to-end SLA, and an idempotency mode that permits automatic recovery from
# indeterminate outcomes (ADR-0006 point 2).
_DEFAULT_TIMEOUTS = Timeouts(
    schedule_to_start="30m",
    start_to_close="1h",
    schedule_to_close="4h",
    heartbeat="5m",
)


class HandlerRegistry:
    """Process-local map from task_ref to handler callable.

    Exists purely so the worker-side entrypoint can resolve `task_ref` to code
    after importing the user's module INSIDE the task container. The control
    plane has no equivalent and must never grow one.
    """

    def __init__(self) -> None:
        self._handlers: dict[str, "TaskDefinition[Any]"] = {}

    def register(self, definition: "TaskDefinition[Any]") -> None:
        ref = definition.task_ref
        existing = self._handlers.get(ref)
        if existing is not None and existing.fn is not definition.fn:
            # Two different handlers behind one task_ref means the worker would
            # pick whichever module imported last -- a silent wrong-code bug that
            # is very hard to diagnose from a run's output.
            raise SpecError(
                f"task_ref {ref!r} is already registered to "
                f"{existing.fn.__module__}.{existing.fn.__qualname__}; task_ref "
                "must uniquely identify a handler"
            )
        self._handlers[ref] = definition

    def get(self, task_ref: str) -> "TaskDefinition[Any] | None":
        return self._handlers.get(task_ref)

    def refs(self) -> list[str]:
        return sorted(self._handlers)

    def clear(self) -> None:
        """Only for tests; a real process registers once at import."""
        self._handlers.clear()


registry = HandlerRegistry()


@dataclasses.dataclass(frozen=True, slots=True)
class _Declaration:
    """The frozen, already-validated author declaration for one task."""

    task_ref: str
    version: int
    queue: str
    resources: Resources | None
    timeouts: Timeouts
    retry: Retry | None
    idempotency: str
    cache: Cache | None
    cancellation: Cancellation | None
    runtime: Runtime | None
    node_selector: Mapping[str, str] | None
    prefer_same_worker_as: str | None
    concurrency: Concurrency | None
    priority: int
    trigger_rule: str
    inputs: Mapping[str, ValueSpec] | None
    outputs: Mapping[str, ValueSpec] | None


class TaskDefinition(Generic[R]):
    """A decorated task: a spec fragment plus (worker-side only) its handler.

    Calling a TaskDefinition inside a `@workflow` body does NOT run the handler.
    It records a node and returns handles to that node's declared outputs, which
    is how the DAG is traced. Calling it outside a workflow body runs the handler
    directly, which makes a task unit-testable without the platform.

    Generic in the handler's return type ``R``, so a type checker follows it:
    a traced call is a ``TaskInvocation[R]``, ``.map(...)`` a
    ``TaskInvocation[list[R | None]]`` (a child that did not succeed contributes
    null), and ``with_overrides`` keeps ``R``. A direct call whose first argument
    is a ``TaskContext`` is typed as the handler's own ``R``.
    """

    def __init__(self, fn: Callable[..., R], decl: _Declaration) -> None:
        self.fn: Callable[..., R] = fn
        self._decl = decl
        self.__name__ = getattr(fn, "__name__", decl.task_ref)
        self.__doc__ = fn.__doc__
        self.__wrapped__ = fn
        self._signature: inspect.Signature | None
        try:
            self._signature = inspect.signature(fn)
        except (TypeError, ValueError):  # builtins / C callables
            self._signature = None
        self.source: SourceLocation | None = SourceLocation.of_callable(fn)

    # ── identity ────────────────────────────────────────────────────────────
    @property
    def task_ref(self) -> str:
        return self._decl.task_ref

    @property
    def version(self) -> int:
        return self._decl.version

    @property
    def declared_outputs(self) -> tuple[str, ...]:
        if self._decl.outputs:
            return tuple(sorted(self._decl.outputs))
        # A task that declares no typed outputs still produces one value: its
        # return. Naming it consistently is what lets edges reference it.
        return ("result",)

    @property
    def parameter_names(self) -> tuple[str, ...]:
        """Handler parameters excluding the leading TaskContext."""
        if self._signature is None:
            return ()
        params = list(self._signature.parameters.values())
        if params and params[0].name in ("ctx", "context"):
            params = params[1:]
        return tuple(
            p.name
            for p in params
            if p.kind
            in (inspect.Parameter.POSITIONAL_ONLY, inspect.Parameter.POSITIONAL_OR_KEYWORD, inspect.Parameter.KEYWORD_ONLY)
        )

    @property
    def declaration(self) -> _Declaration:
        """The validated author declaration (read-only)."""
        return self._decl

    # ── call: trace inside a workflow, execute outside one ──────────────────
    #
    # The overlap is deliberate: a first positional TaskContext means "run the
    # handler" (a unit test), anything else means "trace a node". pyright flags
    # the overlap; mypy accepts it because the first overload is strictly narrower.
    @overload
    def __call__(  # pyright: ignore[reportOverlappingOverload]
        self, ctx: "TaskContext", /, *args: Any, **kwargs: Any
    ) -> R: ...

    @overload
    def __call__(self, *args: Any, **kwargs: Any) -> "TaskInvocation[R]": ...

    def __call__(self, *args: Any, **kwargs: Any) -> "R | TaskInvocation[R]":
        from .workflow import current_builder

        builder = current_builder()
        if builder is None:
            # Direct invocation: the author is unit-testing the handler. We do
            # not synthesize a TaskContext here, because a fake context that
            # silently no-ops `record_effect` would make an idempotency bug pass
            # its test. Tests should pass an explicit context.
            return self.fn(*args, **kwargs)
        return builder.add_node(self, args, dict(kwargs))

    @overload
    def map(
        self,
        over: "OutputRef[Sequence[Any]] | TaskInvocation[Sequence[Any]] | OutputRef[Any] | TaskInvocation[Any]",
        *args: Any,
        item: str,
        max_fanout: int,
        **kwargs: Any,
    ) -> "TaskInvocation[list[R | None]]": ...

    @overload
    def map(self, over: Sequence[Any], *args: Any, item: str, max_fanout: int, **kwargs: Any) -> list[R]: ...

    def map(
        self, over: object, *args: Any, item: str, max_fanout: int, **kwargs: Any
    ) -> "TaskInvocation[list[R | None]] | list[R]":
        """Fan this task out over a runtime collection (dynamic expansion, §5.4).

        Inside a workflow body: `over` is an upstream output holding a JSON
        array; at run time the platform creates one child of this task per
        element, passing the element as parameter `item`. `max_fanout` is
        MANDATORY: a longer collection fails the node explicitly and runs
        nothing. Other arguments are shared by every child. The returned handle
        refers to the JSON ARRAY of the children's outputs, in element order
        (null where a child did not succeed) -- pass it to a fan-in task.

        Outside a workflow body (unit tests): runs the handler once per element
        of the concrete list `over` and returns the list of results.
        """
        from .workflow import current_builder

        builder = current_builder()
        if builder is None:
            if not isinstance(over, Sequence) or isinstance(over, (str, bytes)):
                raise SpecError(
                    f"{self.task_ref}.map outside a workflow needs a concrete list, got "
                    f"{type(over).__name__}"
                )
            if isinstance(max_fanout, int) and len(over) > max_fanout:
                raise SpecError(
                    f"{self.task_ref}.map: {len(over)} items exceed max_fanout={max_fanout}"
                )
            return [self.fn(*args, **{**kwargs, item: element}) for element in over]
        return builder.add_map_node(self, over, item, max_fanout, args, dict(kwargs))

    def bind(self, *args: Any, **kwargs: Any) -> "R | TaskInvocation[R]":
        """Explicit alias for __call__, for readers who find tracing surprising."""
        result: R | TaskInvocation[R] = self(*args, **kwargs)
        return result

    def with_overrides(self, **overrides: Any) -> "TaskDefinition[R]":
        """A copy with some declaration fields replaced.

        Lets one handler appear as several nodes with different resources -- the
        common "same code, bigger box" case -- without duplicating the function.
        """
        decl = _build_declaration(self.fn, dict(_as_author_kwargs(self._decl)) | overrides)
        clone: TaskDefinition[R] = TaskDefinition(self.fn, decl)
        return clone

    # ── compilation ─────────────────────────────────────────────────────────
    def to_task_spec(self, node_id: str, inputs: Mapping[str, ValueSpec]) -> dict[str, Any]:
        """Emit the TaskSpec wire object for one DAG node.

        `node_id` (not task_ref) is the spec's `id`: the same handler may appear
        as several nodes, and node_id participates in the idempotency key, so it
        must be the per-node identity.
        """
        d = self._decl
        spec: dict[str, Any] = {
            "id": node_id,
            "task_ref": d.task_ref,
            "version": d.version,
            "queue": d.queue,
            "idempotency": d.idempotency,
            "trigger_rule": d.trigger_rule,
        }
        if inputs:
            spec["inputs"] = {k: v.to_wire() for k, v in sorted(inputs.items())}
        outputs = d.outputs or {name: ValueSpec(type="json") for name in self.declared_outputs}
        spec["outputs"] = {k: ValueSpec.coerce(v).to_wire() for k, v in sorted(outputs.items())}

        if d.resources is not None:
            res = d.resources.to_wire()
            if res:
                spec["resources"] = res
        timeouts = d.timeouts.to_wire()
        if timeouts:
            spec["timeouts"] = timeouts
        if d.retry is not None:
            spec["retry"] = d.retry.to_wire()
        if d.cache is not None:
            spec["cache"] = d.cache.to_wire()
        if d.cancellation is not None:
            # Omitted entirely when interruptible, so a task that never mentions
            # cancellation compiles to the same bytes -- and the same content
            # address -- as it did before the option existed.
            cancellation = d.cancellation.to_wire()
            if cancellation:
                spec["cancellation"] = cancellation
        if d.runtime is not None:
            spec["runtime"] = d.runtime.to_wire()
        if d.node_selector:
            spec["node_selector"] = {str(k): str(v) for k, v in sorted(d.node_selector.items())}
        if d.prefer_same_worker_as:
            spec["prefer_same_worker_as"] = d.prefer_same_worker_as
        if d.concurrency is not None:
            spec["concurrency_key"] = d.concurrency.key
            spec["concurrency_limit"] = d.concurrency.limit
        if d.priority:
            spec["priority"] = d.priority
        return spec

    def __repr__(self) -> str:
        return f"<task {self.task_ref} v{self.version}>"


def _as_author_kwargs(d: _Declaration) -> dict[str, Any]:
    """Reverse a declaration into author kwargs, for with_overrides."""
    return {
        "task_ref": d.task_ref,
        "version": d.version,
        "queue": d.queue,
        "resources": d.resources,
        "timeouts": d.timeouts,
        "retry": d.retry,
        "idempotency": d.idempotency,
        "cache": d.cache,
        "cancellation": d.cancellation,
        "runtime": d.runtime,
        "node_selector": d.node_selector,
        "prefer_same_worker_as": d.prefer_same_worker_as,
        "concurrency": d.concurrency,
        "priority": d.priority,
        "trigger_rule": d.trigger_rule,
        "inputs": d.inputs,
        "outputs": d.outputs,
    }


def _coerce_values(task_ref: object, field: str, raw: object) -> dict[str, ValueSpec] | None:
    """Coerce and eagerly validate an inputs= / outputs= mapping."""
    if raw is None:
        return None
    if not isinstance(raw, Mapping):
        raise SpecError(f"task {task_ref!r}: {field} must be a mapping, got {type(raw).__name__}")
    out: dict[str, ValueSpec] = {}
    for k, v in raw.items():
        vs = ValueSpec.coerce(v)
        try:
            vs.to_wire()
        except SpecError as e:
            raise SpecError(f"task {task_ref!r}: {field}.{k}: {e}") from None
        out[str(k)] = vs
    return out


def _build_declaration(fn: Callable[..., Any], kw: Mapping[str, Any]) -> _Declaration:
    name = getattr(fn, "__name__", None) or "task"
    task_ref = kw.get("task_ref") or kw.get("name") or name
    version = int(kw.get("version") or 1)
    if version < 1:
        raise SpecError(f"task {task_ref!r}: version must be >= 1, got {version}")

    idempotency = coerce_idempotency(kw.get("idempotency") or "required")
    trigger_rule = coerce_trigger_rule(kw.get("trigger_rule") or "all_success")

    timeouts = Timeouts.coerce(kw.get("timeouts")) or _DEFAULT_TIMEOUTS
    if kw.get("timeouts") is not None:
        # Merge over the platform defaults rather than replacing them wholesale:
        # a user who sets only `start_to_close` should not silently lose the
        # heartbeat timeout, which is the one that catches a wedged task.
        given = timeouts
        timeouts = Timeouts(
            schedule_to_start=given.schedule_to_start
            if given.schedule_to_start is not None
            else _DEFAULT_TIMEOUTS.schedule_to_start,
            start_to_close=given.start_to_close
            if given.start_to_close is not None
            else _DEFAULT_TIMEOUTS.start_to_close,
            schedule_to_close=given.schedule_to_close
            if given.schedule_to_close is not None
            else _DEFAULT_TIMEOUTS.schedule_to_close,
            heartbeat=given.heartbeat
            if given.heartbeat is not None
            else _DEFAULT_TIMEOUTS.heartbeat,
        )
    # Validate eagerly so a bad duration blows up at the decorator, not at submit.
    timeouts.to_wire()

    retry = Retry.coerce(kw.get("retry"))
    if retry is None:
        retry = Retry()
    retry.to_wire()

    cache = Cache.coerce(kw.get("cache"))
    if cache is not None:
        cache.to_wire()

    cancellation = Cancellation.coerce(kw.get("cancellation"))
    if cancellation is not None:
        cancellation.to_wire()

    runtime = Runtime.coerce(kw.get("runtime"))
    if runtime is not None:
        runtime.to_wire()

    raw_resources = kw.get("resources")
    resources: Resources | None
    if isinstance(raw_resources, Mapping):
        resources = Resources(**raw_resources)
    elif raw_resources is None or isinstance(raw_resources, Resources):
        resources = raw_resources
    else:
        raise SpecError(
            f"task {task_ref!r}: resources must be a Resources or a mapping, got "
            f"{type(raw_resources).__name__}"
        )
    if resources is not None:
        resources.to_wire()

    concurrency = Concurrency.coerce(kw.get("concurrency"))

    inputs = _coerce_values(task_ref, "inputs", kw.get("inputs"))
    outputs = _coerce_values(task_ref, "outputs", kw.get("outputs"))

    if runtime is not None and runtime.stdout_output:
        # Mirrors the gateway's submit-time check (validateStdoutOutput): the
        # agent collects only DECLARED outputs, so stdout captured under an
        # undeclared name would vanish and the task would succeed producing
        # nothing. Caught here, the traceback points at this decorator.
        declared = sorted(outputs) if outputs else ["result"]
        if runtime.stdout_output not in declared:
            raise SpecError(
                f"task {task_ref!r}: runtime.stdout_output names {runtime.stdout_output!r}, "
                f"which is not a declared output ({', '.join(declared)}); stdout would be "
                "captured under a name nothing can reference"
            )

    unknown = set(kw) - {
        "task_ref",
        "name",
        "version",
        "queue",
        "resources",
        "timeouts",
        "retry",
        "idempotency",
        "cache",
        "cancellation",
        "runtime",
        "node_selector",
        "prefer_same_worker_as",
        "concurrency",
        "priority",
        "trigger_rule",
        "inputs",
        "outputs",
    }
    if unknown:
        raise SpecError(
            f"task {task_ref!r}: unknown option(s) {', '.join(sorted(unknown))}"
        )

    return _Declaration(
        task_ref=str(task_ref),
        version=version,
        queue=str(kw.get("queue") or "default"),
        resources=resources,
        timeouts=timeouts,
        retry=retry,
        idempotency=idempotency,
        cache=cache,
        cancellation=cancellation,
        runtime=runtime,
        node_selector=dict(kw["node_selector"]) if kw.get("node_selector") else None,
        prefer_same_worker_as=kw.get("prefer_same_worker_as"),
        concurrency=concurrency,
        priority=int(kw.get("priority") or 0),
        trigger_rule=trigger_rule,
        inputs=inputs,
        outputs=outputs,
    )


@overload
def task(_fn: Callable[..., R], /) -> TaskDefinition[R]: ...


@overload
def task(
    _fn: None = None,
    /,
    *,
    task_ref: str | None = None,
    version: int = 1,
    queue: str = "default",
    resources: Resources | Mapping[str, Any] | None = None,
    timeouts: Timeouts | Mapping[str, Any] | None = None,
    retry: Retry | Mapping[str, Any] | int | None = None,
    idempotency: IdempotencyMode = "required",
    cache: Cache | Mapping[str, Any] | bool | None = None,
    cancellation: Cancellation | Mapping[str, Any] | bool | None = None,
    runtime: Runtime | Mapping[str, Any] | None = None,
    node_selector: Mapping[str, str] | None = None,
    prefer_same_worker_as: str | None = None,
    concurrency: Concurrency | Mapping[str, Any] | None = None,
    priority: int = 0,
    trigger_rule: TriggerRule = "all_success",
    inputs: Mapping[str, ValueSpecLike] | None = None,
    outputs: Mapping[str, ValueSpecLike] | None = None,
) -> Callable[[Callable[..., R]], TaskDefinition[R]]: ...


def task(
    _fn: Callable[..., R] | None = None,
    /,
    *,
    task_ref: str | None = None,
    version: int = 1,
    queue: str = "default",
    resources: Resources | Mapping[str, Any] | None = None,
    timeouts: Timeouts | Mapping[str, Any] | None = None,
    retry: Retry | Mapping[str, Any] | int | None = None,
    idempotency: IdempotencyMode = "required",
    cache: Cache | Mapping[str, Any] | bool | None = None,
    cancellation: Cancellation | Mapping[str, Any] | bool | None = None,
    runtime: Runtime | Mapping[str, Any] | None = None,
    node_selector: Mapping[str, str] | None = None,
    prefer_same_worker_as: str | None = None,
    concurrency: Concurrency | Mapping[str, Any] | None = None,
    priority: int = 0,
    trigger_rule: TriggerRule = "all_success",
    inputs: Mapping[str, ValueSpecLike] | None = None,
    outputs: Mapping[str, ValueSpecLike] | None = None,
) -> TaskDefinition[R] | Callable[[Callable[..., R]], TaskDefinition[R]]:
    """Declare a task. Compiles to a TaskSpec; does not hook the scheduler.

    Units are human-friendly and parsed here, in your process: cpu accepts "2"
    or "2000m", memory accepts "8Gi" or "512Mi", durations accept "10s", "30m",
    "4h", "1h30m".

    On idempotency -- three cooperating mechanisms, in DESCENDING order of
    strength. Which one you use determines what guarantee you actually have, so
    the SDK states the ordering rather than hiding it behind an abstraction:

      1. **Downstream-native dedup (strongest).** Pass `ctx.idempotency_key` to a
         system that already deduplicates on it (Stripe-style
         `idempotency_key=`). The downstream system makes the effect exactly-once
         at its own boundary, which is the only place it can be made so.
      2. **Conditional write.** `INSERT ... ON CONFLICT DO NOTHING` keyed on
         `ctx.idempotency_key`, or a compare-and-set. Atomic in the target store,
         so there is no window at all -- but only available where you control the
         schema.
      3. **Platform effect journal (weakest).** `ctx.effect_recorded()` /
         `ctx.record_effect()`. For systems with no native support. The
         write-then-crash window still EXISTS: if the process dies between the
         downstream write and `record_effect`, a retry repeats the effect. What
         the journal buys is that the window narrows from the whole task to a
         single write.

    `idempotency="required"` (the default) permits automatic retry of
    indeterminate (LOST) outcomes. `"none"` refuses it: such a task is marked
    FAILED with reason=indeterminate_outcome and waits for a human, because the
    platform cannot know whether it already ran (ADR-0006 point 5).

    `cancellation=Cancellation(max_wait="5m")` (or `cancellation=True`) declares
    the task uninterruptible: a cancel sends no SIGTERM and waits, bounded by
    `max_wait` and the worker's platform ceiling, then SIGKILLs (ADR-0015).
    """

    def decorate(fn: Callable[..., R]) -> TaskDefinition[R]:
        decl = _build_declaration(
            fn,
            {
                "task_ref": task_ref,
                "version": version,
                "queue": queue,
                "resources": resources,
                "timeouts": timeouts,
                "retry": retry,
                "idempotency": idempotency,
                "cache": cache,
                "cancellation": cancellation,
                "runtime": runtime,
                "node_selector": node_selector,
                "prefer_same_worker_as": prefer_same_worker_as,
                "concurrency": concurrency,
                "priority": priority,
                "trigger_rule": trigger_rule,
                "inputs": inputs,
                "outputs": outputs,
            },
        )
        definition: TaskDefinition[R] = TaskDefinition(fn, decl)
        registry.register(definition)
        return definition

    if _fn is not None:
        return decorate(_fn)
    return decorate
