// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package runs implements the WorkflowRun lifecycle: creation with idempotency
// dedup, the §5.2 state machine, and run-level policy enforcement (run timeout,
// max_active_runs, fail_fast).
package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/workflow-manager/internal/statemachine"
	"github.com/khuriwalgroup/kgf-nexus/services/workflow-manager/timers"
)

// TimerRunDeadline is an alias for the reconciler's kind constant. Aliased rather
// than redeclared so the arming side and the dispatching side can never drift onto
// two different strings, which would leave a timer armed that nothing handles.
const TimerRunDeadline = timers.KindRunDeadline

// EventPublisher is the event-service's API as this package needs it.
//
// run_events is owned by event-service (ADR-0011), so every transition this
// package makes is REPORTED rather than written directly. Narrow interface so the
// ownership boundary is visible in the type system.
type EventPublisher interface {
	Publish(ctx context.Context, runID string, nodeID, attemptID *string, kind string, payload any, actor string) error
}

// DAGEngine is the dag-engine's API as this package needs it.
type DAGEngine interface {
	// MaterializeRun compiles the pinned spec into task_nodes and task_edges. It
	// is the dag-engine's table, so we ask rather than insert.
	MaterializeRun(ctx context.Context, runID, specID string, specJSON []byte) error
	// ResetFailedNodes prepares a retry-from-failed by returning failed nodes and
	// everything downstream of them to PENDING.
	ResetFailedNodes(ctx context.Context, runID string) (resetNodes []string, err error)
	// ReconcileRun recomputes a run's frontier. Only the engine owns the graph, so
	// nothing else in the system can work out which node became ready.
	ReconcileRun(ctx context.Context, runID, nodeID string) (frontier []string, runState string, terminal bool, err error)
}

// Scheduler is the scheduler's API as this package needs it.
type Scheduler interface {
	// CancelRunAttempts asks the scheduler to stop every in-flight attempt of a
	// run. Used ONLY by cancel; pause must never call it.
	CancelRunAttempts(ctx context.Context, runID string) error
}

// Service owns run lifecycle operations.
type Service struct {
	db     store.GWDB
	events EventPublisher
	dag    DAGEngine
	sched  Scheduler
	log    *slog.Logger
}

// New builds the run service.
func New(db store.GWDB, events EventPublisher, dagEngine DAGEngine, sched Scheduler, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: db, events: events, dag: dagEngine, sched: sched, log: log}
}

// CreateRequest describes a run to create.
type CreateRequest struct {
	Namespace string
	// Exactly one of SpecID or SpecName identifies the workflow. SpecName resolves
	// to the LATEST version once and then pins it, so a spec submitted a
	// millisecond later cannot change what this run executes (ADR-0004).
	SpecID   string
	SpecName string

	IdempotencyKey string
	TriggerKind    string
	TriggerDetail  map[string]any
	Params         map[string]any
	LogicalDate    *time.Time
	Priority       int32
}

// CreateResult reports whether a run was created or deduplicated.
type CreateResult struct {
	Run store.RunRow
	// Created is false when an existing run was returned for the same idempotency
	// key. Callers surface this as 200-vs-201 so a client can tell a genuine
	// creation from a safe replay.
	Created bool
}

