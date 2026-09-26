# Nexus — Compatibility and versioning policy

This document is the compatibility contract for KGF Nexus: what a version number
means, what may change within a major version, which mixed-version combinations
are supported during an upgrade, and how much of that the code and CI enforce.

It separates **policy** (what we promise and what reviewers hold changes to) from
**enforced** (what a test, a schema or the runtime checks). Where the two differ,
the gap is stated. A promise with no check behind it can break without anyone
noticing.

| Promise | Status today |
|---|---|
| A rolling deploy can run the previous release's services against the new schema (ADR-0011 rule 5) | **Partly enforced.** The syntactic gate (`internal/migrations`) blocks the obvious hazards. The previous release's DB integration suite runs against the upgraded schema (§7, `make compat-smoke`, CI job `compat`). Built binaries and gRPC between versions are **not** tested. |
| Migration history is append-only: an applied migration is never edited, removed or reordered | **Enforced** by `tests/compat/previous-release-smoke.sh` (§7.1 b), for the previous release. |
| `/v1` REST and `nexus.v1` proto change only additively within a major version | **Policy only.** No `buf breaking`, no OpenAPI diff. Reviewers enforce it. §3.7. |
| N-1 agent against an N control plane (ARCHITECTURE.md §9, "Versioning") | **Policy, partially designed-in, untested.** No test runs an old agent against a new gateway, and the gateway has no minimum agent version. §4. |
| `WorkflowSpec.api_version` pins the grammar | **Enforced**: JSON Schema `const: "nexus.dev/v1"`. §5. |
| SDK supports Python 3.11–3.13 | **Enforced** by the CI `sdk` matrix. §8. |

---

## 1. Components and where their versions come from

| Component | Version source today | Stamped at build? |
|---|---|---|
| Six control-plane services, cluster-controller, artifact-gc | `var version = "dev"` in each `main` package, e.g. `services/api-gateway/cmd/api-gateway/main.go:418`. Exported as the `version` label of `nexus_build_info` (`internal/metrics/service.go:22`). | `deploy/Dockerfile.service:43` passes `-ldflags "-X main.version=${VERSION}"`. `deploy/scripts/k8s-dev-images.sh:29` sets `VERSION` to the 12-char git SHA. The compose build passes no `VERSION`, so compose images report `dev`. |
| Worker agent | `agent/cmd/nexus-agent/main.go:39`, same `-ldflags` mechanism. Sent as `RegisterRequest.agent_version` (field 7), stored in `workers.agent_version`, shown in the fleet view. | Same as the services. |
| `nexusctl` | `cli/internal/cmd/root.go:30` `Version = "0.1.0-dev"` ("stamped at build time via -ldflags"). | **No build in the repo stamps it.** The HTTP `User-Agent` is the constant `nexusctl/0.1` (`cli/internal/client/client.go:30`). |
| Python SDK | `sdk/python/pyproject.toml` `version = "0.1.0"`. `User-Agent: nexus-python-sdk/0.1` (`sdk/python/nexus/client.py:40`). | Static. |
| Helm chart | `deploy/helm/kgf-nexus/Chart.yaml`: `version: 0.1.0`, `appVersion: "0.1.0"`. | Images come from `image.tag`, **not** from `appVersion`. The chart fails to render without a tag (`_helpers.tpl`: "pin an immutable tag (a git SHA), never latest"). `app.kubernetes.io/version` is set to `image.tag`. |

**Current version: 0.1.0**, pre-1.0, with **no release tags** in the repository
(`git tag` lists only `pre-identity-merge`). What a deployment actually runs is
identified by the image tag, which is a git SHA. The chart's `appVersion` is a
label and does not select images.

## 2. What MAJOR.MINOR.PATCH means for this product

Nexus is versioned as **one product**: control plane, agent, migrate image, UI,
chart, CLI and SDK share a version, cut from one commit of this monorepo
(ADR-0001). A release is a git tag `vMAJOR.MINOR.PATCH` on `main`, and the
images of that release are tagged with the same string (plus the immutable SHA
tag the chart already requires).

