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

### F51. Task stdout is never captured as an output — FIXED, invariant 6 NOW PROVEN
**Severity:** real Phase 1 gap. **Status: FIXED and verified end to end.**
Resolution: `runtime.stdout_output` names a DECLARED output that receives the
task's stdout, and `$NEXUS_OUTPUT_DIR/<name>` files are collected for every
declared output. A file WINS over stdout, because writing the file is a
deliberate act while stdout also carries incidental chatter. Only declared names
are collected -- an undeclared file is ignored rather than inferred, since the
DAG validated its edges against the declared interface at submit time. The
gateway rejects a stdout_output naming an undeclared output at SUBMIT time,
because the failure is otherwise silent and late: the task succeeds while
producing nothing and every downstream edge resolves to an absent input.

VERIFIED against the live stack:
- 300,000-byte stdout -> blob in MinIO, `output_ref` holds only the reference
  (`default/4c96aa0e...`, sha256-addressed, 293KiB object confirmed present in
  the bucket). The agent wrote to MinIO DIRECTLY, so no payload transited the
  control plane -- ADR-0008 and invariant 6 are now demonstrated, not assumed.
- 5-byte stdout -> INLINE (`"inline": "dGlueQo="`), and the bucket object count
  stayed at 1, so a trivial output still costs no round-trip.

**A subtle failure found while verifying this** is worth recording: the field
was correctly stored in `workflow_specs` but silently ABSENT from the
materialized `task_nodes.spec_json`, so the agent never captured anything. Cause:
dag-engine and workflow-manager containers were running binaries built before the
proto change, and their `protojson.UnmarshalOptions{DiscardUnknown: true}`
discarded the unknown field without error. DiscardUnknown is right for rolling
upgrades, but it means a stale service silently DROPS new fields rather than
failing. Rebuilding every service onto one generated contract fixed it. Worth a
deployment rule: services sharing a proto must be rebuilt together.
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

### F52b. The JSON Schema lives in TWO places
`schemas/workflow-spec-v1.json` is the canonical copy, but go:embed cannot reach
outside a package directory, so `services/api-gateway/spec/workflow-spec-v1.json`
is a duplicate. A test (`TestEmbeddedSchemaMatchesCanonical`) guards that they
match, which is what caught the drift when only the canonical copy was edited.
Both must be updated together.

### F52. JSON Schema and SDK disagree on enum spelling — FIXED
**Severity:** real interop defect, hit immediately on first hand-written spec.
**Status: FIXED.** The schema now accepts BOTH spellings for every affected enum
(idempotency, retry.strategy, trigger_rule, error classes). Verified: all three
committed examples -- which use proto names -- are accepted by the live gateway.

A SECOND cause was blocking the same examples: F18 added `input_literals` and
`input_params` to TaskSpec, but the JSON Schema never learned them, so
`additionalProperties: false` rejected every SDK-generated spec. Both fields are
now declared. Without this, no SDK-authored workflow could be submitted at all,
which is the very contradiction of ADR-0004 that F52 set out to close.
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

### F58. DEAD worker rows accumulated forever — FIXED
**Severity:** operational noise that buries the signal.
Nothing ever deleted DEAD rows, and every deploy, restart or scale-down leaves
another one: after a single afternoon the console showed 6 DEAD rows against 2
live workers, which is exactly the wrong ratio for a fleet view an operator
scans for problems.
**Resolution:** `ReapDeadWorkers` prunes DEAD rows past
`DeadWorkerRetention` (default 1h), called from the scheduler's existing
liveness loop. Retention is generous on purpose: immediately after a worker
dies its row is the evidence needed to explain why attempts were reclaimed, and
run_events reference the worker id. Deletion is conditional on the row holding
NO leases and NO reservations -- a DEAD row still holding either means the
capacity ledger has not finished unwinding, and deleting it would cascade those
rows away and lose the accounting rather than settle it. Such rows are left for
a later sweep, by which time lease expiry has released them.
Verified: the guard selects exactly the 6 orphaned rows and excludes both ACTIVE
workers.

---

## From exit-criteria verification

### F59. An unschedulable attempt stalled with NO pending_reason — FIXED
**Severity:** high. This is the exact failure §3.2 says pending_reason prevents.
Found while running the 10-node exit criterion: `ingest` requests 100GiB of disk
against 50GiB workers, so it genuinely cannot be placed -- the scheduler was
RIGHT to refuse it. But the attempt sat PENDING with an EMPTY pending_reason, so
the console showed a stuck run beside an idle, healthy-looking fleet and nothing
connecting the two.

Cause: `ClaimForWorker`'s SQL pre-filter (a legitimate optimisation that avoids
paging rows which cannot fit) excludes such attempts BEFORE the authoritative
`domain.Fits` check that records a reason. Excluded rows were never explained.

Two bugs had to be fixed to close it, and both are worth remembering:

1. **The nil-slice bug, again.** The explain query's `NOT (id = ANY($2::uuid[]))`
   received a nil slice, which pgx sends as SQL NULL; `NOT (x = ANY(NULL))` is
   NULL, not true, so EVERY row was filtered out and nothing was ever explained.
   The resource manager's leak detector had the identical bug (F-series, worker
   agent workstream). Verified in Postgres: nil -> NULL, `'{}'` -> true. This is
   an easy mistake to make twice; any `ANY`/`ALL` against a caller-supplied slice
   needs a non-nil guard.
2. **Placement.** The explanation ran only when a poll found NOTHING to claim.
   On a busy queue a poll almost always finds something, so the permanently
   unschedulable attempt was never reached. It now runs on the SUCCESS path too
   -- which is the case that matters most, because the fleet looks healthy while
   one attempt starves.

Verified live: `waiting_for_resources: disk (need 107374182400, free 53687091200)`.

### F60. The ten_node_mixed example cannot run on a default fleet
**Severity:** documentation/example defect, not a platform bug. **Status: open.**
The committed example declares `disk_bytes: 107374182400` (100GiB) on `ingest`,
`gpu_kind: nvidia-a10` on `train_small`, and NO runtime block at all. The compose
fleet advertises 50GiB disk, `nvidia-a100`, and only the `subprocess` executor,
so the example is unschedulable as written -- exactly the shape of problem F59
now makes visible instead of silent.
It is also billed as the Phase 1 exit criterion, so it should be runnable against
the stack the repo ships. Either lower its requests to fit the default fleet or
raise the compose agents' advertised capacity to match the example.

### F61. Multi-queue dispatch does not reach the gpu-queue scheduler — CLOSED (misdiagnosed)
**Severity:** blocked Phase 1 exit criterion 1. **Status: CLOSED. The fan-out was never the bug.**
The 10-node mixed DAG runs its 7 CPU nodes to SUCCEEDED across BOTH workers, then
stalls: `train_large` and `train_small` sit READY/PENDING on the `gpu` queue
forever while `scheduler-gpu` records ZERO claim attempts.

Everything checks out individually, which is what makes this interesting:
- the attempts are PENDING on queue `gpu`, nodes READY, dependencies satisfied;
- the GPU worker advertises `queues={default,gpu}`, `gpu_count=2`,
  `gpu_kind=nvidia-a100` -- an exact match for both tasks;
- `agent-gpu` polls with `NEXUS_QUEUES=default,gpu` and is executing other work;
- the gateway logged `additional scheduler replica wired for worker polls
  addr=scheduler-gpu:9090` at startup, so `claimPeers` is populated;
- `scheduler-gpu` is healthy, listening on 9090, and reachable from the gateway
  (`nc` OPEN);
- the gateway logs NO `scheduler peer failed a worker claim` warning, so it is
  not that the peer is erroring -- the peer loop appears never to be entered.

The fallthrough is conditional: `if err != nil || len(asg) > 0 || len(claimPeers) == 0 { return }`.
So the peer is only tried when the PRIMARY returns cleanly with zero assignments.
The likely cause is that the primary is not returning an empty result the way
that condition expects, but this has not been confirmed and should not be
guessed at.

**Next step:** add a debug log inside `SchedulerClient.ClaimForWorker` recording
the primary's result and whether the peer loop ran. That single line will settle
it; further inspection of correct-looking code will not.

**Note for whoever picks this up:** the queue-per-scheduler-replica design is
itself worth revisiting. A worker serving N queues needs the gateway to fan a
poll across N replicas, and a miss on any of them is silent. An alternative is
one scheduler draining several queues, which removes the fan-out entirely.

---

**RESOLUTION.** The diagnostic was added and the hypothesis above was WRONG. The
peer loop runs on every idle poll:

```
worker claim fan-out  queues=[default gpu] primary_assignments=0
                      peers=1 peer_loop_will_run=true
scheduler peer answered a worker claim  peer_addr=scheduler-gpu:9090
                                        assignments=0
```

