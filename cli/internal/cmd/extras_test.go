// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeSpec(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ── init ─────────────────────────────────────────────────────────────────────

func TestInitListsTemplates(t *testing.T) {
	h := newHarness(t)
	code, out, _ := h.run("init", "--list")
	for _, want := range []string{"etl", "ml-training", "batch-inference", "file-fanout", "--gpu"} {
		if !strings.Contains(out, want) {
			t.Errorf("--list missing %q:\n%s", want, out)
		}
	}
	if code != 0 {
		t.Errorf("exit %d", code)
	}
	_, out, _ = h.run("init", "--list", "-o", "json")
	var list []map[string]any
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list) != 4 {
		t.Errorf("json list: %v %q", err, out)
	}
}

func TestInitWritesASubstitutedProject(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(t.TempDir(), "Orders ETL")
	code, out, errb := h.run("init", "etl", dir, "--namespace", "finance")
	if code != 0 {
		t.Fatalf("init: %d %s", code, errb)
	}
	dag, err := os.ReadFile(filepath.Join(dir, "dag.py"))
	if err != nil {
		t.Fatal(err)
	}
	// The name is derived from the directory and made schema-valid.
	for _, want := range []string{`name="orders-etl"`, `namespace="finance"`, `IMAGE = "orders-etl:0.1.0"`} {
		if !strings.Contains(string(dag), want) {
			t.Errorf("dag.py missing %q", want)
		}
	}
	for _, f := range []string{"README.md", "requirements.txt", "tests/test_tasks.py", "Dockerfile", ".gitignore", "conftest.py"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if !strings.Contains(out, "nexusctl workflow compile dag.py --show") {
		t.Errorf("next steps missing: %q", out)
	}
	if len(h.requests) != 0 {
		t.Error("init must not contact the control plane")
	}
}

func TestInitRefusesToClobber(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	mine := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(mine, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errb := h.run("init", "file-fanout", dir, "--name", "files")
	if code != ExitUsage || !strings.Contains(errb, "not empty") {
		t.Fatalf("non-empty dir: %d %q", code, errb)
	}
	if _, err := os.Stat(filepath.Join(dir, "dag.py")); err == nil {
		t.Fatal("wrote into a non-empty directory without --force")
	}
	code, out, _ := h.run("init", "file-fanout", dir, "--name", "files", "--force", "-o", "json")
	if code != 0 {
		t.Fatalf("--force: exit %d", code)
	}
	if b, _ := os.ReadFile(mine); string(b) != "keep me" {
		t.Error("--force touched a file the template does not own")
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil || res["name"] != "files" || res["template"] != "file-fanout" {
		t.Errorf("json result: %v %q", err, out)
	}
}

func TestInitRejectsBadInvocations(t *testing.T) {
	h := newHarness(t)
	base := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"init", "nope", filepath.Join(base, "a")},
		{"init", "etl", filepath.Join(base, "b"), "--gpu"},
		{"init", "etl", filepath.Join(base, "c"), "--name", "Bad Name"},
		{"init", "etl", filepath.Join(base, "d"), "extra"},
	} {
		if code, _, errb := h.run(args...); code != ExitUsage {
			t.Errorf("%v: exit %d %q", args, code, errb)
		}
	}
	code, _, _ := h.run("init", "batch-inference", filepath.Join(base, "gpu"), "--gpu")
	dag, _ := os.ReadFile(filepath.Join(base, "gpu", "dag.py"))
	if code != 0 || !strings.Contains(string(dag), "USE_GPU = True") {
		t.Errorf("--gpu on batch-inference: exit %d", code)
	}
}

func TestWorkflowNameFromDirectory(t *testing.T) {
	for in, want := range map[string]string{
		"orders":             "orders",
		"./My Project/":      "my-project",
		"/tmp/x/__weird__..": "weird",
		"data.pipeline_v2":   "data.pipeline_v2",
		"UPPER--case":        "upper--case",
	} {
		if got := workflowNameFrom(in); got != want {
			t.Errorf("workflowNameFrom(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── completion ───────────────────────────────────────────────────────────────

func TestCompletionScripts(t *testing.T) {
	h := newHarness(t)
	for _, shell := range []string{"bash", "zsh", "fish"} {
		code, out, _ := h.run("completion", shell)
		if code != 0 || !strings.Contains(out, "nexusctl __complete") {
			t.Errorf("%s: exit %d, script %q", shell, code, out)
		}
	}
	if code, _, errb := h.run("completion", "powershell"); code != ExitUsage || !strings.Contains(errb, "bash, zsh or fish") {
		t.Errorf("unsupported shell: %d %q", code, errb)
	}
	if code, _, _ := h.run("completion"); code != ExitUsage {
		t.Errorf("no shell: exit %d", code)
	}
}

func TestCompleteWalksTheRealCommandTree(t *testing.T) {
	cases := []struct {
		words []string
		want  []string // must be present
		not   []string // must be absent
	}{
		{[]string{""}, []string{"workflow", "schedule", "backfill", "init", "completion", "capacity"}, []string{"__complete"}},
		{[]string{"sch"}, []string{"schedule"}, []string{"run"}},
		{[]string{"schedule", ""}, []string{"create", "preview", "enable"}, []string{"list-all"}},
		{[]string{"--output", "json", "run", "p"}, []string{"pause"}, []string{"list"}},
		{[]string{"schedule", "create", "--"}, []string{"--cron", "--workflow", "--overlap", "--namespace"}, nil},
		{[]string{"schedule", "create", "x", "--overlap", ""}, []string{"skip", "allow", "queue_one"}, nil},
		{[]string{"run", "list", "-o", "y"}, []string{"yaml"}, []string{"json"}},
		{[]string{"run", "list", "--state", "RUN"}, []string{"RUNNING"}, nil},
		{[]string{"init", ""}, []string{"etl", "batch-inference"}, nil},
		{[]string{"init", "etl", ""}, nil, []string{"etl"}},
		{[]string{"completion", "z"}, []string{"zsh"}, nil},
		{[]string{"workflow", "submit", "--w"}, []string{"--watch", "--workflow"}, nil},
		{[]string{"help", "back"}, []string{"backfill"}, nil},
		{[]string{"worker", "quarantine", "w-1", "--"}, []string{"--reason", "--for", "--yes"}, nil},
	}
	for _, c := range cases {
		got := complete(c.words)
		for _, w := range c.want {
			if !slices.Contains(got, w) {
				t.Errorf("complete(%q) = %v, missing %q", c.words, got, w)
			}
		}
		for _, w := range c.not {
			if slices.Contains(got, w) {
				t.Errorf("complete(%q) = %v, should not contain %q", c.words, got, w)
			}
		}
	}
}

// Completion introspects every command's flags by running it with -h. That must
// never reach the network, for any command.
func TestFlagIntrospectionNeverContactsTheServer(t *testing.T) {
	h := newHarness(t)
	t.Setenv("NEXUS_ENDPOINT", h.server.URL)
	var all []*command
	all = append(all, topLevel()...)
	for _, g := range groups() {
		all = append(all, g.commands...)
	}
	for _, c := range all {
		fs := commandFlagSet(c)
		if fs.Lookup("output") == nil {
			t.Errorf("%s: global flags missing from the introspected set", c.name)
		}
	}
	if len(h.requests) != 0 {
		t.Errorf("flag introspection made %d request(s)", len(h.requests))
	}
	if fs := commandFlagSet(scheduleCreateCmd()); fs.Lookup("cron") == nil || fs.Lookup("disabled") == nil {
		t.Error("schedule create's own flags were not introspected")
	}
}

func TestHiddenCommandIsUsableFromTheCLI(t *testing.T) {
	h := newHarness(t)
	code, out, _ := h.run("__complete", "backfill", "")
	if code != 0 || !strings.Contains(out, "start\n") {
		t.Errorf("__complete: %d %q", code, out)
	}
	_, usage, _ := h.run("help")
	if strings.Contains(usage, "__complete") {
		t.Error("the completion engine should not be listed in help")
	}
}

// ── workflow submit --watch ──────────────────────────────────────────────────

const watchSpecA = "api_version: v1\nname: w\ntasks:\n  - {id: a, task_ref: r}\n"
const watchSpecB = "api_version: v1\nname: w\ntasks:\n  - {id: a, task_ref: r}\n  - {id: b, task_ref: r}\n"

// syncBuf is a goroutine-safe writer: the watcher writes while the test reads.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.requests)
}

func startWatch(t *testing.T, h *harness, o watchOptions, format string) (*syncBuf, *syncBuf, context.CancelFunc, chan error) {
	t.Helper()
	out, errb := &syncBuf{}, &syncBuf{}
	env := &Environment{
		IO:      IO{Out: out, Err: errb, In: strings.NewReader("")},
		Env:     func(k string) string { return h.env[k] },
		Globals: GlobalFlags{Output: format},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- watchAndSubmit(ctx, env, o) }()
	return out, errb, cancel, done
}

func TestWatchResubmitsOnChangeAndSurvivesErrors(t *testing.T) {
	h := newHarness(t)
	h.replyJSON(201, `{"version":7,"created":true}`)
	path := writeSpec(t, watchSpecA)
	out, errb, _, done := startWatch(t, h, watchOptions{
		path: path, python: "python3", interval: 5 * time.Millisecond, debounce: 20 * time.Millisecond, stopAfter: 4,
	}, "")

	waitFor(t, "the initial submit", func() bool { return h.count() == 1 })

	// A broken save is reported and does not end the watch.
	if err := os.WriteFile(path, []byte("tasks: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the invalid report", func() bool { return strings.Contains(errb.String(), "INVALID") })

	// A real change is submitted, with its new content address.
	if err := os.WriteFile(path, []byte(watchSpecB), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second submit", func() bool { return h.count() == 2 })

	// A comment-only edit compiles to the same spec: reported, not re-sent.
	if err := os.WriteFile(path, []byte("# just a comment\n"+watchSpecB), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not finish")
	}
	if h.count() != 2 {
		t.Errorf("the comment-only edit was re-sent: %d requests", h.count())
	}
	text := out.String()
	if strings.Count(text, "submitted w version 7 (new version)") != 2 || !strings.Contains(text, "no change to the compiled spec") {
		t.Errorf("unexpected watch output:\n%s", text)
	}
	h.mu.Lock()
	first, second := string(h.requests[0].Body), string(h.requests[1].Body)
	h.mu.Unlock()
	if first == second || !strings.Contains(second, `"id":"b"`) {
		t.Errorf("the second submit did not carry the edited spec")
	}
}

func TestWatchDebouncesABurstOfSaves(t *testing.T) {
	h := newHarness(t)
	path := writeSpec(t, watchSpecA)
	_, _, cancel, done := startWatch(t, h, watchOptions{
		path: path, python: "python3", interval: 5 * time.Millisecond, debounce: 150 * time.Millisecond,
	}, "")
	waitFor(t, "the initial submit", func() bool { return h.count() == 1 })
	// Five saves 20ms apart are one edit to the author and must be one submit.
	for i := 0; i < 5; i++ {
		content := watchSpecB + "  - {id: c" + string(rune('0'+i)) + ", task_ref: r}\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, "the debounced submit", func() bool { return h.count() == 2 })
	time.Sleep(300 * time.Millisecond)
	if n := h.count(); n != 2 {
		t.Errorf("a burst of saves caused %d submits after the first", n-1)
	}
	h.mu.Lock()
	last := string(h.requests[len(h.requests)-1].Body)
	h.mu.Unlock()
	if !strings.Contains(last, `"id":"c4"`) {
		t.Errorf("the debounced submit did not carry the final save: %s", last)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Ctrl-C should end the watch cleanly, got %v", err)
	}
}

func TestWatchDryRunJSONLinesAndCleanStop(t *testing.T) {
	h := newHarness(t)
	h.replyJSON(200, `{"valid":true,"dry_run":true,"spec_hash":"abc123abc123abc123"}`)
	path := writeSpec(t, watchSpecA)
	out, errb, cancel, done := startWatch(t, h, watchOptions{
		path: path, python: "python3", dryRun: true, interval: 5 * time.Millisecond, debounce: 10 * time.Millisecond,
	}, "json")
	waitFor(t, "the initial validation", func() bool { return h.count() == 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watch returned %v", err)
	}
	h.mu.Lock()
	query := h.requests[0].Query
	h.mu.Unlock()
	if query != "dry_run=true" {
		t.Errorf("dry-run watch must send dry_run=true, sent %q", query)
	}
	var ev map[string]any
	line := strings.SplitN(strings.TrimSpace(out.String()), "\n", 2)[0]
	if err := json.Unmarshal([]byte(line), &ev); err != nil || ev["event"] != "validated" || ev["spec_hash"] != "abc123abc123abc123" {
		t.Errorf("JSON line: %v %q", err, out.String())
	}
	if strings.Contains(errb.String(), "Watching") {
		t.Error("human notes must not appear in JSON mode")
	}
}

func TestWatchRejectsStdinAndMissingFiles(t *testing.T) {
	h := newHarness(t)
	if code, _, errb := h.run("workflow", "submit", "-", "--watch"); code != ExitUsage || !strings.Contains(errb, "stdin") {
		t.Errorf("stdin: %d %q", code, errb)
	}
	if code, _, _ := h.run("workflow", "submit", filepath.Join(t.TempDir(), "gone.yaml"), "--watch"); code != ExitUsage {
		t.Errorf("missing file: exit %d", code)
	}
}

func TestWatchedFilesIncludePythonNeighbours(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"dag.py", "helpers.py", ".hidden.py", "notes.txt"} {
		_ = os.WriteFile(filepath.Join(dir, f), nil, 0o600)
	}
	got := watchedFiles(filepath.Join(dir, "dag.py"))
	want := []string{filepath.Join(dir, "dag.py"), filepath.Join(dir, "helpers.py")}
	if !slices.Equal(got, want) {
		t.Errorf("watchedFiles = %v, want %v", got, want)
	}
	if got := watchedFiles(filepath.Join(dir, "notes.txt")); len(got) != 1 {
		t.Errorf("a non-Python spec watches only itself, got %v", got)
	}
}
