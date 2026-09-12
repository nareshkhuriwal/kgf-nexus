package dag

import (
	"strings"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func TestValidate_Table(t *testing.T) {
	tests := []struct {
		name string
		spec *nexusv1.WorkflowSpec
		opts ValidationOptions
		// wantCodes are codes that MUST be present.
		wantCodes []ErrorCode
		// forbidCodes are codes that must NOT be present (guards against
		// over-eager checks firing on valid specs).
		forbidCodes []ErrorCode
		wantClean   bool
	}{
		{
			name:      "empty spec",
			spec:      wf().spec,
			wantCodes: []ErrorCode{ErrEmptySpec},
		},
		{
			name:      "single node, no edges, valid",
			spec:      wf().t("a").spec,
			wantClean: true,
		},
		{
			name:      "simple chain valid",
			spec:      wf().t("a", "b", "c").e([2]string{"a", "b"}, [2]string{"b", "c"}).spec,
			wantClean: true,
		},
		{
			name:      "duplicate node id",
			spec:      wf().t("a", "a").spec,
			wantCodes: []ErrorCode{ErrDuplicateNode},
		},
		{
			name:      "empty node id",
			spec:      wf().t("").spec,
			wantCodes: []ErrorCode{ErrEmptyNodeID},
		},
		{
			name:      "missing task_ref",
			spec:      &nexusv1.WorkflowSpec{Tasks: []*nexusv1.TaskSpec{{Id: "a"}}},
			wantCodes: []ErrorCode{ErrMissingTaskRef},
		},
		{
			name:      "edge to unknown node",
			spec:      wf().t("a").e([2]string{"a", "ghost"}).spec,
			wantCodes: []ErrorCode{ErrUnknownEdgeNode},
		},
		{
			name:      "edge from unknown node",
			spec:      wf().t("a").e([2]string{"ghost", "a"}).spec,
			wantCodes: []ErrorCode{ErrUnknownEdgeNode},
		},
		{
			name:      "self edge",
			spec:      wf().t("a").e([2]string{"a", "a"}).spec,
			wantCodes: []ErrorCode{ErrSelfEdge},
		},
		{
			name:      "duplicate edge",
			spec:      wf().t("a", "b").e([2]string{"a", "b"}, [2]string{"a", "b"}).spec,
			wantCodes: []ErrorCode{ErrDuplicateEdge},
		},
		{
			name:      "two-node cycle",
			spec:      wf().t("a", "b").e([2]string{"a", "b"}, [2]string{"b", "a"}).spec,
			wantCodes: []ErrorCode{ErrCycle},
		},
		{
			name: "three-node cycle",
			spec: wf().t("a", "b", "c").
				e([2]string{"a", "b"}, [2]string{"b", "c"}, [2]string{"c", "a"}).spec,
			wantCodes: []ErrorCode{ErrCycle},
		},
		{
			name: "cycle with a valid tail attached",
			spec: wf().t("a", "b", "c", "d").
				e([2]string{"a", "b"}, [2]string{"b", "c"}, [2]string{"c", "b"}, [2]string{"c", "d"}).spec,
			wantCodes: []ErrorCode{ErrCycle},
		},
		{
			name:      "unreachable node amid a connected workflow",
			spec:      wf().t("a", "b", "orphan").e([2]string{"a", "b"}).spec,
			wantCodes: []ErrorCode{ErrUnreachableNode},
		},
		{
			name:        "all-isolated workflow is not flagged unreachable",
			spec:        wf().t("a", "b", "c").spec,
			wantClean:   true,
			forbidCodes: []ErrorCode{ErrUnreachableNode},
		},
		{
			name:      "unreachable allowed by option",
			spec:      wf().t("a", "b", "orphan").e([2]string{"a", "b"}).spec,
			opts:      ValidationOptions{AllowUnreachable: true},
			wantClean: true,
		},
		{
			name:        "all_done is phase 1",
			spec:        wf().task("a", nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE).spec,
			wantClean:   true,
			forbidCodes: []ErrorCode{ErrTriggerRuleDeferred},
		},
		{
			name:      "one_success deferred in phase 1",
			spec:      wf().task("a", nexusv1.TriggerRule_TRIGGER_RULE_ONE_SUCCESS).spec,
			wantCodes: []ErrorCode{ErrTriggerRuleDeferred},
		},
		{
			name:      "one_success accepted when opted in",
			spec:      wf().task("a", nexusv1.TriggerRule_TRIGGER_RULE_ONE_SUCCESS).spec,
			opts:      ValidationOptions{AllowDeferredTriggerRules: true},
			wantClean: true,
		},
		{
			name:      "unknown value type",
			spec:      wf().taskIO("a", nil, map[string]*nexusv1.ValueSpec{"o": vspec("blob", false)}).spec,
			wantCodes: []ErrorCode{ErrUnknownType},
		},
		{
			name: "invalid default json",
			spec: wf().taskIO("a", map[string]*nexusv1.ValueSpec{
				"i": {Type: "int", Required: true, DefaultJson: "{not json"},
			}, nil).spec,
			wantCodes: []ErrorCode{ErrBadDefault},
		},
		{
			name: "type compatible edge",
			spec: wf().
				taskIO("up", nil, map[string]*nexusv1.ValueSpec{"model": vspec("file_ref", false)}).
				taskIO("down", map[string]*nexusv1.ValueSpec{"dataset": vspec("file_ref", true)}, nil).
				edge("up", "down", map[string]string{"dataset": "model"}).spec,
			wantClean: true,
		},
		{
			name: "type mismatch across edge",
			spec: wf().
				taskIO("up", nil, map[string]*nexusv1.ValueSpec{"n": vspec("string", false)}).
				taskIO("down", map[string]*nexusv1.ValueSpec{"count": vspec("int", true)}, nil).
				edge("up", "down", map[string]string{"count": "n"}).spec,
			wantCodes: []ErrorCode{ErrTypeMismatch},
		},
		{
			name: "int widens to float",
			spec: wf().
				taskIO("up", nil, map[string]*nexusv1.ValueSpec{"n": vspec("int", false)}).
				taskIO("down", map[string]*nexusv1.ValueSpec{"x": vspec("float", true)}, nil).
				edge("up", "down", map[string]string{"x": "n"}).spec,
			wantClean: true,
		},
		{
			name: "anything flows into json",
			spec: wf().
				taskIO("up", nil, map[string]*nexusv1.ValueSpec{"f": vspec("file_ref", false)}).
				taskIO("down", map[string]*nexusv1.ValueSpec{"blob": vspec("json", true)}, nil).
				edge("up", "down", map[string]string{"blob": "f"}).spec,
			wantClean: true,
		},
		{
			name: "json does not silently narrow to int",
			spec: wf().
				taskIO("up", nil, map[string]*nexusv1.ValueSpec{"j": vspec("json", false)}).
				taskIO("down", map[string]*nexusv1.ValueSpec{"n": vspec("int", true)}, nil).
				edge("up", "down", map[string]string{"n": "j"}).spec,
			wantCodes: []ErrorCode{ErrTypeMismatch},
		},
		{
			name: "edge names an output the upstream does not declare",
			spec: wf().
				taskIO("up", nil, map[string]*nexusv1.ValueSpec{"real": vspec("int", false)}).
				taskIO("down", map[string]*nexusv1.ValueSpec{"i": vspec("int", true)}, nil).
				edge("up", "down", map[string]string{"i": "imaginary"}).spec,
			wantCodes: []ErrorCode{ErrUnknownOutput},
		},
		{
			name: "edge names an input the downstream does not declare",
			spec: wf().
				taskIO("up", nil, map[string]*nexusv1.ValueSpec{"o": vspec("int", false)}).
				taskIO("down", map[string]*nexusv1.ValueSpec{"real": vspecDefault("int", "1")}, nil).
				edge("up", "down", map[string]string{"imaginary": "o"}).spec,
			wantCodes: []ErrorCode{ErrUnknownInput},
		},
		{
			name: "required input unsatisfied",
			spec: wf().
				taskIO("a", map[string]*nexusv1.ValueSpec{"needed": vspec("int", true)}, nil).spec,
			wantCodes: []ErrorCode{ErrRequiredInput},
		},
		{
			name: "required input satisfied by default",
			spec: wf().
				taskIO("a", map[string]*nexusv1.ValueSpec{"needed": vspecDefault("int", "10")}, nil).spec,
			wantClean: true,
		},
		{
			name: "required input satisfied by workflow param",
			spec: wf().
				taskIO("a", map[string]*nexusv1.ValueSpec{"needed": vspec("int", true)}, nil).
				param("needed", vspec("int", true)).spec,
			wantClean: true,
		},
		{
			name: "workflow param of the wrong type does not satisfy",
			spec: wf().
				taskIO("a", map[string]*nexusv1.ValueSpec{"needed": vspec("int", true)}, nil).
				param("needed", vspec("string", true)).spec,
			wantCodes: []ErrorCode{ErrTypeMismatch},
		},
		{
			name: "optional unbound input is fine",
			spec: wf().
				taskIO("a", map[string]*nexusv1.ValueSpec{"maybe": vspec("json", false)}, nil).spec,
			wantClean: true,
		},
		{
			name: "input bound twice by two upstreams",
			spec: wf().
				taskIO("u1", nil, map[string]*nexusv1.ValueSpec{"o": vspec("int", false)}).
				taskIO("u2", nil, map[string]*nexusv1.ValueSpec{"o": vspec("int", false)}).
				taskIO("d", map[string]*nexusv1.ValueSpec{"i": vspec("int", true)}, nil).
				edge("u1", "d", map[string]string{"i": "o"}).
				edge("u2", "d", map[string]string{"i": "o"}).spec,
			wantCodes: []ErrorCode{ErrInputDoubleBound},
		},
		{
			name: "one output fanning out to several inputs is legal",
			spec: wf().
				taskIO("u", nil, map[string]*nexusv1.ValueSpec{"o": vspec("int", false)}).
				taskIO("d", map[string]*nexusv1.ValueSpec{
					"i1": vspec("int", true), "i2": vspec("int", true),
				}, nil).
				edge("u", "d", map[string]string{"i1": "o", "i2": "o"}).spec,
			wantClean: true,
		},
		{
			name: "many errors are reported together, not one at a time",
			spec: wf().
				t("a", "a").
				taskIO("b", map[string]*nexusv1.ValueSpec{"needed": vspec("int", true)}, nil).
				e([2]string{"a", "ghost"}, [2]string{"b", "b"}).spec,
			wantCodes: []ErrorCode{ErrDuplicateNode, ErrUnknownEdgeNode, ErrSelfEdge, ErrRequiredInput},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			if opts == (ValidationOptions{}) && tc.opts == (ValidationOptions{}) {
				opts = DefaultValidationOptions()
			}
			_, errs := Validate(tc.spec, opts)
			if tc.wantClean && len(errs) != 0 {
				t.Fatalf("expected a clean spec, got: %v", errs)
			}
			for _, c := range tc.wantCodes {
				if !errs.Has(c) {
					t.Errorf("missing expected code %q; got %v (%v)", c, errs.Codes(), errs)
				}
			}
			for _, c := range tc.forbidCodes {
				if errs.Has(c) {
					t.Errorf("unexpected code %q present: %v", c, errs)
				}
			}
		})
	}
}