`scheduler-gpu` IS offered every poll and answers empty, so the gateway fan-out is
exonerated end to end. Both logs were kept: a silent miss on one of N replicas is
what made this expensive, and the conditional is still conditional.

The real cause is F60. `train_large` asks for 8 CPU / 64 GiB / 200 GiB disk /
`gpu-interconnect=nvlink`, and `train_small` asks for `nvidia-a10`; the shipped
fleet has neither. The gpu scheduler recorded zero claims because it was
correctly refusing work that cannot be placed. Everything "checking out
individually" was accurate — each component was fine, and the mismatch lived
between the example and the fleet, which is the one place nothing was looking.

Two real defects surfaced while proving this:

1. **The idle-queue explanation was rolled back** (see F59, which was only
   half-fixed). On the `ErrNoCandidate` branch `claimOneForWorker` called
   `explainUnschedulable`, which writes `pending_reason`, and then returned
   WITHOUT committing — so the deferred `Rollback` discarded every reason. The
   success path commits, which is why F59 looked fixed: a poll that dispatched
   something carried its explanations along, and only a poll that dispatched
   NOTHING lost them. An idle queue is precisely the case the field exists for.
   Fixed, and pinned by `TestUnschedulableAttemptIsExplainedOnAnIdleQueue`, which
   drives a queue holding one unplaceable attempt so there is no dispatch to
   piggyback a commit on. Nothing covered the store's `ClaimForWorker` at all
   before this.

2. **`LOG_LEVEL` vs `NEXUS_LOG_LEVEL`.** The compose env set only the former; the
   gateway reads the latter, so `NEXUS_LOG_LEVEL=debug` silently did nothing there
   and cost a cycle. Both spellings are now set.

**The design note above still stands** and is now better evidenced: the fan-out
worked, but proving it took a day precisely because a miss would have been
silent. One scheduler draining several queues would remove the failure mode.

### F62. A run with no RUNNING attempts wedges in CANCELLING — FIXED
**Severity:** medium. Blocked the namespace via `max_active_runs`.
Cancelling a run whose attempts are all PENDING leaves it in CANCELLING forever.
`run cancel` reports "running attempts are being signalled; the run reaches
CANCELLED once they stop" — but there are none to signal, so nothing ever drives
the transition, and CANCELLING is not terminal. Two such runs then exhausted
`max_active_runs: 2` and every subsequent create failed, which is how it was
found.

Reproduced with two runs holding one PENDING attempt each and zero RUNNING:
both sat CANCELLING indefinitely and had to be cleared with a direct UPDATE.

This is the same shape as the `MaybeFinalizePause` gap recorded in b97d1ac (a run
stays PAUSING forever and never reaches PAUSED): a state whose exit is driven
only by attempt reports has no path out when there are no attempts to report.
Worth fixing both together — cancel and pause should finalize immediately when
nothing is in flight.

**Also:** `max_active_runs` exhaustion returns gRPC `ResourceExhausted`, which the
gateway maps to **HTTP 500** and the CLI reports as "This is a control-plane
fault." It is not a fault; it is the documented limit doing its job. It should be
429 with the limit named, or an operator will chase a phantom outage.

---

**RESOLUTION.** Three distinct defects, each of which alone was enough to wedge
a cancel. The first was the one that mattered:

1. **`scheduler.CancelRunAttempts` was never wired.** The store method exists and
   the scheduler SERVES the RPC (`nexus.v1.AttemptReportService/CancelRunAttempts`,
   over the same hand-written JSON codec the gateway already uses) -- but
   workflow-manager's client was a stub returning `errNotWired`. So cancel never
   cancelled a single attempt. Worse, `Cancel` logs the failure as *"reconciler
   will retry"* and swallows it, and no reconciler retries this: the operator got
   a success response, the CLI printed "running attempts are being signalled",
   and nothing was signalled. Now wired.

2. **The engine left withheld nodes PENDING.** `suppressDispatch` covers
   CANCELLING, but only the `halted` (fail_fast) branch settled what it withheld
   -- and its comment explains exactly why that is necessary: *"Leaving it PENDING
   is a hung run... the run sits RUNNING forever holding a concurrency slot with
   nothing to alert on."* Cancel withholds work just as permanently and had no
   such branch. `ReasonRunCancelled` was already declared and unused, so the fix
   had been anticipated and never written.

   PAUSING is deliberately excluded: a pause is meant to be resumed and its
   PENDING nodes must survive, so settling them would make pause destructive --
   the very bug b97d1ac fixed.

3. **`Cancel` never asked for a reconcile.** With 1 and 2 fixed the run reached
   CANCELLED, but its graph still showed eight PENDING nodes, because only the
   engine may write `task_nodes` and nothing called it. Added the same
   swallowed-error reconcile `Resume` uses.

Verified live: cancel now returns `state: cancelled` immediately, the racing
attempt is CANCELLED, and all ten nodes settle. Pinned by
`TestReconcile_CancellingSettlesPendingNodes`, which starts from a node still
PENDING when the cancel lands -- the existing cancel test started from nodes
already terminal, which is why it never caught this.

**On the node labels:** descendants of a cancelled node settle as SKIPPED
(propagation runs first and claims the subtree); nodes with no cancelled ancestor
settle as CANCELLED. Deliberate: SKIPPED keeps its existing meaning, matches what
a fail_fast halt produces, and `OnNodesTerminal` treats neither as a failure, so
the run still reports CANCELLED rather than FAILED either way.

**The 500 is fixed too, and the cause is worth recording.** Both ends were
already correct in isolation -- the owner maps `ErrMaxActiveRuns` onto
`ResourceExhausted`, and the gateway maps `ErrMaxActiveRuns` onto 429. The
boundary broke it: a gRPC status crosses the wire as a code and a string, so the
sentinel does not survive, `errors.Is` was always false, and every carefully
mapped code fell through to a blanket 500. `reattachSentinel` now converts the
code back into the sentinel on the gateway side. Any future sentinel mapped in
`runError` needs a case there too, or it silently becomes a 500 again.

### F63. ten_node_mixed asked for hardware the shipped fleet does not have — FIXED
**Severity:** blocked Phase 1 exit criterion 1 (this is F60, fully scoped).
F60 recorded `ingest` (100 GiB disk) and a stray `nvidia-a10`. An audit of all ten
tasks against both agents found FOUR unplaceable, not one:

| task | asked for | fleet has |
|---|---|---|
| `ingest` | 100 GiB disk | 50 GiB |
| `train_baseline` | 8 CPU + 32 GiB | cpu worker 8 CPU/16 GiB, gpu worker 4 CPU/32 GiB — fits neither |
| `train_large` | 8 CPU, 64 GiB, 200 GiB disk, `nvlink` | 4 CPU, 32 GiB, 50 GiB, no such label |
| `train_small` | `nvidia-a10` | only `nvidia-a100` |

`build_features` asked for exactly 50 GiB — the worker's whole disk — so it fit
only on a completely idle worker and any concurrent disk user blocked it.

Fixed by shrinking the example rather than inflating the fleet, so
`docker compose up` + `make submit-example` works out of the box, which is what
an exit criterion needs. The scheduling FACTS each node exists to exercise were
deliberately preserved: `train_large` keeps 2x a100 + the nvlink selector + the
42 GiB VRAM floor, `train_small` keeps its a10. Only the scalars no laptop fleet
can honestly advertise came down.

Two fleet changes, because they are what make the example prove something:
- `agent-gpu` gains `gpu-interconnect=nvlink` (an unsatisfiable node_selector
  tests nothing);
- a new `agent-gpu-a10` advertises 1x `nvidia-a10`, so gpu_kind matching is
  exercised by the shipped stack instead of only in unit tests. The two trainers
  must now land on DIFFERENT workers, and the a10 worker must NOT be able to take
  `train_large`.

All ten tasks are now placeable on the default stack.

### F64. No agent could advertise GPU memory, so every VRAM floor was unsatisfiable — FIXED
**Severity:** high. Silently made a documented scheduling feature unusable.
`domain.Fits` enforces a task's `gpu_memory_bytes` against the worker's, and the
scheduler reports shortfalls against it — but `nexus-agent` had no VRAM flag at
all. It advertised `gpu_count` and `gpu_kind` and left `gpu_memory_bytes` at
zero, so ANY task naming a VRAM requirement was permanently unschedulable on
every fleet. Correctly refused, for a reason no operator would guess from a
worker that plainly has GPUs:

```
waiting_for_resources: gpu_memory (need 42949672960, free 0)
```

