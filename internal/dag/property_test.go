// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package dag

import (
	"fmt"
	"math/rand"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// ── Property-based tests ──────────────────────────────────────────────────────
//
// Architecture §3.2 states the engine "can be property-tested against invariants
// (no node ever runs twice, no node runs before its dependencies, every run
// reaches a terminal state)". Those are the properties asserted here, plus the
// transitive-propagation property that the SKIPPED/UPSTREAM_FAILED distinction
// depends on.
//
// The generator emits random DAGs by construction rather than random graphs
// filtered for acyclicity: nodes are indexed and edges only ever run from a lower
// index to a higher one, so acyclicity is structural and every generated case is
// a valid input. Filtering would silently skew the distribution towards small
// sparse graphs, testing the interesting shapes least.
//
// Seeds are fixed and derived from the case number so a failure is exactly
// reproducible from the reported seed; a property test you cannot replay is a
// property test you cannot debug.

const propertyCases = 400

type genDAG struct {
	graph *Graph
	seed  int64
	desc  string
}

func genRandomDAG(seed int64) genDAG {
	rng := rand.New(rand.NewSource(seed))
	n := 1 + rng.Intn(9) // 1..9 nodes
	rules := []nexusv1.TriggerRule{
		nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS,
		nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE,
		nexusv1.TriggerRule_TRIGGER_RULE_ONE_SUCCESS,
		nexusv1.TriggerRule_TRIGGER_RULE_ONE_FAILED,
		nexusv1.TriggerRule_TRIGGER_RULE_NONE_FAILED,
		nexusv1.TriggerRule_TRIGGER_RULE_ALL_SKIPPED,
	}

	b := wf().failFast(rng.Intn(4) == 0)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = fmt.Sprintf("n%02d", i)
		b.task(ids[i], rules[rng.Intn(len(rules))])
	}
	// Only i<j edges: acyclicity by construction.
	density := 0.15 + rng.Float64()*0.5
	edges := 0
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if rng.Float64() < density {
				b.edge(ids[i], ids[j], nil)
				edges++
			}
		}
	}
	return genDAG{
		graph: b.build(),
		seed:  seed,
		desc:  fmt.Sprintf("seed=%d nodes=%d edges=%d failFast=%v", seed, n, edges, b.spec.GetFailFast()),
	}
}

// outcome models what a worker eventually reports for a dispatched node. The
// simulator picks one at random so the properties are exercised across success,
// failure and cancellation, not just the happy path.
func randomOutcome(rng *rand.Rand) NodeState {
	switch rng.Intn(10) {
	case 0, 1:
		return NodeFailed
	case 2:
		return NodeCancelled
	default:
		return NodeSucceeded
	}
}

// simulate drives a generated DAG to a terminal run state through the real
// Reconcile loop, recording everything the invariants need to check.
type simResult struct {
	dispatchCount map[string]int
	final         States
	runState      RunState
	iterations    int
	// dispatchOrder records, for each dispatch, the states of its upstreams AT
	// the moment of dispatch, so the dependency-ordering property can be checked
	// rather than merely assumed.
	preStateAtDispatch []map[string]NodeState
	dispatchedNode     []string
}

