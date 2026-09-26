// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const migrationsDir = "../../services/migrations"

// accepted records the findings that are real, understood, and deliberately kept.
//
// An allowlist rather than a softened rule, so the exception is visible in review
// and a NEW occurrence of the same pattern still fails. Keyed by file and rule:
// the line is deliberately excluded, because reformatting a migration must not
// require editing this list.
var accepted = map[string]string{
	// 0001 needs no entry: the analyser recognises that an index on a table the
	// same migration CREATEs builds on an empty table. That is the general rule, not
	// an exception -- see TestIndexOnAFreshTableIsNotFlagged.

	// 0002's backfill is the precedent this whole package exists to catch, and it
	// is already applied everywhere. It cannot be rewritten now: migrate.sh keys
	// the ledger on the file name, so editing an applied migration changes nothing
	// on any database that already ran it while silently diverging from the one
	// place the schema is described. Kept, with the cost recorded in
	// docs/OPERATIONS.md, and the rule now stops the NEXT one.
	"0002_event_seq.up.sql/unbounded-update": "applied everywhere; retained as the documented precedent",

	// 0015 is the DEFERRED HALF of 0014, and is the shape this rule asks for
	// rather than an exception to it.
	//
	// The rule says: "add a DEFAULT and enforce NOT NULL a release later." 0014
	// adds tenant_id nullable, backfills it and installs triggers that assign it
	// on every INSERT and UPDATE. 0015 enforces it, in its own migration, to be
	// applied a release later. Splitting them is what makes the enforcement safe,
	// so the finding fires on the file that did the right thing -- the analyser
	// reads one file at a time and cannot see that 0014 came first.
	//
	// What makes it safe is the trigger, not the delay: once assign_*_tenant is
	// installed, a replica that predates the column still writes a correct
	// tenant_id, because the trigger supplies it. A DEFAULT cannot, since the
	// value depends on the row's namespace or its parent.
	//
	// The operator requirement is real and is recorded in the migration's header:
	// do not apply 0015 in the same release as 0014.
	"0015_enforce_tenant_ownership.up.sql/set-not-null": "deferred enforcement of 0014, applied a release later once the 0014 triggers populate tenant_id on every write",

	// The entries below were found when the gate learned DROP INDEX, DROP
	// DEFAULT and validating ADD CONSTRAINT (CONTRACT-FINDINGS F128). All three
	// migrations are applied everywhere and cannot be edited.

	// 0014 adds its tenant foreign keys and (tenant_id, id) unique constraints
	// to populated tables without NOT VALID, so each scanned its table under a
	// write-blocking lock. Old replicas cannot violate them: the same migration
	// installs the triggers that assign tenant_id on every write. The cost was
	// a one-off stall at the 0014 upgrade.
	"0014_explicit_tenant_ownership.up.sql/validating-constraint": "applied everywhere; old writers are covered by the 0014 tenant triggers",

	// 0017 adds email_verified_at with DEFAULT now() only so existing accounts
	// read as verified, then drops the default so new ones start unverified.
	// The column is nullable, and NULL from an old replica is the fail-safe
	// answer ("must still prove the address"), as the migration's header says.
	"0017_email_verification.up.sql/drop-default": "nullable column; NULL from an old replica is the intended fail-safe",

	// 0023 drops only an INVALID leftover of a failed CONCURRENTLY build,
	// which the planner never uses, and re-creates it under the same name.
	// The name is built at run time (format('DROP INDEX %I', ix)), which is
	// why the analyser cannot see the re-creation as it does for 0022 and 0027.
	"0023_dispatch_rank_index.up.sql/drop-index": "drops only an INVALID index it re-creates by name; the name is dynamic",

	// 0003 and 0004 are deliberately ABSENT from this list. Both carry a top-level
	// WHERE that bounds the write set -- 0003 to rows still holding the old JSONB
	// key spelling, 0004 to rows that actually drifted -- so the analyser does not
	// flag them, which is the behaviour that makes the rule worth having. They are
	// the safe shape of the statement 0002 got wrong.
}

