// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package dag

import (
	"fmt"
	"sort"
	"strings"
)

// Operator retry planning: which nodes an operator's retry resets, and to what.
//
// Pure, like the rest of this package, so the semantics can be table-tested
// without a database and so every caller -- the engine applying a retry and any
// component validating one -- gets the same answer from the same function.
//
// THE SEMANTICS (CONTRACT-FINDINGS F100):
//
//   - An operator SELECTS nodes. A selected node must be in a retryable
//     terminal state: FAILED, UPSTREAM_FAILED or CANCELLED. SUCCEEDED is
//     refused (there is nothing to retry and re-running it would repeat a
//     completed effect); SKIPPED is refused (a skip is a decision -- a branch
//     not taken, or work fail_fast declined -- not a fault); PENDING, READY and
//     RUNNING are refused (the work has not finished, so there is nothing to
//     retry yet).
//   - Every DOWNSTREAM dependent of a selected node that did not succeed is
//     reset with it: FAILED, UPSTREAM_FAILED, CANCELLED and SKIPPED
//     descendants alike. They are reset because their outcome was computed from
//     the selected node's failure, and after the retry that input may differ.
//     SUCCEEDED descendants (e.g. an all_done join that ran anyway) are left
//     alone: succeeded work is never re-run by a retry.
//   - UPSTREAM work is never touched. A selected node re-runs against the
//     outputs its succeeded upstreams already produced.
//   - A reset node returns to PENDING and the ordinary reconcile decides what
//     happens next, so a retried graph is evaluated by exactly the rules that
//     evaluated it the first time. A reset SKIPPED node whose branch is still
//     not taken is simply skipped again.
//   - A selected node that would be settled again at once is refused rather
//     than silently re-failed. Selecting an UPSTREAM_FAILED node whose failed
//     ancestor is not also selected is the common case: the reset would last
//     until the next reconcile and then report the same failure, which reads as
//     "retry is broken". The refusal names the ancestor to select instead.
//   - With NO selection the whole run is retried: every node that did not
//     succeed is selected, SKIPPED included (fail_fast skips independent
//     branches, and a whole-run retry must re-evaluate them). This is the
//     original retry-from-failed.
//
// Dynamic expansion (§5.4) needs three extra rules, because a template's state is
// written by its expansion and folded from its children rather than derived from
// attempts:
//
//   - An EXPANDED template that is reset goes to RUNNING, not PENDING: its
//     expansion record exists, so returning it to PENDING would ask for a second
//     expansion that the record's primary key refuses, and the template would
//     wait forever. RUNNING lets the ordinary fold settle it from its children.
//     Reached as a selection or as a descendant, its non-succeeded children are
//     reset too.
//   - A selected CHILD resets its template to RUNNING and the template's
//     downstream, but NOT its siblings: the operator asked for that child.
//   - A template whose expansion was REFUSED (over max_fanout, not an array...)
//     is not retryable. The refusal is a deterministic function of an upstream
//     output that the retry does not change, so it would be refused again.
//     Reached as a descendant it is left as it is.

// ExpansionStatus is what a map template's expansion record says.
type ExpansionStatus int

const (
	// NotExpanded: no expansion record. The template has never fanned out.
	NotExpanded ExpansionStatus = iota
	// Expanded: the children exist (possibly zero of them).
	Expanded
	// ExpansionRefused: the expansion was deliberately refused.
	ExpansionRefused
)

// RetryInput is everything PlanRetry needs.
type RetryInput struct {
	Graph  *Graph
	States States
	// Selected are the nodes the operator named. Empty retries the whole run.
	Selected []string
	// Expansions maps a template id to its expansion record's status. A template
	// absent from the map has not been expanded.
	Expansions map[string]ExpansionStatus
	// InFlight names nodes with a non-terminal attempt. A terminal-looking node
	// with a live attempt is refused rather than reset, because resetting it
	// would let a second attempt start beside the first.
	InFlight map[string]bool
}

// NodeReset is one node a retry resets, with the state it is expected to be in:
// the caller applies it as a compare-and-set, so a node that moved between
// planning and applying makes the whole retry fail rather than apply half of it.
type NodeReset struct {
	NodeID string
	From   NodeState
	To     NodeState
}

// RetryPlan is the result of planning a retry.
type RetryPlan struct {
	// Selected is the effective selection, sorted: the operator's nodes, or
	// the whole-run default when none were named.
	Selected []string
	// Resets is every node the retry changes, sorted by node id. It may be
	// empty for a whole-run retry of a run with nothing to reset (a TIMED_OUT
	// run whose remaining nodes are merely PENDING).
	Resets []NodeReset
}

