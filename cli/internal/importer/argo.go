// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Argo Workflows -> IR. Read with yaml.v3 Nodes so every finding carries the
// line it came from. Argo templates are INLINED: a dag or steps template
// called from a task becomes that task's subgraph, with node ids prefixed by
// the calling task ("A.B"), the way Airflow task groups are flattened.
//
// Argo substitutes {{...}} parameters textually before a container starts.
// Where the value is known now (a literal argument) the converter does the
// same substitution; where it is known only at run time (a workflow param, an
// upstream output, a loop item) it becomes a Nexus input read from
// $NEXUS_INPUT_DIR by a small shell prelude -- which needs a shell in the
// image, and is reported as approximated.

// shellHelper reads a Nexus input, dropping one level of JSON string quotes.
const shellHelper = `nexus_in() { v=$(cat "$NEXUS_INPUT_DIR/$1"); case $v in \"*\") v=${v#\"}; v=${v%\"} ;; esac; printf '%s' "$v"; }` + "\n"

var argoRefRe = regexp.MustCompile(`\{\{\s*(=?)\s*([^{}]*?)\s*\}\}`)

// binding is what a {{...}} reference, or an argument, resolves to.
type binding struct {
	kind    string // literal | param | output | item | env | unknown
	literal string
	param   string // workflow param (kind param)
	node    string // producing node id (kind output)
	output  string // output name on that node
	env     string // shell text (kind env), e.g. ${NEXUS_RUN_ID}
	reason  string // kind unknown
	status  string // approximated for env
}

type seg struct {
	lit   string
	ref   *binding
	input string // the Nexus input the runtime value arrives in
}

// groupResult is what invoking a template produced.
type groupResult struct {
	nodes  []string
	roots  []string
	leaves []string
	leaf   *argoLeaf // set when the invocation is a single task
}

type argoLeaf struct {
	task     *IRTask
	tmplName string
	script   bool
	source   []seg
	command  [][]seg
	args     [][]seg
	outParam map[string]outDecl // template outputs.parameters with valueFrom.path
	outArt   map[string]string  // template output artifacts: name -> path
	inArt    []inArtifact
	needOut  map[string]bool
	line     int
}

type outDecl struct {
	path, def string
	hasDef    bool
}

type inArtifact struct {
	name, path, raw string
	isRaw           bool
}

type argoScope struct {
	prefix string
	inputs map[string]binding
	arts   map[string]binding // input artifacts: bound to an upstream output artifact
	tasks  map[string]*groupResult
	kind   string // "tasks" (dag) or "steps"
}

type argoConv struct {
	file      string
	wf        *IRWorkflow
	templates map[string]*yaml.Node
	defaults  *yaml.Node
	podRes    *yaml.Node
	leaves    []*argoLeaf
	used      map[string]bool
	stack     []string
}

func (c *argoConv) find(line int, construct, status, msg, nexus string) {
	c.wf.find(line, construct, status, msg, nexus)
}

// analyzeArgo reads every Argo document in a YAML file.
func analyzeArgo(path string) irFile {
	f := irFile{Path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		f.Error = err.Error()
		return f
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			f.Error = fmt.Sprintf("not valid YAML: %v", err)
			return f
		}
		if len(doc.Content) == 0 {
			continue
		}
		root := doc.Content[0]
		api := scalar(mapGet(root, "apiVersion"))
		kind := scalar(mapGet(root, "kind"))
		if !strings.HasPrefix(api, "argoproj.io/") {
			continue
		}
		switch kind {
		case "Workflow", "WorkflowTemplate", "ClusterWorkflowTemplate", "CronWorkflow":
			f.Workflows = append(f.Workflows, convertArgo(path, root, kind))
		default:
			f.Findings = append(f.Findings, Finding{File: path, Line: root.Line, Construct: kind, Status: Unsupported,
				Message: fmt.Sprintf("Argo kind %s is not a workflow; not converted", kind)})
		}
	}
	return f
}

func convertArgo(path string, root *yaml.Node, kind string) *IRWorkflow {
	meta := mapGet(root, "metadata")
	name := scalar(mapGet(meta, "name"))
	if name == "" {
		name = strings.TrimRight(scalar(mapGet(meta, "generateName")), "-.")
	}
	if name == "" {
		name = "argo-workflow"
	}
	wf := &IRWorkflow{SourceID: name, File: path, Line: root.Line, Labels: map[string]string{}}
	c := &argoConv{file: path, wf: wf, templates: map[string]*yaml.Node{}, used: map[string]bool{}}
	if ann := mapGet(meta, "annotations"); ann != nil {
		if d := scalar(mapGet(ann, "workflows.argoproj.io/description")); d != "" {
			wf.Description = strings.TrimSpace(d)
		}
	}
	spec := mapGet(root, "spec")
	if kind == "CronWorkflow" {
		c.cron(spec)
		spec = mapGet(spec, "workflowSpec")
		if spec == nil {
			c.find(root.Line, "CronWorkflow", Unsupported, "CronWorkflow without workflowSpec", "")
			c.finish()
			return wf
		}
	}
	if spec == nil {
		c.find(root.Line, kind, Unsupported, "no spec", "")
		c.finish()
		return wf
	}
	c.spec(spec, kind)
	c.finish()
	return wf
}

func (c *argoConv) cron(spec *yaml.Node) {
	var crons []*yaml.Node
	if s := mapGet(spec, "schedule"); s != nil {
		crons = append(crons, s)
	}
	if s := mapGet(spec, "schedules"); s != nil && s.Kind == yaml.SequenceNode {
		crons = append(crons, s.Content...)
	}
	tz := scalar(mapGet(spec, "timezone"))
	overlap, note := "allow", ""
	policyNode := mapGet(spec, "concurrencyPolicy")
	switch p := scalar(policyNode); p {
	case "", "Allow":
	case "Forbid":
		overlap = "skip"
	case "Replace":
		overlap = ""
		c.find(policyNode.Line, "concurrencyPolicy", Unsupported, "concurrencyPolicy Replace cancels the running "+
			"workflow when the next one is due; Nexus schedules skip, allow or queue one (the suggested schedule "+
			"uses the default, skip)", "")
	default:
		c.find(policyNode.Line, "concurrencyPolicy", Unsupported, "unknown concurrencyPolicy "+p, "")
	}
	if overlap != "" && policyNode != nil {
		note = fmt.Sprintf("concurrencyPolicy %s -> overlap %s", scalar(policyNode), overlap)
	}
	for _, n := range crons {
		s := IRSchedule{Line: n.Line, Cron: n.Value, Timezone: tz, Overlap: overlap, Via: "CronWorkflow"}
		if n.Kind != yaml.ScalarNode {
			s.Unsupported = "schedule is not a string"
		}
		if scalar(mapGet(spec, "suspend")) == "true" {
			s.Disabled = true
		}
		c.wf.Schedules = append(c.wf.Schedules, s)
	}
	if note != "" {
		c.find(policyNode.Line, "concurrencyPolicy", Translated, note, "schedule --overlap "+overlap)
	}
	if tz == "" && len(crons) > 0 {
		c.find(spec.Line, "timezone", Approximated, "no timezone: Argo uses the controller's local zone, the "+
			"suggested schedule uses UTC", "schedule --timezone UTC")
	}
	for _, k := range []string{"startingDeadlineSeconds", "successfulJobsHistoryLimit", "failedJobsHistoryLimit"} {
		if n := mapGet(spec, k); n != nil {
			c.find(n.Line, k, Approximated, k+" is managed by the Nexus scheduler; ignored", "")
		}
	}
	for _, k := range []string{"when", "stopStrategy"} {
		if n := mapGet(spec, k); n != nil {
			c.find(n.Line, k, Unsupported, "CronWorkflow "+k+" has no Nexus equivalent", "")
		}
	}
}

