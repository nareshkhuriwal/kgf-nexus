// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/khuriwalgroup/kgf-nexus/internal/specpos"
)

// Problem is one thing wrong with a spec, found locally or reported by the
// gateway, with where it is in the author's file when that is knowable.
type Problem struct {
	Path    string
	Message string
	// File, Line and Column locate the problem: the YAML/JSON file itself, or
	// for a .py spec the Python source of the @task or of the call that traced
	// the node. Line 0 means unknown.
	File   string
	Line   int
	Column int
}

// SourceLocation is one Python location, as the SDK's source map emits it.
type SourceLocation struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column,omitempty"`
}

// NodeSource is where a .py spec's node came from (nexus.sourcemap.NodeSource).
type NodeSource struct {
	Definition *SourceLocation `json:"definition,omitempty"`
	CallSite   *SourceLocation `json:"call_site,omitempty"`
}

// wiringFields are problems with how a node is CONNECTED, which live at the
// call in the workflow body rather than on the @task declaration. Mirrors
// nexus.sourcemap._WIRING_FIELDS.
var wiringFields = map[string]bool{"input_literals": true, "input_params": true, "map": true, "edge": true}

var taskPath = regexp.MustCompile(`^/tasks/([^/]+)(?:/(.*))?$`)

// Locate places a problem path in the author's file. For YAML and JSON it is
// the line of the node the path reaches; for a .py spec it is the @task
// definition, or the workflow-body call for a wiring problem, when the SDK
// reported a source map. ok is false when nothing better than the file name is
// known.
func (d *Document) Locate(path string) (file string, line, col int, ok bool) {
	if d.SourceMap != nil {
		m := taskPath.FindStringSubmatch(path)
		if m == nil {
			return d.Source, 0, 0, false
		}
		key, rest := m[1], m[2]
		if i, err := strconv.Atoi(key); err == nil {
			if id := d.taskIDAt(i); id != "" {
				key = id
			}
		}
		node, found := d.SourceMap[key]
		if !found {
			return d.Source, 0, 0, false
		}
		head := rest
		if i := strings.IndexAny(head, "/. "); i >= 0 {
			head = head[:i]
		}
		first, second := node.Definition, node.CallSite
		if wiringFields[head] {
			first, second = second, first
		}
		for _, loc := range []*SourceLocation{first, second} {
			if loc != nil && loc.Line > 0 {
				return loc.File, loc.Line, loc.Column, true
			}
		}
		return d.Source, 0, 0, false
	}
	if len(d.Raw) == 0 {
		return d.Source, 0, 0, false
	}
	at, found := specpos.New(d.Raw).Lookup(path)
	if !found {
		return d.Source, 0, 0, false
	}
	return d.Source, at.Line, at.Column, true
}

func (d *Document) taskIDAt(i int) string {
	tasks, _ := d.Value["tasks"].([]any)
	if i < 0 || i >= len(tasks) {
		return ""
	}
	t, _ := tasks[i].(map[string]any)
	id, _ := t["id"].(string)
	return id
}

// Positioned fills File/Line/Column on each problem from the document.
func (d *Document) Positioned(ps []Problem) []Problem {
	out := make([]Problem, len(ps))
	for i, p := range ps {
		p.File, p.Line, p.Column, _ = d.Locate(p.Path)
		out[i] = p
	}
	return out
}

// String renders "file:line:col: message (path)", the shape editors and CI log
// scrapers already understand; the path stays for the exact field.
func (p Problem) String() string {
	var b strings.Builder
	b.WriteString(p.File)
	if p.Line > 0 {
		fmt.Fprintf(&b, ":%d", p.Line)
		if p.Column > 0 {
			fmt.Fprintf(&b, ":%d", p.Column)
		}
	}
	b.WriteString(": ")
	b.WriteString(p.Message)
	if p.Path != "" {
		fmt.Fprintf(&b, " (%s)", p.Path)
	}
	return b.String()
}

// FormatProblems renders a positioned list, one per line.
func FormatProblems(ps []Problem) string {
	lines := make([]string, 0, len(ps))
	for _, p := range ps {
		lines = append(lines, "  - "+p.String())
	}
	return strings.Join(lines, "\n")
}
