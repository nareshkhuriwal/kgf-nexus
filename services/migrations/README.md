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

## Reserved: 0028–0031 while wave 3 merges

Wave-3 branches were each handed migration numbers in advance: 0028–0031 to
parallel branches and 0032 (`0032_sso_and_rbac`) to the SSO branch. On any
single branch the numbers another branch owns are absent, so
`internal/migrations/safety_test.go` lists them in `reservedVersions`, which
tolerates them being present or absent. The integration branch removes that
map once all of them have landed; after that a missing number fails again.
Do not use a reserved number for anything else.

## Known gap: there is no 0019

The sequence goes `0018_cluster_registry` → `0020_dynamic_expansion`. No
migration numbered 0019 has ever existed on any branch in this repository's git
history (checked with `git log --all -- 'services/migrations/0019*'`). The
number was skipped, most likely reserved by parallel work that landed under
other numbers. No code, migration or document refers to a 0019, so no schema
change is missing. The number was simply never used.

**Do not create a 0019, not even an empty one.** The appliers pick up every file
whose basename is not in the ledger, in lexical order. On a database already at
0020 or later, a new `0019_*` would run *after* 0020 and every later
migration. On a fresh database it would run *before* them. The same file would
then produce different schemas depending on when a database was created. The
next migration takes the next number after the highest existing one.

A gap does not affect applying migrations. `deploy/migrate.sh`, `make migrate-up`
and `make migrate-down` glob `*.up.sql` / `*.down.sql` and key the ledger on
filenames; none of them counts files or expects consecutive numbers.

`internal/migrations/safety_test.go` holds this in place:

- `TestVersionNumbersAreUnique` fails when two files share a version.
- `TestVersionGapsAreOnlyTheKnownOnes` fails when a version appears below the
  current highest (filling 0019, for example) or when a new gap opens.
- `TestMigrationFilenamesAreWellFormed` fails on names that break the
  `NNNN_description.{up,down}.sql` pattern, and on a `.down.sql` with no `.up.sql`.
