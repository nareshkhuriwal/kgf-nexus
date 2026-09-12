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