// The cycle PATH is the load-bearing part of the error: without it the author
// has to find the loop themselves on a large graph.
func TestValidate_CycleReportsActualPath(t *testing.T) {
	spec := wf().t("a", "b", "c", "d").
		e([2]string{"a", "b"}, [2]string{"b", "c"}, [2]string{"c", "d"}, [2]string{"d", "b"}).spec

	_, errs := Validate(spec, DefaultValidationOptions())
	var cyc *ValidationError
	for i := range errs {
		if errs[i].Code == ErrCycle {
			cyc = &errs[i]
			break
		}
	}
	if cyc == nil {
		t.Fatalf("expected a cycle error, got %v", errs)
	}
	if len(cyc.Path) < 4 {
		t.Fatalf("cycle path too short to be useful: %v", cyc.Path)
	}
	// The reported loop must be closed and must contain exactly the cycle nodes.
	if cyc.Path[0] != cyc.Path[len(cyc.Path)-1] {
		t.Errorf("cycle path is not closed: %v", cyc.Path)
	}
	for _, want := range []string{"b", "c", "d"} {
		if !strings.Contains(strings.Join(cyc.Path, ","), want) {
			t.Errorf("cycle path %v omits %q", cyc.Path, want)
		}
	}
	if strings.Contains(strings.Join(cyc.Path, ","), "a") {
		t.Errorf("cycle path %v wrongly includes acyclic node a", cyc.Path)
	}
	if !strings.Contains(cyc.Error(), "->") {
		t.Errorf("rendered error should show the path: %s", cyc.Error())
	}
}

