// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sdkPython finds an interpreter with the Nexus SDK installed: NEXUS_TEST_PYTHON,
// else the repository's `make sdk-test` venv. Tests that execute workflows skip
// without one; the argument handling is tested regardless.
func sdkPython(t *testing.T) string {
	t.Helper()
	candidates := []string{os.Getenv("NEXUS_TEST_PYTHON")}
	if abs, err := filepath.Abs(filepath.Join("..", "..", "..", ".venv-sdk", "bin", "python")); err == nil {
		candidates = append(candidates, abs)
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if exec.Command(c, "-c", "import nexus.local").Run() == nil {
			return c
		}
	}
	t.Skip("no interpreter with the Nexus SDK (set NEXUS_TEST_PYTHON or run make sdk-test)")
	return ""
}

func repoFile(parts ...string) string {
	p, _ := filepath.Abs(filepath.Join(append([]string{"..", "..", ".."}, parts...)...))
	return p
}

func TestRunWithoutAVerbOrLocalIsExplained(t *testing.T) {
	h := newHarness(t)
	code, _, stderr := h.run("run", "--output", "json")
	if code == 0 || !strings.Contains(stderr, "no command given") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !hasLocalFlag([]string{"x.yaml", "--local"}) || hasLocalFlag([]string{"--", "--local"}) || hasLocalFlag([]string{"list"}) {
		t.Fatal("hasLocalFlag misreads its arguments")
	}
}

