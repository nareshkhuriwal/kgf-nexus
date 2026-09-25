# ADR-0016: Inline-vs-blob result threshold stays 64 KiB, configurable within [1 B, 1 MiB]

- **Status:** Proposed — pending owner sign-off (implemented on the proposing branch)
- **Date:** 2026-09-24
- **Deciders:** platform team
- **Closes:** ARCHITECTURE.md §12 question 4
- **Refines:** ADR-0008

## Context

ADR-0008 put task payloads in an S3-compatible store and allowed small results
inline in `task_attempts.output_ref`, proposing 64 KiB "pending validation
against real payload distributions". The code already made the value
configurable -- `NEXUS_INLINE_THRESHOLD_BYTES` on the gateway, sent to agents in
`RegisterResponse.result_store.inline_threshold_bytes` -- but with three gaps
found while measuring:

1. **No bounds.** Any value was accepted and forwarded. Inline bytes ride the
   gRPC `ReportResult` (the gateway caps a received message at 4 MiB) and are
   stored base64 in JSONB on the hottest table in the system.
2. **The no-store path ignored the configured value.** A worker with no object
   store compared against the compiled-in 64 KiB constant, so an operator who
   *lowered* the threshold still had those workers inlining up to 64 KiB.
3. **0 meant two things.** proto3 cannot tell 0 from unset; the agent maps
   unset to the default, so configuring 0 ("never inline") silently delivered
   64 KiB.

## Evidence

**Payload distribution.** This repository has no production traffic, so the
only corpus is what it ships; it is recorded as the evidence that exists, not
as a representative sample:

| Source | Results produced |
|---|---|
| `examples/*/compiled.yaml` (46 tasks: dispatch_storm 26, ten_node_mixed 10, idempotent_effect 4, hello_dag 3, chaos_effect 3) | shell `echo` tasks with no `stdout_output` and no output file: **0 bytes** |
| SDK example handlers (`examples/*/dag.py`), JSON-serialized | largest `hello_dag.extract` (10 rows) **281 B**; `ten_node_mixed.ingest` 85 B; model records ~55 B |
| DAG-engine literal inputs (`services/dag-engine/engine.go`) | stored inline, sized by the spec author; bounded by the spec body limit |

Every observed result is under 1 KiB -- two orders of magnitude below the
threshold. The decision therefore rests on the cost curve, not on the
distribution.

**Cost curve** (`make bench-inline`, `tests/bench/inline_test.go`): one
producer → one consumer round trip, 200 iterations per size, incompressible
payloads (so TOAST compression cannot flatter inline), Postgres 16 and MinIO in
Docker Desktop on an Apple M4 Pro laptop (10 vCPU / 16 GB to Docker). Inline =
INSERT + SELECT of a row whose `output_ref` carries the payload exactly as the
store encodes it (base64 in JSONB). Blob = the same with a ~200-byte reference,
plus MinIO PUT + GET.

| payload | inline p50 | inline p99 | blob p50 | blob p99 | inline row bytes |
|---:|---:|---:|---:|---:|---:|
| 64 B | 0.48 ms | 0.91 ms | 2.08 ms | 4.26 ms | 170 |
| 1 KiB | 0.52 ms | 1.01 ms | 2.12 ms | 4.19 ms | 1,452 |
| 4 KiB | 0.79 ms | 1.77 ms | 2.60 ms | 7.12 ms | 5,548 |
| 16 KiB | 1.12 ms | 2.16 ms | 2.72 ms | 6.70 ms | 21,933 |
| **64 KiB** | **2.44 ms** | **5.64 ms** | **3.31 ms** | **8.84 ms** | **87,469** |
| 256 KiB | 7.67 ms | 13.89 ms | 5.34 ms | 12.75 ms | 349,614 |
| 1 MiB | 30.56 ms | 50.63 ms | 11.76 ms | 19.84 ms | 1,398,191 |

The crossover is between 64 KiB and 256 KiB (≈ 128 KiB by interpolation).
At 64 KiB inline is still ~0.9 ms (p50) faster, and blob round trips cost a
fixed ~2 ms floor that dominates for the sub-KiB results everything here
produces. Above the crossover, inline is slower *and* bloats the attempt row
(base64 is +33 %: 64 KiB → 87 KB of JSONB).

## Options

1. **64 KiB default, bounded and consistently applied.** *Chosen.*
2. **Raise the default to 128 KiB (the measured crossover).** Rejected: saves
   under 1 ms on a payload size nothing here produces, while doubling the
   worst-case row the attempt table and every downstream read carry. The
   crossover also moves with network distance to the object store; a remote
   S3 bucket makes blobs slower and would argue for more inlining, a colocated
   MinIO less. 64 KiB sits on the safe side of both.
3. **Lower it to 4–16 KiB.** Rejected: pays the ~2 ms blob floor for results
   that inline in under 1.2 ms, and adds object-store dependency to more tasks.
4. **Keep it unbounded.** Rejected: gap 1 above.

## Decision

- Default **64 KiB** (`domain.DefaultInlineThresholdBytes`), shared by the
  gateway and the agent from one definition in `internal/domain`.
- Configurable with `NEXUS_INLINE_THRESHOLD_BYTES` on the gateway within
  **[1 B, 1 MiB]**; the gateway **refuses to start** outside that range (0 is
  rejected because it would be delivered as "unset"). 1 MiB keeps a
  multi-output report well inside the 4 MiB gRPC cap.
- The agent **clamps** whatever it receives into the same range and applies the
  configured value on the no-store path too.
- The Python SDK's in-task `ctx.store` applies the configured value too
  (addendum below); it used to keep its own 64 KiB constant.

Tests: `internal/domain/inline_test.go` (range and clamp table),
`agent/internal/worker/inline_test.go` (a 38-byte result with a 16-byte
threshold on a store-less worker is refused, not inlined; mutation-checked by
reverting to the constant, which makes it fail).

## Consequences

**Positive** -- the default is now measured, not assumed; misconfiguration fails
loudly at the gateway and is contained at the agent; one definition of the
range.

**Negative** -- the measurements are from a laptop with both stores on the
same host: absolute numbers are not production figures and the crossover will
move with real network distance. The payload corpus is small and synthetic;
revisit with real distributions (an `output_ref` size histogram query against a
production database is the cheap way) once there is traffic.

**Neutral** -- default behaviour is unchanged for every existing deployment.

## Addendum (2026-09-25): the SDK's in-task store follows the configured value

The gap recorded above is closed. The agent exports the clamped threshold to
every workload as `NEXUS_INLINE_THRESHOLD_BYTES` (subprocess, docker and
kubernetes executors; `agent/internal/executor/inline_env_test.go`), and the
Python SDK's `FileResultStore` / `InMemoryResultStore` read it through
`nexus.context.resolve_inline_threshold`: an assignment's
`result_store.inline_threshold_bytes` (carried by the replay endpoint,
ADR-0022) first, then the environment variable, then the default, clamped into
the same [1 B, 1 MiB] range as `domain.ClampInlineThreshold`.
`INLINE_MAX_BYTES` remains as a deprecated alias of the default. Tested in
`sdk/python/tests/test_values_and_typing.py` (a 16-byte threshold sends a
51-byte result to a blob, which fails if the constant is restored).