func simulate(t *testing.T, g *Graph, seed int64) simResult {
	rng := rand.New(rand.NewSource(seed ^ 0x5eed))
	st := States{}
	rs := RunRunning
	res := simResult{dispatchCount: map[string]int{}}

	// The bound is generous: a correct engine settles an n-node graph in at most
	// n dispatch rounds, so exceeding 4n+16 means the run is not progressing,
	// which is itself the failure the "every run terminates" property asserts.
	maxIter := 4*len(g.Nodes()) + 16
	for i := 0; i < maxIter; i++ {
		res.iterations = i + 1
		d := Reconcile(ReconcileInput{Graph: g, States: st, RunState: rs})

		// No node may appear twice in a single frontier.
		seen := map[string]bool{}
		for _, id := range d.Frontier {
			if seen[id] {
				t.Fatalf("seed=%d: node %q appears twice in one frontier %v", seed, id, d.Frontier)
			}
			seen[id] = true
		}

		// The snapshot must be taken against the POST-propagation view, because
		// that is the view Reconcile itself decided the frontier against
		// (propagation runs first, by design -- see Reconcile's ordering note).
		// Snapshotting the pre-propagation states would compare the engine's
		// decision against inputs it never saw.
		postProp := ApplyTransitions(st, d.Transitions)
		for _, id := range d.Frontier {
			snap := map[string]NodeState{}
			for _, p := range g.Preds(id) {
				snap[p] = postProp.Get(p)
			}
			res.preStateAtDispatch = append(res.preStateAtDispatch, snap)
			res.dispatchedNode = append(res.dispatchedNode, id)
			res.dispatchCount[id]++
		}

		st = postProp
		rs = d.RunState

		// Settle the freshly dispatched nodes, as the scheduler and a worker
		// eventually would.
		for _, id := range d.Frontier {
			st[id] = randomOutcome(rng)
		}

		if rs == RunSucceeded || rs == RunFailed || rs == RunCancelled {
			res.final, res.runState = st, rs
			return res
		}
		if len(d.Frontier) == 0 && !d.Halted && allTerminal(g, st) {
			// Nothing left to do and not terminal: let the next iteration conclude.
			continue
		}
	}
	res.final, res.runState = st, rs
	return res
}

// PROPERTY: every run reaches a terminal state. A run that can hang is the
// single worst failure mode an orchestrator can have, because it consumes a
// concurrency slot forever and no alert fires.
func TestProperty_EveryRunReachesATerminalState(t *testing.T) {
	for c := 0; c < propertyCases; c++ {
		seed := int64(c) + 1
		gd := genRandomDAG(seed)
		res := simulate(t, gd.graph, seed)

		terminal := res.runState == RunSucceeded || res.runState == RunFailed || res.runState == RunCancelled
		if !terminal {
			t.Fatalf("%s: run did not terminate after %d iterations (state=%s final=%v)",
				gd.desc, res.iterations, res.runState, res.final)
		}
		// Terminal run implies every node settled. A terminal run with an
		// unsettled node would mean we declared a verdict on work still in flight.
		for _, id := range gd.graph.Nodes() {
			if !res.final.Get(id).Terminal() {
				t.Fatalf("%s: run terminal (%s) but node %q is %s",
					gd.desc, res.runState, id, res.final.Get(id))
			}
		}
	}
}

// PROPERTY: no node is ever dispatched before its dependencies are satisfied.
// This is checked against the trigger rule actually declared on the node, using
// the upstream snapshot captured at dispatch time.
func TestProperty_NoNodeReadyBeforeItsDependenciesAreSatisfied(t *testing.T) {
	for c := 0; c < propertyCases; c++ {
		seed := int64(c) + 1
		gd := genRandomDAG(seed)
		res := simulate(t, gd.graph, seed)

		for i, id := range res.dispatchedNode {
			snap := res.preStateAtDispatch[i]
			var tl upstreamTally
			for _, s := range snap {
				tl.total++
				if s.Terminal() {
					tl.terminal++
				}
				switch s {
				case NodeSucceeded:
					tl.succeeded++
				case NodeFailed, NodeUpstreamFailed:
					tl.failed++
				case NodeSkipped:
					tl.skipped++
				case NodeCancelled:
					tl.cancelled++
				}
			}
			rule := gd.graph.Node(id).TriggerRule
			if got := evalTrigger(rule, tl); got != triggerFire {
				t.Fatalf("%s: node %q (rule=%s) dispatched while its trigger was %v; upstreams=%v",
					gd.desc, id, rule, got, snap)
			}
			// The strongest and most common case, stated separately because it is
			// the invariant most people mean by "dependencies satisfied".
			if rule == AllSuccess {
				for p, s := range snap {
					if s != NodeSucceeded {
						t.Fatalf("%s: all_success node %q dispatched while upstream %q was %s",
							gd.desc, id, p, s)
					}
				}
			}
		}
	}
}

