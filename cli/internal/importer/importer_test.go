// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	gatewayspec "github.com/khuriwalgroup/kgf-nexus/services/api-gateway/spec"
)

// Regenerate the golden files with: go test ./cli/internal/importer -run TestGolden -update
var update = flag.Bool("update", false, "rewrite the golden files")

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

// analyzerPython returns an interpreter for the static analyzer (stdlib only,
// Python >= 3.8), or skips. NEXUS_TEST_PYTHON selects one explicitly; CI should
// set it so a missing interpreter fails instead of skipping (the templates test
// follows the same convention).
func analyzerPython(t *testing.T) string {
	t.Helper()
	candidates := []string{"python3", "python"}
	if p := os.Getenv("NEXUS_TEST_PYTHON"); p != "" {
		candidates = []string{p}
	}
	for _, c := range candidates {
		if exec.Command(c, "-c", "import sys; assert sys.version_info >= (3, 8)").Run() == nil {
			return c
		}
	}
	if os.Getenv("NEXUS_TEST_PYTHON") != "" {
		t.Fatalf("NEXUS_TEST_PYTHON=%s is not a usable Python >= 3.8", os.Getenv("NEXUS_TEST_PYTHON"))
	}
	t.Skip("no Python >= 3.8; set NEXUS_TEST_PYTHON")
	return ""
}

// sdkPython returns an interpreter that can import the Nexus SDK from
// sdk/python (for the local-run test), with its environment.
func sdkPython(t *testing.T) (string, []string) {
	t.Helper()
	env := append(os.Environ(), "PYTHONPATH="+filepath.Join(repoRoot(t), "sdk", "python"),
		"PYTHONDONTWRITEBYTECODE=1")
	candidates := []string{"python3", "python"}
	if p := os.Getenv("NEXUS_TEST_PYTHON"); p != "" {
		candidates = []string{p}
	}
	for _, c := range candidates {
		cmd := exec.Command(c, "-c", "import sys, nexus.local; assert sys.version_info >= (3, 11)")
		cmd.Env = env
		if cmd.Run() == nil {
			return c, env
		}
	}
	if os.Getenv("NEXUS_TEST_PYTHON") != "" {
		t.Fatalf("NEXUS_TEST_PYTHON=%s cannot import the Nexus SDK", os.Getenv("NEXUS_TEST_PYTHON"))
	}
	t.Skip("no Python >= 3.11 that can import the SDK; set NEXUS_TEST_PYTHON")
	return "", nil
}

type goldenCase struct {
	source, path string
}

func (c goldenCase) name() string {
	base := filepath.Base(c.path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	kind := "upstream"
	if strings.Contains(c.path, "/cases/") {
		kind = "cases"
	}
	return c.source + "/" + kind + "-" + base
}

func goldenCases(t *testing.T) []goldenCase {
	t.Helper()
	var out []goldenCase
	for _, src := range []string{Airflow, Prefect, Argo} {
		for _, dir := range []string{"testdata/upstream/" + src, "testdata/cases/" + src} {
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				p := dir + "/" + e.Name()
				switch {
				case e.IsDir():
					out = append(out, goldenCase{src, p})
				case strings.HasSuffix(e.Name(), ".py") && src != Argo,
					strings.HasSuffix(e.Name(), ".yaml") && src == Argo:
					out = append(out, goldenCase{src, p})
				}
			}
		}
	}
	return out
}

func convert(t *testing.T, c goldenCase, python string) *Result {
	t.Helper()
	res, err := Convert(context.Background(), Options{Source: c.source, Path: c.path, Namespace: "team-a",
		Python: python, MaxFanout: DefaultMaxFanout})
	if err != nil {
		t.Fatalf("%s: %v", c.path, err)
	}
	return res
}

func platformSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "schemas", "workflow-spec-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("workflow-spec-v1.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("workflow-spec-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// checkPlatform validates a generated spec exactly as the platform would:
// the canonical JSON Schema, then the gateway's full submit pipeline (schema,
// DAG semantics, map rules, fan-in bound, timeout coherence, stdout_output).
func checkPlatform(t *testing.T, schema *jsonschema.Schema, w *Workflow) {
	t.Helper()
	raw, err := json.Marshal(w.Spec)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(doc); err != nil {
		t.Errorf("%s: spec does not validate against schemas/workflow-spec-v1.json:\n%v", w.Name, err)
	}
	// The YAML nexusctl writes (header comment included) is what gets submitted.
	compiled, err := gatewayspec.Compile(w.YAML, gatewayspec.FormatYAML)
	if err != nil {
		t.Errorf("%s: the gateway's submit pipeline rejects the spec:\n%v\n%s", w.Name, err, w.YAML)
		return
	}
	if compiled.Hash != w.Hash {
		t.Errorf("%s: nexusctl's content address %s differs from the gateway's %s", w.Name, w.Hash, compiled.Hash)
	}
}

// TestGolden converts every vendored upstream example and every hand-written
// case, compares spec and report with the golden files, and runs every
// generated spec through the schema and the gateway's submit pipeline.
func TestGolden(t *testing.T) {
	python := analyzerPython(t)
	schema := platformSchema(t)
	for _, c := range goldenCases(t) {
		t.Run(c.name(), func(t *testing.T) {
			res := convert(t, c, python)
			if len(res.Workflows) == 0 {
				t.Fatalf("no workflow found in %s", c.path)
			}
			for _, w := range res.InvalidWorkflows() {
				t.Errorf("%s: converter produced an invalid spec: %v", w.Name, w.Problems)
			}
			var specs bytes.Buffer
			for i, w := range res.Workflows {
				if i > 0 {
					specs.WriteString("---\n")
				}
				specs.Write(w.YAML)
				if !bytes.Contains(w.YAML, []byte("\napi_version: nexus.dev/v1\n")) {
					t.Errorf("%s: not rendered as block YAML (the round-trip fallback fired)", w.Name)
				}
				checkPlatform(t, schema, w)
			}
			golden(t, "testdata/golden/"+c.name()+".spec.yaml", specs.Bytes())
			golden(t, "testdata/golden/"+c.name()+".report.md", []byte(Markdown(res)))
		})
	}
}

func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("%s differs from the golden file; re-run with -update if the change is intended.\n--- got ---\n%s",
			path, got)
	}
}

// ── specific constructs ─────────────────────────────────────────────────────

func findings(w *Workflow, construct, status string) []Finding {
	var out []Finding
	for _, f := range w.Findings {
		if f.Construct == construct && f.Status == status {
			out = append(out, f)
		}
	}
	return out
}

func mustFind(t *testing.T, w *Workflow, construct, status string, line int) Finding {
	t.Helper()
	for _, f := range findings(w, construct, status) {
		if line == 0 || f.Line == line {
			return f
		}
	}
	t.Fatalf("%s: no %s finding for %q at line %d; have:\n%s", w.Name, status, construct, line, dump(w.Findings))
	return Finding{}
}

func dump(fs []Finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(f.Status + " " + filepath.Base(f.File) + ":" + itoa(f.Line) + " " + f.Construct + ": " +
			f.Message + "\n")
	}
	return b.String()
}

func itoa(i int) string { return strconv.Itoa(i) }

func edgeSet(s *Spec) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, e := range s.Edges {
		out[e.From+"->"+e.To] = e.OutputToInput
	}
	return out
}

func task(t *testing.T, s *Spec, id string) *TaskSpec {
	t.Helper()
	for i := range s.Tasks {
		if s.Tasks[i].ID == id {
			return &s.Tasks[i]
		}
	}
	var ids []string
	for _, x := range s.Tasks {
		ids = append(ids, x.ID)
	}
	t.Fatalf("no task %s in %s (have %v)", id, s.Name, ids)
	return nil
}

func one(t *testing.T, r *Result) *Workflow {
	t.Helper()
	if len(r.Workflows) != 1 {
		t.Fatalf("want one workflow, got %d", len(r.Workflows))
	}
	return r.Workflows[0]
}

