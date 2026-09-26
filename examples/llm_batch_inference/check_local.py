# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Run llm_batch_inference on this machine against an in-process mock, and check it.

    python3 examples/llm_batch_inference/check_local.py

The workflow runs under the SDK's local runner (nexus.local) with its real
handlers, the ones `python3 -m nexus.entrypoint --module dag.py` runs on a
stack. One substitution: each task's `runtime.command` (which names
/opt/nexus/...) is dropped, so the local runner runs the registered handler
through `python -m nexus.worker`, one subprocess per attempt. That path gives
each attempt its real attempt number, classifies the deliberate crash as
`transient` (on a stack the agent does so from the error class the entrypoint
declares), and keeps the run's result store across attempts, which is where
`ctx.checkpoint` writes when there is no workload API. The mock throttles every
7th call with a 429, so the in-task retry path runs too.

Beyond the workflow's own verdict, this asserts on the MOCK's counters, which
the workflow cannot see: the mock answered exactly one completion per record
(so no prompt was asked twice across the crash), and its token counts equal
the verdict's.
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
    if "llm_batch_inference_dag" in sys.modules:  # its handlers are registered once per process
        return sys.modules["llm_batch_inference_dag"]
    spec = importlib.util.spec_from_file_location("llm_batch_inference_dag", os.path.join(HERE, "dag.py"))
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def local_spec(dag: Any) -> dict:
    spec = copy.deepcopy(dag.llm_batch_inference.compile().spec)
    for t in spec["tasks"]:
        rt = t["runtime"]
        assert rt["executor"] == "subprocess" and rt["args"] == [t["task_ref"]], rt
        assert rt["command"] == ["python3", "-m", "nexus.entrypoint", "--module", dag.MODULE], rt
        del rt["command"], rt["args"]
    return spec


def run(params: dict | None = None, *, rate_limit_every: int = 7) -> tuple[local.LocalRunResult, Any, Any]:
    """Run the workflow; return (result, verdict or None, the mock's stats)."""
    dag = load_dag()
    with tempfile.TemporaryDirectory(prefix="llm-batch-") as tmp, \
            MockLLMServer(rate_limit_every=rate_limit_every, retry_after_s=0.05) as mock:
        result = local.run(local_spec(dag), {"base_url": mock.base_url, **(params or {})},
                           run_dir=tmp, backoff_scale=0, out=None)
        verdict = result.output("verify", "verdict") if result.succeeded else None
        return result, verdict, mock.stats


def main() -> int:
    result, verdict, stats = run()
    if not result.succeeded:
        print(result.summary(), file=sys.stderr)
        print(f"FAIL: the run ended {result.state}", file=sys.stderr)
        return 1
    records = verdict["records"]
    crash = [n for n in result.nodes.values()
             if n.parent == "answer_shard" and n.map_index == verdict["crash_shard"]]
    checks = {
        "verdict ok": verdict["ok"] is True,
        "every record answered correctly": verdict["correct"] == records,
        "crash shard resumed mid-shard": (verdict["resumed_from"] or 0) > 0,
        "crash shard failed once, then succeeded": len(crash) == 1
        and [a.state for a in crash[0].attempts] == ["FAILED", "SUCCEEDED"],
        "mock answered each prompt exactly once": stats.completions == records,
        "429s were absorbed in-task": stats.throttled > 0,
        "token totals match the mock": (stats.input_tokens, stats.output_tokens)
        == (verdict["input_tokens"], verdict["output_tokens"]),
    }
    # The verdict must be able to fail: mock-small answers ~30% of arithmetic
    # wrongly, and the run must end FAILED at verify.
    bad, _, _ = run({"model": "mock-small", "crash_shard": -1}, rate_limit_every=0)
    checks["the verdict fails a wrong model"] = (
        bad.state == "FAILED" and bad.nodes["verify"].state == "FAILED"
        and "wrong answers" in bad.nodes["verify"].attempts[-1].message
    )
    for name, ok in checks.items():
        print(f"{'ok  ' if ok else 'FAIL'} {name}")
    print(f"verdict: {verdict}")
    return 0 if all(checks.values()) else 1


if __name__ == "__main__":
    raise SystemExit(main())
