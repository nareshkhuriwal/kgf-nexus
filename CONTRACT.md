# FROZEN CONTRACT — Phase 1. Read before writing code. Do not modify.

Module: `github.com/khuriwalgroup/kgf-nexus`  ·  Go 1.27  ·  Postgres 16  ·  MinIO
Generated protobuf types: import `nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"`

## Files you MUST NOT edit
- `proto/nexus/v1/*.proto` and `gen/**` — regenerate only via the Makefile.
- `services/migrations/0001_init.up.sql` — schema is validated and frozen.
- `CONTRACT.md`, `docs/**`.
If you believe the contract is wrong, WRITE THE PROBLEM IN YOUR FINAL REPORT
instead of editing it. A silent divergence breaks every other workstream.

## Canonical names (use these exactly)
- Attempt states: PENDING, DISPATCHED, RUNNING, SUCCEEDED, FAILED, TIMED_OUT, LOST, CANCELLED
- Node states: PENDING, READY, RUNNING, SUCCEEDED, FAILED, SKIPPED, UPSTREAM_FAILED, CANCELLED
- Run states: PENDING, RUNNING, PAUSING, PAUSED, SUCCEEDED, FAILED, CANCELLING, CANCELLED, TIMED_OUT
- Tables: workflow_specs, workflow_runs, task_nodes, task_edges, task_attempts,
  workers, leases, reservations, run_events, timers, idempotency_keys, api_keys, concurrency_slots

## Non-negotiable invariants
1. **Idempotency key excludes attempt_number.**
   `sha256(run_id ‖ node_id ‖ map_index|"-" ‖ task_version)`. All retries of the
   same logical work share one key. This is the core of at-least-once.
2. **LOST ≠ FAILED.** FAILED = we know it ran and did not succeed. LOST = we do
   not know if it ran. LOST + IDEMPOTENCY_MODE_NONE => do NOT auto-retry.
3. **Lease fencing.** Every attempt RPC carries `lease_token`. A stale token is
   REJECTED with `Ack{accepted:false, reason:"stale_lease_token"}`. Never apply a
   stale report.
4. **Dispatch is ONE transaction**: reservation + lease + attempt state CAS +
   run_event + timers. NATS/notification publish happens AFTER commit, never inside.
5. **Control plane never runs user code.** It parses WorkflowSpec JSON only.
6. **Task payloads never transit the control plane.** Workers read/write MinIO directly.
7. **Deadlines use control-plane time.** Worker clocks are never authoritative.
8. **Append-only `run_events`.** Never UPDATE or DELETE it.
9. **Retry creates a NEW task_attempts row** (attempt_number+1). Never mutate a
   terminal attempt.

## Shared Go package layout (own only your own directory)
- `internal/domain/`     — shared enums/helpers + ResourceVector arithmetic (owned by SCHEDULER agent)
- `internal/store/`      — pgx queries (each agent adds only its own file)
- `services/api-gateway/`, `services/scheduler/`, `services/dag-engine/`
- `agent/`               — worker agent
- `cli/`                 — nexusctl
- `ui/`                  — React
- `deploy/`              — docker-compose

## Conventions
- Postgres driver: `github.com/jackc/pgx/v5` (+ `pgxpool`). No ORM.
- Errors: wrap with `fmt.Errorf("...: %w", err)`. Never swallow.
- Logging: `log/slog`, always include run_id/attempt_id/worker_id where known.
- Every exported func gets a doc comment saying WHY when non-obvious.
- `go build ./...` and `go vet ./...` must pass when you finish.
- Comments explain rationale, not mechanics. No "// increment i".

## Local test DB (already running)
`postgres://postgres:test@localhost:55432/nexus` — schema already applied.
This is a note about one developer setup, not a default: no service or test
falls back to it. Services require an explicit DSN (docs/OPERATIONS.md §8) and
integration tests read only `NEXUS_TEST_DATABASE_URL`. On some machines 55432
belongs to an unrelated product's Postgres; check before pointing anything at it.

## MICROSERVICE DECOMPOSITION (ADR-0011) — binding
Six independently deployable services, ONE shared Postgres schema, STRICT
per-service table ownership. A service WRITES only the tables it owns;
cross-service writes go through the owner's gRPC API.

| Service | OWNS (may write) |
|---|---|
| api-gateway      | api_keys, idempotency_keys |
| workflow-manager | workflow_specs, workflow_runs, timers |
| dag-engine       | task_nodes, task_edges |
| scheduler        | task_attempts, leases, concurrency_slots |
| resource-manager | workers, reservations |
| event-service    | run_events |

THE DELIBERATE EXCEPTION (dispatch only): inside the §5.3 claim transaction the
scheduler writes `reservations` + `workers.committed` (resource-manager),
`run_events` (event-service) and `timers` (workflow-manager), because the five
facts of a dispatch MUST commit atomically or the no-double-dispatch guarantee
is lost. Atomicity is the guarantee; ownership is an organizing principle, so
atomicity wins where they conflict. Outside the claim, call the owner's API.

ISOLATION LEVEL: the claim transaction uses READ COMMITTED, never REPEATABLE
READ. Under RR a locking read that reaches an already-committed updated row
aborts with SQLSTATE 40001 instead of skipping it; SKIP LOCKED only skips rows
currently LOCKED. Verified empirically. Correctness rests on the row locks, the
state='PENDING' CAS and UNIQUE(attempt_id), not on snapshot isolation.

EVENT SEQ: allocate `run_events.seq` via
`UPDATE workflow_runs SET event_seq = event_seq + 1 RETURNING event_seq`
(migration 0002). Never `max(seq)+1` — it races.

Every service: own container, own gRPC API, /healthz + /readyz, own scaling unit.
Migrations are applied by a dedicated job, NEVER by a service at startup.