| Bump | Allowed changes |
|---|---|
| **PATCH** | Bug fixes, performance, documentation. No new API surface, no schema change except a fix that is itself additive (e.g. a missing index). |
| **MINOR** | Additive changes only (§3.2): new endpoints, RPCs, optional fields, enum values, error codes, tables, nullable columns, migrations that pass §6. Deprecations may be announced (§3.6). |
| **MAJOR** | Anything that breaks a client, agent or previous-release service: removing or renaming a field, endpoint or RPC; changing a field's type or meaning; a new `api_version` (`nexus.dev/v2`); `/v2` REST; `nexus.v2` proto; a migration that drops what the previous release reads. |

**Pre-1.0 caveat, stated honestly.** Under semver, 0.x promises nothing. This
policy applies the MINOR rules to 0.x anyway, because operators are already
running rolling upgrades. The exception is that a 0.x MINOR *may* contain a
breaking change **if** its release notes say so and give the upgrade procedure.
From 1.0 the table above is binding.

**Gap:** no release process exists yet (no tags, no changelog, no pipeline that
tags images with a version). Until one does, "previous release" means the
previous merge on `main` (§7.2).

## 3. API versioning: `/v1` REST and `nexus.v1` proto

### 3.1 Surfaces

| Surface | Consumers | Version marker |
|---|---|---|
| REST `/v1/...` on the api-gateway (`services/api-gateway/rest/api.go`) | UI, `nexusctl`, Python SDK, customers | URL prefix `/v1` |
| `nexus.v1.WorkerService` (`proto/nexus/v1/worker.proto`) | worker agents | proto package `nexus.v1` |
| Internal gRPC between services (`proto/nexus/v1/services.proto`) | control-plane services only | proto package `nexus.v1` |
| WorkflowSpec document (`schemas/workflow-spec-v1.json`, `workflow.proto`, `task.proto`) | SDK, YAML authors, stored forever in `workflow_specs.spec_json` | `api_version: nexus.dev/v1` (§5) |

### 3.2 Additive-only rules within a major version

Allowed in a MINOR:

- **New proto field** with a **new, never-used field number**. It must be
  meaningful when absent: the zero value has to mean "the old behaviour". The
  precedent is `RetryRunRequest.node_ids = 3` (`services.proto:178`). An empty
  list keeps the old meaning, and the gateway validates the selection itself
  because an older workflow-manager ignores the field.
- **New RPC**, provided every caller treats `UNIMPLEMENTED` as "the peer predates
  this" and not as an error. Precedents: `ReportLogs`. The agent calls it only
  when `RegisterResponse.log_shipping.enabled`, and fences off shipping on
  `UNIMPLEMENTED` (`agent/internal/logship/shipper.go:590`). `RetryRun` against
  an engine without `ResetNodes` returns 501 `retry_unavailable`
  (`services/api-gateway/rest/retry.go:170`, `workflow-manager/grpcapi/clients.go:175`).
- **New REST endpoint**, or a new **optional** request field, or a new
  **response** field.
- **New enum value**, provided every consumer has a default branch that
  tolerates it (§3.4).
- **New error code** string (§3.5).
- **Capability negotiation** instead of version checks: the server advertises
  a feature in a response (the `log_shipping` pattern), and the client uses the
  feature only when it is advertised.

Never allowed within a major:

- Removing or renaming a field, RPC, endpoint, enum value or JSON key.
- **Reusing a field number.** A removed proto field's number and name go in a
  `reserved` statement. No field has been removed so far, so there are no
  `reserved` lines in `proto/nexus/v1` today.
- Changing a field's type, cardinality (`repeated` ↔ singular) or meaning.
- Making an optional request field required, or narrowing what a field accepts.
- Changing the HTTP status or error code of an existing, documented failure.

### 3.3 Unknown-field tolerance: what the code does today

| Path | Behaviour | Where |
|---|---|---|
| Proto binary, all gRPC | Unknown fields are preserved and ignored (proto3). An old peer silently ignores new fields. | protobuf-go default |
| Stored spec / task JSON read by the DAG engine and scheduler | `protojson.UnmarshalOptions{DiscardUnknown: true}`: a spec written by a newer service still parses. This option also ignores unknown **enum names**. | `internal/store/dagengine_store.go:132`, `services/scheduler/grpcapi.go:178`, `internal/store/rm_workers.go:304` |
| REST responses read by the CLI, SDK and UI | Tolerant. Go `encoding/json` ignores unknown keys, and the SDK and UI read dicts/objects. | `cli/internal/client`, `sdk/python/nexus/client.py` |
| REST requests to four admin endpoints | **Strict**: `DisallowUnknownFields`. A newer client that sends a field an older gateway does not know gets `400`. | `rest/rbac.go:343`, `rest/retry.go:61`, `rest/platform.go:32`, `rest/clusters.go:48` |
| WorkflowSpec submission | **Strict**: `additionalProperties: false` throughout the schema. A spec that uses a field added in N+1 is rejected by an N gateway with a structural validation error. | `schemas/workflow-spec-v1.json:13` and every `$def` |

