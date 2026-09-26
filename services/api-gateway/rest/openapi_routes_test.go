// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/health"
)

// TestOpenAPICoversEveryRoute keeps api/openapi.yaml and the registered routes
// the same set, in both directions, and keeps each operation's authorization
// metadata equal to the guard that actually runs.
//
// HOW ROUTES ARE ENUMERATED. The non-test Go sources of this package and of the
// health package are parsed with go/ast, and every mux.Handle/HandleFunc call is
// read:
//
//   - the pattern is a string literal, a package-level string constant
//     (assignmentRoute), or -- for the table-driven registerRBACRoutes -- a
//     field of the loop variable, in which case every route-shaped string in the
//     enclosing function's composite literals is taken;
//   - a handler wrapped in a.guard(authn.ActionX, ...) is guarded with action X;
//     anything else is unguarded;
//   - a call inside registerIdentityRoutes is conditional on identity, and one
//     inside its `if a.email != nil` block on the email flows.
//
// Source parsing alone could count a Handle call that never runs, so every route
// found is then resolved against the REAL mux (identity and email wired, and the
// health routes mounted beside it exactly as cmd/api-gateway's publicHandler
// does) via ServeMux.Handler, which must report that very pattern. Every
// operation in the document is resolved the same way.
func TestOpenAPICoversEveryRoute(t *testing.T) {
	routes := parsedRoutes(t)
	doc := loadOpenAPI(t)
	ops := openAPIOperations(doc)
	mux := liveMux(t)

	// Match on the pattern with parameter NAMES erased, so a renamed parameter
	// is reported as exactly that rather than as one missing and one extra route.
	norm := regexp.MustCompile(`\{[^}]*\}`)
	shape := func(pattern string) string { return norm.ReplaceAllString(pattern, "{}") }

	routeByShape := map[string]parsedRoute{}
	for _, rt := range routes {
		if prev, dup := routeByShape[shape(rt.pattern)]; dup {
			t.Errorf("route %q is registered twice (%s and %s)", rt.pattern, prev.pos, rt.pos)
		}
		routeByShape[shape(rt.pattern)] = rt
	}
	opByShape := map[string]openAPIOp{}
	pathMethods := map[string][]string{}
	for _, op := range ops {
		opByShape[shape(op.pattern)] = op
		pathMethods[shape(op.path)] = append(pathMethods[shape(op.path)], op.method)
	}

	for _, key := range sortedStringKeys(routeByShape) {
		rt := routeByShape[key]
		op, ok := opByShape[key]
		if !ok {
			method, path, _ := strings.Cut(rt.pattern, " ")
			if others := pathMethods[shape(path)]; len(others) > 0 {
				t.Errorf("route %q (registered at %s) is missing from api/openapi.yaml: the path is there, "+
					"but only with %v -- add a %q operation under paths.%q",
					rt.pattern, rt.pos, others, strings.ToLower(method), path)
			} else {
				t.Errorf("route %q (registered at %s) is missing from api/openapi.yaml: add paths.%q.%s",
					rt.pattern, rt.pos, path, strings.ToLower(method))
			}
			continue
		}
		if op.pattern != rt.pattern {
			t.Errorf("path parameter names differ: the code registers %q (%s), api/openapi.yaml says %q; "+
				"the names must match r.PathValue(...) exactly", rt.pattern, rt.pos, op.pattern)
			continue
		}
		checkOperationAuth(t, rt, op)
	}
	for _, key := range sortedStringKeys(opByShape) {
		if _, ok := routeByShape[key]; !ok {
			op := opByShape[key]
			t.Errorf("api/openapi.yaml documents %q (operationId %s), but no such route is registered; "+
				"remove it, or fix its method or path", op.pattern, op.operationID)
		}
	}

	// Runtime: every registered route and every documented operation must
	// resolve, with its parameters filled in, to itself on the live mux.
	resolveAll := map[string]string{}
	for _, rt := range routes {
		resolveAll[rt.pattern] = "registered at " + rt.pos
	}
	for _, op := range ops {
		resolveAll[op.pattern] = "documented as " + op.operationID
	}
	for _, pattern := range sortedStringKeys(resolveAll) {
		method, path, _ := strings.Cut(pattern, " ")
		concrete := norm.ReplaceAllStringFunc(path, func(p string) string {
			return "p-" + strings.Trim(p, "{}") + "-value"
		})
		if got := mux.resolve(method, concrete); got != pattern {
			t.Errorf("%s %s (%s) resolves on the live mux to %q, want %q -- the route is not "+
				"actually registered, or another pattern shadows it", method, concrete, resolveAll[pattern], got, pattern)
		}
	}

	t.Logf("%d routes registered, %d operations documented", len(routes), len(ops))
}