func (c *argoConv) spec(spec *yaml.Node, kind string) {
	if tr := mapGet(spec, "workflowTemplateRef"); tr != nil {
		c.find(tr.Line, "workflowTemplateRef", Unsupported, "the workflow runs a WorkflowTemplate stored in the "+
			"cluster; convert that template's YAML instead", "")
		return
	}
	if ts := mapGet(spec, "templates"); ts != nil {
		for _, t := range ts.Content {
			c.templates[scalar(mapGet(t, "name"))] = t
		}
	}
	c.defaults = mapGet(spec, "templateDefaults")
	if c.defaults != nil {
		c.find(c.defaults.Line, "templateDefaults", Approximated, "templateDefaults: retryStrategy, "+
			"activeDeadlineSeconds and container resources/env are applied to every template; other fields are "+
			"ignored", "")
	}
	c.podRes = mapGet(spec, "podResources")
	if c.podRes != nil {
		c.find(c.podRes.Line, "podResources", Approximated, "pod-level resources are applied to every task that "+
			"declares none of its own (Nexus reserves per task)", "resources")
	}
	if args := mapGet(spec, "arguments"); args != nil {
		c.params(args)
	}
	if n := mapGet(spec, "activeDeadlineSeconds"); n != nil {
		if s, err := strconv.ParseInt(n.Value, 10, 64); err == nil {
			ms := s * 1000
			c.wf.RunTimeoutMs = &ms
			c.find(n.Line, "activeDeadlineSeconds", Translated, fmt.Sprintf("workflow deadline %ds", s),
				"run_timeout_ms")
		}
	}
	if n := mapGet(spec, "onExit"); n != nil {
		c.find(n.Line, "onExit", Unsupported, fmt.Sprintf("exit handler %s runs after the workflow whatever its "+
			"outcome; Nexus has no exit handlers (watch the run with `nexusctl events --follow`, or add a final "+
			"task with trigger_rule all_done). The handler's templates were not converted", n.Value), "")
	}
	for _, k := range []string{"hooks", "parallelism", "serviceAccountName", "volumes", "volumeClaimTemplates",
		"securityContext", "nodeSelector", "tolerations", "affinity", "imagePullSecrets", "hostNetwork",
		"dnsPolicy", "dnsConfig", "podSpecPatch", "priorityClassName", "podPriorityClassName", "executor",
		"automountServiceAccountToken", "synchronization", "suspend", "hostAliases", "shutdown"} {
		if n := mapGet(spec, k); n != nil {
			msg := k + " has no Nexus equivalent"
			switch k {
			case "parallelism":
				msg = "parallelism caps a run's concurrent pods; Nexus has no per-run cap (use concurrency_key/" +
					"concurrency_limit on contending tasks)"
			case "serviceAccountName", "volumes", "volumeClaimTemplates", "securityContext", "nodeSelector",
				"tolerations", "affinity", "imagePullSecrets", "hostNetwork", "dnsPolicy", "dnsConfig",
				"podSpecPatch", "priorityClassName", "podPriorityClassName", "hostAliases":
				msg = k + " is a pod setting the Nexus kubernetes executor does not take from the spec"
			}
			c.find(n.Line, k, Unsupported, msg, "")
		}
	}
	for _, k := range []string{"ttlStrategy", "podGC", "archiveLogs", "metrics", "podMetadata", "workflowMetadata",
		"artifactRepositoryRef", "artifactGC", "podDisruptionBudget", "retryStrategy"} {
		if n := mapGet(spec, k); n != nil {
			c.find(n.Line, k, Approximated, k+" is an Argo controller setting; ignored", "")
		}
	}
	if n := mapGet(spec, "failFast"); n != nil {
		b := n.Value == "true"
		c.wf.FailFast = &b
	}
	ep := mapGet(spec, "entrypoint")
	if ep == nil {
		if kind == "WorkflowTemplate" || kind == "ClusterWorkflowTemplate" {
			c.find(spec.Line, "entrypoint", Unsupported, "a WorkflowTemplate without an entrypoint is a template "+
				"library; convert the workflow that uses it", "")
		} else {
			c.find(spec.Line, "entrypoint", Unsupported, "no entrypoint", "")
		}
		return
	}
	sc := &argoScope{inputs: map[string]binding{}, arts: map[string]binding{}, tasks: map[string]*groupResult{}}
	c.invoke(ep.Value, ep.Value, "", sc, nil, ep.Line, nil)
}

func (c *argoConv) params(args *yaml.Node) {
	if ps := mapGet(args, "parameters"); ps != nil {
		for _, p := range ps.Content {
			name := scalar(mapGet(p, "name"))
			d := ValueDecl{Type: "string", Line: p.Line}
			if v := mapGet(p, "value"); v != nil {
				j, _ := json.Marshal(v.Value)
				s := string(j)
				d.DefaultJSON = &s
			} else if mapGet(p, "valueFrom") != nil {
				c.find(p.Line, "parameter", Unsupported, fmt.Sprintf("param %s takes its value from a ConfigMap or "+
					"another source (valueFrom); declared without a default", name), "params."+name)
				d.Required = true
			} else {
				d.Required = true
			}
			if e := mapGet(p, "enum"); e != nil {
				c.find(e.Line, "enum", Approximated, fmt.Sprintf("param %s: the allowed values (enum) are not "+
					"enforced by Nexus", name), "")
			}
			if c.wf.Params == nil {
				c.wf.Params = map[string]ValueDecl{}
			}
			c.wf.Params[name] = d
			c.find(p.Line, "parameter", Translated, "workflow parameter "+name, "params."+name+" (string)")
		}
	}
	if as := mapGet(args, "artifacts"); as != nil {
		c.find(as.Line, "arguments.artifacts", Unsupported, "workflow input artifacts are fetched by Argo; pass a "+
			"reference as a param and fetch it in the task", "")
	}
}

// invoke expands one template call. name is the calling task's name (or the
// entrypoint), sc the caller's scope, argsNode its arguments, loop its
// withItems/withParam/withSequence (nil for none).
func (c *argoConv) invoke(name, tmplName, prefix string, sc *argoScope, argsNode *yaml.Node, line int,
	task *yaml.Node) *groupResult {
	tmpl := c.templates[tmplName]
	id := prefix + name
	if tmpl == nil {
		return c.placeholder(id, line, fmt.Sprintf("template %s is not defined in this file", tmplName))
	}
	for _, s := range c.stack {
		if s == tmplName {
			return c.placeholder(id, line, fmt.Sprintf("template %s calls itself (recursion); Nexus DAGs are "+
				"static", tmplName))
		}
	}
	if len(c.stack) > 20 {
		return c.placeholder(id, line, "templates nest more than 20 deep")
	}
	c.stack = append(c.stack, tmplName)
	defer func() { c.stack = c.stack[:len(c.stack)-1] }()

	loop := loopNode(task)
	// The template's own inputs, bound from the call's arguments.
	inputs := map[string]binding{}
	var itemInputs []string
	argVals := map[string]*yaml.Node{}
	if argsNode != nil {
		if ps := mapGet(argsNode, "parameters"); ps != nil {
			for _, p := range ps.Content {
				argVals[scalar(mapGet(p, "name"))] = p
			}
		}
	}
	if in := mapGet(tmpl, "inputs"); in != nil {
		if ps := mapGet(in, "parameters"); ps != nil {
			for _, p := range ps.Content {
				pn := scalar(mapGet(p, "name"))
				if a, ok := argVals[pn]; ok {
					if mapGet(a, "valueFrom") != nil {
						inputs[pn] = binding{kind: "unknown", reason: "an argument with valueFrom"}
						continue
					}
					b := c.resolveValue(scalar(mapGet(a, "value")), sc, loop != nil, a.Line)
					inputs[pn] = b
					if b.kind == "item" {
						itemInputs = append(itemInputs, pn)
					}
					continue
				}
				if _, isParam := c.wf.Params[pn]; isParam && sc.kind == "" {
					// Argo fills the entrypoint's inputs from the workflow
					// arguments of the same name.
					inputs[pn] = binding{kind: "param", param: pn}
				} else if v := mapGet(p, "value"); v != nil {
					inputs[pn] = binding{kind: "literal", literal: v.Value}
				} else if v := mapGet(p, "default"); v != nil {
					inputs[pn] = binding{kind: "literal", literal: v.Value}
				} else {
					inputs[pn] = binding{kind: "unknown", reason: fmt.Sprintf("input %s of template %s has no "+
						"argument and no default", pn, tmplName)}
				}
			}
		}
	}
	// Input artifacts bound from the call.
	arts := map[string]binding{}
	if argsNode != nil {
		if as := mapGet(argsNode, "artifacts"); as != nil {
			for _, a := range as.Content {
				an := scalar(mapGet(a, "name"))
				from := scalar(mapGet(a, "from"))
				if from == "" {
					arts[an] = binding{kind: "unknown", reason: "an artifact argument without `from`"}
					continue
				}
				arts[an] = c.resolveArtifact(from, sc)
			}
		}
	}

	switch {
	case mapGet(tmpl, "dag") != nil || mapGet(tmpl, "steps") != nil:
		if loop != nil {
			return c.placeholder(id, line, fmt.Sprintf("a loop over template %s, which is a whole %s: Nexus maps "+
				"a single task, not a subgraph", tmplName, map[bool]string{true: "dag", false: "steps group"}[mapGet(tmpl, "dag") != nil]))
		}
		// The entrypoint's tasks are the top level; a nested dag/steps call
		// prefixes its tasks with the calling task's id.
		inner := &argoScope{prefix: id + ".", inputs: inputs, arts: arts, tasks: map[string]*groupResult{}}
		if sc.kind == "" {
			inner.prefix = ""
		}
		c.templateExtras(tmpl, id)
		if d := mapGet(tmpl, "dag"); d != nil {
			inner.kind = "tasks"
			return c.dag(d, inner)
		}
		inner.kind = "steps"
		return c.steps(mapGet(tmpl, "steps"), inner)
	case mapGet(tmpl, "container") != nil || mapGet(tmpl, "script") != nil:
		return c.leafTask(id, tmplName, tmpl, inputs, arts, itemInputs, loop, sc, line)
	}
	for _, k := range []string{"resource", "suspend", "http", "plugin", "containerSet", "data"} {
		if mapGet(tmpl, k) != nil {
			msg := map[string]string{
				"resource":     "a resource template creates or patches Kubernetes objects; run kubectl from a container task instead",
				"suspend":      "a suspend template pauses the workflow for approval or a duration; Nexus has `nexusctl run pause`/`resume` but no in-DAG suspension",
				"http":         "an http template calls a URL from the controller; use a container task running curl",
				"plugin":       "executor plugins have no Nexus equivalent",
				"containerSet": "a containerSet runs several containers in one pod; split them into tasks",
				"data":         "a data template transforms artifacts in the controller; use a task",
			}[k]
			return c.placeholder(id, line, msg)
		}
	}
	return c.placeholder(id, line, fmt.Sprintf("template %s has no supported body", tmplName))
}

