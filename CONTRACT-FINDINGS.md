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

---

## From worker-agent + resource-manager workstream

### F23. No `ResourceManagerService` exists in the frozen proto — BLOCKER for ADR-0011
**Severity:** real gap; worked around, needs a contract decision.
ADR-0011 requires every service to own a gRPC API, and makes resource-manager the
owner of `workers` and `reservations`. But `proto/nexus/v1/` defines only
`WorkerService` (the worker↔control-plane contract) — there is no RPC surface by
which any other service can ask resource-manager to release a reservation,
reconcile a worker, or transition a worker's state. The ownership matrix is
therefore unimplementable as written: the only way to satisfy it is for other
services to write resource-manager's tables directly, which is precisely what the
matrix forbids.

**Worked around** by defining `nexus.rm.v1.ResourceManagerService` in
`services/resource-manager/proto/nexus/rm/v1/rm.proto`, generated into
`services/resource-manager/gen/`. It imports `nexus/v1/common.proto`, so
`ResourceVector` is literally the frozen type and there is no translation layer to
drift. `CONTRACT.md` forbids editing `proto/nexus/v1/`, so this deliberately lives
in the owning service's own directory.
**Resolution needed:** promote it to `proto/nexus/v1/resource_manager.proto` (or
bless the per-service proto pattern) in the reconciliation pass. Same gap almost
certainly applies to every other service that is not api-gateway.

### F24. `ATTEMPT_OUTCOME_INDETERMINATE` has no error class that means "unknown"
**Severity:** minor, but it forces a small lie.
§6.3 requires a restarted agent to report `INDETERMINATE` for a `STARTING` WAL
entry. `ReportResultRequest` pairs an outcome with a `TaskError`, and the closest
class is `ERROR_CLASS_WORKER_LOST` — documented as "lease expired". In the WAL
replay case the lease has *not* necessarily expired; the agent simply cannot know
the outcome. We report `WORKER_LOST` because it is the class §6.5 policy keys on
for ambiguous outcomes, so behaviour is right, but the name misdescribes the
cause. Either widen the `WORKER_LOST` comment to "outcome unknowable, including
agent restart", or add `ERROR_CLASS_INDETERMINATE`.

### F25. `HeartbeatRequest` has no reconcile-grace field, and reconciliation has no
### documented grace period at all
**Severity:** real correctness issue in the design as stated.
§3.2 says any reservation the worker does not acknowledge "is a leak, and is
released". Taken literally that is wrong: an attempt dispatched one millisecond
ago has not reached the worker yet, so the very next heartbeat would release its
reservation and the leak detector becomes a leak *generator* that cancels work the
instant it is scheduled. A grace period is mandatory, and it must exceed one long
poll plus one heartbeat interval.
Implemented as `reconcile_grace_ms` on the RM's own request message, defaulting to
60s (20s long poll + 5s beat + slack). §3.2 should state the grace requirement
explicitly, since anyone implementing it from the doc alone writes the bug.

### F26. Nothing in the contract says who calls `Release` on attempt completion
**Severity:** ownership gap.
Invariant 4 puts reservation *creation* inside the scheduler's dispatch
transaction, and ADR-0011 blesses that as the one exception. The symmetric
question — who releases the reservation when the attempt finishes — is unassigned.
If the scheduler releases inline while applying `ReportResult`, that is a second
undocumented exception; if resource-manager releases via its API, the release is
not atomic with the attempt's terminal state, so a crash between them leaks until
reconciliation. `Release` is implemented idempotently precisely so either choice
is safe, but the contract should name the owner.

### F27. `ExecutionMetrics.oom_killed` is `int32`, not `bool`
**Severity:** cosmetic, but it invites divergence.
Every other boolean in the protos is a `bool`. A tri-state int (0/1/unknown?) with
no documented meaning for other values will eventually be filled in differently by
two implementations. We write 0/1. Either document it as a 0/1 flag or change the
type.

