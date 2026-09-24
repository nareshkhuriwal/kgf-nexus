# ADR-0020: Durable task logs -- chunks in the result store, a control-plane index, fenced by lease

- **Status:** Proposed — pending owner sign-off
- **Date:** 2026-09-24
- **Deciders:** platform team
- **Refines:** ADR-0008 (result store), ADR-0011 (table ownership), ADR-0014 (worker transport)

## Context

`GET /v1/namespaces/{ns}/attempts/{id}/logs` has answered 501 since Phase 1:
the gateway declares a `LogStreamer` interface and `main.go` passes `nil`.
What actually happens to a task's output today, read from the code rather than
assumed:

| Executor | Where output goes | Survives the attempt? |
|---|---|---|
| subprocess | `<workdir>/combined.log` (stdout and stderr share one fd) | No -- `Cleanup` removes the work directory |
| docker | the container's json-file log | No -- `Cleanup` runs `docker rm --force` |
| kubernetes | the pod's container log | No -- `Cleanup` deletes the pod and Job |

The runner also copies up to 16 MiB of `exec.Logs()` into the *agent's own*
structured log (`runner.streamLogs`), which is neither per-attempt addressable,
nor authorized per tenant, nor retained on any schedule an operator chose. For
the subprocess executor even that copy is empty: `Logs()` opens the file once
and reads to the EOF that exists at start time.

So the product has no durable, viewable task log, and the console's log pane
and `nexusctl logs` both talk to a route that cannot work. The constraints on a
fix are the platform's existing invariants:

1. **Workers hold no database credentials** (plane separation, CONTRACT.md).
2. **Payload bytes do not transit the control plane on the write path**
   (ADR-0008 invariant 6): a task that prints gigabytes must not saturate the
   gateway and starve heartbeats.
3. **Lease fencing** (ADR-0010): a stale attempt must not be able to change
   what the control plane records.
4. **Additive wire and schema changes only** (ADR-0011 rule 5).
5. **Orphans are collected by the reachability sweep, never inline** (ADR-0008).

## Decision

### Write path: the agent writes chunks to the result store, and indexes them through a fenced RPC

The agent tees every attempt's output into a per-attempt **shipper**
(`agent/internal/logship`):

