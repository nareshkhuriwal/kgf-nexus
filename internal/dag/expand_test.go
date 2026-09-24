// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package dag

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// mapSpec is the canonical fixture: list -> process(map over list.files) -> summarize.
// summarize's trigger rule is a parameter because fan-in tolerance is the
// interesting axis.
func mapSpec(maxFanout int32, fanInRule nexusv1.TriggerRule) *nexusv1.WorkflowSpec {
	js := func(req bool) *nexusv1.ValueSpec { return &nexusv1.ValueSpec{Type: "json", Required: req} }
	return &nexusv1.WorkflowSpec{
		ApiVersion: "v1", Namespace: "default", Name: "fanout",
		Tasks: []*nexusv1.TaskSpec{
			{Id: "list", TaskRef: "list:v1", Version: 1,
				Outputs: map[string]*nexusv1.ValueSpec{"files": js(true)}},
			{Id: "process", TaskRef: "process:v1", Version: 1,
				Inputs:  map[string]*nexusv1.ValueSpec{"file": js(true)},
				Outputs: map[string]*nexusv1.ValueSpec{"result": js(true)},
				Map:     &nexusv1.MapSpec{OverNodeId: "list", OverOutput: "files", ItemInput: "file", MaxFanout: maxFanout}},
			{Id: "summarize", TaskRef: "summarize:v1", Version: 1, TriggerRule: fanInRule,
				Inputs: map[string]*nexusv1.ValueSpec{"results": js(true)}},
		},
		Edges: []*nexusv1.Edge{
			{FromNodeId: "list", ToNodeId: "process"},
			{FromNodeId: "process", ToNodeId: "summarize", OutputToInput: map[string]string{"results": "result"}},
		},
	}
}

// expandedGraph returns the run graph after `n` children were materialized, the
// way the service rebuilds it from task_nodes rows.
func expandedGraph(spec *nexusv1.WorkflowSpec, n int) *Graph {
	g := Build(spec)
	out := &nexusv1.WorkflowSpec{FailFast: spec.GetFailFast(), Params: spec.GetParams()}
	out.Tasks = append(out.Tasks, spec.GetTasks()...)
	out.Edges = append(out.Edges, spec.GetEdges()...)
	children := map[string]ChildRef{}
	tmpl := g.Node("process")
	for i := 0; i < n; i++ {
		id := ChildID("process", i)
		out.Tasks = append(out.Tasks, ChildSpec(tmpl.Spec, id))
		for _, e := range ChildEdges(g, "process", id) {
			out.Edges = append(out.Edges, &nexusv1.Edge{FromNodeId: e.From, ToNodeId: e.To, OutputToInput: e.OutputToInput})
		}
		children[id] = ChildRef{Parent: "process", MapIndex: i}
	}
	return BuildWithChildren(out, children)
}

