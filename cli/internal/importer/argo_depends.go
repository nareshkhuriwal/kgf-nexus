// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Argo's `depends` is a boolean expression over task results:
//
//	A && (B.Succeeded || B.Failed) && !C.Skipped
//
// Nexus has one trigger rule per task. Two shapes translate:
//
//   - a conjunction of plain names / NAME.Succeeded  -> all_success
//   - a conjunction where EVERY dependency may end any way
//     ((X.Succeeded || X.Failed [|| X.Errored ...]))  -> all_done (approximated)
//
// Everything else (||, !, a lone .Failed, a mix of the two shapes) is
// unsupported; the edges are still created so the graph keeps its shape.

type depExpr struct {
	op    string // "and", "or", "not", "atom"
	kids  []*depExpr
	name  string
	state string // "", "Succeeded", "Failed", ...
}

type depParser struct {
	toks []string
	pos  int
}

func tokenizeDepends(s string) ([]string, error) {
	var toks []string
	for i := 0; i < len(s); {
		c := rune(s[i])
		switch {
		case unicode.IsSpace(c):
			i++
		case strings.HasPrefix(s[i:], "&&"), strings.HasPrefix(s[i:], "||"):
			toks = append(toks, s[i:i+2])
			i += 2
		case c == '(' || c == ')' || c == '!':
			toks = append(toks, string(c))
			i++
		default:
			j := i
			for j < len(s) && (unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j])) ||
				strings.ContainsRune("-_.", rune(s[j]))) {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("unexpected %q", s[i:i+1])
			}
			toks = append(toks, s[i:j])
			i = j
		}
	}
	return toks, nil
}

func parseDepends(s string) (*depExpr, error) {
	toks, err := tokenizeDepends(s)
	if err != nil {
		return nil, err
	}
	p := &depParser{toks: toks}
	e, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("unexpected %q", p.toks[p.pos])
	}
	return e, nil
}

func (p *depParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *depParser) or() (*depExpr, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	kids := []*depExpr{l}
	for p.peek() == "||" {
		p.pos++
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		kids = append(kids, r)
	}
	if len(kids) == 1 {
		return l, nil
	}
	return &depExpr{op: "or", kids: kids}, nil
}

func (p *depParser) and() (*depExpr, error) {
	l, err := p.unary()
	if err != nil {
		return nil, err
	}
	kids := []*depExpr{l}
	for p.peek() == "&&" {
		p.pos++
		r, err := p.unary()
		if err != nil {
			return nil, err
		}
		kids = append(kids, r)
	}
	if len(kids) == 1 {
		return l, nil
	}
	return &depExpr{op: "and", kids: kids}, nil
}

func (p *depParser) unary() (*depExpr, error) {
	switch t := p.peek(); t {
	case "!":
		p.pos++
		k, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &depExpr{op: "not", kids: []*depExpr{k}}, nil
	case "(":
		p.pos++
		e, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.peek() != ")" {
			return nil, fmt.Errorf("missing )")
		}
		p.pos++
		return e, nil
	case "", ")", "&&", "||":
		return nil, fmt.Errorf("expected a task name, got %q", t)
	default:
		p.pos++
		name, state := t, ""
		if i := strings.LastIndex(t, "."); i > 0 {
			switch s := t[i+1:]; s {
			case "Succeeded", "Failed", "Errored", "Skipped", "Omitted", "Daemoned", "AnySucceeded", "AllFailed":
				name, state = t[:i], s
			}
		}
		return &depExpr{op: "atom", name: name, state: state}, nil
	}
}

func (e *depExpr) names(out map[string]bool) {
	if e.op == "atom" {
		out[e.name] = true
	}
	for _, k := range e.kids {
		k.names(out)
	}
}

// completesAnyway reports whether e is (X.Succeeded || X.Failed [|| ...]) for one X.
func (e *depExpr) completesAnyway() (string, bool) {
	if e.op != "or" {
		return "", false
	}
	name := ""
	states := map[string]bool{}
	for _, k := range e.kids {
		if k.op != "atom" || k.state == "" {
			return "", false
		}
		if name != "" && k.name != name {
			return "", false
		}
		name = k.name
		states[k.state] = true
	}
	return name, states["Succeeded"] && states["Failed"]
}

// dependsRule translates an expression: the task names it references, the
// trigger rule, and a status/message for the report.
func dependsRule(expr string) (names []string, rule, status, msg string) {
	e, err := parseDepends(expr)
	if err != nil {
		return nil, "", Unsupported, fmt.Sprintf("depends %q does not parse: %v", expr, err)
	}
	set := map[string]bool{}
	e.names(set)
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	terms := []*depExpr{e}
	if e.op == "and" {
		terms = e.kids
	}
	plain, anyway := 0, 0
	for _, t := range terms {
		if t.op == "atom" && (t.state == "" || t.state == "Succeeded") {
			plain++
			continue
		}
		if _, ok := t.completesAnyway(); ok {
			anyway++
			continue
		}
	}
	switch {
	case plain == len(terms):
		return names, "all_success", Translated, fmt.Sprintf("depends %q: all must succeed", expr)
	case anyway == len(terms):
		return names, "all_done", Approximated, fmt.Sprintf("depends %q: runs once every dependency finished, "+
			"however it ended (trigger_rule all_done; it also fires after a cancelled or skipped upstream)", expr)
	}
	return names, "", Unsupported, fmt.Sprintf("depends %q needs per-dependency conditions (||, !, .Failed, or a "+
		"mix of must-succeed and may-fail dependencies); Nexus has one trigger rule per task (all_success or "+
		"all_done). The edges were kept with all_success", expr)
}