func loopNode(task *yaml.Node) *yaml.Node {
	for _, k := range []string{"withItems", "withParam", "withSequence"} {
		if n := mapGet(task, k); n != nil {
			return task
		}
	}
	return nil
}

func (c *argoConv) templateExtras(tmpl *yaml.Node, id string) {
	for _, k := range []string{"onExit", "hooks", "parallelism", "synchronization", "failFast"} {
		if n := mapGet(tmpl, k); n != nil {
			c.find(n.Line, k, Unsupported, fmt.Sprintf("%s: template-level %s is not translated", id, k), "")
		}
	}
}

func (c *argoConv) placeholder(id string, line int, reason string) *groupResult {
	id = c.unique(id)
	t := &IRTask{ID: id, Line: line, Kind: KindPlaceholder, Reason: reason}
	c.wf.Tasks = append(c.wf.Tasks, t)
	c.find(line, "template", Unsupported, id+": "+reason, "tasks["+id+"] (placeholder)")
	return &groupResult{nodes: []string{id}, roots: []string{id}, leaves: []string{id}}
}

func (c *argoConv) unique(id string) string {
	if !c.used[id] {
		c.used[id] = true
		return id
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", id, i)
		if !c.used[cand] {
			c.used[cand] = true
			return cand
		}
	}
}

// resolveValue resolves an argument value in the CALLER's scope.
func (c *argoConv) resolveValue(v string, sc *argoScope, loop bool, line int) binding {
	refs := argoRefRe.FindAllStringSubmatchIndex(v, -1)
	if len(refs) == 0 {
		return binding{kind: "literal", literal: v}
	}
	if len(refs) == 1 && refs[0][0] == 0 && refs[0][1] == len(v) {
		m := argoRefRe.FindStringSubmatch(v)
		return c.resolveRef(m[1] == "=", m[2], sc, loop)
	}
	// Several references, or text around one: fine when every part is
	// known now, otherwise a string built at run time.
	var b strings.Builder
	last := 0
	for _, r := range refs {
		b.WriteString(v[last:r[0]])
		m := argoRefRe.FindStringSubmatch(v[r[0]:r[1]])
		rb := c.resolveRef(m[1] == "=", m[2], sc, loop)
		if rb.kind != "literal" {
			return binding{kind: "unknown", reason: fmt.Sprintf("%q combines text with a run-time value; a Nexus "+
				"input carries one upstream value as-is", v)}
		}
		b.WriteString(rb.literal)
		last = r[1]
	}
	b.WriteString(v[last:])
	return binding{kind: "literal", literal: b.String()}
}

func (c *argoConv) resolveRef(expr bool, ref string, sc *argoScope, loop bool) binding {
	if expr {
		return binding{kind: "unknown", reason: fmt.Sprintf("expression template {{=%s}} is evaluated by the Argo "+
			"controller; Nexus has no expression language", ref)}
	}
	parts := strings.Split(ref, ".")
	switch {
	case len(parts) == 3 && parts[0] == "inputs" && parts[1] == "parameters":
		if b, ok := sc.inputs[parts[2]]; ok {
			return b
		}
		return binding{kind: "unknown", reason: "{{" + ref + "}} names an input the template does not declare"}
	case len(parts) == 3 && parts[0] == "workflow" && parts[1] == "parameters":
		return binding{kind: "param", param: parts[2]}
	case len(parts) >= 4 && (parts[0] == "tasks" || parts[0] == "steps") && parts[2] == "outputs":
		return c.outputRef(parts, sc, ref)
	case ref == "item":
		if !loop {
			return binding{kind: "unknown", reason: "{{item}} outside a loop"}
		}
		return binding{kind: "item"}
	case strings.HasPrefix(ref, "item."):
		return binding{kind: "unknown", reason: fmt.Sprintf("{{%s}} reads a field of the loop item; a Nexus map "+
			"hands the whole item to the task, which must read the field itself", ref)}
	case ref == "workflow.name" || ref == "workflow.uid":
		return binding{kind: "env", env: "${NEXUS_RUN_ID}", status: Approximated}
	case ref == "pod.name" || ref == "node.name":
		return binding{kind: "env", env: "${NEXUS_ATTEMPT_ID}", status: Approximated}
	case ref == "workflow.namespace":
		return binding{kind: "literal", literal: ""}
	case strings.HasPrefix(ref, "workflow.scheduledTime") || strings.HasPrefix(ref, "workflow.creationTimestamp"):
		return binding{kind: "unknown", reason: fmt.Sprintf("{{%s}} is the run's scheduled/creation time, which "+
			"Nexus does not pass to tasks; add a workflow param or compute it in the task", ref)}
	case strings.HasPrefix(ref, "workflow.status") || strings.HasPrefix(ref, "workflow.failures") ||
		strings.HasPrefix(ref, "workflow.duration"):
		return binding{kind: "unknown", reason: fmt.Sprintf("{{%s}} exists only in exit handlers", ref)}
	case len(parts) == 4 && parts[0] == "inputs" && parts[1] == "artifacts" && parts[3] == "path":
		return binding{kind: "unknown", reason: "{{" + ref + "}} (resolved below for leaf templates)"}
	case ref == "retries":
		return binding{kind: "env", env: "$(( ${NEXUS_ATTEMPT_NUMBER:-1} - 1 ))", status: Approximated}
	}
	return binding{kind: "unknown", reason: fmt.Sprintf("{{%s}} is not translated", ref)}
}

func (c *argoConv) outputRef(parts []string, sc *argoScope, ref string) binding {
	g := sc.tasks[parts[1]]
	if g == nil {
		return binding{kind: "unknown", reason: fmt.Sprintf("{{%s}} names %s %s, which is not an earlier "+
			"dependency here", ref, strings.TrimSuffix(parts[0], "s"), parts[1])}
	}
	if g.leaf == nil {
		return binding{kind: "unknown", reason: fmt.Sprintf("{{%s}}: %s is a nested dag/steps; reading its "+
			"outputs is not translated", ref, parts[1])}
	}
	switch {
	case len(parts) == 4 && parts[3] == "result":
		g.leaf.needOut["result"] = true
		return binding{kind: "output", node: g.leaf.task.ID, output: "result"}
	case len(parts) == 5 && parts[3] == "parameters":
		if _, ok := g.leaf.outParam[parts[4]]; !ok {
			return binding{kind: "unknown", reason: fmt.Sprintf("{{%s}}: %s declares no output parameter %s "+
				"with valueFrom.path", ref, parts[1], parts[4])}
		}
		g.leaf.needOut["param:"+parts[4]] = true
		return binding{kind: "output", node: g.leaf.task.ID, output: parts[4]}
	case len(parts) == 4 && parts[3] == "exitCode":
		return binding{kind: "unknown", reason: "{{" + ref + "}}: exit codes are not passed between Nexus tasks"}
	}
	return binding{kind: "unknown", reason: fmt.Sprintf("{{%s}} is not translated", ref)}
}