func inlineRef(t *testing.T, v any) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := InlineJSONRef(payload)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestPlanFromCollection_Cardinalities(t *testing.T) {
	cases := []struct {
		name      string
		items     []any
		maxFanout int
		wantN     int
		wantFail  string
	}{
		{"zero items expands to nothing", []any{}, 10, 0, ""},
		{"one item", []any{"a"}, 10, 1, ""},
		{"N items", []any{"a", "b", "c", "d"}, 10, 4, ""},
		{"exactly at the bound is allowed", []any{1, 2, 3}, 3, 3, ""},
		{"one over the bound fails explicitly", []any{1, 2, 3, 4}, 3, 0, ExpandMaxFanoutExceeded},
		{"missing bound fails", []any{1}, 0, 0, ExpandMaxFanoutExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanFromCollection("process", inlineRef(t, tc.items), tc.maxFanout)
			if tc.wantFail != "" {
				var ef *ExpansionFailure
				if !errors.As(err, &ef) || ef.Reason != tc.wantFail {
					t.Fatalf("err = %v, want ExpansionFailure %s", err, tc.wantFail)
				}
				if got != nil {
					t.Fatalf("a refused expansion must plan NOTHING, got %d children", len(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("PlanFromCollection: %v", err)
			}
			if len(got) != tc.wantN {
				t.Fatalf("children = %d, want %d", len(got), tc.wantN)
			}
			for i, c := range got {
				if c.MapIndex != i || c.NodeID != ChildID("process", i) {
					t.Errorf("child %d = %+v", i, c)
				}
				want, _ := json.Marshal(tc.items[i])
				if string(c.Item) != string(want) {
					t.Errorf("child %d item = %s, want %s", i, c.Item, want)
				}
			}
		})
	}
}

func TestPlanFromCollection_RefusesWhatItCannotRead(t *testing.T) {
	blob := json.RawMessage(`{"blob":{"bucket":"b","key":"ns/abc","sha256":"abc"},"content_type":"application/json"}`)
	cases := map[string]struct {
		ref  json.RawMessage
		want string
	}{
		"blob collection (invariant 6)": {blob, ExpandCollectionNotInline},
		"missing value":                 {nil, ExpandCollectionMissing},
		"null value":                    {json.RawMessage("null"), ExpandCollectionMissing},
		"an object, not an array":       {inlineRefRaw(`{"a":1}`), ExpandCollectionNotArray},
		"a scalar":                      {inlineRefRaw(`42`), ExpandCollectionNotArray},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := PlanFromCollection("p", tc.ref, 100)
			var ef *ExpansionFailure
			if !errors.As(err, &ef) || ef.Reason != tc.want {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
		})
	}
}

func inlineRefRaw(payload string) json.RawMessage {
	r, _ := InlineJSONRef([]byte(payload))
	return r
}

// A replayed expansion must produce byte-identical children, or the idempotency
// key of child i would differ between the crashed attempt and the retry.
func TestPlanFromCollection_DeterministicAndCanonical(t *testing.T) {
	a := inlineRefRaw(`[{"b":1,"a":2}, "x", 1.50]`)
	b := inlineRefRaw(`[ {"a":2, "b":1} ,"x",1.50]`)
	pa, err := PlanFromCollection("p", a, 10)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := PlanFromCollection("p", b, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pa, pb) {
		t.Fatalf("two encodings of one collection planned differently:\n%v\n%v", pa, pb)
	}
	if string(pa[0].Item) != `{"a":2,"b":1}` || string(pa[2].Item) != `1.50` {
		t.Errorf("items not canonical / number spelling not preserved: %s %s", pa[0].Item, pa[2].Item)
	}
}

func TestReconcile_TemplateIsExpandedNotDispatched(t *testing.T) {
	spec := mapSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
	g := Build(spec)

	d := Reconcile(ReconcileInput{Graph: g, States: States{}, RunState: RunRunning})
	if !reflect.DeepEqual(d.Frontier, []string{"list"}) || len(d.Expansions) != 0 {
		t.Fatalf("initial: frontier=%v expansions=%v", d.Frontier, d.Expansions)
	}

	d = Reconcile(ReconcileInput{Graph: g, States: States{"list": NodeSucceeded}, RunState: RunRunning})
	if len(d.Frontier) != 0 {
		t.Fatalf("a template must never be in the frontier (it would get an attempt): %v", d.Frontier)
	}
	if !reflect.DeepEqual(d.Expansions, []string{"process"}) {
		t.Fatalf("expansions = %v, want [process]", d.Expansions)
	}
	if d.RunState != RunRunning {
		t.Fatalf("run state = %s", d.RunState)
	}
}

func TestReconcile_ChildrenRunAndFanInWaitsForAll(t *testing.T) {
	spec := mapSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
	g := expandedGraph(spec, 3)
	st := States{"list": NodeSucceeded, "process": NodeRunning}

	d := Reconcile(ReconcileInput{Graph: g, States: st, RunState: RunRunning})
	want := []string{"process[0]", "process[1]", "process[2]"}
	if !reflect.DeepEqual(d.Frontier, want) {
		t.Fatalf("frontier = %v, want %v", d.Frontier, want)
	}

	// Two of three done: the fan-in must still wait.
	st = States{"list": NodeSucceeded, "process": NodeRunning,
		"process[0]": NodeSucceeded, "process[1]": NodeSucceeded, "process[2]": NodeRunning}
	d = Reconcile(ReconcileInput{Graph: g, States: st, RunState: RunRunning})
	if len(d.Frontier) != 0 {
		t.Fatalf("fan-in started before every child finished: %v", d.Frontier)
	}

	// The last one lands: the template settles AND the fan-in starts in ONE pass.
	st["process[2]"] = NodeSucceeded
	d = Reconcile(ReconcileInput{Graph: g, States: st, RunState: RunRunning})
	if !reflect.DeepEqual(d.Frontier, []string{"summarize"}) {
		t.Fatalf("frontier = %v, want [summarize]", d.Frontier)
	}
	if got := ApplyTransitions(st, d.Transitions)["process"]; got != NodeSucceeded {
		t.Fatalf("template = %s, want SUCCEEDED", got)
	}
}

func TestReconcile_EmptyExpansionReleasesFanIn(t *testing.T) {
	// The service writes an empty expansion as template SUCCEEDED with no children.
	g := expandedGraph(mapSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS), 0)
	d := Reconcile(ReconcileInput{Graph: g,
		States: States{"list": NodeSucceeded, "process": NodeSucceeded}, RunState: RunRunning})
	if !reflect.DeepEqual(d.Frontier, []string{"summarize"}) {
		t.Fatalf("frontier = %v", d.Frontier)
	}
	// And a RUNNING template with zero children (defensive) settles to SUCCEEDED.
	d = Reconcile(ReconcileInput{Graph: g,
		States: States{"list": NodeSucceeded, "process": NodeRunning}, RunState: RunRunning})
	if !reflect.DeepEqual(d.Frontier, []string{"summarize"}) {
		t.Fatalf("frontier = %v", d.Frontier)
	}
}

