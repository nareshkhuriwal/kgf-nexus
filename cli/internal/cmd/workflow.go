// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/output"
	"github.com/khuriwalgroup/kgf-nexus/cli/internal/spec"
)

func workflowGroup() *group {
	return &group{
		name:    "workflow",
		summary: "Submit, compile, and inspect workflow specs.",
		commands: []*command{
			workflowSubmitCmd(),
			workflowCompileCmd(),
			workflowVersionsCmd(),
		},
	}
}

func workflowSubmitCmd() *command {
	return &command{
		name:    "submit",
		summary: "Submit a workflow spec (JSON or YAML).",
		usage: `nexusctl workflow submit <file> [flags]

Submit a WorkflowSpec. The file may be JSON or YAML (detected from content, so
stdin works), or a Python file that uses the SDK -- in which case nexusctl runs
YOUR interpreter to compile it, because only your process ever executes your code.

Use "-" to read the spec from stdin.

Specs are immutable and content-addressed: resubmitting a byte-identical spec is
a no-op that returns the existing version, and any change creates a new version.
A run pins its spec version for its lifetime, so editing a workflow never
mutates a run in flight.

  --dry-run    Validate only; do not register. Use this in CI.
  --python     Interpreter used to compile a .py spec (default "python3").
  --workflow   Workflow object to compile, when a .py file defines several.

Examples:
  nexusctl workflow submit pipeline.yaml
  nexusctl workflow submit pipeline.yaml --dry-run
  nexusctl workflow submit dag.py --workflow daily_etl
  cat spec.json | nexusctl workflow submit -`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var dryRun bool
			var python, wfName string
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.BoolVar(&dryRun, "dry-run", false, "Validate only; do not register the spec")
				fs.StringVar(&python, "python", "python3", "Interpreter used to compile a .py spec")
				fs.StringVar(&wfName, "workflow", "", "Workflow object to compile from a .py file")
			})
			if err != nil {
				return err
			}
			if len(rest) != 1 {
				return fmt.Errorf("expected exactly one spec file (or \"-\" for stdin)\n" +
					"Usage: nexusctl workflow submit <file> [--dry-run]")
			}

			doc, err := loadSpec(rest[0], python, wfName, env.IO.In)
			if err != nil {
				return err
			}

			// Local checks first: a missing task_ref should not need a round trip.
			// The server still runs JSON Schema and the semantic checks; this is
			// only a faster feedback loop for the obvious mistakes.
			if err := localProblems(doc); err != nil {
				return err
			}

			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.SubmitSpec(ctx, doc.JSON, dryRun)
			if err != nil {
				return annotateSubmitError(err, doc)
			}

			p, err := env.Printer()
			if err != nil {
				return err
			}
			if !env.IsTable() {
				return p.Print(result, nil)
			}

			m, _ := result.(map[string]any)
			if m == nil {
				m = map[string]any{}
			}
			verb := "Submitted"
			if dryRun {
				verb = "Validated"
			}
			fmt.Fprintf(env.IO.Out, "%s %s\n", verb, doc.Name())
			fmt.Fprintf(env.IO.Out, "  local spec hash:  %s\n", doc.Hash)
			if h := output.Str(m, "spec_hash"); h != "-" {
				fmt.Fprintf(env.IO.Out, "  server spec hash: %s\n", h)
				if h != doc.Hash {
					// A divergence means the two canonicalizers disagree, which
					// would silently split one workflow into two versions. Loud
					// on purpose.
					fmt.Fprintf(env.IO.Err,
						"\nWARNING: the server computed a different content address than nexusctl.\n"+
							"This is a canonicalization bug and should be reported: the same spec\n"+
							"would register as two different versions.\n")
				}
			}
			if v := output.Str(m, "version"); v != "-" {
				fmt.Fprintf(env.IO.Out, "  version:          %s\n", v)
			}
			if created, ok := m["created"].(bool); ok && !created {
				fmt.Fprintf(env.IO.Out, "  (unchanged -- this spec was already registered)\n")
			}
			if dryRun {
				fmt.Fprintf(env.IO.Out, "\nDry run: nothing was registered.\n")
			} else {
				fmt.Fprintf(env.IO.Out, "\nStart a run with:\n  nexusctl run create %s\n", doc.Name())
			}
			return nil
		},
	}
}

