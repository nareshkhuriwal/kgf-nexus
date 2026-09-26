//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package dbroles

// F153: per-installation group-role names (migration 0038) and the
// db-roles options that go with them. The prefix is applied inside a
// transaction that is ROLLED BACK: CREATE ROLE, GRANT and REVOKE are all
// transactional in PostgreSQL, so the test proves the move on the shared test
// database without leaving a role or a changed grant behind for the other
// suites (or other databases on the same server).

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var services = []string{"api_gateway", "workflow_manager", "dag_engine", "scheduler", "resource_manager", "event_service"}

// privileges lists every privilege role holds in the current database, as
// "object|privilege" strings: relations, columns and routines in schema
// public, the schema itself and the database.
func privileges(t *testing.T, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, role string) []string {
	t.Helper()
	rows, err := q.Query(context.Background(), `
WITH r AS (SELECT oid FROM pg_roles WHERE rolname = $1)
SELECT 'rel:' || c.oid::regclass::text || '|' || a.privilege_type
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace, LATERAL aclexplode(c.relacl) a, r
 WHERE n.nspname = 'public' AND a.grantee = r.oid
UNION ALL
SELECT 'col:' || c.oid::regclass::text || '.' || att.attname || '|' || a.privilege_type
  FROM pg_attribute att JOIN pg_class c ON c.oid = att.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace,
       LATERAL aclexplode(att.attacl) a, r
 WHERE n.nspname = 'public' AND NOT att.attisdropped AND a.grantee = r.oid
UNION ALL
SELECT 'fn:' || p.oid::regprocedure::text || '|' || a.privilege_type
  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace, LATERAL aclexplode(p.proacl) a, r
 WHERE n.nspname = 'public' AND a.grantee = r.oid
UNION ALL
SELECT 'schema:public|' || a.privilege_type
  FROM pg_namespace n, LATERAL aclexplode(n.nspacl) a, r WHERE n.nspname = 'public' AND a.grantee = r.oid
UNION ALL
SELECT 'db|' || a.privilege_type
  FROM pg_database d, LATERAL aclexplode(d.datacl) a, r WHERE d.datname = current_database() AND a.grantee = r.oid`, role)
	if err != nil {
		t.Fatalf("privileges of %s: %v", role, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func scalar[T any](t *testing.T, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := q.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func TestRolePrefixDefaultIsTheSharedNames(t *testing.T) {
	owner := ownerConn(t)
	if p := scalar[string](t, owner, `SELECT nexus_role_prefix()`); p != "nexus" {
		t.Fatalf("default prefix %q; want nexus (existing installations unchanged)", p)
	}
	for _, s := range services {
		if got := scalar[string](t, owner, `SELECT nexus_role($1)`, s); got != "nexus_"+s {
			t.Errorf("nexus_role(%s) = %s", s, got)
		}
	}
	if _, err := owner.Exec(context.Background(), `SELECT nexus_role('not_a_service')`); err == nil {
		t.Error("nexus_role accepted an unknown service")
	}
	// Ensuring the default is a no-op that moves nothing.
	before := privileges(t, owner, "nexus_scheduler")
	if p := scalar[string](t, owner, `SELECT nexus_ensure_role_prefix('')`); p != "nexus" {
		t.Fatalf("ensure('') = %q", p)
	}
	if after := privileges(t, owner, "nexus_scheduler"); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatal("ensuring the default prefix changed nexus_scheduler's privileges")
	}
}

func TestApplyingARolePrefixMovesEveryPrivilege(t *testing.T) {
	ctx := context.Background()
	owner := ownerConn(t)
	prefix := "w4ct" + strings.ReplaceAll(uuid.NewString()[:6], "-", "")

	before := map[string][]string{}
	for _, s := range services {
		before[s] = privileges(t, owner, "nexus_"+s)
		if len(before[s]) == 0 {
			t.Fatalf("nexus_%s holds no privileges before the move; the test would be vacuous", s)
		}
	}

	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if got := scalar[string](t, tx, `SELECT nexus_ensure_role_prefix($1)`, prefix); got != prefix {
		t.Fatalf("ensure(%s) = %s", prefix, got)
	}
	for _, s := range services {
		mine := prefix + "_" + s
		if got := privileges(t, tx, mine); strings.Join(got, "\n") != strings.Join(before[s], "\n") {
			t.Errorf("%s holds\n  %v\nwant exactly nexus_%s's former\n  %v", mine, got, s, before[s])
		}
		if left := privileges(t, tx, "nexus_"+s); len(left) != 0 {
			t.Errorf("nexus_%s still holds %v in this database: another installation's logins would keep them", s, left)
		}
		if got := scalar[string](t, tx, `SELECT nexus_role($1)`, s); got != mine {
			t.Errorf("nexus_role(%s) = %s; want %s", s, got, mine)
		}
		if canLogin := scalar[bool](t, tx, `SELECT rolcanlogin FROM pg_roles WHERE rolname = $1`, mine); canLogin {
			t.Errorf("%s can log in; group roles are NOLOGIN", mine)
		}
	}
	if ok := scalar[bool](t, tx, `SELECT has_table_privilege($1, 'task_attempts', 'UPDATE')`, prefix+"_scheduler"); !ok {
		t.Error("the prefixed scheduler role cannot UPDATE its own table")
	}
	if ok := scalar[bool](t, tx, `SELECT has_table_privilege('nexus_scheduler', 'task_attempts', 'UPDATE')`); ok {
		t.Error("the SHARED nexus_scheduler still writes this installation's task_attempts")
	}

	// A migration written before 0038 grants to the shared name; the sync that
	// migrate.sh runs after every migration moves it.
	if _, err := tx.Exec(ctx, `GRANT UPDATE (priority) ON task_attempts TO nexus_scheduler`); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, tx, `SELECT nexus_ensure_role_prefix('')`); got != prefix {
		t.Fatalf("sync returned %q", got)
	}
	if left := privileges(t, tx, "nexus_scheduler"); len(left) != 0 {
		t.Errorf("the sync left %v on nexus_scheduler", left)
	}
	if ok := scalar[bool](t, tx, `SELECT has_column_privilege($1, 'task_attempts', 'priority', 'UPDATE')`, prefix+"_scheduler"); !ok {
		t.Error("the sync did not carry the late grant to the prefixed role")
	}

	// A prefix is fixed once applied: another, or the shared default, is refused.
	for _, other := range []string{prefix + "x", "nexus"} {
		if _, err := tx.Exec(ctx, `SAVEPOINT s`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT nexus_ensure_role_prefix($1)`, other); err == nil {
			t.Errorf("changing the prefix from %s to %s was accepted", prefix, other)
		}
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT s`); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{"Upper", "1digit", "has-dash", strings.Repeat("a", 41), "x;drop"} {
		if _, err := tx.Exec(ctx, `SAVEPOINT s`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT nexus_ensure_role_prefix($1)`, bad); err == nil {
			t.Errorf("prefix %q accepted", bad)
		}
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT s`); err != nil {
			t.Fatal(err)
		}
	}

	// The down direction (0038's rollback) moves everything back.
	if _, err := tx.Exec(ctx, `SELECT nexus_unapply_role_prefix()`); err != nil {
		t.Fatalf("unapply: %v", err)
	}
	for _, s := range services {
		got := privileges(t, tx, "nexus_"+s)
		want := before[s]
		if s == "scheduler" {
			want = append(append([]string(nil), want...), "col:task_attempts.priority|UPDATE")
			sort.Strings(want)
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("after unapply nexus_%s holds\n  %v\nwant\n  %v", s, got, want)
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// Nothing survived the rolled-back transaction.
	if n := scalar[int](t, owner, `SELECT count(*)::int FROM pg_roles WHERE rolname LIKE $1`, prefix+"%"); n != 0 {
		t.Errorf("%d %s_* roles survived the rollback", n, prefix)
	}
	for _, s := range services {
		if got := privileges(t, owner, "nexus_"+s); strings.Join(got, "\n") != strings.Join(before[s], "\n") {
			t.Errorf("nexus_%s's privileges changed outside the transaction", s)
		}
	}
}

// The mutating functions are the owner's: a service role cannot move its own
// installation's grants around.
func TestRolePrefixFunctionsAreNotPublic(t *testing.T) {
	owner := ownerConn(t)
	for _, fn := range []string{"nexus_ensure_role_prefix(text)", "nexus_unapply_role_prefix()", "nexus_move_role_privileges(text,text)"} {
		if ok := scalar[bool](t, owner, `SELECT has_function_privilege('nexus_scheduler', $1, 'EXECUTE')`, fn); ok {
			t.Errorf("nexus_scheduler may EXECUTE %s", fn)
		}
	}
}

func TestParseSpecForAPrefixedInstallation(t *testing.T) {
	spec := []byte(`[{"login":"acme_scheduler_login","member_of":["acme_scheduler"],"password_env":"P"}]`)
	if _, err := ParseSpecWithOptions(spec, Options{GroupPrefix: "acme"}); err != nil {
		t.Fatalf("prefixed spec refused: %v", err)
	}
	if _, err := ParseSpecWithOptions(spec, Options{}); err == nil {
		t.Fatal("a non-nexus group accepted with the default prefix")
	}
	shared := []byte(`[{"login":"acme_scheduler_login","member_of":["nexus_scheduler"],"password_env":"P"}]`)
	if _, err := ParseSpecWithOptions(shared, Options{GroupPrefix: "acme"}); err == nil {
		t.Fatal("a SHARED group role accepted for a prefixed installation")
	}
	if _, err := ParseSpecWithOptions(spec, Options{GroupPrefix: "Bad-Prefix"}); err == nil {
		t.Fatal("a malformed prefix accepted")
	}
}

// Apply refuses a group prefix the database was not migrated with: granting
// acme_* logins on a database whose grants sit on nexus_* would create logins
// with no privileges at all (or, the other way round, on the shared roles).
func TestApplyRefusesAPrefixTheDatabaseDoesNotUse(t *testing.T) {
	owner := ownerConn(t)
	login := "t_pfx_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "") + "_login"
	t.Cleanup(func() { dropLogin(t, owner, login) })
	_, err := ApplyWithOptions(context.Background(), owner,
		[]Login{{Name: login, MemberOf: []string{"acme_scheduler"}, PasswordEnv: "P"}},
		func(string) string { return "Password0123456789abc" }, Options{GroupPrefix: "acme"})
	if err == nil || !strings.Contains(err.Error(), "nexus") {
		t.Fatalf("apply with prefix acme on a nexus database: %v", err)
	}
}

// NEXUS_DB_REVOKE_PUBLIC_CONNECT: after Apply, only this installation's
// logins (granted CONNECT explicitly) and the owner reach the database; a
// login of another installation on the same server does not.
func TestApplyRevokesPublicConnect(t *testing.T) {
	ctx := context.Background()
	owner := ownerConn(t)
	db := scalar[string](t, owner, `SELECT current_database()`)
	dbIdent := pgx.Identifier{db}.Sanitize()
	suffix := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	mine, other := "t_rc_"+suffix+"_login", "t_rc_other_"+suffix+"_login"
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), `GRANT CONNECT ON DATABASE `+dbIdent+` TO PUBLIC`)
		dropLogin(t, owner, mine)
		dropLogin(t, owner, other)
	})
	pw := "Revoke" + suffix + "0123456789"
	if _, err := owner.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, pgx.Identifier{other}.Sanitize(), pw)); err != nil {
		t.Fatal(err)
	}
	// Before: PUBLIC's default CONNECT lets the foreign login in (F153).
	if c, err := connectAs(t, other, pw); err != nil {
		t.Fatalf("precondition: the foreign login cannot connect even before the revoke: %v", err)
	} else {
		c.Close(ctx)
	}

	res, err := ApplyWithOptions(ctx, owner, []Login{{Name: mine, MemberOf: []string{"nexus_scheduler"}, PasswordEnv: "P"}},
		func(string) string { return pw }, Options{RevokePublicConnect: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Created {
		t.Fatalf("result %+v", res)
	}
	if public := scalar[bool](t, owner, `SELECT EXISTS (SELECT 1 FROM pg_database d, aclexplode(d.datacl) a
		WHERE d.datname = current_database() AND a.grantee = 0 AND a.privilege_type = 'CONNECT')`); public {
		t.Fatal("PUBLIC still holds CONNECT")
	}
	if c, err := connectAs(t, mine, pw); err != nil {
		t.Fatalf("this installation's login lost access: %v", err)
	} else {
		c.Close(ctx)
	}
	if c, err := connectAs(t, other, pw); err == nil {
		c.Close(ctx)
		t.Fatal("a login of another installation still connects")
	} else if sqlState(err) != "42501" {
		t.Fatalf("foreign login refused for the wrong reason: %v", err)
	}
	// Idempotent: a second run changes nothing and still succeeds.
	if _, err := ApplyWithOptions(ctx, owner, []Login{{Name: mine, MemberOf: []string{"nexus_scheduler"}, PasswordEnv: "P"}},
		func(string) string { return pw }, Options{RevokePublicConnect: true}); err != nil {
		t.Fatalf("second apply: %v", err)
	}
}
