# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Run llm_eval on this machine against an in-process mock, and check both outcomes.

    python3 examples/llm_eval/check_local.py

Two runs under the SDK's local runner (nexus.local), with the real handlers
(see examples/llm_batch_inference/check_local.py for the one substitution: the
task's /opt/nexus command is dropped and the registered handler runs through
`python -m nexus.worker`):

  1. the default model (mock-large) must SUCCEED with every grader at 1.0;
  2. mock-small must FAIL at the quality gate, naming the grader(s) below
     min_accuracy.

The mock throttles every 9th call with a 429, and the check confirms the mock
answered exactly one completion and one judgement per item.
"""

from __future__ import annotations

import copy
import importlib.util
import os
import sys
import tempfile
from typing import Any

from nexus import local
from nexus.testing import MockLLMServer

HERE = os.path.dirname(os.path.abspath(__file__))


def load_dag() -> Any:
    if "llm_eval_dag" in sys.modules:  # its handlers are registered once per process
        return sys.modules["llm_eval_dag"]
    spec = importlib.util.spec_from_file_location("llm_eval_dag", os.path.join(HERE, "dag.py"))
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def local_spec(dag: Any) -> dict:
    spec = copy.deepcopy(dag.llm_eval.compile().spec)
    for t in spec["tasks"]:
        rt = t["runtime"]
        assert rt["executor"] == "subprocess" and rt["args"] == [t["task_ref"]], rt
        assert rt["command"] == ["python3", "-m", "nexus.entrypoint", "--module", dag.MODULE], rt
        del rt["command"], rt["args"]
    return spec


def run(params: dict | None = None) -> tuple[local.LocalRunResult, Any, Any]:
    """Run the workflow; return (result, verdict or None, the mock's stats)."""
    dag = load_dag()
    with tempfile.TemporaryDirectory(prefix="llm-eval-") as tmp, \
            MockLLMServer(rate_limit_every=9, retry_after_s=0.05) as mock:
        result = local.run(local_spec(dag), {"base_url": mock.base_url, **(params or {})},
                           run_dir=tmp, backoff_scale=0, out=None)
        verdict = result.output("verdict", "verdict") if result.succeeded else None
        return result, verdict, mock.stats


def main() -> int:
    items = len(load_dag().items())
    good, verdict, stats = run()
    if not good.succeeded:
        print(good.summary(), file=sys.stderr)
    checks = {
        "mock-large: run succeeded": good.succeeded,
        "mock-large: every grader at 1.0": bool(verdict)
        and verdict["accuracy"] == {"exact": 1.0, "regex": 1.0, "judge": 1.0},
        "mock-large: one answer and one judgement per item": stats.completions == 2 * items,
        "mock-large: 429s absorbed in-task": stats.throttled > 0,
    }
    bad, _, _ = run({"model": "mock-small"})
    gate = bad.nodes["verdict"]
    message = gate.attempts[-1].message if gate.attempts else ""
    checks["mock-small: the quality gate fails the run"] = (
        bad.state == "FAILED" and gate.state == "FAILED" and "below min_accuracy=0.9" in message
    )
    for name, ok in checks.items():
        print(f"{'ok  ' if ok else 'FAIL'} {name}")
    print(f"verdict (mock-large): {verdict}")
    print(f"gate (mock-small): {message.splitlines()[-1] if message else '(no message)'}")
    return 0 if all(checks.values()) else 1


if __name__ == "__main__":
    raise SystemExit(main())
