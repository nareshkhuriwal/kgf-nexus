// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// APIVersion is the only spec grammar there is.
const APIVersion = "nexus.dev/v1"

// DefaultMaxFanout mirrors Airflow's default max_map_length (1024): the bound a
// mapped Airflow task already had. Argo and Prefect maps have none, so the
// report says which value was chosen.
const DefaultMaxFanout = 1024

// placeholderScript is what an unsupported construct becomes: a task that
// FAILS, with the reason in its log. A placeholder that succeeded would let a
// partial spec run green while skipping work, which is the one outcome a
// converter must never produce.
const placeholderScript = `echo "nexus import: this task stands in for a construct that was not converted: $NEXUS_IMPORT_UNSUPPORTED" >&2; exit 1`

var (
	nodeIDRe     = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
	identifierRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$`)
	taskRefRe    = regexp.MustCompile(`^[A-Za-z0-9][-A-Za-z0-9_./]*(:[A-Za-z0-9][-A-Za-z0-9_.]*)?$`)
	queueRe      = identifierRe
)

// sanitizeNodeID maps a source task id onto the schema's nodeId pattern.
func sanitizeNodeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.TrimFunc(b.String(), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
	if out == "" {
		out = "task"
	}
	if len(out) > 120 {
		out = strings.TrimRight(out[:120], "-_.")
	}
	return out
}

// sanitizeIdentifier maps a name onto the schema's lowercase identifier pattern.
func sanitizeIdentifier(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		default:
			b.WriteByte('_')
		}
	}
	out := strings.TrimFunc(b.String(), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	if out == "" {
		out = "x"
	}
	if len(out) > 253 {
		out = strings.TrimRight(out[:253], "-_.")
	}
	return out
}

func sanitizeTaskRef(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == '/':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.TrimLeft(b.String(), "-_./")
	if out == "" {
		out = "task"
	}
	if len(out) > 500 {
		out = out[:500]
	}
	return out
}

// BuildOptions controls the IR -> spec step.
type BuildOptions struct {
	Source    string // airflow | prefect | argo
	Namespace string
	MaxFanout int
}

// built is one converted workflow.
type built struct {
	IR       *IRWorkflow
	Spec     *Spec
	Findings []Finding
	// IDs maps source task id -> node id, for the report.
	IDs map[string]string
}

// build turns one IR workflow into a spec. It never fails: problems are
// findings, and validation of the result happens afterwards (validate.go).
func build(ir *IRWorkflow, opts BuildOptions) *built {
	b := &built{IR: ir, IDs: map[string]string{}}
	find := func(line int, construct, status, msg, nexus string) {
		b.Findings = append(b.Findings, Finding{File: ir.File, Line: line, Construct: construct, Status: status,
			Message: msg, Nexus: nexus})
	}
	spec := &Spec{APIVersion: APIVersion, Namespace: opts.Namespace, Name: sanitizeIdentifier(ir.SourceID)}
	if spec.Name != ir.SourceID {
		find(ir.Line, "name", Translated, fmt.Sprintf("%s is named %s (the schema allows lowercase letters, digits, "+
			"'-', '_' and '.')", ir.SourceID, spec.Name), "name")
	}
	if d := strings.TrimSpace(ir.Description); d != "" {
		if len(d) > 4096 {
			d = d[:4093] + "..."
		}
		spec.Description = d
	}
	spec.Labels = map[string]string{"imported-from": opts.Source, "source-id": truncate(ir.SourceID, 4096)}
	for k, v := range ir.Labels {
		spec.Labels[k] = truncate(v, 4096)
	}
	if ir.RunTimeoutMs != nil && *ir.RunTimeoutMs > 0 {
		spec.RunTimeoutMs = *ir.RunTimeoutMs
	}
	if ir.MaxActiveRuns != nil && *ir.MaxActiveRuns > 0 {
		spec.MaxActiveRuns = *ir.MaxActiveRuns
	}
	if ir.FailFast != nil {
		spec.FailFast = *ir.FailFast
	}

	// Params, with identifier-safe names.
	paramName := map[string]string{}
	if len(ir.Params) > 0 {
		spec.Params = map[string]ValueSpec{}
	}
	for _, name := range sortedKeys(ir.Params) {
		p := ir.Params[name]
		n := uniqueName(sanitizeIdentifier(name), func(s string) bool { _, ok := spec.Params[s]; return ok })
		if n != name {
			find(p.Line, "param", Approximated, fmt.Sprintf("param %s is named %s in Nexus (identifier rules)", name, n),
				"params."+n)
		}
		paramName[name] = n
		spec.Params[n] = valueSpec(p)
	}

	// Node ids.
	used := map[string]bool{}
	for _, t := range ir.Tasks {
		id := sanitizeNodeID(t.ID)
		id = uniqueName(id, func(s string) bool { return used[s] })
		used[id] = true
		b.IDs[t.ID] = id
		if id != t.ID {
			find(t.Line, "task id", Translated, fmt.Sprintf("task %s is node %s (node id rules)", t.ID, id),
				"tasks["+id+"]")
		}
	}

	ioName := func(s string) string { return sanitizeIdentifier(s) }
	for _, t := range ir.Tasks {
		ts := b.task(t, ir, opts, paramName, ioName, find)
		spec.Tasks = append(spec.Tasks, ts)
	}

	// Edges, de-duplicated, with data bindings.
	type key struct{ from, to string }
	index := map[key]int{}
	addEdge := func(from, to string, bind map[string]string) {
		k := key{from, to}
		if i, ok := index[k]; ok {
			for in, out := range bind {
				if spec.Edges[i].OutputToInput == nil {
					spec.Edges[i].OutputToInput = map[string]string{}
				}
				spec.Edges[i].OutputToInput[in] = out
			}
			return
		}
		e := EdgeSpec{From: from, To: to}
		if len(bind) > 0 {
			e.OutputToInput = map[string]string{}
			for in, out := range bind {
				e.OutputToInput[in] = out
			}
		}
		index[k] = len(spec.Edges)
		spec.Edges = append(spec.Edges, e)
	}
	for _, e := range ir.Edges {
		from, ok1 := b.IDs[e.From]
		to, ok2 := b.IDs[e.To]
		if !ok1 || !ok2 {
			find(e.Line, "dependency", Unsupported, fmt.Sprintf("a dependency names a task that was not converted "+
				"(%s -> %s)", e.From, e.To), "")
			continue
		}
		bind := map[string]string{}
		for in, out := range e.Inputs {
			bind[ioName(in)] = ioName(out)
		}
		addEdge(from, to, bind)
	}

	// Map templates and the synthetic nodes some of them need.
	byID := map[string]int{}
	for i, t := range spec.Tasks {
		byID[t.ID] = i
	}
	for _, t := range ir.Tasks {
		if t.Map == nil || t.Kind == KindPlaceholder {
			continue
		}
		id := b.IDs[t.ID]
		ts := &spec.Tasks[byID[id]]
		m := t.Map
		fanout := m.MaxFanout
		if fanout <= 0 {
			fanout = opts.MaxFanout
			find(t.Line, "max_fanout", Approximated, fmt.Sprintf("%s: the source bounds the collection nowhere; "+
				"max_fanout %d was chosen (--max-fanout). A larger collection fails the map explicitly", id, fanout),
				"tasks["+id+"].map.max_fanout")
		}
		item := ioName(m.ItemInput)
		switch {
		case m.Over != "":
			over, ok := b.IDs[m.Over]
			if !ok {
				find(t.Line, "map", Unsupported, fmt.Sprintf("%s maps over %s, which was not converted", id, m.Over), "")
				continue
			}
			ts.Map = &MapSpec{OverNodeID: over, OverOutput: ioName(orDefault(m.OverOutput, "result")),
				ItemInput: item, MaxFanout: fanout}
			addEdge(over, id, nil)
		case m.LiteralItemsJSON != "" || m.OverParam != "":
			src := uniqueName(sanitizeNodeID(id+"__items"), func(s string) bool { return used[s] })
			used[src] = true
			items := TaskSpec{
				ID: src, TaskRef: "nexus-import/items",
				Outputs: map[string]ValueSpec{"items": {Type: "json", Required: true}},
				Retry:   &Retry{MaxAttempts: 1},
				Runtime: &RuntimeSpec{Executor: "subprocess", Command: []string{"sh", "-c"}},
			}
			if m.LiteralItemsJSON != "" {
				// The list is sh's $1, not an env var: env values are capped at
				// 4096 characters by the schema, and an argument needs no quoting.
				items.Runtime.Args = []string{`printf '%s' "$1" > "$NEXUS_OUTPUT_DIR/items"`, "items",
					m.LiteralItemsJSON}
			} else {
				p := paramName[m.OverParam]
				if p == "" {
					find(t.Line, "map", Unsupported, fmt.Sprintf("%s maps over param %s, which is not declared", id,
						m.OverParam), "")
					continue
				}
				items.Inputs = map[string]ValueSpec{"items": {Type: "json", Required: true}}
				items.InputParams = map[string]string{"items": p}
				items.Runtime.Args = []string{`cat "$NEXUS_INPUT_DIR/items" > "$NEXUS_OUTPUT_DIR/items"`}
			}
			spec.Tasks = append(spec.Tasks, items)
			ts = &spec.Tasks[byID[id]] // the append may have moved the slice
			ts.Map = &MapSpec{OverNodeID: src, OverOutput: "items", ItemInput: item, MaxFanout: fanout}
			addEdge(src, id, nil)
			b.IDs["(items of "+t.ID+")"] = src
		}
		if ts.Inputs == nil {
			ts.Inputs = map[string]ValueSpec{}
		}
		ts.Inputs[item] = ValueSpec{Type: "json", Required: true}
		delete(ts.InputLiterals, item)
	}

	// A node with no edges in an otherwise connected graph is rejected by the
	// gateway as a forgotten dependency. In a converted DAG it is deliberate
	// (an independent task), so hang it off one synthetic no-op root.
	if len(spec.Edges) > 0 {
		touched := map[string]bool{}
		for _, e := range spec.Edges {
			touched[e.From], touched[e.To] = true, true
		}
		var isolated []string
		for _, t := range spec.Tasks {
			if !touched[t.ID] {
				isolated = append(isolated, t.ID)
			}
		}
		if len(isolated) > 0 {
			root := uniqueName("nexus-import-root", func(s string) bool { return used[s] })
			used[root] = true
			spec.Tasks = append(spec.Tasks, TaskSpec{ID: root, TaskRef: "nexus-import/noop", Retry: &Retry{MaxAttempts: 1},
				Runtime: &RuntimeSpec{Executor: "subprocess", Command: []string{"true"}}})
			for _, id := range isolated {
				addEdge(root, id, nil)
			}
			find(ir.Line, "independent task", Approximated, fmt.Sprintf("%s have no dependencies while the rest of "+
				"the graph is connected; the gateway rejects that as a forgotten edge, so they hang off a no-op "+
				"root task %s", strings.Join(isolated, ", "), root), "tasks["+root+"]")
		}
	}
	b.Spec = spec
	return b
}

func (b *built) task(t *IRTask, ir *IRWorkflow, opts BuildOptions, paramName map[string]string,
	ioName func(string) string, find func(int, string, string, string, string)) TaskSpec {
	id := b.IDs[t.ID]
	ts := TaskSpec{ID: id}
	rt := &RuntimeSpec{Executor: "subprocess"}
	switch t.Kind {
	case KindShell:
		interp := orDefault(t.Interpreter, "bash")
		ts.TaskRef = sanitizeTaskRef(opts.Source + "/" + ir.SourceID + "/" + t.ID)
		rt.Command = []string{interp, "-c"}
		rt.Args = []string{t.Script}
	case KindPython:
		ref := sanitizeTaskRef(t.Module + "." + t.Callable)
		ts.TaskRef = ref
		rt.Command = []string{"python3", "-m", "nexus.entrypoint", "--module", t.Module}
		rt.Args = []string{ref}
		if t.Executor != "" && t.Executor != "subprocess" {
			rt.Executor, rt.Image = t.Executor, t.Image
		}
	case KindContainer:
		ts.TaskRef = sanitizeTaskRef(opts.Source + "/" + ir.SourceID + "/" + t.ID)
		rt.Executor = orDefault(t.Executor, "kubernetes")
		rt.Image = t.Image
		rt.Command = append([]string(nil), t.Command...)
		rt.Args = append([]string(nil), t.Args...)
	case KindNoop:
		ts.TaskRef = "nexus-import/noop"
		rt.Command = []string{"true"}
	default: // placeholder, and anything unknown
		ts.TaskRef = "nexus-import/unsupported"
		rt.Command = []string{"sh", "-c"}
		rt.Args = []string{placeholderScript}
		rt.Env = map[string]string{"NEXUS_IMPORT_UNSUPPORTED": orDefault(t.Reason, "unsupported construct")}
	}
	if t.Kind != KindPlaceholder {
		for k, v := range t.Env {
			if rt.Env == nil {
				rt.Env = map[string]string{}
			}
			rt.Env[k] = v
		}
		rt.WorkingDir = t.WorkingDir
		if t.StdoutOutput != "" {
			rt.StdoutOutput = ioName(t.StdoutOutput)
		}
	}
	ts.Runtime = rt

	if t.Kind != KindPlaceholder {
		renamed := func(kind, from, to string) {
			if from != to {
				find(t.Line, kind, Approximated, fmt.Sprintf("%s: %s %s is named %s in Nexus (identifier rules); "+
					"the handler must use the new name", id, kind, from, to), "tasks["+id+"]")
			}
		}
		for _, n := range sortedKeys(t.Inputs) {
			if ts.Inputs == nil {
				ts.Inputs = map[string]ValueSpec{}
			}
			nn := ioName(n)
			renamed("input", n, nn)
			ts.Inputs[nn] = valueSpec(t.Inputs[n])
		}
		for _, n := range sortedKeys(t.Outputs) {
			if ts.Outputs == nil {
				ts.Outputs = map[string]ValueSpec{}
			}
			nn := ioName(n)
			renamed("output", n, nn)
			ts.Outputs[nn] = valueSpec(t.Outputs[n])
		}
		for n, v := range t.InputLiterals {
			if ts.InputLiterals == nil {
				ts.InputLiterals = map[string]string{}
			}
			ts.InputLiterals[ioName(n)] = v
		}
		for n, p := range t.InputParams {
			if ts.InputParams == nil {
				ts.InputParams = map[string]string{}
			}
			pn, ok := paramName[p]
			if !ok {
				pn = ioName(p)
			}
			ts.InputParams[ioName(n)] = pn
		}
		if t.K8sResources != nil {
			ts.Resources = b.resources(id, t, find)
		}
		if t.Queue != "" {
			q := sanitizeIdentifier(t.Queue)
			if len(q) > 128 {
				q = q[:128]
			}
			if q != t.Queue {
				find(t.Line, "queue", Approximated, fmt.Sprintf("%s: queue %s is %s in Nexus (queue name rules)", id,
					t.Queue, q), "tasks["+id+"].queue")
			}
			ts.Queue = q
		}
		ts.Priority = t.Priority
		if t.TriggerRule != "" && t.TriggerRule != "all_success" {
			ts.TriggerRule = t.TriggerRule
		}
		ts.Timeouts = t.Timeouts
		ts.Cache = t.Cache
		if t.ConcurrencyKey != "" && t.ConcurrencyLimit > 0 {
			ts.ConcurrencyKey, ts.ConcurrencyLimit = truncate(t.ConcurrencyKey, 256), t.ConcurrencyLimit
		}
	}
	ts.Retry = t.Retry
	if ts.Retry == nil || ts.Retry.MaxAttempts < 1 {
		// Every source defaults to NO retry; Nexus defaults to three attempts,
		// so the absence has to be written down to mean the same thing.
		ts.Retry = &Retry{MaxAttempts: 1}
	}
	if t.Kind == KindPlaceholder {
		ts.Retry = &Retry{MaxAttempts: 1}
	}
	b.fixTimeouts(id, t, &ts, find)
	return ts
}

// fixTimeouts drops a schedule_to_close the gateway would reject as
// incoherent (smaller than one attempt's start_to_close).
func (b *built) fixTimeouts(id string, t *IRTask, ts *TaskSpec, find func(int, string, string, string, string)) {
	to := ts.Timeouts
	if to == nil {
		return
	}
	c := *to
	if c.StartToCloseMs > 0 && c.ScheduleToCloseMs > 0 && c.ScheduleToCloseMs < c.StartToCloseMs {
		find(t.Line, "timeouts", Approximated, fmt.Sprintf("%s: the total retry budget (%dms) is shorter than one "+
			"attempt's deadline (%dms); Nexus rejects that, so only the per-attempt deadline was kept", id,
			c.ScheduleToCloseMs, c.StartToCloseMs), "tasks["+id+"].timeouts")
		c.ScheduleToCloseMs = 0
	}
	if c == (Timeouts{}) {
		ts.Timeouts = nil
		return
	}
	ts.Timeouts = &c
}

func (b *built) resources(id string, t *IRTask, find func(int, string, string, string, string)) *ResourceRequest {
	conv := func(m map[string]string) *ResourceVector {
		if len(m) == 0 {
			return nil
		}
		v := &ResourceVector{}
		for _, k := range sortedKeys(m) {
			q := m[k]
			var err error
			switch {
			case k == "cpu":
				v.CPUMillis, err = parseCPU(q)
			case k == "memory":
				v.MemoryBytes, err = parseQuantity(q)
			case k == "ephemeral-storage":
				v.DiskBytes, err = parseQuantity(q)
			case strings.HasSuffix(k, "/gpu") || k == "gpu":
				v.GPUCount, err = strconv.ParseInt(q, 10, 64)
				if k != "nvidia.com/gpu" && k != "gpu" {
					v.GPUKind = strings.TrimSuffix(k, "/gpu")
				}
			default:
				var n int64
				n, err = parseQuantity(q)
				if err == nil {
					if v.Custom == nil {
						v.Custom = map[string]int64{}
					}
					v.Custom[truncate(k, 128)] = n
					find(t.Line, "resources", Approximated, fmt.Sprintf("%s: extended resource %s becomes the custom "+
						"countable %s=%d; a worker must advertise it", id, k, k, n), "resources.custom")
				}
			}
			if err != nil {
				find(t.Line, "resources", Unsupported, fmt.Sprintf("%s: resource %s=%q: %v", id, k, q, err), "")
			}
		}
		if v.empty() {
			return nil
		}
		return v
	}
	req := conv(t.K8sResources.Requests)
	lim := conv(t.K8sResources.Limits)
	if req == nil && lim != nil {
		// Kubernetes semantics: requests default to limits.
		cp := *lim
		req = &cp
	}
	if req != nil && lim != nil && req.GPUCount == 0 && lim.GPUCount > 0 {
		req.GPUCount, req.GPUKind = lim.GPUCount, lim.GPUKind
	}
	if req == nil && lim == nil {
		return nil
	}
	find(t.Line, "resources", Translated, id+": resource requests/limits", "tasks["+id+"].resources")
	return &ResourceRequest{Request: req, Limit: lim}
}

var quantityRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?(?:[eE][0-9]+)?)(Ki|Mi|Gi|Ti|Pi|Ei|k|K|M|G|T|P|E|m)?$`)