// Create makes a run, or returns the existing one for a repeated idempotency key.
//
// The spec version is PINNED at creation and never re-resolved. A run's definition
// is immutable for its lifetime: editing a workflow must never mutate a run in
// flight, which is the property that makes a historical run reproducible.
func (s *Service) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	var (
		spec store.SpecRow
		err  error
	)
	switch {
	case req.SpecID != "":
		spec, err = store.WFMGetSpecByID(ctx, s.db, req.SpecID)
	case req.SpecName != "":
		spec, err = store.WFMGetLatestSpec(ctx, s.db, req.Namespace, req.SpecName)
	default:
		return CreateResult{}, fmt.Errorf("either spec_id or spec_name is required")
	}
	if err != nil {
		return CreateResult{}, fmt.Errorf("resolve spec: %w", err)
	}
	if spec.Namespace != req.Namespace {
		// Namespaces are the tenancy boundary; a run must never execute a spec
		// belonging to another tenant even if its UUID is known.
		return CreateResult{}, fmt.Errorf("spec %s belongs to namespace %s, not %s", spec.ID, spec.Namespace, req.Namespace)
	}

	policy, err := parsePolicy(spec.SpecJSON)
	if err != nil {
		return CreateResult{}, fmt.Errorf("read run policy from spec %s: %w", spec.ID, err)
	}

	if missing := missingRequiredParams(spec.SpecJSON, req.Params); len(missing) > 0 {
		// Rejected BEFORE the run row exists. Creating it and letting the task
		// fail would burn the node's retry budget on a problem no retry can fix,
		// and report a bad request as an execution failure.
		return CreateResult{}, fmt.Errorf("%w: %s", ErrMissingParams, strings.Join(missing, ", "))
	}

	priority := req.Priority
	if priority == 0 {
		priority = policy.Priority
	}
	if priority == 0 {
		priority = 100
	}

	triggerDetail, err := json.Marshal(orEmptyMap(req.TriggerDetail))
	if err != nil {
		return CreateResult{}, fmt.Errorf("encode trigger detail: %w", err)
	}
	params, err := json.Marshal(orEmptyMap(req.Params))
	if err != nil {
		return CreateResult{}, fmt.Errorf("encode params: %w", err)
	}

	triggerKind := req.TriggerKind
	if triggerKind == "" {
		triggerKind = "api"
	}

	run, created, err := store.WFMCreateRun(ctx, s.db, store.CreateRunParams{
		SpecID:         spec.ID,
		Namespace:      req.Namespace,
		Priority:       priority,
		IdempotencyKey: req.IdempotencyKey,
		TriggerKind:    triggerKind,
		TriggerDetail:  triggerDetail,
		Params:         params,
		LogicalDate:    req.LogicalDate,
		RunTimeoutMS:   policy.RunTimeoutMS,
		MaxActiveRuns:  policy.MaxActiveRuns,
	})
	if err != nil {
		return CreateResult{}, err
	}
	if !created {
		// A deduplicated creation must do NOTHING else: materializing the DAG or
		// arming timers a second time would double every downstream effect of a
		// retry that was supposed to be free.
		s.log.InfoContext(ctx, "run creation deduplicated by idempotency key",
			"run_id", run.ID, "idempotency_key", req.IdempotencyKey)
		return CreateResult{Run: run, Created: false}, nil
	}

	log := s.log.With("run_id", run.ID, "namespace", run.Namespace)

	if err := s.dag.MaterializeRun(ctx, run.ID, spec.ID, spec.SpecJSON); err != nil {
		return CreateResult{}, fmt.Errorf("materialize dag for run %s: %w", run.ID, err)
	}

	// The run deadline is armed as a DURABLE TIMER, not an in-process timer. A
	// control-plane restart must not silently extend every in-flight run's SLA to
	// infinity (§3.2).
	if run.DeadlineAt != nil {
		if _, err := store.WFMArmTimerAt(ctx, s.db, TimerRunDeadline, run.ID, *run.DeadlineAt, nil); err != nil {
			return CreateResult{}, fmt.Errorf("arm run deadline for %s: %w", run.ID, err)
		}
		log.InfoContext(ctx, "run deadline armed", "deadline_at", run.DeadlineAt)
	}

	s.emit(ctx, run.ID, "RunCreated", map[string]any{
		"spec_id":      spec.ID,
		"spec_name":    spec.Name,
		"spec_version": spec.Version,
		"trigger_kind": triggerKind,
	}, actorFor(ctx))

	log.InfoContext(ctx, "run created", "spec_name", spec.Name, "spec_version", spec.Version)
	return CreateResult{Run: run, Created: true}, nil
}

// Start moves a PENDING run to RUNNING once the DAG engine has its frontier.
func (s *Service) Start(ctx context.Context, runID string) (store.RunRow, error) {
	return s.transition(ctx, runID, statemachine.TriggerStart, statemachine.Outcome{})
}

// Pause holds a run WITHOUT killing in-flight work.
//
// This is the distinction §5.2 insists on and the reason pause is useful at all:
// the run stops dispatching NEW attempts, while attempts already running go to
// completion. Notice what this method does NOT do — it never calls the scheduler's
// cancel. Conflating pause with cancel would throw away hours of compute exactly
// when an operator is trying to preserve it while they investigate.
func (s *Service) Pause(ctx context.Context, runID string) (store.RunRow, error) {
	run, err := s.transition(ctx, runID, statemachine.TriggerPauseRequest, statemachine.Outcome{})
	if err != nil {
		return store.RunRow{}, err
	}
	s.log.InfoContext(ctx, "run pausing: no new attempts will dispatch; in-flight attempts run to completion",
		"run_id", runID, "state", run.State)
	return run, nil
}

