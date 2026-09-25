# Nexus — Operations: backup, restore, upgrade

**Status:** Phase 1. Covers the procedures that exist and have been executed.
**Audience:** whoever is on call.
**Scope:** Postgres backup and restore, the artifact (MinIO result store) backup
and restore and the ordering between the two stores, the restore drills and their
measured results, what these procedures do *not* cover, the RPO/RTO the approach
actually delivers, the schema-migration procedure, TLS certificate rotation (§9), email delivery (§10), the NATS event bus (§11), the cluster controller (§12), the Kubernetes executor (§13), dynamic expansion, memoization and quarantine (§14), the windowed pull claim at large backlogs (§15), gateway rate limiting (§16), metrics: endpoints, scrape configuration and the metrics catalogue (§17), operator retry of a failed run (§18), and durable task logs: retention and sizing (§19).

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
  to restore. The `outbox` table IS in the dump; after a restore the relay
  re-publishes whatever it holds unpublished, and consumers de-duplicate by
  run_id + seq.
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

The sequence has a hole: `0018_cluster_registry` is followed by
`0020_dynamic_expansion`, and there is **no 0019**. That
number was never used on any branch. It is not a lost migration, and it must not
be filled in later: on an existing database a new `0019_*` would run after 0020+,
and on a fresh one before them. See `services/migrations/README.md`.
`internal/migrations` fails `go test` on a duplicate version, a filled or new gap,
or a malformed filename.

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

# Run directly, the DSN must be explicit (§8): there is no built-in default.
export DATABASE_URL='postgres://nexus:...@db.internal:5432/nexus?sslmode=require'

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

## 8. Configuration guard: every service requires an explicit DSN

Related, because it is the same class of failure as an untested backup — a
misconfiguration that looks fine until it is not.

Every service that talks to Postgres (api-gateway, workflow-manager, dag-engine,
scheduler, resource-manager, event-service, artifact-gc) resolves its DSN through
`internal/config.DSN()`. It accepts four spellings, in this precedence order:

1. `NEXUS_DATABASE_URL`
2. `DATABASE_URL`
3. `NEXUS_POSTGRES_DSN`
4. `NEXUS_DSN`

and, for the services that have one, the command-line flag (`-dsn` on scheduler
and dag-engine, `-database-url` on resource-manager and artifact-gc). A flag
given on the command line overrides the environment.

**There is no built-in default, in any environment.** With none of the above
set, the service exits with status 1 before opening anything, printing:

```
scheduler: fatal: no Postgres DSN configured: set NEXUS_DATABASE_URL (also
accepted: DATABASE_URL, NEXUS_POSTGRES_DSN, NEXUS_DSN) or pass -dsn. There is no
built-in default, ...
```

Services used to fall back to `postgres://postgres:test@localhost:55432/nexus`
(artifact-gc to `localhost:55433`) unless `NEXUS_ENV` was `production`/`staging`
or `NEXUS_REQUIRE_DSN` was set. That fallback meant a typo'd variable name, or a
manifest that missed one container, connected the service to whatever Postgres
listened on that port. On at least one developer machine 55432 is an unrelated
product's database. `NEXUS_ENV` and `NEXUS_REQUIRE_DSN` no longer affect the DSN;
setting them is harmless, and `NEXUS_REQUIRE_DSN=0` can no longer switch the
check off.

Where the DSN comes from in each supported way of running Nexus:

| How you run it | DSN source |
|---|---|
| `make up` / `deploy/docker-compose.yml` (and the TLS overlay) | `x-postgres-env` sets `DATABASE_URL` for every service, pointing at the compose `postgres` container. |
| Helm chart (`deploy/helm/kgf-nexus`) | `nexus.dbEnv` builds `DATABASE_URL` from `.Values.postgres.*` and the password in `existingSecret`. |
| `make seed`, `make migrate-*`, `make artifact-gc`, `make check-refs` | The Makefile's `DATABASE_URL`, defaulting to the compose stack on `localhost:$(NEXUS_POSTGRES_PORT)` (55433). Override it to target another database. |
| `go run ./services/<svc>/...` by hand | Export one of the four names yourself, e.g. `export DATABASE_URL='postgres://nexus:nexus-dev-password@localhost:55433/nexus?sslmode=disable'` for the compose stack. |
| Integration tests | `NEXUS_TEST_DATABASE_URL` only (`internal/testdb`); tests skip without it and never read the service spellings. |

`examples/run-suite.sh` and `tests/chaos` drive an already-running stack
(Kubernetes or compose respectively) and do not start services themselves, so
the DSN is whatever that stack was deployed with.

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

## 9. TLS certificates: rotation runbook and expiry alerts

Design and rationale are in [TLS.md §5](TLS.md). This section covers what to do.

**Verified:** everything this section says about processes: hot reload, rejection
of bad material, CA overlap, agent re-enrolment. Unit and integration tests in
`internal/transport` cover it with real gRPC handshakes, and the tests check the
serial number actually presented. `deploy/nexus-certs.sh` has been run on
OpenSSL 3.0 and LibreSSL 3.3, and its `install` step inside alpine.
**Not verified:** the full `make up-tls` stack end to end; anything on AKS (the
manifests have only been parsed, never applied); the Portainer host procedure;
the alert rules against a real Prometheus.

### 9.1 Is rotation working? (any environment)

After any renewal, each process logs one line per role within
`NEXUS_TLS_RELOAD_INTERVAL` (default 10s) plus the time the volume takes to
update (AKS: kubelet sync, usually under a minute):

```
INFO TLS material reloaded; new handshakes use it, ... role=server old_serial=… new_serial=…
```

To confirm from outside what a server actually presents:

```sh
openssl s_client -connect <host>:9090 -servername scheduler </dev/null 2>/dev/null \
  | openssl x509 -noout -serial -enddate
```

Compare that serial with the file: `openssl x509 -in tls.crt -noout -serial`. A
restart is **never** needed to pick up a renewal. If you find yourself restarting
to make a certificate take effect, something is wrong. Look for the ERROR line in
§9.3 first.

### 9.2 Routine leaf renewal

| Environment | Who renews | What you do |
|---|---|---|
| AKS | cert-manager, at 10 days left (30-day leaves) | Nothing. Watch the alerts. |
| Local compose | You | `deploy/nexus-certs.sh issue <svc> --dns <svc>,localhost --ip 127.0.0.1`, then `docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.tls.yml run --rm tls-init`. Or `make dev-certs` then the same `tls-init` for everything. |
| Portainer host | You, monthly (§9.5) | Issue on the CA machine, install on the host. |
| Off-cluster agent | The agent host, monthly (cron) | `csr` → `sign-csr` → `install-signed` (TLS.md §5.5). |

### 9.3 Alerts, and what to do

The rules are in `deploy/k8s/cert-alerts.yaml` (a PrometheusRule). For a plain
Prometheus, for example on the Portainer host, the equivalent rules file is:

```yaml
groups:
  - name: nexus-certificates
    rules:
      - alert: NexusServedCertificateExpiringSoon
        expr: (nexus_tls_cert_not_after_timestamp_seconds - time()) < 3 * 86400
        for: 15m
        labels: { severity: critical }
      - alert: NexusTLSReloadRejected
        expr: max_over_time(nexus_tls_reload_stale[10m]) == 1
        for: 10m
        labels: { severity: warning }
      - alert: NexusCACertificateExpiringSoon
        expr: (nexus_tls_cert_not_after_timestamp_seconds{role="ca"} - time()) < 21 * 86400
        for: 1h
        labels: { severity: critical }
```

Alert on the **timestamp** minus `time()`, never on `nexus_tls_cert_expiry_seconds`.
The latter is refreshed by the process's poller and would freeze if the poller
stalled. On hosts without Prometheus, run `deploy/nexus-certs.sh check <dir>` from
cron. It exits non-zero when anything is within 7 days of expiry
(`--warn-days N`).

| Alert | Meaning | Do this |
|---|---|---|
| `NexusCertificateRenewalOverdue` (AKS) | cert-manager should have renewed 3 days ago and has not | `kubectl -n kgf-nexus describe certificate <name>`, then `kubectl get certificaterequest,order -n kgf-nexus`. The usual causes are a ClusterIssuer that is not Ready (CA secret missing or expired) or cert-manager itself down. Once fixed, `cmctl renew -n kgf-nexus <name>` forces a renewal. |
| `NexusCertificateNotReady` (AKS) | The Certificate's last issuance failed | Same as above. |
| `NexusServedCertificateExpiringSoon` | A **process** is serving a certificate with under 3 days left | First, is the file on disk newer? Compare `openssl x509 -in <file> -noout -enddate` with the metric. **File newer, process older:** the process is not picking it up. Look for `TLS material on disk REJECTED` in its logs; if the mount is a `subPath`, change it to a directory mount. As a last resort, restart that one pod. **File old too:** renewal is broken, so treat it as `RenewalOverdue`. |
| `NexusTLSReloadRejected` | New files arrived and the process refused them. It is still serving the previous certificate, which is safe until that one expires | Read the ERROR log line; it names the reason. `private key does not match public key` means the key and certificate come from different issuances, so re-issue them together. `expired` or `not valid until` means an issuer clock or stale-file problem. `contains no PEM certificates` means the bundle file is wrong. The alert clears as soon as valid files land; no restart is needed. |
| `NexusCACertificateExpiringSoon` | The CA itself expires within 3 weeks | On AKS, cert-manager renews the CA at 30 days left, keeping its key, so this firing means that renewal failed: check the `nexus-internal-ca` Certificate in the `cert-manager` namespace. With a bring-your-own intermediate, issue a new intermediate from the offline root and replace the secret (same key: no overlap needed; new key: §9.4). |

