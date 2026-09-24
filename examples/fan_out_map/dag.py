# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""fan_out_map -- dynamic expansion (architecture §5.4) and memoization (§6.4).

`list_shards` decides at RUN time how many shards there are. `process_shard`
is a map TEMPLATE: the platform creates one child per element of the list,
each receiving its element as `shard`, then `summarize` receives the JSON array
of every child's result, in element order.

Three things worth noticing:

  * `max_fanout=50` is mandatory. If `list_shards` returns 51 items the
    `process_shard` node FAILS with reason max_fanout_exceeded and nothing is
    created -- a runaway upstream fails loudly instead of flooding the
    control plane.
  * The collection travels INLINE (it is a small list of shard names). The
    control plane never reads blob payloads, so a mapped output must fit the
    64 KiB inline threshold: map over references, not over the data.
  * `process_shard` is pure and memoized with a 1h TTL: re-running the workflow
    with the same `source` re-uses every shard's previous result instead of
    re-executing it. Each child is keyed on its own element, so changing one
    shard re-runs only that shard.

    python examples/fan_out_map/dag.py            # print the compiled spec
    nexusctl workflow submit examples/fan_out_map/compiled.yaml
    nexusctl run create fan-out-map --param source=demo
"""

from __future__ import annotations

from nexus import Cache, Resources, Runtime, TaskContext, Timeouts, task, workflow

_SMALL = Resources(cpu="250m", memory="256Mi")


@task(
    task_ref="fanout.list_shards",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="5m"),
    outputs={"shards": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        # Writes a JSON array to the declared output file; five shards here.
        args=['printf \'["shard-0","shard-1","shard-2","shard-3","shard-4"]\' > "$NEXUS_OUTPUT_DIR/shards"'],
    ),
)
def list_shards(ctx: TaskContext, source: str) -> list[str]:
    """Discover the shards of `source`. The count is only known now."""
    return [f"{source}-shard-{i}" for i in range(5)]


@task(
    task_ref="fanout.process_shard",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="10m"),
    cache=Cache(enabled=True, ttl="1h"),
    inputs={"shard": {"type": "json", "required": True}},
    outputs={"result": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        # The element arrives as the input file $NEXUS_INPUT_DIR/shard.
        args=['printf \'{"shard":%s,"rows":42}\' "$(cat "$NEXUS_INPUT_DIR/shard")" > "$NEXUS_OUTPUT_DIR/result"'],
    ),
)
def process_shard(ctx: TaskContext, shard: str) -> dict:
    """Process one shard. Pure, so memoization is valid."""
    return {"shard": shard, "rows": 42}


@task(
    task_ref="fanout.summarize",
    resources=_SMALL,
    timeouts=Timeouts(start_to_close="5m"),
    # all_done: summarize whatever succeeded; a failed shard arrives as null.
    trigger_rule="all_done",
    inputs={"results": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=['echo "summarizing $(cat "$NEXUS_INPUT_DIR/results")"'],
    ),
)
def summarize(ctx: TaskContext, results: list) -> dict:
    """Fan-in: receives one element per shard, null where a shard failed."""
    ok = [r for r in results if r is not None]
    return {"shards": len(results), "succeeded": len(ok), "rows": sum(r["rows"] for r in ok)}


@workflow(
    name="fan-out-map",
    description="List shards at run time, process each in parallel, summarize.",
    params={"source": {"type": "string", "required": True}},
    run_timeout="1h",
)
def fan_out_map(source: str):
    shards = list_shards(source)
    per_shard = process_shard.map(shards["shards"], item="shard", max_fanout=50)
    return summarize(per_shard)


if __name__ == "__main__":
    compiled = fan_out_map.compile()
    print(compiled.to_json(indent=2))
    print(f"\nspec hash: {compiled.hash}")
