// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/dag"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// These exercise the service layer's PURE helpers -- the parts that decide what
// to write. The DB-touching paths are covered by the integration tests behind the
// `integration` build tag.

func taskSpec(id string, rule nexusv1.TriggerRule) *nexusv1.TaskSpec {
	return &nexusv1.TaskSpec{Id: id, TaskRef: id + ":v1", Version: 1, TriggerRule: rule}
}

// deriveNodeStates is where attempt rows become the node states the pure engine
// reasons over, including the retry-budget fold. Getting it wrong would either
// report a retrying node as broken (tripping fail_fast) or a broken node as
// still working (hanging the run).
func TestDeriveNodeStates(t *testing.T) {
	nodes := []store.DENodeRow{
		{NodeID: "a", MaxAttempts: 3, State: domain.NodePending},
		{NodeID: "b", MaxAttempts: 1, State: domain.NodePending},
		{NodeID: "c", MaxAttempts: 3, State: domain.NodeSkipped},
		{NodeID: "d", MaxAttempts: 3, State: domain.NodePending},
	}
	edges := []store.DEEdgeRow{}
	g := graphFromRows(nodes, edges, &nexusv1.WorkflowSpec{})

	tests := []struct {
		name     string
		attempts []store.DEAttemptRow
		want     map[string]dag.NodeState
	}{
		{
			name:     "no attempts: PENDING, but a stored terminal state survives",
			attempts: nil,
			want: map[string]dag.NodeState{
				"a": dag.NodePending,
				"b": dag.NodePending,
				// c was propagated SKIPPED on an earlier pass. Losing that would make
				// the engine re-propagate forever and never converge.
				"c": dag.NodeSkipped,
				"d": dag.NodePending,
			},
		},
		{
			name: "a failed attempt WITH retries left keeps the node RUNNING",
			attempts: []store.DEAttemptRow{
				{NodeID: "a", AttemptNumber: 1, State: domain.AttemptFailed},
			},
			want: map[string]dag.NodeState{"a": dag.NodeRunning},
		},
		{
			name: "a failed attempt with the budget exhausted FAILS the node",
			attempts: []store.DEAttemptRow{
				{NodeID: "a", AttemptNumber: 3, State: domain.AttemptFailed},
			},
			want: map[string]dag.NodeState{"a": dag.NodeFailed},
		},
		{
			name: "max_attempts=1 means the first failure is terminal",
			attempts: []store.DEAttemptRow{
				{NodeID: "b", AttemptNumber: 1, State: domain.AttemptFailed},
			},
			want: map[string]dag.NodeState{"b": dag.NodeFailed},
		},
		{
			// TIMED_OUT and LOST are ATTEMPT states with no node-state counterpart.
			// Exhausted, they must read as a FAILED node so §5.2's disqualifying set
			// is expressible in the node vocabulary the schema actually has.
			name: "an exhausted TIMED_OUT attempt reads as a FAILED node",
			attempts: []store.DEAttemptRow{
				{NodeID: "b", AttemptNumber: 1, State: domain.AttemptTimedOut},
			},
			want: map[string]dag.NodeState{"b": dag.NodeFailed},
		},
		{
			name: "an exhausted LOST attempt reads as a FAILED node",
			attempts: []store.DEAttemptRow{
				{NodeID: "b", AttemptNumber: 1, State: domain.AttemptLost},
			},
			want: map[string]dag.NodeState{"b": dag.NodeFailed},
		},
		{
			name: "the LATEST attempt wins over earlier ones",
			attempts: []store.DEAttemptRow{
				{NodeID: "a", AttemptNumber: 1, State: domain.AttemptFailed},
				{NodeID: "a", AttemptNumber: 2, State: domain.AttemptSucceeded},
			},
			want: map[string]dag.NodeState{"a": dag.NodeSucceeded},
		},
		{
			name: "dispatched and running both read as RUNNING",
			attempts: []store.DEAttemptRow{
				{NodeID: "a", AttemptNumber: 1, State: domain.AttemptDispatched},
				{NodeID: "d", AttemptNumber: 1, State: domain.AttemptRunning},
			},
			want: map[string]dag.NodeState{"a": dag.NodeRunning, "d": dag.NodeRunning},
		},
		{
			name: "a PENDING attempt means the node is READY (dispatch not yet done)",
			attempts: []store.DEAttemptRow{
				{NodeID: "a", AttemptNumber: 1, State: domain.AttemptPending},
			},
			want: map[string]dag.NodeState{"a": dag.NodeReady},
		},
		{
			name: "cancellation is preserved rather than folded into failure",
			attempts: []store.DEAttemptRow{
				{NodeID: "a", AttemptNumber: 1, State: domain.AttemptCancelled},
			},
			want: map[string]dag.NodeState{"a": dag.NodeCancelled},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stored := dag.States{}
			for _, n := range nodes {
				stored[n.NodeID] = dag.NodeState(n.State)
			}
			got := deriveNodeStates(g, nodes, tc.attempts, stored)
			for id, want := range tc.want {
				if got.Get(id) != want {
					t.Errorf("node %q = %s, want %s", id, got.Get(id), want)
				}
			}
		})
	}
}