### F28. `subprocess` resource enforcement is materially weaker than ADR-0007 implies
**Severity:** honest-reporting item, not a defect.
ADR-0007 says `subprocess` "cannot enforce memory limits as strictly as a
container" and labels it development-grade. That is right but understates it, so
the code states the specifics rather than leaving an operator to discover them:
- Linux: enforcement is `ulimit -v` (RLIMIT_AS), which caps *virtual address
  space*, not RSS. A process reserving without touching dies where a cgroup would
  allow it; one that catches the allocation failure survives where a cgroup would
  kill it. OOM detection is therefore inferential, not authoritative.
- **macOS: there is NO memory enforcement.** Darwin accepts `RLIMIT_AS` and
  ignores it. The executor refuses tasks declaring a memory limit unless
  `AllowUnenforceableLimits` is set, rather than running them unconstrained.
- CPU is `ulimit -t` (cumulative CPU-seconds), which cannot express "half a core";
  it is a runaway backstop, not a share guarantee.
- GPU isolation is impossible; `CUDA_VISIBLE_DEVICES` is a hint a task can
  override.
Reported through `LimitFidelity`, which the conformance suite asserts against, so
each executor is held to what it actually claims instead of to a uniform fiction.
Real cgroup-v2 enforcement (`memory.max` in a delegated subtree) is the Phase 2
upgrade; it needs delegation that laptops and unprivileged CI containers lack.

### F29. Go has no pre-exec hook, so subprocess limits go through a shell trampoline
**Severity:** implementation note worth recording.
There is no supported place in the Go stdlib to call `setrlimit` between fork and
exec, and calling it in the agent would lower the *agent's* own limits — eventually
killing the agent and turning one over-limit task into every sibling attempt
becoming LOST. The portable mechanism that runs in the child after fork and before
exec is the shell's own `ulimit` builtin followed by `exec`. Costs one short-lived
`/bin/sh`; every argument is single-quote escaped so a workflow spec cannot inject
shell commands.

### F30. §5.5's liveness numbers leave only one missed beat of margin under load
**Severity:** tuning observation.
A 5s heartbeat against a 30s liveness window is six beats, which is comfortable.
But `next_heartbeat_ms` lets the control plane slow the fleet under load, and
nothing in the contract couples the two: a control plane that raises the interval
to 20s while keeping a 30s window leaves margin for a single missed beat, and a
brief blip then declares live workers dead and duplicates their work — the exact
failure the independent heartbeat goroutine exists to prevent. Suggest the control
plane derive the liveness window from the interval it hands out (e.g. `max(30s,
6 × next_heartbeat_ms)`) rather than holding it fixed.

---

## From api-gateway / workflow-manager workstream

### F31. No `WorkflowManagerService` in the frozen proto (same class as F4)
**Severity:** real gap, blocks a required deliverable.
ADR-0011 requires every service to expose a gRPC API, and the gateway is the ONLY
ingress — so every run mutation must travel gateway → workflow-manager over gRPC.
But `proto/nexus/v1` defines only `WorkerService`. There are no request/response
messages for CreateRun / Cancel / Pause / Resume / RetryFromFailed, and
CONTRACT.md forbids editing the protos.
**What was built instead:** the run lifecycle is complete and fully tested behind
the `grpcapi.RunService` interface, and the listener registers the standard gRPC
health service so the process is genuinely deployable and probeable. Adding the
generated stubs is then a transport-only change with no lifecycle logic to rewrite.
**Resolution:** fold into F4's `proto/nexus/v1/services.proto`. The surface needed
is exactly the `RunService` interface in
`services/workflow-manager/grpcapi/server.go`.

### F32. Invariant 4 vs the gateway's heartbeat reconciliation
**Severity:** clarification; no divergence taken.
§3.2 requires the control plane to release any reservation a worker does not
acknowledge on heartbeat, and the heartbeat arrives at the **api-gateway** (it is
a `WorkerService` RPC). But `reservations` is resource-manager's table, and
ADR-0011 names only the *scheduler's dispatch claim* as an ownership exception.
**Resolution taken:** the gateway calls `resource-manager.Release` per leak rather
than deleting rows. That is correct per ADR-0011 and is what is implemented — but
it means reconciliation is **not atomic**: N releases are N RPCs, and a crash
midway leaves some leaks un-released. Benign, because reconciliation is
idempotent and the next heartbeat retries, and worth stating explicitly so nobody
"fixes" it later by writing the table directly.
Note also that resource-manager's own `HeartbeatWorker` RPC reconciles internally
if given `running_attempt_ids`. The gateway deliberately passes them EMPTY and
reconciles itself, so two components do not both release the same reservation and
double-log every leak. One owner of that decision, not two.