func TestReconcile_ChildFailureAndFanInTolerance(t *testing.T) {
	for _, tc := range []struct {
		rule        nexusv1.TriggerRule
		wantFanIn   NodeState // after the reconcile
		wantRunning bool
	}{
		{nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS, NodeUpstreamFailed, false},
		{nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE, NodeReady, true},
	} {
		t.Run(string(TriggerRuleFromProto(tc.rule)), func(t *testing.T) {
			g := expandedGraph(mapSpec(10, tc.rule), 2)
			st := States{"list": NodeSucceeded, "process": NodeRunning,
				"process[0]": NodeSucceeded, "process[1]": NodeFailed}
			d := Reconcile(ReconcileInput{Graph: g, States: st, RunState: RunRunning})
			after := ApplyTransitions(st, d.Transitions)
			if after["process"] != NodeFailed {
				t.Fatalf("template = %s, want FAILED", after["process"])
			}
			if after["summarize"] != tc.wantFanIn {
				t.Fatalf("fan-in = %s, want %s", after["summarize"], tc.wantFanIn)
			}
			if tc.wantRunning != (d.RunState == RunRunning) {
				t.Fatalf("run state = %s", d.RunState)
			}
			if !tc.wantRunning && d.RunState != RunFailed {
				t.Fatalf("run = %s, want FAILED", d.RunState)
			}
		})
	}
}

func TestReconcile_CancelDuringAndAfterExpansion(t *testing.T) {
	spec := mapSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)

	// Cancel arrives before the template expanded: it must NOT expand.
	d := Reconcile(ReconcileInput{Graph: Build(spec),
		States: States{"list": NodeSucceeded}, RunState: RunCancelling})
	if len(d.Expansions) != 0 {
		t.Fatalf("a cancelling run expanded %v", d.Expansions)
	}
	if got := ApplyTransitions(States{"list": NodeSucceeded}, d.Transitions)["process"]; got != NodeCancelled {
		t.Fatalf("unexpanded template = %s, want CANCELLED", got)
	}
	if d.RunState != RunCancelled {
		t.Fatalf("run = %s, want CANCELLED", d.RunState)
	}

	// Cancel after expansion with nothing in flight: children and template settle
	// in the SAME pass, because nothing will trigger another one.
	g := expandedGraph(spec, 3)
	st := States{"list": NodeSucceeded, "process": NodeRunning,
		"process[0]": NodeSucceeded, "process[1]": NodePending, "process[2]": NodeReady}
	d = Reconcile(ReconcileInput{Graph: g, States: st, RunState: RunCancelling})
	after := ApplyTransitions(st, d.Transitions)
	for _, id := range []string{"process[1]", "process[2]", "process", "summarize"} {
		if after[id] != NodeCancelled {
			t.Errorf("%s = %s, want CANCELLED", id, after[id])
		}
	}
	if d.RunState != RunCancelled {
		t.Fatalf("run = %s, want CANCELLED", d.RunState)
	}

	// With a child still RUNNING the run stays CANCELLING and the template waits.
	st["process[0]"] = NodeRunning
	d = Reconcile(ReconcileInput{Graph: g, States: st, RunState: RunCancelling})
	if d.RunState != RunCancelling {
		t.Fatalf("run = %s, want CANCELLING while a child runs", d.RunState)
	}
}

