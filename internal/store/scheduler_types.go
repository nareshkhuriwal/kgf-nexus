// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// Candidate is a PENDING attempt the scheduler has row-locked and is trying to
// place.
type Candidate struct {
	ID              uuid.UUID
	RunID           uuid.UUID
	NodeID          string
	AttemptNumber   int32
	Queue           string
	Priority        int32
	ResourceRequest *nexusv1.ResourceVector
	ScheduledAt     time.Time
	// EffectivePriority is Priority plus the aging term. Carried out of SQL so
	// the head-of-line-blocking guard can compare a blocked large task against
	// the small tasks that would otherwise keep jumping it (§7.3).
	EffectivePriority float64
	// TenantID is the owning namespace's tenant (1:1 with namespace, 0013).
	// Carried so the claim loop can set a blocked namespace aside for the rest
	// of a cycle (ADR-0017).
	TenantID uuid.UUID
	// FairShare is the namespace's in-flight/weight at selection time, zero
	// when fair share is off. Informational: logged, never re-used for a
	// decision, because it is stale the moment the claim commits.
	FairShare float64
	// Spec is the node's task spec, the source of timeouts, retry policy,
	// node_selector, executor and concurrency settings. Read from
	// task_nodes.spec_json, which the scheduler reads but never writes.
	Spec *TaskSpecView
}

// TaskSpecView is the subset of the task spec the scheduler needs, decoded from
// task_nodes.spec_json.
//
// It is a hand-rolled struct rather than protojson into nexusv1.TaskSpec because
// the DAG engine owns that column's shape and the scheduler must tolerate fields
// it does not understand: an additive spec change by another service must not
// break dispatch (ADR-0011 rule 5).
type TaskSpecView struct {
	TaskRef      string            `json:"task_ref"`
	Version      int32             `json:"version"`
	NodeSelector map[string]string `json:"node_selector"`
	Executor     string            `json:"executor"`

	ScheduleToStartMs int64 `json:"schedule_to_start_ms"`
	StartToCloseMs    int64 `json:"start_to_close_ms"`
	ScheduleToCloseMs int64 `json:"schedule_to_close_ms"`
	HeartbeatMs       int64 `json:"heartbeat_ms"`

	MaxAttempts      int32  `json:"max_attempts"`
	IdempotencyMode  string `json:"idempotency"`
	ConcurrencyKey   string `json:"concurrency_key"`
	ConcurrencyLimit int32  `json:"concurrency_limit"`

	// RetryJSON is the retry policy, kept as raw JSON and decoded lazily so an
	// unparseable policy on one node cannot stop the scheduler from dispatching
	// every other node.
	RetryJSON json.RawMessage `json:"retry"`
}

// IdempotencyMode maps the spec's string form onto the wire enum.
//
// An unrecognised or absent value maps to UNSPECIFIED rather than to the
// documented platform default of `required`, because domain.ShouldRetry treats
// UNSPECIFIED as "no guarantee" and refuses to auto-retry indeterminate
// outcomes. Guessing "required" for a spec we failed to understand would
// authorize double execution of work that may not tolerate it.
func (s *TaskSpecView) IdempotencyModeProto() nexusv1.IdempotencyMode {
	if s == nil {
		return nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_UNSPECIFIED
	}
	switch s.IdempotencyMode {
	case "required", "IDEMPOTENCY_MODE_REQUIRED":
		return nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED
	case "best_effort", "IDEMPOTENCY_MODE_BEST_EFFORT":
		return nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_BEST_EFFORT
	case "none", "IDEMPOTENCY_MODE_NONE":
		return nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_NONE
	default:
		return nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_UNSPECIFIED
	}
}

