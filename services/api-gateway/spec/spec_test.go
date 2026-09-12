// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalJSON is the smallest spec that passes both validation stages.
const minimalJSON = `{
  "api_version": "nexus.dev/v1",
  "namespace": "team-ml",
  "name": "train-pipeline",
  "tasks": [
    {"id": "extract", "task_ref": "etl.extract:v1",
     "outputs": {"dataset": {"type": "file_ref"}}},
    {"id": "train", "task_ref": "ml.train:v3",
     "inputs": {"dataset": {"type": "file_ref", "required": true}}}
  ],
  "edges": [
    {"from_node_id": "extract", "to_node_id": "train",
     "output_to_input": {"dataset": "dataset"}}
  ]
}`

// TestEmbeddedSchemaMatchesCanonical guards the one duplication in this package.
//
// go:embed cannot reach outside a package directory, so the schema exists twice:
// the canonical schemas/workflow-spec-v1.json and the copy compiled into the
// binary. A drift between them would mean the gateway publishes one grammar and
// enforces another, which is worse than publishing none, so it must fail the
// build rather than be discovered in production.
func TestEmbeddedSchemaMatchesCanonical(t *testing.T) {
	embedded, err := SchemaBytes()
	if err != nil {
		t.Fatalf("read embedded schema: %v", err)
	}
	canonicalPath := filepath.Join("..", "..", "..", "schemas", "workflow-spec-v1.json")
	onDisk, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("read %s: %v", canonicalPath, err)
	}
	if !bytes.Equal(embedded, onDisk) {
		t.Fatalf("embedded schema has drifted from %s; re-copy it", canonicalPath)
	}
}

func TestSchemaCompiles(t *testing.T) {
	if _, err := schema(); err != nil {
		t.Fatalf("embedded schema does not compile as draft 2020-12: %v", err)
	}
}

func TestCompileAcceptsMinimalSpec(t *testing.T) {
	c, err := Compile([]byte(minimalJSON), FormatJSON)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(c.Hash) != 64 {
		t.Errorf("content address is %d chars, want 64 hex chars: %q", len(c.Hash), c.Hash)
	}
	if c.Proto.GetName() != "train-pipeline" {
		t.Errorf("name = %q, want train-pipeline", c.Proto.GetName())
	}
	if got := len(c.Proto.GetTasks()); got != 2 {
		t.Fatalf("tasks = %d, want 2", got)
	}
}

// TestCanonicalizationIsStable is the core content-addressing guarantee: the same
// document, however it was formatted, must produce the same address. Without this
// a resubmit would create a redundant version and "byte-identical resubmit is a
// no-op" (ADR-0004) would be false.
func TestCanonicalizationIsStable(t *testing.T) {
	reordered := `{
  "name": "train-pipeline",
  "namespace": "team-ml",
  "tasks": [
    {"task_ref": "etl.extract:v1", "id": "extract",
     "outputs": {"dataset": {"type": "file_ref"}}},
    {"inputs": {"dataset": {"required": true, "type": "file_ref"}},
     "task_ref": "ml.train:v3", "id": "train"}
  ],
  "api_version": "nexus.dev/v1",
  "edges": [{"output_to_input": {"dataset": "dataset"},
             "to_node_id": "train", "from_node_id": "extract"}]
}`

	base, err := Compile([]byte(minimalJSON), FormatJSON)
	if err != nil {
		t.Fatalf("compile base: %v", err)
	}
	shuffled, err := Compile([]byte(reordered), FormatJSON)
	if err != nil {
		t.Fatalf("compile reordered: %v", err)
	}
	if base.Hash != shuffled.Hash {
		t.Errorf("key order changed the content address:\n base = %s\n shuf = %s\n base canonical = %s\n shuf canonical = %s",
			base.Hash, shuffled.Hash, base.Canonical, shuffled.Canonical)
	}

	// Whitespace must not matter either.
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(minimalJSON)); err != nil {
		t.Fatalf("compact: %v", err)
	}
	minified, err := Compile(compact.Bytes(), FormatJSON)
	if err != nil {
		t.Fatalf("compile minified: %v", err)
	}
	if minified.Hash != base.Hash {
		t.Errorf("whitespace changed the content address: %s vs %s", minified.Hash, base.Hash)
	}
}

