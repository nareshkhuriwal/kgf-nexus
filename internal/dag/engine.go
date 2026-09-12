package dag

import (
	"fmt"
	"sort"
)

// TransitionReason explains WHY the engine decided a transition. It is carried
// into run_events and the UI: "node X became UPSTREAM_FAILED" is far less useful
// than "node X became UPSTREAM_FAILED because ancestor Y FAILED", and the
// difference is what makes a 200-node failure diagnosable at a glance.
type TransitionReason string

const (
	ReasonTriggerSatisfied     TransitionReason = "trigger_rule_satisfied"
	ReasonUpstreamFailed       TransitionReason = "upstream_failed"
	ReasonUpstreamSkipped      TransitionReason = "upstream_skipped"
	ReasonTriggerUnsatisfiable TransitionReason = "trigger_rule_unsatisfiable"
	ReasonFailFast             TransitionReason = "fail_fast_halt"
	ReasonRunCancelled         TransitionReason = "run_cancelled"
)

// Transition is one state change the engine concluded from the inputs. The
// engine returns transitions rather than applying them; applying is I/O and
// belongs to the service layer. This is the "set of transitions" of §3.2.
type Transition struct {
	NodeID string
	From   NodeState
	To     NodeState
	Reason TransitionReason
	// Cause names the upstream node responsible, for the propagation reasons.
	Cause string
}

func (t Transition) String() string {
	s := fmt.Sprintf("%s: %s->%s (%s", t.NodeID, t.From, t.To, t.Reason)
	if t.Cause != "" {
		s += " cause=" + t.Cause
	}
	return s + ")"
}

// Decision is the complete pure output of one reconcile: which nodes should
// become READY (and therefore get a PENDING attempt), which nodes propagate to
// a terminal non-executed state, and what the run state now is.
type Decision struct {
	// Frontier is the nodes that should start now. Deterministically ordered and
	// guaranteed duplicate-free.
	Frontier []string
	// Transitions includes the frontier's PENDING->READY changes plus every
	// propagation (SKIPPED / UPSTREAM_FAILED).
	Transitions []Transition
	// RunState is the state the run should now hold. RunRunning while work
	// remains; a terminal state per §5.2 when none does.
	RunState RunState
	// Halted reports that fail_fast has stopped new dispatch. The run is not yet
	// terminal (in-flight attempts still drain), but the frontier is empty by
	// policy rather than by dependency.
	Halted bool
}

// States is the current node-state snapshot the engine reasons over. It is a
// plain map so tests can enumerate configurations exhaustively without touching
// a database.
type States map[string]NodeState

// Get returns the state of a node, defaulting to PENDING. Defaulting rather
// than erroring keeps the engine total: a node row that has not been written yet
// is indistinguishable from PENDING, and treating it as such means a partially
// materialized run reconciles correctly instead of panicking.
func (s States) Get(id string) NodeState {
	if v, ok := s[id]; ok && v != "" {
		return v
	}
	return NodePending
}

