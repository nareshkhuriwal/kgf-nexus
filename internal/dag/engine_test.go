// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package dag

import (
	"fmt"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// ── Trigger rules, exhaustively ───────────────────────────────────────────────
//
// Every rule is exercised against EVERY combination of two upstream states.
// Two upstreams with six settled/unsettled states each is small enough to
// enumerate completely, and a completely enumerated truth table is the only way
// to be confident a trigger evaluator has no hole.

func TestEvalTrigger_ExhaustiveTwoUpstreams(t *testing.T) {
	states := []NodeState{NodePending, NodeRunning, NodeSucceeded, NodeFailed, NodeSkipped, NodeCancelled}
	rules := []TriggerRule{AllSuccess, AllDone, OneSuccess, OneFailed, NoneFailed, AllSkipped}

	// expect encodes the intended semantics independently of the implementation,
	// so the test is a specification rather than a mirror of the code.
	expect := func(rule TriggerRule, a, b NodeState) triggerOutcome {
		var tl upstreamTally
		for _, s := range []NodeState{a, b} {
			tl.total++
			if s.Terminal() {
				tl.terminal++
			}
			switch s {
			case NodeSucceeded:
				tl.succeeded++
			case NodeFailed, NodeUpstreamFailed:
				tl.failed++
			case NodeSkipped:
				tl.skipped++
			case NodeCancelled:
				tl.cancelled++
			}
		}
		allSettled := tl.terminal == tl.total
		switch rule {
		case AllSuccess:
			if tl.succeeded == 2 {
				return triggerFire
			}
			if tl.failed+tl.skipped+tl.cancelled > 0 {
				return triggerUnsatisfiable
			}
			return triggerWait
		case AllDone:
			if allSettled {
				return triggerFire
			}
			return triggerWait
		case OneSuccess:
			if tl.succeeded > 0 {
				return triggerFire
			}
			if allSettled {
				return triggerUnsatisfiable
			}
			return triggerWait
		case OneFailed:
			if tl.failed > 0 {
				return triggerFire
			}
			if allSettled {
				return triggerUnsatisfiable
			}
			return triggerWait
		case NoneFailed:
			if tl.failed > 0 {
				return triggerUnsatisfiable
			}
			if allSettled {
				return triggerFire
			}
			return triggerWait
		case AllSkipped:
			if tl.skipped == 2 {
				return triggerFire
			}
			if allSettled || tl.succeeded+tl.failed+tl.cancelled > 0 {
				return triggerUnsatisfiable
			}
			return triggerWait
		}
		panic("unreachable")
	}

	for _, rule := range rules {
		for _, a := range states {
			for _, b := range states {
				g := wf().t("u1", "u2").task("d", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS).
					e([2]string{"u1", "d"}, [2]string{"u2", "d"}).build()
				g.nodes["d"].TriggerRule = rule
				st := States{"u1": a, "u2": b}
				got := evalTrigger(rule, tally(g, "d", st))
				if want := expect(rule, a, b); got != want {
					t.Errorf("rule=%s upstreams=(%s,%s): got %v want %v", rule, a, b, got, want)
				}
			}
		}
	}
}

// A source node has no upstreams and must fire under every rule; the alternative
// is a root that never starts, which would wedge every run.
func TestEvalTrigger_SourceNodeFiresUnderEveryRule(t *testing.T) {
	for _, rule := range []TriggerRule{AllSuccess, AllDone, OneSuccess, OneFailed, NoneFailed, AllSkipped} {
		if got := evalTrigger(rule, upstreamTally{}); got != triggerFire {
			t.Errorf("rule %s: source node did not fire (%v)", rule, got)
		}
	}
}

// An unrecognized rule must WAIT, never behave like all_success: silently
// guessing would turn a spec-compatibility gap into wrong execution.
func TestEvalTrigger_UnknownRuleWaits(t *testing.T) {
	got := evalTrigger(TriggerRule("not_a_rule"), upstreamTally{total: 1, succeeded: 1, terminal: 1})
	if got != triggerWait {
		t.Errorf("unknown rule should wait, got %v", got)
	}
}

func TestTriggerRule_Phase1Active(t *testing.T) {
	active := map[TriggerRule]bool{
		AllSuccess: true, AllDone: true,
		OneSuccess: false, OneFailed: false, NoneFailed: false, AllSkipped: false,
	}
	for rule, want := range active {
		if got := rule.Phase1Active(); got != want {
			t.Errorf("%s.Phase1Active() = %v, want %v", rule, got, want)
		}
	}
}

// ── ReadyFrontier ─────────────────────────────────────────────────────────────

func TestReadyFrontier_Table(t *testing.T) {
	tests := []struct {
		name  string
		graph *Graph
		st    States
		want  []string
	}{
		{
			name:  "fresh chain exposes only the root",
			graph: chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b", "c"),
			st:    States{},
			want:  []string{"a"},
		},
		{
			name:  "root succeeded exposes the middle",
			graph: chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b", "c"),
			st:    States{"a": NodeSucceeded},
			want:  []string{"b"},
		},
		{
			name:  "root running exposes nothing",
			graph: chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b", "c"),
			st:    States{"a": NodeRunning},
			want:  nil,
		},
		{
			name:  "a READY node is not re-emitted",
			graph: chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b"),
			st:    States{"a": NodeReady},
			want:  nil,
		},
		{
			name:  "fan-out exposes both branches at once",
			graph: diamond(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
			st:    States{"a": NodeSucceeded},
			want:  []string{"b", "c"},
		},
		{
			name:  "join waits for the slower branch",
			graph: diamond(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
			st:    States{"a": NodeSucceeded, "b": NodeSucceeded, "c": NodeRunning},
			want:  nil,
		},
		{
			name:  "join fires when both branches land",
			graph: diamond(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
			st:    States{"a": NodeSucceeded, "b": NodeSucceeded, "c": NodeSucceeded},
			want:  []string{"d"},
		},
		{
			name:  "all_success join does NOT fire on a failed branch",
			graph: diamond(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
			st:    States{"a": NodeSucceeded, "b": NodeSucceeded, "c": NodeFailed},
			want:  nil,
		},
		{
			name:  "all_done join DOES fire on a failed branch",
			graph: diamond(nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE),
			st:    States{"a": NodeSucceeded, "b": NodeSucceeded, "c": NodeFailed},
			want:  []string{"d"},
		},
		{
			name:  "all_done join fires on a skipped branch too",
			graph: diamond(nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE),
			st:    States{"a": NodeSucceeded, "b": NodeSkipped, "c": NodeSkipped},
			want:  []string{"d"},
		},
		{
			name:  "none_failed tolerates a skip",
			graph: diamond(nexusv1.TriggerRule_TRIGGER_RULE_NONE_FAILED),
			st:    States{"a": NodeSucceeded, "b": NodeSucceeded, "c": NodeSkipped},
			want:  []string{"d"},
		},
		{
			name:  "one_success fires on the first success",
			graph: diamond(nexusv1.TriggerRule_TRIGGER_RULE_ONE_SUCCESS),
			st:    States{"a": NodeSucceeded, "b": NodeSucceeded, "c": NodeRunning},
			want:  []string{"d"},
		},
		{
			name:  "one_failed fires on the first failure",
			graph: diamond(nexusv1.TriggerRule_TRIGGER_RULE_ONE_FAILED),
			st:    States{"a": NodeSucceeded, "b": NodeFailed, "c": NodeRunning},
			want:  []string{"d"},
		},
		{
			name:  "all_skipped fires only when every branch skipped",
			graph: diamond(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SKIPPED),
			st:    States{"a": NodeSucceeded, "b": NodeSkipped, "c": NodeSkipped},
			want:  []string{"d"},
		},
		{
			name:  "independent nodes all start together",
			graph: wf().t("a", "b", "c").build(),
			st:    States{},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "terminal nodes never re-enter the frontier",
			graph: wf().t("a", "b", "c").build(),
			st:    States{"a": NodeSucceeded, "b": NodeFailed, "c": NodeSkipped},
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ReadyFrontier(tc.graph, tc.st)
			if !setEq(got, tc.want) {
				t.Fatalf("frontier = %v, want %v", got, tc.want)
			}
			seen := map[string]bool{}
			for _, id := range got {
				if seen[id] {
					t.Fatalf("duplicate node %q in frontier %v", id, got)
				}
				seen[id] = true
			}
		})
	}
}

// ── Propagation: SKIPPED vs UPSTREAM_FAILED ───────────────────────────────────

func TestPropagate_Table(t *testing.T) {
	tests := []struct {
		name  string
		graph *Graph
		st    States
		want  map[string]NodeState
	}{
		{
			name:  "failure propagates one hop as UPSTREAM_FAILED, not FAILED",
			graph: chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b"),
			st:    States{"a": NodeFailed},
			want:  map[string]NodeState{"b": NodeUpstreamFailed},
		},
		{
			name:  "failure propagates TRANSITIVELY down a long chain",
			graph: chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b", "c", "d", "e"),
			st:    States{"a": NodeFailed},
			want: map[string]NodeState{
				"b": NodeUpstreamFailed, "c": NodeUpstreamFailed,
				"d": NodeUpstreamFailed, "e": NodeUpstreamFailed,
			},
		},
		{
			name:  "skip propagates transitively as SKIPPED",
			graph: chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b", "c", "d"),
			st:    States{"a": NodeSkipped},
			want: map[string]NodeState{
				"b": NodeSkipped, "c": NodeSkipped, "d": NodeSkipped,
			},
		},
		{
			name: "SKIPPED and UPSTREAM_FAILED stay distinct on separate branches",
			graph: wf().t("root", "fail", "skip", "df", "ds").
				e([2]string{"root", "fail"}, [2]string{"root", "skip"},
					[2]string{"fail", "df"}, [2]string{"skip", "ds"}).build(),
			st: States{"root": NodeSucceeded, "fail": NodeFailed, "skip": NodeSkipped},
			want: map[string]NodeState{
				"df": NodeUpstreamFailed,
				"ds": NodeSkipped,
			},
		},
		{
			name: "failure dominates skip at a join: the real fault is surfaced",
			graph: wf().t("f", "s", "j").
				e([2]string{"f", "j"}, [2]string{"s", "j"}).build(),
			st:   States{"f": NodeFailed, "s": NodeSkipped},
			want: map[string]NodeState{"j": NodeUpstreamFailed},
		},
		{
			name:  "all_done downstream of a failure is NOT propagated: it must run",
			graph: chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE, "a", "cleanup"),
			st:    States{"a": NodeFailed},
			want:  map[string]NodeState{},
		},
		{
			name:  "none_failed downstream of a skip is NOT propagated",
			graph: chain(nexusv1.TriggerRule_TRIGGER_RULE_NONE_FAILED, "a", "b"),
			st:    States{"a": NodeSkipped},
			want:  map[string]NodeState{},
		},
		{
			name:  "none_failed downstream of a failure IS propagated",
			graph: chain(nexusv1.TriggerRule_TRIGGER_RULE_NONE_FAILED, "a", "b"),
			st:    States{"a": NodeFailed},
			want:  map[string]NodeState{"b": NodeUpstreamFailed},
		},
		{
			name:  "nothing propagates while upstream is still running",
			graph: chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b", "c"),
			st:    States{"a": NodeRunning},
			want:  map[string]NodeState{},
		},
		{
			name:  "cancellation propagates as SKIPPED, not UPSTREAM_FAILED",
			graph: chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b"),
			st:    States{"a": NodeCancelled},
			want:  map[string]NodeState{"b": NodeSkipped},
		},
		{
			name: "wide fan-out below a failure: every descendant is marked",
			graph: wf().t("root", "c1", "c2", "c3", "g1").
				e([2]string{"root", "c1"}, [2]string{"root", "c2"}, [2]string{"root", "c3"},
					[2]string{"c1", "g1"}).build(),
			st: States{"root": NodeFailed},
			want: map[string]NodeState{
				"c1": NodeUpstreamFailed, "c2": NodeUpstreamFailed,
				"c3": NodeUpstreamFailed, "g1": NodeUpstreamFailed,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := Propagate(tc.graph, tc.st)
			got := map[string]NodeState{}
			for _, tr := range ts {
				if prev, dup := got[tr.NodeID]; dup {
					t.Fatalf("node %q transitioned twice in one pass (%s then %s)", tr.NodeID, prev, tr.To)
				}
				got[tr.NodeID] = tr.To
			}
			if len(got) != len(tc.want) {
				t.Fatalf("propagated %v, want %v", got, tc.want)
			}
			for id, want := range tc.want {
				if got[id] != want {
					t.Errorf("node %q = %s, want %s", id, got[id], want)
				}
			}
		})
	}
}

// A propagation transition must name the node responsible: "UPSTREAM_FAILED"
// with no cause forces the operator to trace the graph by hand.
func TestPropagate_AttributesTheCause(t *testing.T) {
	g := chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b", "c")
	ts := Propagate(g, States{"a": NodeFailed})
	byNode := map[string]Transition{}
	for _, tr := range ts {
		byNode[tr.NodeID] = tr
	}
	if got := byNode["b"].Cause; got != "a" {
		t.Errorf("b's cause = %q, want %q", got, "a")
	}
	if got := byNode["b"].Reason; got != ReasonUpstreamFailed {
		t.Errorf("b's reason = %q, want %q", got, ReasonUpstreamFailed)
	}
	// c's immediate cause is b, which is itself UPSTREAM_FAILED: attribution is
	// per-hop so the chain is walkable.
	if got := byNode["c"].Cause; got != "b" {
		t.Errorf("c's cause = %q, want %q", got, "b")
	}
	if s := byNode["c"].String(); s == "" {
		t.Error("transition should render")
	}
}

// ── Terminal run detection: architecture §5.2 verbatim ────────────────────────

func TestRunOutcome_Table(t *testing.T) {
	g := diamond(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
	tests := []struct {
		name string
		st   States
		want RunState
	}{
		{
			name: "all succeeded => SUCCEEDED",
			st:   States{"a": NodeSucceeded, "b": NodeSucceeded, "c": NodeSucceeded, "d": NodeSucceeded},
			want: RunSucceeded,
		},
		{
			name: "all terminal with a SKIP and no failure => SUCCEEDED",
			st:   States{"a": NodeSucceeded, "b": NodeSkipped, "c": NodeSucceeded, "d": NodeSucceeded},
			want: RunSucceeded,
		},
		{
			name: "a FAILED node with everything terminal => FAILED",
			st:   States{"a": NodeSucceeded, "b": NodeFailed, "c": NodeSucceeded, "d": NodeUpstreamFailed},
			want: RunFailed,
		},
		{
			name: "an UPSTREAM_FAILED node alone disqualifies SUCCEEDED",
			st:   States{"a": NodeSucceeded, "b": NodeSkipped, "c": NodeSucceeded, "d": NodeUpstreamFailed},
			want: RunFailed,
		},
		{
			name: "work still in flight => RUNNING, even with a failure",
			st:   States{"a": NodeSucceeded, "b": NodeFailed, "c": NodeRunning},
			want: RunRunning,
		},
		{
			name: "nothing started => RUNNING",
			st:   States{},
			want: RunRunning,
		},
		{
			name: "all terminal with a CANCELLED and no failure => CANCELLED",
			st:   States{"a": NodeSucceeded, "b": NodeCancelled, "c": NodeSkipped, "d": NodeSkipped},
			want: RunCancelled,
		},
		{
			name: "a failure outweighs a cancellation",
			st:   States{"a": NodeSucceeded, "b": NodeCancelled, "c": NodeFailed, "d": NodeUpstreamFailed},
			want: RunFailed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RunOutcome(g, tc.st); got != tc.want {
				t.Errorf("RunOutcome = %s, want %s", got, tc.want)
			}
		})
	}
}

// ── fail_fast ─────────────────────────────────────────────────────────────────

func TestReconcile_FailFastStopsNewDispatchButNotInFlightWork(t *testing.T) {
	// two independent branches; one fails while the other is still running
	g := wf().failFast(true).t("a", "b", "a2", "b2").
		e([2]string{"a", "b"}, [2]string{"a2", "b2"}).build()

	d := Reconcile(ReconcileInput{
		Graph:    g,
		States:   States{"a": NodeFailed, "a2": NodeRunning},
		RunState: RunRunning,
	})
	if !d.Halted {
		t.Error("fail_fast should halt new dispatch after a terminal failure")
	}
	if len(d.Frontier) != 0 {
		t.Errorf("halted run must dispatch nothing new, got %v", d.Frontier)
	}
	// Crucially, the run is NOT yet terminal: a2 is still running and could still
	// write outputs. Reporting FAILED here would lie about in-flight work.
	if d.RunState != RunRunning {
		t.Errorf("run state = %s, want RUNNING while work is in flight", d.RunState)
	}
	// b is still propagated: its trigger is genuinely unsatisfiable regardless of
	// the fail_fast policy.
	got := map[string]NodeState{}
	for _, tr := range d.Transitions {
		got[tr.NodeID] = tr.To
	}
	if got["b"] != NodeUpstreamFailed {
		t.Errorf("b = %s, want UPSTREAM_FAILED", got["b"])
	}
}

func TestReconcile_FailFastVersusContinue(t *testing.T) {
	mk := func(failFast bool) *Graph {
		return wf().failFast(failFast).t("a", "b", "indep").
			e([2]string{"a", "b"}).build()
	}
	// With continue-on-failure, the independent branch still gets dispatched.
	dCont := Reconcile(ReconcileInput{Graph: mk(false), States: States{"a": NodeFailed}, RunState: RunRunning})
	if !setEq(dCont.Frontier, []string{"indep"}) {
		t.Errorf("continue: frontier = %v, want [indep]", dCont.Frontier)
	}
	// With fail_fast the same independent branch is withheld.
	dFF := Reconcile(ReconcileInput{Graph: mk(true), States: States{"a": NodeFailed}, RunState: RunRunning})
	if len(dFF.Frontier) != 0 {
		t.Errorf("fail_fast: frontier = %v, want empty", dFF.Frontier)
	}
	if !dFF.Halted {
		t.Error("fail_fast: Halted should be set")
	}
}

func TestReconcile_FailFastRunTerminatesOnceInFlightDrains(t *testing.T) {
	g := wf().failFast(true).t("a", "b", "indep").e([2]string{"a", "b"}).build()
	st := States{"a": NodeFailed, "indep": NodeSucceeded}
	d := Reconcile(ReconcileInput{Graph: g, States: st, RunState: RunRunning})
	if d.RunState != RunFailed {
		t.Errorf("run state = %s, want FAILED once nothing is in flight", d.RunState)
	}
}

// ── Operator interventions the engine respects but does not own ───────────────

func TestReconcile_PauseAndCancelSuppressDispatch(t *testing.T) {
	g := chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b")
	for _, rs := range []RunState{RunPausing, RunPaused, RunCancelling, RunCancelled} {
		d := Reconcile(ReconcileInput{Graph: g, States: States{}, RunState: rs})
		if len(d.Frontier) != 0 {
			t.Errorf("run state %s: frontier = %v, want empty", rs, d.Frontier)
		}
	}
}

func TestReconcile_PausingReachesPausedWhenNothingInFlight(t *testing.T) {
	g := chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b")
	d := Reconcile(ReconcileInput{Graph: g, States: States{}, RunState: RunPausing})
	if d.RunState != RunPaused {
		t.Errorf("run state = %s, want PAUSED", d.RunState)
	}
	// While something is genuinely executing, PAUSING must persist: pause drains
	// rather than kills (§5.2).
	d2 := Reconcile(ReconcileInput{Graph: g, States: States{"a": NodeRunning}, RunState: RunPausing})
	if d2.RunState != RunPausing {
		t.Errorf("run state = %s, want PAUSING while a is running", d2.RunState)
	}
}

func TestReconcile_CancellingReachesCancelledWhenAllTerminal(t *testing.T) {
	g := chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b")
	d := Reconcile(ReconcileInput{
		Graph:    g,
		States:   States{"a": NodeCancelled, "b": NodeCancelled},
		RunState: RunCancelling,
	})
	if d.RunState != RunCancelled {
		t.Errorf("run state = %s, want CANCELLED", d.RunState)
	}
	d2 := Reconcile(ReconcileInput{
		Graph:    g,
		States:   States{"a": NodeRunning},
		RunState: RunCancelling,
	})
	if d2.RunState != RunCancelling {
		t.Errorf("run state = %s, want CANCELLING while a is running", d2.RunState)
	}
}

// TestReconcile_CancellingSettlesPendingNodes pins F62.
//
// The existing cancel test above starts from nodes that are ALREADY terminal,
// so it never exercised the case that actually wedges a run: a node still
// PENDING when the cancel lands. That happens on any real cancel -- an in-flight
// attempt finishes during the cancel fan-out and the engine creates its
// successor a moment later, after the fan-out has already swept the table.
//
// Such a node is unreachable from both directions. Propagation will not claim it
// (its trigger is satisfied) and dispatch will never take it (the claim query
// admits only PENDING/RUNNING runs), so without settling it here the run sits in
// CANCELLING forever holding a max_active_runs slot. Two of these exhausted the
// namespace limit and blocked every subsequent create, which is how it was found.
func TestReconcile_CancellingSettlesPendingNodes(t *testing.T) {
	g := chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b")

	// "a" succeeded before the cancel landed; "b" is the successor the engine
	// created afterwards and is still PENDING.
	d := Reconcile(ReconcileInput{
		Graph:    g,
		States:   States{"a": NodeSucceeded, "b": NodePending},
		RunState: RunCancelling,
	})

	if got := d.RunState; got != RunCancelled {
		t.Errorf("run state = %s, want CANCELLED; a PENDING node left unsettled wedges the run", got)
	}
	var settled *Transition
	for i := range d.Transitions {
		if d.Transitions[i].NodeID == "b" {
			settled = &d.Transitions[i]
		}
	}
	if settled == nil {
		t.Fatal("node b was left PENDING on a cancelled run: nothing will ever dispatch it and nothing will ever claim it")
	}
	// CANCELLED, not SKIPPED: an operator stopped this work. OnNodesTerminal
	// derives the run's terminal state from these counts, and the distinction
	// between "an operator cancelled it" and "a failure policy declined it" is
	// what keeps a cancelled run from reporting as FAILED.
	if settled.To != NodeCancelled {
		t.Errorf("node b settled to %s, want CANCELLED", settled.To)
	}
	if settled.Reason != ReasonRunCancelled {
		t.Errorf("node b reason = %s, want %s", settled.Reason, ReasonRunCancelled)
	}

	// A READY node -- claimed by the frontier but not yet dispatched -- is the
	// same hazard and must settle too.
	dr := Reconcile(ReconcileInput{
		Graph:    g,
		States:   States{"a": NodeSucceeded, "b": NodeReady},
		RunState: RunCancelling,
	})
	if dr.RunState != RunCancelled {
		t.Errorf("run state = %s with a READY node, want CANCELLED", dr.RunState)
	}

	// Pause must NOT settle anything: it is meant to be resumed, so its PENDING
	// nodes have to survive to be dispatched afterwards. Settling them would make
	// pause destructive, which is the bug §5.2 exists to prevent.
	dp := Reconcile(ReconcileInput{
		Graph:    g,
		States:   States{"a": NodeSucceeded, "b": NodePending},
		RunState: RunPausing,
	})
	for _, tr := range dp.Transitions {
		if tr.NodeID == "b" {
			t.Errorf("pause settled node b to %s; a paused run must keep its pending work", tr.To)
		}
	}
}

// ── Reconcile end to end ──────────────────────────────────────────────────────

// Driving a full run to completion one reconcile at a time is the closest a pure
// test gets to the real loop, and it catches ordering bugs that single-step tests
// miss.
func TestReconcile_DrivesADiamondToSuccess(t *testing.T) {
	g := diamond(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
	st := States{}
	rs := RunRunning
	dispatched := map[string]int{}

	for i := 0; i < 20; i++ {
		d := Reconcile(ReconcileInput{Graph: g, States: st, RunState: rs})
		rs = d.RunState
		if rs == RunSucceeded || rs == RunFailed {
			break
		}
		for _, id := range d.Frontier {
			dispatched[id]++
			st[id] = NodeSucceeded // simulate the scheduler + worker succeeding
		}
		st = ApplyTransitions(st, d.Transitions)
		for _, id := range d.Frontier {
			st[id] = NodeSucceeded
		}
	}

	if rs != RunSucceeded {
		t.Fatalf("run ended %s, want SUCCEEDED (states=%v)", rs, st)
	}
	for id, n := range dispatched {
		if n != 1 {
			t.Errorf("node %q dispatched %d times, want exactly 1", id, n)
		}
	}
	if len(dispatched) != 4 {
		t.Errorf("dispatched %d nodes, want 4: %v", len(dispatched), dispatched)
	}
}

func TestReconcile_DrivesAFailureToTerminalFailed(t *testing.T) {
	g := diamond(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
	st := States{}
	rs := RunRunning

	for i := 0; i < 20; i++ {
		d := Reconcile(ReconcileInput{Graph: g, States: st, RunState: rs})
		st = ApplyTransitions(st, d.Transitions)
		rs = d.RunState
		if rs == RunSucceeded || rs == RunFailed {
			break
		}
		for _, id := range d.Frontier {
			if id == "b" {
				st[id] = NodeFailed
			} else {
				st[id] = NodeSucceeded
			}
		}
	}
	if rs != RunFailed {
		t.Fatalf("run ended %s, want FAILED (states=%v)", rs, st)
	}
	if st.Get("d") != NodeUpstreamFailed {
		t.Errorf("join d = %s, want UPSTREAM_FAILED", st.Get("d"))
	}
	if st.Get("b") != NodeFailed {
		t.Errorf("b = %s, want FAILED (the node that actually broke)", st.Get("b"))
	}
	if st.Get("c") != NodeSucceeded {
		t.Errorf("c = %s, want SUCCEEDED (the branch that worked)", st.Get("c"))
	}
}

// Reconcile must be pure: it may not mutate the caller's state map, or a caller
// that reconciles speculatively would corrupt its own view.
func TestReconcile_DoesNotMutateItsInput(t *testing.T) {
	g := diamond(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
	st := States{"a": NodeFailed}
	before := fmt.Sprint(st)
	_ = Reconcile(ReconcileInput{Graph: g, States: st, RunState: RunRunning})
	if after := fmt.Sprint(st); after != before {
		t.Errorf("Reconcile mutated its input: %s -> %s", before, after)
	}
}

// Determinism: the same input must give byte-identical output. Without it the
// property tests below would be meaningless and run_events would be unstable.
func TestReconcile_IsDeterministic(t *testing.T) {
	g := diamond(nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE)
	st := States{"a": NodeSucceeded, "b": NodeFailed, "c": NodeSkipped}
	first := fmt.Sprint(Reconcile(ReconcileInput{Graph: g, States: st, RunState: RunRunning}))
	for i := 0; i < 50; i++ {
		if got := fmt.Sprint(Reconcile(ReconcileInput{Graph: g, States: st, RunState: RunRunning})); got != first {
			t.Fatalf("non-deterministic output on iteration %d:\n%s\nvs\n%s", i, got, first)
		}
	}
}

func TestStates_GetDefaultsToPending(t *testing.T) {
	st := States{"a": "", "b": NodeRunning}
	if st.Get("a") != NodePending {
		t.Errorf("empty state should read as PENDING, got %s", st.Get("a"))
	}
	if st.Get("missing") != NodePending {
		t.Errorf("absent node should read as PENDING, got %s", st.Get("missing"))
	}
	if st.Get("b") != NodeRunning {
		t.Errorf("present state lost: %s", st.Get("b"))
	}
}

func TestNodeState_TerminalAndFailed(t *testing.T) {
	terminal := map[NodeState]bool{
		NodePending: false, NodeReady: false, NodeRunning: false,
		NodeSucceeded: true, NodeFailed: true, NodeSkipped: true,
		NodeUpstreamFailed: true, NodeCancelled: true,
	}
	for s, want := range terminal {
		if got := s.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %v, want %v", s, got, want)
		}
	}
	failed := map[NodeState]bool{
		NodeFailed: true, NodeUpstreamFailed: true,
		NodeSucceeded: false, NodeSkipped: false, NodeCancelled: false,
		NodePending: false, NodeReady: false, NodeRunning: false,
	}
	for s, want := range failed {
		if got := s.Failed(); got != want {
			t.Errorf("%s.Failed() = %v, want %v", s, got, want)
		}
	}
}

func TestProtoEnumRoundTrips(t *testing.T) {
	for _, s := range []NodeState{
		NodePending, NodeReady, NodeRunning, NodeSucceeded,
		NodeFailed, NodeSkipped, NodeUpstreamFailed, NodeCancelled,
	} {
		if got := NodeStateFromProto(NodeStateToProto(s)); got != s {
			t.Errorf("node state %s round-tripped to %s", s, got)
		}
	}
	rules := map[nexusv1.TriggerRule]TriggerRule{
		nexusv1.TriggerRule_TRIGGER_RULE_UNSPECIFIED: AllSuccess, // proto3 default
		nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS: AllSuccess,
		nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE:    AllDone,
		nexusv1.TriggerRule_TRIGGER_RULE_ONE_SUCCESS: OneSuccess,
		nexusv1.TriggerRule_TRIGGER_RULE_ONE_FAILED:  OneFailed,
		nexusv1.TriggerRule_TRIGGER_RULE_NONE_FAILED: NoneFailed,
		nexusv1.TriggerRule_TRIGGER_RULE_ALL_SKIPPED: AllSkipped,
	}
	for in, want := range rules {
		if got := TriggerRuleFromProto(in); got != want {
			t.Errorf("TriggerRuleFromProto(%v) = %s, want %s", in, got, want)
		}
	}
	for _, s := range []nexusv1.AttemptState{
		nexusv1.AttemptState_ATTEMPT_STATE_PENDING,
		nexusv1.AttemptState_ATTEMPT_STATE_DISPATCHED,
		nexusv1.AttemptState_ATTEMPT_STATE_RUNNING,
		nexusv1.AttemptState_ATTEMPT_STATE_SUCCEEDED,
		nexusv1.AttemptState_ATTEMPT_STATE_FAILED,
		nexusv1.AttemptState_ATTEMPT_STATE_TIMED_OUT,
		nexusv1.AttemptState_ATTEMPT_STATE_LOST,
		nexusv1.AttemptState_ATTEMPT_STATE_CANCELLED,
	} {
		if AttemptStateFromProto(s) == "" {
			t.Errorf("attempt state %v has no canonical name", s)
		}
	}
}

// TIMED_OUT and LOST are ATTEMPT states with no node-state counterpart; §5.2
// nonetheless names TIMED_OUT in the run-disqualifying set. This fold is where
// the two vocabularies are reconciled, so it is tested explicitly.
func TestAttemptStateToNode(t *testing.T) {
	tests := []struct {
		attempt   string
		exhausted bool
		want      NodeState
	}{
		{"SUCCEEDED", false, NodeSucceeded},
		{"SUCCEEDED", true, NodeSucceeded},
		{"CANCELLED", true, NodeCancelled},
		{"FAILED", false, NodeRunning},
		{"FAILED", true, NodeFailed},
		{"TIMED_OUT", false, NodeRunning},
		{"TIMED_OUT", true, NodeFailed},
		{"LOST", false, NodeRunning},
		{"LOST", true, NodeFailed},
		{"PENDING", false, NodeReady},
		{"DISPATCHED", false, NodeRunning},
		{"RUNNING", false, NodeRunning},
		{"nonsense", false, NodePending},
	}
	for _, tc := range tests {
		if got := AttemptStateToNode(tc.attempt, tc.exhausted); got != tc.want {
			t.Errorf("AttemptStateToNode(%s, exhausted=%v) = %s, want %s",
				tc.attempt, tc.exhausted, got, tc.want)
		}
	}
}

func TestGraphAccessorsAreDefensiveCopies(t *testing.T) {
	g := diamond(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
	n := g.Nodes()
	n[0] = "clobbered"
	if g.Nodes()[0] == "clobbered" {
		t.Error("Nodes() leaked its backing array")
	}
	p := g.Preds("d")
	if len(p) != 2 {
		t.Fatalf("d should have 2 preds, got %v", p)
	}
	p[0] = "clobbered"
	if g.Preds("d")[0] == "clobbered" {
		t.Error("Preds() leaked its backing array")
	}
	s := g.Succs("a")
	s[0] = "clobbered"
	if g.Succs("a")[0] == "clobbered" {
		t.Error("Succs() leaked its backing array")
	}
	e := g.Edges()
	if len(e) != 4 {
		t.Fatalf("diamond should have 4 edges, got %d", len(e))
	}
	if got := g.EdgesTo("d"); len(got) != 2 {
		t.Errorf("EdgesTo(d) = %v, want 2 edges", got)
	}
	if g.Node("nope") != nil {
		t.Error("Node() should return nil for an unknown id")
	}
}
