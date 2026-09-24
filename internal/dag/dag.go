// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package dag is the pure dependency-logic core of the DAG engine.
//
// Architecture §3.2 specifies the engine as a function
// (dag_spec, current_attempt_states) → set of transitions, with no I/O. That
// purity is not stylistic: dependency logic is the part of an orchestrator
// where a subtle bug silently corrupts every run, and the only affordable way
// to gain confidence is exhaustive table-driven plus property-based testing.
// Anything that touches a database, a clock, or a network cannot be enumerated
// that way, so none of it lives here. All I/O belongs to services/dag-engine.
//
// Nothing in this package reads the wall clock, opens a connection, logs, or
// mutates global state. Every exported function is deterministic in its
// arguments.
package dag

import (
	"fmt"
	"sort"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// NodeState mirrors nexusv1.NodeState but as a small comparable value so the
// pure core can be tested and reasoned about without constructing protobufs.
// The names are the canonical ones from CONTRACT.md.
type NodeState string

const (
	NodePending        NodeState = "PENDING"
	NodeReady          NodeState = "READY"
	NodeRunning        NodeState = "RUNNING"
	NodeSucceeded      NodeState = "SUCCEEDED"
	NodeFailed         NodeState = "FAILED"
	NodeSkipped        NodeState = "SKIPPED"
	NodeUpstreamFailed NodeState = "UPSTREAM_FAILED"
	NodeCancelled      NodeState = "CANCELLED"
)

// RunState mirrors nexusv1.RunState. Only the states the pure engine can
// conclude on its own are produced here; PAUSING/PAUSED/CANCELLING are operator
// interventions owned by the workflow manager, and the engine merely respects
// them as inputs.
type RunState string

const (
	RunPending    RunState = "PENDING"
	RunRunning    RunState = "RUNNING"
	RunPausing    RunState = "PAUSING"
	RunPaused     RunState = "PAUSED"
	RunSucceeded  RunState = "SUCCEEDED"
	RunFailed     RunState = "FAILED"
	RunCancelling RunState = "CANCELLING"
	RunCancelled  RunState = "CANCELLED"
	RunTimedOut   RunState = "TIMED_OUT"
)

// TriggerRule is Airflow's taxonomy, adopted directly (architecture §3.2).
type TriggerRule string

const (
	AllSuccess TriggerRule = "all_success"
	AllDone    TriggerRule = "all_done"
	OneSuccess TriggerRule = "one_success"
	OneFailed  TriggerRule = "one_failed"
	NoneFailed TriggerRule = "none_failed"
	AllSkipped TriggerRule = "all_skipped"
)

// Phase1Active reports whether this rule is in the Phase-1 MVP scope
// (architecture §11: all_success and all_done only). The remaining rules are
// implemented and tested here because the evaluator makes them nearly free, but
// they are flagged so submit-time validation can reject them until Phase 2
// rather than silently accepting semantics the rest of the platform has not
// been exercised against.
func (r TriggerRule) Phase1Active() bool {
	return r == AllSuccess || r == AllDone
}

// Terminal reports whether a node state admits no further transition.
// SKIPPED and UPSTREAM_FAILED are terminal: a node that was never going to run
// is as finished as one that ran.
func (s NodeState) Terminal() bool {
	switch s {
	case NodeSucceeded, NodeFailed, NodeSkipped, NodeUpstreamFailed, NodeCancelled:
		return true
	}
	return false
}

// Failed reports whether a node state is one of the "this run did not succeed"
// states named in architecture §5.2. UPSTREAM_FAILED counts: a run whose tail
// never ran because its head broke has not succeeded, even though nothing in
// the tail itself failed.
//
// §5.2 lists {FAILED, UPSTREAM_FAILED, TIMED_OUT} as the disqualifying set.
// TIMED_OUT is an *attempt* state, not a node state (CONTRACT.md), so a
// timed-out attempt whose retries are exhausted surfaces here as a FAILED node;
// see Reduce in the service layer.
func (s NodeState) Failed() bool {
	switch s {
	case NodeFailed, NodeUpstreamFailed:
		return true
	}
	return false
}

// Node is one vertex of the compiled graph.
type Node struct {
	ID          string
	TaskRef     string
	TriggerRule TriggerRule
	Spec        *nexusv1.TaskSpec

	// ParentID and MapIndex are set only on a dynamic-expansion child (§5.4):
	// the template it was materialized from and its position in the collection.
	// MapIndex is -1 on every other node, so a zero value can never be mistaken
	// for "child 0".
	ParentID string
	MapIndex int
}

// IsTemplate reports whether this node is a map template: it declares a
// MapSpec and therefore never executes itself, but expands into children.
func (n *Node) IsTemplate() bool { return n != nil && n.Spec.GetMap() != nil && n.ParentID == "" }

// IsChild reports whether this node was materialized by a dynamic expansion.
func (n *Node) IsChild() bool { return n != nil && n.ParentID != "" }

// Edge is one dependency. OutputToInput maps a DOWNSTREAM INPUT NAME to an
// UPSTREAM OUTPUT NAME, matching the frozen proto comment on nexusv1.Edge
// (key = downstream input, value = upstream output). See the package note in
// resolve.go: this is the reverse of the field's name read literally, and the
// proto comment is authoritative because the wire format is frozen.
type Edge struct {
	From          string
	To            string
	OutputToInput map[string]string
}

// Graph is an immutable compiled DAG. Adjacency is precomputed because the
// frontier computation walks predecessors of every non-terminal node on every
// reconcile, and recomputing adjacency per call would make reconcile quadratic
// in edges for no reason.
//
// All slices are kept in deterministic (sorted) order. Determinism is what
// makes the property tests meaningful: a flaky iteration order would turn a
// real ordering bug into an intermittent failure.
type Graph struct {
	nodes    map[string]*Node
	order    []string // topological order; empty if the graph is cyclic
	edges    []Edge
	preds    map[string][]string
	succs    map[string][]string
	edgeByTo map[string][]Edge
	// children maps a template id to its expanded children, ordered by
	// map_index. Empty for a template that has not expanded (or expanded to an
	// empty collection).
	children map[string][]string

	// FailFast mirrors WorkflowSpec.fail_fast: stop dispatching NEW work on the
	// first terminal failure. It does not cancel in-flight work, which is what
	// distinguishes it from cancellation (§5.2).
	FailFast bool

	// Params are the workflow-level declarations, needed at validation time to
	// decide whether a required input with no incoming edge is nonetheless
	// satisfiable.
	Params map[string]*nexusv1.ValueSpec
}

// Nodes returns the node ids in deterministic topological order where the graph
// is acyclic, and sorted order otherwise.
func (g *Graph) Nodes() []string {
	out := make([]string, len(g.order))
	copy(out, g.order)
	return out
}

// Node returns the compiled node, or nil.
func (g *Graph) Node(id string) *Node { return g.nodes[id] }

// Edges returns all edges in deterministic order.
func (g *Graph) Edges() []Edge {
	out := make([]Edge, len(g.edges))
	copy(out, g.edges)
	return out
}

// Preds returns the upstream node ids of id, in deterministic order.
func (g *Graph) Preds(id string) []string { return append([]string(nil), g.preds[id]...) }

// Succs returns the downstream node ids of id, in deterministic order.
func (g *Graph) Succs(id string) []string { return append([]string(nil), g.succs[id]...) }

// EdgesTo returns the edges whose To is id, in deterministic order. The service
// layer uses this to resolve downstream inputs from upstream outputs.
func (g *Graph) EdgesTo(id string) []Edge { return append([]Edge(nil), g.edgeByTo[id]...) }

// TriggerRuleFromProto maps the wire enum onto the internal rule, defaulting to
// all_success. Defaulting rather than erroring on UNSPECIFIED is deliberate:
// proto3 cannot distinguish "absent" from "zero", and all_success is the
// documented default (architecture §3.2), so an author who omits the field gets
// the intuitive behaviour.
func TriggerRuleFromProto(r nexusv1.TriggerRule) TriggerRule {
	switch r {
	case nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE:
		return AllDone
	case nexusv1.TriggerRule_TRIGGER_RULE_ONE_SUCCESS:
		return OneSuccess
	case nexusv1.TriggerRule_TRIGGER_RULE_ONE_FAILED:
		return OneFailed
	case nexusv1.TriggerRule_TRIGGER_RULE_NONE_FAILED:
		return NoneFailed
	case nexusv1.TriggerRule_TRIGGER_RULE_ALL_SKIPPED:
		return AllSkipped
	default:
		return AllSuccess
	}
}

// NodeStateFromProto maps the wire enum onto the internal state.
func NodeStateFromProto(s nexusv1.NodeState) NodeState {
	switch s {
	case nexusv1.NodeState_NODE_STATE_READY:
		return NodeReady
	case nexusv1.NodeState_NODE_STATE_RUNNING:
		return NodeRunning
	case nexusv1.NodeState_NODE_STATE_SUCCEEDED:
		return NodeSucceeded
	case nexusv1.NodeState_NODE_STATE_FAILED:
		return NodeFailed
	case nexusv1.NodeState_NODE_STATE_SKIPPED:
		return NodeSkipped
	case nexusv1.NodeState_NODE_STATE_UPSTREAM_FAILED:
		return NodeUpstreamFailed
	case nexusv1.NodeState_NODE_STATE_CANCELLED:
		return NodeCancelled
	default:
		return NodePending
	}
}

// NodeStateToProto is the inverse of NodeStateFromProto.
func NodeStateToProto(s NodeState) nexusv1.NodeState {
	switch s {
	case NodeReady:
		return nexusv1.NodeState_NODE_STATE_READY
	case NodeRunning:
		return nexusv1.NodeState_NODE_STATE_RUNNING
	case NodeSucceeded:
		return nexusv1.NodeState_NODE_STATE_SUCCEEDED
	case NodeFailed:
		return nexusv1.NodeState_NODE_STATE_FAILED
	case NodeSkipped:
		return nexusv1.NodeState_NODE_STATE_SKIPPED
	case NodeUpstreamFailed:
		return nexusv1.NodeState_NODE_STATE_UPSTREAM_FAILED
	case NodeCancelled:
		return nexusv1.NodeState_NODE_STATE_CANCELLED
	default:
		return nexusv1.NodeState_NODE_STATE_PENDING
	}
}

// Children returns the expanded children of a template, ordered by map_index.
func (g *Graph) Children(id string) []string { return append([]string(nil), g.children[id]...) }

// Templates returns the map template node ids in deterministic order.
func (g *Graph) Templates() []string {
	var out []string
	for _, id := range g.order {
		if g.nodes[id].IsTemplate() {
			out = append(out, id)
		}
	}
	return out
}

// ChildRef locates one materialized expansion child within its template.
type ChildRef struct {
	Parent   string
	MapIndex int
}

// Build compiles a WorkflowSpec into a Graph WITHOUT validating it. Callers that
// accept an untrusted spec must use Validate instead; Build exists separately so
// the validator can inspect a malformed (even cyclic) graph in order to report
// what is wrong with it.
func Build(spec *nexusv1.WorkflowSpec) *Graph { return BuildWithChildren(spec, nil) }

// BuildWithChildren is Build for a RUN's graph, which may contain nodes that
// dynamic expansion materialized after submission. `children` names which of
// the spec's tasks are expansion children and where they sit; every other task
// is an ordinary node or a template.
//
// Children are carried as ordinary nodes rather than as a side table because
// that is the property §5.4 asks for: they flow through exactly the same
// trigger-rule, propagation and terminal-detection code as static nodes, so a
// child cannot end up with second-class state handling.
func BuildWithChildren(spec *nexusv1.WorkflowSpec, children map[string]ChildRef) *Graph {
	g := &Graph{
		nodes:    make(map[string]*Node, len(spec.GetTasks())),
		preds:    make(map[string][]string),
		succs:    make(map[string][]string),
		edgeByTo: make(map[string][]Edge),
		children: make(map[string][]string),
		FailFast: spec.GetFailFast(),
		Params:   spec.GetParams(),
	}

	// A duplicate id is a validation error, not a build error. Keeping the
	// FIRST occurrence rather than the last makes the error message stable and
	// makes Build total, so the validator never has to guard against nil.
	for _, t := range spec.GetTasks() {
		id := t.GetId()
		if _, dup := g.nodes[id]; dup {
			continue
		}
		n := &Node{
			ID:          id,
			TaskRef:     t.GetTaskRef(),
			TriggerRule: TriggerRuleFromProto(t.GetTriggerRule()),
			Spec:        t,
			MapIndex:    -1,
		}
		if ref, ok := children[id]; ok {
			n.ParentID, n.MapIndex = ref.Parent, ref.MapIndex
		}
		g.nodes[id] = n
	}
	for id, n := range g.nodes {
		if n.ParentID != "" && g.nodes[n.ParentID] != nil {
			g.children[n.ParentID] = append(g.children[n.ParentID], id)
		}
	}
	for parent, kids := range g.children {
		sort.Slice(kids, func(i, j int) bool { return g.nodes[kids[i]].MapIndex < g.nodes[kids[j]].MapIndex })
		g.children[parent] = kids
	}

	seenEdge := make(map[[2]string]bool, len(spec.GetEdges()))
	for _, e := range spec.GetEdges() {
		key := [2]string{e.GetFromNodeId(), e.GetToNodeId()}
		// task_edges is PRIMARY KEY (run_id, from_node_id, to_node_id), so a
		// duplicate edge cannot be materialized. Collapsing here (rather than
		// failing) keeps Build total; the validator reports it.
		if seenEdge[key] {
			continue
		}
		seenEdge[key] = true
		ed := Edge{From: e.GetFromNodeId(), To: e.GetToNodeId(), OutputToInput: e.GetOutputToInput()}
		g.edges = append(g.edges, ed)
	}

	sort.Slice(g.edges, func(i, j int) bool {
		if g.edges[i].From != g.edges[j].From {
			return g.edges[i].From < g.edges[j].From
		}
		return g.edges[i].To < g.edges[j].To
	})

	for _, e := range g.edges {
		// Adjacency is only built for edges whose endpoints both exist. An edge
		// referencing an unknown node is reported by the validator; admitting it
		// into adjacency would make a phantom node look like a real dependency
		// and could wedge the frontier forever.
		if g.nodes[e.From] == nil || g.nodes[e.To] == nil {
			continue
		}
		g.preds[e.To] = append(g.preds[e.To], e.From)
		g.succs[e.From] = append(g.succs[e.From], e.To)
		g.edgeByTo[e.To] = append(g.edgeByTo[e.To], e)
	}

	g.order = topoOrder(g)
	if len(g.order) != len(g.nodes) {
		// Cyclic: fall back to sorted order so Nodes() is still deterministic
		// and total. Validate reports the cycle.
		g.order = sortedKeys(g.nodes)
	}
	return g
}

func sortedKeys(m map[string]*Node) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// topoOrder returns a deterministic (lexicographically smallest-first) Kahn
// ordering, or a short slice if the graph has a cycle.
func topoOrder(g *Graph) []string {
	indeg := make(map[string]int, len(g.nodes))
	for id := range g.nodes {
		indeg[id] = len(g.preds[id])
	}
	// Lexicographic tie-breaking rather than map order: a stable topological
	// order means node materialization and event sequence numbers are
	// reproducible for the same spec, which matters when comparing two runs.
	ready := make([]string, 0, len(g.nodes))
	for id, d := range indeg {
		if d == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)

	out := make([]string, 0, len(g.nodes))
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		out = append(out, n)
		for _, s := range g.succs[n] {
			indeg[s]--
			if indeg[s] == 0 {
				ready = append(ready, s)
				sort.Strings(ready)
			}
		}
	}
	return out
}

// String renders the graph compactly for test failure messages.
func (g *Graph) String() string {
	return fmt.Sprintf("Graph{nodes:%v edges:%d failFast:%v}", g.Nodes(), len(g.edges), g.FailFast)
}