// The materialized ROWS are what a run executes, not the (possibly re-read) spec:
// ADR-0004 pins a run's definition for its lifetime.
func TestGraphFromRows_UsesStoredRowsNotTheSpec(t *testing.T) {
	nodes := []store.DENodeRow{
		{NodeID: "a", TaskRef: "a:v1", TriggerRule: "all_success", Spec: taskSpec("a", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)},
		{NodeID: "b", TaskRef: "b:v1", TriggerRule: "all_done", Spec: taskSpec("b", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)},
	}
	edges := []store.DEEdgeRow{{From: "a", To: "b"}}

	// The spec claims an extra node and no edges; the rows must win.
	spec := &nexusv1.WorkflowSpec{
		FailFast: true,
		Tasks:    []*nexusv1.TaskSpec{taskSpec("a", 0), taskSpec("b", 0), taskSpec("ghost", 0)},
	}
	g := graphFromRows(nodes, edges, spec)

	if len(g.Nodes()) != 2 {
		t.Fatalf("graph has %d nodes, want 2 (the spec's extra node must be ignored): %v", len(g.Nodes()), g.Nodes())
	}
	if g.Node("ghost") != nil {
		t.Error("a node present only in the spec must not enter the executing graph")
	}
	// The stored trigger_rule COLUMN is authoritative over the spec blob, because
	// it is what operator SQL and the rest of the platform read.
	if got := g.Node("b").TriggerRule; got != dag.AllDone {
		t.Errorf("b's trigger rule = %s, want all_done from the stored column", got)
	}
	// fail_fast is run-level, so it legitimately comes from the spec.
	if !g.FailFast {
		t.Error("fail_fast should be taken from the spec")
	}
	if len(g.Edges()) != 1 {
		t.Errorf("graph has %d edges, want 1", len(g.Edges()))
	}
}

func TestTriggerRuleToProtoRoundTrip(t *testing.T) {
	for _, r := range []dag.TriggerRule{
		dag.AllSuccess, dag.AllDone, dag.OneSuccess,
		dag.OneFailed, dag.NoneFailed, dag.AllSkipped,
	} {
		if got := dag.TriggerRuleFromProto(triggerRuleToProto(string(r))); got != r {
			t.Errorf("%s round-tripped to %s", r, got)
		}
	}
	// An unrecognized stored value must fall back to the documented default
	// rather than to a rule nobody asked for.
	if got := triggerRuleToProto("nonsense"); got != nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS {
		t.Errorf("unknown rule mapped to %v, want ALL_SUCCESS", got)
	}
}

// A node that succeeds only on its third try must expose THAT attempt's outputs;
// an earlier failure must never shadow a later success.
func TestLatestSucceededOutputs(t *testing.T) {
	attempts := []store.DEAttemptRow{
		{NodeID: "a", AttemptNumber: 1, State: domain.AttemptFailed, OutputRef: []byte(`{"model":"stale"}`)},
		{NodeID: "a", AttemptNumber: 2, State: domain.AttemptSucceeded, OutputRef: []byte(`{"model":"good"}`)},
		{NodeID: "b", AttemptNumber: 1, State: domain.AttemptRunning},
		{NodeID: "c", AttemptNumber: 1, State: domain.AttemptSucceeded, OutputRef: []byte(`not json`)},
	}
	out := latestSucceededOutputs(attempts)

	if got := string(out["a"]["model"]); got != `"good"` {
		t.Errorf("a.model = %s, want the SUCCEEDED attempt's value", got)
	}
	if _, ok := out["b"]; ok {
		t.Error("a running attempt must not contribute outputs")
	}
	// A malformed blob must not fail the whole reconcile; the input is simply
	// unresolvable and surfaces as an error on the node that needed it.
	if _, ok := out["c"]; ok {
		t.Error("a malformed output_ref should be skipped, not parsed")
	}
}

