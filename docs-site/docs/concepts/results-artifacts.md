# Results and artifacts

Task outputs never pass through the control plane
([ADR-0008](../design/adr/0008-result-store-separation.md)):

- **Small values are stored inline** in PostgreSQL, next to the attempt. The
  threshold is 64 KiB by default and configurable from 1 byte to 1 MiB with
  `NEXUS_INLINE_THRESHOLD_BYTES` on the gateway
  ([ADR-0016](../design/adr/0016-inline-result-threshold.md)). Workers are told
  the value, as `$NEXUS_INLINE_THRESHOLD_BYTES`.
- **Larger values are blobs** in an S3-compatible store. The local stack uses
  Silo, a community MinIO fork ([runbook §22.1](../operations/runbook.md#221-the-s3-store-why-silo-and-not-minio)).
  Workers upload and download them **directly**, with credentials scoped to
  their namespace prefix. A blob is content-addressed
  (`<namespace>/<sha256>`) and immutable: a retry writes a new object and never
  changes an existing one.
- Downstream tasks receive both kinds through the same interface. The
  attempt's `output_ref` records which kind a value is.

## Declaring outputs

```python
@task(outputs={"model": {"type": "file_ref"}, "metrics": {"type": "json", "max_bytes": "4Ki"}})
```

- `json`, `string`, `int`, `float` and `bool` values are passed by value.
  Large ones become blobs automatically.
- `file_ref` passes a reference to a stored object. Use it for datasets and
  models, and map over references rather than over data.
- `max_bytes` is your promise about an output's size. The SDK entrypoint
  enforces it before writing. The Go agent does not yet (CONTRACT-FINDINGS
  F125), so non-SDK executors can exceed it.

## Memoization

With `cache: {enabled: true, ttl_ms: …}` (SDK `cache=True`), a task whose
`(namespace, task_ref, version, inputs)` matches an earlier **successful,
executed** attempt within the TTL (7 days by default) is not run again. The
DAG engine inserts it directly as `SUCCEEDED`, pointing at the earlier output,
and emits a `CacheHit` event ([architecture §6.4](../design/architecture.md#64-memoization)).

- Only use it on pure tasks. A task with `idempotency: none` cannot enable it.
- Bump `version` to start a fresh cache namespace, or invalidate:
  `nexusctl cache invalidate --task-ref R [--task-version N]`. Invalidation
  deletes nothing; later runs simply execute.
- Two identical runs that miss at the same time both execute. There is no
  single-flight.

## Retrieving results

- In the console, the attempt panel shows bounded previews and downloads.
- Through the API, a run's attempts carry their `output_ref`, and
  `GET .../runs/{run}/attempts/{id}/assignment` returns an attempt's resolved
  **inputs** for [replay](../recipes/local-development.md#replay-a-failed-attempt).

## Retention and garbage collection

Results and run history are retained independently. The artifact GC sweeps
objects no row references, and the reverse check finds rows whose object has
gone ([runbook §7.7–7.8](../operations/runbook.md#77-reclaiming-orphans-the-gc-sweep)).
Backing up the result store, and why its ordering relative to the database
backup matters, is covered in [runbook §7](../operations/runbook.md#7-artifacts-backing-up-the-result-store).
