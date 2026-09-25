// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"strconv"
	"strings"
	"testing"
)

func TestProblemsCarryPathsAndValidateKeepsItsMessages(t *testing.T) {
	doc, err := Parse([]byte("api_version: v\nname: x\ntasks:\n  - id: a\n  - id: a\n    task_ref: t\n"), "x.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ps := doc.Positioned(doc.Problems())
	got := map[string]Problem{}
	for _, p := range ps {
		got[p.Path] = p
	}
	if p := got["/tasks/0/task_ref"]; p.Line != 4 || !strings.Contains(p.Message, `task "a" missing required field: task_ref`) {
		t.Errorf("task_ref problem = %+v", p)
	}
	if p := got["/tasks/1/id"]; p.Line != 5 || p.Column != 9 || p.String() != `x.yaml:5:9: duplicate task id "a" (/tasks/1/id)` {
		t.Errorf("duplicate id problem = %+v %q", p, p.String())
	}
	if len(doc.Validate()) != len(ps) {
		t.Error("Validate and Problems disagree")
	}
}

func TestPythonSourceMapLocate(t *testing.T) {
	doc := &Document{
		Source: "dag.py",
		Value:  map[string]any{"tasks": []any{map[string]any{"id": "extract"}, map[string]any{"id": "load"}}},
		SourceMap: map[string]NodeSource{
			"load": {Definition: &SourceLocation{File: "tasks.py", Line: 10}, CallSite: &SourceLocation{File: "dag.py", Line: 40}},
		},
	}
	for path, want := range map[string]string{
		"/tasks/load/resources/request": "tasks.py:10",
		"/tasks/1/queue":                "tasks.py:10", // schema paths index the sorted task list
		"/tasks/load/input_params":      "dag.py:40",   // wiring lives at the call site
		"/tasks/load/map.item_input":    "dag.py:40",
		"/tasks/extract/queue":          "dag.py:0",
		"/name":                         "dag.py:0",
	} {
		f, line, _, _ := doc.Locate(path)
		if got := f + ":" + strconv.Itoa(line); got != want {
			t.Errorf("Locate(%q) = %s, want %s", path, got, want)
		}
	}
}
