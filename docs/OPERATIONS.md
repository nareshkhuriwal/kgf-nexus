# Nexus — Operations: backup, restore, upgrade

**Status:** Phase 1. Covers the procedures that exist and have been executed.
**Audience:** whoever is on call.
**Scope:** Postgres backup and restore, the artifact (MinIO result store) backup
and restore and the ordering between the two stores, the restore drills and their
measured results, point-in-time recovery with pgBackRest and its timed drills
(§4), what these procedures do *not* cover, the RPO/RTO each approach actually
delivers, the schema-migration procedure, TLS certificate rotation (§9), email delivery (§10), the NATS event bus and its authorization (§11), the cluster controller (§12), the Kubernetes executor (§13), dynamic expansion, memoization and quarantine (§14), the windowed pull claim at large backlogs (§15), gateway rate limiting (§16), metrics: endpoints, scrape configuration and the metrics catalogue (§17), operator retry of a failed run (§18), durable task logs: retention and sizing (§19), installing with Helm: values reference and production checklist (§20), namespace quotas and usage metering (§21), third-party images and toolchains: what is pinned and why (§22), cost management, which moved to KGF billing (§23), running Nexus inside KGF Cloud: context authentication, tenancy mapping and service accounts (§24), task secrets, the audit trail and air-gapped installation (§25), tracing and the result-store backends (§26), notifications, webhook triggers, sensors and asset lineage (§27), fractional GPUs, spot capacity, model caches and multi-cluster placement (§28), and AI workloads: usage units, shared rate limits and checkpoints (§29).

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

*Update (2026-09-25):* all three are now exercised by `restore-drill.sh --rto`
and `pitr-drill.sh` — the services are started against the restored database,
in-flight leases/reservations held by vanished workers are reconciled, and the
phases are timed on datasets of 54k and 420k attempts. Results and their limits:
§4.6.

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

## 4. RPO, RTO and point-in-time recovery

Stated plainly, because the honest numbers are more useful than aspirational
ones. There are now two recovery mechanisms, and which one you are relying on
changes both numbers.

| | Logical: `backup-all.sh` (§2, §7) | Continuous archiving: pgBackRest (§4.2) | Managed Postgres |
|---|---|---|---|
| What is captured | `pg_dump` + result-bucket mirror, on a schedule | every WAL segment + periodic physical base backups | provider snapshots + WAL |
| Restore to a chosen second | no — you get the last dump | yes: timestamp, LSN, or latest | yes, via the provider |
| RPO | the dump interval | ≤ ~1 minute on a quiet database (§4.6) | provider-defined (typically ≤ 5 min) |
| Roles and grants | not carried (`--no-owner --no-privileges`) | carried (it is the whole cluster) | carried |
| Where it applies | anywhere | the in-cluster/compose Postgres | Azure Flexible Server, RDS/Aurora, Cloud SQL |

**Managed Postgres must use the provider's PITR, not this.** A managed server
does not expose its data directory or `archive_command`, so pgBackRest cannot
run against it; its automated backups and point-in-time restore are the
supported path. Set the backup retention there (Azure Flexible Server: 7–35
days; RDS: up to 35), restore to a NEW server at the chosen time, and repoint
`DATABASE_URL`. The Helm chart refuses `postgres.pitr.enabled` together with
`postgres.enabled=false` for this reason. The logical backup and the result-store
mirror (§7) still apply to a managed database and are still the only copy of
task artifacts.

### 4.1 Why pgBackRest (and not WAL-G)

Both are maintained and both would work. pgBackRest **2.59.1** (released
2026-08-17; pinned by tarball digest in `deploy/pgbackrest/Dockerfile`) was
chosen because:

- `restore --type=time|lsn` picks the right base backup for the target by
  itself; with WAL-G the operator picks the backup and writes the recovery
  settings by hand — during an incident.
- repository encryption (`aes-256-cbc`, client-side) and retention/expiry are
  built in and run after every backup, instead of being a second tool on a cron;
- `pgbackrest verify` re-checksums every backup file and WAL segment in the
  repository, which makes "the backups are good" a checkable claim;
- `pgbackrest check` proves the `archive_command` round trip end to end.

The costs, accepted knowingly: its S3 client is **TLS-only** (so the dev backup
store runs with a generated certificate), and it is built from source into a
`postgres:16.15-alpine3.24` image (`kgf-nexus/postgres-pitr:16.15-pgbackrest2.59.1`).
The image keeps the stock server's major **and libc**, so an existing volume
keeps working — swapping to a glibc image would silently change text collation
and corrupt indexes.

### 4.2 What is configured

- **Archiving.** `wal_level=replica`, `archive_mode=on`,
  `archive_command='pgbackrest --stanza=nexus archive-push %p'` (async, so
  several segments push in parallel; Postgres still only recycles a segment
  after pgBackRest confirms it is in the repository), `archive_timeout=60`.
- **Base backups.** A `pgbackrest` sidecar (`deploy/pgbackrest/nexus-pitr-scheduler.sh`)
  creates the stanza, runs `pgbackrest check`, then takes a FULL backup when
  none exists or the newest is older than `PITR_FULL_INTERVAL_SECONDS` (default
  weekly), otherwise a DIFF when the newest backup is older than
  `PITR_DIFF_INTERVAL_SECONDS` (default daily). The decision reads the
  repository, so a restart neither skips nor duplicates a backup. It writes a
  status line that its healthcheck reads: unhealthy if no backup has succeeded
  within 2× the diff interval.
- **Archive-age guard.** The sidecar also forces `pg_switch_wal()` when nothing
  has been archived for `PITR_WAL_SWITCH_SECONDS` (= `archive_timeout`). This
  exists because the drill measured `archive_timeout` *not* firing for up to
  `checkpoint_timeout` (5 min) after a server start or promotion — §4.6.
- **Separation from task artifacts.** A different bucket (`nexus-pg-backups`,
  prefix `/nexus`), a different principal whose policy allows only that bucket
  (`s3:ListBucket`/`GetBucketLocation` on it, `Get/Put/DeleteObject` in it), and
  in dev a different store instance (`backup-store`). The services' result-store
  credentials cannot read the WAL, and the pgBackRest key cannot touch
  artifacts. In production prefer a bucket in another failure domain.
- **Encryption at rest.** `repo1-cipher-type=aes-256-cbc` with the passphrase
  from a secret (`NEXUS_PITR_CIPHER_PASS` / `cipher-pass`). The passphrase is
  required to restore: keep a copy outside the cluster. Server-side KMS on top
  is available through `extraEnv` (`PGBACKREST_REPO1_S3_KMS_KEY_ID`).
- **Retention knobs.** `repo1-retention-full` (default 2, `count`; or `time` in
  days), `repo1-retention-diff` (7). The PITR window is "from the start of the
  oldest retained full backup until now"; `deploy/pitr-verify.sh` prints it.

Where: compose overlay `deploy/docker-compose.pitr.yml` (`make up-pitr`), Helm
`postgres.pitr.*` in `deploy/helm/kgf-nexus/values.yaml`.

### 4.3 Running it

```sh
make up-pitr                            # compose: stack + archiving + sidecar
make backup-base BACKUP_TYPE=full       # a base backup now (before a risky change)
make pitr-verify                        # check + archiver + backup age + verify; nonzero = broken
```

`pitr-verify.sh` is the monitoring hook: it exits nonzero if `pgbackrest check`
fails, the archiver's last attempt failed, nothing was archived for 15 min, the
newest base backup is older than 2 days, or `pgbackrest verify` finds a bad
file. Its last line is JSON. `--no-verify` skips the (slow) repository re-read
for frequent probes. Negative control, run 2026-09-25: with the backup store
stopped it exits 1 with `pgbackrest check` failed and "repository unreadable";
with the store back it passes.

**Restore to a point in time** (the "undo the bad change" case):

1. Stop the Nexus services (not postgres): `docker compose ... stop api-gateway
   workflow-manager dag-engine scheduler scheduler-gpu resource-manager
   event-service agent-cpu agent-gpu agent-gpu-a10 ui`. A service connected
   during the restore sees the database move backwards under it.
2. `make restore-pitr CONFIRM=yes TARGET_TIME='2026-09-24 14:31:59+00'`
   (or `TARGET_LSN=0/5A0012F8`; neither = latest). The script prints the plan
   and the restorable window, stops postgres and the sidecar, runs
   `pgbackrest restore --delta` in a one-off container with exactly the
   container's volumes and repository settings, starts postgres, waits for
   replay + promotion, and runs `pgbackrest check` on the new timeline.
   Use the database's timezone-qualified time; `--exclusive` stops just before
   the target instead of at it.
3. Start the services. The scheduler sweeps the restored snapshot's workers to
   DEAD and expires their in-flight attempts to LOST (retried per policy); the
   agents re-register (§4.6 measures this).
4. Take a DIFF backup soon (`make backup-base BACKUP_TYPE=diff`). The new
   timeline is restorable from the old backup + WAL (the drill proves it), but a
   fresh backup shortens the next recovery.

The **result store is not rewound**. It does not need to be: objects are
content-addressed and immutable (ADR-0008), so every object a pre-target row
references is still there — the drill checks every reference. Objects written
after the target become orphans for the GC sweep (§7.7), never dangling refs.

Transactions after the target are gone from the database but stay in the WAL
archive until retention expires them, so a second restore to a *later* target
can recover them.

### 4.4 Kubernetes (Helm) — rendered, not exercised

With `postgres.pitr.enabled=true` the postgres StatefulSet runs the pitr image
with the archive settings and a `pgbackrest` sidecar (same env, shared data
volume and unix socket, no readiness probe on purpose: a failing backup must
page someone, not remove the database from its Service). Required values:
`postgres.pitr.existingSecret` (`s3-key`, `s3-key-secret`, `cipher-pass`),
`repo.s3.bucket`, `repo.s3.endpoint`; `repo.s3.caSecret` for a private CA.

```sh
PITR_EXEC="kubectl -n nexus exec -i postgres-0 -c pgbackrest --" deploy/pitr-backup.sh diff
PITR_EXEC="kubectl -n nexus exec -i postgres-0 -c pgbackrest --" deploy/pitr-verify.sh
```

A restore on Kubernetes needs the data volume with postgres NOT running:
scale the control-plane Deployments and the postgres StatefulSet to 0, run a
one-off pod from the pitr image as uid 70 that mounts the `data-postgres-0` PVC
and the same `PGBACKREST_*` env, run
`pgbackrest --stanza=nexus --delta --type=time --target='…' --target-action=promote restore`,
then scale postgres back to 1, wait for `pg_is_in_recovery()=false`, and scale
the services up. **This path has been rendered (`helm template`, `helm lint`)
but not run on a cluster**; treat the first execution as a drill, not an
incident.

### 4.5 The drills

Both drills are destructive to the stack they run on, so run them on an
isolated compose project, never on the one you care about:

```sh
export COMPOSE_PROJECT_NAME=nexus-pitr NEXUS_POSTGRES_PORT=55631 NEXUS_MINIO_PORT=55632 \
       NEXUS_MINIO_CONSOLE_PORT=55633 NEXUS_NATS_PORT=55634 NEXUS_NATS_MONITOR_PORT=55635 \
       NEXUS_API_PORT=55636 NEXUS_GRPC_PORT=55637 NEXUS_MAILPIT_UI_PORT=55638 NEXUS_UI_PORT=55639
make up-pitr        # then install a worker key (make seed) and start agent-cpu
export PG_CONTAINER=nexus-pitr-postgres-1 MINIO_CONTAINER=nexus-pitr-minio-1
make restore-drill RTO=1    # logical: backup-all, drop DB + bucket, restore, restart, reconcile
make pitr-drill PITR_DRILL_ARGS="--runs-a 3000 --artifacts-a 1500"
```

Both refuse the default `nexus` project unless `DRILL_ALLOW_DEFAULT_PROJECT=1`,
write a JSON report (`DRILL_REPORT`, `PITR_REPORT`) and print a phase table.

- **`restore-drill.sh --rto`** — after the existing scratch checks (§7.4), takes
  a real `backup-all.sh`, simulates losing both stores (DROP DATABASE, remove the
  bucket), and times: `stop` (all services + agents) → `restore_db`
  (`restore.sh`) → `restore_artifacts` (`restore-artifacts.sh`) →
  `migrate_verify` (migrate job; every `output_ref` in `task_attempts` resolves;
  a sample of objects re-hashes to its content-addressed key; restored row
  counts vs. the moment of the outage) → `services_healthy` (every `/readyz`) →
  `fleet_reconciliation`. RTO is wall clock from the start of `stop` to the end
  of `fleet_reconciliation`.
- **`pitr-drill.sh`** — seeds data with `deploy/pitr-seed.sh` (tagged batches of
  runs × nodes, ~20% retried attempts, events, content-addressed random-byte
  artifacts, and in-flight runs held under live leases by synthetic workers):
  batch **A** → FULL base backup → batch **B** (exists only in WAL) → target
  time **T** from the database clock → batch **C**. *Scenario 1* restores to T
  and asserts A and B are present with exact counts, C is absent (with a
  negative control that C had been committed), every ref resolves, and the
  in-flight state at T is reconciled. *Scenario 2* runs a writer committing a
  row every ~200 ms and recording each acknowledged commit, SIGKILLs postgres,
  **deletes its data directory**, restores to latest and measures RPO as the
  span of acknowledged commits that did not survive — and asserts the survivors
  are a gap-free prefix (nothing invented, nothing reordered). It ends with
  `pitr-verify.sh` including `pgbackrest verify`.

"Fleet reconciliation" is read from Postgres (`deploy/lib/nexus-stack.sh`):
done when no worker from the restored snapshot (no heartbeat since recovery
began) is still ACTIVE/DRAINING/QUARANTINED, no DISPATCHED/RUNNING attempt is
held by a worker that is not live, and every restarted agent has re-registered.

### 4.6 Measured results — LAPTOP measurements, not production claims

Run on 2026-09-25 in an isolated compose project (`nexus-pitr-*`, ports
55631–55639). **Hardware:** Apple MacBook Pro `Mac16,7`, Apple M4 Pro (14 cores:
10 performance + 4 efficiency), 24 GiB RAM, internal Apple SSD (APFS, 494 GB),
macOS 26.6.2; Docker Desktop 29.6.2 with a Linux VM of 10 vCPU / 15.8 GiB.
Postgres 16.15 (alpine), pgBackRest 2.59.1, MinIO RELEASE.2025-09-07 (a locally
built image; the pinned quay.io image was not pullable from this machine). The
backup store, database and result store all share one SSD, so every transfer
here is a local copy — a real object store over a network is slower, and that
difference is exactly what these numbers do NOT tell you.

Reports: `docs/drills/2026-09-25-*.json` (the 54k PITR report was produced
before a report-formatting fix and repaired by hand; its numbers are unchanged).

**Datasets.** *Medium*: 4,540 runs / 54,400 attempts / 172,260 events,
database 127 MiB; 2,000 artifacts of 1 MiB random bytes = 2.0 GiB.
*Large*: 35,080 runs / 420,800 attempts / 1,332,640 events, database 914 MiB;
2,140 artifacts, 2.0 GiB.

**PITR, scenario 1 — restore to a timestamp** (all assertions passed both times):

| phase | medium | large |
|---|---:|---:|
| stop (services + agents) | 0.8 s | 1.2 s |
| restore_db (stop pg, `pgbackrest restore`, WAL replay to T, promote, check) | 5.7 s | 8.6 s |
| &nbsp;&nbsp;of which WAL replay + promote | 2.4 s | 4.6 s |
| restore_artifacts | skipped (not rewound; §4.3) | skipped |
| migrate_verify (migrate job, 1,800–1,920 refs resolve, re-hash sample) | 2.5 s | 3.0 s |
| services_healthy | 7.0 s | 7.0 s |
| fleet_reconciliation | 4.5 s | 4.5 s |
| **RTO** | **20.6 s** | **24.5 s** |

Base backup (FULL, zstd + aes-256-cbc) before the target: 107 MiB → 15 MiB in
the repository in 1.2 s (medium); 794 MiB → 111 MiB in 2.8 s (large). Batch C
(500 / 1,000 runs committed after T) was absent after both restores; 200 / 220
post-target objects remained as orphans. Segment push latency after a forced
switch: 0.9 s.

**PITR, scenario 2 — primary lost (SIGKILL + data directory deleted), restore
to latest:**

| | medium | large |
|---|---:|---:|
| ticking before the kill | 150 s | 90 s |
| acknowledged commits / survived / lost | 552 / 426 / 126 | 336 / 169 / 167 |
| **RPO** (acknowledged work lost) | **33.8 s** | **44.7 s** |
| stop (services, already failing against a dead DB) | 14.9 s | 14.9 s |
| restore_db (restore + replay across timelines + promote) | 6.1 s | 8.5 s |
| services_healthy + migrate_verify | 8.0 s | 8.6 s |
| fleet_reconciliation | 0.2 s | 0.2 s |
| **RTO** | **29.4 s** | **32.3 s** |

The RPO is the unarchived tail — the WAL written since the last segment switch
— so on a quiet database it lies between 0 and `archive_timeout` + push time
(~61 s) wherever the kill lands; a busy database fills 16 MiB segments faster
and archives continuously. `stop` is long here only because services that have
lost their database take most of `docker stop`'s grace period to exit.

**Logical path — `restore-drill.sh --rto`** (DB dropped AND bucket removed):

| phase | medium | large |
|---|---:|---:|
| backup_all (before the outage; not RTO) | 30.3 s | 34.9 s |
| &nbsp;&nbsp;dump size | 9.7 MiB | 76 MiB |
| stop | 0.9 s | 0.9 s |
| restore_db (`pg_restore` into a new database) | 2.6 s | 12.3 s |
| restore_artifacts (2,000 / 2,140 objects, 2.0 GiB) | 24.3 s | 20.0 s |
| migrate_verify | 2.9 s | 3.3 s |
| services_healthy | 7.1 s | 7.1 s |
| fleet_reconciliation | 0.2 s | 0.2 s |
| **RTO** | **39.0 s** | **45.0 s** |

Its RPO is the dump interval: the drill lost 0 rows only because nothing wrote
between the dump and the stop.

**What the numbers say, and what they do not.**

- On this hardware, database restore is not the long pole: 1 GiB restores and
  replays in under 10 s. **Artifacts** (2 GiB, 20–24 s, local disk to local
  disk) and **service start-up** (~7 s) are. For a real deployment the artifact
  term scales with bytes and object-store bandwidth; measure it there.
- `pg_restore` grows with row count and index builds (2.6 s → 12.3 s for 8.4×
  the rows); PITR restore grows with the base backup and the WAL to replay
  (5.7 s → 8.6 s). A production database of 100 GiB is a different regime
  entirely — these runs cannot be extrapolated to it.
- **Fleet reconciliation is bounded by the liveness window, not by data size.**
  The scheduler declares a worker DEAD only once its heartbeat is older than 30 s
  (`-liveness-window`); in these runs the snapshot's heartbeats were already close
  to or past that window by the time services were up, so it took 0.2–4.5 s. Budget up to
  ~40 s (window + a 10 s sweep interval) after services are healthy.
- **Measured defect, mitigated:** after any server start or promotion,
  `archive_timeout=60` did not switch segments for **301 s** (= `checkpoint_timeout`)
  under steady writes (reproduced three times on 16.15). Without mitigation the
  RPO right after a restart or restore is ~5 minutes, not ~1. The sidecar's
  archive-age guard now forces the switch; measured intervals with it: 59–69 s.
  If the sidecar is down, the guard is too — `pitr-verify.sh` flags an archive
  older than 15 minutes.
- **Observed platform behaviour (not fixed here):** the resource-manager's
  liveness sweeper and the scheduler's both declare workers DEAD. When the
  resource-manager wins, the scheduler's sweep no longer selects the worker and
  does not expire its in-flight attempts; they are recovered by lease expiry
  instead (the agents' lease TTL is 30 s, so this costs up to 30 s, not
  correctness). Separately, a scheduler liveness sweep aborts on the first
  attempt it fails to expire, which stalls the rest of that pass.
- **Docker Desktop quirk:** restarting containers seconds after stopping them
  intermittently failed with "port already in use", and one such failure left
  the container detached from its network. The drill library retries starts and
  reattaches a detached container; a real orchestrator does the equivalent.

**Unverified:** Kubernetes/Helm PITR (rendered and linted only); a real remote
object store (S3/Azure) as the repository; restore of a backup whose
passphrase came from a real secret manager; restore across a Postgres minor
upgrade; repository retention/expiry over weeks; RTO under concurrent
production load.

### 4.7 What an RPO means for correctness, not just data

Nexus is at-least-once with mandated idempotency ([ADR-0006](adr/0006-at-least-once-with-idempotency.md)).
Restoring to any earlier point — a dump or a PITR target — re-creates attempts
that already ran, so tasks **will be re-executed** after a restore. For
idempotent handlers that is the designed behaviour. For a handler that is not
genuinely idempotent, a restore duplicates its side effects — this is the
strongest practical argument for taking the idempotency requirement seriously,
and it is worth saying out loud to task authors.

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
  encrypt before it leaves the host. (The pgBackRest repository, §4.2, IS
  encrypted client-side; the logical dump and the artifact mirror are not.)
- **~~No point-in-time recovery.~~** *Closed for the in-cluster/compose Postgres
  — see §4* (pgBackRest continuous archiving, `deploy/restore-pitr.sh`, drilled
  in §4.6). Managed Postgres uses the provider's PITR. The Helm path is rendered
  but not yet exercised on a cluster (§4.4).
- **Backup monitoring is partial.** For PITR, `deploy/pitr-verify.sh` exits
  nonzero on a broken archive, a stale archive or base backup, or a corrupt
  repository, and the sidecar's healthcheck fails when no backup has succeeded
  within 2× the diff interval — wire either into alerting. Nothing alerts if the
  logical `backup.sh` cron stops running; a job that fails silently for three
  weeks is indistinguishable from no backup at all.
- **No NATS state backup**, deliberately. NATS carries notifications; Postgres
  holds truth ([ADR-0003](adr/0003-nats-as-event-bus.md)). There is nothing there
  to restore. The `outbox` table IS in the dump; after a restore the relay
  re-publishes whatever it holds unpublished, and consumers de-duplicate by
  run_id + seq.
