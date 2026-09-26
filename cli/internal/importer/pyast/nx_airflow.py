# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""Airflow semantics for the analyzer: DAGs, operators, TaskFlow, task groups,
dependencies, default_args, schedules, Jinja in bash_command."""

from __future__ import annotations

import ast
import re
import textwrap

from nx_core import Core
from nx_values import (APPROXIMATED, DT, MISSING, TRANSLATED, TZ, UNSUPPORTED, DagHandle, Dur, FuncRef, GroupRef,
                    Obj, Override, Partial, Scope, Signal, TaskRef, Unknown, annotation_type, describe,
                    ms, new_task, seconds_of, ParamRef, Workflow)

CONTEXT_PARAMS = {
    "ti", "task_instance", "ds", "ds_nodash", "ts", "ts_nodash", "ts_nodash_with_tz", "logical_date",
    "execution_date", "dag", "dag_run", "params", "run_id", "data_interval_start", "data_interval_end",
    "task", "conf", "macros", "var", "conn", "prev_ds", "next_ds", "inlets", "outlets", "templates_dict",
    "triggering_dataset_events", "triggering_asset_events", "outlet_events", "inlet_events",
    "test_mode", "map_index_template",
}

DATE_MACROS = {
    "ds", "ds_nodash", "ts", "ts_nodash", "ts_nodash_with_tz", "logical_date", "execution_date",
    "data_interval_start", "data_interval_end", "prev_ds", "next_ds", "prev_ds_nodash", "next_ds_nodash",
    "yesterday_ds", "tomorrow_ds", "yesterday_ds_nodash", "tomorrow_ds_nodash", "prev_execution_date",
    "next_execution_date", "prev_data_interval_start_success", "prev_start_date_success",
}

PHASE1_RULES = {"all_success", "all_done"}
DEFERRED_RULES = {"one_success", "one_failed", "none_failed", "all_skipped"}

BRANCH_OPERATORS = {
    "BranchPythonOperator", "BranchExternalPythonOperator", "BranchPythonVirtualenvOperator",
    "BranchSQLOperator", "BranchDateTimeOperator", "BranchDayOfWeekOperator", "ShortCircuitOperator",
    "LatestOnlyOperator", "BaseBranchOperator",
}
PYTHON_OPERATORS = {"PythonOperator", "PythonVirtualenvOperator", "ExternalPythonOperator"}
NOOP_OPERATORS = {"EmptyOperator", "DummyOperator"}
POD_OPERATORS = {"KubernetesPodOperator", "GKEStartPodOperator", "EksPodOperator", "AKSPodOperator"}

# The BaseOperator arguments, examined on every operator and in default_args.
BASE_OPERATOR_ARGS = {
    "retries", "retry_delay", "retry_exponential_backoff", "max_retry_delay", "execution_timeout",
    "trigger_rule", "queue", "pool", "pool_slots", "priority_weight", "weight_rule", "depends_on_past",
    "wait_for_downstream", "ignore_first_depends_on_past", "email", "email_on_failure", "email_on_retry",
    "on_failure_callback", "on_success_callback", "on_retry_callback", "on_execute_callback", "on_skipped_callback",
    "sla", "owner", "start_date", "end_date", "doc", "doc_md", "doc_rst", "doc_json", "doc_yaml", "dag",
    "params", "executor_config", "do_xcom_push", "multiple_outputs", "map_index_template",
    "max_active_tis_per_dag", "max_active_tis_per_dagrun", "resources", "run_as_user", "task_display_name",
    "inlets", "outlets", "executor", "task_id", "pre_execute", "post_execute", "task_group",
}
# Accepted, nothing to translate: documentation and bookkeeping.
IGNORED_ARGS = {"doc", "doc_md", "doc_rst", "doc_json", "doc_yaml", "task_display_name", "owner", "dag",
                "task_id", "start_date", "end_date", "map_index_template", "weight_rule", "multiple_outputs",
                "do_xcom_push", "email", "params", "task_group", "pool_slots"}

# Operator arguments each translator reads itself (the rest must be BaseOperator arguments).
OWN_ARGS = {
    "BashOperator": {"bash_command", "env", "append_env", "cwd", "skip_on_exit_code", "output_processor"},
    "PythonOperator": {"python_callable", "op_args", "op_kwargs", "templates_dict", "templates_exts",
                       "show_return_value_in_logs"},
    "DockerOperator": {"image", "command", "environment", "mounts", "volumes", "network_mode",
                       "private_environment", "docker_url", "auto_remove", "privileged", "cap_add", "devices",
                       "device_requests", "shm_size", "user", "api_version", "mount_tmp_dir", "tty",
                       "force_pull", "xcom_all", "retrieve_output", "retrieve_output_path"},
}
OWN_ARGS["PythonVirtualenvOperator"] = OWN_ARGS["PythonOperator"] | {
    "requirements", "python_version", "system_site_packages", "pip_install_options", "use_dill", "serializer",
    "index_urls", "venv_cache_path"}
OWN_ARGS["ExternalPythonOperator"] = OWN_ARGS["PythonOperator"] | {"python", "use_dill", "serializer"}
POD_UNSUPPORTED = ("secrets", "volumes", "volume_mounts", "env_from", "affinity", "tolerations", "node_selector",
                   "init_containers", "image_pull_secrets", "ports", "priority_class_name", "service_account_name",
                   "security_context", "container_security_context", "sidecars", "pod_template_file",
                   "full_pod_spec", "host_aliases", "dnspolicy", "schedulername", "hostnetwork", "do_xcom_push",
                   "config_file", "cluster_context", "annotations")
POD_IGNORED = ("namespace", "name", "labels", "in_cluster", "get_logs", "on_finish_action",
               "is_delete_operator_pod", "startup_timeout_seconds", "image_pull_policy", "random_name_suffix",
               "reattach_on_restart", "log_events_on_failure", "kubernetes_conn_id", "deferrable", "poll_interval",
               "project_id", "location", "cluster_name", "gcp_conn_id", "use_internal_ip", "aws_conn_id", "region")
OWN_ARGS.update({k: {"image", "cmds", "arguments", "env_vars", "container_resources", "resources"}
                 | set(POD_UNSUPPORTED) | set(POD_IGNORED) for k in POD_OPERATORS})

TASKFLOW_OPTS = {"task_id", "multiple_outputs", "image", "requirements", "python_version", "python",
                 "system_site_packages", "use_dill", "serializer", "namespace", "name", "cmds", "env_vars",
                 "venv_cache_path", "pip_install_options", "index_urls", "mode", "poke_interval", "timeout",
                 "soft_fail", "do_xcom_push"}

HELPER = ("nexus_param() { v=$(cat \"$NEXUS_INPUT_DIR/$1\"); "
          "case $v in \\\"*\\\") v=${v#\\\"}; v=${v%\\\"} ;; esac; printf '%s' \"$v\"; }\n")


class Airflow(Core):
    # ── module structure ────────────────────────────────────────────────────
    def finish(self):
        for f in self.funcs.values():
            if f.kind == "dag" and not f.called:
                self.dag_function(f, None, self.module_scope, called=False)

    def classify_function(self, node, sc, nested):
        for d in node.decorator_list:
            call = d if isinstance(d, ast.Call) else None
            q = self.qual(call.func if call else d) or ""
            parts = q.split(".")
            opts = self.kwargs_of(call, sc) if call else {}
            if parts[-1] == "dag":
                return FuncRef(node, "dag", opts=opts, deco_line=d.lineno, nested=nested)
            if parts[-1] == "task_group":
                return FuncRef(node, "group", opts=opts, deco_line=d.lineno, nested=nested)
            if parts[-1] == "task":
                return FuncRef(node, "task", opts=opts, deco_line=d.lineno, nested=nested)
            if len(parts) >= 2 and parts[-2] == "task":
                return FuncRef(node, "task", variant=parts[-1], opts=opts, deco_line=d.lineno, nested=nested)
        return FuncRef(node, "plain", nested=nested)

    def with_stmt(self, st, sc, module):
        pushed = 0
        saved = self.wf
        try:
            for item in st.items:
                ce = item.context_expr
                q = self.qual(ce.func) if isinstance(ce, ast.Call) else None
                short = q.split(".")[-1] if q else None
                if short == "DAG":
                    if self.dynamic:
                        self.find(st.lineno, "DAG", UNSUPPORTED,
                                  "a DAG created inside a loop or condition is generated dynamically; the converter "
                                  "does not evaluate it")
                        return None
                    wf = self.make_dag(ce, sc)
                    self.wf = wf
                    if item.optional_vars is not None:
                        self.bind(item.optional_vars, DagHandle(wf), sc)
                    continue
                if short == "TaskGroup":
                    gid = self.push_group(ce, sc)
                    if gid is None:
                        return None
                    pushed += 1
                    if item.optional_vars is not None:
                        self.bind(item.optional_vars, GroupRef(self.wf, gid), sc)
                    continue
                v = self.ev(ce, sc)
                if isinstance(v, DagHandle):
                    self.wf = v.wf
                    continue
                if self.wf is not None:
                    self.find(st.lineno, "with", APPROXIMATED,
                              "context manager %s is not translated; its body was converted without it"
                              % (q or "expression"))
                if item.optional_vars is not None:
                    self.bind(item.optional_vars, Unknown("a context manager's value", st.lineno), sc)
            return self.body(st.body, sc, module)
        finally:
            for _ in range(pushed):
                self.groups.pop()
            self.wf = saved

    def push_group(self, call, sc):
        args, kwargs, _ = self.args_of(call, sc)
        gid = kwargs.get("group_id", args[0] if args else None)
        if not isinstance(gid, str):
            self.find(call.lineno, "TaskGroup", UNSUPPORTED, "the group_id is not a literal string")
            return None
        if kwargs.get("prefix_group_id") is False:
            self.find(call.lineno, "TaskGroup", APPROXIMATED,
                      "prefix_group_id=False is ignored: task ids are still prefixed with %s" % gid)
        self.groups.append(gid)
        full = ".".join(self.groups)
        if self.wf is not None:
            self.wf.groups.setdefault(full, [])
            self.wf.find(call.lineno, "TaskGroup", TRANSLATED, "task group %s flattened" % full,
                         "node ids %s.*" % full)
        return full

    # ── values ──────────────────────────────────────────────────────────────
    def known_attribute(self, q, node):
        parts = q.split(".")
        if len(parts) >= 2 and parts[-2] in ("TriggerRule", "WeightRule"):
            return parts[-1].lower()
        if q in ("datetime.timezone.utc", "pendulum.UTC"):
            return TZ("UTC")
        return MISSING

    def known_function(self, q, node, sc):
        line = node.lineno
        q = q or ""
        parts = q.split(".")
        short = parts[-1]
        args, kwargs, lines = self.args_of(node, sc)
        g = self.generic_function(q, node, sc, args, kwargs)
        if g is not MISSING:
            return g
        if short == "days_ago":
            self.find(line, "days_ago", APPROXIMATED,
                      "days_ago() moves the start date every time Airflow parses the file; no start date was "
                      "carried over")
            return Unknown("days_ago()", line)
        if (len(parts) >= 2 and parts[-2] == "Variable") or short == "Variable":
            self.find(line, "Variable.get", UNSUPPORTED,
                      "Airflow Variables are read from the metadata database; Nexus has no variable store. Pass "
                      "the value as a workflow param, or read it inside the task")
            return Unknown("Variable.get()", line)
        if short in ("chain", "chain_linear"):
            self.chain(args, line, linear=(short == "chain_linear"))
            return None
        if short == "cross_downstream":
            a = args[0] if args else kwargs.get("from_tasks")
            b = args[1] if len(args) > 1 else kwargs.get("to_tasks")
            ua, ub = self.as_ids(a, "leaves"), self.as_ids(b, "roots")
            if self.wf is None:
                return None
            if ua is None or ub is None:
                self.find(line, "cross_downstream", UNSUPPORTED, "an operand is not statically resolvable")
                return None
            for x in ua:
                for y in ub:
                    self.wf.add_edge(x, y, line)
            self.wf.find(line, "cross_downstream", TRANSLATED, "cross_downstream", "edges")
            return None
        if short == "DAG":
            if self.dynamic:
                self.find(line, "DAG", UNSUPPORTED, "a DAG created inside a loop or condition is generated "
                                                    "dynamically; the converter does not evaluate it")
                return Unknown("a dynamic DAG", line)
            return DagHandle(self.make_dag(node, sc))
        if short in ("TaskGroup", "Param", "Secret") or short.endswith("Timetable"):
            return Obj(short, args, kwargs, line)
        if short in ("Dataset", "Asset", "AssetAlias", "DatasetAlias", "AssetAll", "AssetAny"):
            return Obj("Asset", args, kwargs, line)
        if short.endswith("Operator") or short.endswith("Sensor"):
            if isinstance(node.func, ast.Attribute) and node.func.attr == "partial":
                pass
            return self.operator(short, q, node, args, kwargs, lines)
        if short == "partial" and len(parts) >= 2 and (parts[-2].endswith("Operator") or parts[-2].endswith("Sensor")):
            return Partial(parts[-2], kwargs, line)
        if "kubernetes" in q or (len(parts) >= 2 and parts[-2] in ("k8s", "models")):
            return Obj(short, args, kwargs, line)
        return Unknown("a call to %s()" % (q or "an expression"), line)

    def call_value(self, v, node, sc):
        line = node.lineno
        if isinstance(v, tuple) and v and v[0] == "method":
            return self.method_call(v[1], v[2], node, sc)
        if isinstance(v, FuncRef):
            args, kwargs, lines = self.args_of(node, sc)
            if v.kind == "task":
                return self.call_task(v, args, kwargs, line, {})
            if v.kind == "group":
                return self.call_group(v, args, kwargs, line, sc)
            if v.kind == "dag":
                v.called = True
                return self.dag_function(v, kwargs, sc, called=True)
            if self.wf is not None and any(isinstance(x, TaskRef) for x in list(args) + list(kwargs.values())):
                self.find(line, "call", UNSUPPORTED,
                          "%s() is a plain function applied to task results in the DAG file; that needs code "
                          "between tasks. Make it a task" % v.name)
            return Unknown("the result of %s()" % v.name, line)
        if isinstance(v, Override):
            args, kwargs, _ = self.args_of(node, sc)
            return self.call_task(v.func, args, kwargs, line, v.opts)
        return Unknown("a call of %s" % describe(v), line)

    def method_call(self, base, attr, node, sc):
        line = node.lineno
        if isinstance(base, tuple) and base and base[0] == "method":
            base = self.method_call(base[1], base[2], node, sc)
        if isinstance(base, FuncRef) and base.kind == "task":
            args, kwargs, _ = self.args_of(node, sc)
            if attr == "partial":
                return Partial(base, kwargs, line)
            if attr == "override":
                return Override(base, kwargs)
            if attr == "expand":
                return self.mapped(base, {}, kwargs, {}, line)
            if attr == "expand_kwargs":
                return self.unsupported_task(base.name, line, "expand_kwargs() maps over a list of argument "
                                                              "dicts; a Nexus map expands one input")
        if isinstance(base, Override):
            args, kwargs, _ = self.args_of(node, sc)
            if attr == "expand":
                return self.mapped(base.func, {}, kwargs, base.opts, line)
            if attr == "partial":
                return Partial(base.func, kwargs, line, base.opts)
        if isinstance(base, Partial):
            args, kwargs, _ = self.args_of(node, sc)
            if attr == "expand" and isinstance(base.target, FuncRef):
                return self.mapped(base.target, base.kwargs, kwargs, base.opts, line)
            if attr in ("expand", "expand_kwargs"):
                tid = base.kwargs.get("task_id")
                name = tid if isinstance(tid, str) else getattr(base.target, "name", str(base.target).lower())
                return self.unsupported_task(name, line,
                                             "mapping a classic operator (%s.partial().%s()) is not translated; "
                                             "rewrite it as a TaskFlow task, which maps to a Nexus map"
                                             % (base.target if isinstance(base.target, str) else base.target.name,
                                                attr))
        if isinstance(base, FuncRef) and base.kind == "group" and attr in ("expand", "partial", "expand_kwargs"):
            return self.unsupported_task(base.name, line,
                                         "a mapped task group (%s.%s) expands a whole subgraph per element; a "
                                         "Nexus map expands a single task" % (base.name, attr))
        if isinstance(base, TaskRef):
            if attr in ("as_setup", "as_teardown"):
                self.find(line, attr, UNSUPPORTED,
                          "setup/teardown tasks have no Nexus equivalent; %s runs as an ordinary task"
                          % base.task["id"])
                return base
            if attr in ("map", "zip", "concat"):
                self.find(line, "XComArg.%s" % attr, UNSUPPORTED,
                          "transforming a task result in the DAG file (.%s) needs code between tasks" % attr)
                return Unknown("XComArg.%s()" % attr, line)
        return Core.method_call(self, base, attr, node, sc)

    def chain(self, items, line, linear=False):
        if self.wf is None:
            return
        prev = None
        for cur in items:
            if prev is not None:
                if isinstance(prev, list) and isinstance(cur, list):
                    if linear:
                        a, b = self.as_ids(prev, "leaves"), self.as_ids(cur, "roots")
                        if a is None or b is None:
                            self.find(line, "chain_linear", UNSUPPORTED, "an operand is not statically resolvable")
                        else:
                            for x in a:
                                for y in b:
                                    self.wf.add_edge(x, y, line)
                    elif len(prev) != len(cur):
                        self.find(line, "chain", UNSUPPORTED,
                                  "chain() between lists of different lengths (%d and %d) is rejected by Airflow"
                                  % (len(prev), len(cur)))
                    else:
                        for x, y in zip(prev, cur):
                            self.depend(x, y, line)
                else:
                    self.depend(prev, cur, line)
            prev = cur
        self.wf.find(line, "chain_linear" if linear else "chain", TRANSLATED, "chain", "edges")

    # ── DAGs ────────────────────────────────────────────────────────────────
    def make_dag(self, call, sc):
        args, kwargs, lines = self.args_of(call, sc)
        dag_id = kwargs.get("dag_id", args[0] if args else None)
        wf = Workflow(dag_id if isinstance(dag_id, str) else "dag_line_%d" % call.lineno, self.file, call.lineno)
        self.workflows.append(wf)
        if not isinstance(dag_id, str):
            wf.find(call.lineno, "DAG", UNSUPPORTED, "the dag_id is not a literal string (%s)" % describe(dag_id))
        self.dag_kwargs(wf, kwargs, lines, call.lineno)
        return wf

    def dag_function(self, f, call_kwargs, sc, called):
        opts = {k: v[0] for k, v in f.opts.items() if k != "**"}
        lines = {k: v[1] for k, v in f.opts.items() if k != "**"}
        dag_id = opts.get("dag_id", f.name)
        wf = Workflow(dag_id if isinstance(dag_id, str) else f.name, self.file, f.deco_line)
        self.workflows.append(wf)
        saved, saved_groups = self.wf, self.groups
        self.wf, self.groups = wf, []
        try:
            doc = ast.get_docstring(f.node)
            if doc:
                wf.description = textwrap.dedent(doc).strip()
            self.dag_kwargs(wf, opts, lines, f.deco_line)
            if not called:
                wf.find(f.deco_line, "@dag", APPROXIMATED,
                        "%s() is never called at module level, so Airflow would not register it; converted anyway"
                        % f.name)
            inner = Scope(sc)
            for p, dv in self.signature_params(f.node, sc):
                if call_kwargs and p.arg in call_kwargs:
                    dv = call_kwargs[p.arg]
                self.declare_param(wf, p.arg, dv, p.lineno, annotation_type(p.annotation))
                inner.set(p.arg, ParamRef(p.arg))
            self.body(f.node.body, inner)
        finally:
            self.wf, self.groups = saved, saved_groups
        return DagHandle(wf)

    def dag_kwargs(self, wf, kw, lines, line):
        sched = {"line": line}
        seen = False
        for k, v in kw.items():
            if k in ("dag_id", "**"):
                continue
            ln = lines.get(k, line)
            if k in ("schedule", "schedule_interval", "timetable"):
                seen = True
                sched["line"] = ln
                self.schedule_value(v, sched)
            elif k == "start_date":
                if isinstance(v, DT):
                    sched["start_date"] = v.iso
                    if v.tz:
                        sched["timezone"] = v.tz
                    wf.find(ln, "start_date", TRANSLATED, "start date %s%s" % (v.iso, " (%s)" % v.tz if v.tz else ""),
                            "schedule timezone / backfill --from")
                elif not (isinstance(v, Unknown) and "days_ago" in v.reason):
                    wf.find(ln, "start_date", APPROXIMATED, "start_date is not a literal date (%s)" % describe(v))
            elif k == "end_date":
                if isinstance(v, DT):
                    sched["end_date"] = v.iso
                    wf.find(ln, "end_date", APPROXIMATED,
                            "a Nexus schedule has no end date: disable or delete it after %s" % v.iso)
            elif k == "catchup":
                if isinstance(v, bool):
                    sched["catchup"] = v
                    if v:
                        wf.find(ln, "catchup", APPROXIMATED,
                                "catchup=True: Airflow creates a run for every interval since start_date; a Nexus "
                                "schedule does not, so a backfill command is suggested instead", "backfill start")
                    else:
                        wf.find(ln, "catchup", TRANSLATED, "catchup=False: only new ticks run, as with a Nexus "
                                                           "schedule", "schedule")
            elif k == "default_args":
                if isinstance(v, dict):
                    wf.defaults = self.base_args(wf, v, "default_args", ln)
                    self.policy_findings(wf, wf.defaults, "default_args")
                else:
                    wf.find(ln, "default_args", UNSUPPORTED, "default_args is not a literal dict (%s)" % describe(v))
            elif k == "params":
                if isinstance(v, dict):
                    for pn, pv in v.items():
                        self.declare_param(wf, pn, pv, ln)
                else:
                    wf.find(ln, "params", UNSUPPORTED, "params is not a literal dict")
            elif k in ("description", "doc_md"):
                if isinstance(v, str):
                    if k == "description" or not wf.description:
                        wf.description = textwrap.dedent(v).strip()
                    wf.find(ln, k, TRANSLATED, k, "description")
            elif k == "max_active_runs":
                if isinstance(v, int):
                    wf.max_active_runs = v
                    sched["max_active_runs"] = v
                    wf.find(ln, k, TRANSLATED, "max_active_runs=%d" % v, "max_active_runs")
            elif k in ("max_active_tasks", "concurrency"):
                wf.find(ln, k, UNSUPPORTED,
                        "Nexus has no per-run task concurrency cap; use concurrency_key/concurrency_limit on the "
                        "tasks that contend for a resource")
            elif k == "dagrun_timeout":
                s = seconds_of(v)
                if s is not None:
                    wf.run_timeout_ms = ms(s)
                    wf.find(ln, k, TRANSLATED, "dagrun_timeout %gs" % s, "run_timeout_ms")
            elif k == "tags":
                if isinstance(v, list) and all(isinstance(x, str) for x in v):
                    wf.labels["tags"] = ",".join(v)
                    wf.find(ln, k, TRANSLATED, "tags", "labels.tags")
            elif k == "fail_fast":
                if isinstance(v, bool):
                    wf.fail_fast = v
                    wf.find(ln, k, TRANSLATED, "fail_fast", "fail_fast")
            elif k == "is_paused_upon_creation":
                if v is True:
                    sched["disabled"] = True
                    wf.find(ln, k, TRANSLATED, "created paused", "schedule create --disabled")
            elif k in ("on_success_callback", "on_failure_callback", "sla_miss_callback"):
                if v is not None:
                    wf.find(ln, k, UNSUPPORTED,
                            "DAG callbacks run Airflow code when a run ends; Nexus has none. Watch runs with "
                            "`nexusctl events --follow`, or add a final task with trigger_rule all_done")
            elif k in ("template_searchpath", "user_defined_macros", "user_defined_filters",
                       "jinja_environment_kwargs", "render_template_as_native_obj"):
                if v not in (None, False):
                    wf.find(ln, k, UNSUPPORTED, "Jinja configuration has no Nexus equivalent")
            elif k in ("default_view", "orientation", "owner_links", "access_control", "dag_display_name",
                       "auto_register", "max_consecutive_failed_dag_runs", "doc", "disable_bundle_versioning"):
                wf.find(ln, k, APPROXIMATED, "%s is an Airflow UI/operations setting; ignored" % k)
            else:
                wf.find(ln, k, UNSUPPORTED, "DAG argument %s is not translated" % k)
        if not seen:
            sched["unspecified"] = True
        wf.schedules.append(sched)

    def schedule_value(self, v, sched):
        if v is None:
            sched["none"] = True
        elif isinstance(v, str):
            sched["cron"] = v
        elif isinstance(v, Dur):
            sched["interval_seconds"] = v.seconds
        elif isinstance(v, Obj) and v.cls in ("CronTriggerTimetable", "CronDataIntervalTimetable"):
            c = v.args[0] if v.args else v.kwargs.get("cron")
            tz = v.kwargs.get("timezone")
            if isinstance(c, str):
                sched["cron"] = c
                if isinstance(tz, (str, TZ)):
                    sched["timezone"] = tz.name if isinstance(tz, TZ) else tz
                if v.cls == "CronDataIntervalTimetable":
                    sched["note"] = "CronDataIntervalTimetable runs at the END of each data interval"
            else:
                sched["unsupported"] = "the timetable's cron is not a literal"
        elif isinstance(v, Obj) and v.cls == "DeltaDataIntervalTimetable":
            s = seconds_of(v.args[0] if v.args else None)
            if s:
                sched["interval_seconds"] = s
            else:
                sched["unsupported"] = "the timetable's interval is not a literal"
        elif isinstance(v, list) or (isinstance(v, Obj) and v.cls == "Asset"):
            sched["unsupported"] = ("data-aware (Dataset/Asset) scheduling starts a run when an upstream DAG "
                                    "updates an asset; Nexus schedules are time-based only")
        elif isinstance(v, Obj):
            sched["unsupported"] = "timetable %s has no cron equivalent" % v.cls
        else:
            sched["unsupported"] = "the schedule is not a literal (%s)" % describe(v)

    # ── BaseOperator arguments / default_args ──────────────────────────────
    def base_args(self, wf, args, where, line):
        pol = {}
        for k, v in args.items():
            if k == "retries":
                if isinstance(v, int):
                    pol["retries"] = (v, line)
            elif k == "retry_delay":
                s = seconds_of(v)
                if s is not None:
                    pol["retry_delay"] = (s, line)
            elif k == "retry_exponential_backoff":
                pol["exp"] = (bool(v), line)
            elif k == "max_retry_delay":
                s = seconds_of(v)
                if s is not None:
                    pol["max_retry_delay"] = (s, line)
            elif k == "execution_timeout":
                s = seconds_of(v)
                if s is not None:
                    pol["timeout"] = (s, line)
                elif v is not None:
                    wf.find(line, k, UNSUPPORTED, "%s: execution_timeout is not a literal" % where)
            elif k == "trigger_rule":
                pol["trigger_rule"] = (v, line)
            elif k == "queue":
                if isinstance(v, str):
                    pol["queue"] = (v, line)
            elif k == "priority_weight":
                if isinstance(v, int):
                    pol["priority_weight"] = (v, line)
            elif k == "pool":
                if v not in (None, "default_pool"):
                    wf.find(line, "pool", UNSUPPORTED,
                            "%s: Airflow pool %r: its slot count lives in the Airflow database, not the DAG file. "
                            "The Nexus equivalent is a global semaphore, concurrency_key + concurrency_limit; set "
                            "both by hand" % (where, v))
            elif k == "max_active_tis_per_dag":
                if isinstance(v, int):
                    pol["max_active_tis"] = (v, line)
            elif k in ("depends_on_past", "wait_for_downstream", "ignore_first_depends_on_past"):
                if v and k != "ignore_first_depends_on_past":
                    wf.find(line, k, UNSUPPORTED,
                            "%s: %s=True makes a task wait on the PREVIOUS run; Nexus runs are independent"
                            % (where, k))
            elif k in ("email_on_failure", "email_on_retry"):
                if v:
                    wf.find(line, k, UNSUPPORTED, "%s: email notifications are not part of a Nexus spec" % where)
            elif k in ("on_failure_callback", "on_success_callback", "on_retry_callback", "on_execute_callback",
                       "on_skipped_callback", "sla", "pre_execute", "post_execute"):
                if v is not None:
                    wf.find(line, k, UNSUPPORTED, "%s: %s runs Airflow code around the task; not translated"
                            % (where, k))
            elif k in ("executor_config", "resources", "run_as_user", "executor", "inlets", "outlets",
                       "max_active_tis_per_dagrun"):
                if v:
                    wf.find(line, k, UNSUPPORTED, "%s: %s is not translated" % (where, k))
            elif k in IGNORED_ARGS:
                pass
            else:
                pol.setdefault("_unknown", []).append((k, line))
        return pol

    def policy_findings(self, wf, pol, where):
        for k, val in pol.items():
            if k == "_unknown":
                for name, ln in val:
                    wf.find(ln, name, UNSUPPORTED, "%s: argument %s is not translated" % (where, name))
                continue
            v, ln = val
            if k == "retries":
                wf.find(ln, "retries", TRANSLATED, "%s: retries=%d" % (where, v),
                        "retry.max_attempts=%d" % (min(v, 99) + 1))
            elif k == "retry_delay":
                wf.find(ln, "retry_delay", TRANSLATED, "%s: retry_delay %gs" % (where, v), "retry.initial_ms")
            elif k == "exp" and v:
                wf.find(ln, "retry_exponential_backoff", APPROXIMATED,
                        "%s: exponential backoff with multiplier 2 and full jitter; Airflow's formula differs in "
                        "detail" % where, "retry.strategy=exponential")
            elif k == "max_retry_delay":
                wf.find(ln, "max_retry_delay", TRANSLATED, "%s: max_retry_delay %gs" % (where, v), "retry.max_ms")
            elif k == "timeout":
                wf.find(ln, "execution_timeout", TRANSLATED, "%s: execution_timeout %gs" % (where, v),
                        "timeouts.start_to_close_ms")
            elif k == "queue":
                wf.find(ln, "queue", TRANSLATED, "%s: queue %s (a worker must subscribe to it)" % (where, v),
                        "queue")
            elif k == "priority_weight":
                wf.find(ln, "priority_weight", APPROXIMATED,
                        "%s: priority_weight %d -> priority %d (Nexus default 100; weight_rule is not applied)"
                        % (where, v, max(0, min(1000000, 99 + v))), "priority")
            elif k == "max_active_tis":
                wf.find(ln, "max_active_tis_per_dag", TRANSLATED,
                        "%s: at most %d concurrent instances across runs" % (where, v),
                        "concurrency_key + concurrency_limit")

    def finalize_policy(self, wf, t, pol):
        line = t["line"]
        retries = min(pol.get("retries", (0, line))[0], 99)
        retry = {"max_attempts": retries + 1}
        if retries > 0:
            exp = pol.get("exp", (False, line))[0]
            retry["initial_ms"] = ms(pol.get("retry_delay", (300.0, line))[0])
            if exp:
                retry.update({"strategy": "exponential", "multiplier": 2, "jitter": "full"})
            else:
                retry.update({"strategy": "fixed", "jitter": "none"})
            if "max_retry_delay" in pol:
                retry["max_ms"] = ms(pol["max_retry_delay"][0])
            elif exp:
                retry["max_ms"] = 86400000
        t["retry"] = retry
        if "timeout" in pol:
            t["timeouts"] = {"start_to_close_ms": ms(pol["timeout"][0])}
        if "trigger_rule" in pol:
            self.apply_trigger_rule(t, pol["trigger_rule"][0], pol["trigger_rule"][1], wf)
        if "queue" in pol:
            t["queue"] = pol["queue"][0]
        if "priority_weight" in pol:
            t["priority"] = max(0, min(1000000, 99 + pol["priority_weight"][0]))
        if "max_active_tis" in pol and pol["max_active_tis"][0] > 0:
            t["concurrency_key"] = "%s.%s" % (wf.source_id, t["id"])
            t["concurrency_limit"] = pol["max_active_tis"][0]

    def apply_policy(self, wf, t, own, where, line):
        pol = self.base_args(wf, own, where, line)
        self.policy_findings(wf, pol, where)
        merged = {k: v for k, v in wf.defaults.items() if k != "_unknown"}
        merged.update({k: v for k, v in pol.items() if k != "_unknown"})
        self.finalize_policy(wf, t, merged)

    def apply_trigger_rule(self, t, v, ln, wf=None):
        wf = wf or self.wf
        if not isinstance(v, str):
            wf.find(ln, "trigger_rule", UNSUPPORTED, "%s: trigger_rule is not a literal" % t["id"])
            return
        v = v.lower()
        if v in PHASE1_RULES:
            t["trigger_rule"] = v
            if v != "all_success":
                wf.find(ln, "trigger_rule", TRANSLATED, "%s: trigger_rule %s" % (t["id"], v), "trigger_rule")
        elif v in DEFERRED_RULES:
            wf.find(ln, "trigger_rule", UNSUPPORTED,
                    "%s: trigger_rule %s is in the Nexus schema, but the control plane accepts only all_success and "
                    "all_done in this release; the task keeps all_success" % (t["id"], v))
        else:
            wf.find(ln, "trigger_rule", UNSUPPORTED,
                    "%s: trigger_rule %s has no Nexus equivalent (Nexus has all_success and all_done); the task "
                    "keeps all_success" % (t["id"], v))

    # ── operators ───────────────────────────────────────────────────────────
    def operator(self, short, q, node, args, kwargs, lines):
        line = node.lineno
        dag = kwargs.get("dag")
        saved = self.wf
        if isinstance(dag, DagHandle):
            self.wf = dag.wf
        if self.wf is None:
            self.find(line, short, UNSUPPORTED, "%s is created outside any DAG; Airflow would reject it" % short)
            return Unknown("a task outside a DAG", line)
        try:
            return self._operator(short, q, line, args, kwargs, lines)
        finally:
            self.wf = saved

    def _operator(self, short, q, line, args, kwargs, lines):
        wf = self.wf
        raw_id = kwargs.get("task_id")
        if self.dynamic:
            base = raw_id if isinstance(raw_id, str) else (raw_id.pattern if isinstance(raw_id, Unknown)
                                                          and raw_id.pattern else short.lower())
            tid = wf.unique_id(self.full_id(base))
            kind, dline, _ = self.dynamic[-1]
            return self.register(self.placeholder(tid, line, "created inside a %s at line %d; not unrolled"
                                                  % (kind, dline)))
        if not isinstance(raw_id, str):
            return self.unsupported_task("%s_line_%d" % (short.lower(), line), line,
                                         "the task_id is not a literal string (%s)" % describe(raw_id))
        tid = self.full_id(raw_id)
        if tid in wf.tasks:
            wf.find(line, "task_id", UNSUPPORTED, "duplicate task_id %s (Airflow rejects the DAG)" % tid)
            tid = wf.unique_id(tid)
        if args:
            wf.find(line, short, UNSUPPORTED, "%s: positional operator arguments are not translated" % tid)
        ph = "tasks[%s] (placeholder)" % tid
        if short in NOOP_OPERATORS:
            t = new_task(tid, line, "noop")
            wf.find(line, short, TRANSLATED, "%s: no-op" % tid, "tasks[%s] (runs `true`)" % tid)
        elif short == "BashOperator":
            t = self.bash_task(tid, line, kwargs, lines)
        elif short in PYTHON_OPERATORS:
            t = self.python_operator(tid, line, short, kwargs)
        elif short in POD_OPERATORS:
            t = self.pod_task(tid, line, short, kwargs)
        elif short == "DockerOperator":
            t = self.docker_task(tid, line, kwargs)
        elif short in BRANCH_OPERATORS or short.startswith("Branch"):
            t = self.placeholder(tid, line, "%s (branching) has no Nexus equivalent" % short)
            wf.find(line, short, UNSUPPORTED,
                    "%s: %s chooses downstream branches at run time; Nexus DAGs have no branching or skipping. "
                    "Split the workflow, or move the decision into the downstream tasks" % (tid, short), ph)
        elif short.endswith("Sensor"):
            extra = []
            if isinstance(kwargs.get("mode"), str):
                extra.append("mode=%s" % kwargs["mode"])
            if kwargs.get("deferrable"):
                extra.append("deferrable")
            for k in ("poke_interval", "timeout"):
                s = seconds_of(kwargs.get(k))
                if s is not None:
                    extra.append("%s=%gs" % (k, s))
            t = self.placeholder(tid, line, "%s (a sensor) has no Nexus equivalent" % short)
            wf.find(line, short, UNSUPPORTED,
                    "%s: %s%s waits for an external condition; Nexus has no sensor primitive. Start the workflow "
                    "from the system that produces the event (`nexusctl run create`), or poll inside a task with a "
                    "retry policy" % (tid, short, " (%s)" % ", ".join(extra) if extra else ""), ph)
        elif short == "TriggerDagRunOperator":
            t = self.placeholder(tid, line, "TriggerDagRunOperator has no Nexus equivalent")
            wf.find(line, short, UNSUPPORTED,
                    "%s: to start another workflow, run `nexusctl run create <workflow>` from a shell task" % tid, ph)
        else:
            t = self.placeholder(tid, line, "operator %s has no Nexus equivalent" % short)
            wf.find(line, short, UNSUPPORTED,
                    "%s: operator %s (%s) has no Nexus equivalent; replace it with a shell, container or Python "
                    "task that does the same work" % (tid, short, q), ph)
        own = OWN_ARGS.get(short)
        if own is None:
            base = {k: v for k, v in kwargs.items() if k in BASE_OPERATOR_ARGS}
        else:
            base = {k: v for k, v in kwargs.items() if k not in own}
        self.apply_policy(wf, t, base, tid, line)
        return self.register(t)

    def bash_task(self, tid, line, kwargs, lines):
        wf = self.wf
        cmd = kwargs.get("bash_command")
        if not isinstance(cmd, str):
            wf.find(line, "BashOperator", UNSUPPORTED,
                    "%s: bash_command is not a literal string (%s)" % (tid, describe(cmd)),
                    "tasks[%s] (placeholder)" % tid)
            return self.placeholder(tid, line, "bash_command is not statically known")
        t = new_task(tid, line, "shell")
        t["interpreter"] = "bash"
        t["script"] = self.jinja(cmd, t, lines.get("bash_command", line))
        stripped = cmd.strip()
        if re.search(r"\.(sh|bash)$", stripped) and " " not in stripped:
            wf.find(line, "BashOperator", APPROXIMATED,
                    "%s: bash_command %s is a script FILE that Airflow renders as a Jinja template; Nexus runs the "
                    "path as-is, so ship the script with the task and remove any Jinja from it" % (tid, stripped))
        if re.search(r"\bexit\s+99\b", cmd) or kwargs.get("skip_on_exit_code") is not None:
            wf.find(line, "skip_on_exit_code", APPROXIMATED,
                    "%s: Airflow marks the task SKIPPED on exit code 99 (skip_on_exit_code); Nexus has no skipped "
                    "state, so that exit code fails the task" % tid)
        env = kwargs.get("env")
        if isinstance(env, dict) and all(isinstance(x, str) for x in env.values()):
            t["env"] = dict(env)
            if kwargs.get("append_env") is not True:
                wf.find(line, "env", APPROXIMATED,
                        "%s: in Airflow env REPLACES the environment (append_env=False); Nexus adds these variables "
                        "to the worker's environment" % tid, "runtime.env")
        elif env is not None:
            wf.find(line, "env", UNSUPPORTED, "%s: env is not a literal dict of strings" % tid)
        if isinstance(kwargs.get("cwd"), str):
            t["working_dir"] = kwargs["cwd"]
        wf.find(line, "BashOperator", TRANSLATED, "%s: bash_command" % tid,
                "tasks[%s].runtime (bash -c, subprocess executor)" % tid)
        return t

    def jinja(self, script, t, line):
        wf = self.wf
        if "{{" not in script and "{%" not in script:
            return script
        needs_helper = [False]
        if re.search(r"\{%.*?%\}", script, re.S):
            wf.find(line, "Jinja", UNSUPPORTED,
                    "%s: Jinja statements ({%% ... %%}) are rendered by Airflow before the command runs; Nexus runs "
                    "the command verbatim. Rewrite them as shell" % t["id"])

        def repl(m):
            expr = m.group(1).strip()
            pm = re.fullmatch(r"params\.([A-Za-z_]\w*)|params\[\s*['\"]([^'\"]+)['\"]\s*\]", expr)
            if pm:
                name = pm.group(1) or pm.group(2)
                if name in wf.params:
                    t["inputs"][name] = {"type": "json", "required": True}
                    t["input_params"][name] = name
                    needs_helper[0] = True
                    wf.find(line, "Jinja params", APPROXIMATED,
                            "%s: {{ %s }} reads workflow param %s at run time from $NEXUS_INPUT_DIR (a JSON string's "
                            "quotes are stripped, escapes are not decoded; inside single quotes it will not "
                            "expand)" % (t["id"], expr, name), "input_params.%s" % name)
                    return "$(nexus_param %s)" % name
                wf.find(line, "Jinja params", UNSUPPORTED,
                        "%s: {{ %s }} names a param the DAG does not declare" % (t["id"], expr))
                return m.group(0)
            if expr in ("dag.dag_id", "dag_id"):
                wf.find(line, "Jinja", TRANSLATED, "%s: {{ %s }}" % (t["id"], expr), "literal %s" % wf.source_id)
                return wf.source_id
            if expr in ("task.task_id", "task_id"):
                wf.find(line, "Jinja", TRANSLATED, "%s: {{ %s }}" % (t["id"], expr), "literal")
                return t["id"].split(".")[-1]
            if expr == "run_id":
                wf.find(line, "Jinja", APPROXIMATED,
                        "%s: {{ run_id }} -> $NEXUS_RUN_ID (a UUID, not Airflow's run id)" % t["id"], "$NEXUS_RUN_ID")
                return "${NEXUS_RUN_ID}"
            if expr == "task_instance_key_str":
                wf.find(line, "Jinja", APPROXIMATED,
                        "%s: {{ task_instance_key_str }} (dag__task__date) -> dag__task__$NEXUS_RUN_ID" % t["id"],
                        "$NEXUS_RUN_ID")
                return "%s__%s__${NEXUS_RUN_ID}" % (wf.source_id, t["id"].split(".")[-1])
            head = re.split(r"[.\[( |]", expr, 1)[0]
            if head in DATE_MACROS or head == "macros":
                msg = ("{{ %s }} is the run's LOGICAL date, which Nexus does not have: a scheduled run fires at the "
                       "cron time and a backfill passes no per-tick date. Add a workflow param and pass it "
                       "(`nexusctl run create --param ...`), or compute the date in the task" % expr)
            elif head in ("ti", "task_instance") or "xcom_pull" in expr:
                msg = "{{ %s }} pulls an XCom in a template; pass the upstream output as a task input instead" % expr
            elif head in ("var", "conn"):
                msg = "{{ %s }} reads Airflow Variables/Connections; Nexus has neither" % expr
            else:
                msg = "{{ %s }} is not translated" % expr
            wf.find(line, "Jinja", UNSUPPORTED, "%s: %s" % (t["id"], msg))
            return m.group(0)

        out = re.sub(r"\{\{(.*?)\}\}", repl, script, flags=re.S)
        return HELPER + out if needs_helper[0] else out

    def python_operator(self, tid, line, short, kwargs):
        wf = self.wf
        fn = kwargs.get("python_callable")
        if not isinstance(fn, FuncRef):
            wf.find(line, short, UNSUPPORTED,
                    "%s: python_callable is not a function defined in this file (%s)" % (tid, describe(fn)),
                    "tasks[%s] (placeholder)" % tid)
            return self.placeholder(tid, line, "python_callable is not statically known")
        t = self.python_task(tid, line, fn)
        a = fn.node.args
        names = [p.arg for p in (getattr(a, "posonlyargs", []) + a.args)]
        opargs = kwargs.get("op_args") or []
        opkw = kwargs.get("op_kwargs") or {}
        if not isinstance(opargs, list) or not isinstance(opkw, dict):
            wf.find(line, short, UNSUPPORTED, "%s: op_args/op_kwargs are not literals" % tid)
            opargs, opkw = [], {}
        bound = {}
        for i, v in enumerate(opargs):
            if i < len(names):
                bound[names[i]] = v
            else:
                wf.find(line, "op_args", UNSUPPORTED, "%s: more op_args than %s has parameters" % (tid, fn.name))
        bound.update(opkw)
        for name, v in bound.items():
            self.bind_input(t, name, v, line)
        if kwargs.get("templates_dict") is not None:
            wf.find(line, "templates_dict", UNSUPPORTED, "%s: templates_dict is rendered by Airflow's Jinja" % tid)
        self.context_params(t, fn, set(bound))
        where = {"PythonVirtualenvOperator": "a virtualenv", "ExternalPythonOperator": "an external interpreter"}
        if short in where:
            wf.find(line, short, APPROXIMATED,
                    "%s: Airflow runs the callable in %s; Nexus runs it in the task's own environment (install its "
                    "requirements in the worker image)" % (tid, where[short]))
        return t

    def context_params(self, t, fn, bound):
        a = fn.node.args
        ctx = [p.arg for p in (getattr(a, "posonlyargs", []) + a.args + a.kwonlyargs)
               if p.arg in CONTEXT_PARAMS and p.arg not in bound]
        if a.kwarg is not None:
            ctx.append("**" + a.kwarg.arg)
        if ctx:
            self.wf.find(fn.node.lineno, "task context", APPROXIMATED,
                         "%s: Airflow injects the task context (%s); Nexus does not -- use the NEXUS_* environment "
                         "or nexus.TaskContext" % (t["id"], ", ".join(ctx)))

    def pod_task(self, tid, line, short, kwargs):
        wf = self.wf
        image = kwargs.get("image")
        if not isinstance(image, str):
            wf.find(line, short, UNSUPPORTED, "%s: image is not a literal string" % tid,
                    "tasks[%s] (placeholder)" % tid)
            return self.placeholder(tid, line, "image is not statically known")
        t = new_task(tid, line, "container")
        t["executor"] = "kubernetes"
        t["image"] = image
        for name, field in (("cmds", "command"), ("arguments", "args")):
            v = kwargs.get(name)
            if v is None:
                continue
            if isinstance(v, list) and all(isinstance(x, str) for x in v):
                if any("{{" in x for x in v):
                    wf.find(line, "Jinja", UNSUPPORTED,
                            "%s: %s contain Jinja templates, which a container command cannot evaluate" % (tid, name))
                t[field] = list(v)
            else:
                wf.find(line, name, UNSUPPORTED, "%s: %s is not a literal list of strings" % (tid, name))
        env = kwargs.get("env_vars")
        if isinstance(env, dict) and all(isinstance(x, str) for x in env.values()):
            t["env"] = dict(env)
        elif isinstance(env, list):
            for e in env:
                if isinstance(e, Obj) and isinstance(e.kwargs.get("name"), str) \
                        and isinstance(e.kwargs.get("value"), str):
                    t["env"][e.kwargs["name"]] = e.kwargs["value"]
                else:
                    wf.find(line, "env_vars", UNSUPPORTED, "%s: an env var is not a literal name/value" % tid)
        res = kwargs.get("container_resources", kwargs.get("resources"))
        if isinstance(res, Obj):
            self.k8s_resources(t, res.kwargs.get("requests"), res.kwargs.get("limits"), line)
        elif isinstance(res, dict):
            self.k8s_resources(t, res.get("requests"), res.get("limits"), line)
        wf.find(line, short, TRANSLATED, "%s: image, cmds, arguments" % tid,
                "tasks[%s].runtime (kubernetes executor)" % tid)
        for k in POD_UNSUPPORTED:
            v = kwargs.get(k)
            if v in (None, False, [], {}):
                continue
            hint = {"secrets": "; Nexus SecretRefs are not resolved by workers yet, so provide the value through "
                               "the worker's environment",
                    "do_xcom_push": "; write the value to $NEXUS_OUTPUT_DIR/result instead of "
                                    "/airflow/xcom/return.json",
                    "image_pull_secrets": "; configure NEXUS_K8S_IMAGE_PULL_SECRETS on the agent"}.get(k, "")
            wf.find(line, k, UNSUPPORTED, "%s: %s is not translated by the Nexus kubernetes executor%s"
                    % (tid, k, hint))
        ignored = [k for k in POD_IGNORED if k in kwargs]
        if ignored:
            wf.find(line, "pod settings", APPROXIMATED, "%s: %s are managed by the Nexus kubernetes executor; "
                                                        "ignored" % (tid, ", ".join(ignored)))
        return t

    def docker_task(self, tid, line, kwargs):
        wf = self.wf
        image = kwargs.get("image")
        if not isinstance(image, str):
            wf.find(line, "DockerOperator", UNSUPPORTED, "%s: image is not a literal string" % tid)
            return self.placeholder(tid, line, "image is not statically known")
        t = new_task(tid, line, "container")
        t["executor"] = "docker"
        t["image"] = image
        cmd = kwargs.get("command")
        if isinstance(cmd, str):
            t["command"] = ["sh", "-c"]
            t["args"] = [cmd]
            wf.find(line, "command", APPROXIMATED,
                    "%s: a string command runs through sh -c (Docker splits it without a shell)" % tid)
        elif isinstance(cmd, list) and all(isinstance(x, str) for x in cmd):
            t["command"] = list(cmd)
        env = kwargs.get("environment")
        if isinstance(env, dict) and all(isinstance(x, str) for x in env.values()):
            t["env"] = dict(env)
        wf.find(line, "DockerOperator", TRANSLATED, "%s: image, command" % tid,
                "tasks[%s].runtime (docker executor)" % tid)
        for k in ("mounts", "volumes", "network_mode", "private_environment", "docker_url", "privileged", "cap_add",
                  "devices", "device_requests", "shm_size", "user"):
            if kwargs.get(k) not in (None, False, [], {}):
                wf.find(line, k, UNSUPPORTED, "%s: %s is not translated" % (tid, k))
        return t

    def k8s_resources(self, t, req, lim, line):
        r = {}
        for key, d in (("requests", req), ("limits", lim)):
            if d is None:
                continue
            if not isinstance(d, dict):
                self.wf.find(line, "resources", UNSUPPORTED, "%s: resources are not a literal dict" % t["id"])
                continue
            r[key] = {k: str(v) for k, v in d.items() if isinstance(v, (str, int, float))}
        if r:
            t["k8s_resources"] = r

    # ── TaskFlow ────────────────────────────────────────────────────────────
    def call_task(self, f, args, kwargs, line, overrides):
        wf = self.wf
        if wf is None:
            return Unknown("a task called outside a DAG", line)
        opts = {k: v[0] for k, v in f.opts.items() if k != "**"}
        opts.update(overrides or {})
        base = opts.get("task_id")
        if not isinstance(base, str):
            base = f.name
        tid = wf.unique_id(self.full_id(base))
        if self.dynamic:
            kind, dline, _ = self.dynamic[-1]
            t = self.placeholder(tid, line, "a task call inside a %s at line %d; not unrolled" % (kind, dline))
            ref = self.register(t)
            self.wire_refs(t, args, kwargs, line)
            return ref
        variant = f.variant
        if variant in ("branch", "short_circuit", "branch_virtualenv", "branch_external_python", "sensor"):
            why = ("@task.sensor waits for an external condition; Nexus has no sensor primitive" if variant == "sensor"
                   else "@task.%s chooses downstream branches at run time; Nexus DAGs have no branching or "
                        "skipping" % variant)
            ref = self.unsupported_task(base, line, why)
            self.wire_refs(ref.task, args, kwargs, line)
            return ref
        if variant == "bash":
            t = self.taskflow_bash(tid, f, line)
            if t is None:
                ref = self.unsupported_task(base, line, "@task.bash builds its command at run time; only a literal "
                                                        "returned string is translated")
                self.wire_refs(ref.task, args, kwargs, line)
                return ref
        elif variant in ("python", "virtualenv", "external_python", "docker", "kubernetes"):
            t = self.python_task(tid, line, f)
            if variant in ("docker", "kubernetes"):
                image = opts.get("image")
                if isinstance(image, str):
                    t["executor"] = variant
                    t["image"] = image
                    wf.find(line, "@task.%s" % variant, APPROXIMATED,
                            "%s: runs in %s with the %s executor; the image must contain the Nexus SDK and the "
                            "handler module" % (tid, image, variant))
                else:
                    wf.find(line, "@task.%s" % variant, UNSUPPORTED, "%s: image is not a literal" % tid)
            elif variant != "python":
                wf.find(line, "@task.%s" % variant, APPROXIMATED,
                        "%s: runs in the task's own environment, not a %s" % (tid, variant))
        else:
            ref = self.unsupported_task(base, line, "@task.%s is not translated" % variant)
            self.wire_refs(ref.task, args, kwargs, line)
            return ref
        bound = self.bind_call(t, f, args, kwargs, line, context=CONTEXT_PARAMS)
        if t["kind"] == "python":
            self.context_params(t, f, set(bound))
        own = {k: v for k, v in opts.items() if k not in TASKFLOW_OPTS}
        self.apply_policy(wf, t, own, tid, f.deco_line)
        if opts.get("multiple_outputs"):
            wf.find(f.deco_line, "multiple_outputs", APPROXIMATED,
                    "%s: multiple_outputs: each key read downstream is a separate Nexus output" % tid, "outputs")
        return self.register(t)

    def taskflow_bash(self, tid, f, line):
        body = [s for s in f.node.body if not (isinstance(s, ast.Expr) and isinstance(s.value, ast.Constant))]
        if len(body) == 1 and isinstance(body[0], ast.Return) and isinstance(body[0].value, ast.Constant) \
                and isinstance(body[0].value.value, str):
            t = new_task(tid, line, "shell")
            t["interpreter"] = "bash"
            t["script"] = self.jinja(body[0].value.value, t, line)
            self.wf.find(line, "@task.bash", TRANSLATED, "%s: literal command" % tid, "tasks[%s].runtime" % tid)
            return t
        return None

    def mapped(self, f, partial_kw, expand_kw, overrides, line):
        wf = self.wf
        if wf is None:
            return Unknown("mapping outside a DAG", line)
        expand_kw = {k: v for k, v in expand_kw.items() if k != "**"}
        ref = self.call_task(f, [], dict(partial_kw), line, overrides)
        t = ref.task
        if t["kind"] == "placeholder":
            return ref
        if len(expand_kw) != 1:
            wf.find(line, "expand", UNSUPPORTED,
                    "%s: expand() over %d arguments maps over their cross product; a Nexus map expands one input"
                    % (t["id"], len(expand_kw)))
            return ref
        name, src = list(expand_kw.items())[0]
        self.make_map(t, name, src, line, "expand")
        return ref

    def call_group(self, f, args, kwargs, line, sc):
        wf = self.wf
        if wf is None:
            return Unknown("a task group outside a DAG", line)
        if self.dynamic:
            return self.unsupported_task(f.name, line, "a task group called inside a loop or condition")
        opts = {k: v[0] for k, v in f.opts.items() if k != "**"}
        gid = opts.get("group_id", f.name)
        if not isinstance(gid, str):
            gid = f.name
        full = ".".join(self.groups + [gid])
        if full in wf.groups:
            n = 1
            while "%s__%d" % (full, n) in wf.groups:
                n += 1
            gid = "%s__%d" % (gid, n)
            full = ".".join(self.groups + [gid])
        wf.groups[full] = []
        self.groups.append(gid)
        inner = Scope(sc)
        a = f.node.args
        pos = [p.arg for p in (getattr(a, "posonlyargs", []) + a.args)]
        for i, v in enumerate(args):
            if i < len(pos):
                inner.set(pos[i], v)
        for k, v in kwargs.items():
            inner.set(k, v)
        try:
            r = self.body(f.node.body, inner)
        finally:
            self.groups.pop()
        wf.find(line, "@task_group", TRANSLATED, "task group %s flattened" % full, "node ids %s.*" % full)
        if isinstance(r, Signal) and r.value is not None:
            return r.value
        return GroupRef(wf, full)
