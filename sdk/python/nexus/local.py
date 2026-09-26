# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Run a compiled workflow on this machine, without a cluster (ADR-0022).

    from nexus import local
    result = local.run(daily_etl, {"source": "s3://in"})
    print(result.state, result.output("load"))

or ``nexusctl run --local dag.py --param source=s3://in``.

WHAT IS THE SAME AS THE PLATFORM. The decisions: which node runs next, trigger
rules, SKIPPED / UPSTREAM_FAILED propagation, fail_fast, map expansion
(max_fanout, the collection-must-be-inline rule, per-child element delivery),
fan-in assembly with its size limit, input resolution (edge > literal > param >
default), retry policy with backoff and jitter, start_to_close and
schedule_to_close, memoization keys, idempotency keys. They are computed by
``nexus._dag``, a port of internal/dag and internal/domain that shared
fixtures hold to the Go code. Task execution follows the agent's executor
contract: a shell task gets the same NEXUS_* environment, the same
inputs/ and outputs/ directories and the same output collection rules; a Python
handler runs through the same ``python -m nexus.worker`` entrypoint.

WHAT IS NOT (the parity limits, stated rather than papered over):
  * no leases, heartbeats or LOST attempts -- a local attempt is a child
    process this runner owns; ``timeouts.heartbeat`` is not enforced;
  * no scheduling: no queues, priorities, weighted fair share,
    ``max_active_runs``, ``concurrency_key`` limits or ``node_selector``;
    ready work runs as soon as a slot (``max_parallel``) is free;
  * no resources: requests and limits are neither reserved nor enforced, and
    GPU placement does not exist (CUDA_VISIBLE_DEVICES is not set);
  * executors: ``subprocess`` tasks and Python handlers only. ``docker`` and
    ``kubernetes`` tasks are refused unless ``host_executor=True``, which runs
    their command on the host -- a different environment from their image;
  * memoization is within ONE run (no cross-run cache, no invalidations);
  * no pause, resume, operator retry, schedules or events API;
  * results live in a FileResultStore under the run directory, not in an
    object store, and secrets referenced by the spec are not resolved;
  * tracing: there is no attempt span. A valid TRACEPARENT/TRACESTATE in this
    process's environment (e.g. from a CI job's trace) is handed to every task
    as-is, so handlers join that trace; an invalid one is not passed on.
  * no workload API (ADR-0032): ``ctx.report_usage`` accumulates on the
    context and is logged, ``ctx.acquire_permits`` uses a process-local bucket
    (unlimited unless NEXUS_LOCAL_RATE_LIMITS sets limits), and a
    ``rate_limited`` failure is retried like a transient one, charged to the
    retry budget and without honouring its retry_after. Checkpoints DO carry
    across retries: every attempt of a node shares the run's result store and
    the node's idempotency key, so ``ctx.resume()`` in attempt 2 returns what
    attempt 1 passed to ``ctx.checkpoint()``.
"""

from __future__ import annotations

import argparse
import concurrent.futures as cf
import copy
import dataclasses
import hashlib
import importlib.util
import inspect
import json
import os
import random
import signal
import subprocess
import sys
import threading
import time
import uuid
from typing import IO, Any, Callable, Mapping, Sequence

from . import _dag
from .context import (
    DEFAULT_INLINE_THRESHOLD_BYTES,
    FileResultStore,
    clamp_inline_threshold,
    decode_payload,
    inline_ref,
    ref_payload,
)
from .task import TaskDefinition, registry
from .tracing import TRACEPARENT_ENV, TRACESTATE_ENV, trace_context_from_environment
from .workflow import CompiledWorkflow, WorkflowDefinition

__all__ = ["run", "LocalRunResult", "NodeResult", "AttemptRecord", "LocalRunError", "main"]

#: agent/internal/executor.MaxStdoutOutputBytes
MAX_STDOUT_OUTPUT_BYTES = 32 << 20
#: agent/internal/executor.DefaultGraceAfterTerm
GRACE_AFTER_TERM_S = 10.0
#: agent/internal/executor.MaxLogTailBytes
MAX_LOG_TAIL_BYTES = 4096
#: internal/dag DefaultCacheTTLMs
DEFAULT_CACHE_TTL_MS = 7 * 24 * 60 * 60 * 1000

_TERMINAL_RUN = frozenset({_dag.RUN_SUCCEEDED, _dag.RUN_FAILED, _dag.RUN_CANCELLED, _dag.RUN_TIMED_OUT})


class LocalRunError(Exception):
    """The workflow cannot be run locally at all (a preflight refusal)."""


# ── results ───────────────────────────────────────────────────────────────────


@dataclasses.dataclass
class AttemptRecord:
    """One execution (or memo hit) of one node."""

    attempt_id: str
    node_id: str
    number: int
    state: str  # SUCCEEDED | FAILED | TIMED_OUT | CANCELLED
    started_at: float
    ended_at: float
    error_class: str = ""
    error_type: str = ""
    message: str = ""
    exit_code: int | None = None
    cache_hit_of: str = ""
    work_dir: str = ""
    retry_decision: str = ""

    @property
    def duration_s(self) -> float:
        return max(0.0, self.ended_at - self.started_at)


@dataclasses.dataclass
class NodeResult:
    node_id: str
    task_ref: str
    state: str
    attempts: list[AttemptRecord] = dataclasses.field(default_factory=list)
    reason: str = ""
    parent: str = ""
    map_index: int | None = None
    outputs: dict[str, dict[str, Any]] = dataclasses.field(default_factory=dict)


@dataclasses.dataclass
class LocalRunResult:
    """The outcome of a local run. ``state`` uses the platform's run states."""

    run_id: str
    workflow: str
    state: str
    nodes: dict[str, NodeResult]
    run_dir: str
    started_at: float
    ended_at: float
    store: FileResultStore
    #: Node ids in the order the summary lists them: topological, with each
    #: template's children right after it.
    order: list[str] = dataclasses.field(default_factory=list)

    @property
    def succeeded(self) -> bool:
        return self.state == _dag.RUN_SUCCEEDED

    def output(self, node_id: str, name: str = "result") -> Any:
        """A node's output value: JSON decoded, other payloads as str/bytes."""
        node = self.nodes[node_id]
        if name not in node.outputs:
            raise KeyError(
                f"node {node_id!r} ({node.state}) has no output {name!r}; "
                f"outputs: {', '.join(sorted(node.outputs)) or '(none)'}"
            )
        return self.store.get(node.outputs[name])

    def output_bytes(self, node_id: str, name: str = "result") -> bytes:
        return self.store.get_bytes(self.nodes[node_id].outputs[name])

    def to_json(self) -> dict[str, Any]:
        return {
            "run_id": self.run_id,
            "workflow": self.workflow,
            "state": self.state,
            "run_dir": self.run_dir,
            "duration_s": round(self.ended_at - self.started_at, 3),
            "nodes": [
                {
                    "node_id": n.node_id,
                    "task_ref": n.task_ref,
                    "state": n.state,
                    "reason": n.reason,
                    "parent_node_id": n.parent,
                    "map_index": n.map_index,
                    "outputs": {k: _ref_summary(v) for k, v in sorted(n.outputs.items())},
                    "attempts": [
                        {
                            "attempt_id": a.attempt_id,
                            "number": a.number,
                            "state": a.state,
                            "duration_s": round(a.duration_s, 3),
                            "error_class": a.error_class,
                            "error_type": a.error_type,
                            "message": a.message,
                            "exit_code": a.exit_code,
                            "cache_hit_of": a.cache_hit_of,
                            "work_dir": a.work_dir,
                            "retry_decision": a.retry_decision,
                        }
                        for a in n.attempts
                    ],
                }
                for n in _ordered_nodes(self.nodes, self.order)
            ],
        }

    def summary(self) -> str:
        """A per-node table plus the verdict, for a terminal."""
        rows = [("NODE", "STATE", "ATTEMPTS", "TIME", "DETAIL")]
        for n in _ordered_nodes(self.nodes, self.order):
            last = n.attempts[-1] if n.attempts else None
            detail = n.reason
            if last is not None and last.cache_hit_of:
                detail = f"memoized (reused {last.cache_hit_of[:8]})"
            elif last is not None and last.state != "SUCCEEDED" and not detail:
                detail = f"{_short_class(last.error_class)}: {_headline(last.message) or last.error_type}"
                if last.retry_decision:
                    detail += f" [{last.retry_decision}]"
            took = sum(a.duration_s for a in n.attempts)
            rows.append((
                n.node_id,
                n.state,
                str(len(n.attempts)) if n.attempts else "-",
                f"{took:.2f}s" if n.attempts else "-",
                _clip(detail, 90),
            ))
        widths = [max(len(r[i]) for r in rows) for i in range(4)]
        lines = [
            "  ".join(r[i].ljust(widths[i]) for i in range(4)) + "  " + r[4]
            for r in rows
        ]
        took = self.ended_at - self.started_at
        lines.append("")
        lines.append(f"run {self.run_id} ({self.workflow}): {self.state} in {took:.2f}s")
        lines.append(f"run directory: {self.run_dir}")
        return "\n".join(line.rstrip() for line in lines)


