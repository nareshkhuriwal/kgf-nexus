// Package spec loads a WorkflowSpec from JSON or YAML and canonicalizes it.
//
// YAML authoring is first-class (ADR-0004): it must produce the IDENTICAL
// document to the Python SDK, or the two surfaces would content-address
// differently and the same logical workflow would exist as two spec versions.
// That is why canonicalization lives here and matches sdk/python/nexus/canonical.py
// rule for rule:
//
//   - object keys sorted lexicographically
//   - no insignificant whitespace
//   - UTF-8 output, non-ASCII not escaped
//   - integral floats normalized to integers (2.0 and 2 are one value)
//
// The CLI does NOT compile Python. `workflow compile --show` on a .py file shells
// out to the user's own interpreter, because the control plane's guarantee is
// that nothing but the user's process ever executes user code -- and nexusctl is
// not the user's process's business to bypass.
package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Document is a loaded, canonicalized spec.
type Document struct {
	// Value is the decoded document.
	Value map[string]any
	// JSON is the canonical bytes -- exactly what is hashed and submitted.
	JSON []byte
	// Hash is the content address the server will independently compute.
	Hash string
	// Source is the file it came from, for error messages.
	Source string
}

// Load reads a spec from a file path, or from `stdin` when path is "-".
//
// stdin is passed in rather than read from os.Stdin so the caller's injected
// streams are honoured; that is what makes piping testable without spawning a
// process.
func Load(path string, stdin io.Reader) (*Document, error) {
	var raw []byte
	var err error
	source := path
	if path == "-" {
		source = "stdin"
		if stdin == nil {
			stdin = os.Stdin
		}
		var buf bytes.Buffer
		_, err = buf.ReadFrom(stdin)
		raw = buf.Bytes()
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("reading spec from %s: %w", source, err)
	}
	return Parse(raw, source)
}

// Parse decodes JSON or YAML and canonicalizes it.
//
// The format is detected from the content rather than the extension, because a
// spec piped in on stdin has no extension and guessing wrong produces a baffling
// parse error.
func Parse(raw []byte, source string) (*Document, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%s is empty", source)
	}

	var value any
	if trimmed[0] == '{' || trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return nil, fmt.Errorf("%s is not valid JSON: %w", source, err)
		}
	} else {
		if err := yaml.Unmarshal(trimmed, &value); err != nil {
			return nil, fmt.Errorf("%s is not valid YAML: %w", source, err)
		}
	}

	normalized, err := normalize(value, "$")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	doc, ok := normalized.(map[string]any)
	if !ok {
		return nil, fmt.Errorf(
			"%s: a WorkflowSpec must be a mapping at the top level, got %T", source, normalized)
	}

	canonical, err := Canonical(doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	sum := sha256.Sum256(canonical)
	return &Document{
		Value:  doc,
		JSON:   canonical,
		Hash:   hex.EncodeToString(sum[:]),
		Source: source,
	}, nil
}

// normalize converts YAML's decoded types into the JSON type set.
//
// yaml.v3 decodes mappings as map[string]any when keys are strings, but it also
// yields ints where JSON would have float64, and it permits non-string keys.
// Converting here means Canonical only has to reason about one type set.
func normalize(v any, path string) (any, error) {
	switch t := v.(type) {
	case nil, bool, string:
		return v, nil
	case int:
		return int64(t), nil
	case int64:
		return t, nil
	case uint64:
		return int64(t), nil
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return nil, fmt.Errorf("%s: NaN and Infinity have no JSON representation", path)
		}
		// An integral float and an int must not hash differently, or a
		// multiplier written as 2 in YAML and 2.0 in Python would produce two
		// spec versions of one workflow.
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			return int64(t), nil
		}
		return t, nil
	case json.Number:
		return normalizeNumber(t, path)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			n, err := normalize(val, path+"."+k)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("%s: object key %v is not a string", path, k)
			}
			n, err := normalize(val, path+"."+ks)
			if err != nil {
				return nil, err
			}
			out[ks] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			n, err := normalize(val, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s: %T cannot be represented in JSON", path, v)
	}
}

func normalizeNumber(n json.Number, path string) (any, error) {
	if i, err := n.Int64(); err == nil {
		return i, nil
	}
	f, err := n.Float64()
	if err != nil {
		return nil, fmt.Errorf("%s: %q is not a number", path, n.String())
	}
	return f, nil
}

