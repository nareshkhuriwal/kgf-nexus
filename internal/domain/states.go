// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"

// AttemptState mirrors the Postgres `attempt_state` enum exactly.
//
// These are typed strings rather than integers because they are written
// directly into the DB enum column. A Go-side numbering would be a second
// source of truth that could drift from the schema during a migration; a
// string that IS the enum label cannot.
type AttemptState string

// Attempt states (CONTRACT.md canonical names; schema enum `attempt_state`).
const (
	AttemptPending    AttemptState = "PENDING"
	AttemptDispatched AttemptState = "DISPATCHED"
	AttemptRunning    AttemptState = "RUNNING"
	AttemptSucceeded  AttemptState = "SUCCEEDED"
	AttemptFailed     AttemptState = "FAILED"
	AttemptTimedOut   AttemptState = "TIMED_OUT"
	// AttemptLost means we do not know whether the work ran. It is emphatically
	// not FAILED: FAILED is knowledge, LOST is ignorance, and they get
	// different retry policies (§5.1, invariant 2).
	AttemptLost      AttemptState = "LOST"
	AttemptCancelled AttemptState = "CANCELLED"
)

// NodeState mirrors the Postgres `node_state` enum exactly.
type NodeState string

// Node states (CONTRACT.md canonical names; schema enum `node_state`).
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

// RunState mirrors the Postgres `run_state` enum exactly.
type RunState string

// Run states (CONTRACT.md canonical names; schema enum `run_state`).
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

// WorkerState mirrors the Postgres `worker_state` enum exactly.
type WorkerState string

// Worker states (schema enum `worker_state`; §5.5).
const (
	WorkerActive WorkerState = "ACTIVE"
	// WorkerDraining accepts no new work but finishes what it holds, which is
	// what makes a rolling deploy not cost hours of in-flight compute.
	WorkerDraining WorkerState = "DRAINING"
	// WorkerQuarantined circuit-breaks a poison host: one worker with a broken
	// driver can otherwise burn the retry budget of every task in a queue in
	// seconds (§5.5).
	WorkerQuarantined WorkerState = "QUARANTINED"
	WorkerDead        WorkerState = "DEAD"
)

// Event kinds written to the append-only run_events log. Consumers (UI, alert
// rules, the DAG engine's reconciler) match on these strings, so they are
// constants rather than inline literals at each emit site.
const (
	EventAttemptDispatched = "AttemptDispatched"
	EventAttemptLost       = "AttemptLost"
	EventAttemptScheduled  = "AttemptScheduled"
	EventAttemptBlocked    = "AttemptBlocked"
	EventRetryScheduled    = "RetryScheduled"
	EventRetryExhausted    = "RetryExhausted"
	// EventPossibleDuplicateExecution is the honest warning we owe an operator
	// when we auto-retry a LOST attempt under best_effort idempotency: the work
	// may physically run twice (§6.5).
	EventPossibleDuplicateExecution = "PossibleDuplicateExecution"
	EventIndeterminateOutcome       = "IndeterminateOutcome"
	EventWorkerDead                 = "WorkerDead"
	EventLeaseExpired               = "LeaseExpired"
)

// Timer kinds in the durable `timers` table. Every deadline in the system is a
// row, so a control-plane restart loses no deadlines (ADR-0002).
const (
	TimerScheduleToStart = "schedule_to_start"
	TimerStartToClose    = "start_to_close"
	TimerScheduleToClose = "schedule_to_close"
	TimerHeartbeat       = "heartbeat"
	TimerRetryBackoff    = "retry_backoff"
	TimerLeaseExpiry     = "lease_expiry"
)

// Actor prefixes for run_events.actor, so every transition is attributable.
const ActorScheduler = "scheduler"

// terminalAttempts is the closed set from §5.1. Stated once, as data, so
// IsTerminal and the retry path cannot disagree about it.
var terminalAttempts = map[AttemptState]bool{
	AttemptSucceeded: true,
	AttemptFailed:    true,
	AttemptTimedOut:  true,
	AttemptLost:      true,
	AttemptCancelled: true,
}

// IsTerminal reports whether an attempt state admits no further transition.
//
// Callers use this as the guard before any write to an attempt: §5.1 says
// there is no transition out of a terminal state ever, and invariant 9 says a
// retry is a NEW row. Mutating a terminal attempt would rewrite history and
// make the event log a lie.
func (s AttemptState) IsTerminal() bool { return terminalAttempts[s] }

