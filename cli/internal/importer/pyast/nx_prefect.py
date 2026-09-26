# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""Prefect 2/3 semantics for the analyzer: @flow, @task, futures, .submit,
.map, wait_for, subflows, serve()/deploy() schedules, task options."""

from __future__ import annotations

import ast

from nx_core import Core
from nx_values import (APPROXIMATED, MISSING, TRANSLATED, UNSUPPORTED, FuncRef, Obj, Override, ParamRef, Scope,
                    TaskRef, Unknown, Workflow, annotation_type, describe, is_const, ms, seconds_of)

TASK_OPTS = {"name", "retries", "retry_delay_seconds", "retry_jitter_factor", "timeout_seconds", "cache_key_fn",
             "cache_policy", "cache_expiration", "tags", "log_prints", "task_run_name", "description", "version",
             "persist_result", "refresh_cache"}


class Prefect(Core):
    def classify_function(self, node, sc, nested):
        for d in node.decorator_list:
            call = d if isinstance(d, ast.Call) else None
            q = self.qual(call.func if call else d) or ""
            parts = q.split(".")
            opts = self.kwargs_of(call, sc) if call else {}
            if parts[-1] == "flow":
                return FuncRef(node, "flow", opts=opts, deco_line=d.lineno, nested=nested)
            if parts[-1] == "task":
                return FuncRef(node, "task", opts=opts, deco_line=d.lineno, nested=nested)
        return FuncRef(node, "plain", nested=nested)

    def known_attribute(self, q, node):
        parts = q.split(".")
        if "cache_policies" in parts:
            return Obj("cache_policy", [parts[-1]], line=node.lineno)
        if parts[-1] == "task_input_hash":
            return Obj("task_input_hash", line=node.lineno)
        return MISSING

    def ev_Name(self, node, sc):
        v = sc.lookup(node.id)
        if v is MISSING and node.id in self.imports:
            q = self.imports[node.id]
            parts = q.split(".")
            if "cache_policies" in parts or parts[-1] in ("INPUTS", "TASK_SOURCE", "RUN_ID", "NO_CACHE", "NONE",
                                                          "DEFAULT", "FLOW_PARAMETERS"):
                return Obj("cache_policy", [parts[-1]], line=node.lineno)
            if parts[-1] == "task_input_hash":
                return Obj("task_input_hash", line=node.lineno)
        return Core.ev_Name(self, node, sc)

    def with_stmt(self, st, sc, module):
        for item in st.items:
            v = self.ev(item.context_expr, sc)
            if self.wf is not None:
                if isinstance(v, Obj) and v.cls == "tags":
                    self.find(st.lineno, "tags", APPROXIMATED, "run tags have no Nexus equivalent; dropped")
                else:
                    self.find(st.lineno, "with", APPROXIMATED,
                              "context manager %s is not translated; its body was converted without it"
                              % (self.qual(item.context_expr.func) if isinstance(item.context_expr, ast.Call)
                                 else "expression"))
            if item.optional_vars is not None:
                self.bind(item.optional_vars, Unknown("a context manager's value", st.lineno), sc)
        return self.body(st.body, sc, module)

    def known_function(self, q, node, sc):
        line = node.lineno
        q = q or ""
        short = q.split(".")[-1]
        args, kwargs, lines = self.args_of(node, sc)
        g = self.generic_function(q, node, sc, args, kwargs)
        if g is not MISSING:
            return g
        if short == "unmapped":
            return Obj("unmapped", args, kwargs, line)
        if short == "quote":
            return args[0] if args else None
        if short == "exponential_backoff":
            return Obj("exponential_backoff", args, kwargs, line)
        if short in ("Cron", "Interval", "RRule", "CronSchedule", "IntervalSchedule", "RRuleSchedule"):
            return Obj(short.replace("Schedule", ""), args, kwargs, line)
        if short == "tags":
            return Obj("tags", args, kwargs, line)
        if short in ("get_run_logger", "get_logger"):
            return Obj("logger", line=line)
        if short == "serve":
            for a in args:
                if isinstance(a, Obj) and a.cls == "deployment":
                    pass
            return None
        if short == "concurrency" or short == "rate_limit":
            return Obj(short, args, kwargs, line)
        if self.wf is not None and q.startswith("prefect"):
            self.find(line, short, UNSUPPORTED, "%s() in the flow body is not translated" % q)
        return Unknown("a call to %s()" % (q or "an expression"), line)

    def call_value(self, v, node, sc):
        line = node.lineno
        if isinstance(v, tuple) and v and v[0] == "method":
            return self.method_call(v[1], v[2], node, sc)
        if isinstance(v, FuncRef):
            args, kwargs, lines = self.args_of(node, sc)
            if v.kind == "task":
                return self.call_task(v, args, kwargs, line, {})
            if v.kind == "flow":
                if self.wf is None:
                    return None  # a local run in __main__
                ref = self.unsupported_task(v.name, line,
                                            "calls flow %s as a subflow; Nexus has no nested workflows. The subflow "
                                            "is converted as a workflow of its own: inline its tasks here, or start "
                                            "it from a task with `nexusctl run create`" % v.name)
                self.wire_refs(ref.task, args, kwargs, line)
                return ref
            if self.wf is not None:
                if any(isinstance(x, TaskRef) for x in list(args) + list(kwargs.values())):
                    self.find(line, "call", UNSUPPORTED,
                              "%s() is a plain function applied to task results in the flow body; Nexus has no "
                              "process that runs code between tasks. Make it a task" % v.name)
                else:
                    self.find(line, "call", APPROXIMATED,
                              "%s() runs in the flow body, not in a task; Nexus has no flow-body process, so it is "
                              "not run. Move it into a task if it matters" % v.name)
            return Unknown("the result of %s()" % v.name, line)
        if isinstance(v, Override):
            args, kwargs, _ = self.args_of(node, sc)
            return self.call_task(v.func, args, kwargs, line, v.opts)
        if isinstance(v, Obj) and v.cls == "logger":
            return None
        return Unknown("a call of %s" % describe(v), line)

    def method_call(self, base, attr, node, sc):
        line = node.lineno
        if isinstance(base, tuple) and base and base[0] == "method":
            base = self.method_call(base[1], base[2], node, sc)
        if isinstance(base, (FuncRef, Override)) and (base.kind if isinstance(base, FuncRef) else "task") == "task":
            f = base if isinstance(base, FuncRef) else base.func
            opts = {} if isinstance(base, FuncRef) else base.opts
            args, kwargs, _ = self.args_of(node, sc)
            if attr == "with_options":
                merged = dict(opts)
                merged.update(kwargs)
                return Override(f, merged)
            if attr in ("submit", "delay"):
                return self.call_task(f, args, kwargs, line, opts)
            if attr == "map":
                return self.prefect_map(f, args, kwargs, line, opts)
            if attr == "fn":
                return Unknown("the undecorated function", line)
        if isinstance(base, FuncRef) and base.kind == "flow":
            args, kwargs, _ = self.args_of(node, sc)
            if attr in ("serve", "deploy", "to_deployment"):
                self.serve_call(base, attr, kwargs, line)
                return Obj("deployment", line=line)
            if attr == "from_source":
                return Obj("flow_source", [base], line=line)
            if attr == "with_options":
                return base
            return None
        if isinstance(base, Obj) and base.cls == "flow_source" and attr == "deploy":
            _, kwargs, _ = self.args_of(node, sc)
            self.serve_call(base.args[0], "deploy", kwargs, line)
            return None
        if isinstance(base, TaskRef):
            if attr == "result":
                return base
            if attr == "wait":
                return None
            return Unknown("method %s of a future" % attr, line)
        if isinstance(base, Obj) and base.cls == "logger":
            if self.wf is not None:
                self.find(line, "logging", APPROXIMATED,
                          "a log call in the flow body does not run in Nexus (there is no flow-body process); task "
                          "logs are captured per attempt")
            return None
        return Core.method_call(self, base, attr, node, sc)

    # ── flows ───────────────────────────────────────────────────────────────
    def finish(self):
        for f in self.funcs.values():
            if f.kind != "flow":
                continue
            opts = {k: v[0] for k, v in f.opts.items() if k != "**"}
            lines = {k: v[1] for k, v in f.opts.items() if k != "**"}
            name = opts.get("name")
            wf = Workflow(name if isinstance(name, str) else f.name, self.file, f.deco_line)
            wf.function = f.name
            self.workflows.append(wf)
            wf.fail_fast = True
            wf.find(f.deco_line, "@flow", APPROXIMATED,
                    "a task that raises fails the Prefect flow; fail_fast=true stops new work the same way (tasks "
                    "already running finish)", "fail_fast")
            doc = ast.get_docstring(f.node)
            if isinstance(opts.get("description"), str):
                wf.description = opts["description"]
            elif doc:
                wf.description = doc.strip()
            self.flow_opts(wf, opts, lines, f.deco_line)
            self.wf = wf
            try:
                inner = Scope(self.module_scope)
                for p, dv in self.signature_params(f.node, self.module_scope):
                    self.declare_param(wf, p.arg, dv, p.lineno, annotation_type(p.annotation))
                    inner.set(p.arg, ParamRef(p.arg))
                self.body(f.node.body, inner)
                if not wf.tasks:
                    self.flow_as_task(wf, f)
            finally:
                self.wf = None
        for stmts, sc in self.main_calls:
            self.body(stmts, sc)

    def flow_as_task(self, wf, f):
        """A flow that calls no task is plain Python: it becomes ONE task that
        runs the flow function, with the flow's parameters as its inputs."""
        wf.findings = [x for x in wf.findings if x["construct"] not in ("print", "logging", "call", "@flow")]
        t = self.python_task(f.name, f.deco_line, f)
        for p in wf.params:
            t["inputs"][p] = {"type": "json", "required": True}
            t["input_params"][p] = p
        t["retry"] = {"max_attempts": 1}
        self.register(t)
        wf.fail_fast = None
        wf.find(f.deco_line, "@flow", APPROXIMATED,
                "flow %s calls no task, so the whole flow body becomes one task (its parameters are the task's "
                "inputs)" % f.name, "tasks[%s]" % f.name)

    def flow_opts(self, wf, opts, lines, line):
        for k, v in opts.items():
            ln = lines.get(k, line)
            if k in ("name", "description", "log_prints", "validate_parameters", "flow_run_name",
                     "retry_delay_seconds"):
                continue
            if k == "version":
                if isinstance(v, str):
                    wf.labels["version"] = v
            elif k == "timeout_seconds":
                s = seconds_of(v)
                if s is not None:
                    wf.run_timeout_ms = ms(s)
                    wf.find(ln, k, TRANSLATED, "flow timeout %gs" % s, "run_timeout_ms")
            elif k == "retries":
                if v:
                    wf.find(ln, k, UNSUPPORTED,
                            "flow-level retries re-run the whole flow; Nexus retries tasks, not runs (retry a failed "
                            "run with `nexusctl run retry`)")
            elif k in ("task_runner", "persist_result", "result_storage", "result_serializer",
                       "cache_result_in_memory"):
                wf.find(ln, k, APPROXIMATED, "%s is an execution setting Nexus decides for itself; ignored" % k)
            elif k.startswith("on_"):
                wf.find(ln, k, UNSUPPORTED, "state hooks (%s) run Prefect code on state changes; not translated" % k)
            else:
                wf.find(ln, k, UNSUPPORTED, "flow option %s is not translated" % k)

    # ── tasks ───────────────────────────────────────────────────────────────
    def call_task(self, f, args, kwargs, line, overrides):
        wf = self.wf
        if wf is None:
            return Unknown("a task called outside a flow", line)
        opts = {k: v[0] for k, v in f.opts.items() if k != "**"}
        olines = {k: v[1] for k, v in f.opts.items() if k != "**"}
        opts.update(overrides or {})
        kwargs = dict(kwargs)
        wait_for = kwargs.pop("wait_for", None)
        if kwargs.pop("return_state", None):
            wf.find(line, "return_state", UNSUPPORTED, "return_state=True inspects task states in the flow body")
        tid = wf.unique_id(f.name)
        if self.dynamic:
            kind, dline, _ = self.dynamic[-1]
            t = self.placeholder(tid, line, "a task call inside a %s at line %d; not unrolled" % (kind, dline))
            ref = self.register(t)
            self.wire_refs(t, args, kwargs, line)
            return ref
        t = self.python_task(tid, line, f)
        if f.node.__class__.__name__ == "AsyncFunctionDef":
            wf.find(f.node.lineno, "async task", APPROXIMATED,
                    "%s is a coroutine; nexus.entrypoint calls handlers synchronously, so wrap it with asyncio.run"
                    % f.name)
        self.bind_call(t, f, args, kwargs, line)
        self.task_opts(wf, t, opts, olines, f)
        ref = self.register(t)
        if wait_for is not None:
            items = wait_for if isinstance(wait_for, list) else [wait_for]
            for w in items:
                ids = self.as_ids(w, "leaves")
                if ids is None:
                    wf.find(line, "wait_for", UNSUPPORTED, "%s: a wait_for item is not a task (%s)"
                            % (tid, describe(w)))
                    continue
                for i in ids:
                    wf.add_edge(i, tid, line)
            wf.find(line, "wait_for", TRANSLATED, "%s: wait_for" % tid, "edges (ordering only)")
        return ref

    def task_opts(self, wf, t, opts, lines, f):
        tid, line = t["id"], f.deco_line
        retries = opts.get("retries", 0)
        retry = {"max_attempts": 1}
        if isinstance(retries, int) and retries > 0:
            retry["max_attempts"] = min(retries, 99) + 1
            wf.find(lines.get("retries", line), "retries", TRANSLATED, "%s: retries=%d" % (tid, retries),
                    "retry.max_attempts=%d" % retry["max_attempts"])
            d = opts.get("retry_delay_seconds", 0)
            dl = lines.get("retry_delay_seconds", line)
            if isinstance(d, (int, float)) and not isinstance(d, bool):
                retry.update({"strategy": "fixed", "initial_ms": ms(d), "jitter": "none"})
                if "retry_delay_seconds" in opts:
                    wf.find(dl, "retry_delay_seconds", TRANSLATED, "%s: %gs" % (tid, d), "retry.initial_ms")
            elif isinstance(d, list) and d and all(isinstance(x, (int, float)) for x in d):
                mult = (d[1] / d[0]) if len(d) > 1 and d[0] > 0 else 1
                retry.update({"strategy": "exponential" if mult > 1 else "fixed", "initial_ms": ms(d[0]),
                              "max_ms": ms(max(d)), "jitter": "none"})
                if mult > 1:
                    retry["multiplier"] = round(min(100.0, mult), 3)
                wf.find(dl, "retry_delay_seconds", APPROXIMATED,
                        "%s: the delay list %s becomes %s backoff from %gs%s, capped at %gs"
                        % (tid, d, retry["strategy"], d[0],
                           " x%g" % retry["multiplier"] if mult > 1 else "", max(d)), "retry")
            elif isinstance(d, Obj) and d.cls == "exponential_backoff":
                bf = d.kwargs.get("backoff_factor", d.args[0] if d.args else 1)
                if isinstance(bf, (int, float)):
                    retry.update({"strategy": "exponential", "initial_ms": ms(bf), "multiplier": 2, "jitter": "none"})
                    wf.find(dl, "exponential_backoff", TRANSLATED, "%s: exponential_backoff(%g)" % (tid, bf),
                            "retry.strategy=exponential")
            else:
                wf.find(dl, "retry_delay_seconds", UNSUPPORTED, "%s: retry_delay_seconds is not a literal" % tid)
            if opts.get("retry_jitter_factor") is not None:
                retry["jitter"] = "full"
                wf.find(lines.get("retry_jitter_factor", line), "retry_jitter_factor", APPROXIMATED,
                        "%s: a jitter factor becomes full jitter" % tid, "retry.jitter")
        t["retry"] = retry
        s = seconds_of(opts.get("timeout_seconds"))
        if s:
            t["timeouts"] = {"start_to_close_ms": ms(s)}
            wf.find(lines.get("timeout_seconds", line), "timeout_seconds", TRANSLATED, "%s: %gs" % (tid, s),
                    "timeouts.start_to_close_ms")
        cache = None
        ckf, cpol = opts.get("cache_key_fn"), opts.get("cache_policy")
        if isinstance(ckf, Obj) and ckf.cls == "task_input_hash":
            cache = {"enabled": True}
            wf.find(lines.get("cache_key_fn", line), "cache_key_fn", TRANSLATED,
                    "%s: task_input_hash; Nexus memoizes on task_ref + version + input hash" % tid, "cache")
        elif ckf is not None:
            cache = {"enabled": True}
            wf.find(lines.get("cache_key_fn", line), "cache_key_fn", APPROXIMATED,
                    "%s: a custom cache key function becomes Nexus's input-hash key" % tid, "cache")
        if isinstance(cpol, Obj) and cpol.cls == "cache_policy":
            names = set(cpol.args)
            cl = lines.get("cache_policy", line)
            if names & {"NONE", "NO_CACHE"}:
                cache = None
                wf.find(cl, "cache_policy", TRANSLATED, "%s: no cache" % tid, "no cache")
            elif names == {"INPUTS"}:
                cache = {"enabled": True}
                wf.find(cl, "cache_policy", TRANSLATED, "%s: INPUTS" % tid, "cache")
            elif "RUN_ID" in names or names == {"DEFAULT"}:
                wf.find(cl, "cache_policy", APPROXIMATED,
                        "%s: a cache keyed on the run id only reuses results within one run; a Nexus retry already "
                        "reuses a succeeded task's result, so no cache was enabled" % tid, "no cache")
            else:
                cache = {"enabled": True}
                wf.find(cl, "cache_policy", APPROXIMATED,
                        "%s: %s -> Nexus input-hash memoization; for TASK_SOURCE, bump the task's version when its "
                        "code changes" % (tid, " + ".join(sorted(names))), "cache")
        elif cpol is not None:
            cache = {"enabled": True}
            wf.find(lines.get("cache_policy", line), "cache_policy", APPROXIMATED,
                    "%s: a custom cache policy becomes Nexus's input-hash memoization" % tid, "cache")
        if cache is not None:
            s = seconds_of(opts.get("cache_expiration"))
            if s:
                cache["ttl_ms"] = ms(s)
                wf.find(lines.get("cache_expiration", line), "cache_expiration", TRANSLATED, "%s: %gs" % (tid, s),
                        "cache.ttl_ms")
            t["cache"] = cache
        if opts.get("tags"):
            wf.find(lines.get("tags", line), "tags", APPROXIMATED,
                    "%s: task tags (and Prefect's tag-based concurrency limits) are dropped; use "
                    "concurrency_key/concurrency_limit for a limit" % tid)
        for k in opts:
            if k in TASK_OPTS:
                continue
            ln = lines.get(k, line)
            if k.startswith("on_") or k == "retry_condition_fn":
                wf.find(ln, k, UNSUPPORTED, "%s: %s runs Prefect code on state changes; not translated" % (tid, k))
            elif k in ("result_storage", "result_serializer", "cache_result_in_memory", "result_storage_key"):
                wf.find(ln, k, APPROXIMATED, "%s: %s: Nexus keeps results in its result store" % (tid, k))
            else:
                wf.find(ln, k, UNSUPPORTED, "%s: task option %s is not translated" % (tid, k))

    def prefect_map(self, f, args, kwargs, line, overrides):
        wf = self.wf
        if wf is None:
            return Unknown("a map outside a flow", line)
        a = f.node.args
        pos = [p.arg for p in (getattr(a, "posonlyargs", []) + a.args)]
        mapped, fixed = [], {}
        kwargs = dict(kwargs)
        wait_for = kwargs.pop("wait_for", None)
        pairs = [(pos[i] if i < len(pos) else "arg%d" % i, v) for i, v in enumerate(args)] + list(kwargs.items())
        for name, v in pairs:
            if self.is_mappable(v):
                mapped.append((name, v))
            else:
                fixed[name] = v.args[0] if isinstance(v, Obj) and v.cls == "unmapped" and v.args else v
        if wait_for is not None:
            fixed["wait_for"] = wait_for
        ref = self.call_task(f, [], fixed, line, overrides)
        t = ref.task
        if t["kind"] == "placeholder":
            return ref
        if len(mapped) != 1:
            wf.find(line, ".map", UNSUPPORTED,
                    "%s: .map() over %d iterables zips them; a Nexus map expands exactly one input"
                    % (t["id"], len(mapped)))
            return ref
        self.make_map(t, mapped[0][0], mapped[0][1], line, ".map")
        return ref

    def is_mappable(self, v):
        if isinstance(v, Obj) and v.cls == "unmapped":
            return False
        if isinstance(v, (list, TaskRef)):
            return True
        if isinstance(v, ParamRef):
            p = self.wf.params.get(v.name) if self.wf else None
            return p is None or p.get("type") == "json"
        return False

    def serve_call(self, flow, how, kwargs, line):
        target = None
        for w in self.workflows:
            if w.function == flow.name:
                target = w
        if target is None:
            self.find(line, how, APPROXIMATED, "%s() for a flow that is not converted; ignored" % how)
            return
        via = "%s.%s()" % (flow.name, how)
        params = kwargs.get("parameters")
        params = params if isinstance(params, dict) and is_const(params) else None
        found = False
        base = {"line": line, "via": via}
        if params:
            base["params"] = params
        if isinstance(kwargs.get("cron"), str):
            s = dict(base, cron=kwargs["cron"])
            target.schedules.append(s)
            found = True
        if kwargs.get("interval") is not None:
            sec = seconds_of(kwargs["interval"])
            s = dict(base)
            if sec:
                s["interval_seconds"] = sec
            else:
                s["unsupported"] = "the interval is not a literal"
            target.schedules.append(s)
            found = True
        if kwargs.get("rrule") is not None:
            target.schedules.append(dict(base, unsupported="an RRule schedule has no cron equivalent"))
            found = True
        for key in ("schedule", "schedules"):
            v = kwargs.get(key)
            if v is None:
                continue
            for it in (v if isinstance(v, list) else [v]):
                s = dict(base)
                if isinstance(it, Obj) and it.cls == "Cron":
                    c = it.args[0] if it.args else it.kwargs.get("cron")
                    if isinstance(c, str):
                        s["cron"] = c
                    if isinstance(it.kwargs.get("timezone"), str):
                        s["timezone"] = it.kwargs["timezone"]
                elif isinstance(it, Obj) and it.cls == "Interval":
                    sec = seconds_of(it.args[0] if it.args else it.kwargs.get("interval"))
                    if sec:
                        s["interval_seconds"] = sec
                    if isinstance(it.kwargs.get("timezone"), str):
                        s["timezone"] = it.kwargs["timezone"]
                else:
                    s["unsupported"] = "schedule object %s is not translated" % describe(it)
                target.schedules.append(s)
                found = True
        if not found:
            target.find(line, how, TRANSLATED, "%s without a schedule: runs start on demand" % via, "")
        for k in ("work_pool_name", "image", "work_queue_name", "job_variables", "build", "push"):
            if kwargs.get(k) is not None:
                target.find(line, k, APPROXIMATED,
                            "deployment infrastructure (%s) is not part of a Nexus spec; ignored" % k)