// TestCanonicalizationIsDeterministicAcrossCalls catches the classic Go bug:
// relying on map iteration order. Go randomizes it per iteration, so a
// canonicalizer that forgot to sort keys passes on the first call and fails
// intermittently forever after.
func TestCanonicalizationIsDeterministicAcrossCalls(t *testing.T) {
	first, err := Compile([]byte(minimalJSON), FormatJSON)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := Compile([]byte(minimalJSON), FormatJSON)
		if err != nil {
			t.Fatalf("compile iteration %d: %v", i, err)
		}
		if again.Hash != first.Hash {
			t.Fatalf("iteration %d produced a different address: %s != %s", i, again.Hash, first.Hash)
		}
	}
}

// TestYAMLAndJSONAgree is ADR-0004's "neither surface is second-class" made
// testable: the same workflow authored in YAML and in JSON must be literally the
// same stored version, not merely an equivalent one.
func TestYAMLAndJSONAgree(t *testing.T) {
	yamlBody := `
api_version: nexus.dev/v1
namespace: team-ml
name: train-pipeline
tasks:
  - id: extract
    task_ref: etl.extract:v1
    outputs:
      dataset:
        type: file_ref
  - id: train
    task_ref: ml.train:v3
    inputs:
      dataset:
        type: file_ref
        required: true
edges:
  - from_node_id: extract
    to_node_id: train
    output_to_input:
      dataset: dataset
`
	fromJSON, err := Compile([]byte(minimalJSON), FormatJSON)
	if err != nil {
		t.Fatalf("compile json: %v", err)
	}
	fromYAML, err := Compile([]byte(yamlBody), FormatYAML)
	if err != nil {
		t.Fatalf("compile yaml: %v", err)
	}
	if fromJSON.Hash != fromYAML.Hash {
		t.Errorf("YAML and JSON of the same workflow produced different addresses:\n json = %s\n yaml = %s\n json canonical = %s\n yaml canonical = %s",
			fromJSON.Hash, fromYAML.Hash, fromJSON.Canonical, fromYAML.Canonical)
	}
}

// TestNumericNormalization pins the rule that 10 and 10.0 are one document. Two
// addresses for one workflow would silently double every stored spec version.
func TestNumericNormalization(t *testing.T) {
	withInt := strings.Replace(minimalJSON, `"name": "train-pipeline",`, `"name": "train-pipeline", "run_timeout_ms": 3600000,`, 1)
	withFloat := strings.Replace(minimalJSON, `"name": "train-pipeline",`, `"name": "train-pipeline", "run_timeout_ms": 3600000.0,`, 1)

	a, err := Compile([]byte(withInt), FormatJSON)
	if err != nil {
		t.Fatalf("compile int form: %v", err)
	}
	b, err := Compile([]byte(withFloat), FormatJSON)
	if err != nil {
		t.Fatalf("compile float form: %v", err)
	}
	if a.Hash != b.Hash {
		t.Errorf("3600000 and 3600000.0 produced different addresses: %s vs %s", a.Hash, b.Hash)
	}
}

// TestLargeIntegerSurvivesCanonicalization guards against float64 rounding.
// memory_bytes for a large-memory task can exceed 2^53, and silently rounding it
// would mis-schedule the task.
func TestLargeIntegerSurvivesCanonicalization(t *testing.T) {
	const big = 9007199254740993 // 2^53 + 1, not representable as float64
	body := `{"memory_bytes": 9007199254740993}`
	canonical, err := CanonicalizeJSON([]byte(body))
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if !strings.Contains(string(canonical), "9007199254740993") {
		t.Errorf("large integer was rounded: got %s, want it to contain %d", canonical, int64(big))
	}
}

