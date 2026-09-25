// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package specpos maps a WorkflowSpec problem path back to a line and column
// in the document the author wrote.
//
// The gateway reports a problem by path: "/tasks/3/resources/request" from the
// JSON Schema (an INDEX into tasks) and "/tasks/load/inputs/summary" or
// "/tasks/work/map.item_input" from the semantic checks (a task ID, then a
// field that may be dotted or an edge description). A path is exact but an
// author editing YAML thinks in lines; this package bridges the two for YAML
// and for JSON, which yaml.v3 parses with the same position information.
//
// It depends on nothing but gopkg.in/yaml.v3 so that nexusctl -- which keeps
// its dependencies to the standard library and yaml.v3 -- can share it with the
// gateway instead of carrying a second implementation that would drift.
package specpos

import (
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Position is a 1-based line and column in the source document.
type Position struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// Index answers position lookups for one parsed document.
type Index struct {
	root *yaml.Node
}

// New parses a YAML or JSON document for position lookups. A document that
// does not parse yields an Index whose lookups all fail, never an error: line
// mapping is an aid to a validation message, and must not replace it.
func New(body []byte) *Index {
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil || len(doc.Content) == 0 {
		return &Index{}
	}
	return &Index{root: doc.Content[0]}
}

var edgeField = regexp.MustCompile(`^edge ([^\s\[]+)->([^\s\[]+)`)

// Lookup returns the position of the deepest node the path reaches, and
// whether it reached any node below the document root. A path that names a
// field the author omitted resolves to the closest enclosing node that exists
// -- the task, say -- which is where the missing field has to be added.
func (x *Index) Lookup(path string) (Position, bool) {
	if x == nil || x.root == nil {
		return Position{}, false
	}
	segs := splitPath(path)
	node, reached := x.root, false
	for i := 0; i < len(segs); i++ {
		seg := segs[i]
		// Semantic field forms: "edge a->b[in<-out]" names an edges entry.
		if m := edgeField.FindStringSubmatch(seg); m != nil {
			if e := findEdge(x.root, m[1], m[2]); e != nil {
				return pos(e), true
			}
			return pos(node), reached
		}
		next := child(node, seg)
		if next == nil && node.Kind == yaml.MappingNode && strings.Contains(seg, ".") {
			// "map.item_input", "input_literals.x", "runtime.stdout_output":
			// dotted field paths from internal/dag.
			parts := strings.Split(seg, ".")
			segs = append(segs[:i], append(parts, segs[i+1:]...)...)
			seg = segs[i]
			next = child(node, seg)
		}
		if next == nil {
			return pos(node), reached
		}
		node, reached = next, true
	}
	return pos(node), reached
}

func pos(n *yaml.Node) Position { return Position{Line: n.Line, Column: n.Column} }

func splitPath(path string) []string {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return nil
	}
	parts := strings.Split(path, "/")
	for i, p := range parts {
		// JSON Pointer escapes (RFC 6901).
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
	}
	return parts
}

// child steps one segment: a key of a mapping, or -- in a sequence -- a
// numeric index or, for tasks, the element whose "id" is the segment.
func child(n *yaml.Node, seg string) *yaml.Node {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == seg {
				v := n.Content[i+1]
				if v.Kind == yaml.AliasNode && v.Alias != nil {
					return v.Alias
				}
				return v
			}
		}
	case yaml.SequenceNode:
		if idx, err := strconv.Atoi(seg); err == nil && idx >= 0 && idx < len(n.Content) {
			// An index wins when it is in range, matching JSON Schema paths;
			// a task literally named "3" is still found by id below when the
			// index is out of range.
			return n.Content[idx]
		}
		for _, el := range n.Content {
			if el.Kind != yaml.MappingNode {
				continue
			}
			if id := child(el, "id"); id != nil && id.Value == seg {
				return el
			}
		}
	}
	return nil
}

func findEdge(root *yaml.Node, from, to string) *yaml.Node {
	edges := child(root, "edges")
	if edges == nil || edges.Kind != yaml.SequenceNode {
		return nil
	}
	for _, e := range edges.Content {
		f, t := child(e, "from_node_id"), child(e, "to_node_id")
		if f != nil && t != nil && f.Value == from && t.Value == to {
			return e
		}
	}
	return nil
}
