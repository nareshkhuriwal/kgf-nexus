// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package statemachine encodes the WorkflowRun state machine from architecture
// §5.2.
//
// It is deliberately PURE: no database, no clock, no I/O. The legality of a
// transition is a property of the states involved and nothing else, so it can be
// exhaustively unit-tested, including the negative space — which transitions must
// be impossible. A state machine tangled with its persistence is one that gets
// tested only along the paths someone remembered to write an integration test for.
package statemachine

import (
	"fmt"
	"sort"

	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// Trigger is the cause of a proposed transition. The same target state can be
// legal from one trigger and illegal from another (a run reaches PAUSED by
// draining, never by an operator command), so the trigger is part of the input
// rather than inferred from the target.
type Trigger string

const (
	// TriggerStart is the DAG engine reporting the initial frontier is computed.
	TriggerStart Trigger = "start"
	// TriggerPauseRequest is an operator asking to hold the pipeline.
	TriggerPauseRequest Trigger = "pause_request"
	// TriggerDrained is the reconciler observing that a PAUSING run has no
	// in-flight attempts left.
	TriggerDrained Trigger = "drained"
	// TriggerResume is an operator releasing a hold.
	TriggerResume Trigger = "resume"
	// TriggerCancelRequest is an operator stopping a run.
	TriggerCancelRequest Trigger = "cancel_request"
	// TriggerCancelAcked is every in-flight attempt having confirmed it stopped.
	TriggerCancelAcked Trigger = "cancel_acked"
	// TriggerAllNodesTerminal is the DAG engine reporting the graph is finished.
	TriggerAllNodesTerminal Trigger = "all_nodes_terminal"
	// TriggerNodeFailedFailFast is a terminal node failure under fail_fast.
	TriggerNodeFailedFailFast Trigger = "node_failed_fail_fast"
	// TriggerRunDeadline is the run_timeout_ms timer firing.
	TriggerRunDeadline Trigger = "run_deadline"
	// TriggerRetryFromFailed is an operator retrying a terminal failed run.
	TriggerRetryFromFailed Trigger = "retry_from_failed"
)

// Outcome carries the graph's aggregate result for TriggerAllNodesTerminal.
//
// A run whose nodes are all terminal is SUCCEEDED, FAILED, or CANCELLED depending on
// WHICH terminal states they reached, which is information the state machine cannot
// derive from the run state alone.
type Outcome struct {
	// AnyFailed is true when at least one node is in FAILED or UPSTREAM_FAILED —
	// the §5.2 definition of a failed run.
	AnyFailed bool
	// AnyCancelled is true when at least one node is CANCELLED.
	//
	// §5.2 as written has no rule for an all-terminal run containing a CANCELLED
	// node and no failure, which is a real gap: SUCCEEDED is a lie because the run
	// did not achieve its goal, and FAILED misdirects the operator into hunting a
	// fault that does not exist. We report CANCELLED, matching the resolution the
	// dag-engine workstream adopted for the same gap (CONTRACT-FINDINGS.md F3), so
	// the two services cannot disagree about what a half-cancelled run is.
	AnyCancelled bool
}

// Transition is a legal move.
type Transition struct {
	From    domain.RunState
	Trigger Trigger
	To      domain.RunState
}

// ErrIllegalTransition means the machine refuses the move.
type ErrIllegalTransition struct {
	From    domain.RunState
	Trigger Trigger
}

func (e ErrIllegalTransition) Error() string {
	return fmt.Sprintf("illegal run transition: %s cannot handle %s", e.From, e.Trigger)
}

// table is §5.2 stated once, as data.
//
// Written as a table rather than a switch so the machine can be ENUMERATED: the
// tests walk every (state, trigger) pair and assert that everything absent here
// is rejected. A switch statement can only be tested along the branches someone
// thought to write.
var table = map[domain.RunState]map[Trigger]domain.RunState{
	domain.RunPending: {
		TriggerStart:         domain.RunRunning,
		TriggerCancelRequest: domain.RunCancelling,
		TriggerRunDeadline:   domain.RunTimedOut,
		// A pause before the run has started is honoured immediately: with no
		// in-flight attempts there is nothing to drain, so PAUSING would be a
		// state the run leaves in the same instant it enters.
		TriggerPauseRequest: domain.RunPaused,
		// An empty or fully-skipped graph is terminal on arrival.
		TriggerAllNodesTerminal: domain.RunSucceeded,
	},
	domain.RunRunning: {
		TriggerPauseRequest:       domain.RunPausing,
		TriggerCancelRequest:      domain.RunCancelling,
		TriggerRunDeadline:        domain.RunTimedOut,
		TriggerAllNodesTerminal:   domain.RunSucceeded, // refined by Outcome
		TriggerNodeFailedFailFast: domain.RunFailed,
	},
	domain.RunPausing: {
		// PAUSING means no NEW attempts are dispatched while in-flight ones RUN TO
		// COMPLETION. Pausing does not kill work — that is cancel. Conflating them
		// makes pause useless for its main job: holding a pipeline while you
		// investigate WITHOUT losing hours of compute (§5.2). So PAUSING waits for
		// TriggerDrained; it never forces anything to stop.
		TriggerDrained: domain.RunPaused,
		// Cancelling out of PAUSING is legal and is the escalation path: the
		// operator looked, decided, and now genuinely does want the work stopped.
		TriggerCancelRequest: domain.RunCancelling,
		TriggerRunDeadline:   domain.RunTimedOut,
		// Resuming mid-drain returns to RUNNING directly. The in-flight attempts
		// never stopped, so there is nothing to restart — only the dispatch gate
		// to reopen.
		TriggerResume: domain.RunRunning,
		// A run can finish while draining: the in-flight attempts it was waiting
		// for were the last ones.
		TriggerAllNodesTerminal:   domain.RunSucceeded,
		TriggerNodeFailedFailFast: domain.RunFailed,
	},
	domain.RunPaused: {
		TriggerResume:        domain.RunRunning,
		TriggerCancelRequest: domain.RunCancelling,
		// A paused run still burns its wall-clock budget. Not exempting it is
		// deliberate: run_timeout_ms is an end-to-end SLA, and a pause that
		// silently suspended the SLA would make the guarantee unfalsifiable.
		TriggerRunDeadline: domain.RunTimedOut,
	},
	domain.RunCancelling: {
		TriggerCancelAcked: domain.RunCancelled,
		// Already cancelling; a deadline changes nothing. Cancellation is the more
		// specific operator intent and wins.
	},
	// Terminal states have NO outgoing transitions. §5.1's rule for attempts —
	// "no transition out of a terminal state, ever" — applies here too: a run that
	// reported SUCCEEDED and then moved would make every downstream consumer of
	// that event wrong retroactively.
	domain.RunSucceeded: {},
	domain.RunFailed:    {},
	domain.RunCancelled: {},
	domain.RunTimedOut:  {},
}

// Next computes the resulting state for a proposed transition.
//
// Terminal states are rejected here rather than being quietly ignored, so a
// caller that tries to move a finished run learns it was wrong instead of
// believing it succeeded.
func Next(from domain.RunState, trigger Trigger, outcome Outcome) (domain.RunState, error) {
	moves, known := table[from]
	if !known {
		return "", fmt.Errorf("unknown run state %q", from)
	}
	to, legal := moves[trigger]
	if !legal {
		return "", ErrIllegalTransition{From: from, Trigger: trigger}
	}

	// The one trigger whose target depends on data rather than on states alone.
	// §5.2: SUCCEEDED requires every node terminal AND no node in {FAILED,
	// UPSTREAM_FAILED, TIMED_OUT}.
	if trigger == TriggerAllNodesTerminal {
		switch {
		// A failure outranks a cancellation: if something genuinely broke, that is
		// the fact an operator needs, and reporting CANCELLED would hide it.
		case outcome.AnyFailed:
			return domain.RunFailed, nil
		case outcome.AnyCancelled:
			return domain.RunCancelled, nil
		}
	}
	return to, nil
}

// CanDispatch reports whether the scheduler may start NEW attempts for a run in
// this state.
//
// This single predicate is what makes pause mean what §5.2 says it means. PAUSING
// and PAUSED both return false — no new work — while the attempts already in
// flight are untouched by it, because nothing here cancels anything.
func CanDispatch(state domain.RunState) bool {
	return state == domain.RunPending || state == domain.RunRunning
}

// RetryFromFailed reports whether a run may be retried from its failed nodes.
//
// Only genuinely failed runs qualify. Retrying a CANCELLED run would override a
// deliberate human decision to stop, and retrying a SUCCEEDED run is a rerun, a
// different operation with different semantics (a new run, not a continuation).
func RetryFromFailed(state domain.RunState) error {
	switch state {
	case domain.RunFailed, domain.RunTimedOut:
		return nil
	default:
		return ErrIllegalTransition{From: state, Trigger: TriggerRetryFromFailed}
	}
}

// LegalTriggers lists what a state accepts, sorted. Used by the API to explain a
// rejection ("this run is PAUSED; you may resume or cancel") rather than emitting
// a bare 409.
func LegalTriggers(from domain.RunState) []Trigger {
	moves := table[from]
	out := make([]Trigger, 0, len(moves))
	for t := range moves {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// FromStatesFor returns every state that accepts this trigger.
//
// The persistence layer needs it for its compare-and-set: a transition is applied
// as `UPDATE ... WHERE state = ANY(from)`, so the CAS and this table can never
// disagree about what is legal.
func FromStatesFor(trigger Trigger) []string {
	var out []string
	for state, moves := range table {
		if _, ok := moves[trigger]; ok {
			out = append(out, string(state))
		}
	}
	sort.Strings(out)
	return out
}

// IsTerminal reports whether a run state admits no further transitions.
func IsTerminal(s domain.RunState) bool {
	return len(table[s]) == 0
}