**Readiness does not change** under any of these, deliberately. An expiring
certificate still works, and failing `/readyz` would pull every replica at once.

### 9.4 Rotating the CA key (planned, or after a compromise)

This is the one procedure that can cause an outage if done out of order. The
invariant: **a CA leaves the bundle only after no leaf signed by it is still in
use.** Wait at least one reload interval plus the volume update time between steps.

**Compose / Portainer host / off-cluster agents** (`nexus-certs.sh`, run where the
CA key lives):

1. `deploy/nexus-certs.sh rotate-ca`. This creates a new CA and destroys the old
   CA key; the bundle now holds new + old.
2. Distribute the bundle: re-run `install` (or `tls-init` in compose) on every host,
   and ship `ca-bundle.pem` to every enrolled agent. Wait until every process has
   logged `ca_certs=2`.
3. Re-issue every leaf (`issue …` for services, `sign-csr` for agents) and install.
4. `deploy/nexus-certs.sh drop-retired-cas`. It **refuses** while any leaf it knows
   about is still from the old CA. It cannot see agents enrolled from their own
   CSRs, so confirm those yourself with
   `openssl verify -CAfile ca/ca.crt <agent tls.crt>`.
5. Distribute the bundle again.

**AKS:**

1. Save the current CA certificate: `kubectl -n cert-manager get secret
   nexus-internal-ca-keypair -o jsonpath='{.data.tls\.crt}' | base64 -d > old-ca.pem`.
2. Add it to the `nexus-ca-bundle` Bundle as an `inLine` source (commented in
   `cert-issuer.yaml`) and apply it. trust-manager updates the ConfigMap in every
   labelled namespace.
3. Replace the CA key: either delete the `nexus-internal-ca-keypair` secret, which
   makes cert-manager re-issue the self-signed CA with a new key, or load a new
   intermediate. The Bundle now contains new + old.
4. Re-issue every leaf: `cmctl renew --all -n kgf-nexus`. Confirm that every
   `Certificate` is Ready and that its secret verifies against the new CA.
5. Remove the `inLine` old CA from the Bundle and apply it.

After a **compromise**, the same steps apply, done quickly. Remember that there
is no revocation (TLS.md §5.6): agent connections already established on the
gateway's worker listener survive for up to 5 minutes (the listener's maximum
connection age) before they must re-handshake against the new bundle. To close
that window immediately, restart the gateway after step 5.

### 9.5 Portainer-managed Docker host

The deploy pipeline renders only the base compose file
(`.azure-pipelines/templates/deploy-portainer-host.yml` → `deploy/render_compose.py`).
It does not know about TLS, so TLS on this host is a one-time manual setup,
followed by monthly renewals.

**Where the CA lives:** on an admin workstation or an offline machine, **never on
the Docker host.** The host only receives leaves.

One-time setup:

```sh
# CA machine
NEXUS_CERT_DIR=/secure/nexus-ca NEXUS_CA_NAME="Nexus <env> CA" deploy/nexus-certs.sh init-ca
for s in api-gateway workflow-manager dag-engine scheduler scheduler-gpu resource-manager; do
  NEXUS_CERT_DIR=/secure/nexus-ca deploy/nexus-certs.sh issue "$s" --dns "$s"
done
# copy everything EXCEPT ca/ to the host, e.g.:
rsync -a --exclude 'ca/' --exclude 'ca-retired-*' /secure/nexus-ca/ host:/opt/kgf-nexus/tls-staging/

# Docker host (as root)
/opt/kgf-nexus/nexus-certs.sh install /opt/kgf-nexus/tls-staging /opt/kgf-nexus/tls --owner 10001:10001
rm -rf /opt/kgf-nexus/tls-staging
```

Then add a host-local override next to the stack (the same shape as
`deploy/docker-compose.tls.yml`, with a bind mount of `/opt/kgf-nexus/tls` at
`/etc/nexus/tls:ro` instead of the `tls-init` volume) and include it when bringing
the stack up.

Renewal, monthly (leaves are 30 days): repeat the `issue` loop and the `install`.
The services reload by themselves. Put `deploy/nexus-certs.sh check /secure/nexus-ca`
in the CA machine's cron so that an overdue renewal emails someone.

## 10. Email: SMTP delivery for verification and password recovery

The api-gateway sends two kinds of mail: address-verification links and password-reset links. Both links are credentials. Configure delivery before exposing self-service registration (docs/MULTITENANCY.md).

### 10.1 Settings (api-gateway)

| Variable | Default | Meaning |
|---|---|---|
| `NEXUS_SMTP_HOST` | unset | SMTP server. **Unset disables email delivery** (see 9.2). |
| `NEXUS_SMTP_PORT` | 587 (465 when TLS is `implicit`) | |
| `NEXUS_SMTP_TLS` | `starttls` | `starttls` upgrades before any command and never falls back to plain text if the server does not offer it. `implicit` uses TLS from the first byte (SMTPS). `none` sends in plain text and is for a local relay or Mailpit only. |
| `NEXUS_SMTP_USERNAME` / `NEXUS_SMTP_PASSWORD` | unset | AUTH PLAIN. Credentials with `NEXUS_SMTP_TLS=none` to a non-loopback host stop startup. |
| `NEXUS_SMTP_PASSWORD_FILE` | unset | Read the password from a mounted secret instead of the environment. Setting it together with `NEXUS_SMTP_PASSWORD` is an error. |
| `NEXUS_SMTP_FROM` | required with a host | e.g. `KGF Nexus <no-reply@example.com>` |
| `NEXUS_SMTP_HELO` | `localhost` | EHLO name. Set it to the host's real name if your relay checks it. |
| `NEXUS_SMTP_TIMEOUT_SEC` | 10 | Bounds one whole delivery: dial, TLS, AUTH and DATA. |
| `NEXUS_PUBLIC_URL` | required with a host | External console origin. Links are `<url>/verify-email#token=…` and `<url>/reset-password#token=…`. |
| `NEXUS_EMAIL_VERIFY_TTL_SEC` | 86400 | Verification link lifetime. Allowed range: 5 minutes to 7 days. |
| `NEXUS_PASSWORD_RESET_TTL_SEC` | 1800 | Reset link lifetime. Allowed range: 5 minutes to 24 hours. |
| `NEXUS_EMAIL_VERIFICATION_REQUIRED` | `true` | `false` lets unverified accounts sign in. The gateway logs a WARN at startup when it is `false`. |

The gateway refuses to start on any invalid combination, such as a bad port or TLS mode, a malformed From address, credentials over plain text, or a host without `NEXUS_PUBLIC_URL`. When a host is configured, the gateway also probes SMTP at startup (connect, TLS, AUTH, QUIT, no message). A failed probe logs `SMTP server probe failed` at ERROR but does not stop startup.

### 10.2 Running without a mail server is explicit, not silent

With `NEXUS_SMTP_HOST` unset, the gateway logs `email delivery is DISABLED` at WARN on startup, including the effect on sign-in. The link-request routes then answer `503 email_unavailable` instead of pretending to have sent mail, and registration reports `email_verification.delivery: "mail_disabled"`. With verification required (the default), accounts created while mail is disabled cannot sign in until mail is configured and they request a new link from the sign-in page. Existing accounts are unaffected. Readiness (`/readyz`) does not depend on SMTP. An unreachable mail server must not remove the gateway from the load balancer, so check the startup log and the `user.*` audit outcomes instead.

### 10.3 Delivery and retries

Mail is sent after the database transaction commits, on a bounded background queue: 2 workers, 256 jobs. Each message is retried up to 4 times with exponential backoff, starting at 2 seconds and capped at 30 seconds. 5xx replies are not retried. Every attempt ends in an audit row (`user.email_verification_sent` / `user.password_reset_requested`) with outcome `success` or `error`, and failures are logged with the user id and recipient domain only. Tokens, links and full addresses are never logged. On shutdown, the gateway drains the queue within `NEXUS_SHUTDOWN_TIMEOUT_SEC`. Mail still queued when the process dies is lost, and the person can request another link. Tokens more than a day past expiry are deleted hourly.

### 10.4 Local development: Mailpit

`deploy/docker-compose.yml` runs `axllent/mailpit:v1.27.0` and points the gateway at it (`mailpit:1025`, TLS `none`). Every message appears at http://localhost:8025. That UI binds to loopback because it shows live reset links. **`render_compose.py` deploys the same file**, so a real host must override `NEXUS_SMTP_HOST`, `NEXUS_SMTP_PORT`, `NEXUS_SMTP_TLS`, `NEXUS_SMTP_USERNAME`, `NEXUS_SMTP_PASSWORD`, `NEXUS_SMTP_FROM` and `NEXUS_PUBLIC_URL`. Otherwise reset mail lands in a catcher nobody reads.

### 10.5 Upgrade

Apply migration 0017 before the new gateway. Every existing account is marked verified by the migration itself. Deploy the console together with the gateway: the new pages are `/verify-email`, `/forgot-password` and `/reset-password`. Rolling back 0017 discards which accounts are verified, and re-applying it marks every account that exists at that moment as verified. Export `SELECT id FROM users WHERE email_verified_at IS NULL` first if that matters.

