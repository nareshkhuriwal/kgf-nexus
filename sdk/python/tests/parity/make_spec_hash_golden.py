# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""Regenerate the SPECS of spec_hash_golden.json (CONTRACT-FINDINGS F124).

The golden file pins one content address per spec, computed by the SERVER's
pipeline (services/api-gateway/spec.Compile). This script writes the specs:
every examples/*/dag.py workflow as the SDK compiles it (proto enum spellings),
plus hand-written documents that exercise every enum alias. It keeps the
existing hashes; fill in or refresh them from the server with

    go test ./cli/internal/spec -run TestSpecHashGolden -update-spec-hash-golden

Then `make sdk-test` and `go test ./cli/...` hold the Python SDK, nexusctl and
the gateway to those hashes.

Usage (repo root, SDK importable):  python3 sdk/python/tests/parity/make_spec_hash_golden.py
"""

from __future__ import annotations

import importlib.util
import json
import os
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[4]
GOLDEN = Path(__file__).with_name("spec_hash_golden.json")

sys.path.insert(0, str(ROOT / "sdk" / "python"))
from nexus.task import registry  # noqa: E402
from nexus.workflow import WorkflowDefinition  # noqa: E402


def compile_example(path: Path) -> list[tuple[str, dict[str, Any]]]:
    spec = importlib.util.spec_from_file_location(f"golden_{path.parent.name}", path)
    assert spec and spec.loader
    # Examples reuse task_refs; each is compiled as its own program.
    registry.clear()
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    sys.path.insert(0, str(path.parent))
    try:
        spec.loader.exec_module(module)
    finally:
        sys.path.remove(str(path.parent))
    out = []
    for name, value in sorted(vars(module).items()):
        if isinstance(value, WorkflowDefinition):
            out.append((name, value.compile().spec))
    return out


def task(**extra: Any) -> dict[str, Any]:
    return {"id": "a", "task_ref": "golden.task", "version": 1, **extra}


def handwritten() -> list[dict[str, Any]]:
    base = {"api_version": "nexus.dev/v1", "namespace": "default"}
    return [
        {"name": "aliases-proto", "source": "handwritten", "spec": {**base, "name": "aliases", "tasks": [task(
            idempotency="IDEMPOTENCY_MODE_BEST_EFFORT", trigger_rule="TRIGGER_RULE_ALL_DONE",
            retry={"max_attempts": 3, "strategy": "BACKOFF_STRATEGY_EXPONENTIAL", "initial_ms": 1000,
                   "retry_on": ["ERROR_CLASS_TRANSIENT", "ERROR_CLASS_WORKER_LOST"],
                   "no_retry_on": ["ERROR_CLASS_VALIDATION", "ERROR_CLASS_PERMISSION_DENIED"]})]}},
        {"name": "aliases-short", "source": "handwritten", "spec": {**base, "name": "aliases", "tasks": [task(
            idempotency="best_effort", trigger_rule="all_done",
            retry={"max_attempts": 3, "strategy": "exponential", "initial_ms": 1000,
                   "retry_on": ["transient", "worker_lost"],
                   "no_retry_on": ["validation_error", "permission_denied"]})]}},
        {"name": "no-enums", "source": "handwritten", "spec": {**base, "name": "plain", "tasks": [task()]}},
    ]


def main() -> None:
    old = {e["name"]: e.get("hash", "") for e in json.loads(GOLDEN.read_text())["entries"]} if GOLDEN.exists() else {}
    entries = handwritten()
    for dag in sorted(ROOT.glob("examples/*/dag.py")):
        rel = dag.relative_to(ROOT).as_posix()
        for wf, spec in compile_example(dag):
            entries.append({"name": f"{dag.parent.name}:{wf}", "source": rel, "workflow": wf, "spec": spec})
    for e in entries:
        e["hash"] = old.get(e["name"], "")
    GOLDEN.write_text(json.dumps({
        "_comment": "Content addresses computed by the gateway's spec.Compile. Regenerate: see make_spec_hash_golden.py.",
        "entries": entries,
    }, indent=1, sort_keys=True, ensure_ascii=False) + "\n")
    print(f"wrote {len(entries)} entries to {os.path.relpath(GOLDEN)}")


if __name__ == "__main__":
    main()