func workflowCompileCmd() *command {
	return &command{
		name:    "compile",
		summary: "Print exactly the spec the server will see.",
		usage: `nexusctl workflow compile <file> [--show] [flags]

Print the canonical WorkflowSpec document and its content address WITHOUT
submitting anything.

This exists for one specific failure mode called out in ADR-0004: the SDK
compiles your decorators to a document, and that document may not be the graph
you pictured. This command is the mitigation -- it shows you the document the
control plane will actually parse, so "my Python compiled to a graph I did not
expect" is a thing you can see rather than deduce from a failed run.

The printed hash is the content address the server will independently compute.
If they ever differ, that is a canonicalization bug, not a cosmetic one.

  --show       Print the full spec (implied when --output is json or yaml).
  --hash-only  Print only the content address, for CI comparisons.
  --validate   Also run the local structural checks.
  --python     Interpreter used to compile a .py spec (default "python3").
  --workflow   Workflow object to compile, when a .py file defines several.

Examples:
  nexusctl workflow compile dag.py --show
  nexusctl workflow compile pipeline.yaml --show --output yaml
  nexusctl workflow compile dag.py --hash-only`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var show, hashOnly, validate bool
			var python, wfName string
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.BoolVar(&show, "show", false, "Print the full canonical spec")
				fs.BoolVar(&hashOnly, "hash-only", false, "Print only the content address")
				fs.BoolVar(&validate, "validate", false, "Run local structural checks")
				fs.StringVar(&python, "python", "python3", "Interpreter used to compile a .py spec")
				fs.StringVar(&wfName, "workflow", "", "Workflow object to compile from a .py file")
			})
			if err != nil {
				return err
			}
			if len(rest) != 1 {
				return fmt.Errorf("expected exactly one spec file (or \"-\" for stdin)\n" +
					"Usage: nexusctl workflow compile <file> --show")
			}

			doc, err := loadSpec(rest[0], python, wfName, env.IO.In)
			if err != nil {
				return err
			}
			if validate {
				if err := localProblems(doc); err != nil {
					return err
				}
			}

			if hashOnly {
				fmt.Fprintln(env.IO.Out, doc.Hash)
				return nil
			}

			format, err := output.ParseFormat(env.Globals.Output)
			if err != nil {
				return err
			}
			switch format {
			case output.FormatJSON:
				// Indented, because a human asked to see it. The canonical bytes
				// are what gets hashed and submitted; this rendering is not.
				pretty, err := doc.Indented()
				if err != nil {
					return err
				}
				fmt.Fprintf(env.IO.Out, "%s\n", pretty)
				return nil
			case output.FormatYAML:
				y, err := doc.YAML()
				if err != nil {
					return err
				}
				fmt.Fprintf(env.IO.Out, "%s", y)
				return nil
			}

			// Table mode: a summary, plus the document itself when --show.
			fmt.Fprintf(env.IO.Out, "Workflow:     %s\n", doc.Name())
			if ns, ok := doc.Value["namespace"].(string); ok && ns != "" {
				fmt.Fprintf(env.IO.Out, "Namespace:    %s\n", ns)
			}
			fmt.Fprintf(env.IO.Out, "Spec hash:    %s\n", doc.Hash)
			fmt.Fprintf(env.IO.Out, "Source:       %s\n", doc.Source)

			tasks, _ := doc.Value["tasks"].([]any)
			edges, _ := doc.Value["edges"].([]any)
			fmt.Fprintf(env.IO.Out, "Tasks:        %d\n", len(tasks))
			fmt.Fprintf(env.IO.Out, "Edges:        %d\n\n", len(edges))

			if err := printTaskTable(env, tasks); err != nil {
				return err
			}
			if len(edges) > 0 {
				fmt.Fprintln(env.IO.Out)
				if err := printEdgeTable(env, edges); err != nil {
					return err
				}
			}
			if show {
				pretty, err := doc.Indented()
				if err != nil {
					return err
				}
				fmt.Fprintf(env.IO.Out, "\n--- canonical WorkflowSpec (what the server parses) ---\n%s\n", pretty)
			} else {
				fmt.Fprintf(env.IO.Out, "\nPass --show to print the full document.\n")
			}
			return nil
		},
	}
}

