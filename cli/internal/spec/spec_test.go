// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parse(t *testing.T, body string) *Document {
	t.Helper()
	doc, err := Parse([]byte(body), "test")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return doc
}

// ── canonicalization ─────────────────────────────────────────────────────────

func TestJSONAndYAMLProduceTheIdenticalHash(t *testing.T) {
	// ADR-0004 requires that YAML authoring and Python authoring produce the
	// IDENTICAL document. If they canonicalized differently, the same logical
	// workflow would exist as two content-addressed spec versions, and "resubmit
	// is a no-op" would quietly stop being true.
	jsonBody := `{"api_version":"nexus.dev/v1","name":"w","tasks":[{"id":"a","task_ref":"r","version":1}]}`
	yamlBody := `
api_version: nexus.dev/v1
name: w
tasks:
  - id: a
    task_ref: r
    version: 1
`
	if a, b := parse(t, jsonBody).Hash, parse(t, yamlBody).Hash; a != b {
		t.Errorf("JSON and YAML disagree:\n  json: %s\n  yaml: %s", a, b)
	}
}

func TestKeyOrderDoesNotAffectTheHash(t *testing.T) {
	a := parse(t, `{"name":"w","api_version":"v1","tasks":[]}`)
	b := parse(t, `{"tasks":[],"api_version":"v1","name":"w"}`)
	if a.Hash != b.Hash {
		t.Errorf("key order changed the hash: %s vs %s", a.Hash, b.Hash)
	}
	if string(a.JSON) != string(b.JSON) {
		t.Errorf("canonical bytes differ:\n  %s\n  %s", a.JSON, b.JSON)
	}
}

func TestCanonicalJSONHasSortedKeysAndNoWhitespace(t *testing.T) {
	doc := parse(t, `{"z": 1, "a": {"n": 2, "m": 3}}`)
	if got := string(doc.JSON); got != `{"a":{"m":3,"n":2},"z":1}` {
		t.Errorf("canonical = %s", got)
	}
}

func TestIntegralFloatNormalizesToInt(t *testing.T) {
	// A multiplier written as 2 in YAML and 2.0 in Python must be one value, or
	// the same policy would register as two spec versions.
	if a, b := parse(t, `{"m": 2.0}`).Hash, parse(t, `{"m": 2}`).Hash; a != b {
		t.Errorf("2.0 and 2 hashed differently: %s vs %s", a, b)
	}
	if got := string(parse(t, `{"m": 2.0}`).JSON); got != `{"m":2}` {
		t.Errorf("got %s", got)
	}
}

func TestNonIntegralFloatIsPreserved(t *testing.T) {
	if got := string(parse(t, `{"m": 1.5}`).JSON); got != `{"m":1.5}` {
		t.Errorf("got %s", got)
	}
}

func TestNonASCIIIsNotEscaped(t *testing.T) {
	// Go's encoding/json would HTML-escape and \u-escape, producing different bytes
	// than Python's json.dumps(ensure_ascii=False) -- and therefore a different
	// content address for the same spec. That is the reason for the hand-written
	// encoder.
	doc := parse(t, `{"note": "café"}`)
	if got := string(doc.JSON); got != `{"note":"café"}` {
		t.Errorf("got %s, want the literal UTF-8", got)
	}
}

func TestHTMLCharactersAreNotEscaped(t *testing.T) {
	// encoding/json escapes < > & by default. A command containing them would then
	// hash differently in Go than in Python.
	doc := parse(t, `{"command": "a < b && c > d"}`)
	if !strings.Contains(string(doc.JSON), "a < b && c > d") {
		t.Errorf("HTML characters were escaped: %s", doc.JSON)
	}
}

func TestControlCharactersAreEscaped(t *testing.T) {
	doc := parse(t, `{"s": "a\nb\tc"}`)
	if got := string(doc.JSON); got != `{"s":"a\nb\tc"}` {
		t.Errorf("got %s", got)
	}
}

func TestListOrderIsPreserved(t *testing.T) {
	// argv order is meaningful; sorting it would change the program being run.
	doc := parse(t, `{"command": ["python", "-m", "train"]}`)
	if got := string(doc.JSON); got != `{"command":["python","-m","train"]}` {
		t.Errorf("got %s", got)
	}
}

func TestHashIsSHA256Hex(t *testing.T) {
	h := parse(t, `{"a":1}`).Hash
	if len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
		t.Errorf("hash = %q, want 64 hex chars", h)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for name, body := range map[string]string{
		"empty":          "",
		"whitespace":     "   \n  ",
		"broken json":    `{"a": }`,
		"top-level list": `[1,2,3]`,
		"scalar":         `42`,
	} {
		if _, err := Parse([]byte(body), "test"); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseNamesTheSourceInErrors(t *testing.T) {
	_, err := Parse([]byte(`{"a": }`), "pipeline.yaml")
	if err == nil || !strings.Contains(err.Error(), "pipeline.yaml") {
		t.Errorf("error should name the file, got: %v", err)
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	body := "api_version: nexus.dev/v1\nname: from-file\ntasks:\n  - {id: a, task_ref: r}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Name() != "from-file" {
		t.Errorf("Name = %q", doc.Name())
	}
}

func TestLoadMissingFileMentionsThePath(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), nil)
	if err == nil || !strings.Contains(err.Error(), "nope.yaml") {
		t.Errorf("error should name the path, got: %v", err)
	}
}