// checkOperationAuth compares an operation's security and x-nexus-* metadata
// with the guard the code wraps the route in.
func checkOperationAuth(t *testing.T, rt parsedRoute, op openAPIOp) {
	t.Helper()
	security, hasSecurity := op.raw["security"]
	action, hasAction := op.raw["x-nexus-action"]
	perms, hasPerms := op.raw["x-nexus-permissions"]
	cond, _ := op.raw["x-nexus-conditional"].(string)

	if cond != rt.conditional {
		fix := fmt.Sprintf("set it to %q", rt.conditional)
		if rt.conditional == "" {
			fix = "remove it"
		}
		t.Errorf("%s: x-nexus-conditional is %q but the route is registered %s; %s",
			rt.pattern, cond, describeConditional(rt.conditional), fix)
	}

	if rt.action == "" {
		if !hasSecurity || len(asSlice(security)) != 0 {
			t.Errorf("%s is unguarded in the code (%s) but api/openapi.yaml does not say `security: []`", rt.pattern, rt.pos)
		}
		if hasAction || hasPerms {
			t.Errorf("%s is unguarded; it must not carry x-nexus-action or x-nexus-permissions", rt.pattern)
		}
		return
	}

	if hasSecurity && len(asSlice(security)) == 0 {
		t.Errorf("%s is guarded (%s) but api/openapi.yaml marks it `security: []`", rt.pattern, rt.pos)
	}
	if action != rt.action {
		t.Errorf("%s: x-nexus-action is %v but the guard uses %q (%s)", rt.pattern, action, rt.action, rt.pos)
	}
	want, mapped := routePermissions[rt.pattern]
	if !mapped {
		t.Errorf("%s is guarded but has no routePermissions entry, so every call answers 403", rt.pattern)
		return
	}
	if !hasPerms {
		t.Errorf("%s: missing x-nexus-permissions; want %v", rt.pattern, want)
		return
	}
	var got []string
	for _, p := range asSlice(perms) {
		got = append(got, fmt.Sprint(p))
	}
	if !sameSet(got, want) {
		t.Errorf("%s: x-nexus-permissions is %v but routePermissions requires %v", rt.pattern, got, want)
	}
}

func describeConditional(c string) string {
	switch c {
	case "identity":
		return "only when identity is wired (registerIdentityRoutes)"
	case "email":
		return "only when identity AND the email flows are wired"
	default:
		return "unconditionally"
	}
}

// ── source enumeration ──────────────────────────────────────────────────────

type parsedRoute struct {
	pattern     string
	action      string // "" when unguarded
	conditional string // "", "identity" or "email"
	pos         string
}

var routeShaped = regexp.MustCompile(`^(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS) /`)

var guardActions = map[string]string{
	"ActionRead":       string(authn.ActionRead),
	"ActionWrite":      string(authn.ActionWrite),
	"ActionAdminWrite": string(authn.ActionAdminWrite),
	"ActionWorkerRPC":  string(authn.ActionWorkerRPC),
}

// parsedRoutes enumerates the routes registered by this package and by the
// health package, which cmd/api-gateway mounts on the same listener.
func parsedRoutes(t *testing.T) []parsedRoute {
	t.Helper()
	var out []parsedRoute
	for _, dir := range []string{".", "../health"} {
		out = append(out, parseRouteDir(t, dir)...)
	}
	if len(out) < 50 {
		t.Fatalf("found only %d routes in the source; the enumeration is broken", len(out))
	}
	return out
}

