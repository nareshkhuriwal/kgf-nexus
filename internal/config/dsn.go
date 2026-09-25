// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package config resolves configuration that every control-plane service needs
// to agree on.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// dsnEnvNames are the accepted spellings for the Postgres DSN, in precedence
// order.
//
// Six services were built in parallel and each chose its own name for the same
// value: NEXUS_DATABASE_URL, NEXUS_POSTGRES_DSN, and DATABASE_URL all appeared.
// Rather than renaming five services and breaking anyone's existing deployment
// or compose file, every service accepts all of them. An operator should not
// have to remember which service wants which spelling -- that is exactly the
// kind of incidental knowledge that makes a platform unpleasant to run, and the
// kind of mismatch that fails at container start with a confusing "connection
// refused to localhost" rather than a clear configuration error.
var dsnEnvNames = []string{
	"NEXUS_DATABASE_URL",
	"DATABASE_URL",
	"NEXUS_POSTGRES_DSN",
	"NEXUS_DSN",
}

// DSN returns the Postgres DSN the service must connect to, and exits the
// process with an actionable message when none is configured.
//
// There is deliberately no compiled-in default, in any environment. Every
// service used to fall back to postgres://postgres:test@localhost:55432/nexus
// (artifact-gc to the compose stack on 55433) unless NEXUS_ENV named a
// production-like environment or NEXUS_REQUIRE_DSN was set. That fallback is
// how a service started without its DSN -- a typo such as NEXUS_POSTGRE_DSN, the
// variable exported in the wrong shell, a manifest that forgot one container --
// connected to whatever Postgres happened to listen on that port. On at least
// one developer machine that is an unrelated product's database
// (CONTRACT-FINDINGS.md), so "it started" proved nothing; in production the
// same mistake surfaced as a connection error naming a host the operator never
// configured. A service with no database cannot serve, so there is nothing to
// degrade to: refusing at startup is the only honest behaviour, and it costs
// local development one explicit variable, which deploy/docker-compose.yml, the
// Helm chart and the Makefile already set.
//
// flagNames are the command-line flags, if any, through which the caller also
// accepts a DSN (scheduler and dag-engine: "dsn"; resource-manager and
// artifact-gc: "database-url"). DSN is evaluated as that flag's default, which
// is before flag.Parse runs, so it reads os.Args itself: a DSN passed only on
// the command line is explicit configuration and must not be refused. An
// explicit -h/-help with no DSN anywhere returns "" so the flag package can
// print usage instead of the process dying on a configuration error.
//
// DSN exits rather than returning an error because every caller uses it in a
// flag default or a struct literal before it has a logger; the message goes to
// stderr for the same reason -- a configuration error the operator cannot see
// is worse than an unstructured one.
func DSN(flagNames ...string) string {
	dsn, err := resolveDSN(os.Getenv, os.Args[1:], flagNames)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: fatal: %v\n", filepath.Base(os.Args[0]), err)
		os.Exit(1)
	}
	return dsn
}

// resolveDSN is DSN's logic with its inputs injected and without the exit, so
// every branch is unit-testable.
//
// Precedence: the environment spellings in dsnEnvNames order, then the named
// flags. When both are present the environment value is returned here and the
// flag still wins after flag.Parse, as it always has.
func resolveDSN(getenv func(string) string, args, flagNames []string) (string, error) {
	for _, name := range dsnEnvNames {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v, nil
		}
	}
	if v, ok := flagValue(args, flagNames); ok {
		return v, nil
	}
	if len(flagNames) > 0 && helpRequested(args) {
		return "", nil
	}
	// The message names every accepted spelling. An operator who set one of
	// them almost-correctly needs to see the list to spot the typo;
	// "DATABASE_URL must be set" would send someone who set NEXUS_POSTGRES_DSN
	// looking in the wrong place.
	hint := "set " + dsnEnvNames[0] + " (also accepted: " + strings.Join(dsnEnvNames[1:], ", ") + ")"
	if len(flagNames) > 0 {
		dashed := make([]string, len(flagNames))
		for i, f := range flagNames {
			dashed[i] = "-" + f
		}
		hint += " or pass " + strings.Join(dashed, " / ")
	}
	return "", fmt.Errorf(
		"no Postgres DSN configured: %s. There is no built-in default, so a "+
			"service never silently connects to whatever database listens on "+
			"localhost. deploy/docker-compose.yml and the Helm chart set it for "+
			"every service; to run one outside them, export the DSN of the "+
			"database you mean (the compose stack's is in deploy/README.md)",
		hint,
	)
}

// flagValue finds the value given for any of names in args, accepting the forms
// the standard flag package does: -name value, --name value, -name=value and
// --name=value. The last occurrence wins, as it does in flag.Parse.
//
// Scanning stops at "--", after which the flag package treats everything as
// positional. It does not stop at the first positional argument, because
// without the flag set it cannot tell a positional argument from the value of a
// preceding flag; the cost is that a flag-shaped argument after a positional
// one would be misread, and no Nexus service takes positional arguments.
func flagValue(args, names []string) (string, bool) {
	var (
		val   string
		found bool
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, v, hasEq := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-"), "=")
		if !slices.Contains(names, name) {
			continue
		}
		if !hasEq {
			if i+1 >= len(args) {
				// A trailing valueless flag: flag.Parse rejects it with its own
				// error, so there is no DSN here to honour.
				continue
			}
			i++
			v = args[i]
		}
		// An explicit empty value overrides an earlier one, as in flag.Parse,
		// and is not a DSN.
		v = strings.TrimSpace(v)
		val, found = v, v != ""
	}
	return val, found
}

// helpRequested reports whether args ask the flag package for usage.
func helpRequested(args []string) bool {
	for _, a := range args {
		switch a {
		case "--":
			return false
		case "-h", "--h", "-help", "--help":
			return true
		}
	}
	return false
}

// Addr returns the first non-empty value among the named environment variables,
// or def. The services disagreed on listener naming just as they did on the
// DSN; unlike the DSN, a listen address has a safe default.
func Addr(def string, names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return def
}