Compounding it, the compose file set `AGENT_CAPACITY_GPU_MEMORY_BYTES`, which
nothing has ever read — the stack looked configured for VRAM while advertising
none. Fixed by adding `--gpu-memory-bytes` / `NEXUS_GPU_MEMORY_BYTES` to the
agent and renaming the dead compose variable to match.

This is only visible end to end: the unit tests pass because they construct
capacity vectors directly, and the SQL pre-filter has no VRAM predicate, so the
gap lives exactly where nothing was looking. Found by running the exit criterion.

### F65. The committed examples were not executable — FIXED
**Severity:** high. Blocked Phase 1 exit criterion 1.
None of the three examples in `examples/` had a `runtime` block, so every task
failed on dispatch:

```
InvalidSpec / ERROR_CLASS_VALIDATION:
  prepare: executor: invalid task spec: runtime.command is empty
```

F52 established that all three are ACCEPTED by the live gateway. That is a
weaker claim than it appeared: accepted is not runnable, and no example in the
repo could reach SUCCEEDED. `hello_dag` had never completed a run either.

All three now carry a `subprocess` runtime with a one-line `sh -c echo`, chosen
so the examples demonstrate DAG shape and scheduling constraints without needing
any toolchain on the host. Verified against the default stack: `hello-dag`,
`idempotent-effect` and `ten-node-mixed` all reach SUCCEEDED.

**Worth noting for the exit criteria:** the gap between "the gateway accepts it"
and "the fleet runs it" swallowed F60, F63, F64 and this one. An example that is
never executed in CI decays into a document. Submitting and RUNNING all three
examples against the default stack would have caught every one of them.

### F66. No agent could advertise custom resources either — FIXED
**Severity:** medium. Same shape as F64, found in the same run.
`domain.Fits` treats custom dimensions as countable pooled capacity and its own
comment names the use case ("license tokens, NIC slots"), but `nexus-agent` never
populated `Custom`, so any task requesting one could never be placed:

```
waiting_for_resources: custom:license_token (need 1, free 0)
```

ten_node_mixed's `register` node requests `license_token: 1`, so that half of the
fit check had never once been exercised against a real worker. Added
`--custom-resources` / `NEXUS_CUSTOM_RESOURCES` ("k=n,k2=n2"), and `agent-cpu`
now advertises `license_token=2`. A malformed entry is logged and skipped rather
than fatal: one mis-typed dimension should not take a whole worker out of the
fleet.

F64 and F66 are the same defect twice — a scheduler dimension with no agent-side
way to advertise it. Worth checking any dimension added to `ResourceVector` from
here for an agent flag AND an end-to-end run that uses it, because unit tests
construct the vector directly and cannot see the gap.


### F67. Two allocators for `run_events.seq` collided — FIXED

`run_events` has two writers and they numbered rows differently. event-service
allocates from `workflow_runs.event_seq` (migration 0002, and the EVENT SEQ rule in
CONTRACT.md). The shared `appendEvent` helper used `max(seq)+1` under a
`pg_advisory_xact_lock`, which serializes only the callers that TAKE that lock —
and event-service does not. Observed live:

```
duplicate key value violates unique constraint "run_events_run_id_seq_key"
```

Worse than the race: `max(seq)+1` never advanced the counter, so every run written
by the helper left `event_seq` stale and its next owner-API append was *guaranteed*
to collide. One run's counter read 2 while its actual `max(seq)` was 11; 31 runs on
the dev fleet had drifted. `appendEvent` now allocates from the counter, so the
UPDATE's row lock serializes every writer on one mechanism. Migration 0004 repairs
the already-drifted rows — without it those runs can never record another event.

Worth noting for any future writer of `run_events`: the advisory lock LOOKED
sufficient and was not, because a lock only constrains the code that takes it.

### F68. The workflow-manager's event and scheduler clients were stubs — FIXED

`EventClient.Publish` returned `"event-service gRPC is not wired yet"` and never
sent anything, so no `RunCreated` and no `RunStateChanged` was ever recorded for any
run — the run-level half of the audit trail `run_events` exists to provide was
simply empty. The stub's message also misled: event-service serves plain JSON REST
(`POST /v1/runs/{runID}/events`) and registers no gRPC service at all, so the
"pending proto" everyone was waiting for was never the plan.

A second defect was stacked behind it: every peer is configured `host:9090` because
that is the gRPC convention across the fleet, but event-service binds **8080 only**
— so even a real client would have failed with "connection refused". The client now
adapts that one well-known wrong port and respects any other.

### F69. A peer that MOVED left clients calling the wrong service — FIXED

Recreating several services together makes two of them swap IPs. A client holding an
open connection to the address it remembers then reaches a *different* service:

```
rpc error: code = Unimplemented desc = unknown service nexus.v1.SchedulerService
```

which reads like version skew or a missing registration and is neither. Reproduced
deterministically: recreating dag-engine, scheduler and workflow-manager together
swapped the latter two, after which the gateway held a connection to the
scheduler's address while sending `WorkflowSpecService` over it — 43 errors and
every run creation failing, with all six services healthy.

It cannot be fixed client-side alone, which is the trap. gRPC re-resolves a name
only when a connection BREAKS, and a swapped peer breaks nothing: the TCP session
stays healthy, it just terminates at the wrong process. The fix is
`MaxConnectionAge` on each internal server (`internal/config.PeerServerOptions`), so
connections rotate and each reconnect re-resolves.

### F70. ADR-0011 rule 1's grant enforcement did not exist — FIXED

The ADR states ownership is "enforced by per-service Postgres roles with table-level
grants, so a violation fails at the database, not at code review." There was not one
`GRANT` or `CREATE ROLE` in the repository; every service connected as the owning
superuser. Migration 0005 creates the six roles and the matrix, with the dispatch
exception granted explicitly and column-scoped where possible (`workers.committed`,
`workers.state`, `workflow_runs.event_seq`), and `run_events` INSERT-only for both
its writers so invariant 8 is enforced rather than merely asserted.

Adopting a role is a deployment change (a DSN per service), not a schema change, so
the migration is safe to apply ahead of that work.

### F71. The timer-backlog readiness gate was an availability hazard — FIXED

workflow-manager failed `/readyz` when overdue timers exceeded
`NEXUS_TIMER_BACKLOG_ALERT`. The backlog is a property of the SHARED `timers` table,
so every replica computes the same number and they would all fail readiness
together — emptying the tier out of the load balancer precisely when deadlines were
already firing late, and removing the very replicas that drain the backlog. The
comment claimed it "sheds traffic from this replica", which a global condition
cannot do. An operator who set the threshold low, reasonably believing it an alert,
was configuring an outage.

Now a gauge plus a log warning (`internal/metrics`). On a database error the gauges
keep their last value rather than zeroing, because a zero is indistinguishable from
"the backlog just drained".

### F72. Pause killed the work it exists to preserve — FIXED

`PendingCancellations` included `PAUSING` in its run-state filter, and the gateway
turns every id that query returns into a `CANCEL_ATTEMPT` directive, which the agent
implements as SIGTERM/grace/SIGKILL. Pausing a run therefore killed its in-flight
attempts:

```
{"type":"Signalled","class":"ERROR_CLASS_APPLICATION",
 "message":"workload terminated by signal terminated"}
```

The retry budget was charged for a failure the task never caused, and an operator
pausing to investigate destroyed exactly the compute they were protecting. Draining
needs no directive at all: `PAUSING` already closes the dispatch gate, because the
claim query admits only `PENDING`/`RUNNING` runs.

Two adjacent defects, also fixed: `MaybeFinalizePause` was implemented and had no
caller, so a run never left `PAUSING`; and `Resume` only flipped the state without
asking the dag-engine to reconcile, so a node that became ready during the pause
window stayed `PENDING` and the resumed run was silently wedged.

### F73. `max_attempts` capped nothing — FIXED

`domain.ShouldRetry` compares `task_nodes.attempts_used` against `max_attempts`, but
only the dag-engine's attempt-creation path incremented it. Retries are created by
the scheduler's `CreatePendingRetry`, which never charged the budget, so the counter
stayed at 1 however many times a node was retried and a retryable failure would be
retried indefinitely — bounded only by whatever else happened to make the run
terminal.

### F74. Author-spelled enums were silently discarded — FIXED

`$defs/ErrorClass` accepts both the short authoring spellings and the proto enum
names, so a spec using either passes validation. Three separate places then dropped
them:

- the gateway's `errorClass()` knew only the short form, returning `UNSPECIFIED`
  for every `ERROR_CLASS_*` value;
- `protojson` discards the short form WITHOUT error (`"application"` →
  `retry_on: []`), so the stored node spec carried no policy;
