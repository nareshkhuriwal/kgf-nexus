# ADR-0026: OpenTelemetry tracing across every hop, stored with the work it describes

- **Status:** Proposed — pending owner sign-off
- **Date:** 2026-09-26
- **Deciders:** platform team
- **Refines:** ARCHITECTURE.md §9 ("OpenTelemetry traces spanning submit →
  dispatch → execute → report"), ADR-0019 (metrics), ADR-0003 (outbox)

## Context

ARCHITECTURE §9 promised structured logs with `trace_id` on every line and
OpenTelemetry traces from submit to report. Neither existed (F140): no service
created a span, no hop carried a `traceparent`, and no log line had a trace id.
A slow run could be diagnosed only by joining timestamps across six services'
logs by hand.

Following one run is harder here than in a request/response system, because
the causal chain crosses three **asynchronous** boundaries that no request
carries anything through:

1. **The transactional outbox.** A fact commits in one service; the
   event-service relay publishes it to NATS later, from its own loop.
2. **The attempt row.** The dag-engine inserts a PENDING attempt inside the
   request that made its node ready; a worker's *poll* claims it seconds or
   hours later, in the poll's own context.
3. **The task process.** The agent runs a subprocess, a container or a
   Kubernetes pod that shares no memory with it.

## Decision

**The official OpenTelemetry Go SDK** (`go.opentelemetry.io/otel` v1.46.0,
contrib `otelgrpc` v0.71.0, `github.com/exaring/otelpgx` v0.12.0, pinned in
`go.mod`) in every service and the agent, set up by one package,
`internal/tracing`.

**Configuration is the standard `OTEL_*` environment only.** OTLP over gRPC
or HTTP/protobuf, headers, TLS, timeouts, batch-processor limits, resource
attributes: all the specification's own variables, so an operator's existing
collector configuration applies unchanged.

**Off by default.** Tracing turns on only when `OTEL_TRACES_EXPORTER` names an
exporter or an OTLP endpoint variable is set (`OTEL_SDK_DISABLED=true` forces
it off). The specification's default exporter is `otlp`, which would make every
unconfigured binary retry `localhost:4318` forever. A malformed exporter,
protocol or sampler **fails startup**: a typo must not silently switch tracing
off.

**Sampler: parent-based ratio** (`parentbased_traceidratio`, ratio from
`OTEL_TRACES_SAMPLER_ARG`, default 1.0; the chart defaults to 0.1). The
service that starts a trace decides once; every later hop follows the sampled
flag, so traces are whole or absent, never holed.

**Propagation is always on**, W3C `traceparent`/`tracestate` plus baggage,
even with tracing off, so a partially traced deployment still produces
connected traces.

### The hops

| Hop | Carrier | Where |
|---|---|---|
| REST ingress | `traceparent` header; server span named by the route template (`POST /v1/namespaces/{ns}/runs`) | `tracing.HTTPMiddleware` |
| every gRPC call (internal peers, worker API) | gRPC metadata via `otelgrpc` stats handlers; health and reflection filtered | `metrics.ServerOptions`, `config.PeerDialOptions`, the agent's dial |
| database | one client span per query/batch/acquire; parameters never recorded | `otelpgx` on every pool |
| outbox → NATS | `outbox.trace_context` (migration 0028) → producer span, child of the writer → NATS headers | `internal/store`, the relay |
| attempt row → dispatch | `task_attempts.trace_context` (0028), set by the dag-engine's reconcile (a retry inherits the failed attempt's) | dag-engine, retry timer |
| dispatch → agent | `TaskAssignment.trace_context` (additive proto field 11) | scheduler `attempt.dispatch` span |
| agent → task process | `TRACEPARENT` / `TRACESTATE` environment (OTel env-carrier convention) in all three executors | agent runner |
| task → user code | Python SDK `opentelemetry` extra: the handler runs in a child span | `sdk/python/nexus/tracing.py` |

**The dispatch span is parented on the attempt's stored trace, not on the
poll.** The poll is linked. An operator following a run wants the claim, the
execution and the result under the run; a trace per idle poll is where nobody
looks. The same rule applies to the relay: each published message is a child
of the transaction that wrote its row, linked to the relay batch.

Only `traceparent` is persisted across the asynchronous hops. `tracestate` and
baggage are request-scoped vendor data whose meaning does not survive a queue.

### Spans

API requests; `run.create` (workflow-manager), `run.materialize` and
`run.reconcile` (dag-engine); `scheduler.claim` and `attempt.dispatch`;
`attempt.execute` with `attempt.prepare_inputs`, `attempt.run`,
`attempt.finalize_logs`, `attempt.store_outputs` and `attempt.report_result`;
`resultstore.put` and `objstore.<op>` for every result-store call;
`outbox.publish <topic>`; database queries.

**Cardinality.** Run, attempt, node and worker ids are span attributes
(`nexus.run_id`, ...), which is what makes a trace searchable. They are never
metric labels (ADR-0019 is unchanged).

### Logs

Every service's slog handler is wrapped (`tracing.LogHandler`): a record logged
with a context that carries a span gains `trace_id` and `span_id`. The agent
binds them to the per-attempt logger, so every line an attempt logs carries its
trace id.

### Security

The REST listener is internet-facing. A caller's sampled `traceparent` forces
the parent-based sampler to keep that caller's traces. `NEXUS_TRACING_PUBLIC_ENDPOINT=true`
makes the gateway start a new root and **link** the caller's context instead;
the default honours callers so an SDK-submitted run joins its submitter's
trace. Span attributes never include query parameters, request bodies, task
environment or secrets.

## Consequences

**Positive.** One trace per run, from the API request to the last result
(verified end to end against Jaeger by `make tracing-e2e`); log lines link to
it. Standard variables and OTLP mean any backend (Jaeger, Tempo, Honeycomb,
a vendor collector) works. Tracing off costs a context lookup per span.

**Negative.** A dependency tree (OTel SDK, OTLP exporters, otelgrpc, otelpgx).
One migration (0028) adding two nullable columns, including to the hottest
table; metadata-only, but new binaries need it applied first (as every
migration). A long run is a long trace: backends that cap trace duration or
span count will truncate very large fan-outs (thousands of attempts). With
tracing on, each sampled query is a span, which at ratio 1.0 is a lot of data;
production should sample.

**Neutral.** NATS consumers (the agent's dispatch hints, the event-service
waker) only wake a loop; the trace continues through the attempt row, not the
hint. The OTel metrics SDK is not adopted; ADR-0019's exposition stays.

## Alternatives considered

- **One trace per request, links everywhere.** The OpenTelemetry messaging
  convention's default for queues. Rejected as the primary shape: nobody can
  follow a run across dozens of disconnected traces. Links are used where the
  cause is secondary (poll, relay batch, untrusted callers).
- **Trace context in the outbox payload JSON** (no migration). Rejected: the
  payload is the message body consumers parse; the column keeps transport
  metadata out of the contract and makes rows with no trace NULL.
- **`otelhttp` for the REST listener.** Rejected: it names spans before
  routing and hands the mux a copied request, losing the route template.
- **Tracing on by default.** Rejected for the unconfigured-exporter reason
  above and because the right sampling ratio is a deployment decision.