func TestRunLocalExecutesAYAMLWorkflow(t *testing.T) {
	py := sdkPython(t)
	h := newHarness(t)
	dir := t.TempDir()
	code, stdout, stderr := h.run("run", "--local", repoFile("examples", "fan_out_map", "compiled.yaml"),
		"--param", "source=demo", "--python", py, "--run-dir", dir, "-o", "json")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	var res struct {
		State string `json:"state"`
		Nodes []struct {
			NodeID string `json:"node_id"`
			State  string `json:"state"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout is not the JSON result: %v\n%s", err, stdout)
	}
	if res.State != "SUCCEEDED" || len(res.Nodes) != 8 {
		t.Fatalf("result = %+v", res)
	}
	if len(h.requests) != 0 {
		t.Fatalf("a local run contacted the control plane: %v", h.requests)
	}
	if _, err := os.Stat(filepath.Join(dir, "run.json")); err != nil {
		t.Fatalf("run.json not written: %v", err)
	}
}

func TestRunLocalFailureIsExitOne(t *testing.T) {
	py := sdkPython(t)
	spec := filepath.Join(t.TempDir(), "fail.yaml")
	writeFile(t, spec, `api_version: nexus.dev/v1
name: fails
tasks:
  - id: boom
    task_ref: t.boom
    runtime: {executor: subprocess, command: ["sh", "-c", "echo bad input >&2; exit 3"]}
`)
	h := newHarness(t)
	code, _, stderr := h.run("run", "--local", spec, "--python", py, "--run-dir", t.TempDir())
	if code != 1 || !strings.Contains(stderr, "did not succeed") || !strings.Contains(stderr, "bad input") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestRunLocalReportsLocalProblemsAtTheirLine(t *testing.T) {
	spec := filepath.Join(t.TempDir(), "bad.yaml")
	writeFile(t, spec, `api_version: nexus.dev/v1
name: bad
tasks:
  - id: a
    task_ref: t.a
edges:
  - from_node_id: a
    to_node_id: ghost
`)
	h := newHarness(t)
	code, _, stderr := h.run("run", "--local", spec, "--python", "python3")
	if code == 0 {
		t.Fatal("an invalid spec ran")
	}
	want := spec + ":8:17: edges[0] references unknown to_node_id \"ghost\" (/edges/0/to_node_id)"
	if !strings.Contains(stderr, want) {
		t.Fatalf("stderr lacks %q:\n%s", want, stderr)
	}
}

func TestSubmitMapsServerProblemsToYAMLLines(t *testing.T) {
	spec := filepath.Join(t.TempDir(), "wf.yaml")
	writeFile(t, spec, `api_version: nexus.dev/v1
name: wf
tasks:
  - id: a
    task_ref: t.a
    queue: NOT A QUEUE
  - id: b
    task_ref: t.b
    runtime:
      command: ["true"]
      stdout_output: nope
`)
	h := newHarness(t)
	h.replyJSON(422, `{"error":"spec_invalid","message":"spec failed validation","problems":[
		{"path":"/tasks/0/queue","message":"pattern mismatch","stage":"schema","line":1,"column":80},
		{"path":"/tasks/b/runtime/stdout_output","message":"not declared","stage":"semantic"}]}`)
	code, _, stderr := h.run("workflow", "submit", spec)
	if code == 0 {
		t.Fatal("expected failure")
	}
	for _, want := range []string{
		spec + ":6:12: pattern mismatch (/tasks/0/queue)",
		spec + ":11:22: not declared (/tasks/b/runtime/stdout_output)",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

func TestPythonSpecProblemsPointAtPythonLines(t *testing.T) {
	py := sdkPython(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "wf_lines.py")
	writeFile(t, src, `from nexus import task, workflow


@task(task_ref="l.a", queue="default")
def a(ctx):
    return 1


@workflow(name="lines")
def wf():
    a()
`)
	h := newHarness(t)
	h.replyJSON(422, `{"error":"spec_invalid","message":"spec failed validation","problems":[
		{"path":"/tasks/a/queue","message":"queue is not served","stage":"semantic"}]}`)
	code, _, stderr := h.run("workflow", "submit", src, "--python", py)
	if code == 0 {
		t.Fatal("expected failure")
	}
	if want := src + ":4: queue is not served (/tasks/a/queue)"; !strings.Contains(stderr, want) {
		t.Fatalf("stderr lacks %q:\n%s", want, stderr)
	}
}

const replayResponse = `{"namespace":"default","encoding":"protojson",
 "assignment":{"attempt_id":"22222222-2222-2222-2222-222222222222","run_id":"r","node_id":"a",
  "attempt_number":1,"idempotency_key":"idem-k",
  "spec":{"id":"a","task_ref":"t.a","outputs":{"result":{"type":"json"}},
   "runtime":{"executor":"subprocess","command":["sh","-c"],
    "args":["cat \"$NEXUS_INPUT_DIR/x\" > \"$NEXUS_OUTPUT_FILE\"; echo key=$NEXUS_IDEMPOTENCY_KEY"]}},
  "inputs":{"x":{"inline":"eyJ2IjogNDJ9","content_type":"application/json"}}},
 "result_store":{"inline_threshold_bytes":65536},"secrets":[],"omitted":["lease_token"]}`

func TestReplayDownloadsTheAssignment(t *testing.T) {
	h := newHarness(t)
	h.replyJSON(http.StatusOK, replayResponse)
	out := filepath.Join(t.TempDir(), "rp")
	code, stdout, stderr := h.run("replay", "22222222-2222-2222-2222-222222222222", "--out", out)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if got := h.last().Path; got != "/v1/namespaces/default/runs/-/attempts/22222222-2222-2222-2222-222222222222/assignment" {
		t.Fatalf("path = %s", got)
	}
	if !strings.Contains(stdout, "--local") {
		t.Errorf("stdout does not say how to replay: %s", stdout)
	}
	info, err := os.Stat(filepath.Join(out, "assignment.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("assignment.json: %v %v", info, err)
	}
	// --run narrows the lookup to one run.
	h.run("replay", "22222222-2222-2222-2222-222222222222", "--run", "r-1", "--out", out)
	if got := h.last().Path; !strings.Contains(got, "/runs/r-1/attempts/") {
		t.Fatalf("--run ignored: %s", got)
	}
}

func TestReplayLocalRunsTheTaskWithItsOriginalInputs(t *testing.T) {
	py := sdkPython(t)
	h := newHarness(t)
	h.replyJSON(http.StatusOK, replayResponse)
	out := filepath.Join(t.TempDir(), "rp")
	code, _, stderr := h.run("replay", "22222222-2222-2222-2222-222222222222", "--local", "--out", out, "--python", py)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	result, err := os.ReadFile(filepath.Join(out, "work", "outputs", "result"))
	if err != nil || strings.TrimSpace(string(result)) != `{"v": 42}` {
		t.Fatalf("output = %q (%v)", result, err)
	}
	log, _ := os.ReadFile(filepath.Join(out, "work", "stdout.log"))
	if !strings.Contains(string(log), "key=idem-k") {
		t.Fatalf("the replay did not use the original idempotency key: %s", log)
	}
}

func TestReplayDenialIsReported(t *testing.T) {
	h := newHarness(t)
	h.replyJSON(http.StatusForbidden, `{"error":"forbidden","message":"Missing permission: tasks.results.view"}`)
	code, _, stderr := h.run("replay", "22222222-2222-2222-2222-222222222222")
	if code == 0 || !strings.Contains(stderr, "tasks.results.view") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
