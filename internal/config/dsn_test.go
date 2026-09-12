// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

const devDefault = "postgres://postgres:test@localhost:55432/nexus"

// clearDSNEnv unsets every name the resolver consults, because the test process
// inherits the developer's shell and an exported DATABASE_URL would otherwise
// make the default-path assertions pass or fail for the wrong reason.
func clearDSNEnv(t *testing.T) {
	t.Helper()
	for _, name := range dsnEnvNames {
		t.Setenv(name, "")
	}
	for _, name := range dsnRequiredEnvNames {
		t.Setenv(name, "")
	}
}

// TestEachAcceptedNameWins covers the reason all four spellings exist: a service
// must honour whichever one the deployment happens to emit, not just its own.
func TestEachAcceptedNameWins(t *testing.T) {
	for _, name := range dsnEnvNames {
		t.Run(name, func(t *testing.T) {
			clearDSNEnv(t)
			want := "postgres://nexus:pw@db:5432/nexus?via=" + name
			t.Setenv(name, want)
			if got, err := dsnOrError(devDefault); err != nil || got != want {
				t.Errorf("dsnOrError with only %s set = (%q, %v), want (%q, nil)", name, got, err, want)
			}
		})
	}
}

// TestPrecedenceIsDocumentedOrder guards the ordering itself. When a compose file
// and an inherited shell each set a different name, which one wins has to be
// stable, or the same deployment reaches a different database depending on what
// else is exported.
func TestPrecedenceIsDocumentedOrder(t *testing.T) {
	// Set every name, then remove them from the front one at a time. The winner
	// must always be the earliest name still set.
	for i := range dsnEnvNames {
		clearDSNEnv(t)
		for j, name := range dsnEnvNames {
			if j < i {
				continue
			}
			t.Setenv(name, "postgres://host/"+name)
		}
		want := "postgres://host/" + dsnEnvNames[i]
		if got, err := dsnOrError(devDefault); err != nil || got != want {
			t.Errorf("with %v set, dsnOrError = (%q, %v), want (%q, nil)",
				dsnEnvNames[i:], got, err, want)
		}
	}
}

// TestDevDefaultStillApplies is the behaviour the production guard must not cost:
// `go test` and a bare `go run` with nothing exported keep working.
func TestDevDefaultStillApplies(t *testing.T) {
	clearDSNEnv(t)
	if got, err := dsnOrError(devDefault); err != nil || got != devDefault {
		t.Errorf("dsnOrError with no env = (%q, %v), want (%q, nil)", got, err, devDefault)
	}
}

// TestProductionRefusesTheDefault is the point of the guard: a deployment that
// declares itself production must fail at startup rather than dial localhost.
func TestProductionRefusesTheDefault(t *testing.T) {
	for _, env := range []struct{ name, value string }{
		{"NEXUS_ENV", "production"},
		{"NEXUS_ENV", "prod"},
		{"NEXUS_ENV", "PRODUCTION"},
		{"NEXUS_ENV", "staging"},
		{"NEXUS_REQUIRE_DSN", "1"},
		{"NEXUS_REQUIRE_DSN", "true"},
	} {
		t.Run(env.name+"="+env.value, func(t *testing.T) {
			clearDSNEnv(t)
			t.Setenv(env.name, env.value)
			got, err := dsnOrError(devDefault)
			if err == nil {
				t.Fatalf("dsnOrError returned %q, want an error", got)
			}
			// The message is the whole value of the check. An operator who set
			// the wrong spelling has to be able to see the right one without
			// reading the source.
			for _, name := range dsnEnvNames {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error does not name %s: %v", name, err)
				}
			}
		})
	}
}

// TestProductionAcceptsAnExplicitDSN confirms the guard checks for a DSN and not
// merely for the environment name.
func TestProductionAcceptsAnExplicitDSN(t *testing.T) {
	clearDSNEnv(t)
	t.Setenv("NEXUS_ENV", "production")
	want := "postgres://nexus:pw@prod-db:5432/nexus?sslmode=require"
	t.Setenv("DATABASE_URL", want)
	if got, err := dsnOrError(devDefault); err != nil || got != want {
		t.Errorf("dsnOrError in production with DATABASE_URL set = (%q, %v), want (%q, nil)", got, err, want)
	}
}

// TestNonProductionEnvKeepsTheDefault: NEXUS_ENV exists for more than the guard,
// so a developer setting NEXUS_ENV=dev must not be forced to supply a DSN.
func TestNonProductionEnvKeepsTheDefault(t *testing.T) {
	for _, value := range []string{"dev", "development", "test", "local", ""} {
		clearDSNEnv(t)
		t.Setenv("NEXUS_ENV", value)
		if got, err := dsnOrError(devDefault); err != nil || got != devDefault {
			t.Errorf("NEXUS_ENV=%q: dsnOrError = (%q, %v), want (%q, nil)", value, got, err, devDefault)
		}
	}
}

// TestRequireDSNCanBeDisabledExplicitly: an operator who inherits
// NEXUS_REQUIRE_DSN from a base config needs a way to turn it off that is not
// "unset it", because unsetting a variable in a layered config system is often
// impossible.
func TestRequireDSNCanBeDisabledExplicitly(t *testing.T) {
	for _, value := range []string{"0", "false", "no", "off"} {
		clearDSNEnv(t)
		t.Setenv("NEXUS_ENV", "production")
		t.Setenv("NEXUS_REQUIRE_DSN", value)
		if DSNRequired() {
			t.Errorf("NEXUS_REQUIRE_DSN=%q did not override NEXUS_ENV=production", value)
		}
		if got, err := dsnOrError(devDefault); err != nil || got != devDefault {
			t.Errorf("NEXUS_REQUIRE_DSN=%q: dsnOrError = (%q, %v), want (%q, nil)", value, got, err, devDefault)
		}
	}
}