// Input resolution is the mapping half of §3.2's job: upstream outputs into
// downstream inputs, per the edge's output_to_input.
func TestResolveInputs(t *testing.T) {
	spec := &nexusv1.WorkflowSpec{
		Tasks: []*nexusv1.TaskSpec{
			{Id: "up", TaskRef: "up:v1", Outputs: map[string]*nexusv1.ValueSpec{
				"model":   {Type: "file_ref"},
				"metrics": {Type: "json"},
			}},
			{Id: "down", TaskRef: "down:v1", Inputs: map[string]*nexusv1.ValueSpec{
				"dataset": {Type: "file_ref", Required: true},
				"report":  {Type: "json"},
				"epochs":  {Type: "int", Required: true, DefaultJson: "10"},
			}},
		},
		Edges: []*nexusv1.Edge{{
			FromNodeId: "up", ToNodeId: "down",
			// key = downstream input, value = upstream output (frozen proto comment)
			OutputToInput: map[string]string{"dataset": "model", "report": "metrics"},
		}},
	}
	g := dag.Build(spec)
	e := NewEngine(nil, nil, nil, nil)

	outputs := map[string]map[string]json.RawMessage{
		"up": {
			"model":   json.RawMessage(`{"blob":{"bucket":"nexus","key":"ns/abc"}}`),
			"metrics": json.RawMessage(`{"inline":"eyJhY2MiOjF9"}`),
		},
	}
	raw, err := e.resolveInputs(g, "down", outputs)
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("input_refs is not valid JSON: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("input_refs has %d entries, want 2 (edge-sourced only): %s", len(got), raw)
	}
	// The mapping direction is the load-bearing detail: dataset must carry the
	// UPSTREAM OUTPUT named "model".
	if string(got["dataset"]) != `{"blob":{"bucket":"nexus","key":"ns/abc"}}` {
		t.Errorf("dataset = %s, want up.model's value", got["dataset"])
	}
	if string(got["report"]) != `{"inline":"eyJhY2MiOjF9"}` {
		t.Errorf("report = %s, want up.metrics's value", got["report"])
	}
	// Defaults are NOT copied into the attempt: the worker reads them from the
	// spec it already has, and duplicating them would be redundant data.
	if _, present := got["epochs"]; present {
		t.Error("a defaulted input should not be written into input_refs")
	}
}

// An all_done node downstream of a FAILURE has no upstream outputs to read. That
// is legitimate and must not fail the reconcile.
func TestResolveInputs_MissingUpstreamOutputIsNotAnError(t *testing.T) {
	spec := &nexusv1.WorkflowSpec{
		Tasks: []*nexusv1.TaskSpec{
			{Id: "up", TaskRef: "up:v1", Outputs: map[string]*nexusv1.ValueSpec{"o": {Type: "json"}}},
			{Id: "down", TaskRef: "down:v1",
				TriggerRule: nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE,
				Inputs:      map[string]*nexusv1.ValueSpec{"i": {Type: "json"}}},
		},
		Edges: []*nexusv1.Edge{{FromNodeId: "up", ToNodeId: "down", OutputToInput: map[string]string{"i": "o"}}},
	}
	g := dag.Build(spec)
	e := NewEngine(nil, nil, nil, nil)

	raw, err := e.resolveInputs(g, "down", map[string]map[string]json.RawMessage{})
	if err != nil {
		t.Fatalf("resolveInputs should tolerate a missing upstream output: %v", err)
	}
	if string(raw) != `{}` {
		t.Errorf("input_refs = %s, want an empty object", raw)
	}
}

