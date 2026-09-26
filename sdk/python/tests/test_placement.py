# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""Fractional GPUs and placement (ADR-0029, ADR-0030, ADR-0031) in the SDK."""

from __future__ import annotations

import pytest

from nexus import Artifact, Gpu, Placement, Resources, Runtime, SpecError, task
from nexus.canonical import normalize_enums

SHA = "sha256:" + "ab" * 32


def _spec(**kw: object) -> dict:
    @task(task_ref="p.t", runtime=Runtime(executor="docker", image="i", command=["true"]), **kw)  # type: ignore[arg-type]
    def t(ctx):  # type: ignore[no-untyped-def]
        return None

    return t.to_spec("n")  # type: ignore[attr-defined]


def test_fractional_gpu_request_compiles_to_the_wire_fields() -> None:
    wire = Resources(cpu="1", gpu=Gpu(count=0, share=0.25, share_memory="10Gi", mig={"1g.10gb": 1})).to_wire()
    req = wire["request"]
    assert req["gpu_share_milli"] == 250
    assert req["gpu_share_memory_bytes"] == 10 << 30
    assert req["mig"] == {"1g.10gb": 1}
    assert "gpu_count" not in req
    assert wire["limit"]["gpu_share_memory_bytes"] == 10 << 30


def test_gpu_share_validation() -> None:
    with pytest.raises(SpecError):
        Gpu(count=0, share=1.5)
    with pytest.raises(SpecError):
        Gpu(count=0, mig={"1g.10gb": -1})


def test_placement_wire_form_and_hash_normalisation() -> None:
    p = Placement(
        capacity="on_demand_only",
        required_labels={"nexus.io/region": "eu-west-1"},
        max_cost_tier=2,
        preferred_data_locality=["lake-eu"],
        artifacts=[Artifact(name="llama", uri="https://m.example/llama", key=SHA, size="40Gi")],
        max_preemptions=3,
    ).to_wire()
    assert p["capacity"] == "CAPACITY_PREFERENCE_ON_DEMAND_ONLY"
    assert p["artifacts"][0]["size_bytes"] == 40 << 30
    doc = {"tasks": [{"placement": p}]}
    assert normalize_enums(doc)["tasks"][0]["placement"]["capacity"] == "on_demand_only"
    # The default is omitted, so a task that says nothing hashes as before.
    assert Placement(capacity="spot_allowed").to_wire() == {}


def test_artifact_validation() -> None:
    with pytest.raises(SpecError):
        Artifact(name="m", uri="ftp://x").to_wire()
    with pytest.raises(SpecError):
        Artifact(name="m", uri="https://x", key="md5:00").to_wire()


def test_placement_accepts_a_mapping() -> None:
    p = Placement.coerce({"capacity": "spot_preferred", "artifacts": [{"name": "m", "uri": "file:///m"}]})
    assert p is not None and p.to_wire()["capacity"] == "CAPACITY_PREFERENCE_SPOT_PREFERRED"
