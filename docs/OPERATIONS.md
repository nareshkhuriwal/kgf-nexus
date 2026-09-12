# Nexus — Operations: backup, restore, upgrade

**Status:** Phase 1. Covers the procedures that exist and have been executed.
**Audience:** whoever is on call.
**Scope:** Postgres backup and restore, the restore drill and its measured
result, what these procedures do *not* cover, the RPO/RTO the approach actually
delivers, and the schema-migration procedure.

This document states what has been verified and marks what has not. An
operations runbook that overclaims is worse than none, because it is trusted
during an incident and discovered to be wrong at the worst possible moment.

---

## 1. What is at stake

Postgres is the **sole system of record** ([ADR-0002](adr/0002-postgres-as-metadata-store.md)).
Every workflow spec, run, task node, attempt, lease, timer, concurrency slot,
idempotency key and API key lives there and nowhere else. NATS carries
notifications, not truth ([ADR-0003](adr/0003-nats-as-event-bus.md)); MinIO holds
task payloads but no orchestration state ([ADR-0008](adr/0008-result-store-separation.md)).

So: **losing the Postgres volume loses the platform's entire history and every
in-flight run.** Recovering from a lost Postgres by replaying NATS or reading
MinIO is not possible — neither one knows what the DAG was.

Before the scripts described here existed there was no backup of any kind: no
`pg_dump`, no PITR, no documented procedure, no stated RPO or RTO.

---

## 2. Backup

### 2.1 Running it

```bash
# Against the compose stack (no DSN in the environment):
BACKUP_DIR=/var/backups/nexus deploy/backup.sh

# Against any reachable database:
DATABASE_URL='postgres://nexus:...@db.internal:5432/nexus?sslmode=require' \
  BACKUP_DIR=/var/backups/nexus deploy/backup.sh
```

| Env var | Default | Meaning |
|---|---|---|
| `NEXUS_DATABASE_URL` / `DATABASE_URL` / `NEXUS_POSTGRES_DSN` / `NEXUS_DSN` | unset | The DSN. Same four spellings `internal/config/dsn.go` accepts, in the same precedence order, so the backup job reads the config the services read. |
| `BACKUP_DIR` (or `$1`) | `./backups` | Output directory. |
| `BACKUP_KEEP_DAYS` | `7` | Prune `nexus-*.dump` older than this. `0` disables pruning. |
| `PG_CONTAINER` | `nexus-postgres-1` | Container used when no DSN is set. |
| `PG_USER` / `PG_DB` | `nexus` / `nexus` | Role and database for container mode. |

### 2.2 The choices, and why

**Custom format (`-Fc`), not plain SQL.** The difference only matters during an
incident, which is exactly when it matters. `pg_restore` can pull a **single
table** out of a custom archive (`-t workflow_runs`) and can restore with `-j` on
several cores. A plain `.sql` file is one serial `psql` stream, all or nothing.
The realistic incident is "one table was corrupted by a bad migration", not "the
whole cluster is gone", and plain format answers that badly.

**Two connection modes.** With a DSN set, `pg_dump` runs on the host against that
URL. With no DSN and the compose container present, `pg_dump` runs **inside the
container**. The reason for preferring in-container is version skew: a host
`pg_dump` from a newer major produces an archive the server's own `pg_restore`
cannot fully replay, and that is discovered at restore time. This is not
hypothetical — see §3.4, where it was reproduced on this machine.

**`.partial` then rename.** A dump killed halfway leaves a file nobody will
mistake for a good backup. The rename is the commit point.

**Verification before the rename.** The script rejects an empty file, and runs
`pg_restore --list` over the archive: reading the table of contents fails on a
truncated or non-archive file, which is what a full disk or a killed `pg_dump`
actually produces. A backup that exits 0 having written garbage is worse than a
backup that fails, because it will be trusted.

**UTC, sortable timestamps.** Local time in a filename is ambiguous twice a year
and unsortable across a DST boundary — precisely when someone is picking "the
last good backup" out of an `ls`.

### 2.3 Recommended schedule

Nothing here schedules anything; the script is the mechanism, cron/systemd/a
Kubernetes CronJob is the policy. The recommendation:

| | Frequency | Retention | Rationale |
|---|---|---|---|
| Base dump | **hourly** while runs are active, **daily** minimum | 7 days on local disk (`BACKUP_KEEP_DAYS=7`) | The dump is small relative to the data — the dev database dumped to **110 KB** for 632 events — so hourly is cheap until the history grows. |
| Off-host copy | every dump | 30 days | A backup on the same disk as the database does not survive the failure that destroys the disk. **This script does not copy anything off-host.** |
| Restore drill | monthly, and after any schema change | — | An untested backup is a hypothesis. §3 is the drill; run it. |