func TestIndentedIsNotWhatGetsHashed(t *testing.T) {
	// Pretty-printing changes the bytes, so it must never be the submitted form.
	doc := parse(t, `{"api_version":"v1","name":"w","tasks":[{"id":"a","task_ref":"r"}]}`)
	pretty, err := doc.Indented()
	if err != nil {
		t.Fatal(err)
	}
	if string(pretty) == string(doc.JSON) {
		t.Error("indented output should differ from the canonical bytes")
	}
	if !strings.Contains(string(pretty), "\n") {
		t.Error("indented output should be multi-line")
	}
}

func TestYAMLRoundTripsToTheSameHash(t *testing.T) {
	// `compile --output yaml` writes a file that must recompile to the same content
	// address, or the emitted YAML would be a different spec from its source.
	original := parse(t, `
api_version: nexus.dev/v1
name: round-trip
tasks:
  - id: a
    task_ref: r
    version: 1
    resources: {request: {cpu_millis: 2000, memory_bytes: 8589934592}}
edges:
  - {from_node_id: a, to_node_id: a2, output_to_input: {x: result}}
`)
	rendered, err := original.YAML()
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(rendered, "rendered.yaml")
	if err != nil {
		t.Fatalf("re-parsing our own YAML failed: %v", err)
	}
	if again.Hash != original.Hash {
		t.Errorf("YAML round trip changed the hash:\n  before: %s\n  after:  %s",
			original.Hash, again.Hash)
	}
}

// ── validation ───────────────────────────────────────────────────────────────

func TestValidateAcceptsAWellFormedSpec(t *testing.T) {
	doc := parse(t, `
api_version: nexus.dev/v1
name: good
tasks:
  - {id: a, task_ref: r.a}
  - {id: b, task_ref: r.b}
edges:
  - {from_node_id: a, to_node_id: b}
`)
	if problems := doc.Validate(); len(problems) != 0 {
		t.Errorf("unexpected problems: %v", problems)
	}
}

func TestValidateReportsMissingRequiredFields(t *testing.T) {
	doc := parse(t, `{"tasks":[{"id":"a"}]}`)
	problems := strings.Join(doc.Validate(), "; ")
	for _, want := range []string{"name", "api_version", "task_ref"} {
		if !strings.Contains(problems, want) {
			t.Errorf("problems should mention %q, got: %s", want, problems)
		}
	}
}

func TestValidateRejectsEmptyTasks(t *testing.T) {
	doc := parse(t, `{"api_version":"v1","name":"w","tasks":[]}`)
	problems := strings.Join(doc.Validate(), "; ")
	if !strings.Contains(problems, "at least one task") {
		t.Errorf("got: %s", problems)
	}
}

func TestValidateRejectsDuplicateTaskIDs(t *testing.T) {
	// node_id participates in the idempotency key, so two nodes sharing one id
	// would share one key -- not a cosmetic problem.
	doc := parse(t, `
api_version: v1
name: w
tasks:
  - {id: dup, task_ref: r}
  - {id: dup, task_ref: r2}
`)
	problems := strings.Join(doc.Validate(), "; ")
	if !strings.Contains(problems, `duplicate task id "dup"`) {
		t.Errorf("got: %s", problems)
	}
}

func TestValidateRejectsEdgesToUnknownNodes(t *testing.T) {
	doc := parse(t, `
api_version: v1
name: w
tasks:
  - {id: a, task_ref: r}
edges:
  - {from_node_id: a, to_node_id: ghost}
`)
	problems := strings.Join(doc.Validate(), "; ")
	if !strings.Contains(problems, "ghost") {
		t.Errorf("got: %s", problems)
	}
}

func TestValidateDetectsCycles(t *testing.T) {
	// The server checks this too, but catching it locally puts the error next to
	// the file being edited.
	doc := parse(t, `
api_version: v1
name: w
tasks:
  - {id: a, task_ref: r}
  - {id: b, task_ref: r}
  - {id: c, task_ref: r}
edges:
  - {from_node_id: a, to_node_id: b}
  - {from_node_id: b, to_node_id: c}
  - {from_node_id: c, to_node_id: a}
`)
	problems := strings.Join(doc.Validate(), "; ")
	if !strings.Contains(problems, "cycle") {
		t.Fatalf("a cycle was not detected: %s", problems)
	}
	for _, id := range []string{"a", "b", "c"} {
		if !strings.Contains(problems, id) {
			t.Errorf("cycle report should name %q, got: %s", id, problems)
		}
	}
}

func TestValidateAcceptsADiamond(t *testing.T) {
	// Fan-out plus fan-in is not a cycle; a naive check would call it one.
	doc := parse(t, `
api_version: v1
name: w
tasks:
  - {id: root, task_ref: r}
  - {id: left, task_ref: r}
  - {id: right, task_ref: r}
  - {id: join, task_ref: r}
edges:
  - {from_node_id: root, to_node_id: left}
  - {from_node_id: root, to_node_id: right}
  - {from_node_id: left, to_node_id: join}
  - {from_node_id: right, to_node_id: join}
`)
	if problems := doc.Validate(); len(problems) != 0 {
		t.Errorf("a diamond is acyclic: %v", problems)
	}
}

func TestIsPython(t *testing.T) {
	for path, want := range map[string]bool{
		"dag.py": true, "DAG.PY": true, "spec.yaml": false, "spec.json": false, "noext": false,
	} {
		if got := IsPython(path); got != want {
			t.Errorf("IsPython(%q) = %v, want %v", path, got, want)
		}
	}
}
