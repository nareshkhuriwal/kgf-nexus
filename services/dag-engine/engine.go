// Command dag-engine is the Nexus DAG engine service (architecture §3.2).
//
// It owns the dependency decisions of a run: materializing the compiled graph,
// computing the ready frontier, propagating SKIPPED and UPSTREAM_FAILED, and
// deciding when a run is terminal.
//
// ALL dependency logic lives in internal/dag, which is pure and has no I/O. This
// package is deliberately the thin impure shell around it: load state, call the
// pure function, write the result. Keeping the split sharp is what lets the
// interesting half be exhaustively and property-tested (§3.2), and it means a bug
// found here is a plumbing bug, not a semantics bug.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/dag"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// RunStateWriter publishes a terminal run state.
//
// The DAG engine DECIDES the run is terminal but does not OWN workflow_runs
// (ADR-0011 gives it to the workflow manager), so the write goes through the
// owner's API. It is an interface rather than a concrete client so the engine can
// be tested without standing up a second service, and so a deployment that
// colocates them can pass a direct call.
type RunStateWriter interface {
	SetRunState(ctx context.Context, runID string, state domain.RunState, reason string) error
}

// EventWriter appends to run_events, owned by the event service.
//
// Events are best-effort from the engine's perspective: run_events is the audit
// log, and failing a reconcile because the audit sink was briefly unavailable
// would trade a correctness-neutral gap for a stalled run. The current-state
// tables remain correct either way.
type EventWriter interface {
	AppendEvent(ctx context.Context, runID, nodeID, kind string, payload map[string]any) error
}

// Engine is the service-layer orchestrator.
type Engine struct {
	store *store.DAGEngineStore
	runs  RunStateWriter
	ev    EventWriter
	log   *slog.Logger

	// validation is the submit-time policy. Held here so the same binary can run
	// with Phase-2 trigger rules enabled by configuration rather than a rebuild.
	validation dag.ValidationOptions
}

// NewEngine wires the engine. runs and ev may be nil, in which case the engine
// runs "headless": it still maintains task_nodes/task_edges and creates attempts,
// but cannot publish run state or events. That mode exists for tests and for a
// first-boot ordering where the other services are not up yet.
func NewEngine(st *store.DAGEngineStore, runs RunStateWriter, ev EventWriter, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		store:      st,
		runs:       runs,
		ev:         ev,
		log:        log,
		validation: dag.DefaultValidationOptions(),
	}
}

// SetValidationOptions overrides submit-time validation policy.
func (e *Engine) SetValidationOptions(o dag.ValidationOptions) { e.validation = o }

// MaterializeResult reports what Materialize did.
type MaterializeResult struct {
	RunID           string
	NodesInserted   int64
	EdgesInserted   int64
	Frontier        []string
	AttemptsCreated []store.CreatedAttempt
	AlreadyExisted  bool
}