func TestAirflowDependencyShapes(t *testing.T) {
	python := analyzerPython(t)
	w := one(t, convert(t, goldenCase{Airflow, "testdata/cases/airflow/tricky_dependencies.py"}, python))
	if w.Name != "tricky_dependencies" {
		t.Errorf("name %q", w.Name)
	}
	mustFind(t, w, ">>", Unsupported, 40)    // [a, b] >> [c, d]
	mustFind(t, w, "chain", Unsupported, 46) // chain() between lists of different lengths
	edges := edgeSet(w.Spec)
	for _, e := range []string{
		"a->b", "a->c", "b->d", "c->e", "d->f", "e->f", // chain(a, [b, c], [d, e], f): pairwise
		"a->e", "a->f", "b->e", "b->f", // cross_downstream
		"c->f", // f.set_upstream([c, d])
		"f->greet", "greet->outer.first", "outer.first->outer.inner.work", "outer.inner.work->outer.inner.done",
		"outer.inner.done->report", // group -> task uses the group's leaves
	} {
		if _, ok := edges[e]; !ok {
			t.Errorf("missing edge %s", e)
		}
	}
	for _, e := range []string{"a->d", "b->c"} { // the rejected list>>list must not become edges
		if _, ok := edges[e]; ok {
			t.Errorf("unexpected edge %s", e)
		}
	}
	g := task(t, w.Spec, "greet")
	if g.TriggerRule != "all_done" || g.Queue != "high-memory" || g.ConcurrencyLimit != 2 ||
		g.Priority == nil || *g.Priority != 104 {
		t.Errorf("greet: %+v", g)
	}
	if g.Retry == nil || g.Retry.MaxAttempts != 3 || g.Retry.Strategy != "exponential" || g.Retry.InitialMs != 30000 ||
		g.Retry.MaxMs != 600000 || g.Timeouts == nil || g.Timeouts.StartToCloseMs != 900000 {
		t.Errorf("greet retry/timeouts from default_args: %+v %+v", g.Retry, g.Timeouts)
	}
	if g.InputParams["greeting"] != "greeting" || !strings.Contains(g.Runtime.Args[0], "$(nexus_param greeting)") {
		t.Errorf("{{ params.greeting }} not bound: %+v", g)
	}
	r := task(t, w.Spec, "report")
	if r.TriggerRule != "" || r.Retry.MaxAttempts != 1 {
		t.Errorf("report keeps all_success and retries=0: %+v", r)
	}
	mustFind(t, w, "trigger_rule", Unsupported, 65)
	mustFind(t, w, "Jinja", Unsupported, 67) // {{ ds }}
	mustFind(t, w, "pool", Unsupported, 56)
	if len(w.Companions) != 1 {
		t.Fatalf("companions: %+v", w.Companions)
	}
	c := w.Companions[0]
	if c.Cron != "0 0 * * *" || c.Timezone != "Europe/Amsterdam" || c.Overlap != "queue_one" ||
		!strings.Contains(c.Backfill, "--from 2026-01-01") {
		t.Errorf("companion: %+v", c)
	}
}

func TestAirflowMappingAndUnsupported(t *testing.T) {
	python := analyzerPython(t)
	res := convert(t, goldenCase{Airflow, "testdata/cases/airflow/dynamic_and_unsupported.py"}, python)
	w := one(t, res)
	// Module-level Variable.get is reported with its line, outside the workflow.
	found := false
	for _, f := range res.Findings {
		if f.Construct == "Variable.get" && f.Status == Unsupported && f.Line == 14 {
			found = true
		}
	}
	if !found {
		t.Errorf("Variable.get not reported: %v", res.Findings)
	}
	// expand over an upstream output: a map over list_files.result, partial() as a param binding.
	p := task(t, w.Spec, "process")
	if p.Map == nil || p.Map.OverNodeID != "list_files" || p.Map.OverOutput != "result" || p.Map.ItemInput != "path" ||
		p.Map.MaxFanout != DefaultMaxFanout || p.InputParams["batch"] != "batch" {
		t.Errorf("process: %+v map=%+v", p, p.Map)
	}
	// expand over a literal: a generated emitter task.
	sq := task(t, w.Spec, "square")
	if sq.Map == nil || sq.Map.OverNodeID != "square__items" || sq.Map.MaxFanout != 3 {
		t.Errorf("square: %+v", sq.Map)
	}
	items := task(t, w.Spec, "square__items")
	if items.Runtime.Args[2] != "[1, 2, 3]" {
		t.Errorf("items emitter: %+v", items.Runtime)
	}
	// Fan-in: total reads process's children as a JSON array.
	if b := edgeSet(w.Spec)["process->total"]; b["counts"] != "result" {
		t.Errorf("fan-in edge: %v", b)
	}
	for _, id := range []string{"wait_for_file", "branch", "pick", "shard_N", "echo_each"} {
		if task(t, w.Spec, id).TaskRef != "nexus-import/unsupported" {
			t.Errorf("%s should be a failing placeholder", id)
		}
	}
	mustFind(t, w, "FileSensor", Unsupported, 51)
	mustFind(t, w, "BranchPythonOperator", Unsupported, 52)
	mustFind(t, w, "for", Unsupported, 71)
	mustFind(t, w, "expand", Unsupported, 49) // two expand arguments
	tr := task(t, w.Spec, "train")
	if tr.Runtime.Executor != "kubernetes" || tr.Runtime.Image != "ghcr.io/acme/train:1.4" ||
		tr.Resources == nil || tr.Resources.Request.CPUMillis != 2000 || tr.Resources.Request.GPUCount != 1 ||
		tr.Resources.Limit.MemoryBytes != 16<<30 {
		t.Errorf("train: %+v %+v", tr.Runtime, tr.Resources)
	}
	if w.Spec.RunTimeoutMs != 7200000 {
		t.Errorf("dagrun_timeout: %d", w.Spec.RunTimeoutMs)
	}
	if c := w.Companions[0]; c.Cron != "0 */6 * * *" {
		t.Errorf("timedelta(hours=6): %+v", c)
	}
}

