// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package config resolves configuration that every control-plane service needs
// to agree on.
package config

import (
	"fmt"
	"os"
	"strings"
)

// dsnEnvNames are the accepted spellings for the Postgres DSN, in precedence
// order.
//
// Six services were built in parallel and each chose its own name for the same
// value: NEXUS_DATABASE_URL, NEXUS_POSTGRES_DSN, and DATABASE_URL all appeared.
// Rather than renaming five services and breaking anyone's existing deployment
// or compose file, every service accepts all three. An operator should not have
// to remember which service wants which spelling -- that is exactly the kind of
// incidental knowledge that makes a platform unpleasant to run, and the kind of
// mismatch that fails at container start with a confusing "connection refused
// to localhost" rather than a clear configuration error.
var dsnEnvNames = []string{
	"NEXUS_DATABASE_URL",
	"DATABASE_URL",
	"NEXUS_POSTGRES_DSN",
	"NEXUS_DSN",
}

// dsnRequiredEnvNames are the spellings that turn the compiled-in default from a
// convenience into a fatal error.
//
// Accepting four DSN names is what makes the dev experience painless, and it is
// also the failure mode this guards: setting NEXUS_POSTGRE_DSN, or exporting the
// DSN in the wrong shell, leaves every service silently pointed at the
// developer's localhost test database. In dev that is obvious within a second.
// In production nothing is obvious -- the pool dials localhost, fails, and the
// service reports a connection error naming a host the operator never
// configured, so the investigation starts at the network layer instead of at the
// one line of YAML that is actually wrong.
//
// Two spellings because deployments express "this is production" differently:
// NEXUS_ENV mirrors the ubiquitous APP_ENV/RAILS_ENV convention an operator will
// already be setting for other reasons, while NEXUS_REQUIRE_DSN states the
// intent directly for anyone who wants the check without claiming an
// environment name (staging, canary, a CI job that must never touch a laptop
// database).
var dsnRequiredEnvNames = []string{
	"NEXUS_REQUIRE_DSN",
	"NEXUS_ENV",
}

// DSN returns the first Postgres DSN found among the accepted environment
// variable names, falling back to def when none is set.
//
// When the deployment has asked for an explicit DSN (see DSNRequired) and none
// of the accepted names is set, this exits the process rather than returning
// def. It cannot return an error instead: the six callers use it in a
// flag.String default or a struct literal, and threading an error through all of
// them would be a wider change than the guard is worth. Exiting at startup is
// also the correct behaviour -- a service with no database cannot serve, so
// there is nothing to degrade to.
func DSN(def string) string {
	dsn, err := dsnOrError(def)
	if err != nil {
		// Written to stderr rather than through slog because every caller
		// resolves the DSN before it has a logger, and a configuration error the
		// operator cannot see is worse than an unstructured one.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
	return dsn
}

// DSNRequired reports whether the deployment has demanded an explicit DSN.
//
// NEXUS_ENV counts only when it names a production-like environment, because
// NEXUS_ENV=dev must keep the built-in default working -- the whole point is
// that the dev path is unchanged.
func DSNRequired() bool {
	if v := os.Getenv("NEXUS_REQUIRE_DSN"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "0", "false", "no", "off":
			return false
		default:
			return true
		}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NEXUS_ENV"))) {
	case "production", "prod", "staging":
		return true
	}
	return false
}

// dsnOrError is DSN's logic without the process exit, so the refusal is testable
// and so a future caller that can handle an error has something to call.
func dsnOrError(def string) (string, error) {
	for _, name := range dsnEnvNames {
		if v := os.Getenv(name); v != "" {
			return v, nil
		}
	}
	if DSNRequired() {
		// The message names every spelling that was checked. An operator who set
		// one of them almost-correctly needs to see the list to spot the typo;
		// "DATABASE_URL must be set" would send someone who set
		// NEXUS_POSTGRES_DSN looking in the wrong place.
		return "", fmt.Errorf(
			"no Postgres DSN configured, and %s requires an explicit one: set one of %s (the built-in default %q is a developer convenience and is refused here)",
			strings.Join(dsnRequiredEnvNames, " or "),
			strings.Join(dsnEnvNames, ", "),
			def,
		)
	}
	return def, nil
}

// AddrEnvNames maps a logical listener to its accepted environment spellings,
// for the same reason as DSN: the services disagreed on naming.
func Addr(def string, names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return def
}