### F33. §8.2 lists no spec-GET endpoint, only `/versions`
**Severity:** minor gap in the sketch.
§8.2 has `POST /specs` and `GET /specs/{name}/versions`, but nothing returns a
stored spec DOCUMENT. Since specs are content-addressed and immutable and a run
pins its version for life, "show me exactly what this run executes" is a question
the REST surface cannot currently answer — which undercuts the reproducibility
ADR-0004 claims as a benefit. Suggest `GET /specs/{name}/versions/{version}`
returning the stored canonical JSON.

### F34. §8.2's drain endpoint implies a synchronous result it cannot have
**Severity:** doc precision.
`POST /workers/{id}/drain` reads as though the worker is drained when it returns.
It cannot be: the control plane never dials a worker (ADR-0005), so drain only
marks the worker DRAINING and the directive rides the next heartbeat RESPONSE.
Implemented as **202 Accepted** with the delivery mechanism stated in the body.
§8.2 should say 202 so client authors do not wait on a completion that never comes.

### F35. §5.2 does not say whether a PAUSED run keeps burning `run_timeout_ms`
**Severity:** genuine gap; a judgement call the doc must settle.
**Resolution adopted:** it does. `run_timeout_ms` is an end-to-end SLA, and a
pause that silently suspended it would make the guarantee unfalsifiable — a run
could sit PAUSED for a week and still claim it met a 1-hour budget. So
`PAUSING`/`PAUSED` both accept the `run_deadline` trigger and move to `TIMED_OUT`.
Operators who need to hold a pipeline longer than its SLA should cancel and re-run.
Worth documenting either way, because the opposite choice is equally defensible
and the two are indistinguishable from the outside until it matters.

### F36. `timers` is workflow-manager's table but most timer EFFECTS are the scheduler's
**Severity:** structural awkwardness, not a defect.
The reconciler must live with the table (one sweeper, one backlog metric, one
place to answer "what is overdue"), but `schedule_to_start`, `start_to_close`,
`schedule_to_close`, `heartbeat`, `retry_backoff`, and `lease_expiry` all mutate
`task_attempts`/`leases`. So the timer FIRES in workflow-manager and is APPLIED in
scheduler, one RPC per fire. Accepted deliberately; the alternative — a timer table
per service — multiplies reconcilers and sweepers and destroys the single overdue
view. Worth noting that this makes the scheduler's timer RPCs hot.

### F37. Schema duplication forced by `go:embed`
**Severity:** tooling, no contract impact.
The gateway must embed the JSON Schema (a validator that reads its rules from disk
can start fine and then validate against a stale copy after a partial deploy), but
`go:embed` cannot reach outside its package directory, so
`schemas/workflow-spec-v1.json` is copied into
`services/api-gateway/spec/`. `TestEmbeddedSchemaMatchesCanonical` asserts
byte-equality so drift fails the build. A `go:generate` copy step in the Makefile
would be tidier.

---

## From deployment (Docker Desktop bring-up)

These were found by actually running the stack, not by review. Every one of them
passed unit tests and `docker compose config` while still being broken at runtime.

