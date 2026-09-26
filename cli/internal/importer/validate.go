// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"encoding/json"
	"fmt"
	"sort"

	clispec "github.com/khuriwalgroup/kgf-nexus/cli/internal/spec"
)

// validate checks a generated spec against every rule of the gateway's submit
// pipeline that a converter could break: the JSON Schema's patterns, enums,
// ranges and conditionals, and the semantic checks (edge interfaces, required
// inputs, map templates, reachability, Phase-1 trigger rules, timeout
// coherence, stdout_output). A problem here is a CONVERTER bug, never the
// user's: the command refuses to write anything and says so.
//
// Why not import services/api-gateway/spec? nexusctl is standard library plus
// yaml.v3 by design (cli/README.md), and the gateway package pulls in
// protobuf, a JSON-Schema engine and golang.org/x/text. The tests run every
// generated spec through the real schema and the real gateway pipeline, which
// keeps this list honest; `nexusctl workflow submit --dry-run` gives the
// server's verdict before anything is stored.
func validate(s *Spec) []string {
	var p []string
	add := func(format string, a ...any) { p = append(p, fmt.Sprintf(format, a...)) }

	if s.APIVersion != APIVersion {
		add("api_version is %q", s.APIVersion)
	}
	for what, v := range map[string]string{"namespace": s.Namespace, "name": s.Name} {
		if !identifierRe.MatchString(v) || len(v) > 253 {
			add("%s %q does not match the identifier pattern", what, v)
		}
	}
	if len(s.Description) > 4096 {
		add("description is longer than 4096 characters")
	}
	if len(s.Tasks) == 0 {
		add("no tasks")
	}
	if len(s.Tasks) > 1000 {
		add("%d tasks; the schema allows 1000", len(s.Tasks))
	}
	if len(s.Edges) > 10000 {
		add("%d edges; the schema allows 10000", len(s.Edges))
	}
	for k, v := range s.Labels {
		if len(v) > 4096 {
			add("label %s is longer than 4096 characters", k)
		}
	}
	for name, v := range s.Params {
		if !identifierRe.MatchString(name) {
			add("param %q is not an identifier", name)
		}
		checkValue(add, "param "+name, v)
	}
	if s.RunTimeoutMs < 0 || s.RunTimeoutMs > 315360000000 {
		add("run_timeout_ms out of range")
	}

	tasks := map[string]*TaskSpec{}
	for i := range s.Tasks {
		t := &s.Tasks[i]
		if _, dup := tasks[t.ID]; dup {
			add("duplicate task id %q", t.ID)
		}
		tasks[t.ID] = t
		checkTask(add, t, s)
	}

	type ek struct{ f, t string }
	seen := map[ek]bool{}
	preds := map[string][]string{}
	succs := map[string][]string{}
	boundBy := map[string]map[string]string{} // node -> input -> upstream
	for _, e := range s.Edges {
		if !nodeIDRe.MatchString(e.From) || !nodeIDRe.MatchString(e.To) {
			add("edge %s -> %s has an invalid node id", e.From, e.To)
		}
		up, ok1 := tasks[e.From]
		down, ok2 := tasks[e.To]
		if !ok1 || !ok2 {
			add("edge %s -> %s names an unknown task", e.From, e.To)
			continue
		}
		if e.From == e.To {
			add("self edge on %s", e.From)
		}
		if seen[ek{e.From, e.To}] {
			add("duplicate edge %s -> %s", e.From, e.To)
		}
		seen[ek{e.From, e.To}] = true
		preds[e.To] = append(preds[e.To], e.From)
		succs[e.From] = append(succs[e.From], e.To)
		for in, out := range e.OutputToInput {
			if !identifierRe.MatchString(in) || !identifierRe.MatchString(out) {
				add("edge %s -> %s binds %s <- %s: not identifiers", e.From, e.To, in, out)
			}
			iv, okIn := down.Inputs[in]
			ov, okOut := up.Outputs[out]
			if !okIn {
				add("edge %s -> %s binds input %q, which %s does not declare", e.From, e.To, in, e.To)
			}
			if !okOut {
				add("edge %s -> %s reads output %q, which %s does not declare", e.From, e.To, out, e.From)
			}
			if okIn && okOut && up.Map == nil && !assignable(ov.Type, iv.Type) {
				add("edge %s -> %s: %s is %s but %s expects %s", e.From, e.To, out, ov.Type, in, iv.Type)
			}
			if up.Map != nil && okIn && iv.Type != "json" {
				add("%s receives the fan-in of map template %s in input %s, which must be json", e.To, e.From, in)
			}
			if boundBy[e.To] == nil {
				boundBy[e.To] = map[string]string{}
			}
			if prev, dup := boundBy[e.To][in]; dup {
				add("input %s of %s is bound by both %s and %s", in, e.To, prev, e.From)
			}
			boundBy[e.To][in] = e.From
		}
	}

	for _, t := range s.Tasks {
		for _, in := range sortedKeys(t.Inputs) {
			v := t.Inputs[in]
			if !v.Required {
				continue
			}
			if _, ok := boundBy[t.ID][in]; ok {
				continue
			}
			if t.Map != nil && t.Map.ItemInput == in {
				continue
			}
			if v.DefaultJSON != "" {
				continue
			}
			if _, ok := t.InputLiterals[in]; ok {
				continue
			}
			pn := in
			if b := t.InputParams[in]; b != "" {
				pn = b
			}
			if pv, ok := s.Params[pn]; ok {
				if !assignable(pv.Type, v.Type) {
					add("%s: param %s is %s but input %s expects %s", t.ID, pn, pv.Type, in, v.Type)
				}
				continue
			}
			add("%s: required input %s is satisfied by no edge, param, literal or default", t.ID, in)
		}
		for in, pn := range t.InputParams {
			if _, ok := s.Params[pn]; !ok {
				add("%s: input %s is bound to param %s, which is not declared", t.ID, in, pn)
			}
		}
		if m := t.Map; m != nil {
			if m.MaxFanout < 1 || m.MaxFanout > 10000 {
				add("%s: max_fanout %d is outside 1..10000 (the default operator ceiling)", t.ID, m.MaxFanout)
			}
			up, ok := tasks[m.OverNodeID]
			switch {
			case !ok:
				add("%s: map.over_node_id %s is not a task", t.ID, m.OverNodeID)
			case !contains(preds[t.ID], m.OverNodeID):
				add("%s: map.over_node_id %s is not a direct upstream", t.ID, m.OverNodeID)
			default:
				ov, declared := up.Outputs[m.OverOutput]
				if !declared {
					add("%s: %s declares no output %s", t.ID, m.OverNodeID, m.OverOutput)
				} else if up.Map == nil && ov.Type != "json" {
					add("%s: %s.%s is %s; a mapped collection must be json", t.ID, m.OverNodeID, m.OverOutput, ov.Type)
				}
			}
			if _, ok := t.Inputs[m.ItemInput]; !ok {
				add("%s: map.item_input %s is not a declared input", t.ID, m.ItemInput)
			}
			if _, ok := boundBy[t.ID][m.ItemInput]; ok {
				add("%s: map.item_input %s is also bound by an edge", t.ID, m.ItemInput)
			}
			if _, ok := t.InputLiterals[m.ItemInput]; ok {
				add("%s: map.item_input %s is also a literal", t.ID, m.ItemInput)
			}
		}
	}

	if len(s.Edges) > 0 {
		for _, t := range s.Tasks {
			if len(preds[t.ID]) == 0 && len(succs[t.ID]) == 0 {
				add("%s has no edges while the rest of the graph is connected (the gateway rejects it)", t.ID)
			}
		}
	}

	// Cycles and the other document-level checks nexusctl already runs on
	// every spec it submits.
	raw, err := json.Marshal(s)
	if err != nil {
		add("encode: %v", err)
		return p
	}
	doc, err := clispec.Parse(raw, "generated spec")
	if err != nil {
		add("%v", err)
		return p
	}
	for _, pr := range doc.Problems() {
		add("%s", pr.Message)
	}
	sort.Strings(p)
	return p
}

