# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
"""ADR-0015: the bounded ``uninterruptible`` declaration, as the SDK compiles it."""

from __future__ import annotations

import pytest

from nexus import Cancellation, SpecError, task, workflow
from nexus.task import registry


@pytest.fixture(autouse=True)
def _isolate_registry():
    registry.clear()
    yield
    registry.clear()


def _compile_one(**opts):
    # Several compilations per test re-declare the same task_ref on purpose.
    registry.clear()

    @task(task_ref="charge", idempotency="none", **opts)
    def charge(ctx):
        return 1

    @workflow(name="pay")
    def wf():
        return charge()

    return wf.compile().spec["tasks"][0]


def test_uninterruptible_with_bound_compiles_to_the_schema_shape() -> None:
    spec = _compile_one(cancellation=Cancellation(max_wait="5m"))
    assert spec["cancellation"] == {"uninterruptible": True, "max_uninterruptible_ms": 300_000}


def test_bare_true_means_uninterruptible_up_to_the_worker_ceiling() -> None:
    assert _compile_one(cancellation=True)["cancellation"] == {"uninterruptible": True}


def test_interruptible_is_omitted_so_existing_specs_keep_their_hash() -> None:
    # A spec that never mentions cancellation must compile to the same bytes it
    # did before the option existed; an explicit false must not change them either.
    baseline = _compile_one()
    assert "cancellation" not in baseline
    assert _compile_one(cancellation=False) == baseline
    assert _compile_one(cancellation=Cancellation(uninterruptible=False)) == baseline


@pytest.mark.parametrize("bad", ["2h", "0s", 3_600_001])
def test_bound_outside_one_hour_is_rejected_at_the_decorator(bad) -> None:
    with pytest.raises(SpecError, match="max_wait"):
        _compile_one(cancellation=Cancellation(max_wait=bad))


def test_bound_without_the_declaration_is_rejected() -> None:
    with pytest.raises(SpecError, match="requires uninterruptible"):
        _compile_one(cancellation=Cancellation(uninterruptible=False, max_wait="1m"))


def test_mapping_form_is_accepted() -> None:
    spec = _compile_one(cancellation={"uninterruptible": True, "max_wait": 90_000})
    assert spec["cancellation"] == {"uninterruptible": True, "max_uninterruptible_ms": 90_000}