// Materialize compiles a run's pinned spec into task_nodes and task_edges, then
// creates the initial PENDING attempts for the ready frontier.
//
// It re-validates the spec even though the gateway validated it at submit time.
// That is not redundant: the spec row could have been written by a different
// service version, and materializing a cyclic or type-incompatible graph would
// produce a run that can never complete. Validation is cheap; a wedged run is not.
//
// Materialize is idempotent, because at-least-once delivery means it will be
// called twice (CONTRACT.md invariant: at-least-once + idempotency). Re-calling it
// on an already-materialized run reconciles instead of duplicating.
func (e *Engine) Materialize(ctx context.Context, runID string) (*MaterializeResult, error) {
	log := e.log.With("run_id", runID)

	run, err := e.store.LoadRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("materialize: %w", err)
	}
	spec, err := e.store.LoadSpec(ctx, run.SpecID)
	if err != nil {
		return nil, fmt.Errorf("materialize run %s: %w", runID, err)
	}

	g, verrs := dag.Validate(spec, e.validation)
	if len(verrs) > 0 {
		// Refusing to materialize is the right failure: a run that cannot be
		// expressed as a DAG must fail at creation with a precise reason, not
		// stall later with an unexplained empty frontier.
		log.Error("refusing to materialize an invalid spec", "errors", verrs.Error())
		return nil, fmt.Errorf("materialize run %s: invalid spec: %w", runID, verrs.AsError())
	}

	existing, err := e.store.CountNodes(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("materialize run %s: %w", runID, err)
	}

	nodes := make([]store.DENodeRow, 0, len(g.Nodes()))
	for _, id := range g.Nodes() {
		n := g.Node(id)
		nodes = append(nodes, store.DENodeRow{
			RunID:       runID,
			NodeID:      id,
			TaskRef:     n.TaskRef,
			State:       domain.NodePending,
			TriggerRule: string(n.TriggerRule),
			MaxAttempts: domain.MaxAttempts(n.Spec.GetRetry()),
			Spec:        n.Spec,
		})
	}
	edges := make([]store.DEEdgeRow, 0, len(g.Edges()))
	for _, ed := range g.Edges() {
		edges = append(edges, store.DEEdgeRow{
			RunID: runID, From: ed.From, To: ed.To, OutputToInput: ed.OutputToInput,
		})
	}

	ni, ei, err := e.store.InsertNodesAndEdges(ctx, runID, nodes, edges)
	if err != nil {
		return nil, fmt.Errorf("materialize run %s: %w", runID, err)
	}
	log.Info("materialized dag", "nodes_inserted", ni, "edges_inserted", ei, "node_count", len(nodes))

	res := &MaterializeResult{
		RunID: runID, NodesInserted: ni, EdgesInserted: ei, AlreadyExisted: existing > 0,
	}

	// The initial frontier is just a reconcile over an all-PENDING graph, so it
	// goes through exactly the same code path as every later one. A separate
	// "initial dispatch" path would be a second implementation of the frontier
	// rule and would eventually disagree with the first.
	rec, err := e.Reconcile(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("materialize run %s: initial reconcile: %w", runID, err)
	}
	res.Frontier = rec.Frontier
	res.AttemptsCreated = rec.AttemptsCreated
	return res, nil
}

// ReconcileResult reports one reconcile pass.
type ReconcileResult struct {
	RunID           string
	Frontier        []string
	Transitions     []dag.Transition
	AttemptsCreated []store.CreatedAttempt
	RunState        domain.RunState
	RunTerminal     bool
	Halted          bool
	CASMisses       []string
}

