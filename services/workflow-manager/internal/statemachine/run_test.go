// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package statemachine

import (
	"errors"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// allStates is the closed set from CONTRACT.md. Written out literally rather than
// derived from the table, so a typo in the table cannot make the exhaustive tests
// below vacuously pass.
var allStates = []domain.RunState{
	domain.RunPending, domain.RunRunning, domain.RunPausing, domain.RunPaused,
	domain.RunSucceeded, domain.RunFailed, domain.RunCancelling,
	domain.RunCancelled, domain.RunTimedOut,
}

var allTriggers = []Trigger{
	TriggerStart, TriggerPauseRequest, TriggerDrained, TriggerResume,
	TriggerCancelRequest, TriggerCancelAcked, TriggerAllNodesTerminal,
	TriggerNodeFailedFailFast, TriggerRunDeadline, TriggerRetryFromFailed,
}

func TestEveryContractStateIsInTheTable(t *testing.T) {
	for _, s := range allStates {
		if _, ok := table[s]; !ok {
			t.Errorf("state %s from CONTRACT.md is missing from the transition table", s)
		}
	}
	if len(table) != len(allStates) {
		t.Errorf("table has %d states, CONTRACT.md defines %d", len(table), len(allStates))
	}
}

// TestHappyPath walks the ordinary life of a run.
func TestHappyPath(t *testing.T) {
	state := domain.RunPending
	state = mustNext(t, state, TriggerStart, Outcome{})
	if state != domain.RunRunning {
		t.Fatalf("after start: %s, want RUNNING", state)
	}
	state = mustNext(t, state, TriggerAllNodesTerminal, Outcome{AnyFailed: false})
	if state != domain.RunSucceeded {
		t.Fatalf("after all nodes terminal with no failures: %s, want SUCCEEDED", state)
	}
}

// TestAllNodesTerminalRefinedByOutcome pins the §5.2 rule that the same trigger
// yields SUCCEEDED or FAILED depending on the node states, not on the run state.
func TestAllNodesTerminalRefinedByOutcome(t *testing.T) {
	got := mustNext(t, domain.RunRunning, TriggerAllNodesTerminal, Outcome{AnyFailed: true})
	if got != domain.RunFailed {
		t.Errorf("a run with a failed node reached %s, want FAILED", got)
	}
	got = mustNext(t, domain.RunRunning, TriggerAllNodesTerminal, Outcome{AnyFailed: false})
	if got != domain.RunSucceeded {
		t.Errorf("a run with no failed node reached %s, want SUCCEEDED", got)
	}
}

// TestPauseDrainsRatherThanKills is the behaviour §5.2 calls out specifically:
// PAUSING waits for in-flight work, it does not stop it.
func TestPauseDrainsRatherThanKills(t *testing.T) {
	state := mustNext(t, domain.RunRunning, TriggerPauseRequest, Outcome{})
	if state != domain.RunPausing {
		t.Fatalf("pause on a RUNNING run gave %s, want PAUSING", state)
	}

	// A PAUSING run must NOT be able to reach PAUSED by any trigger that implies
	// killing work; only draining gets it there.
	if _, err := Next(domain.RunPausing, TriggerPauseRequest, Outcome{}); err == nil {
		t.Error("a second pause request on a PAUSING run should be rejected, not treated as progress")
	}

	state = mustNext(t, state, TriggerDrained, Outcome{})
	if state != domain.RunPaused {
		t.Fatalf("after draining: %s, want PAUSED", state)
	}

	// Neither PAUSING nor PAUSED may dispatch NEW work; that is the entire
	// mechanism of a pause.
	for _, s := range []domain.RunState{domain.RunPausing, domain.RunPaused} {
		if CanDispatch(s) {
			t.Errorf("CanDispatch(%s) = true; a paused run must not start new attempts", s)
		}
	}
	// ...while RUNNING and PENDING may.
	for _, s := range []domain.RunState{domain.RunPending, domain.RunRunning} {
		if !CanDispatch(s) {
			t.Errorf("CanDispatch(%s) = false; an active run must be dispatchable", s)
		}
	}
}

func TestResumeFromBothPauseStates(t *testing.T) {
	// From PAUSED: the ordinary case.
	if got := mustNext(t, domain.RunPaused, TriggerResume, Outcome{}); got != domain.RunRunning {
		t.Errorf("resume from PAUSED gave %s, want RUNNING", got)
	}
	// From PAUSING: the operator changed their mind mid-drain. The in-flight
	// attempts never stopped, so this only reopens the dispatch gate.
	if got := mustNext(t, domain.RunPausing, TriggerResume, Outcome{}); got != domain.RunRunning {
		t.Errorf("resume from PAUSING gave %s, want RUNNING", got)
	}
}

func TestCancelPath(t *testing.T) {
	state := mustNext(t, domain.RunRunning, TriggerCancelRequest, Outcome{})
	if state != domain.RunCancelling {
		t.Fatalf("cancel gave %s, want CANCELLING", state)
	}
	// CANCELLING waits for workers to ack; it does not jump straight to CANCELLED.
	state = mustNext(t, state, TriggerCancelAcked, Outcome{})
	if state != domain.RunCancelled {
		t.Fatalf("after acks: %s, want CANCELLED", state)
	}
}

// TestCancelIsReachableFromEveryNonTerminalState: an operator must always be able
// to stop a run that is still doing something. A non-terminal state that cannot be
// cancelled would be an un-stoppable run.
func TestCancelIsReachableFromEveryNonTerminalState(t *testing.T) {
	for _, s := range allStates {
		if IsTerminal(s) || s == domain.RunCancelling {
			continue
		}
		if _, err := Next(s, TriggerCancelRequest, Outcome{}); err != nil {
			t.Errorf("cancel is not accepted from %s: %v", s, err)
		}
	}
}

// TestTerminalStatesAreAbsorbing is the invariant that makes the event log
// truthful: no transition out of a terminal state, ever.
func TestTerminalStatesAreAbsorbing(t *testing.T) {
	terminals := []domain.RunState{
		domain.RunSucceeded, domain.RunFailed, domain.RunCancelled, domain.RunTimedOut,
	}
	for _, s := range terminals {
		if !IsTerminal(s) {
			t.Errorf("IsTerminal(%s) = false, want true", s)
		}
		for _, tr := range allTriggers {
			if got, err := Next(s, tr, Outcome{AnyFailed: true}); err == nil {
				t.Errorf("terminal %s accepted %s and moved to %s; terminal states must be absorbing", s, tr, got)
			}
		}
		if CanDispatch(s) {
			t.Errorf("CanDispatch(%s) = true; a finished run must never dispatch", s)
		}
	}
}

// TestIllegalTransitionsAreRejected walks the ENTIRE (state, trigger) space and
// asserts that anything not in the table is refused. This is the negative space
// the table form exists to make testable: it catches an accidentally-permissive
// entry that no happy-path test would ever exercise.
func TestIllegalTransitionsAreRejected(t *testing.T) {
	for _, s := range allStates {
		for _, tr := range allTriggers {
			_, legal := table[s][tr]
			_, err := Next(s, tr, Outcome{})
			if legal && err != nil {
				t.Errorf("table permits %s + %s but Next rejected it: %v", s, tr, err)
			}
			if !legal && err == nil {
				t.Errorf("table forbids %s + %s but Next allowed it", s, tr)
			}
			if !legal {
				var ill ErrIllegalTransition
				if !errors.As(err, &ill) {
					t.Errorf("rejection of %s + %s is %T, want ErrIllegalTransition", s, tr, err)
				}
			}
		}
	}
}

// TestSpecificIllegalMoves names the mistakes that would be most damaging, so a
// future edit that re-permits one fails with an obvious message.
func TestSpecificIllegalMoves(t *testing.T) {
	cases := []struct {
		from   domain.RunState
		tr     Trigger
		reason string
	}{
		{domain.RunSucceeded, TriggerRunDeadline, "a finished run must not be timed out after the fact"},
		{domain.RunCancelled, TriggerResume, "a cancelled run must not be resumable"},
		{domain.RunPaused, TriggerStart, "a paused run must resume, not restart"},
		{domain.RunCancelling, TriggerResume, "cancellation must not be undoable by resume"},
		{domain.RunCancelling, TriggerRunDeadline, "a deadline must not override an in-progress cancel"},
		{domain.RunRunning, TriggerDrained, "drained is only meaningful while PAUSING"},
		{domain.RunRunning, TriggerCancelAcked, "cancel acks are only meaningful while CANCELLING"},
		{domain.RunPending, TriggerResume, "a run that was never paused cannot be resumed"},
	}
	for _, c := range cases {
		if _, err := Next(c.from, c.tr, Outcome{}); err == nil {
			t.Errorf("%s + %s was permitted; %s", c.from, c.tr, c.reason)
		}
	}
}

// TestPendingPauseSkipsPausing: with no in-flight attempts there is nothing to
// drain, so PAUSING would be a state the run leaves in the same instant.
func TestPendingPauseSkipsPausing(t *testing.T) {
	if got := mustNext(t, domain.RunPending, TriggerPauseRequest, Outcome{}); got != domain.RunPaused {
		t.Errorf("pause on a PENDING run gave %s, want PAUSED directly", got)
	}
}

// TestPausedRunStillTimesOut: run_timeout_ms is an end-to-end SLA. A pause that
// silently suspended it would make the guarantee unfalsifiable.
func TestPausedRunStillTimesOut(t *testing.T) {
	for _, s := range []domain.RunState{domain.RunPausing, domain.RunPaused} {
		got, err := Next(s, TriggerRunDeadline, Outcome{})
		if err != nil {
			t.Errorf("run deadline rejected from %s: %v", s, err)
			continue
		}
		if got != domain.RunTimedOut {
			t.Errorf("deadline from %s gave %s, want TIMED_OUT", s, got)
		}
	}
}

func TestFailFastFailsTheRun(t *testing.T) {
	if got := mustNext(t, domain.RunRunning, TriggerNodeFailedFailFast, Outcome{}); got != domain.RunFailed {
		t.Errorf("fail_fast node failure gave %s, want FAILED", got)
	}
}

func TestRetryFromFailed(t *testing.T) {
	for _, s := range []domain.RunState{domain.RunFailed, domain.RunTimedOut} {
		if err := RetryFromFailed(s); err != nil {
			t.Errorf("RetryFromFailed(%s) = %v, want permitted", s, err)
		}
	}
	// Retrying a cancelled run would override a deliberate human decision;
	// retrying a succeeded run is a rerun, a different operation.
	for _, s := range []domain.RunState{
		domain.RunSucceeded, domain.RunCancelled, domain.RunRunning,
		domain.RunPaused, domain.RunPending, domain.RunPausing, domain.RunCancelling,
	} {
		if err := RetryFromFailed(s); err == nil {
			t.Errorf("RetryFromFailed(%s) was permitted; only genuinely failed runs may be retried", s)
		}
	}
}

// TestFromStatesForMatchesTable guards the coupling between this machine and the
// SQL compare-and-set that enforces it. If they disagree, the database would
// permit a transition the machine forbids, or vice versa.
func TestFromStatesForMatchesTable(t *testing.T) {
	for _, tr := range allTriggers {
		got := FromStatesFor(tr)
		seen := map[string]bool{}
		for _, s := range got {
			seen[s] = true
		}
		for _, s := range allStates {
			_, legal := table[s][tr]
			if legal != seen[string(s)] {
				t.Errorf("FromStatesFor(%s) disagrees with the table about %s: table=%v, list=%v", tr, s, legal, seen[string(s)])
			}
		}
	}
}

func TestLegalTriggersIsSortedAndComplete(t *testing.T) {
	got := LegalTriggers(domain.RunPaused)
	if len(got) != len(table[domain.RunPaused]) {
		t.Fatalf("LegalTriggers returned %d triggers, table has %d", len(got), len(table[domain.RunPaused]))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("LegalTriggers is not sorted: %v", got)
			break
		}
	}
}

