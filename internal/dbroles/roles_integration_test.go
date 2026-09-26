//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package dbroles

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
)

func ownerConn(t *testing.T) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), testdb.DSN(t))
	if err != nil {
		testdb.SkipUnreachable(t, err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// connectAs opens a connection to the test database as login/password,
// keeping host, port, database and TLS settings from the owner DSN.
func connectAs(t *testing.T, login, password string) (*pgx.Conn, error) {
	t.Helper()
	cfg, err := pgx.ParseConfig(testdb.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Password = login, password
	return pgx.ConnectConfig(context.Background(), cfg)
}

func dropLogin(t *testing.T, owner *pgx.Conn, name string) {
	t.Helper()
	ctx := context.Background()
	id := pgx.Identifier{name}.Sanitize()
	// Default privileges and grants must go before the role can.
	_, _ = owner.Exec(ctx, "ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE SELECT ON TABLES FROM "+id)
	_, _ = owner.Exec(ctx, "DROP OWNED BY "+id)
	if _, err := owner.Exec(ctx, "DROP ROLE IF EXISTS "+id); err != nil {
		t.Logf("cleanup: drop %s: %v", name, err)
	}
}

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// TestApplyCreatesLeastPrivilegeLogins is the end-to-end claim: the job creates
// a login that authenticates with the password (so the client-side verifier is
// right), inherits exactly its service's grants, cannot change the schema, and
// a read-only login can read and never write.
func TestApplyCreatesLeastPrivilegeLogins(t *testing.T) {
	ctx := context.Background()
	owner := ownerConn(t)
	suffix := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	sched := "t_sched_" + suffix + "_login"
	gc := "t_gc_" + suffix + "_login"
	t.Cleanup(func() { dropLogin(t, owner, sched); dropLogin(t, owner, gc) })

	pw := map[string]string{"PW_S": "Sched" + suffix + "0123456789abcdef", "PW_G": "Gc" + suffix + "0123456789abcdefgh"}
	one := 3
	logins := []Login{
		{Name: sched, MemberOf: []string{"nexus_scheduler"}, PasswordEnv: "PW_S", ConnectionLimit: &one},
		{Name: gc, ReadOnly: true, PasswordEnv: "PW_G"},
	}
	res, err := Apply(ctx, owner, logins, func(k string) string { return pw[k] })
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res[0].Created || len(res[0].Granted) != 1 || !res[1].Created {
		t.Fatalf("results %+v", res)
	}

	// The password stored as a verifier authenticates.
	s, err := connectAs(t, sched, pw["PW_S"])
	if err != nil {
		t.Fatalf("scheduler login cannot connect with its password: %v", err)
	}
	defer s.Close(ctx)

	var super, createRole, createDB bool
	var limit int
	if err := owner.QueryRow(ctx, `SELECT rolsuper, rolcreaterole, rolcreatedb, rolconnlimit FROM pg_roles WHERE rolname=$1`, sched).
		Scan(&super, &createRole, &createDB, &limit); err != nil {
		t.Fatal(err)
	}
	if super || createRole || createDB || limit != 3 {
		t.Fatalf("login attributes super=%v createrole=%v createdb=%v connlimit=%d", super, createRole, createDB, limit)
	}

	// Inherited grants: reads everything, writes its own table, not another's.
	check := func(c *pgx.Conn, table, priv string, want bool) {
		t.Helper()
		var ok bool
		if err := c.QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, $2)`, table, priv).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if ok != want {
			t.Errorf("%s: %s on %s = %v, want %v", c.Config().User, priv, table, ok, want)
		}
	}
	// CONNECT is granted on this database explicitly, not inherited from
	// PUBLIC, so revoking PUBLIC's CONNECT on a shared server (F153) leaves the
	// service's own logins working.
	var connect bool
	if err := owner.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database d, aclexplode(d.datacl) a
		 WHERE d.datname = current_database() AND a.grantee = $1::regrole AND a.privilege_type = 'CONNECT')`,
		sched).Scan(&connect); err != nil {
		t.Fatal(err)
	}
	if !connect {
		t.Errorf("%s holds CONNECT on the database only through PUBLIC", sched)
	}

	check(s, "task_attempts", "UPDATE", true)
	check(s, "api_keys", "SELECT", true)
	check(s, "api_keys", "INSERT", false)
	check(s, "workflow_specs", "DELETE", false)

	// No DDL: the runtime login cannot alter or drop what the owner created.
	if _, err := s.Exec(ctx, `CREATE TABLE t_should_fail_`+suffix+` (x int)`); sqlState(err) != "42501" {
		t.Errorf("CREATE TABLE as runtime login: err=%v, want insufficient_privilege", err)
	}
	if _, err := s.Exec(ctx, `ALTER TABLE task_attempts ADD COLUMN t_x int`); sqlState(err) != "42501" {
		t.Errorf("ALTER TABLE as runtime login: err=%v, want insufficient_privilege (must be owner)", err)
	}
	if _, err := s.Exec(ctx, `DELETE FROM run_events WHERE false`); sqlState(err) != "42501" {
		t.Errorf("DELETE run_events as scheduler login: err=%v, want insufficient_privilege (append-only)", err)
	}

	// Read-only login: reads, never writes, and sees tables created later.
	g, err := connectAs(t, gc, pw["PW_G"])
	if err != nil {
		t.Fatalf("read-only login cannot connect: %v", err)
	}
	defer g.Close(ctx)
	check(g, "task_attempts", "SELECT", true)
	check(g, "task_attempts", "UPDATE", false)
	check(g, "attempt_logs", "DELETE", false)
	later := "t_later_" + suffix
	if _, err := owner.Exec(ctx, `CREATE TABLE `+later+` (x int)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(context.Background(), `DROP TABLE IF EXISTS `+later) })
	check(g, later, "SELECT", true)
	check(g, later, "INSERT", false)
}

// TestApplyRotatesAndConverges re-runs the job with a new password and a
// different group: the old password stops working, the new one works, and the
// old membership is revoked rather than accumulated.
func TestApplyRotatesAndConverges(t *testing.T) {
	ctx := context.Background()
	owner := ownerConn(t)
	name := "t_rot_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "") + "_login"
	t.Cleanup(func() { dropLogin(t, owner, name) })

	first, second := "FirstPassword0123456789", "SecondPassword987654321"
	if _, err := Apply(ctx, owner, []Login{{Name: name, MemberOf: []string{"nexus_dag_engine"}, PasswordEnv: "P"}},
		func(string) string { return first }); err != nil {
		t.Fatal(err)
	}
	res, err := Apply(ctx, owner, []Login{{Name: name, MemberOf: []string{"nexus_workflow_manager"}, PasswordEnv: "P"}},
		func(string) string { return second })
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Created || fmt.Sprint(res[0].Granted) != "[nexus_workflow_manager]" || fmt.Sprint(res[0].Revoked) != "[nexus_dag_engine]" {
		t.Fatalf("second run %+v", res[0])
	}
	// A server on `trust` authentication (a developer's host PostgreSQL)
	// accepts any password, so a rejected old password proves nothing there.
	if c, err := connectAs(t, name, "not-the-password-"+name); err == nil {
		c.Close(ctx)
		t.Log("server does not check passwords (trust authentication); rotation of the verifier not asserted")
	} else if c, err := connectAs(t, name, first); err == nil {
		c.Close(ctx)
		t.Fatal("old password still authenticates after rotation")
	}
	c, err := connectAs(t, name, second)
	if err != nil {
		t.Fatalf("new password refused: %v", err)
	}
	c.Close(ctx)
}

// TestApplyRefusesPrivilegedOrForeignRoles: the job never adopts a role that
// could change the schema or roles, never gives a group role a password, and
// a missing Secret key aborts before any change.
func TestApplyRefusesPrivilegedOrForeignRoles(t *testing.T) {
	ctx := context.Background()
	owner := ownerConn(t)
	priv := "t_priv_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "") + "_login"
	if _, err := owner.Exec(ctx, "CREATE ROLE "+priv+" LOGIN CREATEROLE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dropLogin(t, owner, priv) })
	pw := func(string) string { return "Adequate0123456789pw" }

	if _, err := Apply(ctx, owner, []Login{{Name: priv, ReadOnly: true, PasswordEnv: "P"}}, pw); err == nil ||
		!strings.Contains(err.Error(), "refusing") {
		t.Errorf("adopted a CREATEROLE role: %v", err)
	}
	if _, err := Apply(ctx, owner, []Login{{Name: "nexus_scheduler_login", MemberOf: []string{"nexus_does_not_exist"}, PasswordEnv: "P"}}, pw); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Errorf("granted a missing group role: %v", err)
	}
	var self string
	_ = owner.QueryRow(ctx, "SELECT current_user").Scan(&self)
	if _, err := Apply(ctx, owner, []Login{{Name: self, ReadOnly: true, PasswordEnv: "P"}}, pw); err == nil {
		t.Error("re-passworded the owner login")
	}
	missing := "t_missing_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "") + "_login"
	if _, err := Apply(ctx, owner, []Login{{Name: missing, ReadOnly: true, PasswordEnv: "P"}}, func(string) string { return "" }); err == nil {
		t.Error("applied with an empty password")
	}
	var n int
	_ = owner.QueryRow(ctx, "SELECT count(*) FROM pg_roles WHERE rolname=$1", missing).Scan(&n)
	if n != 0 {
		dropLogin(t, owner, missing)
		t.Error("a failed run created the login anyway")
	}
}
