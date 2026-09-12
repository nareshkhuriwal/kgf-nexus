# Contract findings from Phase 1 implementation

Issues agents raised against the frozen contract. Per CONTRACT.md agents
REPORT rather than edit, so divergences get reconciled centrally here.

Proto/gen changes are deferred until all Phase 1 agents finish: regenerating
`gen/` mid-flight would break every in-progress build.

## Open — needs a contract change

### F1. `Edge.output_to_input` is named backwards from its semantics
**From:** dag-engine · **Severity:** naming only, no behaviour change
The comment says "key = downstream input name, value = upstream output name",
which is the reverse of what the field name reads as. The comment describes the
correct direction and implementations follow it: a downstream input has exactly
one source, while one upstream output may fan out to many inputs — so keying by
*input* is the direction that is actually a function. Keying by output would
require a repeated value and could not express fan-out to two inputs of the
same downstream node.
**Resolution:** rename to `input_from_output` in the next proto revision.
Semantics stay as documented. Tracked; not urgent, since every implementation
already follows the comment.

### F2. `TIMED_OUT` / `LOST` are attempt states, not node states
**From:** dag-engine · **Severity:** doc inconsistency
Architecture §5.2 lists `TIMED_OUT` in the run-disqualifying set, but the node
enum has no such member (nor `LOST`) — both are *attempt* states.
**Resolution (adopted):** a retry-exhausted `TIMED_OUT` or `LOST` attempt folds
to a **FAILED node**, matching §5.2's "terminally failed after retries
exhausted". Implemented in `AttemptStateToNode`. Architecture §5.2 should say
this explicitly.

### F3. §5.2 has no rule for an all-terminal run containing CANCELLED and no failure
**From:** dag-engine · **Severity:** genuine gap; a judgement call the doc must settle
Neither SUCCEEDED nor FAILED is truthful for a run whose nodes are terminal
with at least one CANCELLED and zero failures.
**Resolution (adopted):** return `CANCELLED`. Rationale: the run did not
achieve its goal, so SUCCEEDED is a lie; nothing broke, so FAILED misdirects
the operator to hunt a non-existent fault. Architecture §5.2 needs this rule
added.

### F4. No DAG-engine gRPC service in the frozen proto
**From:** dag-engine · **Severity:** real gap
`proto/` defines only `WorkerService`, yet ADR-0011 requires every service to
expose a gRPC API. The agent hand-wrote a `grpc.ServiceDesc` for
`nexus.v1.DagEngineService` (MaterializeRun / ReconcileRun / ValidateSpec) with
a JSON codec, isolated to one file so a generated stub is a drop-in replacement.
**Resolution:** add `proto/nexus/v1/services.proto` covering the internal
service APIs (dag-engine, workflow-manager, resource-manager, event-service)
once agents are done, then regenerate and swap out the hand-written descriptor.

## Resolved

### F5. `go.mod` Go directive vs CONTRACT.md
CONTRACT.md says Go 1.27 (toolchain is 1.27.1); `go.mod` says `go 1.25.0`.
Not a defect: the `go` directive is a *minimum language version*, not the
toolchain, and 1.25.0 is a deliberately conservative floor that still builds on
1.27.1. No change needed; CONTRACT.md's "Go 1.27" means the toolchain in use.

## Ownership clarification (not a defect)

### task_attempts: creation vs mutation
ADR-0011 assigns `task_attempts` to the scheduler; architecture §3.2/§5.1 say
PENDING attempts are created by the DAG engine. Read literally these conflict.
**Resolution (adopted):** split on the *concern*, not the table.
- **CREATION** of a PENDING row is a dependency decision — only the engine
  knows the graph and trigger rules. DAG engine owns it.
- **MUTATION** from PENDING onward (state CAS, lease, reservation, worker
  assignment) is a scheduling decision and must stay inside the §5.3 atomic
  claim the no-double-dispatch guarantee rests on. Scheduler owns it.
- **Retry attempts** (attempt_number+1) are the scheduler's, since they arise
  from retry policy rather than dependency logic.
These never contend: the engine only INSERTs rows in PENDING and never UPDATEs;
the scheduler only UPDATEs from PENDING onward and never INSERTs an initial
attempt. No row is written by both in the same state, so no lost update.
**Actionable:** the dag-engine's Postgres grant on `task_attempts` must be
`INSERT` only — table-level, satisfying ADR-0011's "enforced by the database,
not code review" requirement.

---

## From scheduler workstream