// Resume releases a hold, reopening the dispatch gate and recomputing the frontier.
//
// The reconcile is not optional bookkeeping — without it a resumed run is wedged.
// Reopening the gate only makes READY nodes dispatchable again; it does not make
// any node READY. An attempt that completed DURING the pause window reported into
// a run that was no longer RUNNING, so the frontier advance that normally follows
// a terminal attempt never happened, and its successor stayed PENDING. The run
// then sat at RUNNING with a SUCCEEDED node and nothing in flight, forever,
// because only the engine computes readiness and nothing else would ask it to.
func (s *Service) Resume(ctx context.Context, runID string) (store.RunRow, error) {
	run, err := s.transition(ctx, runID, statemachine.TriggerResume, statemachine.Outcome{})
	if err != nil {
		return store.RunRow{}, err
	}

	// Logged and swallowed rather than returned: the resume itself is committed and
	// durable, so failing the operator's call would misreport a state change that
	// already happened. A missed reconcile costs latency, not correctness — the next
	// terminal attempt triggers one anyway.
	if _, _, _, rerr := s.dag.ReconcileRun(ctx, runID, ""); rerr != nil {
		s.log.ErrorContext(ctx, "run resumed but the frontier could not be recomputed; nodes that became ready during the pause may not dispatch until the next attempt reports",
			"run_id", runID, "error", rerr)
	}
	return run, nil
}

// Cancel stops a run AND its in-flight work.
//
// Unlike pause, this does ask the scheduler to stop the attempts. The run enters
// CANCELLING and only reaches CANCELLED once every in-flight attempt has been
// acked, because a run reported CANCELLED while a worker is still executing one of
// its tasks would be a lie an operator acts on.
func (s *Service) Cancel(ctx context.Context, runID string) (store.RunRow, error) {
	if _, err := s.transition(ctx, runID, statemachine.TriggerCancelRequest, statemachine.Outcome{}); err != nil {
		return store.RunRow{}, err
	}

	if err := s.sched.CancelRunAttempts(ctx, runID); err != nil {
		// The run is already CANCELLING and that state is durable, so a failure
		// here delays cancellation rather than losing it: the reconciler retries.
		s.log.ErrorContext(ctx, "failed to request attempt cancellation; reconciler will retry",
			"run_id", runID, "error", err)
	}

	// Deadline timers for a cancelled run are moot, and leaving them armed would
	// later try to transition a terminal run and generate confusing rejections on
	// a path operators read during incidents.
	if _, err := store.WFMCancelTimers(ctx, s.db, runID, TimerRunDeadline); err != nil {
		s.log.WarnContext(ctx, "failed to cancel run deadline timer", "run_id", runID, "error", err)
	}

	// Settle the nodes cancel withheld. Cancelling the ATTEMPTS above is not the
	// same as settling the NODES: a cancelled run's PENDING and READY nodes are
	// unreachable from both directions -- the engine will not advance them (the
	// run's state suppresses dispatch) and no attempt will ever be created for
	// them -- so without this the run reports CANCELLED while its graph still
	// shows eight nodes waiting to run, and any consumer counting node states
	// sees a run that never finished. Only the engine may write task_nodes
	// (ADR-0011), so this is a call, not an update.
	//
	// Swallowed like Resume's: the cancel is committed and durable, and failing
	// the operator's call would misreport a state change that already happened.
	if _, _, _, rerr := s.dag.ReconcileRun(ctx, runID, ""); rerr != nil {
		s.log.ErrorContext(ctx, "run cancelled but its withheld nodes could not be settled; the graph will show PENDING nodes on a terminal run",
			"run_id", runID, "error", rerr)
	}

	return s.MaybeFinalizeCancel(ctx, runID)
}

// MaybeFinalizeCancel advances CANCELLING -> CANCELLED once nothing is in flight.
func (s *Service) MaybeFinalizeCancel(ctx context.Context, runID string) (store.RunRow, error) {
	counts, err := store.WFMCountRunAttemptsByState(ctx, s.db, runID)
	if err != nil {
		return store.RunRow{}, err
	}
	if inFlight(counts) > 0 {
		return store.WFMGetRun(ctx, s.db, runID)
	}
	run, err := s.transition(ctx, runID, statemachine.TriggerCancelAcked, statemachine.Outcome{})
	if errors.Is(err, store.ErrRunStateConflict) {
		// Another replica finalized first. Not an error: both wanted the same
		// outcome and it has been reached.
		return store.WFMGetRun(ctx, s.db, runID)
	}
	return run, err
}