### F38. Six services invented six different env var names — FIXED
**Severity:** the stack could not start. **Root cause of most of this section.**
Built in parallel, each service chose its own spelling for the same values:
`NEXUS_DATABASE_URL` / `DATABASE_URL` / `NEXUS_POSTGRES_DSN` for the DSN;
`NEXUS_HTTP_ADDR` / `HTTP_ADDR` / `RM_HTTP_ADDR` / `NEXUS_WFM_HEALTH_ADDR` for the
health listener; `NEXUS_CPU_MILLIS` vs compose's `AGENT_CAPACITY_CPU_MILLIS`.
Compose emitted one generic set, so every service silently fell back to its
compiled-in default — pointing at `localhost:55432` (the developer's test DB) and
at ports the healthchecks were not probing. Workers registered with EMPTY
capacity, so the scheduler could fit nothing.
**Resolution:** `internal/config.DSN()` accepts every DSN spelling, and compose
emits every listener/capacity name the services actually read. Services are
unit-tested against their own names; compose is not, so compose adapts.
**Lesson for Phase 2:** a shared config package should have been part of the
frozen contract, not left to six independent choices.

### F39. Dockerfile COPY did not preserve the exec bit — FIXED
A plain `COPY --from=build` carries the build stage's mode bits, leaving the
binary non-executable for the unprivileged runtime user. Failed at container
start with a bare `permission denied` naming no cause. Now `COPY --chmod=0755`.

### F40. event-service built the package dir, not its main package — FIXED
`SERVICE_PATH: services/event-service` pointed at the library directory, so the
build produced a non-executable object and the container died with
`exec format error`. Now `services/event-service/cmd/event-service`.

### F41. Agent WAL volume mounted at a path the agent does not use — FIXED
Compose mounted the volume at `/var/lib/nexus/wal` but set `AGENT_WAL_DIR`, which
the agent does not read, so the agent used its default `/var/lib/nexus-agent/wal`
— outside the volume, where the unprivileged user cannot mkdir. Now sets
`NEXUS_WAL_DIR`, and the image pre-creates both paths owned by the runtime user.

### F42. The agent never sent a credential on any gRPC call — FIXED
**Severity:** real gap, not just config.
`GRPCClient` plumbed the post-Register session token but had no bootstrap
credential, and `Register` itself sent nothing at all — so the very first RPC was
unauthenticated and the gateway correctly refused it with `Unauthenticated`.
Register cannot present a session token, because obtaining one is the purpose of
the call, so a separate bootstrap credential is structurally required.
**Resolution:** `WithAPIKey` reads `NEXUS_API_KEY` and attaches `x-api-key` to
every RPC including Register. The two credentials answer different questions —
the API key authenticates the WORKER (may this host join?), the session token
authenticates the SESSION (is this registration still valid?) — so both are sent
once both exist.

### F43. No dev API keys were seeded — FIXED
The migration job creates the `api_keys` table but no rows, so a fresh stack
cannot authenticate anything. Seeded `dev-admin-key` (admin) and
`dev-worker-key` (worker), sha256-hashed at rest.
**Phase 2:** the migration or an init job should seed these, gated on a dev flag.

### F44. REST and worker gRPC accept DIFFERENT auth headers
**Severity:** works, but an avoidable trap. **Status: open, low priority.**
REST reads `Authorization` (Bearer or bare); worker gRPC reads `x-api-key` or
`authorization` metadata. Both are defensible, but an operator who learns one
surface guesses wrong on the other. Worth unifying so both accept both.

### F45. nginx cached the gateway's IP at startup — FIXED
**Severity:** subtle and genuinely hard to debug.
With a literal address in `proxy_pass`, nginx resolves the upstream ONCE at
config load. Any `compose up` that recreates api-gateway gives it a new IP, after
which nginx proxies to the dead one forever — presenting as a 404 from a gateway
that is demonstrably healthy when called directly.
**Resolution:** use Docker's embedded resolver (127.0.0.11) with the target in a
variable, forcing per-request resolution. Verified by force-recreating
api-gateway: previously permanent breakage, now HTTP 200.

### F46. Scheduler had no gRPC service, so PollTask could not claim work — FIXED
The gateway's PollTask and Heartbeat call `scheduler.ClaimForWorker` and
`scheduler.PendingCancellations`, which returned "not wired yet" stubs because
`services.proto` (added for F4/F23/F31) omitted SchedulerService. Added, with
ClaimForWorker returning an EMPTY response rather than an error when nothing
fits, since "no work available" is the normal steady state.

### F47. ResourceVector is stored as protobuf camelCase JSON, not snake_case
**Severity:** upgraded to HIGH once it was seen in production. **Status: FIXED.**
Not merely an operator-SQL trap: the gateway's own capacity aggregation reads
`capacity ->> 'cpu_millis'`, so it summed a key nothing had and reported
**0 cpu / 0 cpu for a fleet with 12 cores and 2 GPUs**. The console showed an
empty capacity summary and blank per-worker CPU/memory columns. A JSONB key that
does not exist is SQL NULL rather than an error, so nothing failed loudly.
**Resolution:** writers now use `protojson.MarshalOptions{UseProtoNames: true}`
(rm_workers, scheduler_json, dagengine_writes), so stored JSON matches the
documented spelling, and migration 0003 rewrites rows already written. The
dual-dialect readers stay, since they cost nothing and tolerate old rows.
Verified: /capacity now reports cpu_millis 12000, gpu_count 2 across 2 workers.
`workers.capacity` and `task_attempts.resource_request` store protojson output,
so keys are `cpuMillis` / `memoryBytes` / `gpuCount` — not the `cpu_millis`
spelling used in the schema comments, the proto source, and CONTRACT.md. Any
operator SQL written from the documentation (`capacity->>'cpu_millis'`) silently
returns NULL rather than erroring, which reads as "this worker advertised no
capacity" — a scary and completely wrong conclusion.
**Resolution options:** marshal with protojson's `UseProtoNames` so stored JSON
matches the documented spelling, or document the camelCase form prominently.
The first is better: the database is an operator-facing surface and should speak
the same names as the docs.

### F48. A worker marked DEAD recovers only via REREGISTER — VERIFIED WORKING
Not a defect; recorded because it was worth proving. Restarting api-gateway is
enough to push workers past the liveness window, after which the sweeper marks
them DEAD and the resource-manager's heartbeat update (deliberately scoped to
ACTIVE/DRAINING) will never touch them again.
Observed end to end: gateway logged "heartbeat from a session that is gone or
DEAD; ordering re-register", both agents ended their sessions and re-registered
with NEW worker ids, and returned to ACTIVE with full advertised capacity. The
§5.5 rule held -- a new session cannot inherit the dead session's leases.
Without that path a healthy host would heartbeat forever against a row that is
never updated and never run another task: idle, but looking perfectly healthy,
which is the worst shape of failure because it is invisible.

