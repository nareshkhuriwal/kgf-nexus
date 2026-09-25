# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""The `@workflow` decorator: traces task calls into a WorkflowSpec document.

How tracing works, and why it works this way:

The workflow body is a plain Python function. We call it ONCE, at compile time,
in the author's process, with a builder installed in a context variable. Task
calls inside the body do not execute handlers -- they record nodes and return
`OutputRef` handles. Passing an `OutputRef` as an argument to another task is
what creates an edge, and the argument's parameter NAME becomes the downstream
input name in `Edge.output_to_input`.

This is deliberately a *tracing* compiler and not an AST analyser: tracing means
loops, comprehensions and helper functions in the body all work, because they are
just Python. The cost is that the body runs, so a body with side effects will
perform them at compile time. That cost falls on the author's machine, never on
the control plane, which is the whole point (ADR-0004).

The emitted document is canonically ordered -- tasks sorted by id, edges sorted
by (from, to), every map key sorted -- because the server content-addresses it.
An unstable document would mint a new spec version on every compile of an
unchanged file.
"""

from __future__ import annotations

import contextvars
import dataclasses
import inspect
from typing import TYPE_CHECKING, Any, Callable, Generic, Mapping, NoReturn, Sequence, TypeVar, overload

from .canonical import canonical_json, spec_hash
from .sourcemap import NodeSource, SourceLocation, locate
from .types import SpecError, ValueSpec

if TYPE_CHECKING:
    from .task import TaskDefinition

__all__ = [
    "workflow",
    "WorkflowDefinition",
    "CompiledWorkflow",
    "OutputRef",
    "TaskInvocation",
    "Param",
    "Ref",
    "current_builder",
]

T_co = TypeVar("T_co", covariant=True)
_V = TypeVar("_V")

API_VERSION = "nexus.dev/v1"

#: internal/dag DefaultMaxFanInBytes: the fan-in limit a gateway applies unless
#: its operator configured NEXUS_DAG_MAX_FANIN_BYTES. The local runner uses it.
DEFAULT_MAX_FAN_IN_BYTES = 16 << 20
#: internal/dag MaxFanInBytesCeiling: the most any operator may configure.
MAX_FAN_IN_BYTES_CEILING = 256 << 20


def fan_in_worst_case(max_fanout: int, max_bytes: int) -> int:
    """Largest fan-in array max_fanout children of max_bytes each can produce:
    the elements, the commas between them and the two brackets (internal/dag
    validate.go uses the same arithmetic)."""
    return max_fanout * max_bytes + (max_fanout - 1) + 2

_builder: contextvars.ContextVar["_Builder | None"] = contextvars.ContextVar(
    "nexus_workflow_builder", default=None
)


def current_builder() -> "_Builder | None":
    """The builder for the workflow currently being traced, or None.

    A ContextVar rather than a module global so that compiling two workflows
    concurrently (threads, or async) cannot interleave their nodes.
    """
    return _builder.get()


# ── Handles produced during tracing ───────────────────────────────────────────


@dataclasses.dataclass(frozen=True, slots=True)
class OutputRef(Generic[T_co]):
    """A handle to one named output of one traced node.

    Generic in the type the output will hold AT RUN TIME, so a type checker can
    follow a task's return annotation through the graph: ``extract(src)`` is a
    ``TaskInvocation[dict[str, int]]`` and its ``.primary`` an
    ``OutputRef[dict[str, int]]``. The parameter is erased at run time; it is
    purely a static-typing aid and never reaches the compiled document.

    Opaque on purpose: it holds no value, because no task has run. Attempting to
    use it as a value (arithmetic, truth-testing, iteration) raises rather than
    silently producing something meaningless, since a compiler that quietly
    accepts `if result:` would emit a graph the author did not write.
    """

    node_id: str
    output: str

    def __bool__(self) -> bool:
        raise SpecError(
            f"cannot evaluate the truth of {self!r}: task outputs do not exist at "
            "compile time. Use a trigger_rule or (Phase 2) a conditional edge to "
            "branch on runtime values."
        )

    def __iter__(self) -> NoReturn:
        raise SpecError(
            f"cannot iterate {self!r}: the size of a task's output is a runtime "
            "property. To fan out over a runtime collection use dynamic expansion: "
            "`some_task.map(this_output, item=\"param\", max_fanout=N)` (§5.4)."
        )

    def __getattr__(self, item: str) -> NoReturn:
        if item.startswith("_"):
            raise AttributeError(item)
        raise SpecError(
            f"cannot access .{item} on {self!r}: task outputs are references, not "
            "values, at compile time"
        )

    def __repr__(self) -> str:
        return f"OutputRef({self.node_id}.{self.output})"


class TaskInvocation(Generic[T_co]):
    """The result of calling a task inside a workflow body.

    Behaves as its primary output (so `b(a())` reads naturally for single-output
    tasks) while also exposing every declared output by name via `["model"]`.

    Generic in the handler's return type: ``primary`` is an ``OutputRef`` of that
    type. A NAMED output (``node["model"]``) is ``OutputRef[Any]`` because the
    compiler cannot know which part of a multi-output return each name holds;
    ``node.output("model", Model)`` states it explicitly.
    """

    __slots__ = ("node_id", "_outputs")

    def __init__(self, node_id: str, outputs: Sequence[str]) -> None:
        self.node_id = node_id
        self._outputs: dict[str, OutputRef[Any]] = {name: OutputRef(node_id, name) for name in outputs}

    @property
    def primary(self) -> OutputRef[T_co]:
        if len(self._outputs) == 1:
            return next(iter(self._outputs.values()))
        if "result" in self._outputs:
            return self._outputs["result"]
        raise SpecError(
            f"node {self.node_id!r} declares multiple outputs "
            f"({', '.join(sorted(self._outputs))}); select one explicitly, e.g. "
            f"node['{sorted(self._outputs)[0]}']"
        )

    def __getitem__(self, name: str) -> OutputRef[Any]:
        try:
            return self._outputs[name]
        except KeyError:
            raise SpecError(
                f"node {self.node_id!r} has no output {name!r}; declared outputs are "
                f"{', '.join(sorted(self._outputs)) or '(none)'}"
            ) from None

    def output(self, name: str, _type: type[_V]) -> OutputRef[_V]:
        """A named output with its run-time type stated, for type checkers.

        ``node.output("model", dict)`` is ``node["model"]`` typed as
        ``OutputRef[dict]``. The type argument is not checked at run time --
        outputs do not exist at compile time -- it only informs static analysis.
        """
        return self[name]

    def outputs(self) -> Mapping[str, OutputRef[Any]]:
        return dict(self._outputs)

    def __bool__(self) -> bool:
        # A traced call returns this, not an OutputRef, so the value-semantics
        # guard has to live here as well or `if task():` compiles a graph the
        # author never wrote.
        raise SpecError(
            f"cannot evaluate the truth of {self!r}: task outputs do not exist at "
            "compile time. Use a trigger_rule or (Phase 2) a conditional edge to "
            "branch on runtime values."
        )

    def __iter__(self) -> NoReturn:
        raise SpecError(
            f"cannot iterate {self!r}: the size of a task's output is a runtime "
            "property. To fan out over a runtime collection use dynamic expansion: "
            "`some_task.map(this_output, item=\"param\", max_fanout=N)` (§5.4)."
        )

    def __repr__(self) -> str:
        return f"TaskInvocation({self.node_id}: {', '.join(sorted(self._outputs))})"


#: Anything a workflow body can pass where a task's run-time value of type T is
#: expected: a traced node or one of its outputs. Useful for annotating helper
#: functions that wire several tasks together.
Ref = OutputRef[T_co] | TaskInvocation[T_co]


@dataclasses.dataclass(frozen=True, slots=True)
class Param:
    """A reference to a workflow-level parameter, bound at run creation.

    Distinct from a literal so the compiled spec records "this input comes from
    run param X" instead of baking in whatever value the author's process
    happened to hold. Baking it in would make the spec hash depend on the
    author's environment and defeat content addressing.
    """

    name: str

    def __repr__(self) -> str:
        return f"Param({self.name})"


# ── The builder ───────────────────────────────────────────────────────────────


@dataclasses.dataclass(slots=True)
class _Node:
    node_id: str
    definition: "TaskDefinition[Any]"
    literals: dict[str, Any]
    params: dict[str, str]
    upstream: dict[str, tuple[str, str]]  # input name -> (from_node, output)
    # Set on a dynamic-expansion TEMPLATE (§5.4): the wire MapSpec.
    map_spec: dict[str, Any] | None = None
    # The line in the workflow body whose call traced this node.
    call_site: SourceLocation | None = None


class _Builder:
    """Accumulates nodes and edges while a workflow body is traced."""

    def __init__(self, workflow_name: str) -> None:
        self.workflow_name = workflow_name
        self.nodes: list[_Node] = []
        self._by_id: dict[str, _Node] = {}
        self._ref_counts: dict[str, int] = {}

    def _allocate_node_id(self, definition: "TaskDefinition[Any]") -> str:
        """Derive a stable node id from the handler name, suffixed on reuse.

        Stability matters beyond neatness: node_id participates in the
        idempotency key (§6.2), so a node id that shifted between compiles would
        change the key and defeat dedup across retries of the same logical work.
        The suffix is assigned by call order within the body, which is
        deterministic for a deterministic body.
        """
        base = str(getattr(definition, "__name__", None) or definition.task_ref)
        n = self._ref_counts.get(base, 0)
        self._ref_counts[base] = n + 1
        node_id = base if n == 0 else f"{base}_{n + 1}"
        while node_id in self._by_id:  # an author-chosen id may already hold it
            n += 1
            self._ref_counts[base] = n + 1
            node_id = f"{base}_{n + 1}"
        return node_id

    def add_node(
        self, definition: "TaskDefinition[Any]", args: Sequence[Any], kwargs: dict[str, Any]
    ) -> TaskInvocation[Any]:
        requested = kwargs.pop("node_id", None)
        if requested is None:
            node_id = self._allocate_node_id(definition)
        else:
            node_id = str(requested)
            if node_id in self._by_id:
                raise SpecError(f"duplicate node_id {node_id!r} in workflow")

        bound = self._bind_arguments(definition, node_id, args, kwargs)

        node = _Node(
            node_id=node_id,
            definition=definition,
            literals={},
            params={},
            upstream={},
            call_site=SourceLocation.of_caller(),
        )
        for name, value in bound.items():
            self._classify_argument(node, name, value)

        self.nodes.append(node)
        self._by_id[node_id] = node
        return TaskInvocation(node_id, definition.declared_outputs)

    def add_map_node(
        self,
        definition: "TaskDefinition[Any]",
        over: object,
        item: str,
        max_fanout: int,
        args: Sequence[Any],
        kwargs: Mapping[str, Any],
    ) -> TaskInvocation[Any]:
        """Record a map TEMPLATE node (§5.4).

        `over` names the collection; `item` is the handler parameter each child
        receives one element through. The remaining arguments bind exactly as
        for an ordinary call and are shared by every child. The returned handle's
        outputs are, at run time, JSON ARRAYS of the children's outputs in
        element order -- which is what a downstream fan-in task receives.
        """
        if isinstance(over, TaskInvocation):
            over = over.primary
        if not isinstance(over, OutputRef):
            raise SpecError(
                f"{definition.task_ref}.map: `over` must be an upstream task output, got "
                f"{type(over).__name__}; a literal list is known at compile time and "
                "needs no dynamic expansion"
            )
        if isinstance(max_fanout, bool) or not isinstance(max_fanout, int) or max_fanout < 1:
            raise SpecError(
                f"{definition.task_ref}.map: max_fanout is mandatory and must be a positive "
                f"integer, got {max_fanout!r}. An unbounded fan-out is how one runaway "
                "upstream takes the control plane down (§5.4)."
            )
        names = definition.parameter_names
        if names and item not in names:
            raise SpecError(
                f"{definition.task_ref}.map: item={item!r} is not a handler parameter; "
                f"parameters are {', '.join(names) or 'none'}"
            )
        if item in kwargs:
            raise SpecError(
                f"{definition.task_ref}.map: {item!r} is bound by the map, one element per "
                "child; do not also pass it"
            )
        # Positional args must skip the item parameter: bind by name instead.
        remaining = [n for n in names if n != item]
        if len(args) > len(remaining):
            raise SpecError(
                f"{definition.task_ref}.map: too many positional arguments; pass shared "
                "inputs by keyword"
            )
        kw = dict(zip(remaining, args))
        kw.update(kwargs)
        inv = self.add_node(definition, (), kw)
        node = self._by_id[inv.node_id]
        node.map_spec = {
            "over_node_id": over.node_id,
            "over_output": over.output,
            "item_input": item,
            "max_fanout": int(max_fanout),
        }
        return inv

    def _bind_arguments(
        self, definition: "TaskDefinition[Any]", node_id: str, args: Sequence[Any], kwargs: Mapping[str, Any]
    ) -> dict[str, Any]:
        """Map positional and keyword args onto handler parameter names.

        Parameter names are the interface: `Edge.output_to_input` is keyed by the
        DOWNSTREAM INPUT NAME, so a positional argument has to be resolved to a
        name before an edge can be emitted at all.
        """
        names = definition.parameter_names
        if len(args) > len(names):
            raise SpecError(
                f"node {node_id!r} ({definition.task_ref}): got {len(args)} positional "
                f"argument(s) but the handler accepts {len(names)} "
                f"({', '.join(names) or 'none'} after ctx)"
            )
        bound: dict[str, Any] = {}
        for name, value in zip(names, args):
            bound[name] = value
        for name, value in kwargs.items():
            if name in bound:
                raise SpecError(
                    f"node {node_id!r}: argument {name!r} given both positionally "
                    "and by keyword"
                )
            if names and name not in names:
                raise SpecError(
                    f"node {node_id!r} ({definition.task_ref}): handler has no "
                    f"parameter {name!r}; parameters are {', '.join(names) or 'none'}"
                )
            bound[name] = value
        return bound

    def _classify_argument(self, node: _Node, name: str, value: Any) -> None:
        """Sort one argument into upstream edge / run param / literal."""
        if isinstance(value, TaskInvocation):
            value = value.primary
        if isinstance(value, OutputRef):
            node.upstream[name] = (value.node_id, value.output)
            return
        if isinstance(value, Param):
            node.params[name] = value.name
            return
        if isinstance(value, (list, tuple)) and any(
            isinstance(v, (OutputRef, TaskInvocation)) for v in value
        ):
            # A list mixing refs and literals has no representation in the wire
            # Edge type (output_to_input is a flat name->name map). Rejecting is
            # better than picking one interpretation the author cannot see.
            raise SpecError(
                f"node {node.node_id!r}: argument {name!r} is a collection containing "
                "task outputs. Pass a single upstream output per input; collecting "
                "several requires a fan-in task that takes them as separate inputs."
            )
        node.literals[name] = value


# ── Compiled output ───────────────────────────────────────────────────────────


@dataclasses.dataclass(frozen=True, slots=True)
class CompiledWorkflow:
    """A WorkflowSpec document plus its content address.

    `spec` is the exact object submitted; `json` is the exact bytes hashed. Both
    are exposed because `nexusctl workflow compile --show` exists precisely so a
    user can see the document rather than trust that it matches their intent
    (ADR-0004 consequences).
    """

    spec: Mapping[str, Any]
    json: str
    hash: str
    #: Node id -> where the node came from in the author's Python. Carried beside
    #: the document, never in it, so moving a line does not change the hash.
    source_map: Mapping[str, NodeSource] = dataclasses.field(default_factory=dict)

    def locate(self, path: str) -> SourceLocation | None:
        """The Python source location for a problem path such as
        ``/tasks/load/inputs/summary`` (semantic) or ``/tasks/2/queue`` (schema)."""
        return locate(self.source_map, self.task_ids, path)

    def source_map_json(self) -> dict[str, Any]:
        """The source map as plain JSON, for tools such as nexusctl."""
        return {k: v.to_json() for k, v in sorted(self.source_map.items())}

    def to_json(self, indent: int | None = None) -> str:
        """Canonical JSON by default; indent only for human reading.

        An indented rendering is NOT what gets hashed -- pretty-printing changes
        the bytes -- so this returns the canonical string unless asked otherwise.
        """
        if indent is None:
            return self.json
        import json as _json

        return _json.dumps(self.spec, indent=indent, sort_keys=True, ensure_ascii=False)

    def to_yaml(self) -> str:
        """YAML rendering for review and for the first-class YAML authoring path.

        ADR-0004 requires YAML and Python to produce the identical document, so
        this is a rendering of the same object, never a separate serializer.
        """
        try:
            import yaml  # type: ignore[import-untyped,unused-ignore]
        except ModuleNotFoundError as e:  # pragma: no cover - optional extra
            raise RuntimeError(
                "YAML output needs PyYAML: pip install 'nexus-sdk[yaml]'"
            ) from e
        return str(yaml.safe_dump(_plain(self.spec), sort_keys=True, default_flow_style=False))

    @property
    def task_ids(self) -> list[str]:
        return [str(t["id"]) for t in self.spec["tasks"]]


def _plain(value: Any) -> Any:
    """Recursively convert to plain builtins so yaml.safe_dump accepts it."""
    if isinstance(value, Mapping):
        return {k: _plain(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [_plain(v) for v in value]
    return value


class WorkflowDefinition:
    """A decorated workflow. `compile()` produces the submittable document."""

    def __init__(self, fn: Callable[..., object], options: Mapping[str, Any]) -> None:
        self.fn = fn
        self.name: str = str(options.get("name") or getattr(fn, "__name__", "workflow"))
        self.namespace: str = str(options.get("namespace") or "default")
        self.description: str = str(options.get("description") or (fn.__doc__ or "").strip())
        self.params: Mapping[str, ValueSpec] = {
            str(k): ValueSpec.coerce(v) for k, v in (options.get("params") or {}).items()
        }
        self.run_timeout = options.get("run_timeout")
        self.fail_fast: bool = bool(options.get("fail_fast", False))
        self.max_active_runs: int = int(options.get("max_active_runs") or 0)
        self.priority: int = int(options.get("priority") or 0)
        self.labels: Mapping[str, str] = {
            str(k): str(v) for k, v in (options.get("labels") or {}).items()
        }
        self.__name__ = getattr(fn, "__name__", self.name)
        self.__doc__ = fn.__doc__
        self.__wrapped__ = fn

        unknown = set(options) - {
            "name",
            "namespace",
            "description",
            "params",
            "run_timeout",
            "fail_fast",
            "max_active_runs",
            "priority",
            "labels",
        }
        if unknown:
            raise SpecError(
                f"workflow {self.name!r}: unknown option(s) {', '.join(sorted(unknown))}"
            )

    def __call__(self, *args: Any, **kwargs: Any) -> object:
        """Calling a workflow directly runs the body untraced.

        Useful for reading the body in a REPL; it does NOT submit or execute
        anything on the platform.
        """
        return self.fn(*args, **kwargs)

    def compile(self, **param_overrides: Any) -> CompiledWorkflow:
        """Trace the body and emit the canonical WorkflowSpec document.

        `param_overrides` supplies Param placeholders for body parameters the
        author did not declare in `params=`; it does not bake values into the
        spec, only satisfies the body's signature during tracing.
        """
        builder = _Builder(self.name)
        token = _builder.set(builder)
        try:
            self._invoke_body(builder, param_overrides)
        finally:
            _builder.reset(token)

        if not builder.nodes:
            raise SpecError(
                f"workflow {self.name!r} traced zero tasks. A workflow body must call "
                "at least one @task; note that calls inside an `if` that was false at "
                "compile time are not traced."
            )

        tasks, edges = self._emit_graph(builder)
        spec: dict[str, Any] = {
            "api_version": API_VERSION,
            "namespace": self.namespace,
            "name": self.name,
            "tasks": tasks,
        }
        if edges:
            spec["edges"] = edges
        if self.description:
            spec["description"] = self.description
        if self.params:
            spec["params"] = {k: v.to_wire() for k, v in sorted(self.params.items())}
        if self.run_timeout is not None:
            from .units import parse_duration_ms

            spec["run_timeout_ms"] = parse_duration_ms(self.run_timeout)
        if self.fail_fast:
            spec["fail_fast"] = True
        if self.max_active_runs:
            spec["max_active_runs"] = self.max_active_runs
        if self.priority:
            spec["priority"] = self.priority
        if self.labels:
            spec["labels"] = dict(sorted(self.labels.items()))

        text = canonical_json(spec)
        source_map = {
            n.node_id: NodeSource(definition=n.definition.source, call_site=n.call_site)
            for n in builder.nodes
        }
        return CompiledWorkflow(spec=spec, json=text, hash=spec_hash(spec), source_map=source_map)

    def _invoke_body(self, builder: _Builder, overrides: Mapping[str, Any]) -> None:
        """Call the body with Param placeholders for each declared parameter."""
        try:
            sig = inspect.signature(self.fn)
        except (TypeError, ValueError):  # pragma: no cover
            self.fn()
            return

        args: list[Any] = []
        kwargs: dict[str, Any] = {}
        for i, p in enumerate(sig.parameters.values()):
            if i == 0 and p.name in ("ctx", "context"):
                # The workflow-level ctx is a compile-time object, not a
                # TaskContext: there is no run yet, so nothing runtime-shaped can
                # be offered here without lying about it.
                args.append(_WorkflowContext(self.name))
                continue
            if p.kind in (inspect.Parameter.VAR_POSITIONAL, inspect.Parameter.VAR_KEYWORD):
                continue
            if p.name in overrides:
                value: Any = overrides[p.name]
            elif p.name in self.params or p.default is inspect.Parameter.empty:
                value = Param(p.name)
            else:
                value = Param(p.name)
            if p.kind == inspect.Parameter.KEYWORD_ONLY:
                kwargs[p.name] = value
            else:
                args.append(value)
        self.fn(*args, **kwargs)

    def _emit_graph(self, builder: _Builder) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
        """Emit canonically-ordered tasks[] and edges[]."""
        known = {n.node_id for n in builder.nodes}
        tasks: list[dict[str, Any]] = []
        edge_map: dict[tuple[str, str], dict[str, str]] = {}

        for node in builder.nodes:
            inputs: dict[str, ValueSpec] = {}
            declared = node.definition.declaration.inputs or {}
            bound_names = set(node.literals) | set(node.params) | set(node.upstream)
            if node.map_spec is not None:
                bound_names.add(node.map_spec["item_input"])
            for name in sorted(bound_names):
                if name in declared:
                    inputs[name] = declared[name]
                    continue
                # Undeclared inputs still need a wire entry, or the server cannot
                # type-check the edge. `json` is the permissive fallback; authors
                # who want submit-time type checking declare inputs= explicitly.
                inputs[name] = ValueSpec(type="json", required=name not in node.literals)
            # A declared-but-unsupplied input with a default is still part of the
            # interface; emit it so the server can apply the default.
            for name, vspec in declared.items():
                inputs.setdefault(name, vspec)

            spec = node.definition.to_task_spec(node.node_id, inputs)
            if node.literals:
                # Literal bindings ride on the task spec rather than on an edge,
                # because there is no upstream node to put them on.
                spec["input_literals"] = {
                    k: canonical_json(v) for k, v in sorted(node.literals.items())
                }
            if node.params:
                spec["input_params"] = dict(sorted(node.params.items()))
            if node.map_spec is not None:
                spec["map"] = dict(sorted(node.map_spec.items()))
                src = node.map_spec["over_node_id"]
                if src not in known:
                    raise SpecError(
                        f"node {node.node_id!r} maps over {src!r}, which is not part of this workflow"
                    )
                # The collection's producer must be a DIRECT upstream in the graph,
                # not an implication of a string (the server validates the same).
                edge_map.setdefault((src, node.node_id), {})
            tasks.append(spec)

            for input_name, (from_node, output) in node.upstream.items():
                if from_node not in known:
                    raise SpecError(
                        f"node {node.node_id!r} consumes output of {from_node!r}, which "
                        "is not part of this workflow"
                    )
                edge_map.setdefault((from_node, node.node_id), {})[input_name] = output

        self._check_fan_in_bounds(builder, edge_map)
        tasks.sort(key=lambda t: t["id"])
        edges = [
            {
                "from_node_id": frm,
                "to_node_id": to,
                "output_to_input": dict(sorted(mapping.items())),
            }
            for (frm, to), mapping in sorted(edge_map.items())
        ]
        self._reject_cycles(tasks, edges)
        return tasks, edges

    @staticmethod
    def _check_fan_in_bounds(
        builder: _Builder, edge_map: Mapping[tuple[str, str], Mapping[str, str]]
    ) -> None:
        """Reject a fan-in that no platform configuration could ever assemble.

        When a map template declares ``max_bytes`` on an output, max_fanout
        children of that size either fit the fan-in limit or do not. The
        gateway checks against the limit its operator configured; the SDK
        cannot know that value, so it checks against the CEILING no
        configuration may exceed (MAX_FAN_IN_BYTES_CEILING) -- anything above
        it is certain to be refused, and saying so at the decorator is cheaper
        than a submit round trip.
        """
        by_id = {n.node_id: n for n in builder.nodes}
        for (frm, to), mapping in edge_map.items():
            src = by_id.get(frm)
            if src is None or src.map_spec is None:
                continue
            outputs = src.definition.declaration.outputs or {}
            fanout = int(src.map_spec["max_fanout"])
            for input_name, output in mapping.items():
                ob = outputs.get(output)
                if ob is None or not ob.max_bytes:
                    continue
                worst = fan_in_worst_case(fanout, ob.max_bytes)
                if worst > MAX_FAN_IN_BYTES_CEILING:
                    raise SpecError(
                        f"node {to!r} input {input_name!r}: the fan-in of {frm}.{output} can "
                        f"reach {worst} bytes (max_fanout {fanout} x max_bytes {ob.max_bytes}), "
                        f"over the platform ceiling of {MAX_FAN_IN_BYTES_CEILING} bytes; lower "
                        "max_fanout, declare a smaller max_bytes, or pass references"
                    )

    @staticmethod
    def _reject_cycles(tasks: Sequence[Mapping[str, Any]], edges: Sequence[Mapping[str, Any]]) -> None:
        """Fail on a cycle here, not at submit.

        The server also checks acyclicity, but a traced body can only produce a
        cycle through an explicit node_id reuse pattern, and catching it locally
        puts the error next to the code that caused it.
        """
        indegree = {t["id"]: 0 for t in tasks}
        adjacency: dict[str, list[str]] = {t["id"]: [] for t in tasks}
        for e in edges:
            adjacency[e["from_node_id"]].append(e["to_node_id"])
            indegree[e["to_node_id"]] += 1
        queue = [n for n, d in sorted(indegree.items()) if d == 0]
        visited = 0
        while queue:
            n = queue.pop()
            visited += 1
            for m in adjacency[n]:
                indegree[m] -= 1
                if indegree[m] == 0:
                    queue.append(m)
        if visited != len(tasks):
            stuck = sorted(n for n, d in indegree.items() if d > 0)
            raise SpecError(
                f"workflow graph contains a cycle involving: {', '.join(stuck)}"
            )

    def __repr__(self) -> str:
        return f"<workflow {self.namespace}/{self.name}>"


class _WorkflowContext:
    """The object passed as a workflow body's first parameter, if it takes one.

    Intentionally almost empty. There is no run at compile time, so anything
    run-shaped offered here would be a fiction, and a fiction that authors would
    reasonably build logic on.
    """

    __slots__ = ("workflow_name",)

    def __init__(self, workflow_name: str) -> None:
        self.workflow_name = workflow_name

    def param(self, name: str) -> Param:
        """Reference a run parameter not in the body's signature."""
        return Param(name)

    def __repr__(self) -> str:
        return f"<workflow-compile-context {self.workflow_name}>"


