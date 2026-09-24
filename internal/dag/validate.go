// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package dag

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// ErrorCode classifies a validation failure. Codes exist so the API gateway can
// map failures onto stable machine-readable reasons; a UI that has to regex
// prose to decide how to render an error is a UI that breaks on every wording
// change.
type ErrorCode string

const (
	ErrEmptySpec           ErrorCode = "empty_spec"
	ErrDuplicateNode       ErrorCode = "duplicate_node_id"
	ErrEmptyNodeID         ErrorCode = "empty_node_id"
	ErrUnknownEdgeNode     ErrorCode = "unknown_edge_node"
	ErrDuplicateEdge       ErrorCode = "duplicate_edge"
	ErrSelfEdge            ErrorCode = "self_edge"
	ErrCycle               ErrorCode = "cycle"
	ErrUnreachableNode     ErrorCode = "unreachable_node"
	ErrUnknownOutput       ErrorCode = "unknown_upstream_output"
	ErrUnknownInput        ErrorCode = "unknown_downstream_input"
	ErrTypeMismatch        ErrorCode = "type_mismatch"
	ErrRequiredInput       ErrorCode = "required_input_unsatisfied"
	ErrInputDoubleBound    ErrorCode = "input_bound_twice"
	ErrUnknownType         ErrorCode = "unknown_value_type"
	ErrBadDefault          ErrorCode = "invalid_default_json"
	ErrTriggerRuleDeferred ErrorCode = "trigger_rule_not_in_phase_1"
	ErrMissingTaskRef      ErrorCode = "missing_task_ref"
	// ErrReservedNodeID: "[" and "]" are reserved for dynamic-expansion child
	// ids ("process[3]"), so an authored id containing them could collide with a
	// child the engine materializes.
	ErrReservedNodeID ErrorCode = "reserved_node_id"

	// Dynamic expansion (§5.4).
	ErrMapMaxFanout ErrorCode = "map_max_fanout_invalid"
	ErrMapSource    ErrorCode = "map_source_invalid"
	ErrMapItemInput ErrorCode = "map_item_input_invalid"
	ErrMapFanInType ErrorCode = "map_fan_in_type_mismatch"

	// Memoization (§6.4).
	ErrCacheTTLRequired   ErrorCode = "cache_ttl_invalid"
	ErrCacheNotIdempotent ErrorCode = "cache_requires_idempotency"
)

// ValidationError is one specific, actionable problem with a spec.
type ValidationError struct {
	Code   ErrorCode
	NodeID string // the node at fault, where attributable
	Field  string // the input/output/edge at fault, where attributable
	Msg    string
	// Path carries the actual cycle for ErrCycle. "cycle detected" without the
	// path is useless on a 200-node graph: the author still has to find it.
	Path []string
}

func (e ValidationError) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.NodeID != "" {
		fmt.Fprintf(&b, " [node=%s]", e.NodeID)
	}
	if e.Field != "" {
		fmt.Fprintf(&b, " [field=%s]", e.Field)
	}
	if len(e.Path) > 0 {
		fmt.Fprintf(&b, " [path=%s]", strings.Join(e.Path, " -> "))
	}
	if e.Msg != "" {
		b.WriteString(": ")
		b.WriteString(e.Msg)
	}
	return b.String()
}

// ValidationErrors is every problem found in one pass.
//
// Returning ALL errors rather than the first is a deliberate ergonomics choice:
// a spec is usually authored or generated in bulk, and fix-one-resubmit-repeat
// turns a 30-second correction into a 20-minute loop. This is the difference
// between a compiler and a syntax-error-at-a-time interpreter.
type ValidationErrors []ValidationError

func (v ValidationErrors) Error() string {
	if len(v) == 0 {
		return "no validation errors"
	}
	parts := make([]string, len(v))
	for i, e := range v {
		parts[i] = e.Error()
	}
	return fmt.Sprintf("%d validation error(s):\n  - %s", len(v), strings.Join(parts, "\n  - "))
}

// Has reports whether any error carries the given code, for tests and for
// callers that branch on a specific failure.
func (v ValidationErrors) Has(code ErrorCode) bool {
	for _, e := range v {
		if e.Code == code {
			return true
		}
	}
	return false
}