// Reconcile recomputes the run from ground truth and applies the result.
//
// It rebuilds node state from the ATTEMPT rows rather than trusting
// task_nodes.state, because attempts are the record of what actually happened and
// task_nodes.state is a projection of them. On any disagreement the attempts win;
// this is the same "the log is the truth, current state is a projection" rule the
// schema states for run_events, applied one level down. It also means a reconcile
// that crashed halfway through self-heals on the next pass instead of needing
// repair.
//
// Reconcile is safe to call concurrently from several replicas and safe to call
// repeatedly: every write it makes is CAS-guarded or ON CONFLICT DO NOTHING.
func (e *Engine) Reconcile(ctx context.Context, runID string) (*ReconcileResult, error) {
	log := e.log.With("run_id", runID)

	run, err := e.store.LoadRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("reconcile: %w", err)
	}
	nodes, err := e.store.LoadNodes(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("reconcile run %s: %w", runID, err)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("reconcile run %s: no nodes materialized", runID)
	}
	edges, err := e.store.LoadEdges(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("reconcile run %s: %w", runID, err)
	}
	attempts, err := e.store.LoadAttempts(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("reconcile run %s: %w", runID, err)
	}

	spec, err := e.store.LoadSpec(ctx, run.SpecID)
	if err != nil {
		return nil, fmt.Errorf("reconcile run %s: %w", runID, err)
	}
	g := graphFromRows(nodes, edges, spec)

	stored := make(dag.States, len(nodes))
	for _, n := range nodes {
		stored[n.NodeID] = dag.NodeState(n.State)
	}
	current := deriveNodeStates(g, nodes, attempts, stored)

	decision := dag.Reconcile(dag.ReconcileInput{
		Graph:    g,
		States:   current,
		RunState: dag.RunState(run.State),
	})

	// Only transitions that actually change the stored value are written. Writing
	// a no-op CAS for every node on every pass would turn a read-mostly reconcile
	// into a write storm on large runs.
	var writes []store.NodeTransition
	for _, t := range decision.Transitions {
		from := stored.Get(t.NodeID)
		if from == t.To {
			continue
		}
		writes = append(writes, store.NodeTransition{
			NodeID: t.NodeID,
			From:   domain.NodeState(from),
			To:     domain.NodeState(t.To),
		})
	}
	// A node whose derived state has moved on (RUNNING, SUCCEEDED, FAILED from its
	// attempts) must be written back too, or task_nodes.state would drift from the
	// attempts it projects and the UI would show a stale DAG.
	for _, n := range nodes {
		derived := current.Get(n.NodeID)
		if domain.NodeState(derived) == n.State {
			continue
		}
		if containsTransition(writes, n.NodeID) {
			continue
		}
		writes = append(writes, store.NodeTransition{
			NodeID: n.NodeID, From: n.State, To: domain.NodeState(derived),
		})
	}

	newAttempts, err := e.buildAttempts(ctx, runID, run, g, decision.Frontier, attempts)
	if err != nil {
		return nil, fmt.Errorf("reconcile run %s: %w", runID, err)
	}

	applied, err := e.store.ApplyNodeStateAndCreateAttempts(ctx, runID, writes, newAttempts)
	if err != nil {
		return nil, fmt.Errorf("reconcile run %s: %w", runID, err)
	}

	for _, t := range decision.Transitions {
		if t.To == dag.NodeSkipped || t.To == dag.NodeUpstreamFailed {
			// Logged individually and at INFO: "why did this node not run" is the
			// most common question asked of a failed run, and the cause is the
			// answer.
			log.Info("node did not run",
				"node_id", t.NodeID, "state", string(t.To),
				"reason", string(t.Reason), "cause", t.Cause)
			e.emit(ctx, runID, t.NodeID, "NodeStateChanged", map[string]any{
				"state": string(t.To), "reason": string(t.Reason), "cause": t.Cause,
			})
		}
	}
	for _, a := range applied.AttemptsCreated {
		log.Info("created pending attempt",
			"node_id", a.NodeID, "attempt_id", a.AttemptID, "attempt_number", a.AttemptNumber)
		e.emit(ctx, runID, a.NodeID, domain.EventAttemptScheduled, map[string]any{
			"attempt_id": a.AttemptID, "attempt_number": a.AttemptNumber,
		})
	}
	if len(applied.NodeCASMisses) > 0 {
		// Normal under concurrency: another replica won the race. Sustained high
		// rates mean replicas are fighting, which is worth noticing.
		log.Debug("node cas misses", "nodes", applied.NodeCASMisses)
	}

	res := &ReconcileResult{
		RunID:           runID,
		Frontier:        decision.Frontier,
		Transitions:     decision.Transitions,
		AttemptsCreated: applied.AttemptsCreated,
		RunState:        domain.RunState(decision.RunState),
		Halted:          decision.Halted,
		CASMisses:       applied.NodeCASMisses,
	}

	// A PENDING run with work in flight is RUNNING. The engine publishes this as
	// well as the terminal states because it is the component that knows: the run
	// becomes RUNNING exactly when the frontier first yields attempts, and only
	// the engine computes the frontier.
	//
	// Without it a run stays PENDING for its whole life while its nodes succeed
	// underneath it, and — worse than cosmetic — the scheduler's candidate query
	// admits only runs in ('PENDING','RUNNING'), so the two states differ for
	// pause and cancel handling downstream.
	if run.State == domain.RunPending && len(applied.AttemptsCreated) > 0 {
		if err := e.publishRunState(ctx, runID, domain.RunRunning); err != nil {
			// Visibility, not correctness: the attempts are already durable and
			// dispatchable, and the next reconcile republishes.
			log.Error("could not publish RUNNING run state", "error", err)
		}
	}

	switch decision.RunState {
	case dag.RunSucceeded, dag.RunFailed, dag.RunCancelled, dag.RunTimedOut:
		res.RunTerminal = true
		if domain.RunState(decision.RunState) != run.State {
			log.Info("run reached a terminal state", "state", string(decision.RunState))
			if err := e.publishRunState(ctx, runID, domain.RunState(decision.RunState)); err != nil {
				// The node state is already correct and durable, so a failure to
				// publish is a visibility problem, not a correctness one. The next
				// reconcile republishes.
				log.Error("could not publish terminal run state", "error", err,
					"state", string(decision.RunState))
			}
		}

	// PAUSED is not terminal, but it is just as much a conclusion only this
	// component can reach. internal/dag decides PAUSING -> PAUSED once nothing is
	// in flight, and for the same reason it decides everything else: it is the only
	// component that can see whether any attempt is still running.
	//
	// Publishing it was missing, so the decision was computed and thrown away on
	// every reconcile: a paused run's last attempt would finish and the run would
	// sit at PAUSING forever, with nothing in flight and nothing left to trigger
	// another look. An operator could not tell "still draining" from "drained", and
	// PAUSING is what §5.2 uses to mean the first.
	case dag.RunPaused:
		if domain.RunState(decision.RunState) != run.State {
			log.Info("run finished draining and is now paused",
				"previous_state", string(run.State))
			if err := e.publishRunState(ctx, runID, domain.RunPaused); err != nil {
				log.Error("could not publish PAUSED run state", "error", err)
			}
		}
	}
	return res, nil
}

