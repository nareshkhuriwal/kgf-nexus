# Nexus — Operations: backup, restore, upgrade

**Status:** Phase 1. Covers the procedures that exist and have been executed.
**Audience:** whoever is on call.
**Scope:** Postgres backup and restore, the artifact (MinIO result store) backup
and restore and the ordering between the two stores, the restore drills and their
measured results, what these procedures do *not* cover, the RPO/RTO the approach
actually delivers, and the schema-migration procedure.

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

**Postgres is not the only thing that must be backed up.** Task results larger
than `RESULT_STORE_INLINE_THRESHOLD_BYTES` (64 KiB) are *not* in Postgres: the
row holds only a reference, and the bytes live in MinIO
([ADR-0008](adr/0008-result-store-separation.md)). A Postgres-only backup
therefore restores a history whose large `output_ref`s point at objects that no
longer exist — a **dangling ref**, which is silent data loss that *asserts* the
data is present. §7 covers the artifact half and the ordering between the two.
`deploy/backup.sh` on its own is half a backup; **`deploy/backup-all.sh` is the
whole one.**

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

- **~~MinIO / the result store is not backed up by these scripts.~~** *Closed —
  see §7.* `deploy/backup-artifacts.sh` mirrors the result bucket,
  `deploy/restore-artifacts.sh` restores it, and `deploy/backup-all.sh` runs both
  halves in the order that makes the skew safe. `deploy/restore-drill.sh` proves a
  restored artifact is byte-identical and that its `output_ref` still resolves.
  What remains uncovered is listed in §7.6 — notably that the mirror is a plain
  local copy with no off-host replication and no encryption, exactly like the
  Postgres dump.
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

### 6.2 What makes a migration safe to deploy

[ADR-0011](adr/0011-microservice-decomposition.md) rule 5 promises a rolling deploy
can run mixed versions against one schema, and requires additive migrations only:
"a column is dropped a release *after* the last reader is gone."

Nothing enforced that. `migrate.sh` applies whatever SQL it finds — it has no
opinion about whether a statement is safe to run while the previous version of the
fleet is still serving. `make test` now runs a gate
(`internal/migrations`) that scans every `*.up.sql` and fails on the shapes that
break a mixed-version deploy:

| Rule | Severity | Why it breaks a rolling deploy |
|---|---|---|
| `DROP COLUMN` | blocking | an old replica still `SELECT`s it |
| `DROP TABLE` | blocking | an old replica still reads it |
| `RENAME` | blocking | a drop and an add at once; neither version sees the other's name |
| `ALTER ... TYPE` | blocking | rewrites the table under `ACCESS EXCLUSIVE`, and can change what an old replica reads back |
| `SET NOT NULL` | blocking | an old replica that `INSERT`s without the column starts failing |
| `ADD COLUMN ... NOT NULL` with no `DEFAULT` | blocking | forces a rewrite and breaks inserts from an old replica |
| `UPDATE` with no top-level `WHERE` | review | row locks across the whole table inside the migration's transaction |
| `CREATE INDEX` without `CONCURRENTLY` | review | holds a write lock for the whole build |

A finding names the file, line and reason, so the reviewer judges. Deliberate
exceptions go in the `accepted` allowlist **with a reason** rather than by relaxing
a rule, and a second test fails if an allowlist entry stops matching — a stale
exemption is how a hazard gets silently re-admitted later.

The checks are syntactic on purpose. A SQL parser would be more precise and also a
dependency; these rules are about *shape* — "does this statement's blast radius
extend to the whole table" — and the migration set is small and reviewed.

#### The 0002 precedent, and why it was not rewritten

`0002_event_seq.up.sql` is the statement the gate exists to catch:

```sql
ALTER TABLE workflow_runs ADD COLUMN event_seq INT NOT NULL DEFAULT 0;
UPDATE workflow_runs r
   SET event_seq = COALESCE((SELECT max(e.seq) FROM run_events e WHERE e.run_id = r.id), 0);
```

Two things are worth stating precisely, because the obvious reading of each is
wrong:

- **The `ADD COLUMN` does not rewrite the table.** On PostgreSQL 11+ a `NOT NULL
  DEFAULT` addition is metadata-only. Verified on the PG 16 this stack runs:
  `pg_relation_filenode` is unchanged across the statement. It still takes
  `ACCESS EXCLUSIVE`, so it queues behind open transactions and then blocks
  readers and writers behind it — but it is fast.