---

## From end-to-end execution

### F49. A DAG executes end to end — VERIFIED
Runs `66b19b61`, `0e91724a` and `c6784714` all reached SUCCEEDED with attempts
transitioning PENDING -> DISPATCHED -> RUNNING -> SUCCEEDED, real subprocesses
executed on the agents, and the two nodes of the 2-task DAG landing on DIFFERENT
workers -- so dispatch, leasing, edge dependencies and result reporting all work
across a distributed fleet. Ledger afterwards: 0 reservations, 0 leases, 0
unclaimed timers.

### F50. Result-store env names mismatched, so NOTHING could reach MinIO — FIXED
**Severity:** invariant 6 was silently unenforceable. Same root cause as F38.
The gateway reads `NEXUS_RESULT_STORE_*` and the agent reads `NEXUS_S3_*`, but
compose emitted only `RESULT_STORE_*`. The gateway therefore fell back to its
default `http://localhost:9000` and advertised THAT to every worker in
RegisterResponse -- so no agent could reach MinIO, and the failure was masked
because small outputs are inlined. Compose now emits all three families.

### F51. Task stdout is never captured as an output — OPEN, invariant 6 UNPROVEN
**Severity:** real Phase 1 gap. **Status: open.**
Verified empirically: a task emitting ~300KB (well above the 64KiB inline
threshold) ran, SUCCEEDED, and left `task_attempts.output_ref` NULL with the
MinIO bucket completely empty. The agent never attempted a store.
So the result store is wired and reachable, but nothing currently produces a
ResultRef for a plain command task: there is no convention by which a
`runtime.executor: subprocess` task's stdout becomes a declared output.
**Consequence:** ADR-0008 and invariant 6 (payloads never transit the control
plane) remain UNPROVEN in practice. They are not violated -- no payload is
moving through the control plane because no payload is being captured at all --
but the guarantee is untested end to end.
**Resolution needed:** define how a task emits outputs. The SDK's TaskContext
has `ctx.store.put`, which is the intended path for SDK-authored tasks; a raw
command task needs an explicit convention (a well-known output file or a
stdout-capture opt-in in the task spec).