// Two entry points into the same loop must not produce two errors: duplicate
// reports of one fault make a validation report harder to act on.
func TestValidate_CycleDeduplicated(t *testing.T) {
	spec := wf().t("x", "y", "a", "b").
		e([2]string{"x", "a"}, [2]string{"y", "a"},
			[2]string{"a", "b"}, [2]string{"b", "a"}).spec
	_, errs := Validate(spec, DefaultValidationOptions())
	n := 0
	for _, e := range errs {
		if e.Code == ErrCycle {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 cycle error, got %d: %v", n, errs)
	}
}

func TestValidate_SelfEdgeNotAlsoUnreachable(t *testing.T) {
	spec := wf().t("a", "b").e([2]string{"a", "b"}, [2]string{"a", "a"}).spec
	_, errs := Validate(spec, DefaultValidationOptions())
	if errs.Has(ErrUnreachableNode) {
		t.Errorf("self-edged node should not be reported unreachable: %v", errs)
	}
}

func TestValidationErrors_ErrorAndAsError(t *testing.T) {
	var empty ValidationErrors
	if empty.AsError() != nil {
		t.Error("empty ValidationErrors should collapse to a nil error")
	}
	if !strings.Contains(empty.Error(), "no validation errors") {
		t.Errorf("unexpected empty rendering: %s", empty.Error())
	}
	errs := ValidationErrors{{Code: ErrCycle, NodeID: "a", Path: []string{"a", "a"}}}
	if errs.AsError() == nil {
		t.Error("non-empty ValidationErrors must produce an error")
	}
	if !strings.Contains(errs.Error(), "1 validation error") {
		t.Errorf("unexpected rendering: %s", errs.Error())
	}
}

// Build must be total: it is called by the validator on specs that are known to
// be broken, so it may never panic.
func TestBuild_TotalOnMalformedSpecs(t *testing.T) {
	specs := []*nexusv1.WorkflowSpec{
		nil,
		{},
		wf().t("a", "a").spec,
		wf().e([2]string{"ghost", "phantom"}).spec,
		wf().t("a").e([2]string{"a", "a"}).spec,
		wf().t("a", "b").e([2]string{"a", "b"}, [2]string{"b", "a"}).spec,
	}
	for i, s := range specs {
		g := Build(s)
		if g == nil {
			t.Fatalf("spec %d: Build returned nil", i)
		}
		// Nodes() must stay deterministic and total even for a cyclic graph.
		if got := len(g.Nodes()); got != len(g.nodes) {
			t.Errorf("spec %d: Nodes() length %d != node count %d", i, got, len(g.nodes))
		}
		_ = g.String()
	}
}

func TestBuild_TopologicalOrderIsDeterministic(t *testing.T) {
	g1 := wf().t("z", "m", "a").e([2]string{"z", "m"}, [2]string{"m", "a"}).build()
	want := []string{"z", "m", "a"}
	for i, id := range g1.Nodes() {
		if id != want[i] {
			t.Fatalf("topological order %v, want %v", g1.Nodes(), want)
		}
	}
	// Independent nodes must be lexicographic, not map-order.
	g2 := wf().t("c", "a", "b").build()
	if got := g2.Nodes(); got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("independent nodes should be lexicographic, got %v", got)
	}
}