### F6. §5.3 specified `REPEATABLE READ`, which does not work — CORRECTED
**Severity:** real bug in the architecture spec. **Status: FIXED in docs/ARCHITECTURE.md §5.3.**
Under REPEATABLE READ, a locking read reaching a row another transaction has
already COMMITTED an update to does not skip it — it aborts the whole
transaction with SQLSTATE 40001. `SKIP LOCKED` only skips rows *currently
locked*; it does nothing for rows already committed by someone else. With N
replicas on one queue those aborts are constant and every claim degrades into a
retry loop.

Verified empirically against Postgres 16 (concurrent sessions, snapshot held
open across a committed UPDATE):
- `REPEATABLE READ` → `ERROR: could not serialize access due to concurrent update`
- `READ COMMITTED`  → claim proceeds normally

Nothing is lost: the no-double-dispatch argument rests on the row locks, the
`state='PENDING'` CAS guard, and `UNIQUE(attempt_id)` on leases — none of which
depend on snapshot isolation. §5.3 now specifies READ COMMITTED with this
rationale inline.

### F7. `run_events.seq` has no safe allocation mechanism
**Severity:** real schema gap. **Status: FIXED via migration 0002.**
`UNIQUE(run_id, seq)` with a plain `INT seq` and no counter forces `max(seq)+1`,
which races — the scheduler hit `duplicate key value violates unique constraint
"run_events_run_id_seq_key"` under concurrency. The agent worked around it with
`pg_advisory_xact_lock(hashtextextended(run_id))`, which is correct but
serializes all event writers for a run on an advisory lock.
**Resolution:** added `workflow_runs.event_seq` as an in-row counter; allocation
becomes `UPDATE ... SET event_seq = event_seq + 1 RETURNING event_seq`, which is
atomic, contends only on that run's row, and needs no advisory lock. The
advisory-lock path remains correct and can be removed opportunistically.

### F8. Invariant 4 contradicts the ADR-0011 ownership matrix (run_events, timers)
**Severity:** genuine contract inconsistency. **Status: resolved — see below.**
Invariant 4 requires FIVE facts to commit in one transaction (reservation,
lease, attempt CAS, run_event, timers), but the matrix assigns `run_events` to
event-service and `timers` to workflow-manager, while naming only ONE exception
(reservations + workers.committed). These cannot both hold.
**Resolution (adopted):** invariant 4 wins — atomicity is the guarantee; table
ownership is an organizing principle. The dispatch transaction is therefore a
**documented three-table exception**: reservations + workers.committed,
run_events, and timers. ADR-0011's exception clause needs widening to say so
explicitly. Everywhere *outside* the claim, ownership holds and services call
the owner's API.

### F9. Worker DEAD transition spans scheduler and resource-manager ownership
**Severity:** real, needs a decision.
The liveness sweeper writes `workers.state`, owned by resource-manager. It lives
in the scheduler because the DEAD transition must be atomic with expiring that
worker's leases (scheduler-owned): a worker marked DEAD whose leases are still
live is dispatched-to by no one and recovered by no one — work silently stalls.
**Options:** (a) a fourth documented exception, or (b) move the whole
worker-death path into resource-manager and have it call the scheduler's API to
expire leases, accepting that the two writes are then not atomic.
**Deferred to reconciliation pass.** Current behaviour is correct but violates
the stated matrix.

### F10. §5.3 step 3 references namespace quota, which the schema cannot express
**Severity:** minor doc/scope mismatch.
There is no quota table; quotas are Phase 2 per §11. The scheduler implemented
the `concurrency_slots` semaphore (which the schema does support) and correctly
left quota out rather than inventing storage. §5.3 should mark the quota step as
Phase 2.

### Scheduler scoping decision (not a defect)
Timer claiming, lease reconciliation and the liveness sweep are scoped to the
replica's own queue. The docs do not specify this, but a fleet-wide reconciler
would have every replica contending on every other queue's rows — exactly the
coordination ADR-0002 exists to eliminate. Worth adding to §3.2.

---

## From UI + deploy workstream

### F11. No REST endpoint returns attempt rows — WILL BREAK INTEGRATION
**Severity:** real contract gap, highest integration risk. **Status: open, needs gateway change.**
§8.2 gives `GET /runs/{id}` (detail + node states) and `GET /attempts/{id}/logs`,
but nothing returns the attempt rows themselves. Yet "every attempt for a node,
retries as separate rows" is an explicit Phase 1 UI requirement, and it is the
whole point of invariant 9 (a retry is a NEW attempt; history is never mutated).
The UI codes against `GET /runs/{id}/attempts`, which is NOT in the contract.
**Resolution:** add `GET /runs/{id}/attempts` to §8.2 and implement it in the
gateway. Embedding attempts in the run-detail response is the alternative, but a
separate endpoint is better: attempt history grows unboundedly with retries and
should be independently pageable.