// TestNoNewRollingDeployHazards is the gate.
//
// ADR-0011 rule 5 promises a rolling deploy can run mixed versions against one
// schema, and nothing enforced that: migrate.sh applies whatever SQL it finds. A
// DROP COLUMN, a rename, or a SET NOT NULL lands fine on an empty dev database and
// takes the fleet down mid-deploy, because the old replicas are still serving.
func TestNoNewRollingDeployHazards(t *testing.T) {
	findings, err := Analyse(migrationsDir)
	if err != nil {
		t.Fatalf("analyse migrations: %v", err)
	}

	var unexpected []Finding
	for _, f := range findings {
		if _, ok := accepted[f.File+"/"+f.Rule]; !ok {
			unexpected = append(unexpected, f)
		}
	}
	for _, f := range unexpected {
		t.Errorf("migration hazard:\n%s", f)
	}
	if len(unexpected) > 0 {
		t.Log("If a finding is deliberate, add it to `accepted` with the reason. " +
			"Do not relax the rule: the next migration needs it.")
	}
}

// TestAcceptedListIsNotStale keeps the allowlist honest. An entry that no longer
// matches anything means a migration changed shape, and a stale exemption is how a
// hazard gets silently re-admitted later.
func TestAcceptedListIsNotStale(t *testing.T) {
	findings, err := Analyse(migrationsDir)
	if err != nil {
		t.Fatalf("analyse migrations: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range findings {
		seen[f.File+"/"+f.Rule] = true
	}
	for key := range accepted {
		if !seen[key] {
			t.Errorf("accepted[%q] matches nothing; remove it, or the exemption will "+
				"cover a future hazard nobody reviewed", key)
		}
	}
}

// TestEveryUpHasADown covers the other half of rule 5: a migration you cannot roll
// back is one you cannot deploy cautiously.
func TestEveryUpHasADown(t *testing.T) {
	ups, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(ups) == 0 {
		t.Fatal("no migrations found; the gate would pass vacuously")
	}
	for _, up := range ups {
		down := strings.TrimSuffix(up, ".up.sql") + ".down.sql"
		if _, serr := filepath.Glob(down); serr != nil {
			t.Fatalf("glob %s: %v", down, serr)
		}
		matches, _ := filepath.Glob(down)
		if len(matches) == 0 {
			t.Errorf("%s has no .down.sql; a migration that cannot be rolled back "+
				"cannot be deployed cautiously", filepath.Base(up))
		}
	}
}

// TestRulesActuallyFire guards the gate itself. A regex that matches nothing is
// worse than no rule, because it reads in review as protection that is not there.
func TestRulesActuallyFire(t *testing.T) {
	cases := map[string]string{
		"drop-column":                    "ALTER TABLE workflow_runs DROP COLUMN event_seq;",
		"drop-table":                     "DROP TABLE run_events;",
		"rename":                         "ALTER TABLE workflow_runs RENAME COLUMN event_seq TO seq;",
		"alter-column-type":              "ALTER TABLE workflow_runs ALTER COLUMN event_seq SET DATA TYPE bigint;",
		"set-not-null":                   "ALTER TABLE workflow_runs ALTER COLUMN params SET NOT NULL;",
		"add-column-not-null-no-default": "ALTER TABLE workflow_runs ADD COLUMN owner TEXT NOT NULL;",
		"unbounded-update":               "UPDATE workflow_runs SET event_seq = 0;",
		"create-index-blocking":          "CREATE INDEX idx_x ON run_events (run_id);",
	}
	for want, sql := range cases {
		got := firstMatch(sql)
		if got != want {
			t.Errorf("%q matched rule %q, want %q", sql, got, want)
		}
	}
}

// TestDestructiveFormsAreCaught is the audit of what the gate used to miss.
// Each of these passed it before, and each can break a replica of the previous
// release still serving during a rolling deploy, or lose data outright.
func TestDestructiveFormsAreCaught(t *testing.T) {
	cases := []struct{ rule, sql string }{
		// An old replica's ON CONFLICT (cols) or ON CONSTRAINT names the
		// constraint; its writes start failing the moment it is gone.
		{"drop-constraint", "ALTER TABLE workflow_specs DROP CONSTRAINT workflow_specs_namespace_name_version_key;"},
		{"drop-constraint", "ALTER TABLE ONLY leases DROP CONSTRAINT IF EXISTS leases_pkey CASCADE;"},
		{"drop-index", "DROP INDEX idx_attempts_dispatch;"},
		{"drop-index", "DROP INDEX CONCURRENTLY IF EXISTS idx_attempts_dispatch;"},
		{"drop-index", "DO $$ BEGIN EXECUTE 'DROP INDEX idx_x'; END $$;"},
		// Not only tables and columns get renamed.
		{"rename", "ALTER INDEX idx_a RENAME TO idx_b;"},
		{"rename", "ALTER TYPE run_state RENAME VALUE 'PAUSING' TO 'SUSPENDING';"},
		{"rename", "ALTER TYPE run_state RENAME TO run_status;"},
		{"rename", "ALTER SEQUENCE run_events_id_seq RENAME TO ev_seq;"},
		{"rename", "ALTER FUNCTION assign_tenant() RENAME TO assign_tenant_v2;"},
		{"rename", "ALTER VIEW v_runs RENAME TO runs_view;"},
		{"rename", "ALTER TABLE workflow_runs RENAME CONSTRAINT a TO b;"},
		// Every spelling of a column type change.
		{"alter-column-type", "ALTER TABLE t ALTER COLUMN c TYPE bigint;"},
		{"alter-column-type", "ALTER TABLE t ALTER c TYPE bigint USING c::bigint;"},
		{"alter-column-type", `ALTER TABLE t ALTER COLUMN "c" SET DATA TYPE text;`},
		// Dropping other objects an old replica or a trigger still uses.
		{"drop-object", "DROP TYPE run_state;"},
		{"drop-object", "DROP FUNCTION IF EXISTS assign_tenant_id();"},
		{"drop-object", "DROP VIEW v_runs;"},
		{"drop-object", "DROP MATERIALIZED VIEW mv_usage;"},
		{"drop-object", "DROP SEQUENCE run_events_id_seq;"},
		{"drop-object", "DROP SCHEMA archive CASCADE;"},
		// The 0014 tenant triggers are what let old replicas keep writing.
		{"drop-trigger", "DROP TRIGGER tenant_assignment ON task_attempts;"},
		{"truncate", "TRUNCATE run_events;"},
		{"truncate", "TRUNCATE TABLE ONLY run_events RESTART IDENTITY;"},
		{"drop-default", "ALTER TABLE users ALTER COLUMN role DROP DEFAULT;"},
		{"unbounded-delete", "DELETE FROM run_events;"},
		{"validating-constraint", "ALTER TABLE task_attempts ADD CONSTRAINT attempts_fk FOREIGN KEY (worker_id) REFERENCES workers(id);"},
		{"validating-constraint", "ALTER TABLE task_attempts ADD CONSTRAINT attempts_uq UNIQUE (run_id, node_id, attempt_number);"},
		{"validating-constraint", "ALTER TABLE task_attempts ADD CHECK (attempt_number > 0);"},
	}
	for _, c := range cases {
		fs := analyseString(t, c.sql)
		if len(fs) != 1 || fs[0].Rule != c.rule {
			t.Errorf("%q: findings %v, want exactly one %q", c.sql, fs, c.rule)
		}
	}
}

// TestSafeFormsOfTheNewRulesAreNotFlagged: the additive or self-contained
// shapes of the statements above.
func TestSafeFormsOfTheNewRulesAreNotFlagged(t *testing.T) {
	safe := []string{
		// Validated later, without the scan under lock.
		"ALTER TABLE task_attempts ADD CONSTRAINT attempts_fk FOREIGN KEY (worker_id) REFERENCES workers(id) NOT VALID;",
		"ALTER TABLE task_attempts VALIDATE CONSTRAINT attempts_fk;",
		// A column named "type" is not a type change.
		"ALTER TABLE events ADD COLUMN type TEXT;",
		"ALTER TABLE events ADD CONSTRAINT events_type_ck CHECK (type IN ('a','b')) NOT VALID;",
		// A bounded delete.
		"DELETE FROM idempotency_keys WHERE created_at < now() - interval '1 day';",
		// Adding an enum value is additive.
		"ALTER TYPE run_state ADD VALUE IF NOT EXISTS 'SUSPENDED';",
		// Recreating a function in place keeps its name.
		"CREATE OR REPLACE FUNCTION f() RETURNS int AS $$ SELECT 1 $$ LANGUAGE sql;",
		// Dropped and re-created under the same name in the same migration:
		// the idempotent re-apply idiom every tenant trigger uses.
		`DROP TRIGGER IF EXISTS tenant_assignment ON email_tokens;
CREATE TRIGGER tenant_assignment BEFORE INSERT ON email_tokens FOR EACH ROW EXECUTE FUNCTION assign_tenant();`,
		`DO $$ BEGIN EXECUTE 'DROP INDEX idx_attempts_inflight_tenant'; END $$;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_attempts_inflight_tenant ON task_attempts (tenant_id);`,
		`DROP FUNCTION IF EXISTS assign_tenant();
CREATE FUNCTION assign_tenant() RETURNS trigger AS $$ BEGIN RETURN NEW; END $$ LANGUAGE plpgsql;`,
		// Constraints and triggers on a table this migration created.
		`CREATE TABLE quotas (id uuid PRIMARY KEY, ns text);
ALTER TABLE quotas ADD CONSTRAINT quotas_ns_uq UNIQUE (ns);
ALTER TABLE quotas DROP CONSTRAINT quotas_ns_uq;`,
	}
	for _, sql := range safe {
		if fs := analyseString(t, sql); len(fs) != 0 {
			t.Errorf("%q was flagged: %v", sql, fs)
		}
	}
}

// TestSafeStatementsAreNotFlagged is the other direction: a gate that fires on
// safe SQL gets switched off, which is the same as having none.
func TestSafeStatementsAreNotFlagged(t *testing.T) {
	safe := []string{
		// The additive shape rule 5 actually prescribes.
		"ALTER TABLE workflow_runs ADD COLUMN owner TEXT;",
		"ALTER TABLE workflow_runs ADD COLUMN event_seq INT NOT NULL DEFAULT 0;",
		"CREATE INDEX CONCURRENTLY idx_x ON run_events (run_id);",
		"CREATE TABLE schedules (id uuid PRIMARY KEY);",
		"GRANT SELECT ON run_events TO nexus_scheduler;",
		// A bounded backfill is the safe form of 0002's statement.
		"UPDATE workflow_runs SET event_seq = 1 WHERE event_seq < 1;",
		"SELECT 1;",
	}
	for _, sql := range safe {
		if name := firstMatch(sql); name != "" {
			t.Errorf("%q was flagged by %q; a gate that fires on safe SQL gets "+
				"disabled, which is the same as not having one", sql, name)
		}
	}
}

// TestCommentsDoNotTriggerRules matters because these migrations are heavily
// commented and several comments quote the exact statements the rules look for --
// 0004's header discusses max(seq) and 0005's discusses GRANT and DROP ROLE.
func TestCommentsDoNotTriggerRules(t *testing.T) {
	src := `-- ALTER TABLE workflow_runs DROP COLUMN event_seq;
-- and then we would DROP TABLE run_events, which is why we do not.
SELECT 1;`
	for _, st := range split(src) {
		if name := firstMatch(st.text); name != "" {
			t.Errorf("rule %q fired on commented-out SQL: %q", name, st.text)
		}
	}
}

// TestDollarQuotedBodiesAreOneStatement covers 0005, which is a single DO $$ block
// containing semicolons. Splitting on those would misread one statement as many and
// could match a rule across a boundary that does not exist.
func TestDollarQuotedBodiesAreOneStatement(t *testing.T) {
	src := `DO $$
BEGIN
    EXECUTE 'GRANT SELECT ON x TO y';
    EXECUTE 'GRANT INSERT ON x TO y';
END $$;`
	if got := split(src); len(got) != 1 {
		t.Errorf("split produced %d statements, want 1; a $$ body's semicolons do not end a statement", len(got))
	}
}

// firstMatch mirrors Analyse's matching, predicate included, so these tests
// exercise the real decision rather than the regexes in isolation.
func firstMatch(stmt string) string {
	for _, r := range rules {
		if !r.re.MatchString(stmt) {
			continue
		}
		if r.extra != nil && !r.extra(stmt) {
			continue
		}
		return r.name
	}
	return ""
}

// TestIndexOnAFreshTableIsNotFlagged covers the distinction that keeps the
// allowlist from growing for ordinary safe work.
//
// CREATE INDEX without CONCURRENTLY stalls writes only if the table has rows. An
// index on a table the SAME migration just created is building on an empty table,
// so the cost the rule warns about does not exist. Without this, every migration
// that introduces a table with indexes would need an exemption, and an allowlist
// that grows for routine work is one people stop reading.
func TestIndexOnAFreshTableIsNotFlagged(t *testing.T) {
	fresh := `CREATE TABLE schedules (id uuid PRIMARY KEY, next_fire_at timestamptz);
CREATE INDEX idx_due ON schedules (next_fire_at);`
	for _, f := range analyseString(t, fresh) {
		if f.Rule == "create-index-blocking" {
			t.Errorf("index on a table created in the same migration was flagged: %s", f)
		}
	}

	// An index on a table that already exists IS the hazard, and must still fire.
	existing := `CREATE INDEX idx_runs ON workflow_runs (namespace);`
	var found bool
	for _, f := range analyseString(t, existing) {
		if f.Rule == "create-index-blocking" {
			found = true
		}
	}
	if !found {
		t.Error("index on a pre-existing table was not flagged; the rule no longer protects anything")
	}
}

// analyseString runs the real analyser over one migration body.
func analyseString(t *testing.T, body string) []Finding {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "0999_probe.up.sql")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write probe migration: %v", err)
	}
	fs, err := Analyse(dir)
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	return fs
}

