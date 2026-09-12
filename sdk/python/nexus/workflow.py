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
from typing import Any, Callable, Iterable, Mapping, Sequence

from .canonical import canonical_bytes, canonical_json, spec_hash
from .types import SpecError, ValueSpec

__all__ = [
    "workflow",
    "WorkflowDefinition",
    "CompiledWorkflow",
    "OutputRef",
    "TaskInvocation",
    "Param",
    "current_builder",
]

API_VERSION = "nexus.dev/v1"

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
class OutputRef:
    """A handle to one named output of one traced node.

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

    def __iter__(self) -> Any:
        raise SpecError(
            f"cannot iterate {self!r}: the size of a task's output is a runtime "
            "property. Fan-out over a runtime collection needs dynamic expansion "
            "(architecture §5.4), which is deferred past Phase 1."
        )

    def __getattr__(self, item: str) -> Any:
        if item.startswith("_"):
            raise AttributeError(item)
        raise SpecError(
            f"cannot access .{item} on {self!r}: task outputs are references, not "
            "values, at compile time"
        )

    def __repr__(self) -> str:
        return f"OutputRef({self.node_id}.{self.output})"


class TaskInvocation:
    """The result of calling a task inside a workflow body.

    Behaves as its primary output (so `b(a())` reads naturally for single-output
    tasks) while also exposing every declared output by name via `["model"]`.
    """

    __slots__ = ("node_id", "_outputs")

    def __init__(self, node_id: str, outputs: Sequence[str]) -> None:
        self.node_id = node_id
        self._outputs = {name: OutputRef(node_id, name) for name in outputs}

    @property
    def primary(self) -> OutputRef:
        if len(self._outputs) == 1:
            return next(iter(self._outputs.values()))
        if "result" in self._outputs:
            return self._outputs["result"]
        raise SpecError(
            f"node {self.node_id!r} declares multiple outputs "
            f"({', '.join(sorted(self._outputs))}); select one explicitly, e.g. "
            f"node['{sorted(self._outputs)[0]}']"
        )

    def __getitem__(self, name: str) -> OutputRef:
        try:
            return self._outputs[name]
        except KeyError:
            raise SpecError(
                f"node {self.node_id!r} has no output {name!r}; declared outputs are "
                f"{', '.join(sorted(self._outputs)) or '(none)'}"
            ) from None

    def outputs(self) -> Mapping[str, OutputRef]:
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

    def __iter__(self) -> Any:
        raise SpecError(
            f"cannot iterate {self!r}: the size of a task's output is a runtime "
            "property. Fan-out over a runtime collection needs dynamic expansion "
            "(architecture §5.4), which is deferred past Phase 1."
        )

    def __repr__(self) -> str:
        return f"TaskInvocation({self.node_id}: {', '.join(sorted(self._outputs))})"


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
    definition: Any  # TaskDefinition; untyped here to avoid a circular import.
    literals: dict[str, Any]
    params: dict[str, str]
    upstream: dict[str, tuple[str, str]]  # input name -> (from_node, output)


class _Builder:
    """Accumulates nodes and edges while a workflow body is traced."""

    def __init__(self, workflow_name: str) -> None:
        self.workflow_name = workflow_name
        self.nodes: list[_Node] = []
        self._by_id: dict[str, _Node] = {}
        self._ref_counts: dict[str, int] = {}

    def _allocate_node_id(self, definition: Any) -> str:
        """Derive a stable node id from the handler name, suffixed on reuse.

        Stability matters beyond neatness: node_id participates in the
        idempotency key (§6.2), so a node id that shifted between compiles would
        change the key and defeat dedup across retries of the same logical work.
        The suffix is assigned by call order within the body, which is
        deterministic for a deterministic body.
        """
        base = getattr(definition, "__name__", None) or definition.task_ref
        base = str(base)
        n = self._ref_counts.get(base, 0)
        self._ref_counts[base] = n + 1
        node_id = base if n == 0 else f"{base}_{n + 1}"
        while node_id in self._by_id:  # an author-chosen id may already hold it
            n += 1
            self._ref_counts[base] = n + 1
            node_id = f"{base}_{n + 1}"
        return node_id

    def add_node(
        self, definition: Any, args: Sequence[Any], kwargs: Mapping[str, Any]
    ) -> TaskInvocation:
        node_id = kwargs.pop("node_id", None) if "node_id" in kwargs else None
        if node_id is None:
            node_id = self._allocate_node_id(definition)
        else:
            node_id = str(node_id)
            if node_id in self._by_id:
                raise SpecError(f"duplicate node_id {node_id!r} in workflow")

        bound = self._bind_arguments(definition, node_id, args, kwargs)

        node = _Node(node_id=node_id, definition=definition, literals={}, params={}, upstream={})
        for name, value in bound.items():
            self._classify_argument(node, name, value)

        self.nodes.append(node)
        self._by_id[node_id] = node
        return TaskInvocation(node_id, definition.declared_outputs)

    def _bind_arguments(
        self, definition: Any, node_id: str, args: Sequence[Any], kwargs: Mapping[str, Any]
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
            import yaml  # type: ignore[import-untyped]
        except ModuleNotFoundError as e:  # pragma: no cover - optional extra
            raise RuntimeError(
                "YAML output needs PyYAML: pip install 'nexus-sdk[yaml]'"
            ) from e
        return yaml.safe_dump(_plain(self.spec), sort_keys=True, default_flow_style=False)

    @property
    def task_ids(self) -> list[str]:
        return [t["id"] for t in self.spec["tasks"]]


def _plain(value: Any) -> Any:
    """Recursively convert to plain builtins so yaml.safe_dump accepts it."""
    if isinstance(value, Mapping):
        return {k: _plain(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [_plain(v) for v in value]
    return value


class WorkflowDefinition:
    """A decorated workflow. `compile()` produces the submittable document."""

    def __init__(self, fn: Callable[..., Any], options: Mapping[str, Any]) -> None:
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

    def __call__(self, *args: Any, **kwargs: Any) -> Any:
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
        return CompiledWorkflow(spec=spec, json=text, hash=spec_hash(spec))

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
            declared = getattr(node.definition, "_decl").inputs or {}
            for name in sorted(set(node.literals) | set(node.params) | set(node.upstream)):
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
            tasks.append(spec)

            for input_name, (from_node, output) in node.upstream.items():
                if from_node not in known:
                    raise SpecError(
                        f"node {node.node_id!r} consumes output of {from_node!r}, which "
                        "is not part of this workflow"
                    )
                edge_map.setdefault((from_node, node.node_id), {})[input_name] = output

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


def workflow(
    _fn: Callable[..., Any] | None = None,
    *,
    name: str | None = None,
    namespace: str = "default",
    description: str | None = None,
    params: Mapping[str, Any] | None = None,
    run_timeout: str | int | None = None,
    fail_fast: bool = False,
    max_active_runs: int = 0,
    priority: int = 0,
    labels: Mapping[str, str] | None = None,
) -> Any:
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

    def decorate(fn: Callable[..., Any]) -> WorkflowDefinition:
        return WorkflowDefinition(fn, options)

    if _fn is not None:
        return decorate(_fn)
    return decorate
