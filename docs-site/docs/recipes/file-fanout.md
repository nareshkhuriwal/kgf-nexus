# File fan-out

"Process every file under a prefix" is the canonical dynamic fan-out: nobody
knows how many files exist until the listing runs.

```bash
nexusctl init file-fanout files
```

```text
list_objects --> process_object x N (one per object) --> build_manifest
```

The template ([source](repo:cli/internal/templates/files/file-fanout/dag.py))
lists a prefix: a local directory, or `s3://bucket/path` once you add `boto3`
to `requirements.txt`. It processes each object in its own child and fans in a
manifest.

## The rules that keep it safe

- **Map over keys, not contents.** The collection a map expands over must be
  inline, 64 KiB by default. A list of object keys fits easily; the objects
  themselves do not
  ([why](../concepts/fan-out-fan-in.md#rules)).
- **`max_fanout` is mandatory.** Pick the most files you ever expect, with
  headroom. A larger listing fails the template explicitly rather than
  creating a million tasks.
- **Bound the fan-in.** `process_object` declares `max_bytes` on its output, so the
  gateway proves at submit time that `max_fanout × max_bytes` fits.
- **Children are independent attempts.** One bad file fails one child.
  `build_manifest` uses `all_done`, so it runs after every child has
  settled. It receives `null` for each failed child, and fails naming all of
  them at once instead of one per run.

## Shape in YAML

```yaml
tasks:
  - id: list_objects
    task_ref: files.list
    outputs: {keys: {type: json}}
    # ...
  - id: process
    task_ref: files.process
    inputs: {key: {type: string}}
    outputs: {result: {type: json, max_bytes: 1024}}
    map: {over_node_id: list_objects, over_output: keys, item_input: key, max_fanout: 1000}
  - id: manifest
    task_ref: files.manifest
    trigger_rule: all_done
    inputs: {results: {type: json}}
edges:
  - {from_node_id: list_objects, to_node_id: process}
  - {from_node_id: process, to_node_id: manifest, output_to_input: {results: result}}
```

[`examples/fan_out_map`](repo:examples/fan_out_map/) is the minimal runnable
version, including memoized children.
[`examples/fanout_scale`](repo:examples/fanout_scale/) fans out to 500 tiny
tasks and checks the sum.