The strict paths are deliberate. A typo'd field that was silently ignored would
be worse. The consequence is the rule in §8: **a client or SDK newer than the
control plane is not supported** where it uses newer request fields.

### 3.4 Enums

Proto3 enums are open in Go: an unknown numeric value decodes and reaches the
code. Every `switch` over a received enum must have a `default` that logs and
ignores the value. The model is the agent's directive loop
(`agent/internal/heartbeat/heartbeat.go:240`): "an unknown directive from a
newer control plane must be ignored, not fatal". In the spec, enum values
accept both the short and the `PROTO_CONSTANT` spellings, and the gateway
normalizes them before hashing (`services/api-gateway/spec/pipeline.go`). A new
value is therefore additive only once the gateway that normalizes it is
deployed.

**Unverified:** there is no test that every enum switch has a tolerant default.
Review is the only check.

### 3.5 Error codes

REST errors are `{"error": "<snake_case_code>", "message": "..."}`
(`rest/wire.go:59`). The **code** is contract and the **message** is not.
Clients branch on the code and fall back to the HTTP status class for codes they
do not know. New codes are additive. Renaming a code is breaking. The worker
protocol's stale-lease rejection `Ack{accepted:false, reason:"stale_lease_token"}`
is frozen (CONTRACT.md invariant 3).

**Gap:** the codes are not listed in one place. They are string literals across
`services/api-gateway/rest`. A catalogue (and a test that every `writeError`
code is in it) is recommended.

### 3.6 Deprecation

Policy:

1. A deprecation is announced in the release notes of the MINOR that introduces
   the replacement, and in the doc comment of the field, RPC or endpoint
   (`// Deprecated: use X; removal no earlier than vN+1.0.0.`). For proto, also
   mark it `[deprecated = true]`.
2. REST endpoints that are deprecated send `Deprecation` and `Sunset` response
   headers (RFC 9745 / RFC 8594) from the announcing release on.
3. Removal happens only in the next MAJOR, and no sooner than **two MINOR
   releases or 6 months** after the announcement, whichever is later.
4. A database column follows the same rule in migrations: stop reading it in
   release N, drop it in N+1 or later (ADR-0011 rule 5).

Today nothing is deprecated. No code emits `Deprecation`/`Sunset` headers, and
no proto element carries `deprecated = true`.

### 3.7 Enforcement today, and what is recommended

- **No `buf` configuration and no breaking-change check.** The repo has no
  `buf.yaml`, and `make proto` runs `protoc` directly. ADR-0001 says "a breaking
  change to the plane boundary fails the build", but that is true only for
  changes that stop Go code compiling. A renumbered field, a removed field or a
  changed type compiles, ships, and breaks every older peer on the wire.
  **Recommended:** add `buf.yaml` (module root `proto/`) and a CI step
  `buf breaking proto --against '.git#branch=origin/main,subdir=proto'` with the
  `WIRE_JSON` rule set. `WIRE_JSON` is needed because specs are stored as
  protojson, so JSON names matter too.
- **No REST contract diff.** There is no OpenAPI document. The route table in
  `rest/api.go` and ARCHITECTURE.md §8 are the only references.
  **Recommended:** generate an OpenAPI description, or a golden route/response
  fixture set, and diff it in CI.
- **The spec schema** is published at `GET /v1/schemas/workflow-spec-v1.json`
  (`rest/api.go:303`) from the embedded copy. `TestEmbeddedSchemaMatchesCanonical`
  keeps that copy byte-identical to `schemas/`. Nothing checks that a schema
  change is additive. **Recommended:** a test that every spec in `examples/`
  compiled by the previous release still validates. The previous-release smoke
  (§7) is the natural place for it.

## 4. Agent ↔ control-plane skew (N-1)

### 4.1 Supported combinations

