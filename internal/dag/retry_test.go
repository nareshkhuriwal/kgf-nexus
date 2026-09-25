// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package dag

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// resetIDs renders a plan's resets as "id:FROM->TO" so a failing table row says
// exactly which node moved where.
func resetIDs(p RetryPlan) []string {
	var out []string
	for _, r := range p.Resets {
		out = append(out, r.NodeID+":"+string(r.From)+"->"+string(r.To))
	}
	return out
}

func mustRefuse(t *testing.T, err error) *RetryError {
	t.Helper()
	var re *RetryError
	if !errors.As(err, &re) {
		t.Fatalf("PlanRetry err = %v, want *RetryError", err)
	}
	return re
}

// The core semantics on one graph: a fan-out where one branch failed.
//
//	a -> b -> d
//	a -> c -> d        e is independent of everything
//	d -> f (all_done)
func branchGraph(failFast bool) *Graph {
	return wf().failFast(failFast).
		t("a", "b", "c", "d", "e").task("f", nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE).
		e([2]string{"a", "b"}, [2]string{"a", "c"}, [2]string{"b", "d"}, [2]string{"c", "d"},
			[2]string{"d", "f"}).
		build()
}

func TestPlanRetry_SelectedNodeAndItsNonSucceededDependents(t *testing.T) {
	g := branchGraph(false)
	st := States{
		"a": NodeSucceeded, "b": NodeFailed, "c": NodeSucceeded,
		"d": NodeUpstreamFailed, "e": NodeFailed, "f": NodeSucceeded,
	}
	plan, err := PlanRetry(RetryInput{Graph: g, States: st, Selected: []string{"b"}})
	if err != nil {
		t.Fatalf("PlanRetry: %v", err)
	}
	// b and its UPSTREAM_FAILED dependent d reset. a and c (succeeded upstream)
	// are untouched; f SUCCEEDED under all_done and stays; e failed too but was
	// not selected and is not downstream of b.
	want := []string{"b:FAILED->PENDING", "d:UPSTREAM_FAILED->PENDING"}
	if got := resetIDs(plan); !reflect.DeepEqual(got, want) {
		t.Errorf("resets = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(plan.Selected, []string{"b"}) {
		t.Errorf("selected = %v, want [b]", plan.Selected)
	}
}

func TestPlanRetry_DependentsAreResetTransitivelyWhateverTheirTerminalState(t *testing.T) {
	g := chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE, "a", "b", "c", "d")
	// all_done lets the chain keep running past failures, so descendants can
	// be FAILED themselves, CANCELLED, or SKIPPED -- all are reset.
	st := States{"a": NodeFailed, "b": NodeFailed, "c": NodeCancelled, "d": NodeSkipped}
	plan, err := PlanRetry(RetryInput{Graph: g, States: st, Selected: []string{"a"}})
	if err != nil {
		t.Fatalf("PlanRetry: %v", err)
	}
	want := []string{"a:FAILED->PENDING", "b:FAILED->PENDING", "c:CANCELLED->PENDING", "d:SKIPPED->PENDING"}
	if got := resetIDs(plan); !reflect.DeepEqual(got, want) {
		t.Errorf("resets = %v, want %v", got, want)
	}
}

func TestPlanRetry_SucceededDescendantIsNeverReset(t *testing.T) {
	g := chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE, "a", "b", "c")
	st := States{"a": NodeFailed, "b": NodeSucceeded, "c": NodeFailed}
	plan, err := PlanRetry(RetryInput{Graph: g, States: st, Selected: []string{"a"}})
	if err != nil {
		t.Fatalf("PlanRetry: %v", err)
	}
	// The walk continues THROUGH the succeeded b, so c (a descendant of a) is
	// reset, but b itself is not re-run.
	want := []string{"a:FAILED->PENDING", "c:FAILED->PENDING"}
	if got := resetIDs(plan); !reflect.DeepEqual(got, want) {
		t.Errorf("resets = %v, want %v", got, want)
	}
}