### F52. JSON Schema and SDK disagree on enum spelling
**Severity:** real interop defect, hit immediately on first hand-written spec.
The gateway's JSON Schema requires SHORT enum forms (`required`, `exponential`,
`all_success`) but the SDK emits PROTO names (`IDEMPOTENCY_MODE_REQUIRED`,
`BACKOFF_STRATEGY_EXPONENTIAL`). SDK-generated specs and hand-written specs
therefore need different spellings for the same field, which contradicts
ADR-0004's "both surfaces produce the identical document".
The examples' `compiled.yaml` files use proto names, so they would be REJECTED
by the schema as written. **The schema should accept both**, exactly as the SDK
already does for `validation`/`validation_error` (F20).

### F53. The console had no way to supply an API key — FIXED
**Severity:** the UI was unusable; every panel showed HTTP 401.
The client read `VITE_NEXUS_API_KEY` at BUILD time and compose never passed it,
so the shipped bundle carried an empty credential and every request was refused.
Baking one in would have been the wrong fix: anything in the bundle is readable
by everyone who loads the page.
**Resolution:** the key is entered in the header and stored in `localStorage`,
alongside the namespace control that already worked this way. It is read on
every request rather than captured once, so saving takes effect immediately
without a reload -- otherwise the console sits on stale 401 panels next to a
valid key, which reads as "my key is wrong". Saving invalidates all queries so
existing error panels re-fetch. The build-time variable remains as a fallback
for local convenience builds only.
The 401 message now names the control that fixes it rather than saying "check
the API key", which leaves an operator hunting through config for something they
can set in the header.
Verified: `dev-admin-key` does not appear in the built bundle, and the console
renders 3 runs and the worker fleet once a key is saved.

### F54. API keys were guessable literals; now generated — FIXED
**Severity:** real security defect, plus the cause of a confusing 401.
`make seed` defaulted to the literals `dev-admin-key` / `dev-worker-key`, and
compose defaulted the agents to the same. Any deployment that forgot to override
them shipped a credential an attacker could guess in one try, and because the
worker key grants registration, that is fleet access.
**Resolution:** `make seed` now generates 256-bit random keys
(`nxs_admin_<64 hex>`), prints each plaintext exactly once, and stores only the
SHA-256 hash. Compose no longer has a literal default for the worker key -- the
variable is required, so a missing key fails loudly at startup rather than
silently falling back to a known one.
Verified: the generated admin key returns 200, the revoked literals return 401,
and both agents re-registered on the generated worker key.

### F55. Console 401 could not be distinguished from 403 — FIXED
A key lacking the role (403) and a key not recognised at all (401) rendered the
same "check the API key" message, sending the operator to re-paste a key that
was never the problem. The console now distinguishes them, names the header
control that fixes it, and reports the stored key's length -- a key pasted from
a terminal with a trailing newline is otherwise indistinguishable from a wrong
one. The key is also trimmed on read as well as on write, since a key stored by
an earlier build was never trimmed at all.

### F56. The console's key indicator did not reflect what was stored — FIXED
**Severity:** made a 401 undiagnosable.
`saveKey` set React state from the typed value while `setApiKey` stored a
trimmed one, and the localStorage write was never verified. If persistence
failed -- blocked site data, private browsing, storage pressure -- the header
displayed "API key ✓" while nothing was stored and every request was refused.
A console that insists a key is set while sending none is the worst version of
this failure, because it directs the operator away from the actual cause.
**Resolution:** `setApiKey` now reads the value back and returns whether it is
actually readable; the header reflects stored state and shows "API key ✗ not
saved" when persistence failed.

### F57. Requiring NEXUS_WORKER_API_KEY broke unrelated compose commands — FIXED
Removing the weak literal default (F54) used compose's `:?` required-variable
form, which is evaluated for EVERY compose invocation -- so `build ui` and even
`ps` failed with a message about a key they do not use. The credential now lives
in `deploy/.env` (gitignored, written by `make seed`), which compose reads
automatically. This keeps the no-weak-default property without making every
command depend on an agent credential.
