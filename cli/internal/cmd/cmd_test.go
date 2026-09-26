// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// harness runs the CLI against a stub server and captures its output.
type harness struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex // guards requests against a command running in a goroutine
	requests []recorded
	handler  func(w http.ResponseWriter, r *http.Request) bool
	env      map[string]string
}

type recorded struct {
	Method  string
	Path    string
	Query   string
	Headers http.Header
	Body    []byte
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, env: map[string]string{"HOME": t.TempDir()}}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.requests = append(h.requests, recorded{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Headers: r.Header.Clone(), Body: body,
		})
		h.mu.Unlock()
		if h.handler != nil && h.handler(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(h.server.Close)
	h.env["NEXUS_ENDPOINT"] = h.server.URL
	return h
}

// run executes the CLI and returns exit code, stdout, and stderr.
func (h *harness) run(args ...string) (int, string, string) {
	h.t.Helper()
	var out, errb strings.Builder
	code := Execute(args, IO{Out: &out, Err: &errb, In: strings.NewReader("")}, func(k string) string {
		return h.env[k]
	})
	return code, out.String(), errb.String()
}

func (h *harness) last() recorded {
	h.t.Helper()
	if len(h.requests) == 0 {
		h.t.Fatal("no request was made")
	}
	return h.requests[len(h.requests)-1]
}

func (h *harness) replyJSON(status int, body string) {
	h.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
		return true
	}
}

// ── argument parsing ─────────────────────────────────────────────────────────

func TestUnknownCommandIsActionable(t *testing.T) {
	h := newHarness(t)
	code, _, errb := h.run("wrokflow", "submit")
	if code == 0 {
		t.Fatal("expected a non-zero exit")
	}
	if !strings.Contains(errb, "wrokflow") || !strings.Contains(errb, "nexusctl help") {
		t.Errorf("error should name the typo and point at help, got: %q", errb)
	}
}

func TestUnknownVerbListsAvailableOnes(t *testing.T) {
	h := newHarness(t)
	code, _, errb := h.run("run", "creat")
	if code == 0 {
		t.Fatal("expected a non-zero exit")
	}
	// Listing the real verbs turns a dead end into a one-step fix.
	for _, want := range []string{"creat", "create", "cancel", "retry"} {
		if !strings.Contains(errb, want) {
			t.Errorf("error should mention %q, got: %q", want, errb)
		}
	}
}

func TestNoArgsPrintsUsageAndSucceeds(t *testing.T) {
	h := newHarness(t)
	code, out, _ := h.run()
	if code != 0 {
		t.Errorf("bare invocation should exit 0, got %d", code)
	}
	for _, want := range []string{"workflow submit", "run create", "worker ls", "capacity"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage should list %q", want)
		}
	}
}

func TestGlobalFlagsWorkBeforeAndAfterTheVerb(t *testing.T) {
	// Users type the operand first constantly; a CLI that rejects
	// `run list --output json` feels broken.
	h := newHarness(t)
	h.replyJSON(200, `{"runs":[]}`)
	for _, args := range [][]string{
		{"--output", "json", "run", "list"},
		{"run", "list", "--output", "json"},
		{"run", "--output", "json", "list"},
		{"run", "list", "-o", "json"},
	} {
		code, out, errb := h.run(args...)
		if code != 0 {
			t.Errorf("%v exited %d: %s", args, code, errb)
			continue
		}
		if !strings.HasPrefix(strings.TrimSpace(out), "{") {
			t.Errorf("%v did not produce JSON: %q", args, out)
		}
	}
}

func TestOperandBeforeFlagsIsParsed(t *testing.T) {
	// Go's flag package stops at the first operand, so this would otherwise treat
	// --hash-only as a positional and report the wrong error.
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(path, []byte(
		"api_version: v1\nname: w\ntasks:\n  - {id: a, task_ref: r}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	code, out, errb := h.run("workflow", "compile", path, "--hash-only")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	if len(strings.TrimSpace(out)) != 64 {
		t.Errorf("want a bare hash, got %q", out)
	}
}

func TestMissingOperandGivesUsage(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"run", "get"},
		{"run", "cancel"},
		{"workflow", "submit"},
		{"logs"},
		{"events"},
	} {
		code, _, errb := h.run(args...)
		if code == 0 {
			t.Errorf("%v should fail without an operand", args)
		}
		if !strings.Contains(errb, "Usage:") {
			t.Errorf("%v error should include usage, got: %q", args, errb)
		}
	}
}