func (c *argoConv) resolveArtifact(from string, sc *argoScope) binding {
	m := argoRefRe.FindStringSubmatch(from)
	if m == nil {
		return binding{kind: "unknown", reason: "artifact `from` is not a reference"}
	}
	parts := strings.Split(m[2], ".")
	if len(parts) == 5 && (parts[0] == "tasks" || parts[0] == "steps") && parts[2] == "outputs" &&
		parts[3] == "artifacts" {
		g := sc.tasks[parts[1]]
		if g == nil || g.leaf == nil {
			return binding{kind: "unknown", reason: fmt.Sprintf("artifact from %s: not a single earlier task",
				m[2])}
		}
		if _, ok := g.leaf.outArt[parts[4]]; !ok {
			return binding{kind: "unknown", reason: fmt.Sprintf("%s declares no output artifact %s with a path",
				parts[1], parts[4])}
		}
		g.leaf.needOut["art:"+parts[4]] = true
		return binding{kind: "output", node: g.leaf.task.ID, output: parts[4]}
	}
	if len(parts) == 3 && parts[0] == "inputs" && parts[1] == "artifacts" {
		if b, ok := sc.arts[parts[2]]; ok {
			return b
		}
	}
	return binding{kind: "unknown", reason: fmt.Sprintf("artifact from %s is not translated", m[2])}
}

func (c *argoConv) dag(d *yaml.Node, sc *argoScope) *groupResult {
	tasks := mapGet(d, "tasks")
	if tasks == nil || len(tasks.Content) == 0 {
		return &groupResult{}
	}
	if n := mapGet(d, "failFast"); n != nil && n.Value == "false" {
		c.find(n.Line, "failFast", Translated, "failFast: false", "fail_fast=false")
		f := false
		c.wf.FailFast = &f
	}
	if n := mapGet(d, "target"); n != nil {
		c.find(n.Line, "target", Unsupported, "dag.target runs only part of the DAG; Nexus runs the whole spec", "")
	}
	byName := map[string]*yaml.Node{}
	var order []string
	for _, t := range tasks.Content {
		n := scalar(mapGet(t, "name"))
		byName[n] = t
		order = append(order, n)
	}
	// Dependencies first: an output can only be read from a task already built.
	deps := map[string][]string{}
	rules := map[string][4]string{}
	for _, n := range order {
		t := byName[n]
		if ds := mapGet(t, "dependencies"); ds != nil {
			for _, x := range ds.Content {
				deps[n] = append(deps[n], x.Value)
			}
		}
		if dp := mapGet(t, "depends"); dp != nil {
			names, rule, status, msg := dependsRule(dp.Value)
			deps[n] = append(deps[n], names...)
			rules[n] = [4]string{rule, status, msg, strconv.Itoa(dp.Line)}
		}
	}
	sorted, cyclic := topo(order, deps)
	if cyclic {
		c.find(d.Line, "dag", Unsupported, "the DAG's dependencies form a cycle", "")
	}
	res := &groupResult{}
	for _, n := range sorted {
		t := byName[n]
		g := c.dagTask(n, t, sc)
		sc.tasks[n] = g
		res.nodes = append(res.nodes, g.nodes...)
		for _, dn := range deps[n] {
			up := sc.tasks[dn]
			if up == nil {
				c.find(t.Line, "dependency", Unsupported, fmt.Sprintf("%s depends on %s, which is not a task of "+
					"this DAG", n, dn), "")
				continue
			}
			for _, a := range up.leaves {
				for _, b := range g.roots {
					c.wf.Edges = append(c.wf.Edges, IREdge{From: a, To: b, Line: t.Line})
				}
			}
		}
		if r, ok := rules[n]; ok {
			ln, _ := strconv.Atoi(r[3])
			c.find(ln, "depends", r[1], sc.prefix+n+": "+r[2], "edges")
			if r[0] == "all_done" {
				for _, root := range g.roots {
					c.taskByID(root).TriggerRule = "all_done"
				}
			}
		}
	}
	c.rootsLeaves(res)
	return res
}

func (c *argoConv) steps(s *yaml.Node, sc *argoScope) *groupResult {
	res := &groupResult{}
	var prev []*groupResult
	for i, grp := range s.Content {
		items := grp.Content
		if grp.Kind == yaml.MappingNode { // a single step written without the inner list
			items = []*yaml.Node{grp}
		}
		var cur []*groupResult
		for _, st := range items {
			n := scalar(mapGet(st, "name"))
			g := c.dagTask(n, st, sc)
			for _, p := range prev {
				for _, a := range p.leaves {
					for _, b := range g.roots {
						c.wf.Edges = append(c.wf.Edges, IREdge{From: a, To: b, Line: st.Line})
					}
				}
			}
			cur = append(cur, g)
			res.nodes = append(res.nodes, g.nodes...)
		}
		// Siblings in one group run in parallel; their names become visible to
		// later groups only.
		for j, st := range items {
			sc.tasks[scalar(mapGet(st, "name"))] = cur[j]
		}
		if len(items) > 1 {
			c.find(grp.Line, "steps", Translated, fmt.Sprintf("step group %d: %d parallel steps", i+1, len(items)),
				"edges from every step of the previous group")
		}
		prev = cur
	}
	c.rootsLeaves(res)
	return res
}

// dagTask handles one dag task or step: its template call and its own flags.
func (c *argoConv) dagTask(name string, t *yaml.Node, sc *argoScope) *groupResult {
	id := sc.prefix + name
	if n := mapGet(t, "templateRef"); n != nil {
		return c.placeholder(id, t.Line, fmt.Sprintf("templateRef %s/%s runs a template stored in the cluster; "+
			"convert that WorkflowTemplate and inline the template here", scalar(mapGet(n, "name")),
			scalar(mapGet(n, "template"))))
	}
	if n := mapGet(t, "inline"); n != nil {
		return c.placeholder(id, t.Line, "an inline template is not translated; give it a name under templates")
	}
	tmplName := scalar(mapGet(t, "template"))
	g := c.invoke(name, tmplName, sc.prefix, sc, mapGet(t, "arguments"), t.Line, t)
	if n := mapGet(t, "when"); n != nil {
		c.find(n.Line, "when", Unsupported, fmt.Sprintf("%s: `when: %s` runs the %s only if a condition holds at "+
			"run time; Nexus DAGs are static, so in the partial spec it runs unconditionally. Move the condition "+
			"into the task", id, n.Value, strings.TrimSuffix(sc.kind, "s")), "tasks["+id+"]")
	}
	if n := mapGet(t, "continueOn"); n != nil {
		c.find(n.Line, "continueOn", Unsupported, fmt.Sprintf("%s: continueOn lets the workflow carry on after "+
			"this task fails; give its downstream tasks trigger_rule all_done by hand if that is the intent", id), "")
	}
	for _, k := range []string{"onExit", "hooks"} {
		if n := mapGet(t, k); n != nil {
			c.find(n.Line, k, Unsupported, fmt.Sprintf("%s: %s is not translated", id, k), "")
		}
	}
	return g
}

func (c *argoConv) rootsLeaves(g *groupResult) {
	in := map[string]bool{}
	for _, n := range g.nodes {
		in[n] = true
	}
	hasUp, hasDown := map[string]bool{}, map[string]bool{}
	for _, e := range c.wf.Edges {
		if in[e.From] && in[e.To] {
			hasUp[e.To], hasDown[e.From] = true, true
		}
	}
	for _, n := range g.nodes {
		if !hasUp[n] {
			g.roots = append(g.roots, n)
		}
		if !hasDown[n] {
			g.leaves = append(g.leaves, n)
		}
	}
}

func (c *argoConv) taskByID(id string) *IRTask {
	for _, t := range c.wf.Tasks {
		if t.ID == id {
			return t
		}
	}
	return &IRTask{}
}

func topo(order []string, deps map[string][]string) ([]string, bool) {
	known := map[string]bool{}
	for _, n := range order {
		known[n] = true
	}
	var out []string
	state := map[string]int{}
	cyclic := false
	var visit func(string)
	visit = func(n string) {
		switch state[n] {
		case 1:
			cyclic = true
			return
		case 2:
			return
		}
		state[n] = 1
		for _, d := range deps[n] {
			if known[d] {
				visit(d)
			}
		}
		state[n] = 2
		out = append(out, n)
	}
	for _, n := range order {
		visit(n)
	}
	return out, cyclic
}

// ── leaf templates ──────────────────────────────────────────────────────────