// TestVersionNumbersAreUnique catches two migrations claiming one version.
//
// It happened: 0007_backfill_lease and 0007_identity were written in parallel
// branches and merged, so both exist. Nothing BROKE, because migrate.sh keys the
// ledger on the full filename and applies in lexical order, and those two touch
// disjoint objects idempotently. But the version number stops being a total order,
// and that is the property the whole scheme rests on:
//
//   - "apply everything after 0006" is no longer answerable;
//   - a third migration numbered 0007 that DID depend on one of them would apply in
//     whatever order the names happened to sort, which is not a decision anyone made;
//   - and a reviewer reading `ls` can no longer tell what ran before what.
//
// Cheap to state, so it is stated here rather than left to be rediscovered by
// whoever writes the migration that finally collides.
func TestVersionNumbersAreUnique(t *testing.T) {
	ups, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(ups) == 0 {
		t.Fatal("no migrations found; the check would pass vacuously")
	}

	seen := map[string][]string{}
	for _, up := range ups {
		name := filepath.Base(up)
		// The version is everything before the first underscore: 0007_identity -> 0007.
		version := name
		if i := strings.Index(name, "_"); i > 0 {
			version = name[:i]
		}
		seen[version] = append(seen[version], name)
	}

	for version, files := range seen {
		if len(files) > 1 {
			sort.Strings(files)
			t.Errorf("version %s is claimed by %d migrations: %v\n"+
				"    Renumber all but one. Two migrations at one version means the "+
				"version no longer expresses which ran first, and nothing else does.",
				version, len(files), files)
		}
	}
}