func TestTooManyOperandsIsRejected(t *testing.T) {
	h := newHarness(t)
	code, _, errb := h.run("run", "get", "run-1", "run-2")
	if code == 0 {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(errb, "exactly one") {
		t.Errorf("got: %q", errb)
	}
}

func TestBadOutputFormatIsRejected(t *testing.T) {
	h := newHarness(t)
	code, _, errb := h.run("version", "--output", "xml")
	if code == 0 {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(errb, "table, json, or yaml") {
		t.Errorf("got: %q", errb)
	}
}

func TestVersionWorksWithNoEndpointConfigured(t *testing.T) {
	// Config resolution is lazy precisely so this works on a machine with nothing
	// set up yet.
	var out, errb strings.Builder
	code := Execute([]string{"version"}, IO{Out: &out, Err: &errb}, func(string) string { return "" })
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "nexusctl") {
		t.Errorf("got %q", out.String())
	}
}

func TestHelpForASubcommand(t *testing.T) {
	h := newHarness(t)
	code, out, _ := h.run("help", "workflow", "compile")
	if code != 0 {
		t.Fatalf("exited %d", code)
	}
	// This command is ADR-0004's stated mitigation, so its help should say so.
	if !strings.Contains(out, "--show") {
		t.Errorf("help should document --show, got: %q", out)
	}
}

// ── run create: params and the idempotency key ───────────────────────────────

func TestRunCreateSendsIdempotencyKeyAsAHeader(t *testing.T) {
	// Per §8.2 it is a HEADER. In the body it would never reach the gateway's
	// deduplication, and "submit twice safely" would silently not work.
	h := newHarness(t)
	h.replyJSON(200, `{"id":"run-1","state":"PENDING"}`)
	code, _, errb := h.run("run", "create", "etl", "--idempotency-key", "daily-2026-09-12")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	req := h.last()
	if got := req.Headers.Get("Idempotency-Key"); got != "daily-2026-09-12" {
		t.Errorf("Idempotency-Key header = %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if _, present := body["idempotency_key"]; present {
		t.Error("the key must not also be in the body")
	}
}

func TestRunCreateOmitsTheHeaderWhenNoKeyIsGiven(t *testing.T) {
	h := newHarness(t)
	h.replyJSON(200, `{"id":"run-1"}`)
	if code, _, errb := h.run("run", "create", "etl"); code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	if got := h.last().Headers.Get("Idempotency-Key"); got != "" {
		t.Errorf("header should be absent, got %q", got)
	}
}

func TestRunCreateParamTypeInference(t *testing.T) {
	// `--param epochs=20` must bind an integer, or it would fail type validation
	// against an `int` input at submit time.
	h := newHarness(t)
	h.replyJSON(200, `{"id":"run-1"}`)
	code, _, errb := h.run("run", "create", "etl",
		"--param", "epochs=20",
		"--param", "rate=0.5",
		"--param", "enabled=true",
		"--param", "disabled=false",
		"--param", "nothing=null",
		"--param", `sources=["s3://a","s3://b"]`,
		"--param", `config={"k":"v"}`,
		"--param", "name=plain-string",
		"--param-string", "version=20",
	)
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	var body struct{ Params map[string]any }
	if err := json.Unmarshal(h.last().Body, &body); err != nil {
		t.Fatal(err)
	}
	p := body.Params
	if p["epochs"].(float64) != 20 {
		t.Errorf("epochs = %#v, want a number", p["epochs"])
	}
	if p["rate"].(float64) != 0.5 {
		t.Errorf("rate = %#v", p["rate"])
	}
	if p["enabled"] != true || p["disabled"] != false {
		t.Errorf("booleans not inferred: %#v %#v", p["enabled"], p["disabled"])
	}
	if p["nothing"] != nil {
		t.Errorf("nothing = %#v, want null", p["nothing"])
	}
	if list, ok := p["sources"].([]any); !ok || len(list) != 2 {
		t.Errorf("sources = %#v, want a 2-element list", p["sources"])
	}
	if obj, ok := p["config"].(map[string]any); !ok || obj["k"] != "v" {
		t.Errorf("config = %#v", p["config"])
	}
	if p["name"] != "plain-string" {
		t.Errorf("name = %#v", p["name"])
	}
	// --param-string forces a string even when the value looks numeric.
	if p["version"] != "20" {
		t.Errorf("version = %#v, want the string \"20\"", p["version"])
	}
}

func TestMalformedParamIsRejectedWithAnExample(t *testing.T) {
	h := newHarness(t)
	code, _, errb := h.run("run", "create", "etl", "--param", "no-equals-sign")
	if code == 0 {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(errb, "KEY=VALUE") || !strings.Contains(errb, "Example") {
		t.Errorf("error should show the right form, got: %q", errb)
	}
}

// ── error messages ───────────────────────────────────────────────────────────

func TestBare500BecomesActionable(t *testing.T) {
	// "an orchestrator CLI that prints a bare 500 is useless" -- the request id is
	// the thing that makes it reportable.
	h := newHarness(t)
	h.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("X-Request-Id", "req-abc123")
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"message":"scheduler panic"}`))
		return true
	}
	code, _, errb := h.run("run", "get", "run-1")
	if code == 0 {
		t.Fatal("expected a failure")
	}
	for _, want := range []string{"500", "scheduler panic", "req-abc123", "control-plane fault"} {
		if !strings.Contains(errb, want) {
			t.Errorf("error should contain %q, got:\n%s", want, errb)
		}
	}
}

func TestAuthFailureNamesTheFix(t *testing.T) {
	h := newHarness(t)
	h.replyJSON(401, `{"message":"invalid api key"}`)
	code, _, errb := h.run("capacity")
	if code == 0 {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(errb, "NEXUS_API_KEY") {
		t.Errorf("a 401 should name the env var, got:\n%s", errb)
	}
}

func TestValidationFailureSuggestsDryRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(path, []byte(
		"api_version: v1\nname: w\ntasks:\n  - {id: a, task_ref: r}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.replyJSON(422, `{"message":"spec invalid","details":{"tasks[0]":"cpu required"}}`)
	code, _, errb := h.run("workflow", "submit", path)
	if code == 0 {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(errb, "cpu required") {
		t.Errorf("server details should be surfaced, got:\n%s", errb)
	}
	if !strings.Contains(errb, "--dry-run") {
		t.Errorf("a 422 should suggest --dry-run, got:\n%s", errb)
	}
}

func TestConflictExplainsTheIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	h.replyJSON(409, `{"message":"key in use"}`)
	code, _, errb := h.run("run", "create", "etl", "--idempotency-key", "dup")
	if code == 0 {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(errb, "idempotency key") {
		t.Errorf("got:\n%s", errb)
	}
}

func TestUnreachableEndpointNamesTheEndpoint(t *testing.T) {
	var out, errb strings.Builder
	code := Execute([]string{"capacity"}, IO{Out: &out, Err: &errb}, func(k string) string {
		if k == "NEXUS_ENDPOINT" {
			return "http://127.0.0.1:1"
		}
		return ""
	})
	if code == 0 {
		t.Fatal("expected a failure")
	}
	s := errb.String()
	if !strings.Contains(s, "127.0.0.1:1") || !strings.Contains(s, "cannot reach") {
		t.Errorf("got:\n%s", s)
	}
}

func TestNonJSONResponseSuggestsTheWrongEndpoint(t *testing.T) {
	h := newHarness(t)
	h.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>nginx</html>"))
		return true
	}
	code, _, errb := h.run("capacity")
	if code == 0 {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(errb, "--endpoint") {
		t.Errorf("got:\n%s", errb)
	}
}

func TestLocalValidationFailsBeforeAnyRequest(t *testing.T) {
	// A missing task_ref should not need a network round trip to discover.
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("api_version: v1\nname: w\ntasks:\n  - {id: a}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	code, _, errb := h.run("workflow", "submit", path)
	if code == 0 {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(errb, "task_ref") {
		t.Errorf("got:\n%s", errb)
	}
	if len(h.requests) != 0 {
		t.Errorf("a locally-invalid spec should not be sent, but %d request(s) were made", len(h.requests))
	}
}

// ── never print key material ─────────────────────────────────────────────────

func TestAPIKeyIsNeverPrintedInAnyOutput(t *testing.T) {
	const secret = "nx_live_SECRET_KEY_MATERIAL_zzz"
	h := newHarness(t)
	h.env["NEXUS_API_KEY"] = secret

	for _, args := range [][]string{
		{"config", "view"},
		{"config", "view", "--output", "json"},
		{"config", "view", "--output", "yaml"},
		{"capacity"},
		{"run", "list"},
	} {
		h.replyJSON(200, `{}`)
		_, out, errb := h.run(args...)
		// Output reaches terminals, CI logs, and tickets. A key in any of those is
		// a leaked key, so this holds for every format including JSON.
		if strings.Contains(out, secret) {
			t.Errorf("%v leaked the api key on stdout", args)
		}
		if strings.Contains(errb, secret) {
			t.Errorf("%v leaked the api key on stderr", args)
		}
		if strings.Contains(out+errb, "SECRET_KEY_MATERIAL") {
			t.Errorf("%v leaked part of the api key", args)
		}
	}
}

func TestConfigViewStillReportsWhetherAKeyIsSet(t *testing.T) {
	// Redaction must not make an auth problem undiagnosable.
	h := newHarness(t)
	h.env["NEXUS_API_KEY"] = "some-key"
	_, out, _ := h.run("config", "view")
	if !strings.Contains(out, "api_key") || !strings.Contains(out, "set") {
		t.Errorf("config view should say a key is set, got:\n%s", out)
	}

	h2 := newHarness(t)
	_, out2, _ := h2.run("config", "view")
	if !strings.Contains(out2, "not set") {
		t.Errorf("config view should say no key is set, got:\n%s", out2)
	}
}

func TestAPIKeyIsSentAsABearerToken(t *testing.T) {
	h := newHarness(t)
	h.env["NEXUS_API_KEY"] = "k123"
	h.replyJSON(200, `{}`)
	if code, _, errb := h.run("capacity"); code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	if got := h.last().Headers.Get("Authorization"); got != "Bearer k123" {
		t.Errorf("Authorization = %q", got)
	}
}

// ── request shapes ───────────────────────────────────────────────────────────

func TestRequestPathsMatchTheDocumentedAPI(t *testing.T) {
	// Paths per architecture §8.2. Getting one wrong is a 404 the user cannot fix.
	h := newHarness(t)
	h.env["NEXUS_NAMESPACE"] = "prod"
	cases := []struct {
		args   []string
		method string
		path   string
	}{
		{[]string{"run", "get", "r1"}, "GET", "/v1/namespaces/prod/runs/r1"},
		{[]string{"run", "list"}, "GET", "/v1/namespaces/prod/runs"},
		{[]string{"run", "create", "w"}, "POST", "/v1/namespaces/prod/runs"},
		{[]string{"run", "cancel", "r1"}, "POST", "/v1/namespaces/prod/runs/r1/cancel"},
		{[]string{"run", "retry", "r1"}, "POST", "/v1/namespaces/prod/runs/r1/retry"},
		{[]string{"worker", "ls"}, "GET", "/v1/namespaces/prod/workers"},
		{[]string{"capacity"}, "GET", "/v1/namespaces/prod/capacity"},
		{[]string{"events", "r1"}, "GET", "/v1/namespaces/prod/runs/r1/events"},
		{[]string{"logs", "a1"}, "GET", "/v1/namespaces/prod/attempts/a1/logs"},
		{[]string{"workflow", "versions", "w"}, "GET", "/v1/namespaces/prod/specs/w/versions"},
	}
	for _, tc := range cases {
		h.replyJSON(200, `{}`)
		if code, _, errb := h.run(tc.args...); code != 0 {
			t.Errorf("%v exited %d: %s", tc.args, code, errb)
			continue
		}
		req := h.last()
		if req.Method != tc.method || req.Path != tc.path {
			t.Errorf("%v -> %s %s, want %s %s", tc.args, req.Method, req.Path, tc.method, tc.path)
		}
	}
}

func TestDryRunIsAQueryParameterAndNothingIsRegistered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(path, []byte(
		"api_version: v1\nname: w\ntasks:\n  - {id: a, task_ref: r}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.replyJSON(200, `{"valid":true}`)
	code, out, errb := h.run("workflow", "submit", path, "--dry-run")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	if !strings.Contains(h.last().Query, "dry_run=true") {
		t.Errorf("query = %q", h.last().Query)
	}
	if !strings.Contains(out, "Dry run") {
		t.Errorf("output should say nothing was registered, got:\n%s", out)
	}
}

func TestSubmitSendsCanonicalBytesMatchingTheReportedHash(t *testing.T) {
	// The server content-addresses what it receives, so the bytes on the wire must
	// be the same bytes we hashed and told the user about.
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(path, []byte(
		"name: w\napi_version: v1\ntasks:\n  - {task_ref: r, id: a}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.replyJSON(200, `{"version":1}`)
	code, out, errb := h.run("workflow", "submit", path)
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	sent := string(h.last().Body)
	// Canonical: sorted keys, no whitespace.
	if strings.Contains(sent, " ") || strings.Contains(sent, "\n") {
		t.Errorf("body is not canonical: %q", sent)
	}
	if !strings.HasPrefix(sent, `{"api_version"`) {
		t.Errorf("keys not sorted: %q", sent)
	}
	if !strings.Contains(out, "local spec hash") {
		t.Errorf("submit should report the hash, got:\n%s", out)
	}
}

func TestHashMismatchWithTheServerIsLoud(t *testing.T) {
	// A divergence means the two canonicalizers disagree, which would split one
	// workflow into two spec versions. Never silent.
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(path, []byte(
		"api_version: v1\nname: w\ntasks:\n  - {id: a, task_ref: r}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.replyJSON(200, `{"version":1,"spec_hash":"0000000000000000000000000000000000000000000000000000000000000000"}`)
	code, _, errb := h.run("workflow", "submit", path)
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	if !strings.Contains(errb, "WARNING") || !strings.Contains(errb, "canonicalization") {
		t.Errorf("a hash mismatch must warn loudly, got:\n%s", errb)
	}
}

func TestFiltersBecomeQueryParameters(t *testing.T) {
	h := newHarness(t)
	h.replyJSON(200, `{"runs":[]}`)
	if code, _, errb := h.run("run", "list", "--state", "running", "--spec", "etl", "--limit", "5"); code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	q := h.last().Query
	// State is upper-cased to the canonical enum spelling, so `--state running`
	// works as well as `--state RUNNING`.
	for _, want := range []string{"state=RUNNING", "spec=etl", "limit=5"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q missing %q", q, want)
		}
	}
}

func TestFollowFlagsBecomeQueryParameters(t *testing.T) {
	h := newHarness(t)
	h.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("line one\n"))
		return true
	}
	if code, out, errb := h.run("logs", "a1", "--follow"); code != 0 {
		t.Fatalf("exited %d: %s (%s)", code, errb, out)
	}
	if !strings.Contains(h.last().Query, "follow=true") {
		t.Errorf("query = %q", h.last().Query)
	}

	h2 := newHarness(t)
	h2.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"seq\":1,\"type\":\"RunStarted\"}\n\n"))
		return true
	}
	if code, _, errb := h2.run("events", "r1", "-f"); code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	if !strings.Contains(h2.last().Query, "watch=true") {
		t.Errorf("query = %q", h2.last().Query)
	}
}

func TestEventsFollowEmitsJSONLinesNotAnArray(t *testing.T) {
	// A single enclosing array could never be closed on a live stream, so a
	// consumer would have to buffer forever before parsing anything.
	h := newHarness(t)
	h.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"seq\":1,\"type\":\"RunStarted\"}\n\n" +
				": keep-alive\n\n" +
				"data: {\"seq\":2,\"type\":\"NodeSucceeded\"}\n\n"))
		return true
	}
	code, out, errb := h.run("events", "r1", "--follow", "--output", "json")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 JSON lines, got %d: %q", len(lines), out)
	}
	for i, line := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Errorf("line %d is not valid JSON: %v", i, err)
		}
	}
}

// TestLogsTailAndStreamFlags: the durable-log options reach the gateway as the
// query parameters it serves (ADR-0020), and the CLI asks for raw text.
func TestLogsTailAndStreamFlags(t *testing.T) {
	h := newHarness(t)
	var accept string
	h.handler = func(w http.ResponseWriter, r *http.Request) bool {
		accept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("[nexus] 3 lines were omitted by the per-attempt log cap\nlast\n"))
		return true
	}
	code, out, errb := h.run("logs", "a1", "--tail", "50", "--stream", "stderr", "-f")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	q := h.last().Query
	for _, want := range []string{"tail=50", "stream_name=stderr", "follow=true"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q missing %q", q, want)
		}
	}
	if !strings.Contains(accept, "text/plain") {
		t.Errorf("Accept = %q; the gateway streams raw lines only for text/plain", accept)
	}
	if !strings.Contains(out, "[nexus] 3 lines were omitted") {
		t.Errorf("platform markers must be printed, not filtered: %q", out)
	}

	if code, _, _ := newHarness(t).run("logs", "a1", "--stream", "bogus"); code == 0 {
		t.Error("--stream bogus was accepted")
	}
}

func TestLogsAreStreamedVerbatim(t *testing.T) {
	// A log line is evidence; re-encoding it would corrupt what the task printed.
	h := newHarness(t)
	h.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("plain line\n{\"json\":\"line\"}\n  indented\n"))
		return true
	}
	code, out, errb := h.run("logs", "a1")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	for _, want := range []string{"plain line", `{"json":"line"}`, "  indented"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q verbatim, got:\n%s", want, out)
		}
	}
}

func TestRunGetRendersNodeStates(t *testing.T) {
	h := newHarness(t)
	h.replyJSON(200, `{
      "id":"run-1","spec_name":"etl","state":"FAILED",
      "nodes":[
        {"node_id":"extract","state":"SUCCEEDED","attempt_count":1},
        {"node_id":"transform","state":"FAILED","attempt_count":3,
         "error":{"class":"ERROR_CLASS_APPLICATION","message":"boom"}},
        {"node_id":"load","state":"UPSTREAM_FAILED","attempt_count":0}
      ]}`)
	code, out, errb := h.run("run", "get", "run-1")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	// FAILED and UPSTREAM_FAILED must stay distinguishable: that is what makes
	// "what actually broke" answerable at a glance.
	for _, want := range []string{"extract", "succeeded", "transform", "failed", "upstream_failed", "boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q, got:\n%s", want, out)
		}
	}
	// A failed run should say what to do next.
	if !strings.Contains(errb, "run retry") {
		t.Errorf("stderr should suggest a retry, got:\n%s", errb)
	}
}

// The gateway's real run document (api/openapi.yaml): node state and
// attempts_used on the node, everything about an execution on its attempts.
// The table used to read attempt_count, worker and duration_ms off the node,
// fields the gateway never sends, so every one printed "-" and the attempt id
// that `nexusctl logs` needs was nowhere on screen.
func TestRunGetShowsTheLatestAttemptFromTheGatewayDocument(t *testing.T) {
	h := newHarness(t)
	h.replyJSON(200, `{
      "id":"run-1","spec_name":"etl","state":"FAILED",
      "nodes":[
        {"node_id":"extract","state":"SUCCEEDED","attempts_used":1,"max_attempts":4},
        {"node_id":"load","state":"FAILED","attempts_used":2,"max_attempts":2}
      ],
      "attempts":[
        {"id":"att-e1","node_id":"extract","attempt_number":1,"state":"SUCCEEDED",
         "worker_hostname":"agent-cpu-1","started_at":"2026-09-26T06:54:00Z","ended_at":"2026-09-26T06:54:01.5Z"},
        {"id":"att-l1","node_id":"load","attempt_number":1,"state":"FAILED",
         "error":{"class":"ERROR_CLASS_TRANSIENT","message":"reset"}},
        {"id":"att-l2","node_id":"load","attempt_number":2,"state":"FAILED",
         "error":{"class":"ERROR_CLASS_APPLICATION","message":"boom"}}
      ]}`)
	code, out, errb := h.run("run", "get", "run-1")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	for _, want := range []string{"LAST ATTEMPT", "1/4", "att-e1", "agent-cpu-1", " 1s ", "2/2", "att-l2", "application: boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "att-l1") || strings.Contains(out, "reset") {
		t.Errorf("an earlier attempt leaked into the row, got:\n%s", out)
	}
}

func TestEventsTableReadsKindTimeAndPayload(t *testing.T) {
	h := newHarness(t)
	h.replyJSON(200, `{"events":[
      {"seq":6,"kind":"AttemptFailed","node_id":"load","occurred_at":"2026-09-26T06:54:02.123Z",
       "payload":{"state":"FAILED","error_class":"ERROR_CLASS_APPLICATION"}},
      {"seq":7,"kind":"RetryExhausted","node_id":"load","occurred_at":"2026-09-26T06:54:02.2Z",
       "payload":{"reason":"error_class_not_in_retry_on"}}]}`)
	code, out, errb := h.run("events", "run-1")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	for _, want := range []string{"AttemptFailed", "06:54:02.123", "RetryExhausted", "error_class_not_in_retry_on"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q, got:\n%s", want, out)
		}
	}
}

func TestEmptyListingExplainsRatherThanShowingABareHeader(t *testing.T) {
	h := newHarness(t)
	h.env["NEXUS_NAMESPACE"] = "prod"
	h.replyJSON(200, `{"workers":[]}`)
	code, out, errb := h.run("worker", "ls")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	if !strings.Contains(out, "No workers registered") || !strings.Contains(out, "prod") {
		t.Errorf("got:\n%s", out)
	}
	// Explaining the consequence is the actionable part.
	if !strings.Contains(out, "queue") {
		t.Errorf("should explain what happens with no workers, got:\n%s", out)
	}
}

// TestWorkerListShowsCapacity: `worker ls` renders committed/advertised from
// the fields the gateway actually sends (rest.workerPayload: "capacity",
// "committed", "active_attempts"). It read "advertised" and
// "running_attempts", which the worker listing never had, so the cpu, memory,
// gpu and running columns were always "-".
func TestWorkerListShowsCapacity(t *testing.T) {
	h := newHarness(t)
	h.env["NEXUS_NAMESPACE"] = "prod"
	h.replyJSON(200, `{"workers":[{
		"id":"w-1","hostname":"node-a","queues":["default"],"executors":["subprocess"],
		"state":"ACTIVE","labels":{},
		"capacity":{"cpu_millis":8000,"memory_bytes":17179869184,"gpu_count":2,"gpu_kind":"","gpu_memory_bytes":0,"disk_bytes":0,"custom":null},
		"committed":{"cpu_millis":2000,"memory_bytes":4294967296,"gpu_count":1},
		"agent_version":"1.4.0","registered_at":"2026-09-25T10:00:00Z",
		"last_heartbeat_at":"2026-09-25T10:00:00Z","active_attempts":3}]}`)
	code, out, errb := h.run("worker", "ls")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	var row string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "w-1") {
			row = l
		}
	}
	fields := strings.Fields(row)
	if len(fields) < 7 {
		t.Fatalf("no worker row in:\n%s", out)
	}
	cpu, mem, gpu, running := fields[3], fields[4], fields[5], fields[6]
	if cpu == "-" || mem == "-" || gpu == "-" || running == "-" {
		t.Fatalf("capacity columns are blank: cpu=%s mem=%s gpu=%s running=%s\n%s", cpu, mem, gpu, running, out)
	}
	if gpu != "1/2" || running != "3" {
		t.Errorf("gpu=%s running=%s, want 1/2 and 3\n%s", gpu, running, out)
	}
	if !strings.Contains(cpu, "/") || !strings.Contains(mem, "/") {
		t.Errorf("cpu=%s mem=%s, want committed/advertised\n%s", cpu, mem, out)
	}
}

func TestProgressNotesGoToStderrSoJSONStaysParseable(t *testing.T) {
	// `nexusctl run get x -o json | jq` must work, which means nothing decorative
	// may ever reach stdout.
	h := newHarness(t)
	h.replyJSON(200, `{"id":"run-1","state":"FAILED","nodes":[]}`)
	code, out, _ := h.run("run", "get", "run-1", "--output", "json")
	if code != 0 {
		t.Fatalf("exited %d", code)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("stdout is not pure JSON: %v\n%s", err, out)
	}
}

func TestCompileDoesNotContactTheServer(t *testing.T) {
	// compile is a local operation by design: it is what you reach for when you
	// want to see the document before anything is registered.
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(path, []byte(
		"api_version: v1\nname: w\ntasks:\n  - {id: a, task_ref: r}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	if code, _, errb := h.run("workflow", "compile", path, "--show"); code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	if len(h.requests) != 0 {
		t.Errorf("compile made %d request(s); it must be local", len(h.requests))
	}
}

func TestCompileShowPrintsTheDocument(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(path, []byte(
		"api_version: v1\nname: w\ntasks:\n  - {id: a, task_ref: r.a}\n"+
			"  - {id: b, task_ref: r.b}\nedges:\n  - {from_node_id: a, to_node_id: b}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	_, out, _ := h.run("workflow", "compile", path, "--show")
	for _, want := range []string{"Spec hash:", "Tasks:", "canonical WorkflowSpec", "r.a", "r.b"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q, got:\n%s", want, out)
		}
	}
	// Without --show the user gets a summary plus a pointer, not the whole document.
	_, brief, _ := h.run("workflow", "compile", path)
	if strings.Contains(brief, "canonical WorkflowSpec") {
		t.Error("the document should only print with --show")
	}
	if !strings.Contains(brief, "--show") {
		t.Error("the summary should mention --show")
	}
}

func TestCompileStdin(t *testing.T) {
	var out, errb strings.Builder
	code := Execute(
		[]string{"workflow", "compile", "-", "--hash-only"},
		IO{
			Out: &out, Err: &errb,
			In: strings.NewReader(`{"api_version":"v1","name":"w","tasks":[{"id":"a","task_ref":"r"}]}`),
		},
		func(string) string { return "" },
	)
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb.String())
	}
	if len(strings.TrimSpace(out.String())) != 64 {
		t.Errorf("want a hash, got %q", out.String())
	}
}

// ── run retry (CONTRACT-FINDINGS F100) ────────────────────────────────────────

// The --node selection must reach the gateway in the body. It always did; what
// was broken was the gateway ignoring it, so this pins the CLI half of the
// contract the gateway now honours.
func TestRunRetrySendsTheSelectedNodes(t *testing.T) {
	h := newHarness(t)
	h.env["NEXUS_NAMESPACE"] = "prod"
	h.replyJSON(200, `{"id":"r1","state":"RUNNING","retried_nodes":["load","transform"]}`)
	code, out, errb := h.run("run", "retry", "r1", "--node", "transform", "--node", "load")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	req := h.last()
	if req.Method != "POST" || req.Path != "/v1/namespaces/prod/runs/r1/retry" {
		t.Errorf("request = %s %s", req.Method, req.Path)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("body %q: %v", req.Body, err)
	}
	if got, _ := json.Marshal(body["nodes"]); string(got) != `["transform","load"]` {
		t.Errorf("body nodes = %v, want [transform load]", body["nodes"])
	}
	if !strings.Contains(out, "transform, load") {
		t.Errorf("output does not name the retried nodes: %q", out)
	}
}

func TestRunRetryWithoutNodesIsAWholeRunRetry(t *testing.T) {
	h := newHarness(t)
	h.replyJSON(200, `{"id":"r1","state":"RUNNING"}`)
	if code, _, errb := h.run("run", "retry", "r1"); code != 0 {
		t.Fatalf("exited %d: %s", code, errb)
	}
	var body map[string]any
	if err := json.Unmarshal(h.last().Body, &body); err != nil {
		t.Fatal(err)
	}
	if _, present := body["nodes"]; present {
		t.Errorf("a whole-run retry sent nodes: %v", body)
	}
}

// A refused selection is an error the operator can act on, not a silent no-op.
func TestRunRetryReportsARefusedSelection(t *testing.T) {
	h := newHarness(t)
	h.replyJSON(409, `{"error":"nodes_not_retryable","message":"only FAILED, UPSTREAM_FAILED and CANCELLED nodes can be retried; extract is SUCCEEDED","nodes":[{"node_id":"extract","state":"SUCCEEDED"}]}`)
	code, _, errb := h.run("run", "retry", "r1", "--node", "extract")
	if code == 0 {
		t.Fatal("a refused retry exited 0")
	}
	if !strings.Contains(errb, "extract is SUCCEEDED") {
		t.Errorf("stderr does not explain the refusal: %q", errb)
	}
}