- **The backfill is the real cost, and its `WHERE` does not bound it.** The `WHERE`
  belongs to the *subquery*; the outer `UPDATE` touches every row in
  `workflow_runs`, taking row locks across the table inside the migration's single
  transaction. On an empty development database that is instant, which is exactly
  why it was never noticed. Contrast `0003` and `0004`, which carry a genuine
  top-level `WHERE` and are correctly not flagged — they are the safe shape of the
  same statement.

It was **not** rewritten. `migrate.sh` keys the ledger on the file name, so editing
an applied migration changes nothing on any database that already ran it, while
silently diverging the file from the schema it describes. The migration is applied
on every existing database; the cost is historical and is recorded here instead.

The mixed-version *allocator* hazard that used to accompany it is genuinely gone: an
old replica allocating `max(seq)+1` alongside a new one allocating from `event_seq`
would have reintroduced the collision F7 fixed, and there is no longer any
`max(seq)+1` allocator in the tree (see F67).

### 6.3 Upgrade procedure

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

### 6.4 Rolling back

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

## 7. Artifacts: backing up the result store

Postgres is the system of record, but it is not the whole backup. Results above
`RESULT_STORE_INLINE_THRESHOLD_BYTES` (64 KiB) live in MinIO; Postgres holds only
a reference ([ADR-0008](adr/0008-result-store-separation.md)). This section is the
other half of §2 and §3.

### 7.1 Running it

```bash
# The whole backup: objects THEN Postgres, in that order, which matters (§7.2).
BACKUP_DIR=/var/backups/nexus deploy/backup-all.sh     # or: make backup-all

# Either half alone, if you need it:
BACKUP_DIR=/var/backups/nexus deploy/backup-artifacts.sh
BACKUP_DIR=/var/backups/nexus deploy/backup.sh
```

Two connection modes, exactly like `backup.sh`, so the same script runs on a
laptop and in production:

| Mode | Trigger | What it does |
|---|---|---|
| host | `RESULT_STORE_ENDPOINT` (or `NEXUS_RESULT_STORE_ENDPOINT`, `NEXUS_S3_ENDPOINT`) set | runs `mc` on this host against that endpoint |
| container | nothing set, `nexus-minio-1` present | runs the `mc` **inside** the MinIO container |

Container mode exists because the MinIO image already ships a version-matched
`mc`, so no host install is needed. Credentials come from
`RESULT_STORE_ACCESS_KEY` / `RESULT_STORE_SECRET_KEY`, falling back to
`MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD`.

Each run writes `nexus-artifacts-<stamp>/` (the objects, laid out under their
real keys) and `nexus-artifacts-<stamp>.manifest` (`sha256  size  key` per line).
Because keys are content-addressed, the manifest is a genuine integrity record
rather than a restatement of the filename — the drill recomputes every digest and
compares.

### 7.2 The ordering, and why it is not arbitrary

**Objects are captured first, then Postgres.** This is the part that needed a
decision rather than a command.

The two stores cannot be snapshotted atomically: no shared transaction, no shared
clock, and the stack keeps running while both are read. Whichever is captured
second contains writes the first one missed. That skew cannot be removed, only
*steered* — into one of two failure modes:

| | What it is | Severity |
|---|---|---|
| **Dangling `output_ref`** | row exists, object does not | **Unrecoverable.** Silent: the run reads SUCCEEDED, the UI shows an output link, and a downstream retry fails on an input the database swears exists. The producing attempt is over and the object was content-addressed, so there is no other copy. |
| **Orphan object** | object exists, no row references it | **Benign.** Wasted disk. ADR-0008 already mandates a reachability-keyed GC sweep over live `output_ref`s, so this is a condition the design expects and already has a cleaner for. |

Capturing objects first means the skew window only ever contains rows written
*after* the mirror closed. `agent/internal/resultstore.Put` is what upgrades that
from a hope to a guarantee: it returns the `ResultRef` only **after** `PutObject`
succeeds, so an object is durable strictly before the row naming it exists. Any
attempt row in the later Postgres dump therefore references an object that already
existed when the earlier mirror ran.

**Result: the pair can produce orphans, never dangling refs.** Reversing the order
inverts exactly this. Running the two halves *concurrently* discards the guarantee
altogether — with no ordering between them, a row written during the overlap can
reference an object the mirror had already passed. `backup-all.sh` exists so this
ordering is not left to whoever writes the cron entry.

What this does **not** buy: a consistent point in time. Neither half is an
instant, and objects written during the mirror may or may not be included. The
only claim is the directional one above.

### 7.3 Restore