func TestPlanRetry_RefusesUnknownAndNonRetryableNodesAllOrNothing(t *testing.T) {
	g := branchGraph(false)
	st := States{
		"a": NodeSucceeded, "b": NodeFailed, "c": NodeSkipped,
		"d": NodeUpstreamFailed, "e": NodeRunning, "f": NodePending,
	}
	_, err := PlanRetry(RetryInput{Graph: g, States: st, Selected: []string{"b", "a", "c", "e", "f", "nope", "zzz"}})
	re := mustRefuse(t, err)
	if !reflect.DeepEqual(re.Unknown, []string{"nope", "zzz"}) {
		t.Errorf("unknown = %v, want [nope zzz]", re.Unknown)
	}
	got := map[string]string{}
	for _, r := range re.Refused {
		got[r.NodeID] = string(r.State) + "/" + r.Reason
	}
	want := map[string]string{
		"a": "SUCCEEDED/state_not_retryable",
		"c": "SKIPPED/state_not_retryable",
		"e": "RUNNING/state_not_retryable",
		"f": "PENDING/state_not_retryable",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("refused = %v, want %v", got, want)
	}
	// Every problem is reported at once, so an operator fixes the request in
	// one round trip rather than one node per attempt.
	for _, frag := range []string{"nope", "zzz", "node a is SUCCEEDED", "node c is SKIPPED"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("error %q does not mention %q", err, frag)
		}
	}
}

func TestPlanRetry_RefusesASelectionThatWouldFailAgainAtOnce(t *testing.T) {
	g := chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b", "c")
	st := States{"a": NodeFailed, "b": NodeUpstreamFailed, "c": NodeUpstreamFailed}

	// b alone: a stays FAILED, so b would be UPSTREAM_FAILED again on the next
	// reconcile. Refused, naming a.
	_, err := PlanRetry(RetryInput{Graph: g, States: st, Selected: []string{"b"}})
	re := mustRefuse(t, err)
	if len(re.Refused) != 1 || re.Refused[0].Reason != RefusalWouldResettle || re.Refused[0].Cause != "a" {
		t.Fatalf("refused = %+v, want b would_fail_again caused by a", re.Refused)
	}
	if !strings.Contains(err.Error(), "select a as well") {
		t.Errorf("error %q does not tell the operator what to select", err)
	}

	// Selecting the root cause as well is accepted.
	plan, err := PlanRetry(RetryInput{Graph: g, States: st, Selected: []string{"a", "b"}})
	if err != nil {
		t.Fatalf("PlanRetry(a,b): %v", err)
	}
	if len(plan.Resets) != 3 {
		t.Errorf("resets = %v, want all three", resetIDs(plan))
	}
}

func TestPlanRetry_CancelledUpstreamThatIsNotRetriedIsNamed(t *testing.T) {
	g := chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b")
	st := States{"a": NodeCancelled, "b": NodeCancelled}
	_, err := PlanRetry(RetryInput{Graph: g, States: st, Selected: []string{"b"}})
	re := mustRefuse(t, err)
	if len(re.Refused) != 1 || re.Refused[0].Cause != "a" {
		t.Fatalf("refused = %+v, want b blocked by cancelled a", re.Refused)
	}
}

func TestPlanRetry_FailFastRefusesWhileAnotherFailureStands(t *testing.T) {
	g := branchGraph(true)
	st := States{
		"a": NodeSucceeded, "b": NodeFailed, "c": NodeSucceeded,
		"d": NodeUpstreamFailed, "e": NodeFailed, "f": NodeSucceeded,
	}
	_, err := PlanRetry(RetryInput{Graph: g, States: st, Selected: []string{"b"}})
	re := mustRefuse(t, err)
	if len(re.Refused) != 1 || re.Refused[0].Reason != RefusalFailFastHalted || re.Refused[0].Cause != "e" {
		t.Fatalf("refused = %+v, want b fail_fast_halted by e", re.Refused)
	}
	if _, err := PlanRetry(RetryInput{Graph: g, States: st, Selected: []string{"b", "e"}}); err != nil {
		t.Fatalf("retrying both failures under fail_fast: %v", err)
	}
}