- `TaskSpecView.RetryPolicy()` typed the millisecond fields as `int64`, but
  protojson renders 64-bit integers as quoted strings — so `json.Unmarshal` failed
  on the whole struct and returned an EMPTY policy, discarding `retry_on`,
  `strategy` and the backoff together. The same failure silently erased executor
  constraints and all four timeouts.

Net effect: every `retry_on` in the system was inert, and failures reported
`error_class_not_in_retry_on` naming the exact class the author had allowed.

### F75. The console called endpoints that do not exist — FIXED

`GET /runs/{id}/attempts` is not a registered route and returned 404 (the data is
already inline in `GET /runs/{id}`), so the DAG, node panel and log tail rendered
empty while the data sat on the wire. The events cursor was spelled `since_seq`
where the gateway reads `after_seq`, and an unknown query parameter is ignored
rather than rejected, so the cursor silently replayed the whole history. SSE sent
`follow=true`, which the gateway never reads — it negotiates on `stream=true` or an
`Accept` header `EventSource` cannot set, so live updates always "fell back" to
polling. `client.ts` had no tests at all, which is why three URL-level bugs went
unnoticed while 45 tests passed on pure helpers.

### F76. No backup existed, and rate limiting was claimed but absent — FIXED

Postgres is the sole system of record and had no backup of any kind: no `pg_dump`,
no PITR, no procedure, no RPO/RTO — while `make migrate-down` is a
`DROP TABLE CASCADE` behind a three-second sleep. `deploy/backup.sh` and
`deploy/restore.sh` now exist, with a drill verified against the live stack (all
eleven tables matched exactly), and `docs/OPERATIONS.md` records what the drill does
NOT prove: no service was pointed at the restored database, in-flight leases and
timers referring to vanished workers were not exercised, and MinIO artifacts still
have no backup at all.

Separately, ARCHITECTURE.md §3.2 listed "rate limit" among the gateway's
responsibilities with nothing implementing it, and the CLI already carried a hint
for a 429 the server could never send. Now a per-credential token bucket, off unless
`NEXUS_RATE_LIMIT_RPS` is set.

A pattern across F67–F76 worth stating plainly: in this codebase the inline
comments are consistently more accurate than the design documents. Several of these
defects were a doc asserting a mechanism in the present tense (grants enforcing
ownership, Prometheus metrics, rate limiting) that no code implemented. Prefer the
comments, and treat a doc's present tense as a claim to verify.

### F79. A terminal run state could be computed and then dropped — FIXED
**Severity:** high. Blocked Phase 1 exit criterion 2, intermittently.
Found by the new chaos suite on its first real run, which is the entire argument
for having one: the manual version of the same scenario had passed minutes
earlier.

The DAG engine correctly concluded a run was SUCCEEDED and then lost it:

```
dag-engine  "run reached a terminal state"  state=SUCCEEDED
dag-engine  "could not publish terminal run state"
            error=... rpc error: code = Canceled
                     desc = context canceled while waiting for connections to become ready
```

All three nodes SUCCEEDED; the run sat at RUNNING forever.

The publish used the INBOUND request's context. `ReconcileRun` is called at the
tail of an attempt report, behind the gateway's per-call timeout, so a slow final
reconcile cancels the publish *after* the node work has already committed.

What made it permanent was the comment above it: *"The next reconcile
republishes."* That is true of the RUNNING publish a few lines earlier and false
here. A reconcile happens only when an attempt reports or an operator intervenes
-- and a run whose every node is terminal will never have another attempt. There
is no next reconcile. The engine computed the answer, dropped it, and nothing
would ever ask again.

Fixed by detaching that one publish (`context.WithoutCancel` + its own 30s
budget). The asymmetry with the RUNNING publish above it is deliberate and
commented: that one really is retried, this one is the last word.

**Not fixed, and worth a decision:** there is still no durable backstop. If the
detached publish itself fails -- workflow-manager genuinely down for 30s at that
instant -- the run is stranded exactly as before, and only a sweep for
"non-terminal runs whose nodes are all terminal" would recover it. Detaching
removes the failure we actually hit; it does not make the announcement durable.
The same reasoning applies to every other best-effort publish in this file.