func printTaskTable(env *Environment, tasks []any) error {
	t := &output.Table{
		Columns: []string{"id", "task_ref", "ver", "queue", "cpu", "memory", "gpu", "idempotency", "retries"},
		Empty:   "This spec declares no tasks.",
	}
	for _, raw := range tasks {
		task, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		cpu, mem, gpu := resourceCells(task)
		retries := "-"
		if r, ok := task["retry"].(map[string]any); ok {
			retries = fmt.Sprintf("%d", output.Int(r, "max_attempts"))
		}
		t.Rows = append(t.Rows, []string{
			output.Str(task, "id"),
			output.Str(task, "task_ref"),
			output.Str(task, "version"),
			output.Str(task, "queue"),
			cpu, mem, gpu,
			shortEnum(output.Str(task, "idempotency"), "IDEMPOTENCY_MODE_"),
			retries,
		})
	}
	return output.New(output.FormatTable, env.IO.Out).Print(nil, t)
}

func resourceCells(task map[string]any) (cpu, mem, gpu string) {
	cpu, mem, gpu = "-", "-", "-"
	res, ok := task["resources"].(map[string]any)
	if !ok {
		return
	}
	req, ok := res["request"].(map[string]any)
	if !ok {
		return
	}
	cpu = output.CPU(output.Int(req, "cpu_millis"))
	mem = output.Bytes(output.Int(req, "memory_bytes"))
	if n := output.Int(req, "gpu_count"); n > 0 {
		gpu = fmt.Sprintf("%d", n)
		if kind := output.Str(req, "gpu_kind"); kind != "-" {
			gpu += " " + kind
		}
	}
	return
}

func printEdgeTable(env *Environment, edges []any) error {
	t := &output.Table{Columns: []string{"from", "to", "output -> input"}}
	for _, raw := range edges {
		e, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		mapping := "-"
		if m, ok := e["output_to_input"].(map[string]any); ok && len(m) > 0 {
			var parts []string
			// Sorted so repeated output is diffable.
			for _, in := range output.SortedKeys(m) {
				parts = append(parts, fmt.Sprintf("%s -> %s", output.Str(m, in), in))
			}
			mapping = strings.Join(parts, ", ")
		}
		t.Rows = append(t.Rows, []string{
			output.Str(e, "from_node_id"),
			output.Str(e, "to_node_id"),
			mapping,
		})
	}
	return output.New(output.FormatTable, env.IO.Out).Print(nil, t)
}

func workflowVersionsCmd() *command {
	return &command{
		name:    "versions",
		summary: "List registered versions of a workflow.",
		usage: `nexusctl workflow versions <name>

List the registered spec versions of a workflow. Specs are immutable, so this is
the full history: a past run's exact definition is always recoverable.`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			rest, err := env.parseFlags(args, nil)
			if err != nil {
				return err
			}
			if len(rest) != 1 {
				return fmt.Errorf("expected exactly one workflow name\n" +
					"Usage: nexusctl workflow versions <name>")
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			result, err := c.ListSpecVersions(ctx, rest[0])
			if err != nil {
				return err
			}
			p, err := env.Printer()
			if err != nil {
				return err
			}
			t := &output.Table{
				Columns: []string{"version", "spec_hash", "created_by", "age"},
				Empty:   fmt.Sprintf("No versions of %q are registered in namespace %q.", rest[0], c.Namespace()),
			}
			for _, v := range output.Items(result, "versions") {
				t.Rows = append(t.Rows, []string{
					output.Str(v, "version"),
					shortHash(output.Str(v, "spec_hash")),
					output.Str(v, "created_by"),
					output.Age(v, "created_at"),
				})
			}
			return p.Print(result, t)
		},
	}
}

