// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/khuriwalgroup/kgf-nexus/internal/dag"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// ErrRunNotRetryable means ResetNodes was asked to reset a run that is not in a
// retryable terminal state. Retry is defined on a run that has FINISHED badly;
// resetting nodes under a run that is still RUNNING would race the reconciles
// that are still settling it (CONTRACT-FINDINGS F100).
var ErrRunNotRetryable = errors.New("run is not in a retryable state")

// EventNodeRetried is the per-node history entry of an operator retry.
const EventNodeRetried = "NodeStateChanged"

// ReasonOperatorRetry is the transition reason recorded for a retry's resets.
const ReasonOperatorRetry = "operator_retry"

// ResetResult reports what a retry reset.
type ResetResult struct {
	RunID string
	// Selected is the effective selection: the requested nodes, or the
	// whole-run default when none were named.
	Selected []string
	// Reset is every node that was reset, selected nodes and dependents alike.
	Reset []string
}

// ResetNodes applies an operator retry to a run's graph: the selected nodes and
// their non-succeeded dependents go back to PENDING (an expanded template to
// RUNNING), and nothing else changes. See dag.PlanRetry for the semantics.
//
// It does NOT reconcile, and that is deliberate. The workflow-manager calls this
// while it holds the run row locked and has the run's move back to RUNNING
// uncommitted; it reconciles only after committing. Reconciling here would see
// the run still FAILED, and could publish a terminal state back to the
// workflow-manager -- which would block on the very lock its caller holds.
//
// The run must still be FAILED or TIMED_OUT as committed. That is what the
// workflow-manager's uncommitted transition leaves visible, and it keeps any
// other caller from resetting nodes under a live run.
func (e *Engine) ResetNodes(ctx context.Context, runID string, selected []string, actor string) (*ResetResult, error) {
	res, err := e.resetNodes(ctx, runID, selected, actor)
	reset := 0
	if res != nil {
		reset = len(res.Reset)
	}
	e.metrics.observeOperatorRetry(retryResult(err), reset)
	return res, err
}

func (e *Engine) resetNodes(ctx context.Context, runID string, selected []string, actor string) (*ResetResult, error) {
	log := e.log.With("run_id", runID)

	run, err := e.store.LoadRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("reset nodes: %w", err)
	}
	if run.State != domain.RunFailed && run.State != domain.RunTimedOut {
		return nil, fmt.Errorf("reset nodes of run %s: %w: it is %s, and only FAILED or TIMED_OUT runs can be retried",
			runID, ErrRunNotRetryable, run.State)
	}
	nodes, err := e.store.LoadNodes(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("reset nodes of run %s: %w", runID, err)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("reset nodes of run %s: %w", runID, ErrNotMaterialized)
	}
	edges, err := e.store.LoadEdges(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("reset nodes of run %s: %w", runID, err)
	}
	spec, err := e.store.LoadSpec(ctx, run.SpecID)
	if err != nil {
		return nil, fmt.Errorf("reset nodes of run %s: %w", runID, err)
	}
	exps, err := e.store.LoadExpansions(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("reset nodes of run %s: %w", runID, err)
	}
	inFlight, err := e.store.InFlightNodes(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("reset nodes of run %s: %w", runID, err)
	}

	// Planned from the STORED states, not re-derived ones: the resets are
	// applied as compare-and-sets against the stored value. For a terminal run
	// the two agree -- the reconcile that made it terminal wrote them back.
	states := make(dag.States, len(nodes))
	for _, n := range nodes {
		states[n.NodeID] = dag.NodeState(n.State)
	}
	expansions := make(map[string]dag.ExpansionStatus, len(exps))
	for _, x := range exps {
		if x.Outcome == store.ExpansionOutcomeFailed {
			expansions[x.TemplateNodeID] = dag.ExpansionRefused
		} else {
			expansions[x.TemplateNodeID] = dag.Expanded
		}
	}

	plan, err := dag.PlanRetry(dag.RetryInput{
		Graph:      graphFromRows(nodes, edges, spec),
		States:     states,
		Selected:   selected,
		Expansions: expansions,
		InFlight:   inFlight,
	})
	if err != nil {
		log.Info("retry refused", "selected", selected, "reason", err.Error())
		return nil, fmt.Errorf("reset nodes of run %s: %w", runID, err)
	}

	writes := make([]store.NodeRetryReset, 0, len(plan.Resets))
	res := &ResetResult{RunID: runID, Selected: plan.Selected}
	for _, r := range plan.Resets {
		writes = append(writes, store.NodeRetryReset{
			NodeID: r.NodeID, From: domain.NodeState(r.From), To: domain.NodeState(r.To),
		})
		res.Reset = append(res.Reset, r.NodeID)
	}
	if err := e.store.ApplyRetryResets(ctx, runID, writes); err != nil {
		return nil, fmt.Errorf("reset nodes of run %s: %w", runID, err)
	}

	for _, r := range plan.Resets {
		e.emit(ctx, runID, r.NodeID, EventNodeRetried, map[string]any{
			"state": string(r.To), "previous_state": string(r.From),
			"reason": ReasonOperatorRetry, "requested_by": actor,
		})
	}
	log.Info("nodes reset for operator retry",
		"selected", plan.Selected, "reset", res.Reset, "requested_by", actor)
	return res, nil
}

// isRetryRefusal reports whether err is the planner refusing the selection, and
// whether the refusal is about the REQUEST (unknown node ids) rather than the
// run's current state.
func isRetryRefusal(err error) (refused, badRequest bool) {
	var re *dag.RetryError
	if !errors.As(err, &re) {
		return false, false
	}
	return true, len(re.Unknown) > 0
}

// Operator-retry results, the closed label set of nexus_dag_operator_retries_total.
const (
	retryResultReset         = "reset"
	retryResultInvalid       = "invalid"
	retryResultNotRetryable  = "not_retryable"
	retryResultNotFound      = "not_found"
	retryResultConflict      = "conflict"
	retryResultInternalError = "error"
)

// retryResult classifies a ResetNodes outcome the same way toStatus maps it to
// a gRPC code, so the metric and the caller's view of the retry agree:
// invalid (unknown node ids: the request was malformed), not_retryable (the
// run or the selected nodes are not in a retryable state), not_found,
// conflict (a concurrent writer won; asking again may succeed) and error.
func retryResult(err error) string {
	switch {
	case err == nil:
		return retryResultReset
	case errors.Is(err, store.ErrDAGEngineRunNotFound):
		return retryResultNotFound
	case errors.Is(err, ErrNotMaterialized), errors.Is(err, ErrRunNotRetryable):
		return retryResultNotRetryable
	case errors.Is(err, store.ErrNodeRetryConflict):
		return retryResultConflict
	}
	if refused, badRequest := isRetryRefusal(err); refused {
		if badRequest {
			return retryResultInvalid
		}
		return retryResultNotRetryable
	}
	return retryResultInternalError
}
