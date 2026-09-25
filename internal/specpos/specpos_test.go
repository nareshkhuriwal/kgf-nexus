// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package specpos

import "testing"

const doc = `api_version: nexus.dev/v1
name: demo
tasks:
  - id: extract
    task_ref: t.extract
    queue: Not_Valid
  - id: work
    task_ref: t.work
    map:
      over_node_id: extract
      item_input: itme
    input_literals:
      n: "5"
edges:
  - from_node_id: extract
    to_node_id: work
`

func TestLookup(t *testing.T) {
	x := New([]byte(doc))
	cases := []struct {
		path       string
		line, col  int
		reachedAny bool
	}{
		{"/tasks/0/queue", 6, 12, true},                       // schema path: index
		{"/tasks/extract/queue", 6, 12, true},                 // semantic path: id
		{"/tasks/work/map.item_input", 11, 19, true},          // dotted field
		{"/tasks/work/input_literals.n", 13, 10, true},        // dotted into a map
		{"/tasks/work/edge extract->work[item]", 15, 5, true}, // edge description
		{"/tasks/work/resources/request", 7, 5, true},         // absent field: the task
		{"/tasks/nope/queue", 4, 3, true},                     // unknown task: the list
		{"/name", 2, 7, true},
		{"", 1, 1, false},
	}
	for _, c := range cases {
		got, ok := x.Lookup(c.path)
		if ok != c.reachedAny || got.Line != c.line || got.Column != c.col {
			t.Errorf("Lookup(%q) = %+v %v, want %d:%d %v", c.path, got, ok, c.line, c.col, c.reachedAny)
		}
	}
}

func TestJSONDocumentsHavePositionsToo(t *testing.T) {
	x := New([]byte("{\n  \"name\": \"demo\",\n  \"tasks\": [\n    {\"id\": \"a\", \"queue\": \"BAD\"}\n  ]\n}\n"))
	got, ok := x.Lookup("/tasks/a/queue")
	if !ok || got.Line != 4 || got.Column != 26 {
		t.Fatalf("got %+v %v", got, ok)
	}
}

func TestUnparseableDocumentNeverFails(t *testing.T) {
	if _, ok := New([]byte("tasks: [unclosed")).Lookup("/tasks/0"); ok {
		t.Fatal("lookup on an unparseable document must report no position")
	}
	var nilIndex *Index
	if _, ok := nilIndex.Lookup("/x"); ok {
		t.Fatal("nil index")
	}
}