// PROPERTY: no node runs twice. A node dispatched twice means duplicate work,
// duplicate side effects, and a broken idempotency story.
func TestProperty_NoNodeIsDispatchedTwice(t *testing.T) {
	for c := 0; c < propertyCases; c++ {
		seed := int64(c) + 1
		gd := genRandomDAG(seed)
		res := simulate(t, gd.graph, seed)
		for id, n := range res.dispatchCount {
			if n != 1 {
				t.Fatalf("%s: node %q dispatched %d times, want at most 1", gd.desc, id, n)
			}
		}
	}
}

// PROPERTY: no node appears twice within a single frontier. Checked inside
// simulate for every iteration; restated here directly against ReadyFrontier over
// arbitrary (not just reachable) state assignments, which covers states the
// simulator would never produce.
func TestProperty_FrontierIsAlwaysDuplicateFree(t *testing.T) {
	allStates := []NodeState{
		NodePending, NodeReady, NodeRunning, NodeSucceeded,
		NodeFailed, NodeSkipped, NodeUpstreamFailed, NodeCancelled,
	}
	for c := 0; c < propertyCases; c++ {
		seed := int64(c) + 1
		gd := genRandomDAG(seed)
		rng := rand.New(rand.NewSource(seed ^ 0xf00d))

		for trial := 0; trial < 12; trial++ {
			st := States{}
			for _, id := range gd.graph.Nodes() {
				st[id] = allStates[rng.Intn(len(allStates))]
			}
			f := ReadyFrontier(gd.graph, st)
			seen := map[string]bool{}
			for _, id := range f {
				if seen[id] {
					t.Fatalf("%s: duplicate %q in frontier %v (states=%v)", gd.desc, id, f, st)
				}
				seen[id] = true
				// A frontier entry must be a real node in PENDING; anything else
				// means we are about to dispatch something already accounted for.
				if gd.graph.Node(id) == nil {
					t.Fatalf("%s: frontier names unknown node %q", gd.desc, id)
				}
				if st.Get(id) != NodePending {
					t.Fatalf("%s: frontier names %q which is %s, not PENDING", gd.desc, id, st.Get(id))
				}
			}
		}
	}
}

// PROPERTY: SKIPPED and UPSTREAM_FAILED propagate TRANSITIVELY to all
// descendants. Specifically: once propagation reaches a fixed point, no PENDING
// node may have an unsatisfiable trigger — if one did, it would sit PENDING
// forever and the run would never terminate.
func TestProperty_PropagationReachesAFixedPointWithNoStuckNodes(t *testing.T) {
	for c := 0; c < propertyCases; c++ {
		seed := int64(c) + 1
		gd := genRandomDAG(seed)
		rng := rand.New(rand.NewSource(seed ^ 0xbeef))

		for trial := 0; trial < 12; trial++ {
			// Seed a random subset of nodes with terminal outcomes, leaving the
			// rest PENDING, then propagate.
			st := States{}
			for _, id := range gd.graph.Nodes() {
				switch rng.Intn(4) {
				case 0:
					st[id] = NodeSucceeded
				case 1:
					st[id] = NodeFailed
				case 2:
					st[id] = NodeSkipped
				}
			}
			after := ApplyTransitions(st, Propagate(gd.graph, st))

			// Fixed point: propagating again changes nothing.
			if extra := Propagate(gd.graph, after); len(extra) != 0 {
				t.Fatalf("%s: Propagate is not idempotent; second pass produced %v", gd.desc, extra)
			}
			for _, id := range gd.graph.Nodes() {
				if after.Get(id) != NodePending {
					continue
				}
				if got := evalTrigger(gd.graph.Node(id).TriggerRule, tally(gd.graph, id, after)); got == triggerUnsatisfiable {
					t.Fatalf("%s: node %q left PENDING with an unsatisfiable trigger; it would hang forever (states=%v)",
						gd.desc, id, after)
				}
			}
		}
	}
}