func TestCanonicalDoesNotHTMLEscape(t *testing.T) {
	canonical, err := CanonicalizeJSON([]byte(`{"description":"a & b < c"}`))
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if strings.Contains(string(canonical), `\u0026`) {
		t.Errorf("canonical form HTML-escaped &, making the hash depend on an encoder flag: %s", canonical)
	}
	if want := `{"description":"a & b < c"}`; string(canonical) != want {
		t.Errorf("canonical = %s, want %s", canonical, want)
	}
}

func TestCanonicalRejectsTrailingContent(t *testing.T) {
	if _, err := CanonicalizeJSON([]byte(`{"a":1}{"b":2}`)); err == nil {
		t.Error("two concatenated documents must not silently canonicalize to the first")
	}
}

func TestStructuralRejections(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // a substring of the expected problem path or message
	}{
		{
			name: "wrong api_version",
			body: `{"api_version":"v2","namespace":"ns","name":"w","tasks":[{"id":"a","task_ref":"x"}]}`,
			want: "/api_version",
		},
		{
			name: "no tasks",
			body: `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w","tasks":[]}`,
			want: "/tasks",
		},
		{
			name: "unknown field rejected",
			body: `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w","tasks":[{"id":"a","task_ref":"x"}],"typo_field":1}`,
			want: "typo_field",
		},
		{
			name: "bad value type",
			body: `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w","tasks":[{"id":"a","task_ref":"x","outputs":{"o":{"type":"complex128"}}}]}`,
			want: "/tasks/0/outputs/o/type",
		},
		{
			name: "negative resource",
			body: `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w","tasks":[{"id":"a","task_ref":"x","resources":{"request":{"cpu_millis":-1}}}]}`,
			want: "cpu_millis",
		},
		{
			name: "docker without image",
			body: `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w","tasks":[{"id":"a","task_ref":"x","runtime":{"executor":"docker"}}]}`,
			want: "/tasks/0/runtime",
		},
		{
			name: "concurrency limit without key",
			body: `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w","tasks":[{"id":"a","task_ref":"x","concurrency_limit":3}]}`,
			want: "/tasks/0",
		},
		{
			name: "secret ref is not a provider uri",
			body: `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w","tasks":[{"id":"a","task_ref":"x","runtime":{"secrets":[{"name":"S","ref":"hunter2"}]}}]}`,
			want: "ref",
		},
		{
			name: "required input with a default is contradictory",
			body: `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w","tasks":[{"id":"a","task_ref":"x","inputs":{"i":{"type":"int","required":true,"default_json":"5"}}}]}`,
			want: "/tasks/0/inputs/i",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile([]byte(tc.body), FormatJSON)
			if err == nil {
				t.Fatal("expected rejection, got acceptance")
			}
			var vf *ValidationFailure
			if !errors.As(err, &vf) {
				t.Fatalf("error is %T, want *ValidationFailure: %v", err, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("problems do not mention %q:\n%v", tc.want, err)
			}
		})
	}
}

// TestSemanticRejections exercises stage two: problems a JSON Schema cannot
// express, because they are properties of the graph rather than of the document.
func TestSemanticRejections(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "cycle",
			body: `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w",
			  "tasks":[{"id":"a","task_ref":"x"},{"id":"b","task_ref":"y"}],
			  "edges":[{"from_node_id":"a","to_node_id":"b"},{"from_node_id":"b","to_node_id":"a"}]}`,
			want: "cycle",
		},
		{
			name: "edge to unknown node",
			body: `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w",
			  "tasks":[{"id":"a","task_ref":"x"}],
			  "edges":[{"from_node_id":"a","to_node_id":"ghost"}]}`,
			want: "unknown_edge_node",
		},
		{
			name: "type mismatch across an edge",
			body: `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w",
			  "tasks":[
			    {"id":"a","task_ref":"x","outputs":{"o":{"type":"string"}}},
			    {"id":"b","task_ref":"y","inputs":{"i":{"type":"int","required":true}}}],
			  "edges":[{"from_node_id":"a","to_node_id":"b","output_to_input":{"i":"o"}}]}`,
			want: "type_mismatch",
		},
		{
			name: "required input nothing satisfies",
			body: `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w",
			  "tasks":[{"id":"a","task_ref":"x","inputs":{"i":{"type":"int","required":true}}}]}`,
			want: "required_input_unsatisfied",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile([]byte(tc.body), FormatJSON)
			if err == nil {
				t.Fatal("expected rejection, got acceptance")
			}
			var vf *ValidationFailure
			if !errors.As(err, &vf) {
				t.Fatalf("error is %T, want *ValidationFailure: %v", err, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("problems do not mention %q:\n%v", tc.want, err)
			}
			for _, p := range vf.Problems {
				if p.Stage != "semantic" {
					continue
				}
				return
			}
			t.Errorf("no problem was attributed to the semantic stage: %+v", vf.Problems)
		})
	}
}