@overload
def workflow(_fn: Callable[..., object], /) -> WorkflowDefinition: ...


@overload
def workflow(
    _fn: None = None,
    /,
    *,
    name: str | None = None,
    namespace: str = "default",
    description: str | None = None,
    params: Mapping[str, ValueSpec | Mapping[str, Any] | str] | None = None,
    run_timeout: str | int | None = None,
    fail_fast: bool = False,
    max_active_runs: int = 0,
    priority: int = 0,
    labels: Mapping[str, str] | None = None,
) -> Callable[[Callable[..., object]], WorkflowDefinition]: ...


def workflow(
    _fn: Callable[..., object] | None = None,
    /,
    *,
    name: str | None = None,
    namespace: str = "default",
    description: str | None = None,
    params: Mapping[str, ValueSpec | Mapping[str, Any] | str] | None = None,
    run_timeout: str | int | None = None,
    fail_fast: bool = False,
    max_active_runs: int = 0,
    priority: int = 0,
    labels: Mapping[str, str] | None = None,
) -> WorkflowDefinition | Callable[[Callable[..., object]], WorkflowDefinition]:
    """Declare a workflow. `.compile()` emits the WorkflowSpec document.

    The decorated body is traced once, in your process, to discover the graph.
    It is never shipped to or executed by the control plane (ADR-0004), so the
    control plane needs none of your dependencies installed and cannot be
    destabilized by your code.

    The emitted document is canonical and deterministic: compiling an unchanged
    workflow twice produces byte-identical JSON and therefore the same content
    address, so resubmitting is a no-op instead of a new version.
    """
    options: dict[str, Any] = {
        "name": name,
        "namespace": namespace,
        "description": description,
        "params": params,
        "run_timeout": run_timeout,
        "fail_fast": fail_fast,
        "max_active_runs": max_active_runs,
        "priority": priority,
        "labels": labels,
    }

    def decorate(fn: Callable[..., object]) -> WorkflowDefinition:
        return WorkflowDefinition(fn, options)

    if _fn is not None:
        return decorate(_fn)
    return decorate