func TestPlanRetry_InFlightNodeIsRefused(t *testing.T) {
	g := chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a")
	_, err := PlanRetry(RetryInput{
		Graph: g, States: States{"a": NodeCancelled}, Selected: []string{"a"},
		InFlight: map[string]bool{"a": true},
	})
	re := mustRefuse(t, err)
	if len(re.Refused) != 1 || re.Refused[0].Reason != RefusalAttemptInFlight {
		t.Fatalf("refused = %+v, want attempt_in_flight", re.Refused)
	}
}

func TestPlanRetry_WholeRunSelectsEveryNodeThatDidNotSucceed(t *testing.T) {
	g := branchGraph(true)
	st := States{
		"a": NodeSucceeded, "b": NodeFailed, "c": NodeSucceeded,
		"d": NodeUpstreamFailed, "e": NodeSkipped, "f": NodeCancelled,
	}
	plan, err := PlanRetry(RetryInput{Graph: g, States: st})
	if err != nil {
		t.Fatalf("PlanRetry: %v", err)
	}
	// e was SKIPPED by fail_fast; a whole-run retry must re-evaluate it, which
	// is why the default is wider than what an operator may name.
	want := []string{"b:FAILED->PENDING", "d:UPSTREAM_FAILED->PENDING", "e:SKIPPED->PENDING", "f:CANCELLED->PENDING"}
	if got := resetIDs(plan); !reflect.DeepEqual(got, want) {
		t.Errorf("resets = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(plan.Selected, []string{"b", "d", "e", "f"}) {
		t.Errorf("selected = %v", plan.Selected)
	}
}

func TestPlanRetry_WholeRunWithNothingToResetIsAnEmptyPlan(t *testing.T) {
	g := chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b")
	plan, err := PlanRetry(RetryInput{Graph: g, States: States{"a": NodeSucceeded, "b": NodePending}})
	if err != nil {
		t.Fatalf("PlanRetry: %v", err)
	}
	if len(plan.Resets) != 0 {
		t.Errorf("resets = %v, want none", resetIDs(plan))
	}
}

// The plan's resets, fed to the real reconcile, must produce the expected next
// step: the retried node dispatches and its dependents wait. This is the check
// that the planner and the engine agree about what a reset means.
func TestPlanRetry_ResetGraphReconcilesToTheRetriedFrontier(t *testing.T) {
	g := chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b", "c")
	st := States{"a": NodeSucceeded, "b": NodeFailed, "c": NodeUpstreamFailed}
	plan, err := PlanRetry(RetryInput{Graph: g, States: st, Selected: []string{"b"}})
	if err != nil {
		t.Fatalf("PlanRetry: %v", err)
	}
	d := Reconcile(ReconcileInput{Graph: g, States: ApplyResets(st, plan.Resets), RunState: RunRunning})
	if !reflect.DeepEqual(d.Frontier, []string{"b"}) {
		t.Errorf("frontier after reset = %v, want [b]", d.Frontier)
	}
	if d.RunState != RunRunning {
		t.Errorf("run state after reset = %s, want RUNNING", d.RunState)
	}
	for _, tr := range d.Transitions {
		if tr.NodeID == "c" {
			t.Errorf("c moved to %s; it must wait for b", tr.To)
		}
	}
}

// ── dynamic expansion ──────────────────────────────────────────────────────

func TestPlanRetry_ChildResetsItsTemplateButNotItsSiblings(t *testing.T) {
	g := expandedGraph(mapSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS), 3)
	c0, c1, c2 := ChildID("process", 0), ChildID("process", 1), ChildID("process", 2)
	st := States{
		"list": NodeSucceeded, "process": NodeFailed, "summarize": NodeUpstreamFailed,
		c0: NodeSucceeded, c1: NodeFailed, c2: NodeFailed,
	}
	plan, err := PlanRetry(RetryInput{
		Graph: g, States: st, Selected: []string{c1},
		Expansions: map[string]ExpansionStatus{"process": Expanded},
	})
	if err != nil {
		t.Fatalf("PlanRetry: %v", err)
	}
	want := []string{
		"process:FAILED->RUNNING",
		c1 + ":FAILED->PENDING",
		"summarize:UPSTREAM_FAILED->PENDING",
	}
	if got := resetIDs(plan); !reflect.DeepEqual(got, want) {
		t.Errorf("resets = %v, want %v", got, want)
	}
}

