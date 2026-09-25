# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""The local runner decides what runs next the way the platform does.

tests/parity/scenarios.json is ALSO executed by internal/dag/localparity_test.go
against the Go engine. Here it runs twice:

  * through ``nexus._dag`` with the same wave harness the Go test uses, which
    pins the port function for function; and
  * through the real ``nexus.local`` runner loop (threads, retry queue,
    expansion, fan-in) with a scripted executor, for every scenario whose
    outcome does not depend on completion order (fail_fast ones do).
"""

from __future__ import annotations

import copy
import json
import pathlib
import threading
from typing import Any

import pytest

from nexus import _dag
from nexus import local as local_mod
from nexus.context import inline_ref

FIXTURES = json.loads((pathlib.Path(__file__).parent / "parity" / "scenarios.json").read_text())["scenarios"]


def _simulate(sc: dict[str, Any]) -> tuple[str, dict[str, str], dict[str, int], dict[str, int]]:
    spec = copy.deepcopy(sc["spec"])
    spec.setdefault("edges", [])
    refs: dict[str, tuple[str, int]] = {}
    g = _dag.Graph(spec, refs)
    st: dict[str, str] = {}
    attempts: dict[str, int] = {}
    children: dict[str, int] = {}
    for _ in range(100):
        d = _dag.reconcile(g, st)
        for t in d.transitions:
            st[t.node_id] = t.to
        progressed = False
        for tmpl in d.expansions:
            progressed = True
            node = g.nodes[tmpl]
            m = node.spec["map"]
            count = sc.get("collections", {}).get(m["over_node_id"], 0)
            payload = json.dumps(list(range(count))).encode()
            try:
                items = _dag.plan_from_collection(tmpl, payload, True, int(m["max_fanout"]))
            except _dag.ExpansionFailure:
                st[tmpl] = _dag.FAILED
                continue
            children[tmpl] = len(items)
            for i, _item in enumerate(items):
                cid = _dag.child_id(tmpl, i)
                cspec = dict(node.spec, id=cid)
                cspec.pop("map")
                spec["tasks"].append(cspec)
                for e in g.edges_to.get(tmpl, []):
                    spec["edges"].append({"from_node_id": e.frm, "to_node_id": cid,
                                          "output_to_input": dict(e.output_to_input)})
                refs[cid] = (tmpl, i)
            g = _dag.Graph(spec, refs)
            st[tmpl] = _dag.RUNNING
        if progressed:
            continue
        for nid in sorted(d.frontier):
            progressed = True
            node = g.nodes[nid]
            script = sc.get("script", {}).get(nid, [])
            number = 0
            while True:
                number += 1
                attempts[nid] = attempts.get(nid, 0) + 1
                outcome = script[number - 1] if number <= len(script) else "ok"
                if outcome == "ok":
                    st[nid] = _dag.SUCCEEDED
                    break
                retry, _ = _dag.should_retry(
                    outcome, node.spec.get("idempotency") or "IDEMPOTENCY_MODE_REQUIRED", number,
                    node.spec.get("retry"),
                )
                if not retry:
                    st[nid] = _dag.FAILED
                    break
        if not progressed:
            assert d.run_state in (_dag.RUN_SUCCEEDED, _dag.RUN_FAILED, _dag.RUN_CANCELLED), st
            return d.run_state, st, attempts, children
    raise AssertionError("no terminal state")


def _check(sc: dict[str, Any], run: str, st: dict[str, str], attempts: dict[str, int],
           children: dict[str, int]) -> None:
    exp = sc["expect"]
    assert run == exp["run"], (sc["name"], st)
    for nid, want in exp.get("nodes", {}).items():
        assert st.get(nid, _dag.PENDING) == want, (sc["name"], nid)
    for nid, want in exp.get("attempts", {}).items():
        assert attempts.get(nid, 0) == want, (sc["name"], nid)
    for nid, want in exp.get("children", {}).items():
        assert children.get(nid, 0) == want, (sc["name"], nid)


@pytest.mark.parametrize("sc", FIXTURES, ids=[s["name"] for s in FIXTURES])
def test_port_matches_the_shared_fixtures(sc):
    _check(sc, *_simulate(sc))


class _ScriptedRunner(local_mod._Runner):
    """The real runner loop with the executor replaced by the fixture script."""

    def __init__(self, sc: dict[str, Any], run_dir: str) -> None:
        super().__init__(
            sc["spec"], {}, run_id="parity", run_dir=run_dir, modules=[], python="python3",
            isolation="subprocess", max_parallel=4, backoff_scale=0.0,
            fan_in_limit=_dag.DEFAULT_MAX_FAN_IN_BYTES, max_fanout_ceiling=10000,
            inline_threshold=65536, host_executor=False, seed=1, log=lambda _m: None,
        )
        self.sc = sc
        self.calls: dict[str, int] = {}
        self._calls_lock = threading.Lock()

    def preflight(self) -> None:  # tasks have no command and no handler
        pass

    def execute(self, a):  # type: ignore[no-untyped-def]
        with self._calls_lock:
            self.calls[a.node_id] = self.calls.get(a.node_id, 0) + 1
            n = self.calls[a.node_id]
        script = self.sc.get("script", {}).get(a.node_id, [])
        outcome = script[n - 1] if n <= len(script) else "ok"
        if outcome != "ok":
            return local_mod._Outcome("FAILED", error_class=outcome, message="scripted")
        count = self.sc.get("collections", {}).get(a.node_id)
        if count is not None:
            return local_mod._Outcome("SUCCEEDED", outputs={
                "ids": inline_ref(json.dumps(list(range(count))).encode(), "application/json")})
        return local_mod._Outcome("SUCCEEDED", outputs={"result": inline_ref(b"1", "application/json")})


@pytest.mark.parametrize(
    "sc", [s for s in FIXTURES if not s.get("order_dependent")],
    ids=[s["name"] for s in FIXTURES if not s.get("order_dependent")],
)
def test_runner_loop_matches_the_shared_fixtures(sc, tmp_path):
    r = _ScriptedRunner(sc, str(tmp_path))
    run = r.run(None)
    attempts = {k: len(v.attempts) for k, v in r.results.items()}
    children = {k: len(v) for k, v in r.graph.children.items()}
    _check(sc, run, r.states, attempts, children)