Restore runs in the **opposite** order — Postgres first, then objects:

```bash
deploy/restore.sh           backups/nexus-<stamp>.dump          [target-db]
deploy/restore-artifacts.sh backups/nexus-artifacts-<stamp>/    [target-bucket] [--yes]
```

The inversion is deliberate. Both orderings converge on the same end state; the
order only decides which failure is visible *while the restore is in flight*.
Restoring objects first would leave a window where a service pointed at the
half-restored database sees runs whose outputs are missing — the dangling-ref
failure, reintroduced during recovery. Postgres first leaves rows whose objects
are still arriving, which is the same transient condition as a task that has not
finished uploading, and which the retry path already handles.

This reduces the blast radius of pointing services at a half-restored stack; it
does not make it safe. **Wait for both halves.**

Like `restore.sh`, the artifact restore prints a plan and **refuses a target
bucket that already holds objects** unless `--yes` / `FORCE=1` is given, pointing
you at a scratch bucket instead. Objects are immutable and content-addressed, so
re-uploading identical content is a no-op — but nothing can verify a mirror came
from *this* deployment, and a cross-deployment key collision overwrites data with
no undo.

### 7.4 The drill

```bash
deploy/restore-drill.sh      # or: make restore-drill
```

Precedent: commit c9f6ede proved a Postgres restore rather than asserting one.
This applies the same standard. "The script exited 0" is not evidence; the drill
proves the two claims that matter:

1. a restored object is **byte-identical** to the original (sha256 of the bytes
   read back *out of the bucket*, not out of the mirror), and
2. a restored `task_attempts.output_ref` **still resolves** to it — following the
   real `{"result":{"blob":{"key","bucket","sha256"}}}` shape from row to object,
   the same path a downstream task retried after a restore would take.

Claim 2 is what makes this a drill of the *gap* rather than of `mc mirror`. The
gap was never "objects cannot be copied"; it was that a restored database points
at objects nobody backed up.

It uses a **scratch bucket and a scratch database**, both suffixed with the
script's PID and dropped on exit. It never writes to `nexus-results` and never
restores over `nexus`.

### 7.5 Why the drill's checks are trusted

A check nobody has seen fail is not evidence. Every assertion is also run against
a deliberately broken input, and the drill fails if a check passes when it should
not:

- an object is **deleted** from the restored bucket → the presence check must catch it
- a **byte is corrupted** → the digest comparison must catch it
- an `output_ref` points at a **missing key** → the resolution sweep must find exactly one dangling ref
- a count is read from a **bad alias** → the instrument must error rather than return 0

That last one is not hypothetical, and it is the trap worth knowing about:

> **`mc ls <misspelled-alias>/<bucket>` exits 0 and prints nothing** — byte-identical
> to "the bucket is empty". A misspelled *bucket* errors (rc=1); a misspelled
> *alias* does not.

So a zero object count means either "nothing to back up" or "this has been reading
a typo the whole time". `mc stat` on the bucket distinguishes them because it
fails on an alias that does not resolve, and **every count in these scripts is
gated behind that stat**. The drill verifies this instrument before trusting any
count.

### 7.6 What is still NOT covered

- **No off-host copy, and no encryption.** The mirror is a local directory, same
  as the Postgres dump (§5). Objects are task outputs and must be treated as
  secret.
- **No consistent point in time.** See §7.2. The guarantee is directional
  (orphans, never dangling refs), not a snapshot.
- **Orphan objects accumulate** after a restore, by design. The ADR-0008 GC sweep
  that reclaims them now exists — see §7.7 — but it is an operator-run command
  that defaults to reporting, so nothing reclaims that space until someone runs
  it in delete mode.
- **No verification that a full production-sized mirror completes in useful time.**
  The drill uses a single 128 KiB object. `mc mirror` is incremental and
  content-addressed keys never change, so subsequent mirrors copy only new
  objects — but the *first* mirror of a large bucket is unmeasured, and the RTO in
  §4 does not include artifact restore time. **Measure it on your data.**
- **Bucket policies, IAM users and versioning are not captured.** Only objects
  are. A restore recreates the bucket with `anonymous set none`, matching
  `minio-init`; anything beyond that must be reapplied.
- **Dangling refs are reported on demand, not continuously.** `make check-refs`
  (§7.8) walks every reference and reports rows whose object is missing, but
  nothing runs it on a schedule: no service starts it, so it closes the gap only
  as often as someone invokes it. Note that the GC sweep in §7.7 is the
  *opposite* direction — objects with no row, not rows with no object.