func (c *argoConv) leafTask(id, tmplName string, tmpl *yaml.Node, inputs map[string]binding,
	arts map[string]binding, itemInputs []string, loop *yaml.Node, caller *argoScope, line int) *groupResult {
	id = c.unique(id)
	isScript := mapGet(tmpl, "script") != nil
	body := mapGet(tmpl, "container")
	if isScript {
		body = mapGet(tmpl, "script")
	}
	t := &IRTask{ID: id, Line: line, Kind: KindContainer, Executor: "kubernetes",
		Inputs: map[string]ValueDecl{}, Outputs: map[string]ValueDecl{}, InputParams: map[string]string{},
		InputLiterals: map[string]string{}, Env: map[string]string{}}
	c.wf.Tasks = append(c.wf.Tasks, t)
	lf := &argoLeaf{task: t, tmplName: tmplName, script: isScript, outParam: map[string]outDecl{},
		outArt: map[string]string{}, needOut: map[string]bool{}, line: line}
	c.leaves = append(c.leaves, lf)

	t.Image = scalar(mapGet(body, "image"))
	if t.Image == "" {
		t.Image = scalar(mapGet(mapGet(c.defaults, "container"), "image"))
	}
	if t.Image == "" {
		c.find(line, "image", Unsupported, id+": the container has no image", "")
		t.Kind, t.Reason = KindPlaceholder, "the container has no image"
		return &groupResult{nodes: []string{id}, roots: []string{id}, leaves: []string{id}, leaf: lf}
	}
	scope := &argoScope{inputs: inputs, arts: arts}
	strs := func(n *yaml.Node) [][]seg {
		var out [][]seg
		if n == nil {
			return nil
		}
		for _, x := range n.Content {
			out = append(out, c.segments(x.Value, scope, id, x.Line, lf))
		}
		return out
	}
	lf.command = strs(mapGet(body, "command"))
	lf.args = strs(mapGet(body, "args"))
	if isScript {
		src := mapGet(body, "source")
		lf.source = c.segments(scalar(src), scope, id, line, lf)
	}
	if wd := scalar(mapGet(body, "workingDir")); wd != "" {
		t.WorkingDir = wd
	}
	c.env(t, mapGet(body, "env"), id)
	if c.defaults != nil {
		c.env(t, mapGet(mapGet(c.defaults, "container"), "env"), id)
	}
	for _, k := range []string{"volumeMounts", "securityContext", "ports", "envFrom", "volumeDevices", "lifecycle"} {
		if n := mapGet(body, k); n != nil {
			c.find(n.Line, k, Unsupported, fmt.Sprintf("%s: container %s is not translated by the Nexus kubernetes "+
				"executor", id, k), "")
		}
	}
	// Resources.
	res := mapGet(body, "resources")
	if res == nil && c.defaults != nil {
		res = mapGet(mapGet(c.defaults, "container"), "resources")
	}
	if res == nil && c.podRes != nil {
		res = c.podRes
	}
	if res != nil {
		k := &K8sResources{Requests: quantities(mapGet(res, "requests")), Limits: quantities(mapGet(res, "limits"))}
		if len(k.Requests)+len(k.Limits) > 0 {
			t.K8sResources = k
		}
	}
	// Outputs the template can produce.
	if out := mapGet(tmpl, "outputs"); out != nil {
		if ps := mapGet(out, "parameters"); ps != nil {
			for _, p := range ps.Content {
				pn := scalar(mapGet(p, "name"))
				vf := mapGet(p, "valueFrom")
				if path := scalar(mapGet(vf, "path")); path != "" {
					d := outDecl{path: path}
					if dv := mapGet(vf, "default"); dv != nil {
						d.def, d.hasDef = dv.Value, true
					}
					lf.outParam[pn] = d
				} else {
					c.find(p.Line, "output parameter", Unsupported, fmt.Sprintf("%s: output parameter %s is not "+
						"read from a file (valueFrom.path)", id, pn), "")
				}
			}
		}
		if as := mapGet(out, "artifacts"); as != nil {
			for _, a := range as.Content {
				an := scalar(mapGet(a, "name"))
				if path := scalar(mapGet(a, "path")); path != "" {
					lf.outArt[an] = path
				}
			}
		}
	}
	// Input artifacts.
	if in := mapGet(tmpl, "inputs"); in != nil {
		if as := mapGet(in, "artifacts"); as != nil {
			for _, a := range as.Content {
				an := scalar(mapGet(a, "name"))
				path := scalar(mapGet(a, "path"))
				if raw := mapGet(a, "raw"); raw != nil {
					lf.inArt = append(lf.inArt, inArtifact{name: an, path: path, raw: scalar(mapGet(raw, "data")),
						isRaw: true})
					c.find(a.Line, "raw artifact", Translated, fmt.Sprintf("%s: raw artifact %s written to %s before "+
						"the command", id, an, path), "runtime (shell prelude)")
					continue
				}
				b, ok := arts[an]
				if !ok {
					kind := "an external source"
					for _, s := range []string{"s3", "gcs", "http", "git", "oss", "hdfs", "artifactory", "azure"} {
						if mapGet(a, s) != nil {
							kind = s
						}
					}
					c.find(a.Line, "input artifact", Unsupported, fmt.Sprintf("%s: input artifact %s comes from %s, "+
						"which Nexus does not fetch; fetch it in the task", id, an, kind), "")
					continue
				}
				if b.kind != "output" {
					c.find(a.Line, "input artifact", Unsupported, fmt.Sprintf("%s: input artifact %s: %s", id, an,
						b.reason), "")
					continue
				}
				nin := sanitizeIdentifier(an)
				t.Inputs[nin] = ValueDecl{Type: "json", Required: true}
				c.addEdge(b.node, id, map[string]string{nin: b.output}, a.Line)
				lf.inArt = append(lf.inArt, inArtifact{name: nin, path: path})
				c.find(a.Line, "input artifact", Approximated, fmt.Sprintf("%s: artifact %s travels through the "+
					"Nexus result store as a task output and is copied to %s before the command (files only, not "+
					"directories; large files should be passed by reference)", id, an, path), "edge "+b.node+" -> "+id)
			}
		}
	}
	c.retry(t, tmpl, id)
	c.timeouts(t, tmpl, id)
	c.leafExtras(t, tmpl, id)

	// The loop.
	if loop != nil && !c.loop(t, loop, itemInputs, caller, id) {
		// Without its collection the task cannot run as written.
		t.Kind, t.Reason = KindPlaceholder, "its loop (withItems/withParam/withSequence) was not converted"
		t.Map = nil
		return &groupResult{nodes: []string{id}, roots: []string{id}, leaves: []string{id}, leaf: lf}
	}
	c.find(line, map[bool]string{true: "script", false: "container"}[isScript], Translated,
		fmt.Sprintf("%s: template %s (image %s)", id, tmplName, t.Image), "tasks["+id+"].runtime (kubernetes executor)")
	return &groupResult{nodes: []string{id}, roots: []string{id}, leaves: []string{id}, leaf: lf}
}

func (c *argoConv) addEdge(from, to string, inputs map[string]string, line int) {
	c.wf.Edges = append(c.wf.Edges, IREdge{From: from, To: to, Inputs: inputs, Line: line})
}

func quantities(n *yaml.Node) map[string]string {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	out := map[string]string{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		out[n.Content[i].Value] = n.Content[i+1].Value
	}
	return out
}

func (c *argoConv) env(t *IRTask, n *yaml.Node, id string) {
	if n == nil {
		return
	}
	for _, e := range n.Content {
		name := scalar(mapGet(e, "name"))
		if _, set := t.Env[name]; set {
			continue
		}
		if v := mapGet(e, "value"); v != nil {
			if argoRefRe.MatchString(v.Value) {
				c.find(v.Line, "env", Unsupported, fmt.Sprintf("%s: env %s uses a template reference; Nexus env "+
					"values are static", id, name), "")
			}
			t.Env[name] = v.Value
			continue
		}
		if vf := mapGet(e, "valueFrom"); vf != nil {
			what := "a valueFrom source"
			if mapGet(vf, "secretKeyRef") != nil {
				what = "a Kubernetes Secret (secretKeyRef); Nexus SecretRefs are not resolved by workers yet, so " +
					"provide it through the worker's environment"
			} else if mapGet(vf, "configMapKeyRef") != nil {
				what = "a ConfigMap (configMapKeyRef)"
			} else if mapGet(vf, "fieldRef") != nil {
				what = "a pod field (fieldRef)"
			}
			c.find(e.Line, "env", Unsupported, fmt.Sprintf("%s: env %s comes from %s", id, name, what), "")
		}
	}
}

