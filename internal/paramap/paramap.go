// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package paramap maps an incoming document (a webhook payload, an observed
// object, an upstream run) onto run parameters, for webhook triggers and sensors
// (ADR-0028).
//
// A mapping is {param name: expression}. An expression is one of:
//
//   - a JSONPath into the document: "$.repository.name", "$.commits[0].id",
//     "$['odd key']". Only the deterministic subset is supported -- member and
//     index access. No wildcards, filters or recursive descent: a mapping must
//     yield exactly one value, and those operators yield sets.
//   - a template: any string containing {{ $.path }} placeholders, rendered to a
//     string ("s3://{{ $.object.bucket }}/{{ $.object.key }}").
//   - anything else is a literal and passes through unchanged.
//
// A path that does not resolve is an ERROR, not a silently omitted parameter:
// the sender gets a 422 naming the path instead of a run started without it.
package paramap

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MaxPathLen bounds an expression.
const MaxPathLen = 512

type step struct {
	key   string
	index int
	isIdx bool
}

// ParsePath parses the supported JSONPath subset.
func ParsePath(p string) ([]step, error) {
	p = strings.TrimSpace(p)
	if len(p) > MaxPathLen {
		return nil, fmt.Errorf("path longer than %d characters", MaxPathLen)
	}
	if !strings.HasPrefix(p, "$") {
		return nil, fmt.Errorf("path %q must start with $", p)
	}
	var out []step
	i := 1
	for i < len(p) {
		switch p[i] {
		case '.':
			i++
			j := i
			for j < len(p) && p[j] != '.' && p[j] != '[' {
				j++
			}
			name := p[i:j]
			if name == "" || name == "*" {
				return nil, fmt.Errorf("path %q: empty member names, wildcards and recursive descent are not supported", p)
			}
			out = append(out, step{key: name})
			i = j
		case '[':
			end := strings.IndexByte(p[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("path %q has an unclosed [", p)
			}
			inner := strings.TrimSpace(p[i+1 : i+end])
			i += end + 1
			if len(inner) >= 2 && (inner[0] == '\'' || inner[0] == '"') && inner[len(inner)-1] == inner[0] {
				out = append(out, step{key: inner[1 : len(inner)-1]})
				continue
			}
			n, err := strconv.Atoi(inner)
			if err != nil {
				return nil, fmt.Errorf("path %q: [%s] is neither an index nor a quoted key (filters and wildcards are not supported)", p, inner)
			}
			out = append(out, step{index: n, isIdx: true})
		default:
			return nil, fmt.Errorf("path %q: unexpected %q at offset %d", p, p[i], i)
		}
	}
	return out, nil
}

// Eval resolves a path against a decoded JSON document. found=false means the
// path is well formed but absent from this document.
func Eval(path string, doc any) (value any, found bool, err error) {
	steps, err := ParsePath(path)
	if err != nil {
		return nil, false, err
	}
	cur := doc
	for _, s := range steps {
		if s.isIdx {
			arr, ok := cur.([]any)
			if !ok {
				return nil, false, nil
			}
			idx := s.index
			if idx < 0 {
				idx += len(arr)
			}
			if idx < 0 || idx >= len(arr) {
				return nil, false, nil
			}
			cur = arr[idx]
			continue
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		v, ok := obj[s.key]
		if !ok {
			return nil, false, nil
		}
		cur = v
	}
	return cur, true, nil
}

var placeholder = regexp.MustCompile(`\{\{\s*(\$[^}]*?)\s*\}\}`)

func isTemplate(s string) bool { return strings.Contains(s, "{{") }

// Validate checks a mapping's syntax without a document.
func Validate(mapping map[string]any) error {
	if len(mapping) > 100 {
		return fmt.Errorf("at most 100 mapped parameters")
	}
	for _, name := range sortedKeys(mapping) {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("a parameter name is empty")
		}
		s, ok := mapping[name].(string)
		if !ok {
			continue
		}
		switch {
		case isTemplate(s):
			if len(s) > 4096 {
				return fmt.Errorf("param %q: template longer than 4096 characters", name)
			}
			matches := placeholder.FindAllStringSubmatch(s, -1)
			if len(matches) == 0 || strings.Count(s, "{{") != len(matches) {
				return fmt.Errorf("param %q: every {{ }} placeholder must hold a $ path", name)
			}
			for _, m := range matches {
				if _, err := ParsePath(m[1]); err != nil {
					return fmt.Errorf("param %q: %w", name, err)
				}
			}
		case strings.HasPrefix(s, "$"):
			if _, err := ParsePath(s); err != nil {
				return fmt.Errorf("param %q: %w", name, err)
			}
		}
	}
	return nil
}

// MissingError lists the paths a document did not contain.
type MissingError struct{ Paths []string }

func (e *MissingError) Error() string {
	return "the document has no value at " + strings.Join(e.Paths, ", ")
}

// Apply evaluates a mapping against doc.
func Apply(mapping map[string]any, doc any) (map[string]any, error) {
	out := make(map[string]any, len(mapping))
	var missing []string
	for _, name := range sortedKeys(mapping) {
		raw := mapping[name]
		s, ok := raw.(string)
		if !ok {
			out[name] = raw
			continue
		}
		switch {
		case isTemplate(s):
			var rerr error
			out[name] = placeholder.ReplaceAllStringFunc(s, func(m string) string {
				path := placeholder.FindStringSubmatch(m)[1]
				v, found, err := Eval(path, doc)
				if err != nil {
					rerr = fmt.Errorf("param %q: %w", name, err)
					return ""
				}
				if !found {
					missing = append(missing, path)
					return ""
				}
				return Stringify(v)
			})
			if rerr != nil {
				return nil, rerr
			}
		case strings.HasPrefix(s, "$"):
			v, found, err := Eval(s, doc)
			if err != nil {
				return nil, fmt.Errorf("param %q: %w", name, err)
			}
			if !found {
				missing = append(missing, s)
				continue
			}
			out[name] = v
		default:
			out[name] = s
		}
	}
	if len(missing) > 0 {
		return nil, &MissingError{Paths: missing}
	}
	return out, nil
}

// Stringify renders a value for a template or an idempotency key.
func Stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Glob is a compiled object-key pattern: * matches within one path segment,
// ** across segments ("**/" also matches zero directories), ? one character.
// An empty pattern matches every key.
type Glob struct{ re *regexp.Regexp }

// CompileGlob compiles a pattern.
func CompileGlob(pattern string) (*Glob, error) {
	if len(pattern) > 1024 {
		return nil, fmt.Errorf("pattern longer than 1024 characters")
	}
	if pattern == "" {
		return &Glob{}, nil
	}
	var sb strings.Builder
	sb.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					sb.WriteString("(?:.*/)?")
				} else {
					sb.WriteString(".*")
				}
			} else {
				sb.WriteString("[^/]*")
			}
		case '?':
			sb.WriteString("[^/]")
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteString("$")
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return nil, err
	}
	return &Glob{re: re}, nil
}

// Match reports whether name matches.
func (g *Glob) Match(name string) bool {
	if g == nil || g.re == nil {
		return true
	}
	return g.re.MatchString(name)
}