func parseRouteDir(t *testing.T, dir string) []parsedRoute {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}

	consts := map[string]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						if v, ok := stringLit(vs.Values[i]); ok {
							consts[n.Name] = v
						}
					}
				}
			}
		}
	}

	var out []parsedRoute
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// The spans of `if a.email != nil { ... }` blocks in this function.
			var emailBlocks [][2]token.Pos
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if ifs, ok := n.(*ast.IfStmt); ok && strings.Contains(exprString(ifs.Cond), "email") {
					emailBlocks = append(emailBlocks, [2]token.Pos{ifs.Body.Pos(), ifs.Body.End()})
				}
				return true
			})
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
					return true
				}
				pos := fset.Position(call.Pos())
				where := fmt.Sprintf("%s:%d", filepath.Base(pos.Filename), pos.Line)

				var patterns []string
				switch arg := call.Args[0].(type) {
				case *ast.BasicLit:
					v, _ := stringLit(arg)
					patterns = []string{v}
				case *ast.Ident:
					v, ok := consts[arg.Name]
					if !ok {
						t.Errorf("%s: route pattern %s is not a string constant of this package; teach this test to read it", where, arg.Name)
						return true
					}
					patterns = []string{v}
				case *ast.SelectorExpr:
					// A table-driven registration: the patterns are the
					// route-shaped string literals of the function's tables.
					ast.Inspect(fn.Body, func(m ast.Node) bool {
						cl, ok := m.(*ast.CompositeLit)
						if !ok || len(cl.Elts) == 0 {
							return true
						}
						if v, ok := stringLit(cl.Elts[0]); ok && routeShaped.MatchString(v) {
							patterns = append(patterns, v)
						}
						return true
					})
					if len(patterns) == 0 {
						t.Errorf("%s: table-driven registration with no route table found; teach this test to read it", where)
					}
				default:
					t.Errorf("%s: unrecognised route pattern expression %s; teach this test to read it", where, exprString(call.Args[0]))
					return true
				}

				action := ""
				if sel.Sel.Name == "Handle" {
					if g, ok := call.Args[1].(*ast.CallExpr); ok {
						if gs, ok := g.Fun.(*ast.SelectorExpr); ok && gs.Sel.Name == "guard" && len(g.Args) == 2 {
							if as, ok := g.Args[0].(*ast.SelectorExpr); ok {
								action = guardActions[as.Sel.Name]
							}
							if action == "" {
								t.Errorf("%s: unrecognised guard action %s", where, exprString(g.Args[0]))
							}
						}
					}
				}
				conditional := ""
				if fn.Name.Name == "registerIdentityRoutes" {
					conditional = "identity"
					for _, b := range emailBlocks {
						if call.Pos() >= b[0] && call.Pos() < b[1] {
							conditional = "email"
						}
					}
				}
				for _, p := range patterns {
					if !routeShaped.MatchString(p) {
						t.Errorf("%s: pattern %q has no method; every route must be METHOD /path", where, p)
						continue
					}
					out = append(out, parsedRoute{pattern: p, action: action, conditional: conditional, pos: where})
				}
				return true
			})
		}
	}
	return out
}

func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(bl.Value)
	return v, err == nil
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.BinaryExpr:
		return exprString(x.X) + " " + x.Op.String() + " " + exprString(x.Y)
	case *ast.UnaryExpr:
		return x.Op.String() + exprString(x.X)
	case *ast.BasicLit:
		return x.Value
	default:
		return fmt.Sprintf("%T", e)
	}
}

// ── the document's operations ───────────────────────────────────────────────

type openAPIOp struct {
	method, path, pattern, operationID string
	raw                                map[string]any
}

func openAPIOperations(doc map[string]any) []openAPIOp {
	var out []openAPIOp
	paths := asMap(doc["paths"])
	for _, path := range sortedKeys(paths) {
		item := asMap(paths[path])
		for _, m := range httpMethods {
			op, ok := item[m].(map[string]any)
			if !ok {
				continue
			}
			method := strings.ToUpper(m)
			id, _ := op["operationId"].(string)
			out = append(out, openAPIOp{method: method, path: path, pattern: method + " " + path, operationID: id, raw: op})
		}
	}
	return out
}

// ── the live mux ────────────────────────────────────────────────────────────

type liveRoutes struct{ outer, api *http.ServeMux }

// liveMux builds the gateway's public handler the way cmd/api-gateway does:
// the REST mux (identity and email flows wired, so every conditional route is
// registered) under /v1/, and the health probes beside it.
func liveMux(t *testing.T) liveRoutes {
	t.Helper()
	_, h, _ := newEmailUnitAPI(t, DefaultEmailFlowConfig()) // closes its flows on cleanup
	api, ok := h.(*http.ServeMux)
	if !ok {
		t.Fatalf("Routes() returned %T, not *http.ServeMux; update this test", h)
	}
	outer := http.NewServeMux()
	outer.Handle("/v1/", api)
	health.New(nil).Routes(outer)
	return liveRoutes{outer: outer, api: api}
}

// resolve reports the pattern that serves method+path, looking through the
// /v1/ mount into the REST mux as net/http itself would.
func (l liveRoutes) resolve(method, path string) string {
	req := httptest.NewRequest(method, path, nil)
	_, pattern := l.outer.Handler(req)
	if pattern == "/v1/" {
		_, pattern = l.api.Handler(req)
	}
	return pattern
}

func sameSet(a, b []string) bool {
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return strings.Join(x, "\x00") == strings.Join(y, "\x00")
}

func sortedStringKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