// RetryPolicy decodes the node's retry policy, falling back to an empty policy
// (which domain.MaxAttempts and domain.Backoff resolve to platform defaults)
// when the field is absent or malformed.
func (s *TaskSpecView) RetryPolicy() *nexusv1.RetryPolicy {
	if s == nil || len(s.RetryJSON) == 0 {
		return &nexusv1.RetryPolicy{}
	}
	// The millisecond fields are json.Number, not int64, because this document has
	// TWO producers with different renderings of the same value: the gateway writes
	// a bare JSON number, and protojson (which the DAG engine marshals node specs
	// with) renders 64-bit integers as QUOTED STRINGS. Declaring them int64 made
	// Unmarshal fail on the whole struct for any protojson-written node, and the
	// error path below returns an EMPTY policy -- so retry_on, no_retry_on and the
	// backoff strategy were all silently discarded and every failure reported
	// "error_class_not_in_retry_on" regardless of what the author allowed.
	var raw struct {
		MaxAttempts int32        `json:"max_attempts"`
		Strategy    string       `json:"strategy"`
		InitialMs   *json.Number `json:"initial_ms"`
		MaxMs       *json.Number `json:"max_ms"`
		Multiplier  float64      `json:"multiplier"`
		Jitter      string       `json:"jitter"`
		RetryOn     []string     `json:"retry_on"`
		NoRetryOn   []string     `json:"no_retry_on"`
		DiffWorker  bool         `json:"retry_on_different_worker"`
	}
	if err := json.Unmarshal(s.RetryJSON, &raw); err != nil {
		return &nexusv1.RetryPolicy{}
	}
	initialMs, _ := numInt64(raw.InitialMs)
	maxMs, _ := numInt64(raw.MaxMs)
	p := &nexusv1.RetryPolicy{
		MaxAttempts:            raw.MaxAttempts,
		InitialMs:              initialMs,
		MaxMs:                  maxMs,
		Multiplier:             raw.Multiplier,
		Jitter:                 raw.Jitter,
		RetryOnDifferentWorker: raw.DiffWorker,
	}
	switch raw.Strategy {
	case "fixed", "BACKOFF_STRATEGY_FIXED":
		p.Strategy = nexusv1.BackoffStrategy_BACKOFF_STRATEGY_FIXED
	case "linear", "BACKOFF_STRATEGY_LINEAR":
		p.Strategy = nexusv1.BackoffStrategy_BACKOFF_STRATEGY_LINEAR
	case "exponential", "BACKOFF_STRATEGY_EXPONENTIAL":
		p.Strategy = nexusv1.BackoffStrategy_BACKOFF_STRATEGY_EXPONENTIAL
	}
	for _, c := range raw.RetryOn {
		if ec := errorClassFromString(c); ec != nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED {
			p.RetryOn = append(p.RetryOn, ec)
		}
	}
	for _, c := range raw.NoRetryOn {
		if ec := errorClassFromString(c); ec != nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED {
			p.NoRetryOn = append(p.NoRetryOn, ec)
		}
	}
	return p
}

// errorClassFromString accepts both the authoring spellings from §4.3 and the
// proto enum names, because the spec JSON may be produced by either the CLI
// (authoring form) or a protojson marshal (enum form).
func errorClassFromString(s string) nexusv1.ErrorClass {
	switch s {
	case "transient", "ERROR_CLASS_TRANSIENT":
		return nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT
	case "timeout", "ERROR_CLASS_TIMEOUT":
		return nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT
	case "worker_lost", "ERROR_CLASS_WORKER_LOST":
		return nexusv1.ErrorClass_ERROR_CLASS_WORKER_LOST
	case "validation", "validation_error", "ERROR_CLASS_VALIDATION":
		return nexusv1.ErrorClass_ERROR_CLASS_VALIDATION
	case "permission_denied", "ERROR_CLASS_PERMISSION_DENIED":
		return nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED
	case "application", "ERROR_CLASS_APPLICATION":
		return nexusv1.ErrorClass_ERROR_CLASS_APPLICATION
	case "oom", "ERROR_CLASS_OOM":
		return nexusv1.ErrorClass_ERROR_CLASS_OOM
	case "not_found", "ERROR_CLASS_NOT_FOUND":
		return nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND
	case "infrastructure", "ERROR_CLASS_INFRASTRUCTURE":
		return nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE
	default:
		return nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED
	}
}

// WorkerCandidate is a locked worker row considered for placement.
type WorkerCandidate struct {
	ID        uuid.UUID
	Hostname  string
	Capacity  *nexusv1.ResourceVector
	Committed *nexusv1.ResourceVector
	Score     float64
}

// DispatchResult describes a committed dispatch.
//
// The bus announcement of a dispatch is NOT published from this. The claim
// writes an outbox row (the AttemptDispatched run_event's, see outbox.go) inside
// its own transaction, and the event-service relay publishes it after commit
// (invariant 4). Publishing from here, after commit, would lose the message
// whenever the process died between COMMIT and publish; publishing inside the
// transaction would demand a distributed transaction across Postgres and NATS.
type DispatchResult struct {
	AttemptID  uuid.UUID
	RunID      uuid.UUID
	NodeID     string
	WorkerID   uuid.UUID
	LeaseToken string
	LeaseID    uuid.UUID
	ExpiresAt  time.Time
}

// ClaimOutcome says what one claim cycle did, so the caller can log and meter
// without re-deriving it from nil checks.
type ClaimOutcome int