- Output is split into **records**: one line, tagged with a stream
  (`stdout`, `stderr`, `combined` when the substrate cannot separate them, or
  `nexus` for the platform's own markers), an agent timestamp, and a
  **line number `n` that is contiguous from 1 per attempt**. Lines longer
  than 32 KiB are split and marked partial. Records are NDJSON.
- Records accumulate into **chunks** sealed at 256 KiB or every 2 s while
  output is flowing (10 s after 300 chunks, so a slow logger that runs for a
  day produces thousands of chunks, not tens of thousands). Each chunk is
  written **directly to the result store** with the agent's existing
  credentials at `<namespace>/logs/<attempt_id>/<seq>.ndjson` -- under the
  namespace prefix, so namespace-scoped worker credentials keep working.
- After the object exists, the agent calls a new unary RPC,
  **`WorkerService.ReportLogs(attempt_id, lease_token, chunks[], final)`**,
  carrying only chunk *metadata* (key, seq, first line, line count, byte
  offset, size, sha256, first/last timestamp). The RPC goes through the same
  `fence()` as every other attempt RPC, so **a stale or revoked lease cannot
  add to the index**. Chunk bytes never cross the gateway.
- **Per-attempt cap** (default 64 MiB of encoded records, control-plane
  configured): past it the shipper emits a `nexus` marker, keeps a 2 MiB
  rolling tail in memory, and on completion ships a second marker with the
  omitted line and byte counts followed by that tail. Head and tail survive;
  the middle is dropped and *said to be* dropped.
- **Backpressure never reaches the workload.** Writes into the shipper never
  block; if unshipped bytes exceed 8 MiB (object store down, gateway
  unreachable) new lines are counted and dropped, and a marker records how
  many when shipping recovers. A task is never slowed or killed by its log.
- **Redaction hook.** Every record passes through a `Redactor` that replaces
  known secret values with `[REDACTED:<name>]` before anything is buffered,
  spooled or uploaded. The agent does not resolve `SecretRef`s today (nothing
  in the repository does), so the hook is wired but receives no values; the
  resolver, when it lands, must feed it. Redaction is per line: a secret that
  spans a line break or a 32 KiB split is not matched.
- **Finalization.** When the workload exits, and *before* `ReportResult`
  (while the lease is still valid), the shipper seals the last chunk, drains
  its queue, uploads a **compacted object** `<ns>/logs/<attempt_id>/log.ndjson`
  (the byte-for-byte concatenation of the chunks, from a local spool) and sends
  `ReportLogs` with `final` set. Because every chunk row records its byte
  offset in that concatenation, the index stays valid after compaction and
  reads become HTTP range requests into one object. Finalization is bounded
  (30 s); if it cannot finish, the result is reported anyway and the log is
  left `partial`, readable up to the last indexed chunk.

Old agents never call `ReportLogs`; old gateways do not advertise log shipping
in `RegisterResponse.log_shipping`, and a new agent then does not ship at all
(no objects nobody could index). Both directions of a rolling upgrade degrade
to "no log recorded", which the read API states rather than hides.

### Index: two gateway-owned tables (migration 0025)

`attempt_logs` (one row per attempt: state, totals, truncation/drop counts,
compacted key) and `attempt_log_chunks` (one row per chunk, PK
`(attempt_id, seq)`). Both cascade from `task_attempts`, so deleting a run
deletes its log index, which is what makes the GC's reachability rule work for
logs. Both carry `tenant_id`, assigned by the 0014 trigger.

The index is owned by the **api-gateway** -- an addition to ADR-0011's matrix,
recorded here rather than by rewriting that accepted record. The gateway
terminates the worker RPC that writes it and serves the API that reads it; the
rows are not part of the dispatch transaction and have no atomicity
requirement with any other service's table, so routing them through the
scheduler would add a hop and a proto method for no guarantee.

Idempotency: `ON CONFLICT (attempt_id, seq) DO NOTHING`. A redelivered chunk
(RPC retried after a lost ack) is a no-op; the summary counters are advanced
only by rows actually inserted, in the same transaction.

Server-side validation, because the index is written from outside the control
plane: every key must be under `<attempt's namespace>/logs/<attempt_id>/`
(otherwise a worker could point the index at another tenant's object and read
it through the log API); sizes, counts and seq are bounded; chunks after
`final` are refused with `log_finalized`.

### Read path: the gateway implements LogStreamer over index + object store

The gateway gains read access to the result store (`NEXUS_S3_*`, the same
variables the agent reads). It reads only keys the index names.

- **Tail** `?tail=N` (≤ 10 000), **page** `?from_line=K&limit=L` (≤ 5 000),
  both JSON: `{log: {state, …}, lines: [{n, ts, stream, message}], next_line,
  has_more}`.
- **Follow**: `Accept: text/event-stream` or `?stream=true` → SSE with
  `id: <n>`; resume via `Last-Event-ID` or `from_line`. Closes with an `end`
  event once the attempt is terminal and the log is complete (or has been
  silent for 30 s after the attempt ended). `Accept: text/plain` streams raw
  lines, with `follow=true` for the CLI and SDK.
- **Search** `?q=…` (substring, optional `regex=true` with Go RE2 -- linear
  time, no catastrophic backtracking -- pattern ≤ 512 bytes,
  `case=insensitive`, `stream=`): scans forward from `from_line`, bounded by
  matches (≤ 1 000), bytes (default 64 MiB, max 256 MiB) and 15 s; returns
  `next_line` to continue and `scan_complete`. Bounds make a search over a
  64 MiB log cheap and over a pathological one merely incomplete, never an
  outage.
- **States** are explicit: `none` (nothing recorded yet), `live`, `complete`,
  `partial` (attempt ended without a final report: agent crash, LOST lease,
  object-store outage -- whatever was indexed is readable), `expired`
  (index rows exist, objects are gone: retention or out-of-band deletion).
  Unreadable or missing chunks become `nexus` gap lines naming the missing
  line range; nothing is silently skipped.
- **Authorization**: the route stays behind `guard()` with `tasks.logs.view`;
  the attempt is resolved namespace-scoped *before* anything is read; the SSE
  loop re-authenticates on every poll like the event stream does.

### Retention: artifact-gc is log-aware

`artifactgc.LiveKeys` gains two reference sources: chunk keys of logs that are
not compacted, and compacted keys. Everything else under `<ns>/logs/` --
chunks superseded by compaction, objects a fenced (stale) attempt uploaded but
could not index, uploads whose RPC never landed -- is an ordinary orphan,
collected after the grace period. The list-then-query ordering argument holds
unchanged because the object is always written before the row that names it.

Optional **log retention** (`artifact-gc -log-retention 720h`, default off):
logs finalized (or last written) longer ago than that drop out of the live set
and are collected; the index rows remain, so the API reports `expired` instead
of an empty log. Retention is safe for logs specifically because nothing
downstream consumes a log -- which is not true of outputs, so outputs keep
pure reachability.

## Consequences

**Positive**
- Logs survive the workload, the agent and the node; they are per attempt, so
  retries keep their own evidence.
- The write path adds no payload bytes to the control plane; the gateway's
  worker port carries only small metadata RPCs.
- Fencing, idempotency, tenancy and retention reuse mechanisms that are
  already proven (fence(), ON CONFLICT, the 0014 trigger, the GC sweep).
- A crashed agent leaves a readable partial log up to its last indexed chunk.

**Negative**
- Live tail latency is the flush interval plus an object PUT plus an RPC
  (≈2–3 s), not a pipe. Acceptable for an operator console; not a terminal.
- The gateway now holds read credentials to the result store. It already
  held the endpoint; scoping the credential to `*/logs/*` is recommended
  (OPERATIONS).
- A second copy of the workload's stdout/stderr ordering is approximate once
  the streams are separated (two pipes), exactly as `docker logs` is.
- The subprocess executor's capture path changes when shipping is enabled
  (agent-owned pipes instead of a shared fd). With shipping disabled it is
  byte-for-byte the old behaviour.
- One more table pair the gateway owns, and one more worker RPC to keep
  compatible.

**Neutral**
- Kubernetes pod logs do not separate streams (the split-streams API is
  alpha), so those records are tagged `combined`.
- Redaction is a hook with no production input until secret resolution
  exists; this is recorded as a known limit, not claimed as a feature.

## Alternatives considered

**Stream chunks through the worker gRPC channel and let the gateway write the
object store.** One credential fewer on the worker. Rejected: it puts every
byte of every log through the gateway's worker port -- the heartbeat path --
which is precisely the chokepoint ADR-0008 exists to avoid, and it would make
the 4 MiB message cap a log-size concern.

**No database index; list `<ns>/logs/<attempt>/` at read time.** Simpler, no
migration. Rejected: an object store cannot fence, so a zombie attempt's
writes would be served as the attempt's log; listing is also eventually
consistent on some S3 implementations and costs a LIST per page view.

**Logs in Postgres.** Rejected for the same reasons ADR-0008 rejected outputs
in Postgres, and more strongly: logs are larger and far less valuable per
byte than outputs.

**An external log system (Loki, Elasticsearch).** Better search. Rejected as a
platform dependency: the brief requires standard databases and object stores,
and a deployment that wants Loki can still scrape agent output. The index
format here does not preclude exporting to one later.

**Index rows owned by the scheduler** (it owns `task_attempts`). Rejected:
it would need a scheduler RPC per chunk batch for no transactional benefit;
the rows share no invariant with scheduler state beyond the FK cascade.