## 11. The event bus (NATS JetStream)

NATS is **optional** and **never authoritative** ([ADR-0003](adr/0003-nats-as-event-bus.md)).
Everything on it is either a hint that work may exist or a projection of a
fact Postgres already holds. Losing it, emptying it, or never running it costs
latency and live-update freshness, never correctness.

### 11.1 Running it

- Compose ships `nats:2.14.7-alpine` with JetStream on the `nats-data` volume
  and a `/healthz?js-enabled-only=true` healthcheck. Monitoring is on port 8222
  (`/varz`, `/connz`, `/jsz`).
- Configuration (F38 naming): `NEXUS_NATS_URL` (empty disables the bus) and
  `NEXUS_NATS_CREDS` (optional `.creds` file). Only the **event-service** (relay,
  SSE wake-ups) and the **agents** (dispatch hints) connect. The scheduler,
  dag-engine and workflow-manager only write `outbox` rows to Postgres.
- Nothing `depends_on` NATS and nothing needs it to start: clients connect in
  the background and retry forever.
- Streams are created by the relay if missing and **never modified** if present,
  so retention retuned with the `nats` CLI sticks:
  `NEXUS_RUNS` (`nexus.run.>`, file, 7 days, 4 GiB, 10-minute duplicate window)
  and `NEXUS_DISPATCH` (`nexus.task.dispatch.>`, memory, 2 minutes).
- **Production needs authorization.** The compose broker is unauthenticated, like
  every other dev credential there. Anyone who can reach an unauthenticated
  broker can read every tenant's run events and publish fake hints. A fake hint
  only costs a poll; reading run events is a real disclosure. Give the
  event-service publish rights on `nexus.>`; give each agent subscribe-only
  rights on `nexus.task.dispatch.<its namespace>.>` and nothing else.

### 11.2 Relay tuning (event-service)

| Variable | Default | Meaning |
|---|---|---|
| `NEXUS_OUTBOX_BATCH_SIZE` | 256 | rows per publish transaction |
| `NEXUS_OUTBOX_POLL_INTERVAL` | 200ms | idle pause between polls of the outbox |
| `NEXUS_OUTBOX_PUBLISH_TIMEOUT` | 5s | ack wait; bounds how long a batch holds row locks |
| `NEXUS_OUTBOX_MAX_BACKOFF` | 10s | cap on the relay-wide backoff during an outage |
| `NEXUS_OUTBOX_RETENTION` | 1h | published rows kept for inspection, then pruned |
| `NEXUS_OUTBOX_MAX_AGE` | 24h | unpublished rows older than this are DROPPED |
| `NEXUS_OUTBOX_HINT_INTERVAL` | 10s | periodic re-announcement of queues with PENDING work |

With `NEXUS_NATS_URL` empty the relay only prunes, so the table holds at most
`NEXUS_OUTBOX_MAX_AGE` worth of writes.

### 11.3 Monitoring (event-service `/metrics`)

| Metric | Alert on |
|---|---|
| `nexus_outbox_oldest_unpublished_seconds` | > 60 for 5m: the bus is behind (outage or relay down) |
| `nexus_outbox_backlog_rows` | sustained growth |
| `nexus_outbox_broker_connected` | 0 for 5m on every replica |
| `nexus_outbox_publish_failures_total{kind="row"}` | increasing while connected: a poison row (see `outbox.last_error`) |
| `nexus_outbox_publish_failures_total{kind="broker"}` | informational during an outage |
| `nexus_outbox_pruned_total{reason="expired"}` | any increase with the bus enabled: announcements were dropped unpublished |
| `nexus_outbox_publish_lag_seconds` | p99, for end-to-end freshness |
| `nexus_outbox_duplicate_acks_total` | informational: re-sends absorbed by de-duplication |

Inspect the backlog directly:
`SELECT topic, count(*), min(created_at), max(attempts) FROM outbox WHERE published_at IS NULL GROUP BY topic;`

### 11.4 Outage behaviour

- **Broker down:** writers are unaffected (they write Postgres). The relay stops
  opening transactions, backs off (jittered, capped), and the backlog waits in
  `outbox`. Agents keep long-polling; SSE tails keep polling at 1s. Dispatch
  latency returns to the long-poll baseline.
- **Broker back:** clients reconnect on their own; the relay notices within
  about 100ms, re-verifies the streams (recreating them if the broker lost its
  storage) and drains the backlog oldest-first. Re-sends inside the duplicate
  window are dropped by `Nats-Msg-Id`; beyond it a consumer may see a duplicate
  and must de-duplicate by run_id + seq. Within one hint interval every queue
  with pending work is re-announced.
- **Down for longer than `NEXUS_OUTBOX_MAX_AGE`:** the oldest unpublished rows
  are dropped and counted. Their facts are still in Postgres; only the
  announcements are gone.
- **Relay replica dies mid-batch:** its transaction rolls back and a peer (or the
  restart) re-sends the batch; de-duplication absorbs it.
- Ordering is not guaranteed in any of these cases.

### 11.5 Upgrading

Apply migration 0016 **before** rolling any service that writes outbox rows: a
new scheduler against a schema without `outbox` fails its claim transactions.
Rolling the migration back requires first rolling back those services (see the
comment in `0016_outbox.down.sql`).

### 11.6 Testing

`make test-nats` runs the relay, outbox and hint suites against a real
JetStream and a real Postgres in throwaway containers (`nexus-nats-agent-*`,
ports 55511/55512), including stopping and restarting the broker.

---

## 12. Cluster controller: installing, enabling apply, and backing out

Design and safety argument: docs/CLUSTER-SCALING.md. This section is the runbook.

### 12.1 Prerequisites

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

### 12.2 Plan mode (default)

```sh
kubectl apply -k deploy/k8s/cluster-controller/base   # after setting NEXUS_CLUSTER_NAME
kubectl -n kgf-nexus get lease nexus-cluster-controller-<cluster> -o jsonpath='{.spec.holderIdentity}'
```

In the console (Cluster & scaling → Live cluster) confirm: a report under a minute
old, "Controller running: plan", the observed UID, desired = observed revision,
the scheduler HPA with `ScalingActive`, and plan-only events that match what you
would have done by hand. `/readyz` is `standby` on the follower and fails on the
leader after four intervals without a successful cycle -- alert on that.

### 12.3 Enabling apply

1. `kubectl get namespace kube-system -o jsonpath='{.metadata.uid}'` against the
   intended cluster; enter it in the console and confirm apply mode.
2. `kubectl apply -k deploy/k8s/cluster-controller/apply`.
3. Watch for `scale_out` / `drain` / `scale_in` events and "Controller running:
   apply". If the tab still says plan, its reason names the missing opt-in.

Keep registry ceilings (`max_replicas`) at what the budget allows: there is no
provider cost estimate, and budget alerts are not hard caps.

### 12.4 Backing out

Fastest to slowest, all safe at any moment:

1. Console: **Switch to plan**. The next cycle (≤ 15 s) stops mutating; the resource
   manager refuses drains immediately.
2. `kubectl apply -k deploy/k8s/cluster-controller/base` (drops the apply Role
   binding and `NEXUS_CONTROLLER_ALLOW_APPLY`).
3. `kubectl -n kgf-nexus scale deploy/nexus-cluster-controller --replicas=0`.

Backing out never un-drains a worker: DRAINING is terminal by design. A drained
pod that is no longer going to be removed can simply be deleted
(`kubectl delete pod`); its agent registers afresh in the replacement pod.

### 12.5 Diagnosing a pool that does not scale

The Live cluster tab shows the last decision and its holds per Deployment. The
common ones: `scale-in cooldown`, `removal of drained pods held: pod … is not
Running and Ready` (fix the pod), `pod … kept: holds N live lease(s)` (working as
intended -- it is removed when the work finishes), `namespace quota: …` (raise the
quota or the pod requests are undeclared), and `deployment changed since it was
read` (someone else is writing replicas -- find the second writer).

### 12.6 Testing locally

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

## 13. Kubernetes executor

An agent that runs its attempts as Kubernetes Jobs (ADR-0007, Implementation
notes). It is opt-in per agent: setting `NEXUS_K8S_NAMESPACE` enables it, and
the agent advertises `kubernetes` (and its alias `k8s`) only after a startup
probe passes. The probe checks that the cluster is reachable, that the server is
1.29 or later (native sidecars), and, through SelfSubjectAccessReview, that the
agent holds the RBAC verbs the executor uses. If the probe fails, the agent logs
`kubernetes executor configured but not usable; not advertising it` with the
reason and keeps serving its other executors.

### 13.1 Deploying

```bash
kubectl apply -f deploy/k8s/executor-namespace.yaml   # nexus-exec, Pod Security "restricted" ENFORCED
kubectl apply -f deploy/k8s/agent-rbac.yaml           # SA + namespace Role; no exec, no secret reads
kubectl apply -f deploy/k8s/executor-policies.yaml    # default-deny NetworkPolicy + LimitRange for task pods
kubectl -n nexus-exec create secret generic nexus-agent-credentials --from-literal=api-key="$NEXUS_WORKER_API_KEY"
# Edit the image (pin a digest) and NEXUS_CONTROL_PLANE, then:
kubectl apply -f deploy/k8s/agent-deployment.yaml
```