// MaybeFinalizePause advances PAUSING -> PAUSED once in-flight attempts have
// drained. The reconciler calls this; nothing here forces an attempt to stop.
func (s *Service) MaybeFinalizePause(ctx context.Context, runID string) (store.RunRow, error) {
	counts, err := store.WFMCountRunAttemptsByState(ctx, s.db, runID)
	if err != nil {
		return store.RunRow{}, err
	}
	if inFlight(counts) > 0 {
		s.log.DebugContext(ctx, "run still draining before pause",
			"run_id", runID, "in_flight", inFlight(counts))
		return store.WFMGetRun(ctx, s.db, runID)
	}
	run, err := s.transition(ctx, runID, statemachine.TriggerDrained, statemachine.Outcome{})
	if errors.Is(err, store.ErrRunStateConflict) {
		return store.WFMGetRun(ctx, s.db, runID)
	}
	return run, err
}

// ReconcileStalled asks the engine to recompute a run that stopped progressing.
//
// Called only by the stalled-run sweep. It is a thin pass-through rather than a
// transition of its own on purpose: the sweep must not DECIDE anything about the
// run. Only the engine can say what a graph of terminal nodes means, and having
// a second component guess would give two answers to a question that has to have
// one -- exactly the split-brain OnNodesTerminal exists to prevent.
//
// Safe to call repeatedly and on a run that turns out to be healthy: the engine
// recomputes from node state and publishes only when its conclusion differs from
// what the run already says.
func (s *Service) ReconcileStalled(ctx context.Context, runID string) error {
	_, runState, terminal, err := s.dag.ReconcileRun(ctx, runID, "")
	if err != nil {
		return err
	}
	s.log.InfoContext(ctx, "reconciled a stalled run",
		"run_id", runID, "run_state", runState, "terminal", terminal)
	return nil
}

// RetryFromFailed restarts a terminally failed run from its failed nodes.
//
// Nodes that already SUCCEEDED are left alone, so a 50-node pipeline that failed
// at node 49 does not redo 48 nodes of work. The run returns to RUNNING rather
// than PENDING because its DAG is already materialized.
func (s *Service) RetryFromFailed(ctx context.Context, runID string) (store.RunRow, error) {
	state, _, _, err := store.WFMGetRunForUpdate(ctx, s.db, runID)
	if err != nil {
		return store.RunRow{}, err
	}
	if err := statemachine.RetryFromFailed(domain.RunState(state)); err != nil {
		return store.RunRow{}, fmt.Errorf("%w: %s", store.ErrRunStateConflict, err)
	}

	reset, err := s.dag.ResetFailedNodes(ctx, runID)
	if err != nil {
		return store.RunRow{}, fmt.Errorf("reset failed nodes for run %s: %w", runID, err)
	}

	// A retried run gets a FRESH deadline window: the original deadline has
	// already passed for a TIMED_OUT run, so reusing it would time the retry out
	// the instant it started.
	if _, err := store.WFMCancelTimers(ctx, s.db, runID, TimerRunDeadline); err != nil {
		s.log.WarnContext(ctx, "failed to clear old run deadline on retry", "run_id", runID, "error", err)
	}

	run, err := store.WFMTransitionRun(ctx, s.db, runID,
		[]string{string(domain.RunFailed), string(domain.RunTimedOut)},
		string(domain.RunRunning))
	if err != nil {
		return store.RunRow{}, err
	}

	s.emit(ctx, runID, "RunRetriedFromFailed", map[string]any{"reset_nodes": reset}, actorFor(ctx))
	s.log.InfoContext(ctx, "run retried from failed nodes", "run_id", runID, "reset_nodes", len(reset))
	return run, nil
}

