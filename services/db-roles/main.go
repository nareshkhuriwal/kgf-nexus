// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Command nexus-db-roles creates or updates the per-service LOGIN roles the
// Nexus services connect as (internal/dbroles). It runs as the database
// owner/admin login, from the migrate image, as the Helm chart's db-roles Job.
//
//	DATABASE_URL=postgres://<owner>:...@host/nexus?sslmode=verify-full&sslrootcert=... \
//	NEXUS_DB_ROLES='[{"login":"nexus_api_gateway_login","member_of":["nexus_api_gateway"],"password_env":"PW_API_GATEWAY"}]' \
//	PW_API_GATEWAY=... nexus-db-roles
//
// The spec names the environment variable holding each password, never the
// password itself, so it can live in a manifest. Output names logins and the
// grants changed; it never prints a password or a verifier.
//
// On a server shared by several Nexus installations (F153, OPERATIONS §20.12):
//
//	NEXUS_DB_ROLE_PREFIX=acme            the installation's group roles are acme_*
//	                                     (the migrate job's same setting, 0038)
//	NEXUS_DB_REVOKE_PUBLIC_CONNECT=true  REVOKE CONNECT ON DATABASE <this> FROM PUBLIC
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/dbroles"
)

var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("db-roles failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	spec := os.Getenv("NEXUS_DB_ROLES")
	if strings.TrimSpace(spec) == "" {
		return fmt.Errorf("NEXUS_DB_ROLES is empty: nothing to do (the chart sets it when postgres.runtimeRoles.enabled)")
	}
	// F153: the installation's group-role prefix (the migrate job's
	// NEXUS_DB_ROLE_PREFIX, migration 0038) and whether to shut PUBLIC out of
	// this database, so logins made for other installations on a shared
	// server cannot connect here at all.
	opts := dbroles.Options{GroupPrefix: strings.TrimSpace(os.Getenv("NEXUS_DB_ROLE_PREFIX"))}
	switch v := strings.TrimSpace(os.Getenv("NEXUS_DB_REVOKE_PUBLIC_CONNECT")); v {
	case "", "false":
	case "true":
		opts.RevokePublicConnect = true
	default:
		return fmt.Errorf("NEXUS_DB_REVOKE_PUBLIC_CONNECT=%q: want true or false", v)
	}
	logins, err := dbroles.ParseSpecWithOptions([]byte(spec), opts)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	conn, err := pgx.Connect(ctx, config.DSN())
	if err != nil {
		return fmt.Errorf("connect as the owner login: %w", err)
	}
	defer conn.Close(context.Background())

	results, err := dbroles.ApplyWithOptions(ctx, conn, logins, os.Getenv, opts)
	if err != nil {
		return err
	}
	for _, r := range results {
		log.Info("runtime login in place",
			"login", r.Login, "created", r.Created, "read_only", r.ReadOnly,
			"granted", r.Granted, "revoked", r.Revoked, "version", version)
	}
	if opts.RevokePublicConnect {
		log.Info("PUBLIC's CONNECT on this database revoked; only the owner and the logins above connect",
			"version", version)
	}
	return nil
}
