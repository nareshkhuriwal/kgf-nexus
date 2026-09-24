# ADR-0014: Worker transport stays gRPC; no HTTP/JSON fallback yet, proxy requirements documented

- **Status:** Proposed — pending owner sign-off
- **Date:** 2026-09-24
- **Deciders:** platform team
- **Closes:** ARCHITECTURE.md §12 question 2

## Context

§12 asked whether workers need an HTTP/1.1 + JSON fallback for networks that
cannot carry gRPC. The worker contract today (`proto/nexus/v1/worker.proto`,
`services/api-gateway/workergrpc`) is what the decision has to be made against,
so it was read rather than assumed:

| Fact | Where | Consequence for proxies |
|---|---|---|
| 7 RPCs, **all unary** (Register, Heartbeat, PollTask, ReportStarted, ExtendLease, ReportProgress, ReportResult) | `worker.proto` | No client/server/bidi streams. Any proxy that forwards unary gRPC works; nothing needs a long-lived stream. |
| Control-plane → worker commands ride on the Heartbeat **response** | ADR-0005 | No server push, so no inbound connectivity to workers and nothing a proxy must hold open. |
| Longest request: PollTask long poll, capped at `MaxPollWait` = 30 s | `workergrpc.DefaultConfig` | Proxy request/idle timeout must exceed 30 s. |
| Largest request: ReportResult with inline outputs; server cap 4 MiB | `grpc.MaxRecvMsgSize(4<<20)` in the gateway; ADR-0016 caps the inline threshold at 1 MiB | Proxy body limit must be ≥ 4 MiB. |
| Server keepalive 30 s / 10 s; enforcement MinTime 10 s, PermitWithoutStream | gateway `main.go` | A proxy that strips HTTP/2 PINGs is harmless (keepalive is advisory), one that closes idle h2 connections < 30 s causes reconnect churn, not errors. |
| TLS optional, mTLS supported | `internal/transport` | TLS-terminating proxies must re-originate h2 (or pass through). |
| Channels rotate (`MaxConnectionAge`) and use `round_robin` | F81 | Proxies must tolerate GOAWAY-driven reconnects. |

What a JSON fallback would buy is therefore narrow: environments whose only
egress is an HTTP/1.1-only proxy (some corporate forward proxies, some legacy
L7 load balancers). No deployment target in this repo (compose, the Azure
pipeline, Portainer, Kubernetes) is such an environment.

## Options

1. **Keep gRPC only; document proxy requirements.** *Chosen.*
2. **gRPC-Web or Connect protocol on the gateway.** Rejected for now: both
   need a new dependency (`connectrpc.com/connect` or an improbable-eng
   wrapper), which touches `go.mod`, the gateway's auth interceptor chain and
   TLS setup, and doubles the surface the chaos suite must cover. It is the
   right tool when the trigger below fires, not before.
3. **Hand-written HTTP/1.1 JSON shim** (`POST /worker/v1/<Method>` with
   protojson bodies, reusing `workergrpc.Server` methods directly). Rejected for
   now, not on size -- seven unary methods map mechanically -- but on safety:
   auth today is a gRPC interceptor (`AuthUnaryInterceptor`) reading the API key
   from gRPC metadata, and a second ingress path is a second place the
   fencing and authentication rules must be re-proven. A fallback nobody
   exercises is exactly the kind of untested path F89 warns about. Its small
   size is recorded here because it lowers the cost of doing it when needed.
4. **Tunnel gRPC over WebSocket.** Rejected: bespoke framing, no ecosystem
   support, and it would reintroduce a long-lived connection the pull design
   deliberately avoids.

## Decision

gRPC over HTTP/2 remains the only worker transport. A proxy between agents and
the gateway MUST:

- speak **HTTP/2 end to end** to the gateway (ALPN `h2` with TLS, or h2c for a
  plaintext internal hop) and **pass response trailers** (`grpc-status` lives
  in trailers; a proxy that drops them turns every call into `UNKNOWN`);
- allow a **request/idle timeout of at least 60 s** (30 s long poll plus
  margin; nginx `grpc_read_timeout 60s`, envoy route `timeout: 60s`);
- allow **request bodies of at least 4 MiB** (`client_max_body_size 4m`);
- **not buffer** responses (nginx `grpc_pass` already streams; generic HTTP
  proxies in buffering mode break long polls);
- tolerate **GOAWAY and reconnects** (connection rotation, F81).

Known-good: nginx ≥ 1.13.10 with `grpc_pass`, Envoy, Traefik, HAProxy ≥ 2.0
in HTTP mode with `proto h2`, Azure Application Gateway v2 / AWS ALB with a
gRPC target group. Known-bad: HTTP/1.1-only forward proxies, and AWS Classic
ELB in HTTP mode.

**Revisit trigger:** the first deployment where agents must egress through an
HTTP/1.1-only proxy. At that point implement option 2 (Connect's JSON protocol
serves unary RPCs over HTTP/1.1 with the same handler), gated by the chaos suite
running over it.

## Consequences

**Positive** -- one transport, one auth path, one set of fencing checks; no new
dependency; the requirement list is short because every RPC is unary.

**Negative** -- an environment that can only pass HTTP/1.1 cannot run workers
until the trigger fires; that is a sales/onboarding blocker we are choosing to
accept rather than a technical one. Nothing in this change *measured* a proxy:
the requirements are derived from the code's timeouts and limits, and the
known-good list is from vendor documentation, not from runs here.

**Neutral** -- if Option 2 is adopted later it is additive: gRPC clients are
unaffected.