### 7.7 Reclaiming orphans: the GC sweep

[ADR-0008](adr/0008-result-store-separation.md) requires "a garbage-collection
sweep keyed on reachability from live `output_ref`s — deliberately a background
reconciler, never an inline delete". §7.2 and §7.6 are where orphans come from:
the backup ordering produces them on purpose, and a restore leaves them behind.

```bash
# Report only. Deletes NOTHING. This is the default.
make artifact-gc

# Same, scoped to one namespace prefix, with a longer grace period.
go run ./services/artifact-gc/cmd/artifact-gc -prefix default/ -grace 72h

# Actually delete. BOTH flags are required; neither alone does anything.
go run ./services/artifact-gc/cmd/artifact-gc \
  -delete -i-understand-this-deletes-data
```

**It is not wired to run automatically.** `artifactgc.Sweeper` has a `Run` loop
for the day the background reconciler is turned on, but no service starts it and
no compose entry references it. Collection happens when an operator decides it
happens.

**What "reachable" means.** The reference set is derived from the database by
query, over **two** columns, not one:

| Source | Why it counts |
|---|---|
| `task_attempts.output_ref` | the attempt that produced the object |
| `task_attempts.input_refs` | a **downstream** attempt that consumed it |

The second is easy to miss — ADR-0008 says "live `output_ref`s" — and missing it
deletes live data. The DAG engine copies an upstream `output_ref` verbatim into
the downstream attempt's `input_refs`, so once a producing run is pruned
(`ON DELETE CASCADE`), a consuming attempt in another run can be the *only*
row naming that object. A sweep over `output_ref` alone would collect it.

Extraction is **shape-driven**, not path-driven: it looks for a `blob` object
with a `key` member anywhere in the document. The restore drill's
`output_ref->'result'->'blob'->>'key'` is right for a row the drill constructs,
but wrong here — the top-level name is the task's declared *output name*, chosen
by the workflow author, and this database already contains `result`, `out` and
`features`. A fixed-path reader would see no reference in the others and report
their objects as orphans.

**The grace period** (default 24h, `-grace`) is what covers the window the
reachability query cannot see: an object already uploaded by an in-flight attempt
whose row has not committed. That window is not bounded by anything in the
control plane — a worker can upload a checkpoint, keep running for hours, then
buffer its report through a control-plane outage — so the threshold is chosen
generously rather than derived. Raise it if your tasks checkpoint early; never
lower it toward minutes.

**Ordering.** The sweep lists objects **first**, then queries Postgres, and only
considers the intersection. This is the same skew problem as §7.2 and the
reasoning is the mirror image: reading the reference set strictly *later* than
the candidate set means an object created during the window is simply not a
candidate this pass. The reverse order — query, then list — deletes an object
whose row was committed in between. See the comment on `artifactgc.Sweeper.Once`
for the worked timeline.

**Bounded and auditable.** One pass examines at most `-max-objects` (default
10,000) rather than listing a whole bucket into memory, so a large backlog takes
several runs. Every pass logs what it examined, kept as reachable, skipped as too
young, and deleted, with counts.

---

## 8. Configuration guard: an explicit DSN in production

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

### 7.8 The other direction: rows whose object is gone

§7.7 reclaims objects nothing references. This is its mirror image, and the more
serious of the two: a **dangling reference** is a row whose `output_ref` or
`input_refs` names a key the store does not have. An orphan wastes disk and
breaks nothing; a dangling ref means a task's result is **unreadable**, and any
downstream task that maps that output into its inputs will fail when it fetches.

```sh
make check-refs
```

It is **read-only by construction** and exits nonzero when it finds anything, so
it works as a monitoring check. There is deliberately no repair mode: the object
is gone, so the row cannot be fixed — only deleted or rewritten, and both destroy
the evidence needed to work out what happened. Worse, the likeliest cause of a
mass "dangling ref" report is a **misconfigured check** (wrong bucket, wrong
endpoint, a half-finished restore), and a repairer pointed at an empty bucket
would erase every reference in the database. A human decides.

The command refuses a bucket that does not resolve, for the same reason the
backup scripts gate every count behind `mc stat`: an empty answer from a bucket
that is not there is indistinguishable from a bucket in perfect health.

**A nonzero result is a real fault, not drift.** §7.2's ordering — objects first,
then Postgres — is designed to produce orphans and never dangling refs. So a hit
means a restore ran out of order, an object was deleted out of band, or the §7.7
sweep removed something it should not have. Any of those is worth investigating
immediately.