// OnRunDeadline handles the run_deadline timer.
func (s *Service) OnRunDeadline(ctx context.Context, runID string) error {
	run, err := s.transition(ctx, runID, statemachine.TriggerRunDeadline, statemachine.Outcome{})
	if errors.Is(err, store.ErrRunStateConflict) {
		// The run finished on its own between the timer firing and this handler.
		// A no-op, and the common case for any deadline that races completion.
		s.log.DebugContext(ctx, "run deadline fired on an already-terminal run", "run_id", runID)
		return nil
	}
	if err != nil {
		return err
	}

	// Timing out means the work must actually stop; unlike pause, this is a kill.
	if err := s.sched.CancelRunAttempts(ctx, runID); err != nil {
		s.log.ErrorContext(ctx, "failed to stop attempts of a timed-out run", "run_id", runID, "error", err)
	}
	s.log.WarnContext(ctx, "run exceeded run_timeout_ms", "run_id", runID, "state", run.State)
	return nil
}

// OnNodesTerminal is the DAG engine reporting the graph is finished.
func (s *Service) OnNodesTerminal(ctx context.Context, runID string) (store.RunRow, error) {
	counts, err := store.WFMCountRunNodesByState(ctx, s.db, runID)
	if err != nil {
		return store.RunRow{}, err
	}
	// §5.2: SUCCEEDED requires every node terminal AND none in {FAILED,
	// UPSTREAM_FAILED, TIMED_OUT}. A CANCELLED node is not a fault, so it does not
	// make the run FAILED — but it does mean the run did not achieve its goal, so it
	// does not make it SUCCEEDED either; the machine reports CANCELLED
	// (CONTRACT-FINDINGS.md F3).
	outcome := statemachine.Outcome{
		AnyFailed: counts[string(domain.NodeFailed)] > 0 ||
			counts[string(domain.NodeUpstreamFailed)] > 0,
		AnyCancelled: counts[string(domain.NodeCancelled)] > 0,
	}
	run, err := s.transition(ctx, runID, statemachine.TriggerAllNodesTerminal, outcome)
	if err != nil {
		return store.RunRow{}, err
	}
	if _, err := store.WFMCancelTimers(ctx, s.db, runID, TimerRunDeadline); err != nil {
		s.log.WarnContext(ctx, "failed to cancel deadline on a finished run", "run_id", runID, "error", err)
	}
	return run, nil
}

// OnNodeFailedFailFast stops dispatching after a terminal failure under fail_fast.
//
// fail_fast stops NEW work; it does not kill in-flight attempts, which is the same
// distinction as pause. An operator who wants the running work stopped issues a
// cancel.
func (s *Service) OnNodeFailedFailFast(ctx context.Context, runID, nodeID string) (store.RunRow, error) {
	run, err := s.transition(ctx, runID, statemachine.TriggerNodeFailedFailFast, statemachine.Outcome{})
	if err != nil {
		return store.RunRow{}, err
	}
	s.emit(ctx, runID, "RunFailedFast", map[string]any{"node_id": nodeID}, domain.ActorScheduler)
	return run, nil
}

// transition applies a state machine move to the database.
//
// The legality decision comes from the PURE machine and the database enforces it
// with a compare-and-set on the same set of source states. Two layers agreeing is
// the point: the machine is exhaustively testable without a database, and the CAS
// makes concurrent transitions from several replicas safe.
func (s *Service) transition(ctx context.Context, runID string, trigger statemachine.Trigger, outcome statemachine.Outcome) (store.RunRow, error) {
	state, _, _, err := store.WFMGetRunForUpdate(ctx, s.db, runID)
	if err != nil {
		return store.RunRow{}, err
	}
	from := domain.RunState(state)

	to, err := statemachine.Next(from, trigger, outcome)
	if err != nil {
		return store.RunRow{}, fmt.Errorf("%w: %v (legal from %s: %v)",
			store.ErrRunStateConflict, err, from, statemachine.LegalTriggers(from))
	}

	run, err := store.WFMTransitionRun(ctx, s.db, runID, statemachine.FromStatesFor(trigger), string(to))
	if err != nil {
		return store.RunRow{}, err
	}

	s.emit(ctx, runID, "RunStateChanged", map[string]any{
		"from":    string(from),
		"to":      string(to),
		"trigger": string(trigger),
	}, actorFor(ctx))

	s.log.InfoContext(ctx, "run state changed",
		"run_id", runID, "from", from, "to", to, "trigger", trigger)
	return run, nil
}

