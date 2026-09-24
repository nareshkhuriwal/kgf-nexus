// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/dag"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// Event kinds the engine emits for expansion and memoization. Distinct kinds
// rather than payload flags so the console's timeline and any alerting can
// filter on them without parsing payloads.
const (
	EventNodeExpanded    = "NodeExpanded"
	EventExpansionFailed = "ExpansionFailed"
	EventCacheHit        = "CacheHit"
)

// expandTemplate performs ONE template's expansion (§5.4): read the collection
// from the upstream's output, plan the children, and commit them atomically.
//
// Returns applied=false when a concurrent reconciler already expanded it, which
// is the normal outcome of a race or a replay and needs no action.
//
// An expansion the author's data makes impossible -- over max_fanout, not an
// array, a blob instead of an inline value -- is NOT an error of this function.
// It is committed as a FAILED expansion, which fails the template node
// explicitly and lets propagation settle everything downstream. Returning an
// error instead would make reconcile fail forever on the same input, which is a
// wedged run, the exact outcome §5.4's mandatory bound exists to prevent.
func (e *Engine) expandTemplate(
	ctx context.Context, run *store.DERunRow, g *dag.Graph, templateID string,
	outputs map[string]map[string]json.RawMessage, sourceAttempts map[string]store.DEAttemptRow,
) (bool, error) {
	log := e.log.With("run_id", run.ID, "node_id", templateID)
	tmpl := g.Node(templateID)
	if tmpl == nil || !tmpl.IsTemplate() {
		return false, fmt.Errorf("expand %s: not a map template", templateID)
	}
	m := tmpl.Spec.GetMap()
	w := store.ExpansionWrite{
		RunID:           run.ID,
		TemplateNodeID:  templateID,
		TemplateTaskRef: tmpl.TaskRef,
		// Children inherit the template's rule: they have exactly its upstreams.
		TriggerRule:  string(tmpl.TriggerRule),
		MaxAttempts:  domain.MaxAttempts(tmpl.Spec.GetRetry()),
		SourceNodeID: m.GetOverNodeId(),
		SourceOutput: m.GetOverOutput(),
		MaxFanout:    m.GetMaxFanout(),
		Outcome:      store.ExpansionOutcomeExpanded,
	}
	if a, ok := sourceAttempts[m.GetOverNodeId()]; ok {
		id := a.ID
		w.SourceAttemptID = &id
	}

	planned, err := dag.PlanFromCollection(templateID, outputs[m.GetOverNodeId()][m.GetOverOutput()], int(m.GetMaxFanout()))
	var refusal *dag.ExpansionFailure
	switch {
	case errors.As(err, &refusal):
		w.Outcome, w.Reason, w.Detail = store.ExpansionOutcomeFailed, refusal.Reason, refusal.Detail
	case err != nil:
		return false, fmt.Errorf("plan expansion of %s: %w", templateID, err)
	default:
		item := m.GetItemInput()
		w.Children = make([]store.ExpansionChild, 0, len(planned))
		for _, pc := range planned {
			spec := dag.ChildSpec(tmpl.Spec, pc.NodeID)
			// The element travels as a LITERAL on the child's own spec. That makes
			// the child row self-describing -- the console can show which item a
			// child processed, and an operator can read it in SQL -- and it reuses
			// the literal path of input resolution rather than inventing a fourth
			// way for a value to reach a task.
			if spec.InputLiterals == nil {
				spec.InputLiterals = map[string]string{}
			}
			spec.InputLiterals[item] = string(pc.Item)
			var edges []store.DEEdgeRow
			for _, ed := range dag.ChildEdges(g, templateID, pc.NodeID) {
				edges = append(edges, store.DEEdgeRow{RunID: run.ID, From: ed.From, To: ed.To, OutputToInput: ed.OutputToInput})
			}
			w.Children = append(w.Children, store.ExpansionChild{
				NodeID: pc.NodeID, MapIndex: int32(pc.MapIndex), Spec: spec, Edges: edges,
			})
		}
	}

	start := time.Now()
	applied, err := e.store.ApplyExpansion(ctx, w)
	if err != nil {
		return false, fmt.Errorf("apply expansion of %s: %w", templateID, err)
	}
	if !applied {
		log.Debug("expansion already applied by another reconciler")
		return false, nil
	}
	if w.Outcome == store.ExpansionOutcomeFailed {
		log.Warn("expansion refused; template node FAILED",
			"reason", w.Reason, "detail", w.Detail, "max_fanout", w.MaxFanout)
		e.emit(ctx, run.ID, templateID, EventExpansionFailed, map[string]any{
			"reason": w.Reason, "detail": w.Detail, "max_fanout": w.MaxFanout,
			"source_node_id": w.SourceNodeID, "source_output": w.SourceOutput,
		})
		return true, nil
	}
	log.Info("expanded map template",
		"children", len(w.Children), "max_fanout", w.MaxFanout, "took", time.Since(start))
	e.emit(ctx, run.ID, templateID, EventNodeExpanded, map[string]any{
		"children": len(w.Children), "max_fanout": w.MaxFanout,
		"source_node_id": w.SourceNodeID, "source_output": w.SourceOutput,
		"source_attempt_id": w.SourceAttemptID,
	})
	return true, nil
}

