# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""The SDK's content address is the gateway's (CONTRACT-FINDINGS F124).

tests/parity/spec_hash_golden.json pins, per spec, the hash the gateway's
spec.Compile stores it under; cli/internal/spec/hash_parity_test.go holds
nexusctl and the gateway to the same file. Here: the SDK's spec_hash agrees,
and every example workflow still compiles to the spec the golden file holds,
so the file cannot silently go stale.
"""

from __future__ import annotations

import importlib.util
import json
import sys
from pathlib import Path
from typing import Any

import pytest

from nexus.canonical import canonical_bytes, normalize_enums, spec_hash
from nexus.task import registry
from nexus.workflow import WorkflowDefinition

GOLDEN = Path(__file__).parent / "parity" / "spec_hash_golden.json"
ROOT = Path(__file__).resolve().parents[3]
ENTRIES: list[dict[str, Any]] = json.loads(GOLDEN.read_text(encoding="utf-8"))["entries"]


@pytest.mark.parametrize("entry", ENTRIES, ids=[e["name"] for e in ENTRIES])
def test_sdk_hash_is_the_gateway_hash(entry: dict[str, Any]) -> None:
    assert entry["hash"], "golden hash missing; regenerate (see make_spec_hash_golden.py)"
    assert spec_hash(entry["spec"]) == entry["hash"]


def test_both_spellings_are_one_version() -> None:
    by_name = {e["name"]: e for e in ENTRIES}
    assert by_name["aliases-proto"]["hash"] == by_name["aliases-short"]["hash"]


def test_normalize_enums_does_not_touch_its_input() -> None:
    spec = next(e["spec"] for e in ENTRIES if e["name"] == "aliases-proto")
    before = canonical_bytes(spec)
    normalized = normalize_enums(spec)
    assert canonical_bytes(spec) == before
    task = normalized["tasks"][0]
    assert task["idempotency"] == "best_effort"
    assert task["trigger_rule"] == "all_done"
    assert task["retry"]["strategy"] == "exponential"
    assert task["retry"]["retry_on"] == ["transient", "worker_lost"]
    assert task["retry"]["no_retry_on"] == ["validation", "permission_denied"]
    assert normalize_enums(normalized) == normalized  # idempotent


EXAMPLES = [e for e in ENTRIES if e["source"].startswith("examples/")]


@pytest.mark.parametrize("entry", EXAMPLES, ids=[e["name"] for e in EXAMPLES])
def test_examples_compile_to_the_golden_spec(entry: dict[str, Any]) -> None:
    path = ROOT / entry["source"]
    registry.clear()
    spec = importlib.util.spec_from_file_location(f"golden_check_{path.parent.name}", path)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    sys.path.insert(0, str(path.parent))
    try:
        spec.loader.exec_module(module)
    finally:
        sys.path.remove(str(path.parent))
        registry.clear()
    wf = getattr(module, entry["workflow"])
    assert isinstance(wf, WorkflowDefinition)
    compiled = wf.compile()
    assert json.loads(compiled.json) == entry["spec"], (
        "the example changed; regenerate the golden file (make_spec_hash_golden.py, then "
        "go test ./cli/internal/spec -run TestSpecHashGolden -update-spec-hash-golden)"
    )
    assert compiled.hash == entry["hash"]
