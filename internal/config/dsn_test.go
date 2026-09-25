// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// envMap is an injected getenv, so these tests never read the developer's shell:
// an exported DATABASE_URL would otherwise make the refusal assertions pass or
// fail for the wrong reason.
func envMap(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

var noEnv = envMap(nil)

// TestEachAcceptedNameWins covers the reason all four spellings exist: a service
// must honour whichever one the deployment happens to emit, not just its own.
func TestEachAcceptedNameWins(t *testing.T) {
	for _, name := range dsnEnvNames {
		t.Run(name, func(t *testing.T) {
			want := "postgres://nexus:pw@db:5432/nexus?via=" + name
			got, err := resolveDSN(envMap(map[string]string{name: want}), nil, nil)
			if err != nil || got != want {
				t.Errorf("resolveDSN with only %s set = (%q, %v), want (%q, nil)", name, got, err, want)
			}
		})
	}
}

// TestPrecedenceIsDocumentedOrder guards the ordering itself. When a compose file
// and an inherited shell each set a different name, which one wins has to be
// stable, or the same deployment reaches a different database depending on what
// else is exported.
func TestPrecedenceIsDocumentedOrder(t *testing.T) {
	// Set every name from index i onward; the winner must be the earliest set.
	for i := range dsnEnvNames {
		env := map[string]string{}
		for _, name := range dsnEnvNames[i:] {
			env[name] = "postgres://host/" + name
		}
		want := "postgres://host/" + dsnEnvNames[i]
		if got, err := resolveDSN(envMap(env), nil, nil); err != nil || got != want {
			t.Errorf("with %v set, resolveDSN = (%q, %v), want (%q, nil)", dsnEnvNames[i:], got, err, want)
		}
	}
}

// TestNoDSNIsRefusedInEveryEnvironment is the point of the helper: there is no
// compiled-in fallback, so a service without a DSN must refuse to start whatever
// NEXUS_ENV says -- including dev, which is where the old localhost default
// pointed services at an unrelated database.
func TestNoDSNIsRefusedInEveryEnvironment(t *testing.T) {
	for _, nexusEnv := range []string{"", "dev", "development", "test", "local", "production", "staging"} {
		for _, requireDSN := range []string{"", "0", "false", "1"} {
			env := envMap(map[string]string{"NEXUS_ENV": nexusEnv, "NEXUS_REQUIRE_DSN": requireDSN})
			got, err := resolveDSN(env, nil, nil)
			if err == nil {
				t.Errorf("NEXUS_ENV=%q NEXUS_REQUIRE_DSN=%q: resolveDSN = %q, want an error", nexusEnv, requireDSN, got)
			}
		}
	}
}

// TestRefusalIsActionable: the message is the whole value of the check. An
// operator who set a near-miss spelling has to see every accepted one, and the
// flag if the service has one, without reading the source.
func TestRefusalIsActionable(t *testing.T) {
	_, err := resolveDSN(noEnv, nil, []string{"database-url"})
	if err == nil {
		t.Fatal("resolveDSN with nothing configured returned no error")
	}
	msg := err.Error()
	for _, want := range append(append([]string{}, dsnEnvNames...), "-database-url", "no built-in default", "deploy/README.md") {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not mention %q: %s", want, msg)
		}
	}
	// And it must not suggest a DSN the operator never chose.
	if strings.Contains(msg, "postgres://") {
		t.Errorf("refusal embeds a DSN, which is a default by another name: %s", msg)
	}

	_, err = resolveDSN(noEnv, nil, nil)
	if err == nil || strings.Contains(err.Error(), " pass -") {
		t.Errorf("a service with no DSN flag must not be told to pass one: %v", err)
	}
}

// TestWhitespaceIsNotADSN: `DATABASE_URL=" "` in a YAML file is a mistake, not
// a configuration, and must be refused like an unset variable.
func TestWhitespaceIsNotADSN(t *testing.T) {
	if got, err := resolveDSN(envMap(map[string]string{"DATABASE_URL": "  "}), nil, nil); err == nil {
		t.Errorf("whitespace DATABASE_URL accepted as %q", got)
	}
}

