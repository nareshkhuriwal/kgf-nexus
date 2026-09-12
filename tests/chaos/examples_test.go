//go:build chaos

package chaos

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestEveryCommittedExampleActuallyRuns submits every examples/*/compiled.yaml
// to the live stack and drives it to SUCCEEDED.
//
// This exists because of what F52 turned out to have proven. It established that
// all three committed examples were ACCEPTED by the gateway, and that read as
// "the examples work". It was a much weaker claim than it looked: accepted is not
// runnable, and at the time NO example in the repo could reach SUCCEEDED. Every
// task died on dispatch with "runtime.command is empty" because none of them
// declared a runtime block (F65).
//
// Four separate defects lived in that gap between "the gateway accepts it" and
// "the fleet runs it", and all four blocked a Phase 1 exit criterion:
//
//	F63  four of ten tasks demanded hardware the shipped fleet does not have
//	F64  no agent could advertise GPU memory, so any VRAM floor was unsatisfiable
//	F65  no example declared a runtime, so no example could execute at all
//	F66  no agent could advertise custom resources, so `register` never placed
//
// Every one was found by running an example BY HAND. Nothing in the repo would
// have caught any of them, and nothing stopped them recurring. Criterion 1 is the
// only exit criterion with no automated proof, and this is it.
//
// Discovering the examples from disk rather than listing them is deliberate: an
// example added later is covered without anyone remembering to register it here,
// which is precisely the kind of thing nobody remembers.
func TestEveryCommittedExampleActuallyRuns(t *testing.T) {
	requireStack(t)

	examples := discoverExamples(t)
	if len(examples) == 0 {
		t.Fatal("no examples/*/compiled.yaml found; this test would silently prove nothing")
	}
	t.Logf("found %d committed examples", len(examples))

	for _, ex := range examples {
		ex := ex
		t.Run(ex.Name, func(t *testing.T) {
			// Not parallel: the examples share one fleet, and several 10-node
			// DAGs at once would compete for capacity and turn a scheduling
			// delay into a spurious timeout. This test is about whether each
			// example CAN run, not about contention -- criterion 3 covers that.
			// Pin the run to the spec ID this submission resolved to, rather
			// than to the workflow NAME.
			//
			// Specs are content-addressed and immutable, so submitting an edited
			// example returns the id of whatever version matches its bytes --
			// and `run create <name>` would quietly use the LATEST registered
			// version instead. Verified: after stripping the runtime blocks back
			// out of hello_dag, a name-addressed run still passed, because it
			// ran the good version registered earlier. The test would never have
			// caught a regression in an example, which is its entire job.
			specID := submitSpec(t, ex.Path)

			runID := createRunFromSpec(t, specID)
			// Bounded well under waitForTerminalRun's budget, and non-fatal, so
			// a broken example reports WHY rather than just timing out. A CI
			// failure that says only "still RUNNING" sends the next person
			// digging through the database by hand -- the diagnostic below is
			// the pending_reason and error the platform already recorded.
			state := waitForTerminalRunWithin(t, runID, exampleBudget)
			if state != "SUCCEEDED" {
				t.Errorf("example %s reached %s, want SUCCEEDED (run %s)", ex.Name, state, runID)
				for _, line := range failedAttempts(t, runID) {
					t.Errorf("  %s", line)
				}
				return
			}

			// A run can reach SUCCEEDED with nodes that never ran, if the graph
			// let the engine conclude early. Assert the work actually happened.
			total, succeeded := nodeCounts(t, runID)
			if total == 0 {
				t.Errorf("example %s SUCCEEDED with no nodes at all", ex.Name)
			}
			if succeeded != total {
				t.Errorf("example %s SUCCEEDED but only %d of %d nodes did",
					ex.Name, succeeded, total)
			}
			t.Logf("%-18s %2d/%d nodes SUCCEEDED", ex.Name, succeeded, total)
		})
	}
}

// exampleBudget is how long one example gets. Generous next to the ~36s
// ten_node_mixed takes, but far short of runBudget: an example that is not going
// to finish should say so while the reason is still worth printing.
const exampleBudget = 2 * time.Minute

type example struct {
	Name     string // directory name, e.g. ten_node_mixed
	Workflow string // spec name from the yaml, e.g. ten-node-mixed
	Path     string
}