Ordering is the reverse of the sweep's, deliberately. The sweep lists objects
before querying Postgres so a newly written object is simply not a candidate.
This reads Postgres **first** and checks the store **second**, so a reference
read at T1 has its object checked at T2 > T1 — and since `resultstore.Put` makes
the object durable strictly before the row is written, an object that existed at
T1 still exists at T2. A false *negative* (an object deleted after the check) is
acceptable and shows up next run; a false *positive* would send someone hunting
for data loss that never happened.

---

## 9. Cluster controller: installing, enabling apply, and backing out

Design and safety argument: docs/CLUSTER-SCALING.md. This section is the runbook.

### 9.1 Prerequisites

- Migration `0018_cluster_registry` applied (additive; safe before the new
  gateway and resource-manager are deployed).
- A resource-manager that serves `ClusterControllerService` (this release), reachable
  from the controller at `NEXUS_RM_ADDR`. The controller uses the same TLS
  environment as other peers (`internal/transport`).
- The worker Deployment follows `deploy/k8s/cluster-controller/base/worker-deployment.example.yaml`:
  no hostname override, CPU/memory requests, `terminationGracePeriodSeconds` at
  least `NEXUS_DRAIN_GRACE`, no HPA of its own.
- The cluster-controller image built with
  `docker build -f deploy/Dockerfile.service --build-arg SERVICE=cluster-controller .`
  and pinned by digest (`kustomize edit set image`).

### 9.2 Plan mode (default)

```sh
kubectl apply -k deploy/k8s/cluster-controller/base   # after setting NEXUS_CLUSTER_NAME
kubectl -n kgf-nexus get lease nexus-cluster-controller-<cluster> -o jsonpath='{.spec.holderIdentity}'
```

In the console (Cluster & scaling → Live cluster) confirm: a report under a minute
old, "Controller running: plan", the observed UID, desired = observed revision,
the scheduler HPA with `ScalingActive`, and plan-only events that match what you
would have done by hand. `/readyz` is `standby` on the follower and fails on the
leader after four intervals without a successful cycle -- alert on that.

### 9.3 Enabling apply

1. `kubectl get namespace kube-system -o jsonpath='{.metadata.uid}'` against the
   intended cluster; enter it in the console and confirm apply mode.
2. `kubectl apply -k deploy/k8s/cluster-controller/apply`.
3. Watch for `scale_out` / `drain` / `scale_in` events and "Controller running:
   apply". If the tab still says plan, its reason names the missing opt-in.

Keep registry ceilings (`max_replicas`) at what the budget allows: there is no
provider cost estimate, and budget alerts are not hard caps.

### 9.4 Backing out

Fastest to slowest, all safe at any moment:

1. Console: **Switch to plan**. The next cycle (≤ 15 s) stops mutating; the resource
   manager refuses drains immediately.
2. `kubectl apply -k deploy/k8s/cluster-controller/base` (drops the apply Role
   binding and `NEXUS_CONTROLLER_ALLOW_APPLY`).
3. `kubectl -n kgf-nexus scale deploy/nexus-cluster-controller --replicas=0`.

Backing out never un-drains a worker: DRAINING is terminal by design. A drained
pod that is no longer going to be removed can simply be deleted
(`kubectl delete pod`); its agent registers afresh in the replacement pod.

### 9.5 Diagnosing a pool that does not scale

The Live cluster tab shows the last decision and its holds per Deployment. The
common ones: `scale-in cooldown`, `removal of drained pods held: pod … is not
Running and Ready` (fix the pod), `pod … kept: holds N live lease(s)` (working as
intended -- it is removed when the work finishes), `namespace quota: …` (raise the
quota or the pod requests are undeclared), and `deployment changed since it was
read` (someone else is writing replicas -- find the second writer).

### 9.6 Testing locally

The end-to-end test only runs against `docker-desktop` or `kind-*` contexts, in
namespace `nexus-scaling-test`, which it creates and deletes:

```sh
docker run -d --name nexus-scaling-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=nexus \
  -p 127.0.0.1:55531:5432 postgres:16-alpine   # then apply services/migrations/*.up.sql
NEXUS_TEST_DATABASE_URL=postgres://postgres:test@localhost:55531/nexus \
  go test -tags k8s_integration -run TestK8s -v ./services/cluster-controller/internal/controller/
NEXUS_TEST_DATABASE_URL=postgres://postgres:test@localhost:55531/nexus \
  go test -tags integration -run TestIntegrationClusterRegistry ./internal/store/
```