def _ordered_nodes(nodes: Mapping[str, NodeResult], order: Sequence[str] = ()) -> list[NodeResult]:
    rank = {nid: i for i, nid in enumerate(n for n in order if not nodes[n].parent)} if order else {}

    def key(n: NodeResult) -> tuple[int, str, int]:
        top = n.parent or n.node_id
        return (rank.get(top, len(rank)), top, -1 if n.map_index is None else n.map_index)

    return sorted(nodes.values(), key=key)


def _ref_summary(ref: Mapping[str, Any]) -> dict[str, Any]:
    out: dict[str, Any] = {"size_bytes": ref.get("size_bytes"), "content_type": ref.get("content_type")}
    out["stored"] = "blob" if "blob" in ref else "inline"
    return out


def _headline(message: str) -> str:
    """First line of a failure, plus the last line of its log tail -- usually
    the one line that says what went wrong ("exit code 3: bad input")."""
    lines = message.splitlines()
    if not lines:
        return ""
    head = lines[0]
    if "--- log tail ---" in lines:
        tail = [ln.strip() for ln in lines[lines.index("--- log tail ---") + 1:] if ln.strip()]
        if tail:
            head += f": {tail[-1]}"
    return head


def _short_class(c: str) -> str:
    return c.removeprefix("ERROR_CLASS_").lower() if c else "error"


def _clip(s: str, n: int) -> str:
    s = " ".join(s.split())
    return s if len(s) <= n else s[: n - 3] + "..."


# ── the runner ────────────────────────────────────────────────────────────────


@dataclasses.dataclass
class _Attempt:
    node_id: str
    number: int
    attempt_id: str
    inputs: dict[str, dict[str, Any]]
    work_dir: str


@dataclasses.dataclass
class _Outcome:
    state: str  # SUCCEEDED | FAILED | TIMED_OUT | CANCELLED
    outputs: dict[str, dict[str, Any]] = dataclasses.field(default_factory=dict)
    error_class: str = ""
    error_type: str = ""
    message: str = ""
    exit_code: int | None = None


