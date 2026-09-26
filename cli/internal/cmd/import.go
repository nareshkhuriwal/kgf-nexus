// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/importer"
)

// `nexusctl import airflow|prefect|argo <path>`: convert another
// orchestrator's pipelines into WorkflowSpecs plus a report (importer package).
//
// Nothing is written while any construct is unsupported, unless
// --allow-partial: a CI job that converts and submits must not submit a
// workflow that silently lost a sensor or a branch. The report is written
// either way, because it is what tells you what to fix.

func importGroup() *group {
	return &group{
		name:    "import",
		summary: "Convert Airflow DAGs, Prefect flows or Argo Workflows into Nexus specs.",
		commands: []*command{
			importCmd(importer.Airflow, "Airflow DAG files (.py), statically: nothing is imported or run."),
			importCmd(importer.Prefect, "Prefect 2/3 flow files (.py) and prefect.yaml, statically."),
			importCmd(importer.Argo, "Argo Workflow, WorkflowTemplate and CronWorkflow YAML."),
		},
	}
}

func importCmd(source, summary string) *command {
	pyNote := ""
	if source != importer.Argo {
		pyNote = `
The file is parsed with Python's ast module by an analyzer embedded in
nexusctl, run with YOUR interpreter (--python, like "workflow compile"). It
never imports or executes the file: whatever the analyzer cannot resolve
without running code -- a loop that creates tasks, Variable.get(), a value
computed at run time -- is reported as unsupported with its line.
`
	}
	return &command{
		name:    source,
		summary: summary,
		usage: fmt.Sprintf(`nexusctl import %[1]s <file|dir> [--out FILE | --out-dir DIR] [--report FILE] [flags]

Convert %[2]s into Nexus WorkflowSpecs (YAML), one per workflow found, and a
report classifying every construct as translated, approximated or unsupported,
with its file:line.
%[3]s
Output
  --out FILE        Write the spec to FILE. Only when the input holds exactly
                    one workflow (a spec file holds one document).
  --out-dir DIR     Write one <name>.yaml per workflow into DIR (created).
  (neither)         Print the spec(s) on stdout, separated by "---".
  --report FILE     Write the full report: FILE.json as JSON, anything else as
                    Markdown. Written even when the specs are not.
  -o json|yaml      Print the report (with each written spec inline) on stdout
                    instead of the human summary on stderr.

Safety
  Any UNSUPPORTED construct -> nothing is written and the exit code is 1,
  unless --allow-partial: then the specs are written with each unsupported
  task as a placeholder that FAILS when run (task_ref nexus-import/unsupported),
  and the exit code is 0. A generated spec that fails validation is a converter
  bug: nothing is written, exit 1.

Schedules are not part of a spec in Nexus: the report prints the
"nexusctl schedule create" (and, for Airflow catchup, "backfill start")
commands to run after "workflow submit".

  --allow-partial        Write specs even when constructs are unsupported.
  --namespace NS         Namespace written into the specs (default: the
                         resolved --namespace / NEXUS_NAMESPACE / config).
  --python PATH          Interpreter for the analyzer (default python3).
  --max-fanout N         max_fanout for maps the source leaves unbounded
                         (default %[4]d, Airflow's max_map_length).

Examples:
  nexusctl import %[1]s %[5]s --out-dir nexus/ --report import-report.md
  nexusctl import %[1]s %[5]s --allow-partial -o json > import.json`,
			source, map[string]string{importer.Airflow: "Airflow DAG files", importer.Prefect: "Prefect flows",
				importer.Argo: "Argo Workflows"}[source], pyNote, importer.DefaultMaxFanout,
			map[string]string{importer.Airflow: "dags/", importer.Prefect: "flows/etl.py",
				importer.Argo: "workflows/"}[source]),
		run: func(ctx context.Context, env *Environment, args []string) error {
			return runImport(ctx, env, source, args)
		},
	}
}