func TestReconcile_PauseWithholdsExpansionAndReachesPaused(t *testing.T) {
	spec := mapSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
	d := Reconcile(ReconcileInput{Graph: Build(spec),
		States: States{"list": NodeSucceeded}, RunState: RunPausing})
	if len(d.Expansions) != 0 {
		t.Fatalf("a pausing run expanded %v", d.Expansions)
	}
	if d.RunState != RunPaused {
		t.Fatalf("run = %s, want PAUSED", d.RunState)
	}

	// Expanded, children undispatched: the RUNNING template must not hold the run
	// in PAUSING forever.
	g := expandedGraph(spec, 2)
	d = Reconcile(ReconcileInput{Graph: g,
		States: States{"list": NodeSucceeded, "process": NodeRunning}, RunState: RunPausing})
	if d.RunState != RunPaused || len(d.Frontier) != 0 {
		t.Fatalf("run = %s frontier = %v, want PAUSED and no dispatch", d.RunState, d.Frontier)
	}
	// Resume: the children are dispatched.
	d = Reconcile(ReconcileInput{Graph: g,
		States: States{"list": NodeSucceeded, "process": NodeRunning}, RunState: RunRunning})
	if len(d.Frontier) != 2 {
		t.Fatalf("after resume frontier = %v", d.Frontier)
	}
}

func TestReconcile_FailFastSkipsUnexpandedTemplate(t *testing.T) {
	spec := mapSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE)
	spec.FailFast = true
	spec.Tasks = append(spec.Tasks, &nexusv1.TaskSpec{Id: "side", TaskRef: "side:v1"})
	spec.Edges = append(spec.Edges, &nexusv1.Edge{FromNodeId: "list", ToNodeId: "side"})
	g := Build(spec)
	st := States{"list": NodeSucceeded, "side": NodeFailed}
	d := Reconcile(ReconcileInput{Graph: g, States: st, RunState: RunRunning})
	if len(d.Expansions) != 0 {
		t.Fatalf("fail_fast halted run expanded %v", d.Expansions)
	}
	if got := ApplyTransitions(st, d.Transitions)["process"]; got != NodeSkipped {
		t.Fatalf("template = %s, want SKIPPED", got)
	}
}

func TestInputPlan_ChildItemComesFromTheMap(t *testing.T) {
	g := expandedGraph(mapSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS), 2)
	plan, err := InputPlan(g, "process[1]")
	if err != nil {
		t.Fatalf("InputPlan: %v", err)
	}
	if len(plan) != 1 || plan[0].InputName != "file" || plan[0].Source() != FromMapItem {
		t.Fatalf("plan = %+v, want file<-map_item", plan)
	}
	// And the fan-in reads the TEMPLATE, not a child.
	plan, err = InputPlan(g, "summarize")
	if err != nil {
		t.Fatalf("InputPlan: %v", err)
	}
	if plan[0].UpstreamNode != "process" {
		t.Fatalf("fan-in binding = %+v", plan[0])
	}
}