func containsTransition(ts []store.NodeTransition, nodeID string) bool {
	for _, t := range ts {
		if t.NodeID == nodeID {
			return true
		}
	}
	return false
}

// buildAttempts turns a frontier into PENDING attempt rows, resolving each node's
// inputs from its upstreams' outputs.
func (e *Engine) buildAttempts(
	ctx context.Context, runID string, run *store.DERunRow, g *dag.Graph,
	frontier []string, attempts []store.DEAttemptRow,
) ([]store.NewAttempt, error) {
	if len(frontier) == 0 {
		return nil, nil
	}
	outputs := latestSucceededOutputs(attempts)

	out := make([]store.NewAttempt, 0, len(frontier))
	for _, nodeID := range frontier {
		n := g.Node(nodeID)
		if n == nil {
			return nil, fmt.Errorf("frontier names unknown node %q", nodeID)
		}
		inputRefs, err := e.resolveInputs(g, nodeID, outputs)
		if err != nil {
			return nil, fmt.Errorf("resolve inputs for node %q: %w", nodeID, err)
		}
		priority := n.Spec.GetPriority()
		if priority == 0 {
			// Inherit the run's priority so a spec that does not set a per-task
			// priority still respects the run's place in the queue.
			priority = run.Priority
		}
		out = append(out, store.NewAttempt{
			RunID:    runID,
			NodeID:   nodeID,
			Queue:    n.Spec.GetQueue(),
			Priority: priority,
			// request, not limit: request is what the scheduler subtracts from
			// worker capacity; limit is what the executor enforces (§4.3).
			Resources: n.Spec.GetResources().GetRequest(),
			// Excludes attempt_number by construction (CONTRACT.md invariant 1),
			// so every retry of this work shares one key.
			IdempotencyKey: domain.IdempotencyKey(runID, nodeID, nil, n.Spec.GetVersion()),
			InputRefs:      inputRefs,
		})
	}
	return out, nil
}