// IsRetryable reports whether a terminal state is one that retry policy may
// act on at all. SUCCEEDED needs no retry and CANCELLED was a human decision
// we must not override.
func (s AttemptState) IsRetryable() bool {
	return s == AttemptFailed || s == AttemptTimedOut || s == AttemptLost
}

// IsActive reports whether an attempt is dispatched-or-running, i.e. holds a
// lease and a capacity reservation.
func (s AttemptState) IsActive() bool {
	return s == AttemptDispatched || s == AttemptRunning
}

var terminalNodes = map[NodeState]bool{
	NodeSucceeded:      true,
	NodeFailed:         true,
	NodeSkipped:        true,
	NodeUpstreamFailed: true,
	NodeCancelled:      true,
}

// IsTerminal reports whether a node state admits no further transition.
func (s NodeState) IsTerminal() bool { return terminalNodes[s] }

var terminalRuns = map[RunState]bool{
	RunSucceeded: true,
	RunFailed:    true,
	RunCancelled: true,
	RunTimedOut:  true,
}

// IsTerminal reports whether a run state admits no further transition.
//
// PAUSED is deliberately absent: a paused run is quiescent but resumable, and
// treating it as terminal is how a pause turns into silent data loss.
func (s RunState) IsTerminal() bool { return terminalRuns[s] }

// AcceptsNewDispatch reports whether the scheduler may dispatch new attempts
// for a run in this state. PAUSING/PAUSED stop NEW dispatch while letting
// in-flight attempts finish — pausing is not cancelling (§5.2).
func (s RunState) AcceptsNewDispatch() bool {
	return s == RunPending || s == RunRunning
}

// attemptStateProto maps DB labels to the wire enum. The two exist separately
// because the schema is frozen on strings and the API is frozen on protobuf
// numbers; this is the single place they are reconciled.
var attemptStateProto = map[AttemptState]nexusv1.AttemptState{
	AttemptPending:    nexusv1.AttemptState_ATTEMPT_STATE_PENDING,
	AttemptDispatched: nexusv1.AttemptState_ATTEMPT_STATE_DISPATCHED,
	AttemptRunning:    nexusv1.AttemptState_ATTEMPT_STATE_RUNNING,
	AttemptSucceeded:  nexusv1.AttemptState_ATTEMPT_STATE_SUCCEEDED,
	AttemptFailed:     nexusv1.AttemptState_ATTEMPT_STATE_FAILED,
	AttemptTimedOut:   nexusv1.AttemptState_ATTEMPT_STATE_TIMED_OUT,
	AttemptLost:       nexusv1.AttemptState_ATTEMPT_STATE_LOST,
	AttemptCancelled:  nexusv1.AttemptState_ATTEMPT_STATE_CANCELLED,
}

// Proto converts a DB attempt state to its wire enum value.
func (s AttemptState) Proto() nexusv1.AttemptState {
	if p, ok := attemptStateProto[s]; ok {
		return p
	}
	return nexusv1.AttemptState_ATTEMPT_STATE_UNSPECIFIED
}

// AttemptStateFromProto converts a wire enum value to its DB label. The empty
// string signals UNSPECIFIED, which must never be written to the enum column.
func AttemptStateFromProto(p nexusv1.AttemptState) AttemptState {
	for s, q := range attemptStateProto {
		if q == p {
			return s
		}
	}
	return ""
}

// AttemptStateForOutcome maps a worker-reported outcome to the state we record.
//
// INDETERMINATE maps to LOST rather than FAILED, which is the whole point of
// the WAL replay path (§6.3): the agent knows it started the task and cannot
// know if it finished, so recording FAILED would assert knowledge we do not
// have and would let §6.5 auto-retry non-idempotent work.
func AttemptStateForOutcome(o nexusv1.AttemptOutcome) AttemptState {
	switch o {
	case nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED:
		return AttemptSucceeded
	case nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED:
		return AttemptFailed
	case nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_TIMED_OUT:
		return AttemptTimedOut
	case nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_INDETERMINATE:
		return AttemptLost
	default:
		return ""
	}
}