func (c *argoConv) retry(t *IRTask, tmpl *yaml.Node, id string) {
	rs := mapGet(tmpl, "retryStrategy")
	if rs == nil && c.defaults != nil {
		rs = mapGet(c.defaults, "retryStrategy")
	}
	t.Retry = &Retry{MaxAttempts: 1}
	if rs == nil {
		return
	}
	lim := mapGet(rs, "limit")
	if lim == nil {
		t.Retry.MaxAttempts = 100
		c.find(rs.Line, "retryStrategy", Approximated, id+": a retryStrategy without a limit retries forever; "+
			"capped at 100 attempts (the schema maximum)", "retry.max_attempts=100")
	} else if n, err := strconv.Atoi(lim.Value); err == nil {
		if n > 99 {
			n = 99
		}
		t.Retry.MaxAttempts = n + 1
		c.find(lim.Line, "retryStrategy.limit", Translated, fmt.Sprintf("%s: limit %s", id, lim.Value),
			fmt.Sprintf("retry.max_attempts=%d", n+1))
	} else {
		c.find(lim.Line, "retryStrategy.limit", Unsupported, fmt.Sprintf("%s: limit %q is not a number", id,
			lim.Value), "")
	}
	if b := mapGet(rs, "backoff"); b != nil {
		if d := mapGet(b, "duration"); d != nil {
			if ms, ok := argoDurationMs(d.Value); ok {
				t.Retry.InitialMs = ms
			}
		}
		t.Retry.Strategy, t.Retry.Jitter = "fixed", "none"
		if f := mapGet(b, "factor"); f != nil {
			if v, err := strconv.ParseFloat(f.Value, 64); err == nil && v > 1 {
				t.Retry.Strategy, t.Retry.Multiplier = "exponential", v
			}
		}
		if cp := mapGet(b, "cap"); cp != nil {
			if ms, ok := argoDurationMs(cp.Value); ok {
				t.Retry.MaxMs = ms
			}
		}
		c.find(b.Line, "retryStrategy.backoff", Translated, fmt.Sprintf("%s: backoff %s", id, t.Retry.Strategy),
			"retry")
		if md := mapGet(b, "maxDuration"); md != nil {
			if ms, ok := argoDurationMs(md.Value); ok {
				if t.Timeouts == nil {
					t.Timeouts = &Timeouts{}
				}
				t.Timeouts.ScheduleToCloseMs = ms
				c.find(md.Line, "backoff.maxDuration", Approximated, fmt.Sprintf("%s: maxDuration %s stops retrying "+
					"after that long; it becomes the task's total budget across attempts", id, md.Value),
					"timeouts.schedule_to_close_ms")
			}
		}
	}
	switch p := scalar(mapGet(rs, "retryPolicy")); p {
	case "", "OnFailure":
		t.Retry.RetryOn = []string{"application", "timeout", "oom"}
		if t.Retry.MaxAttempts > 1 {
			c.find(rs.Line, "retryPolicy", Approximated, id+": OnFailure (Argo's default) -> retry on "+
				"application, timeout and oom errors", "retry.retry_on")
		}
	case "OnError":
		t.Retry.RetryOn = []string{"infrastructure", "worker_lost", "transient"}
		c.find(mapGet(rs, "retryPolicy").Line, "retryPolicy", Approximated, id+": OnError -> retry on "+
			"infrastructure, worker_lost and transient errors", "retry.retry_on")
	case "OnTransientError":
		t.Retry.RetryOn = []string{"transient"}
		c.find(mapGet(rs, "retryPolicy").Line, "retryPolicy", Translated, id+": OnTransientError", "retry.retry_on")
	case "Always":
		c.find(mapGet(rs, "retryPolicy").Line, "retryPolicy", Approximated, id+": Always -> the platform's default "+
			"retryable error classes", "retry")
	default:
		c.find(mapGet(rs, "retryPolicy").Line, "retryPolicy", Unsupported, id+": retryPolicy "+p, "")
	}
	if e := mapGet(rs, "expression"); e != nil {
		c.find(e.Line, "retryStrategy.expression", Unsupported, id+": a retry expression is evaluated by the Argo "+
			"controller; Nexus retries by error class", "")
	}
	if a := mapGet(rs, "affinity"); a != nil && mapGet(a, "nodeAntiAffinity") != nil {
		t.Retry.RetryOnDifferentWorker = true
		c.find(a.Line, "nodeAntiAffinity", Translated, id+": retry on a different node",
			"retry.retry_on_different_worker")
	}
	if t.Retry.MaxAttempts <= 1 {
		t.Retry.RetryOn = nil
	}
}

func (c *argoConv) timeouts(t *IRTask, tmpl *yaml.Node, id string) {
	ad := mapGet(tmpl, "activeDeadlineSeconds")
	if ad == nil && c.defaults != nil {
		ad = mapGet(c.defaults, "activeDeadlineSeconds")
	}
	if ad != nil {
		if s, err := strconv.ParseInt(ad.Value, 10, 64); err == nil {
			if t.Timeouts == nil {
				t.Timeouts = &Timeouts{}
			}
			t.Timeouts.StartToCloseMs = s * 1000
			c.find(ad.Line, "activeDeadlineSeconds", Translated, fmt.Sprintf("%s: %ds per attempt", id, s),
				"timeouts.start_to_close_ms")
		}
	}
	if to := mapGet(tmpl, "timeout"); to != nil {
		if ms, ok := argoDurationMs(to.Value); ok {
			if t.Timeouts == nil {
				t.Timeouts = &Timeouts{}
			}
			t.Timeouts.ScheduleToCloseMs = ms
			c.find(to.Line, "timeout", Approximated, fmt.Sprintf("%s: timeout %s bounds the template including "+
				"retries -> schedule_to_close", id, to.Value), "timeouts.schedule_to_close_ms")
		}
	}
}

func (c *argoConv) leafExtras(t *IRTask, tmpl *yaml.Node, id string) {
	if m := mapGet(tmpl, "memoize"); m != nil {
		t.Cache = &Cache{Enabled: true}
		if ma := mapGet(m, "maxAge"); ma != nil {
			if ms, ok := argoDurationMs(ma.Value); ok {
				t.Cache.TTLMs = ms
			}
		}
		c.find(m.Line, "memoize", Approximated, id+": memoization keyed by Nexus on task_ref + version + input "+
			"hash, not on the template's key expression", "cache")
	}
	if s := mapGet(tmpl, "synchronization"); s != nil {
		if mu := mapGet(s, "mutex"); mu != nil && scalar(mapGet(mu, "name")) != "" {
			t.ConcurrencyKey, t.ConcurrencyLimit = "argo-mutex."+scalar(mapGet(mu, "name")), 1
			c.find(s.Line, "synchronization.mutex", Translated, id+": mutex", "concurrency_key + concurrency_limit 1")
		} else {
			c.find(s.Line, "synchronization", Unsupported, id+": a semaphore's limit lives in a ConfigMap; set "+
				"concurrency_key/concurrency_limit by hand", "")
		}
	}
	for _, k := range []string{"nodeSelector", "tolerations", "affinity", "volumes", "sidecars", "initContainers",
		"podSpecPatch", "serviceAccountName", "securityContext", "daemon", "hostAliases", "priorityClassName",
		"schedulerName", "automountServiceAccountToken", "executor", "parallelism"} {
		if n := mapGet(tmpl, k); n != nil {
			c.find(n.Line, k, Unsupported, fmt.Sprintf("%s: template %s is not translated", id, k), "")
		}
	}
	for _, k := range []string{"metadata", "archiveLocation", "priority", "metrics", "outputs.artifacts"} {
		if n := mapGet(tmpl, k); n != nil {
			c.find(n.Line, k, Approximated, fmt.Sprintf("%s: template %s is an Argo setting; ignored", id, k), "")
		}
	}
}

func (c *argoConv) loop(t *IRTask, task *yaml.Node, itemInputs []string, caller *argoScope, id string) bool {
	item := "item"
	if len(itemInputs) > 1 {
		c.find(task.Line, "loop", Unsupported, fmt.Sprintf("%s: several inputs take {{item}} (%s); a Nexus map "+
			"delivers the item through one input", id, strings.Join(itemInputs, ", ")), "")
	}
	if len(itemInputs) > 0 {
		item = sanitizeIdentifier(itemInputs[0])
	}
	t.Inputs[item] = ValueDecl{Type: "json", Required: true}
	m := &IRMap{ItemInput: item}
	switch {
	case mapGet(task, "withItems") != nil:
		n := mapGet(task, "withItems")
		var items []any
		if err := n.Decode(&items); err != nil {
			c.find(n.Line, "withItems", Unsupported, id+": withItems is not a list", "")
			return false
		}
		b, _ := json.Marshal(normalizeYAML(items))
		m.LiteralItemsJSON, m.MaxFanout = string(b), max(1, len(items))
		c.find(n.Line, "withItems", Approximated, fmt.Sprintf("%s: a loop over %d literal items: a generated task "+
			"emits them and %s maps over them (max_fanout %d)", id, len(items), id, m.MaxFanout), "map")
	case mapGet(task, "withSequence") != nil:
		n := mapGet(task, "withSequence")
		items, err := argoSequence(n)
		if err != nil {
			c.find(n.Line, "withSequence", Unsupported, id+": "+err.Error(), "")
			return false
		}
		b, _ := json.Marshal(items)
		m.LiteralItemsJSON, m.MaxFanout = string(b), max(1, len(items))
		c.find(n.Line, "withSequence", Approximated, fmt.Sprintf("%s: a sequence of %d numbers, emitted by a "+
			"generated task and mapped over (max_fanout %d)", id, len(items), m.MaxFanout), "map")
	default:
		n := mapGet(task, "withParam")
		b := c.resolveValue(n.Value, caller, false, n.Line)
		switch b.kind {
		case "output":
			m.Over, m.OverOutput = b.node, b.output
			c.addEdge(b.node, t.ID, nil, n.Line)
			c.find(n.Line, "withParam", Translated, fmt.Sprintf("%s: map over the output of %s", id, b.node),
				"map.over_node_id="+b.node)
		case "param":
			if p, ok := c.wf.Params[b.param]; ok {
				p.Type = "json"
				c.wf.Params[b.param] = p
			}
			m.OverParam = b.param
			c.find(n.Line, "withParam", Approximated, fmt.Sprintf("%s: map over param %s, now declared json (pass "+
				"a JSON list)", id, b.param), "map")
		default:
			c.find(n.Line, "withParam", Unsupported, fmt.Sprintf("%s: withParam %s: %s", id, n.Value, b.reason), "")
			return false
		}
	}
	t.Map = m
	return true
}

