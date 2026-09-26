# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""Abstract values and the IR builder for nexus_import.py (see its docstring)."""

from __future__ import annotations

import ast

TRANSLATED, APPROXIMATED, UNSUPPORTED = "translated", "approximated", "unsupported"


class Unknown:
    """A value the converter cannot know without running code."""

    def __init__(self, reason, line=0, pattern=None):
        self.reason, self.line, self.pattern = reason, line, pattern


class Dur:
    def __init__(self, seconds):
        self.seconds = float(seconds)


class DT:
    def __init__(self, iso, tz=None):
        self.iso, self.tz = iso, tz


class TZ:
    def __init__(self, name):
        self.name = name


class TaskRef:
    """A task, or one named output of it (an XComArg / a Prefect future)."""

    def __init__(self, task, key=None):
        self.task, self.key = task, key


class GroupRef:
    def __init__(self, wf, gid):
        self.wf, self.gid = wf, gid


class ParamRef:
    def __init__(self, name):
        self.name = name


class FuncRef:
    """A function defined in the analysed file, with what its decorator makes it."""

    def __init__(self, node, kind, variant="python", opts=None, deco_line=0, nested=False):
        self.node, self.kind, self.variant = node, kind, variant
        self.opts = opts or {}
        self.deco_line = deco_line or node.lineno
        self.nested = nested
        self.name = node.name
        self.called = False


class Override:
    """f.override(...) (Airflow) / f.with_options(...) (Prefect)."""

    def __init__(self, func, opts):
        self.func, self.opts = func, opts


class Partial:
    def __init__(self, target, kwargs, line, opts=None):
        self.target, self.kwargs, self.line, self.opts = target, kwargs, line, opts or {}


class Obj:
    """A call to a known, side-effect-free constructor, kept as data."""

    def __init__(self, cls, args=None, kwargs=None, line=0):
        self.cls, self.args, self.kwargs, self.line = cls, args or [], kwargs or {}, line


class DagHandle:
    def __init__(self, wf):
        self.wf = wf


class Signal:
    """A `return` inside a function body being walked."""

    def __init__(self, value):
        self.value = value


MISSING = object()


class Scope:
    def __init__(self, parent):
        self.parent, self.vars = parent, {}

    def set(self, name, value):
        self.vars[name] = value

    def lookup(self, name):
        s = self
        while s is not None:
            if name in s.vars:
                return s.vars[name]
            s = s.parent
        return MISSING


def is_const(v):
    if v is None or isinstance(v, (str, int, float, bool)):
        return True
    if isinstance(v, list):
        return all(is_const(x) for x in v)
    if isinstance(v, dict):
        return all(isinstance(k, str) and is_const(x) for k, x in v.items())
    return False


def contains_refs(v):
    if isinstance(v, (TaskRef, GroupRef)):
        return True
    if isinstance(v, list):
        return any(contains_refs(x) for x in v)
    if isinstance(v, dict):
        return any(contains_refs(x) for x in v.values())
    return False


def refs_in(v):
    if isinstance(v, TaskRef):
        return [v]
    out = []
    if isinstance(v, list):
        for x in v:
            out.extend(refs_in(x))
    elif isinstance(v, dict):
        for x in v.values():
            out.extend(refs_in(x))
    return out


def describe(v):
    if isinstance(v, Unknown):
        return v.reason
    if isinstance(v, TaskRef):
        return "the output of task %s" % v.task["id"]
    if isinstance(v, ParamRef):
        return "parameter %s" % v.name
    if v is None:
        return "None"
    return type(v).__name__


def ms(seconds):
    return int(round(float(seconds) * 1000))


def value_type(v):
    """Nexus ValueSpec type for a Python literal."""
    if isinstance(v, bool):
        return "bool"
    if isinstance(v, int):
        return "int"
    if isinstance(v, float):
        return "float"
    if isinstance(v, str):
        return "string"
    return "json"


ANNOTATION_TYPES = {
    "str": "string", "int": "int", "float": "float", "bool": "bool",
    "Path": "string", "PosixPath": "string",
}


def annotation_type(node):
    if node is None:
        return None
    if isinstance(node, ast.Name):
        return ANNOTATION_TYPES.get(node.id, "json")
    if isinstance(node, ast.Attribute):
        return ANNOTATION_TYPES.get(node.attr, "json")
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return ANNOTATION_TYPES.get(node.value, "json")
    return "json"


def seconds_of(v):
    if isinstance(v, Dur):
        return v.seconds
    if isinstance(v, (int, float)) and not isinstance(v, bool):
        return float(v)
    return None


def new_task(tid, line, kind):
    return {"id": tid, "line": line, "kind": kind, "inputs": {}, "outputs": {},
            "input_literals": {}, "input_params": {}, "env": {}}


class Workflow:
    """One DAG / flow being built, in IR form."""

    def __init__(self, source_id, file, line):
        self.source_id, self.file, self.line = source_id, file, line
        self.description = ""
        self.tasks = {}  # id -> task dict, in creation order
        self.edges = {}  # (from, to) -> edge dict
        self.params = {}
        self.groups = {}  # full group id -> [task ids]
        self.findings = []
        self.schedules = []
        self.run_timeout_ms = None
        self.max_active_runs = None
        self.fail_fast = None
        self.labels = {}
        self.defaults = {}  # policy from default_args
        self.used_ids = {}
        self.function = None

    def find(self, line, construct, status, message, nexus=""):
        self.findings.append({"file": self.file, "line": line, "construct": construct,
                              "status": status, "message": message, "nexus": nexus})

    def unique_id(self, base):
        """TaskFlow semantics: a second call of the same task gets __1, __2, ..."""
        if base not in self.tasks:
            return base
        n = self.used_ids.get(base, 0)
        while True:
            n += 1
            cand = "%s__%d" % (base, n)
            if cand not in self.tasks:
                self.used_ids[base] = n
                return cand

    def add_edge(self, frm, to, line, inputs=None):
        if frm == to:
            self.find(line, "dependency", UNSUPPORTED, "task %s depends on itself" % frm)
            return
        e = self.edges.get((frm, to))
        if e is None:
            e = {"from": frm, "to": to, "inputs": {}, "line": line}
            self.edges[(frm, to)] = e
        if inputs:
            e["inputs"].update(inputs)

    def members(self, gid):
        return list(self.groups.get(gid, []))

    def leaves(self, gid):
        m = set(self.members(gid))
        return [t for t in self.members(gid) if not any(f == t and to in m for (f, to) in self.edges)]

    def roots(self, gid):
        m = set(self.members(gid))
        return [t for t in self.members(gid) if not any(to == t and f in m for (f, to) in self.edges)]

    def to_ir(self):
        tasks = [{k: v for k, v in t.items() if not k.startswith("_")} for t in self.tasks.values()]
        return {
            "source_id": self.source_id,
            "file": self.file,
            "line": self.line,
            "description": self.description,
            "params": self.params,
            "tasks": tasks,
            "edges": list(self.edges.values()),
            "schedules": self.schedules,
            "run_timeout_ms": self.run_timeout_ms,
            "max_active_runs": self.max_active_runs,
            "fail_fast": self.fail_fast,
            "labels": self.labels,
            "function": self.function,
            "findings": self.findings,
        }