const (
	// ClaimNothingPending means the queue is empty; the loop should back off
	// rather than spin on an empty queue burning a Postgres connection.
	ClaimNothingPending ClaimOutcome = iota
	// ClaimBlocked means an attempt was examined and left PENDING with a
	// pending_reason. Deliberately NOT a failure (§5.3 step 3).
	ClaimBlocked
	// ClaimDispatched means capacity, lease, state, event and timers committed.
	ClaimDispatched
	// ClaimRaceLost means another scheduler replica won the CAS. Expected and
	// harmless under N replicas; it is the guard working, not an error.
	ClaimRaceLost
)

func (o ClaimOutcome) String() string {
	switch o {
	case ClaimNothingPending:
		return "nothing_pending"
	case ClaimBlocked:
		return "blocked"
	case ClaimDispatched:
		return "dispatched"
	case ClaimRaceLost:
		return "race_lost"
	default:
		return "unknown"
	}
}

// ClaimReport is the full result of one claim cycle.
type ClaimReport struct {
	Outcome ClaimOutcome
	// Attempt is set whenever an attempt was examined, dispatched or not, so
	// the caller can log run_id/attempt_id per CONTRACT.md logging rules.
	Attempt  *Candidate
	Dispatch *DispatchResult
	// Reason is the structured pending_reason written for ClaimBlocked.
	Reason string
}

// LostAttempt is an attempt the reconciler moved to LOST, carrying what the
// retry decision needs.
type LostAttempt struct {
	AttemptID     uuid.UUID
	RunID         uuid.UUID
	NodeID        string
	AttemptNumber int32
	WorkerID      uuid.UUID
	Decision      domain.RetryDecision
	// RetryAt is when the new attempt becomes PENDING, from the backoff ladder.
	RetryAt time.Time
	// NewAttemptID is set when a retry row was created in the same transaction.
	NewAttemptID *uuid.UUID
}

