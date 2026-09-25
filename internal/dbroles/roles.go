// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package dbroles creates and maintains the LOGIN roles the Nexus services
// connect as, so no service runs as the database owner.
//
// Migration 0005 created one NOLOGIN group role per service (nexus_api_gateway,
// nexus_scheduler, ...) carrying ADR-0011's write grants, and said "a
// deployment grants one to the login user it creates per service". This
// package is that deployment step. It runs as the owner/admin login (the one
// the migrate Job uses, which created the group roles and so holds ADMIN
// OPTION on them) and, for each configured login:
//
//   - creates it (LOGIN, and nothing else: no SUPERUSER, CREATEDB, CREATEROLE,
//     REPLICATION or BYPASSRLS), or refuses to adopt an existing role that has
//     any of those;
//   - sets its password from a client-side SCRAM-SHA-256 verifier, so the
//     plaintext never reaches the server or its logs;
//   - makes it a member of exactly the configured nexus_* group roles, revoking
//     any other nexus_* membership (a login moved between services does not keep
//     its old rights);
//   - or, for a read-only login (artifact-gc), grants SELECT on every table in
//     the schema now and, through default privileges, on every table the owner
//     creates later.
//
// Everything happens in one transaction: a failure leaves every login as it
// was. Re-running is safe and is how passwords are rotated.
//
// Managed servers (Azure Flexible Server, RDS, Cloud SQL) cannot be reached by
// Terraform, which runs outside their private network; the Helm chart runs this
// as a Job inside the cluster instead (deploy/helm, OPERATIONS §20.9).
package dbroles

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Login is one runtime login to maintain.
type Login struct {
	// Name of the LOGIN role, e.g. nexus_api_gateway_login.
	Name string `json:"login"`
	// MemberOf lists the nexus_* NOLOGIN group roles the login inherits.
	MemberOf []string `json:"member_of,omitempty"`
	// ReadOnly grants SELECT on every table instead of group membership.
	ReadOnly bool `json:"read_only,omitempty"`
	// PasswordEnv names the environment variable holding the password. The spec
	// itself never carries a secret, so it can be rendered into a manifest.
	PasswordEnv string `json:"password_env"`
	// ConnectionLimit caps concurrent sessions for the login; nil or -1 = none.
	ConnectionLimit *int `json:"connection_limit,omitempty"`
}

var (
	loginRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	groupRe = regexp.MustCompile(`^nexus_[a-z_]+$`)
	envRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ParseSpec decodes and validates a JSON array of logins.
func ParseSpec(raw []byte) ([]Login, error) {
	var logins []Login
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&logins); err != nil {
		return nil, fmt.Errorf("decode login spec: %w", err)
	}
	if len(logins) == 0 {
		return nil, fmt.Errorf("login spec is empty")
	}
	seen := map[string]bool{}
	for i, l := range logins {
		if !loginRe.MatchString(l.Name) {
			return nil, fmt.Errorf("login %d: name %q must match %s", i, l.Name, loginRe)
		}
		if groupRe.MatchString(l.Name) && !strings.HasSuffix(l.Name, "_login") {
			// nexus_<service> is the group role's own name space. A login named
			// like a group role is how "grant the login" turns into "give the
			// group role a password".
			return nil, fmt.Errorf("login %q: names of the form nexus_* are reserved for group roles unless they end in _login", l.Name)
		}
		if seen[l.Name] {
			return nil, fmt.Errorf("login %q is listed twice", l.Name)
		}
		seen[l.Name] = true
		if l.ReadOnly == (len(l.MemberOf) > 0) {
			return nil, fmt.Errorf("login %q: set exactly one of member_of or read_only", l.Name)
		}
		for _, g := range l.MemberOf {
			if !groupRe.MatchString(g) {
				return nil, fmt.Errorf("login %q: group %q is not a nexus_* group role", l.Name, g)
			}
		}
		if !envRe.MatchString(l.PasswordEnv) {
			return nil, fmt.Errorf("login %q: password_env %q is not an environment variable name", l.Name, l.PasswordEnv)
		}
		if l.ConnectionLimit != nil && *l.ConnectionLimit < -1 {
			return nil, fmt.Errorf("login %q: connection_limit must be -1 (none) or more", l.Name)
		}
	}
	return logins, nil
}

// Result reports what Apply did for one login. It never contains a secret.
type Result struct {
	Login    string
	Created  bool
	Granted  []string
	Revoked  []string
	ReadOnly bool
}

