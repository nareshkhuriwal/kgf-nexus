# Schema migrations

Plain SQL, applied in lexical filename order by `deploy/migrate.sh` (the compose
`migrate` service and the Helm migrate Job both run it). `make migrate-up` is a
local equivalent. Services never migrate at startup (ADR-0011 rule 4).

## Conventions

- **Name:** `NNNN_description.up.sql` with a matching `NNNN_description.down.sql`.
  `NNNN` is a zero-padded four-digit version, unique across the directory.
- **Ledger:** `schema_migrations.version` stores the full basename
  (`0020_dynamic_expansion`), not the number. A file is applied when its
  basename is not in the ledger. Editing an applied migration therefore does
  nothing on databases that already ran it. Never edit one; add a new one.
- **Transactions:** each file runs in a single transaction unless it declares
  `-- migrate: no-transaction` on a line of its own (needed for
  `CREATE INDEX CONCURRENTLY`). A file that opts out must be safe to re-run.
- **Additive only** (ADR-0011 rule 5). `internal/migrations` checks every
  `*.up.sql` for rolling-deploy hazards and fails `go test` on new ones.

## Wave 3 (integrated on `integrate/wave-3`)

| Version | Migration | Design |
|---|---|---|
| 0028 | `0028_trace_context` | ADR-0026: W3C trace context on the outbox and the attempt row |
| 0029, 0030 | — | **unused** (known gaps, below) |
| 0031 | — | **unused**: the held-out cost-management branch's number (known gap, below) |
| 0032 | `0032_kgf_context_and_service_accounts` | ADR-0024: `kgf_scopes`, `audit_log.request_id`, service accounts and token ceilings |
| 0033 | `0033_audit_chain_export_retention` | ADR-0025: the audit hash chain, KGF attribution columns, checkpoints, retention, legal hold, sinks |
| 0034 | `0034_events_triggers_notifications` | ADR-0028: notifications, webhook triggers, sensors, asset lineage |
| 0035 | `0035_gpu_spot_multicluster` | ADR-0029/0030/0031: the preemption budget, cached artifacts, cluster labels and headroom; the scheduler's `UPDATE (attempts_used, preemptions_used)` on `task_nodes` (F147) |
| 0036 | `0036_workload_api` | ADR-0032: attempt usage, rate-limit buckets, checkpoints, `free_retries`; the scheduler's `UPDATE (free_retries)` |
| 0037 | `0037_strict_service_grants` | F132: the grants per-service logins need (scheduler timer lease and worker reaping, gateway lease renewal, `SELECT (id, namespace)` on `tenants`); grants only |
| 0038 | `0038_installation_role_prefix` | F153: opt-in per-installation service group roles (`NEXUS_DB_ROLE_PREFIX`): `installation_role_prefix`, `nexus_role()`, and the move of every grant off the shared `nexus_*` roles that `deploy/migrate.sh` runs after each migrate. No prefix: nothing changes |

## After wave 3

| Version | Migration | Design |
|---|---|---|
| 0039 | `0039_run_reconcile_queue` | F160: the durable run-reconcile queue (`run_reconcile_requests`), its triggers on `task_attempts` (terminal) and `workflow_runs` (state changes that need the engine), and a one-time request for every live materialized run, which repairs runs stranded before the upgrade. Owned by the dag-engine; see the ADR-0011 addendum |
| 0040 | `0040_kgf_workspaces` | ADR-0034: `kgf_workspaces`, one row per KGF.Nexus workspace namespace (`UNIQUE (organization, resource_id)`, state active/deleted); SELECT for every service, INSERT and `UPDATE (state, deleted_at, deleted_by)` for the api-gateway, through `nexus_role`. Owned by the api-gateway |
| 0041 | `0041_service_instances` | ADR-0037: `service_instances`, the platform-scoped registry of running control-plane processes (no `tenant_id`), keyed by (component, cluster, pod namespace, instance); every service writes only its own row every 10 s and prunes rows unseen for 24 h, so SELECT, INSERT, UPDATE and DELETE for every service, through `nexus_role` |
| 0042 | `0042_attempt_runtime` | ADR-0037: `attempt_runtime`, one row per attempt with the executor, cluster, Kubernetes namespace, pod and node the agent reported (ReportPlacement), fenced on the lease and its epoch; references the run (cascade), tenant assigned from the attempt. SELECT for every service, INSERT/UPDATE/DELETE for the api-gateway, through `nexus_role`. Owned by the api-gateway |

**Granting after 0038.** An installation may have renamed its group roles, so
a migration numbered above 0038 must not name `nexus_<service>` literally:
resolve it with `nexus_role('<service>')` inside `EXECUTE format('GRANT ...
TO %I', ...)`, so a REVOKE reaches a prefixed installation's role too.
`TestNoLiteralGroupRolesAfterThePrefixMigration` enforces it. In a prefixed
installation, roll back 0038 (which moves every grant back to `nexus_*`)
before rolling back any earlier migration: their down files revoke from the
shared names.

**One column, one owner.** 0032 and 0033 both touched `audit_log`; the
integration left one set of columns: `request_id` (and its index) is 0032's,
the chain columns and the `kgf_*` attribution columns are 0033's, and each
file's rollback drops only its own. 0035 and 0036 both granted the scheduler
`UPDATE (attempts_used)` on `task_nodes`; it is granted once, by 0035.

**Rollback order.** Roll back in reverse version order (`0038`, `0037`, `0036`,
`0035`, `0034`, `0033`, `0032`, `0028`), each after the services that read it
are back on a release that does not. `make migrate-down` does exactly that.

## Known gaps: 0019, 0029, 0030, 0031

These version numbers below the current highest are unused, and must stay
unused. `knownVersionGaps` in `internal/migrations/safety_test.go` is the one
list of them; there is no separate list of numbers "reserved" for branches in
flight.

- **0019.** The sequence goes `0018_cluster_registry` → `0020_dynamic_expansion`.
  No migration numbered 0019 has ever existed on any branch in this
  repository's git history (checked with `git log --all -- 'services/migrations/0019*'`).
  The number was skipped, most likely reserved by parallel work that landed
  under other numbers. No code, migration or document refers to a 0019, so no
  schema change is missing.
- **0029 and 0030.** Handed to wave-3 branches in advance; those branches
  landed their schema under other numbers, so nothing ever used them.
- **0031.** The cost-management branch's number (price books, budgets). That
  branch is held out: cost management moves to KGF billing
  (docs/OPERATIONS.md §23), so 0031 will not be created in this repository.

**Do not create a migration in a gap, not even an empty one.** The appliers
pick up every file whose basename is not in the ledger, in lexical order. On a
database already past the gap, a new file there would run *after* every later
migration; on a fresh database it would run *before* them. The same file would
then produce different schemas depending on when a database was created. The
next migration takes the next number after the highest existing one.

A gap does not affect applying migrations. `deploy/migrate.sh`, `make migrate-up`
and `make migrate-down` glob `*.up.sql` / `*.down.sql` and key the ledger on
filenames; none of them counts files or expects consecutive numbers.

`internal/migrations/safety_test.go` holds this in place:

- `TestVersionNumbersAreUnique` fails when two files share a version.
- `TestVersionGapsAreOnlyTheKnownOnes` fails when a known gap is filled, when a
  new gap opens below the highest version (a migration dropped by a merge), or
  when a known gap lies above the highest version.
- `TestMigrationFilenamesAreWellFormed` fails on names that break the
  `NNNN_description.{up,down}.sql` pattern, and on a `.down.sql` with no `.up.sql`.