func TestUnknownStateIsAnError(t *testing.T) {
	if _, err := Next(domain.RunState("NOT_A_STATE"), TriggerStart, Outcome{}); err == nil {
		t.Error("an unknown state must be an error, not silently treated as terminal")
	}
}

func mustNext(t *testing.T, from domain.RunState, tr Trigger, o Outcome) domain.RunState {
	t.Helper()
	got, err := Next(from, tr, o)
	if err != nil {
		t.Fatalf("Next(%s, %s): %v", from, tr, err)
	}
	return got
}

// TestAllTerminalWithCancelledNode covers the §5.2 gap recorded as
// CONTRACT-FINDINGS.md F3: an all-terminal run with a cancelled node and no failure
// is neither SUCCEEDED (it did not achieve its goal) nor FAILED (nothing broke).
func TestAllTerminalWithCancelledNode(t *testing.T) {
	got := mustNext(t, domain.RunRunning, TriggerAllNodesTerminal,
		Outcome{AnyCancelled: true})
	if got != domain.RunCancelled {
		t.Errorf("a run with a cancelled node and no failure reached %s, want CANCELLED", got)
	}
}

// TestFailureOutranksCancellation: if something genuinely broke, that is the fact an
// operator needs. Reporting CANCELLED would hide a real fault.
func TestFailureOutranksCancellation(t *testing.T) {
	got := mustNext(t, domain.RunRunning, TriggerAllNodesTerminal,
		Outcome{AnyFailed: true, AnyCancelled: true})
	if got != domain.RunFailed {
		t.Errorf("a run with BOTH a failure and a cancellation reached %s, want FAILED", got)
	}
}