- **Postgres roles and grants are not in the dump** (they ARE in a pgBackRest
  backup, which is the whole cluster). `pg_dump --no-owner
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
- **The backward-compatibility rule is checked at the database layer only.**
  [ADR-0011](adr/0011-microservice-decomposition.md) rule 5 requires services to
  be compatible across one schema version — additive migrations only, a column
  dropped a release *after* its last reader is gone. Two checks now exist: the
  syntactic gate in §6.2, and `make compat-smoke`
  ([COMPATIBILITY.md §7](COMPATIBILITY.md)), which runs the **previous
  release's** database integration suite against a database upgraded to this
  tree's migrations and fails when an old query breaks on the new schema. It does
  not run the previous *service binaries* on the wire, and `migrate.sh` will still
  apply a `DROP COLUMN` if you let it. A non-additive migration that slips past
  both breaks the running services between step 2 and step 3 above.

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
| Integration tests (object store) | `NEXUS_TEST_S3_ENDPOINT`, `NEXUS_TEST_S3_ACCESS_KEY_ID`, `NEXUS_TEST_S3_SECRET_ACCESS_KEY`, optional `NEXUS_TEST_S3_BUCKET` / `NEXUS_TEST_S3_USE_TLS` only (`internal/tests3`). No default endpoint; without the endpoint the tests skip with `skipping integration test: no test S3 store`, which CI treats as a failure; an endpoint without both keys fails. They never read `NEXUS_S3_*`, `RESULT_STORE_*` or `MINIO_ROOT_*`. |

`examples/run-suite.sh` and `tests/chaos` drive an already-running stack
(Kubernetes or compose respectively) and do not start services themselves, so
the DSN is whatever that stack was deployed with.

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
  and a `/healthz?js-enabled-only=true` healthcheck. The broker **requires
  authentication** and runs a least-privilege permission model (§11.7). Its
  monitoring endpoint (`/varz`, `/connz`, `/jsz`) listens on the container's
  loopback only and is not published, because nats-server does not
  authenticate it. The client port is published on `127.0.0.1` only.
- Configuration (F38 naming): `NEXUS_NATS_URL` (empty disables the bus) plus
  one credential and optional TLS (§11.7, §11.9). Only the **event-service**
  (relay, SSE wake-ups) and the **agents** (dispatch hints) connect. The
  scheduler, dag-engine, workflow-manager, resource-manager and api-gateway only
  write `outbox` rows to Postgres and hold no NATS identity.
- Nothing `depends_on` NATS and nothing needs it to start: clients connect in
  the background and retry forever.
- Streams are created by the relay if missing and **never modified** if present,
  so retention retuned with the `nats` CLI sticks:
  `NEXUS_RUNS` (`nexus.run.>`, file, 7 days, 4 GiB, 10-minute duplicate window)
  and `NEXUS_DISPATCH` (`nexus.task.dispatch.>`, memory, 2 minutes).
- **Every deployment path provisions authorization.** Compose generates
  identities in its `nats-init` step, the Helm chart refuses to render a broker
  without them, and the services refuse to start without credentials when
  `NEXUS_ENV` is production-like. The procedure is §11.7, rotation §11.8.

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
ports 55511-55514), including stopping and restarting the broker. It then runs
the authorization suite (`TestNATSAuthz*`) twice: against a broker configured
by `deploy/nats/nexus-nats-auth.sh` (the compose path, plaintext) and against
one running the Helm chart's rendered ConfigMap with TLS and client
verification. Each role is exercised through the production code (`bus.Connect`,
the real relay, `SubscribeDispatchHints`, the SSE waker), and every denial has a
positive control. Needs Docker and `helm`. `AUTHZ=0` skips the authorization
half and says so.

### 11.7 Authorization: identities and permissions

**The choice: NKeys in the server config, not decentralized JWT.** Each client
holds one NKey seed (Ed25519). The server config lists only public keys and
their permissions, so the config holds no secret and can live in a ConfigMap
or in git. Authentication is challenge-response: the seed never crosses the
wire, so it is safe even before TLS is on. Decentralized JWT (operator, account
and user JWTs, a resolver, `nsc`) was considered and not chosen. Its strengths
are delegated account administration and adding users without touching the
server, and this platform has one operator and a handful of tenants. It would
add an operator signing key and a resolver as new things to protect and back up.
The client supports `.creds` files too (`NEXUS_NATS_CREDS`), so moving to JWT
later is a server-side change only. Plain user and password is supported for
brokers you do not run. It is refused over plaintext to a non-loopback host,
because the password is sent in `CONNECT`.

**The model.** It is rendered identically by `deploy/nats/nexus-nats-auth.sh`
(compose, Portainer host) and `templates/nats-config.yaml` (Helm):

| Identity | Account | Publish | Subscribe |
|---|---|---|---|
| `event-service` | NEXUS | `nexus.run.>`, `nexus.task.dispatch.>`, `$JS.API.STREAM.INFO.NEXUS_RUNS/NEXUS_DISPATCH`, `$JS.API.STREAM.CREATE.NEXUS_RUNS/NEXUS_DISPATCH` | `nexus.run.>` (SSE waker), `_INBOX.nexus-event-service.>` (its own JetStream acks) |
| `agent-<ns>` (one per tenant) | NEXUS | **nothing** (`deny >`) | `nexus.task.dispatch.<ns>.>` only |
| `monitor` | SYS (system account) | read-only `$SYS.REQ.SERVER.PING.*` / `$SYS.REQ.SERVER.*.<Z>` / `$SYS.REQ.ACCOUNT.*.<Z>` queries (no RELOAD, KICK or LDM) | `_INBOX.>`, `$SYS.SERVER.>`, `$SYS.ACCOUNT.>` |

There is no anonymous user and no `no_auth_user`, so an unauthenticated
connection is refused. The event-service can inspect and create its two streams
and nothing else: it cannot delete, purge or update them, or create consumers.
Each client sets a reply-inbox prefix of `_INBOX.<connection name>`, which is
why the event-service's subscribe grant covers only its own replies. The
scheduler, dag-engine, workflow-manager, resource-manager and api-gateway have
no identity at all, because they never connect. They reach the bus only through
`outbox` rows the event-service relays.

**Tenants.** An agent identity is per Nexus namespace. A tenant's agents share
it, or you list several public keys for one tenant to give each agent (or edge
host) its own. The subject token is the namespace itself for names matching
`[A-Za-z0-9_-]+`. Other names are percent-encoded on the bus (`bus.Token`), and
both renderers refuse them rather than guess. To add tenant `acme`:

```sh
# compose / Portainer host (add -f deploy/docker-compose.tls.yml if you use it)
docker compose -f deploy/docker-compose.yml run --rm nats-init init --tenant acme
docker compose -f deploy/docker-compose.yml run --rm nats-init   # default command: render + install
docker compose -f deploy/docker-compose.yml kill -s HUP nats
# acme's agents need /store/agent-acme/user.nk from the nats-auth-store volume
# Kubernetes (admin machine that holds the key directory)
NEXUS_NATS_DIR=/secure/nexus-nats deploy/nats/nexus-nats-auth.sh add-tenant acme
NEXUS_NATS_DIR=/secure/nexus-nats deploy/nats/nexus-nats-auth.sh helm-values  # merge into values, helm upgrade
# give acme's agents /secure/nexus-nats/agent-acme/user.nk as NEXUS_NATS_NKEY_SEED_FILE
```

**Client settings (event-service and agent).** All are validated at startup.
A configuration error is fatal, but an unreachable broker never is (ADR-0003).

| Variable | Meaning |
|---|---|
| `NEXUS_NATS_URL` | `nats://` or `tls://`, comma-separated for a cluster. Empty disables the bus. |
| `NEXUS_NATS_NKEY_SEED_FILE` | NKey user seed file (`SU…`, bare or armoured). **The shipped method.** |
| `NEXUS_NATS_CREDS` | `.creds` file (JWT + seed) for a JWT-mode broker. Re-read on every reconnect. |
| `NEXUS_NATS_USER` + `NEXUS_NATS_PASSWORD` or `NEXUS_NATS_PASSWORD_FILE` | Password auth. The file is re-read on every reconnect. Setting both password variables is an error. Refused without TLS unless every server is loopback. |
| `NEXUS_NATS_REQUIRE_AUTH` | Makes "no credentials" a startup error. **Armed by `NEXUS_ENV=production/prod/staging`.** An explicit `false` overrides that and logs a WARN. Compose and Helm set it to `1`. |
| `NEXUS_NATS_TLS`, `NEXUS_NATS_TLS_CA_FILE`, `NEXUS_NATS_TLS_CERT_FILE`, `NEXUS_NATS_TLS_KEY_FILE`, `NEXUS_NATS_TLS_SERVER_NAME` | §11.9 |

Setting more than one credential method is an error, and so is a seed or creds
file that is unreadable or not a *user* key. Error messages never include file
contents. Clients keep retrying after authentication errors
(`IgnoreAuthErrorAbort`), so a broker-side mistake during a rotation heals once
it is fixed and does not leave a process permanently deaf. A permissions
violation is logged at ERROR (`nats denied an operation for this identity`).
Either the deployed permissions are too narrow or a process is doing something
its role must not do.

**Compose.** The one-shot `nats-init` container runs
`nexus-nats-auth.sh compose-init`. On the first `up` it generates the
identities into the `nats-auth-store` volume and reuses them afterwards. It
renders the server config into `nats-auth-server` and copies each seed into a
volume only its consumer mounts: `nats-auth-event-service` and
`nats-auth-agent` (uid 10001, mode 0400). No setup step is needed and nothing is
written to the source tree. `render_compose.py` inlines the script for the
Portainer host, which has no source tree. To query the broker as the monitor:

```sh
docker compose -f deploy/docker-compose.yml run --rm --entrypoint nats nats-init \
  --server nats://nats:4222 --nkey /store/monitor/user.nk server report jetstream
```

A host-side key directory (`NEXUS_NATS_DIR`, default `./.dev-nats`, which is
gitignored) is used by `make k8s-dev-up` and by the Kubernetes procedure below.

**Helm.** `nats.auth.mode: nkey` is the default, and rendering **fails** until
`nats.auth.eventService.publicKeys`, `nats.auth.monitor.publicKeys` and a
`nats.auth.tenants` entry for `nexusNamespace` are set. Public keys are also
checked for format. The seeds go in a Secret, by default `existingSecret`,
under the keys `nats-event-service-nkey` and `nats-agent-nkey`. Each pod
projects only its own key to `/etc/nexus/nats/user.nk`. An agent entry may set
`natsSeedKey` to use its own identity.

```sh
NEXUS_NATS_DIR=/secure/nexus-nats deploy/nats/nexus-nats-auth.sh init --tenant <nexusNamespace>
NEXUS_NATS_DIR=/secure/nexus-nats deploy/nats/nexus-nats-auth.sh helm-values > nats-values.yaml
kubectl -n <ns> create secret generic nexus-nats-seeds \
  --from-file=nats-event-service-nkey=/secure/nexus-nats/event-service/user.nk \
  --from-file=nats-agent-nkey=/secure/nexus-nats/agent-<nexusNamespace>/user.nk
helm upgrade --install … -f nats-values.yaml --set nats.auth.seedSecret=nexus-nats-seeds
```

Generate keys on an admin machine, not in the cluster. The monitor seed stays
there (or goes to whoever runs nats-surveyor). Also in the chart:

- A `nats` NetworkPolicy that admits only event-service and agent pods on 4222
  (`nats.networkPolicy.extraFrom` for anything else).
- Monitoring on the pod's loopback only, with no `monitor` port on the Service.
  Probes run `wget` in the container.
- A config-reloader sidecar (`natsio/nats-server-config-reloader:0.21.1`, with
  `shareProcessNamespace`) that sends SIGHUP to the broker when the ConfigMap or
  its TLS files change.

`nats.auth.mode: none` exists for a throwaway cluster. It sets
`NEXUS_NATS_REQUIRE_AUTH=0` on the clients and is never appropriate where more
than one tenant, or anything untrusted, can reach the broker. `deploy/terraform`
reads the key directory (`nats_credentials_dir`, which `make k8s-dev-up`
populates) and adds the seeds to its credentials Secret. Like every other
generated credential there, they are in Terraform state.

**Upgrading an existing deployment.** Provision identities first, then roll
everything together:

- **Compose:** `up` does it.
- **Helm:** set the values and the Secret. The upgrade fails to render without them.
- **Custom deployments:** set `NEXUS_NATS_NKEY_SEED_FILE` on the event-service
  and agents before switching the broker to the authenticated config.

Between the broker switch and the client roll, the bus is down, not the
platform: agents long-poll and the outbox holds the backlog (§11.4). Off-cluster
agents need their tenant's seed, and their `NEXUS_NATS_URL` must point at a
listener they can reach.

**Streams from the unauthenticated broker.** A broker that ran without
authorization kept its JetStream streams in the global account (`$G`). Once
accounts are configured nothing owns `$G`, so the server cannot recover those
streams and reports them unhealthy on every health check (F107). Both deploy
paths therefore move `/data/jetstream/$G` to
`/data/retired-global-account-<UTC timestamp>` before the server starts: the
Helm chart in the `retire-global-account-streams` init container, compose in
the `nats` service's entrypoint. Nothing is deleted. Those streams held only
notifications, PostgreSQL remains authoritative, and the event-service recreates
`NEXUS_RUNS` and `NEXUS_DISPATCH` in the `NEXUS` account on start. Once the
upgrade is verified, delete the retired directory:

```bash
kubectl -n <ns> exec nats-0 -c nats -- sh -c 'ls -d /data/retired-global-account-*'
kubectl -n <ns> exec nats-0 -c nats -- sh -c 'rm -rf /data/retired-global-account-*'
```

The step is a no-op on a fresh volume and on every later start.

### 11.8 Rotating NATS credentials

Rotate on compromise, on staff change and on a schedule (for example
annually). Every step keeps the bus up, because the server accepts the old and
the new key for the same role during the overlap.

```sh
# Kubernetes / host-managed key directory
# 1. new key alongside the old one (the old public key moves to retiring.pub)
NEXUS_NATS_DIR=<dir> deploy/nats/nexus-nats-auth.sh rotate agent-acme
# 2. make the broker accept both keys: helm-values -> values, helm upgrade.
#    The reloader sends SIGHUP to the broker.
# 3. update the Secret key and IMMEDIATELY: kubectl rollout restart deploy/<agents or event-service>
# 4. stop accepting the old key
NEXUS_NATS_DIR=<dir> deploy/nats/nexus-nats-auth.sh retire agent-acme
#    ...then helm-values -> values, helm upgrade

# compose (C="docker compose -f deploy/docker-compose.yml", plus the TLS override if used)
$C run --rm nats-init rotate agent-default     # 1
$C run --rm nats-init && $C kill -s HUP nats   # 2 (also installs the new seed)
$C up -d --force-recreate agent-cpu agent-gpu agent-gpu-a10   # 3, straight after 2
$C run --rm nats-init retire agent-default     # 4
$C run --rm nats-init && $C kill -s HUP nats
```

A process holding an NKey must restart to switch seeds. nats.go fixes the public
key when the connection is created, but it re-reads the seed file each time it
signs. Between installing a new seed and the restart, a reconnect would fail,
so restart straight away. Nothing else breaks in the meantime. `.creds` and password files are
re-read at every reconnect. `rotate` deletes nothing the clients still use, and
`retire` is the step that revokes. For a **compromised** seed, skip the
overlap: `rotate`, then `retire` straight away, re-render and reload. The
compromised key is refused from the next connection attempt. Connections
already open with it are dropped by the reload itself, which was verified
against nats-server 2.14.7: the live subscriber was disconnected at the SIGHUP
and its reconnect was refused. Confirm with `nats server report connections` as the
monitor, or with `/connz` inside the pod. The role's clients are down until
they have the new seed, and the platform keeps working by polling.

### 11.9 TLS to the broker

The NKey authenticates each client. TLS adds confidentiality, which matters
because run events carry tenant data, and it authenticates the broker. The
clients reuse `internal/transport`'s hot-reloading material. A renewed
certificate or CA bundle is used at the next reconnect without a restart.

- **Client:** TLS is on when `NEXUS_NATS_TLS=1`, the URL uses `tls://`, or any
  `NEXUS_NATS_TLS_*_FILE` is set. The CA defaults to `NEXUS_TLS_CA_FILE`, and
  the client pair defaults to `NEXUS_TLS_CLIENT_CERT_FILE`/`_KEY_FILE`: one
  private CA for every internal hop (docs/TLS.md). **`NEXUS_REQUIRE_TLS=1`
  makes a plaintext broker connection fatal**, just like a plaintext gRPC hop.
  Explicitly turning TLS off while also asking for it (for example
  `NEXUS_NATS_TLS=0` with a `tls://` URL) is an error.
- **Compose:** `make up-tls` issues a `nats` leaf (`make dev-certs`). The broker
  serves it and requires a client certificate from the dev CA
  (`verify: true`). The agents and event-service dial with their leaves.
  nats-server re-reads certificates only on reload, so after a renewal run
  `docker compose kill -s HUP nats`.
- **Helm:** `nats.tls.enabled=true` with `secretName` (a cert-manager
  Certificate for `nats`, `nats.<ns>.svc`). With `verifyClients` (the default),
  `clientSecrets.eventService` and `.agent` must also be set. Otherwise
  `caSecret` supplies `ca.crt`. The reloader picks renewed certificates up.

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
- The cluster-controller image, pinned by digest. The release pipeline builds
  and pushes `<acr>/nexus/cluster-controller`; `deploy/scripts/cluster-controller-overlay.sh`
  renders `base` or `apply` with that digest (the `kustomize edit set image`
  retarget, as a throwaway overlay) and fails if any container would run
  something else. `templates/deploy-aks.yml` runs it when its
  `clusterController` parameter is `base` or `apply` (default `none`). By hand:
  `NAMESPACE=kgf-nexus deploy/scripts/cluster-controller-overlay.sh base <acr>/nexus/cluster-controller@sha256:<digest> | kubectl apply -f -`.
  On a local kind/Docker Desktop cluster, `make k8s-dev-images` builds and loads
  `kgf-nexus/cluster-controller:<tag>`; apply it with `ALLOW_TAG=1` and that tag.

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
| agent | own listener: `/metrics`, `/healthz`, `/readyz` (503 until registered) | `NEXUS_METRICS_ADDR` / `-metrics-addr` (`127.0.0.1:8089`; `off` disables) | `127.0.0.1:19120-19122` | pod port `metrics` 8089 (PodMonitor) |
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
- The gateway rate-limit families are labelled by bucket **kind** (`address`,
  `auth_failure`, `auth_failure_repeat`, `credential`, `email_address`,
  `email_recipient`), never by the client address, credential or recipient
  the bucket is keyed on.

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
| `nexus_gateway_ratelimit_rejections_total` | C | bucket (address/auth_failure/credential/email_address/email_recipient) | api-gateway | Requests refused by a limiter (§16): a 429, or the silent drop of `email_recipient`. A rise in `auth_failure` is credential stuffing; a rise in `address` or `credential` from legitimate clients means the limit is too tight |
| `nexus_gateway_ratelimit_buckets` | G | bucket (the above plus auth_failure_repeat) | api-gateway | Live keys each limiter tracks, per replica. Only enabled limiters report. At the per-limiter cap of 100000 the least recently used key is evicted; see `_evictions_total` |
| `nexus_gateway_ratelimit_evictions_total` | C | bucket | api-gateway | Buckets dropped at the entry cap rather than by idling. Non-zero: the cap is too small for the traffic, or the gateway is being sprayed from many addresses |
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
| `nexus_dag_operator_retries_total` | C | result (reset/invalid/not_retryable/not_found/conflict/error) | dag-engine | Operator retries (§18) applied to a run's graph. `invalid` and `not_retryable` are the caller's problem; `conflict` means a concurrent writer won and a retry of the request may succeed; `error` > 0 needs a look |
| `nexus_dag_operator_retry_nodes_reset_total` | C | — | dag-engine | Nodes reset by operator retries, selected nodes and their dependents alike |
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
| `nexus_objstore_operations_total` | C | backend, op, outcome (ok/not_found/exists/error) | agent, api-gateway, artifact-gc | Every result-store call (§26.5). `outcome="error"` rising: the store or its credentials |
| `nexus_objstore_operation_duration_seconds` | H | backend, op | agent, api-gateway, artifact-gc | Call latency; for `get`, time to open the object |
| `nexus_objstore_bytes_total` | C | backend, direction (in/out) | agent, api-gateway, artifact-gc | Payload bytes through the result store |
| `nexus_agent_sessions_total` | C | — | agent | Registrations. More than 1 per process means the control plane forced a re-registration |
| `nexus_agent_advertised_capacity` | G | resource | agent | Capacity advertised for scheduling, which the runtime does not necessarily enforce |
| `nexus_agent_result_report_failures_total` | C | — | agent | Reports not delivered. The WAL retains them |
| `nexus_agent_registration_attempts_total` | C | result (ok/retryable_error/permanent_error) | agent | Register calls. An agent that starts before the gateway retries `Unavailable`, `DeadlineExceeded`, `ResourceExhausted`, `Aborted` and `Internal` with capped exponential backoff (0.5 s to 30 s, full jitter) and stays not-ready meanwhile; a rising `retryable_error` with no `ok` means the gateway is unreachable or its auth backend is down. `permanent_error` (bad API key, wrong role, invalid request) exits the process with a message naming the fix |
| `nexus_agent_registered` | G | — | agent | 1 while the agent holds a registered session; the `/readyz` condition |
| `nexus_agent_log_dropped_lines_total`, `_dropped_bytes_total` | C | — | agent | Task log lines (and their bytes) dropped because shipping fell behind and the buffer was full (§19). Non-zero: the object store or the gateway is slow or unreachable from this agent |
| `nexus_agent_log_truncated_lines_total`, `_truncated_bytes_total` | C | — | agent | Task log lines omitted by the per-attempt cap (head and tail are kept). Steady growth: a task logs more than the cap; raise it or quiet the task |
| `nexus_agent_log_fenced_bytes_total` | C | — | agent | Task output refused after an attempt's log shipping was fenced (lease rejected, or a gateway without `ReportLogs`) |
| `nexus_agent_log_finalize_total` | C | outcome (finalized/fenced/timeout/partial) | agent | Attempt logs finalized. Every outcome but `finalized` leaves a partial log. `timeout` or `partial` rising: object store trouble; `fenced` rising: lease loss (see `nexus_scheduler_attempts_lost_total`) |
| `nexus_artifact_gc_last_run_objects` | G | disposition | artifact-gc | Last sweep: scanned, live_refs, reachable, too_young, orphan, deleted, delete_error |
| `nexus_artifact_gc_last_run_orphan_bytes` | G | — | artifact-gc | Reclaimable bytes found |
| `nexus_artifact_gc_last_run_log_objects` | G | disposition (scanned/orphan/deleted/delete_error) | artifact-gc | The task-log share of the last sweep (§19): log objects examined, collectable, collected and not removable. A subset of `_last_run_objects` |
| `nexus_artifact_gc_last_run_log_bytes` | G | disposition (orphan/deleted) | artifact-gc | Task-log bytes collectable and actually reclaimed by the last sweep (`deleted` is 0 in a dry run) |
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
| `NEXUS_S3_ACCESS_KEY_ID`, `NEXUS_S3_SECRET_ACCESS_KEY` | unset | READ credentials for log objects on the `s3` backend, or any credential mode / backend in §26.3 (IRSA, Pod Identity, Azure or GCS workload identity). If the gateway cannot open the store: the logs route answers 501 and agents are told not to ship. |
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

---

## 20. Installing with Helm

The chart is `deploy/helm/kgf-nexus` (chart 0.2.0). One release per Kubernetes
namespace: Services have fixed names (`api-gateway`, `postgres`, …) exactly as
compose does. `deploy/terraform` drives it for the environments it owns; this
section is for installing it directly. The design documents behind each part
are docs/TLS.md (mTLS), docs/CLUSTER-SCALING.md (controller), §7.7/§19.2
(artifact GC) and ADR-0018 (scheduler scaling).

### 20.1 Before you install

| Need | Why | Check |
|---|---|---|
| Kubernetes ≥ 1.29 | `kubeVersion` in Chart.yaml; `matchLabelKeys` in spread constraints | `kubectl version` |
| Images for **one immutable tag**: api-gateway, workflow-manager, dag-engine, scheduler, resource-manager, event-service, agent, migrate, ui, **artifact-gc**, **cluster-controller** | every component renders `<registry><prefix><component>:<tag>`; the last two are new in 0.2.0 | the release pipeline builds all eleven (`.azure-pipelines/templates/images.yml`); locally `make k8s-dev-images` |
| A credentials Secret (`existingSecret`) | no credential is ever rendered into a manifest | keys `postgres-password`, `minio-root-user`, `minio-root-password`, `worker-api-key`, optional `admin-api-key` |
| NATS identities (`nats.auth`) | the chart refuses an unauthenticated broker | §11.7; `nats.auth.mode=none` only for a throwaway cluster |
| metrics-server | HPAs (`autoscaling.enabled`) | `kubectl get apiservice v1beta1.metrics.k8s.io` |
| cert-manager (v1.18+) | `tls.enabled` with `tls.certManager.enabled` | `kubectl get crd certificates.cert-manager.io` |
| A PriorityClass | only if `placement.priorityClassName` is set | `kubectl get priorityclass` |