// loadSpec reads a spec file, compiling a .py file through the user's interpreter.
//
// The CLI does not embed a Python runtime and does not parse Python. ADR-0004's
// guarantee is that only the user's own process executes their code; nexusctl
// shelling out to `python3` keeps that true, and keeps the CLI honest about what
// it is doing.
func loadSpec(path, python, workflowName string, stdin io.Reader) (*spec.Document, error) {
	if !spec.IsPython(path) {
		return spec.Load(path, stdin)
	}
	if python == "" {
		python = "python3"
	}
	raw, sourceMap, err := compilePythonMapped(python, path, workflowName)
	if err != nil {
		return nil, err
	}
	doc, err := spec.Parse(raw, path)
	if err != nil {
		return nil, err
	}
	// Problems in a .py spec are reported at Python lines, not in the JSON it
	// compiled to.
	doc.Raw, doc.SourceMap = nil, sourceMap
	if doc.SourceMap == nil {
		doc.SourceMap = map[string]spec.NodeSource{}
	}
	return doc, nil
}

// pythonCompileScript emits the compiled spec of one @workflow in a module.
//
// Printing the SDK's own canonical bytes rather than re-encoding means the hash
// nexusctl reports is the hash the SDK computed, so the two surfaces cannot drift.
const pythonCompileScript = `
import importlib.util, os, sys
sys.dont_write_bytecode = True
path, wanted = sys.argv[1], (sys.argv[2] if len(sys.argv) > 2 else "")
try:
    from nexus.workflow import WorkflowDefinition
except ModuleNotFoundError:
    sys.stderr.write(
        "the Nexus Python SDK is not importable by this interpreter.\n"
        "Install it with:  pip install -e sdk/python\n"
        "or point nexusctl at the right interpreter with --python.\n")
    raise SystemExit(3)

spec_ = importlib.util.spec_from_file_location(
    os.path.splitext(os.path.basename(path))[0], path)
module = importlib.util.module_from_spec(spec_)
sys.modules[spec_.name] = module
sys.path.insert(0, os.path.dirname(os.path.abspath(path)))
spec_.loader.exec_module(module)

found = {n: v for n, v in vars(module).items()
         if isinstance(v, WorkflowDefinition)}
if not found:
    sys.stderr.write(f"{path} defines no @workflow.\n")
    raise SystemExit(4)
if wanted:
    # Match either the Python identifier or the workflow's declared name, since
    # a user may reasonably reach for either.
    by_name = {v.name: v for v in found.values()}
    wf = found.get(wanted) or by_name.get(wanted)
    if wf is None:
        names = sorted(set(found) | set(by_name))
        sys.stderr.write(
            f"{path} has no workflow {wanted!r}; found: {', '.join(names)}\n")
        raise SystemExit(4)
elif len(found) == 1:
    wf = next(iter(found.values()))
else:
    names = sorted(found)
    sys.stderr.write(
        f"{path} defines {len(found)} workflows ({', '.join(names)}).\n"
        "Select one with --workflow.\n")
    raise SystemExit(4)

sys.stdout.write(wf.compile().json)
`

func compilePython(python, path, workflowName string) ([]byte, error) {
	args := []string{"-c", pythonCompileScript, path}
	if workflowName != "" {
		args = append(args, workflowName)
	}
	cmd := exec.Command(python, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		// Distinguish "interpreter not found" from "your workflow module failed to
		// import": they need completely different fixes from the user.
		if _, lookErr := exec.LookPath(python); lookErr != nil {
			return nil, fmt.Errorf(
				"cannot run %q to compile %s: %v\n"+
					"  Pass --python with the path to an interpreter that has the Nexus SDK installed.",
				python, path, lookErr)
		}
		return nil, fmt.Errorf("compiling %s with %s failed:\n%s", path, python, indent(msg))
	}
	return out, nil
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// shortEnum trims a canonical enum prefix for display. The wire form stays
// canonical; only the table is abbreviated.
func shortEnum(v, prefix string) string {
	if v == "-" {
		return v
	}
	return strings.ToLower(strings.TrimPrefix(v, prefix))
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