// TestFlagCountsAsExplicit: resource-manager and artifact-gc are launched with
// only -database-url (the cluster-controller integration test and `make
// artifact-gc` both do), so a DSN given on the command line alone must satisfy
// the check, in every syntax the flag package accepts.
func TestFlagCountsAsExplicit(t *testing.T) {
	const want = "postgres://nexus:pw@db:5432/nexus"
	for _, args := range [][]string{
		{"-dsn", want},
		{"--dsn", want},
		{"-dsn=" + want},
		{"--dsn=" + want},
		{"-queue", "gpu", "-dsn", want, "-log-level", "warn"},
		{"-dsn", "postgres://earlier", "-dsn", want}, // last wins, as in flag.Parse
	} {
		got, err := resolveDSN(noEnv, args, []string{"dsn"})
		if err != nil || got != want {
			t.Errorf("args %q: resolveDSN = (%q, %v), want (%q, nil)", args, got, err, want)
		}
	}
}

// TestFlagEdgeCasesAreRefused covers the argv shapes that look like a DSN flag
// but do not carry one.
func TestFlagEdgeCasesAreRefused(t *testing.T) {
	for _, args := range [][]string{
		{"-dsn"},                          // trailing flag with no value
		{"-dsn="},                         // explicit empty
		{"-dsn", "postgres://x", "-dsn="}, // later empty overrides earlier
		{"--", "-dsn", "postgres://x"},    // after the terminator: positional
		{"-dsnx", "postgres://x"},         // a different flag
		{"-database-url", "postgres://x"}, // a flag this service does not have
	} {
		if got, err := resolveDSN(noEnv, args, []string{"dsn"}); err == nil {
			t.Errorf("args %q: resolveDSN = %q, want an error", args, got)
		}
	}
	// Flag names only count when the caller declared them: event-service has no
	// DSN flag, so -dsn on its command line is not configuration.
	if got, err := resolveDSN(noEnv, []string{"-dsn", "postgres://x"}, nil); err == nil {
		t.Errorf("undeclared flag accepted as %q", got)
	}
}

// TestEnvBeatsFlagHere documents the split of responsibility: with both set,
// resolveDSN returns the environment value as the flag's default and flag.Parse
// then applies the command-line value on top, so the flag wins overall.
func TestEnvBeatsFlagHere(t *testing.T) {
	got, err := resolveDSN(envMap(map[string]string{"DATABASE_URL": "postgres://env"}),
		[]string{"-dsn", "postgres://flag"}, []string{"dsn"})
	if err != nil || got != "postgres://env" {
		t.Errorf("resolveDSN = (%q, %v), want (%q, nil)", got, err, "postgres://env")
	}
}

// TestHelpIsNotAConfigurationError: `scheduler -h` must print usage, not die
// complaining about a database -- but only for a service that has flags to show.
func TestHelpIsNotAConfigurationError(t *testing.T) {
	for _, h := range []string{"-h", "--h", "-help", "--help"} {
		if got, err := resolveDSN(noEnv, []string{h}, []string{"dsn"}); err != nil || got != "" {
			t.Errorf("%s: resolveDSN = (%q, %v), want (\"\", nil)", h, got, err)
		}
	}
	if _, err := resolveDSN(noEnv, []string{"-h"}, nil); err == nil {
		t.Error("-h suppressed the refusal for a service with no DSN flag")
	}
	if _, err := resolveDSN(noEnv, []string{"--", "-h"}, []string{"dsn"}); err == nil {
		t.Error("a positional -h after -- suppressed the refusal")
	}
}

// TestDSNExitsNonZero exercises the real exit path in a subprocess: the unit
// tests above prove the decision, this proves a service actually stops.
func TestDSNExitsNonZero(t *testing.T) {
	if os.Getenv("NEXUS_DSN_EXIT_CHILD") == "1" {
		DSN()
		return // unreachable when the refusal works; the parent then fails
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDSNExitsNonZero$")
	env := []string{"NEXUS_DSN_EXIT_CHILD=1"}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(dsnEnvNames, name) {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("DSN() with nothing configured: err = %v, want exit status 1; output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "fatal: no Postgres DSN configured") {
		t.Errorf("stderr does not carry the refusal:\n%s", out)
	}
}