func TestPrefectFlowsAndDeployments(t *testing.T) {
	python := analyzerPython(t)
	res := convert(t, goldenCase{Prefect, "testdata/cases/prefect/deploy"}, python)
	if len(res.Workflows) != 2 {
		t.Fatalf("workflows: %d", len(res.Workflows))
	}
	var etl, report *Workflow
	for _, w := range res.Workflows {
		switch w.Name {
		case "nightly-etl":
			etl = w
		case "report":
			report = w
		}
	}
	if etl == nil || report == nil {
		t.Fatalf("names: %v, %v", res.Workflows[0].Name, res.Workflows[1].Name)
	}
	s := etl.Spec
	edges := edgeSet(s)
	if edges["list_shards->process"] == nil && !hasEdge(s, "list_shards", "process") {
		t.Error("map source edge missing")
	}
	if !hasEdge(s, "warm_up", "process") {
		t.Error("wait_for=[ready] edge missing")
	}
	p := task(t, s, "process")
	if p.Map == nil || p.Map.OverNodeID != "list_shards" || p.Map.ItemInput != "shard" || p.InputParams["mode"] != "mode" {
		t.Errorf("process.map(shards, unmapped(mode)): %+v %+v", p.Map, p.InputParams)
	}
	if p.Cache == nil || !p.Cache.Enabled || p.Cache.TTLMs != 3600000 || p.Retry.Strategy != "exponential" ||
		p.Retry.InitialMs != 5000 {
		t.Errorf("process options: %+v %+v", p.Cache, p.Retry)
	}
	ls := task(t, s, "list_shards")
	if ls.Retry.MaxAttempts != 4 || ls.Retry.Multiplier != 10 || ls.Retry.MaxMs != 100000 ||
		ls.Timeouts.StartToCloseMs != 300000 {
		t.Errorf("list_shards: %+v", ls.Retry)
	}
	if edgeSet(s)["process->combine"]["counts"] != "result" {
		t.Error("fan-in into combine missing")
	}
	if task(t, s, "notify").TaskRef != "nexus-import/unsupported" {
		t.Error("a task under `if total.result() > limit` must be a placeholder")
	}
	if task(t, s, "report").TaskRef != "nexus-import/unsupported" {
		t.Error("a subflow call must be a placeholder")
	}
	mustFind(t, etl, "if", Unsupported, 43)
	mustFind(t, etl, "retries", Unsupported, 36)
	if s.RunTimeoutMs != 3600000 || s.Params["limit"].Type != "int" || s.Params["source"].DefaultJSON != `"s3://bucket/in"` {
		t.Errorf("flow options/params: %+v", s.Params)
	}
	if len(etl.Companions) != 1 || etl.Companions[0].Cron != "0 3 * * *" || etl.Companions[0].Params["mode"] != `"incremental"` {
		t.Errorf("serve(cron=...): %+v", etl.Companions)
	}
	if len(report.Companions) != 2 || report.Companions[0].Timezone != "America/New_York" ||
		report.Companions[1].Cron != "*/30 * * * *" || !report.Companions[1].Disabled {
		t.Errorf("prefect.yaml schedules: %+v", report.Companions)
	}
	for _, f := range report.Findings {
		if strings.Contains(f.Construct, "prefect.yaml") && filepath.Base(f.File) != "prefect.yaml" {
			t.Errorf("a prefect.yaml finding points at %s", f.File)
		}
	}
}

func hasEdge(s *Spec, from, to string) bool {
	for _, e := range s.Edges {
		if e.From == from && e.To == to {
			return true
		}
	}
	return false
}

