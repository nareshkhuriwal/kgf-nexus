# Fan-out and fan-in

There are two kinds of fan-out.

**Static fan-out** is ordinary graph structure: one node with several
downstream edges. Everything is known at submit time.

**Dynamic fan-out** handles a collection whose size is only known at runtime,
such as "process every file the listing step found". A task with `map` is a
**template**. It never runs itself. Once its upstreams settle, the DAG engine
reads a JSON array from an upstream output and creates one child per element.

```yaml
- id: process
  task_ref: files.process
  inputs: {key: {type: string}}
  outputs: {result: {type: json, max_bytes: 1024}}
  map:
    over_node_id: list_files     # a direct upstream (an edge must exist)
    over_output: keys            # declared type json, holding an array
    item_input: key              # receives one element per child
    max_fanout: 500              # MANDATORY ceiling
```

In the SDK: `process.map(list_files(prefix), item="key", max_fanout=500)`.

## Rules

- **`max_fanout` is mandatory.** A larger collection fails the template with
  `max_fanout_exceeded` and creates nothing. An operator ceiling (default
  10,000) also applies.
- **The collection must be inline**, at most the inline-result threshold
  (64 KiB by default). The control plane counts and slices it, and it never
  fetches blobs. Map over *references* such as object keys, not over data.
- **Expansion is atomic and happens at most once.** The template's state
  change, the expansion record, and every child and edge are written in one
  transaction. A crash leaves nothing behind. The retry derives byte-identical
  children from the same immutable upstream output.
- **Children are real nodes** (`process[0]`, `process[1]`, …). Each has its own
  attempts, retries, logs and idempotency key. `map_index` is part of the key,
  so retrying child 7 re-runs child 7's work only.
- **An empty collection** gives zero children, and the template succeeds.

## Fan-in

Downstream nodes depend on the **template**, never on individual children. A
downstream input bound to a template output receives a JSON array in
`map_index` order. Each element is the child's inline value, a
`{"result_ref": …}` for a blob, or `null` for a child that did not succeed.
In the SDK this is typed `list[R | None]`.

The template's state aggregates its children: any failed child makes it
`FAILED`, then any cancelled makes it `CANCELLED`, then any skipped makes it
`SKIPPED`, otherwise `SUCCEEDED`. The fan-in node's trigger rule sets how much
partial failure you tolerate. `all_success` is the default. Use `all_done` to reduce
over partial results, and handle the `null`s.

The control plane builds the fan-in array, so it is **bounded** by
`NEXUS_DAG_MAX_FANIN_BYTES` (default 16 MiB). Declaring `max_bytes` on the
template's output lets the gateway prove at submit time that
`max_fanout × max_bytes` fits, so an impossible fan-in fails before any child
runs.

## Not supported

- `min_success_ratio` thresholds. Use `all_done` and decide in the reducer.
- Nested maps inside one template.
- Mapping over a blob collection.

The full design is [architecture §5.4](../design/architecture.md#54-dynamic-expansion),
and the operational side is [runbook §14.1](../operations/runbook.md#141-dynamic-expansion-54).
Runnable examples are [`examples/fan_out_map`](repo:examples/fan_out_map/) and
[`examples/fanout_scale`](repo:examples/fanout_scale/).
