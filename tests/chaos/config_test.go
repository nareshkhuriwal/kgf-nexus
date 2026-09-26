// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package chaos

// Unit tests for the suite's configuration. No build tag: they need no stack,
// so `go test ./tests/chaos/` runs them while the chaos tests stay opt-in.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestComposeFileArgs(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"default", nil, []string{"-f", "deploy/docker-compose.yml"}},
		{"one file", map[string]string{"NEXUS_COMPOSE_FILE": "deploy/x.yml"}, []string{"-f", "deploy/x.yml"}},
		{
			"overlays, comma-separated, in order",
			map[string]string{"NEXUS_COMPOSE_FILE": "deploy/docker-compose.yml, deploy/e2e/docker-compose.e2e.yml,deploy/docker-compose.host-db.yml"},
			[]string{"-f", "deploy/docker-compose.yml", "-f", "deploy/e2e/docker-compose.e2e.yml", "-f", "deploy/docker-compose.host-db.yml"},
		},
		{"empty entries are ignored", map[string]string{"NEXUS_COMPOSE_FILE": "a.yml,,b.yml,"}, []string{"-f", "a.yml", "-f", "b.yml"}},
		// Compose reads COMPOSE_FILE itself (with its own separator); passing -f
		// would override it.
		{"COMPOSE_FILE is left to compose", map[string]string{"COMPOSE_FILE": "a.yml:b.yml"}, nil},
		{"NEXUS_COMPOSE_FILE wins", map[string]string{"COMPOSE_FILE": "a.yml", "NEXUS_COMPOSE_FILE": "b.yml"}, []string{"-f", "b.yml"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := composeFileArgs(envMap(tc.env)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("composeFileArgs = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExampleBudget(t *testing.T) {
	get := func(name string, env map[string]string) time.Duration {
		t.Helper()
		d, err := exampleBudget(name, envMap(env))
		if err != nil {
			t.Fatalf("exampleBudget(%s): %v", name, err)
		}
		return d
	}
	if d := get("hello_dag", nil); d != defaultExampleBudget {
		t.Errorf("an example with no entry gets %s, want the default %s", d, defaultExampleBudget)
	}
	if long := get("credit_risk_ml", nil); long <= defaultExampleBudget {
		t.Errorf("credit_risk_ml (~11 min on the dev cluster) gets only %s", long)
	}
	if d := get("hello_dag", map[string]string{"NEXUS_CHAOS_EXAMPLE_BUDGET": "90s"}); d != 90*time.Second {
		t.Errorf("override: %s", d)
	}
	if d := get("credit_risk_ml", map[string]string{"NEXUS_CHAOS_EXAMPLE_BUDGET": "90s"}); d != exampleBudgets["credit_risk_ml"] {
		t.Errorf("the default override must not shrink a declared per-example budget: %s", d)
	}
	if d := get("hello_dag", map[string]string{"NEXUS_CHAOS_EXAMPLE_BUDGET_SCALE": "2.5"}); d != time.Duration(2.5*float64(defaultExampleBudget)) {
		t.Errorf("scale: %s", d)
	}
	for _, bad := range []map[string]string{
		{"NEXUS_CHAOS_EXAMPLE_BUDGET": "soon"},
		{"NEXUS_CHAOS_EXAMPLE_BUDGET": "-1m"},
		{"NEXUS_CHAOS_EXAMPLE_BUDGET_SCALE": "0"},
		{"NEXUS_CHAOS_EXAMPLE_BUDGET_SCALE": "x"},
	} {
		if _, err := exampleBudget("hello_dag", envMap(bad)); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// Every budget names an example that exists, so a renamed example cannot keep
// a stale entry while the real one silently falls back to the default.
func TestExampleBudgetsNameRealExamples(t *testing.T) {
	for name := range exampleBudgets {
		if _, err := os.Stat(filepath.Join(composeCwd, "examples", name, "compiled.yaml")); err != nil {
			t.Errorf("exampleBudgets has %q, which is not an example: %v", name, err)
		}
	}
}

func TestSelectedExamples(t *testing.T) {
	all := []example{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	got, err := selectExamples(all, envMap(map[string]string{"NEXUS_CHAOS_EXAMPLES": "c, a"}))
	if err != nil || len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("selectExamples = %v, %v", got, err)
	}
	if got, _ := selectExamples(all, envMap(nil)); len(got) != 3 {
		t.Fatalf("no filter: %v", got)
	}
	if _, err := selectExamples(all, envMap(map[string]string{"NEXUS_CHAOS_EXAMPLES": "a,nope"})); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("an unknown name must be an error, not a silently smaller sweep: %v", err)
	}
}
