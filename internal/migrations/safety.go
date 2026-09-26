// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package migrations analyses the SQL migration set for the properties ADR-0011
// rule 5 requires, so a migration that would break a rolling deploy fails a test
// instead of an upgrade.
//
// WHY THIS EXISTS. Rule 5 states that services stay compatible across one schema
// version so a rolling deploy can run mixed versions, and that migrations are
// "additive only; a column is dropped a release AFTER the last reader is gone".
// Nothing enforced it. migrate.sh applies whatever SQL it finds, in order, in a
// transaction -- it has no opinion about whether the statements are safe to run
// while the previous version of the fleet is still serving.
//
// The concrete precedent is 0002. It adds a column and then backfills it with a
// full-table UPDATE on workflow_runs, the hottest table in the system. On
// Postgres 11+ the ADD COLUMN itself is metadata-only (verified: the relfilenode
// does not change), so the DDL is cheap -- but it still takes ACCESS EXCLUSIVE,
// and the backfill takes row locks across the whole table inside the same
// transaction. On a busy control plane that is a stall, and the reason it went
// unnoticed is that on an empty development database it is instant.
//
// These checks are deliberately syntactic. A full SQL parser would be more
// precise and would also be a dependency and a maintenance burden for a set of
// rules that are about SHAPE: "does this migration contain a statement whose
// blast radius is the whole table". Syntactic checks over a small, reviewed
// migration set are the right amount of machinery, and every finding names the
// file and line so the reviewer judges rather than the tool deciding.
package migrations

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Severity separates "this will break a rolling deploy" from "a reviewer should
// look at this".
type Severity string

const (
	// Blocking means a mixed-version fleet cannot survive this migration: an old
	// replica still reading or writing the affected shape will fail.
	Blocking Severity = "blocking"
	// Review means the statement is legitimate but its cost or blast radius scales
	// with table size, so it needs a deliberate decision rather than a default.
	Review Severity = "review"
)

// Finding is one flagged statement.
type Finding struct {
	File     string
	Line     int
	Severity Severity
	Rule     string
	Stmt     string
	// Why states the failure, not the pattern. A finding an author cannot act on
	// gets suppressed rather than fixed.
	Why string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d [%s/%s] %s\n    %s", f.File, f.Line, f.Severity, f.Rule, f.Why, f.Stmt)
}

// rule is one syntactic pattern and the reason it matters.
type rule struct {
	name     string
	severity Severity
	re       *regexp.Regexp
	// extra narrows a match that a regex alone cannot express. Go's regexp is RE2,
	// which has no negative lookahead, and two of these rules are inherently
	// "matches X but not Y" -- so the exclusion lives in code rather than in an
	// unreadable pattern.
	extra func(stmt string) bool
	// exempt waives a match using what the rest of the SAME migration does: a
	// statement on a table the migration created, or an object dropped and
	// re-created under the same name (the idempotent re-apply idiom). Nil means
	// no exemption.
	exempt func(stmt string, fc fileContext) bool
	why    string
}

// fileContext is what one migration file creates, collected before any rule
// runs because a CREATE may precede or follow the statement it excuses.
type fileContext struct {
	// fresh holds tables this migration creates: empty, so a lock or scan on
	// them stalls nothing, and no older replica can depend on their shape.
	fresh map[string]bool
	// created holds "kind:name" (and "trigger:name@table") of indexes,
	// triggers, functions, procedures, views and types this migration creates.
	created map[string]bool
}

