# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""The source-independent half of the analyzer: a statement walker and a
static evaluator over literals. Airflow and Prefect specifics live in
nx_airflow.py and nx_prefect.py; both are mixed into one Analyzer."""

from __future__ import annotations

import ast
import json
import os
import re
import textwrap

from nx_values import (APPROXIMATED, DT, MISSING, TRANSLATED, TZ, UNSUPPORTED, DagHandle, Dur, FuncRef, GroupRef,
                    Obj, Override, ParamRef, Partial, Scope, Signal, TaskRef, Unknown, contains_refs, describe,
                    is_const, new_task, refs_in, value_type)


class Core:
    def __init__(self, path, source, mode):
        self.path, self.source, self.mode = path, source, mode
        self.file = path
        stem = os.path.splitext(os.path.basename(path))[0]
        self.module_name = re.sub(r"\W", "_", stem)
        self.imports = {}
        self.findings = []  # file-level findings (outside any workflow)
        self.workflows = []
        self.wf = None  # the workflow currently being built
        self.groups = []  # stack of group ids
        self.dynamic = []  # stack of (kind, line, [created task ids])
        self.funcs = {}  # module-level FuncRefs by name
        self.main_calls = []
        self.module_scope = Scope(None)

    # ── findings ────────────────────────────────────────────────────────────
    def find(self, line, construct, status, message, nexus=""):
        target = self.wf.findings if self.wf is not None else self.findings
        target.append({"file": self.file, "line": line, "construct": construct,
                       "status": status, "message": message, "nexus": nexus})

    # ── name resolution ─────────────────────────────────────────────────────
    def qual(self, node):
        if isinstance(node, ast.Name):
            return self.imports.get(node.id, node.id)
        if isinstance(node, ast.Attribute):
            base = self.qual(node.value)
            return None if base is None else base + "." + node.attr
        return None

    def record_import(self, node):
        if isinstance(node, ast.Import):
            for a in node.names:
                if a.asname:
                    self.imports[a.asname] = a.name
                else:
                    head = a.name.split(".")[0]
                    self.imports[head] = head
        elif isinstance(node, ast.ImportFrom):
            mod = node.module or ""
            for a in node.names:
                self.imports[a.asname or a.name] = (mod + "." if mod else "") + a.name

    # ── entry point ─────────────────────────────────────────────────────────
    def run(self):
        try:
            tree = ast.parse(self.source, filename=self.path)
        except SyntaxError as e:
            self.findings.append({"file": self.file, "line": e.lineno or 0, "construct": "syntax",
                                  "status": UNSUPPORTED, "message": "Python syntax error: %s" % e.msg,
                                  "nexus": ""})
            return
        self.body(tree.body, self.module_scope, module=True)
        self.finish()

    def finish(self):
        pass

    # ── statements ──────────────────────────────────────────────────────────
    def body(self, stmts, sc, module=False):
        for st in stmts:
            r = self.stmt(st, sc, module)
            if isinstance(r, Signal):
                return r
        return None

    def stmt(self, st, sc, module=False):
        if isinstance(st, (ast.Import, ast.ImportFrom)):
            self.record_import(st)
            return None
        if isinstance(st, (ast.FunctionDef, ast.AsyncFunctionDef)):
            f = self.classify_function(st, sc, nested=not module)
            sc.set(st.name, f)
            if module:
                self.funcs[st.name] = f
            return None
        if isinstance(st, ast.ClassDef):
            return None
        if isinstance(st, ast.Expr):
            if isinstance(st.value, ast.Constant):
                return None  # docstring or bare literal
            self.ev(st.value, sc)
            return None
        if isinstance(st, (ast.Assign, ast.AnnAssign)):
            if st.value is None:
                return None
            v = self.ev(st.value, sc)
            targets = st.targets if isinstance(st, ast.Assign) else [st.target]
            for t in targets:
                self.bind(t, v, sc)
            return None
        if isinstance(st, ast.AugAssign):
            cur = self.ev(st.target, sc)
            v = self.ev(st.value, sc)
            if isinstance(cur, list) and isinstance(st.op, ast.Add) and isinstance(v, list):
                cur.extend(v)
                return None
            self.bind(st.target, self.binop(st.op, cur, v, st), sc)
            return None
        if isinstance(st, (ast.With, ast.AsyncWith)):
            return self.with_stmt(st, sc, module)
        if isinstance(st, (ast.For, ast.AsyncFor, ast.While)):
            return self.loop(st, sc)
        if isinstance(st, ast.If):
            return self.if_stmt(st, sc, module)
        if isinstance(st, ast.Try) or type(st).__name__ == "TryStar":
            if self.wf is not None and st.handlers:
                self.find(st.lineno, "try/except", APPROXIMATED,
                          "exception handlers around DAG/flow construction are not translated; the body was "
                          "converted as if it did not raise")
            for part in (st.body, st.orelse, st.finalbody):
                r = self.body(part, sc, module)
                if r is not None:
                    return r
            return None
        if isinstance(st, ast.Return):
            return Signal(self.ev(st.value, sc) if st.value is not None else None)
        if type(st).__name__ == "Match":
            self.dynamic_block("match", st.lineno, lambda: [self.body(c.body, sc) for c in st.cases])
        return None

    def bind(self, target, v, sc):
        if isinstance(target, ast.Name):
            sc.set(target.id, v)
        elif isinstance(target, (ast.Tuple, ast.List)):
            vals = v if isinstance(v, list) and len(v) == len(target.elts) else None
            for i, t in enumerate(target.elts):
                self.bind(t, vals[i] if vals is not None else Unknown("unpacked value", target.lineno), sc)
        elif isinstance(target, ast.Attribute):
            base = self.ev(target.value, sc)
            if isinstance(base, DagHandle) and target.attr in ("doc_md", "description", "doc"):
                if isinstance(v, str):
                    base.wf.description = textwrap.dedent(v).strip()
            elif isinstance(base, TaskRef) and target.attr == "trigger_rule" and self.wf is not None:
                self.apply_trigger_rule(base.task, v, target.lineno)
        elif isinstance(target, ast.Subscript):
            base = self.ev(target.value, sc)
            key = self.ev(target.slice, sc)
            if isinstance(base, dict) and isinstance(key, str):
                base[key] = v

    def apply_trigger_rule(self, t, v, ln, wf=None):
        pass

    def dynamic_block(self, kind, line, fn):
        created = []
        self.dynamic.append((kind, line, created))
        try:
            fn()
        finally:
            self.dynamic.pop()
        if created and self.wf is not None:
            what = {"for": "a for loop", "while": "a while loop", "if": "a condition", "match": "a match statement",
                    "comprehension": "a comprehension"}.get(kind, kind)
            self.find(line, kind, UNSUPPORTED,
                      "%s creates or wires tasks (%s); the converter does not unroll loops or evaluate conditions, "
                      "so each is a failing placeholder. Write the tasks out explicitly, or use a Nexus map "
                      "(dynamic expansion) for a run-time collection" % (what, ", ".join(created)),
                      "placeholders: " + ", ".join(created))
            if self.dynamic:
                self.dynamic[-1][2].extend(c for c in created if c not in self.dynamic[-1][2])
        return created

    def loop(self, st, sc):
        kind = "while" if isinstance(st, ast.While) else "for"

        def run():
            if kind == "for":
                it = self.ev(st.iter, sc)
                if isinstance(it, TaskRef) and self.wf is not None:
                    self.find(st.lineno, "for", UNSUPPORTED,
                              "iterates over the result of task %s, which only exists at run time; use a map "
                              "(Nexus dynamic expansion) instead" % it.task["id"])
                self.bind_loop_target(st.target, sc)
            else:
                self.ev(st.test, sc)
            self.body(st.body, sc)
            self.body(st.orelse, sc)
        self.dynamic_block(kind, st.lineno, run)
        return None

    def bind_loop_target(self, target, sc):
        if isinstance(target, ast.Name):
            sc.set(target.id, Unknown("loop variable %s" % target.id, target.lineno, pattern="N"))
        elif isinstance(target, (ast.Tuple, ast.List)):
            for t in target.elts:
                self.bind_loop_target(t, sc)

    def if_stmt(self, st, sc, module):
        t = st.test
        if (isinstance(t, ast.Compare) and isinstance(t.left, ast.Name) and t.left.id == "__name__"
                and len(t.comparators) == 1 and isinstance(t.comparators[0], ast.Constant)
                and t.comparators[0].value == "__main__"):
            self.main_calls.append((st.body, sc))
            return None
        cond = self.ev(t, sc)
        if is_const(cond):
            return self.body(st.body if cond else st.orelse, sc, module)
        if self.wf is not None and (contains_refs(cond) or self.mentions_task(t, sc)):
            self.find(st.lineno, "if", UNSUPPORTED,
                      "branches on a task result at run time; Nexus DAGs are static (no conditional tasks). "
                      "Move the decision into a task, or into the downstream task's code")

        def run():
            self.body(st.body, sc, module)
            self.body(st.orelse, sc, module)
        self.dynamic_block("if", st.lineno, run)
        return None

    def mentions_task(self, node, sc):
        for n in ast.walk(node):
            if isinstance(n, ast.Name) and isinstance(sc.lookup(n.id), TaskRef):
                return True
        return False

    def with_stmt(self, st, sc, module):
        return self.body(st.body, sc, module)

    # ── functions and decorators ────────────────────────────────────────────
    def kwargs_of(self, call, sc):
        out = {}
        for k in call.keywords:
            if k.arg is None:
                out["**"] = (Unknown("**kwargs expansion", k.value.lineno), k.value.lineno)
            else:
                out[k.arg] = (self.ev(k.value, sc), k.value.lineno)
        return out

    # ── expressions ─────────────────────────────────────────────────────────
    def ev(self, node, sc):
        if node is None:
            return None
        m = getattr(self, "ev_" + type(node).__name__, None)
        if m is None:
            return Unknown("%s expression" % type(node).__name__, getattr(node, "lineno", 0))
        return m(node, sc)

    def ev_Constant(self, node, sc):
        return node.value

    def ev_JoinedStr(self, node, sc):
        parts, pattern, known = [], [], True
        for v in node.values:
            if isinstance(v, ast.Constant):
                parts.append(str(v.value))
                pattern.append(str(v.value))
                continue
            x = self.ev(v.value, sc) if isinstance(v, ast.FormattedValue) else Unknown("f-string", node.lineno)
            if is_const(x) and not isinstance(x, (list, dict)) and not getattr(v, "format_spec", None):
                parts.append(str(x))
                pattern.append(str(x))
            else:
                known = False
                pattern.append(x.pattern if isinstance(x, Unknown) and x.pattern else "N")
        if known:
            return "".join(parts)
        return Unknown("an f-string with run-time values", node.lineno, pattern="".join(pattern))

    def ev_Name(self, node, sc):
        v = sc.lookup(node.id)
        if v is not MISSING:
            return v
        if node.id in self.imports:
            return Obj("module", [self.imports[node.id]], line=node.lineno)
        return Unknown("name %s is not defined in this file" % node.id, node.lineno)

    def ev_List(self, node, sc):
        out = []
        for e in node.elts:
            if isinstance(e, ast.Starred):
                inner = self.ev(e.value, sc)
                if isinstance(inner, list):
                    out.extend(inner)
                    continue
                return Unknown("a starred expression", node.lineno)
            out.append(self.ev(e, sc))
        return out

    ev_Tuple = ev_List
    ev_Set = ev_List

    def ev_Dict(self, node, sc):
        out = {}
        for k, v in zip(node.keys, node.values):
            if k is None:
                inner = self.ev(v, sc)
                if isinstance(inner, dict):
                    out.update(inner)
                    continue
                return Unknown("dict unpacking", node.lineno)
            kv = self.ev(k, sc)
            if not isinstance(kv, (str, int, float, bool)):
                return Unknown("a dict with computed keys", node.lineno)
            out[kv if isinstance(kv, str) else str(kv)] = self.ev(v, sc)
        return out

    def ev_Await(self, node, sc):
        return self.ev(node.value, sc)

    def ev_Lambda(self, node, sc):
        return Unknown("a lambda", node.lineno)

    def ev_IfExp(self, node, sc):
        t = self.ev(node.test, sc)
        if is_const(t):
            return self.ev(node.body if t else node.orelse, sc)
        return Unknown("a conditional expression on a run-time value", node.lineno)

    def ev_BoolOp(self, node, sc):
        vals = [self.ev(v, sc) for v in node.values]
        if all(is_const(v) for v in vals):
            r = vals[0]
            for v in vals[1:]:
                r = (r and v) if isinstance(node.op, ast.And) else (r or v)
            return r
        return Unknown("a boolean expression on run-time values", node.lineno)

    def ev_Compare(self, node, sc):
        left = self.ev(node.left, sc)
        rights = [self.ev(c, sc) for c in node.comparators]
        if not (is_const(left) and all(is_const(r) for r in rights)):
            return Unknown("a comparison of run-time values", node.lineno)
        ops = {ast.Eq: lambda a, b: a == b, ast.NotEq: lambda a, b: a != b, ast.Lt: lambda a, b: a < b,
               ast.LtE: lambda a, b: a <= b, ast.Gt: lambda a, b: a > b, ast.GtE: lambda a, b: a >= b,
               ast.In: lambda a, b: a in b, ast.NotIn: lambda a, b: a not in b,
               ast.Is: lambda a, b: a is b, ast.IsNot: lambda a, b: a is not b}
        cur = left
        try:
            for op, r in zip(node.ops, rights):
                if not ops[type(op)](cur, r):
                    return False
                cur = r
        except Exception:
            return Unknown("a comparison", node.lineno)
        return True

    def ev_UnaryOp(self, node, sc):
        v = self.ev(node.operand, sc)
        if isinstance(node.op, ast.USub) and isinstance(v, (int, float)) and not isinstance(v, bool):
            return -v
        if isinstance(node.op, ast.USub) and isinstance(v, Dur):
            return Dur(-v.seconds)
        if isinstance(node.op, ast.Not) and is_const(v):
            return not v
        return Unknown("a unary expression on a run-time value", node.lineno)

    def ev_Subscript(self, node, sc):
        base = self.ev(node.value, sc)
        key = self.ev(node.slice, sc)
        if isinstance(base, TaskRef) and isinstance(key, str):
            return TaskRef(base.task, key)
        if isinstance(base, (list, dict)) and is_const(key) and not isinstance(key, (list, dict)):
            try:
                return base[key]
            except Exception:
                return Unknown("an index out of range", node.lineno)
        return Unknown("a subscript of %s" % describe(base), node.lineno)

    def ev_Index(self, node, sc):  # Python < 3.9
        return self.ev(node.value, sc)

    def ev_Attribute(self, node, sc):
        q = self.qual(node)
        if q:
            known = self.known_attribute(q, node)
            if known is not MISSING:
                return known
        base = self.ev(node.value, sc)
        a = node.attr
        if isinstance(base, TaskRef) and a == "output":
            return TaskRef(base.task, None)
        if isinstance(base, DagHandle):
            if a == "dag_id":
                return base.wf.source_id
            if a == "tasks":
                return Unknown("dag.tasks (the task list Airflow builds while parsing)", node.lineno)
            return Unknown("dag.%s" % a, node.lineno)
        if isinstance(base, Obj):
            if base.cls == "module":
                return Obj("module", [base.args[0] + "." + a], line=node.lineno)
            if a in base.kwargs:
                return base.kwargs[a]
        if isinstance(base, (FuncRef, Override, Partial)):
            return ("method", base, a)
        return Unknown("attribute %s of %s" % (a, describe(base)), node.lineno)

    def known_attribute(self, q, node):
        return MISSING

    def ev_BinOp(self, node, sc):
        if isinstance(node.op, (ast.RShift, ast.LShift)):
            left = self.ev(node.left, sc)
            right = self.ev(node.right, sc)
            if isinstance(node.op, ast.RShift):
                self.depend(left, right, node.lineno)
            else:
                self.depend(right, left, node.lineno)
            return right
        left = self.ev(node.left, sc)
        right = self.ev(node.right, sc)
        return self.binop(node.op, left, right, node)

    def binop(self, op, left, right, node):
        line = getattr(node, "lineno", 0)
        if isinstance(left, Obj) and isinstance(right, Obj) and left.cls == right.cls == "cache_policy" \
                and isinstance(op, ast.Add):
            return Obj("cache_policy", left.args + right.args, line=line)
        if isinstance(left, Dur) and isinstance(right, Dur):
            if isinstance(op, ast.Add):
                return Dur(left.seconds + right.seconds)
            if isinstance(op, ast.Sub):
                return Dur(left.seconds - right.seconds)
        if isinstance(left, Dur) and isinstance(right, (int, float)) and isinstance(op, ast.Mult):
            return Dur(left.seconds * right)
        if isinstance(right, Dur) and isinstance(left, (int, float)) and isinstance(op, ast.Mult):
            return Dur(right.seconds * left)
        if is_const(left) and is_const(right):
            try:
                if isinstance(op, ast.Add):
                    return left + right
                if isinstance(op, ast.Sub):
                    return left - right
                if isinstance(op, ast.Mult):
                    return left * right
                if isinstance(op, ast.Div):
                    return left / right
                if isinstance(op, ast.FloorDiv):
                    return left // right
                if isinstance(op, ast.Mod) and isinstance(left, (int, float)):
                    return left % right
                if isinstance(op, ast.Pow) and abs(right) < 64:
                    return left ** right
            except Exception:
                pass
        return Unknown("arithmetic on run-time values", line)

    def ev_ListComp(self, node, sc):
        # [f.result() for f in futures] / [f for f in futures] is a Prefect
        # idiom for "the results": the same reference as `futures`.
        if len(node.generators) == 1 and isinstance(node.generators[0].target, ast.Name) \
                and not node.generators[0].ifs:
            g = node.generators[0]
            name = g.target.id
            elt = node.elt
            simple = (isinstance(elt, ast.Name) and elt.id == name) or (
                isinstance(elt, ast.Call) and isinstance(elt.func, ast.Attribute) and elt.func.attr == "result"
                and isinstance(elt.func.value, ast.Name) and elt.func.value.id == name)
            if simple:
                it = self.ev(g.iter, sc)
                if isinstance(it, TaskRef):
                    return it
        inner = Scope(sc)

        def run():
            for g in node.generators:
                self.ev(g.iter, inner)
                self.bind_loop_target(g.target, inner)
            self.ev(node.elt, inner)
        created = self.dynamic_block("comprehension", node.lineno, run)
        if created and self.wf is not None:
            return [TaskRef(self.wf.tasks[c]) for c in created if c in self.wf.tasks]
        return Unknown("a comprehension", node.lineno)

    ev_GeneratorExp = ev_ListComp
    ev_SetComp = ev_ListComp

    def ev_Call(self, node, sc):
        f = node.func
        if isinstance(f, ast.Attribute):
            q = self.qual(f)
            if q and self.is_known_function(q, sc, f):
                return self.known_function(q, node, sc)
            base = self.ev(f.value, sc)
            return self.method_call(base, f.attr, node, sc)
        if isinstance(f, ast.Name):
            v = sc.lookup(f.id)
            if v is not MISSING and not (isinstance(v, Obj) and v.cls == "module"):
                return self.call_value(v, node, sc)
            return self.known_function(self.qual(f), node, sc)
        return self.call_value(self.ev(f, sc), node, sc)

    def is_known_function(self, q, sc, f):
        head = f
        while isinstance(head, ast.Attribute):
            head = head.value
        if isinstance(head, ast.Name) and sc.lookup(head.id) is not MISSING:
            v = sc.lookup(head.id)
            if not (isinstance(v, Obj) and v.cls == "module"):
                return False  # a local value's method, not a module function
        return isinstance(head, ast.Name) and head.id in self.imports

    def args_of(self, node, sc):
        args = []
        for a in node.args:
            if isinstance(a, ast.Starred):
                inner = self.ev(a.value, sc)
                if isinstance(inner, list):
                    args.extend(inner)
                else:
                    args.append(Unknown("*args expansion", a.lineno))
                continue
            args.append(self.ev(a, sc))
        kwargs, lines = {}, {}
        for k in node.keywords:
            if k.arg is None:
                inner = self.ev(k.value, sc)
                if isinstance(inner, dict):
                    kwargs.update(inner)
                    continue
                kwargs["**"] = Unknown("**kwargs expansion", k.value.lineno)
                continue
            kwargs[k.arg] = self.ev(k.value, sc)
            lines[k.arg] = k.value.lineno
        return args, kwargs, lines

    def generic_function(self, q, node, sc, args, kwargs):
        """Pure functions every source may use. MISSING when q is not one."""
        line = node.lineno
        parts = (q or "").split(".")
        short = parts[-1]
        if short in ("timedelta", "duration") and parts[0] in ("datetime", "pendulum", "timedelta"):
            names = ["days", "seconds", "microseconds", "milliseconds", "minutes", "hours", "weeks"]
            mult = {"days": 86400, "seconds": 1, "microseconds": 1e-6, "milliseconds": 1e-3, "minutes": 60,
                    "hours": 3600, "weeks": 604800}
            total = 0.0
            for i, a in enumerate(args):
                if i >= len(names) or not isinstance(a, (int, float)) or isinstance(a, bool):
                    return Unknown("a timedelta of run-time values", line)
                total += a * mult[names[i]]
            for k, a in kwargs.items():
                if k not in mult or not isinstance(a, (int, float)) or isinstance(a, bool):
                    return Unknown("timedelta(%s=...)" % k, line)
                total += a * mult[k]
            return Dur(total)
        if short == "datetime" and parts[0] in ("datetime", "pendulum"):
            vals = [0, 1, 1, 0, 0, 0]
            for i, a in enumerate(args[:6]):
                if not isinstance(a, int):
                    return Unknown("a datetime of run-time values", line)
                vals[i] = a
            for i, k in enumerate(("year", "month", "day", "hour", "minute", "second")):
                if isinstance(kwargs.get(k), int):
                    vals[i] = kwargs[k]
            tz = kwargs.get("tz", kwargs.get("tzinfo"))
            tzname = tz.name if isinstance(tz, TZ) else tz if isinstance(tz, str) else None
            return DT("%04d-%02d-%02dT%02d:%02d:%02d" % tuple(vals), tzname)
        if short in ("timezone", "ZoneInfo") and parts[0] in ("pendulum", "zoneinfo"):
            if args and isinstance(args[0], str):
                return TZ(args[0])
            return Unknown("a time zone", line)
        if short == "dedent" and parts[0] == "textwrap":
            if args and isinstance(args[0], str):
                return textwrap.dedent(args[0])
            return Unknown("dedent() of a run-time value", line)
        if parts[0] == "json" and short in ("dumps", "loads") and args and is_const(args[0]):
            try:
                return json.dumps(args[0]) if short == "dumps" else json.loads(args[0])
            except Exception:
                return Unknown("json", line)
        if short in ("now", "utcnow", "today", "yesterday", "tomorrow") and parts[0] in ("datetime", "pendulum"):
            return Unknown("%s() (evaluated when the file is parsed)" % q, line)
        if q in ("os.environ.get", "os.getenv") or (q or "").startswith("os.environ"):
            return Unknown("an environment variable of the parsing process (%s)" % q, line)
        if q == "asyncio.gather":
            return list(args)
        if q == (short if q else None):
            return self.builtin(short, args, kwargs, line)
        return MISSING

    def builtin(self, name, args, kwargs, line):
        consts = all(is_const(a) for a in args) and all(is_const(v) for v in kwargs.values())
        if name == "range" and consts and all(isinstance(a, int) for a in args) and 1 <= len(args) <= 3:
            r = list(range(*args))
            return r if len(r) <= 100000 else Unknown("a large range", line)
        if name in ("list", "tuple") and len(args) == 1 and isinstance(args[0], (list, TaskRef)):
            return args[0] if isinstance(args[0], TaskRef) else list(args[0])
        if name == "print" and self.wf is not None and self.mode == "prefect":
            self.find(line, "print", APPROXIMATED,
                      "print() in the flow body does not run in Nexus: there is no flow-body process")
            return None
        if not consts:
            return MISSING
        try:
            if name == "len":
                return len(args[0])
            if name in ("str", "int", "float", "bool", "abs", "round", "min", "max", "sum", "sorted"):
                return {"str": str, "int": int, "float": float, "bool": bool, "abs": abs, "round": round,
                        "min": min, "max": max, "sum": sum, "sorted": sorted}[name](*args)
            if name == "dict":
                d = dict(args[0]) if args else {}
                d.update(kwargs)
                return d
        except Exception:
            return Unknown(name, line)
        return MISSING

    def call_value(self, v, node, sc):
        return Unknown("a call of %s" % describe(v), node.lineno)

    def method_call(self, base, attr, node, sc):
        line = node.lineno
        if isinstance(base, (TaskRef, GroupRef, list)) and attr in ("set_downstream", "set_upstream"):
            args, _, _ = self.args_of(node, sc)
            other = args[0] if args else None
            if attr == "set_downstream":
                self.depend(base, other, line)
            else:
                self.depend(other, base, line)
            return None
        if isinstance(base, list) and attr in ("append", "extend"):
            args, _, _ = self.args_of(node, sc)
            if args:
                if attr == "append":
                    base.append(args[0])
                elif isinstance(args[0], list):
                    base.extend(args[0])
            return None
        if isinstance(base, str) and attr in ("format", "strip", "lower", "upper", "replace", "join", "split",
                                              "lstrip", "rstrip", "title"):
            args, kwargs, _ = self.args_of(node, sc)
            if all(is_const(a) for a in args) and all(is_const(v) for v in kwargs.values()):
                try:
                    return getattr(base, attr)(*args, **kwargs)
                except Exception:
                    pass
            return Unknown("a string method on run-time values", line)
        if isinstance(base, dict) and attr == "get":
            args, _, _ = self.args_of(node, sc)
            if args and isinstance(args[0], str):
                return base.get(args[0], args[1] if len(args) > 1 else None)
        if isinstance(base, Obj) and base.cls == "module":
            return self.known_function(base.args[0] + "." + attr, node, sc)
        if isinstance(base, DagHandle):
            return None
        return Unknown("method %s of %s" % (attr, describe(base)), line)

    # ── dependencies ────────────────────────────────────────────────────────
    def as_ids(self, v, which):
        if isinstance(v, TaskRef):
            return [v.task["id"]]
        if isinstance(v, GroupRef):
            return v.wf.leaves(v.gid) if which == "leaves" else v.wf.roots(v.gid)
        if isinstance(v, list):
            out = []
            for x in v:
                if isinstance(x, list):
                    return None
                ids = self.as_ids(x, which)
                if ids is None:
                    return None
                out.extend(ids)
            return out
        return None

    def wf_of(self, *vals):
        """The workflow a task value belongs to: a `>>` at module level, after
        the `with DAG` block, still wires that DAG's tasks."""
        for v in vals:
            for r in refs_in(v if not isinstance(v, GroupRef) else []):
                if r.task.get("_wf") is not None:
                    return r.task["_wf"]
            if isinstance(v, GroupRef):
                return v.wf
        return None

    def depend(self, left, right, line):
        if self.wf is None:
            wf = self.wf_of(left, right)
            if wf is None:
                return
            self.wf = wf
            try:
                return self.depend(left, right, line)
            finally:
                self.wf = None
        if isinstance(left, list) and isinstance(right, list):
            self.find(line, ">>", UNSUPPORTED,
                      "a list on both sides of >> (or <<) is rejected by Airflow itself (TypeError); use "
                      "cross_downstream() for all pairs or chain() for pairwise dependencies")
            return
        a = self.as_ids(left, "leaves")
        b = self.as_ids(right, "roots")
        if a is None or b is None:
            bad = left if a is None else right
            self.find(line, "dependency", UNSUPPORTED,
                      "a dependency operand is not statically resolvable (%s); the edge was not created"
                      % describe(bad))
            return
        for x in a:
            for y in b:
                self.wf.add_edge(x, y, line)

    # ── tasks ───────────────────────────────────────────────────────────────
    def full_id(self, tid):
        return ".".join(self.groups + [tid]) if self.groups else tid

    def register(self, t):
        wf = self.wf
        t["_wf"] = wf
        wf.tasks[t["id"]] = t
        for i in range(len(self.groups)):
            gid = ".".join(self.groups[: i + 1])
            wf.groups.setdefault(gid, []).append(t["id"])
        if self.dynamic:
            self.dynamic[-1][2].append(t["id"])
        return TaskRef(t)

    def placeholder(self, tid, line, reason):
        t = new_task(tid, line, "placeholder")
        t["reason"] = reason
        return t

    def unsupported_task(self, name, line, reason):
        if self.wf is None:
            return Unknown(reason, line)
        tid = self.wf.unique_id(self.full_id(name))
        t = self.placeholder(tid, line, reason)
        self.wf.find(line, "task", UNSUPPORTED, "%s: %s" % (tid, reason), "tasks[%s] (placeholder)" % tid)
        return self.register(t)

    def wire_refs(self, t, args, kwargs, line):
        for v in list(args) + list(kwargs.values()):
            for r in refs_in(v):
                self.wf.add_edge(r.task["id"], t["id"], line)

    def python_task(self, tid, line, fn):
        wf = self.wf
        t = new_task(tid, line, "python")
        t["module"] = self.module_name
        t["callable"] = fn.name
        t["outputs"]["result"] = {"type": "json"}
        nested = ("it is defined inside the DAG/flow function, so move it to module level where it is importable; "
                  if fn.nested else "")
        wf.find(fn.node.lineno, "python task", APPROXIMATED,
                "%s: runs %s.%s through `python -m nexus.entrypoint`; %sdecorate it with "
                "@nexus.task(task_ref=\"%s.%s\") -- inputs arrive as keyword arguments and the return value is "
                "output `result`" % (tid, self.module_name, fn.name, nested, self.module_name, fn.name),
                "tasks[%s].runtime" % tid)
        return t

    def bind_call(self, t, f, args, kwargs, line, context=()):
        a = f.node.args
        pos = [p.arg for p in (getattr(a, "posonlyargs", []) + a.args)]
        names = pos + [p.arg for p in a.kwonlyargs]
        bound = {}
        for i, v in enumerate(args):
            if i < len(pos):
                bound[pos[i]] = v
            else:
                self.wf.find(line, "call", UNSUPPORTED,
                             "%s: extra positional arguments cannot be bound to named inputs" % t["id"])
        for k, v in kwargs.items():
            if k == "**":
                self.wf.find(line, "**kwargs", UNSUPPORTED, "%s: **kwargs expansion is not translated" % t["id"])
                continue
            if k in names or a.kwarg is not None:
                bound[k] = v
            else:
                self.wf.find(line, "call", UNSUPPORTED, "%s: unexpected argument %s" % (t["id"], k))
        for name, v in bound.items():
            self.bind_input(t, name, v, line)
        # Parameters left unbound keep their literal defaults, so the handler's
        # interface stays visible in the spec.
        defaults = {}
        if a.defaults:
            for p, d in zip(pos[-len(a.defaults):], a.defaults):
                defaults[p] = d
        for p, d in zip(a.kwonlyargs, a.kw_defaults):
            if d is not None:
                defaults[p.arg] = d
        for n in names:
            if n in bound or n in context:
                continue
            if n in defaults:
                dv = self.ev(defaults[n], self.module_scope)
                if is_const(dv):
                    t["inputs"][n] = {"type": "json", "default_json": json.dumps(dv, sort_keys=True)}
        return bound

    def bind_input(self, t, name, v, line):
        wf = self.wf
        if isinstance(v, Obj) and v.cls == "unmapped":
            v = v.args[0] if v.args else None
        if isinstance(v, TaskRef):
            up = v.task
            out = v.key or "result"
            if v.key:
                wf.find(line, "output key", APPROXIMATED,
                        "%s: %s[%r] is declared as a separate output %s of %s; a Nexus handler with several outputs "
                        "returns a mapping holding each of them" % (t["id"], up["id"], v.key, v.key, up["id"]),
                        "outputs.%s" % v.key)
            up["outputs"][out] = {"type": "json"}
            if up.get("kind") in ("shell", "container") and out == "result":
                if not up.get("stdout_output"):
                    up["stdout_output"] = "result"
                    wf.find(line, "stdout result", APPROXIMATED,
                            "%s: the value of %s is its whole stdout (Airflow's XCom keeps only the last line)"
                            % (t["id"], up["id"]), "runtime.stdout_output")
            t["inputs"][name] = {"type": "json", "required": True}
            wf.add_edge(up["id"], t["id"], line, {name: out})
            wf.find(line, "data dependency", TRANSLATED, "%s(%s=%s)" % (t["id"], name, up["id"]),
                    "edge %s -> %s {%s: %s}" % (up["id"], t["id"], name, out))
        elif isinstance(v, ParamRef):
            t["inputs"][name] = {"type": "json", "required": True}
            t["input_params"][name] = v.name
        elif is_const(v) or isinstance(v, (Dur, DT)):
            val = v.seconds if isinstance(v, Dur) else v.iso if isinstance(v, DT) else v
            if isinstance(v, (Dur, DT)):
                wf.find(line, "argument", APPROXIMATED, "%s: %s passed as %s" % (t["id"], name, json.dumps(val)))
            t["inputs"][name] = {"type": "json", "required": True}
            t["input_literals"][name] = json.dumps(val, sort_keys=True)
        elif contains_refs(v):
            wf.find(line, "argument", UNSUPPORTED,
                    "%s: argument %s is a collection of task outputs; a Nexus input binds exactly one upstream "
                    "output. Ordering edges were kept" % (t["id"], name))
            for r in refs_in(v):
                wf.add_edge(r.task["id"], t["id"], line)
        else:
            wf.find(line, "argument", UNSUPPORTED,
                    "%s: argument %s is not statically known (%s)" % (t["id"], name, describe(v)))

    def make_map(self, t, name, src, line, what):
        wf = self.wf
        t["inputs"][name] = {"type": "json", "required": True}
        t["input_literals"].pop(name, None)
        if isinstance(src, list) and is_const(src):
            n = max(1, len(src))
            t["map"] = {"item_input": name, "literal_items_json": json.dumps(src, sort_keys=True), "max_fanout": n}
            wf.find(line, what, APPROXIMATED,
                    "%s: mapping over a literal list of %d: a generated task emits the list and %s maps over it "
                    "(max_fanout %d)" % (t["id"], len(src), t["id"], n), "map")
        elif isinstance(src, TaskRef):
            out = src.key or "result"
            src.task["outputs"][out] = {"type": "json"}
            if src.task.get("kind") in ("shell", "container") and out == "result":
                src.task["stdout_output"] = "result"
            t["map"] = {"over": src.task["id"], "over_output": out, "item_input": name, "max_fanout": 0}
            wf.add_edge(src.task["id"], t["id"], line)
            wf.find(line, what, TRANSLATED, "%s: map over the output of %s" % (t["id"], src.task["id"]),
                    "map.over_node_id=%s" % src.task["id"])
        elif isinstance(src, ParamRef):
            if src.name in wf.params:
                wf.params[src.name]["type"] = "json"
            t["map"] = {"over_param": src.name, "item_input": name, "max_fanout": 0}
            wf.find(line, what, APPROXIMATED,
                    "%s: map over param %s: a generated task emits the param (declared json) for the map"
                    % (t["id"], src.name), "map")
        else:
            wf.find(line, what, UNSUPPORTED, "%s: the mapped collection is not statically known (%s)"
                    % (t["id"], describe(src)))

    def declare_param(self, wf, name, default, line, hinted=None):
        spec = {"line": line}
        if isinstance(default, Obj) and default.cls == "Param":
            d = default.args[0] if default.args else default.kwargs.get("default", MISSING)
            typ = default.kwargs.get("type")
            tmap = {"string": "string", "integer": "int", "number": "float", "boolean": "bool",
                    "array": "json", "object": "json"}
            t = tmap.get(typ) if isinstance(typ, str) else ("json" if typ is not None else None)
            if d is MISSING:
                spec.update({"type": t or "json", "required": True})
            elif is_const(d):
                spec.update({"type": (t or value_type(d)) if d is not None else "json",
                             "default_json": json.dumps(d, sort_keys=True)})
            else:
                spec["type"] = t or "json"
                wf.find(line, "Param", UNSUPPORTED, "the default of param %s is not a literal" % name)
            extra = sorted(k for k in default.kwargs if k not in ("type", "default", "description", "title"))
            if extra:
                wf.find(line, "Param", APPROXIMATED,
                        "param %s: its JSON-schema constraints (%s) are not enforced by Nexus params"
                        % (name, ", ".join(extra)))
        elif default is MISSING:
            spec.update({"type": hinted or "json", "required": True})
        elif is_const(default):
            t = hinted or value_type(default)
            if default is None:
                t = "json"
            spec.update({"type": t, "default_json": json.dumps(default, sort_keys=True)})
        elif isinstance(default, (Dur, DT)):
            val = default.seconds if isinstance(default, Dur) else default.iso
            spec.update({"type": "json", "default_json": json.dumps(val)})
            wf.find(line, "param", APPROXIMATED, "param %s's default was converted to %s" % (name, json.dumps(val)))
        else:
            spec["type"] = hinted or "json"
            wf.find(line, "param", UNSUPPORTED,
                    "the default of param %s is not a literal (%s)" % (name, describe(default)))
        wf.params[name] = spec
        wf.find(line, "param", TRANSLATED, "param %s" % name, "params.%s (%s)" % (name, spec["type"]))

    def signature_params(self, fnode, sc):
        """[(arg node, default value or MISSING)] for a function's parameters."""
        a = fnode.args
        pos = getattr(a, "posonlyargs", []) + a.args
        defaults = {}
        if a.defaults:
            for p, d in zip(pos[-len(a.defaults):], a.defaults):
                defaults[p.arg] = d
        for p, d in zip(a.kwonlyargs, a.kw_defaults):
            if d is not None:
                defaults[p.arg] = d
        out = []
        for p in pos + a.kwonlyargs:
            d = defaults.get(p.arg)
            out.append((p, self.ev(d, sc) if d is not None else MISSING))
        return out
