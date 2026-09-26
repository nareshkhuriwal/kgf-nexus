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
// migration uses, and never will.
//
// 0019 was skipped: no file with that number has existed on any branch of the
// repository (services/migrations/README.md). It must stay empty. The appliers
// run every file whose basename is not in the ledger, in lexical order, so a
// 0019 added now would run AFTER 0020 and every later migration on every
// existing database and BEFORE them on a fresh one, and the two would not be
// guaranteed to agree.
var knownVersionGaps = map[int]bool{19: true}

// reservedVersions are numbers handed to parallel branches that have not all
// merged yet. Unlike a known gap, a reserved number may be present OR absent:
// the wave-3 branches were each given numbers (0028–0031 elsewhere, 0032 to the
// SSO branch), so any one branch on its own has a hole below its migration that
// is filled only when the integration branch merges the others.
//
// Not an escape hatch for a dropped migration: the numbers are fixed here, and
// the integration branch removes this map once every reserved migration has
// landed, after which a missing one fails again.
var reservedVersions = map[int]bool{28: true, 29: true, 30: true, 31: true}

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
		case !versions[v] && !knownVersionGaps[v] && !reservedVersions[v]:
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