// rules are ordered most-severe first so a statement matching several is reported
// under the one that actually blocks a deploy.
var rules = []rule{
	{
		name:     "drop-column",
		severity: Blocking,
		re:       regexp.MustCompile(`(?i)\bALTER\s+TABLE\b[\s\S]*?\bDROP\s+COLUMN\b`),
		why: "an old replica still SELECTs this column and will fail mid-deploy; " +
			"drop it a release after the last reader is gone (ADR-0011 rule 5)",
	},
	{
		name:     "drop-table",
		severity: Blocking,
		re:       regexp.MustCompile(`(?i)\bDROP\s+TABLE\b`),
		why:      "an old replica still reads this table and will fail mid-deploy",
	},
	{
		name:     "truncate",
		severity: Blocking,
		re:       regexp.MustCompile(`(?i)\bTRUNCATE\b`),
		why: "deletes every row, with no WHERE and no undo short of a restore, under an " +
			"ACCESS EXCLUSIVE lock; a migration must not destroy data an old replica still serves",
	},
	{
		name:     "drop-object",
		severity: Blocking,
		re:       regexp.MustCompile(`(?i)\bDROP\s+(?:TYPE|DOMAIN|FUNCTION|PROCEDURE|VIEW|MATERIALIZED\s+VIEW|SEQUENCE|SCHEMA)\b`),
		exempt:   droppedObjectIsRecreated,
		why: "an old replica, a column default or a trigger still uses this object and " +
			"fails once it is gone; drop it a release after the last user",
	},
	{
		name:     "drop-trigger",
		severity: Blocking,
		re:       regexp.MustCompile(`(?i)\bDROP\s+TRIGGER\b`),
		exempt:   droppedTriggerIsRecreated,
		why: "triggers carry invariants old replicas rely on -- the 0014 tenant_assignment " +
			"triggers are what let a replica that predates tenant_id keep writing; " +
			"re-create it in the same migration or remove it a release later",
	},
	{
		name:     "drop-constraint",
		severity: Blocking,
		re:       regexp.MustCompile(`(?i)\bDROP\s+CONSTRAINT\b`),
		exempt:   alterTargetIsFresh,
		why: "an old replica's ON CONFLICT (or ON CONSTRAINT) may name this unique or " +
			"primary key and fails without it, and its correctness may rest on a CHECK or " +
			"FOREIGN KEY; drop it a release after the last reader",
	},
	{
		name:     "drop-index",
		severity: Blocking,
		re:       regexp.MustCompile(`(?i)\bDROP\s+INDEX\b`),
		exempt:   droppedIndexIsRecreated,
		why: "a unique index can be what an old replica's ON CONFLICT relies on, a query " +
			"plan may depend on any index, and without CONCURRENTLY the drop holds " +
			"ACCESS EXCLUSIVE on the table; drop it a release after the last user",
	},
	{
		name:     "rename",
		severity: Blocking,
		// Tables, columns and constraints, and every other named object an old
		// replica can reference: an index (ON CONFLICT ON CONSTRAINT), an enum
		// value (RENAME VALUE), a type, a sequence, a function or a view.
		re: regexp.MustCompile(`(?i)\bALTER\s+(?:TABLE|INDEX|TYPE|DOMAIN|SEQUENCE|VIEW|MATERIALIZED\s+VIEW|FUNCTION|PROCEDURE|SCHEMA)\b[\s\S]*?\bRENAME\b`),
		why: "a rename is a drop and an add at once, so neither the old nor the new " +
			"replica can see the name the other uses; add the new name, migrate readers, remove the old",
	},
	{
		name:     "alter-column-type",
		severity: Blocking,
		re:       regexp.MustCompile(`(?i)\bALTER\s+TABLE\b[\s\S]*?\bALTER\s+(?:COLUMN\s+)?(?:"[^"]+"|\w+)\s+(?:SET\s+DATA\s+)?TYPE\b`),
		why: "changing a column's type rewrites the table under an ACCESS EXCLUSIVE lock " +
			"and can change what an old replica reads back; add a new column instead",
	},
	{
		name:     "set-not-null",
		severity: Blocking,
		re:       regexp.MustCompile(`(?i)\bALTER\s+TABLE\b[\s\S]*?\bSET\s+NOT\s+NULL\b`),
		why: "an old replica that INSERTs without this column will start failing; " +
			"add a DEFAULT and enforce NOT NULL a release later",
	},
	{
		name:     "add-column-not-null-no-default",
		severity: Blocking,
		re:       regexp.MustCompile(`(?i)\bADD\s+COLUMN\b[\s\S]*?\bNOT\s+NULL\b`),
		// A DEFAULT makes the same statement safe and is the additive shape rule 5
		// prescribes, so only the defaultless form is a hazard.
		extra: func(stmt string) bool { return !hasDefault.MatchString(stmt) },
		why: "NOT NULL with no DEFAULT breaks every INSERT from an old replica that does " +
			"not know the column, and forces a table rewrite; add a DEFAULT and enforce " +
			"NOT NULL a release later",
	},
	{
		name:     "drop-default",
		severity: Blocking,
		re:       regexp.MustCompile(`(?i)\bALTER\b[\s\S]*?\bDROP\s+DEFAULT\b`),
		why: "an old replica that INSERTs without this column relied on the default; on a " +
			"NOT NULL column every such INSERT now fails, and on a nullable one it silently " +
			"stores NULL instead",
	},
	{
		name:     "unbounded-update",
		severity: Review,
		re:       regexp.MustCompile(`(?i)^\s*UPDATE\s+\w+`),
		extra:    func(stmt string) bool { return !boundedUpdate(stmt) },
		why: "a backfill with no WHERE bound takes row locks across the whole table " +
			"inside the migration's transaction; on a large table that is a stall, " +
			"and it is invisible on an empty dev database (this is migration 0002's real cost)",
	},
	{
		name:     "unbounded-delete",
		severity: Review,
		re:       regexp.MustCompile(`(?i)^\s*DELETE\s+FROM\b`),
		extra:    func(stmt string) bool { return !boundedUpdate(stmt) },
		exempt:   func(stmt string, fc fileContext) bool { return targetIsFresh(deleteTargetRe, stmt, fc) },
		why: "a DELETE with no top-level WHERE removes every row and locks them all inside " +
			"the migration's transaction; bound it, or say why the whole table goes",
	},
	{
		name:     "validating-constraint",
		severity: Review,
		re:       regexp.MustCompile(`(?i)\bADD\s+(?:CONSTRAINT\s+(?:"[^"]+"|\w+)\s+)?(?:FOREIGN\s+KEY|CHECK|UNIQUE|PRIMARY\s+KEY|EXCLUDE)\b`),
		// NOT VALID defers the scan (VALIDATE CONSTRAINT later takes only SHARE
		// UPDATE EXCLUSIVE); USING INDEX adopts an index built CONCURRENTLY.
		extra:  func(stmt string) bool { return !notValidOrUsingIndex.MatchString(stmt) },
		exempt: alterTargetIsFresh,
		why: "adding a constraint validates every existing row under a lock that blocks " +
			"writes (a UNIQUE or PRIMARY KEY also builds its index there), and an old " +
			"replica's writes that violate it start failing; add CHECK/FOREIGN KEY NOT " +
			"VALID and VALIDATE later, or UNIQUE ... USING INDEX over a CONCURRENTLY-built index",
	},
	{
		name:     "create-index-blocking",
		severity: Review,
		re:       regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\b`),
		extra:    func(stmt string) bool { return !hasConcurrently.MatchString(stmt) },
		why: "CREATE INDEX without CONCURRENTLY holds a write lock for the whole build; " +
			"acceptable in the initial schema, a stall on a populated table",
	},
}

// Patterns for the exclusions the rules above delegate to `extra`.
var (
	hasDefault      = regexp.MustCompile(`(?i)\bDEFAULT\b`)
	hasConcurrently = regexp.MustCompile(`(?i)\bCONCURRENTLY\b`)
	// hasWhere is deliberately NOT a plain /WHERE/ search. 0002's backfill is
	// `UPDATE workflow_runs r SET event_seq = COALESCE((SELECT ... WHERE e.run_id
	// = r.id), 0);` -- a WHERE inside a subquery, which bounds the SUBQUERY and
	// leaves the outer UPDATE touching every row. Counting that as bounded would
	// have exempted the exact statement this package was written to catch.
	hasWhere = regexp.MustCompile(`(?i)\bWHERE\b`)

	notValidOrUsingIndex = regexp.MustCompile(`(?i)\bNOT\s+VALID\b|\bUSING\s+INDEX\b`)
)

var (
	createTableRe  = regexp.MustCompile(`(?is)\bCREATE\s+TABLE\b(?:\s+IF\s+NOT\s+EXISTS)?\s+"?(\w+)"?`)
	createIndexRe  = regexp.MustCompile(`(?is)\bCREATE\s+(?:UNIQUE\s+)?INDEX\b[\s\S]*?\bON\s+"?(\w+)"?`)
	updateTargetRe = regexp.MustCompile(`(?is)^\s*UPDATE\s+(?:ONLY\s+)?"?(\w+)"?`)
	deleteTargetRe = regexp.MustCompile(`(?is)^\s*DELETE\s+FROM\s+(?:ONLY\s+)?"?(\w+)"?`)
	alterTargetRe  = regexp.MustCompile(`(?is)\bALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?"?(\w+)"?`)

	createdIndexRe   = regexp.MustCompile(`(?is)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?"?(\w+)"?`)
	createdTriggerRe = regexp.MustCompile(`(?is)\bCREATE\s+(?:OR\s+REPLACE\s+)?(?:CONSTRAINT\s+)?TRIGGER\s+"?(\w+)"?[\s\S]*?\bON\s+(?:ONLY\s+)?"?(\w+)"?`)
	createdObjectRe  = regexp.MustCompile(`(?is)\bCREATE\s+(?:OR\s+REPLACE\s+)?(FUNCTION|PROCEDURE|VIEW|TYPE|DOMAIN)\s+(?:\w+\.)?"?(\w+)"?`)
	droppedIndexRe   = regexp.MustCompile(`(?is)\bDROP\s+INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+EXISTS\s+)?"?(\w+)"?`)
	droppedTriggerRe = regexp.MustCompile(`(?is)\bDROP\s+TRIGGER\s+(?:IF\s+EXISTS\s+)?"?(\w+)"?\s+ON\s+(?:ONLY\s+)?"?(\w+)"?`)
	droppedObjectRe  = regexp.MustCompile(`(?is)\bDROP\s+(FUNCTION|PROCEDURE|VIEW|TYPE|DOMAIN)\s+(?:IF\s+EXISTS\s+)?(?:\w+\.)?"?(\w+)"?`)
)

// newFileContext collects what a migration creates.
func newFileContext(stmts []statement) fileContext {
	fc := fileContext{fresh: tablesCreatedIn(stmts), created: map[string]bool{}}
	for _, st := range stmts {
		for _, m := range createdIndexRe.FindAllStringSubmatch(st.text, -1) {
			fc.created["index:"+strings.ToLower(m[1])] = true
		}
		for _, m := range createdTriggerRe.FindAllStringSubmatch(st.text, -1) {
			fc.created["trigger:"+strings.ToLower(m[1])+"@"+strings.ToLower(m[2])] = true
		}
		for _, m := range createdObjectRe.FindAllStringSubmatch(st.text, -1) {
			fc.created[strings.ToLower(m[1])+":"+strings.ToLower(m[2])] = true
		}
	}
	return fc
}

// targetIsFresh reports whether re's first capture names a table this
// migration created.
func targetIsFresh(re *regexp.Regexp, stmt string, fc fileContext) bool {
	m := re.FindStringSubmatch(stmt)
	return m != nil && fc.fresh[strings.ToLower(m[1])]
}

func alterTargetIsFresh(stmt string, fc fileContext) bool {
	return targetIsFresh(alterTargetRe, stmt, fc)
}

// droppedIndexIsRecreated: every index the statement drops is created again,
// under the same name, by this migration -- the "drop a leftover INVALID copy,
// then CREATE INDEX CONCURRENTLY" idiom of 0022 and 0027. A name built at run
// time (format('DROP INDEX %I', ...)) cannot be read and is not exempt.
func droppedIndexIsRecreated(stmt string, fc fileContext) bool {
	ms := droppedIndexRe.FindAllStringSubmatch(stmt, -1)
	if len(ms) == 0 {
		return false
	}
	for _, m := range ms {
		if !fc.created["index:"+strings.ToLower(m[1])] {
			return false
		}
	}
	return true
}

// droppedTriggerIsRecreated: DROP TRIGGER IF EXISTS x ON t followed by CREATE
// TRIGGER x ... ON t, the idempotent re-apply idiom of every tenant trigger.
func droppedTriggerIsRecreated(stmt string, fc fileContext) bool {
	ms := droppedTriggerRe.FindAllStringSubmatch(stmt, -1)
	if len(ms) == 0 {
		return false
	}
	for _, m := range ms {
		if !fc.created["trigger:"+strings.ToLower(m[1])+"@"+strings.ToLower(m[2])] {
			return false
		}
	}
	return true
}

// droppedObjectIsRecreated: a function, procedure, view, type or domain
// dropped and created again under the same name (a signature change).
func droppedObjectIsRecreated(stmt string, fc fileContext) bool {
	ms := droppedObjectRe.FindAllStringSubmatch(stmt, -1)
	if len(ms) == 0 {
		return false
	}
	for _, m := range ms {
		if !fc.created[strings.ToLower(m[1])+":"+strings.ToLower(m[2])] {
			return false
		}
	}
	return true
}

// tablesCreatedIn lists the tables a migration creates itself.
func tablesCreatedIn(stmts []statement) map[string]bool {
	out := map[string]bool{}
	for _, st := range stmts {
		if m := createTableRe.FindStringSubmatch(st.text); m != nil {
			out[strings.ToLower(m[1])] = true
		}
	}
	return out
}

// indexTargetIsFresh reports whether a CREATE INDEX targets a table this same
// migration created.
//
// Such an index builds on an empty table, so the lock it takes is uncontended and
// instantaneous -- the cost the rule warns about does not exist. Without this
// distinction every migration that introduces a table with indexes would need an
// allowlist entry, and an allowlist that has to grow for ordinary safe work is one
// people stop reading.
func indexTargetIsFresh(stmt string, fresh map[string]bool) bool {
	m := createIndexRe.FindStringSubmatch(stmt)
	if m == nil {
		return false
	}
	return fresh[strings.ToLower(m[1])]
}

// updateTargetIsFresh reports whether an UPDATE targets a table this same
// migration created.
//
// The same reasoning as indexTargetIsFresh, applied to the other rule that
// measures cost by how many rows already exist. A backfill of a table created
// a few statements earlier can only touch rows this migration just inserted:
// no other session can have written to it, so the row locks are uncontended
// and there is no stall to warn about.
//
// 0013 is exactly this shape -- it CREATEs tenants, populates it from the
// namespaces already in use, then sets owner_user_id on what it just inserted.
// Reporting that as an unbounded backfill of a populated table describes a
// migration nobody wrote.
func updateTargetIsFresh(stmt string, fresh map[string]bool) bool {
	m := updateTargetRe.FindStringSubmatch(stmt)
	if m == nil {
		return false
	}
	return fresh[strings.ToLower(m[1])]
}

// boundedUpdate reports whether an UPDATE restricts its write set at the TOP
// level, ignoring any WHERE that belongs to a subquery.
//
// Parenthesis depth is enough for this and a SQL parser is not required: a
// top-level WHERE is one that appears at depth zero.
func boundedUpdate(stmt string) bool {
	depth := 0
	upper := strings.ToUpper(stmt)
	for i := 0; i < len(upper); i++ {
		switch upper[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case 'W':
			if depth == 0 && strings.HasPrefix(upper[i:], "WHERE") && isBoundary(upper, i-1) &&
				isBoundary(upper, i+len("WHERE")) {
				return true
			}
		}
	}
	return false
}

// isBoundary reports whether the byte at i is not part of an identifier, so
// "WHERE" is matched as a word rather than as a substring of something else.
func isBoundary(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return true
	}
	c := s[i]
	return !(c == '_' || (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z'))
}

// statement is one SQL statement with the line it started on.
type statement struct {
	text string
	line int
}

// Analyse reports the findings for every *.up.sql in dir.
//
// Down-migrations are deliberately NOT analysed: a rollback is a break-glass
// operation an operator runs deliberately, with the fleet's version already
// known, so the mixed-version reasoning that motivates these rules does not apply.
func Analyse(dir string) ([]Finding, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		return nil, fmt.Errorf("list migrations in %s: %w", dir, err)
	}
	sort.Strings(files)

	var out []Finding
	for _, path := range files {
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, fmt.Errorf("read %s: %w", path, rerr)
		}
		name := filepath.Base(path)
		stmts := split(string(body))
		// Tables created by THIS migration are empty, so an index on one cannot
		// stall anything. Collected up front because a CREATE INDEX may precede or
		// follow the CREATE TABLE it belongs to.
		fc := newFileContext(stmts)
		fresh := fc.fresh
		for _, st := range stmts {
			for _, r := range rules {
				if !r.re.MatchString(st.text) {
					continue
				}
				if r.extra != nil && !r.extra(st.text) {
					continue
				}
				if r.name == "create-index-blocking" && indexTargetIsFresh(st.text, fresh) {
					continue
				}
				if r.name == "unbounded-update" && updateTargetIsFresh(st.text, fresh) {
					continue
				}
				if r.exempt != nil && r.exempt(st.text, fc) {
					continue
				}
				{
					out = append(out, Finding{
						File: name, Line: st.line, Severity: r.severity,
						Rule: r.name, Stmt: summarise(st.text), Why: r.why,
					})
					// First match only: rules are ordered by severity, so the
					// most serious reason is the one reported.
					break
				}
			}
		}
	}
	return out, nil
}

// split breaks SQL into statements, dropping comments so a rule never fires on
// prose. This matters here because these migrations are heavily commented and
// several comments quote the very statements the rules look for.
func split(src string) []statement {
	var out []statement
	var buf strings.Builder
	startLine, line := 1, 0
	// dollarQuote tracks $$ ... $$ bodies, which contain semicolons that do NOT
	// end a statement. 0005 is one long DO $$ block, so without this the whole
	// file would be misread as dozens of fragments.
	inDollar := false

	sc := bufio.NewScanner(strings.NewReader(src))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line++
		raw := sc.Text()
		if strings.Count(raw, "$$")%2 == 1 {
			inDollar = !inDollar
		}
		text := raw
		if !inDollar {
			if i := strings.Index(text, "--"); i >= 0 {
				text = text[:i]
			}
		}
		if strings.TrimSpace(text) == "" && buf.Len() == 0 {
			continue
		}
		if buf.Len() == 0 {
			startLine = line
		}
		buf.WriteString(text)
		buf.WriteString("\n")
		if !inDollar && strings.Contains(text, ";") {
			if s := strings.TrimSpace(buf.String()); s != "" {
				out = append(out, statement{text: s, line: startLine})
			}
			buf.Reset()
		}
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		out = append(out, statement{text: s, line: startLine})
	}
	return out
}

// summarise collapses a statement to one line so a failure message stays readable.
func summarise(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 140 {
		return s[:137] + "..."
	}
	return s
}

// Blocking filters to the findings that would break a rolling deploy.
func BlockingOnly(fs []Finding) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Severity == Blocking {
			out = append(out, f)
		}
	}
	return out
}