func runImport(ctx context.Context, env *Environment, source string, args []string) error {
	var out, outDir, report, python string
	var allowPartial bool
	var maxFanout int
	rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
		fs.StringVar(&out, "out", "", "Write the spec to this file (one workflow only)")
		fs.StringVar(&outDir, "out-dir", "", "Write one <name>.yaml per workflow into this directory")
		fs.StringVar(&report, "report", "", "Write the full report (.json -> JSON, else Markdown)")
		fs.BoolVar(&allowPartial, "allow-partial", false, "Write specs even when constructs are unsupported")
		fs.StringVar(&python, "python", "python3", "Interpreter for the Airflow/Prefect analyzer")
		fs.IntVar(&maxFanout, "max-fanout", importer.DefaultMaxFanout, "max_fanout for unbounded maps")
	})
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("expected exactly one file or directory\nUsage: nexusctl import %s <file|dir> [flags]", source)
	}
	if out != "" && outDir != "" {
		return usagef("--out and --out-dir are mutually exclusive")
	}
	if maxFanout < 1 || maxFanout > 10000 {
		return usagef("--max-fanout must be between 1 and 10000 (the platform's default ceiling), got %d", maxFanout)
	}
	if !env.IsTable() {
		if _, err := env.Printer(); err != nil {
			return err
		}
	}
	namespace := env.Globals.Namespace
	if namespace == "" {
		if cfg, cerr := env.Config(); cerr == nil {
			namespace = cfg.Namespace
		}
	}
	if namespace == "" {
		namespace = "default"
	}

	res, err := importer.Convert(ctx, importer.Options{Source: source, Path: rest[0], Namespace: namespace,
		Python: python, MaxFanout: maxFanout})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return usagef("%v", err)
		}
		return err
	}

	var failure error
	blocked := false
	switch {
	case len(res.Workflows) == 0:
		failure = fmt.Errorf("no %s workflow found in %s", source, rest[0])
		blocked = true
	case len(res.InvalidWorkflows()) > 0:
		var names []string
		for _, w := range res.InvalidWorkflows() {
			names = append(names, w.Name)
		}
		failure = fmt.Errorf("the converter produced an invalid spec for %s; nothing was written. This is a "+
			"nexusctl bug: please report it with the input file and the report", strings.Join(names, ", "))
		blocked = true
	case res.Unsupported() && !allowPartial:
		failure = fmt.Errorf("%d construct(s) could not be converted; nothing was written.\n"+
			"  Review them in the report, then re-run with --allow-partial to write the partial spec(s), in which "+
			"each one is a task that fails when run", res.Summary.Unsupported)
		blocked = true
	case out != "" && len(res.Workflows) > 1:
		return usagef("%s holds %d workflows; --out writes one spec, use --out-dir", rest[0], len(res.Workflows))
	}

	if !blocked {
		if err := writeSpecs(env, res, out, outDir); err != nil {
			return err
		}
		res.Written = true
		res.Partial = res.Unsupported()
	} else {
		for _, w := range res.Workflows {
			w.Spec = nil // a report must not carry a spec that was refused
		}
	}
	if report != "" {
		if err := writeReport(res, report); err != nil {
			return err
		}
	}
	if !env.IsTable() {
		p, perr := env.Printer()
		if perr != nil {
			return perr
		}
		if err := p.Print(res, nil); err != nil {
			return err
		}
	} else {
		fmt.Fprint(env.IO.Err, importer.Summary(res))
		if report != "" {
			fmt.Fprintf(env.IO.Err, "Full report: %s\n", report)
		}
	}
	return failure
}

func writeSpecs(env *Environment, res *importer.Result, out, outDir string) error {
	switch {
	case outDir != "":
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", outDir, err)
		}
		for _, w := range res.Workflows {
			p := filepath.Join(outDir, w.Name+".yaml")
			if err := os.WriteFile(p, w.YAML, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", p, err)
			}
			w.Output = p
		}
	case out != "":
		w := res.Workflows[0]
		if dir := filepath.Dir(out); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", dir, err)
			}
		}
		if err := os.WriteFile(out, w.YAML, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", out, err)
		}
		w.Output = out
	default:
		if !env.IsTable() {
			return nil // the specs are inline in the structured output
		}
		var b bytes.Buffer
		for i, w := range res.Workflows {
			if i > 0 {
				b.WriteString("---\n")
			}
			b.Write(w.YAML)
		}
		_, err := env.IO.Out.Write(b.Bytes())
		return err
	}
	return nil
}

func writeReport(res *importer.Result, path string) error {
	var body []byte
	if strings.EqualFold(filepath.Ext(path), ".json") {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		body = append(b, '\n')
	} else {
		body = []byte(importer.Markdown(res))
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
