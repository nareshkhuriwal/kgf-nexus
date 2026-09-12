// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package dag

import (
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Builders that keep the tables below readable. A test table whose rows are
// 40-line protobuf literals is a table nobody reads, and an unread table is an
// unmaintained one.

type tb struct{ spec *nexusv1.WorkflowSpec }

func wf() *tb {
	return &tb{spec: &nexusv1.WorkflowSpec{ApiVersion: "v1", Namespace: "default", Name: "t"}}
}

func (b *tb) failFast(v bool) *tb { b.spec.FailFast = v; return b }

// task adds a node with a trigger rule and no typed interface.
func (b *tb) task(id string, rule nexusv1.TriggerRule) *tb {
	b.spec.Tasks = append(b.spec.Tasks, &nexusv1.TaskSpec{
		Id: id, TaskRef: id + ":v1", Version: 1, TriggerRule: rule,
	})
	return b
}

func (b *tb) t(ids ...string) *tb {
	for _, id := range ids {
		b.task(id, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
	}
	return b
}

// taskIO adds a node with declared inputs/outputs, for interface validation.
func (b *tb) taskIO(id string, in, out map[string]*nexusv1.ValueSpec) *tb {
	b.spec.Tasks = append(b.spec.Tasks, &nexusv1.TaskSpec{
		Id: id, TaskRef: id + ":v1", Version: 1, Inputs: in, Outputs: out,
	})
	return b
}

func (b *tb) edge(from, to string, m map[string]string) *tb {
	b.spec.Edges = append(b.spec.Edges, &nexusv1.Edge{FromNodeId: from, ToNodeId: to, OutputToInput: m})
	return b
}

func (b *tb) e(pairs ...[2]string) *tb {
	for _, p := range pairs {
		b.edge(p[0], p[1], nil)
	}
	return b
}

func (b *tb) param(name string, vs *nexusv1.ValueSpec) *tb {
	if b.spec.Params == nil {
		b.spec.Params = map[string]*nexusv1.ValueSpec{}
	}
	b.spec.Params[name] = vs
	return b
}

func (b *tb) build() *Graph { return Build(b.spec) }

// vspec is a terse ValueSpec literal.
func vspec(typ string, required bool) *nexusv1.ValueSpec {
	return &nexusv1.ValueSpec{Type: typ, Required: required}
}

func vspecDefault(typ, def string) *nexusv1.ValueSpec {
	return &nexusv1.ValueSpec{Type: typ, Required: true, DefaultJson: def}
}

// chain builds a->b->c->... with the given trigger rule on every node.
func chain(rule nexusv1.TriggerRule, ids ...string) *Graph {
	b := wf()
	for _, id := range ids {
		b.task(id, rule)
	}
	for i := 0; i+1 < len(ids); i++ {
		b.edge(ids[i], ids[i+1], nil)
	}
	return b.build()
}

// diamond: a -> {b,c} -> d, with `rule` on d.
func diamond(rule nexusv1.TriggerRule) *Graph {
	return wf().
		t("a", "b", "c").task("d", rule).
		e([2]string{"a", "b"}, [2]string{"a", "c"}, [2]string{"b", "d"}, [2]string{"c", "d"}).
		build()
}

func setEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, v := range a {
		m[v]++
	}
	for _, v := range b {
		m[v]--
		if m[v] < 0 {
			return false
		}
	}
	return true
}