### 20.2 Install, upgrade, uninstall

```sh
kubectl create namespace nexus
kubectl -n nexus create secret generic nexus-credentials --from-env-file=creds.env   # generated elsewhere
helm install nexus deploy/helm/kgf-nexus -n nexus -f my-values.yaml --wait --wait-for-jobs --timeout 15m
kubectl -n nexus wait --for=condition=Ready pod -l 'app.kubernetes.io/component notin (migrate,bootstrap,minio-init,artifact-gc)' --timeout=5m
```

- **Do not rely on `helm --wait` alone.** With Helm 4 the release reported
  `deployed` while an agent pod was still crash-looping (§20.8). Wait on pod
  readiness as above.
- **Upgrade with `-f my-values.yaml` or `--reset-then-reuse-values`, never
  `--reuse-values`.** `--reuse-values` replays the previous release's computed
  values and drops this version's new defaults; the chart detects the missing
  sections and refuses with that advice rather than failing on a nil value.
- Upgrade order is the §6.3 order: back up, then `helm upgrade` (the migrate Job
  runs first; every service waits for it in `wait-for-schema`).
- The migrate/bootstrap/minio-init Job names hash the image tag, the chart
  version, the pinned `image.digests.migrate` (F112) and `minio.mcImage`, so a
  chart-only upgrade, a re-pushed image under the same tag or a new client image
  re-runs them (all three are idempotent) instead of failing on an immutable Job
  template (F113).
- `helm uninstall` keeps the StatefulSets' PVCs (your data) and, because
  cert-manager does not delete them, the `nexus-*-tls` Secrets. Delete them
  deliberately, or delete the namespace.

### 20.3 Values reference

Everything before 0.2.0 is unchanged and still accepted. The keys below are the
ones an operator sets; `values.yaml` documents each one inline, and
`values.schema.json` rejects wrong types, unknown enum values, malformed
durations, a `latest` tag, and any misspelt **top-level** key at install time.

| Key | Default | Meaning |
|---|---|---|
| `existingSecret` | — (required) | Secret with the credentials in §20.1 |
| `image.tag` | — (required) | immutable tag for every Nexus image; `latest` is refused |
| `image.registry`, `image.repositoryPrefix`, `image.pullSecrets` | `""`, `kgf-nexus/`, `[]` | image location |
| `image.digests.<component>` | `{}` | `sha256:<64 hex>` per image; renders `<ref>:<tag>@<digest>`. The release pipeline writes it (`deploy/scripts/helm-image-digests.py`, §22) |
| `nexusNamespace` | `default` | the Nexus tenant the agents serve and bootstrap creates |
| `postgres.enabled` / `.host` / `.sslmode` | `true` / `postgres` / `disable` | `false` + host for a managed database (use `verify-full`) |
| `postgres.caBundle.secretName` / `.configMapName` / `.key` | `""` / `""` / `ca.crt` | root CAs for `verify-ca`/`verify-full`, mounted as `sslrootcert`, §20.11 |
| `postgres.params`, `postgres.connections.<c>.params` | `{}` | extra libpq parameters, global and per connection (`apiGateway` … `artifactGC`, `migrate`), §20.11 |
| `postgres.runtimeRoles.enabled` / `.scope` / `.existingSecret` / `.passwordRevision` / `.connectionLimit` | off / union / `existingSecret` / 1 / -1 | per-service logins instead of the owner, §20.12 |
| `serviceAccounts.<component>` `{name, annotations, automountServiceAccountToken, podLabels}` | `nexus-<component>`, token not mounted | per-component identity (IRSA, Workload Identity), §20.13 |
| `agents[].nodeSelector` / `.tolerations` / `.affinity` / `.gpu` / `.serviceAccount` | global placement; GPUs requested when `gpuCount > 0` | agent pools and GPU devices, §20.14 |
| `postgres.pitr.*` | off | pgBackRest archiving, §4 |
| `minio.enabled` / `.endpoint` / `.useTLS` | `true` / `http://minio:9000` / `false` | object store |
| `minio.image` / `.mcImage` | digest-pinned `pgsty/silo` | in-cluster S3 server and bucket-init client, §22 |
| `nats.enabled`, `nats.auth.*`, `nats.tls.*` | on, nkey, off | event bus, §11 |
| `services.<svc>.replicas` / `.resources` | 1 / `serviceDefaults` | per-service sizing; `<svc>` ∈ apiGateway, workflowManager, dagEngine, resourceManager, eventService |
| `services.<svc>.autoscaling` | merged over `autoscaling` | per-service HPA override (not resourceManager) |
| `services.<svc>.podDisruptionBudget` | merged over `podDisruptionBudget` | per-service PDB override |
| `schedulers[]` `{name, queue, replicas, resources, autoscaling, podDisruptionBudget}` | `scheduler`/default, `scheduler-gpu`/gpu | one Deployment per queue; at least one required |
| `agents[]` | one CPU agent | worker Deployments; `podDisruptionBudget` per entry |
| `ui.replicas` | 1 | console replicas |
| `placement.priorityClassName` | `""` | PriorityClass for every Nexus pod (must exist) |
| `placement.topologySpread.*` | on, zone then hostname, `ScheduleAnyway` | spread replicas; `DoNotSchedule` makes it a hard rule |
| `placement.nodeSelector` / `.tolerations` / `.affinity` | empty | applied to every Nexus Deployment and the GC job |
| `podDisruptionBudget.enabled` / `.maxUnavailable` / `.minAvailable` | on / 1 / null | a PDB for every Deployment whatever its size; `minAvailable ≥ replicas` is refused |
| `autoscaling.enabled` / `.minReplicas` / `.maxReplicas` / `.targetCPUUtilizationPercentage` / `.targetMemoryUtilizationPercentage` / `.behavior` | off / 2 / 6 / 70 / null / 1 pod per 60 s down, 300 s window | HPAs for the four stateless services |
| `tls.enabled` / `.requireClientCert` / `.requirePeerTLS` / `.reloadInterval` / `.caBundleConfigMap` | off / true / true / 10s / `""` | gRPC mTLS, §20.4 |
| `tls.certManager.enabled` / `.issuerRef` / `.duration` / `.renewBefore` / `.gatewayExtraDnsNames` / `.natsLeaves` | on / self-signed bootstrap / 720h / 240h / [] / false | leaf issuance |
| `clusterController.enabled` / `.clusterName` / `.replicas` / `.interval` / `.workerNamespaces` / `.observeNodes` | off / — / 2 / 15s / [release ns] / true | §20.6 |
| `clusterController.apply.enabled` / `.expectedClusterUID` | off / `""` | apply mode, §20.6 |
| `artifactGC.enabled` / `.schedule` / `.timeZone` / `.mode` / `.confirmDelete` | on / `17 3 * * *` / `""` / report / false | §20.7 |
| `artifactGC.grace` / `.logRetention` / `.maxObjects` / `.prefix` / `.pushgatewayUrl` | 24h / `""` / 10000 / `""` / `""` | sweep tuning and metrics |
| `metrics.serviceMonitor.enabled` / `.podMonitor.enabled` | off / follows | prometheus-operator objects, §17 |
| `gateway.*` | off | Gateway API routes |

`deploy/helm/kgf-nexus/ci/*-values.yaml` are three complete, validated
examples: minimal, every feature on, and the production shape (managed database
and object store, external issuer, HPAs, `minAvailable` PDBs).

### 20.4 mTLS between services

`tls.enabled=true` wires every gRPC hop (docs/TLS.md §3 variables): each
control-plane service serves and dials with its leaf `nexus-<component>-tls`
(the five services; all schedulers share `nexus-scheduler-tls`), agents and the
cluster controller dial with client-only leaves, every server requires a client
certificate, and `NEXUS_REQUIRE_TLS=1` turns a missing certificate into a
startup failure. Secrets are mounted without `subPath`, so renewals are picked
up in place (TLS §5.0). REST, probes and `/metrics` stay HTTP.

- **Issuer.** Production: apply `deploy/k8s/cert-issuer.yaml` once (its CA key
  lives in the cert-manager namespace) and set
  `tls.certManager.issuerRef: {name: nexus-internal-ca, kind: ClusterIssuer}`.
  With no issuer name the chart bootstraps a self-signed CA **in the release
  namespace** — anyone who can read Secrets there can mint identities, so this
  is for test clusters only (NOTES.txt says so after install).
- **Trust bundle.** For a CA key rotation with an overlap window, publish the
  trust-manager Bundle to a ConfigMap and set `tls.caBundleConfigMap` (§9.4).
- **NATS.** The agents apply `NEXUS_REQUIRE_TLS` to the broker connection too,
  so peer TLS with a plaintext broker crash-loops them. The chart refuses that
  combination. Set `tls.certManager.natsLeaves=true` and
  `nats.tls: {enabled: true, secretName: nexus-nats-tls, clientSecrets: {eventService: nexus-event-service-tls, agent: nexus-agent-tls}}`.
- **External agents** dial `api-gateway-external`: add that name to
  `tls.certManager.gatewayExtraDnsNames` and enrol the agents per TLS §5.5.
  During that enrolment `tls.requireClientCert=false` is the documented bridge.
- **BYO certificates:** `tls.certManager.enabled=false` and create the
  `nexus-<component>-tls` Secrets yourself (SANs as in
  `deploy/k8s/cert-certificates.yaml`).

### 20.5 Autoscaling, disruption budgets, placement

- **HPAs** (`autoscaling.enabled`) cover api-gateway, workflow-manager,
  dag-engine and event-service. Their correctness does not depend on replica
  count (CAS reconcile, `SKIP LOCKED` relay and timers). With an HPA the
  Deployment has no `replicas`, so an upgrade never resets the autoscaler.
  Gateway rate limits are per replica (§16.6): N replicas admit up to N× a
  client's rate.
- **resource-manager** has no HPA and the chart refuses one.
- **Schedulers:** not autoscaled by default. Claim cost is bound by
  **Postgres** CPU, not the scheduler's (ADR-0018). Since the windowed pull
  claim (§15), it no longer grows with the backlog, but extra replicas stop
  helping once the database is saturated. The laptop curves are in
  [BENCHMARKS.md](BENCHMARKS.md). CPU on the scheduler pod is therefore the
  wrong signal. Size `schedulers[].replicas` (2–4 per busy queue) and watch
  `nexus_scheduler_claim_duration_seconds` (p99 against ADR-0018's 250 ms
  trigger) and `nexus_scheduler_pending_attempts` (§17.4).
  `schedulers[].autoscaling` exists for an operator who has measured their own
  curve; the cluster controller reports scheduler HPAs either way.
- **Workers** are scaled by the cluster controller (apply mode) or by hand.
  Never put an HPA on a worker Deployment the controller is bound to
  (CLUSTER-SCALING, "Who scales what").
- **PDBs** exist for every Deployment from one replica up. `maxUnavailable: 1`
  never blocks a node drain; `minAvailable` is safer only once replicas > it,
  and the chart refuses a budget that would block every drain. With an HPA the
  check uses `minReplicas`.
- **Placement:** zone-then-hostname spread with `ScheduleAnyway` by default.
  `DoNotSchedule` needs nodes that carry `topology.kubernetes.io/zone`; on a
  cluster without zone labels, drop that key or pods stay Pending.

### 20.6 Cluster controller via Helm

`clusterController.enabled=true` with `clusterName` installs the ported
`deploy/k8s/cluster-controller/base`: a ServiceAccount, two replicas with a Lease
(`nexus-cluster-controller-<cluster>`), a read-only Role in the release
namespace (leases get/create/update, Deployments and HPAs read), a read-only Role
in each `workerNamespaces` entry (Deployments, pods, quotas, Jobs), and one
read-only ClusterRole for Nodes plus `get` on the `kube-system` Namespace only.
It is **observe-only**: `NEXUS_CONTROLLER_ALLOW_APPLY=false` and no mutating
permission anywhere.

1. Register the cluster first (console, or `PUT
   /v1/namespaces/<ns>/platform/clusters/<name>` with `provider`,
   `control_plane_namespace` = the release namespace, `mode: plan`). Until then
   the leader logs `cluster not registered` and, after four intervals, reports
   NotReady — which is the alert you want.
2. Confirm in the console (Live cluster): a fresh report, effective mode `plan`,
   the observed kube-system UID, the scheduler Deployments and HPAs.
