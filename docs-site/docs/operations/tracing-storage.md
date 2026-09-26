# Tracing and storage backends

## Tracing

Every service, the scheduler, the agents, artifact-gc and the cluster
controller can export OpenTelemetry traces over OTLP. It is **off** until you
point it at a collector, and configured only through the standard `OTEL_*`
variables (Helm: `tracing.enabled`, `tracing.endpoint`, `tracing.samplerArg`).
W3C trace context propagates across HTTP, gRPC, the transactional outbox and
the attempt row even when nothing is exported, so **one run is one trace**:
the API request that created it, the engine's reconciliation, each dispatch,
the agent's execution phases, the result-store calls and the database
queries. Every log line written in a request's or an attempt's context
carries `trace_id` and `span_id`.

A task process receives the attempt's context as `TRACEPARENT`/`TRACESTATE`.
With the SDK's `opentelemetry` extra installed
(`pip install 'nexus-sdk[opentelemetry]'`), the handler runs in a child span
and `ctx.trace_headers()` propagates further. For callers you do not control,
`NEXUS_TRACING_PUBLIC_ENDPOINT=true` turns an inbound `traceparent` into a
span link instead of the parent.

Design: [ADR-0026](../design/adr/0026-opentelemetry-tracing.md).
Configuration and reading a trace:
[runbook §26.1-§26.2](runbook.md#261-turning-tracing-on). Locally,
`docker compose --profile tracing up` starts a pinned Jaeger.

## Result-store backends

Results, logs and checkpoints live in the result store, reached through one
interface with three native backends, all held to one conformance suite:

| Backend | Authenticates with |
| --- | --- |
| `s3` -- Amazon S3, MinIO/Silo, any S3-compatible store | static keys, or a credential chain: IRSA, EKS Pod Identity, ECS, instance roles |
| `azblob` -- Azure Blob Storage | workload identity, managed identity or a shared key |
| `gcs` -- Google Cloud Storage | Application Default Credentials (GKE Workload Identity) |

The gateway advertises the backend to the agents at registration, so the
whole fleet agrees. In Kubernetes the result-store clients -- api-gateway,
the workflow-manager's sensors, the agents and artifact-gc -- run under the
ServiceAccounts a cloud identity is bound to, and the Terraform environments
for AKS, EKS and GKE configure the keyless path by default
([Terraform](terraform/index.md)).

Design: [ADR-0027](../design/adr/0027-result-store-backends.md).
Choosing and configuring a backend, least privilege and lifecycle rules:
[runbook §26.3-§26.4](runbook.md#263-choosing-and-configuring-the-result-store-backend).
The conformance suite runs with `make objstore-conformance` against Silo,
Azurite and fake-gcs-server; emulators are not the clouds, and no cloud apply
has been recorded yet (F115).