// Canonical encodes a normalized value as canonical JSON.
//
// Written by hand rather than using encoding/json's marshaller because Go's
// marshaller HTML-escapes <, > and & by default, which would produce different
// bytes -- and therefore a different content address -- than the Python SDK for
// any spec containing those characters in a command or an env value.
func Canonical(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := writeCanonical(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeCanonical(b *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeString(b, t)
	case int64:
		fmt.Fprintf(b, "%d", t)
	case int:
		fmt.Fprintf(b, "%d", t)
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return fmt.Errorf("NaN and Infinity have no JSON representation")
		}
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			fmt.Fprintf(b, "%d", int64(t))
		} else {
			// 'g' with -1 precision gives the shortest representation that
			// round-trips, which is what Python's repr also produces.
			fmt.Fprintf(b, "%s", strings.TrimSuffix(fmt.Sprintf("%g", t), ".0"))
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			if err := writeCanonical(b, t[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		// List ORDER is significant and preserved. The emitter (the SDK, or the
		// author's YAML) is responsible for ordering lists whose order carries
		// no meaning, such as tasks and edges.
		b.WriteByte('[')
		for i, val := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, val); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	default:
		return fmt.Errorf("%T cannot be canonically encoded", v)
	}
	return nil
}

// writeString emits a JSON string with minimal escaping and no HTML escaping,
// matching Python's json.dumps(ensure_ascii=False).
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// YAML renders the document as YAML, for `compile --show --output yaml`.
func (d *Document) YAML() ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(d.Value); err != nil {
		return nil, fmt.Errorf("rendering YAML: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("rendering YAML: %w", err)
	}
	return b.Bytes(), nil
}

// Indented renders the canonical document with indentation, for human reading.
// This is NOT what gets hashed -- pretty-printing changes the bytes.
func (d *Document) Indented() ([]byte, error) {
	var b bytes.Buffer
	if err := json.Indent(&b, d.JSON, "", "  "); err != nil {
		return nil, fmt.Errorf("indenting spec: %w", err)
	}
	return b.Bytes(), nil
}

// Name returns the workflow name, for messages and run creation.
func (d *Document) Name() string {
	if s, ok := d.Value["name"].(string); ok {
		return s
	}
	return ""
}

// Validate performs the structural checks that can be done without the server.
//
// Local validation is not a substitute for the server's JSON Schema and semantic
// checks -- it is a faster feedback loop for the mistakes that are obvious from
// the document alone, so a user editing YAML is not waiting on a round trip to
// learn they typed `taks:`.
func (d *Document) Validate() []string {
	var problems []string
	if d.Name() == "" {
		problems = append(problems, "missing required field: name")
	}
	if _, ok := d.Value["api_version"]; !ok {
		problems = append(problems, "missing required field: api_version")
	}

	tasksRaw, ok := d.Value["tasks"]
	if !ok {
		return append(problems, "missing required field: tasks")
	}
	tasks, ok := tasksRaw.([]any)
	if !ok {
		return append(problems, "tasks must be a list")
	}
	if len(tasks) == 0 {
		problems = append(problems, "tasks is empty: a workflow needs at least one task")
	}

	ids := make(map[string]bool, len(tasks))
	for i, t := range tasks {
		task, ok := t.(map[string]any)
		if !ok {
			problems = append(problems, fmt.Sprintf("tasks[%d] must be a mapping", i))
			continue
		}
		id, _ := task["id"].(string)
		if id == "" {
			problems = append(problems, fmt.Sprintf("tasks[%d] missing required field: id", i))
			continue
		}
		if ids[id] {
			// node_id participates in the idempotency key, so a duplicate is not
			// a cosmetic problem -- two nodes would share one key.
			problems = append(problems, fmt.Sprintf("duplicate task id %q", id))
		}
		ids[id] = true
		if ref, _ := task["task_ref"].(string); ref == "" {
			problems = append(problems, fmt.Sprintf("task %q missing required field: task_ref", id))
		}
	}

	edgesRaw, ok := d.Value["edges"]
	if !ok {
		return problems
	}
	edges, ok := edgesRaw.([]any)
	if !ok {
		return append(problems, "edges must be a list")
	}
	type edge struct{ from, to string }
	var parsed []edge
	for i, e := range edges {
		em, ok := e.(map[string]any)
		if !ok {
			problems = append(problems, fmt.Sprintf("edges[%d] must be a mapping", i))
			continue
		}
		from, _ := em["from_node_id"].(string)
		to, _ := em["to_node_id"].(string)
		if from == "" || to == "" {
			problems = append(problems, fmt.Sprintf("edges[%d] needs both from_node_id and to_node_id", i))
			continue
		}
		if !ids[from] {
			problems = append(problems, fmt.Sprintf("edges[%d] references unknown from_node_id %q", i, from))
		}
		if !ids[to] {
			problems = append(problems, fmt.Sprintf("edges[%d] references unknown to_node_id %q", i, to))
		}
		parsed = append(parsed, edge{from, to})
	}

	// Acyclicity. The server checks this too, but finding it locally puts the
	// error next to the file the user is editing.
	indeg := make(map[string]int, len(ids))
	adj := make(map[string][]string, len(ids))
	for id := range ids {
		indeg[id] = 0
	}
	for _, e := range parsed {
		if ids[e.from] && ids[e.to] {
			adj[e.from] = append(adj[e.from], e.to)
			indeg[e.to]++
		}
	}
	var queue []string
	for id, d := range indeg {
		if d == 0 {
			queue = append(queue, id)
		}
	}
	visited := 0
	for len(queue) > 0 {
		n := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		visited++
		for _, m := range adj[n] {
			indeg[m]--
			if indeg[m] == 0 {
				queue = append(queue, m)
			}
		}
	}
	if visited != len(ids) {
		var stuck []string
		for id, d := range indeg {
			if d > 0 {
				stuck = append(stuck, id)
			}
		}
		sort.Strings(stuck)
		problems = append(problems, "graph contains a cycle involving: "+strings.Join(stuck, ", "))
	}
	return problems
}

// IsPython reports whether a path looks like a Python authoring file.
func IsPython(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".py")
}