func TestArgoDependsStepsAndLoops(t *testing.T) {
	w := one(t, convert(t, goldenCase{Argo, "testdata/cases/argo/depends-and-unsupported.yaml"}, ""))
	if task(t, w.Spec, "all-done").TriggerRule != "all_done" {
		t.Error("(B.Succeeded || B.Failed) && (C...) should be all_done")
	}
	mustFind(t, w, "depends", Unsupported, 33) // B || C
	mustFind(t, w, "depends", Unsupported, 36) // A && !C.Skipped
	mustFind(t, w, "when", Unsupported, 41)
	mustFind(t, w, "onExit", Unsupported, 9)
	for _, id := range []string{"remote", "approve", "fields"} {
		if task(t, w.Spec, id).TaskRef != "nexus-import/unsupported" && id != "fields" {
			t.Errorf("%s should be a placeholder", id)
		}
	}
	c := task(t, w.Spec, "C")
	if c.Retry.MaxAttempts != 5 || c.Retry.Strategy != "exponential" || c.Retry.Multiplier != 3 ||
		c.Retry.InitialMs != 5000 || c.Retry.MaxMs != 60000 || !c.Retry.RetryOnDifferentWorker ||
		strings.Join(c.Retry.RetryOn, ",") != "infrastructure,worker_lost,transient" {
		t.Errorf("C retry: %+v", c.Retry)
	}
	if c.ConcurrencyKey != "argo-mutex.flaky-lock" || c.Cache == nil || c.Cache.TTLMs != 3600000 {
		t.Errorf("C mutex/memoize: %+v %+v", c.ConcurrencyKey, c.Cache)
	}
	a := task(t, w.Spec, "A")
	if a.Resources == nil || a.Resources.Request.CPUMillis != 500 || a.Resources.Request.GPUCount != 1 ||
		a.Resources.Limit.MemoryBytes != 1<<30 || a.Timeouts.StartToCloseMs != 600000 {
		t.Errorf("A resources/deadline: %+v %+v", a.Resources, a.Timeouts)
	}
	ps := task(t, w.Spec, "per-shard")
	if ps.Map == nil || ps.Map.OverNodeID != "per-shard__items" || w.Spec.Params["shards"].Type != "json" {
		t.Errorf("withParam over a workflow param: %+v", ps.Map)
	}

	r := one(t, convert(t, goldenCase{Argo, "testdata/cases/argo/runnable-map-reduce.yaml"}, ""))
	s := r.Spec
	if !hasEdge(s, "generate", "shout") || !hasEdge(s, "stamp", "shout") {
		t.Error("a step waits for every step of the previous parallel group")
	}
	sh := task(t, s, "shout")
	if sh.Map == nil || sh.Map.OverNodeID != "generate" || sh.Map.ItemInput != "word" {
		t.Errorf("withParam from outputs.result: %+v", sh.Map)
	}
	if task(t, s, "generate").Runtime.StdoutOutput != "result" {
		t.Error("outputs.result is stdout")
	}
	if edgeSet(s)["stamp->collect"]["stamp"] != "stamp" || edgeSet(s)["shout->collect"]["words"] != "result" {
		t.Errorf("collect bindings: %v", edgeSet(s))
	}
	if r.Counts.Unsupported != 0 {
		t.Errorf("runnable case has unsupported findings:\n%s", dump(r.Findings))
	}
}

func TestDependsRule(t *testing.T) {
	cases := []struct{ expr, rule, status string }{
		{"A", "all_success", Translated},
		{"A && B.Succeeded", "all_success", Translated},
		{"(A.Succeeded || A.Failed) && (B.Failed || B.Succeeded || B.Errored)", "all_done", Approximated},
		{"A && (C.Succeeded || C.Failed)", "", Unsupported},
		{"A || B", "", Unsupported},
		{"!A", "", Unsupported},
		{"A.Failed", "", Unsupported},
		{"A &&", "", Unsupported},
	}
	for _, c := range cases {
		_, rule, status, _ := dependsRule(c.expr)
		if rule != c.rule || status != c.status {
			t.Errorf("%q: got %s/%s, want %s/%s", c.expr, rule, status, c.rule, c.status)
		}
	}
}

