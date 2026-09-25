# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""nexus.local: example conformance and executor behaviour (ADR-0022).

The conformance tests run the committed examples -- the same files
examples/run-suite.sh submits to a cluster -- and assert the outcomes those
examples themselves check: hello_dag's arithmetic, fan_out_map's fan-in of every
shard in element order, csv_sales_pipeline's row accounting (its report task
FAILS the run if a row is lost).
"""

from __future__ import annotations

import json
import os
import pathlib
import sys
from typing import Any

import pytest

from nexus import Cache, Retry, Runtime, TaskContext, Timeouts, derive_idempotency_key, task, workflow
from nexus import local
from nexus._dag import idempotency_key
from nexus.task import registry

ROOT = pathlib.Path(__file__).resolve().parents[3]
EXAMPLES = ROOT / "examples"
yaml = pytest.importorskip("yaml")


@pytest.fixture(autouse=True)
def _isolate_registry():
    registry.clear()
    yield
    registry.clear()


def _load_yaml(name: str) -> dict[str, Any]:
    doc = yaml.safe_load((EXAMPLES / name / "compiled.yaml").read_text())
    assert isinstance(doc, dict)
    return doc


def _run(spec: Any, params: dict[str, Any] | None = None, **kw: Any) -> local.LocalRunResult:
    kw.setdefault("backoff_scale", 0.0)
    kw.setdefault("out", None)
    kw.setdefault("seed", 7)
    return local.run(spec, params or {}, **kw)


def _sh(script: str, **kw: Any) -> Runtime:
    return Runtime(executor="subprocess", command=["sh", "-c"], args=[script], **kw)


# ── conformance: the committed examples ──────────────────────────────────────


def test_hello_dag_python_handlers(tmp_path):
    sys.path.insert(0, str(EXAMPLES / "hello_dag"))
    try:
        import importlib.util

        spec = importlib.util.spec_from_file_location("hello_dag_example", EXAMPLES / "hello_dag" / "dag.py")
        assert spec is not None and spec.loader is not None
        mod = importlib.util.module_from_spec(spec)
        sys.modules["hello_dag_example"] = mod
        spec.loader.exec_module(mod)
        res = _run(mod.hello_dag, {"source": "s3://example/input.csv"}, run_dir=str(tmp_path))
    finally:
        sys.path.remove(str(EXAMPLES / "hello_dag"))
        sys.modules.pop("hello_dag_example", None)
    assert res.state == "SUCCEEDED", res.summary()
    assert [n.state for n in res.nodes.values()] == ["SUCCEEDED"] * 3
    # extract emits values 0,3,...,27; transform sums them; load reports the total.
    assert res.output("transform") == {"source": "s3://example/input.csv", "total": 135, "count": 10}
    assert res.output("load") == {"written": True, "total": 135}


def test_hello_dag_compiled_yaml(tmp_path):
    res = _run(_load_yaml("hello_dag"), {"source": "demo"}, run_dir=str(tmp_path))
    assert res.state == "SUCCEEDED", res.summary()
    assert set(res.nodes) == {"extract", "transform", "load"}
    log = (pathlib.Path(res.nodes["load"].attempts[0].work_dir) / "stdout.log").read_text()
    assert "loading the transformed records" in log


def test_fan_out_map_compiled_yaml(tmp_path):
    res = _run(_load_yaml("fan_out_map"), {"source": "demo"}, run_dir=str(tmp_path))
    assert res.state == "SUCCEEDED", res.summary()
    kids = [n for n in res.nodes.values() if n.parent == "process_shard"]
    assert [k.map_index for k in sorted(kids, key=lambda k: k.map_index or 0)] == [0, 1, 2, 3, 4]
    assert res.nodes["process_shard"].state == "SUCCEEDED"
    # summarize echoes the fan-in: every shard's result, in element order.
    out = (pathlib.Path(res.nodes["summarize"].attempts[0].work_dir) / "stdout.log").read_text()
    fan_in = json.loads(out.split("summarizing ", 1)[1])
    assert fan_in == [{"rows": 42, "shard": f"shard-{i}"} for i in range(5)]
    # Each child got its OWN element and the platform-derived key of TEMPLATE+index.
    assert res.output("process_shard[3]") == {"rows": 42, "shard": "shard-3"}


def test_csv_sales_pipeline_row_accounting(tmp_path):
    rows = 1500  # ~100 KiB per region: above the inline threshold, so CSVs are blobs
    res = _run(_load_yaml("csv_sales_pipeline"), {"rows_per_region": rows, "seed": 7}, run_dir=str(tmp_path))
    assert res.state == "SUCCEEDED", res.summary()
    assert len(res.nodes) == 49
    report = res.output("report", "report")
    assert report["row_accounting"] == "ok"
    assert report["regions"] == 16
    assert report["rows"] == 16 * rows
    assert report["clean"] + report["rejected"] == report["rows"]
    csv_ref = res.nodes["generate_00"].outputs["csv"]
    assert "blob" in csv_ref, "a region CSV above the inline threshold must be stored as a blob"


def test_fan_out_map_python_source_matches_yaml(tmp_path):
    """The .py and the committed YAML are one workflow, and run the same."""
    import importlib.util

    spec = importlib.util.spec_from_file_location("fan_out_map_example", EXAMPLES / "fan_out_map" / "dag.py")
    assert spec is not None and spec.loader is not None
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    res = _run(mod.fan_out_map, {"source": "demo"}, run_dir=str(tmp_path))
    assert res.state == "SUCCEEDED", res.summary()
    assert sum(1 for n in res.nodes.values() if n.parent) == 5


# ── retries, timeouts, failures ──────────────────────────────────────────────


def test_transient_failure_is_retried_with_backoff(tmp_path):
    marker = tmp_path / "count"

    @task(task_ref="lr.flaky", retry=Retry(max_attempts=3, retry_on=["application"], initial="1s"),
          runtime=_sh(f'n=$(cat {marker} 2>/dev/null || echo 0); n=$((n+1)); echo $n > {marker}; '
                      '[ "$n" -ge 2 ] && echo \'{"ok":true}\' > "$NEXUS_OUTPUT_FILE"; [ "$n" -ge 2 ]'))
    def flaky(ctx):
        return None

    @workflow(name="lr-retry")
    def wf():
        flaky()

    res = _run(wf, run_dir=str(tmp_path / "run"), backoff_scale=0.001)
    assert res.state == "SUCCEEDED", res.summary()
    attempts = res.nodes["flaky"].attempts
    assert [a.state for a in attempts] == ["FAILED", "SUCCEEDED"]
    assert attempts[0].error_class == "ERROR_CLASS_APPLICATION"
    assert attempts[0].retry_decision == "retry_error_class_retryable"
    assert res.output("flaky") == {"ok": True}


def test_application_failure_is_terminal_and_propagates(tmp_path):
    @task(task_ref="lr.boom", runtime=_sh("echo broken >&2; exit 3"))
    def boom(ctx):
        return None

    @task(task_ref="lr.after", runtime=_sh("true"))
    def after(ctx, x):
        return None

    @task(task_ref="lr.cleanup", trigger_rule="all_done", runtime=_sh("true"))
    def cleanup(ctx, x):
        return None

    @workflow(name="lr-fail")
    def wf():
        b = boom()
        after(b)
        cleanup(b)

    res = _run(wf, run_dir=str(tmp_path))
    assert res.state == "FAILED"
    a = res.nodes["boom"].attempts
    assert len(a) == 1 and a[0].exit_code == 3 and "broken" in a[0].message
    assert a[0].retry_decision == "error_class_not_in_retry_on"
    assert res.nodes["after"].state == "UPSTREAM_FAILED"
    assert res.nodes["cleanup"].state == "SUCCEEDED"
    assert "FAILED" in res.summary() and "UPSTREAM_FAILED" in res.summary()


def test_start_to_close_timeout_kills_the_process_group(tmp_path):
    @task(task_ref="lr.slow", timeouts=Timeouts(start_to_close="1s", heartbeat="1s"),
          retry=Retry(max_attempts=2, initial="1s"),
          runtime=_sh("sleep 30 & wait"))
    def slow(ctx):
        return None

    @workflow(name="lr-timeout")
    def wf():
        slow()

    res = _run(wf, run_dir=str(tmp_path))
    assert res.state == "FAILED"
    attempts = res.nodes["slow"].attempts
    assert [a.state for a in attempts] == ["TIMED_OUT", "TIMED_OUT"]
    assert all(a.error_class == "ERROR_CLASS_TIMEOUT" for a in attempts)
    assert all(a.duration_s < 15 for a in attempts), "the grandchild sleep must be killed too"


def test_missing_executable_is_not_found(tmp_path):
    @task(task_ref="lr.nf", runtime=Runtime(command=["/definitely/not/here"]))
    def nf(ctx):
        return None

    @workflow(name="lr-nf")
    def wf():
        nf()

    res = _run(wf, run_dir=str(tmp_path))
    assert res.nodes["nf"].attempts[0].error_class == "ERROR_CLASS_NOT_FOUND"
    assert len(res.nodes["nf"].attempts) == 1


def test_run_timeout_cancels_and_times_out_the_run(tmp_path):
    @task(task_ref="lr.long", timeouts=Timeouts(start_to_close="1m", heartbeat="30s"), runtime=_sh("sleep 30"))
    def long(ctx):
        return None

    @task(task_ref="lr.next", runtime=_sh("true"))
    def nxt(ctx, x):
        return None

    @workflow(name="lr-runtimeout", run_timeout="1s")
    def wf():
        nxt(long())

    res = _run(wf, run_dir=str(tmp_path))
    assert res.state == "TIMED_OUT"
    assert res.nodes["long"].state == "CANCELLED"
    assert res.nodes["nxt"].state == "CANCELLED"


# ── fan-out / fan-in ─────────────────────────────────────────────────────────


def _mapped(max_fanout: int, n: int, child_script: str = 'cat "$NEXUS_INPUT_DIR/item" > "$NEXUS_OUTPUT_FILE"'):
    @task(task_ref="lr.list", outputs={"ids": {"type": "json"}},
          runtime=_sh(f'python3 -c "import json;print(json.dumps(list(range({n}))))" > "$NEXUS_OUTPUT_DIR/ids"'))
    def list_ids(ctx):
        return None

    @task(task_ref="lr.child", runtime=_sh(child_script))
    def child(ctx, item):
        return None

    @task(task_ref="lr.join", runtime=_sh('cp "$NEXUS_INPUT_DIR/rs" "$NEXUS_OUTPUT_FILE"'))
    def join(ctx, rs):
        return None

    @workflow(name="lr-map")
    def wf():
        join(rs=child.map(list_ids()["ids"], item="item", max_fanout=max_fanout))

    return wf


def test_map_expands_and_fans_in_in_element_order(tmp_path):
    res = _run(_mapped(10, 4), run_dir=str(tmp_path))
    assert res.state == "SUCCEEDED", res.summary()
    assert res.output("join") == [0, 1, 2, 3]


def test_map_over_max_fanout_fails_explicitly(tmp_path):
    res = _run(_mapped(3, 4), run_dir=str(tmp_path))
    assert res.state == "FAILED"
    assert res.nodes["child"].state == "FAILED"
    assert res.nodes["child"].reason.startswith("max_fanout_exceeded")
    assert not any(n.parent for n in res.nodes.values())
    assert res.nodes["join"].state == "UPSTREAM_FAILED"


def test_map_over_a_blob_collection_is_refused(tmp_path):
    res = _run(_mapped(5000, 3000), run_dir=str(tmp_path), inline_threshold=1024)
    assert res.nodes["child"].reason.startswith("collection_not_inline")


def test_fan_in_over_the_limit_fails_the_consumer_without_an_attempt(tmp_path):
    big = 'python3 -c "print(\'\\"\' + \'x\' * 40000 + \'\\"\')" > "$NEXUS_OUTPUT_FILE"'
    res = _run(_mapped(10, 3, child_script=big), run_dir=str(tmp_path), fan_in_limit=64 << 10)
    assert res.state == "FAILED"
    assert res.nodes["child"].state == "SUCCEEDED"
    join = res.nodes["join"]
    assert join.state == "FAILED" and join.attempts == []
    assert "fan_in_too_large" in join.reason


# ── memoization, outputs, isolation ──────────────────────────────────────────


def test_memoization_reuses_an_identical_result_within_the_run(tmp_path):
    counter = tmp_path / "runs"

    @task(task_ref="lr.pure", cache=Cache(enabled=True),
          runtime=_sh(f'echo x >> {counter}; cat "$NEXUS_INPUT_DIR/n" > "$NEXUS_OUTPUT_FILE"'))
    def pure(ctx, n):
        return None

    @workflow(name="lr-memo")
    def wf():
        first = pure(n=5)
        second = pure(n=5, node_id="again")
        pure(n=6, node_id="different")
        # Order the second after the first, with identical inputs.
        return first, second

    compiled = wf.compile()
    spec = json.loads(compiled.json)
    spec.setdefault("edges", []).append({"from_node_id": "pure", "to_node_id": "again", "output_to_input": {}})
    spec["edges"].append({"from_node_id": "again", "to_node_id": "different", "output_to_input": {}})
    res = _run(spec, run_dir=str(tmp_path / "run"))
    assert res.state == "SUCCEEDED", res.summary()
    hit = res.nodes["again"].attempts[0]
    assert hit.cache_hit_of == res.nodes["pure"].attempts[0].attempt_id
    assert res.nodes["different"].attempts[0].cache_hit_of == ""
    assert counter.read_text().count("x") == 2
    assert res.output("again") == 5
    assert "memoized" in res.summary()


def test_stdout_output_and_named_outputs(tmp_path):
    @task(task_ref="lr.so", outputs={"report": {"type": "string"}, "meta": {"type": "json"}},
          runtime=_sh('echo hello-from-stdout; echo \'{"k":1}\' > "$NEXUS_OUTPUT_DIR/meta"', stdout_output="report"))
    def so(ctx):
        return None

    @workflow(name="lr-so")
    def wf():
        so()

    res = _run(wf, run_dir=str(tmp_path))
    assert res.output("so", "report") == "hello-from-stdout\n"
    assert res.output("so", "meta") == {"k": 1}


def test_python_multi_output_and_inprocess_isolation(tmp_path):
    @task(task_ref="lr.py.split", outputs={"a": {"type": "json"}, "b": {"type": "json"}})
    def split(ctx: TaskContext, x: int) -> dict[str, int]:
        return {"a": x, "b": x * 2}

    @task(task_ref="lr.py.add")
    def add(ctx: TaskContext, a: int, b: int) -> int:
        assert ctx.idempotency_key == derive_idempotency_key(ctx.run_id, "add", None, 1)
        return a + b

    @workflow(name="lr-py", params={"x": {"type": "int", "required": True}})
    def wf(x: int):
        s = split(x)
        add(a=s["a"], b=s["b"])

    res = _run(wf, {"x": 4}, run_dir=str(tmp_path), isolation="inprocess")
    assert res.state == "SUCCEEDED", res.summary()
    assert res.output("add") == 12


def test_python_handler_exception_is_classified(tmp_path):
    @task(task_ref="lr.py.bad", retry=Retry(max_attempts=3))
    def bad(ctx: TaskContext) -> None:
        raise ValueError("nope")

    @workflow(name="lr-py-bad")
    def wf():
        bad()

    res = _run(wf, run_dir=str(tmp_path), isolation="inprocess")
    a = res.nodes["bad"].attempts
    assert len(a) == 1, "VALIDATION is terminal: never retried"
    assert a[0].error_class == "ERROR_CLASS_VALIDATION" and "nope" in a[0].message


def test_preflight_refuses_what_it_cannot_run(tmp_path):
    spec = {"api_version": "nexus.dev/v1", "name": "x", "tasks": [
        {"id": "d", "task_ref": "x.d", "runtime": {"executor": "docker", "image": "i", "command": ["true"]}},
        {"id": "p", "task_ref": "x.unregistered"}]}
    with pytest.raises(local.LocalRunError) as e:
        _run(spec, run_dir=str(tmp_path))
    msg = str(e.value)
    assert "docker executor" in msg and "--host-executor" in msg and "x.unregistered" in msg
    res = _run({"api_version": "nexus.dev/v1", "name": "x", "tasks": [spec["tasks"][0]]},
               run_dir=str(tmp_path / "h"), host_executor=True)
    assert res.state == "SUCCEEDED"


def test_an_unsatisfiable_input_is_refused_before_anything_runs(tmp_path):
    spec = {"api_version": "nexus.dev/v1", "name": "x", "tasks": [
        {"id": "a", "task_ref": "x.a", "inputs": {"n": {"type": "json", "required": True}},
         "runtime": {"command": ["true"]}}]}
    with pytest.raises(local.LocalRunError, match="required input 'n' has no source"):
        _run(spec, run_dir=str(tmp_path))


def test_run_json_and_events_are_written(tmp_path):
    res = _run(_load_yaml("hello_dag"), {"source": "demo"}, run_dir=str(tmp_path))
    doc = json.loads((tmp_path / "run.json").read_text())
    assert doc["state"] == "SUCCEEDED" and [n["node_id"] for n in doc["nodes"]] == ["extract", "transform", "load"]
    kinds = {json.loads(l)["kind"] for l in (tmp_path / "events.jsonl").read_text().splitlines()}
    assert {"node_state", "attempt_started", "attempt_finished"} <= kinds
    assert res.to_json()["run_id"] == doc["run_id"]


def test_cli_entrypoint(tmp_path, capsys):
    code = local.main([str(EXAMPLES / "hello_dag" / "compiled.yaml"), "--param", "source=demo",
                       "--run-dir", str(tmp_path), "--output", "json", "--backoff-scale", "0"])
    assert code == 0
    assert json.loads(capsys.readouterr().out)["state"] == "SUCCEEDED"
    assert local.main([str(tmp_path / "missing.yaml")]) == 2


# ── keys agree with the control plane (F116) ─────────────────────────────────


def test_idempotency_key_matches_the_control_plane():
    # Golden values asserted identically by internal/domain/idemkey_golden_test.go.
    want1 = "f32346b2310e2593016da7f96b7d3e3a29db83715b033b8966446983933c3755"
    want2 = "62abba5d696c14b1da10f3cd8ecbffca7740b9e073b28a1e7fea6dd956bc2933"
    assert derive_idempotency_key("run-1", "node-1", None, 1) == want1
    assert idempotency_key("run-1", "node-1", None, 1) == want1
    assert derive_idempotency_key("run-1", "tmpl", 0, 2) == want2
    assert idempotency_key("run-1", "tmpl", 0, 2) == want2
