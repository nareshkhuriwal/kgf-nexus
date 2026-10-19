# Nexus operator console

React + Vite + TypeScript + TanStack Query. Phase 1 scope: **read-only plus
cancel** (architecture §11).

```bash
npm install
npm run dev      # http://localhost:5173, proxies /v1 to localhost:8090 (the gateway `make up` publishes)
npm run build    # tsc -b && vite build
npm test         # vitest
npm run e2e      # Playwright, against a running stack (see below; `make e2e` does it all)
```

## Screens

| Screen | What it answers |
|---|---|
| Run list (`/runs`) | Which runs exist, in what state. Filter by state and spec, newest first; filters live in the URL so a view is shareable. |
| Run detail (`/runs/:id`) | The DAG with live per-node state, the execution timeline (Gantt), the full attempt history for the selected node, a streaming log tail, and the append-only event timeline with actor attribution. Cancel and retry live here. |
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

**The execution timeline answers "where did the time go".** One row per
attempt (retries included, grouped by node; map children collapsible under their
template), each bar split into queued (scheduled→dispatched), starting
(dispatched→started) and running (started→ended). Colour is the final state from
the same state table; phase is bar height, so it reads in greyscale. Ctrl/⌘+wheel
zooms, Shift+wheel or drag pans, and the keyboard does both (arrows, +/−, 0).
While the run is live a now-line advances and open phases grow; an operator zoom
survives polling until "Follow live". Selection is the page's URL selection in
both directions, `?timeline=table` switches to the equivalent table, and rows are
virtualised, so a fan-out of thousands of attempts mounts a few dozen rows. The
maths is pure and unit-tested in `src/lib/timeline.ts`. It needs the gateway's
`dispatched_at` (F120); against an older gateway queued runs straight to started
and is marked as not reported, rather than invented.

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

## Browser end-to-end tests

`ui/e2e/` is a Playwright suite (pinned `@playwright/test`, Chromium) that drives
the console against a real, isolated stack:

```bash
make e2e                                   # stack up, suite, stack down (pass or fail)
make e2e E2E_KEEP=1                        # leave the stack up afterwards; make e2e-down
make e2e E2E_MINIO_IMAGE=<local image>     # quay.io MinIO answers 401 on this network
make e2e-up && (cd ui && npx playwright test --ui)   # iterate against a kept stack
```

`deploy/e2e/e2e-stack.sh` starts the dev compose file under its own project
(`E2E_PROJECT`, default `nexus-e2e`) with every host port it needs in nine
consecutive ports from `E2E_PORT_BASE` (default 55711: console, gateway,
Postgres, MinIO, MinIO console, Mailpit, NATS, gRPC, gateway metrics) and every
other published port dropped (`deploy/e2e/docker-compose.e2e.yml`), so it runs
beside `make up`. It mints fresh keys, seeds them with `make seed`, waits for an
ACTIVE worker and writes `ui/e2e/.stack.env` (gitignored) for the suite.

The stack's database is on the host's PostgreSQL, never a container: `up`
creates `E2E_DB` (default the project name with `-` as `_`, e.g. `nexus_e2e`)
on the server `NEXUS_TEST_PG_SERVER` names (default
`postgres://127.0.0.1:5432/postgres`), which the compose file uses by default
(`NEXUS_HOST_DATABASE_URL`), and `down` drops it. Set `NEXUS_DB_MAX_CONNS` on a server shared with other
workloads. `E2E_BUNDLED_POSTGRES=1` opts in to the compose file's own postgres
container through `deploy/docker-compose.bundled-postgres.yml` (CI runners,
which have no host server, set it).

What the suite covers, all through the browser:

* **Setup** (`global.setup.ts`): an administrator creates an operator; the
  console refuses the unverified sign-in; the operator confirms the address from
  the link the gateway mailed (read from Mailpit) and signs in through the form.
  The fixture workflow is registered the way the SDK/CLI does it (POST of the
  document).
* **Journey** (`fan-out-retry.spec.ts`, fixture `fixtures/fan-out-retry.yaml`):
  catalog search, version and graph; submit with parameters; watch the live run
  and the map expand; the run fails on a controlled, retry-only failure; open the
  failed child from the graph; read its logs and download the full log; the
  timeline's tooltip, keyboard, zoom and table view; retry the child with
  "Retry this task"; the run reaches SUCCEEDED and the fan-in's log shows all
  five results; the API agrees.

Rules the suite keeps: no fixed sleeps (web-first assertions and `expect.poll`
only), one worker, and on failure a trace, screenshot and video per test plus
every service's log in `ui/e2e/test-results/`. `npm run e2e:report` opens the
HTML report. CI runs it in the `browser-e2e` job of `.github/workflows/ci.yml`.

There is no endpoint that serves a task's output payload, so "retrieve an
output" is covered through the attempt log (the summarising task prints what it
received) and the log download.