func TestPlanRetry_ExpandedTemplateResetsToRunningWithItsFailedChildren(t *testing.T) {
	g := expandedGraph(mapSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS), 2)
	c0, c1 := ChildID("process", 0), ChildID("process", 1)
	st := States{
		"list": NodeSucceeded, "process": NodeFailed, "summarize": NodeUpstreamFailed,
		c0: NodeSucceeded, c1: NodeFailed,
	}
	exp := map[string]ExpansionStatus{"process": Expanded}
	plan, err := PlanRetry(RetryInput{Graph: g, States: st, Selected: []string{"process"}, Expansions: exp})
	if err != nil {
		t.Fatalf("PlanRetry: %v", err)
	}
	// RUNNING, never PENDING: the expansion record exists, so a PENDING
	// template would wait forever for an expansion its primary key refuses.
	want := []string{"process:FAILED->RUNNING", c1 + ":FAILED->PENDING", "summarize:UPSTREAM_FAILED->PENDING"}
	if got := resetIDs(plan); !reflect.DeepEqual(got, want) {
		t.Errorf("resets = %v, want %v", got, want)
	}

	// And the reconcile that follows dispatches exactly the failed child.
	d := Reconcile(ReconcileInput{Graph: g, States: ApplyResets(st, plan.Resets), RunState: RunRunning})
	if !reflect.DeepEqual(d.Frontier, []string{c1}) {
		t.Errorf("frontier = %v, want [%s]", d.Frontier, c1)
	}
}

func TestPlanRetry_UnexpandedTemplateGoesBackToPending(t *testing.T) {
	g := Build(mapSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS))
	st := States{"list": NodeFailed, "process": NodeUpstreamFailed, "summarize": NodeUpstreamFailed}
	plan, err := PlanRetry(RetryInput{Graph: g, States: st, Selected: []string{"list"}})
	if err != nil {
		t.Fatalf("PlanRetry: %v", err)
	}
	want := []string{"list:FAILED->PENDING", "process:UPSTREAM_FAILED->PENDING", "summarize:UPSTREAM_FAILED->PENDING"}
	if got := resetIDs(plan); !reflect.DeepEqual(got, want) {
		t.Errorf("resets = %v, want %v", got, want)
	}
}

func TestPlanRetry_RefusedExpansionIsNotRetryable(t *testing.T) {
	g := Build(mapSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS))
	st := States{"list": NodeSucceeded, "process": NodeFailed, "summarize": NodeUpstreamFailed}
	exp := map[string]ExpansionStatus{"process": ExpansionRefused}

	_, err := PlanRetry(RetryInput{Graph: g, States: st, Selected: []string{"process"}, Expansions: exp})
	re := mustRefuse(t, err)
	if len(re.Refused) != 1 || re.Refused[0].Reason != RefusalExpansionRefused {
		t.Fatalf("refused = %+v, want expansion_refused", re.Refused)
	}

	// A whole-run retry leaves the refused template as it is rather than
	// wedging it in PENDING, and so leaves its dependents to fail again too.
	plan, err := PlanRetry(RetryInput{Graph: g, States: st, Expansions: exp})
	if err != nil {
		t.Fatalf("whole-run PlanRetry: %v", err)
	}
	for _, r := range plan.Resets {
		if r.NodeID == "process" {
			t.Errorf("refused template was reset: %+v", r)
		}
	}
}

func TestPlanRetry_DuplicateSelectionsAreHarmless(t *testing.T) {
	g := chain(nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, "a", "b")
	plan, err := PlanRetry(RetryInput{
		Graph: g, States: States{"a": NodeFailed, "b": NodeUpstreamFailed}, Selected: []string{"a", "a"},
	})
	if err != nil {
		t.Fatalf("PlanRetry: %v", err)
	}
	if !reflect.DeepEqual(plan.Selected, []string{"a"}) || len(plan.Resets) != 2 {
		t.Errorf("plan = %+v", plan)
	}
}
