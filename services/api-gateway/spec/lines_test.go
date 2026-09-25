// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// A YAML author is told the LINE of every problem: a schema problem (whose path
// indexes tasks by position) and a semantic one (whose path names the task id)
// alike.
func TestValidationProblemsCarryYAMLLines(t *testing.T) {
	body := []byte(`api_version: nexus.dev/v1
namespace: default
name: lines
tasks:
  - id: a
    task_ref: t.a
    queue: NOT A QUEUE
  - id: b
    task_ref: t.b
    runtime:
      executor: subprocess
      command: ["true"]
      stdout_output: nope
`)
	_, err := Compile(body, FormatYAML)
	var vf *ValidationFailure
	if !errors.As(err, &vf) {
		t.Fatalf("want a validation failure, got %v", err)
	}
	byPath := map[string]Problem{}
	for _, p := range vf.Problems {
		byPath[p.Path] = p
	}
	q, ok := byPath["/tasks/0/queue"]
	if !ok || q.Line != 7 || q.Column != 12 {
		t.Fatalf("schema problem = %+v (all: %+v)", q, vf.Problems)
	}
	// The schema stage stops the pipeline, so resubmit with a valid queue to
	// reach the semantic stage.
	_, err = Compile([]byte(strings.Replace(string(body), "NOT A QUEUE", "default", 1)), FormatYAML)
	if !errors.As(err, &vf) {
		t.Fatalf("want a semantic failure, got %v", err)
	}
	s := vf.Problems[0]
	if s.Path != "/tasks/b/runtime/stdout_output" || s.Line != 13 || s.Column != 22 {
		t.Fatalf("semantic problem = %+v", s)
	}
	if !strings.Contains(s.String(), "line 13:22") {
		t.Errorf("String() = %q, want the line", s.String())
	}
	// Additive on the wire: present when known, absent otherwise.
	raw, _ := json.Marshal(s)
	if !strings.Contains(string(raw), `"line":13`) {
		t.Errorf("wire form %s lacks line", raw)
	}
	raw, _ = json.Marshal(Problem{Path: "", Message: "m", Stage: "parse"})
	if strings.Contains(string(raw), "line") {
		t.Errorf("unknown position must be omitted: %s", raw)
	}
}

func TestJSONSubmissionsGetLinesToo(t *testing.T) {
	body := []byte("{\n  \"api_version\": \"nexus.dev/v1\",\n  \"name\": \"j\",\n  \"tasks\": [\n    {\"id\": \"a\", \"task_ref\": \"t\", \"queue\": \"NOT A QUEUE\"}\n  ]\n}\n")
	_, err := Compile(body, FormatJSON)
	var vf *ValidationFailure
	if !errors.As(err, &vf) {
		t.Fatalf("got %v", err)
	}
	for _, p := range vf.Problems {
		if p.Path == "/tasks/0/queue" && p.Line == 5 {
			return
		}
	}
	t.Fatalf("no positioned queue problem in %+v", vf.Problems)
}