The agent and its task Jobs share one namespace. That is required, not a
convenience: each Job is owned by the agent pod through the downward API, and
Kubernetes owner references cannot cross namespaces. A cross-namespace owner is
treated as missing, and the garbage collector would delete every Job the moment
it was created. For that reason the executor sets the owner reference only when
`NEXUS_K8S_OWNER_POD_NAMESPACE` equals `NEXUS_K8S_NAMESPACE`.

**Capacity.** Set `NEXUS_CPU_MILLIS`, `NEXUS_MEMORY_BYTES` and `NEXUS_DISK_BYTES`
to what the namespace can really run: its ResourceQuota, or the size of the node
pool. Two schedulers are involved. Nexus places the attempt on this worker, then
Kubernetes places the pod. If you over-advertise, attempts sit with pods Pending
`Unschedulable` until `NEXUS_K8S_START_TIMEOUT` (default 10m). They then fail as
retryable INFRASTRUCTURE. Each attempt pod also carries a collector sidecar that
requests 50m CPU and 32Mi of memory, on top of the task's own request.

### 13.2 Configuration

| Variable | Default | Meaning |
|---|---|---|
| `NEXUS_K8S_NAMESPACE` | *(unset: disabled)* | Where Jobs are created. Setting it enables the executor. |
| `NEXUS_K8S_KUBECONFIG`, `NEXUS_K8S_CONTEXT` | in-cluster if `KUBERNETES_SERVICE_HOST` is set, else `$KUBECONFIG` / `~/.kube/config` | Out-of-cluster access. |
| `NEXUS_K8S_OWNER` | hostname (the pod name in-cluster) | This agent instance's identity, used for orphan reaping. It must be **stable across restarts** and **unique among agents that share the namespace**. |
| `NEXUS_K8S_OWNER_POD_NAME` / `_UID` / `_NAMESPACE` | unset | Downward API values. When set, each Job is owned by the agent pod. |
| `NEXUS_K8S_SERVICE_ACCOUNT` | namespace default | The service account task pods run as. Its token is never mounted. |
| `NEXUS_K8S_HELPER_IMAGE` | `busybox:1.37.0` | The collector sidecar image. It needs `sh`, `base64`, `wc` and `sleep`. Mirror it for air-gapped clusters. |
| `NEXUS_K8S_IMAGE_PULL_POLICY` / `_IMAGE_PULL_SECRETS` | Kubernetes default / none | Image pull settings for task images. |
| `NEXUS_K8S_RUN_AS_USER` | `65532` | The non-root uid, gid and fsGroup for task pods. |
| `NEXUS_K8S_WRITABLE_ROOT_FS` | `false` | Set to `true` only for images that must write outside `/nexus` and `/tmp`. |
| `NEXUS_K8S_NODE_SELECTOR`, `NEXUS_K8S_TOLERATIONS` | none | Pins this worker's pods to a node pool. Format: `k=v,k2=v2` and `key=value:Effect,key:Effect`. |
| `NEXUS_K8S_GPU_RESOURCE` | unset: GPU tasks are refused | The device-plugin resource, for example `nvidia.com/gpu`. |
| `NEXUS_K8S_GPU_KIND_LABEL` | unset | A node label, for example `nvidia.com/gpu.product`. When set, `gpu_kind` becomes a nodeSelector on that label. |
| `NEXUS_K8S_TTL_AFTER_FINISHED` | `10m` | Lets the Job controller delete Jobs the agent never cleaned up. This is also the window in which a restarted agent can harvest a finished attempt. |
| `NEXUS_K8S_IMAGE_PULL_TIMEOUT` | `10m` | How long a **transient** pull failure may last. A pull that fails as not-found or unauthorised fails immediately. |
| `NEXUS_K8S_START_TIMEOUT` | `10m` | How long an attempt may go unscheduled or unstarted before it fails as retryable. |

### 13.3 What happens to attempts when things fail

| Event | Outcome |
|---|---|
| Image not found / unauthorised / invalid | Prepare fails with `NOT_FOUND` / `PERMISSION_DENIED` / `VALIDATION`, all terminal. The Job is deleted. |
| Collector sidecar crash-loops or cannot pull | `INFRASTRUCTURE`, retryable. This is an agent or cluster fault, never the task's. |
| Task over its memory limit | `OOM`, retryable. The kernel OOM-kills the container and the runtime reports `OOMKilled`. |
| Over `start_to_close` | `TIMED_OUT`. The Job's `activeDeadlineSeconds` sends SIGTERM, then SIGKILL after the grace period. |
| Pod evicted or preempted | `INFRASTRUCTURE` (`Evicted`), retryable. |
| Cancel or lease revoked | Pod deleted with the grace period (SIGTERM, grace, SIGKILL), then the Job is deleted. |
| Agent **container** restarts | The WAL (an emptyDir) survives the restart. A Job that finished meanwhile is harvested and reported for real. A Job still running is killed, because this session cannot renew its lease and a retry is coming. Its STARTING entry is then replayed as INDETERMINATE. |
| Agent **pod** deleted | The garbage collector deletes its Jobs. The WAL is lost, so those attempts become LOST through lease expiry and are retried. |

### 13.4 Inspecting

```bash
kubectl -n nexus-exec get jobs,pods -l app.kubernetes.io/managed-by=nexus-agent
kubectl -n nexus-exec get jobs -l nexus.dev/run=<run_id>           # everything for one run
kubectl -n nexus-exec describe job nexus-<attempt_id>              # nexus.dev/enforced-limits annotation
kubectl -n nexus-exec logs job/nexus-<attempt_id> -c task          # the task's log
```

Attempt ids that are not valid DNS labels are hashed into the Job name. The
exact id is always in the `nexus.dev/attempt-id` annotation.

### 13.5 Limits you will hit

See CONTRACT-FINDINGS F96 for detail. In short:

- Inputs are limited to about 1 MiB in total, because they are staged through a
  Secret.
- Outputs are limited to 4 MiB in total, because they leave the pod through the
  collector's log.
- `runtime.stdout_output` is rejected, because the pod log API cannot separate
  stdout from stderr.
