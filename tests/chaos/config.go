// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package chaos

// Configuration the chaos suite reads from its environment. Untagged, unlike
// the tests themselves, so the parsing is unit-tested without a stack.

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// composeCwd is where compose runs and examples are found: the repository root.
const composeCwd = "../.."

// composeFileArgs returns the `-f` flags for every compose command the suite
// runs.
//
//   - NEXUS_COMPOSE_FILE: one file or a comma-separated list, applied in order,
//     so an isolated stack with overlays (the e2e port override, the host
//     database, a GPU simulation) is addressed exactly as it was brought up.
//     It used to take one file, which forced a `docker compose config` render
//     of the overlays into a single file (F150).
//   - otherwise COMPOSE_FILE, which compose reads itself (with its own
//     separator), so no -f is passed;
//   - otherwise deploy/docker-compose.yml.
func composeFileArgs(getenv func(string) string) []string {
	raw := getenv("NEXUS_COMPOSE_FILE")
	if raw == "" {
		if getenv("COMPOSE_FILE") != "" {
			return nil
		}
		raw = "deploy/docker-compose.yml"
	}
	var args []string
	for _, f := range strings.Split(raw, ",") {
		if f = strings.TrimSpace(f); f != "" {
			args = append(args, "-f", f)
		}
	}
	return args
}

// example is one committed examples/<name>/compiled.yaml.
type example struct {
	Name     string // directory name, e.g. ten_node_mixed
	Workflow string // spec name from the yaml, e.g. ten-node-mixed
	Path     string
}

// defaultExampleBudget is how long an example gets when exampleBudgets does not
// name it: several times the slowest short example measured on a loaded
// laptop compose stack (fanout_scale, 503 nodes, 42 s; ten_node_mixed ~36 s),
// so a delay is not mistaken for a hang, while a wedged example still reports
// its pending reasons within minutes.
const defaultExampleBudget = 5 * time.Minute

// exampleBudgets are the examples that legitimately take longer than the
// default, with room for a loaded host. A single 120 s budget for all of them
// timed out 6 of 14 in the F150 sweep, runs that SUCCEEDED when checked
// afterwards. Measured on the compose stack on a loaded laptop (F157 sweep,
// every service pool capped at 2 connections) and with examples/run-suite.sh
// on the Kubernetes dev environment.
var exampleBudgets = map[string]time.Duration{
	// A 50-trial CV search, bootstrap and permutation importance: ~11 min on
	// the dev cluster; on the loaded compose stack 44 of 50 trials had
	// finished at 20 min (trials took 2-8 min each on one 8-core agent).
	"credit_risk_ml": 40 * time.Minute,
	// Detection and denoise passes on the GPU workers: ~7 min on the dev
	// cluster, 8 min 5 s on the loaded compose stack.
	"image_processing_gpu": 15 * time.Minute,
}

// exampleBudget is the budget for one example.
//
//	NEXUS_CHAOS_EXAMPLE_BUDGET        replaces the default (a Go duration);
//	                                  never shrinks a declared per-example budget
//	NEXUS_CHAOS_EXAMPLE_BUDGET_SCALE  multiplies every budget (> 0), for a host
//	                                  known to be slow or shared
func exampleBudget(name string, getenv func(string) string) (time.Duration, error) {
	def := defaultExampleBudget
	if raw := getenv("NEXUS_CHAOS_EXAMPLE_BUDGET"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("NEXUS_CHAOS_EXAMPLE_BUDGET=%q: want a positive duration such as 5m", raw)
		}
		def = d
	}
	budget := def
	if d, ok := exampleBudgets[name]; ok && d > budget {
		budget = d
	}
	if raw := getenv("NEXUS_CHAOS_EXAMPLE_BUDGET_SCALE"); raw != "" {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil || f <= 0 {
			return 0, fmt.Errorf("NEXUS_CHAOS_EXAMPLE_BUDGET_SCALE=%q: want a number > 0", raw)
		}
		budget = time.Duration(f * float64(budget))
	}
	return budget, nil
}

// selectExamples narrows the sweep to NEXUS_CHAOS_EXAMPLES (comma-separated
// directory names), in discovery order. A name that is not an example is an
// error: a typo must not turn into a smaller sweep that passes.
func selectExamples(all []example, getenv func(string) string) ([]example, error) {
	raw := getenv("NEXUS_CHAOS_EXAMPLES")
	if strings.TrimSpace(raw) == "" {
		return all, nil
	}
	want := map[string]bool{}
	for _, n := range strings.Split(raw, ",") {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = true
		}
	}
	var out []example
	for _, ex := range all {
		if want[ex.Name] {
			out = append(out, ex)
			delete(want, ex.Name)
		}
	}
	if len(want) > 0 {
		var missing []string
		for n := range want {
			missing = append(missing, n)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("NEXUS_CHAOS_EXAMPLES names %s, which %s not a runnable example here", strings.Join(missing, ", "), map[bool]string{true: "is", false: "are"}[len(missing) == 1])
	}
	return out, nil
}

// chaosDatabaseURL is the stack's database as the host reaches it, for a stack
// with no postgres container (the host-database overlay). Empty means query
// the stack's own postgres service.
func chaosDatabaseURL() string { return os.Getenv("NEXUS_CHAOS_DATABASE_URL") }