// PROPERTY (the distinction that makes failures diagnosable): every descendant of
// a failed node, along an all_success-only path, ends UPSTREAM_FAILED and never
// FAILED. FAILED must mean "this node ran and broke" exclusively.
func TestProperty_FailureMarksDescendantsUpstreamFailedNotFailed(t *testing.T) {
	for c := 0; c < propertyCases; c++ {
		seed := int64(c) + 1
		rng := rand.New(rand.NewSource(seed))
		n := 2 + rng.Intn(8)

		// all_success everywhere, so propagation is unambiguous.
		b := wf()
		ids := make([]string, n)
		for i := 0; i < n; i++ {
			ids[i] = fmt.Sprintf("n%02d", i)
			b.task(ids[i], nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
		}
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				if rng.Float64() < 0.4 {
					b.edge(ids[i], ids[j], nil)
				}
			}
		}
		g := b.build()

		// Fail exactly one node; everything else that can succeed, does.
		victim := ids[rng.Intn(n)]
		st := States{}
		for _, id := range g.Nodes() {
			if id == victim {
				st[id] = NodeFailed
			} else if len(g.Preds(id)) == 0 || id < victim {
				st[id] = NodeSucceeded
			}
		}
		// Drive to a fixed point through the real loop.
		rs := RunRunning
		for i := 0; i < 4*n+16; i++ {
			d := Reconcile(ReconcileInput{Graph: g, States: st, RunState: rs})
			st = ApplyTransitions(st, d.Transitions)
			rs = d.RunState
			if rs != RunRunning {
				break
			}
			if len(d.Frontier) == 0 {
				break
			}
			for _, id := range d.Frontier {
				st[id] = NodeSucceeded
			}
		}

		// Every node reachable from the victim must be UPSTREAM_FAILED, never
		// FAILED: exactly one node in the run actually broke.
		desc := descendants(g, victim)
		for id := range desc {
			got := st.Get(id)
			if got == NodeFailed {
				t.Fatalf("seed=%d: descendant %q of failed %q is FAILED; only the node that ran and broke may be FAILED",
					seed, id, victim)
			}
			if got != NodeUpstreamFailed && got != NodePending {
				t.Fatalf("seed=%d: descendant %q of failed %q is %s, want UPSTREAM_FAILED",
					seed, id, victim, got)
			}
		}
		failedCount := 0
		for _, id := range g.Nodes() {
			if st.Get(id) == NodeFailed {
				failedCount++
			}
		}
		if failedCount != 1 {
			t.Fatalf("seed=%d: %d nodes FAILED, want exactly 1 (%s)", seed, failedCount, victim)
		}
	}
}

// PROPERTY: the run's verdict agrees with §5.2 given the final states. This
// re-derives the rule independently of RunOutcome rather than calling it, so the
// test is a specification and not a tautology.
func TestProperty_TerminalRunStateMatchesArchitecture52(t *testing.T) {
	for c := 0; c < propertyCases; c++ {
		seed := int64(c) + 1
		gd := genRandomDAG(seed)
		res := simulate(t, gd.graph, seed)

		everyTerminal, anyFail, anyCancel := true, false, false
		for _, id := range gd.graph.Nodes() {
			s := res.final.Get(id)
			if !s.Terminal() {
				everyTerminal = false
			}
			if s == NodeFailed || s == NodeUpstreamFailed {
				anyFail = true
			}
			if s == NodeCancelled {
				anyCancel = true
			}
		}
		var want RunState
		switch {
		case anyFail && everyTerminal:
			want = RunFailed
		case !everyTerminal:
			want = RunRunning
		case anyCancel:
			want = RunCancelled
		default:
			want = RunSucceeded
		}
		if res.runState != want {
			t.Fatalf("%s: run state %s, want %s per §5.2 (final=%v)",
				gd.desc, res.runState, want, res.final)
		}
		// The §5.2 SUCCEEDED clause, restated directly.
		if res.runState == RunSucceeded {
			for _, id := range gd.graph.Nodes() {
				s := res.final.Get(id)
				if !s.Terminal() {
					t.Fatalf("%s: SUCCEEDED with non-terminal node %q (%s)", gd.desc, id, s)
				}
				if s == NodeFailed || s == NodeUpstreamFailed {
					t.Fatalf("%s: SUCCEEDED with node %q in %s", gd.desc, id, s)
				}
			}
		}
	}
}