- The pod status sent through ReportProgress ("pod Pending: Unschedulable:
  ...") is stored per attempt and shown in the console as the **executor
  status**. That is separate from the scheduler's own placement reason. It is
  the latest status only, capped at 512 bytes. The run history keeps at most
  50 `AttemptProgress` events per attempt, rate-limited, so read the attempt
  card for what the pod is doing now.

### 13.6 Running the executor's cluster tests

```bash
kubectl config current-context        # MUST be a local cluster (docker-desktop, kind-*, minikube, ...)
NEXUS_K8S_CONFORMANCE=1 go test ./agent/internal/executor/ ./agent/internal/worker/ \
  -run 'TestConformance/.*/kubernetes|TestK8s' -v
```

The tests refuse to run unless the context name is recognisably local **and**
its API server is on a loopback address. They create namespace
`nexus-k8s-exec-test`, with Pod Security "restricted" enforced, and delete it
afterwards. Set `NEXUS_K8S_KEEP_NAMESPACE=1` to keep it for debugging.

Check that the subtests actually ran (F89). Each `…/kubernetes` line should take
seconds, not `0.00s`. A `-run` pattern that does not match the three-level
subtest names skips them silently, and the run still passes.

## 14. Dynamic expansion, memoization and worker quarantine

### 14.1 Dynamic expansion (§5.4)

- **Operator ceiling on `max_fanout`.** Submit-time validation rejects a template whose
  `max_fanout` exceeds 10,000 (`dag.DefaultMaxFanoutCeiling`).
- **Fan-in size limit.** `NEXUS_DAG_MAX_FANIN_BYTES` on the dag-engine (default `16777216`,
  16 MiB; must be within 65536..268435456 or the service refuses to start) bounds the JSON
  array a fan-in node receives. Over it, the consumer fails with `fan_in_too_large`:
  `SELECT * FROM fan_in_refusals WHERE run_id = '<run>'`, the `FanInRejected` event, or the
  run page. Prefer fixing the workflow over raising the limit: have children write large
  results as blob outputs (passed as references, a few hundred bytes each), emit smaller
  summaries, or lower `max_fanout`. Declaring `max_bytes` on the template's output makes an
  impossible fan-in fail at submit instead of after every child ran.
- **Why did a map node fail?** `SELECT outcome, reason, detail, item_count, max_fanout FROM
  task_expansions WHERE run_id = '<run>'`, or the run page (the node footer reads "expansion
  refused: …"). Reasons: `max_fanout_exceeded`, `collection_missing`, `collection_not_array`,
  `collection_not_inline`.
- The console folds a template's children into the template; unfold is offered up to 50.

### 14.2 Memoization (§6.4)

- **Invalidate** when a memoized task's results must not be reused (bad input data, a
  handler bug fixed without a version bump):

  ```sh
  curl -X POST "$NEXUS_ENDPOINT/v1/namespaces/$NS/cache/invalidations" \
       -H "Authorization: Bearer $NEXUS_API_KEY" \
       -d '{"task_ref":"fanout.process_shard","reason":"reprocess after bad listing"}'
  ```

  Add `"task_version": N` to scope to one version, or pass `"cache_key"` for one entry.
  Needs `workflows.cache.invalidate`. It removes nothing; later runs simply execute.
  Bumping the task's `version` is the author-side equivalent.
- **Which nodes were cached?** `SELECT node_id, cache_hit_of FROM task_attempts WHERE run_id =
  '<run>' AND cache_hit_of IS NOT NULL`; `CacheHit` events name the source attempt.

### 14.3 Worker quarantine (§5.5)

Detector settings (resource-manager environment), with defaults:

| Variable | Default | Meaning |
|---|---|---|
| `NEXUS_QUARANTINE_ENABLED` | `true` | Detection on/off. Expiry of existing quarantines runs either way. |
| `NEXUS_QUARANTINE_INTERVAL` | `30s` | Pass period. |
| `NEXUS_QUARANTINE_WINDOW` | `10m` | Evidence window. |
| `NEXUS_QUARANTINE_MIN_FAILURES` | `3` | Infrastructure-class failures needed. |
| `NEXUS_QUARANTINE_MIN_DISTINCT_TASKS` | `2` | Distinct `task_ref`s they must span (worker-, not task-correlated). |
| `NEXUS_QUARANTINE_MIN_FAILURE_RATIO` | `0.5` | Share of the worker's terminal attempts. |
| `NEXUS_QUARANTINE_BASE_DURATION` | `15m` | First automatic quarantine; doubles per relapse within 24h. |
| `NEXUS_QUARANTINE_MAX_DURATION` | `24h` | Cap. |
| `NEXUS_QUARANTINE_MAX_FLEET_FRACTION` | `0.5` | Most of a namespace that may be quarantined at once. |

- **A burst of "not quarantined: namespace at its quarantine cap" errors** means many workers
  fail the same way at once -- almost always a shared dependency (result store, registry,
  network), not bad hosts. Fix the dependency; do not raise the cap.
- **Manual:** `POST …/workers/{id}/quarantine {"reason": "...", "duration_ms": 0}` (0 =
  until released) and `POST …/workers/{id}/release`; permission `workers.quarantine`
  (Infrastructure Operator, Administrator). Both are audited.
- **History:** `GET …/workers/{id}/quarantines`, the worker page, or `worker_quarantines`.
- Quarantine follows the host: restarting the agent does not clear it. Release it instead.
- **Rolling back 0021** (break-glass): release every quarantined worker first; with the
  episode table gone nothing expires a quarantine.

## 15. Dispatch at large backlogs: the windowed pull claim

A polling worker's claim (`ClaimForWorker`) used to evaluate and sort the
queue's whole PENDING backlog on every poll, so its cost grew with the backlog
(ADR-0018). With migration 0023 it reads a small window of candidates off an
index instead, and only joins and filters those.

### 15.1 Deploy order

1. Apply migration 0023. It builds two indexes `CONCURRENTLY`, so it takes no
   table lock and can run under load; on a large `task_attempts` expect the
   build to take a while. It is safe to re-run.
2. Restart (or roll) the schedulers. **A scheduler decides at startup** whether
   the rank function exists; one started before 0023 keeps scanning the backlog
   until it restarts. That is the safe direction: it never fails a claim.

To roll back, restart the schedulers with `--claim-window=0` (or roll them back)
**before** running `0023_dispatch_rank_index.down.sql`.

### 15.2 Reading the startup log

The scheduler's `scheduler starting` line carries `claim_window`,
`claim_window_enabled` and `claim_window_indexed`. Two WARN lines mean dispatch
is slower than it could be:

| WARN | Meaning | Fix |
|---|---|---|
| `pull claims scan the whole PENDING backlog` | 0023 is not applied, or `--claim-window=0` | Apply 0023 and restart |
| `pull claims are windowed but unindexed` | `--aging-rate` is not `1` | The index is built for rate 1. Other rates still claim in the correct order but sort the backlog. |

### 15.3 Tuning

| Flag | Default | Effect |
|---|---|---|
| `--claim-window` | 64 | Candidates read per window. `0` disables windowing. |
| `--claim-window-pages` | 4 | Windows one claim may read before falling back to a full scan. |

The fallback exists for a worker that can run nothing near the top of its queue,
such as a CPU worker behind thousands of GPU tasks. Paging on through the whole
backlog a window at a time would be slower than one scan, so after the last page
the claim scans once. Such a worker pays the old cost; every other claim pays
for a window.

### 15.4 Checking the index is used

```sql
SELECT idx_scan FROM pg_stat_user_indexes
 WHERE indexrelname = 'idx_attempts_dispatch_rank';
```

This should rise with every poll. If it stays flat while workers poll, the
scheduler is not windowing; check the startup log (§15.2).

---

## 16. Gateway rate limiting

The api-gateway is the only ingress, so it carries the only request limiter
(`services/api-gateway/rest/ratelimit.go`, finding F99). It is **off unless
`NEXUS_RATE_LIMIT_RPS` is set**. The startup line `rate limiting enabled (in
memory, per replica)` lists the effective numbers for all three buckets.

### 16.1 What is limited, in order

| # | Bucket | Keyed by | When it is spent | 429 message |
|---|---|---|---|---|
| 1 | Per address | Client address; IPv6 folded to its /64 | Every request, before authentication | `too many requests from this address` |
| 2 | Auth failures | Client address; IPv6 folded to its /64 | Once per **rejected** credential or login password. When it is empty, the address may not attempt authentication until it refills | `too many failed authentication attempts from this address` |
| 3 | Per credential | Kind and database ID of the verified api key, session or scoped token | Every request, after successful authentication | `too many requests for this credential` |

The pre-authentication key is always the address. Nothing in the request selects
it, so a random `Authorization` header per request gets no fresh bucket (that was
the F99 bypass). A request with **no** credential is not counted as a failure; it
gets its 401 as before. The **same** rejected value repeated from one address,
such as a console tab polling with a revoked session or a job with a rotated-out
key, is charged once per 5 minutes rather than once per request. Distinct guesses
are each charged.

When bucket 2 is empty the whole address is refused authentication, including
with a valid key. That is deliberate: otherwise an attacker interleaves a known
key with guesses. It is a throttle, not a lockout. One attempt comes back each
refill interval (6 s by default).

The email-link routes (`/v1/auth/email/*`, `/v1/auth/password/*`) have their own
per-address limit as well (§10, docs/MULTITENANCY.md), keyed the same way.

### 16.2 Which address

The peer address of the TCP connection, with one exception. When the peer is
loopback or a private address (RFC 1918, RFC 4193 `fc00::/7`), such as the
console's nginx, the gateway uses the `X-Real-IP` that proxy set.
`X-Forwarded-For` is never used for limiting.

- **Behind a load balancer or ingress on a private network**, that proxy MUST
  overwrite `X-Real-IP` with the client address. If it does not, every client
  shares the proxy's bucket. A client that can reach the gateway directly from a
  private network can choose its own `X-Real-IP`. Do not expose the gateway's
  port to an untrusted private network.
- **Behind a public-address proxy**, the gateway sees only the proxy's address.
  Every client then shares one address bucket. Raise
  `NEXUS_RATE_LIMIT_IP_RPS`/`_BURST` or move the proxy onto a private network.

### 16.3 Settings

| Variable | Default | Meaning |
|---|---|---|
| `NEXUS_RATE_LIMIT_RPS` | unset (off) | Per-credential sustained rate. **Also the switch for all three buckets.** Unchanged meaning since F76. |
| `NEXUS_RATE_LIMIT_BURST` | = RPS | Per-credential burst. Unchanged meaning. |
| `NEXUS_RATE_LIMIT_IP_RPS` | 4 × RPS | Per-address sustained rate. Setting it without `_IP_BURST` sets the burst equal to the rate. |
| `NEXUS_RATE_LIMIT_IP_BURST` | 4 × BURST | Per-address burst. |
| `NEXUS_RATE_LIMIT_AUTH_FAIL_RPS` | 0.1667 (1 per 6 s) | Auth-failure refill rate per address. |
| `NEXUS_RATE_LIMIT_AUTH_FAIL_BURST` | 10 | Failures an address may have before it is throttled. |
| `NEXUS_RATE_LIMIT_MAX_KEYS` | 100000 | Most buckets each limiter holds. |

The address default is a multiple of the credential allowance so that several
principals behind one NAT or CI egress are not capped at one credential's rate.
Guessing is bounded by the auth-failure bucket, not by this one.

An unparseable value is ignored and the default is used, the same as every other
gateway float setting.

### 16.4 Responses

A refusal is `429` with body `{"error":"rate_limited","message":"…"}` and:

- `Retry-After`: whole seconds until one request is admitted, rounded up, at least 1.
- `RateLimit-Limit`: the refusing bucket's burst.
- `RateLimit-Remaining`: `0`.
- `RateLimit-Reset`: same as `Retry-After`.

The headers are sent on 429 only. The Python SDK retries 429 with backoff. The
CLI does not retry; it prints a "retry with backoff" hint.

### 16.5 Memory

Each limiter is a map capped at `NEXUS_RATE_LIMIT_MAX_KEYS`, roughly 20 MB at the
default. There are four maps: the three buckets and the repeat-failure record.
Idle buckets are dropped after 10 minutes (15 minutes for auth failures), and
never before they would have refilled, so eviction never forgives a throttled
key early. At the cap the least recently used bucket is dropped. An actively
throttled client stays recently used, so a spray of new addresses cannot flush
it. Keys are addresses and credential **IDs**. No presented secret, header or
password is stored. The repeat-failure record holds a keyed MAC whose key is
random per process.

### 16.6 Limits of this design — read before sizing

- **Per replica, in memory.** Each gateway replica has its own buckets. With N
  replicas a client can get up to N × each rate, depending on how the load
  balancer spreads its connections. A restart or rollout forgets every bucket.
  Size the numbers per replica. For a hard global quota you need a shared store
  (Redis or similar) or an edge limiter in front of the gateway. Neither is
  provided.
- **Not a DDoS defence.** A flood from many addresses is bounded per address
  only. Volumetric protection belongs at the network edge.
- **NAT neighbours share buckets 1 and 2.** One misbehaving client behind an
  office NAT can throttle its neighbours' sign-in for a few refill intervals.
  Tune `_AUTH_FAIL_*` if that is your topology.
- **No limiter-specific metrics.** Rejections are visible only as
  `nexus_http_requests_total{code="429"}` on the gateway's ops listener (§17),
  labelled by route; which bucket refused a request (credential, address or
  auth failures) and how many keys each limiter holds are neither exported nor
  logged. The refusing bucket's burst is in the `RateLimit-Limit` header (§16.4).

---

## 17. Metrics

Every Nexus process serves Prometheus metrics at `GET /metrics` in the text
format (0.0.4, gzip on request). The exposition is client_golang's (ADR-0019).
A scrape answers from memory and touches no dependency, so `/metrics` keeps
working during the database or broker outage you are trying to see.

### 17.1 Where each process serves `/metrics`

`/metrics` is **never** on a public port. The gateway's REST port is the
internet-facing ingress, so the gateway has a dedicated operations listener.
Every other service serves it on its existing health listener, which is
already internal-only.

| Process | Listener | Setting (default) | Compose (loopback) | Helm / k8s |
|---|---|---|---|---|
| api-gateway | ops listener: `/metrics`, `/healthz`, `/readyz` | `NEXUS_METRICS_ADDR` (`:8088`; `off` disables) | `127.0.0.1:19100` | Service port `metrics` (9464) → 8088 |
| workflow-manager | health | `NEXUS_WFM_HEALTH_ADDR` (`:8081`) | `127.0.0.1:19101` | `metrics` (9464) → 8080 |
| dag-engine | health | `NEXUS_HTTP_ADDR` (`:8082`) | `127.0.0.1:19102` | `metrics` → 8080 |
| scheduler (one per queue) | health | `NEXUS_HTTP_ADDR` (`:8081`) | `127.0.0.1:19110-19119` (scalable); scheduler-gpu `19104` | `metrics` → 8080 |
| resource-manager | health | `RM_HTTP_ADDR` (`:8082`) | `127.0.0.1:19105` | `metrics` → 8080 |
| event-service | its internal HTTP API port | `HTTP_ADDR` (`:8085`) | `127.0.0.1:19106` | `metrics` → 8080 |
| cluster-controller | health | `CONTROLLER_HTTP_ADDR` (`:8090`) | — | pod annotation, port 8090 |
| agent | own listener: `/metrics`, `/healthz` | `NEXUS_METRICS_ADDR` / `-metrics-addr` (`127.0.0.1:8089`; `off` disables) | `127.0.0.1:19120-19122` | pod port `metrics` 8089 (PodMonitor) |
| artifact-gc (batch job) | optional, only while the pass runs | `-metrics-addr` / `ARTIFACT_GC_METRICS_ADDR` (off) | — | — |

The agent binds loopback by default, so an agent on a laptop or shared host
does not publish metrics to the LAN. Deployments that scrape it set
`NEXUS_METRICS_ADDR=:8089`, as compose and Helm do. If the agent cannot bind
its port, it logs the error and keeps running work. If the gateway cannot bind,
it fails to start and names the address in the error.

**artifact-gc is a batch job.** A pull scrape usually misses a process that
lives for seconds, so point it at a Pushgateway with
`artifact-gc -metrics-pushgateway http://pushgateway:9091` (or
`ARTIFACT_GC_PUSHGATEWAY_URL`). When the pass ends, whether it succeeded or
failed, the job PUTs its `nexus_artifact_gc_*` families under
`job="artifact-gc"`. It does not push Go runtime or process families. Alert on
`time() - nexus_artifact_gc_last_success_timestamp_seconds`.

### 17.2 Scrape configuration

- **Helm, annotation discovery** (default): pods carry `prometheus.io/scrape`,
  `prometheus.io/port` and `prometheus.io/path`. Set
  `metrics.podAnnotations=false` to turn them off.
- **Helm, prometheus-operator**: `metrics.serviceMonitor.enabled=true` renders
  a ServiceMonitor over every Service port named `metrics`, plus a PodMonitor
  for the agents. Set `metrics.podMonitor.enabled` to control the PodMonitor
  separately. Add `metrics.serviceMonitor.labels.release=<your stack>` to match
  the operator's selector. The chart does not install the CRDs; they must
  already exist.
- **deploy/k8s**: the standalone agent Deployment and the cluster-controller
  pods carry the same annotations.
- **Compose**: every metrics listener is published on `127.0.0.1` only. A local
  Prometheus can scrape them:

  ```yaml
  scrape_configs:
    - job_name: nexus
      static_configs:
        - targets: ['host.docker.internal:19100', 'host.docker.internal:19101',
                    'host.docker.internal:19102', 'host.docker.internal:19104',
                    'host.docker.internal:19105', 'host.docker.internal:19106',
                    'host.docker.internal:19110', 'host.docker.internal:19120',
                    'host.docker.internal:19121', 'host.docker.internal:19122']
  ```

To check any target by hand, run `curl -s localhost:19100/metrics | grep nexus_build_info`.

### 17.3 Cardinality rules

- No run, attempt, node, worker, key or host id is ever a label. Those belong
  in logs and traces.
- HTTP requests are labelled by the **ServeMux route template**
  (`/v1/namespaces/{ns}/runs/{id}`), never by the raw path. Every unmatched
  path is `route="unmatched"`, and every non-standard method is
  `method="OTHER"`.
- gRPC labels take only the methods registered on the server.
- The backlog gauges report at most 50 queues by name. Any further queues are
  summed into `queue="_other"`.
- The cluster-controller's `pool` label is the binding name an operator
  configured.

If a caller passes labels that do not match a family's declared labels, the
package normalizes them and logs `metrics: label set does not match` once per
family. Treat that log line as a bug.

### 17.4 Catalogue

On top of the metrics below, every process exposes:

- `go_*`: the Go runtime.
- `process_*`: CPU, RSS, file descriptors and start time, on Linux.
- `nexus_build_info{service,version,revision,go_version} 1`.

Services with a Postgres pool also expose `nexus_db_pool_*`. In the T column,
C is a counter, G a gauge and H a histogram.

| Metric | T | Labels | Service | Meaning / alert on |
|---|---|---|---|---|
| `nexus_build_info` | G | service, version, revision, go_version | all | Always 1. Join on it to see which build a series came from |
| `nexus_db_pool_max_conns`, `_total_conns`, `_acquired_conns`, `_idle_conns` | G | — | all DB services | Pool size and use. `acquired == max` sustained: pool exhausted |
| `nexus_db_pool_acquires_total` | C | — | all DB services | Successful acquisitions |
| `nexus_db_pool_acquire_duration_seconds_total` | C | — | all DB services | Cumulative wait; `rate(this)/rate(acquires)` is mean wait |
| `nexus_db_pool_empty_acquires_total` | C | — | all DB services | Waits on an empty pool. Rising: raise the pool size or find the slow query |
| `nexus_db_pool_canceled_acquires_total` | C | — | all DB services | Callers gave up waiting |
| `nexus_http_requests_total` | C | method, route, code | api-gateway, event-service | REST rate and errors (`code=~"5.."`) |
| `nexus_http_request_duration_seconds` | H | method, route, code | api-gateway, event-service | REST latency, 1 ms–60 s. SSE tails are observed when they end |
| `nexus_http_response_size_bytes` | H | method, route | api-gateway, event-service | Response size |
| `nexus_http_requests_in_flight` | G | — | api-gateway, event-service | Concurrent requests, open SSE tails included |
| `grpc_server_started_total` | C | grpc_type, grpc_service, grpc_method | api-gateway (worker API), workflow-manager, dag-engine, scheduler, resource-manager | RPCs started |
| `grpc_server_handled_total` | C | grpc_type, grpc_service, grpc_method, grpc_code | same | RPCs finished, by status. `Unauthenticated` on the gateway: bad worker keys |
| `grpc_server_handling_seconds` | H | grpc_type, grpc_service, grpc_method | same | RPC latency, 1 ms–120 s. `PollTask` is a long poll by design |
| `nexus_scheduler_claims_total` | C | queue, path (poll/push), outcome (dispatched/empty/blocked/race_lost/error) | scheduler | Claims per second. `outcome="error"` > 0: dispatch is failing |
| `nexus_scheduler_claim_duration_seconds` | H | queue, path | scheduler | Claim transaction latency. p99 > 250 ms: see §15 and ADR-0018 |
| `nexus_scheduler_attempts_dispatched_total` | C | queue, path | scheduler | Attempts leased to workers |
| `nexus_scheduler_attempts_lost_total` | C | queue, cause (lease_expired/worker_dead), retry | scheduler | LOST attempts. Rising: worker or network trouble |
| `nexus_scheduler_lease_expiries_total` | C | queue | scheduler | Leases the reconciler expired |
| `nexus_scheduler_workers_declared_dead_total` | C | queue | scheduler | Workers the liveness sweep declared DEAD |
| `nexus_scheduler_timers_fired_total` | C | queue, kind, result | scheduler | retry_backoff and schedule_to_start timers handled |
| `nexus_scheduler_loop_errors_total` | C | queue, loop | scheduler | Background loop failures, which also fail `/readyz` |
| `nexus_scheduler_pending_attempts` | G | queue | scheduler | PENDING backlog across the fleet, sampled every `NEXUS_BACKLOG_SAMPLE_INTERVAL` (15 s). Every replica reports every queue: aggregate with `max by (queue)` |
| `nexus_scheduler_oldest_pending_age_seconds` | G | queue | scheduler | Queue age, the queue SLO signal. Also exposes a queue that has no scheduler replica |
| `nexus_dag_operations_total` | C | op (materialize/reconcile), result | dag-engine | Engine operations |
| `nexus_dag_operation_duration_seconds` | H | op | dag-engine | Operation latency |
| `nexus_dag_node_transitions_total` | C | from, to | dag-engine | Node state transitions committed (CAS won) |
| `nexus_dag_attempts_created_total` | C | kind (new/cache_hit) | dag-engine | Attempts created, with memo hits counted separately |
| `nexus_dag_node_cas_misses_total` | C | — | dag-engine | Lost CAS races. Sustained: replicas are fighting over runs |
| `nexus_dag_map_expansions_total` | C | — | dag-engine | Map templates expanded |
| `nexus_dag_fanin_refused_total` | C | — | dag-engine | Fan-ins refused for exceeding `NEXUS_DAG_MAX_FANIN_BYTES` |
| `nexus_dag_run_state_published_total` | C | state, result | dag-engine | Run state publishes. `result="error"` on a terminal state: a run may be stranded |
| `nexus_rm_reservations_total` | C | result (granted/refused/not_found/error) | resource-manager | Reserve calls |
| `nexus_rm_reserve_duration_seconds` | H | result | resource-manager | Reserve latency |
| `nexus_rm_releases_total` | C | result (released/absent/error) | resource-manager | Releases. `absent` is expected duplicate traffic |
| `nexus_rm_leaked_reservations_reclaimed_total` | C | source (heartbeat/reconcile/dead_worker) | resource-manager | Capacity reclaimed as leaks. Steady non-zero `heartbeat`: the ledger is drifting |
| `nexus_rm_heartbeats_total` | C | result (ok/unknown_worker/error) | resource-manager | Heartbeats. `unknown_worker` forces a re-registration |
| `nexus_rm_workers_declared_dead_total` | C | — | resource-manager | Workers declared DEAD by the liveness sweep |
| `nexus_rm_quarantine_changes_total` | C | action (quarantined/expired) | resource-manager | Automatic quarantines (§14) |
| `nexus_controller_reconcile_cycles_total` | C | result | cluster-controller | Reconcile cycles run by the leader |
| `nexus_controller_reconcile_duration_seconds` | H | — | cluster-controller | Cycle duration |
| `nexus_controller_pool_reconcile_errors_total` | C | pool | cluster-controller | Failed pool reconciles |
| `nexus_controller_scale_actions_total` | C | pool, action, mode, applied | cluster-controller | Scale decisions: scale_out, scale_in, drain, hold, refused |
| `nexus_controller_pool_replicas` | G | pool, kind (current/desired) | cluster-controller | Replicas observed and planned |
| `nexus_controller_last_success_timestamp_seconds` | G | — | cluster-controller | Alert on the leader when `time() - this > 4 * interval` |
| `nexus_controller_leader` | G | — | cluster-controller | 1 on the leader. Alert if `sum() != 1` for 5 minutes: no leader, or two |
| `nexus_agent_tasks_running` | G | executor | agent | Attempts executing |
| `nexus_agent_tasks_started_total` | C | executor | agent | Attempts started |
| `nexus_agent_tasks_completed_total` | C | executor, outcome, error_class | agent | Attempts reported. `error_class="infrastructure"` rising on one host: a quarantine candidate |
| `nexus_agent_task_duration_seconds` | H | executor, outcome | agent | Task wall time, 1 s–1 day |
| `nexus_agent_heartbeats_total` | C | result | agent | Heartbeats sent |
| `nexus_agent_heartbeat_duration_seconds` | H | — | agent | Heartbeat round trip |
| `nexus_agent_heartbeat_consecutive_failures` | G | — | agent | Current failure streak. At 3 or more, expect the worker to be declared DEAD |
| `nexus_agent_last_heartbeat_success_timestamp_seconds` | G | — | agent | Last acknowledged beat |
| `nexus_agent_result_store_puts_total` | C | result (inline/uploaded/deduplicated/error) | agent | Outputs stored. `error` > 0: object store trouble |
| `nexus_agent_result_store_upload_bytes_total` | C | result | agent | Output bytes. `uploaded` counts bytes actually transferred |
| `nexus_agent_result_store_upload_duration_seconds` | H | result | agent | Upload latency. Inline outputs are not observed |
| `nexus_agent_result_store_upload_size_bytes` | H | — | agent | Uploaded output sizes |
| `nexus_agent_sessions_total` | C | — | agent | Registrations. More than 1 per process means the control plane forced a re-registration |
| `nexus_agent_advertised_capacity` | G | resource | agent | Capacity advertised for scheduling, which the runtime does not necessarily enforce |
| `nexus_agent_result_report_failures_total` | C | — | agent | Reports not delivered. The WAL retains them |
| `nexus_artifact_gc_last_run_objects` | G | disposition | artifact-gc | Last sweep: scanned, live_refs, reachable, too_young, orphan, deleted, delete_error |
| `nexus_artifact_gc_last_run_orphan_bytes` | G | — | artifact-gc | Reclaimable bytes found |
| `nexus_artifact_gc_last_run_duration_seconds` | G | — | artifact-gc | Pass duration |
| `nexus_artifact_gc_last_success_timestamp_seconds`, `_last_failure_timestamp_seconds` | G | — | artifact-gc | Batch-job liveness, via the Pushgateway |
| `nexus_artifact_gc_dry_run` | G | — | artifact-gc | 1 when nothing was deleted |
| `nexus_artifact_gc_last_check_refs` | G | kind | artifact-gc | `-check-refs` result. `missing_keys` > 0 is data loss |
| `nexus_timer_*`, `nexus_timers_fired_total`, `nexus_stalled_runs_recovered_total` | — | — | workflow-manager | Durable timers. Unchanged by this work |
| `nexus_outbox_*` | — | — | event-service | Outbox relay (§11.3) |
| `nexus_tls_cert_not_after_timestamp_seconds`, `nexus_tls_cert_expiry_seconds`, `nexus_tls_reload_total`, `nexus_tls_reload_stale` | — | role | services with TLS | Certificate expiry and reloads (§9.3) |

### 17.5 Verifying after an upgrade

1. `curl -s <target>/metrics | head` answers 200 with `Content-Type: text/plain; version=0.0.4`.
2. `nexus_build_info` shows the version you deployed on every target.
3. In Prometheus, `up{job=~".*nexus.*"} == 0` returns nothing.
4. The gateway's REST port answers 404 for `/metrics`. If it answers 200, the
   ops listener has been routed to the public port.

---

## 18. Operator retry of a failed run

`nexusctl run retry <run-id> [--node ID ...]`, the SDK's `retry_run(run_id,
nodes=[...])` and the console's "Retry failed tasks" / "Retry this task" all call
`POST /v1/namespaces/{ns}/runs/{id}/retry`. The semantics are in
[ARCHITECTURE §5.2, Operator retry](ARCHITECTURE.md); CONTRACT-FINDINGS F100
records why the endpoint used to ignore the node selection. Until F100 every
retry, whole-run or node-level, failed with "dag-engine.ResetFailedNodes is not
wired yet", so there is no earlier retry behaviour to stay compatible with.

### 18.1 Deploy order

1. Apply migration 0026. It adds one column, `task_nodes.retry_floor INT NOT NULL
   DEFAULT 0` — metadata-only on PostgreSQL 16, no rewrite, no new grant.
2. Roll the **dag-engine** and **scheduler**. The engine must ignore attempts at or
   below the floor; the scheduler must count the retry budget from it. An older
   engine reconciling a retried run derives the retried node from its superseded
   FAILED attempt and puts it straight back to FAILED — safe (nothing runs twice)
   but the retry is lost.
3. Roll the **workflow-manager**, then the **api-gateway**. A gateway ahead of the
   workflow-manager sends `node_ids`, which an older workflow-manager ignores; the
   gateway's own selection check still refuses bad selections, but a good one
   becomes a whole-run retry until step 3 finishes. A workflow-manager ahead of the
   dag-engine answers `501 retry_unavailable` (the engine has no `ResetNodes`), and
   changes nothing.

### 18.2 Reading the answers

| HTTP | `error` | Meaning | Action |
|---|---|---|---|
| 200 | — | Committed: the run is RUNNING | Watch the run |
| 400 | `invalid_body`, `invalid_nodes` | Malformed request | Fix the request |
| 404 | `not_found` | No such run in this namespace | Check the namespace |
| 409 | `nodes_not_retryable` | A named node is SUCCEEDED, SKIPPED, PENDING… | Name FAILED / UPSTREAM_FAILED / CANCELLED nodes |
| 409 | `retry_refused` | Run not FAILED/TIMED_OUT, or a node would fail again at once, or fail_fast leaves another failure standing | The message names the node to add |
| 409 | `retry_conflict` | A concurrent change won; nothing was retried | Re-read the run, retry again |
| 422 | `unknown_nodes` | The run has no node by that name | Fix the node id |
| 501 | `retry_unavailable` | dag-engine too old or not configured | Finish §18.1 |
| 503 | `retry_outcome_unknown` | The engine call timed out; the run was committed RUNNING and reconciled so it converges either way | Re-read the run before retrying again |

Every attempt, successful or not, writes a `run.retry` audit record with the
requested nodes. The run's history carries `RunRetriedFromFailed` with
`requested_nodes`, `selected` and `reset_nodes`.

### 18.3 Finding what a retry superseded

```sql
SELECT n.node_id, n.retry_floor, n.attempts_used, n.max_attempts,
       a.attempt_number, a.state, a.attempt_number <= n.retry_floor AS superseded
  FROM task_nodes n JOIN task_attempts a USING (run_id, node_id)
 WHERE n.run_id = $1 ORDER BY n.node_id, a.attempt_number;