Sizing caution: 110 KB is the size of a nearly-empty dev database. `run_events` is
append-only and will dominate growth. Re-measure before assuming hourly dumps
stay cheap, and re-measure the restore *time* (§4) as the archive grows — that is
the number the RTO depends on, and it does not scale with the dump's byte size in
any way worth predicting.

---

## 3. Restore

### 3.1 Running it

```bash
deploy/restore.sh <dump-file> [target-db] [--yes]
```

The script **refuses to restore over a database that already has tables** unless
`--yes` (or `FORCE=1`) is passed. This is the one guard that matters:
`pg_restore --clean` drops the real tables before it writes the archive's, so a
mistyped target destroys live data with no undo. The refusal prints what the
target currently holds and how to restore into a scratch database instead.

Every restore prints a plan first (archive, size, mode, target, current table
count, whether the action is destructive) and row counts afterwards. The counts
are not decoration: "`pg_restore` finished" and "the data is there" are different
claims, and counting is the only way to tell them apart.

### 3.2 The drill that was actually run

Executed 2026-09-12 against the live compose stack on this machine (Postgres
16.15, `postgres:16-alpine`). The stack was **running and writing** throughout,
which is why §3.3 discusses drift.

**Step 1 — back up the live dev database.**

```
$ BACKUP_DIR=.../backups deploy/backup.sh
backup: no DSN set, dumping inside nexus-postgres-1
backup: waiting for postgres to accept connections
backup: dumping nexus to .../nexus-20260912T102914Z.dump (custom format)
backup: wrote .../nexus-20260912T102914Z.dump (110868 bytes, 102 archive entries)
backup: done (pruned 0 older than 7 days)
```

**Step 2 — confirm the guard refuses the live database.**

```
$ deploy/restore.sh .../nexus-20260912T102914Z.dump nexus
  target db    : nexus
  target state : EXISTS, 14 table(s) in schema public
  action       : DROP and recreate every object in the archive (DESTRUCTIVE)

restore: REFUSING to restore over nexus -- it already has 14 table(s).
$ echo $?
1
```

**Step 3 — restore into a new database.** `nexus` itself was never a target.

```
$ deploy/restore.sh .../nexus-20260912T102914Z.dump nexus_restore_drill
  target db    : nexus_restore_drill
  target state : does not exist, will be created
  action       : load the archive into an empty database
restore: creating database nexus_restore_drill
restore: restoring into nexus_restore_drill
```

`pg_restore` reported no errors in container mode.

**Step 4 — compare row counts.** Source counts were taken immediately after the
dump completed.

| Table | `nexus` (source) | `nexus_restore_drill` |
|---|---|---|
| `workflow_specs` | 20 | **20** |
| `workflow_runs` | 64 | **64** |
| `task_nodes` | 277 | **277** |
| `task_attempts` | 188 | **188** |
| `run_events` | 632 | **632** |
| `workers` | 17 | **17** |
| `api_keys` | 7 | **7** |

All seven match exactly. Schema objects were compared as well and are identical:
14 tables, 38 indexes, 1 sequence, 128 table constraints, 36 functions in schema
`public` in both databases. The `schema_migrations` ledger restored with all five
applied versions and their original `applied_at` timestamps, so a restored
database is not re-migrated by the next `migrate` job. `max(seq)` in `run_events`
was 30 in both.

**Step 5 — drop the drill database.**

```
$ docker exec nexus-postgres-1 psql -U nexus -d postgres -c 'DROP DATABASE nexus_restore_drill'
DROP DATABASE
```

Verified afterwards: no `nexus_restore%` database remains, and `nexus` still
served queries (`workflow_runs` = 65, having advanced by one run while the drill
ran — the live data was untouched).

### 3.3 Honest note on what the drill proves

The row counts matching exactly is evidence the dump is **internally
consistent**: `pg_dump` runs in a single repeatable-read snapshot, so the archive
is a point-in-time image even though the stack kept writing. It is *not* evidence
that the restored database matches the source *now* — by the time the comparison
ran, the live `run_events` sequence had advanced from 828 to 835 and
`workflow_runs` from 64 to 65. That drift is the RPO (§4), not a restore defect.

What the drill does **not** prove, and was not attempted:

- **No service was pointed at the restored database.** The drill verifies the
  data is present and the schema is intact; it does not verify that the six
  control-plane services start clean against a restored database and reconcile
  in-flight runs sensibly. That is a separate, larger exercise and it has not
  been done.
- **In-flight state was not reasoned about.** A restored database contains
  `leases`, `timers` and `reservations` that refer to workers which no longer
  exist. The liveness sweep and reconcilers are designed to clear these, but that
  behaviour was not exercised here.