func argoSequence(n *yaml.Node) ([]any, error) {
	get := func(k string) (int, bool, error) {
		v := mapGet(n, k)
		if v == nil {
			return 0, false, nil
		}
		if argoRefRe.MatchString(v.Value) {
			return 0, true, fmt.Errorf("withSequence %s is a run-time value (%s)", k, v.Value)
		}
		i, err := strconv.Atoi(v.Value)
		return i, true, err
	}
	if f := mapGet(n, "format"); f != nil {
		return nil, fmt.Errorf("withSequence format %q (printf formatting) is not translated", f.Value)
	}
	count, hasCount, err := get("count")
	if err != nil {
		return nil, err
	}
	start, _, err := get("start")
	if err != nil {
		return nil, err
	}
	end, hasEnd, err := get("end")
	if err != nil {
		return nil, err
	}
	var out []any
	switch {
	case hasCount:
		for i := 0; i < count; i++ {
			out = append(out, strconv.Itoa(start+i))
		}
	case hasEnd:
		step := 1
		if end < start {
			step = -1
		}
		for i := start; ; i += step {
			out = append(out, strconv.Itoa(i))
			if i == end {
				break
			}
		}
	default:
		return nil, fmt.Errorf("withSequence needs count or end")
	}
	if len(out) > 10000 {
		return nil, fmt.Errorf("withSequence of %d items exceeds the fan-out ceiling", len(out))
	}
	return out, nil
}

// normalizeYAML converts yaml.v3's map[string]interface{} trees to JSON-able ones.
func normalizeYAML(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = normalizeYAML(x)
		}
	case map[any]any:
		m := map[string]any{}
		for k, x := range t {
			m[fmt.Sprint(k)] = normalizeYAML(x)
		}
		return m
	case []any:
		for i, x := range t {
			t[i] = normalizeYAML(x)
		}
	}
	return v
}

// argoDurationMs parses Argo durations: a bare number is seconds.
func argoDurationMs(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(n * 1000), true
	}
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64); err == nil {
			return int64(n * 86400000), true
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, false
	}
	return d.Milliseconds(), true
}

// segments splits a template string into literal text and run-time references,
// resolved in the LEAF template's scope.
func (c *argoConv) segments(s string, sc *argoScope, id string, line int, lf *argoLeaf) []seg {
	var out []seg
	last := 0
	for _, r := range argoRefRe.FindAllStringSubmatchIndex(s, -1) {
		if r[0] > last {
			out = append(out, seg{lit: s[last:r[0]]})
		}
		m := argoRefRe.FindStringSubmatch(s[r[0]:r[1]])
		ref := m[2]
		var b binding
		input := ""
		parts := strings.Split(ref, ".")
		switch {
		case m[1] == "":
			switch {
			case len(parts) == 3 && parts[0] == "inputs" && parts[1] == "parameters":
				b, input = c.resolveRef(false, ref, sc, false), parts[2]
				if bb, ok := sc.inputs[parts[2]]; ok && bb.kind == "item" {
					b = bb
				}
			case len(parts) == 4 && parts[0] == "inputs" && parts[1] == "artifacts" && parts[3] == "path":
				b = binding{kind: "unknown", reason: "input artifact path"}
				if as := mapGet(mapGet(c.templates[lf.tmplName], "inputs"), "artifacts"); as != nil {
					for _, a := range as.Content {
						if scalar(mapGet(a, "name")) == parts[2] {
							b = binding{kind: "literal", literal: scalar(mapGet(a, "path"))}
						}
					}
				}
			case len(parts) == 4 && parts[0] == "outputs" && parts[1] == "parameters" && parts[3] == "path":
				b = binding{kind: "unknown", reason: "{{" + ref + "}}"}
			default:
				b = c.resolveRef(false, ref, sc, false)
				if b.kind == "param" {
					input = b.param
				}
			}
		default:
			b = c.resolveRef(true, ref, sc, false)
		}
		switch b.kind {
		case "literal":
			out = append(out, seg{lit: b.literal})
		case "unknown":
			c.find(line, "template reference", Unsupported, fmt.Sprintf("%s: %s", id, b.reason), "")
			out = append(out, seg{lit: s[r[0]:r[1]]})
		default:
			bb := b
			out = append(out, seg{ref: &bb, input: sanitizeIdentifier(orDefault(input, "value"))})
		}
		last = r[1]
	}
	if last < len(s) {
		out = append(out, seg{lit: s[last:]})
	}
	return out
}

func literalOnly(ss []seg) (string, bool) {
	var b strings.Builder
	for _, s := range ss {
		if s.ref != nil {
			return "", false
		}
		b.WriteString(s.lit)
	}
	return b.String(), true
}

// finish assembles every leaf's command once all downstream references are
// known (an output is only captured if something reads it).
func (c *argoConv) finish() {
	for _, lf := range c.leaves {
		c.assemble(lf)
	}
	sort.SliceStable(c.wf.Findings, func(i, j int) bool { return c.wf.Findings[i].Line < c.wf.Findings[j].Line })
}

var shellNames = map[string]bool{"sh": true, "bash": true, "/bin/sh": true, "/bin/bash": true, "ash": true,
	"/bin/ash": true, "zsh": true, "/usr/bin/env": false}