class _Runner:
    def __init__(
        self,
        spec: Mapping[str, Any],
        params: Mapping[str, Any],
        *,
        run_id: str,
        run_dir: str,
        modules: Sequence[str],
        python: str,
        isolation: str,
        max_parallel: int,
        backoff_scale: float,
        fan_in_limit: int,
        max_fanout_ceiling: int,
        inline_threshold: int,
        host_executor: bool,
        seed: int | None,
        log: Callable[[str], None],
    ) -> None:
        self.spec = copy.deepcopy(dict(spec))
        self.params = dict(params)
        self.run_id = run_id
        self.run_dir = run_dir
        self.namespace = str(self.spec.get("namespace") or "default")
        self.modules = list(modules)
        self.python = python
        self.isolation = isolation
        self.max_parallel = max(1, max_parallel)
        self.backoff_scale = max(0.0, backoff_scale)
        self.fan_in_limit = fan_in_limit
        self.max_fanout_ceiling = max_fanout_ceiling
        self.inline_threshold = clamp_inline_threshold(inline_threshold)
        self.host_executor = host_executor
        #: Replay pins the control plane's key instead of deriving one.
        self.fixed_idempotency_key: str | None = None
        self.rng = random.Random(seed)
        self.log = log
        self.store = FileResultStore(os.path.join(run_dir, "results"), inline_max_bytes=self.inline_threshold)
        self.children: dict[str, tuple[str, int]] = {}
        self.graph = _dag.Graph(self.spec)
        self.states: dict[str, str] = {n: _dag.PENDING for n in self.graph.nodes}
        self.results: dict[str, NodeResult] = {
            n: NodeResult(node_id=n, task_ref=str(node.spec.get("task_ref", "")), state=_dag.PENDING)
            for n, node in self.graph.nodes.items()
        }
        self.first_scheduled: dict[str, float] = {}
        self.memo: dict[str, tuple[str, dict[str, dict[str, Any]], float]] = {}
        self.memo_keys: dict[str, str] = {}
        self.cancel = threading.Event()
        self.events = open(os.path.join(run_dir, "events.jsonl"), "a", encoding="utf-8")
        self._lock = threading.Lock()

    # ── bookkeeping ─────────────────────────────────────────────────────────
    def event(self, kind: str, node: str = "", **fields: Any) -> None:
        rec = {"ts": round(time.time(), 3), "kind": kind, "node_id": node, **fields}
        with self._lock:
            self.events.write(json.dumps(rec, sort_keys=True, default=str) + "\n")
            self.events.flush()

    def set_state(self, node: str, state: str, reason: str = "") -> None:
        if self.states.get(node) == state:
            return
        self.states[node] = state
        self.results[node].state = state
        if reason and not self.results[node].reason:
            self.results[node].reason = reason
        self.event("node_state", node, state=state, reason=reason)

    # ── preflight ───────────────────────────────────────────────────────────
    def preflight(self) -> None:
        problems: list[str] = []
        for nid, node in self.graph.nodes.items():
            rt = node.spec.get("runtime") or {}
            executor = rt.get("executor") or "subprocess"
            if rt.get("command"):
                if executor != "subprocess" and not self.host_executor:
                    problems.append(
                        f"task {nid!r} uses the {executor} executor, which the local runner does not "
                        "provide; pass --host-executor to run its command on this host instead of "
                        "in its image"
                    )
                continue
            if executor != "subprocess" and not self.host_executor:
                problems.append(f"task {nid!r} uses the {executor} executor with no command")
                continue
            ref = str(node.spec.get("task_ref", ""))
            if registry.get(ref) is None:
                problems.append(
                    f"task {nid!r} has no runtime.command and no Python handler is registered for "
                    f"task_ref {ref!r}; import the module that defines it (--module)"
                )
        for nid in self.graph.order:
            try:
                _dag.input_plan(self.graph, nid)
            except ValueError as e:
                # The gateway refuses such a spec at submit; say so before running.
                problems.append(str(e).removeprefix("input plan: "))
        if problems:
            raise LocalRunError("cannot run this workflow locally:\n  - " + "\n  - ".join(problems))
        declared = self.graph.params
        for name in sorted(self.params):
            if name not in declared:
                self.log(f"warning: parameter {name!r} is not declared by the workflow")
        for name, vs in sorted(declared.items()):
            if (vs or {}).get("required") and name not in self.params and not (vs or {}).get("default_json"):
                self.log(f"warning: required parameter {name!r} was not given")

    # ── graph growth (expansion) ────────────────────────────────────────────
    def rebuild(self) -> None:
        self.graph = _dag.Graph(self.spec, self.children)

    def expand(self, template: str) -> None:
        node = self.graph.nodes[template]
        m = node.spec["map"]
        max_fanout = int(m.get("max_fanout") or 0)
        src, src_out = str(m.get("over_node_id", "")), str(m.get("over_output", ""))
        try:
            if max_fanout > self.max_fanout_ceiling:
                raise _dag.ExpansionFailure(
                    "max_fanout_exceeded",
                    f"max_fanout {max_fanout} exceeds the platform ceiling {self.max_fanout_ceiling}",
                )
            payload: bytes | None
            inline = True
            src_node = self.graph.nodes.get(src)
            if src_node is not None and src_node.is_template:
                payload = self.fan_in(src, src_out)
            else:
                ref = self.results[src].outputs.get(src_out) if src in self.results else None
                payload = None if ref is None else ref_payload(ref)
                inline = ref is None or payload is not None
            items = _dag.plan_from_collection(template, payload, inline, max_fanout)
        except _dag.FanInTooLarge as e:
            self.set_state(template, _dag.FAILED, f"{e.reason}: {e}")
            self.event("expansion_failed", template, reason=e.reason)
            return
        except _dag.ExpansionFailure as e:
            self.set_state(template, _dag.FAILED, f"{e.reason}: {e.detail}")
            self.event("expansion_failed", template, reason=e.reason, detail=e.detail)
            return
        item_input = str(m.get("item_input", ""))
        for i, item in enumerate(items):
            cid = _dag.child_id(template, i)
            cspec = copy.deepcopy(node.spec)
            cspec["id"] = cid
            cspec.pop("map", None)
            lits = dict(cspec.get("input_literals") or {})
            lits[item_input] = item.decode("utf-8")
            cspec["input_literals"] = lits
            self.spec["tasks"].append(cspec)
            for edge in self.graph.edges_to.get(template, []):
                self.spec.setdefault("edges", []).append(
                    {"from_node_id": edge.frm, "to_node_id": cid, "output_to_input": dict(edge.output_to_input)}
                )
            self.children[cid] = (template, i)
            self.states[cid] = _dag.PENDING
            self.results[cid] = NodeResult(
                node_id=cid, task_ref=str(cspec.get("task_ref", "")), state=_dag.PENDING,
                parent=template, map_index=i,
            )
        self.rebuild()
        self.set_state(template, _dag.RUNNING)
        self.results[template].reason = f"expanded into {len(items)} children (max_fanout {max_fanout})"
        self.event("node_expanded", template, children=len(items))

    def fan_in(self, template: str, output: str) -> bytes:
        kids = self.graph.children.get(template, [])
        b = _dag.FanInBuilder(template, output, len(kids), self.fan_in_limit)
        for k in kids:
            res = self.results[k]
            ref = res.outputs.get(output) if res.state == _dag.SUCCEEDED else None
            if ref is None:
                b.add(b"null")
                continue
            payload = ref_payload(ref)
            b.add(_dag.fan_in_element(payload, payload is not None, _wire_ref(ref)))
        return b.finish()

    # ── input resolution (engine.go resolveInputs + resolveRunInputs) ───────
    def resolve_inputs(self, nid: str) -> dict[str, dict[str, Any]]:
        g = self.graph
        node = g.nodes[nid]
        literals: Mapping[str, str] = node.spec.get("input_literals") or {}
        input_params: Mapping[str, str] = node.spec.get("input_params") or {}
        refs: dict[str, dict[str, Any]] = {}
        for b in _dag.input_plan(g, nid):
            if b.source == "edge":
                up = g.nodes.get(b.upstream_node)
                if up is not None and up.is_template:
                    payload = self.fan_in(b.upstream_node, b.upstream_output)  # may raise FanInTooLarge
                    refs[b.input] = inline_ref(payload, "application/json")
                    continue
                ref = self.results[b.upstream_node].outputs.get(b.upstream_output)
                if self.states.get(b.upstream_node) == _dag.SUCCEEDED and ref is not None:
                    refs[b.input] = ref
                continue
            value: str | None
            if b.input in literals:
                value = str(literals[b.input])
            elif b.source == "map_item":
                raise LocalRunError(f"map item input {b.input!r} of {nid} has no element")
            else:
                pname = input_params.get(b.input) or b.input
                value = None
                if pname in self.params:
                    value = json.dumps(self.params[pname], sort_keys=True, separators=(",", ":"))
                if value is None:
                    pdecl = g.params.get(pname) or {}
                    value = pdecl.get("default_json") or None
                if value is None:
                    value = b.default_json or None
            if value is None:
                continue
            try:
                json.loads(value)
            except ValueError:
                raise LocalRunError(f"node {nid}: invalid JSON for input {b.input!r}") from None
            refs[b.input] = inline_ref(value.encode("utf-8"), "application/json")
        return refs

    def inputs_digest(self, refs: Mapping[str, Mapping[str, Any]]) -> bytes:
        entries: dict[str, tuple[str, str]] = {}
        for name, ref in refs.items():
            payload = ref_payload(ref)
            if payload is None:
                sha = str(ref["blob"].get("sha256") or "")
            else:
                sha = hashlib.sha256(payload).hexdigest()
            entries[name] = (sha, str(ref.get("content_type") or ""))
        return _dag.inputs_digest(entries)

    # ── attempts ─────────────────────────────────────────────────────────────
    def new_attempt(self, nid: str, inputs: dict[str, dict[str, Any]]) -> _Attempt:
        number = len(self.results[nid].attempts) + 1
        attempt_id = str(uuid.uuid4())
        safe = "".join(c if c.isalnum() or c in "-_" else "_" for c in nid)
        work = os.path.join(self.run_dir, "attempts", f"{safe}.{number}")
        os.makedirs(os.path.join(work, "inputs"), exist_ok=True)
        os.makedirs(os.path.join(work, "outputs"), exist_ok=True)
        return _Attempt(nid, number, attempt_id, inputs, work)

    def idem_key(self, nid: str) -> str:
        if self.fixed_idempotency_key:
            return self.fixed_idempotency_key
        n = self.graph.nodes[nid]
        version = int(n.spec.get("version") or 1)
        if n.is_child:
            return _dag.idempotency_key(self.run_id, n.parent, n.map_index, version)
        return _dag.idempotency_key(self.run_id, nid, None, version)

    def execute(self, a: _Attempt) -> _Outcome:
        """Runs on a pool thread. Never raises: a failure is an outcome."""
        try:
            spec = self.graph.nodes[a.node_id].spec
            if (spec.get("runtime") or {}).get("command"):
                return self._run_shell(a, spec)
            return self._run_python(a, spec)
        except Exception as e:  # an executor bug must not wedge the run
            return _Outcome("FAILED", error_class="ERROR_CLASS_INFRASTRUCTURE",
                            error_type=type(e).__name__, message=f"local executor failed: {e}")

    def _deadline_s(self, spec: Mapping[str, Any]) -> float | None:
        ms = (spec.get("timeouts") or {}).get("start_to_close_ms")
        return float(ms) / 1000.0 if ms else None

    def _base_env(self, a: _Attempt, spec: Mapping[str, Any]) -> dict[str, str]:
        env = dict(os.environ)
        rt = spec.get("runtime") or {}
        for k, v in (rt.get("env") or {}).items():
            env[str(k)] = str(v)
        outputs = sorted((spec.get("outputs") or {}).keys())
        out_file = (
            os.path.join(a.work_dir, "outputs", _safe(outputs[0]))
            if len(outputs) == 1
            else os.path.join(a.work_dir, "output.json")
        )
        env.update({
            "NEXUS_ATTEMPT_ID": a.attempt_id,
            "NEXUS_RUN_ID": self.run_id,
            "NEXUS_NODE_ID": a.node_id,
            "NEXUS_IDEMPOTENCY_KEY": self.idem_key(a.node_id),
            "NEXUS_WORK_DIR": a.work_dir,
            "NEXUS_INPUT_DIR": os.path.join(a.work_dir, "inputs"),
            "NEXUS_OUTPUT_FILE": out_file,
            "NEXUS_OUTPUT_DIR": os.path.join(a.work_dir, "outputs"),
            "NEXUS_OUTPUT_NAMES": " ".join(outputs),
            "NEXUS_INLINE_THRESHOLD_BYTES": str(self.inline_threshold),
            "NEXUS_LOCAL_RUN": "1",
        })
        # The agent exports the attempt's W3C context as TRACEPARENT/TRACESTATE.
        # Locally there is no attempt span, so the runner's own context (when
        # valid) stands in for it; a malformed or task-set value is not passed on.
        env.pop(TRACEPARENT_ENV, None)
        env.pop(TRACESTATE_ENV, None)
        trace = trace_context_from_environment(os.environ)
        if trace is not None:
            env[TRACEPARENT_ENV] = trace.traceparent
            if trace.tracestate:
                env[TRACESTATE_ENV] = trace.tracestate
        # A local run has no workload API. If this runner itself runs inside a
        # Nexus task, its children must not act on that task's attempt.
        env.pop("NEXUS_TASK_API", None)
        env.pop("NEXUS_TASK_API_TOKEN", None)
        deadline = self._deadline_s(spec)
        if deadline:
            env["NEXUS_DEADLINE_UNIX_MS"] = str(int((time.time() + deadline) * 1000))
        return env

    def _wait(self, proc: subprocess.Popen[bytes], deadline: float | None) -> str:
        """Wait for the process; SIGTERM then SIGKILL its GROUP on timeout or
        run cancellation. Returns "exited", "timed_out" or "cancelled"."""
        end = None if deadline is None else time.monotonic() + deadline
        while True:
            try:
                proc.wait(timeout=0.1)
                return "exited"
            except subprocess.TimeoutExpired:
                pass
            why = ""
            if self.cancel.is_set():
                why = "cancelled"
            elif end is not None and time.monotonic() >= end:
                why = "timed_out"
            if why:
                _kill_group(proc, signal.SIGTERM)
                try:
                    proc.wait(timeout=GRACE_AFTER_TERM_S)
                except subprocess.TimeoutExpired:
                    _kill_group(proc, signal.SIGKILL)
                    proc.wait()
                return why

    def _run_shell(self, a: _Attempt, spec: Mapping[str, Any]) -> _Outcome:
        rt = spec.get("runtime") or {}
        for name, ref in a.inputs.items():
            with open(os.path.join(a.work_dir, "inputs", _safe(name)), "wb") as f:
                f.write(self.store.get_bytes(ref))
        argv = [str(x) for x in (rt.get("command") or [])] + [str(x) for x in (rt.get("args") or [])]
        cwd = str(rt.get("working_dir") or a.work_dir)
        stdout_path = os.path.join(a.work_dir, "stdout.log")
        stderr_path = os.path.join(a.work_dir, "stderr.log")
        started = time.monotonic()
        with open(stdout_path, "wb") as so, open(stderr_path, "wb") as se:
            try:
                proc = subprocess.Popen(
                    argv, cwd=cwd, env=self._base_env(a, spec), stdout=so, stderr=se,
                    stdin=subprocess.DEVNULL, start_new_session=True,
                )
            except FileNotFoundError as e:
                return _Outcome("FAILED", error_class="ERROR_CLASS_NOT_FOUND", error_type="NotFound",
                                message=str(e))
            except PermissionError as e:
                return _Outcome("FAILED", error_class="ERROR_CLASS_PERMISSION_DENIED",
                                error_type="PermissionDenied", message=str(e))
            how = self._wait(proc, self._deadline_s(spec))
        wall_ms = int((time.monotonic() - started) * 1000)
        tail = _log_tail(stderr_path) or _log_tail(stdout_path)
        if how == "cancelled":
            return _Outcome("CANCELLED", error_class="ERROR_CLASS_TRANSIENT", error_type="Cancelled",
                            message="the run was cancelled")
        if how == "timed_out":
            return _Outcome("TIMED_OUT", error_class="ERROR_CLASS_TIMEOUT", error_type="DeadlineExceeded",
                            message=f"exceeded start_to_close deadline after {wall_ms}ms", exit_code=proc.returncode)
        if proc.returncode != 0:
            msg = f"exit code {proc.returncode}"
            if proc.returncode < 0:
                msg = f"workload terminated by signal {-proc.returncode}"
            if tail:
                msg += "\n--- log tail ---\n" + tail
            return _Outcome("FAILED", error_class="ERROR_CLASS_APPLICATION", error_type="TaskFailed",
                            message=msg, exit_code=proc.returncode)
        return _Outcome("SUCCEEDED", outputs=self._collect(a, spec, stdout_path), exit_code=0)

    def _collect(self, a: _Attempt, spec: Mapping[str, Any], stdout_path: str) -> dict[str, dict[str, Any]]:
        """CollectOutputs + the runner's legacy output.json -> "result" rule."""
        declared = sorted((spec.get("outputs") or {}).keys())
        produced: dict[str, bytes] = {}
        for name in declared:
            path = os.path.join(a.work_dir, "outputs", _safe(name))
            if os.path.isfile(path):
                with open(path, "rb") as f:
                    produced[name] = f.read()
        so_name = str((spec.get("runtime") or {}).get("stdout_output") or "")
        if so_name and so_name not in produced:
            with open(stdout_path, "rb") as f:
                data = f.read(MAX_STDOUT_OUTPUT_BYTES + 1)
            if len(data) > MAX_STDOUT_OUTPUT_BYTES:
                self.log(f"warning: {a.node_id}: stdout output truncated at {MAX_STDOUT_OUTPUT_BYTES} bytes")
                data = data[:MAX_STDOUT_OUTPUT_BYTES]
            produced[so_name] = data
        if not produced:
            legacy = os.path.join(a.work_dir, "output.json")
            if os.path.isfile(legacy):
                with open(legacy, "rb") as f:
                    produced["result"] = f.read()
        return {name: self.store.put_bytes(data) for name, data in produced.items()}

    def _assignment(self, a: _Attempt, spec: Mapping[str, Any]) -> dict[str, Any]:
        n = self.graph.nodes[a.node_id]
        deadline = self._deadline_s(spec)
        return {
            "attempt_id": a.attempt_id,
            "run_id": self.run_id,
            "node_id": a.node_id,
            "map_index": n.map_index if n.is_child else None,
            "attempt_number": a.number,
            "namespace": self.namespace,
            "idempotency_key": self.idem_key(a.node_id),
            "spec": dict(spec),
            "inputs": a.inputs,
            "start_to_close_deadline_unix_ms": int((time.time() + deadline) * 1000) if deadline else 0,
            "result_store": {"inline_threshold_bytes": self.inline_threshold},
        }

    def _run_python(self, a: _Attempt, spec: Mapping[str, Any]) -> _Outcome:
        from .worker import execute_assignment

        assignment = self._assignment(a, spec)
        with open(os.path.join(a.work_dir, "assignment.json"), "w", encoding="utf-8") as f:
            json.dump(assignment, f, sort_keys=True, indent=2)
        if self.isolation == "inprocess":
            outcome = execute_assignment(assignment, store=self.store, task_api=None)
            return self._python_outcome(a, spec, outcome)
        outcome_path = os.path.join(a.work_dir, "outcome.json")
        argv = [self.python, "-m", "nexus.worker", "--assignment", os.path.join(a.work_dir, "assignment.json"),
                "--result-dir", self.store.root, "--output", outcome_path]
        for m in self.modules:
            argv += ["--module", m]
        env = self._base_env(a, spec)
        env["NEXUS_RESULT_DIR"] = self.store.root
        env["PYTHONPATH"] = os.pathsep.join(
            [p for p in _module_dirs(self.modules)] + [p for p in sys.path if p and os.path.isdir(p)]
        )
        with open(os.path.join(a.work_dir, "stdout.log"), "wb") as so, \
                open(os.path.join(a.work_dir, "stderr.log"), "wb") as se:
            proc = subprocess.Popen(argv, cwd=a.work_dir, env=env, stdout=so, stderr=se,
                                    stdin=subprocess.DEVNULL, start_new_session=True)
            how = self._wait(proc, self._deadline_s(spec))
        if how == "cancelled":
            return _Outcome("CANCELLED", error_class="ERROR_CLASS_TRANSIENT", error_type="Cancelled",
                            message="the run was cancelled")
        if how == "timed_out":
            return _Outcome("TIMED_OUT", error_class="ERROR_CLASS_TIMEOUT", error_type="DeadlineExceeded",
                            message="exceeded start_to_close deadline")
        try:
            with open(outcome_path, encoding="utf-8") as f:
                outcome = json.load(f)
        except (OSError, ValueError):
            tail = _log_tail(os.path.join(a.work_dir, "stderr.log"))
            return _Outcome("FAILED", error_class="ERROR_CLASS_INFRASTRUCTURE", error_type="NoOutcome",
                            message=f"python -m nexus.worker exited {proc.returncode} without an outcome"
                            + (f"\n--- log tail ---\n{tail}" if tail else ""),
                            exit_code=proc.returncode)
        return self._python_outcome(a, spec, outcome)

    def _python_outcome(self, a: _Attempt, spec: Mapping[str, Any], outcome: Mapping[str, Any]) -> _Outcome:
        if outcome.get("outcome") != "ATTEMPT_OUTCOME_SUCCEEDED":
            err = outcome.get("error") or {}
            return _Outcome("FAILED", error_class=str(err.get("class") or "ERROR_CLASS_APPLICATION"),
                            error_type=str(err.get("type") or ""), message=str(err.get("message") or ""))
        ref = outcome.get("output_ref")
        declared = sorted((spec.get("outputs") or {}).keys()) or ["result"]
        if ref is None:
            return _Outcome("SUCCEEDED")
        if len(declared) == 1:
            return _Outcome("SUCCEEDED", outputs={declared[0]: dict(ref)})
        # Several declared outputs: the handler returns a mapping keyed by them.
        value = self.store.get(ref)
        if not isinstance(value, Mapping):
            return _Outcome("FAILED", error_class="ERROR_CLASS_VALIDATION", error_type="OutputShape",
                            message=f"task declares outputs {', '.join(declared)} but the handler returned "
                            f"{type(value).__name__}; return a mapping keyed by output name")
        outs = {
            name: self.store.put_bytes(
                json.dumps(value[name], sort_keys=True, separators=(",", ":")).encode("utf-8"),
                "application/json",
            )
            for name in declared
            if name in value
        }
        return _Outcome("SUCCEEDED", outputs=outs)

    # ── the loop ────────────────────────────────────────────────────────────
    def run(self, run_timeout_s: float | None) -> str:
        try:
            self.preflight()
        except LocalRunError:
            self.events.close()
            raise
        started = time.monotonic()
        running: dict[cf.Future[_Outcome], _Attempt] = {}
        attempt_started: dict[str, float] = {}
        retry_at: dict[str, float] = {}  # node -> monotonic time its next attempt may start
        queued: list[str] = []  # nodes whose next attempt is due
        run_state = _dag.RUN_RUNNING
        timed_out = False
        pool = cf.ThreadPoolExecutor(max_workers=self.max_parallel, thread_name_prefix="nexus-local")
        try:
            while True:
                if run_timeout_s is not None and not timed_out and time.monotonic() - started > run_timeout_s:
                    timed_out = True
                    self.cancel.set()
                    self.log(f"run_timeout of {run_timeout_s:.0f}s exceeded; cancelling")
                    queued.clear()
                    retry_at.clear()
                decision = _dag.reconcile(
                    self.graph, self.states, "CANCELLING" if self.cancel.is_set() else _dag.RUN_RUNNING
                )
                for t in decision.transitions:
                    if t.to == _dag.READY:
                        continue  # READY is set below, when the attempt is created
                    reason = t.reason if not t.cause else f"{t.reason} (cause: {t.cause})"
                    if t.to in (_dag.SKIPPED, _dag.UPSTREAM_FAILED, _dag.CANCELLED):
                        self.set_state(t.node_id, t.to, reason)
                    else:
                        self.set_state(t.node_id, t.to)
                run_state = decision.run_state
                changed = False
                for tmpl in decision.expansions:
                    self.expand(tmpl)
                    changed = True
                for nid in decision.frontier:
                    changed = True
                    self.first_scheduled.setdefault(nid, time.monotonic())
                    if self.try_memo_or_refuse(nid):
                        continue
                    self.set_state(nid, _dag.READY)
                    queued.append(nid)
                if changed:
                    continue
                now = time.monotonic()
                for nid, at in list(retry_at.items()):
                    if at <= now:
                        del retry_at[nid]
                        queued.append(nid)
                while queued and len(running) < self.max_parallel and not self.cancel.is_set():
                    nid = queued.pop(0)
                    try:
                        inputs = self.resolve_inputs(nid)
                    except _dag.FanInTooLarge as e:
                        self.set_state(nid, _dag.FAILED, str(e))
                        continue
                    a = self.new_attempt(nid, inputs)
                    self.set_state(nid, _dag.RUNNING)
                    attempt_started[a.attempt_id] = time.time()
                    self.event("attempt_started", nid, attempt_id=a.attempt_id, number=a.number)
                    running[pool.submit(self.execute, a)] = a
                if not running and not retry_at and not queued:
                    if run_state in _TERMINAL_RUN:
                        break
                    if self.cancel.is_set():
                        break
                    raise LocalRunError(
                        "local run stalled with nothing runnable; node states: "
                        + ", ".join(f"{k}={v}" for k, v in sorted(self.states.items()))
                    )
                timeout = None
                if retry_at:
                    timeout = max(0.0, min(retry_at.values()) - time.monotonic())
                if run_timeout_s is not None and not timed_out:
                    left = max(0.0, run_timeout_s - (time.monotonic() - started))
                    timeout = left if timeout is None else min(timeout, left)
                if not running:
                    time.sleep(min(timeout or 0.0, 1.0))
                    continue
                done, _ = cf.wait(list(running), timeout=timeout, return_when=cf.FIRST_COMPLETED)
                for fut in done:
                    a = running.pop(fut)
                    self.finish(a, fut.result(), attempt_started.pop(a.attempt_id), retry_at)
        finally:
            self.cancel.set()
            pool.shutdown(wait=True)
            self.events.close()
        if timed_out:
            return _dag.RUN_TIMED_OUT
        return run_state

    def try_memo_or_refuse(self, nid: str) -> bool:
        """A frontier node satisfied from the memo cache, or failed by an
        oversized fan-in before any attempt exists. True when settled."""
        node = self.graph.nodes[nid]
        try:
            inputs = self.resolve_inputs(nid)
        except _dag.FanInTooLarge as e:
            self.set_state(nid, _dag.FAILED, str(e))
            return True
        cache = node.spec.get("cache") or {}
        if not cache.get("enabled") or node.spec.get("idempotency") == "IDEMPOTENCY_MODE_NONE":
            return False
        key = _dag.memo_key(self.namespace, str(node.spec.get("task_ref", "")),
                            int(node.spec.get("version") or 1), self.inputs_digest(inputs))
        hit = self.memo.get(key)
        ttl_s = float(cache.get("ttl_ms") or DEFAULT_CACHE_TTL_MS) / 1000.0
        if hit is None or time.monotonic() - hit[2] > ttl_s:
            self.memo_keys[nid] = key
            return False
        source_attempt, outputs, _ = hit
        a = self.new_attempt(nid, inputs)
        now = time.time()
        rec = AttemptRecord(a.attempt_id, nid, a.number, "SUCCEEDED", now, now,
                            cache_hit_of=source_attempt, work_dir=a.work_dir)
        self.results[nid].attempts.append(rec)
        self.results[nid].outputs = dict(outputs)
        self.set_state(nid, _dag.SUCCEEDED)
        self.event("memo_hit", nid, cache_hit_of=source_attempt)
        return True

    def finish(self, a: _Attempt, o: _Outcome, started: float, retry_at: dict[str, float]) -> None:
        nid = a.node_id
        node = self.graph.nodes[nid]
        rec = AttemptRecord(
            a.attempt_id, nid, a.number, o.state, started, time.time(),
            error_class=o.error_class, error_type=o.error_type, message=o.message,
            exit_code=o.exit_code, work_dir=a.work_dir,
        )
        self.results[nid].attempts.append(rec)
        self.event("attempt_finished", nid, attempt_id=a.attempt_id, state=o.state, error_class=o.error_class)
        if o.state == "SUCCEEDED":
            self.results[nid].outputs = o.outputs
            self.set_state(nid, _dag.SUCCEEDED)
            key = self.memo_keys.get(nid)
            if key is not None:
                self.memo.setdefault(key, (a.attempt_id, dict(o.outputs), time.monotonic()))
            return
        if o.state == "CANCELLED":
            self.set_state(nid, _dag.CANCELLED, "run cancelled")
            return
        stc_ms = float((node.spec.get("timeouts") or {}).get("schedule_to_close_ms") or 0)
        stc_exceeded = stc_ms > 0 and (
            time.monotonic() - self.first_scheduled.get(nid, time.monotonic())
        ) * 1000 >= stc_ms
        retry, why = _dag.should_retry(
            o.error_class or "ERROR_CLASS_UNSPECIFIED",
            str(node.spec.get("idempotency") or "IDEMPOTENCY_MODE_REQUIRED"),
            a.number, node.spec.get("retry"), stc_exceeded,
        )
        rec.retry_decision = why
        if retry and not self.cancel.is_set():
            delay = _dag.backoff_seconds(node.spec.get("retry"), a.number, self.rng.random) * self.backoff_scale
            retry_at[nid] = time.monotonic() + delay
            self.log(f"{nid}: attempt {a.number} {o.state} ({_short_class(o.error_class)}); "
                     f"retrying in {delay:.1f}s [{why}]")
            return
        self.set_state(nid, _dag.FAILED)