// PROPERTY: fail_fast never dispatches new work after a terminal failure exists.
func TestProperty_FailFastNeverDispatchesAfterAFailure(t *testing.T) {
	for c := 0; c < propertyCases; c++ {
		seed := int64(c) + 1
		rng := rand.New(rand.NewSource(seed ^ 0xfa17))
		n := 2 + rng.Intn(8)
		b := wf().failFast(true)
		ids := make([]string, n)
		for i := 0; i < n; i++ {
			ids[i] = fmt.Sprintf("n%02d", i)
			b.task(ids[i], nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
		}
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				if rng.Float64() < 0.3 {
					b.edge(ids[i], ids[j], nil)
				}
			}
		}
		g := b.build()

		st := States{}
		rs := RunRunning
		sawFailure := false
		for i := 0; i < 4*n+16; i++ {
			d := Reconcile(ReconcileInput{Graph: g, States: st, RunState: rs})
			if sawFailure && len(d.Frontier) != 0 {
				t.Fatalf("seed=%d: fail_fast dispatched %v after a terminal failure (states=%v)",
					seed, d.Frontier, st)
			}
			st = ApplyTransitions(st, d.Transitions)
			rs = d.RunState
			if rs != RunRunning {
				break
			}
			if len(d.Frontier) == 0 {
				break
			}
			for _, id := range d.Frontier {
				if rng.Intn(5) == 0 {
					st[id] = NodeFailed
					sawFailure = true
				} else {
					st[id] = NodeSucceeded
				}
			}
		}
	}
}

// PROPERTY: Reconcile never mutates its inputs, on arbitrary graphs and states.
// Purity is the premise of every test above, so it is asserted directly.
func TestProperty_ReconcileIsPure(t *testing.T) {
	allStates := []NodeState{
		NodePending, NodeReady, NodeRunning, NodeSucceeded,
		NodeFailed, NodeSkipped, NodeUpstreamFailed, NodeCancelled,
	}
	for c := 0; c < propertyCases; c++ {
		seed := int64(c) + 1
		gd := genRandomDAG(seed)
		rng := rand.New(rand.NewSource(seed ^ 0x9a5e))

		st := States{}
		for _, id := range gd.graph.Nodes() {
			st[id] = allStates[rng.Intn(len(allStates))]
		}
		snapshot := map[string]NodeState{}
		for k, v := range st {
			snapshot[k] = v
		}
		nodesBefore := fmt.Sprint(gd.graph.Nodes())
		edgesBefore := fmt.Sprint(gd.graph.Edges())

		first := fmt.Sprint(Reconcile(ReconcileInput{Graph: gd.graph, States: st, RunState: RunRunning}))

		for k, v := range snapshot {
			if st[k] != v {
				t.Fatalf("%s: Reconcile mutated input state %q: %s -> %s", gd.desc, k, v, st[k])
			}
		}
		if len(st) != len(snapshot) {
			t.Fatalf("%s: Reconcile added keys to the input state map", gd.desc)
		}
		if got := fmt.Sprint(gd.graph.Nodes()); got != nodesBefore {
			t.Fatalf("%s: Reconcile mutated the graph's nodes", gd.desc)
		}
		if got := fmt.Sprint(gd.graph.Edges()); got != edgesBefore {
			t.Fatalf("%s: Reconcile mutated the graph's edges", gd.desc)
		}
		// Same input, same output — repeatedly.
		for i := 0; i < 5; i++ {
			if got := fmt.Sprint(Reconcile(ReconcileInput{Graph: gd.graph, States: st, RunState: RunRunning})); got != first {
				t.Fatalf("%s: Reconcile is not deterministic", gd.desc)
			}
		}
	}
}