| Agent | Control plane | Supported? | Why |
|---|---|---|---|
| N | N | yes | — |
| **N-1** | **N** | **yes (policy)** | The normal state during and after an upgrade: the control plane first, the fleet at its own pace. |
| N | N-1 | **only transiently**, during a single rollout | Helm rolls agents and services in the same release, so for minutes a new agent can talk to an old gateway. New agent features must therefore degrade when the gateway lacks them (the `log_shipping` pattern). Running this combination on purpose is unsupported. |
| N-2 or older | N | **no** | |

### 4.2 Upgrade order

1. **Back up** (OPERATIONS.md §6.3).
2. **Migrations.** The dedicated migrate job, never a service at startup
   (ADR-0011 rule 4). In Helm, services have a `wait-for-schema` init container
   (`deploy/helm/kgf-nexus/templates/_helpers.tpl`, `nexus.waitForSchema`). It
   blocks new pods until the newest migration in the **same-tag** migrate image
   is in `schema_migrations`. Old pods keep serving meanwhile. That window is
   what §6 and §7 are about.
3. **Control plane**, rolling. For a while old and new replicas of each service
   run side by side and call each other over `nexus.v1` gRPC.
4. **Agents.** An agent session re-registers on restart (per-session identity,
   `RegisterResponse`), so an agent upgrade drains or loses only its own
   attempts, and those are recovered by lease expiry (ADR-0010).

Downgrade is not a supported path. Restoring (OPERATIONS.md §3, §6.4) is.

### 4.3 How the product behaves today

- **No version negotiation, but an optional admission gate.**
  `RegisterRequest.agent_version` is logged, stored
  (`internal/store/rm_workers.go`) and displayed. Nothing negotiates on it.
  An operator can refuse agents below a minimum version, or missing a
  capability, at Register (§4.4); by default every agent is admitted.
- **Capability, not version.** New agent-facing behaviour is gated on what the
  control plane advertises. Log shipping is on only when `RegisterResponse.log_shipping.enabled`
  (`agent/internal/worker/agent.go:440`). An agent calling a
  gateway that predates `ReportLogs` gets `UNIMPLEMENTED` and stops shipping
  (`agent/internal/logship/shipper.go:590`). Unknown heartbeat directives are
  ignored (`agent/internal/heartbeat/heartbeat.go:240`).
- **Placement constraints are the one existing capability filter.** A worker
  advertises `executors`, and the claim matches on them
  (`internal/store/scheduler_claim.go:489`).
- **The hazard: new task semantics reaching an old agent.** A `TaskSpec` field
  added in N (for example `cancellation.uninterruptible`, ADR-0015, field 50)
  is silently ignored by an N-1 agent, which then runs the task with the *old*
  semantics. Here that means SIGTERM on cancel for a task that declared itself
  uninterruptible. Proto tolerance makes this compile and run, so it fails
  silently. **Policy:** a new task-level feature that an agent must honour must
  either be safe to ignore, or be gated at dispatch on an agent capability.
  The gating would work like `executors`: the agent advertises a capability,
  the task requires it, and the claim filters on it. `RegisterRequest`
  carries `repeated string capabilities = 8` since this change (§4.4), and
  the gateway can require capabilities fleet-wide at Register. **Gap:**
  per-task capability requirements, filtered at dispatch, are not built.
- **Untested:** no test starts an N-1 agent binary against an N gateway. The
  closest evidence is unit tests of the tolerant paths above.

### 4.4 Minimum agent version and required capabilities

The gateway applies an admission gate in `Register`
(`services/api-gateway/workergrpc/server.go`, `domain.AgentPolicy` in
`internal/domain/agentcompat.go`) before any worker row is written:

| Setting (gateway env / Helm value) | Effect |
|---|---|
| `NEXUS_MIN_AGENT_VERSION` / `agentPolicy.minVersion` | Refuse an agent whose `agent_version` is older, by semantic-version precedence (`1.4.0-rc.1` < `1.4.0`; build metadata ignored). With a minimum set, an agent whose version is not a semantic version -- a `dev` build -- is refused, since nothing shows it is new enough. |
| `NEXUS_REQUIRED_AGENT_CAPABILITIES` / `agentPolicy.requiredCapabilities` | Comma-separated; refuse an agent that does not advertise every one. An agent older than this change advertises none, so requiring any capability also refuses it. |

Both are empty by default, which admits every agent: nothing changes for a
deployment that does not opt in. The gateway refuses to start with an
unparsable minimum or a malformed capability name, rather than refusing
every agent; the Helm schema rejects them at install.

