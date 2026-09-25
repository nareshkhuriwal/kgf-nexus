# ADR-0019: Metrics exposition through client_golang, behind the in-house instrument API

- **Status:** Proposed — pending owner sign-off
- **Date:** 2026-09-24
- **Deciders:** platform team
- **Refines:** the `internal/metrics` package comment ("swapping in client_golang behind this package's API is the right move" once runtime collectors are needed)

## Context

Only workflow-manager and event-service served `/metrics`. Every other service,
and the agent, exposed nothing. Extending coverage means deciding first whether
the exposition itself is fit for production.

`internal/metrics` was a standard-library-only encoder of the Prometheus text
format (0.0.4). We evaluated its output by parsing it with
`prometheus/common/expfmt`, the parser Prometheus uses:

**Compliant on the happy path.** `# HELP` / `# TYPE` once per family, label
values escaped (`\\`, `\"`, `\n`), HELP escaped, histograms with cumulative
`le` buckets, a mandatory `le="+Inf"` bucket equal to `_count`, `_sum` and
`_count`, reserved spellings for `+Inf`/`-Inf`/`NaN`, a versioned
`Content-Type`. Histograms existed and were correct.

**Non-compliant at the edges.** Each of these produced an exposition that fails
to parse — and a failed parse loses *every* metric on the target, not one:

1. A histogram label named `le` rendered `{le="x",le="0.1"}`.
2. A duplicated bucket bound (or an explicit `+Inf`) emitted two series with
   the same `le`.
3. `With()` accepted any label set, so one family could carry series with
   different label names (accepted by the text format, rejected by
   client_golang's consistency checks and a latent dashboard hazard).
4. The series key was `a=1,b=2`; a label value containing `,` or `=` could
   collide with a different label set and merge two series.

**Missing for production.** No Go runtime or process collectors (goroutines,
GC, heap, RSS, CPU seconds, file descriptors, start time), no build info, no
`Accept`-header negotiation or gzip, no HTTP/gRPC middleware.

## Decision

Keep the in-house **instruments** (`Registry`, `CounterVec`/`GaugeVec`/
`HistogramVec`, `With(Labels)`, `Value()`, the negative-add and NaN rules) and
replace the **exposition** with `github.com/prometheus/client_golang`
**v1.24.1** and `prometheus/common` **v0.71.0**, pinned in `go.mod`:

- The registry registers one *unchecked* `prometheus.Collector` that emits the
  in-house values as const metrics at scrape time; `Handler()` is
  `promhttp.HandlerFor` (text 0.0.4 by default, gzip, `ContinueOnError`, a
  bounded in-flight limit).
- Label names are fixed at registration and `With()` normalizes to them
  (undeclared labels dropped, missing ones empty, one warning per family);
  `le` on a histogram and `__`-prefixed names panic at registration; bucket
  bounds are sorted, de-duplicated and stripped of `+Inf`; the series key uses
  a separator that cannot occur in UTF-8.
- `NewServiceRegistry(service, version)` adds `collectors.NewGoCollector`,
  `collectors.NewProcessCollector` and `nexus_build_info`.
- The package adds route-template HTTP RED middleware and gRPC server
  interceptors using the grpc-ecosystem metric names.

## Consequences

**Positive.** No call site changed (the timer reconciler, the outbox relay and
TLS metrics are untouched and their tests still pass). The wire format,
negotiation and runtime collectors are the reference implementation's, so a
future Prometheus/OpenMetrics revision is a dependency bump, not an encoder
rewrite. Standard community dashboards for Go runtime and gRPC work unmodified.
Exemplars, native histograms and OpenMetrics are now options, not projects.

**Negative.** A new direct dependency tree: `client_golang`, `client_model`,
`common`, `procfs`, `beorn7/perks` (the module graph already carried older
`client_golang`/`common` via Kubernetes client libraries, so the incremental
supply-chain surface is smaller than it looks, but it is real and must be kept
current). Two layers now exist — in-house instruments and client_golang
exposition — and a const-metric bridge allocates per series per scrape (at the
series counts here, microseconds). `Content-Type` now carries
`escaping=underscores`; scrapers ignore it, one test was adjusted.

**Neutral.** Exposition order changed from registration order to client_golang's
sorted order (still deterministic and diffable). A family with no observations
still emits nothing.

## Alternatives considered

- **Fix the encoder in place, write runtime/process collectors by hand.**
  Rejected as the *higher*-risk option: the four defects are small, but a
  correct process collector means parsing `/proc/self/stat`, `/limits` and
  `/fd` per platform, and a Go runtime collector means tracking
  `runtime/metrics` names across Go releases — exactly the code client_golang
  maintains, would be maintained worse here, and would still emit metric names
  dashboards expect only if we reproduced theirs exactly.
- **Migrate every call site to client_golang's native API.** Rejected: more
  churn (every `metrics.Labels` and `Value()` in tests changes), no additional
  capability, and it loses the deliberate API choices of this package (dropped
  negative counter adds, explicit per-registry isolation).
- **OpenTelemetry metrics SDK with a Prometheus exporter.** Deferred, not
  rejected: tracing is a separate roadmap item; when OTel arrives the bridge
  in `registry.go` is the single place an OTel reader can attach.
