"""hello_dag -- the smallest useful Nexus workflow: a 3-task linear DAG.

Read this first. It shows the whole shape of the platform in thirty lines:
declare tasks with resources and timeouts, wire them together by passing one
task's output into the next, and compile to a WorkflowSpec document.

The thing to notice is WHERE THE WORK HAPPENS. The @task and @workflow decorators
run in THIS process when you import this file. The body of `hello_dag` is traced
once, here, to discover the graph. The output is a JSON document; the control
plane parses that document and never imports this file (ADR-0004). That is why
the control plane needs none of the dependencies your handlers use, and why a
slow import here cannot affect anyone else's scheduling.

    nexusctl workflow compile examples/hello_dag/dag.py --show
"""

from __future__ import annotations

from nexus import Resources, TaskContext, Timeouts, task, workflow


@task(
    task_ref="hello.extract",
    resources=Resources(cpu="500m", memory="512Mi"),
    # Only start_to_close is set here; the other three keep their platform
    # defaults rather than being silently dropped. The heartbeat default matters
    # most -- it is what catches a task that has gone silent rather than slow.
    timeouts=Timeouts(start_to_close="10m"),
)
def extract(ctx: TaskContext, source: str) -> dict:
    """Read rows from a source. Pure read, so retrying is trivially safe."""
    ctx.log.info("extracting", source=source)
    ctx.heartbeat("connecting")
    return {"source": source, "rows": [{"id": i, "value": i * 3} for i in range(10)]}


@task(
    task_ref="hello.transform",
    resources=Resources(cpu="1", memory="1Gi"),
    timeouts=Timeouts(start_to_close="15m"),
)
def transform(ctx: TaskContext, data: dict) -> dict:
    """Compute over the extracted rows. No side effects, so also safe to retry."""
    rows = data["rows"]
    # progress() doubles as a heartbeat, which is what a long loop needs in order
    # not to look wedged to the control plane.
    for i, _ in enumerate(rows):
        ctx.progress((i + 1) / len(rows))
    return {"source": data["source"], "total": sum(r["value"] for r in rows), "count": len(rows)}


@task(
    task_ref="hello.load",
    resources=Resources(cpu="500m", memory="512Mi"),
    timeouts=Timeouts(start_to_close="10m"),
    # This task is the one with an external effect, so it is the one whose
    # idempotency mode is load-bearing. `required` (the default) tells the
    # platform it may auto-retry an INDETERMINATE outcome. See
    # examples/idempotent_effect for how a handler earns that claim.
    idempotency="required",
)
def load(ctx: TaskContext, summary: dict) -> dict:
    """Write the summary somewhere. Keyed on ctx.idempotency_key in a real sink."""
    ctx.log.info("loading", total=summary["total"], idem=ctx.idempotency_key[:12])
    return {"written": True, "total": summary["total"]}


@workflow(
    name="hello-dag",
    description="Three tasks in a line: extract, transform, load.",
    # `source` is a run PARAMETER, bound when a run is created, not baked into
    # the spec. Baking it in would make the spec hash depend on whoever compiled
    # it, and content addressing would stop meaning anything.
    params={"source": {"type": "string", "required": True}},
    run_timeout="1h",
)
def hello_dag(source: str):
    """extract -> transform -> load.

    Each call records a node and returns a handle to its output. Passing that
    handle to the next task is what creates the edge -- there is no separate
    `set_downstream` step, because the data dependency IS the dependency.
    """
    rows = extract(source)
    summary = transform(rows)
    return load(summary)


if __name__ == "__main__":
    # Running this file prints the document the control plane will see. Useful
    # without nexusctl installed, and identical to `workflow compile --show`.
    compiled = hello_dag.compile()
    print(compiled.to_json(indent=2))
    print(f"\nspec hash: {compiled.hash}")