func TestIntervalCronAndQuantities(t *testing.T) {
	for sec, want := range map[float64]string{60: "* * * * *", 900: "*/15 * * * *", 3600: "0 * * * *",
		21600: "0 */6 * * *", 86400: "0 0 * * *", 604800: "0 0 * * 0", 172800: "0 0 */2 * *"} {
		if got, ok := intervalCron(sec); !ok || got != want {
			t.Errorf("%gs: %q %v", sec, got, ok)
		}
	}
	for _, sec := range []float64{7, 420, 25200, 3600 * 5, 1.5} {
		if got, ok := intervalCron(sec); ok {
			t.Errorf("%gs should have no cron, got %q", sec, got)
		}
	}
	for q, want := range map[string]int64{"1Gi": 1 << 30, "512Mi": 512 << 20, "1G": 1e9, "100": 100, "1.5Ki": 1536} {
		if got, err := parseQuantity(q); err != nil || got != want {
			t.Errorf("%s: %d %v", q, got, err)
		}
	}
	for q, want := range map[string]int64{"500m": 500, "1": 1000, "0.25": 250, "2": 2000} {
		if got, err := parseCPU(q); err != nil || got != want {
			t.Errorf("cpu %s: %d %v", q, got, err)
		}
	}
}

// TestAnalyzerNeverExecutesUserCode: the analyzer parses; it must never run
// the file, its imports, or anything the file would do at import time.
func TestAnalyzerNeverExecutesUserCode(t *testing.T) {
	python := analyzerPython(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "executed")
	src := `import os
open(` + pyQuote(marker) + `, "w").write("x")
os.system("touch ` + marker + `")
import this_module_does_not_exist
from airflow.sdk import DAG
from airflow.providers.standard.operators.bash import BashOperator
with DAG("safe", schedule=None) as dag:
    BashOperator(task_id="t", bash_command="echo " + str(__import__("os").getpid()))
`
	path := filepath.Join(dir, "evil.py")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Convert(context.Background(), Options{Source: Airflow, Path: path, Python: python})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the analyzer executed the file under analysis")
	}
	w := one(t, res)
	if task(t, w.Spec, "t").TaskRef != "nexus-import/unsupported" {
		t.Error("a bash_command computed at run time must be a placeholder")
	}
}

func pyQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// TestConvertedPipelinesRunLocally runs converted pipelines for real with the
// SDK's local runner (`python -m nexus.local`, the engine behind
// `nexusctl run --local`): shell tasks as subprocesses, and Argo's container
// tasks on the host (--host-executor; the images are alpine/busybox shells).
func TestConvertedPipelinesRunLocally(t *testing.T) {
	analyzer := analyzerPython(t)
	py, env := sdkPython(t)
	for _, c := range []struct {
		gc   goldenCase
		host bool
	}{
		{goldenCase{Airflow, "testdata/cases/airflow/runnable_bash.py"}, false},
		{goldenCase{Airflow, "testdata/upstream/airflow/example_task_group.py"}, false},
		{goldenCase{Argo, "testdata/cases/argo/runnable-map-reduce.yaml"}, true},
		{goldenCase{Argo, "testdata/upstream/argo/dag-diamond.yaml"}, true},
		{goldenCase{Argo, "testdata/upstream/argo/steps.yaml"}, true},
		{goldenCase{Argo, "testdata/upstream/argo/output-parameter.yaml"}, true},
	} {
		t.Run(c.gc.name(), func(t *testing.T) {
			w := one(t, convert(t, c.gc, analyzer))
			if w.Counts.Unsupported != 0 {
				t.Fatalf("not fully convertible:\n%s", dump(w.Findings))
			}
			dir := t.TempDir()
			specPath := filepath.Join(dir, "spec.json")
			raw, _ := json.Marshal(w.Spec)
			if err := os.WriteFile(specPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"-m", "nexus.local", specPath, "--run-dir", filepath.Join(dir, "run"),
				"--backoff-scale", "0", "--output", "json"}
			if c.host {
				args = append(args, "--host-executor")
			}
			cmd := exec.Command(py, args...)
			cmd.Env = env
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("local run failed: %v\n%s\n%s", err, out, stderr.String())
			}
			var result struct {
				State string `json:"state"`
				Nodes []struct {
					NodeID string `json:"node_id"`
					State  string `json:"state"`
				} `json:"nodes"`
			}
			if err := json.Unmarshal(out, &result); err != nil {
				t.Fatalf("run output: %v\n%s", err, out)
			}
			if result.State != "SUCCEEDED" {
				t.Fatalf("run %s:\n%s\n%s", result.State, out, stderr.String())
			}
			var states []string
			for _, n := range result.Nodes {
				states = append(states, n.NodeID+"="+n.State)
			}
			sort.Strings(states)
			t.Logf("%s: %s", w.Name, strings.Join(states, " "))
		})
	}
}