// emit reports an event through the event-service.
//
// Failures are logged but never fail the caller's operation: the state change is
// already durably committed, and refusing the operator's cancel because the audit
// log was briefly unavailable would be the wrong trade. The events table is
// append-only and authoritative for history, but the current-state tables are what
// the platform acts on.
func (s *Service) emit(ctx context.Context, runID, kind string, payload any, actor string) {
	if s.events == nil {
		return
	}
	if err := s.events.Publish(ctx, runID, nil, nil, kind, payload, actor); err != nil {
		s.log.ErrorContext(ctx, "failed to publish run event", "run_id", runID, "kind", kind, "error", err)
	}
}

// inFlight counts attempts that have not reached a terminal state.
func inFlight(counts map[string]int) int {
	return counts[string(domain.AttemptPending)] +
		counts[string(domain.AttemptDispatched)] +
		counts[string(domain.AttemptRunning)]
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// runPolicy is the run-level policy carried by a stored spec.
type runPolicy struct {
	RunTimeoutMS  int64
	MaxActiveRuns int32
	FailFast      bool
	Priority      int32
}

// parsePolicy reads run-level policy out of the stored spec JSON.
//
// Read from the SPEC rather than from a denormalized column, because the run pins
// its spec version and the policy must be the one that spec declared — not
// whatever the workflow's newest version says today.
func parsePolicy(specJSON []byte) (runPolicy, error) {
	var doc struct {
		RunTimeoutMS  int64 `json:"run_timeout_ms"`
		MaxActiveRuns int32 `json:"max_active_runs"`
		FailFast      bool  `json:"fail_fast"`
		Priority      int32 `json:"priority"`
	}
	if len(specJSON) == 0 {
		return runPolicy{}, nil
	}
	if err := json.Unmarshal(specJSON, &doc); err != nil {
		return runPolicy{}, fmt.Errorf("decode spec json: %w", err)
	}
	return runPolicy{
		RunTimeoutMS:  doc.RunTimeoutMS,
		MaxActiveRuns: doc.MaxActiveRuns,
		FailFast:      doc.FailFast,
		Priority:      doc.Priority,
	}, nil
}

// ErrMissingParams means the request omitted a parameter the spec declares
// required. A distinct error so the API layer can answer 422 -- the request was
// well-formed, the DOCUMENT is not -- rather than folding it into a 500.
var ErrMissingParams = errors.New("required parameters are missing")

// missingRequiredParams returns the names of params the spec declares required
// and the request did not supply, in a stable order.
//
// WHY THIS IS CHECKED AT ALL. A spec declares `{"source": {"type": "string",
// "required": true}}` and nothing enforced it, so a run omitting `source` was
// accepted, dispatched, and failed inside the task -- burning a retry budget and
// reporting the problem as an execution failure rather than a bad request. The
// spec already says what is required; the only thing missing was reading it.
//
// A param with a default_json is NOT missing: the default is what makes it
// optional in practice, and treating a defaulted param as required would reject
// every run that relies on the spec's own fallback.
//
// The check is on PRESENCE, not type. Declaring "int" and passing a string is a
// type error the task will surface with far better context than this layer can,
// and a half-implemented type checker here would be a second, weaker opinion
// about the same question.
func missingRequiredParams(specJSON []byte, params map[string]any) []string {
	if len(specJSON) == 0 {
		return nil
	}
	var doc struct {
		Params map[string]struct {
			Required    bool   `json:"required"`
			DefaultJSON string `json:"default_json"`
		} `json:"params"`
	}
	if err := json.Unmarshal(specJSON, &doc); err != nil {
		// A spec whose JSON will not parse is a problem, but it is not THIS
		// function's problem to report: parsePolicy already fails on it and the
		// caller surfaces that. Returning "nothing missing" here keeps a single
		// error path for a malformed spec.
		return nil
	}
	var missing []string
	for name, p := range doc.Params {
		if !p.Required || p.DefaultJSON != "" {
			continue
		}
		if _, ok := params[name]; !ok {
			missing = append(missing, name)
		}
	}
	// Sorted so two identical requests produce identical messages; an error whose
	// wording changes between attempts is one nobody can match against a log.
	sort.Strings(missing)
	return missing
}

// actorKey lets the API layer attribute a transition to the operator who caused
// it, so run_events answers "who cancelled this" rather than just "it was
// cancelled".
type actorKey struct{}

// WithActor attaches the acting principal to a context.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

func actorFor(ctx context.Context) string {
	if a, ok := ctx.Value(actorKey{}).(string); ok && a != "" {
		return a
	}
	return "workflow-manager"
}
