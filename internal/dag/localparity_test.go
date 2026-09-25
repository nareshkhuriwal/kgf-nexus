// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package dag_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/dag"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"google.golang.org/protobuf/encoding/protojson"
)

// The local runner (sdk/python/nexus/local.py, ADR-0022) re-implements this
// package's decisions in Python. These fixtures are executed by BOTH engines --
// here, and by sdk/python/tests/test_local_parity.py -- so a change to either
// that alters an outcome fails a test on the side that moved.

type parityFile struct {
	Scenarios []parityScenario `json:"scenarios"`
}

type parityScenario struct {
	Name        string              `json:"name"`
	Spec        json.RawMessage     `json:"spec"`
	Script      map[string][]string `json:"script"`
	Collections map[string]int      `json:"collections"`
	Expect      struct {
		Run      string            `json:"run"`
		Nodes    map[string]string `json:"nodes"`
		Attempts map[string]int    `json:"attempts"`
		Children map[string]int    `json:"children"`
	} `json:"expect"`
}

func TestLocalRunnerParityFixtures(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "sdk", "python", "tests", "parity", "scenarios.json"))
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var f parityFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode fixtures: %v", err)
	}
	if len(f.Scenarios) == 0 {
		t.Fatal("no scenarios")
	}
	for _, sc := range f.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			run, states, attempts, children := simulate(t, sc)
			if string(run) != sc.Expect.Run {
				t.Errorf("run state = %s, want %s (nodes: %v)", run, sc.Expect.Run, states)
			}
			for id, want := range sc.Expect.Nodes {
				if got := states.Get(id); string(got) != want {
					t.Errorf("node %s = %s, want %s", id, got, want)
				}
			}
			for id, want := range sc.Expect.Attempts {
				if attempts[id] != want {
					t.Errorf("node %s attempts = %d, want %d", id, attempts[id], want)
				}
			}
			for id, want := range sc.Expect.Children {
				if children[id] != want {
					t.Errorf("template %s children = %d, want %d", id, children[id], want)
				}
			}
		})
	}
}

// simulate drives the pure engine in waves: reconcile, expand, run the whole
// frontier to completion (retrying per domain.ShouldRetry), repeat.
func simulate(t *testing.T, sc parityScenario) (dag.RunState, dag.States, map[string]int, map[string]int) {
	t.Helper()
	spec := &nexusv1.WorkflowSpec{}
	if err := protojson.Unmarshal(sc.Spec, spec); err != nil {
		t.Fatalf("spec: %v", err)
	}
	refs := map[string]dag.ChildRef{}
	g := dag.BuildWithChildren(spec, refs)
	st := dag.States{}
	attempts := map[string]int{}
	children := map[string]int{}
	for wave := 0; wave < 100; wave++ {
		d := dag.Reconcile(dag.ReconcileInput{Graph: g, States: st, RunState: dag.RunRunning})
		st = dag.ApplyTransitions(st, d.Transitions)
		progressed := false
		for _, tmpl := range d.Expansions {
			progressed = true
			n := g.Node(tmpl)
			m := n.Spec.GetMap()
			count := sc.Collections[m.GetOverNodeId()]
			items := make([]int, count)
			for i := range items {
				items[i] = i
			}
			coll, _ := json.Marshal(items)
			ref, _ := dag.InlineJSONRef(coll)
			planned, err := dag.PlanFromCollection(tmpl, ref, int(m.GetMaxFanout()))
			if err != nil {
				st[tmpl] = dag.NodeFailed
				continue
			}
			children[tmpl] = len(planned)
			for _, pc := range planned {
				spec.Tasks = append(spec.Tasks, dag.ChildSpec(n.Spec, pc.NodeID))
				for _, e := range dag.ChildEdges(g, tmpl, pc.NodeID) {
					spec.Edges = append(spec.Edges, &nexusv1.Edge{FromNodeId: e.From, ToNodeId: e.To, OutputToInput: e.OutputToInput})
				}
				refs[pc.NodeID] = dag.ChildRef{Parent: tmpl, MapIndex: pc.MapIndex}
			}
			g = dag.BuildWithChildren(spec, refs)
			st[tmpl] = dag.NodeRunning
		}
		if progressed {
			continue
		}
		frontier := append([]string(nil), d.Frontier...)
		sort.Strings(frontier)
		for _, id := range frontier {
			progressed = true
			n := g.Node(id)
			script := sc.Script[id]
			for number := int32(1); ; number++ {
				attempts[id]++
				outcome := "ok"
				if int(number) <= len(script) {
					outcome = script[number-1]
				}
				if outcome == "ok" {
					st[id] = dag.NodeSucceeded
					break
				}
				class := nexusv1.ErrorClass(nexusv1.ErrorClass_value[outcome])
				idem := n.Spec.GetIdempotency()
				if idem == nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_UNSPECIFIED {
					idem = nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED
				}
				dec := domain.ShouldRetry(class, idem, number, domain.MaxAttempts(n.Spec.GetRetry()), false, n.Spec.GetRetry())
				if !dec.Retry {
					st[id] = dag.NodeFailed
					break
				}
			}
		}
		if !progressed {
			switch d.RunState {
			case dag.RunSucceeded, dag.RunFailed, dag.RunCancelled:
				return d.RunState, st, attempts, children
			}
			t.Fatalf("stalled in %s: %v", d.RunState, st)
		}
	}
	t.Fatal(fmt.Sprintf("no terminal state after 100 waves: %v", st))
	return "", nil, nil, nil
}