// migrationName is the only filename shape the appliers and these checks agree
// on: a four-digit version, an underscore, a lower-case description.
var migrationName = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.(up|down)\.sql$`)

// knownVersionGaps are version numbers below the current highest that no
// migration uses, and never will (services/migrations/README.md, "Known
// gaps"). ONE documented set; there is no list of numbers "reserved" for
// branches still in flight.
//
//   - 0019 was skipped: no file with that number has existed on any branch.
//   - 0029 and 0030 were handed to wave-3 branches that landed their schema
//     under other numbers; nothing ever used them.
//   - 0031 was the cost-management branch's (price books, budgets). That
//     branch was held out: cost management moves to KGF billing
//     (docs/OPERATIONS.md §23), so 0031 will not be created in this repo.
//
// A gap must stay empty. The appliers run every file whose basename is not in
// the ledger, in lexical order, so a 0019 (or 0029-0031) added now would run
// AFTER every later migration on every existing database and BEFORE them on a
// fresh one, and the two would not be guaranteed to agree.
var knownVersionGaps = map[int]bool{19: true, 29: true, 30: true, 31: true}

// reservedForParallelBranches are versions handed out to wave-3 branches that
// are developed in parallel and merged by the integrator. On any one branch
// most of them are absent, which is expected, not a dropped migration; once the
// integration branch holds all of them the entries can be removed. Each of these
// migrations is independent of the others (none reads what another adds), so
// the order in which they reach an existing database does not matter -- which
// is the condition for reserving numbers ahead of time at all.
var reservedForParallelBranches = map[int]bool{28: true, 29: true, 30: true, 31: true, 32: true, 33: true, 34: true}

// TestVersionGapsAreOnlyTheKnownOnes pins the version sequence.
//
// Two mistakes look alike to migrate.sh and this catches both. Filling a known
// gap back-dates a migration, as described on knownVersionGaps. Opening a new
// gap usually means a migration is missing from a merge: a branch numbered its
// file 0025 while the 0024 it depended on stayed on another branch.
func TestVersionGapsAreOnlyTheKnownOnes(t *testing.T) {
	versions := migrationVersions(t)
	highest := 0
	for v := range versions {
		highest = max(highest, v)
	}
	for v := 1; v <= highest; v++ {
		switch {
		case versions[v] && knownVersionGaps[v]:
			t.Errorf("migration version %04d is a recorded gap and must stay unused: "+
				"a file there runs after every later migration on existing databases "+
				"but before them on fresh ones. Renumber it to %04d", v, highest+1)
		case !versions[v] && !knownVersionGaps[v] && !reservedForParallelBranches[v]:
			t.Errorf("migration version %04d is missing below the highest (%04d). "+
				"If a merge dropped it, restore it; if the number was deliberately "+
				"skipped, record it in knownVersionGaps and services/migrations/README.md",
				v, highest)
		}
	}
	for v := range knownVersionGaps {
		if v > highest {
			t.Errorf("knownVersionGaps lists %04d, above the highest migration %04d; remove it", v, highest)
		}
	}
}

// TestMigrationFilenamesAreWellFormed keeps the version extractable. The
// uniqueness and gap checks read the version from the name, so a file named
// 19_x.up.sql or 0024-x.up.sql would slip past both while still being applied by
// migrate.sh's *.up.sql glob. It also requires an .up.sql for every .down.sql,
// the direction TestEveryUpHasADown does not cover: `make migrate-down` would
// otherwise run a stray down against a schema nothing created.
func TestMigrationFilenamesAreWellFormed(t *testing.T) {
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read %s: %v", migrationsDir, err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue // README.md is documentation, not a migration
		}
		checked++
		if !migrationName.MatchString(name) {
			t.Errorf("%s does not match NNNN_description.{up,down}.sql", name)
			continue
		}
		if strings.HasSuffix(name, ".down.sql") {
			up := strings.TrimSuffix(name, ".down.sql") + ".up.sql"
			if _, serr := os.Stat(filepath.Join(migrationsDir, up)); serr != nil {
				t.Errorf("%s has no matching %s", name, up)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no migrations found; the check would pass vacuously")
	}
}

// migrationVersions returns the set of versions used by *.up.sql files.
func migrationVersions(t *testing.T) map[int]bool {
	t.Helper()
	ups, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(ups) == 0 {
		t.Fatal("no migrations found; the check would pass vacuously")
	}
	out := map[int]bool{}
	for _, up := range ups {
		m := migrationName.FindStringSubmatch(filepath.Base(up))
		if m == nil {
			continue // reported by TestMigrationFilenamesAreWellFormed
		}
		v, _ := strconv.Atoi(m[1])
		out[v] = true
	}
	return out
}

// TestNoLiteralGroupRolesAfterThePrefixMigration: since 0038 an installation
// may rename its group roles (F153), so a later migration that grants to (or
// revokes from) nexus_scheduler literally would miss a prefixed installation's
// roles -- silently, for a REVOKE. They must name nexus_role('scheduler').
func TestNoLiteralGroupRolesAfterThePrefixMigration(t *testing.T) {
	fs, err := LiteralGroupRoles(migrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		t.Error(f)
	}
}

func TestLiteralGroupRoleCheckFires(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Before the boundary, literal names are the only kind there is.
	write("0037_old.up.sql", "GRANT SELECT ON x TO nexus_scheduler;\n")
	write("0038_installation_role_prefix.up.sql", "GRANT SELECT ON installation_role_prefix TO nexus_dag_engine;\n")
	write("0039_bad.up.sql", "-- grants to nexus_scheduler would be wrong here\n"+
		"GRANT UPDATE (x) ON t TO nexus_scheduler;\n"+
		"DO $$ BEGIN EXECUTE format('REVOKE ALL ON t FROM %I', 'nexus_api_gateway'); END $$;\n")
	write("0040_good.up.sql", "DO $$ BEGIN EXECUTE format('GRANT SELECT ON t TO %I', nexus_role('scheduler')); END $$;\n"+
		"-- the nexus_scheduler_login is a login, not a group role\n"+
		"SELECT 'nexus_scheduler_login';\n")
	fs, err := LiteralGroupRoles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 {
		t.Fatalf("findings %v; want the GRANT and the dynamic REVOKE in 0039 only", fs)
	}
	for _, f := range fs {
		if f.File != "0039_bad.up.sql" || f.Rule != "literal-group-role" || f.Severity != Blocking {
			t.Errorf("finding %v", f)
		}
	}
}