A refusal is `FAILED_PRECONDITION` naming each unmet requirement, logged at
WARN on the gateway with the agent's host, version and capabilities. The
agent's register backoff treats `FAILED_PRECONDITION` as permanent
(`agent/internal/worker/register.go`, `RegistrationRetryable`): it does not
retry, counts `permanent_error`, and exits with "upgrade the agent" and the
reason. Retrying could not make an old binary new.

Capabilities this release's agent advertises (`domain.AgentCapabilities`):

| Capability | Meaning |
|---|---|
| `log_shipping` | Ships task logs through `ReportLogs` (ADR-0020). |
| `progress` | Reports executor phase and progress through `ReportProgress` (F96). |
| `uninterruptible_cancel` | Honours `cancellation.uninterruptible` and the operator's ceiling (ADR-0015); an agent without it SIGTERMs such a task. |
| `output_max_bytes` | Enforces each output's declared `max_bytes` before storing it (CONTRACT-FINDINGS F125). |

Capability names are stable wire strings: new ones are added, none is
renamed or reused. Upgrade order is unchanged (§4.2): raise the minimum, or
add a required capability, only after the fleet runs a version that meets
it, or the agents still on the old version stop at their next restart.

## 5. Workflow spec `api_version`

- **Schema:** `api_version` is required and is `const: "nexus.dev/v1"`
  (`schemas/workflow-spec-v1.json:15-18`). Its description says it is "pinned
  so a future spec grammar can be introduced without reinterpreting existing
  stored documents".
- **Validator:** the gateway compiles the schema **embedded in the binary**
  (`services/api-gateway/spec/schema.go`, `//go:embed workflow-spec-v1.json`)
  and runs parse → structure → normalize → semantics → canonicalize → hash
  (`spec/pipeline.go`). Any other `api_version` is a structural validation
  error. `TestEmbeddedSchemaMatchesCanonical` fails the build if the embedded
  copy drifts from `schemas/`.
- **Clients:** the SDK emits `API_VERSION = "nexus.dev/v1"`
  (`sdk/python/nexus/workflow.py:47`). `nexusctl`'s local pre-check verifies
  only that the key is present (`cli/internal/spec/spec.go:353`), so the server
  is authoritative for the value.
- **Stored specs are immutable and content-addressed** (ADR-0004). A run pins
  its spec version for its lifetime. Readers in the control plane decode
  stored specs with `DiscardUnknown`, so a spec stored by N+1 with a new
  optional field can still be materialized by an N engine. This is intended
  for the rolling window, not as a supported steady state.

**Policy.**

- Within `nexus.dev/v1`, the grammar may only grow: new **optional**
  properties, new enum values, relaxed bounds.
- Every document ever accepted under v1 must stay valid and mean the same
  thing, because it is stored forever and its hash is its identity.
- A change that would make a stored v1 document invalid, or change what it
  means, needs `nexus.dev/v2`. The gateway would then accept both, dispatch on
  `api_version`, and keep the v1 schema and semantics indefinitely for stored
  specs.
- Because the schema is `additionalProperties: false`, a spec using a
  field added in release N is rejected by N-1. Authors must not use new spec
  fields until the gateway is upgraded.

**Gap:** nothing tests that a schema change is backward compatible (§3.7).

## 6. Database migrations: expand / contract

### 6.1 The rule

Every release's migrations must leave a schema that **the previous release's
services can still run against**. In a rolling deploy, step 2 of §4.2 finishes
while every replica is still the old code, and some old replicas keep serving
until step 3 completes. The pattern is expand/contract across releases:

| Release | Schema change | Code |
|---|---|---|
| N (expand) | Add the new table, column (nullable, or `DEFAULT`), index (`CONCURRENTLY`), or trigger that fills the new shape for old writers | Writes both shapes, reads the new one with a fallback |
| N+1 | Backfill in bounded batches; enforce (`SET NOT NULL`, `VALIDATE CONSTRAINT`) | Reads only the new shape |
| N+2 (contract) | `DROP` the old column or table, once no deployed version reads it | — |

The in-repo precedent is 0014 → 0015. 0014 adds `tenant_id` nullable with
triggers that fill it for writers that predate it. 0015 enforces `NOT NULL`,
"to be applied a release later". **Gap:** `migrate.sh` cannot express "a release
later". Both files are in the tree and a fresh upgrade applies them together.
Only the header comment and the operator stand between them.

### 6.2 What `internal/migrations` proves, and what it does not