func parseQuantity(q string) (int64, error) {
	m := quantityRe.FindStringSubmatch(strings.TrimSpace(q))
	if m == nil {
		return 0, fmt.Errorf("not a Kubernetes quantity")
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	mult := map[string]float64{"": 1, "m": 0.001, "k": 1e3, "K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15,
		"E": 1e18, "Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "Pi": 1 << 50, "Ei": 1 << 60}[m[2]]
	return int64(math.Ceil(f * mult)), nil
}

func parseCPU(q string) (int64, error) {
	q = strings.TrimSpace(q)
	if strings.HasSuffix(q, "m") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(q, "m"), 64)
		return int64(math.Ceil(n)), err
	}
	n, err := strconv.ParseFloat(q, 64)
	return int64(math.Ceil(n * 1000)), err
}

func valueSpec(d ValueDecl) ValueSpec {
	t := d.Type
	switch t {
	case "int", "float", "string", "bool", "json", "file_ref":
	default:
		t = "json"
	}
	v := ValueSpec{Type: t, Required: d.Required}
	if d.DefaultJSON != nil {
		v.DefaultJSON = *d.DefaultJSON
		v.Required = false // a required input with a default is contradictory
	}
	return v
}

func uniqueName(base string, taken func(string) bool) string {
	if !taken(base) {
		return base
	}
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s-%d", base, i)
		if !taken(c) {
			return c
		}
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