def _wire_ref(ref: Mapping[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in ref.items() if k in ("blob", "content_type", "size_bytes")}


def _safe(name: str) -> str:
    out = "".join(c if c.isalnum() or c in "-_" else "_" for c in name)
    return out or "unnamed"


def _kill_group(proc: subprocess.Popen[bytes], sig: int) -> None:
    try:
        os.killpg(proc.pid, sig)
    except (ProcessLookupError, PermissionError):
        pass


def _log_tail(path: str) -> str:
    try:
        with open(path, "rb") as f:
            f.seek(0, os.SEEK_END)
            size = f.tell()
            f.seek(max(0, size - MAX_LOG_TAIL_BYTES))
            data = f.read()
    except OSError:
        return ""
    text = data.decode("utf-8", errors="replace").strip()
    if size > MAX_LOG_TAIL_BYTES and "\n" in text:
        text = "...(truncated)...\n" + text.split("\n", 1)[1]
    return text


def _module_dirs(modules: Sequence[str]) -> list[str]:
    return sorted({os.path.dirname(os.path.abspath(m)) for m in modules if m.endswith(".py")})


def _handler_modules(spec: Mapping[str, Any]) -> list[str]:
    """What a worker subprocess must import to find each Python handler: the
    module NAME when it is importable, else its source FILE."""
    out: list[str] = []
    for t in spec.get("tasks") or []:
        if (t.get("runtime") or {}).get("command"):
            continue
        d = registry.get(str(t.get("task_ref", "")))
        if d is None:
            continue
        mod = getattr(d.fn, "__module__", "") or ""
        target = ""
        # A module inside a package is imported by NAME (its relative imports
        # need the package); a top-level module by FILE, since it may have been
        # loaded from a path that is not importable by name at all.
        if "." in mod:
            try:
                found = importlib.util.find_spec(mod)
            except (ImportError, ValueError):
                found = None
            if found is not None:
                target = mod
        if not target:
            try:
                target = inspect.getsourcefile(d.fn) or ""
            except TypeError:
                target = ""
        if target and target not in out:
            out.append(target)
    return out


# ── the public API ────────────────────────────────────────────────────────────


def run(
    workflow: WorkflowDefinition | CompiledWorkflow | Mapping[str, Any],
    params: Mapping[str, Any] | None = None,
    *,
    run_dir: str | None = None,
    modules: Sequence[str] = (),
    isolation: str = "subprocess",
    python: str | None = None,
    max_parallel: int | None = None,
    backoff_scale: float = 1.0,
    fan_in_limit: int = _dag.DEFAULT_MAX_FAN_IN_BYTES,
    max_fanout_ceiling: int = _dag.DEFAULT_MAX_FANOUT_CEILING,
    inline_threshold: int = DEFAULT_INLINE_THRESHOLD_BYTES,
    host_executor: bool = False,
    run_id: str | None = None,
    seed: int | None = None,
    out: IO[str] | None = sys.stderr,
) -> LocalRunResult:
    """Execute a workflow on this machine and return its outcome.

    ``workflow`` is a ``@workflow``, a compiled workflow, or a WorkflowSpec
    document (e.g. a loaded compiled.yaml). Shell tasks run their
    ``runtime.command``; tasks without one run their registered Python handler
    through ``python -m nexus.worker`` (``isolation="subprocess"``, the default:
    one process per attempt, killable on timeout) or in this process
    (``isolation="inprocess"``: debuggable, but a timeout cannot stop it).

    ``backoff_scale`` multiplies retry backoff (0 retries immediately);
    ``fan_in_limit``, ``max_fanout_ceiling`` and ``inline_threshold`` are the
    operator settings the platform would apply. ``out`` receives progress lines
    and the final summary; pass None for silence.

    The platform's semantics are followed; its infrastructure is not. See the
    module docstring and ADR-0022 for exactly what differs.
    """
    if isolation not in ("subprocess", "inprocess"):
        raise ValueError("isolation must be 'subprocess' or 'inprocess'")
    if not (_dag.MIN_FAN_IN_BYTES <= fan_in_limit <= _dag.MAX_FAN_IN_BYTES_CEILING):
        raise ValueError(
            f"fan_in_limit must be within [{_dag.MIN_FAN_IN_BYTES}, {_dag.MAX_FAN_IN_BYTES_CEILING}]"
        )
    if isinstance(workflow, WorkflowDefinition):
        compiled: Mapping[str, Any] = workflow.compile().spec
    elif isinstance(workflow, CompiledWorkflow):
        compiled = workflow.spec
    else:
        compiled = workflow
    spec = json.loads(json.dumps(compiled))  # a private, plain copy
    rid = run_id or str(uuid.uuid4())
    root = os.path.abspath(run_dir or os.path.join(".nexus-local", "runs", rid))
    os.makedirs(root, exist_ok=True)
    with open(os.path.join(root, "spec.json"), "w", encoding="utf-8") as f:
        json.dump(spec, f, sort_keys=True, indent=2)

    def log(msg: str) -> None:
        if out is not None:
            print(f"[local] {msg}", file=out, flush=True)

    mods = list(modules) or _handler_modules(spec)
    runner = _Runner(
        spec, params or {}, run_id=rid, run_dir=root, modules=mods,
        python=python or sys.executable, isolation=isolation,
        max_parallel=max_parallel or min(8, os.cpu_count() or 2),
        backoff_scale=backoff_scale, fan_in_limit=fan_in_limit,
        max_fanout_ceiling=max_fanout_ceiling, inline_threshold=inline_threshold,
        host_executor=host_executor, seed=seed, log=log,
    )
    started = time.time()
    run_timeout = spec.get("run_timeout_ms")
    log(f"run {rid} of {spec.get('name')} in {root}")
    state = runner.run(float(run_timeout) / 1000.0 if run_timeout else None)
    result = LocalRunResult(
        run_id=rid, workflow=str(spec.get("name", "")), state=state, nodes=runner.results,
        run_dir=root, started_at=started, ended_at=time.time(), store=runner.store,
        order=list(runner.graph.order),
    )
    with open(os.path.join(root, "run.json"), "w", encoding="utf-8") as f:
        json.dump(result.to_json(), f, sort_keys=True, indent=2)
    if out is not None:
        print(result.summary(), file=out, flush=True)
    return result


# ── python -m nexus.local ─────────────────────────────────────────────────────


def _load_workflow(path: str, wanted: str) -> Mapping[str, Any]:
    if path.endswith(".py"):
        name = os.path.splitext(os.path.basename(path))[0]
        spec = importlib.util.spec_from_file_location(name, path)
        if spec is None or spec.loader is None:
            raise LocalRunError(f"cannot load {path}")
        module = importlib.util.module_from_spec(spec)
        sys.modules[name] = module
        sys.path.insert(0, os.path.dirname(os.path.abspath(path)))
        spec.loader.exec_module(module)
        found = {k: v for k, v in vars(module).items() if isinstance(v, WorkflowDefinition)}
        if not found:
            raise LocalRunError(f"{path} defines no @workflow")
        if wanted:
            by_name = {v.name: v for v in found.values()}
            wf = found.get(wanted) or by_name.get(wanted)
            if wf is None:
                raise LocalRunError(f"{path} has no workflow {wanted!r}; found: {', '.join(sorted(found))}")
        elif len(found) == 1:
            wf = next(iter(found.values()))
        else:
            raise LocalRunError(f"{path} defines {len(found)} workflows ({', '.join(sorted(found))}); "
                                "select one with --workflow")
        return wf.compile().spec
    with open(path, "rb") as f:
        raw = f.read()
    if raw.lstrip()[:1] in (b"{", b"["):
        doc = json.loads(raw)
    else:
        try:
            import yaml  # type: ignore[import-untyped,unused-ignore]
        except ModuleNotFoundError:
            raise LocalRunError(
                f"{path} is YAML and PyYAML is not installed: pip install 'nexus-sdk[yaml]', "
                "or run it with `nexusctl run --local`, which converts YAML itself"
            ) from None
        doc = yaml.safe_load(raw)
    if not isinstance(doc, dict):
        raise LocalRunError(f"{path}: a WorkflowSpec must be a mapping")
    return doc


def _parse_param(raw: str) -> tuple[str, Any]:
    key, sep, value = raw.partition("=")
    if not sep or not key:
        raise LocalRunError(f"--param {raw!r} is not KEY=VALUE")
    try:
        return key, json.loads(value)
    except ValueError:
        return key, value


def main(argv: Sequence[str] | None = None) -> int:
    p = argparse.ArgumentParser(
        prog="python -m nexus.local",
        description="Run a workflow on this machine without a cluster (ADR-0022).",
    )
    p.add_argument("file", help="A .py file with a @workflow, or a WorkflowSpec as JSON or YAML.")
    p.add_argument("--workflow", default="", help="Workflow to run when a .py file defines several.")
    p.add_argument("--param", action="append", default=[], help="KEY=VALUE (JSON-parsed when valid JSON).")
    p.add_argument("--params-json", default="", help="All parameters as one JSON object.")
    p.add_argument("--module", action="append", default=[], help="Module or .py path defining handlers.")
    p.add_argument("--run-dir", default="", help="Directory for results, logs and run.json.")
    p.add_argument("--run-id", default="", help="Run id (default: a random UUID).")
    p.add_argument("--isolation", choices=("subprocess", "inprocess"), default="subprocess")
    p.add_argument("--max-parallel", type=int, default=0)
    p.add_argument("--backoff-scale", type=float, default=1.0)
    p.add_argument("--fan-in-limit", type=int, default=_dag.DEFAULT_MAX_FAN_IN_BYTES)
    p.add_argument("--inline-threshold", type=int, default=DEFAULT_INLINE_THRESHOLD_BYTES)
    p.add_argument("--host-executor", action="store_true",
                   help="Run docker/kubernetes tasks' commands on this host (not in their image).")
    p.add_argument("--source", default="", help=argparse.SUPPRESS)
    p.add_argument("--output", choices=("table", "json"), default="table")
    args = p.parse_args(argv)

    try:
        for m in args.module:
            from .worker import import_modules

            import_modules([m])
        spec = _load_workflow(args.file, args.workflow)
        params: dict[str, Any] = {}
        if args.params_json:
            loaded = json.loads(args.params_json)
            if not isinstance(loaded, dict):
                raise LocalRunError("--params-json must be a JSON object")
            params.update(loaded)
        for raw in args.param:
            k, v = _parse_param(raw)
            params[k] = v
        result = run(
            spec, params, run_dir=args.run_dir or None, modules=args.module,
            isolation=args.isolation, max_parallel=args.max_parallel or None,
            backoff_scale=args.backoff_scale, fan_in_limit=args.fan_in_limit,
            inline_threshold=args.inline_threshold, host_executor=args.host_executor,
            run_id=args.run_id or None,
            out=sys.stderr if args.output == "table" else None,
        )
    except (LocalRunError, ValueError, OSError) as e:
        print(f"nexus.local: {e}", file=sys.stderr)
        return 2
    if args.output == "json":
        print(json.dumps(result.to_json(), sort_keys=True, indent=2))
    return 0 if result.succeeded else 1


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
