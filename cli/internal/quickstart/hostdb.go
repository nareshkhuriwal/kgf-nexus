// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package quickstart

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The stack's database lives on the HOST machine's PostgreSQL by default (the
// project rule; deploy/docker-compose.yml header). nexusctl creates one
// database per compose project, tags it, points the stack at it through
// NEXUS_HOST_DATABASE_URL, and drops it on `down --volumes`. The bundled
// postgres container is an explicit opt-in (--bundled-postgres or
// NEXUS_BUNDLED_POSTGRES=1), which layers bundledOverlay.

// bundledOverlay is the compose file, next to the base file, that enables the
// bundled postgres container.
const bundledOverlay = "docker-compose.bundled-postgres.yml"

// hostDBTag marks a database nexusctl created; only a tagged one is dropped.
const hostDBTag = "org.kgf-nexus.nexusctl"

// hostDBURLEnv is the compose variable naming the stack's database as
// containers reach it.
const hostDBURLEnv = "NEXUS_HOST_DATABASE_URL"

// defaultHostAdminURL is the host server nexusctl manages databases on.
const defaultHostAdminURL = "postgres://127.0.0.1:5432/postgres?sslmode=disable"

// KindHostDatabase is a failure to reach or prepare the host's PostgreSQL.
const KindHostDatabase Kind = "host_database"

// HostDB creates, checks and drops the stack's database on the host server.
type HostDB interface {
	// Ensure makes sure database name exists, creating and tagging it if not.
	// A database of that name that nexusctl did not create is an error.
	Ensure(ctx context.Context, name string) error
	Exists(ctx context.Context, name string) (bool, error)
	// Drop removes name if nexusctl created it; a missing database is fine.
	Drop(ctx context.Context, name string) error
	// Server describes where it is, for messages.
	Server() string
}

// PgHostDB is HostDB over pgx, connecting to AdminURL (a maintenance
// database, as a role that may CREATE DATABASE).
type PgHostDB struct{ AdminURL string }

var validDBName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func (p PgHostDB) connect(ctx context.Context) (*pgx.Conn, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return pgx.Connect(cctx, p.AdminURL)
}

func (p PgHostDB) Server() string { return redactURL(p.AdminURL) }

func (p PgHostDB) comment(ctx context.Context, c *pgx.Conn, name string) (exists bool, tag string, err error) {
	var t *string
	err = c.QueryRow(ctx, `SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1`, name).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if t != nil {
		tag = *t
	}
	return true, tag, nil
}

func (p PgHostDB) Ensure(ctx context.Context, name string) error {
	if !validDBName.MatchString(name) {
		return fmt.Errorf("database name %q must match %s", name, validDBName)
	}
	c, err := p.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	exists, tag, err := p.comment(ctx, c, name)
	if err != nil {
		return err
	}
	if exists {
		if tag != hostDBTag {
			return fmt.Errorf("database %s already exists and was not created by nexusctl; use another --project, or name a database yourself with %s", name, hostDBURLEnv)
		}
		return nil
	}
	if _, err := c.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	_, err = c.Exec(ctx, "COMMENT ON DATABASE "+name+" IS '"+hostDBTag+"'")
	return err
}

func (p PgHostDB) Exists(ctx context.Context, name string) (bool, error) {
	c, err := p.connect(ctx)
	if err != nil {
		return false, err
	}
	defer c.Close(ctx)
	exists, _, err := p.comment(ctx, c, name)
	return exists, err
}

func (p PgHostDB) Drop(ctx context.Context, name string) error {
	if !validDBName.MatchString(name) {
		return nil
	}
	c, err := p.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	exists, tag, err := p.comment(ctx, c, name)
	if err != nil || !exists {
		return err
	}
	if tag != hostDBTag {
		return fmt.Errorf("database %s was not created by nexusctl; not dropping it", name)
	}
	_, err = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	return err
}

// hostDBName is the database nexusctl uses for a compose project.
func hostDBName(project string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(project) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	name := "nexusctl_" + b.String()
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

// containerDBURL is database name on the admin URL's server as a container on
// this machine reaches it: loopback becomes host.docker.internal, and a URL
// with no user gets the OS user it implied (libpq in a container would
// otherwise log in as root).
func containerDBURL(adminURL, name string, environ []string) (string, error) {
	u, err := url.Parse(adminURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("host database admin URL %q is not a postgres:// URL", redactURL(adminURL))
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		host, port = u.Host, "5432"
	}
	switch host {
	case "127.0.0.1", "localhost", "::1":
		host = "host.docker.internal"
	}
	u.Host = net.JoinHostPort(host, port)
	if u.User == nil || u.User.Username() == "" {
		user := envValue(environ, "USER")
		if user == "" {
			user = os.Getenv("USER")
		}
		if user == "" {
			user = "postgres"
		}
		u.User = url.User(user)
	}
	u.Path = "/" + name
	return u.String(), nil
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable URL)"
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			u.User = url.UserPassword(u.User.Username(), "***")
		}
	}
	return u.String()
}

// hostDBError is the actionable failure for an unreachable or unusable host
// server.
func hostDBError(db HostDB, err error) *Error {
	return &Error{
		Kind:    KindHostDatabase,
		Message: "cannot use the host's PostgreSQL at " + db.Server() + " for the stack's database",
		Hint: "The stack runs on the host machine's PostgreSQL by default. Start it " +
			"(macOS: `brew services start postgresql@18`; Linux: `sudo systemctl start postgresql`), " +
			"or point NEXUS_HOST_PG_ADMIN_URL at a server where your role may CREATE DATABASE. " +
			"To use the bundled Postgres container instead, opt in: `nexusctl up --bundled-postgres` " +
			"(or NEXUS_BUNDLED_POSTGRES=1).",
		Err: err,
	}
}
