# Nexus operator console

React + Vite + TypeScript + TanStack Query. Phase 1 scope: **read-only plus
cancel** (architecture §11).

```bash
npm install
npm run dev      # http://localhost:5173, proxies /v1 to localhost:8090 (the gateway `make up` publishes)
npm run build    # tsc -b && vite build
npm test         # vitest
```

## Screens

| Screen | What it answers |
|---|---|
| Run list (`/runs`) | Which runs exist, in what state. Filter by state and spec, newest first; filters live in the URL so a view is shareable. |
| Run detail (`/runs/:id`) | The DAG with live per-node state, the full attempt history for the selected node, a streaming log tail, and the append-only event timeline with actor attribution. Cancel lives here. |
| Workers (`/workers`) | The fleet: state, advertised vs committed capacity per dimension, heartbeat age, queues, labels — plus a capacity summary. |

## Design decisions worth knowing

**One state table.** `src/lib/states.ts` is the single source of truth for how
every run / node / attempt / worker state renders. The requirement it exists to
guarantee is that **FAILED, UPSTREAM_FAILED and SKIPPED never look alike**: the
whole operational value of separating those states is that "what actually broke"
is answerable at a glance, and that only holds if exactly one module decides.
Each state carries four independent channels — tone, glyph, border style, and a
hint — so the distinction survives greyscale and colour-blindness. The
distinctness is asserted by test, not by eyeballing.

**Root-cause attribution.** `src/lib/dag.ts` marks the FAILED nodes with no
failed ancestor. A `FAILED` node downstream of another failure is collateral; the
graph draws a halo only around real origins, and the run-detail page auto-selects
one when you open a failed run.

**`pending_reason` is prominent, not buried.** A PENDING attempt leads its card
with *why*. Without it the console shows an unexplained stall instead of
`waiting_for_resources: gpu (need 4, free 3)`.

**Every attempt is its own row.** A retry is a new attempt and history is never
mutated (invariant 9), so the attempt list shows all of them with per-attempt
worker, timings, resource request, error class, and metrics. Collapsing retries
into "failed 3 times" throws away exactly what makes a flaky task diagnosable.

**Live updates degrade honestly.** SSE against the events endpoint, falling back
to polling, and the header says which transport is actually carrying data. A pane
that has silently stopped following looks identical to a stalled run — the worst
failure mode this tool can have. The event stream is treated as a *hint*: it
invalidates queries, and the server remains the authority, so a dropped message
costs latency and never correctness.

**No optimistic cancel.** Cancel is asynchronous by design (RUNNING →
CANCELLING → CANCELLED once workers ack, §5.2). Faking a terminal state would
show the operator a lie.

**Never a blank screen.** Every panel routes through `AsyncPanel`
(loading → error → empty → data), an unreachable backend is distinguished from a
5xx and from a not-yet-implemented endpoint, stale data is kept on screen with a
warning rather than replaced by an error box, and an `ErrorBoundary` catches
render bugs.

## Configuration

| Var | Default | Purpose |
|---|---|---|
| `VITE_NEXUS_API_BASE` | `""` (same origin) | Gateway base URL |
| `VITE_NEXUS_NAMESPACE` | `default` | Initial namespace |
| `VITE_NEXUS_API_KEY` | unset | Dev-only static bearer token |
| `NEXUS_API_URL` | `http://localhost:8090` | Dev-server proxy target |

## API contract

Coded against architecture §8.2 and the enums in `proto/nexus/v1/*.proto` — not
against the Go services' source. `src/api/types.ts` uses the bare Postgres enum
labels (`UPSTREAM_FAILED`), and `normalizeState` also accepts the protobuf
spelling (`NODE_STATE_UPSTREAM_FAILED`), so either serialisation renders
correctly. List endpoints are accepted as `T[]`, `{items:[…]}` or `{runs:[…]}`.
