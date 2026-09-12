// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package dag

import (
	"fmt"
	"sort"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// ── A note on the direction of Edge.output_to_input ───────────────────────────
//
// The field NAME reads as "upstream output -> downstream input", but the frozen
// proto comment on nexusv1.Edge states the opposite mapping:
//
//	"Maps upstream outputs to downstream inputs: key = downstream input name,
//	 value = upstream output name."
//
// We follow the COMMENT, because the comment describes the wire semantics other
// services will implement and the proto is frozen. Key = downstream input is
// also the only direction that works: a downstream input must have exactly one
// source, while one upstream output may legitimately feed several downstream
// inputs, so keying by input is the direction that is a function. Keying by
// output would make the map lossy for fan-out.
//
// The naming mismatch is reported in the handover notes rather than patched here.

// InputBinding is one resolved downstream input: where its value comes from.
type InputBinding struct {
	InputName string
	// UpstreamNode and UpstreamOutput are set when the value comes from an edge.
	UpstreamNode   string
	UpstreamOutput string
	// DefaultJSON is set when the value comes from the task's declared default.
	DefaultJSON string
	// FromParam is set when the value comes from a workflow-level param.
	FromParam bool
	Required  bool
	Type      string
}

// Source classifies where a binding's value comes from, for logging and for the
// service layer's dispatch on how to fetch it.
type BindingSource string

const (
	FromEdge     BindingSource = "edge"
	FromDefault  BindingSource = "default"
	FromParamSrc BindingSource = "param"
)

// Source reports the binding's origin.
func (b InputBinding) Source() BindingSource {
	switch {
	case b.UpstreamNode != "":
		return FromEdge
	case b.FromParam:
		return FromParamSrc
	default:
		return FromDefault
	}
}

// InputPlan is the complete, deterministic set of bindings for one node. It is
// computed purely from the graph, with no attempt data, so it can be built at
// materialization time and checked in tests.
//
// Resolution is split into a pure PLAN (this) and an impure FETCH (the service
// layer looking up upstream output_refs). That split is what lets the interesting
// half — which input reads which upstream output — be unit-tested without a
// database.
func InputPlan(g *Graph, nodeID string) ([]InputBinding, error) {
	n := g.nodes[nodeID]
	if n == nil {
		return nil, fmt.Errorf("input plan: unknown node %q", nodeID)
	}

	bound := map[string]InputBinding{}
	for _, e := range g.edgeByTo[nodeID] {
		for inName, outName := range e.OutputToInput {
			vs := n.Spec.GetInputs()[inName]
			bound[inName] = InputBinding{
				InputName:      inName,
				UpstreamNode:   e.From,
				UpstreamOutput: outName,
				Required:       vs.GetRequired(),
				Type:           vs.GetType(),
			}
		}
	}

	for inName, vs := range n.Spec.GetInputs() {
		if _, ok := bound[inName]; ok {
			continue
		}
		b := InputBinding{InputName: inName, Required: vs.GetRequired(), Type: vs.GetType()}
		switch {
		case g.Params[inName] != nil:
			// Edge beats param beats default. A node wired to an upstream must not
			// silently read a run param instead — that would make the graph a lie.
			b.FromParam = true
		case vs.GetDefaultJson() != "":
			b.DefaultJSON = vs.GetDefaultJson()
		case vs.GetRequired():
			// Unreachable for a validated spec; returning an error rather than
			// panicking keeps this usable on specs that bypassed Validate.
			return nil, fmt.Errorf("input plan: node %q required input %q has no source", nodeID, inName)
		default:
			// Optional and unbound: omitted from the assignment entirely, so the
			// handler sees "absent" rather than a synthetic zero value.
			continue
		}
		bound[inName] = b
	}

	out := make([]InputBinding, 0, len(bound))
	for _, k := range sortedBindingKeys(bound) {
		out = append(out, bound[k])
	}
	return out, nil
}

func sortedBindingKeys(m map[string]InputBinding) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── Dynamic expansion (§5.4) — DESIGNED, DEFERRED from Phase 1 ────────────────
//
// Architecture §11 defers expansion to Phase 3. The types below exist so the
// pure engine's shape does not have to change when it lands, because the thing
// that makes expansion safe is a property of the GRAPH, not of the service:
// children are real nodes, so they must flow through the same trigger-rule,
// propagation, and terminal-detection code that static nodes do. Bolting
// expansion on later as a parallel path would give map children second-class
// state handling, which is exactly the footgun §5.4 warns against.
//
// Nothing here is called by Phase-1 code. It is deliberately small: the contract
// (what an expansion request is, what it produces) rather than an implementation
// that would rot before Phase 3.

// ExpansionRequest is the engine's view of "materialize N children of this
// template node".
type ExpansionRequest struct {
	TemplateNodeID string
	// Cardinality comes from the upstream task's output at runtime.
	Cardinality int
	// MaxFanout is MANDATORY in the spec (§5.4). An upstream returning ten
	// million items must fail the expansion explicitly rather than silently
	// trying to insert ten million rows and taking the database down with it.
	MaxFanout int
}

// ExpandedChild is one materialized map child. It carries ParentNodeID and
// MapIndex because task_nodes already has those columns, and because map_index
// participates in the idempotency key — retrying child 7 must re-run exactly
// child 7's work (§5.4, §6.2).
type ExpandedChild struct {
	NodeID       string
	ParentNodeID string
	MapIndex     int
}

// PlanExpansion is the pure half of dynamic expansion: it validates the fanout
// bound and names the children deterministically. Deterministic child ids matter
// because the idempotency key is derived from the node id, so a non-deterministic
// naming scheme would break at-least-once on retry.
//
// DEFERRED: not reachable from Phase-1 code paths. Present and tested so the
// Phase-3 work is a service-layer change, not an engine redesign.
func PlanExpansion(req ExpansionRequest) ([]ExpandedChild, error) {
	if req.MaxFanout <= 0 {
		return nil, fmt.Errorf("expansion of %q: max_fanout is mandatory and must be positive (§5.4)", req.TemplateNodeID)
	}
	if req.Cardinality < 0 {
		return nil, fmt.Errorf("expansion of %q: negative cardinality %d", req.TemplateNodeID, req.Cardinality)
	}
	if req.Cardinality > req.MaxFanout {
		return nil, fmt.Errorf("expansion of %q: cardinality %d exceeds max_fanout %d; failing explicitly rather than materializing an unbounded graph",
			req.TemplateNodeID, req.Cardinality, req.MaxFanout)
	}
	out := make([]ExpandedChild, 0, req.Cardinality)
	for i := 0; i < req.Cardinality; i++ {
		out = append(out, ExpandedChild{
			NodeID:       fmt.Sprintf("%s[%d]", req.TemplateNodeID, i),
			ParentNodeID: req.TemplateNodeID,
			MapIndex:     i,
		})
	}
	return out, nil
}

// AttemptStateToNode folds a terminal ATTEMPT state into the NODE state the
// engine reasons over, given whether the node's retry budget is exhausted.
//
// This is where §5.2's "TIMED_OUT" and CONTRACT.md's node-state enum are
// reconciled: TIMED_OUT and LOST are attempt states with no node-state
// counterpart, so a retry-exhausted attempt in either becomes a FAILED node.
// That is the correct reading of §5.2 — "at least one node terminally failed
// AFTER RETRIES EXHAUSTED" — and it keeps the disqualifying set of §5.2
// expressible in the node-state vocabulary the schema actually has.
//
// While retries remain, the node stays RUNNING: the logical unit of work has not
// finished, only this attempt has, and reporting the node as FAILED between
// attempts would make a retrying run look broken and could trip fail_fast.
func AttemptStateToNode(attempt string, retriesExhausted bool) NodeState {
	switch attempt {
	case "SUCCEEDED":
		return NodeSucceeded
	case "CANCELLED":
		return NodeCancelled
	case "FAILED", "TIMED_OUT", "LOST":
		if retriesExhausted {
			return NodeFailed
		}
		return NodeRunning
	case "PENDING":
		return NodeReady
	case "DISPATCHED", "RUNNING":
		return NodeRunning
	default:
		return NodePending
	}
}

// AttemptStateFromProto renders the wire enum as the canonical CONTRACT.md name,
// so the service layer and AttemptStateToNode speak one vocabulary.
func AttemptStateFromProto(s nexusv1.AttemptState) string {
	switch s {
	case nexusv1.AttemptState_ATTEMPT_STATE_PENDING:
		return "PENDING"
	case nexusv1.AttemptState_ATTEMPT_STATE_DISPATCHED:
		return "DISPATCHED"
	case nexusv1.AttemptState_ATTEMPT_STATE_RUNNING:
		return "RUNNING"
	case nexusv1.AttemptState_ATTEMPT_STATE_SUCCEEDED:
		return "SUCCEEDED"
	case nexusv1.AttemptState_ATTEMPT_STATE_FAILED:
		return "FAILED"
	case nexusv1.AttemptState_ATTEMPT_STATE_TIMED_OUT:
		return "TIMED_OUT"
	case nexusv1.AttemptState_ATTEMPT_STATE_LOST:
		return "LOST"
	case nexusv1.AttemptState_ATTEMPT_STATE_CANCELLED:
		return "CANCELLED"
	default:
		return ""
	}
}