// templateOutputs synthesizes the outputs of every settled map template: for
// each output name the template declares, the fan-in array of its children's
// values (dag.FanInValue). A template has no attempt of its own, so without
// this a downstream fan-in would find nothing to read.
//
// Only SETTLED templates contribute. The fan-in is gated on the template being
// terminal anyway, and building the array for a template still running would be
// wasted work on the hottest path of a large fan-out.
func templateOutputs(g *dag.Graph, st dag.States, outputs map[string]map[string]json.RawMessage) error {
	for _, t := range g.Templates() {
		if !st.Get(t).Terminal() {
			continue
		}
		kids := g.Children(t)
		childOuts := make([]map[string]json.RawMessage, len(kids))
		for i, k := range kids {
			if st.Get(k) == dag.NodeSucceeded {
				childOuts[i] = outputs[k]
			}
		}
		agg := map[string]json.RawMessage{}
		for name := range g.Node(t).Spec.GetOutputs() {
			v, err := dag.FanInValue(childOuts, name)
			if err != nil {
				return fmt.Errorf("fan-in of %s.%s: %w", t, name, err)
			}
			agg[name] = v
		}
		outputs[t] = agg
	}
	return nil
}

// latestSucceededAttempts indexes each node's most recent SUCCEEDED attempt,
// for naming the exact attempt an expansion read its collection from.
func latestSucceededAttempts(attempts []store.DEAttemptRow) map[string]store.DEAttemptRow {
	out := map[string]store.DEAttemptRow{}
	for _, a := range attempts {
		if a.State == domain.AttemptSucceeded {
			out[a.NodeID] = a
		}
	}
	return out
}

// memoFor decides whether a frontier node's work can be satisfied from the
// memo cache (§6.4), returning the cache key to record on the attempt and the
// hit, if any.
//
// The key covers task identity (task_ref, version), the RESOLVED inputs, and the
// namespace. Resolved inputs, not declared ones: two runs whose upstream
// produced different data must never share a result. They are hashed by
// CONTENT (dag.InputsDigest), so the same bytes delivered inline in one run and
// as a blob in another -- or carried by two ref encodings -- still hit.
//
// The namespace is in the key deliberately, going beyond §6.4's formula: a
// result's blob lives under its tenant's result-store prefix, which another
// tenant's workers have no credentials for, and a cross-tenant hit would also be
// a data leak. Isolation is enforced twice -- here, and in the lookup predicate.
func (e *Engine) memoFor(
	ctx context.Context, run *store.DERunRow, n *dag.Node, inputRefs []byte, now time.Time,
) (cacheKey *string, hit *store.MemoHit, err error) {
	if !dag.Memoizable(n.Spec) {
		return nil, nil, nil
	}
	digest, err := dag.InputsDigest(inputRefs)
	if err != nil {
		// Unhashable inputs mean "not memoizable this time", never a failed
		// reconcile: memoization is an optimisation, and an optimisation that can
		// wedge a run is a liability.
		e.log.Warn("inputs not hashable; running without memoization",
			"run_id", run.ID, "node_id", n.ID, "error", err)
		return nil, nil, nil
	}
	key := domain.MemoKey(run.Namespace, n.TaskRef, n.Spec.GetVersion(), digest)
	hit, err = e.store.LookupMemo(ctx, store.MemoQuery{
		Namespace: run.Namespace, CacheKey: key, TaskRef: n.TaskRef, Version: n.Spec.GetVersion(),
		TTL: time.Duration(dag.CacheTTLMs(n.Spec)) * time.Millisecond, Now: now,
	})
	if err != nil {
		// Same reasoning: a lookup failure degrades to executing the task.
		e.log.Warn("memo lookup failed; running without memoization",
			"run_id", run.ID, "node_id", n.ID, "error", err)
		return &key, nil, nil
	}
	return &key, hit, nil
}

// InvalidateCache records an operator's memo invalidation (§6.4).
func (e *Engine) InvalidateCache(ctx context.Context, inv store.MemoInvalidation) (int64, time.Time, error) {
	id, at, err := e.store.InvalidateMemo(ctx, inv)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("invalidate cache: %w", err)
	}
	e.log.Info("memo cache invalidated",
		"namespace", inv.Namespace, "task_ref", inv.TaskRef, "cache_key", inv.CacheKey,
		"actor", inv.Actor, "reason", inv.Reason, "invalidation_id", id)
	return id, at, nil
}
