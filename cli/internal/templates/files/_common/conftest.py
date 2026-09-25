"""Shared test fixtures: a real TaskContext for calling handlers directly.

Calling a @task function outside a workflow body runs the handler, so tests
exercise the real code. The context is a real TaskContext (not a mock) so an
idempotency bug that depends on `ctx.idempotency_key` shows up here.
"""

from __future__ import annotations

import itertools

import pytest

from nexus import InMemoryResultStore, TaskContext, derive_idempotency_key

_attempts = itertools.count(1)


@pytest.fixture
def make_ctx():
    def make(node_id: str = "node", *, run_id: str = "run-test", attempt_number: int = 1) -> TaskContext:
        return TaskContext(
            run_id=run_id,
            node_id=node_id,
            attempt_id=f"attempt-{next(_attempts)}",
            attempt_number=attempt_number,
            task_ref=f"__WORKFLOW_NAME__.{node_id}",
            task_version=1,
            # Stable across attempts of the same node, exactly as the platform
            # derives it: that is what makes a retried effect deduplicable.
            idempotency_key=derive_idempotency_key(run_id, node_id, None, 1),
            store=InMemoryResultStore(),
        )

    return make