func (c *argoConv) assemble(lf *argoLeaf) {
	t := lf.task
	if t.Kind == KindPlaceholder {
		return
	}
	id := t.ID
	runtime := false
	approx := []string{}
	envUsed := false
	declare := func(ss []seg) {
		for _, s := range ss {
			if s.ref == nil {
				continue
			}
			runtime = true
			switch s.ref.kind {
			case "param":
				t.Inputs[s.input] = ValueDecl{Type: "json", Required: true}
				t.InputParams[s.input] = s.ref.param
			case "output":
				t.Inputs[s.input] = ValueDecl{Type: "json", Required: true}
				c.addEdge(s.ref.node, id, map[string]string{s.input: s.ref.output}, lf.line)
			case "item":
				if t.Map != nil {
					t.Map.ItemInput = s.input
				}
				t.Inputs[s.input] = ValueDecl{Type: "json", Required: true}
			case "env":
				envUsed = true
			}
		}
	}
	for _, x := range lf.command {
		declare(x)
	}
	for _, x := range lf.args {
		declare(x)
	}
	declare(lf.source)
	if t.Map != nil {
		if _, ok := t.Inputs[t.Map.ItemInput]; !ok {
			t.Inputs[t.Map.ItemInput] = ValueDecl{Type: "json", Required: true}
		}
		// A leftover "item" input declared before the item was named.
		if t.Map.ItemInput != "item" {
			if d, ok := t.Inputs["item"]; ok && d.Required {
				if !c.inputUsed(lf, "item") {
					delete(t.Inputs, "item")
				}
			}
		}
	}

	subst := func(s seg) string {
		if s.ref.kind == "env" {
			return s.ref.env
		}
		return `$(nexus_in ` + s.input + `)`
	}
	shellText := func(ss []seg) string {
		var b strings.Builder
		for _, s := range ss {
			if s.ref == nil {
				b.WriteString(s.lit)
			} else {
				b.WriteString(subst(s))
			}
		}
		return b.String()
	}
	word := func(ss []seg) string {
		var b strings.Builder
		b.WriteByte('"')
		for _, s := range ss {
			if s.ref == nil {
				r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")
				b.WriteString(r.Replace(s.lit))
			} else {
				b.WriteString(subst(s))
			}
		}
		b.WriteByte('"')
		return b.String()
	}

	// Pre/post actions for artifacts and output parameters.
	var pre, post []string
	for _, a := range lf.inArt {
		dir := a.path[:max(0, strings.LastIndex(a.path, "/"))]
		if dir == "" {
			dir = "/"
		}
		if a.isRaw {
			pre = append(pre, fmt.Sprintf("mkdir -p %s && printf '%%s' %s > %s", shellQuote(dir), shellQuote(a.raw),
				shellQuote(a.path)))
		} else {
			pre = append(pre, fmt.Sprintf("mkdir -p %s && cp \"$NEXUS_INPUT_DIR/%s\" %s", shellQuote(dir), a.name,
				shellQuote(a.path)))
		}
	}
	for _, k := range sortedKeys(lf.needOut) {
		switch {
		case k == "result":
			t.Outputs["result"] = ValueDecl{Type: "json"}
			t.StdoutOutput = "result"
			c.find(lf.line, "outputs.result", Translated, id+": stdout is output result", "runtime.stdout_output")
		case strings.HasPrefix(k, "param:"):
			name := strings.TrimPrefix(k, "param:")
			d := lf.outParam[name]
			on := sanitizeIdentifier(name)
			t.Outputs[on] = ValueDecl{Type: "json"}
			line := fmt.Sprintf("cp %s \"$NEXUS_OUTPUT_DIR/%s\"", shellQuote(d.path), on)
			if d.hasDef {
				line += fmt.Sprintf(" 2>/dev/null || printf '%%s' %s > \"$NEXUS_OUTPUT_DIR/%s\"", shellQuote(d.def), on)
			}
			post = append(post, line)
			approx = append(approx, fmt.Sprintf("output parameter %s is copied from %s to $NEXUS_OUTPUT_DIR after "+
				"the command", name, d.path))
		case strings.HasPrefix(k, "art:"):
			name := strings.TrimPrefix(k, "art:")
			on := sanitizeIdentifier(name)
			t.Outputs[on] = ValueDecl{Type: "json"}
			post = append(post, fmt.Sprintf("cp %s \"$NEXUS_OUTPUT_DIR/%s\"", shellQuote(lf.outArt[name]), on))
			approx = append(approx, fmt.Sprintf("output artifact %s (%s) is stored as a Nexus output (files only)",
				name, lf.outArt[name]))
		}
	}
	for name, path := range lf.outArt {
		if !lf.needOut["art:"+name] {
			c.find(lf.line, "output artifact", Approximated, fmt.Sprintf("%s: output artifact %s (%s) is not read "+
				"by any converted task, so it is not stored; Argo would archive it", id, name, path), "")
		}
	}

	cmd := lf.command
	args := lf.args
	if lf.script {
		// Argo appends the script's file path to command. The same effect,
		// without a file: `-c SOURCE` for shells and python, a heredoc otherwise.
		var head []string
		for _, x := range cmd {
			s, _ := literalOnly(x)
			head = append(head, s)
		}
		src := lf.source
		switch {
		case len(head) >= 1 && shellNames[head[0]]:
			if head[len(head)-1] == "-c" || strings.HasPrefix(head[len(head)-1], "-") && strings.Contains(head[len(head)-1], "c") {
				args = [][]seg{src}
			} else {
				cmd, args = append(append([][]seg{}, cmd...), []seg{{lit: "-c"}}), [][]seg{src}
			}
		case len(head) >= 1 && (strings.HasSuffix(head[0], "python") || strings.HasSuffix(head[0], "python3")):
			if runtimeRefs(src) {
				c.find(lf.line, "script source", Approximated, id+": run-time {{...}} values are substituted "+
					"into the Python source by sh, textually, as Argo does; reading them from $NEXUS_INPUT_DIR in "+
					"the script is more robust", "tasks["+id+"].runtime")
			}
			cmd, args = append(append([][]seg{}, cmd...), []seg{{lit: "-c"}}), [][]seg{src}
		case len(head) >= 1 && strings.HasSuffix(head[0], "node"):
			cmd, args = append(append([][]seg{}, cmd...), []seg{{lit: "-e"}}), [][]seg{src}
		default:
			text, _ := literalOnly(src)
			heredoc := "f=$(mktemp) && cat > \"$f\" <<'NEXUS_SCRIPT_EOF'\n" + text + "\nNEXUS_SCRIPT_EOF\n"
			var words []string
			for _, x := range cmd {
				words = append(words, word(x))
			}
			approx = append(approx, "the script is written to a temporary file by sh (the image needs a shell)")
			cmd = [][]seg{{{lit: "sh"}}, {{lit: "-c"}}}
			args = [][]seg{{{lit: heredoc + strings.Join(words, " ") + " \"$f\""}}}
		}
	}

	// Is the original already `SHELL -c SCRIPT`?
	isShellC := false
	var shell string
	var scriptIdx int // index into the concatenated command+args of the script
	all := append(append([][]seg{}, cmd...), args...)
	if len(cmd) >= 1 {
		h, ok := literalOnly(cmd[0])
		if ok && shellNames[h] {
			for i := 1; i < len(all); i++ {
				f, ok := literalOnly(all[i])
				if !ok {
					break
				}
				if strings.HasPrefix(f, "-") && strings.Contains(f, "c") {
					if i+1 < len(all) {
						isShellC, shell, scriptIdx = true, h, i+1
					}
					break
				}
				if !strings.HasPrefix(f, "-") {
					break
				}
			}
		}
	}

	if !runtime && len(pre) == 0 && len(post) == 0 {
		t.Command = flatten(cmd)
		t.Args = flatten(args)
	} else {
		helper := ""
		if runtime && !envOnly(all) {
			helper = shellHelper
		}
		var main string
		if isShellC {
			script := shellText(all[scriptIdx])
			if len(pre)+len(post) > 0 {
				main = "(\n" + script + "\n)"
			} else {
				main = script
			}
			// Anything after the script is $0, $1, ... of the original shell.
			if scriptIdx+1 < len(all) {
				var extra []string
				for _, x := range all[scriptIdx+1:] {
					extra = append(extra, word(x))
				}
				main = "set -- " + strings.Join(extra[1:], " ") + "\n" + main
				if len(extra) == 1 {
					main = strings.TrimPrefix(main, "set -- \n")
				}
			}
			if runtime {
				approx = append(approx, "run-time values are substituted into the shell script from "+
					"$NEXUS_INPUT_DIR (a placeholder inside single quotes will not expand)")
			}
		} else {
			shell = "sh"
			var words []string
			for _, x := range all {
				words = append(words, word(x))
			}
			main = strings.Join(words, " ")
			if len(post) == 0 {
				main = "exec " + main
			}
			approx = append(approx, "the command runs under sh so run-time values and file copies can be applied "+
				"(the image needs a shell)")
		}
		var b strings.Builder
		b.WriteString(helper)
		for _, p := range pre {
			b.WriteString(p + " || exit 1\n")
		}
		b.WriteString(main)
		b.WriteString("\n")
		if len(post) > 0 {
			b.WriteString("rc=$?\n")
			for _, p := range post {
				b.WriteString(p + "\n")
			}
			b.WriteString("exit $rc\n")
		}
		t.Command = []string{shell, "-c"}
		t.Args = []string{strings.TrimRight(b.String(), "\n")}
	}
	if envUsed {
		approx = append(approx, "{{workflow.name}}/{{pod.name}} become $NEXUS_RUN_ID/$NEXUS_ATTEMPT_ID")
	}
	for _, a := range approx {
		c.find(lf.line, "command", Approximated, id+": "+a, "tasks["+id+"].runtime")
	}
	if len(t.Inputs) == 0 {
		t.Inputs = nil
	}
}

func (c *argoConv) inputUsed(lf *argoLeaf, name string) bool {
	check := func(ss []seg) bool {
		for _, s := range ss {
			if s.ref != nil && s.input == name {
				return true
			}
		}
		return false
	}
	for _, x := range append(append([][]seg{}, lf.command...), lf.args...) {
		if check(x) {
			return true
		}
	}
	return check(lf.source)
}

func runtimeRefs(ss []seg) bool {
	for _, s := range ss {
		if s.ref != nil {
			return true
		}
	}
	return false
}

func envOnly(all [][]seg) bool {
	for _, x := range all {
		for _, s := range x {
			if s.ref != nil && s.ref.kind != "env" {
				return false
			}
		}
	}
	return true
}

func flatten(x [][]seg) []string {
	var out []string
	for _, s := range x {
		v, _ := literalOnly(s)
		out = append(out, v)
	}
	return out
}