// UnmarshalJSON decodes task_nodes.spec_json in EITHER dialect it can arrive in.
//
// This exists because the column has two producers writing two shapes, and the
// scheduler must read both or silently mis-schedule:
//
//   - the DAG engine writes it with protojson (internal/store/dagengine_writes.go),
//     which is lowerCamelCase, NESTS the timeout and runtime fields under
//     `timeouts`/`runtime`, and — the part that makes a naive struct tag fail
//     silently — encodes every int64 as a JSON STRING; and
//   - the flat snake_case form above, which the scheduler's own tests and
//     fixtures write.
//
// Reading only the flat form is not a cosmetic problem. Executor, NodeSelector
// and StartToCloseMs would all decode as zero against a protojson document, so
// dispatch would ignore executor compatibility and hard label constraints
// entirely (§5.3 step 2) and set no start_to_close timer — a task would be placed
// on a worker that cannot run it and would then never time out. json.Unmarshal
// reports no error for a field it simply does not find, which is why this went
// unnoticed: every spec looked like an empty spec.
//
// The flat form is applied first and the nested form second, so a protojson
// document wins where both could match. Neither producer emits both.
func (s *TaskSpecView) UnmarshalJSON(b []byte) error {
	// A type alias sheds the method set, so this is the plain struct decode and
	// not an infinite recursion back into here.
	type flat TaskSpecView
	var f flat
	if err := json.Unmarshal(b, &f); err != nil {
		// NOT fatal. The DAG engine marshals node specs with protojson
		// UseProtoNames, which keeps these snake_case keys but renders every 64-bit
		// integer as a QUOTED STRING, and a quoted "60000" fails to decode into
		// int64 -- taking the whole struct with it, including the string and map
		// fields that had parsed fine. Returning an error here made every such node
		// look like an empty spec: no executor constraint, no timeouts, no retry
		// policy. The snake_case pass below re-reads the same document with
		// json.Number so either rendering works.
		f = flat{}
	}
	*s = TaskSpecView(f)

	// protojson + UseProtoNames: snake_case keys, 64-bit integers as strings.
	// Applied before the camelCase pass because both are protojson dialects and
	// only one producer writes each; neither emits both.
	var sn struct {
		TaskRef      string            `json:"task_ref"`
		Version      *json.Number      `json:"version"`
		NodeSelector map[string]string `json:"node_selector"`
		Timeouts     *struct {
			ScheduleToStartMs *json.Number `json:"schedule_to_start_ms"`
			StartToCloseMs    *json.Number `json:"start_to_close_ms"`
			ScheduleToCloseMs *json.Number `json:"schedule_to_close_ms"`
			HeartbeatMs       *json.Number `json:"heartbeat_ms"`
		} `json:"timeouts"`
		Runtime *struct {
			Executor string `json:"executor"`
		} `json:"runtime"`
		Idempotency      string          `json:"idempotency"`
		ConcurrencyKey   string          `json:"concurrency_key"`
		ConcurrencyLimit *json.Number    `json:"concurrency_limit"`
		Retry            json.RawMessage `json:"retry"`
	}
	if err := json.Unmarshal(b, &sn); err == nil {
		if sn.TaskRef != "" {
			s.TaskRef = sn.TaskRef
		}
		if v, ok := numInt64(sn.Version); ok {
			s.Version = int32(v)
		}
		if len(sn.NodeSelector) > 0 {
			s.NodeSelector = sn.NodeSelector
		}
		if sn.Runtime != nil && sn.Runtime.Executor != "" {
			s.Executor = sn.Runtime.Executor
		}
		if t := sn.Timeouts; t != nil {
			if v, ok := numInt64(t.ScheduleToStartMs); ok {
				s.ScheduleToStartMs = v
			}
			if v, ok := numInt64(t.StartToCloseMs); ok {
				s.StartToCloseMs = v
			}
			if v, ok := numInt64(t.ScheduleToCloseMs); ok {
				s.ScheduleToCloseMs = v
			}
			if v, ok := numInt64(t.HeartbeatMs); ok {
				s.HeartbeatMs = v
			}
		}
		if sn.Idempotency != "" {
			s.IdempotencyMode = sn.Idempotency
		}
		if sn.ConcurrencyKey != "" {
			s.ConcurrencyKey = sn.ConcurrencyKey
		}
		if v, ok := numInt64(sn.ConcurrencyLimit); ok {
			s.ConcurrencyLimit = int32(v)
		}
		if len(sn.Retry) > 0 {
			s.RetryJSON = sn.Retry
		}
	}

	// protojson's shape. Every numeric field is `json.Number` so it parses
	// whether the producer wrote 60000 or "60000"; protojson writes the latter
	// for 64-bit integers and a plain int for 32-bit ones.
	var pj struct {
		TaskRef      string            `json:"taskRef"`
		Version      *json.Number      `json:"version"`
		NodeSelector map[string]string `json:"nodeSelector"`
		Timeouts     *struct {
			ScheduleToStartMs *json.Number `json:"scheduleToStartMs"`
			StartToCloseMs    *json.Number `json:"startToCloseMs"`
			ScheduleToCloseMs *json.Number `json:"scheduleToCloseMs"`
			HeartbeatMs       *json.Number `json:"heartbeatMs"`
		} `json:"timeouts"`
		Runtime *struct {
			Executor string `json:"executor"`
		} `json:"runtime"`
		Idempotency      string          `json:"idempotency"`
		ConcurrencyKey   string          `json:"concurrencyKey"`
		ConcurrencyLimit *json.Number    `json:"concurrencyLimit"`
		Retry            json.RawMessage `json:"retry"`
	}
	// A document that fails this second decode is not an error: it is simply the
	// flat dialect, already decoded above. Tolerating it is ADR-0011 rule 5 —
	// an additive change by the column's owner must not break dispatch.
	if err := json.Unmarshal(b, &pj); err != nil {
		return nil
	}

	if pj.TaskRef != "" {
		s.TaskRef = pj.TaskRef
	}
	if v, ok := numInt64(pj.Version); ok {
		s.Version = int32(v)
	}
	if len(pj.NodeSelector) > 0 {
		s.NodeSelector = pj.NodeSelector
	}
	if pj.Runtime != nil && pj.Runtime.Executor != "" {
		s.Executor = pj.Runtime.Executor
	}
	if t := pj.Timeouts; t != nil {
		if v, ok := numInt64(t.ScheduleToStartMs); ok {
			s.ScheduleToStartMs = v
		}
		if v, ok := numInt64(t.StartToCloseMs); ok {
			s.StartToCloseMs = v
		}
		if v, ok := numInt64(t.ScheduleToCloseMs); ok {
			s.ScheduleToCloseMs = v
		}
		if v, ok := numInt64(t.HeartbeatMs); ok {
			s.HeartbeatMs = v
		}
	}
	if pj.Idempotency != "" {
		s.IdempotencyMode = pj.Idempotency
	}
	if pj.ConcurrencyKey != "" {
		s.ConcurrencyKey = pj.ConcurrencyKey
	}
	if v, ok := numInt64(pj.ConcurrencyLimit); ok {
		s.ConcurrencyLimit = int32(v)
	}
	if len(pj.Retry) > 0 {
		s.RetryJSON = pj.Retry
	}
	return nil
}

// numInt64 reads a protojson integer, which may be a JSON number or a quoted
// string depending on its width. A value we cannot parse is reported as absent
// rather than as zero, so it leaves whatever the flat decode found in place.
func numInt64(n *json.Number) (int64, bool) {
	if n == nil || *n == "" {
		return 0, false
	}
	v, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return v, true
}