`internal/migrations` (run by `go test ./...` in the CI `build` job) scans every
`*.up.sql` and fails on shapes that break old replicas: `DROP COLUMN`,
`DROP TABLE`, `RENAME`, `ALTER … TYPE`, `SET NOT NULL`,
`ADD COLUMN … NOT NULL` with no default, top-level `UPDATE` with no `WHERE`, and
`CREATE INDEX` without `CONCURRENTLY`. It also checks versions are unique, gaps
are only the known one (0019), and names are well formed (OPERATIONS.md §6.2).

It is **syntactic**. It does not know what the old code runs, so it cannot
catch any of the following:

- `DROP CONSTRAINT` / `DROP INDEX` removing a unique index that an old
  `INSERT … ON CONFLICT (cols)` depends on. §7.4 shows this passing the gate
  and failing the old code.
- A new `CHECK`, `FOREIGN KEY` or trigger that rejects rows an old writer
  still produces.
- A changed function or trigger body, a changed default, or a changed view.
- Semantic changes: a column whose meaning changed without its shape changing.

The previous-release smoke (§7) closes part of that gap by running the old
code's queries.

## 7. The previous-release skew test

`tests/compat/previous-release-smoke.sh` (`make compat-smoke`; CI job `compat`
in `.github/workflows/ci.yml`).

### 7.1 What it does

a. **Resolves the previous release** (§7.2) and extracts it with `git archive`
   into a scratch directory. It does no checkout or worktree, and does not
   change the git state of the current checkout.

b. **Checks migration history.** Every previous `*.up.sql` must exist in this
   tree byte-for-byte. The ledger keys on file name and never re-runs an
   applied file, so an edit silently makes upgraded and fresh databases differ.
   Every new migration must also sort after the previous highest. Otherwise an
   upgraded database applies it last and a fresh one applies it earlier
   (services/migrations/README.md). Violations exit 1.

c. **Starts a disposable Postgres** (`postgres:16-alpine`, what compose ships),
   container `nexus-w2c-compat-pg-<port>`, bound to `127.0.0.1:55688`
   (`NEXUS_COMPAT_PG_PORT=55689` is the only alternative).

d. **Takes the upgrade path, not a fresh install.** It applies the previous
   release's migrations with the previous release's `deploy/migrate.sh`, which
   gives the database production has. It then applies this tree's migrations
   with this tree's `deploy/migrate.sh` (`MIGRATIONS_DIR=services/migrations`)
   and asserts that `schema_migrations` equals this tree's migration set
   exactly. A failing upgrade exits 1.