### F12. event-service was never assigned to any workstream
**Severity:** my fan-out omission, not an agent error. **Status: FIXED — built centrally.**
ADR-0011 lists six services; only five were assigned. `docker compose up` cannot
start until it exists. Built directly rather than spawning another agent.

### F13. Service main.go paths are inconsistent
**Severity:** cosmetic but breaks the parameterised Dockerfile.
Three services use `services/<name>/cmd/<name>/main.go`, two use
`services/<name>/main.go`. The single parameterised Dockerfile needs one
`SERVICE_PATH` per service as a result.
**Resolution:** accept both via a per-service build arg (already how it works).
Not worth churning five services to normalize during Phase 1.

### F14. Node-state derivation when node and latest attempt disagree
**Severity:** genuine unspecified behaviour.
`task_nodes.state` and `task_attempts.state` are independent columns and nothing
says which wins when they disagree (node RUNNING, latest attempt LOST).
**Resolution (adopted):** the **node column is authoritative** for node state —
it is what the DAG engine reconciles and what run-terminality is computed from.
Attempt state is authoritative for *attempt* display, and `pending_reason` is
lifted from the latest attempt. This is what the UI implemented. Should be
stated in §5.1.

### F15. JSON enum serialisation undecided
**Severity:** real interop risk.
The DB uses bare labels (`UPSTREAM_FAILED`); protobuf uses prefixed
(`NODE_STATE_UPSTREAM_FAILED`). §8.2 never says which the JSON API emits.
**Resolution (adopted):** the REST API emits **bare DB labels** — they match
CONTRACT.md's canonical names, are what operators see in SQL, and keep the wire
format readable. The UI normalizes both defensively, which is correct regardless.
Gateway must confirm it emits bare labels.

### F16. MinIO images moved off Docker Hub
**Severity:** environmental, affects anyone pulling MinIO.
`docker.io/minio/minio` no longer resolves (pull access denied). Compose now uses
`quay.io/minio/minio` and `quay.io/minio/mc`.

### F17. §8.2 has no /capacity response shape
Minor. The UI falls back to client-side aggregation over ACTIVE/DRAINING workers
when `/capacity` 404s, which is a reasonable degradation either way.

---

## From CLI + SDK workstream

### F18. WorkflowSpec has no wire home for literal or param input bindings — BLOCKER
**Severity:** highest. Without this, no workflow can pass a constant or bind a
run parameter to a task. **Status: open, needs a proto decision.**
`Edge.output_to_input` covers upstream→downstream wiring, but nothing expresses
"this input is the literal 10" or "this input comes from run param `source`".
The SDK emits `input_literals` (JSON-encoded scalars) and `input_params` on
TaskSpec; these are the SDK author's invention and will fail the gateway's JSON
Schema until the proto adopts them.
**Resolution:** add both fields to `TaskSpec` in the next proto revision. The
names are reasonable and already implemented on both the SDK and CLI sides;
`internal/dag`'s InputPlan already models edge ≻ param ≻ default precedence, so
the resolution order is settled — only the wire fields are missing.

### F19. §4.3's `idempotency_key_from` contradicts invariant 1
**Severity:** doc defect. **Status: resolved — remove from §4.3.**
The §4.3 YAML sketch shows a user-configurable
`idempotency_key_from: [run_id, node_id, map_index]`, but CONTRACT.md invariant 1
and ADR-0006 both state the PLATFORM derives the key and users never influence
it — and the field is absent from `TaskSpec` in the proto. The SDK correctly
treated the proto as authoritative and did not expose it.
Leaving it in the doc would invite users to think they can weaken the key, which
is exactly the mistake ADR-0006 exists to prevent. Delete it from §4.3.

### F20. `ERROR_CLASS_VALIDATION` vs `validation_error` spelling
Minor. §4.3's YAML writes `validation_error`; `common.proto` has
`ERROR_CLASS_VALIDATION`. The SDK accepts both author spellings and emits the
proto name. The JSON Schema should accept both for the same reason.

### F21. §4.3 fields absent from the proto
`tolerations` and `cache.key_from` appear in the §4.3 sketch but not in
`TaskSpec`. The SDK omitted them rather than inventing field names — correct.
Also undefined: whether a task's `priority` or the workflow's wins when both are
set. Needs a rule; suggest task overrides workflow, workflow supplies the default.

### F22. schedule_to_close vs start_to_close is unvalidated
A spec with `schedule_to_close < start_to_close` can never complete even one
attempt. The SDK added a client-side rejection. The gateway's semantic
validation should reject it server-side too, since the SDK is not the only
submission path.