// resolveInputs maps upstream attempt output_ref values into this node's
// input_refs, per the edge's output_to_input mapping.
//
// The mapping direction follows the frozen proto comment on nexusv1.Edge:
// key = downstream input name, value = upstream output name. See the note in
// internal/dag/resolve.go.
//
// Only inputs sourced from an EDGE are written here. Params and defaults are
// resolved by the worker from the spec and the run's params, because those values
// are already available there and copying them into every attempt row would
// duplicate data the spec already holds.
func (e *Engine) resolveInputs(
	g *dag.Graph, nodeID string, outputs map[string]map[string]json.RawMessage,
) ([]byte, error) {
	plan, err := dag.InputPlan(g, nodeID)
	if err != nil {
		return nil, err
	}
	refs := map[string]json.RawMessage{}
	for _, b := range plan {
		if b.Source() != dag.FromEdge {
			continue
		}
		upOutputs, ok := outputs[b.UpstreamNode]
		if !ok {
			// Legitimate: an all_done downstream of a FAILED upstream has no
			// outputs to read. The input is simply absent, and the handler sees it
			// as absent rather than as a null that looks like a real value.
			continue
		}
		v, ok := upOutputs[b.UpstreamOutput]
		if !ok {
			continue
		}
		refs[b.InputName] = v
	}
	if len(refs) == 0 {
		return []byte(`{}`), nil
	}
	raw, err := json.Marshal(refs)
	if err != nil {
		return nil, fmt.Errorf("marshal input_refs: %w", err)
	}
	return raw, nil
}

// latestSucceededOutputs indexes each node's outputs from its most recent
// SUCCEEDED attempt.
//
// Most recent SUCCEEDED, specifically: a node that failed twice and then
// succeeded must expose the successful run's outputs, and a later failed attempt
// must never shadow an earlier success. Attempts arrive ordered by
// attempt_number, so a simple last-write-wins over the successes is correct.
func latestSucceededOutputs(attempts []store.DEAttemptRow) map[string]map[string]json.RawMessage {
	out := map[string]map[string]json.RawMessage{}
	for _, a := range attempts {
		if a.State != domain.AttemptSucceeded || len(a.OutputRef) == 0 {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(a.OutputRef, &m); err != nil {
			// A malformed output_ref must not fail the whole reconcile: the
			// downstream input is simply unresolvable, which surfaces as a task
			// error on the node that needed it rather than a stalled run.
			continue
		}
		out[a.NodeID] = m
	}
	return out
}

// deriveNodeStates rebuilds node state from attempts, which are the ground truth.
//
// The retry budget decides whether a terminal-but-unsuccessful attempt means the
// NODE failed or is merely between tries. A node with retries left is RUNNING:
// the logical unit of work has not finished, and reporting it FAILED between
// attempts would make a retrying run look broken and could trip fail_fast.
func deriveNodeStates(
	g *dag.Graph, nodes []store.DENodeRow, attempts []store.DEAttemptRow, stored dag.States,
) dag.States {
	byNode := map[string][]store.DEAttemptRow{}
	for _, a := range attempts {
		byNode[a.NodeID] = append(byNode[a.NodeID], a)
	}

	out := make(dag.States, len(nodes))
	for _, n := range nodes {
		as := byNode[n.NodeID]
		if len(as) == 0 {
			// No attempt yet. Preserve a stored terminal state (SKIPPED or
			// UPSTREAM_FAILED, written by an earlier propagation); otherwise the
			// node is PENDING. Without this the engine would re-propagate the same
			// nodes on every pass and never converge.
			s := stored.Get(n.NodeID)
			if s.Terminal() {
				out[n.NodeID] = s
			} else {
				out[n.NodeID] = dag.NodePending
			}
			continue
		}
		last := as[len(as)-1]
		maxAttempts := n.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = 1
		}
		// Exhausted when no further attempt may be created. attempts_used is
		// incremented with each attempt row, so comparing the highest
		// attempt_number against the ceiling is the honest test.
		exhausted := last.AttemptNumber >= maxAttempts
		out[n.NodeID] = dag.AttemptStateToNode(string(last.State), exhausted)
	}
	return out
}