// TestProtoDefaultsResolved pins the two most consequential defaults. proto3
// cannot distinguish absent from zero, so an omitted field must be defaulted in
// conversion rather than left as UNSPECIFIED.
func TestProtoDefaultsResolved(t *testing.T) {
	c, err := Compile([]byte(minimalJSON), FormatJSON)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, task := range c.Proto.GetTasks() {
		if got := task.GetTriggerRule().String(); got != "TRIGGER_RULE_ALL_SUCCESS" {
			t.Errorf("task %s trigger_rule = %s, want ALL_SUCCESS by default", task.GetId(), got)
		}
		if got := task.GetIdempotency().String(); got != "IDEMPOTENCY_MODE_REQUIRED" {
			t.Errorf("task %s idempotency = %s, want REQUIRED by default", task.GetId(), got)
		}
		if got := task.GetQueue(); got != "default" {
			t.Errorf("task %s queue = %q, want \"default\"", task.GetId(), got)
		}
	}
}

// TestYAMLSurfaceQuirks covers the YAML-to-JSON conversions that would otherwise
// let a YAML author be held to different rules than a JSON author.
func TestYAMLSurfaceQuirks(t *testing.T) {
	t.Run("duplicate key rejected", func(t *testing.T) {
		_, err := Parse([]byte("a: 1\na: 2\n"), FormatYAML)
		if err == nil || !strings.Contains(err.Error(), "duplicate key") {
			t.Errorf("want duplicate-key rejection, got %v", err)
		}
	})

	t.Run("quoted version stays a string", func(t *testing.T) {
		v, err := Parse([]byte(`version: "1.0"`), FormatYAML)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		m := v.(map[string]any)
		if s, ok := m["version"].(string); !ok || s != "1.0" {
			t.Errorf("quoted 1.0 became %#v, want the string \"1.0\"", m["version"])
		}
	})

	t.Run("field named on stays a string key", func(t *testing.T) {
		v, err := Parse([]byte("on: value\nyes: other\n"), FormatYAML)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		m := v.(map[string]any)
		if _, ok := m["on"]; !ok {
			t.Errorf("key \"on\" was type-resolved away: %#v", m)
		}
		if _, ok := m["yes"]; !ok {
			t.Errorf("key \"yes\" was type-resolved away: %#v", m)
		}
	})

	t.Run("anchors expand to the effective document", func(t *testing.T) {
		body := `
base: &res
  cpu_millis: 1000
a: *res
b: *res
`
		v, err := Parse([]byte(body), FormatYAML)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		m := v.(map[string]any)
		for _, k := range []string{"a", "b"} {
			sub, ok := m[k].(map[string]any)
			if !ok {
				t.Fatalf("%s is %T, want the expanded mapping", k, m[k])
			}
			if sub["cpu_millis"] == nil {
				t.Errorf("%s did not expand the anchor: %#v", k, sub)
			}
		}
	})
}