- **Restore duration is measured but not meaningful.** A repeat of the drill,
  timed end to end (connection wait, plan, CREATE DATABASE, pg_restore, row-count
  query), took **0.51s** for the 110 KB archive. That number says nothing useful
  about a production archive and must not be used as an RTO input.

### 3.4 Version skew, reproduced

The host-mode path was tested too, with `pg_dump`/`pg_restore` **18.3** against
the **16.15** server. The backup succeeded and all data restored, but
`pg_restore` 18 emits a statement Postgres 16 does not know:

```
pg_restore: error: could not execute query: ERROR:  unrecognized configuration parameter "transaction_timeout"
Command was: SET transaction_timeout = 0;
pg_restore: warning: errors ignored on restore: 1
```

Harmless here, and the script says so rather than aborting (the row counts are
what decide success). But it is the concrete reason container mode is preferred
when no DSN is given, and the reason **the client major version should match the
server major version in production**. Do not run a restore for the first time
during an incident with mismatched clients.

`--exit-on-error` is deliberately *not* passed, for this reason: aborting on a
non-fatal object error would leave a half-loaded database when the data itself
restored perfectly. The row-count comparison is the acceptance test, not
`pg_restore`'s exit code — which is also why the script prints the counts for you.

---

## 4. RPO and RTO — what this approach actually gives you

Stated plainly, because the honest numbers are more useful than aspirational
ones.

**RPO (how much work you lose) = the backup interval.** With hourly dumps, up to
one hour of run history: completed runs, attempt results, emitted events. There
is **no point-in-time recovery**. `pg_dump` is a logical snapshot; it cannot
replay to a chosen second. Recovering to "just before the bad deploy at 14:32"
is not possible with this tooling — you get the 14:00 dump.

If a sub-hour RPO is required, `pg_dump` is the wrong mechanism and this
procedure must be replaced (not supplemented) by continuous archiving:
`archive_mode = on` with WAL shipping, or a managed service that does it
(RDS/Cloud SQL automated backups plus PITR). That is **not implemented**, and
nothing in this repository configures WAL archiving.

**RTO (how long you are down) = restore time + reconciliation time**, and only
the first term is under this procedure's control. The restore itself is
`pg_restore -j` into a fresh database, which for a logical dump scales roughly
with row count and index build cost — index rebuilds usually dominate. The
measured dev restore (0.51s end to end, 110 KB archive, 632 events) is not a
basis for any production estimate. **Measure it on your data and write the number
here.**

The second term is unmeasured: how long the fleet takes to settle after being
pointed at a restored database (dead workers swept, stale leases expired, timers
re-armed) has not been tested (§3.3).

**What an RPO of "one hour" means for correctness, not just data.** Nexus is
at-least-once with mandated idempotency ([ADR-0006](adr/0006-at-least-once-with-idempotency.md)).
Restoring to an earlier snapshot re-creates attempts that already ran, so tasks
**will be re-executed** after a restore. For idempotent handlers that is the
designed behaviour. For a handler that is not genuinely idempotent, a restore
duplicates its side effects — this is the strongest practical argument for taking
the idempotency requirement seriously, and it is worth saying out loud to task
authors.

---

## 5. What is NOT covered

Listed explicitly, because the gap between "we have backups" and "we can recover"
is made of items like these.

- **MinIO / the result store is not backed up by these scripts.** Task inputs and
  outputs live in S3-compatible object storage, content-addressed and immutable
  ([ADR-0008](adr/0008-result-store-separation.md)); the compose stack runs MinIO
  with bucket `nexus-results`. Postgres holds only *references* (plus results
  inlined below 64 KiB — `RESULT_STORE_INLINE_THRESHOLD_BYTES`, so small outputs
  *are* in the Postgres dump). Restoring Postgres alone therefore gives you a
  history whose large `output_ref`s point at objects that may no longer exist, and
  a downstream task retried after such a restore will fail to read its input.
  Object-store backup is a separate procedure — bucket replication, versioning, or
  `mc mirror` — and **it does not exist yet**. Because objects are immutable and
  content-addressed, mirroring is straightforward; it simply has not been written.
- **No off-host copy.** `backup.sh` writes to a local directory. Shipping it
  somewhere that survives the host is the operator's job and is not automated
  here.
- **No encryption at rest for the dump.** The archive contains `api_keys` rows and
  every workflow parameter. Treat a dump as a secret: restrict the directory, and
  encrypt before it leaves the host.
- **No point-in-time recovery.** See §4.
- **No backup monitoring.** Nothing alerts if `backup.sh` stops running. A cron
  job that fails silently for three weeks is indistinguishable from no backup at
  all; wire the exit code into whatever alerts you.
