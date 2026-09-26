# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""A line-by-line port of the pure DAG core (internal/dag, internal/domain).

The local runner (nexus.local, ADR-0022) must decide what runs next exactly as
the platform's dag-engine does, so this module mirrors -- function for
function, and in the same order of reasoning -- the Go code it names:

    evaluate_trigger / propagate / reconcile  internal/dag/engine.go
    aggregate_children / fold_templates       internal/dag/expand.go
    plan_from_collection / FanInBuilder       internal/dag/expand.go
    input_plan                                internal/dag/resolve.go
    should_retry / backoff_seconds            internal/domain/{retry,backoff}.go
    idempotency_key / memo_key                internal/domain/idemkey.go

Nothing here performs I/O, reads a clock or draws randomness except through
arguments, for the same reason the Go package is pure: it is the part that is
exhaustively tested. The shared fixtures in tests/parity/*.json are executed by
BOTH this module (tests/test_local_parity.py) and internal/dag
(internal/dag/localparity_test.go); a divergence fails one of them.
"""

from __future__ import annotations

import base64
import dataclasses
import hashlib
import json
import math
from typing import Any, Callable, Iterable, Mapping, Sequence

# ── states ────────────────────────────────────────────────────────────────────

PENDING, READY, RUNNING = "PENDING", "READY", "RUNNING"
SUCCEEDED, FAILED, SKIPPED = "SUCCEEDED", "FAILED", "SKIPPED"
UPSTREAM_FAILED, CANCELLED = "UPSTREAM_FAILED", "CANCELLED"

RUN_RUNNING, RUN_SUCCEEDED, RUN_FAILED = "RUNNING", "SUCCEEDED", "FAILED"
RUN_CANCELLED, RUN_TIMED_OUT = "CANCELLED", "TIMED_OUT"

TERMINAL = frozenset({SUCCEEDED, FAILED, SKIPPED, UPSTREAM_FAILED, CANCELLED})

_TRIGGERS = {
    "TRIGGER_RULE_ALL_SUCCESS": "all_success",
    "TRIGGER_RULE_ALL_DONE": "all_done",
    "TRIGGER_RULE_ONE_SUCCESS": "one_success",
    "TRIGGER_RULE_ONE_FAILED": "one_failed",
    "TRIGGER_RULE_NONE_FAILED": "none_failed",
    "TRIGGER_RULE_ALL_SKIPPED": "all_skipped",
}


def trigger_rule(value: object) -> str:
    """TriggerRuleFromProto: unknown/unset means all_success."""
    if isinstance(value, str):
        short = value.strip().lower()
        if short in _TRIGGERS.values():
            return short
        return _TRIGGERS.get(value.strip().upper(), "all_success")
    return "all_success"


def is_failed(state: str) -> bool:
    return state in (FAILED, UPSTREAM_FAILED)


# ── graph ─────────────────────────────────────────────────────────────────────


@dataclasses.dataclass
class Node:
    id: str
    spec: dict[str, Any]
    trigger: str
    parent: str = ""
    map_index: int = -1

    @property
    def is_template(self) -> bool:
        return isinstance(self.spec.get("map"), Mapping) and not self.parent

    @property
    def is_child(self) -> bool:
        return bool(self.parent)


@dataclasses.dataclass(frozen=True)
class Edge:
    frm: str
    to: str
    output_to_input: Mapping[str, str]


class Graph:
    """BuildWithChildren: nodes, sorted edges, adjacency, Kahn order."""

    def __init__(self, spec: Mapping[str, Any], children: Mapping[str, tuple[str, int]] | None = None):
        children = children or {}
        self.fail_fast = bool(spec.get("fail_fast"))
        self.params: Mapping[str, Any] = spec.get("params") or {}
        self.nodes: dict[str, Node] = {}
        for t in spec.get("tasks") or []:
            nid = str(t["id"])
            if nid in self.nodes:
                continue
            n = Node(id=nid, spec=dict(t), trigger=trigger_rule(t.get("trigger_rule")))
            if nid in children:
                n.parent, n.map_index = children[nid]
            self.nodes[nid] = n
        self.children: dict[str, list[str]] = {}
        for nid, n in self.nodes.items():
            if n.parent and n.parent in self.nodes:
                self.children.setdefault(n.parent, []).append(nid)
        for kids in self.children.values():
            kids.sort(key=lambda k: self.nodes[k].map_index)
        seen: set[tuple[str, str]] = set()
        edges: list[Edge] = []
        for e in spec.get("edges") or []:
            key = (str(e["from_node_id"]), str(e["to_node_id"]))
            if key in seen:
                continue
            seen.add(key)
            edges.append(Edge(key[0], key[1], dict(e.get("output_to_input") or {})))
        edges.sort(key=lambda e: (e.frm, e.to))
        self.edges = edges
        self.preds: dict[str, list[str]] = {}
        self.succs: dict[str, list[str]] = {}
        self.edges_to: dict[str, list[Edge]] = {}
        for e in edges:
            if e.frm not in self.nodes or e.to not in self.nodes:
                continue
            self.preds.setdefault(e.to, []).append(e.frm)
            self.succs.setdefault(e.frm, []).append(e.to)
            self.edges_to.setdefault(e.to, []).append(e)
        self.order = self._topo()
        if len(self.order) != len(self.nodes):
            self.order = sorted(self.nodes)

    def _topo(self) -> list[str]:
        indeg = {nid: len(self.preds.get(nid, [])) for nid in self.nodes}
        ready = sorted(n for n, d in indeg.items() if d == 0)
        out: list[str] = []
        while ready:
            n = ready.pop(0)
            out.append(n)
            for s in self.succs.get(n, []):
                indeg[s] -= 1
                if indeg[s] == 0:
                    ready.append(s)
                    ready.sort()
        return out

    def templates(self) -> list[str]:
        return [nid for nid in self.order if self.nodes[nid].is_template]


# ── trigger evaluation and propagation (engine.go) ───────────────────────────

WAIT, FIRE, UNSATISFIABLE = 0, 1, 2


@dataclasses.dataclass
class _Tally:
    total: int = 0
    succeeded: int = 0
    failed: int = 0
    skipped: int = 0
    cancelled: int = 0
    terminal: int = 0


def _tally(g: Graph, nid: str, st: Mapping[str, str]) -> _Tally:
    t = _Tally()
    for p in g.preds.get(nid, []):
        s = st.get(p, PENDING)
        t.total += 1
        if s in TERMINAL:
            t.terminal += 1
        if s == SUCCEEDED:
            t.succeeded += 1
        elif s in (FAILED, UPSTREAM_FAILED):
            t.failed += 1
        elif s == SKIPPED:
            t.skipped += 1
        elif s == CANCELLED:
            t.cancelled += 1
    return t


def evaluate_trigger(rule: str, t: _Tally) -> int:
    if t.total == 0:
        return FIRE
    if rule == "all_success":
        if t.succeeded == t.total:
            return FIRE
        if t.failed or t.skipped or t.cancelled:
            return UNSATISFIABLE
        return WAIT
    if rule == "all_done":
        return FIRE if t.terminal == t.total else WAIT
    if rule == "one_success":
        if t.succeeded:
            return FIRE
        return UNSATISFIABLE if t.terminal == t.total else WAIT
    if rule == "one_failed":
        if t.failed:
            return FIRE
        return UNSATISFIABLE if t.terminal == t.total else WAIT
    if rule == "none_failed":
        if t.failed:
            return UNSATISFIABLE
        return FIRE if t.terminal == t.total else WAIT
    if rule == "all_skipped":
        if t.skipped == t.total:
            return FIRE
        if t.terminal == t.total:
            return UNSATISFIABLE
        if t.succeeded or t.failed or t.cancelled:
            return UNSATISFIABLE
        return WAIT
    return WAIT


@dataclasses.dataclass(frozen=True)
class Transition:
    node_id: str
    frm: str
    to: str
    reason: str
    cause: str = ""


def _first_pred_in(g: Graph, nid: str, st: Mapping[str, str], want: Iterable[str]) -> str:
    wanted = set(want)
    for p in sorted(g.preds.get(nid, [])):
        if st.get(p, PENDING) in wanted:
            return p
    return ""


def propagate(g: Graph, st: Mapping[str, str]) -> list[Transition]:
    work = dict(st)
    out: list[Transition] = []
    while True:
        progressed = False
        for nid in g.order:
            if work.get(nid, PENDING) != PENDING:
                continue
            t = _tally(g, nid, work)
            if evaluate_trigger(g.nodes[nid].trigger, t) != UNSATISFIABLE:
                continue
            to, reason, cause = SKIPPED, "upstream_skipped", ""
            if t.failed:
                to, reason = UPSTREAM_FAILED, "upstream_failed"
                cause = _first_pred_in(g, nid, work, (FAILED, UPSTREAM_FAILED))
            elif t.skipped:
                cause = _first_pred_in(g, nid, work, (SKIPPED,))
            elif t.cancelled:
                reason = "trigger_rule_unsatisfiable"
                cause = _first_pred_in(g, nid, work, (CANCELLED,))
            else:
                reason = "trigger_rule_unsatisfiable"
            out.append(Transition(nid, PENDING, to, reason, cause))
            work[nid] = to
            progressed = True
        if not progressed:
            break
    out.sort(key=lambda t: t.node_id)
    return out


def aggregate_children(states: Sequence[str]) -> str | None:
    failed = cancelled = skipped = 0
    for s in states:
        if s not in TERMINAL:
            return None
        if s in (FAILED, UPSTREAM_FAILED):
            failed += 1
        elif s == CANCELLED:
            cancelled += 1
        elif s == SKIPPED:
            skipped += 1
    if failed:
        return FAILED
    if cancelled:
        return CANCELLED
    if skipped:
        return SKIPPED
    return SUCCEEDED


def fold_templates(g: Graph, st: dict[str, str]) -> list[Transition]:
    out: list[Transition] = []
    for nid in g.templates():
        if st.get(nid, PENDING) != RUNNING:
            continue
        agg = aggregate_children([st.get(k, PENDING) for k in g.children.get(nid, [])])
        if agg is None:
            continue
        out.append(Transition(nid, RUNNING, agg, "expansion_settled"))
        st[nid] = agg
    return out


def _ready(g: Graph, st: Mapping[str, str]) -> list[str]:
    return sorted(
        nid
        for nid in g.order
        if st.get(nid, PENDING) == PENDING
        and evaluate_trigger(g.nodes[nid].trigger, _tally(g, nid, st)) == FIRE
    )


def run_outcome(g: Graph, st: Mapping[str, str]) -> str:
    states = [st.get(n, PENDING) for n in g.order]
    all_terminal = all(s in TERMINAL for s in states)
    failed = any(is_failed(s) for s in states)
    if failed and all_terminal:
        return RUN_FAILED
    if not all_terminal:
        return RUN_RUNNING
    if CANCELLED in states:
        return RUN_CANCELLED
    return RUN_SUCCEEDED


@dataclasses.dataclass
class Decision:
    frontier: list[str]
    expansions: list[str]
    transitions: list[Transition]
    run_state: str
    halted: bool


def reconcile(g: Graph, states: Mapping[str, str], run_state: str = RUN_RUNNING) -> Decision:
    """engine.go Reconcile, including the cancel and fail_fast settlement."""
    st = dict(states)
    transitions: list[Transition] = list(fold_templates(g, st))
    prop = propagate(g, st)
    for t in prop:
        st[t.node_id] = t.to
    transitions += prop
    halted = g.fail_fast and any(is_failed(st.get(n, PENDING)) for n in g.order)
    cancelling = run_state in ("CANCELLING", RUN_CANCELLED)
    frontier: list[str] = []
    expansions: list[str] = []
    if not (halted or cancelling):
        ready = _ready(g, st)
        expansions = [n for n in ready if g.nodes[n].is_template]
        frontier = [n for n in ready if not g.nodes[n].is_template]
        for nid in frontier:
            transitions.append(Transition(nid, st.get(nid, PENDING), READY, "trigger_rule_satisfied"))
            st[nid] = READY
    elif cancelling:
        for nid in g.order:
            if st.get(nid, PENDING) in (PENDING, READY):
                transitions.append(Transition(nid, st.get(nid, PENDING), CANCELLED, "run_cancelled"))
                st[nid] = CANCELLED
    else:
        for nid in g.order:
            if st.get(nid, PENDING) == PENDING:
                transitions.append(Transition(nid, PENDING, SKIPPED, "fail_fast_halt"))
                st[nid] = SKIPPED
    transitions += fold_templates(g, st)
    if cancelling:
        out_state = RUN_CANCELLED if all(st.get(n, PENDING) in TERMINAL for n in g.order) else "CANCELLING"
    else:
        out_state = run_outcome(g, st)
    return Decision(frontier, expansions, transitions, out_state, halted)


# ── expansion and fan-in (expand.go) ──────────────────────────────────────────

DEFAULT_MAX_FANOUT_CEILING = 10000
DEFAULT_MAX_FAN_IN_BYTES = 16 << 20
MAX_FAN_IN_BYTES_CEILING = 256 << 20
MIN_FAN_IN_BYTES = 64 << 10


def child_id(template: str, index: int) -> str:
    return f"{template}[{index}]"


class ExpansionFailure(Exception):
    def __init__(self, reason: str, detail: str) -> None:
        super().__init__(f"{reason}: {detail}")
        self.reason = reason
        self.detail = detail


def plan_from_collection(template: str, payload: bytes | None, inline: bool, max_fanout: int) -> list[bytes]:
    """PlanFromCollection over an already-extracted payload (None and
    inline=True when the upstream produced nothing; inline=False for a blob).
    Returns each element's canonical JSON, in order."""
    if not inline:
        raise ExpansionFailure(
            "collection_not_inline",
            "the collection was stored as a blob; the control plane never reads task payloads "
            "(invariant 6), so a mapped collection must fit the inline threshold -- emit a list of "
            "references (object keys, ids) rather than the data itself",
        )
    if payload is None or payload.strip() == b"null":
        raise ExpansionFailure("collection_missing", "the upstream produced no value for the mapped output")
    try:
        items = json.loads(payload, parse_int=_RawNumber, parse_float=_RawNumber)
    except ValueError as e:
        raise ExpansionFailure("collection_not_array", f"the mapped output is not a JSON array: {e}") from None
    if not isinstance(items, list):
        raise ExpansionFailure("collection_not_array", "the mapped output is not a JSON array")
    if max_fanout <= 0:
        raise ExpansionFailure("max_fanout_exceeded", f"expansion of {template!r}: max_fanout is mandatory")
    if len(items) > max_fanout:
        raise ExpansionFailure(
            "max_fanout_exceeded",
            f"expansion of {template!r}: cardinality {len(items)} exceeds max_fanout {max_fanout}; "
            "failing explicitly rather than materializing an unbounded graph",
        )
    return [canonical_json(it).encode("utf-8") for it in items]


class FanInTooLarge(Exception):
    reason = "fan_in_too_large"

    def __init__(self, template: str, output: str, limit: int, children: int, total: int) -> None:
        self.template, self.output, self.limit = template, output, limit
        self.children, self.total = children, total
        super().__init__(
            f"fan_in_too_large: the fan-in of {template}.{output} exceeds the {limit}-byte limit "
            f"after {children} of {total} children; raise the fan-in limit (max "
            f"{MAX_FAN_IN_BYTES_CEILING}) or have the children write large results as blob "
            "outputs (passed by reference) or smaller summaries"
        )


class FanInBuilder:
    """FanInBuilder: one element per child in map_index order, bounded BEFORE
    each append. Elements: the child's inline JSON value; {"inline_base64":..}
    for a non-JSON inline payload; {"result_ref": ref} for a blob; null for a
    child that did not succeed."""

    def __init__(self, template: str, output: str, total: int, limit: int) -> None:
        self.template, self.output, self.total = template, output, total
        self.limit = limit if limit > 0 else DEFAULT_MAX_FAN_IN_BYTES
        self.parts: list[bytes] = []
        self.size = 1  # "["

    def add(self, element: bytes) -> None:
        sep = 1 if self.parts else 0
        if self.size + sep + len(element) + 1 > self.limit:
            raise FanInTooLarge(self.template, self.output, self.limit, len(self.parts), self.total)
        self.parts.append(element)
        self.size += sep + len(element)

    def finish(self) -> bytes:
        return b"[" + b",".join(self.parts) + b"]"


def fan_in_element(payload: bytes | None, inline: bool, ref_json: Mapping[str, Any] | None) -> bytes:
    if payload is None and ref_json is None:
        return b"null"
    if not inline:
        return b'{"result_ref":' + canonical_json(ref_json).encode("utf-8") + b"}"
    assert payload is not None
    try:
        value = json.loads(payload, parse_int=_RawNumber, parse_float=_RawNumber)
    except ValueError:
        return canonical_json({"inline_base64": base64.b64encode(payload).decode("ascii")}).encode("utf-8")
    return canonical_json(value).encode("utf-8")


# ── input plan (resolve.go) ───────────────────────────────────────────────────


@dataclasses.dataclass(frozen=True)
class Binding:
    input: str
    source: str  # "edge" | "param" | "default" | "map_item"
    upstream_node: str = ""
    upstream_output: str = ""
    param: str = ""
    default_json: str = ""
    required: bool = False


def map_item_input(g: Graph, nid: str) -> str:
    n = g.nodes.get(nid)
    if n is None:
        return ""
    if n.is_template:
        return str(n.spec["map"].get("item_input", ""))
    if n.is_child:
        p = g.nodes.get(n.parent)
        if p is not None and isinstance(p.spec.get("map"), Mapping):
            return str(p.spec["map"].get("item_input", ""))
    return ""


def input_plan(g: Graph, nid: str) -> list[Binding]:
    n = g.nodes[nid]
    inputs: Mapping[str, Any] = n.spec.get("inputs") or {}
    literals: Mapping[str, str] = n.spec.get("input_literals") or {}
    input_params: Mapping[str, str] = n.spec.get("input_params") or {}
    bound: dict[str, Binding] = {}
    for e in g.edges_to.get(nid, []):
        for in_name, out_name in e.output_to_input.items():
            vs = inputs.get(in_name) or {}
            bound[in_name] = Binding(in_name, "edge", e.frm, out_name, required=bool(vs.get("required")))
    item = map_item_input(g, nid)
    for in_name, vs in inputs.items():
        if in_name in bound:
            continue
        required = bool((vs or {}).get("required"))
        if in_name == item:
            bound[in_name] = Binding(in_name, "map_item", required=required)
            continue
        param = input_params.get(in_name) or in_name
        if param in g.params:
            bound[in_name] = Binding(in_name, "param", param=param, required=required)
        elif in_name in literals:
            bound[in_name] = Binding(in_name, "default", default_json=str(literals[in_name]), required=required)
        elif (vs or {}).get("default_json"):
            bound[in_name] = Binding(in_name, "default", default_json=str(vs["default_json"]), required=required)
        elif required:
            raise ValueError(f"input plan: node {nid!r} required input {in_name!r} has no source")
    return [bound[k] for k in sorted(bound)]


# ── retry policy (domain/retry.go, domain/backoff.go) ────────────────────────

_DEFAULT_RETRY_ON = frozenset(
    {"ERROR_CLASS_TRANSIENT", "ERROR_CLASS_TIMEOUT", "ERROR_CLASS_WORKER_LOST", "ERROR_CLASS_OOM",
     "ERROR_CLASS_INFRASTRUCTURE"}
)
_ALWAYS_TERMINAL = frozenset(
    {"ERROR_CLASS_VALIDATION", "ERROR_CLASS_PERMISSION_DENIED", "ERROR_CLASS_NOT_FOUND"}
)
DEFAULT_MAX_ATTEMPTS = 3


def max_attempts(policy: Mapping[str, Any] | None) -> int:
    n = int((policy or {}).get("max_attempts") or 0)
    return n if n > 0 else DEFAULT_MAX_ATTEMPTS


def _class_allowed(error_class: str, policy: Mapping[str, Any] | None) -> tuple[bool, str]:
    p = policy or {}
    if error_class in (p.get("no_retry_on") or []):
        return False, "error_class_in_no_retry_on"
    if error_class == "ERROR_CLASS_RATE_LIMITED":
        # A throttle is the platform's concern, not the author's error: an
        # explicit retry_on that omits it still retries it (ADR-0032 §3). The
        # platform's free, uncharged retries are not modelled locally.
        return True, ""
    if error_class in _ALWAYS_TERMINAL:
        return False, "error_class_terminal"
    allow = p.get("retry_on") or []
    if allow:
        return (True, "") if error_class in allow else (False, "error_class_not_in_retry_on")
    if error_class in _DEFAULT_RETRY_ON:
        return True, ""
    return False, "error_class_not_in_retry_on"


def should_retry(
    error_class: str,
    idempotency: str,
    attempts_used: int,
    policy: Mapping[str, Any] | None,
    schedule_to_close_exceeded: bool = False,
) -> tuple[bool, str]:
    """ShouldRetry: budget, then the indeterminate gate, then class policy."""
    if attempts_used >= max_attempts(policy):
        return False, "attempts_exhausted"
    if schedule_to_close_exceeded:
        return False, "schedule_to_close_exceeded"
    if error_class == "ERROR_CLASS_WORKER_LOST":
        if idempotency == "IDEMPOTENCY_MODE_NONE" or idempotency not in (
            "IDEMPOTENCY_MODE_REQUIRED", "IDEMPOTENCY_MODE_BEST_EFFORT"
        ):
            return False, "indeterminate_outcome_requires_human"
        ok, why = _class_allowed(error_class, policy)
        if not ok:
            return False, why
        return True, (
            "retry_indeterminate_best_effort"
            if idempotency == "IDEMPOTENCY_MODE_BEST_EFFORT"
            else "retry_indeterminate_idempotent"
        )
    ok, why = _class_allowed(error_class, policy)
    return (True, "retry_error_class_retryable") if ok else (False, why)


def backoff_seconds(policy: Mapping[str, Any] | None, attempt_number: int, uniform: Callable[[], float]) -> float:
    """domain.Backoff, in seconds. attempt_number is the attempt that just ended."""
    p = policy or {}
    # float(): a spec read back from the control plane is proto JSON, whose
    # int64 fields are strings.
    initial = float(p.get("initial_ms") or 10_000) / 1000.0
    ceiling = float(p.get("max_ms") or 600_000) / 1000.0
    if ceiling < initial:
        initial = ceiling
    steps = max(0, attempt_number - 1)
    mult = float(p.get("multiplier") or 0)
    if mult <= 1:
        mult = 2.0
    strategy = p.get("strategy")
    if strategy == "BACKOFF_STRATEGY_FIXED":
        base = initial
    elif strategy == "BACKOFF_STRATEGY_LINEAR":
        base = initial * (1 + steps)
    else:
        try:
            base = initial * math.pow(mult, steps)
        except OverflowError:
            base = math.inf
    if math.isinf(base) or base > ceiling:
        base = ceiling
    jitter = p.get("jitter") or "full"
    if jitter == "none":
        return base
    if jitter == "equal":
        return base / 2 + uniform() * (base / 2)
    return uniform() * base


# ── keys (domain/idemkey.go) and content hashing (dag/memo.go) ───────────────

_SEP = "\x1f"


def idempotency_key(run_id: str, node_id: str, map_index: int | None, task_version: int) -> str:
    idx = "-" if map_index is None else str(map_index)
    return hashlib.sha256(_SEP.join((run_id, node_id, idx, str(task_version))).encode("utf-8")).hexdigest()


def memo_key(namespace: str, task_ref: str, version: int, inputs_digest: bytes) -> str:
    cache = hashlib.sha256(
        task_ref.encode("utf-8") + _SEP.encode() + str(version).encode() + _SEP.encode() + inputs_digest
    ).hexdigest()
    return hashlib.sha256((namespace + _SEP + cache).encode("utf-8")).hexdigest()


def inputs_digest(entries: Mapping[str, tuple[str, str]]) -> bytes:
    """InputsDigest over (content sha256, content type) per input name."""
    doc = {k: {"content_type": ct, "sha256": sha} for k, (sha, ct) in sorted(entries.items())}
    return json.dumps(doc, sort_keys=True, separators=(",", ":")).encode("utf-8")


# ── canonical JSON (dag.CanonicalJSON: sorted keys, number spelling kept) ────


class _RawNumber(str):
    """A JSON number carried as its source text, like Go's json.Number."""


def _go_string(s: str) -> str:
    # encoding/json escapes <, > and & (HTML-safe) and U+2028/2029.
    out = json.dumps(s, ensure_ascii=False)
    return (
        out.replace("<", "\\u003c").replace(">", "\\u003e").replace("&", "\\u0026")
        .replace("\u2028", "\\u2028").replace("\u2029", "\\u2029")
    )


def canonical_json(value: Any) -> str:
    if isinstance(value, _RawNumber):
        return str.__str__(value)
    if isinstance(value, bool):
        return "true" if value else "false"
    if value is None:
        return "null"
    if isinstance(value, str):
        return _go_string(value)
    if isinstance(value, (int, float)):
        return json.dumps(value)
    if isinstance(value, Mapping):
        return "{" + ",".join(f"{_go_string(str(k))}:{canonical_json(value[k])}" for k in sorted(value)) + "}"
    if isinstance(value, (list, tuple)):
        return "[" + ",".join(canonical_json(v) for v in value) + "]"
    raise TypeError(f"{type(value).__name__} is not JSON")


def canonicalize_bytes(raw: bytes) -> bytes:
    return canonical_json(json.loads(raw, parse_int=_RawNumber, parse_float=_RawNumber)).encode("utf-8")