// Codes returns the sorted, de-duplicated set of codes present.
func (v ValidationErrors) Codes() []ErrorCode {
	seen := map[ErrorCode]bool{}
	out := []ErrorCode{}
	for _, e := range v {
		if !seen[e.Code] {
			seen[e.Code] = true
			out = append(out, e.Code)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ValueType is the closed set of types a ValueSpec may declare. The set is
// closed on purpose: an open string type would defer every type error to
// runtime, which defeats the point of having a typed interface at all
// (ADR-0004, Flyte's typed-interface idea).
var valueTypes = map[string]bool{
	"int": true, "float": true, "string": true,
	"bool": true, "json": true, "file_ref": true,
}

// assignable reports whether a value of type `from` may flow into an input of
// type `to`.
//
// The rules are deliberately narrow. `json` accepts anything because it is the
// declared escape hatch for unstructured payloads, and int→float is allowed
// because it is lossless and universally expected. Everything else must match
// exactly: silently coercing string→int at the edge would move a type error
// from submit time to 3am, which is the exact failure ADR-0004 exists to
// prevent.
func assignable(from, to string) bool {
	if from == to {
		return true
	}
	if to == "json" {
		return true
	}
	if from == "int" && to == "float" {
		return true
	}
	return false
}

// ValidationOptions tunes the checks that are policy rather than correctness.
type ValidationOptions struct {
	// AllowDeferredTriggerRules accepts the four Phase-2 trigger rules. The pure
	// evaluator implements all six, but accepting a rule at submit time is a
	// product decision: a run that depends on one_failed semantics the rest of
	// the platform has not been exercised against is worse than a rejection.
	AllowDeferredTriggerRules bool

	// AllowUnreachable downgrades unreachable-node detection. An unreachable
	// node in a static DAG is nearly always an authoring mistake (a forgotten
	// edge), so it is an error by default, but a spec generator that emits
	// deliberately isolated nodes can opt out.
	AllowUnreachable bool

	// MaxFanoutCeiling is the operator's upper bound on any template's declared
	// max_fanout. Zero means DefaultMaxFanoutCeiling.
	MaxFanoutCeiling int
}

func (o ValidationOptions) fanoutCeiling() int {
	if o.MaxFanoutCeiling > 0 {
		return o.MaxFanoutCeiling
	}
	return DefaultMaxFanoutCeiling
}

// DefaultValidationOptions is the Phase-1 policy.
func DefaultValidationOptions() ValidationOptions {
	return ValidationOptions{AllowDeferredTriggerRules: false, AllowUnreachable: false}
}

// Validate compiles and fully checks a spec at submit time, returning the graph
// and every problem found. The graph is returned even on failure so callers can
// render a partial DAG for the author.
//
// Checks: empty spec, empty/duplicate node ids, missing task_ref, unknown edge
// endpoints, duplicate and self edges, acyclicity WITH the offending cycle path,
// unreachable nodes, declared-type sanity, edge type compatibility, inputs bound
// twice, and required inputs unsatisfied by any of {edge, param, default}.
func Validate(spec *nexusv1.WorkflowSpec, opts ValidationOptions) (*Graph, ValidationErrors) {
	var errs ValidationErrors
	g := Build(spec)

	if len(spec.GetTasks()) == 0 {
		errs = append(errs, ValidationError{Code: ErrEmptySpec, Msg: "workflow declares no tasks"})
		return g, errs
	}

	errs = append(errs, validateNodes(spec)...)
	errs = append(errs, validateEdgeTopology(spec, g)...)
	errs = append(errs, validateAcyclic(g)...)
	if !opts.AllowUnreachable {
		errs = append(errs, validateReachability(g)...)
	}
	errs = append(errs, validateTriggerRules(g, opts)...)
	errs = append(errs, validateInterfaces(g)...)
	errs = append(errs, validateMaps(g, opts)...)

	sortErrors(errs)
	return g, errs
}

// sortErrors gives a stable, readable ordering: by node, then field, then code.
// Stability matters because these messages end up in test golden output and in
// CI logs that humans diff.
func sortErrors(errs ValidationErrors) {
	sort.SliceStable(errs, func(i, j int) bool {
		a, b := errs[i], errs[j]
		if a.NodeID != b.NodeID {
			return a.NodeID < b.NodeID
		}
		if a.Field != b.Field {
			return a.Field < b.Field
		}
		return a.Code < b.Code
	})
}

func validateNodes(spec *nexusv1.WorkflowSpec) ValidationErrors {
	var errs ValidationErrors
	seen := map[string]int{}
	for _, t := range spec.GetTasks() {
		id := t.GetId()
		if strings.TrimSpace(id) == "" {
			errs = append(errs, ValidationError{Code: ErrEmptyNodeID, Msg: "task has an empty id"})
			continue
		}
		if strings.ContainsAny(id, "[]") {
			errs = append(errs, ValidationError{
				Code: ErrReservedNodeID, NodeID: id,
				Msg: "node ids may not contain '[' or ']'; those characters are reserved for dynamic-expansion children",
			})
		}
		seen[id]++
		if seen[id] == 2 {
			errs = append(errs, ValidationError{
				Code: ErrDuplicateNode, NodeID: id,
				Msg: "node id declared more than once; ids must be unique within a workflow because they participate in the idempotency key",
			})
		}
		if strings.TrimSpace(t.GetTaskRef()) == "" {
			errs = append(errs, ValidationError{
				Code: ErrMissingTaskRef, NodeID: id,
				Msg: "task_ref is empty; the worker cannot resolve a handler",
			})
		}
		for name, vs := range t.GetInputs() {
			errs = append(errs, validateValueSpec(id, "inputs."+name, vs)...)
		}
		for name, vs := range t.GetOutputs() {
			errs = append(errs, validateValueSpec(id, "outputs."+name, vs)...)
		}
	}
	return errs
}

func validateValueSpec(nodeID, field string, vs *nexusv1.ValueSpec) ValidationErrors {
	var errs ValidationErrors
	typ := vs.GetType()
	if !valueTypes[typ] {
		errs = append(errs, ValidationError{
			Code: ErrUnknownType, NodeID: nodeID, Field: field,
			Msg: fmt.Sprintf("unknown type %q; must be one of int,float,string,bool,json,file_ref", typ),
		})
	}
	if d := vs.GetDefaultJson(); d != "" {
		// A default that is not valid JSON would fail at input-resolution time
		// on the worker, long after the author could have fixed it cheaply.
		var any interface{}
		if err := json.Unmarshal([]byte(d), &any); err != nil {
			errs = append(errs, ValidationError{
				Code: ErrBadDefault, NodeID: nodeID, Field: field,
				Msg: fmt.Sprintf("default_json is not valid JSON: %v", err),
			})
		}
	}
	return errs
}

func validateEdgeTopology(spec *nexusv1.WorkflowSpec, g *Graph) ValidationErrors {
	var errs ValidationErrors
	seen := map[[2]string]int{}
	for _, e := range spec.GetEdges() {
		from, to := e.GetFromNodeId(), e.GetToNodeId()
		if g.nodes[from] == nil {
			errs = append(errs, ValidationError{
				Code: ErrUnknownEdgeNode, NodeID: from, Field: fmt.Sprintf("edge %s->%s", from, to),
				Msg: "edge references an unknown from_node_id",
			})
		}
		if g.nodes[to] == nil {
			errs = append(errs, ValidationError{
				Code: ErrUnknownEdgeNode, NodeID: to, Field: fmt.Sprintf("edge %s->%s", from, to),
				Msg: "edge references an unknown to_node_id",
			})
		}
		if from == to && from != "" {
			// A self edge is a one-node cycle. Naming it specifically is kinder
			// than reporting it as a cycle path of length one.
			errs = append(errs, ValidationError{
				Code: ErrSelfEdge, NodeID: from, Field: fmt.Sprintf("edge %s->%s", from, to),
				Msg: "a node cannot depend on itself",
			})
		}
		key := [2]string{from, to}
		seen[key]++
		if seen[key] == 2 {
			errs = append(errs, ValidationError{
				Code: ErrDuplicateEdge, Field: fmt.Sprintf("edge %s->%s", from, to),
				Msg: "edge declared more than once; task_edges is keyed on (run_id, from, to) so the duplicate cannot be materialized",
			})
		}
	}
	return errs
}

// validateAcyclic reports one error per distinct cycle found, each carrying the
// actual node path. The path is the whole point: on any non-trivial graph,
// "cycle detected" leaves the author to bisect their own spec by hand.
func validateAcyclic(g *Graph) ValidationErrors {
	const (
		white = 0 // unvisited
		grey  = 1 // on the current DFS stack
		black = 2 // fully explored
	)
	color := make(map[string]int, len(g.nodes))
	var stack []string
	var errs ValidationErrors
	reported := map[string]bool{}

	// Iterative DFS would avoid a deep-graph stack overflow, but a spec deep
	// enough to blow the Go stack (hundreds of thousands of nodes in a chain) is
	// already rejected by size limits upstream, and recursion keeps the
	// cycle-path reconstruction legible.
	var visit func(string)
	visit = func(n string) {
		color[n] = grey
		stack = append(stack, n)
		for _, s := range g.succs[n] {
			switch color[s] {
			case white:
				visit(s)
			case grey:
				// A grey successor closes a cycle: the path is the current stack
				// from that successor onward, plus the successor again to make
				// the loop explicit in the message.
				idx := 0
				for i, v := range stack {
					if v == s {
						idx = i
						break
					}
				}
				cyc := append([]string(nil), stack[idx:]...)
				cyc = append(cyc, s)
				// Canonicalize so the same cycle reached by two different DFS
				// paths is reported once, not once per entry point.
				if key := canonicalCycle(cyc); !reported[key] {
					reported[key] = true
					errs = append(errs, ValidationError{
						Code: ErrCycle, NodeID: s, Path: cyc,
						Msg: "dependency cycle; a DAG cannot contain one and the run could never make progress",
					})
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
	}

	for _, id := range sortedKeys(g.nodes) {
		if color[id] == white {
			visit(id)
		}
	}
	return errs
}

// canonicalCycle produces a rotation-independent key for a cycle path so the
// same loop discovered from different entry points is deduplicated.
func canonicalCycle(cyc []string) string {
	if len(cyc) <= 1 {
		return strings.Join(cyc, ">")
	}
	ring := cyc[:len(cyc)-1] // drop the repeated closing node
	best := -1
	for i, v := range ring {
		if best == -1 || v < ring[best] {
			best = i
		}
	}
	rot := make([]string, 0, len(ring))
	for i := range ring {
		rot = append(rot, ring[(best+i)%len(ring)])
	}
	return strings.Join(rot, ">")
}

// validateReachability flags nodes with neither an in-edge nor an out-edge.
//
// "Unreachable" here means disconnected, not "not reachable from a root": a
// source node with no predecessors is a legitimate DAG root and must not be
// flagged. A node with no edges at all, in a workflow that has edges, is almost
// always a forgotten dependency.
func validateReachability(g *Graph) ValidationErrors {
	if len(g.edges) == 0 {
		// A workflow of independent tasks is a valid (if degenerate) DAG: every
		// node is a root. Flagging all of them would be noise.
		return nil
	}
	var errs ValidationErrors
	for _, id := range sortedKeys(g.nodes) {
		if len(g.preds[id]) == 0 && len(g.succs[id]) == 0 {
			errs = append(errs, ValidationError{
				Code: ErrUnreachableNode, NodeID: id,
				Msg: "node has no incoming or outgoing edges while the rest of the workflow is connected; likely a forgotten dependency",
			})
		}
	}
	return errs
}

func validateTriggerRules(g *Graph, opts ValidationOptions) ValidationErrors {
	if opts.AllowDeferredTriggerRules {
		return nil
	}
	var errs ValidationErrors
	for _, id := range sortedKeys(g.nodes) {
		if r := g.nodes[id].TriggerRule; !r.Phase1Active() {
			errs = append(errs, ValidationError{
				Code: ErrTriggerRuleDeferred, NodeID: id, Field: "trigger_rule",
				Msg: fmt.Sprintf("trigger rule %q is implemented but deferred to Phase 2 (architecture §11 ships all_success and all_done)", r),
			})
		}
	}
	return errs
}

// validateInterfaces is the typed-interface check (ADR-0004): every edge
// mapping must name a real upstream output and a real downstream input whose
// declared types are compatible, no input may be bound twice, and every required
// input must be satisfied by an edge, a workflow param, or a default.
//
// Catching this at submit time rather than at execution time is the single
// cheapest reliability win available: a type error found now costs a resubmit,
// and the same error found mid-run costs whatever the upstream tasks cost.
func validateInterfaces(g *Graph) ValidationErrors {
	var errs ValidationErrors

	for _, to := range sortedKeys(g.nodes) {
		down := g.nodes[to]
		boundBy := map[string]string{} // downstream input -> upstream node that bound it

		for _, e := range g.edgeByTo[to] {
			up := g.nodes[e.From]
			if up == nil {
				continue // already reported as an unknown endpoint
			}
			inputs := sortedMapKeys(e.OutputToInput)
			for _, inName := range inputs {
				outName := e.OutputToInput[inName]
				field := fmt.Sprintf("edge %s->%s[%s<-%s]", e.From, to, inName, outName)

				inSpec, okIn := down.Spec.GetInputs()[inName]
				if !okIn {
					errs = append(errs, ValidationError{
						Code: ErrUnknownInput, NodeID: to, Field: field,
						Msg: fmt.Sprintf("edge binds input %q which %s does not declare", inName, to),
					})
				}
				outSpec, okOut := up.Spec.GetOutputs()[outName]
				if !okOut {
					errs = append(errs, ValidationError{
						Code: ErrUnknownOutput, NodeID: e.From, Field: field,
						Msg: fmt.Sprintf("edge reads output %q which %s does not declare", outName, e.From),
					})
				}
				if okIn && okOut && !assignable(outSpec.GetType(), inSpec.GetType()) {
					errs = append(errs, ValidationError{
						Code: ErrTypeMismatch, NodeID: to, Field: field,
						Msg: fmt.Sprintf("upstream %s.%s is %s but downstream %s.%s expects %s",
							e.From, outName, outSpec.GetType(), to, inName, inSpec.GetType()),
					})
				}
				if prev, dup := boundBy[inName]; dup {
					errs = append(errs, ValidationError{
						Code: ErrInputDoubleBound, NodeID: to, Field: field,
						Msg: fmt.Sprintf("input %q is bound by both %s and %s; the winner would be arbitrary", inName, prev, e.From),
					})
				} else {
					boundBy[inName] = e.From
				}
			}
		}

		// A required input must have a value from SOMEWHERE. Checking all three
		// sources (edge, workflow param, declared default) rather than only edges
		// is what lets a workflow take runtime parameters without every node
		// needing a synthetic upstream.
		for _, inName := range sortedValueSpecKeys(down.Spec.GetInputs()) {
			vs := down.Spec.GetInputs()[inName]
			if !vs.GetRequired() {
				continue
			}
			if _, bound := boundBy[inName]; bound {
				continue
			}
			if down.IsTemplate() && inName == down.Spec.GetMap().GetItemInput() {
				// Satisfied by the expansion itself, one element per child.
				continue
			}
			if vs.GetDefaultJson() != "" {
				continue
			}
			// F18's bindings satisfy an input too. The validator predates them, so a
			// literal-bound required input was reported unsatisfied even though
			// input resolution delivers it -- and a param bound under a different
			// name was checked against the wrong param.
			if _, lit := down.Spec.GetInputLiterals()[inName]; lit {
				continue
			}
			paramName := inName
			if bound := down.Spec.GetInputParams()[inName]; bound != "" {
				paramName = bound
			}
			if p, ok := g.Params[paramName]; ok {
				// A param can satisfy an input only if its type is compatible;
				// otherwise the mismatch just moves from the edge to the binding.
				if assignable(p.GetType(), vs.GetType()) {
					continue
				}
				errs = append(errs, ValidationError{
					Code: ErrTypeMismatch, NodeID: to, Field: "inputs." + inName,
					Msg: fmt.Sprintf("workflow param %q is %s but input expects %s", inName, p.GetType(), vs.GetType()),
				})
				continue
			}
			errs = append(errs, ValidationError{
				Code: ErrRequiredInput, NodeID: to, Field: "inputs." + inName,
				Msg: fmt.Sprintf("required input %q is satisfied by no edge, workflow param, or default", inName),
			})
		}
	}
	return errs
}

// validateMaps checks every map template (§5.4) at submit time, so an expansion
// that could never work fails on the author's desk rather than mid-run:
//
//   - max_fanout is mandatory, positive, and within the operator's ceiling;
//   - the collection comes from a DIRECT upstream (an edge exists), through an
//     output that upstream declares, typed json (a list has no other type);
//   - item_input is a declared input of the template, bound by no edge (the
//     expansion binds it, per child);
//   - a template's outputs reach downstream only as json, because the value a
//     downstream receives is the ARRAY of child outputs, not one of them;
//   - templates do not nest: a template's children are ordinary tasks.
func validateMaps(g *Graph, opts ValidationOptions) ValidationErrors {
	var errs ValidationErrors
	ceiling := opts.fanoutCeiling()
	for _, id := range sortedKeys(g.nodes) {
		n := g.nodes[id]
		m := n.Spec.GetMap()
		if m == nil {
			continue
		}
		if mf := int(m.GetMaxFanout()); mf <= 0 {
			errs = append(errs, ValidationError{
				Code: ErrMapMaxFanout, NodeID: id, Field: "map.max_fanout",
				Msg: "max_fanout is mandatory and must be positive; an unbounded fan-out is how one runaway upstream takes the database down (§5.4)",
			})
		} else if mf > ceiling {
			errs = append(errs, ValidationError{
				Code: ErrMapMaxFanout, NodeID: id, Field: "map.max_fanout",
				Msg: fmt.Sprintf("max_fanout %d exceeds the platform ceiling of %d", mf, ceiling),
			})
		}

		src := m.GetOverNodeId()
		up := g.nodes[src]
		switch {
		case src == "":
			errs = append(errs, ValidationError{Code: ErrMapSource, NodeID: id, Field: "map.over_node_id",
				Msg: "over_node_id is required: it names the upstream whose output is the collection"})
		case up == nil:
			errs = append(errs, ValidationError{Code: ErrMapSource, NodeID: id, Field: "map.over_node_id",
				Msg: fmt.Sprintf("over_node_id %q is not a task in this workflow", src)})
		case !contains(g.preds[id], src):
			errs = append(errs, ValidationError{Code: ErrMapSource, NodeID: id, Field: "map.over_node_id",
				Msg: fmt.Sprintf("over_node_id %q must be a direct upstream of %s: add an edge %s->%s so the dependency is part of the graph", src, id, src, id)})
		}
		if up != nil {
			outName := m.GetOverOutput()
			outSpec, ok := up.Spec.GetOutputs()[outName]
			switch {
			case outName == "":
				errs = append(errs, ValidationError{Code: ErrMapSource, NodeID: id, Field: "map.over_output",
					Msg: "over_output is required: it names the upstream output holding the JSON array"})
			case !ok:
				errs = append(errs, ValidationError{Code: ErrMapSource, NodeID: id, Field: "map.over_output",
					Msg: fmt.Sprintf("%s declares no output %q", src, outName)})
			case !up.IsTemplate() && outSpec.GetType() != "json":
				// A template's outputs reach downstream as the ARRAY of its
				// children's values, which is json whatever each child declares,
				// so mapping over a template's fan-in is always well-typed.
				errs = append(errs, ValidationError{Code: ErrMapSource, NodeID: id, Field: "map.over_output",
					Msg: fmt.Sprintf("%s.%s is %s; a mapped collection must be json (an array)", src, outName, outSpec.GetType())})
			}
		}

		item := m.GetItemInput()
		if item == "" {
			errs = append(errs, ValidationError{Code: ErrMapItemInput, NodeID: id, Field: "map.item_input",
				Msg: "item_input is required: it names the input each child receives its element through"})
		} else {
			if _, ok := n.Spec.GetInputs()[item]; !ok {
				errs = append(errs, ValidationError{Code: ErrMapItemInput, NodeID: id, Field: "map.item_input",
					Msg: fmt.Sprintf("%s declares no input %q", id, item)})
			}
			for _, e := range g.edgeByTo[id] {
				if _, bound := e.OutputToInput[item]; bound {
					errs = append(errs, ValidationError{Code: ErrMapItemInput, NodeID: id,
						Field: fmt.Sprintf("edge %s->%s[%s]", e.From, id, item),
						Msg:   fmt.Sprintf("input %q is bound by the map, one element per child; an edge binding it would give every child the whole value", item)})
				}
			}
			if _, lit := n.Spec.GetInputLiterals()[item]; lit {
				errs = append(errs, ValidationError{Code: ErrMapItemInput, NodeID: id, Field: "input_literals." + item,
					Msg: fmt.Sprintf("input %q is bound by the map and cannot also be a literal", item)})
			}
		}

		for _, down := range g.succs[id] {
			dn := g.nodes[down]
			for _, e := range g.edgeByTo[down] {
				if e.From != id {
					continue
				}
				for inName := range e.OutputToInput {
					if vs, ok := dn.Spec.GetInputs()[inName]; ok && vs.GetType() != "json" {
						errs = append(errs, ValidationError{Code: ErrMapFanInType, NodeID: down,
							Field: fmt.Sprintf("edge %s->%s[%s]", id, down, inName),
							Msg:   fmt.Sprintf("%s is a map template, so %s receives a JSON array of its children's outputs; declare input %q as json", id, down, inName)})
					}
				}
			}
		}
	}
	return errs
}

// DefaultCacheTTLMs applies when a task enables memoization without a TTL:
// §4.3's illustrative spec uses 7d, and a finite default means an entry cannot
// outlive every reason it was valid just because the author omitted a field.
const DefaultCacheTTLMs int64 = 7 * 24 * 60 * 60 * 1000

// CacheTTLMs is the effective memoization TTL for a task.
func CacheTTLMs(spec *nexusv1.TaskSpec) int64 {
	if ttl := spec.GetCache().GetTtlMs(); ttl > 0 {
		return ttl
	}
	return DefaultCacheTTLMs
}

// Memoizable reports whether a task's results may be reused (§6.4): opt-in,
// and never for work that declared it is not idempotent. A task that cannot be
// run twice safely is certainly not pure enough to have its result reused, and
// memoizing it would hide the one execution the author said must happen.
func Memoizable(spec *nexusv1.TaskSpec) bool {
	return spec.GetCache().GetEnabled() &&
		spec.GetIdempotency() != nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_NONE
}

// ValidateCache is the SUBMIT-TIME memoization check.
//
// Deliberately not part of Validate: Validate also runs when the DAG engine
// materializes a stored spec, and a rule added now must not make a spec that
// was accepted yesterday impossible to run today (ADR-0011 rule 5). The runtime
// enforces the same rule anyway -- Memoizable is false for idempotency:none --
// so an old spec loses only the (never-delivered) memoization, not its run.
func ValidateCache(spec *nexusv1.WorkflowSpec) ValidationErrors {
	var errs ValidationErrors
	for _, t := range spec.GetTasks() {
		c := t.GetCache()
		if !c.GetEnabled() {
			continue
		}
		if c.GetTtlMs() < 0 {
			errs = append(errs, ValidationError{Code: ErrCacheTTLRequired, NodeID: t.GetId(), Field: "cache.ttl_ms",
				Msg: "ttl_ms must not be negative"})
		}
		if t.GetIdempotency() == nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_NONE {
			errs = append(errs, ValidationError{Code: ErrCacheNotIdempotent, NodeID: t.GetId(), Field: "cache.enabled",
				Msg: "idempotency: none cannot be memoized; memoization is only valid for pure tasks"})
		}
	}
	sortErrors(errs)
	return errs
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedValueSpecKeys(m map[string]*nexusv1.ValueSpec) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AsError collapses ValidationErrors into an error, or nil when empty, so
// callers can use ordinary error handling.
func (v ValidationErrors) AsError() error {
	if len(v) == 0 {
		return nil
	}
	return errors.New(v.Error())
}
