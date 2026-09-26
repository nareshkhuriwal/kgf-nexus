# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Declared asset lineage on tasks (ADR-0028): produces= / consumes=."""

from __future__ import annotations

import json
import pathlib

import pytest

from nexus import Asset, SpecError, task, workflow
from nexus.task import registry


@pytest.fixture(autouse=True)
def _isolate_registry():
    registry.clear()
    yield
    registry.clear()


def _spec_task(compiled, node_id):
    return next(t for t in compiled.spec["tasks"] if t["id"] == node_id)


def test_produces_and_consumes_compile_to_task_assets() -> None:
    @task(task_ref="extract", consumes="s3://raw/orders",
          produces=[Asset("postgres://dw/public.orders", name="Orders"), {"uri": "s3://lake/orders/"}])
    def extract(ctx):
        return 1

    @task(task_ref="report", consumes=["postgres://dw/public.orders"])
    def report(ctx, x):
        return x

    @workflow(name="orders")
    def wf():
        return report(extract())

    c = wf.compile()
    e = _spec_task(c, "extract")
    assert e["assets"] == {
        "produces": [{"uri": "postgres://dw/public.orders", "name": "Orders"}, {"uri": "s3://lake/orders/"}],
        "consumes": [{"uri": "s3://raw/orders"}],
    }
    assert _spec_task(c, "report")["assets"] == {"consumes": [{"uri": "postgres://dw/public.orders"}]}


def test_a_task_without_assets_has_no_assets_key() -> None:
    @task(task_ref="plain")
    def plain(ctx):
        return 1

    @workflow(name="plain")
    def wf():
        return plain()

    assert "assets" not in _spec_task(wf.compile(), "plain")


@pytest.mark.parametrize("bad", ["orders", "s3://has space", {"uri": "s3://b/k", "owner": "me"}, 42])
def test_invalid_assets_fail_at_the_decorator(bad) -> None:
    with pytest.raises(SpecError):
        @task(task_ref="bad", produces=[bad])
        def bad_task(ctx):
            return 1


def test_duplicate_assets_are_refused() -> None:
    with pytest.raises(SpecError, match="twice"):
        @task(task_ref="dup", consumes=["s3://a/b", "s3://a/b"])
        def dup(ctx):
            return 1


def test_with_overrides_keeps_assets() -> None:
    @task(task_ref="t", produces="s3://out/x")
    def t(ctx):
        return 1

    t2 = t.with_overrides(queue="gpu")
    assert t2.to_task_spec("t", {})["assets"] == {"produces": [{"uri": "s3://out/x"}]}


def test_schema_accepts_the_compiled_shape() -> None:
    schema = json.loads((pathlib.Path(__file__).resolve().parents[3] / "schemas" / "workflow-spec-v1.json").read_text())
    ref = schema["$defs"]["TaskSpec"]["properties"]["assets"]["$ref"]
    assert ref == "#/$defs/TaskAssets"
    assert set(schema["$defs"]["TaskAssets"]["properties"]) == {"produces", "consumes"}
    assert schema["$defs"]["AssetRef"]["required"] == ["uri"]