// Clone returns an independent copy, so callers can apply transitions
// speculatively without mutating their input.
func (s States) Clone() States {
	out := make(States, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// upstreamTally summarizes the predecessor states a trigger rule needs.
type upstreamTally struct {
	total, succeeded, failed, skipped, cancelled, terminal int
}

func tally(g *Graph, id string, st States) upstreamTally {
	var t upstreamTally
	for _, p := range g.preds[id] {
		s := st.Get(p)
		t.total++
		if s.Terminal() {
			t.terminal++
		}
		switch s {
		case NodeSucceeded:
			t.succeeded++
		case NodeFailed, NodeUpstreamFailed:
			t.failed++
		case NodeSkipped:
			t.skipped++
		case NodeCancelled:
			t.cancelled++
		}
	}
	return t
}

// triggerOutcome is the three-valued result of evaluating a rule. Three values
// rather than a bool is the key design point: "not yet" and "never" demand
// opposite actions (wait vs. propagate a terminal state), and collapsing them
// into false is how orchestrators end up with runs that hang forever.
type triggerOutcome int

const (
	triggerWait          triggerOutcome = iota // upstreams not settled yet
	triggerFire                                // rule satisfied; node may run
	triggerUnsatisfiable                       // rule can never be satisfied
)

// evalTrigger evaluates one node's trigger rule against its upstream states.
//
// Every rule is implemented; TriggerRule.Phase1Active marks which ones submit
// validation accepts today. Implementing all six costs a few lines each and
// means enabling the rest in Phase 2 is a validation-policy flip, not an engine
// change — which is exactly the structure architecture §3.2 asks for.
func evalTrigger(rule TriggerRule, t upstreamTally) triggerOutcome {
	if t.total == 0 {
		// A source node has nothing to wait for under any rule. all_skipped and
		// one_* rules are vacuous here; firing is the only useful reading, since
		// the alternative is a root that never starts.
		return triggerFire
	}
	switch rule {
	case AllSuccess:
		switch {
		case t.succeeded == t.total:
			return triggerFire
		// Any upstream that ended other than SUCCEEDED makes all_success
		// permanently impossible, so we can decide NOW rather than waiting for
		// the remaining upstreams to settle. Deciding early is what lets a large
		// fan-out collapse promptly instead of one layer per reconcile.
		case t.failed > 0 || t.skipped > 0 || t.cancelled > 0:
			return triggerUnsatisfiable
		default:
			return triggerWait
		}

	case AllDone:
		// all_done cares only that upstreams have settled, not how. This is the
		// cleanup/teardown rule.
		if t.terminal == t.total {
			return triggerFire
		}
		return triggerWait

	case OneSuccess:
		if t.succeeded > 0 {
			return triggerFire
		}
		if t.terminal == t.total {
			return triggerUnsatisfiable
		}
		return triggerWait

	case OneFailed:
		if t.failed > 0 {
			return triggerFire
		}
		if t.terminal == t.total {
			return triggerUnsatisfiable
		}
		return triggerWait

	case NoneFailed:
		// Tolerates SKIPPED but not failure. The distinction from all_success is
		// precisely branch tolerance: a node downstream of an untaken branch
		// should still run.
		if t.failed > 0 {
			return triggerUnsatisfiable
		}
		if t.terminal == t.total {
			return triggerFire
		}
		return triggerWait

	case AllSkipped:
		if t.skipped == t.total {
			return triggerFire
		}
		if t.terminal == t.total {
			return triggerUnsatisfiable
		}
		// A non-skipped terminal upstream already rules it out; decide early for
		// the same reason as all_success.
		if t.succeeded > 0 || t.failed > 0 || t.cancelled > 0 {
			return triggerUnsatisfiable
		}
		return triggerWait

	default:
		// An unknown rule must never silently behave like all_success; that would
		// turn a spec-compatibility gap into wrong execution. Waiting forever is
		// visible and safe, and submit validation rejects unknown rules anyway.
		return triggerWait
	}
}

// ReadyFrontier returns the nodes whose trigger rule is satisfied and which are
// still PENDING, in deterministic order with no duplicates.
//
// It is intentionally narrow: it answers only "what may start now". It does NOT
// decide propagation or run state, because a caller that wants just the frontier
// should not have to reason about the rest. Reconcile composes all three.
func ReadyFrontier(g *Graph, st States) []string {
	var out []string
	for _, id := range g.Nodes() {
		if st.Get(id) != NodePending {
			// Only PENDING nodes can enter the frontier. Anything already READY,
			// RUNNING or terminal has been accounted for, and re-emitting it is
			// how a node gets dispatched twice.
			continue
		}
		if evalTrigger(g.nodes[id].TriggerRule, tally(g, id, st)) == triggerFire {
			out = append(out, id)
		}
	}
	// g.Nodes() is already deterministic, so out is too; the sort is belt and
	// braces against a future change to node ordering.
	sort.Strings(out)
	return out
}

// Propagate computes the SKIPPED and UPSTREAM_FAILED transitions implied by the
// current states, transitively, to a fixed point.
//
// UPSTREAM_FAILED and SKIPPED are kept strictly distinct (proto comment on
// NODE_STATE_UPSTREAM_FAILED): UPSTREAM_FAILED means an ancestor broke and this
// node never ran, SKIPPED means a branch was not taken. Collapsing them into
// FAILED is what makes a large run's failure report unreadable — you can no
// longer tell the one real fault from the hundred consequences.
//
// Which one a node gets is decided by WHY its trigger became unsatisfiable: if
// any upstream is in a failure state, the cause is failure; otherwise the cause
// is a not-taken branch.
func Propagate(g *Graph, st States) []Transition {
	work := st.Clone()
	var out []Transition

	// Iterate in topological order and repeat to a fixed point. Topological order
	// means a single pass usually suffices; the outer loop is what guarantees
	// TRANSITIVE propagation to every descendant even in orderings where a
	// node's cause settles after it was examined.
	for {
		progressed := false
		for _, id := range g.Nodes() {
			if work.Get(id) != NodePending {
				continue
			}
			t := tally(g, id, work)
			if evalTrigger(g.nodes[id].TriggerRule, t) != triggerUnsatisfiable {
				continue
			}
			to, reason, cause := NodeSkipped, ReasonUpstreamSkipped, ""
			if t.failed > 0 {
				// Failure dominates: a node with one failed and one skipped
				// ancestor is UPSTREAM_FAILED, because a human reading the run
				// wants the failure surfaced, not hidden behind a skip.
				to, reason = NodeUpstreamFailed, ReasonUpstreamFailed
				cause = firstPredIn(g, id, work, NodeFailed, NodeUpstreamFailed)
			} else if t.skipped > 0 {
				cause = firstPredIn(g, id, work, NodeSkipped)
			} else if t.cancelled > 0 {
				to, reason = NodeSkipped, ReasonTriggerUnsatisfiable
				cause = firstPredIn(g, id, work, NodeCancelled)
			} else {
				reason = ReasonTriggerUnsatisfiable
			}
			out = append(out, Transition{NodeID: id, From: NodePending, To: to, Reason: reason, Cause: cause})
			work[id] = to
			progressed = true
		}
		if !progressed {
			break
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// firstPredIn returns the deterministically-first predecessor in any of the
// given states, for attribution in the transition reason.
func firstPredIn(g *Graph, id string, st States, want ...NodeState) string {
	set := make(map[NodeState]bool, len(want))
	for _, w := range want {
		set[w] = true
	}
	preds := g.Preds(id)
	sort.Strings(preds)
	for _, p := range preds {
		if set[st.Get(p)] {
			return p
		}
	}
	return ""
}

// anyTerminallyFailed reports whether some node is in a state that disqualifies
// the run from SUCCEEDED per §5.2.
func anyTerminallyFailed(g *Graph, st States) bool {
	for _, id := range g.Nodes() {
		if st.Get(id).Failed() {
			return true
		}
	}
	return false
}

// RunOutcome computes the run state implied by the node states, implementing
// architecture §5.2 exactly:
//
//	SUCCEEDED: every node terminal AND no node in {FAILED, UPSTREAM_FAILED, TIMED_OUT}
//	FAILED:    at least one node terminally failed after retries exhausted
//
// TIMED_OUT is an attempt state, not a node state (CONTRACT.md), so the service
// layer folds a retry-exhausted TIMED_OUT attempt into a FAILED node before
// calling here. A run whose nodes are all terminal with at least one CANCELLED
// and no failure is reported CANCELLED, since neither SUCCEEDED nor FAILED is
// truthful about it.
func RunOutcome(g *Graph, st States) RunState {
	allTerminal := true
	anyCancelled := false
	for _, id := range g.Nodes() {
		s := st.Get(id)
		if !s.Terminal() {
			allTerminal = false
		}
		if s == NodeCancelled {
			anyCancelled = true
		}
	}
	failed := anyTerminallyFailed(g, st)

	switch {
	case failed && allTerminal:
		return RunFailed
	case !allTerminal:
		// A failure with work still in flight is NOT yet terminal: fail_fast stops
		// new dispatch but §5.2 has in-flight attempts drain first. Reporting
		// FAILED here would lie about attempts that are still running and could
		// still write outputs.
		return RunRunning
	case anyCancelled:
		return RunCancelled
	default:
		return RunSucceeded
	}
}

// ReconcileInput is everything the pure engine needs to decide. Gathering it into
// a struct keeps the signature stable as the engine grows, and makes the
// property tests read as "for all inputs" rather than "for all seven arguments".
type ReconcileInput struct {
	Graph  *Graph
	States States
	// RunState is the CURRENT run state, so the engine can respect operator
	// interventions it does not own. PAUSING/PAUSED suppress new dispatch;
	// CANCELLING/CANCELLED suppress everything.
	RunState RunState
}

// Reconcile is THE pure engine function of architecture §3.2:
// (dag_spec, current node states) → set of transitions.
//
// It is deterministic, allocation-only, and performs no I/O. Given identical
// inputs it returns identical output, which is what makes it exhaustively
// table-testable and property-testable.
//
// Order of reasoning matters and is deliberate:
//  1. Propagate first, so a node whose trigger is already unsatisfiable is
//     settled before we ask what may start. Asking first would let a node enter
//     the frontier in the same pass that its ancestor's failure should have
//     ruled it out.
//  2. Then compute the frontier against the post-propagation view.
//  3. Then decide the run state against the fully settled view, so a run that
//     became terminal in this pass is reported terminal now rather than one
//     reconcile later.
func Reconcile(in ReconcileInput) Decision {
	g := in.Graph
	st := in.States.Clone()
	d := Decision{RunState: in.RunState}

	transitions := Propagate(g, st)
	for _, t := range transitions {
		st[t.NodeID] = t.To
	}
	d.Transitions = append(d.Transitions, transitions...)

	// fail_fast: stop dispatching NEW work on the first terminal failure. Note it
	// does not cancel anything — that is cancel, and conflating the two makes
	// pause/fail_fast useless for their actual job (§5.2).
	halted := g.FailFast && anyTerminallyFailed(g, st)
	d.Halted = halted

	suppressDispatch := halted ||
		in.RunState == RunPausing || in.RunState == RunPaused ||
		in.RunState == RunCancelling || in.RunState == RunCancelled

	if !suppressDispatch {
		frontier := ReadyFrontier(g, st)
		d.Frontier = frontier
		for _, id := range frontier {
			d.Transitions = append(d.Transitions, Transition{
				NodeID: id, From: st.Get(id), To: NodeReady, Reason: ReasonTriggerSatisfied,
			})
			st[id] = NodeReady
		}
	} else if halted {
		// fail_fast withholds work PERMANENTLY, so anything it withholds must be
		// settled rather than left PENDING. Leaving it PENDING is a hung run: such
		// a node's trigger is satisfied, so propagation will never claim it, yet
		// dispatch will never pick it up either -- the run sits RUNNING forever
		// holding a concurrency slot with nothing to alert on. Settling the
		// withheld work is what makes "every run reaches a terminal state" true in
		// the presence of fail_fast.
		//
		// SKIPPED rather than UPSTREAM_FAILED: these nodes are not victims of a
		// broken ancestor (their own triggers are satisfied) -- they are work the
		// run's failure POLICY declined to do. Keeping that distinct preserves the
		// meaning of UPSTREAM_FAILED as exactly "an ancestor broke".
		for _, id := range g.Nodes() {
			if st.Get(id) != NodePending {
				continue
			}
			d.Transitions = append(d.Transitions, Transition{
				NodeID: id, From: NodePending, To: NodeSkipped, Reason: ReasonFailFast,
			})
			st[id] = NodeSkipped
		}
	}

	// A run under operator control keeps its operator-assigned state; the engine
	// must not overwrite CANCELLING with RUNNING and undo an intervention it does
	// not own.
	switch in.RunState {
	case RunCancelling, RunCancelled, RunPausing, RunPaused:
		d.RunState = in.RunState
		// Cancellation still needs a terminal verdict once nothing is in flight,
		// otherwise a cancelled run never leaves CANCELLING.
		if in.RunState == RunCancelling && allTerminal(g, st) {
			d.RunState = RunCancelled
		}
		if in.RunState == RunPausing && noneInFlight(g, st) {
			d.RunState = RunPaused
		}
	default:
		d.RunState = RunOutcome(g, st)
		if halted && d.RunState == RunRunning {
			// fail_fast with in-flight work: still RUNNING, but the frontier is
			// empty by policy. Once the in-flight attempts settle, RunOutcome
			// returns FAILED on the next reconcile.
			d.RunState = RunRunning
		}
	}

	return d
}

func allTerminal(g *Graph, st States) bool {
	for _, id := range g.Nodes() {
		if !st.Get(id).Terminal() {
			return false
		}
	}
	return true
}

// noneInFlight reports whether no node is actually executing. PENDING nodes do
// not count as in flight: pause means "dispatch nothing new", so a run with
// undispatched work reaches PAUSED rather than waiting for it (§5.2).
func noneInFlight(g *Graph, st States) bool {
	for _, id := range g.Nodes() {
		switch st.Get(id) {
		case NodeReady, NodeRunning:
			return false
		}
	}
	return true
}

// ApplyTransitions returns a new state map with every transition applied. It
// exists for tests and for the service layer's in-memory bookkeeping, so neither
// has to hand-roll the fold.
func ApplyTransitions(st States, ts []Transition) States {
	out := st.Clone()
	for _, t := range ts {
		out[t.NodeID] = t.To
	}
	return out
}