// NodeRefusal explains why one selected node cannot be retried.
type NodeRefusal struct {
	NodeID string
	State  NodeState
	// Reason is a stable machine-readable code.
	Reason string
	// Cause names the node responsible, where there is one (the failed ancestor
	// of a node that would be settled again).
	Cause string
}

// Explain renders the refusal for a human, naming what to do instead.
func (r NodeRefusal) Explain() string {
	switch r.Reason {
	case RefusalWouldResettle:
		return fmt.Sprintf("node %s is %s and would fail again at once: it depends on %s, which is not being retried; select %s as well",
			r.NodeID, r.State, r.Cause, r.Cause)
	case RefusalFailFastHalted:
		return fmt.Sprintf("node %s cannot run while node %s stays failed, because the workflow is fail_fast; select %s as well",
			r.NodeID, r.Cause, r.Cause)
	case RefusalExpansionRefused:
		return fmt.Sprintf("node %s is a map template whose expansion was refused; the same upstream output would be refused again", r.NodeID)
	case RefusalAttemptInFlight:
		return fmt.Sprintf("node %s still has an attempt in flight", r.NodeID)
	default:
		return fmt.Sprintf("node %s is %s; only FAILED, UPSTREAM_FAILED and CANCELLED nodes can be retried", r.NodeID, r.State)
	}
}

// Refusal reasons. Stable codes, surfaced to API clients.
const (
	RefusalNotRetryableState = "state_not_retryable"
	RefusalAttemptInFlight   = "attempt_in_flight"
	RefusalExpansionRefused  = "expansion_refused"
	RefusalWouldResettle     = "would_fail_again"
	RefusalFailFastHalted    = "fail_fast_halted"
)

// RetryError is a refused retry. A retry is all-or-nothing: if any selected
// node is unknown or not retryable nothing is reset, so an operator never has
// to work out which half of their request took effect.
type RetryError struct {
	// Unknown are selected ids that are not nodes of this run.
	Unknown []string
	// Refused are selected nodes that exist but cannot be retried.
	Refused []NodeRefusal
}

func (e *RetryError) Error() string {
	var parts []string
	if len(e.Unknown) > 0 {
		parts = append(parts, "unknown nodes: "+strings.Join(e.Unknown, ", "))
	}
	for _, r := range e.Refused {
		parts = append(parts, r.Explain())
	}
	return "retry refused: " + strings.Join(parts, "; ")
}

// RetryableSelection reports whether a node in this state may be named in a
// retry. Exported so a caller validating a request before it reaches the engine
// applies the same rule the engine does.
func RetryableSelection(s NodeState) bool {
	switch s {
	case NodeFailed, NodeUpstreamFailed, NodeCancelled:
		return true
	}
	return false
}

// resettable reports whether a node in this state is reset when a retry reaches
// it. Wider than RetryableSelection by SKIPPED: a skip computed from a failure
// that is being retried must be re-evaluated, even though it may not be named.
func resettable(s NodeState) bool {
	return RetryableSelection(s) || s == NodeSkipped
}

