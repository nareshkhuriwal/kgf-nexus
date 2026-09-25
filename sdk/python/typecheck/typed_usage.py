# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""A typed usage example that CI type-checks (`make sdk-typecheck`).

Every `assert_type` below is a STATIC assertion: mypy --strict fails the build
if the SDK stops carrying a task's return type through tracing, `.map`,
`.with_overrides` or the workflow wiring. It also runs (pytest imports and
compiles it), so the example cannot drift from what actually works.
"""

from __future__ import annotations

from typing import Any, assert_type

from nexus import (
    CompiledWorkflow,
    OutputRef,
    Ref,
    Resources,
    Runtime,
    TaskContext,
    TaskDefinition,
    TaskInvocation,
    ValueSpec,
    WorkflowDefinition,
    task,
    workflow,
)


@task(task_ref="typed.extract", resources=Resources(cpu="250m", memory="256Mi"))
def extract(ctx: TaskContext, source: str) -> dict[str, int]:
    return {"rows": len(source)}


@task(task_ref="typed.list_ids", outputs={"ids": {"type": "json", "max_bytes": "4Ki"}})
def list_ids(ctx: TaskContext, n: int) -> list[str]:
    return [f"id-{i}" for i in range(n)]


@task(
    task_ref="typed.score",
    outputs={"score": ValueSpec(type="json", max_bytes=64)},
)
def score(ctx: TaskContext, item: str) -> float:
    return float(len(item))


@task
def total(ctx: TaskContext, scores: list[float | None], rows: dict[str, int]) -> float:
    return sum(s for s in scores if s is not None) + rows["rows"]


@task(
    task_ref="typed.shell",
    outputs={"report": {"type": "string"}},
    runtime=Runtime(executor="subprocess", command=["sh", "-c"], args=["echo ok"], stdout_output="report"),
)
def shell(ctx: TaskContext, total: float) -> str:
    raise NotImplementedError("runs as a shell workload")


# The decorators keep the handler's return type.
assert_type(extract, TaskDefinition[dict[str, int]])
assert_type(score, TaskDefinition[float])
assert_type(total, TaskDefinition[float])

# with_overrides keeps it too: same code, bigger box.
big_extract = extract.with_overrides(resources=Resources(cpu="2", memory="4Gi"))
assert_type(big_extract, TaskDefinition[dict[str, int]])


def wire_scores(ids: Ref[list[str]]) -> TaskInvocation[list[float | None]]:
    """A helper that wires tasks, annotated with the exported Ref alias."""
    return score.map(ids, item="item", max_fanout=100)


@workflow(name="typed-usage", params={"source": {"type": "string", "required": True}})
def typed_usage(source: str) -> None:
    rows = extract(source)
    assert_type(rows, TaskInvocation[dict[str, int]])
    assert_type(rows.primary, OutputRef[dict[str, int]])

    ids = list_ids(n=3)
    assert_type(ids, TaskInvocation[list[str]])
    # A named output is Any unless its type is stated.
    assert_type(ids["ids"], OutputRef[Any])
    assert_type(ids.output("ids", list), OutputRef[list[Any]])

    # A map over a traced output is a fan-in handle: one element per child,
    # None where a child did not succeed.
    scores = wire_scores(ids.output("ids", list))
    assert_type(scores, TaskInvocation[list[float | None]])

    summary = total(scores=scores, rows=rows)
    assert_type(summary, TaskInvocation[float])
    shell(total=summary)


assert_type(typed_usage, WorkflowDefinition)

# Outside a workflow body, a call with a TaskContext runs the handler and is
# typed as its return; `.map` over a concrete list returns the plain list.
ctx = TaskContext(
    run_id="r", node_id="n", attempt_id="a", attempt_number=1,
    task_ref="typed.score", task_version=1, idempotency_key="k",
)
assert_type(score(ctx, "abc"), float)
assert_type(score.map(["a", "bb"], ctx, item="item", max_fanout=5), list[float])

compiled = typed_usage.compile()
assert_type(compiled, CompiledWorkflow)
assert_type(compiled.hash, str)