func checkValue(add func(string, ...any), where string, v ValueSpec) {
	switch v.Type {
	case "int", "float", "string", "bool", "json", "file_ref":
	default:
		add("%s: type %q", where, v.Type)
	}
	if v.Required && v.DefaultJSON != "" {
		add("%s: required with a default", where)
	}
	if v.DefaultJSON != "" && !json.Valid([]byte(v.DefaultJSON)) {
		add("%s: default_json is not JSON", where)
	}
	if len(v.DefaultJSON) > 65536 {
		add("%s: default_json too long", where)
	}
}

func checkTask(add func(string, ...any), t *TaskSpec, s *Spec) {
	id := t.ID
	if !nodeIDRe.MatchString(id) || len(id) > 128 {
		add("task id %q does not match the node id pattern", id)
	}
	if !taskRefRe.MatchString(t.TaskRef) || len(t.TaskRef) > 512 {
		add("%s: task_ref %q does not match the pattern", id, t.TaskRef)
	}
	for n, v := range t.Inputs {
		if !identifierRe.MatchString(n) {
			add("%s: input %q is not an identifier", id, n)
		}
		checkValue(add, id+" input "+n, v)
	}
	for n, v := range t.Outputs {
		if !identifierRe.MatchString(n) {
			add("%s: output %q is not an identifier", id, n)
		}
		checkValue(add, id+" output "+n, v)
	}
	for n, v := range t.InputLiterals {
		if !json.Valid([]byte(v)) {
			add("%s: input_literals.%s is not JSON", id, n)
		}
		if _, ok := t.Inputs[n]; !ok {
			add("%s: input_literals.%s binds an undeclared input", id, n)
		}
	}
	if t.Queue != "" && (!queueRe.MatchString(t.Queue) || len(t.Queue) > 128) {
		add("%s: queue %q does not match the pattern", id, t.Queue)
	}
	if t.Priority != nil && (*t.Priority < 0 || *t.Priority > 1000000) {
		add("%s: priority out of range", id)
	}
	switch t.TriggerRule {
	case "", "all_success", "all_done":
	default:
		add("%s: trigger_rule %s is not accepted by the control plane in this release", id, t.TriggerRule)
	}
	if r := t.Retry; r != nil {
		if r.MaxAttempts < 1 || r.MaxAttempts > 100 {
			add("%s: retry.max_attempts %d is outside 1..100", id, r.MaxAttempts)
		}
		if r.Multiplier != 0 && (r.Multiplier < 1 || r.Multiplier > 100) {
			add("%s: retry.multiplier %g is outside 1..100", id, r.Multiplier)
		}
		switch r.Strategy {
		case "", "fixed", "linear", "exponential":
		default:
			add("%s: retry.strategy %q", id, r.Strategy)
		}
		switch r.Jitter {
		case "", "none", "full", "equal":
		default:
			add("%s: retry.jitter %q", id, r.Jitter)
		}
		for _, d := range []int64{r.InitialMs, r.MaxMs} {
			if d < 0 || d > 315360000000 {
				add("%s: retry duration out of range", id)
			}
		}
		seen := map[string]bool{}
		for _, c := range r.RetryOn {
			if seen[c] {
				add("%s: retry_on repeats %s", id, c)
			}
			seen[c] = true
			switch c {
			case "transient", "timeout", "worker_lost", "validation", "permission_denied", "application", "oom",
				"not_found", "infrastructure":
			default:
				add("%s: retry_on %q is not an error class", id, c)
			}
		}
	}
	if to := t.Timeouts; to != nil {
		for _, d := range []int64{to.ScheduleToStartMs, to.StartToCloseMs, to.ScheduleToCloseMs, to.HeartbeatMs} {
			if d < 0 || d > 315360000000 {
				add("%s: timeout out of range", id)
			}
		}
		if to.StartToCloseMs > 0 && to.ScheduleToCloseMs > 0 && to.ScheduleToCloseMs < to.StartToCloseMs {
			add("%s: schedule_to_close_ms < start_to_close_ms", id)
		}
		if to.ScheduleToStartMs > 0 && to.ScheduleToCloseMs > 0 && to.ScheduleToCloseMs < to.ScheduleToStartMs {
			add("%s: schedule_to_close_ms < schedule_to_start_ms", id)
		}
		if to.HeartbeatMs > 0 && to.StartToCloseMs > 0 && to.HeartbeatMs > to.StartToCloseMs {
			add("%s: heartbeat_ms > start_to_close_ms", id)
		}
	}
	if c := t.Cache; c != nil && c.TTLMs < 0 {
		add("%s: cache.ttl_ms negative", id)
	}
	if (t.ConcurrencyKey == "") != (t.ConcurrencyLimit == 0) || t.ConcurrencyLimit < 0 {
		add("%s: concurrency_key and concurrency_limit must be set together", id)
	}
	if len(t.ConcurrencyKey) > 256 {
		add("%s: concurrency_key too long", id)
	}
	if rt := t.Runtime; rt != nil {
		switch rt.Executor {
		case "subprocess":
			if rt.Image != "" {
				add("%s: a subprocess task carries an image", id)
			}
		case "docker", "k8s", "kubernetes":
			if rt.Image == "" {
				add("%s: the %s executor needs an image", id, rt.Executor)
			}
		default:
			add("%s: executor %q", id, rt.Executor)
		}
		if len(rt.Image) > 1024 {
			add("%s: image too long", id)
		}
		if rt.StdoutOutput != "" {
			if _, ok := t.Outputs[rt.StdoutOutput]; !ok {
				add("%s: stdout_output %s is not a declared output", id, rt.StdoutOutput)
			}
		}
		for k, v := range rt.Env {
			if len(v) > 4096 {
				add("%s: env %s is longer than 4096 characters", id, k)
			}
		}
		if len(rt.WorkingDir) > 4096 {
			add("%s: working_dir too long", id)
		}
	}
	if m := t.Map; m != nil {
		if !nodeIDRe.MatchString(m.OverNodeID) || !identifierRe.MatchString(m.OverOutput) ||
			!identifierRe.MatchString(m.ItemInput) {
			add("%s: map fields do not match their patterns", id)
		}
	}
	_ = s
}

// assignable mirrors internal/dag: same type, anything into json, int into float.
func assignable(from, to string) bool {
	return from == to || to == "json" || (from == "int" && to == "float")
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