### F80. Chaos suite: exit criterion 2 is met — DONE
`tests/chaos` is the suite architecture §11 lists as a Phase 1 deliverable ("a
chaos suite that kills workers mid-task and asserts exactly-one-logical-effect").
It drives the real compose stack under its own `chaos` build tag so
`go test ./...` can never trip over it: submit a run through the gateway, wait
until a task is genuinely RUNNING, `docker kill` that worker's container, then
assert on the recovery. `make chaos`.

SIGKILL rather than a graceful stop is the point. A SIGTERM starts the agent's
drain and it reports its attempts honestly, which tests the happy path; SIGKILL
means no report ever arrives and the control plane must work out on its own,
from the lease alone, that the work is gone.

The counting is what makes the assertion meaningful. Nexus is at-least-once
(ADR-0006), so "the task ran once" is the wrong claim -- it would be
exactly-once, which the system does not promise. The workflow separates the two
counts physically: each task appends one line per EXECUTION to a file named after
`NEXUS_IDEMPOTENCY_KEY`, which excludes `attempt_number`.

```
node seed    effects=1 executions=1
node slow    effects=1 executions=2   <- killed mid-task
node settle  effects=1 executions=1
```

Two executions, one effect. Two *effects* would mean two idempotency keys for one
logical unit of work, which is the double-dispatch this design exists to prevent.
The test also fails if `slow` executed only once -- a suite that passes without
exercising the path it names is worse than none.

Observed recovery, unprompted and correct: attempt 1 -> LOST (`lease_expired`,
`retry_indeterminate_idempotent`), retried as attempt 2 on a surviving worker,
dead worker declared DEAD with its capacity released. A second test asserts that
last part directly -- a DEAD worker holding committed capacity shrinks the fleet
permanently, one incident at a time, and every individual run still succeeds
while it happens.

Verified over five consecutive runs after F79 was fixed.

**Two fixes the suite needed in itself**, both worth noting because each first
looked like a platform bug: the killed container must be allowed to re-register
before the next test starts (restarting the container is not the same as having
a worker), and the effects must be read from whichever agent is alive rather than
a hard-coded `agent-cpu` -- which is sometimes the container the test just killed.


### F77. Nothing enforced ADR-0011 rule 5's additive-migration requirement — FIXED

Rule 5 promises a rolling deploy can run mixed versions against one schema and
requires additive migrations only, "a column is dropped a release *after* the last
reader is gone". `migrate.sh` applies whatever SQL it finds: it has no opinion about
whether a statement is safe while the previous version of the fleet is still
serving. A `DROP COLUMN`, a rename or a `SET NOT NULL` lands fine on an empty
development database and takes the fleet down mid-deploy.

`internal/migrations` is now a gate in `go test ./...`. Eight syntactic rules,
split into "blocking" (breaks a mixed-version fleet) and "review" (blast radius
scales with table size), each finding naming the file, line and consequence.
Deliberate exceptions go in an allowlist with a reason; a second test fails when an
allowlist entry stops matching, because a stale exemption is how a hazard gets
re-admitted later. Verified by adding a migration containing `DROP COLUMN` plus an
unbounded `UPDATE` — both were caught with actionable messages.

Two things about migration 0002 that the obvious reading gets wrong, and that are
worth recording because they changed the conclusion:

- **The `ADD COLUMN ... NOT NULL DEFAULT 0` does NOT rewrite the table.** On
  PostgreSQL 11+ it is metadata-only. Measured on the PG 16 this stack runs:
  `pg_relation_filenode` is unchanged across the statement. It does take
  `ACCESS EXCLUSIVE`, so it queues behind open transactions, but it is fast — the
  earlier assessment that it rewrites `workflow_runs` was wrong.
- **The backfill's `WHERE` does not bound it.** It belongs to the subquery, so the
  outer `UPDATE` touches every row and holds row locks across the table inside the
  migration's transaction. A naive `/WHERE/` check would have exempted the exact
  statement this package was written to catch, so the rule tests for a *top-level*
  WHERE by parenthesis depth. 0003 and 0004 carry genuine top-level bounds and are
  correctly not flagged — they are the safe shape of the same statement.

0002 was deliberately NOT rewritten. `migrate.sh` keys the ledger on the file name,
so editing an applied migration changes nothing on any database that already ran it
while silently diverging the file from the schema it describes. The cost is
historical and recorded in docs/OPERATIONS.md §6.2 instead.

The mixed-version *allocator* hazard that used to accompany 0002 is genuinely gone:
an old replica allocating `max(seq)+1` beside a new one allocating from `event_seq`
would have reintroduced F7's collision, and F79 removed the last `max(seq)+1`
allocator from the tree.

### F81. Scaling the scheduler added no dispatch capacity — FIXED
**Severity:** high. A documented operation was a no-op.
The compose file tells operators exactly this:

```
# Correctness comes from the atomic claim (§5.3), not leader election, so
# replicas can be scaled up freely: `docker compose up --scale scheduler=3`.
```

Correctness-wise that is true, and the criterion-3 test confirms it. Capacity-wise
it was false: `--scale scheduler=3` produced three replicas of which ONE did all
the work.

```
replica nexus-scheduler-1  claimed 576 attempts
replica nexus-scheduler-2  claimed 0
replica nexus-scheduler-3  claimed 0
```

Two things combine. gRPC defaults to `pick_first`, so the gateway's connection to
`scheduler:9090` binds ONE address and stays there — no load-balancing policy was
set anywhere in the codebase. And a scheduler's background dispatch loop is
disabled when no notifier is configured (the compose default), which the replica
says on startup:

```
push dispatch disabled: no notifier configured, so workers are served
exclusively by ClaimForWorker
loop stopped  loop=dispatch
```

So worker polls arriving over that pinned connection are the ONLY way a replica
ever dispatches. Pin the connection and the other replicas have no path to work
at all.

Load did eventually spread, which is what made this easy to miss: `MaxConnectionAge`
(60s) forces a reconnect and each reconnect re-resolves DNS, so over minutes the
claims drifted across replicas. Balance was a side effect of connection rotation,
not a property of the design — and on any run shorter than the rotation window,
scaling bought nothing.

Fixed with `round_robin` in `PeerDialOptions`, so it applies to every
control-plane peer rather than the scheduler alone. Same fleet, after:

```
replica nexus-scheduler-1  claimed 365 attempts
replica nexus-scheduler-3  claimed  89
replica nexus-scheduler-2  claimed  53
```

**Worth noting:** this composes with the `MaxConnectionAge` design above it rather
than replacing it. That mechanism exists so a client re-resolves after an address
SWAP; round_robin is about using every address a name currently resolves to. Both
are needed, for different failures.

### F82. Criterion 3: two schedulers never double-dispatch under load — DONE
`TestSchedulerReplicasNeverDoubleDispatchUnderLoad` in `tests/chaos`.

`TestConcurrentSchedulersNeverDoubleDispatch` already proved the claim
transaction is safe, and proves it harder in some ways — 8 concurrent claimers,
deliberate over-subscription, three cross-checked ledgers. The gap is what it
cannot reach: it runs 8 goroutines in ONE process against `store.Claim`, the
background dispatch loop. The fleet does not dispatch that way. Real work arrives
through `ClaimForWorker`, from separate scheduler PROCESSES with separate
connection pools, over gRPC through the gateway. Different code path (which had
no integration coverage at all until F61) and a different concurrency model —
and "no leader election is needed" is a claim about processes, not goroutines.

The new test asserts the same invariant one layer out: real replicas, the real
poll path, 8 concurrent 24-wide storms, assertions read from the database rather
than from any replica's own accounting — which is precisely what a
double-dispatch bug would corrupt.

Result over 15 storm runs / 397 attempts: zero attempts dispatched twice, zero
over-commit, no ledger drift, no orphaned reservations.

**Two things the test needed before it proved anything**, both worth recording
because each version passed while testing less than it appeared to:

1. *Sampling only after the runs finish is nearly vacuous.* Leases are DELETED as
   attempts complete, so by the time every run is terminal the table is almost
   empty and "no attempt holds two leases" asks a question of one row. A watcher
   now samples the ledger every 100ms DURING the storms, and the test fails if
   peak concurrent leases never exceeded 1 — i.e. if the storms never actually
   overlapped.

2. *Counting `scheduler-gpu` as an active replica hid F81.* It drains a different
   queue and always claims a little, so "at least 2 replicas competed" passed
   with every default-queue replica but one sitting idle — the exact condition
   the assertion exists to detect. It now counts only `scheduler` replicas, and
   fails with an explanation naming pick_first.

The test scales the schedulers itself rather than skipping when the stack runs
the default single replica: a concurrency test that quietly skips when the
environment is not prepared is a test that never runs, and this is the one
criterion whose entire subject is having more than one of something.

Verified by reverting F81: the test fails with `576/0/0` and names the cause.

---

## Phase 1 exit criteria — all three met

*Architecture §11: "a 10-node DAG with mixed CPU/GPU tasks completes correctly;
killing a worker mid-run yields exactly one logical effect per task; two
schedulers never double-dispatch under load."*

| # | Criterion | Evidence |
|---|---|---|
| 1 | 10-node mixed CPU/GPU DAG completes | `ten-node-mixed` SUCCEEDED, all 10 nodes, 36s, default stack (F63/F64/F65/F66) |
| 2 | Killing a worker yields exactly one logical effect | `make chaos`: `slow effects=1 executions=2` after SIGKILL (F79/F80) |
| 3 | Two schedulers never double-dispatch under load | 15 storms / 397 attempts, 3 replicas, zero duplicates (F81/F82) |

Criteria 2 and 3 are pinned by `tests/chaos`, which runs on the real compose
stack under its own build tag. Criterion 1 has no automated equivalent — it is a
`make submit-example` away, and the four defects that blocked it were all found
by running it by hand. **Running the examples in CI, not merely submitting them,
is the single highest-value thing left undone here.**

### F83. Nothing ran the examples, and nothing ran anything — FIXED
**Severity:** high (process). The repo had NO CI at all: no `.github/workflows`,
nothing that built, vetted, or tested on a push.

That is how F63-F66 all shipped. Each blocked Phase 1 exit criterion 1, each was
found by running an example by hand, and none was reachable by the tests that did
exist:

- **unit tests could not see them** — they construct `ResourceVector` directly,
  so nothing noticed that no agent could advertise GPU memory (F64) or custom
  resources (F66);
- **spec validation could not see them** — F52 proved the examples were
  ACCEPTED, which read as "the examples work" but is a far weaker claim. At that
  moment not one example in the repo could reach SUCCEEDED, because none declared
  a runtime block (F65).

Only running the thing finds those. `.github/workflows/ci.yml` now does, in four
jobs: `build` (build, vet, `go test -race`), `integration` (against a real
Postgres service, migrations applied by a job as ADR-0011 rule 4 requires), `ui`,
and `e2e` — which brings up the compose stack and runs `make chaos`.

`TestEveryCommittedExampleActuallyRuns` is the new part, and it is criterion 1's
first automated proof. It DISCOVERS `examples/*/compiled.yaml` from disk rather
than listing them, so an example added later is covered without anyone
remembering to register it. All five currently run to SUCCEEDED with every node
executed:

```
chaos_effect        3/3   dispatch_storm  26/26   hello_dag  3/3
idempotent_effect   4/4   ten_node_mixed  10/10
```

**Two things it needed before it proved anything**, both of which are the same
mistake in different clothes — a test that passes while checking something
weaker than it claims:

1. *Addressing the run by workflow NAME made it untestable.* Specs are
   content-addressed and immutable, so submitting an edited example returns the
   id of whatever version matches its bytes, and `run create <name>` then uses
   the LATEST registered version instead. Verified by stripping the runtime
   blocks back out of `hello_dag`: the test still passed, because it ran the good
   version registered earlier. It would never have caught a regression in an
   example, which is its whole job. It now pins the run to the spec id the
   submission returned.

2. *A bare timeout is not a diagnosis.* The first failing version said only
   "still RUNNING" after five minutes. It now waits a bounded two minutes and
   prints the attempt states with their `pending_reason` and error — so the same
   induced defect reports `runtime.command is empty`, which names F65 outright.

Also fixed here: `make up` printed the API as port 8080 while compose publishes
8090. Small, but it is the first thing the target tells a new developer to use.

**Still worth doing.** `e2e` builds every service image on each run, which is the
bulk of its wall clock; caching the compose build would pay for itself quickly.
And the suite currently proves the examples run — it does not assert their
OUTPUTS are correct, only that every node succeeded.

### F84. Runs could stall forever with nothing to notice — FIXED
**Severity:** high. Closes the backstop F79 explicitly left open.
Observed live: run `0926cfe0` sat at PAUSING for **seven hours** with all three of
its nodes terminal and nothing in flight. Nothing would ever have moved it.
`MaybeFinalizePause` is reachable only from a DAG-engine report, a report needs a
reconcile, and a reconcile needs an attempt to report — so a run with no live
attempts can never produce one. The conclusion the engine had already computed
was simply never applied.

Three causes of this exact shape have now been fixed at their own call sites:

- **F62** cancel wedged a run in CANCELLING;
- **F79** a terminal publish was cancelled after the node work had committed;
- **b97d1ac** a paused run never reached PAUSED.

Each fix was right, and each was found only because someone noticed a stuck run.
They share a shape, and the shape predicts a fourth. `sweepStalledRuns` in the
workflow-manager reconciler is the backstop: every minute it looks for runs where
every node is terminal, nothing is in flight, and the RUN is still non-terminal,
then asks the engine to conclude them.

**The detection is the point, not the repair.** A sweep that force-transitions
runs can hide the bug that stranded them, so every run it touches is logged at
WARN naming the run, and `nexus_stalled_runs_recovered_total` is an ALARM rather
than a throughput metric. A quiet sweep means the point fixes are holding; a
sweep that keeps finding work means something upstream is broken and this is
papering over it. The repairer is also optional — a deployment that would rather
alert than self-heal can leave it unset and still get the signal.

Two guards make it safe, and both are proven load-bearing by deleting them and
watching the test fail:

- **an age threshold**, because a healthy run legitimately shows every node
  terminal for the moment between its last attempt reporting and the engine
  publishing the outcome. Sweeping that window would replace a correct conclusion
  with a guessed one;
- **a materialized-DAG check**, because a run whose graph does not exist yet also
  has no live attempts, and concluding THAT run would kill it before it started.

Verified end to end against the live stack: a run forced into the F79 shape
(RUNNING, three SUCCEEDED nodes) was skipped while fresh and recovered once it
aged past the threshold, reaching SUCCEEDED with the WARN logged.

**A bug in the first cut, worth recording** because it is the failure mode this
whole session keeps meeting. `main.go` builds the reconciler `Config` as a
literal and never set the new fields, so `StalledRunAge` was zero and the race
guard was disabled in production — the live log read
`stalled_for_at_least: 0`. Zero is not "no minimum age" here; it is "sweep runs
that settled a millisecond ago", the exact case the guard exists to prevent. `New`
now normalizes it, so a caller who forgets the field gets the safe default rather
than the dangerous one, and a test pins that.

### F85. CI silently skipped four integration packages — FIXED
Found while wiring F84's test into CI. Two DSN conventions coexist:
`internal/store`, `resource-manager` and `schedules` read
`NEXUS_TEST_DATABASE_URL`; the scheduler harness reads `NEXUS_TEST_DSN`. The
`integration` job set only the latter, so four packages **skipped** — and a skip
is indistinguishable from a pass in `go test` output. The job would have reported
green having run almost none of the SQL it exists to exercise, including the new
stalled-run query.

Both spellings are now set, plus an explicit step that greps for
`skipping integration test` and fails the job. Verified in both directions: it
passes with the database reachable and fails without it.

This is the third variant of one mistake in this session — a check that passes
while testing less than it appears to (see F82's vacuous lease assertion and
F83's name-addressed example runs). Worth watching for directly: **if a test can
skip, CI must prove it did not.**

### F78. ADR-0011 rule 5 demonstrated on a live stack, not just by inspection

Rule 5 promises a rolling deploy can run mixed versions against one schema. Until now
nothing had exercised it: the migration gate (F77) reasons about SQL statement shapes,
which is necessary and not the same as running an old binary against a new schema.

The identity merge (`c6321ff`, adding `users`, `sessions`, `scoped_tokens`,
`audit_log` via `0007_identity`) made a genuine test cheap, so it was run rather than
argued about. A gateway built from the pre-merge commit (`1831450`, verified to contain
zero `/namespaces/{ns}/users` route strings) was pointed at the post-merge database
(verified to have all four identity tables):

```
healthz                        200
readyz                         200
request with no credential     401
request with a valid API key   200
pre-existing schedule routes   200
routes the binary never knew   404
```

Zero panics in its log. The two results that matter are the last two: an old replica
keeps serving its own routes normally, and a route it has never heard of is a clean
404 rather than a 500 — so a load balancer during a rolling deploy sees a working
replica, not a half-broken one.

This makes `0007_identity` additive **in practice**, which is a stronger claim than
additive by inspection. It also generalises: the property being demonstrated is that
new TABLES are invisible to an old binary, which is the easy case. It says nothing
about an altered or dropped column, and F77's gate is still what prevents those from
being written.

One attribution note, recorded because it cost real time twice in one session: a
running container is NOT evidence of the binary you think it is. The same mixed-version
state was first reported from the container's start time, but the image had been
rebuilt eight minutes after the merge and contained BOTH route sets — so the
"pre-merge binary" was in fact post-merge. Verified here by checking route strings in
the binary rather than by timestamps, which is the only reliable way to tell.

### F86. The double-dispatch test was flaky in the worst direction — FIXED
Found by running `make chaos` against the identity merge (c6321ff) to check
whether the new credential-dispatching `guard()` broke REST auth. It did not --
all 40 routes work, every example still runs, dispatch still spreads evenly
across replicas. But the suite went red anyway, roughly half the time, on my own
assertion:

```
sampled the ledger 18 times under load; peak concurrent leases 1
peak concurrent leases was 1: the storms did not overlap
```

...while the same run showed three replicas claiming 73, 64 and 76 attempts. The
fleet was demonstrably busy; the measurement was wrong.

`peakLeases` is a SAMPLED quantity and could not carry that assertion. Leases are
deleted the instant an attempt completes and these tasks are sub-second, so a
100ms poll routinely lands between them. That makes the test red when the system
is fine -- the one direction a concurrency test must never fail, because it
trains everyone to re-run it, and a re-run is exactly how a real double-dispatch
would get waved through.

Replaced with a sweep line over `(dispatched_at, ended_at)` from `task_attempts`:
every dispatch +1, every end -1, running maximum. Durable, cannot be missed, and
does not care when the test happens to look. The same runs that reported a peak
of 1 now report **9 and 12** overlapping dispatches -- so the storms were always
contending and the sampler was simply blind to it.

Also made the suite's `curl` retry (`--retry 3 --retry-connrefused`). A burst of
requests occasionally hits curl exit 56 when it races the gateway rotating a
connection, which `PeerServerOptions` caps deliberately -- expected behaviour, not
a fault, and a test that treats it as one is flaky for a reason unrelated to what
it asserts.

Verified over six consecutive runs of the test alone and two of the full suite.

**The pattern this belongs to.** That is now the fourth variant in this session of
one mistake: F82's vacuous post-hoc lease check, F83's name-addressed example
runs, F85's silently-skipped CI packages, and this. Each was a check that did not
measure what it claimed. The first three passed while proving nothing; this one
failed while nothing was wrong. Same root cause -- **the instrument was chosen
for convenience rather than for whether it can actually see the thing** -- and the
failing direction is the more expensive one, because a green test that proves
nothing is discovered eventually, while a red test that means nothing gets
re-run until it passes.

### F87. Two migrations share version 0007 — FIXED (by another session)
`0007_backfill_lease` and `0007_identity` were authored on parallel branches and
both landed. Harmless right now, verified rather than assumed: they touch
disjoint tables (`schedule_backfills` vs the identity tables), neither depends on
the other, and `schema_migrations` keys on the full filename so both applied
cleanly in glob order.

The hazard is that the number no longer orders anything. `make migrate-up`
iterates `services/migrations/*.up.sql` in glob order, so two files at the same
version apply in whatever order the shell returns -- fine for independent
migrations, wrong the moment a same-numbered pair has a dependency, and silent
either way. A duplicate version should fail the migration job rather than be
resolved by `ls`.

**RESOLVED in a506ab4 by a parallel session**, which renumbered backfill_lease to
0008 (rather than identity, because 0007_identity was already applied and
referenced by merged commits while backfill_lease was newer). Three sessions
found this independently and none assumed another had it, which is the outcome to
want from parallel work.

Verified here rather than taken on report: `services/migrations` now holds one
migration per version 0001-0008 with no collision, and `TestVersionNumbersAreUnique`
in `internal/migrations` guards it. Mutation-tested by renaming 0008 back to 0007
-- the test fails naming both files, and passes again on restore.

**Where the gate actually sits, which is worth knowing.** It is a Go test, not a
check inside `deploy/migrate.sh`, and that script still applies `*.up.sql` in
lexical glob order with no duplicate detection (confirmed by reading it). So a
deployment path that applied migrations WITHOUT going through CI would still
resolve a duplicate pair by `ls`. In practice CI runs `go test -race ./...`
before anything deploys, so a duplicate breaks the build earlier than it could
reach a migration job -- earlier and better than what F87 originally asked for.
The residual gap is only for a deploy that bypasses CI entirely.

### F88. This file had the same collision it was documenting — FIXED
Closing F87 surfaced nine duplicate finding numbers: F67 through F75 were each
used twice, by two sessions numbering in parallel from the same high-water mark.
The findings file had reproduced, in its own headings, exactly the defect F87
records about `services/migrations`.

Renumbered mine to F79-F87 and left theirs at F67-F78, on the same reasoning e2
used for the migrations: theirs are older and referenced by merged commit
messages, so moving them would break references that already exist in history.
Cross-references inside the renumbered sections were rewritten with them; the
identical numbers appearing in earlier sections belong to the other session's
findings and were deliberately left alone.

**The general point, since it has now cost time twice in one day.** Any
monotonically numbered namespace that several people append to concurrently will
collide, and neither of ours failed loudly -- migrations resolved the tie by
`ls`, this file just had two headings with one number. The migrations case now
has `TestVersionNumbersAreUnique`. This file has nothing, and a duplicate here is
cheaper (a confusing reference, not a mis-ordered schema change), so a gate may
not be worth it -- but the next such namespace someone adds is worth checking
before it has two authors.

### F89. Verification-by-grep produced confident wrong answers, repeatedly — NOTE
Not a defect in the system. A defect in how all three sessions were CHECKING the
system, worth recording because it wasted more time today than any single bug and
it is invisible by construction.

Six instances across three sessions, all the same shape -- **a check that returns
a plausible answer when it has actually failed to look at the right thing**:

| # | Check | What it actually did |
|---|---|---|
| F82 | post-hoc `leases` count | table nearly empty by then; asked a question of one row |
| F83 | `run create <name>` | ran the LATEST spec version, not the edited file |
| F85 | CI integration job | four packages skipped; a skip reads as a pass |
| F86 | `peakLeases` sampler | 100ms poll blind to sub-second work; red while fine |
| — | `grep -oE '^### F[0-9]+'` | truncated `F52b` to `F52`, inventing a duplicate |
| — | container `StartedAt`, a grep against a path that did not exist | plausible answers to unasked questions |

The first four passed or failed for reasons unrelated to the system. The last two
are the same error one level up: the instrument was wrong, so the ANSWER about
the instrument was wrong.

**Why this shape is expensive.** A broken check does not error. It returns
something that looks like a result, so it is believed, and the belief is acted
on. F82/F83/F85 were green while proving nothing -- discovered eventually. F86
was red while nothing was wrong, which is worse: red-when-fine trains everyone to
re-run, and a re-run is exactly how a real double-dispatch gets waved through.

**What actually worked**, in every case: pick an instrument that CANNOT miss the
thing, and prove it fails when the thing is broken. The sweep line over
`(dispatched_at, ended_at)` cannot miss a dispatch because those rows are
durable. `TestVersionNumbersAreUnique` was mutation-tested by reintroducing the
duplicate. Both F82's and F84's guards were verified by deleting them and
watching the negative cases fail. **A check nobody has seen fail is not yet
evidence** -- it is an untested assertion about an untested assertion.

**Adjacent inventory**, since F88's point generalises the same way. Concurrent
append-only namespaces here: migration versions (gated by
`TestVersionNumbersAreUnique`), finding numbers (F88 -- collided, cheap, left
ungated), ADR numbers in `docs/adr` (0001-0011, contiguous, currently
single-author), and `run_events.seq`. That last one is the model worth copying:
it allocates with `UPDATE workflow_runs SET event_seq = event_seq + 1 RETURNING`,
a single atomic statement. It got there by being the bug first -- `max(seq)+1`
under an advisory lock, which drifted 31 runs apart before F79 fixed it.

### F90. Schedules were UTC-only, which silently shifted every local schedule — FIXED

`schedules.timezone` was stored and never consulted, so every cron expression was
evaluated in UTC. A "0 2 * * *" schedule created by an author thinking in London time
ran at 02:00 UTC — correct in winter, an hour wrong all summer, with nothing reporting
it. Backfill made it worse: catching up six months of history crosses a transition, so
the shift changes partway through a single job.

Now evaluated in the schedule's own IANA zone. That required deciding the two cases
where wall-clock time is neither total nor unique, and Go's `time.Date` does something
surprising in both — measured against tzdata rather than assumed:

- **The spring gap.** An hour of local time never occurs (2026-03-29, London,
  01:00–02:00). `time.Date` normalizes a nonexistent wall time FORWARD, so `01:30`
  returns `02:30` — usable, but local 01:30 and local 02:30 then resolve to the SAME
  instant, and a naive walk fires it twice. Verified: an hourly walk produced one
  instant from two distinct local hours.
- **The autumn fold.** An hour occurs twice (2026-10-25, London). `time.Date` returns
  the SECOND occurrence, so the first is never produced and a tick is silently skipped.
  Verified: the 01:00 BST instant was never generated by a local hourly walk.

The rule adopted is **exactly once per wall-clock time the author wrote**:

- a gap tick fires once, at the first instant that exists. Skipping the day is the
  other defensible option and is worse for the common case — a nightly backup should
  run an hour late rather than not at all. The normalization collision is suppressed so
  it cannot double.
- a fold tick fires once, on the FIRST occurrence. Firing twice would manufacture a
  duplicate for one logical time; firing on the second delays it an hour for nothing.

UTC remains the default and has neither case by construction, so `Parse` keeps its
signature and an unmigrated caller cannot regress.

Two things worth carrying forward:

- **An unloadable timezone is a hard error, not a UTC fallback.** Falling back would
  reintroduce exactly this bug as "graceful" degradation. A schedule that cannot be
  compiled stays unscheduled and is reported every sweep.
- **A mutation test found a guard nothing exercised.** Deleting the gap-collision
  suppression left every test green, because the chained `Next()` walk discards the
  duplicate via its `After(after)` ordering guard anyway. The suppression is still
  correct for a direct call, so a test now exercises `resolve` directly. A guard no
  test covers is one the next person deletes while tidying.

Verified live: the same expression previews 02:00 local in UTC, Europe/London and
America/New_York with the right offsets; a minutely London schedule fired two runs to
SUCCEEDED, with `next_fire_at` stored as the UTC instant and the preview rendered in the
author's wall clock.

### F91. The UI test suite ran against stale compiled JavaScript, not the sources — FIXED

**Severity:** every UI test result before this was unevidence. Not wrong — unevidence.

The old typecheck script (`tsc -b --noEmit false --emitDeclarationOnly false`, F-fixed
in 29ab9a8) explicitly forced emission, leaving 37 `.js` files beside their `.ts`/`.tsx`
sources under `ui/src/`. `.gitignore:15` matches `ui/src/**/*.js`, so `git status` never
showed them. Vite resolves the extensionless import `./Status` to `Status.js` BEFORE
`Status.tsx`, so every import in every test loaded the stale compile.

Found the way this class of thing is always found: a break-verification refused to go
red. Removing the stale-data guard from `Status.tsx` left all 14 of its tests passing.

Reproduced deliberately to confirm the mechanism rather than infer it. With ONE stale
`Status.js` restored and `StaleWarning` replaced by `return null`, the file's tests are
14 passed. Delete the shadow, same breakage in place: 1 failed | 13 passed. Restore the
source: 14 passed.

`vite dev` resolves identically, so local development was serving stale code too.

All 152 pre-existing tests still pass against the real sources, so no regression was
being masked. That is luck, not a defense: the suite could not have caught one.

**Fix:** the emitter was already gone — 29ab9a8 replaced that script while fixing a
separate defect (it always failed its first command and passed via `||`, so it never
checked tsconfig.node.json at all). The 37 residual artifacts are quarantined outside
the repo. `tsconfig.app.json` sets `noEmit: true`, and `build` runs `tsc -b` against it,
so nothing in the current scripts re-emits.

**The generalisable part, and why this belongs next to F89.** F89 catalogued six checks
that returned plausible answers while looking at the wrong thing. This is the same shape
one level deeper: not a wrong check, but a correct check pointed at a stale copy of the
subject. A suite that cannot fail is indistinguishable from a suite that passes, and the
only thing that separated them here was someone insisting on watching a test go red
before trusting it green.

**Related:** [[F89]] — the check that lies. The discipline that caught this is the one
F89 prescribes: prove the instrument fails when the thing is broken.

### F92. Certificates could not rotate without a restart, and TLS.md said NEXUS_ENV armed a guard it does not — FIXED

(Number chosen by the certificate workstream on a parallel branch; renumber on merge
if another F92 lands first — see F88.)

**What was wrong.** `internal/transport` read the key pair and CA bundle once, at
startup. A renewed certificate on disk did nothing to a running process, so every
renewal was silently coupled to a restart, and a missed restart meant serving the
old leaf until it expired and every handshake in the control plane failed at once.
Nothing measured expiry. Two parts of `docs/TLS.md` were also stale in ways an
operator would act on: §6 said nothing was wired (every server and client already
called `ServerOptions`/`DialOption`), and §4.4 said `NEXUS_ENV=production` makes
plaintext fatal — the code and `TestNexusEnvDoesNotArmTheTLSGuard` say the
opposite, deliberately. An operator trusting §4.4 would believe plaintext was
impossible in production while it was merely unguarded.

**Fix.** Servers answer each handshake from `GetConfigForClient`, clients build a
config per handshake with `GetClientCertificate`, both from a snapshot a poller
replaces only after validation (key matches cert, within validity, bundle holds a
valid CA). Rejected material is logged once, counted, flagged by
`nexus_tls_reload_stale`, and the old material keeps serving. CA bundles may hold
several CAs, which makes CA rotation hitless. Issuance: `deploy/nexus-certs.sh`
(compose via `make dev-certs` / `make up-tls`, the Portainer host, and CSR-based
agent enrolment where the agent's key never leaves its host), and cert-manager
manifests for AKS (`deploy/k8s/cert-*`). Runbook and alerts: OPERATIONS §9.

**The F89 part.** Every rotation test asserts on the serial number presented in a
real handshake, in both directions, never on the reloader's own bookkeeping — a
"reload succeeded" check could pass while handshakes still used the old leaf. Five
mutations were then applied to prove the tests can fail: a no-op poller, a server
pinned to its startup snapshot, a client pinned to its first snapshot, no validity
check, no debounce. Each turned tests red. The first mutation run was itself
misleading: M1 appeared to leave the integration tests green because the debounce
test's writer goroutine outlived its test, panicked the binary, and hid every later
test's result. `ok`/`FAIL` at package level said FAIL; the per-test list simply
omitted the tests that never ran. Fixed (the goroutine is joined), re-run, all five
mutations caught. **A mutation run whose failure list is shorter than expected is
not evidence either** — count what ran.

**Still open.** Only workflow-manager serves `/metrics`, so only it exports the
served-certificate metrics (others log a WARN near expiry). No revocation; agent
connections already open on the gateway's worker listener (which has no
`MaxConnectionAge`) survive a CA withdrawal until they drop. Nothing on AKS or the
Portainer host has been applied or run; the full `make up-tls` stack has not been
brought up end to end. `TestServerNameOverrideFixesTheComposeNameMismatch` takes
1–9s under `-race` in a full package run (0.06s alone) — measured identical on the
pre-change code, so not introduced here, but within reach of its 10s deadline.

### F92. Registration accepted any email address, and there was no password recovery — FIXED

Self-service registration (0013) created a tenant owner for any address typed in, and nothing proved the registrant controlled it. There was also no recovery path: a forgotten password needed an administrator of the same tenant, and the registrant was usually the only administrator. MULTITENANCY.md listed both as prerequisites for public registration.

**Fix:** migration 0017 (`users.email_verified_at`, `email_tokens`), `internal/mail` (SMTP with STARTTLS or implicit TLS, timeouts and retry), four unguarded and rate-limited gateway routes, and console pages. The policy, the no-oracle design and the limits are in docs/MULTITENANCY.md; configuration is in docs/OPERATIONS.md §9. Decisions worth recording here:

- **Existing accounts are verified by the migration, without a backfill.** `ADD COLUMN … DEFAULT now()` followed by `DROP DEFAULT` stores the value as the column's missing value for existing rows. New rows, including rows an old replica inserts mid-deploy, are unverified. This was verified on Postgres 16 and passes the migration safety gate unmodified.
- **Unverified accounts cannot sign in**, and the 403 is returned only after the password is verified. Admin-created and address-changed accounts are unverified too; a trigger clears verification when the address changes.
- **Link requests are answered before the lookup**, so the response does not depend on whether the account exists in body, headers or timing.
- **A disabled mailer fails loudly.** It returns 503 `email_unavailable` and logs a WARN at startup, and never reports a successful send.

**Found along the way (not fixed, outside this slice):**

- The existing per-credential limiter keys unauthenticated requests on `r.RemoteAddr` *including the ephemeral port*. Each new TCP connection therefore gets a fresh bucket, so login and registration are barely limited against a client that does not reuse connections. The new per-IP limiter strips the port.
- `clientIP()` trusts `X-Forwarded-For` from any peer. That is fine for audit notes, as its comment says, and wrong for limits, which is why the new routes use `requesterIP()` instead.
- Several pre-0014 store integration tests create rows in namespaces that were never registered as tenants and now fail with `Namespace must belong to a registered tenant`: `TestIntegrationDisabledUserCannotAuthenticate`, `TestIntegrationDuplicateEmailIsRejected`, `TestIntegrationSpecContentAddressing`, `TestConcurrentReserveDoesNotOverCommit`, `TestConcurrentReconcileDoesNotDoubleSubtract`, `TestConcurrentTickClaimsYieldOneWinner` and `TestConcurrentEventAppendsNeverCollide`. `TestIntegrationTenantRegistrationIsolation` uses fixed emails and counts every registration in the database, so it passes only once per database.
- The default integration DSN `localhost:55432` is, on at least one developer machine, a different product's Postgres (`polaris-postgres`). Always set `NEXUS_TEST_DATABASE_URL`.
- `deploy/render_compose.py` deploys the development compose file, so development defaults (now including Mailpit as the SMTP host) reach deployed hosts unless they are overridden.

### F92. NATS was designed as publish-after-commit, which loses messages; built as an outbox instead — DONE

ADR-0003 and §5.3 step 5 said: commit the claim, then publish the notification.
That is correct about *where not to publish* (never inside the transaction) and
wrong about what it leaves: a process that dies between COMMIT and publish drops
the message, and nothing ever notices, because the bus is a hint and a missing
hint is silent. The same shape applied to run-event fan-out would silently drop
UI updates.

Built instead (migration 0016, `internal/bus`, `services/event-service/relay`):

- **A transactional outbox, written by the fact's own statement.** Every
  `run_events` append (including `AttemptDispatched` inside the §5.3 claim),
  every new `PENDING` attempt and every run state transition inserts its outbox
  row as a data-modifying CTE of the same SQL statement. The claim gains no round
  trip and no lock, and its isolation level is unchanged. A CTE also cannot be
  "forgotten" by a later refactor the way a second call can.
- **The event-service owns and relays it**, not the scheduler: the one component
  allowed to be down stays off the one path that must never stall. Writers have
  INSERT-only grants, like the dispatch exception's `run_events` grant.
- **Effectively-once into JetStream** via `Nats-Msg-Id` = outbox id + created_at
  (created_at so a restored database whose id sequence rewinds cannot collide
  with an old id inside the duplicate window). Rows are claimed `FOR UPDATE SKIP
  LOCKED`, marked only after the broker acks, in the same transaction.
- **Hints mean "work may exist", not "you were dispatched X".** Dispatch is pull,
  so the worker that wins a claim already has the attempt; what is worth
  broadcasting is that claimable work appeared. The agent uses a hint only to cut
  the pause *between* long polls. It never cancels a poll in flight: a cancelled
  `PollTask` whose claim had already committed would strand a lease until expiry
  and turn the attempt LOST.
- **Periodic reconciliation**: the relay re-announces every queue with pending
  work every 10s, so transitions no outbox row covers (resume, freed capacity)
  and anything missed during an outage are re-hinted from Postgres.

**Honest scale of the latency win.** The gateway already re-checks a held long
poll every 250ms, so with a healthy control plane a hint saves at most that pause
plus the between-poll gap. The larger payoffs -- raising the gateway's re-check
interval to cut per-worker claim transactions, and live UI updates without the
1s SSE poll -- are now possible but only the second is wired (SSE tails wake on
run subjects; the 1s poll stays as the fallback).

**Verification, and making it able to fail (F89).** Real NATS and real Postgres in
throwaway containers (`make test-nats`): rollback leaves no message (with a
committed transition as the control), duplicate re-publish is absorbed, a crash
between ack and mark re-sends without a duplicate, a real `docker stop`/`start`
of the broker drains a 40-row backlog written during the outage exactly once,
two relays never publish the same row (made deterministic by holding one relay's
batch open rather than hoping for a race, F86), and duplicate/stale hints to two
workers produce exactly one claim. Mutation-checked: removing `WithMsgID` fails
the dedupe and crash tests; removing `SKIP LOCKED` fails the two-relay test.
That test first HUNG under the mutation instead of failing -- the held relay kept
its row locks and the cleanup `DELETE` waited on them forever -- so it now
releases on every exit path. A test that can only hang when the property breaks
is F89's red-while-fine problem in a new shape.

**Also found:** on a freshly migrated database, most pre-existing integration
fixtures in `internal/store`, `services/scheduler`, `resource-manager` and
`api-gateway/rest` fail with `Namespace must belong to a registered tenant`:
they predate migration 0014 and never create a tenant row. CI applies migrations
too, so this is worth checking there. The NATS tests register their tenants.
Not fixed here (out of scope).