func TestDetectFormat(t *testing.T) {
	cases := []struct {
		body        string
		contentType string
		want        Format
	}{
		{`{"a":1}`, "", FormatJSON},
		{"  \n\t{\"a\":1}", "", FormatJSON},
		{"a: 1", "", FormatYAML},
		{"a: 1", "application/json", FormatJSON},
		{`{"a":1}`, "application/yaml", FormatYAML},
	}
	for _, tc := range cases {
		if got := DetectFormat([]byte(tc.body), tc.contentType); got != tc.want {
			t.Errorf("DetectFormat(%q, %q) = %v, want %v", tc.body, tc.contentType, got, tc.want)
		}
	}
}

// TestTimeoutCoherenceRejections covers CONTRACT-FINDINGS.md F22 and its siblings:
// timeout combinations that are individually valid but jointly impossible. These fail
// SILENTLY at runtime rather than loudly, which is exactly why they must be caught at
// submit time.
func TestTimeoutCoherenceRejections(t *testing.T) {
	build := func(timeouts string) string {
		return `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w",
		  "tasks":[{"id":"a","task_ref":"x","timeouts":` + timeouts + `}]}`
	}

	cases := []struct {
		name     string
		timeouts string
		want     string
	}{
		{
			name:     "schedule_to_close below start_to_close",
			timeouts: `{"start_to_close_ms":14400000,"schedule_to_close_ms":3600000}`,
			want:     "not even one attempt can finish",
		},
		{
			name:     "schedule_to_close below schedule_to_start",
			timeouts: `{"schedule_to_start_ms":1800000,"schedule_to_close_ms":600000}`,
			want:     "queueing allowance exceeds the total budget",
		},
		{
			name:     "heartbeat longer than the attempt itself",
			timeouts: `{"start_to_close_ms":60000,"heartbeat_ms":300000}`,
			want:     "can never fire",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile([]byte(build(tc.timeouts)), FormatJSON)
			if err == nil {
				t.Fatal("an impossible timeout combination was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the rejection does not explain the problem (%q):\n%v", tc.want, err)
			}
		})
	}
}

// TestCoherentTimeoutsAccepted guards against the check being over-eager: a sensible
// four-timeout configuration from architecture §4.3 must pass.
func TestCoherentTimeoutsAccepted(t *testing.T) {
	body := `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w",
	  "tasks":[{"id":"a","task_ref":"x","timeouts":{
	    "schedule_to_start_ms":1800000,
	    "start_to_close_ms":14400000,
	    "schedule_to_close_ms":21600000,
	    "heartbeat_ms":300000}}]}`
	if _, err := Compile([]byte(body), FormatJSON); err != nil {
		t.Errorf("the §4.3 example timeout set was rejected: %v", err)
	}

	// Zero means "not enforced", so partial configurations must not trip the check.
	partial := `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w",
	  "tasks":[{"id":"a","task_ref":"x","timeouts":{"start_to_close_ms":60000}}]}`
	if _, err := Compile([]byte(partial), FormatJSON); err != nil {
		t.Errorf("a partial timeout set was rejected: %v", err)
	}
}

// TestErrorClassSpellingsBothAccepted covers F20: an author who copied the
// architecture §4.3 YAML example must not be rejected for the doc's own spelling.
func TestErrorClassSpellingsBothAccepted(t *testing.T) {
	for _, spelling := range []string{"validation", "validation_error"} {
		body := `{"api_version":"nexus.dev/v1","namespace":"ns","name":"w",
		  "tasks":[{"id":"a","task_ref":"x","retry":{"max_attempts":3,"no_retry_on":["` + spelling + `"]}}]}`
		c, err := Compile([]byte(body), FormatJSON)
		if err != nil {
			t.Fatalf("%q was rejected: %v", spelling, err)
		}
		got := c.Proto.GetTasks()[0].GetRetry().GetNoRetryOn()
		if len(got) != 1 || got[0].String() != "ERROR_CLASS_VALIDATION" {
			t.Errorf("%q normalized to %v, want ERROR_CLASS_VALIDATION", spelling, got)
		}
	}
}
