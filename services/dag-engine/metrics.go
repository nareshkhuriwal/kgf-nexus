// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// Metrics are the DAG engine's service instruments. Labels are node/run STATE
// enums and operation names only -- never run or node ids -- so the series count
// is fixed by the state machine, not by how many runs exist.
//
// A nil *Metrics records nothing.
type Metrics struct {
	ops         *metrics.CounterVec
	opDuration  *metrics.HistogramVec
	transitions *metrics.CounterVec
	attempts    *metrics.CounterVec
	casMisses   *metrics.Counter
	expansions  *metrics.Counter
	fanInRefuse *metrics.Counter
	runStates   *metrics.CounterVec
	retries     *metrics.CounterVec
	retryResets *metrics.Counter
}

// NewMetrics registers the dag-engine families on reg.
func NewMetrics(reg *metrics.Registry) *Metrics {
	return &Metrics{
		ops: reg.NewCounter("nexus_dag_operations_total",
			"Engine operations completed, by operation (materialize, reconcile) and result (ok, error).",
			"op", "result"),
		opDuration: reg.NewHistogram("nexus_dag_operation_duration_seconds",
			"Latency of one engine operation (every pass of a reconcile), by operation.",
			[]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
			"op"),
		transitions: reg.NewCounter("nexus_dag_node_transitions_total",
			"Node state transitions committed (CAS won), by from and to state.",
			"from", "to"),
		attempts: reg.NewCounter("nexus_dag_attempts_created_total",
			"Attempts created by the engine, by kind (new = PENDING work, cache_hit = satisfied from the memo cache).",
			"kind"),
		casMisses: reg.NewCounter("nexus_dag_node_cas_misses_total",
			"Node writes that lost a compare-and-swap to a concurrent reconcile. A sustained rate means replicas are fighting over the same runs.").Counter(),
		expansions: reg.NewCounter("nexus_dag_map_expansions_total",
			"Map templates expanded into children.").Counter(),
		fanInRefuse: reg.NewCounter("nexus_dag_fanin_refused_total",
			"Fan-in assemblies refused for exceeding NEXUS_DAG_MAX_FANIN_BYTES (the consuming node FAILED).").Counter(),
		runStates: reg.NewCounter("nexus_dag_run_state_published_total",
			"Run state changes the engine published to the workflow-manager, by state and result.",
			"state", "result"),
		retries: reg.NewCounter("nexus_dag_operator_retries_total",
			"Operator retries (ResetNodes) applied to a run's graph, by result: reset, invalid (unknown node ids), not_retryable (run or nodes not in a retryable state), not_found, conflict (a concurrent writer won) or error.",
			"result"),
		retryResets: reg.NewCounter("nexus_dag_operator_retry_nodes_reset_total",
			"Nodes reset to PENDING (or an expanded template to RUNNING) by operator retries, selected nodes and their dependents alike.").Counter(),
	}
}

func (m *Metrics) observeOp(op string, start time.Time, err error) {
	if m == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error"
	}
	m.ops.With(metrics.Labels{"op": op, "result": result}).Inc()
	m.opDuration.With(metrics.Labels{"op": op}).Observe(time.Since(start).Seconds())
}

// observeApplied records what one reconcile pass actually committed.
func (m *Metrics) observeApplied(writes []store.NodeTransition, casMisses []string,
	created []store.CreatedAttempt, refusals, expanded int) {
	if m == nil {
		return
	}
	missed := make(map[string]bool, len(casMisses))
	for _, id := range casMisses {
		missed[id] = true
	}
	for _, w := range writes {
		if missed[w.NodeID] {
			continue
		}
		m.transitions.With(metrics.Labels{"from": string(w.From), "to": string(w.To)}).Inc()
	}
	m.casMisses.Add(float64(len(casMisses)))
	for _, a := range created {
		kind := "new"
		if a.CacheHitOf != nil {
			kind = "cache_hit"
		}
		m.attempts.With(metrics.Labels{"kind": kind}).Inc()
	}
	m.fanInRefuse.Add(float64(refusals))
	m.expansions.Add(float64(expanded))
}

func (m *Metrics) observeRunState(state string, err error) {
	if m == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error"
	}
	m.runStates.With(metrics.Labels{"state": state, "result": result}).Inc()
}

// observeOperatorRetry records one ResetNodes call: its result and how many
// nodes it reset.
func (m *Metrics) observeOperatorRetry(result string, nodesReset int) {
	if m == nil {
		return
	}
	m.retries.With(metrics.Labels{"result": result}).Inc()
	m.retryResets.Add(float64(nodesReset))
}