```

Superseded attempts are history, not garbage: never delete them. They share the
retried attempt's idempotency key, which is how a handler recognises the retry of
work it may already have done.

### 18.4 Rolling back 0026

Roll the dag-engine and scheduler back first (they read the column), then run
`0026_node_retry_floor.down.sql` by hand (§6.4). Runs that were retried by node
lose the record of which attempts were superseded; an older engine derives those
nodes from their latest attempt, as it always did.

---

## 19. Durable task logs: retention and sizing

Design: [ADR-0020](adr/0020-durable-task-logs.md). Agents write each attempt's
stdout/stderr as NDJSON chunks directly to the result store under
`<namespace>/logs/<attempt_id>/`, index them through the lease-fenced
`ReportLogs` RPC (tables `attempt_logs`, `attempt_log_chunks`, migration 0025),
and on completion upload one compacted `log.ndjson`. The gateway serves
`GET /v1/namespaces/{ns}/attempts/{id}/logs` from the index plus range reads of
the store; `nexusctl logs <attempt> [-f] [--tail N]`, the SDK's `get_logs` /
`search_logs` and the console's log pane all use it.

### 19.1 Enabling it

Log shipping is ON when the gateway can read the result store. It reads the same
variables as the agent, which compose (`x-minio-env`) and Helm
(`nexus.resultStoreEnv`) already give it:

| Setting (api-gateway) | Default | Meaning |
|---|---|---|
| `NEXUS_S3_ACCESS_KEY_ID`, `NEXUS_S3_SECRET_ACCESS_KEY` | unset | READ credentials for log objects. Unset: the logs route answers 501 and agents are told not to ship. |
| `NEXUS_RESULT_STORE_ENDPOINT`, `_BUCKET`, `_REGION`, `NEXUS_S3_USE_TLS` | compose values | Where logs are read from; identical to where agents write. |
| `NEXUS_LOG_SHIPPING` | `true` | `false` turns the feature off fleet-wide (501 + no shipping). |
| `NEXUS_LOG_MAX_BYTES_PER_ATTEMPT` | 64 MiB | Per-attempt cap on shipped (encoded) bytes. Past it the head is kept, the last 2 MiB is kept, and the omission is written into the log and the index. |
| `NEXUS_LOG_CHUNK_BYTES` | 256 KiB | A chunk is sealed at this size (agents clamp to 4 MiB; the gateway refuses > 8 MiB). |
| `NEXUS_LOG_FLUSH_INTERVAL_MS` | 2000 | A non-empty chunk is sealed at least this often. This is the live-tail latency, and the per-attempt ReportLogs rate (see §19.3). Agents slow to 5× this after 300 chunks. |

The policy is sent to agents in `RegisterResponse.log_shipping`, so a change
takes effect as agents re-register (restart or `REREGISTER`), not per host.

The startup line `durable task logs enabled` (or the WARN explaining why they
are not) is the first thing to check. Agents log `durable task logs enabled`
at registration and `attempt log shipped` (lines, bytes, chunks, finalized,
truncated/dropped counts) per attempt.

**Credential scoping (recommended for production).** The gateway needs only
`s3:GetObject` on `arn:aws:s3:::<bucket>/*/logs/*`. Workers need `s3:PutObject`
on `<bucket>/<their namespace>/logs/*` in addition to their existing output
prefix. The agent refuses to write a log key outside its own namespace's
`logs/` prefix, and the gateway refuses to index one, so a mis-scoped
credential cannot be used through Nexus to read or index another tenant's
objects -- but scoping the credential is what bounds a compromised host.

**Upgrade order.** 0025 first (additive), then gateways, then agents. An old
agent under a new gateway ships nothing (the API says `none` with an
explanation); a new agent under an old gateway is not told to ship and ships
nothing. No mixed-version combination writes objects that cannot be read.
Rolling back: roll gateways back first; `0025_attempt_logs.down.sql` drops the
index only, and the objects become orphans for artifact-gc.

### 19.2 Retention

Log objects are reference-counted by artifact-gc exactly like outputs (§7.7):

- a log's objects are live while its `task_attempts` row exists, so **deleting
  a run** (which cascades to its attempts and log index) makes its logs orphans;
- after compaction, the chunk objects are orphans and only `log.ndjson` is
  live -- compaction's space is reclaimed by the sweep, not inline;
- chunks a fenced (stale) attempt uploaded but could not index, and uploads
  whose `ReportLogs` never landed, are orphans;
- all orphans wait out the grace period (`-grace`, default 24h).

Time-based retention is opt-in and applies to logs only:

```sh
# Report what a 30-day log retention would collect (dry run, the default):
artifact-gc -log-retention 720h
# Collect it:
artifact-gc -log-retention 720h -delete -i-understand-this-deletes-data
```

A log whose last write (finalization, or last indexed chunk) is older than the
window drops out of the live set; its index rows remain, so the API reports it
as `expired` with an explanation rather than as an empty log. Outputs are never
subject to time retention: they are inputs to downstream tasks and retries.
The summary line `objects examined N (M task-log objects)` shows how much of a
pass was logs. `-check-refs` (dangling references) does not yet cover the log
index; a log whose objects vanished out of band shows up in the API as gap
lines or `expired`.

### 19.3 Sizing

Per attempt that is producing output (defaults):

| Resource | Cost |
|---|---|
| Object store PUTs | 1 per chunk: ≤ 1 per flush interval (0.5/s), plus 1 compaction PUT at the end |
| Gateway worker-port RPCs | 1 `ReportLogs` per upload batch (≤ 0.5/s) plus 1 final. At 1,000 concurrently *logging* attempts that is ≈ 500 small RPS and ≈ 500 single-row inserts/s into `attempt_log_chunks`; raise `NEXUS_LOG_FLUSH_INTERVAL_MS` for larger fleets. Silent attempts cost nothing. |
| Postgres | 1 row per chunk (≈ 200 B plus index) and 1 per attempt. A 64 MiB log ≈ 256 rows; a task printing a line a second for a day ≈ 9,000 rows. Rows go with the run. |
| Object storage | ≈ 1.2–1.6× the raw output (the NDJSON envelope is ~50 B/line), capped at 64 MiB + 2 MiB tail per attempt. Until the sweep collects compacted-away chunks, a compacted log occupies twice its size. |
| Agent memory | ≤ 8 MiB unshipped buffer + 2 MiB tail per attempt, independent of output volume. Past the buffer, lines are dropped and counted, never blocking the task. |
| Agent disk | A spool (for compaction) up to the per-attempt cap, under `<WAL dir>/log-spool`, deleted at the end of each attempt. |
| Gateway per request | ≤ 32 MiB of object reads per page; search scans ≤ 64 MiB by default (256 MiB max, `scan_bytes`), ≤ 1,000 matches and 15 s; each bound is reported (`stopped_by`) and the scan is resumable via `next_line`. |
| Result latency | Finalization (drain + compaction + final report) precedes `ReportResult` and is bounded at 30 s; normally one PUT and one RPC. If it cannot finish, the result is reported anyway and the log is `partial`. |

### 19.4 Reading the states

| State | Meaning | Action |
|---|---|---|
| `none` | Nothing indexed. Not started, no output yet, or agent/gateway predates shipping, or shipping disabled. | Check the gateway startup line and the agent's registration line. |
| `live` | Shipping while the attempt runs. | -- |
| `complete` | Finalized by the agent. | -- |
| `partial` | The attempt ended without a final report: agent crash, lost lease, or object store unreachable during finalization. Everything indexed is readable. | Agent log for `log finalization ran out of time` / `log shipping stopped`. |
| `expired` | Index rows exist, objects are gone (retention or out-of-band deletion). | Expected after `-log-retention`; otherwise investigate like a dangling ref (§7.8). |

Lines on stream `nexus` are the platform's: `truncated` / `truncation_summary`
(cap), `dropped` (backpressure), `gap` (a range the reader could not read, with
the reason). They are never silently omitted.

### 19.5 Known limits

- **Redaction has no input yet.** The agent redacts known secret values from
  every line before it leaves the host, but nothing in the platform resolves
  `SecretRef` values on the worker today, so there is nothing to redact. The
  resolver, when added, must feed `Runner.SecretValues`. Matching is per line.
- Kubernetes pod logs interleave stdout and stderr; those lines are tagged
  `combined`.
- Live tail is poll-based (≈ flush interval + 1 s), not a pipe.
- Timestamps are the agent's clock (informational; ordering is by line number).