3. **Apply** needs all three opt-ins (CLUSTER-SCALING, "Apply is three
   independent opt-ins"): the registry entry in apply mode with the UID
   (console), **and** `clusterController.apply.enabled=true` with
   `apply.expectedClusterUID` set to that UID. The chart then sets the process
   flag and binds the apply Role (`deployments/scale` update, `pods` patch,
   worker namespaces only). On a real install/upgrade the chart compares the
   UID with the live cluster and refuses a mismatch, so values written for one
   cluster cannot enable apply on another. (`helm template` cannot look it up.)
4. Back out: console **Switch to plan**, then `apply.enabled=false` (drops the
   flag and the Role), then `clusterController.enabled=false`. §12.4 applies.

The scheduler Deployments carry `app.kubernetes.io/component=scheduler` and
`app.kubernetes.io/part-of=kgf-nexus` on the Deployment object (pods keep their
per-queue component), which is the selector the controller uses; before 0.2.0
the chart's schedulers were invisible to it (F113).

### 20.7 Artifact GC CronJob

Report-only by default (`artifactGC.mode=report`): the nightly job logs what it
*would* collect (the §7.7 summary) and deletes nothing. To collect, set **both**
`mode: delete` and `confirmDelete: true` — they map to the binary's
`-delete -i-understand-this-deletes-data`, and the chart refuses `delete`
without the confirmation. `logRetention` (e.g. `720h`) enables §19.2 task-log
retention; outputs are never time-expired. `pushgatewayUrl` pushes the final
metrics under `job=artifact-gc` (alert on a stale
`nexus_artifact_gc_last_success_timestamp_seconds`, §17). `concurrencyPolicy: Forbid`
and the bounded `maxObjects` mean a backlog drains over several nights.

```sh
kubectl -n nexus create job --from=cronjob/artifact-gc artifact-gc-manual-$(date +%s)   # run now
kubectl -n nexus logs job/artifact-gc-manual-<ts>                                        # read the summary
```

Run report mode for at least a week and read the summaries before enabling
delete.

### 20.8 What was verified (2026-09-25, docker-desktop, Kubernetes v1.34.3)

Static, `deploy/helm/validate.sh` (Helm v4.1.1, kubeconform, K8s 1.29.0
schemas plus the CRDs catalog for cert-manager and prometheus-operator):
`helm lint --strict` and strict kubeconform on all three `ci/` values files
(38, 73 and 47 objects, all valid), and 15 bad-values cases each refused with the
expected message (misspelt section, `latest` tag, GC delete without confirmation,
controller without a name, apply without a UID, a drain-blocking PDB, HPA
min > max, HPA on resource-manager, bad duration, no schedulers, peer TLS with a
plaintext broker, …).

Live, in namespace `nexus-w2c-helm` (release `nexus-w2c`, images `46f491dfabb5`,
values: mTLS with the self-signed bootstrap CA and NATS TLS, HPAs 1–3, the
controller enabled):

| Check | Result |
|---|---|
| Every Deployment/StatefulSet pod Ready | 14/14 pods Ready (two controller replicas); migrate, bootstrap, minio-init Complete |
| Certificates | 10/10 Ready; every service logged `TLS material loaded` (server and client roles) |
| mTLS end to end | agent registered over mTLS and connected to NATS with TLS; `hello_dag` submitted with `nexusctl` ran to SUCCEEDED |
| Controller observe-only | Lease held by one replica, both Ready; registry `plan` → effective mode `plan`, observed revision 1, no error; observed 4 HPAs, both scheduler Deployments, the Node |
| Controller RBAC | `kubectl auth can-i --as=<sa>`: update deployments/scale no, patch pods no, get secrets no, anything in another namespace no, list nodes yes |
| Apply guard | `--dry-run=server` with a wrong `expectedClusterUID` refused with the real UID in the message; with the right one the apply Role rendered |
| HPAs | 4 created; `ScalingActive` reported by the controller |
| Artifact GC | `kubectl create job --from=cronjob/artifact-gc` completed; DRY RUN summary, 0 deleted |
| Upgrade from the previous chart (origin/main, 0.1.0) | `--reuse-values` refused with the §20.2 advice; `--reset-then-reuse-values` upgraded in place, existing overrides kept (2 gateway replicas), PDBs and CronJob added |

Observed and not fixed here: an agent that starts before the gateway is
reachable exits and is restarted by the kubelet (2 restarts on a cold install)
instead of retrying its first registration; and Helm 4's `--wait` returned while
a pod was crash-looping, hence the readiness wait in §20.2.

Not verified: a managed database or object store, a multi-node or multi-zone
cluster (spread and PDB behaviour under a real drain), HPA scale-out under load,
apply mode against bound worker Deployments via the chart (the controller's own
tests cover apply, §12.6), a trust-manager bundle, and external agents over the
external gRPC Service.

### 20.9 Production checklist

- [ ] `image.tag` is a release tag or SHA whose eleven images exist in your
      registry, ideally referenced by digest; `image.pullPolicy: IfNotPresent`.
- [ ] `existingSecret` created from a secret manager, not a shell history; the
      Postgres password is URL-safe.
- [ ] `postgres.enabled=false` with a managed database, `sslmode: verify-full`
      with `postgres.caBundle` holding the provider's ROOT CAs (§20.11), and the
      provider's PITR (§4); or `postgres.pitr.enabled` with an off-cluster
      repository whose cipher passphrase is stored outside the cluster.
- [ ] `postgres.runtimeRoles.enabled` so no service connects as the owner
      (§20.12); the owner password is then used only by the three Jobs.
- [ ] Cloud identity bound to `serviceAccounts.{apiGateway,agent,artifactGC}`
      only (§20.13); GPU agents request their devices and sit on the GPU pool
      (§20.14); simulated GPUs never in production.
- [ ] `minio.enabled=false` with a managed bucket and `useTLS: true`; the
      artifact backup of §7 in place.
- [ ] `nats.auth.mode: nkey` with keys generated off-cluster (§11.7), and
      `nats.tls` if the network is not trusted.
- [ ] `tls.enabled` with `issuerRef` naming a ClusterIssuer whose CA key is
      outside the release namespace; `requireClientCert` and `requirePeerTLS`
      true; expiry alerts (`deploy/k8s/cert-alerts.yaml`, §9.3) loaded.
- [ ] At least 2 replicas (or an HPA with `minReplicas: 2`) for api-gateway,
      workflow-manager, dag-engine, event-service and resource-manager;
      2 schedulers per busy queue; PDBs on; `placement.topologySpread` on and a
      `priorityClassName` above batch workloads.
- [ ] metrics-server present if `autoscaling.enabled`; `metrics.serviceMonitor`
      on where prometheus-operator runs; the §17 alerts loaded.
- [ ] `artifactGC` in report mode, summaries read, then delete mode with a
      grace no shorter than your longest task; `pushgatewayUrl` set.
- [ ] Cluster controller observe-only first; apply only after a representative
      period of plan-only events (§12.3), with registry ceilings set.
- [ ] `mailpit.enabled=false`; `smtp.*` set with `publicUrl`.
- [ ] Rolling upgrades follow §6.3 and COMPATIBILITY.md: migrations are
      expand/contract, `make compat-smoke` passed for the release, upgrade with
      `-f` values, then wait on pod readiness.
- [ ] `deploy/helm/validate.sh` passes on your values file (copy it into
      `ci/`). Running `helm lint --strict -f` by hand is **not** enough on
      Helm 4 (§20.10); `helm template -f` must exit 0.
- [ ] After every install or upgrade, readiness and restart counts were
      checked as in §20.10, not only Helm's `--wait`.

### 20.10 Helm 4: what lint and `--wait` do not tell you

Two Helm 4 behaviours make its own exit codes weaker gates than they look.
`deploy/helm/validate.sh` (CI: GitHub job `helm`, Azure stage `Helm`) is built
around both. Helm and kubeconform are pinned and checksum-verified
(`deploy/scripts/pinned-tool.sh`), and so are the Kubernetes and CRD schemas
(`deploy/helm/schemas.lock`).

**`helm lint` does not fail on the chart's guards.** Every guard in the chart
(`existingSecret is required`, `confirmDelete=true`, the PDB and HPA checks, …)
is a template `fail`. Helm 4's lint logs such a `fail` at INFO
(`level=INFO msg="funcMap fail" message=...`) and still reports
`0 chart(s) failed`, exit 0. Helm 3 printed it as an `[ERROR]`. A lint-only gate
therefore passes values the chart would refuse at install time. Lint also does
not apply `key: null` overrides from `-f` files the way install and template
do. With `ci/external-deps-values.yaml` (which nulls
`podDisruptionBudget.maxUnavailable`), lint "fails" the PDB guard on every
service although the real render is fine. So:

- The gate is `helm template` with the same values and `--kube-version`. It
  coalesces values exactly as `helm install` does, and exits 1 with
  `execution error at (...): <message>` when a guard fires. `validate.sh`
  asserts that exit status and prints the error. It then checks the output with
  kubeconform. It still runs `helm lint --strict` for lint's own findings, and
  prints lint's `funcMap fail` lines as a note when the render passed.
- `validate.sh` proves the gate itself with a probe:
  `artifactGC.mode=delete` without `confirmDelete` must fail the render step,
  although `helm lint` exits 0 on it.
- By hand: `helm template nexus deploy/helm/kgf-nexus -f my-values.yaml
  --kube-version <your cluster's> >/dev/null && echo OK`. Treat lint's exit code
  as advisory.

**`helm install/upgrade --wait` can return while a pod is crash-looping.** Seen
in §20.8: the release reported `deployed` with an agent in `CrashLoopBackOff`.
`--wait` considers a Deployment ready once enough replicas were Ready at some
point, and a container that passes its probes briefly and then restarts
satisfies it. After every install or upgrade:

```sh
for d in $(kubectl -n nexus get deploy -o name); do
  kubectl -n nexus rollout status "$d" --timeout=5m || exit 1
done
kubectl -n nexus wait --for=condition=Ready pod \
  -l 'app.kubernetes.io/component notin (migrate,bootstrap,minio-init,artifact-gc)' --timeout=5m
# Then, a minute later: nothing restarting.
kubectl -n nexus get pods -o custom-columns='POD:.metadata.name,RESTARTS:.status.containerStatuses[*].restartCount,STATE:.status.containerStatuses[*].state.waiting.reason' \
  | awk 'NR==1 || $2 ~ /[1-9]/ || $3 ~ /CrashLoop|Error/'
```

A non-empty list after the header means the upgrade is not done. Read
`kubectl -n nexus logs <pod> --previous` before retrying or rolling back
(§6.3). An agent restarting once or twice on a cold install is the known
first-registration race noted in §20.8. More restarts than that, or any
restarting control-plane pod, is a failed rollout.

**Why the chart's bootstrap Job writes to the database.** The `bootstrap` Job
inserts the tenant row and the hashes of `worker-api-key` (and
`admin-api-key`, if set) from `existingSecret` directly into Postgres. This is
the one place a credential is created by SQL rather than by the API, because
nothing can call the API yet. The agents need a worker credential when they
first start, a fresh install has no user or token to authenticate a
`POST /tokens`, and self-registration (what `nexusctl up` uses locally) needs
e-mail verification, which a production install cannot automate. The Job uses
the chart's database credentials, is idempotent, and stores only SHA-256 hashes.
Once the release is up, mint further credentials, including rotated worker
tokens (role `worker`, scope `worker_rpc`, F114), through the console or
`POST /v1/namespaces/{ns}/tokens`, and revoke the bootstrap keys. Locally,
`nexusctl up` mints both of its credentials through the API and writes nothing
to the database (cli/README.md, "Local quick start").

### 20.11 Database TLS: verify-full and the CA bundle

`sslmode=require` encrypts but accepts any server that answers, so anyone who
can redirect the database's address (DNS, a compromised pod on the path)
collects every service's password. `verify-full` checks the certificate chain
against a CA the operator chose **and** that the certificate names the host in
the DSN.

```yaml
postgres:
  enabled: false
  host: nexus-pg.postgres.database.azure.com   # the name the certificate carries
  sslmode: verify-full
  caBundle: { secretName: nexus-postgres-ca, key: ca.crt }   # or configMapName
  params: { connect_timeout: "10" }                          # optional, every connection
  connections:
    scheduler: { params: { statement_timeout: "30000" } }    # optional, one connection
```

The bundle is mounted read-only at `/etc/nexus/postgres-ca/ca.crt` in every pod
that connects (services, their `wait-for-schema` init containers, the migrate,
bootstrap and db-roles Jobs, artifact-gc) and every `DATABASE_URL` carries
`sslmode=verify-full&sslrootcert=/etc/nexus/postgres-ca/ca.crt`. The Go
services (pgx) then trust exactly that file, verify the host name and have no
fallback; psql (libpq) in the Jobs does the same
(`internal/config/dsn_tls_test.go`). The chart refuses `verify-*` without a
bundle, a bundle with a non-verifying mode, both a Secret and a ConfigMap, TLS
modes against the in-cluster postgres (it serves none), and identity keys
(`user`, `host`, `sslmode`, `sslrootcert`, …) smuggled through `params`.

**What goes in the bundle: root CAs only, never an intermediate or the server
certificate** (providers rotate those unannounced). deploy/terraform supplies:

| Cloud | Bundle | Source and pinning |
|---|---|---|
| AKS (Flexible Server) | `ca-bundles/azure-postgres-roots.pem`: DigiCert Global Root G2 + Microsoft RSA Root CA 2017 | vendored; `ca-bundles/refresh.sh` pins each download's SHA-256 and each root's fingerprint; environments refuse a file that no longer matches `SHA256SUMS` |
| EKS (RDS) | `ca-bundles/rds-global-bundle.pem`: every RDS CA in every commercial region (the instance uses `rds-ca-rsa2048-g1`) | vendored, pinned the same way |
| GKE (Cloud SQL) | every CA the instance trusts (`google_sql_ca_certs`: current, upcoming, previous) | read from the Cloud SQL Admin API at plan time. The instance uses the shared CA hierarchy (`GOOGLE_MANAGED_CAS_CA`), whose server certificates carry the instance DNS name; a private Cloud DNS zone resolves that name to the private IP and it is the host Nexus uses. The per-instance CA puts no usable name in the certificate for private services access, so verify-full was impossible with it. |

**Rotation.** pgx reads `sslrootcert` when a pool is built, so the Go services
need a restart to pick up a new bundle; the Jobs read it per connection.

1. *New root announced / RDS bundle changed.* `ca-bundles/refresh.sh` fails
   its check. Review the change (a new root, not a removed one), update the pin
   in `refresh.sh`, `refresh.sh --write`, commit, `terraform apply` (updates the
   `<release>-postgres-ca` Secret), then
   `kubectl -n <ns> rollout restart deploy` so every pool trusts old **and** new.
   Only after that switch the server (RDS: `ca_cert_identifier`; Azure/Cloud SQL:
   the provider's rotation). Remove the old root in a later release.
2. *Cloud SQL CA rotation.* `gcloud sql ssl server-ca-certs create --instance …`
   (adds the upcoming CA), `terraform apply` (the bundle now has both), restart
   the Deployments, then `gcloud sql ssl server-ca-certs rotate`. Server
   *certificates* renew automatically in the maintenance window
   (`AUTOMATIC_ROTATION_DURING_MAINTENANCE`) and need nothing.
3. *Your own CA (self-managed Postgres).* Put old and new root in one bundle,
   restart, switch the server certificate, drop the old root later.

**Prove it** before relying on it: `TAG=<tag> deploy/scripts/db-tls-drill.sh`
starts a TLS-only Postgres with a private CA and checks, with the real images,
migrate (libpq), db-roles (pgx) and every service connecting over verify-full
as their own logins, and refusal of a wrong CA, a host name the certificate
lacks and plaintext. After an install: `SELECT usename, ssl, version FROM
pg_stat_activity JOIN pg_stat_ssl USING (pid)` shows every Nexus session.

### 20.12 Runtime logins: services never connect as the owner

Migration 0005 created one NOLOGIN group role per service carrying ADR-0011's
grants, but nothing created logins, so every service ran as the owner login —
on a managed server, the provider's admin (`azure_pg_admin`, `rds_superuser`,
`cloudsqlsuperuser` member) with DDL, CREATEROLE and every table.

```yaml
postgres:
  user: nexus                  # owner: migrate, bootstrap, db-roles Jobs only
  runtimeRoles:
    enabled: true
    existingSecret: nexus-db-logins   # keys: postgres-password-<component>
    scope: union
```

`nexus-db-logins` holds one URL-safe password (≥ 16 of `A-Z a-z 0-9 . _ ~ -`)
per `postgres.connections.<c>.passwordKey`: api-gateway, workflow-manager,
dag-engine, scheduler, resource-manager, event-service, and artifact-gc when the
CronJob is on. The **db-roles Job** (`nexus-db-roles` in the migrate image) runs
as the owner **inside the cluster** — the only place a managed server's private
endpoint is reachable, which is why Terraform does not run this SQL — and in
one transaction:

- creates each login `LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION
  NOBYPASSRLS`, or refuses to adopt an existing role with any of those;
- sets its password from a **SCRAM-SHA-256 verifier computed in the Job**, so
  the plaintext never reaches the server, its statement log or pgaudit;
- grants the `nexus_*` group roles of the chosen scope and revokes any other
  `nexus_*` membership; artifact-gc gets SELECT on every table plus default
  privileges for tables later migrations create, and nothing else.

Services wait in `wait-for-schema` until their own login works, so ordering
needs nothing extra. The owner password is then mounted only by the three Jobs.

`scope`:

- `union` (default): every login joins all six group roles — the union of the
  DML the platform grants, still with no DDL, no ownership, no TRUNCATE, no role
  management, and `run_events` still append-only. Verified live (§20.15).
- `service`: each login joins only its own group role, ADR-0011's write matrix
  enforced at the database. **Not production-ready**: the scheduler's timer
  claim (`UPDATE timers`) and dead-worker reaping (`DELETE FROM workers`) are not
  granted to `nexus_scheduler` (F132). Use it to find such paths.

**Rotation.** Put new passwords in the Secret (Terraform:
`runtime_db_password_version += 1`), bump `postgres.runtimeRoles.passwordRevision`
(Terraform does both). The db-roles Job re-runs under a new name and every
connecting pod rolls (the revision is a pod annotation). Existing sessions
survive a password change; each pod reconnects with the new one after its
restart. The owner password rotates as before (deploy/terraform README), and
now restarts only the Jobs' next run.

### 20.13 ServiceAccounts and cloud identity

Every Nexus pod runs under its own ServiceAccount (`nexus-api-gateway`,
`nexus-scheduler`, `nexus-agent`, `nexus-artifact-gc`, `nexus-jobs`, …), never
`default`, with **no API token mounted** (`automountServiceAccountToken:
false`); none of them calls the Kubernetes API. An agent whose `executors`
include `kubernetes` defaults to a mounted token (it launches task Jobs; bind
its RBAC yourself). The cluster controller keeps its own RBAC-bound account.

A cloud identity is bound by naming one account for the components that use
the result store — api-gateway (task logs), workflow-manager (object-store
sensors, §27), agents, artifact-gc — and nothing
else. The chart creates a shared account once and refuses two components
naming it with different settings:

```yaml
serviceAccounts:
  apiGateway: &artifacts
    name: nexus-artifacts
    annotations: { eks.amazonaws.com/role-arn: arn:aws:iam::<acct>:role/<name>-nexus-artifacts }
    # GKE: { iam.gke.io/gcp-service-account: <gsa>@<project>.iam.gserviceaccount.com }
    # AKS: { azure.workload.identity/client-id: <id> }
  workflowManager: *artifacts
  agent: *artifacts
  artifactGC: *artifacts
```

IRSA, EKS Pod Identity and Azure Workload Identity inject their own projected
token and GKE uses the metadata server, so all work with the API token
unmounted. The Terraform environments set exactly this, with the platform
modules' trust (`nexus_service_account = "nexus-artifacts"`); under EKS Pod
Identity the association names the account and no annotation is needed.

These are the accounts the result-store clients authenticate with (§26):
`resultStore.s3.credentials` `web-identity`/`pod-identity`/`auto`,
`resultStore.azure.auth=workload-identity` and `resultStore.gcs.auth=auto` all
take the identity bound here. With `resultStore.backend=azblob` and
`auth=workload-identity` the chart adds the pod label
`azure.workload.identity/use: "true"` to exactly these four components (the
webhook injects the federated token only into labelled pods), and it refuses
to create their accounts without the `azure.workload.identity/client-id`
annotation; `s3.credentials=web-identity` likewise requires
`eks.amazonaws.com/role-arn`. With `serviceAccounts.create=false` the accounts
are yours and are not inspected. There is no separate result-store
ServiceAccount setting: one identity model for every pod.

### 20.14 Agent pools and GPUs

An `agents[]` entry may set `nodeSelector`, `tolerations` and `affinity`; each
replaces the global `placement.*` value for that agent. With `gpuCount > 0` the
agent pod **requests `gpuCount` devices** of `gpu.resourceName`
(`agentDefaults.gpuResourceName`, `nvidia.com/gpu`) in requests and limits, and
tolerates the `<resourceName>:NoSchedule` taint the deploy/terraform GPU pools
carry. So what the agent advertises (`NEXUS_GPU_COUNT`, `--gpu-kind`,
`--gpu-memory-bytes`) is what the kubelet allocated and mounted, and task
subprocesses see those devices. An explicit device limit different from
`gpuCount` is refused. `gpu: { requestDevices: false }` advertises without
allocating — **simulated** capacity — and the chart then requires the label
`simulated=true` (the dev environment's agents use it).

```yaml
agents:
  - name: agent-gpu
    queues: gpu
    labels: zone=cluster,accelerator=nvidia
    cpuMillis: 3000
    memoryBytes: 10737418240
    gpuCount: 1
    gpuKind: nvidia-l4
    gpuMemoryBytes: 25769803776
    diskBytes: 161061273600
    nodeSelector: { nexus.khuriwalgroup.com/pool: gpu }
    resources: { requests: { cpu: 500m, memory: 1Gi }, limits: { memory: 12Gi } }
```

The AKS/EKS/GKE environments add exactly such an agent (variable `gpu_agent`)
when their GPU pool is enabled, next to a CPU agent pinned to the cpu pool.
Size the agent inside one GPU node. GPU execution itself is still unqualified
on real hardware (production brief).

### 20.15 What was verified for F115 (2026-09-26, docker-desktop)

| Check | Result |
|---|---|
| `deploy/helm/validate.sh` | 5 values files lint and validate strictly (57, 83, 47, 48, 58 objects); 33 refusals with the expected message, 17 of them new (TLS without a bundle, bundle without verification, TLS on in-cluster postgres, reserved params, owner as runtime login, shared login, owner password key, bad scope and login names, GPU limit mismatch, unlabelled simulated GPUs, bad resource name, conflicting shared ServiceAccount, the controller's account, a misspelt key) |
| `deploy/scripts/db-tls-drill.sh` (containers `nexus-w3c-drill-*`, port 55791) | 18/18: migrate (libpq) and db-roles (pgx) over verify-full; the six services as their own logins with TLSv1.3 sessions and event-service `/readyz` 200; wrong CA, wrong host name and plaintext refused (libpq, pgx and a running service, which stays unready); runtime login refused DDL |
| `internal/dbroles` integration (migrated Postgres 16) | logins authenticate with client-side verifiers; attributes and connection limit; own-table writes allowed, other writes, DDL and `run_events` DELETE refused; read-only login reads later tables and writes none; rotation invalidates the old password and moves membership; privileged or missing roles refused with no partial change |
| Helm install, namespace `nexus-w3c-helm`, images `w3c-f115a` | external TLS-only Postgres with a private CA in the namespace; `verify-full`, `runtimeRoles` (union), a shared `nexus-artifacts` account for gateway/agents/GC, a simulated GPU agent. All pods Ready; migrate, bootstrap, db-roles Complete; `hello_dag` SUCCEEDED; every service session in `pg_stat_ssl` was TLSv1.3 as its own `*_login`, none as the owner; artifact-gc ran as the read-only login under `nexus-artifacts` with no token; from a pod, a rogue CA, a wrong host name, a bare IP and plaintext were refused and the runtime login could neither `CREATE TABLE` nor `TRUNCATE run_events` |
| `scope: service` on the same release | `hello_dag` SUCCEEDED, but the scheduler's timer claim and dead-worker reaping were denied (F132) |
| `make tf-validate` / `make tf-lint` | all modules and environments valid, mocked-plan tests pass (new: CA Secret, runtime logins, non-PEM refusal, Cloud SQL shared CA and private DNS, GPU agent on the GPU pool, pinned bundle); tflint clean, checkov 296 passed / 0 failed, trivy clean |

Not verified: any managed database (no cloud apply, F115 part 6), in
particular that Cloud SQL's `google_sql_ca_certs` returns the shared-CA root
and that the PSA DNS name resolves through the private zone; real GPU
scheduling (no GPU node); IRSA/Workload Identity tokens reaching the object
store (the client change is separate).

---

## 21. Namespace quotas and usage metering

Design: [ADR-0021](adr/0021-namespace-quotas-and-usage-metering.md).
Migration 0027. Findings F122 (measured metrics were dropped) and F123 (quotas
were documented, not implemented).

### 21.1 What a quota does

| Limit (`quotas.*`) | Enforced | What happens when it is reached |
|---|---|---|
| `max_inflight_attempts` | in the dispatch claim | The attempt stays PENDING with `pending_reason = quota_exceeded: inflight_attempts (used X of Y)` and runs when one of the namespace's attempts ends. |
| `max_cpu_millis`, `max_memory_bytes`, `max_gpu_count`, `max_custom.<name>` | in the dispatch claim | Same, with the dimension named (`cpu_millis`, `memory_bytes`, `gpu_count`, `custom:<name>`). An attempt that does not request a resource is never held back by that resource's quota. If ONE attempt asks for more than the limit, the reason adds `request R exceeds the quota`: it will never run until the quota is raised. |
| `max_active_runs` | at run creation | `POST .../runs` answers `429 namespace_quota_exceeded` (`dimension: active_runs`). |
| `max_runs_per_hour` | at run creation (sliding 60 min, database clock) | `429 namespace_quota_exceeded` with `Retry-After`. |
| `storage_soft_limit_bytes` | never (reported) | Shown against measured storage. Measured = durable task-log bytes only; result-store artifacts are not metered yet. |

**0 or absent means unlimited**, and every namespace starts unlimited, so
applying 0027 changes nothing until someone sets a quota. Quotas count
**requested** resources of DISPATCHED and RUNNING attempts on every queue of the
namespace, not measured use. A schedule tick refused by a run quota is not
lost: it is retried on the next scheduler pass, as for `max_active_runs` on a
workflow. A held-back attempt spends no retry budget; if it has a
`schedule_to_start` deadline, that deadline still runs and can time it out,
which is the correct signal that the namespace is under-provisioned.

### 21.2 Setting quotas

```sh
# Read (quotas.view): limits, current usage per dimension, and the revision.
curl -s -H "Authorization: Bearer $TOKEN" $API/v1/namespaces/$NS/quotas

# Replace (quotas.manage). The revision is required: a stale one gets 409.
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  $API/v1/namespaces/$NS/quotas -d '{
    "revision": 4,
    "quotas": {"max_inflight_attempts": 200, "max_cpu_millis": 256000,
               "max_memory_bytes": 1099511627776, "max_gpu_count": 8,
               "max_custom": {"license": 10},
               "max_active_runs": 50, "max_runs_per_hour": 600,
               "storage_soft_limit_bytes": 0, "locked": false}}'
```

The console has the same under **Govern → Quotas & usage**. Quotas share the
namespace platform policy's revision: saving quotas makes an open
Infrastructure editor's next save answer 409 (reload), and vice versa. The
Infrastructure route keeps stored quotas when a client omits them and refuses
(400) a request that changes them.

**Locking.** Quotas start unlocked: a namespace administrator with
`quotas.manage` can set and change them. A platform operator (a session of a
user in `platform_operators`) can set `"locked": true`; after that a namespace
administrator may only lower limits or add new ones (403 `quotas_locked`, with
the list of what would have been loosened). Only an operator can lock, unlock
or loosen. Use the lock for tenants whose limits are part of a commercial plan.

Every accepted change writes a `quota.update` audit row with `before`,
`after`, the new revision and, when applicable, what was loosened.

### 21.3 Permissions

| Permission | Grants | Default roles |
|---|---|---|
| `quotas.view` | GET quotas | Viewer, Workflow Operator/Developer, Infrastructure Operator, Auditor, Administrator; legacy reader keys |
| `quotas.manage` | PUT quotas | Infrastructure Operator, Administrator; legacy admin keys |
| `usage.view` | GET usage | Infrastructure Operator, Auditor, Administrator; legacy admin keys (not reader keys: usage is cost data) |

`GET /v1/platform/usage` (every namespace, one bucket each) needs a platform
operator session.

### 21.4 Usage ledger

`usage_ledger` has one row per attempt that held a reservation, written in the
same transaction as the attempt's terminal transition: worker report
(SUCCEEDED, FAILED, TIMED_OUT, LOST on INDETERMINATE), lease expiry or worker
death (LOST), cancel of a DISPATCHED/RUNNING attempt (CANCELLED). An attempt
that never dispatched has no row. `UNIQUE(attempt_id)` makes duplicates
impossible. Owned by the scheduler (INSERT only); every service can read it;
no service can UPDATE or DELETE it, and a trigger refuses UPDATE even for the
table owner. Rows are not foreign-keyed to runs, so deleting a run keeps its
usage.

Quantities: `reserved_seconds` = ended − dispatched (what the reservation
held); `cpu_core_seconds` = cpu_millis/1000 × reserved_seconds;
`memory_gib_seconds` = memory_bytes/2^30 × reserved_seconds; `gpu_seconds` =
gpu_count × reserved_seconds; `wall_seconds` = ended − started. All exact
`NUMERIC`. `measured_*` hold the worker's ExecutionMetrics when the attempt
ended by a worker report, NULL otherwise.

```sh
# Rollups (usage.view). group_by: day | queue | pool | worker | executor | outcome | none.
# from/to: RFC 3339 or YYYY-MM-DD (default: last 30 days); range <= 400 days;
# limit <= 500; follow next_cursor.
curl -s -H "Authorization: Bearer $TOKEN" \
  "$API/v1/namespaces/$NS/usage?group_by=day&from=2026-09-01&to=2026-10-01"
```

An attempt counts in the UTC day it **ended**; a long attempt crossing
midnight is attributed wholly to the later day.

### 21.5 Export to a billing system

Two paths, both at-least-once; **deduplicate on `attempt_id`**:

- **Push.** Each ledger row writes an outbox row (topic `usage_recorded`) in
  the same statement. The event-service relay publishes it to
  `nexus.usage.recorded.<namespace>` in stream `NEXUS_USAGE` (file storage, 30
  days, 4 GiB). The message carries `ledger_id`, `attempt_id`, tenant,
  namespace, run, outcome, queue, pool, `ended_at` and the four derived
  quantities. The relay's NATS identity needs publish on `nexus.usage.>` and
  the JetStream INFO/CREATE API for `NEXUS_USAGE`; both renderings
  (`deploy/nats/nexus-nats-auth.sh`, the Helm `nats-config.yaml`) grant it.
  Re-render the broker authorization when upgrading: an old authorization makes
  every usage publish fail, the rows stay unpublished in the outbox, and
  nothing else is affected.
- **Pull.** `internal/metering` (`Exporter`, `Cursor`, `Pump`) reads the
  ledger by id after a persisted cursor. Rows younger than 30 s are held back
  so a transaction that committed late cannot be skipped. The ledger is the
  record of truth; replay the bus from it.

### 21.6 Retention

The ledger grows by one row per dispatched attempt (≈ 400 bytes with
indexes). There is no automatic pruning: it is billing data. Export first, then
delete as the database owner:

```sql
\copy (SELECT * FROM usage_ledger WHERE ended_at < '2026-01-01') TO 'usage-2025.csv' CSV HEADER
DELETE FROM usage_ledger WHERE ended_at < '2026-01-01';   -- as the table owner
```

The down migration drops the ledger. Export it before any rollback.

### 21.7 Metrics and alerts

| Series | Meaning |
|---|---|
| `nexus_scheduler_quota_blocked_total{queue,dimension}` | Claims that held an attempt back for a quota. Counted per examination, so one waiting attempt counts once per claim that looks at it; use `rate()` as "namespaces are pressing their quotas", not as a count of attempts. Custom resources are one label value, `custom`. |
| `nexus_scheduler_usage_ledger_writes_total{queue,outcome}` | Ledger rows written. Should track `rate(nexus_scheduler_attempts_dispatched_total)` over hours; a sustained gap means terminal transitions are failing. |

Run-level refusals are HTTP 429 with `error: namespace_quota_exceeded` in the
gateway's request metrics and logs.

Suggested alert: `sum(rate(nexus_scheduler_quota_blocked_total[15m])) > 0`
for 1 h on a namespace that should not be quota-bound, and
`rate(nexus_scheduler_usage_ledger_writes_total[1h]) == 0` while
`rate(nexus_scheduler_attempts_dispatched_total[1h]) > 0`.

### 21.8 Cost on the dispatch path, and rollback

A namespace **without** concurrent quotas pays one indexed lookup per claim
(returning no row) and takes no lock. A namespace **with** them serializes its
own dispatches across replicas: one claim transaction at a time per namespace
holds its quota lock, and on the push path a namespace being claimed by another
replica is skipped for that cycle. Each quota-bound claim reads the namespace's
in-flight rows through `idx_attempts_inflight_tenant`, O(in-flight attempts
of that namespace).

**Measured** (`tests/bench`, `TestDispatchCeiling`; laptop, Postgres 16.15 · client GOMAXPROCS=14 · 64 workers;
64 workers; 1,500 dispatches per cell, 500 at the 1,000 backlog). "Before" is
`origin/main` built from a clean export; "after" is this branch, once with the
namespace unlimited and once with quotas set far above what the cell reaches
(`NEXUS_BENCH_QUOTA=true`), so every claim takes the full quota path without
being refused. Both against the same database with 0027 applied, three
builds alternated, two rounds, mean of the two:

| mode | backlog | replicas | before: claims/s | before: p50 / p99 | after, unlimited ns: claims/s | p50 / p99 | after, quota-bound ns: claims/s | p50 / p99 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| pull | 1000 | 1 | 100 | 8.9 / 30.4 ms | 162 | 5.6 / 14.2 ms | 144 | 6.1 / 20.0 ms |
| pull | 1000 | 4 | 432 | 8.9 / 28.7 ms | 522 | 6.7 / 27.7 ms | 148 | 24.0 / 70.2 ms |
| pull | 10000 | 1 | 184 | 4.8 / 13.6 ms | 159 | 5.2 / 22.6 ms | 142 | 6.3 / 17.2 ms |
| pull | 10000 | 4 | 485 | 6.6 / 50.5 ms | 509 | 6.8 / 22.9 ms | 138 | 28.5 / 61.0 ms |
| push | 1000 | 1 | 105 | 8.1 / 29.3 ms | 108 | 8.6 / 23.5 ms | 102 | 8.9 / 19.1 ms |
| push | 1000 | 4 | 179 | 19.0 / 54.7 ms | 169 | 20.0 / 59.8 ms | 108 | 23.1 / 97.7 ms |
| push | 10000 | 1 | 26 | 35.0 / 83.2 ms | 26 | 36.4 / 95.7 ms | 26 | 35.6 / 115.0 ms |
| push | 10000 | 4 | 64 | 50.2 / 185.2 ms | 55 | 59.6 / 238.3 ms | 51 | 61.4 / 252.8 ms |

Reading it: for an **unlimited** namespace the differences are inside the
run-to-run noise of this machine (single cells moved by 30–60 % between rounds
of the same build); the one extra lookup is not visible against the ADR-0018
candidate sort. For a **quota-bound** namespace the pull path with several
replicas is capped at about 140 dispatches/s for that namespace, and p50
includes the wait for the namespace lock: that is the serialization the
design chooses (ADR-0021, Negative), and it does not apply to other
namespaces. The bench never completes work, so the quota path's usage read
covered up to 1,500 in-flight attempts by the end of each cell, which is a
pessimistic case for the O(in-flight) read. Reproduce with
`make bench-pg`, `NEXUS_BENCH_QUOTA=true make bench-dispatch`.

**Upgrade.** Apply 0027 (`CREATE INDEX CONCURRENTLY`, re-runnable), then roll
services. A scheduler started before 0027 logs `namespace quotas are not
enforced` / `usage metering is off` and runs as before; restart it after the
migration. Attempts that end during that window have no ledger row.

**Rollback.** `NEXUS_QUOTAS=false` (or `-quotas=false`) on the scheduler stops
claim-side enforcement without a deploy of new code; run-level quotas stop when
the quotas are set to 0. Metering cannot be switched off; roll back the
scheduler to stop it. Before reverting 0027, export the ledger (§21.6).

---

## 22. Third-party images and toolchains: what is pinned, and why

Every image below is pinned; a floating tag is a build that changes under you.
Digest pins (`name:tag@sha256:...`) are used where the upstream's publication
history has already broken us once. The tag stays beside the digest for humans;
the digest is what Docker and Kubernetes actually resolve.

| Component | Reference | Where |
|---|---|---|
| S3 result store and backup store (server) | `docker.io/pgsty/silo:RELEASE.2026-09-16T00-00-00Z@sha256:635197cb9f36d01bee221d34d1c7d7960f6a95c48b0b6c01d99cd13bdae51a46` (multi-arch index: amd64 + arm64) | `deploy/docker-compose.yml` (`minio`), `deploy/docker-compose.pitr.yml` (`backup-store`, `NEXUS_PITR_MINIO_IMAGE`), Helm `minio.image`, Makefile `S3_IMAGE` (bench, CI) |
| `mc` bucket-init jobs | the **same** Silo image (it bundles a version-matched `mc`) | compose `minio-init`, `backup-store-init` (`NEXUS_PITR_MC_IMAGE`), Helm `minio.mcImage` |
| Console build | `node:24.21.0-alpine3.24@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1` | `deploy/Dockerfile.ui`; CI uses Node `24.21.0` (`.github/workflows/ci.yml`, both Azure pipelines) |

### 22.1 The S3 store: why Silo, and not MinIO

MinIO Inc. stopped publishing community-edition binaries and container images
in October 2025, removed `minio/minio` and `minio/mc` from Docker Hub, then made
the `quay.io/minio/minio` and `quay.io/minio/mc` repositories private
(unauthenticated pulls answer **401**, reproduced 2026-09-25), and archived
`github.com/minio/minio` as "no longer maintained". The last source tag,
`RELEASE.2025-10-15T17-29-55Z`, fixed a privilege-escalation CVE; nothing after it
will be fixed. The options assessed (CONTRACT-FINDINGS.md F111):

| Option | Verdict |
|---|---|
| `quay.io/minio/aistor/minio` (MinIO AIStor) | Rejected as the default: proprietary MinIO Software License; even the free tier needs a license key and is single-node. Fine for an operator who buys it: set `minio.image`. |
| Build the last upstream source tag ourselves | Rejected: reproducible, but frozen on an archived, unpatched codebase, and we would own the build. |
| Chainguard `cgr.dev/chainguard/minio` | Rejected: free tier is `latest` only (versioned tags are paid), no `mc` image. |
| Bitnami | Rejected: Bitnami's free catalogue moved to `bitnamilegacy` without updates in 2025. |
| **`pgsty/silo` (community fork, formerly `pgsty/minio`)** | **Chosen.** AGPL-3.0 like upstream; S3 API, `MINIO_*` variables, `.minio.sys` on-disk format and the `server` argv are kept (the entrypoint maps `minio`/`server` to `silo`); released every 1-2 months with public advisories; images built in GitHub Actions from verified release binaries, with SPDX SBOMs and a **GitHub/Sigstore SLSA build-provenance attestation per image**. |

Residual risk, accepted knowingly: Silo is a community fork maintained by one
organisation (PGSTY), with no commercial support and a history of renaming
(`pgsty/minio` → `pgsty/silo`). The digest pin means a compromised or deleted tag
cannot change what we run; the attestation check means a new digest is only
adopted when it was built by `pgsty/silo`'s own release workflow. If the fork
stalls, the exits are AIStor (licensed) or another S3 store; nothing in Nexus is
MinIO-specific beyond `mc` in the init jobs and the health-probe paths.

Production note: none of this changes the production guidance. A managed S3
store (or a supported object store) is preferred to a single-replica
in-cluster Silo; the chart's `minio.enabled: false` + `minio.endpoint` path is
unchanged.

### 22.2 Bumping the S3 image

```sh
# 1. find the new release and its multi-arch INDEX digest
docker buildx imagetools inspect docker.io/pgsty/silo:RELEASE.<new>
# 2. set S3_IMAGE in the Makefile, then check its provenance
make verify-s3-image            # gh attestation verify ... --repo pgsty/silo
# 3. replace the same reference in deploy/docker-compose.yml,
#    deploy/docker-compose.pitr.yml and deploy/helm/kgf-nexus/values.yaml
git grep -n 'pgsty/silo'        # every hit must show the new digest
# 4. run the object-store integration tests against it (tests3, §8)
```

CI runs `make verify-s3-image` before starting the store the integration job
tests against (GitHub Actions).

### 22.3 Node

Node 20 reached end of life on 2026-04-30. The console builds and CI run on
Node **24** ("Krypton"), Active LTS until 2026-10-20 and maintained until
2028-04-30 (github.com/nodejs/Release, `schedule.json`). The exact patch
(`24.21.0`) is pinned in all three CI definitions and in the Dockerfile, and
they move together. Node 26 becomes LTS on 2026-10-28; move once it has.

---

## 23. Cost management: moved to KGF billing

Cost management (price books, budgets and cost reporting) is not part of
Nexus: inside KGF Cloud it belongs to KGF billing, which prices usage for
every KGF service alike (docs/KGF-INTEGRATION.md). Nexus meters and exports
the usage it is authoritative for (the scheduler-owned usage ledger and its
export, §21) and enforces namespace quotas; it keeps no prices, currencies or
budgets. This section number is kept so the references to §24 and later stay
stable.

---

## 24. Running Nexus inside KGF Cloud: context authentication, tenancy mapping and service accounts

**Design:** [ADR-0024](adr/0024-nexus-inside-kgf-cloud.md). **Migration:**
`0032_kgf_context_and_service_accounts`. **Verified:** `kgfctx` unit tests
(including a conformance vector minted by kgf-cloud's own `pkg/tenantctx`
signer and re-minted byte for byte), `authn` unit tests of the action mapping,
and the PostgreSQL integration suite (`TestIntegrationKGF*`,
`TestIntegrationServiceAccountsAndTokenCeilings`,
`TestIntegrationUsersCursorPagination`). **Not verified:** a live kgf-gateway
forwarding to Nexus; the tests mint contexts in the gateway's format but do
not run kgf-cloud's processes.

In KGF Cloud, kgf-identity owns people and kgf-gateway authenticates every
request, then forwards it to the provider with a freshly signed Ed25519
`X-KGF-Context` (`kgf-cloud/docs/platform-services.md`). In KGF mode Nexus
trusts that context instead of its own passwords and sessions.

### 24.1 Modes

| `NEXUS_AUTH_MODE` | People | Machine credentials | `/v1/auth/login`, `/register`, email flows |
| --- | --- | --- | --- |
| `standalone` (default) | Nexus sessions | API keys, scoped and service-account tokens | served |
| `kgf` | KGF context only; Nexus sessions refused | yes | **not served** (404) |
| `both` | KGF context or Nexus session | yes | served |

The worker gRPC listener is the same in every mode. Use `both` only while
migrating a standalone installation into KGF; `kgf` is the target.

Requests that would confuse identity are refused, never guessed at:

| Request | Answer |
| --- | --- |
| `X-KGF-Context` **and** `Authorization` (kgf, both) | `400 ambiguous_credentials` |
| `X-KGF-Context` in `standalone` without KGF keys | `400 kgf_context_not_accepted` |
| `X-KGF-Context` with a Nexus credential in `standalone` **with** KGF keys | accepted: the credential authenticates and authorizes; the context, if it verifies, only attributes the audit row (§25.2) |
| Nexus session (`nxsess_…`) in `kgf` | `401` |
| Context with no subscription or resource group | `403 kgf_scope_required` |
| Pure KGF service context not listed in `NEXUS_KGF_SERVICE_PRINCIPALS` | `403 kgf_service_not_allowed` |
| Any verification failure (signature, key, expiry, audience, issuer, replay) | one opaque `401`; the reason is logged at INFO (`kgf context refused`) |

### 24.2 Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `NEXUS_AUTH_MODE` | `standalone` | `standalone`, `kgf` or `both`. Anything else stops startup. |
| `NEXUS_KGF_CONTEXT_KEYS_URL` | — | kgf-gateway's key document, e.g. `http://kgf-gateway.kgf-platform.svc:18080/.well-known/kgf-context.json`. |
| `NEXUS_KGF_CONTEXT_PUBLIC_KEYS` | — | Alternatively, static base64url Ed25519 public keys, comma separated (the value of the gateway's `key` field). Exactly one of the two is required in `kgf`/`both`; in `standalone` either one turns on audit attribution only (§25.2). |
| `NEXUS_KGF_CONTEXT_KEYS_REFRESH_SEC` | `300` | Periodic refresh of the key document. |
| `NEXUS_KGF_AUDIENCE` | `kgf-nexus` | The audience kgf-gateway mints for Nexus: `kgf-` + the manifest `id`. Must match the name Nexus is registered under. |
| `NEXUS_KGF_USER_ISSUERS` | `kgf-gateway` | Issuers allowed to carry a human principal. Add a KGF facade service here only if it delegates (`obo`) to Nexus. |
| `NEXUS_KGF_SERVICE_PRINCIPALS` | — | KGF services that may call Nexus for themselves, with the role they act as: `svc:kgf-billing=viewer,svc:kgf-orchestrator=operator`. Unlisted services are refused. |
| `NEXUS_KGF_ACCEPT_ANY_AUDIENCE` | `false` | Accept tenantctx's wildcard audience `*`. Only for KGF's single-process local profile. |

Helm: the `kgf:` values block (`authMode`, `contextKeysUrl`,
`contextPublicKeys`, `audience`, `userIssuers`, `servicePrincipals`) sets
these on the api-gateway; the schema rejects an unknown mode. Compose passes
`NEXUS_AUTH_MODE`, `NEXUS_KGF_CONTEXT_KEYS_URL` and
`NEXUS_KGF_CONTEXT_PUBLIC_KEYS` through.

**Startup and readiness.** The gateway logs `authentication mode: KGF
context` with the audience, issuers and key source. With a key URL it tries a
first fetch; if kgf-gateway is not up yet it logs a WARN and starts, and
`/readyz` fails (`kgf_context_keys`) until a key is held, so the load balancer
does not send it traffic it would refuse.

**Registering Nexus in KGF.** kgf-gateway routes `KGF.Nexus` to the manifest's
`internalURL`. Today `kgf-cloud/services/nexus` (the simulation provider) owns
that registration. Pointing KGF at this gateway is a kgf-cloud change: the
manifest endpoint (or a facade that delegates with
`tenantctx.Signer.DelegateFrom` to audience `kgf-nexus`) and the actions in
24.6. Nothing in this repository changes kgf-cloud.

### 24.3 Key rotation

kgf-gateway publishes `{"keys":[{"kid","alg":"EdDSA","key"}]}` with
`kid = "kgf-" + hex(sha256(key)[:6])`. Nexus:

- refetches immediately when it sees an unknown `kid` (at most once per 30 s),
  so a new gateway key is trusted on its first token;
- keeps a key that left the document for 5 minutes (the maximum context
  lifetime), so tokens minted just before the rotation still verify;
- ignores a published key whose `kid` is not its derived id;
- keeps its keys through a failed refresh.

To rotate: deploy the gateway with the new key; nothing is needed on Nexus.
With static keys, add the new key to `NEXUS_KGF_CONTEXT_PUBLIC_KEYS` first,
roll the gateway, then remove the old one after 5 minutes.

### 24.4 Tenancy mapping

Each KGF (organization, subscription, resource group) is one Nexus tenant
namespace, created on the first request that names it:

```
namespace = "kgf-" + hex(sha256("kgf-nexus/v1" \0 org \0 sub \0 rg))[:32]      (lower-cased)
```

`whoami` reports it together with the KGF identity. Read the mapping, or find
a namespace's KGF scope, with:

```sql
SELECT namespace, organization, subscription, resource_group, created_at
  FROM kgf_scopes WHERE organization = 'org-acme' ORDER BY created_at;
```

Every request's path namespace must equal the caller's mapped namespace
(`/v1/namespaces/<ns>/…`); another is `403`. Two organizations with equal
subscription and group names get different namespaces. A tenant with the
derived name that was not created by this mapping is refused
(`namespace_conflict` in the log), never adopted. Deleting a KGF resource
group does not delete its Nexus tenant; its data follows Nexus's retention.

### 24.5 Authorization

The context's role (the delegated person's, for an `obo` context) becomes
KGF's built-in action set (operator = Contributor, viewer = Reader), and a
Nexus permission is held when its KGF action is in that set:

| KGF role | Holds |
| --- | --- |
| `operator` | every permission in the table in ADR-0024 §4: pipelines, runs (create, control, delete), schedules, backfills, workers (drain, quarantine), infrastructure, quotas, usage, audit, roles, service accounts, tokens |
| `viewer` | every `…/read` action: workflows, runs, task metadata, **logs and results**, schedules, backfills, workers, infrastructure, quotas, usage, audit, roles, service accounts |
| either | **never** `users.view` or `users.manage`: KGF owns people |
| either | **never** `audit.manage` (retention, legal hold, purge, SIEM sinks; §25.2): changing what evidence is kept must not ride on a Contributor wildcard. It stays with the platform owner's Nexus machine credential (an admin API key, or a service account an administrator bound) |

A viewer reading task logs and results follows KGF's Reader ("view every
resource"); Nexus's own Viewer preset is narrower. Permissions not in the
table are denied.

### 24.6 KGF.Nexus actions to declare in kgf-cloud

The manifest declares `KGF.Nexus/pipelines/{read,list,write,delete,run}`.
Nexus also evaluates these, which kgf-cloud should add to
`services/nexus/manifest.json` so custom KGF roles can name them:

```
KGF.Nexus/pipelines/cache/action
KGF.Nexus/pipelines/runs/{read,delete}
KGF.Nexus/pipelines/runs/{logs,results}/read
KGF.Nexus/pipelines/runs/{cancel,pause,resume,retry}/action
KGF.Nexus/schedules/{read,write,delete}
KGF.Nexus/schedules/backfills/{read,write}
KGF.Nexus/workers/read
KGF.Nexus/workers/{drain,quarantine}/action
KGF.Nexus/{infrastructure,quotas}/{read,write}
KGF.Nexus/{usage,auditEvents}/read
KGF.Nexus/{roles,serviceAccounts}/{read,write}
KGF.Nexus/notifications/{read,write}
KGF.Nexus/{triggers,sensors}/{read,write,delete}
KGF.Nexus/assets/read
```

### 24.7 Audit and support references

Every audit row written for a KGF principal also carries the chained KGF
columns of §25.2 (`kgf_organization`, `kgf_subscription`,
`kgf_resource_group`, `kgf_principal`, `kgf_on_behalf_of`), taken from the
context that authenticated it; `request_id` is one column, added by 0032 and
shared with §25.2. It has `actor_kind = 'kgf'`,
`actor_label` = the KGF principal (the person, for a delegated context),
`request_id` = the KGF request id, and `detail.kgf` = organization,
subscription, resource group, role, issuer, token id and, for a delegated
context, `via` (the acting service). Responses carry `X-Request-ID` with the
same value. To resolve a KGF support reference:

```bash
curl -H "X-KGF-Context: …" "$NEXUS/v1/namespaces/$NS/audit?request_id=<id>"
```

```sql
SELECT occurred_at, action, outcome, actor_label, detail->'kgf'
  FROM audit_log WHERE request_id = '<id>';   -- indexed
```

### 24.8 Service accounts for agents and CI

Machine credentials stay Nexus's: **service accounts** hold role bindings, and
their tokens carry a permission ceiling (`POST …/tokens` with
`service_account_id` and `permissions`). Effective grants are the account's
current roles ∩ the ceiling, re-read on every request; disabling the account
refuses its tokens at once. In KGF mode an **operator** manages service
accounts, custom roles and tokens for its resource group; a binding or ceiling
can hold only permissions the operator holds, never an identity-administration
grant (`users.manage`, `roles.manage`, `tokens.manage`,
`service_accounts.manage`), and an unceilinged token can be minted only by a
Nexus administrator. Grant-bearing tokens never speak the worker protocol.

`GET /v1/namespaces/{ns}/users?limit=&cursor=&q=` is keyset-paginated (at most
200 per page, server-side search); in KGF mode it is denied (KGF owns people).

### 24.9 Mixed versions, upgrade and rollback

Apply `0032_kgf_context_and_service_accounts` (no-transaction: it builds the
`audit_log.request_id` index `CONCURRENTLY`; re-run it if interrupted), then
deploy the gateway. During a rolling upgrade an old replica ignores the new
columns: a service-account token is stored with an empty scope list, so an old
replica grants it nothing; a ceilinged token is stored with only the coarse
scopes its ceiling implies. An old replica does not understand
`X-KGF-Context`: switch `NEXUS_AUTH_MODE` to `kgf` only once every replica
runs the new gateway.

Rollback: set `NEXUS_AUTH_MODE=standalone`, roll the gateway back, then (only
if required) apply the down migration after exporting `kgf_scopes` and
`service_accounts`. KGF-provisioned tenants remain as ordinary tenants.

### 24.10 Known limits

- The replay guard is per replica (as in kgf-cloud's tenantctx); the audience
  check and the one-minute lifetime bound the window.
- Authorization follows the flat role in the context. A KGF custom role
  narrower than Contributor or Reader is enforced by kgf-gateway's coarse
  check, not re-derived here, until contexts carry assignments.
- The per-replica provisioning cache means the first request for a new scope
  on each replica runs one small transaction.

---

## 25. Task secrets, the audit trail, and air-gapped installation

ADR-0025. Nexus runs as a service of KGF Cloud; §25.1 and §25.3 are Nexus's
own mechanisms, §25.2 feeds KGF Cloud's audit and compliance program
(docs/SOC2-READINESS.md). Migration 0033 is required for §25.2.

### 25.1 Worker-side secret resolution

A task declares references, never values:

```yaml
runtime:
  secrets:
    - { name: DB_PASSWORD, ref: "vault://kv/team-a/db#password" }        # env (default)
    - { name: tls.key,     ref: "akv:///nexus-team-a-tls-key", inject: file }
```

`inject: env` sets the variable `name`; `inject: file` writes
`$NEXUS_SECRETS_DIR/<name>` (0400) and keeps the value out of the
environment. **Prefer `file`**: environment variables are visible to
same-host root (`docker inspect`, `/proc/<pid>/environ`). The agent resolves
every declared secret **before** the workload is prepared; a task gets all of
them or does not start.

**Configuration** (`NEXUS_SECRETS_CONFIG`, mode 0600/0640, never group- or
world-writable; unknown keys are rejected so a typo cannot disable a control).
Keyed by namespace: the agent loads **only its own `--namespace` block** and
never constructs another tenant's providers.

```yaml
version: 1
tenants:
  team-a:
    timeout: 30s
    env:   { allow: ["TEAM_A_*"] }                 # env://NAME; "*" is refused
    file:  { allow_dirs: ["/var/run/secrets/nexus"], max_bytes: 65536 }
    vault:
      address: https://vault.internal:8200
      namespace: team-a                            # Vault Enterprise namespace, optional
      mounts: ["kv"]                               # KV v2 mounts; longest prefix wins
      allow_paths: ["kv/team-a"]                   # required; agent-side second fence
      auth: { method: kubernetes, role: nexus-team-a }   # or approle / token (files only)
      tls:  { ca_file: /etc/nexus/vault-ca.pem }
    akv:
      default_vault: kgf-platform-kv-prod          # KGF stamp Key Vault (global.keyVaultName)
      allow_vaults: ["kgf-platform-kv-prod"]
      allow_secret_prefixes: ["nexus-team-a-"]     # isolation inside the shared vault
      auth: { method: workload_identity }          # KGF stamp workload identity
```

| Scheme | Ref | Notes |
|---|---|---|
| `env://` | `env://NAME` | Agent environment, allow-listed. The agent's own credentials (`NEXUS_API_KEY`, `NEXUS_S3_*`, NATS, `VAULT_TOKEN`, `AZURE_CLIENT_SECRET`, ...) are never resolvable. |
| `file://` | `file:///abs/path` | Must stay inside `allow_dirs` after symlink resolution; 1 MiB cap. On KGF Cloud, mount the CSI-projected Key Vault secret (`agentDefaults.secrets.fileSecrets`) and allow its directory. |
| `vault://` | `vault://<mount>/<path>[?version=N][#field]` | KV v2. No field: the secret's data map as JSON. Auth `kubernetes` (projected SA token), `approle` (role id + secret-id file), `token` (token file, re-read on every login so Vault Agent can rotate it). Plain `http` and `insecure_skip_verify` only for loopback. A 403 triggers one re-login. |
| `akv://` | `akv://<vault>/<secret>[/<version>]`, or `akv:///<secret>` for `default_vault` | Official Azure SDK. `workload_identity` (KGF's convention: the stamp's federated identity; chart `agentDefaults.secrets.workloadIdentityClientId`), `managed_identity`, `client_secret` / `client_certificate` (files). Secret names follow KGF's service-prefixed kebab-case (`nexus-<tenant>-<name>`); `allow_secret_prefixes` enforces it. |

`aws-sm://` and `gcp-sm://` are not implemented.

**Failure classes** (`TaskError.type = SecretResolutionFailed`): malformed ref,
reserved `NEXUS_*` name, clash with `runtime.env`, missing field ->
`VALIDATION`; scheme not configured for the tenant, not allow-listed, 401/403
-> `PERMISSION_DENIED`; absent secret/version -> `NOT_FOUND`; unreachable,
timeout, 429, 5xx, sealed Vault -> `TRANSIENT` (retryable). None is retried by
the agent itself; the control plane's retry policy decides.

**Where values go, and do not.** Executors: subprocess (env + files),
docker (`--env-file` 0600, removed right after `docker run`; files bind-mounted
read-only at `/run/nexus/secrets`), kubernetes (the per-attempt Secret, by
`secretKeyRef` and a projected volume; never a literal in the pod spec).
Host-side files live under `NEXUS_SECRETS_DIR_ROOT` (the chart mounts a
memory-backed emptyDir there) and are removed by Cleanup on every path.
Values never enter the WAL, the result store (unless the task writes them into
its own output), reports, events or agent log lines; shipped task logs, the
agent-log copy, progress notes and error messages pass the ADR-0020 redactor
(per line, exact match, values under 3 bytes are not redactable and are named
in a WARN). With providers configured the agent sets `RLIMIT_CORE=0` and, on
Linux, `PR_SET_DUMPABLE=0`.

**Subprocess executor (F138, F151).** A subprocess task's environment is
built from an allow-list, never inherited wholesale: `PATH`, `HOME`, `LANG`,
`LC_ALL`, `LC_CTYPE`, `TZ`, `TMPDIR`, `USER`, `LOGNAME`, `NEXUS_TASK_MODULES`,
`NEXUS_RESULT_DIR`, then `runtime.env`, env-injected secrets and the platform
`NEXUS_*` variables. Pass anything else explicitly with
`NEXUS_SUBPROCESS_ENV_PASSTHROUGH=GLOB[,GLOB...]` (default empty; Helm
`agentDefaults.secrets.subprocessEnvPassthrough`). The agent's own credentials
(`NEXUS_API_KEY`, `NEXUS_S3_*`, `NEXUS_RESULT_STORE_*`, `RESULT_STORE_*`, NATS,
Vault, Azure, AWS, GCP) never pass, whatever the pattern says. Docker and
Kubernetes tasks start from an empty environment. The subprocess executor
still runs tasks as the agent's user: development-grade, as ADR-0007 says.

**Local runs and replay** (`nexus.local`, `nexusctl run --local`,
`nexusctl replay`) run on the developer's own machine: tasks inherit the
developer's full environment plus the `NEXUS_*` contract (so a local task can
use the developer's tools and credentials), and `SecretRef`s are **not**
resolved; export the values a task needs, under the names it declares.
Nothing from a local run leaves the machine.

**Tests.** `make test-vault` (a digest-pinned Vault 1.21.4 dev container:
token and AppRole login, KV v2 fields and versions, policy denial, revoked
token, sealed server). Azure Key Vault is covered by a fake behind the
`AKVGetter` interface; **manual test against a real vault**:

1. `az keyvault secret set --vault-name <vault> --name nexus-team-a-probe --value "$(openssl rand -hex 16)"`;
   grant the agent's identity *Key Vault Secrets User* on the secret or vault.
2. Configure the tenant's `akv:` block (workload identity on AKS; for a laptop
   use `client_secret` with a secret file).
3. Submit a task with `{ name: PROBE, ref: "akv:///nexus-team-a-probe", inject: file }`
   that runs `sha256sum "$NEXUS_SECRETS_DIR/PROBE"`; compare with the local
   `sha256sum` of the value. Then `echo "$(cat $NEXUS_SECRETS_DIR/PROBE)"` and
   confirm the attempt log shows `[REDACTED:PROBE]`.
4. Negative: a name without the tenant prefix -> `PERMISSION_DENIED`; a missing
   name -> `NOT_FOUND`; revoke the role -> `PERMISSION_DENIED`.

### 25.2 The audit trail: pagination, export, KGF Cloud, sinks, retention, hash chain

**Routes** (Administrator; the Auditor role has `audit.view`, only
Administrator has `audit.manage`):

| Route | Permission | |
|---|---|---|
| `GET /v1/namespaces/{ns}/audit?limit=&cursor=&since=&until=&action=&outcome=&actor_id=` | audit.view | newest first; `next_cursor` is an opaque keyset cursor (stable while rows arrive), `null` on the last page |
| `GET .../audit/export?format=ndjson\|csv\|kgf&since=&until=&action=&outcome=&actor_id=` | audit.view | streamed from one REPEATABLE READ snapshot, no row cap |
| `GET/PUT .../audit/policy` `{retention_days|null, revision}` | view / manage | |
| `POST .../audit/legal-hold` `{hold, reason, revision}` | audit.manage | a reason is required to set a hold |
| `POST .../audit/purge` | audit.manage | apply retention now (the gateway also runs it hourly) |
| `GET/POST .../audit/sinks`, `PUT/DELETE .../audit/sinks/{id}`, `POST .../audit/sinks/{id}/rotate-secret` | view / manage | |

**Formats.** `ndjson` is the verifiable export: a header (`encoding`,
`complete` = unfiltered, `anchor` = the genesis or latest purge checkpoint,
`chain_started_at`), one `audit` record per row in chain order with `chain_seq`,
`prev_hash`, `row_hash`, and a trailer with the row count and the chain head.
`csv` is RFC 4180 with a quote prefixed to any cell starting `= + - @`, tab or
CR (the same neutralisation KGF's export applies). `kgf` is KGF Cloud's audit
event shape, one per line.

**KGF Cloud.** KGF has no audit ingestion API today (its `audit_events` are
written by its own services and exported per project), so Nexus provides the
KGF-shaped export and a KGF-format sink for a KGF-side collector. Mapping:

| KGF field | From |
|---|---|
| `organization_id` | verified KGF context `org`; empty when the request carried none (the collector maps by `nexus.namespace`) |
| `id` | `nexus:<namespace>:<chain_seq>` (stable, so redelivery de-duplicates on KGF's `(organization_id, id)` key) |
| `action` | `nexus.` + Nexus action (KGF's dotted lower-case style), e.g. `nexus.token.create` |
| `resource_id` | `/organizations/{org}/subscriptions/{sub}/resourceGroups/{rg}/providers/KGF.Nexus/namespaces/{ns}/{target_kind}/{target_id}` (KGF authz scope form) when the KGF scope is known, else `nexus://namespaces/{ns}/...` |
| `subscription_id`, `resource_group` | verified context `sub`, `rg` |
| `created_at` | `occurred_at`, RFC 3339 UTC |
| `principal`, `on_behalf_of` | verified context `principal`, `obo`; else `nexus:<actor_kind>:<label>` |
| `request_id` | verified context `request_id`, else the caller's `X-Request-ID` (sanitised, correlation only) |
| `outcome`, `source` (`kgf-nexus`), `kgf_attributed` | |
| `nexus{}` | namespace, audit id, Nexus action, actor, target, client IP, detail |
| `chain{}` | encoding, seq, prev_hash, row_hash: tamper evidence survives the hand-over |

Attribution uses the ONE KGF context verifier of §24 (`internal/kgfctx`):
the same keys (`NEXUS_KGF_CONTEXT_PUBLIC_KEYS` or `NEXUS_KGF_CONTEXT_KEYS_URL`),
audience (`NEXUS_KGF_AUDIENCE`, default `kgf-nexus`), user issuers
(`NEXUS_KGF_USER_ISSUERS`, default `kgf-gateway`), replay guard and
tenantctx-identical checks. Chart: the `kgf` block. Where the attribution comes
from depends on `NEXUS_AUTH_MODE`:

| Mode | Attribution |
|---|---|
| `kgf`, `both`, request authenticated by `X-KGF-Context` | the context that authenticated it; the actor is recorded as `actor_kind = 'kgf'` (§24.7) |
| `standalone` with keys configured | an `X-KGF-Context` sent **with** a Nexus credential, if it verifies: **attribution only**, the Nexus credential alone authorizes. An unverifiable or replayed token contributes nothing and is logged at WARN |
| `standalone` without keys | none; a request carrying `X-KGF-Context` is refused (`400 kgf_context_not_accepted`, §24.2) rather than silently ignored |
| any mode, machine credential without a context | none; `request_id` is the caller's sanitised `X-Request-ID` |

**Sinks.** A webhook sink receives NDJSON batches (`format: nexus` records or
`format: kgf` events), `POST`ed with `Content-Type: application/x-ndjson` and
`X-Nexus-Signature: t=<unix>,v1=<hex HMAC-SHA256(secret, "<t>." + body)>`,
`X-Nexus-Delivery: <sink>:<first>-<last>` (stable per batch: de-duplicate on
it), `X-Nexus-Namespace`, `X-Nexus-Format`. Verify: recompute the HMAC over the
exact body bytes with the secret, compare in constant time, and reject a `t`
older than your tolerance (300 s). Any 2xx acknowledges the batch and advances
the sink's cursor; anything else is retried with capped exponential backoff
(5 s doubling to 10 min, full jitter): **at least once**. Only one gateway
replica delivers a given sink at a time (advisory lock). The signing secret is
generated by Nexus, returned **once** at creation or rotation, and stored
AES-256-GCM-sealed (bound to the namespace) under `NEXUS_AUDIT_SINK_KEY` or
`NEXUS_AUDIT_SINK_KEY_FILE` (32 random bytes, base64: `openssl rand -base64 32`;
chart `auditExport.sinkKeySecret`, key `audit-sink-key`). Without the key,
sink creation answers 503 `sinks_disabled`. Rotating the gateway key makes
existing sinks fail with "rotate the sink's secret": rotate each sink.
Destinations are `https` only and may not resolve to loopback, link-local
(169.254.169.254), private or unspecified addresses unless listed in
`NEXUS_AUDIT_SINK_ALLOW_CIDRS` (checked at configuration AND at dial time, no
proxies, no redirects). `NEXUS_AUDIT_SINK_ALLOW_HTTP=true` is for development.

**Retention and legal hold.** `retention_days` null (default) keeps everything.
Rows leave `audit_log` only through `audit_purge()` (SECURITY DEFINER; the
gateway role stays INSERT-only), which: does nothing without a policy; refuses
under legal hold; removes only a **prefix** of the chain, and never a row an
enabled sink has not acknowledged; writes a `purge` checkpoint (seq and hash of
the last removed row) in the same transaction. Triggers make `audit_log`
UPDATE-proof and DELETE-proof for every role outside `audit_purge()`.

**Hash chain.** Per namespace. `row_hash = hex(sha256(canonical_v1))`, where
canonical_v1 is the compact JSON array

```
["nexus-audit-v1", namespace, chain_seq, occurred_at, actor_id|null, actor_kind,
 actor_label, action, target_kind, target_id, outcome, detail, client_ip,
 kgf_organization, kgf_subscription, kgf_resource_group, kgf_principal,
 kgf_on_behalf_of, request_id, prev_hash]
```

with `occurred_at` as `YYYY-MM-DDTHH:MM:SS.ffffffZ` (UTC, microseconds),
`actor_id` lower-case, `detail` re-encoded from PostgreSQL's jsonb form with
numbers verbatim, object keys sorted, no whitespace and no HTML escaping, and
strings as Go's `encoding/json` writes them. `prev_hash` of seq 1 is
`hex(sha256("nexus-audit-v1:genesis:" + namespace))`. The append locks the
namespace's `audit_chain_heads` row in the change's transaction, so replicas
cannot fork the chain. Rows written before 0033 carry no chain position and are
reported as pre-chain; a row without a position written after the chain
started (an older gateway replica mid-upgrade, or direct SQL) is reported as
**unchained** and fails verification unless `--allow-unchained`.

**Verify.** `nexusctl audit verify` (via the API) or
`nexusctl audit verify --file export.ndjson` (offline, no credentials): exit
0 intact, **8** broken, printing the first broken sequence (altered row, gap,
reordered row, broken link, truncated tail). Evidence procedure: export
(`nexusctl audit export --file audit-<date>.ndjson`), verify, store the file and
the reported head **outside** the database (or keep a SIEM sink). The chain
proves retained rows were not altered relative to each other; a superuser who
rewrites the whole tail is caught only by comparing with a copy held elsewhere.

**Fail-closed decision (F139).** Security grants (login, user create / enable /
update / password reset / email verified, roles, tokens minted, tenant
creation, platform/cluster/quota configuration, every audit-configuration
change) commit **with** their audit row or not at all: `503 audit_unavailable`.
Revocations (logout, token revoke, user disable, sessions revoked, role delete)
are never blocked; a failed write logs `AUDIT RECORD LOST` at ERROR.
Operational actions (retries, deletes of runs/specs/schedules, quarantine,
cache invalidation, email-link sends) log and continue. The table is
`auditActionClasses` in `services/api-gateway/rest/audit.go`; a test fails the
build if any action string is unclassified. Alert on
`increase(nexus_audit_write_failures_total[5m]) > 0` (labels `class`,
`blocked`), `nexus_audit_sink_delivery_failures_total` rising, and on a sink
whose `last_success_at` is older than your RPO.

**Migration 0033** is additive and `no-transaction` (a concurrent index):
re-runnable after a partial apply. Apply it before rolling out the new gateway;
old replicas keep writing unchained rows until replaced. Rollback
(`0033_*.down.sql`) drops the chain columns, checkpoints, policies and sinks:
**export first**, or the tamper evidence for retained rows is lost.

### 25.3 Air-gapped installation

`make airgap-bundle` (on a connected host; `AIRGAP_VERSION=`, `PLATFORMS=`)
writes `dist/nexus-airgap-<version>.tar.gz` and its `.sha256`: every image the
chart and compose reference, resolved and saved **by digest** (skopeo when
installed, else `docker save`), the packaged chart, `nexusctl` and
`nexus-agent` binaries, the SDK wheel, `schemas/`, `manifest.json` and
`SHA256SUMS` (cosign-signed when cosign is installed; the manifest says
`"signed": false` otherwise). Inside the gap, `load.sh --registry
<host>[/path]` verifies checksums (and the signature), pushes every image,
records the digest the **registry** reports, and writes `values-airgap.yaml`
pinning every chart image to `<registry>/...@sha256`, then proves it with
`helm template`. `make airgap-test` repeats this against a throwaway local
registry. Full procedure, what is not included, and the digest caveats:
docs/AIRGAP.md.

---

## 26. Tracing and result-store backends

Two production features share this section because they share configuration
surfaces (env, Helm values, compose) and one verification loop: OpenTelemetry
tracing (ADR-0026) and the native S3, Azure Blob and GCS result-store backends
(ADR-0027).

### 26.1 Turning tracing on

Tracing is **off** until you point it at a collector. Every service and the
agent read only the standard OpenTelemetry variables:

| Variable | Nexus default | Notes |
|---|---|---|
| `OTEL_TRACES_EXPORTER` | unset (off) | `otlp` or `console` turns tracing on; `none` forces it off. An unknown value fails startup. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` / `_TRACES_ENDPOINT` | unset | Setting either also turns tracing on. |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf` | `grpc` for port 4317. `http/json` is refused. |
| `OTEL_EXPORTER_OTLP_HEADERS`, `_INSECURE`, `_CERTIFICATE`, `_TIMEOUT`, `_COMPRESSION` | SDK defaults | Read by the exporter itself. Keep headers (API tokens) in a Secret. |
| `OTEL_TRACES_SAMPLER` | `parentbased_traceidratio` | Also `parentbased_always_on/off`, `traceidratio`, `always_on/off`. |
| `OTEL_TRACES_SAMPLER_ARG` | `1.0` | The chart defaults to `0.1`. |
| `OTEL_SERVICE_NAME` | the binary's name | Do not set it fleet-wide: every service would report as one. |
| `OTEL_RESOURCE_ATTRIBUTES` | — | e.g. `deployment.environment.name=prod`. |
| `OTEL_BSP_*` | SDK defaults | Batch size, queue, delay. The queue drops when full; export never blocks work. |
| `OTEL_SDK_DISABLED` | — | `true` forces tracing off. |
| `NEXUS_TRACING_PUBLIC_ENDPOINT` (gateway) | `false` | `true`: an inbound REST `traceparent` becomes a **link**, not the parent (untrusted callers cannot force sampling or join your traces). |

**Helm:** `tracing.enabled=true`, `tracing.endpoint=http://otel-collector.observability:4317`,
`tracing.protocol=grpc`, `tracing.samplerArg="0.1"`, optional
`tracing.headersSecret.{name,key}`, `tracing.resourceAttributes`,
`tracing.publicEndpoint`. The chart refuses `enabled` without an endpoint.
**Compose:** `docker compose --profile tracing up -d` starts a pinned Jaeger v2
(UI on `127.0.0.1:16686`), then run the stack with
`OTEL_TRACES_EXPORTER=otlp OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4318`.

Each process logs `OpenTelemetry tracing enabled` with exporter, sampler and
endpoint at startup. Exporter errors (collector down) are logged at most once a
minute per process; spans are dropped, work is not slowed.

### 26.2 Reading a run's trace

One run is one trace: the API request that created it, `run.create`
(workflow-manager), `run.materialize` / `run.reconcile` (dag-engine),
`attempt.dispatch` (scheduler, one per attempt, linked to the worker poll that
claimed it), `attempt.execute` with `prepare_inputs`, `run`, `finalize_logs`,
`store_outputs`, `report_result` (agent), `objstore.*` result-store calls, and
database queries. Search your backend by `nexus.run_id` or `nexus.attempt_id`
(span attributes), or take `trace_id` from any log line: every record logged
with a request context, and every line of an attempt's agent log, carries
`trace_id` and `span_id`.

Task code sees the attempt's context as `TRACEPARENT` / `TRACESTATE`. The
Python SDK (`pip install 'nexus-sdk[opentelemetry]'`) runs the handler in a
child span and exposes `ctx.traceparent`; configure a `TracerProvider` in the
task image (for example `opentelemetry-instrument`) to export the task's own
spans. A run created before migration 0028, or by an untraced caller, is
dispatched under the worker's poll trace instead.

**Sizing.** At ratio 1.0 every query is a span; a 1 000-attempt fan-out is tens
of thousands of spans in one trace, which some backends truncate. Sample in
production (0.01–0.1) and raise the ratio for a namespace under investigation
by sending a sampled `traceparent` with the run's creation request.

**Upgrade order.** Apply `0028_trace_context` (two nullable columns, no
rewrite) with the other migrations, then roll services. Old agents ignore
`TaskAssignment.trace_context`. Rollback: roll the scheduler, dag-engine,
workflow-manager and event-service back before `0028_…down.sql`.

### 26.3 Choosing and configuring the result-store backend

| Setting | Values | Applies to |
|---|---|---|
| `NEXUS_RESULT_STORE_BACKEND` | `s3` (default), `azblob`, `gcs` | gateway, agents, artifact-gc |
| `NEXUS_RESULT_STORE_ENDPOINT` / `_BUCKET` / `_REGION` | S3 endpoint; the Azure container / GCS bucket in `_BUCKET`; endpoint optional for Azure (sovereign cloud, private DNS, Azurite) and empty for real GCS | all |
| `NEXUS_RESULT_STORE_PART_SIZE_BYTES` | 5 MiB – 512 MiB, default 16 MiB | multipart part / Azure block / GCS chunk |
| `NEXUS_S3_CREDENTIALS` | `auto`, `static`, `web-identity`, `pod-identity`, `iam`, `env`, `anonymous` | s3 |
| `NEXUS_S3_ACCESS_KEY_ID` / `_SECRET_ACCESS_KEY` / `_SESSION_TOKEN` / `_USE_TLS` | static keys, as before | s3 |
| `NEXUS_AZURE_STORAGE_ACCOUNT` | account name | azblob |
| `NEXUS_AZURE_STORAGE_AUTH` | `auto`, `workload-identity`, `managed-identity`, `shared-key` | azblob |
| `NEXUS_AZURE_STORAGE_KEY`, `NEXUS_AZURE_CLIENT_ID` | shared key (from a Secret); user-assigned identity / client id override | azblob |
| `NEXUS_GCS_AUTH` | `auto` (ADC, Workload Identity), `none` (emulators only) | gcs |

**`auto` resolution.** S3: static keys if both are set; else an AWS identity
visible in the environment (`AWS_WEB_IDENTITY_TOKEN_FILE`,
`AWS_CONTAINER_CREDENTIALS_*`, `AWS_ACCESS_KEY_ID`); else startup reports "no
S3 credentials". EC2 instance roles are never probed implicitly (an IMDS probe
hangs off EC2): use `iam`. Azure: a configured key, else workload identity when
`AZURE_FEDERATED_TOKEN_FILE` is present, else managed identity.

**Per cloud** (the Terraform environments do this by default; F115). "ServiceAccount"
means `serviceAccounts.apiGateway`, `.workflowManager`, `.agent` and
`.artifactGC`, the four result-store clients, which the environments point at one shared
`nexus-artifacts` account (§20.13):

| Cloud | Backend and identity | Chart values |
|---|---|---|
| EKS | `s3`, Pod Identity (`pod-identity`) or IRSA (`web-identity`) | `resultStore.s3.credentials`, ServiceAccount (IRSA: annotation `eks.amazonaws.com/role-arn`) |
| AKS | `azblob`, `workload-identity` | `resultStore.azure.account`, `.clientId`; ServiceAccount annotation `azure.workload.identity/client-id` (the chart adds the pod label `azure.workload.identity/use: "true"`) |
| GKE | `gcs`, Workload Identity | ServiceAccount annotation `iam.gke.io/gcp-service-account` |
| anywhere | `s3` + static keys (Silo/MinIO, GCS HMAC interop at `https://storage.googleapis.com`) | `resultStore.s3.accessKeySecret` |

The gateway advertises its backend to agents; an agent's own
`NEXUS_RESULT_STORE_BACKEND` wins. **Upgrade agents before** moving a
deployment to `azblob` or `gcs`: an old agent ignores the advertisement and
speaks S3. Changing backends never migrates objects: copy the bucket first
(`rclone sync`, `azcopy copy`, `gcloud storage cp -r`), keeping keys identical,
then switch every component at once.

The first log line to check is the gateway's `durable task logs enabled`
(now with `backend`) or the WARN naming why the store could not be opened.

### 26.4 Least privilege and lifecycle

| Principal | S3 (IAM) | Azure (RBAC) | GCS (IAM) |
|---|---|---|---|
| agents | `s3:GetObject`, `s3:PutObject` on `<bucket>/<namespace>/*`; `s3:ListBucket` for the readiness probe | `Storage Blob Data Contributor` on the container (Azure RBAC has no prefix scope; use a container per trust boundary) | `roles/storage.objectUser` on the bucket (IAM Conditions on `resource.name.startsWith` for per-namespace scope) |
| gateway (logs) | `s3:GetObject` on `<bucket>/*/logs/*` | `Storage Blob Data Reader` on the container | `roles/storage.objectViewer` |
| artifact-gc | `s3:ListBucket`, `s3:GetObject`, `s3:DeleteObject` | `Storage Blob Data Contributor` | `roles/storage.objectUser` |

Every backend's bucket existence check works with these object-level roles.

Lifecycle rules on the bucket are housekeeping only; **reachability is decided
by artifact-gc**, so never add an age-based expiry on current objects (it
deletes outputs a live run still references). Provision, as the Terraform
modules do: abort incomplete multipart uploads after 7 days (S3, GCS; Azure
discards uncommitted blocks after 7 days by itself), expire noncurrent
versions after 30 days (S3 rule, GCS rule, Azure management policy), soft
delete for accidental deletes. Log retention remains `artifact-gc
-log-retention` (§19.2).

**Deduplicated outputs are refreshed.** An output identical to an existing
object more than an hour old is re-uploaded instead of reused (F141), so the
GC's grace period (§7.7) always covers it. Expect `uploaded` rather than
`deduplicated` in `nexus_agent_result_store_puts_total` for such repeats.

### 26.5 Metrics

`nexus_objstore_operations_total{backend,op,outcome}`,
`nexus_objstore_operation_duration_seconds{backend,op}` and
`nexus_objstore_bytes_total{backend,direction}` are exported by the agent, the
gateway and artifact-gc (§17.4). `outcome` is `ok`, `not_found`, `exists` or
`error`. Alert on a rising `outcome="error"` rate per backend; `not_found` on
`op="get"` at the gateway means log objects were removed out of band (the log
API reports `expired`).

### 26.6 Verifying

```sh
make objstore-conformance   # one suite against Silo, Azurite and fake-gcs-server; a skip fails
make tracing-e2e            # real binaries + Jaeger: one trace from API to agent and back
make test-stores-down
```

Both start pinned containers with `make test-stores-up`
(`TEST_STORES_NAME`, `TEST_*_PORT` override names and loopback ports). On a
new cloud deployment, additionally: submit the fan-out example with a > 64 MiB
artifact (multipart), run `artifact-gc` in dry-run and `-check-refs`, read a
task log through the console, and find the run's trace in your backend. Record
the results here per cloud; none has been recorded yet (F115).

---

## 27. Notifications, webhook triggers, sensors and asset lineage

ADR-0028 describes the design. This section covers running it. Migration 0034 adds the
tables. Services never migrate themselves, so apply it with the migrate job before
you deploy code that uses it (§6). No NATS subject or permission changes: the
notifier reads the outbox from Postgres, so everything here works with the bus
disabled.

| Piece | Runs in | Tables it writes |
|---|---|---|
| Channel/rule/trigger/sensor configuration, hook receiver | api-gateway | `notification_channels`, `notification_rules`, `notification_rule_channels`, `webhook_triggers`, `webhook_receipts`, `sensors` (+ INSERT on `notification_deliveries` for test and redrive) |
| Notifier (evaluate, SLA sweep, dispatch) and lineage recorder | event-service | `notification_deliveries`, `notification_channel_windows`, `notification_incidents`, `assets`, `asset_lineage_edges`, `asset_materializations`, `outbox.notified_at` |
| Sensor loop | workflow-manager | `sensor_state`, `sensor_observations` (+ runs, through the run service) |

Every loop is safe with any number of replicas and needs no leader: claims use
`FOR UPDATE SKIP LOCKED` or a lease, and every output is keyed by a unique
constraint.

### 27.1 Configuration

| Variable | Service | Default | Meaning |
|---|---|---|---|
| `NEXUS_SECRET_KEY` / `_FILE` | gateway, event-service | unset | 32-byte key (base64 or hex) sealing channel and trigger secrets (AES-256-GCM). **Both services need the same value.** Unset: inline channel secrets and webhook triggers are refused (503 `secret_key_unconfigured`); `secret_ref` channels still work. A malformed value stops the service at startup. |
| `NEXUS_SECRET_KEY_PREVIOUS` / `_FILE` | gateway, event-service | unset | A retired key that still opens old ciphertexts during a rotation (§27.2). |
| `NEXUS_NOTIFY_ALLOW_PRIVATE_TARGETS` | gateway, event-service | `false` | Allows `http://` and private, loopback and link-local destinations. Keep it false in shared deployments: otherwise a channel can be aimed at internal services and at the cloud metadata endpoint. Compose sets it true for local receivers. |
| `NEXUS_PUBLIC_URL` | gateway | unset | Turns a trigger's hook path into the absolute `url` shown to operators. |
| `NEXUS_CONSOLE_URL` | event-service | unset | Links each notification to `<console>/runs/<id>`. |
| `NEXUS_SMTP_*` | event-service | mail disabled | Email channels. The settings are the same as the gateway's (§10). With no host, email deliveries fail permanently and say why. |
| `NEXUS_NOTIFY_EVAL_BATCH`, `_EVAL_INTERVAL`, `_DISPATCH_BATCH`, `_DISPATCH_INTERVAL`, `_CONCURRENCY`, `_SLA_INTERVAL`, `_BASE_BACKOFF`, `_MAX_BACKOFF`, `_RETENTION` | event-service | 200, 1s, 50, 1s, 8, 30s, 30s, 1h, 720h | Notifier tuning. |
| `NEXUS_SENSOR_S3_ENDPOINT`, `_REGION`, `_ACCESS_KEY_ID`, `_SECRET_ACCESS_KEY`, `_USE_TLS` | workflow-manager | the result store's `NEXUS_RESULT_STORE_*` / `NEXUS_S3_*` | The store that `s3://` sensors list. |
| `NEXUS_SENSOR_INTERVAL` | workflow-manager | 2s | How often a replica looks for due sensors. Each sensor's own `poll_interval_seconds` decides how often it is actually polled. |

In Helm, set `events.secretKey.existingSecret` (and `key`, and `previousKey`
during a rotation) to a Secret you create. Set `events.allowPrivateTargets` only
for on-premises receivers.

```sh
kubectl -n nexus create secret generic nexus-events --from-literal=secret-key="$(openssl rand -base64 32)"
helm upgrade nexus deploy/helm/kgf-nexus --reuse-values --set events.secretKey.existingSecret=nexus-events
```

Compose ships a **public development key** so a local stack works out of the
box. Never deploy it.

### 27.2 Rotating the secret key

Every ciphertext records which key sealed it.

1. Put the new key in `NEXUS_SECRET_KEY` and the old key in
   `NEXUS_SECRET_KEY_PREVIOUS`, on the gateway **and** the event-service. Roll
   both.
2. Re-seal: rotate every webhook trigger (`nexusctl trigger rotate-secret <name>`
   re-seals the grace-period copy under the new key), and re-save every channel
   with an inline secret (`PUT` with `secret`).
3. Remove `NEXUS_SECRET_KEY_PREVIOUS`.

If a key is lost, its ciphertexts cannot be opened. Deliveries on those
channels go to the dead letters with "the channel secret could not be opened",
and the hooks answer 401. Recover by re-entering channel secrets and rotating
trigger secrets.

### 27.3 Channels and rules

| Kind | Secret (inline `secret` or `secret_ref: env:NAME \| file:/path`) | `config` |
|---|---|---|
| `slack_webhook` | incoming-webhook URL | — |
| `slack_bot` | bot token (`chat:write`) | `channel` (id) |
| `teams` | Workflows (Power Automate) webhook URL; sends an Adaptive Card 1.4 | — |
| `pagerduty` | Events API v2 routing key | `severity`: critical \| error \| warning \| info |
| `webhook` | HMAC signing key; generated and **shown once** when omitted | `url` |
| `email` | — | `to`: 1–20 addresses |

Reads never return a secret. They show only `{configured, source, hint}`, where the
hint is a URL's host or the last four characters of the secret. A `secret_ref` is
resolved by the **event-service** in its own environment. Refs that name platform
credentials (`NEXUS_SECRET_KEY*`, `DATABASE_URL`, `PG*`, `NEXUS_NATS*`, and the
S3 secrets) are refused.

A rule matches events in its namespace:

- **Events:** `run.failed`, `run.succeeded`, `run.timed_out`, `run.sla_exceeded`
  (needs `sla_seconds`: a run still active this long after creation) and
  `attempt.lost`.
- **Filters:** an optional list of workflow names and labels (every `k=v` must
  appear in the workflow's spec `labels`).
- **Targets:** one or more channels.
- **Rules never page about history.** A rule matches only facts at or after its
  creation.
- **Budget and quota events are not available** (F142). Nothing durable records
  them yet.

```sh
nexusctl notify channel-create oncall --kind pagerduty --secret-env PD_ROUTING_KEY --severity critical
nexusctl notify channel-create ops-hook --kind webhook --url https://hooks.example.com/nexus   # prints the signing secret once
nexusctl notify rule-create etl-failures --event run.failed --event run.timed_out \
    --event run.sla_exceeded --sla 2h --workflow nightly-etl --label team=data --channel oncall
nexusctl notify channel-test oncall      # queued and sent like a real one; waits for the outcome
```

### 27.4 Delivery semantics

- **One delivery per event per channel.** The key is `UNIQUE(channel, event_key)`.
  The event key is the fact's identity, such as `run_state:<run>:<state_version>`
  or `run_event:<id>`. Two rules that match one event on one channel send once.
- **At-least-once to the receiver.** A dispatcher leases a delivery for 60 s, sends
  it outside any transaction, and records the outcome only while it still holds
  the lease. A crash mid-send means the delivery is sent again, so every send
  carries a stable dedup key:
  - webhooks: `Idempotency-Key` and `X-Nexus-Delivery`
  - PagerDuty: `dedup_key`

  Slack, Teams and SMTP cannot deduplicate, so a crash inside that window can
  send one duplicate message.
- **Retries.** Backoff is exponential with jitter: 30 s, doubling, capped at 1 h,
  and never shorter than a receiver's `Retry-After`. A delivery gets at most 8
  attempts (about 4 h). 408, 425, 429, 5xx and network errors are retried. Any
  other 4xx is permanent, as are a refused private address, a redirect (never
  followed), a disabled channel, and a secret that cannot be opened. A permanent
  failure goes straight to the **dead letters** (`state=dead`).
- **Redrive.** `nexusctl notify redrive <id>` (or the console) inserts a **new**
  delivery that keeps the original dedup key. The dead row stays as evidence.
- **Rate limit.** Each channel has a fixed one-minute window,
  `rate_limit_per_minute` (default 30). The limit is exact across replicas. An
  over-limit delivery waits for the next window and spends no attempt.
- **PagerDuty.** A trigger opens an incident per (rule, channel, workflow). Repeat
  failures reuse its dedup key, so they group into one incident. The next
  `run.succeeded` of that workflow sends a `resolve` with the same key. A success
  with nothing open sends nothing, and a channel test resolves itself.
- **Retention.** Delivered and dead rows are pruned after `NEXUS_NOTIFY_RETENTION`
  (30 days). Outbox rows are kept until the notifier has evaluated them, up to
  the relay's `NEXUS_OUTBOX_MAX_AGE` (24 h). A notifier that is down for longer
  than that loses notifications for the oldest facts.

Receivers of the generic webhook verify
`X-Nexus-Signature: v1=hex(HMAC-SHA256(key, X-Nexus-Timestamp + "." + body))`,
reject stale timestamps, and deduplicate on `Idempotency-Key`.

### 27.5 Inbound webhook triggers

```sh
nexusctl trigger create github-push --workflow build \
    --map repo='$.payload.repository.name' --map ref='$.payload.ref' \
    --idempotency-key-path '$.payload.after'           # prints url and secret ONCE
export NEXUS_TRIGGER_SECRET=...                           # the printed secret
nexusctl trigger fire github-push --data '{"after":"abc","ref":"main","repository":{"name":"svc"}}'
```

A sender POSTs JSON to `/v1/hooks/<ns>/<trigger-id>` with:

```
X-Nexus-Timestamp: <unix seconds>
X-Nexus-Signature: v1=<hex HMAC-SHA256(secret, timestamp + "." + raw body)>
```

For example: `sig=$(printf '%s.%s' "$ts" "$body" | openssl dgst -sha256 -hmac "$secret" | cut -d' ' -f2)`.

| Answer | Meaning |
|---|---|
| 202 `{run_id, created}` | Run created (`created: true`), or an existing run for the same idempotency key. |
| 401 | Missing or bad signature, or a timestamp outside `tolerance_seconds` (default 300). |
| 404 | Unknown **or disabled** trigger. They are indistinguishable, so the route cannot enumerate triggers. |
| 409 `replayed` | This exact signed delivery was already accepted. |
| 422 | Not JSON, no value at the idempotency key path, a params path missing from the payload, the workflow gone, or a required param missing. |
| 429 | The workflow's `max_active_runs` or the namespace run quota. The delivery is recorded as rejected, so **retrying the identical request is accepted** once there is room. |

Params come from `params_mapping`. Each value is a JSONPath into
`{"payload": body, "trigger": {id, name}}`, a template (`"{{ $.payload.ref }}"`),
or a literal. Runs record `trigger_kind: webhook` and
`trigger_detail: {trigger_id, trigger_name, delivery}`.

The audit log records every create, update, delete and rotation (with the actor)
and every delivery as `trigger.fire`, including refusals (`outcome=denied`).
`nexusctl trigger deliveries <name>` shows the last 7 days.

Rotation: `nexusctl trigger rotate-secret <name> --grace 24h` returns the new
secret. The old one keeps verifying until the grace period ends, and `--grace 0`
revokes it immediately.

Put the hook route behind the gateway's per-address rate limit (§16). It is the
one route an unauthenticated sender can reach besides login.

### 27.6 Sensors

```sh
nexusctl sensor create landing --workflow ingest --uri s3://raw/landing/ --pattern '**/*.parquet' \
    --map path='$.object.uri' --map size='$.object.size' --poll 1m
nexusctl sensor create after-etl --workflow report --upstream nightly-etl --map date='$.run.params.date'
nexusctl sensor list                    # status, triggered, pending, last error
nexusctl sensor observations landing    # what it saw and which run it started
```

- **Exactly one run per object or upstream run.** An observation is claimed by a
  unique row. Its run is created under `sensor:<id>:<hash>`, and a crash between
  the two is retried under the same key.
- **Baseline vs backfill.** By default the first full sweep records what already
  exists as `baseline` and triggers nothing. `--backfill` triggers for everything.
  Changing a sensor's definition restarts its sweep. Without backfill, it
  re-baselines.
- **Durable cursor.** Listing resumes after the last key (S3 `StartAfter`), 1000
  keys per pass, and wraps at the end of the prefix. An object whose key sorts
  behind the cursor is found on the next sweep. `sensor_observations` grows with
  the number of distinct keys and is removed only when the sensor is deleted.
  Prefer prefixes that are pruned upstream.
- **Status:**
  - `waiting`: no replica has picked it up yet
  - `initializing`: first sweep in progress
  - `ok`
  - `error`: see `last_error`; polling backs off to at most 15 min
  - `stale`: no replica has polled it for 5 min. Check that the workflow-manager
    is running.
  - `disabled`
- **Object stores.** `s3://` is served now. `gs://` and `az://` definitions are
  accepted and report "no object-store lister is configured" until native
  listers are registered. The lister interface (`sensors.ObjectLister`) is two
  behaviours: list after a key, and page.

### 27.7 Asset lineage

Tasks declare `assets: {produces: [{uri, name}], consumes: [...]}` in the spec,
or `@task(produces=..., consumes=...)` in the SDK. The notifier records each
run's edges the first time it sees the run. When a producing node succeeds it
records a **materialization**. A memoized (cache-hit) node does not count as one.
Query lineage with `nexusctl asset list | get <uri> | graph --uri <uri>`, the
console's Assets page, or `GET /v1/namespaces/<ns>/assets/{,detail,graph}`.
Lineage is **declared**: it states what a task said it touches, not what it did.

### 27.8 Monitoring

| Metric | Alert when |
|---|---|
| `nexus_notify_pending_deliveries` | Growing for 15 min: receivers are down or rate limits are too low. |
| `nexus_notify_oldest_due_delivery_seconds` | > 600: deliveries are due and not being sent. Is the event-service running, and does it have DB connections? |
| `nexus_notify_dead_letters_total{kind}` | Any increase. Inspect `nexusctl notify deliveries --state dead`. |
| `nexus_notify_send_attempts_total{kind,outcome}` | Rising `retry` ratio. |
| `nexus_notify_eval_errors_total` | Any increase: a fact could not be evaluated; see the event-service log. |
| `nexus_notify_rate_limited_total` | Sustained: raise the channel's `rate_limit_per_minute` or narrow its rules. |
| `nexus_outbox_backlog_rows` (existing) | Also counts rows waiting for the notifier when NATS is disabled. |

### 27.9 Upgrade and rollback

Upgrade:

- 0034 is additive: new tables, plus one nullable `outbox` column added with
  `CREATE INDEX CONCURRENTLY` (`migrate: no-transaction`, safe to re-run).
- A mixed fleet is safe. Old writers leave `notified_at` NULL, which means "not
  yet evaluated". An old relay may prune a published row an hour after
  publishing, which in practice is long after a new notifier evaluated it.
- Deploy order: migrate, then the event-service, workflow-manager and gateway
  (any order among the three).

Rollback:

- Roll back the code, then apply `0034_*.down.sql` only if you mean to discard
  all of it: configuration, delivery history, trigger secrets, sensor state and
  lineage.
- Export first: `pg_dump -t 'notification_*' -t 'webhook_*' -t 'sensor*' -t 'asset*'`.

---

## 28. Fractional GPUs, spot capacity, model caches and multi-cluster placement

Design: [ADR-0029](adr/0029-gpu-sharing-and-packing.md) (MIG, shared GPUs,
packing), [ADR-0030](adr/0030-spot-capacity-and-model-cache.md) (spot,
eviction, artifact cache), [ADR-0031](adr/0031-multi-cluster-placement.md)
(multi-cluster placement). Migration 0035. Findings F146, F147.

Nothing here changes behaviour until it is configured: a task without the new
fields, on agents without the new settings, is scheduled exactly as before.

### 28.1 What a task can ask for

| Author field | Meaning |
|---|---|
| `resources.request.mig: {"1g.10gb": 1}` | MIG instances of a profile. Exact: each instance is its own device. |
| `resources.request.gpu_share_milli: 250` | A quarter of ONE shared GPU (MPS or time-sliced). |
| `resources.request.gpu_share_memory_bytes` | VRAM on that ONE shared GPU, subtracted from the device, so several jobs pack onto it. |
| `placement.capacity` | `spot_allowed` (default), `spot_preferred`, `on_demand_only` (hard). |
| `placement.required_labels` | Hard: the worker's labels overlaid with its cluster's must contain these (e.g. `nexus.io/region`). |
| `placement.max_cost_tier` | Hard: `nexus.io/cost-tier` at most this. |
| `placement.preferred_data_locality`, `preferred_labels` | Soft. |
| `placement.artifacts` | Models/datasets served from the agent's node-local cache (`$NEXUS_ARTIFACT_<NAME>`); cached workers preferred. |
| `placement.max_preemptions` | Retries after spot evictions, which do not spend `retry.max_attempts` (default 5). |
| `prefer_same_worker_as` | Soft: the host that ran that node's successful attempt. Read since F146. |

In the SDK: `Gpu(count=0, share=0.25, share_memory="18Gi")`,
`Gpu(count=0, mig={"1g.10gb": 1})`, `Placement(...)`, `Artifact(...)`.

A soft preference is honoured by **bounded delay scheduling**: a polling
worker skips an attempt that a strictly better live worker could take, for at
most `NEXUS_PLACEMENT_WAIT` (default 3 s) after the attempt became due. A hard
constraint that no worker meets leaves the attempt PENDING indefinitely -- as
`node_selector` always has.

### 28.2 Configuring GPU workers

| Agent setting | Example | Notes |
|---|---|---|
| `NEXUS_GPU_COUNT` | `2` | Whole GPUs, indices 0..n-1 (unchanged). |
| `NEXUS_GPU_MIG` | `1g.10gb=MIG-aaa,MIG-bbb;3g.40gb=MIG-ccc` | Real instance UUIDs. A count (`1g.10gb=3`) synthesizes UUIDs and labels the worker `simulated=true`. |
| `NEXUS_GPU_SHARED` | `2:mps:80Gi,3:timeslice:24Gi` | Shared devices; indices must not overlap the whole ones. |
| `NEXUS_GPU_DISCOVERY=nvidia-smi` | | Reads whole GPUs and MIG instances from `nvidia-smi -L` instead. |
| `NEXUS_GPU_MPS_PIPE_DIR` | `/tmp/nvidia-mps` | Mounted into containers on MPS devices (with `--ipc=host`). |
| `NEXUS_K8S_MIG_RESOURCE_PREFIX` | `nvidia.com/mig-` | Kubernetes executor, device plugin `mixed` strategy. |
| `NEXUS_K8S_GPU_SHARED_RESOURCE` | `nvidia.com/gpu.shared` | Required for shared slices on the kubernetes executor; unset refuses them. |

A device is in exactly one mode; the agent refuses to start on an inventory
that counts one twice. Re-partitioning MIG needs an agent restart (a new
session).

**Isolation (read before enabling shared devices).** MIG isolates memory,
compute and faults in hardware. MPS enforces the VRAM declaration
(`CUDA_MPS_PINNED_DEVICE_MEM_LIMIT`) and a compute share
(`CUDA_MPS_ACTIVE_THREAD_PERCENTAGE`), but a fatal fault in one client kills
every client on the device, and clients must share a UID. **Time-slicing
enforces nothing**: a job that exceeds its declared VRAM can OOM its
neighbours. Untrusted tenants belong on MIG or whole devices.

**Kubernetes caveat.** On the kubernetes executor the device plugin, not the
agent, picks the physical GPU for a shared replica: Nexus's per-device packing
bounds the worker's total, but per-device VRAM is only enforced by MPS.

### 28.3 Spot workers and eviction

The agent advertises `nexus.io/capacity-type` (`NEXUS_CAPACITY_TYPE`, else read
from its Node's `karpenter.sh/capacity-type`, `eks.amazonaws.com/capacityType`,
`cloud.google.com/gke-spot`, `cloud.google.com/gke-preemptible` or
`kubernetes.azure.com/scalesetpriority`; that read needs `get` on its Node and
`NEXUS_NODE_NAME`, which the chart sets from `spec.nodeName`).

`NEXUS_EVICTION_SOURCES` (comma list): `kubernetes` (own Node's taints, needs
`get nodes`), `aws` (IMDSv2 spot instance-action), `gcp` (metadata
`preempted`), `azure` (Scheduled Events Preempt/Terminate), `file`
(`NEXUS_EVICTION_FILE`; for node-local handlers and tests). `NEXUS_EVICTION_TAINTS`
replaces the default taint list; `NEXUS_EVICTION_POLL` (5 s),
`NEXUS_EVICTION_GRACE` (25 s, for notices without a deadline) and
`NEXUS_EVICTION_MAX_GRACE` (90 s) tune it.

On a notice the agent stops polling, reports `drain_reason` (the gateway marks
it DRAINING), and sends every running task SIGTERM with the notice's time
minus 5 s. A task that exits non-zero is reported `PREEMPTED` and retried
against `task_nodes.preemptions_used`; exit 0 is a success. The scheduler moves
a worker that reports `PREEMPTED` to DRAINING in the same transaction, so the
retry can never be handed back to it (not even by a long poll it had already
parked). **Checkpointing
tasks should exit non-zero (143) after writing their checkpoint.** A spot
worker with no eviction source logs a warning: its reclaimed tasks become LOST
and spend the ordinary budget.

Scheduler: `NEXUS_MAX_PREEMPTION_RETRIES` (5), `NEXUS_PREEMPTION_RETRY_DELAY`
(2 s). Metrics: `nexus_agent_eviction_notices_total{source}`,
`nexus_agent_tasks_preempted_total`; `PREEMPTED` in
`nexus_agent_tasks_completed_total{error_class}`.

### 28.4 Node-local artifact cache

`NEXUS_ARTIFACT_CACHE_DIR` enables it; `NEXUS_ARTIFACT_CACHE_BYTES` bounds it
(default 50Gi), `NEXUS_ARTIFACT_CACHE_MAX_ENTRIES` optionally bounds the count,
`NEXUS_ARTIFACT_CACHE_VERIFY=hit` re-hashes on every hit. Entries in use are
pinned; eviction is least-recently-used among the rest. Downloads are
single-flight and sha256-verified before they become visible; a mismatch fails
the attempt as VALIDATION and increments
`nexus_agent_artifact_cache_integrity_failures_total` -- **alert on any rate**.
Other metrics: `..._hits_total`, `..._misses_total`, `..._evictions_total`,
`..._evicted_bytes_total`, `..._bytes`, `..._fetch_seconds`.

The cache is not part of `disk_bytes`; size the volume for both. In Helm
(`agents[].artifactCache`) it is an `emptyDir` with `sizeLimit` above the
bound, so it does not survive pod replacement; use a local PV or hostPath (and
accept its security review) where warm restarts matter. The kubernetes
executor's pods run on other nodes, so its tasks get no cache (the agent
advertises nothing for them). Presigned URIs are redacted from logs and cache
metadata.

### 28.5 Clusters

Label agents with their cluster: `NEXUS_CLUSTER_NAME` (Helm
`claimPlacement.clusterName`), which must name a cluster registered **in the
same namespace**. The cluster-controller publishes `NEXUS_CLUSTER_LABELS`
(Helm `clusterController.clusterLabels`, e.g.
`nexus.io/cost-tier=1,nexus.io/data-locality=lake-eu|s3-eu-west-1`) over the
observed `nexus.io/region` and `nexus.io/provider`, and each bound namespace's
ResourceQuota headroom (`NEXUS_CLUSTER_GPU_RESOURCES` names what counts as a
GPU). Check what the claim sees:

```sql
SELECT c.name, cs.labels, cs.headroom, cs.headroom_observed_at
  FROM clusters c JOIN cluster_status cs ON cs.cluster_id = c.id;
```

Headroom gates only kubernetes-executor attempts on workers labelled
`nexus.io/k8s-task-namespace`, only while younger than
`NEXUS_CLUSTER_HEADROOM_TTL` (2 m), and is reduced by reservations made since
the observation. A gated attempt reads
`waiting_for_cluster_quota: cluster <c> namespace <ns> <dim> (need N, headroom H)`.
It is a routing guard: the API server's quota admission remains the enforcement.

### 28.6 Scheduler placement settings and their cost

| Setting | Default | |
|---|---|---|
| `NEXUS_PLACEMENT_POLICY` | `pack` | `spread` balances load instead of minimizing waste. |
| `NEXUS_PLACEMENT_WAIT` | `3s` | 0 disables pull-path deferral. |
| `NEXUS_CLUSTER_HEADROOM_TTL` | `2m` | |

Metric: `nexus_scheduler_placement_deferrals_total`. A sustained high rate with
growing queue age means preferences point at workers that are always busy;
lower the wait or relax the preference.

Claim latency before and after this change, measured with `tests/bench` at the
ADR-0018 cells on the same machine and database, alternating binaries, is
recorded in the table below (laptop numbers: Apple M4 Pro, Postgres 16 in
Docker Desktop, 64 workers).

**Read these as laptop numbers taken under contention.** Apple M4 Pro,
Postgres 16.15 in Docker Desktop (`shared_buffers=512MB`), 64 workers,
3,000 claims per cell (backlog/2 at 1k). Other sessions' benchmark and
cluster containers ran on the same machine throughout (`docker stats` showed a
second Postgres benchmark at ~100 % CPU), so single cells swing by 2-3x
between runs of the SAME binary. Before = `integrate/wave-2`, after = this
branch; binaries alternated on one migrated database (0035 applied, which the
old binary ignores). Three rounds; round 3 ran on the quietest machine and is
shown. Full tables: rounds 1-3 reproduce with `NEXUS_BENCH_*` as in
`tests/bench/dispatch_test.go`.

| path | backlog | replicas | before claims/s | after claims/s | before p50 / p99 | after p50 / p99 |
|---|---:|---:|---:|---:|---:|---:|
| pull | 1,000 | 1 | 172 | 172 | 5.0 / 14.0 ms | 5.3 / 12.9 ms |
| pull | 1,000 | 4 | 501 | 507 | 7.0 / 18.3 ms | 6.6 / 22.5 ms |
| pull | 1,000 | 16 | 871 | 846 | 15.7 / 72.8 ms | 15.6 / 83.9 ms |
| pull | 10,000 | 1 | 130 | 140 | 6.1 / 28.2 ms | 6.1 / 21.2 ms |
| pull | 10,000 | 4 | 383 | 515 | 8.6 / 37.9 ms | 6.9 / 17.6 ms |
| pull | 10,000 | 16 | 999 | 754 | 13.8 / 54.4 ms | 16.4 / 125.5 ms |
| pull | 100,000 | 1 | 131 | 136 | 6.1 / 24.1 ms | 6.1 / 24.1 ms |
| pull | 100,000 | 4 | 435 | 262 | 7.5 / 35.6 ms | 11.6 / 63.1 ms |
| pull | 100,000 | 16 | 785 | 815 | 16.0 / 112.4 ms | 16.5 / 69.6 ms |
| push | 1,000 | 1 | 74 | 32 | 12.1 / 29.8 ms | 18.7 / 158.5 ms |
| push | 1,000 | 4 | 67 | 128 | 43.4 / 197.6 ms | 22.7 / 79.6 ms |
| push | 1,000 | 16 | 47 | 145 | 169.6 / 534.3 ms | 48.8 / 141.9 ms |
| push | 10,000 | 1 | 21 | 30 | 46.2 / 91.0 ms | 31.4 / 61.1 ms |
| push | 10,000 | 4 | 30 | 73 | 99.8 / 467.9 ms | 37.5 / 166.6 ms |
| push | 10,000 | 16 | 12 | 47 | 849 ms / 4.30 s | 161 ms / 934 ms |
| push | 100,000 | 1 | 3 | 3 | 358 / 828 ms | 332 / 603 ms |
| push | 100,000 | 4 | 7 | 7 | 398 ms / 2.19 s | 400 ms / 1.53 s |
| push | 100,000 | 16 | 7 | 4 | 1.63 / 7.52 s | 3.05 / 11.84 s |

GPU work on the new path (`NEXUS_BENCH_GPU=true`: every attempt a shared-GPU
slice on workers with 8 shared devices each; after only, round 3):

| path | backlog | replicas | claims/s | p50 / p99 |
|---|---:|---:|---:|---:|
| pull | 1,000 | 1 | 159 | 5.3 / 17.5 ms |
| pull | 10,000 | 1 | 159 | 5.4 / 16.4 ms |
| pull | 100,000 | 1 | 158 | 5.6 / 15.3 ms |
| pull | 100,000 | 16 | 745 | 17.2 / 69.3 ms |
| push | 1,000 | 1 | 116 | 8.0 / 14.4 ms |
| push | 10,000 | 1 | 27 | 32.9 / 68.6 ms |

**Reading.** On the pull path (the default deployment) single-replica p50 is
unchanged within 0.3 ms at every backlog, and across three rounds the
before/after differences change sign from round to round: no regression is
distinguishable from the machine's noise. Non-GPU claims do strictly more work
only in reading the cluster columns (one guarded LATERAL lookup that is skipped
for a worker without `nexus.io/cluster`) and decoding a slightly wider worker
row; everything else is behind `resource_request ? ...` / `spec_json ?
'placement'` guards or `WantsScoring`, which a plain CPU task never passes.
Fractional-GPU claims cost the same as non-GPU ones on the pull path. The push
path is unwindowed (ADR-0018) and, under contention, its cells swing both ways
(round 1 after was worse at 16 replicas, round 3 after is better in 7 of 9
cells); the one structural addition there, per-worker cluster lookups, was
collapsed into the guarded LATERAL join after round 1. Re-measure on
production-class hardware with no co-tenant load before relying on any single
cell.

### 28.7 Rollout and rollback

1. Apply migration 0035 (additive; metadata-only column adds). A scheduler
   started before it logs "placement facts are off" and dispatches as before.
2. Roll the scheduler, resource manager and gateway, then agents, then the
   cluster-controller. Old agents ignore `TaskAssignment.reserved` and place
   shared slices locally with the same rule; old gateways drop the new
   heartbeat fields (the stored cache set is then left alone).
3. Rollback: `NEXUS_PLACEMENT_WAIT=0` stops deferral; removing an agent's GPU
   settings removes its fractional capacity at its next session. Attempts
   holding fractional reservations complete normally. Revert 0035 only after
   every service is back on a release that does not read it.

**Order hazard:** do not submit specs with `placement` or fractional requests
to a gateway older than this release (it rejects them at the schema), and do
not run such tasks on old schedulers (they ignore the constraints).

### 28.8 What needs real hardware to qualify

Everything above is verified with simulated devices (unit, property,
integration and compose end-to-end tests). These claims are **not** qualified
until run on the named hardware:

| Area | Needs | What to verify |
|---|---|---|
| MIG discovery | A100/H100 with MIG enabled, mixed profiles | `NEXUS_GPU_DISCOVERY=nvidia-smi` parses the real `nvidia-smi -L` (driver branch differences), UUIDs match `nvidia-smi mig -lgi` |
| MIG isolation, docker | Same, NVIDIA container toolkit | `--gpus device=MIG-...` exposes exactly the instance; two tasks on different instances cannot see each other's memory; `nvidia-smi` inside shows one device |
| MIG, kubernetes | GPU operator / device plugin with `mixed` strategy | `nvidia.com/mig-<profile>` requests schedule and bind; quota headroom counts them via `NEXUS_CLUSTER_GPU_RESOURCES` |
| MPS limits | Volta+ GPU with the MPS control daemon | `CUDA_MPS_PINNED_DEVICE_MEM_LIMIT` refuses allocations past the declared VRAM; `ACTIVE_THREAD_PERCENTAGE` caps SMs; the pipe mount and `--ipc=host` work under the container toolkit; one client's fatal fault behaviour documented |
| MPS/time-slicing on kubernetes | Device plugin with sharing config (`replicas`) | shared replicas bind; per-device placement is advisory as documented |
| Time-slicing | Any NVIDIA GPU | throughput interference of packed jobs, to set sane `gpu_share_milli` guidance |
| VRAM packing economics | H100 80 GB | several vLLM/Triton servers packed by `gpu_share_memory_bytes` with `gpu_memory_utilization` derived from `NEXUS_GPU_SHARE_MEMORY_BYTES` |
| Spot, AWS | EC2 spot instance (or AWS FIS "send spot interruption") | IMDSv2 notice seen within one poll; tasks checkpoint and report PREEMPTED before the instance stops; NTH taint path |
| Spot, GCP | Spot/preemptible VM, `gcloud compute instances simulate-maintenance-event` | `preempted` flips; 30 s budget suffices for report |
| Spot, Azure | Spot VMSS, `simulateEviction` API | Scheduled Events Preempt observed, `NotBefore` deadline honoured |
| Spot, Kubernetes | Karpenter/cluster-autoscaler interruption | taint observed; `get nodes` RBAC; capacity-type discovery from Node labels |
| Artifact cache at scale | Local NVMe, 100 GB+ models | fetch throughput, `VERIFY=hit` cost, eviction under pinned load, disk-pressure interaction with kubelet |
| Headroom accuracy | A cluster with ResourceQuotas under concurrent dispatch | refusal rate at the quota edge vs the guard's prediction |

### 28.9 Known gaps

- Quotas (ADR-0021) do not count MIG instances or shared fractions yet
  (`max_gpu_count` counts whole devices); mirror them with `max_custom` if a
  hard cap is needed now.
- Pull-path deferral reads at most 256 peer workers per tenant and queue.
- Cluster labels need a running cluster-controller; without one only the
  agent's own labels count.
- The console does not display placement specs, fractional GPU ledgers or
  cluster labels yet (visible through the API and SQL above).

---

## 29. AI workloads: usage units, shared rate limits and checkpoints

Design: [ADR-0032](adr/0032-ai-workloads-usage-rate-limits-checkpoints.md).
Migration 0036. Findings F148 (Python heartbeats never reached the control
plane; the workload API) and F149 (retries under role separation).

### 29.1 What a task can do

A task started by the subprocess or docker executor finds an agent-local
socket in `NEXUS_TASK_API` (with a bearer token in `NEXUS_TASK_API_TOKEN`).
Through it, and through the SDK calls that wrap it, a task can:

| SDK call | What reaches the control plane |
|---|---|
| `ctx.report_usage({...})`, `ctx.report_tokens(...)`, `LLMClient` | cumulative usage units on the attempt (`attempt_usage`), copied into the usage ledger's `custom` object when the attempt ends |
| `ctx.acquire_permits(keys, requests=, tokens=)`, `LLMClient` | permits from the namespace's shared token buckets (`rate_limit_buckets`) |
| `ctx.checkpoint(state)` / `ctx.resume()` | an immutable object `<ns>/checkpoints/<idempotency_key>/<attempt_id>/<seq>` and a forward-only pointer (`task_checkpoints`) |
| `ctx.heartbeat()` / `ctx.progress()` | ReportProgress, which re-arms the heartbeat timeout (before 0036 these were no-ops under a real agent, F148) |
| raising `nexus.RateLimited` | `ERROR_CLASS_RATE_LIMITED` with the provider's Retry-After |

The **kubernetes executor** does not offer the socket yet: pods run on other
nodes. There the SDK degrades to in-process behaviour (usage logged, permits
process-local, checkpoints in `ctx.store`); do not rely on shared limits or
recorded token usage for tasks on that executor.

The gateway advertises the capabilities at registration
(`NEXUS_WORKLOAD_API`, default `true`; `NEXUS_MAX_CHECKPOINT_BYTES`, default
64 MiB). The agent's socket root is `NEXUS_TASK_API_ROOT` / `-task-api-root`
(default `/tmp/nexus-task`; it must be owned by the agent user and short,
because a Unix socket path is limited to 104 bytes on Darwin). If the root is
unusable the agent logs one ERROR at registration and runs tasks without the
workload API. The docker executor bind-mounts the socket directory at
`/run/nexus-task`; that works on Linux hosts and was not verified under
Docker Desktop's VM.

### 29.2 Usage units and the ledger

Keys are `<kind>:<subject>[:<qualifier>]`:

```
tokens:<provider>/<model>:input     tokens:<provider>/<model>:output
tokens:<provider>/<model>:cached    requests:<provider>/<model>
```

Provider and model never contain `:` (the SDK maps any to `_`, so
`ft:gpt-4o-mini:org:x` is reported as `openai/ft_gpt-4o-mini_org_x`). Values are
non-negative integers, at most 64 keys per attempt. The price book prices
every ledger `custom` key that contains `:` as a unit with no time factor.

```sql
-- Tokens by model for one namespace, last 7 days, from the ledger.
SELECT key, sum(value::numeric) AS units
  FROM usage_ledger, jsonb_each_text(custom) AS c(key, value)
 WHERE namespace = 'team-a' AND ended_at > now() - interval '7 days'
   AND strpos(key, ':') > 0
 GROUP BY key ORDER BY key;
```

Units reported by an attempt that later failed, was LOST or was cancelled are in
its ledger row: they were spent. The console shows them per attempt and summed
per run ("Usage (all attempts)"). Usage is self-reported by task code; treat
it as cost attribution, not as the provider's invoice. Usage keys cannot be
quota-limited yet (`max_custom` names refuse `:`); that is a follow-up.

### 29.3 Configuring shared rate limits

Limits are part of the quotas document (`quotas.manage`, audited, revisioned):

```sh
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  $API/v1/namespaces/$NS/quotas -d '{
    "revision": 7,
    "quotas": {"max_inflight_attempts": 200,
               "rate_limits": {
                 "openai":             {"requests_per_second": 50},
                 "openai/gpt-4o-mini": {"requests_per_second": 20, "tokens_per_minute": 2000000}}}}'
```

A key is `<provider>` or `<provider>/<model>`; `LLMClient` asks for both by
default, all or none. `burst_requests` / `burst_tokens` default to one second
of requests (at least 1) and one minute of tokens. A PUT that omits
`rate_limits` keeps the stored ones. While quotas are locked, a namespace
administrator may only lower a rate or add a key. Unconfigured keys are
unlimited. Inspect the live buckets:

```sql
SELECT t.namespace, b.key, round(b.request_level::numeric, 2) AS requests,
       round(b.token_level::numeric) AS tokens, b.granted_requests, b.granted_tokens, b.denied
  FROM rate_limit_buckets b JOIN tenants t ON t.id = b.tenant_id ORDER BY 1, 2;
```

A bucket is state, not configuration: deleting its row resets it to full.

### 29.4 Throttling and retries

`LLMClient` retries a 429/503 in the task, honouring `Retry-After`, up to
`max_retries`; past that (or when the provider asks for longer than
`max_retry_wait_s`) the task fails `RATE_LIMITED`. The scheduler then retries
it **without charging the retry budget**, after the provider's Retry-After
clamped to [1 s, 15 min], up to `NEXUS_RATE_LIMITED_FREE_RETRIES` per node
(scheduler flag `-rate-limited-free-retries`; default 10, negative disables).
The RetryScheduled event says `"charged": false` and `free_retries_used`.
Past the cap the failure is charged like TRANSIENT. `no_retry_on:
[rate_limited]` vetoes it. An operator retry (§18) resets the count.

### 29.5 Checkpoints

A retry, and an operator retry of the node, resume from the latest
checkpoint of the node's idempotency key; a new run starts clean. The pointer
moves only forward (a higher attempt number, or the same attempt and a higher
seq), and only while the saving attempt holds its lease, so a zombie cannot
overwrite a newer checkpoint. `ctx.resume()` verifies the object's sha256 and
returns `None` (and logs) if it is missing or corrupt; starting over is always
correct.

artifact-gc (§7) treats the pointer of every node that has not SUCCEEDED as
live. Superseded checkpoints, fenced uploads, the checkpoints of succeeded
nodes and everything of a deleted run are orphans, collected after the grace
period. `make artifact-gc` prints how many checkpoint objects it examined.

### 29.6 Capacity: the permit service

Every permit is one short statement on the bucket row, never inside the
dispatch claim. Measured with `make bench-pg && make bench-ratelimit`
(`tests/bench/ratelimit_test.go`) on the ADR-0018 laptop (Apple M4 Pro,
Postgres 16.15 in Docker Desktop, 10 vCPU; other workloads were running, so
treat the numbers as indicative), 8 callers per replica, every request
granted:

| keys | replicas (x8 callers) | permits/s | p50 | p99 |
|---|---:|---:|---:|---:|
| 1 hot | 1 | 817 | 7.9 ms | 36 ms |
| 1 hot | 4 | 625 | 31 ms | 258 ms |
| 1 hot | 16 | 373 | 179 ms | 1.65 s |
| 16 spread | 1 | 1,072 | 6.3 ms | 25 ms |
| 16 spread | 4 | 1,624 | 16 ms | 76 ms |
| 16 spread | 16 | 775 | 105 ms | 670 ms |

The first implementation held the row lock across three client round trips
and managed 506 / 300 / 118 permits/s on the hot key; the single-statement
version replaced it. Under a saturated 100 req/s limit, granted permits never
exceeded burst + rate x elapsed at 1, 4 or 16 replicas (e.g. 594 granted
against a ceiling of 600 in 5 s).

**Reading it.** A single key sustains several hundred permits per second
with 8-32 concurrent callers, far above typical provider limits; with 128
callers queued on one row, latency grows to seconds. Spread load (per-model
keys) scales with replicas until the database CPU saturates. Permit traffic
shares the database with dispatch: pull-claim throughput at a 10k backlog with
4 replicas measured 175-184 claims/s alone and 75-161 claims/s beside 270-300
permits/s on one hot key (two runs). Size limits so the fleet's permit rate
stays in the low hundreds per second per key, and prefer fewer, larger calls
(batching prompts) to many tiny ones.

### 29.7 Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `nexus_scheduler_usage_reports_total` | `result` (stored, unchanged, stale, invalid, error) | ReportUsage calls |
| `nexus_scheduler_usage_units_total` | `kind` (tokens, requests, other) | growth of reported units; `rate(...{kind="tokens"}[5m])` is the fleet's token throughput |
| `nexus_scheduler_permit_requests_total` | `result` (granted, denied, unlimited, settled, stale, invalid, error) | AcquirePermits calls |
| `nexus_scheduler_permit_duration_seconds` | | latency of one permit call |
| `nexus_scheduler_checkpoints_total` | `result` (saved, superseded, stale, invalid, error) | SaveCheckpoint calls |
| `nexus_agent_workload_api_requests_total` | `endpoint`, `code` | requests tasks made to their socket |
| `nexus_agent_usage_flushes_total` | `result` (ok, stale, error) | usage snapshots sent |
| `nexus_agent_checkpoints_total` | `result` (saved, stale, too_large, error) | checkpoints written by the agent |

Keys and model names are never labels. Worth alerting on: a sustained
`permit_requests_total{result="error"}`, `permit_duration_seconds` p99 above
1 s (a hot key; spread it or raise the limit), and
`usage_reports_total{result="invalid"}` (a task reporting malformed keys).

### 29.8 Running the AI examples and templates

`examples/llm_batch_inference` and `examples/llm_eval` run against the SDK's
mock model server; nothing calls a real provider. Without a stack:

```sh
python3 examples/llm_batch_inference/check_local.py
python3 examples/llm_eval/check_local.py
```

On a stack, the AI overlay adds a Python-capable agent and the mock sidecar
(`mock-llm:8089`, injecting a 429 every 7th call):

```sh
E2E_PROJECT=nexus-ai E2E_PORT_BASE=55711 E2E_AI=1 deploy/e2e/e2e-stack.sh up
. ui/e2e/.stack.env
NEXUS_ENDPOINT=$E2E_API_URL NEXUS_API_KEY=$E2E_API_KEY SUITE=ai examples/run-suite.sh
```

`nexusctl init llm-batch-inference | llm-eval | fine-tune` generate projects
with the same patterns and their own mock-backed tests.

**Real providers.** Point `base_url` at `https://api.openai.com` or
`https://api.anthropic.com`, set `api_key_env` to the variable that holds the
key (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`), and deliver that variable to the
task through the platform's secret mechanism, never in a spec, a parameter or
a log. Configure `rate_limits` (§29.3) at or below the organisation's provider
limits before the first large run: the platform limit is what keeps a
5,000-way fan-out inside them.

### 29.9 Upgrading and rolling back

Apply 0036 before rolling out services. A scheduler started without it logs
"the workload API is off", keeps the pre-0036 ledger statement and ignores
free retries; its workload RPCs answer Unavailable. A gateway without the
feature (`NEXUS_WORKLOAD_API=false`, or an old build) advertises nothing and
agents offer tasks no usage, permit or checkpoint endpoints; the SDK falls
back in-process. Rolling back 0036 (`0036_workload_api.down.sql`) drops
reported units not yet in the ledger, bucket state and checkpoint pointers;
set `NEXUS_WORKLOAD_API=false` and restart the gateways and schedulers first.