// Apply converges every login in one transaction on conn. password returns the
// value of an environment variable (os.Getenv in production).
func Apply(ctx context.Context, conn *pgx.Conn, logins []Login, password func(string) string) ([]Result, error) {
	// Verify and hash every password before touching the database, so a missing
	// Secret key fails the Job without a half-applied transaction to roll back.
	verifiers := make(map[string]string, len(logins))
	for _, l := range logins {
		p := password(l.PasswordEnv)
		if p == "" {
			return nil, fmt.Errorf("login %q: %s is empty (is the Secret key present?)", l.Name, l.PasswordEnv)
		}
		v, err := NewVerifier(p)
		if err != nil {
			return nil, fmt.Errorf("login %q: %w", l.Name, err)
		}
		verifiers[l.Name] = v
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var self string
	if err := tx.QueryRow(ctx, `SELECT current_user`).Scan(&self); err != nil {
		return nil, fmt.Errorf("current_user: %w", err)
	}

	results := make([]Result, 0, len(logins))
	for _, l := range logins {
		if l.Name == self {
			return nil, fmt.Errorf("login %q is the owner login this job runs as; a runtime login must be a different role", l.Name)
		}
		r, err := applyOne(ctx, tx, l, verifiers[l.Name])
		if err != nil {
			return nil, fmt.Errorf("login %q: %w", l.Name, err)
		}
		results = append(results, r)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return results, nil
}

type roleAttrs struct {
	exists, canLogin, super, createRole, createDB, replication, bypassRLS bool
}

func readRole(ctx context.Context, tx pgx.Tx, name string) (roleAttrs, error) {
	var a roleAttrs
	err := tx.QueryRow(ctx, `
		SELECT rolcanlogin, rolsuper, rolcreaterole, rolcreatedb, rolreplication, rolbypassrls
		  FROM pg_roles WHERE rolname = $1`, name).
		Scan(&a.canLogin, &a.super, &a.createRole, &a.createDB, &a.replication, &a.bypassRLS)
	if err == pgx.ErrNoRows {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	a.exists = true
	return a, nil
}

func applyOne(ctx context.Context, tx pgx.Tx, l Login, verifier string) (Result, error) {
	res := Result{Login: l.Name, ReadOnly: l.ReadOnly}
	ident := pgx.Identifier{l.Name}.Sanitize()

	for _, g := range l.MemberOf {
		a, err := readRole(ctx, tx, g)
		if err != nil {
			return res, fmt.Errorf("read group role %s: %w", g, err)
		}
		if !a.exists {
			return res, fmt.Errorf("group role %s does not exist: run the migrations first (migration 0005 creates it)", g)
		}
		if a.canLogin {
			return res, fmt.Errorf("group role %s can log in; refusing to grant it (group roles are NOLOGIN)", g)
		}
	}

	a, err := readRole(ctx, tx, l.Name)
	if err != nil {
		return res, fmt.Errorf("read role: %w", err)
	}
	if a.exists {
		if a.super || a.createRole || a.createDB || a.replication || a.bypassRLS {
			return res, fmt.Errorf("role exists with SUPERUSER, CREATEROLE, CREATEDB, REPLICATION or BYPASSRLS; refusing to use it as a least-privilege runtime login")
		}
		if !a.canLogin && groupRe.MatchString(l.Name) {
			return res, fmt.Errorf("role exists as a NOLOGIN group role; refusing to turn it into a login")
		}
	}

	limit := -1
	if l.ConnectionLimit != nil {
		limit = *l.ConnectionLimit
	}
	// The verifier's alphabet is base64 plus "$:-" and digits, so it cannot
	// contain a quote; quoteLiteral is defence in depth.
	if !a.exists {
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`CREATE ROLE %s WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS INHERIT CONNECTION LIMIT %d PASSWORD %s`,
			ident, limit, quoteLiteral(verifier))); err != nil {
			return res, fmt.Errorf("create role: %w", err)
		}
		res.Created = true
	} else {
		// Only LOGIN, INHERIT, the limit and the password: a CREATEROLE login
		// that is not a superuser may not name the other attributes at all on
		// some managed servers, and they were checked above instead.
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`ALTER ROLE %s WITH LOGIN INHERIT CONNECTION LIMIT %d PASSWORD %s`,
			ident, limit, quoteLiteral(verifier))); err != nil {
			return res, fmt.Errorf("alter role: %w", err)
		}
	}

	// Memberships: exactly the configured nexus_* groups.
	rows, err := tx.Query(ctx, `
		SELECT g.rolname
		  FROM pg_auth_members m
		  JOIN pg_roles g ON g.oid = m.roleid
		  JOIN pg_roles u ON u.oid = m.member
		 WHERE u.rolname = $1`, l.Name)
	if err != nil {
		return res, fmt.Errorf("read memberships: %w", err)
	}
	var current []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			rows.Close()
			return res, err
		}
		current = append(current, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for _, g := range l.MemberOf {
		if slices.Contains(current, g) {
			continue
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`GRANT %s TO %s`, pgx.Identifier{g}.Sanitize(), ident)); err != nil {
			return res, fmt.Errorf("grant %s: %w", g, err)
		}
		res.Granted = append(res.Granted, g)
	}
	for _, g := range current {
		// Only nexus_* groups are ours to revoke: a provider's own roles
		// (rds_iam, azure_pg_admin, cloudsqliamuser) are left alone.
		if !groupRe.MatchString(g) || slices.Contains(l.MemberOf, g) {
			continue
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`REVOKE %s FROM %s`, pgx.Identifier{g}.Sanitize(), ident)); err != nil {
			return res, fmt.Errorf("revoke %s: %w", g, err)
		}
		res.Revoked = append(res.Revoked, g)
	}

	if l.ReadOnly {
		var db string
		if err := tx.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
			return res, err
		}
		for _, stmt := range []string{
			`GRANT CONNECT ON DATABASE %[2]s TO %[1]s`,
			`GRANT USAGE ON SCHEMA public TO %[1]s`,
			`GRANT SELECT ON ALL TABLES IN SCHEMA public TO %[1]s`,
			// Tables the owner creates in later migrations become readable too,
			// so a new release never breaks the read-only login.
			`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO %[1]s`,
		} {
			if _, err := tx.Exec(ctx, fmt.Sprintf(stmt, ident, pgx.Identifier{db}.Sanitize())); err != nil {
				return res, fmt.Errorf("read-only grants: %w", err)
			}
		}
	}
	return res, nil
}

func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