func TestAggregateChildren(t *testing.T) {
	cases := []struct {
		in   []NodeState
		want NodeState
		ok   bool
	}{
		{nil, NodeSucceeded, true},
		{[]NodeState{NodeSucceeded, NodeSucceeded}, NodeSucceeded, true},
		{[]NodeState{NodeSucceeded, NodeRunning}, "", false},
		{[]NodeState{NodeSucceeded, NodePending}, "", false},
		{[]NodeState{NodeFailed, NodeCancelled, NodeSkipped}, NodeFailed, true},
		{[]NodeState{NodeUpstreamFailed, NodeSucceeded}, NodeFailed, true},
		{[]NodeState{NodeCancelled, NodeSucceeded, NodeSkipped}, NodeCancelled, true},
		{[]NodeState{NodeSkipped, NodeSucceeded}, NodeSkipped, true},
	}
	for _, tc := range cases {
		got, ok := AggregateChildren(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("AggregateChildren(%v) = %s,%v want %s,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestFanInValue_PositionalWithNullsAndRefs(t *testing.T) {
	blob := json.RawMessage(`{"blob":{"bucket":"b","key":"ns/k","sha256":"k"}}`)
	outs := []map[string]json.RawMessage{
		{"result": inlineRefRaw(`{"n":1}`)},
		nil, // failed child
		{"result": blob},
		{"result": mustRef([]byte("not json"))},
	}
	ref, err := FanInValue(outs, "result", 0)
	if err != nil {
		t.Fatal(err)
	}
	payload, inline, err := inlinePayload(ref)
	if err != nil || !inline {
		t.Fatalf("fan-in ref must be inline: %v", err)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(payload, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 4 {
		t.Fatalf("len = %d", len(arr))
	}
	if string(arr[0]) != `{"n":1}` || string(arr[1]) != "null" {
		t.Errorf("arr[0..1] = %s %s", arr[0], arr[1])
	}
	var r map[string]json.RawMessage
	_ = json.Unmarshal(arr[2], &r)
	if _, ok := r["result_ref"]; !ok {
		t.Errorf("blob child must be passed as a reference, got %s", arr[2])
	}
	want := fmt.Sprintf(`{"inline_base64":%q}`, base64.StdEncoding.EncodeToString([]byte("not json")))
	if string(arr[3]) != want {
		t.Errorf("arr[3] = %s want %s", arr[3], want)
	}
}

func mustRef(b []byte) json.RawMessage {
	r, _ := json.Marshal(map[string]any{"inline": b, "content_type": "text/plain"})
	return r
}

func TestValidate_MapSpecs(t *testing.T) {
	ok := mapSpec(100, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
	if _, errs := Validate(ok, DefaultValidationOptions()); len(errs) > 0 {
		t.Fatalf("valid map spec rejected: %v", errs)
	}

	mutate := func(f func(s *nexusv1.WorkflowSpec)) *nexusv1.WorkflowSpec {
		s := mapSpec(100, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
		f(s)
		return s
	}
	cases := map[string]struct {
		spec *nexusv1.WorkflowSpec
		want ErrorCode
		opts ValidationOptions
	}{
		"max_fanout missing": {mutate(func(s *nexusv1.WorkflowSpec) { s.Tasks[1].Map.MaxFanout = 0 }), ErrMapMaxFanout, DefaultValidationOptions()},
		"max_fanout above the operator ceiling": {mutate(func(s *nexusv1.WorkflowSpec) { s.Tasks[1].Map.MaxFanout = 11 }), ErrMapMaxFanout,
			ValidationOptions{MaxFanoutCeiling: 10}},
		"source is not a direct upstream": {mutate(func(s *nexusv1.WorkflowSpec) {
			s.Edges[0] = &nexusv1.Edge{FromNodeId: "summarize", ToNodeId: "list"}
			s.Edges = s.Edges[1:]
			s.Tasks = append(s.Tasks, &nexusv1.TaskSpec{Id: "x", TaskRef: "x"})
			s.Edges = append(s.Edges, &nexusv1.Edge{FromNodeId: "x", ToNodeId: "process"})
		}), ErrMapSource, DefaultValidationOptions()},
		"source output undeclared": {mutate(func(s *nexusv1.WorkflowSpec) { s.Tasks[1].Map.OverOutput = "nope" }), ErrMapSource, DefaultValidationOptions()},
		"source output not json":   {mutate(func(s *nexusv1.WorkflowSpec) { s.Tasks[0].Outputs["files"].Type = "string" }), ErrMapSource, DefaultValidationOptions()},
		"item input undeclared":    {mutate(func(s *nexusv1.WorkflowSpec) { s.Tasks[1].Map.ItemInput = "nope" }), ErrMapItemInput, DefaultValidationOptions()},
		"item input bound by an edge": {mutate(func(s *nexusv1.WorkflowSpec) {
			s.Edges[0].OutputToInput = map[string]string{"file": "files"}
		}), ErrMapItemInput, DefaultValidationOptions()},
		"fan-in input not json": {mutate(func(s *nexusv1.WorkflowSpec) { s.Tasks[2].Inputs["results"].Type = "string" }), ErrMapFanInType, DefaultValidationOptions()},
		"bracket in an authored id": {mutate(func(s *nexusv1.WorkflowSpec) {
			s.Tasks = append(s.Tasks, &nexusv1.TaskSpec{Id: "process[0]", TaskRef: "x"})
			s.Edges = append(s.Edges, &nexusv1.Edge{FromNodeId: "list", ToNodeId: "process[0]"})
		}), ErrReservedNodeID, DefaultValidationOptions()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, errs := Validate(tc.spec, tc.opts)
			if !errs.Has(tc.want) {
				t.Fatalf("want %s, got %v", tc.want, errs)
			}
		})
	}
}

func TestValidateCache(t *testing.T) {
	s := &nexusv1.WorkflowSpec{Tasks: []*nexusv1.TaskSpec{
		{Id: "pure", TaskRef: "p", Cache: &nexusv1.CachePolicy{Enabled: true, TtlMs: 1000}},
		{Id: "impure", TaskRef: "i", Idempotency: nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_NONE,
			Cache: &nexusv1.CachePolicy{Enabled: true}},
	}}
	errs := ValidateCache(s)
	if len(errs) != 1 || errs[0].NodeID != "impure" || errs[0].Code != ErrCacheNotIdempotent {
		t.Fatalf("errs = %v", errs)
	}
	if Memoizable(s.Tasks[1]) {
		t.Fatal("idempotency:none must never be memoizable, even when cache is enabled")
	}
	if !Memoizable(s.Tasks[0]) || Memoizable(&nexusv1.TaskSpec{}) {
		t.Fatal("memoization must be opt-in")
	}
	if CacheTTLMs(s.Tasks[1]) != DefaultCacheTTLMs || CacheTTLMs(s.Tasks[0]) != 1000 {
		t.Fatal("ttl defaulting wrong")
	}
}

// Property: for any collection size within the bound and any assignment of
// terminal child outcomes, driving the engine to a fixed point terminates, the
// template holds exactly the aggregate of its children, and the run state is the
// §5.2 verdict over all nodes.
func TestProperty_ExpansionConverges(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	outcomes := []NodeState{NodeSucceeded, NodeSucceeded, NodeSucceeded, NodeFailed, NodeCancelled}
	for iter := 0; iter < 400; iter++ {
		n := rng.Intn(12)
		rule := nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS
		if rng.Intn(2) == 0 {
			rule = nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE
		}
		spec := mapSpec(16, rule)
		g := expandedGraph(spec, n)
		st := States{"list": NodeSucceeded, "process": NodeRunning}
		var kids []NodeState
		for i := 0; i < n; i++ {
			o := outcomes[rng.Intn(len(outcomes))]
			st[ChildID("process", i)] = o
			kids = append(kids, o)
		}
		want, _ := AggregateChildren(kids)

		var d Decision
		for step := 0; step < 5; step++ {
			d = Reconcile(ReconcileInput{Graph: g, States: st, RunState: RunRunning})
			st = ApplyTransitions(st, d.Transitions)
			// Any node the engine readied "runs" and succeeds.
			for _, id := range d.Frontier {
				st[id] = NodeSucceeded
			}
		}
		if st["process"] != want {
			t.Fatalf("iter %d: template = %s, want aggregate %s of %v", iter, st["process"], want, kids)
		}
		if !allTerminal(g, st) {
			t.Fatalf("iter %d: did not converge: %v", iter, st)
		}
		if got := RunOutcome(g, st); d.RunState != got {
			t.Fatalf("iter %d: run = %s, RunOutcome = %s", iter, d.RunState, got)
		}
	}
}