func discoverExamples(t *testing.T) []example {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(composeCwd, "examples", "*", "compiled.yaml"))
	if err != nil {
		t.Fatalf("glob examples: %v", err)
	}
	var got []example
	for _, path := range matches {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		name := workflowName(string(body))
		if name == "" {
			t.Errorf("%s declares no top-level name; it cannot be run", path)
			continue
		}
		got = append(got, example{
			Name:     filepath.Base(filepath.Dir(path)),
			Workflow: name,
			Path:     path,
		})
	}
	return got
}

// workflowName pulls the top-level `name:` out of a compiled spec. A real YAML
// parse would be better, but the chaos package deliberately has no dependencies
// beyond the standard library, and the compiled specs are machine-generated with
// the name unindented at the top level.
func workflowName(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "name:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "name:"))
		}
	}
	return ""
}

// submitSpec registers the example. Resubmitting a byte-identical spec is a
// no-op that returns the existing version (specs are content-addressed), so this
// is safe to run repeatedly and it exercises the real admission path -- which is
// where F52's enum and schema defects lived.
func submitSpec(t *testing.T, path string) string {
	t.Helper()
	endpoint := envOr("NEXUS_ENDPOINT", "http://localhost:8090")
	key := os.Getenv("NEXUS_API_KEY")
	if key == "" {
		t.Skip("NEXUS_API_KEY is unset")
	}
	out, err := curl(t, "-X", "POST",
		endpoint+"/v1/namespaces/default/specs",
		"-H", "Authorization: Bearer "+key,
		"-H", "Content-Type: application/yaml",
		"--data-binary", "@"+path)
	if err != nil {
		t.Fatalf("submit %s: %v", path, err)
	}
	if strings.Contains(out, `"error"`) {
		t.Fatalf("submit %s was rejected: %s", path, out)
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode submit response %q: %v", out, err)
	}
	if resp.ID == "" {
		t.Fatalf("submit %s returned no spec id: %s", path, out)
	}
	return resp.ID
}

// createRunFromSpec starts a run against one exact spec version.
func createRunFromSpec(t *testing.T, specID string) string {
	t.Helper()
	endpoint := envOr("NEXUS_ENDPOINT", "http://localhost:8090")
	key := os.Getenv("NEXUS_API_KEY")
	if key == "" {
		t.Skip("NEXUS_API_KEY is unset")
	}
	out, err := curl(t, "-X", "POST",
		endpoint+"/v1/namespaces/default/runs",
		"-H", "Authorization: Bearer "+key,
		"-H", "Content-Type: application/json",
		"--data", fmt.Sprintf(`{"spec_id":%q}`, specID))
	if err != nil {
		t.Fatalf("create run from spec %s: %v", specID, err)
	}
	var resp struct {
		ID      string `json:"id"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode create-run response %q: %v", out, err)
	}
	if resp.ID == "" {
		t.Fatalf("create run from spec %s failed: %s %s", specID, resp.Error, resp.Message)
	}
	return resp.ID
}

func nodeCounts(t *testing.T, runID string) (total, succeeded int) {
	t.Helper()
	total, _ = strconv.Atoi(psql(t, fmt.Sprintf(
		`SELECT count(*)::int FROM task_nodes WHERE run_id='%s'`, runID)))
	succeeded, _ = strconv.Atoi(psql(t, fmt.Sprintf(
		`SELECT count(*)::int FROM task_nodes WHERE run_id='%s' AND state='SUCCEEDED'`, runID)))
	return total, succeeded
}

// failedAttempts reports why an example did not finish. Without this a CI
// failure says only "reached RUNNING, want SUCCEEDED", which is the least useful
// half of what the database already knows -- and the pending_reason it prints is
// the field F59 exists to populate.
func failedAttempts(t *testing.T, runID string) []string {
	t.Helper()
	out := psql(t, fmt.Sprintf(
		`SELECT node_id||' ['||state||'] '||
		        coalesce(nullif(pending_reason,''), left(coalesce(error::text,''),200), '-')
		   FROM task_attempts
		  WHERE run_id='%s' AND state <> 'SUCCEEDED'
		  ORDER BY node_id`, runID))
	var got []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			got = append(got, line)
		}
	}
	return got
}