// graphFromRows rebuilds the pure Graph from the materialized rows rather than
// from the spec.
//
// The ROWS are what the run is actually executing: they were written when the run
// was created and are immutable for its lifetime (ADR-0004). Rebuilding from the
// spec would reintroduce exactly the bug ADR-0004 exists to prevent, where a spec
// edited mid-flight changes a running graph. Only fail_fast and params, which are
// run-level and not per-node, are taken from the spec.
func graphFromRows(nodes []store.DENodeRow, edges []store.DEEdgeRow, spec *nexusv1.WorkflowSpec) *dag.Graph {
	rebuilt := &nexusv1.WorkflowSpec{
		FailFast: spec.GetFailFast(),
		Params:   spec.GetParams(),
	}
	for _, n := range nodes {
		ts := n.Spec
		if ts == nil {
			ts = &nexusv1.TaskSpec{Id: n.NodeID, TaskRef: n.TaskRef}
		}
		// The stored trigger_rule column is authoritative over the spec blob: it is
		// the column the rest of the platform and any operator SQL reads, so a
		// disagreement must resolve the same way everywhere.
		ts.TriggerRule = triggerRuleToProto(n.TriggerRule)
		rebuilt.Tasks = append(rebuilt.Tasks, ts)
	}
	for _, ed := range edges {
		rebuilt.Edges = append(rebuilt.Edges, &nexusv1.Edge{
			FromNodeId: ed.From, ToNodeId: ed.To, OutputToInput: ed.OutputToInput,
		})
	}
	return dag.Build(rebuilt)
}

func triggerRuleToProto(s string) nexusv1.TriggerRule {
	switch dag.TriggerRule(s) {
	case dag.AllDone:
		return nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE
	case dag.OneSuccess:
		return nexusv1.TriggerRule_TRIGGER_RULE_ONE_SUCCESS
	case dag.OneFailed:
		return nexusv1.TriggerRule_TRIGGER_RULE_ONE_FAILED
	case dag.NoneFailed:
		return nexusv1.TriggerRule_TRIGGER_RULE_NONE_FAILED
	case dag.AllSkipped:
		return nexusv1.TriggerRule_TRIGGER_RULE_ALL_SKIPPED
	default:
		return nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS
	}
}

func (e *Engine) publishRunState(ctx context.Context, runID string, s domain.RunState) error {
	if e.runs == nil {
		return nil
	}
	if err := e.runs.SetRunState(ctx, runID, s, "dag_engine_terminal_detection"); err != nil {
		return fmt.Errorf("publish run state %s for %s: %w", s, runID, err)
	}
	return nil
}

// emit appends a run event, best-effort. See EventWriter.
func (e *Engine) emit(ctx context.Context, runID, nodeID, kind string, payload map[string]any) {
	if e.ev == nil {
		return
	}
	if err := e.ev.AppendEvent(ctx, runID, nodeID, kind, payload); err != nil {
		e.log.Warn("could not append run event",
			"run_id", runID, "node_id", nodeID, "kind", kind, "error", err)
	}
}

// ValidateSpec exposes submit-time validation so the gateway can reuse the exact
// evaluator the engine will execute with. Two implementations of "is this spec
// valid" would eventually disagree, and the disagreement would only ever show up
// as an accepted spec that cannot run.
func (e *Engine) ValidateSpec(spec *nexusv1.WorkflowSpec) error {
	_, errs := dag.Validate(spec, e.validation)
	return errs.AsError()
}

// ErrNotMaterialized is returned when a run has no nodes yet.
var ErrNotMaterialized = errors.New("run has no materialized nodes")