// PlanRetry computes which nodes a retry resets. It returns a *RetryError when
// the selection is refused.
func PlanRetry(in RetryInput) (RetryPlan, error) {
	g, st := in.Graph, in.States
	explicit := len(in.Selected) > 0

	selected := dedupeSorted(in.Selected)
	if !explicit {
		for _, id := range g.Nodes() {
			if resettable(st.Get(id)) && !refusedTemplate(g, in.Expansions, id) {
				selected = append(selected, id)
			}
		}
		sort.Strings(selected)
	}

	rerr := &RetryError{}
	for _, id := range selected {
		n := g.Node(id)
		if n == nil {
			rerr.Unknown = append(rerr.Unknown, id)
			continue
		}
		s := st.Get(id)
		switch {
		case explicit && !RetryableSelection(s):
			rerr.Refused = append(rerr.Refused, NodeRefusal{NodeID: id, State: s, Reason: RefusalNotRetryableState})
		case refusedTemplate(g, in.Expansions, id):
			rerr.Refused = append(rerr.Refused, NodeRefusal{NodeID: id, State: s, Reason: RefusalExpansionRefused})
		case in.InFlight[id]:
			rerr.Refused = append(rerr.Refused, NodeRefusal{NodeID: id, State: s, Reason: RefusalAttemptInFlight})
		}
	}
	if len(rerr.Unknown) > 0 || len(rerr.Refused) > 0 {
		return RetryPlan{}, rerr
	}

	resets := closeOverDependents(in, selected)

	if explicit {
		// Would any selected node be settled again by the very next reconcile?
		// Asked of the same Propagate the reconcile runs, so the answer cannot
		// drift from what would actually happen.
		after := ApplyResets(st, resets)
		for _, t := range Propagate(g, after) {
			if containsSorted(selected, t.NodeID) {
				rerr.Refused = append(rerr.Refused, NodeRefusal{
					NodeID: t.NodeID, State: st.Get(t.NodeID), Reason: RefusalWouldResettle, Cause: t.Cause,
				})
			}
		}
		if len(rerr.Refused) > 0 {
			return RetryPlan{}, rerr
		}
		// fail_fast halts ALL new dispatch while any node is terminally failed,
		// and settles what it withholds as SKIPPED. A retry that leaves some
		// other failure standing would therefore have its reset nodes skipped by
		// the next reconcile -- the same silent re-failure as above, for a
		// different reason.
		if g.FailFast {
			settled := ApplyTransitions(after, Propagate(g, after))
			if culprit := firstFailed(g, settled); culprit != "" {
				for _, id := range selected {
					rerr.Refused = append(rerr.Refused, NodeRefusal{
						NodeID: id, State: st.Get(id), Reason: RefusalFailFastHalted, Cause: culprit,
					})
				}
				return RetryPlan{}, rerr
			}
		}
	}

	return RetryPlan{Selected: selected, Resets: resets}, nil
}

// closeOverDependents walks from the selection to every node the retry resets.
//
// Two directions of travel, because expansion makes the graph two-level: DOWN
// (a node's successors, and an expanded template's children) and UP (a child's
// template). A template reached only upward is reset without its other children;
// reached downward as well, its children are reset too.
func closeOverDependents(in RetryInput, selected []string) []NodeReset {
	g, st := in.Graph, in.States
	type visit struct {
		id     string
		upward bool
	}
	var queue []visit
	for _, id := range selected {
		queue = append(queue, visit{id: id})
	}

	down := map[string]bool{} // visited travelling down
	up := map[string]bool{}   // visited travelling up
	reset := map[string]bool{}
	var out []NodeReset
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		if down[v.id] || (v.upward && up[v.id]) {
			continue
		}
		if v.upward {
			up[v.id] = true
		} else {
			down[v.id] = true
		}

		n := g.Node(v.id)
		if n == nil {
			continue
		}
		s := st.Get(v.id)
		expanded := n.IsTemplate() && in.Expansions[v.id] == Expanded

		if !reset[v.id] && resettable(s) && !refusedTemplate(g, in.Expansions, v.id) && !in.InFlight[v.id] {
			reset[v.id] = true
			to := NodePending
			if expanded {
				to = NodeRunning
			}
			out = append(out, NodeReset{NodeID: v.id, From: s, To: to})
		}

		if expanded && !v.upward {
			for _, c := range g.Children(v.id) {
				queue = append(queue, visit{id: c})
			}
		}
		if n.IsChild() {
			queue = append(queue, visit{id: n.ParentID, upward: true})
		}
		// Successors are reached in both directions: a retried child changes
		// its template's outcome, and so everything downstream of the template.
		for _, succ := range g.Succs(v.id) {
			queue = append(queue, visit{id: succ})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// ApplyResets returns the states a plan's resets would produce.
func ApplyResets(st States, resets []NodeReset) States {
	out := st.Clone()
	for _, r := range resets {
		out[r.NodeID] = r.To
	}
	return out
}

// firstFailed returns the deterministically-first terminally failed node, or "".
func firstFailed(g *Graph, st States) string {
	var failed []string
	for _, id := range g.Nodes() {
		if st.Get(id).Failed() {
			failed = append(failed, id)
		}
	}
	sort.Strings(failed)
	if len(failed) == 0 {
		return ""
	}
	return failed[0]
}

func refusedTemplate(g *Graph, exp map[string]ExpansionStatus, id string) bool {
	n := g.Node(id)
	return n != nil && n.IsTemplate() && exp[id] == ExpansionRefused
}

func dedupeSorted(ids []string) []string {
	set := map[string]bool{}
	var out []string
	for _, id := range ids {
		if !set[id] {
			set[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func containsSorted(sorted []string, id string) bool {
	i := sort.SearchStrings(sorted, id)
	return i < len(sorted) && sorted[i] == id
}
