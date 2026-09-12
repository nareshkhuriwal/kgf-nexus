package dag

import (
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// REGRESSION: found by the property test "every run reaches a terminal state".
//
// With fail_fast, a node whose own trigger rule IS satisfied (e.g. all_done
// downstream of a failure) is withheld from dispatch forever. Left PENDING it is
// invisible to propagation (its trigger is satisfiable) and invisible to dispatch
// (policy withholds it), so the run stays RUNNING indefinitely -- a hung run
// holding a concurrency slot with nothing to alert on. The engine must settle
// withheld work as SKIPPED instead.
func TestReconcile_FailFastSettlesWithheldWorkRatherThanHanging(t *testing.T) {
	// f fails; cleanup is all_done (so its trigger fires despite the failure) and
	// tail depends on cleanup.
	g := wf().failFast(true).
		task("f", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS).
		task("cleanup", nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE).
		task("tail", nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE).
		e([2]string{"f", "cleanup"}, [2]string{"cleanup", "tail"}).build()

	d := Reconcile(ReconcileInput{Graph: g, States: States{"f": NodeFailed}, RunState: RunRunning})

	if len(d.Frontier) != 0 {
		t.Fatalf("fail_fast must dispatch nothing, got %v", d.Frontier)
	}
	st := ApplyTransitions(States{"f": NodeFailed}, d.Transitions)
	for _, id := range []string{"cleanup", "tail"} {
		if got := st.Get(id); got != NodeSkipped {
			t.Errorf("%s = %s, want SKIPPED (withheld by the fail_fast policy)", id, got)
		}
	}
	// SKIPPED, not UPSTREAM_FAILED: these nodes' triggers were satisfied; policy
	// declined the work. UPSTREAM_FAILED must keep meaning "an ancestor broke".
	for _, tr := range d.Transitions {
		if tr.To == NodeUpstreamFailed {
			t.Errorf("node %q wrongly marked UPSTREAM_FAILED by fail_fast", tr.NodeID)
		}
	}
	if d.RunState != RunFailed {
		t.Errorf("run state = %s, want FAILED once everything is settled", d.RunState)
	}
	// And the run must genuinely be settled, not merely declared so.
	for _, id := range g.Nodes() {
		if !st.Get(id).Terminal() {
			t.Errorf("node %q left non-terminal (%s) in a FAILED run", id, st.Get(id))
		}
	}
}

// Without fail_fast the same graph must still RUN the cleanup: that is the whole
// point of all_done, and the fix above must not have broken it.
func TestReconcile_ContinueOnFailureStillRunsAllDoneCleanup(t *testing.T) {
	g := wf().failFast(false).
		task("f", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS).
		task("cleanup", nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE).
		e([2]string{"f", "cleanup"}).build()

	d := Reconcile(ReconcileInput{Graph: g, States: States{"f": NodeFailed}, RunState: RunRunning})
	if !setEq(d.Frontier, []string{"cleanup"}) {
		t.Fatalf("frontier = %v, want [cleanup]", d.Frontier)
	}
}

// ── InputPlan ─────────────────────────────────────────────────────────────────

func TestInputPlan_Table(t *testing.T) {
	// up emits model:file_ref and metrics:json; down consumes several inputs from
	// different sources, which is the case that exercises the precedence rules.
	g := wf().
		taskIO("up", nil, map[string]*nexusv1.ValueSpec{
			"model":   vspec("file_ref", false),
			"metrics": vspec("json", false),
		}).
		taskIO("down", map[string]*nexusv1.ValueSpec{
			"dataset":  vspec("file_ref", true),
			"report":   vspec("json", false),
			"epochs":   vspecDefault("int", "10"),
			"tuning":   vspec("json", true),
			"optional": vspec("string", false),
		}, nil).
		param("tuning", vspec("json", true)).
		edge("up", "down", map[string]string{"dataset": "model", "report": "metrics"}).build()

	plan, err := InputPlan(g, "down")
	if err != nil {
		t.Fatalf("InputPlan: %v", err)
	}
	byName := map[string]InputBinding{}
	for _, b := range plan {
		byName[b.InputName] = b
	}

	// Edge-sourced: must name the upstream node AND the upstream OUTPUT, since
	// the mapping key is the downstream input (see the direction note in
	// resolve.go).
	if b := byName["dataset"]; b.UpstreamNode != "up" || b.UpstreamOutput != "model" {
		t.Errorf("dataset binding = %+v, want up.model", b)
	}
	if got := byName["dataset"].Source(); got != FromEdge {
		t.Errorf("dataset source = %s, want edge", got)
	}
	if b := byName["report"]; b.UpstreamNode != "up" || b.UpstreamOutput != "metrics" {
		t.Errorf("report binding = %+v, want up.metrics", b)
	}
	if b := byName["epochs"]; b.DefaultJSON != "10" || b.Source() != FromDefault {
		t.Errorf("epochs binding = %+v, want default 10", b)
	}
	if b := byName["tuning"]; !b.FromParam || b.Source() != FromParamSrc {
		t.Errorf("tuning binding = %+v, want workflow param", b)
	}
	// An optional, unbound input is OMITTED rather than given a synthetic zero,
	// so the handler can tell "absent" from "empty".
	if _, present := byName["optional"]; present {
		t.Errorf("unbound optional input should be omitted, got %+v", byName["optional"])
	}
	if len(plan) != 4 {
		t.Errorf("plan has %d bindings, want 4: %+v", len(plan), plan)
	}
	// Deterministic order, so materialization is reproducible.
	want := []string{"dataset", "epochs", "report", "tuning"}
	for i, b := range plan {
		if b.InputName != want[i] {
			t.Errorf("plan order = %v, want %v", plan, want)
			break
		}
	}
}

// An edge must beat a same-named workflow param: a node wired to an upstream
// silently reading a run param instead would make the graph a lie.
func TestInputPlan_EdgeBeatsParamBeatsDefault(t *testing.T) {
	g := wf().
		taskIO("up", nil, map[string]*nexusv1.ValueSpec{"o": vspec("int", false)}).
		taskIO("down", map[string]*nexusv1.ValueSpec{
			"x": {Type: "int", Required: true, DefaultJson: "99"},
		}, nil).
		param("x", vspec("int", true)).
		edge("up", "down", map[string]string{"x": "o"}).build()

	plan, err := InputPlan(g, "down")
	if err != nil {
		t.Fatalf("InputPlan: %v", err)
	}
	if len(plan) != 1 || plan[0].Source() != FromEdge {
		t.Fatalf("edge must win, got %+v", plan)
	}

	// With the edge removed, the param wins over the default.
	g2 := wf().
		taskIO("down", map[string]*nexusv1.ValueSpec{
			"x": {Type: "int", Required: true, DefaultJson: "99"},
		}, nil).
		param("x", vspec("int", true)).build()
	plan2, err := InputPlan(g2, "down")
	if err != nil {
		t.Fatalf("InputPlan: %v", err)
	}
	if plan2[0].Source() != FromParamSrc {
		t.Fatalf("param must beat default, got %+v", plan2)
	}
}

func TestInputPlan_Errors(t *testing.T) {
	g := wf().t("a").build()
	if _, err := InputPlan(g, "ghost"); err == nil {
		t.Error("expected an error for an unknown node")
	}
	// A required input with no source cannot be planned. Validate rejects this,
	// but InputPlan must not panic on a spec that bypassed validation.
	g2 := wf().taskIO("a", map[string]*nexusv1.ValueSpec{"need": vspec("int", true)}, nil).build()
	if _, err := InputPlan(g2, "a"); err == nil {
		t.Error("expected an error for an unsatisfiable required input")
	}
}

// Every input of a VALIDATED spec must be plannable. This is the contract
// between Validate and InputPlan: if validation passed, planning cannot fail.
func TestInputPlan_ValidatedSpecAlwaysPlans(t *testing.T) {
	spec := wf().
		taskIO("a", nil, map[string]*nexusv1.ValueSpec{"o": vspec("file_ref", false)}).
		taskIO("b", map[string]*nexusv1.ValueSpec{
			"i": vspec("file_ref", true), "n": vspecDefault("int", "3"),
		}, map[string]*nexusv1.ValueSpec{"r": vspec("json", false)}).
		taskIO("c", map[string]*nexusv1.ValueSpec{"j": vspec("json", true)}, nil).
		edge("a", "b", map[string]string{"i": "o"}).
		edge("b", "c", map[string]string{"j": "r"}).spec

	g, errs := Validate(spec, DefaultValidationOptions())
	if len(errs) != 0 {
		t.Fatalf("spec should validate: %v", errs)
	}
	for _, id := range g.Nodes() {
		if _, err := InputPlan(g, id); err != nil {
			t.Errorf("validated node %q failed to plan: %v", id, err)
		}
	}
}

// ── Dynamic expansion: DESIGNED, DEFERRED (§5.4 / §11 Phase 3) ────────────────

func TestPlanExpansion_Table(t *testing.T) {
	tests := []struct {
		name      string
		req       ExpansionRequest
		wantN     int
		wantError bool
	}{
		{
			name:  "ordinary fanout",
			req:   ExpansionRequest{TemplateNodeID: "process", Cardinality: 4, MaxFanout: 10},
			wantN: 4,
		},
		{
			name:  "cardinality exactly at the bound is allowed",
			req:   ExpansionRequest{TemplateNodeID: "process", Cardinality: 10, MaxFanout: 10},
			wantN: 10,
		},
		{
			name:  "empty collection expands to nothing",
			req:   ExpansionRequest{TemplateNodeID: "process", Cardinality: 0, MaxFanout: 10},
			wantN: 0,
		},
		{
			// §5.4: an upstream returning ten million items must fail EXPLICITLY
			// rather than silently trying to insert ten million rows.
			name:      "exceeding max_fanout fails explicitly",
			req:       ExpansionRequest{TemplateNodeID: "process", Cardinality: 11, MaxFanout: 10},
			wantError: true,
		},
		{
			name:      "max_fanout is mandatory",
			req:       ExpansionRequest{TemplateNodeID: "process", Cardinality: 3},
			wantError: true,
		},
		{
			name:      "negative cardinality is rejected",
			req:       ExpansionRequest{TemplateNodeID: "process", Cardinality: -1, MaxFanout: 10},
			wantError: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanExpansion(tc.req)
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected an error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tc.wantN {
				t.Fatalf("expanded to %d children, want %d", len(got), tc.wantN)
			}
			for i, c := range got {
				if c.MapIndex != i {
					t.Errorf("child %d has map_index %d", i, c.MapIndex)
				}
				if c.ParentNodeID != tc.req.TemplateNodeID {
					t.Errorf("child %d parent = %q, want %q", i, c.ParentNodeID, tc.req.TemplateNodeID)
				}
			}
		})
	}
}

// Child ids must be DETERMINISTIC: map_index participates in the idempotency
// key, so non-deterministic naming would break at-least-once on retry (§5.4).
func TestPlanExpansion_ChildIDsAreDeterministic(t *testing.T) {
	req := ExpansionRequest{TemplateNodeID: "process", Cardinality: 5, MaxFanout: 100}
	first, err := PlanExpansion(req)
	if err != nil {
		t.Fatalf("PlanExpansion: %v", err)
	}
	for i := 0; i < 10; i++ {
		again, err := PlanExpansion(req)
		if err != nil {
			t.Fatalf("PlanExpansion: %v", err)
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("child %d differs between runs: %+v vs %+v", j, again[j], first[j])
			}
		}
	}
	if first[3].NodeID != "process[3]" {
		t.Errorf("child id = %q, want process[3]", first[3].NodeID)
	}
}
