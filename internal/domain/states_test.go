// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// TestStateLabelsMatchTheFrozenContract pins the exact strings, because these
// values are written straight into Postgres enum columns. A rename that only
// touches Go compiles fine and then fails at runtime on every insert.
func TestStateLabelsMatchTheFrozenContract(t *testing.T) {
	attempts := map[AttemptState]string{
		AttemptPending: "PENDING", AttemptDispatched: "DISPATCHED",
		AttemptRunning: "RUNNING", AttemptSucceeded: "SUCCEEDED",
		AttemptFailed: "FAILED", AttemptTimedOut: "TIMED_OUT",
		AttemptLost: "LOST", AttemptCancelled: "CANCELLED",
	}
	for got, want := range attempts {
		if string(got) != want {
			t.Errorf("attempt state %q != contract %q", got, want)
		}
	}
	if len(attempts) != 8 {
		t.Errorf("contract lists 8 attempt states, have %d", len(attempts))
	}

	nodes := map[NodeState]string{
		NodePending: "PENDING", NodeReady: "READY", NodeRunning: "RUNNING",
		NodeSucceeded: "SUCCEEDED", NodeFailed: "FAILED", NodeSkipped: "SKIPPED",
		NodeUpstreamFailed: "UPSTREAM_FAILED", NodeCancelled: "CANCELLED",
	}
	for got, want := range nodes {
		if string(got) != want {
			t.Errorf("node state %q != contract %q", got, want)
		}
	}
	if len(nodes) != 8 {
		t.Errorf("contract lists 8 node states, have %d", len(nodes))
	}

	runs := map[RunState]string{
		RunPending: "PENDING", RunRunning: "RUNNING", RunPausing: "PAUSING",
		RunPaused: "PAUSED", RunSucceeded: "SUCCEEDED", RunFailed: "FAILED",
		RunCancelling: "CANCELLING", RunCancelled: "CANCELLED", RunTimedOut: "TIMED_OUT",
	}
	for got, want := range runs {
		if string(got) != want {
			t.Errorf("run state %q != contract %q", got, want)
		}
	}
	if len(runs) != 9 {
		t.Errorf("contract lists 9 run states, have %d", len(runs))
	}

	workers := map[WorkerState]string{
		WorkerActive: "ACTIVE", WorkerDraining: "DRAINING",
		WorkerQuarantined: "QUARANTINED", WorkerDead: "DEAD",
	}
	for got, want := range workers {
		if string(got) != want {
			t.Errorf("worker state %q != schema %q", got, want)
		}
	}
}

func TestAttemptStatePredicates(t *testing.T) {
	tests := []struct {
		state     AttemptState
		terminal  bool
		retryable bool
		active    bool
	}{
		{AttemptPending, false, false, false},
		{AttemptDispatched, false, false, true},
		{AttemptRunning, false, false, true},
		{AttemptSucceeded, true, false, false},
		{AttemptFailed, true, true, false},
		{AttemptTimedOut, true, true, false},
		{AttemptLost, true, true, false},
		// CANCELLED is terminal but never retried: a human decided to stop
		// this work and the scheduler must not override that.
		{AttemptCancelled, true, false, false},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			if got := tc.state.IsTerminal(); got != tc.terminal {
				t.Errorf("IsTerminal = %v, want %v", got, tc.terminal)
			}
			if got := tc.state.IsRetryable(); got != tc.retryable {
				t.Errorf("IsRetryable = %v, want %v", got, tc.retryable)
			}
			if got := tc.state.IsActive(); got != tc.active {
				t.Errorf("IsActive = %v, want %v", got, tc.active)
			}
			if tc.retryable && !tc.terminal {
				t.Error("a retryable state must also be terminal")
			}
		})
	}
	if AttemptState("BOGUS").IsTerminal() {
		t.Error("an unknown state must not be reported terminal")
	}
}

func TestNodeAndRunTerminality(t *testing.T) {
	nodeTests := map[NodeState]bool{
		NodePending: false, NodeReady: false, NodeRunning: false,
		NodeSucceeded: true, NodeFailed: true, NodeSkipped: true,
		NodeUpstreamFailed: true, NodeCancelled: true,
	}
	for s, want := range nodeTests {
		if got := s.IsTerminal(); got != want {
			t.Errorf("NodeState(%s).IsTerminal = %v, want %v", s, got, want)
		}
	}

	runTests := map[RunState]bool{
		RunPending: false, RunRunning: false, RunPausing: false,
		// PAUSED is quiescent but resumable. Treating it as terminal is how a
		// pause becomes silent data loss.
		RunPaused: false, RunCancelling: false,
		RunSucceeded: true, RunFailed: true, RunCancelled: true, RunTimedOut: true,
	}
	for s, want := range runTests {
		if got := s.IsTerminal(); got != want {
			t.Errorf("RunState(%s).IsTerminal = %v, want %v", s, got, want)
		}
	}
}

func TestRunStateAcceptsNewDispatch(t *testing.T) {
	// PAUSING/PAUSED must block NEW dispatch while in-flight attempts finish:
	// pausing is not cancelling (§5.2).
	tests := map[RunState]bool{
		RunPending: true, RunRunning: true,
		RunPausing: false, RunPaused: false, RunCancelling: false,
		RunSucceeded: false, RunFailed: false, RunCancelled: false, RunTimedOut: false,
	}
	for s, want := range tests {
		if got := s.AcceptsNewDispatch(); got != want {
			t.Errorf("RunState(%s).AcceptsNewDispatch = %v, want %v", s, got, want)
		}
	}
}

func TestAttemptStateProtoRoundTrip(t *testing.T) {
	for _, s := range []AttemptState{
		AttemptPending, AttemptDispatched, AttemptRunning, AttemptSucceeded,
		AttemptFailed, AttemptTimedOut, AttemptLost, AttemptCancelled,
	} {
		p := s.Proto()
		if p == nexusv1.AttemptState_ATTEMPT_STATE_UNSPECIFIED {
			t.Fatalf("%s has no wire mapping", s)
		}
		if back := AttemptStateFromProto(p); back != s {
			t.Fatalf("round trip %s -> %v -> %s", s, p, back)
		}
	}
	if got := AttemptState("BOGUS").Proto(); got != nexusv1.AttemptState_ATTEMPT_STATE_UNSPECIFIED {
		t.Fatalf("unknown state mapped to %v, want UNSPECIFIED", got)
	}
	// The empty string signals "do not write this to the enum column".
	if got := AttemptStateFromProto(nexusv1.AttemptState_ATTEMPT_STATE_UNSPECIFIED); got != "" {
		t.Fatalf("UNSPECIFIED mapped to %q, want the empty string", got)
	}
}

func TestAttemptStateForOutcome(t *testing.T) {
	tests := []struct {
		outcome nexusv1.AttemptOutcome
		want    AttemptState
	}{
		{nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED, AttemptSucceeded},
		{nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED, AttemptFailed},
		{nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_TIMED_OUT, AttemptTimedOut},
		// INDETERMINATE is LOST, never FAILED: recording FAILED would assert
		// knowledge we do not have and would let §6.5 auto-retry
		// non-idempotent work.
		{nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_INDETERMINATE, AttemptLost},
		{nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_UNSPECIFIED, ""},
	}
	for _, tc := range tests {
		if got := AttemptStateForOutcome(tc.outcome); got != tc.want {
			t.Errorf("AttemptStateForOutcome(%v) = %q, want %q", tc.outcome, got, tc.want)
		}
	}
}