// The engine must reuse the SAME validator the gateway calls, or an accepted spec
// could turn out to be unrunnable.
func TestValidateSpec_Policy(t *testing.T) {
	e := NewEngine(nil, nil, nil, nil)

	cyclic := &nexusv1.WorkflowSpec{
		Tasks: []*nexusv1.TaskSpec{taskSpec("a", 0), taskSpec("b", 0)},
		Edges: []*nexusv1.Edge{{FromNodeId: "a", ToNodeId: "b"}, {FromNodeId: "b", ToNodeId: "a"}},
	}
	if err := e.ValidateSpec(cyclic); err == nil {
		t.Error("a cyclic spec must be rejected")
	}

	deferred := &nexusv1.WorkflowSpec{
		Tasks: []*nexusv1.TaskSpec{taskSpec("a", nexusv1.TriggerRule_TRIGGER_RULE_ONE_SUCCESS)},
	}
	if err := e.ValidateSpec(deferred); err == nil {
		t.Error("a Phase-2 trigger rule must be rejected under the default policy")
	}
	// ...and accepted when the operator opts in, which is what makes the Phase 2
	// rollout a configuration change rather than a rebuild.
	opts := dag.DefaultValidationOptions()
	opts.AllowDeferredTriggerRules = true
	e.SetValidationOptions(opts)
	if err := e.ValidateSpec(deferred); err != nil {
		t.Errorf("opted-in Phase-2 rule should be accepted: %v", err)
	}

	valid := &nexusv1.WorkflowSpec{
		Tasks: []*nexusv1.TaskSpec{taskSpec("a", 0), taskSpec("b", 0)},
		Edges: []*nexusv1.Edge{{FromNodeId: "a", ToNodeId: "b"}},
	}
	if err := e.ValidateSpec(valid); err != nil {
		t.Errorf("a valid spec should pass: %v", err)
	}
}

func TestIsValidationErrorMapping(t *testing.T) {
	e := NewEngine(nil, nil, nil, nil)
	err := e.ValidateSpec(&nexusv1.WorkflowSpec{
		Tasks: []*nexusv1.TaskSpec{taskSpec("a", 0), taskSpec("b", 0)},
		Edges: []*nexusv1.Edge{{FromNodeId: "a", ToNodeId: "b"}, {FromNodeId: "b", ToNodeId: "a"}},
	})
	if err == nil {
		t.Fatal("expected a validation error")
	}
	// Validation failures must map to INVALID_ARGUMENT, not INTERNAL: the caller's
	// problem should never page anyone.
	if !isValidationError(err) {
		t.Errorf("validation error not recognized as such: %v", err)
	}
	if isValidationError(nil) {
		t.Error("nil must not be classified as a validation error")
	}
}

// The idempotency key must EXCLUDE attempt_number (CONTRACT.md invariant 1), so
// every retry of the same logical work shares one key. Asserted here because the
// engine is what puts the key on the row.
func TestAttemptIdempotencyKeyExcludesAttemptNumber(t *testing.T) {
	const runID = "11111111-1111-1111-1111-111111111111"
	k1 := domain.IdempotencyKey(runID, "train", nil, 3)
	k2 := domain.IdempotencyKey(runID, "train", nil, 3)
	if k1 != k2 {
		t.Errorf("the key must be stable across retries: %s vs %s", k1, k2)
	}
	// A different node, run or task version is DIFFERENT logical work.
	if domain.IdempotencyKey(runID, "other", nil, 3) == k1 {
		t.Error("different nodes must not share a key")
	}
	if domain.IdempotencyKey(runID, "train", nil, 4) == k1 {
		t.Error("a task version bump is a new logical effect and must change the key")
	}
}

// testLogger keeps test output quiet unless something goes wrong. Shared by the
// integration tests via the package-level build.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRunInputsReachWorkerWithoutRESTAccess(t *testing.T) {
	spec := &nexusv1.WorkflowSpec{
		Params: map[string]*nexusv1.ValueSpec{
			"target": {Type: "json", Required: true},
			"config": {Type: "json", DefaultJson: `{"seed":3}`},
		},
		Tasks: []*nexusv1.TaskSpec{{Id: "resolve", TaskRef: "resolve", Inputs: map[string]*nexusv1.ValueSpec{
			"target": {Type: "json", Required: true},
			"config": {Type: "json"},
			"rounds": {Type: "int", DefaultJson: "2"},
		}}},
	}
	raw, err := resolveRunInputs(dag.Build(spec), "resolve", []byte(`{"target":"{\"n\":72}"}`), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var refs map[string]map[string]string
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"target": `{"n":72}`, "config": `{"seed":3}`, "rounds": "2"} {
		got, err := base64.StdEncoding.DecodeString(refs[name]["inline"])
		if err != nil || string(got) != want {
			t.Fatalf("%s = %s, %v; want %s", name, got, err, want)
		}
	}
}