- **No NATS state backup**, deliberately. NATS carries notifications; Postgres
  holds truth ([ADR-0003](adr/0003-nats-as-event-bus.md)). There is nothing there
  to restore.
- **Postgres roles and grants are not in the dump.** `pg_dump --no-owner
  --no-privileges` is used so the archive restores cleanly as whatever role you
  have, which means role definitions are *not* carried. In the MVP there is one
  role, so this costs nothing; once per-service roles exist (they arrive as a
  migration), the roles must be created by the migration or by
  `pg_dumpall --roles-only`, not by this script.

---

## 6. Schema migrations and upgrades

### 6.1 The mechanism

Migrations are applied by a **dedicated job, never by a service at startup**
([ADR-0011](adr/0011-microservice-decomposition.md) rule 4). Six service replicas
racing to run `CREATE TABLE` is how you get a half-migrated schema and six crash
loops.

- `services/migrations/NNNN_name.up.sql` / `.down.sql`, applied in lexical order.
- `deploy/migrate.sh` is the job. It waits up to 60s for Postgres, creates a
  `schema_migrations(version, applied_at)` ledger, then applies each unapplied
  `*.up.sql` in **one transaction per migration** and records it. A failure
  leaves the schema untouched rather than half-migrated, so a retry starts from a
  known state.
- Idempotence comes from the ledger, not from the SQL: `0001_init.up.sql` is
  frozen and contains plain `CREATE TABLE`, so re-running it would fail. The
  ledger is what makes `docker compose up` twice in a row work.
- `deploy/Dockerfile.migrate` is a plain `postgres:16-alpine` running `psql`, not
  a Go binary, so the migration job cannot fail to start because service code
  does not compile.
- Every service declares `depends_on: {migrate: {condition:
  service_completed_successfully}}`, so nothing starts against an unmigrated
  schema.

At the time of writing, five versions are applied in the dev database
(`0001_init` … `0005_service_roles`).

### 6.2 Upgrade procedure

```bash
# 1. Back up FIRST, and verify the archive is readable. A migration is the most
#    likely reason you will need a restore.
BACKUP_DIR=/var/backups/nexus deploy/backup.sh

# 2. Apply migrations. In compose this is the migrate job; anywhere else it is
#    the same script with a DSN and a migrations directory.
docker compose -f deploy/docker-compose.yml run --rm migrate
#   or:  DATABASE_URL=... MIGRATIONS_DIR=./services/migrations deploy/migrate.sh

# 3. Only then roll the services.
```

Ordering is the whole procedure: back up, migrate, *then* deploy code. A
migration is the change most likely to need the backup you took, and taking it
afterwards is taking it too late.

### 6.3 Rolling back

Honest limitations, in order of how much they will surprise you:

- **`.down.sql` files exist for every migration, but the job cannot run them.**
  `migrate.sh` only applies `*.up.sql`. Rolling back means running the `.down.sql`
  by hand with `psql` and deleting the row from `schema_migrations` yourself.
  Neither half has been exercised.
- **A down migration that drops a column destroys data.** Restoring from §3 is
  usually the better answer, at the cost of the RPO in §4.
- **The backward-compatibility rule is stated but not enforced.**
  [ADR-0011](adr/0011-microservice-decomposition.md) rule 5 requires services to
  be compatible across one schema version — additive migrations only, a column
  dropped a release *after* its last reader is gone. Nothing checks this: no test
  runs the previous service version against the new schema, and `migrate.sh` will
  happily apply a `DROP COLUMN`. So a non-additive migration breaks the
  currently-running services between step 2 and step 3 above. For a single-replica
  dev stack that is a non-issue; for a rolling production deploy it is the thing
  to get right, and the tooling does not help you — the discipline is yours to
  keep.

---

## 7. Configuration guard: an explicit DSN in production

Related, because it is the same class of failure as an untested backup — a
misconfiguration that looks fine until it is not.

Six services resolve their DSN through `internal/config.DSN()`, which accepts
four spellings (`NEXUS_DATABASE_URL`, `DATABASE_URL`, `NEXUS_POSTGRES_DSN`,
`NEXUS_DSN`) and falls back to a compiled-in developer default pointing at
`localhost:55432`. Accepting four names is what keeps dev painless; the fallback
is what makes a typo in production look like a network problem instead of a
configuration problem.

Set **`NEXUS_ENV=production`** (or `prod`, or `staging`) or **`NEXUS_REQUIRE_DSN=1`**
in any real deployment. With either set and no DSN found, every service exits at
startup with a message naming all four spellings it checked, instead of quietly
dialing `localhost`. `NEXUS_REQUIRE_DSN=0` overrides `NEXUS_ENV` for the case
where the requirement is inherited from a base config that cannot be edited.

With neither set, behaviour is exactly as before: the dev default applies and
`go test` needs no environment.