e. **Runs the previous release's own integration suite** (`go test -tags
   integration -count=1 -p 1 -json`, every package of the *old* tree that has an
   integration-tagged test file) against that database via
   `NEXUS_TEST_DATABASE_URL`, the only variable those tests read
   (`internal/testdb`).
   These tests **do not create, migrate or drop schema**. They were read to
   confirm this: none contains DDL, and they rely on a pre-migrated database
   exactly as CI's `integration` job does. So they genuinely execute the old
   store layer and in-process services against the new schema. A
   database-missing skip (`testdb.SkipMarker`) fails the run (F85), and so does
   zero passing tests.

f. **Triages failures automatically.** The failing tests are re-run against a
   second database in the same container that has **only** the previous
   release's migrations:
   - a test that passes there and fails on the new schema is a **skew
     incompatibility** (exit 1);
   - a test that fails on both is a pre-existing failure of the old suite, which
     makes the run inconclusive (exit 3), not a compatibility verdict.

g. **Always cleans up** (trap on EXIT/INT/TERM): removes exactly the one
   container it named and the scratch tree. `NEXUS_COMPAT_KEEP=1` keeps both
   for debugging. The go test log is copied to `NEXUS_COMPAT_LOG` (default
   `$TMPDIR/nexus-compat-<sha>.log`) so it survives cleanup. CI uploads it as
   an artifact.

It is idempotent: a stale container from a crashed run is removed first, and
every run builds its database from scratch. When `psql` is not on the host, the
same `migrate.sh` runs inside the Postgres container.

### 7.2 What "previous release" resolves to

In order:

1. `NEXUS_COMPAT_PREVIOUS_REF`, if set.
2. The newest tag matching strict `vMAJOR.MINOR.PATCH`, ignoring a tag on
   HEAD itself. Once releases are tagged, the tag is what production runs.
3. **Without such a tag**, which is the case today, the previous merge on
   `main` is the best available proxy for what production runs, because every
   merge to `main` is what gets deployed next:
   - HEAD **is** the `origin/main` tip (a push to main): `origin/main~1`, its
     first parent, which is the previous merge;
   - HEAD is anything else (a PR or a branch): the `origin/main` tip, which is
     what production runs before this change merges.

   `NEXUS_COMPAT_MAIN_REF` overrides `origin/main`. CI checks out with
   `fetch-depth: 0` so both are resolvable.

The script prints the resolved revision and the reason first.

### 7.3 What it proves, and what it does not

**Proves**, for the resolved previous release:

- The upgrade path previous → current applies cleanly and yields exactly this
  tree's ledger.
- Migration history was not rewritten.
- The previous release's store queries, and the service logic its integration
  tests drive in-process, work against the upgraded schema. That covers the
  scheduler claim, the DAG engine, the workflow-manager schedules, the gateway
  REST handlers over a real database, sessions, the outbox, RBAC and tenancy.

**Does not prove:**

- **Built binaries or cross-version gRPC.** The old code runs as Go tests, not
  as the old images. An N-1 scheduler calling an N resource-manager is not
  exercised, and neither is an N-1 agent against an N gateway (§4.3).
- **Old code reading rows written by new code.** The database is empty apart
  from what the old tests write. A new writer that fills a column with a value
  an old reader mis-parses is not caught.
- **Anything the old suite does not cover.** Tests needing NATS, S3/MinIO,
  Mailpit or Kubernetes skip themselves here. The summary lists them by name so
  that nobody mistakes them for passes.
- **Performance and locking** of migrations on a populated database (a
  `CREATE INDEX` on a big table, a backfill). The database is empty.
- **Forward skew** (new code on the old schema). This is unsupported by design,
  because migrations run first.
- **When there are no new migrations** since the previous release, the run
  exercises the harness and history checks only, and prints a WARNING saying so.
  `NEXUS_COMPAT_REQUIRE_SKEW=1` turns that into a failure.

### 7.4 Results (2026-09-25, laptop, Postgres 16, Go 1.27.1)

| Run | Previous | New migrations | Result |
|---|---|---|---|
| Default (`make compat-smoke`) | `origin/main~1` = `c2f90ba` (Merge #12, wave-1). HEAD `2b4936f` is the `origin/main` tip. | **none**: #13 changed Helm and NATS only | **PASS**: 533 top-level tests, 0 fail, 27 skipped for NATS/S3/Mailpit/K8s. The WARNING about no skew was printed. |
| Real skew: `NEXUS_COMPAT_PREVIOUS_REF=626f90b` (Merge #11, before wave-1) | `626f90b` | `0024_attempt_progress`, `0025_attempt_logs`, `0026_node_retry_floor` | **PASS**: 451 tests, 0 fail, 17 skipped. The pre-wave-1 release runs against the wave-1 schema. |
| **Negative control** (`NEXUS_COMPAT_MIGRATIONS_DIR=<copy of services/migrations + a drill 0027>`) | `626f90b` | the three above, plus a drill 0027 that builds `UNIQUE (namespace, key, request_hash)` `CONCURRENTLY` and drops `idempotency_keys_pkey` | **FAIL, exit 1, as intended.** `TestIntegrationIdempotencyKeyFlow` and `TestIntegrationIdempotencyClaimIsAtomic` failed with `there is no unique or exclusion constraint matching the ON CONFLICT specification (SQLSTATE 42P10)`, and both passed on the baseline, so they were classified as SKEW INCOMPATIBILITY. The same 0027 **passes** `go test ./internal/migrations/` (checked in a scratch copy), so this is a break that only this test catches. The drill ran a second time with `psql` removed from `PATH` to exercise the in-container fallback, with the same verdict. |

The suite phase took 48–71 s per run. Most of the rest of the wall time is
compiling the old tree.

### 7.5 Running it

```bash
make compat-smoke                                        # what CI runs
NEXUS_COMPAT_PREVIOUS_REF=v0.3.0 make compat-smoke       # against a specific release
NEXUS_COMPAT_REQUIRE_SKEW=1 make compat-smoke            # fail when there is nothing to test
NEXUS_COMPAT_KEEP=1 make compat-smoke                    # keep DB + tree to debug
```

Exit codes: `0` pass. `1` skew incompatibility or rewritten history. `2`
environment or usage error. `3` inconclusive: the old suite fails on its own
schema or does not build.

## 8. SDK and CLI support matrix

| Client | Supported runtime | Enforced by |
|---|---|---|
| Python SDK `nexus-sdk` 0.1.0 | CPython **3.11, 3.12, 3.13** (`requires-python = ">=3.11"`, classifiers). No runtime dependencies, and `PyYAML>=6.0` only for the `yaml` extra. | CI `sdk` job matrix `["3.11","3.12","3.13"]`. 3.14+ is not claimed or tested. |
| Go (control plane, agent, `nexusctl`) | **Go 1.25** (`go.mod: go 1.25.0`, CI `GO_VERSION: "1.25"`, `Dockerfile.service` `golang:1.25-alpine`). Newer toolchains build it (1.27.1 was used for §7.4). | CI `build` job. |
| Postgres | **16** (compose, CI, Helm dependency image). | CI `integration`, `compat` jobs. |

**Client ↔ control-plane skew (policy):**

| Client version vs control plane | Supported? |
|---|---|
| Same release | yes |
| Client **older** by up to one MINOR (N-1 CLI/SDK against N) | yes. The server only adds. Old clients ignore new response fields (§3.3). |
| Client **newer** than the control plane | **no**. The spec schema and four admin endpoints reject unknown fields (§3.3), and a newer client may call endpoints that return 404. Upgrade the control plane first. |
| Across a MAJOR | no |

**Gaps:**

- Neither the CLI nor the SDK sends its real version. The `User-Agent`s are
  hard-coded `nexusctl/0.1` and `nexus-python-sdk/0.1`.
- The gateway exposes no version endpoint. `nexus_build_info` on the metrics
  port is the only way to read a service's version.
- `nexusctl version` prints `0.1.0-dev` because nothing stamps it.

So a client cannot detect that it is newer than the server, and an operator
cannot see which client versions are in use. **Recommended:** stamp
`cli/internal/cmd.Version` and use it in the `User-Agent`. Add `GET /v1/version`
(build version plus the latest applied migration). Have clients warn when they
are newer than the server.

## 9. Open items

1. `buf breaking` against `origin/main` in CI (§3.7). This is the single
   highest-value missing check.
2. A test that runs an N-1 **agent binary** against an N gateway. It would
   extend §7 from the store layer to the wire.
3. Capability-gated **dispatch** for task semantics an old agent would
   silently ignore (§4.3). `RegisterRequest.capabilities` and the fleet-wide
   admission gate exist (§4.4); per-task requirements do not.
4. A release process: `vX.Y.Z` tags, versioned images, a changelog with a
   deprecations section. §7 then tests against real releases instead of
   `main~1`.
5. An error-code catalogue (§3.5), `GET /v1/version` and stamped client
   versions (§8).
6. §7 extensions: seed data written by the **new** code before running the old
   suite, and bring up NATS/MinIO so the relay and log tests run too.
7. ~~`docs/OPERATIONS.md` §6.4 is stale~~ -- updated in the same change to
   point here; it now says only the built-service (wire) part is untested.
8. ~~**The CLI and the Python SDK compute a different spec content address from
   the server.**~~ Fixed as CONTRACT-FINDINGS F124 (shared `internal/specenum`
   tables, `nexus.canonical.normalize_enums`, and a golden file checked from Go
   and Python). What it was: The gateway rewrites enum values to their short spelling
   (`services/api-gateway/spec/normalize.go`, `NormalizeEnums`, called from
   `pipeline.go` before `Canonicalize`); `cli/internal/spec` and
   `sdk/python/nexus/canonical.py` hash the proto spelling
   (`IDEMPOTENCY_MODE_REQUIRED` vs `required`, `BACKOFF_STRATEGY_EXPONENTIAL`
   vs `exponential`, `TRIGGER_RULE_ALL_SUCCESS` vs `all_success`, and the
   retry-class lists). Every file under `examples/` shows nexusctl's "different
   content address" warning. The server is self-consistent, so versions do not
   split and re-submit is idempotent, but a client-side hash (`workflow compile
   --hash-only`, `CompiledWorkflow.hash`) never matches `spec_hash`. Fix: port
   the alias tables to both clients (or a shared package) and add a golden test
   that every example hashes the same through the CLI, the SDK and
   `spec.Compile`.