// PROPERTY: a randomly generated DAG is always acyclic by construction, so the
// validator must never report a cycle in one. This guards the validator against
// false positives, which would reject legitimate workflows.
func TestProperty_ValidatorFindsNoCycleInAGeneratedDAG(t *testing.T) {
	for c := 0; c < propertyCases; c++ {
		seed := int64(c) + 1
		rng := rand.New(rand.NewSource(seed))
		n := 1 + rng.Intn(9)
		b := wf()
		ids := make([]string, n)
		for i := 0; i < n; i++ {
			ids[i] = fmt.Sprintf("n%02d", i)
			b.task(ids[i], nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
		}
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				if rng.Float64() < 0.4 {
					b.edge(ids[i], ids[j], nil)
				}
			}
		}
		_, errs := Validate(b.spec, ValidationOptions{AllowUnreachable: true})
		if errs.Has(ErrCycle) {
			t.Fatalf("seed=%d: validator reported a cycle in an acyclic-by-construction DAG: %v", seed, errs)
		}
		// Topological order must cover every node when the graph is acyclic.
		g := b.build()
		if len(g.Nodes()) != n {
			t.Fatalf("seed=%d: topological order covers %d of %d nodes", seed, len(g.Nodes()), n)
		}
		pos := map[string]int{}
		for i, id := range g.Nodes() {
			pos[id] = i
		}
		for _, e := range g.Edges() {
			if pos[e.From] >= pos[e.To] {
				t.Fatalf("seed=%d: edge %s->%s violates topological order %v", seed, e.From, e.To, g.Nodes())
			}
		}
	}
}

// PROPERTY: injecting a back edge into a generated DAG must ALWAYS be caught, and
// the reported path must be a genuine cycle in the graph. A validator that misses
// a cycle admits a workflow that can never complete.
func TestProperty_InjectedCycleIsAlwaysCaughtWithAValidPath(t *testing.T) {
	for c := 0; c < propertyCases; c++ {
		seed := int64(c) + 1
		rng := rand.New(rand.NewSource(seed ^ 0xc7c1e))
		n := 2 + rng.Intn(8)
		b := wf()
		ids := make([]string, n)
		for i := 0; i < n; i++ {
			ids[i] = fmt.Sprintf("n%02d", i)
			b.task(ids[i], nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
		}
		// A guaranteed path 0->1->...->n-1, so a back edge always closes a loop.
		for i := 0; i+1 < n; i++ {
			b.edge(ids[i], ids[i+1], nil)
		}
		hi := 1 + rng.Intn(n-1)
		lo := rng.Intn(hi)
		b.edge(ids[hi], ids[lo], nil) // the back edge

		g := b.build()
		_, errs := Validate(b.spec, ValidationOptions{AllowUnreachable: true})
		if !errs.Has(ErrCycle) {
			t.Fatalf("seed=%d: back edge %s->%s not detected: %v", seed, ids[hi], ids[lo], errs)
		}
		for _, e := range errs {
			if e.Code != ErrCycle {
				continue
			}
			if len(e.Path) < 2 {
				t.Fatalf("seed=%d: cycle path %v is too short to be actionable", seed, e.Path)
			}
			if e.Path[0] != e.Path[len(e.Path)-1] {
				t.Fatalf("seed=%d: cycle path %v is not closed", seed, e.Path)
			}
			// Every consecutive pair in the reported path must be a real edge.
			edgeSet := map[[2]string]bool{}
			for _, ed := range g.Edges() {
				edgeSet[[2]string{ed.From, ed.To}] = true
			}
			for i := 0; i+1 < len(e.Path); i++ {
				if !edgeSet[[2]string{e.Path[i], e.Path[i+1]}] {
					t.Fatalf("seed=%d: reported path %v contains non-edge %s->%s",
						seed, e.Path, e.Path[i], e.Path[i+1])
				}
			}
		}
	}
}

// descendants returns every node reachable from start, excluding start.
func descendants(g *Graph, start string) map[string]bool {
	out := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		for _, s := range g.Succs(n) {
			if !out[s] {
				out[s] = true
				walk(s)
			}
		}
	}
	walk(start)
	return out
}
