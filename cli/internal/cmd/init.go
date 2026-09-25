// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/output"
	"github.com/khuriwalgroup/kgf-nexus/cli/internal/templates"
)

func initCmd() *command {
	return &command{
		name:    "init",
		summary: "Create a new workflow project from a template.",
		usage: `nexusctl init <template> [dir] [flags]
nexusctl init --list

Write a starter project: dag.py (SDK decorators, sensible resources, retries,
timeouts and caching), tests for its task functions, requirements.txt, a
Dockerfile for the task image, and a README.

Templates:
` + templateHelp() + `
  --name NAME        Workflow name (default: the directory's name). Also the
                     task_ref prefix and image name.
  --namespace NS     Namespace written into the spec (default: the resolved
                     --namespace / NEXUS_NAMESPACE / config, else "default").
  --gpu              Request GPUs where the template supports it
                     (batch-inference).
  --force            Write into a non-empty directory, overwriting files the
                     template provides. Other files are never touched.
  --list             List the templates.

Nothing is contacted: init only writes files.

Exit codes: 0 written; 2 unknown template, bad name, or the directory is not
empty (without --force).

Examples:
  nexusctl init etl orders-etl
  nexusctl init batch-inference scoring --gpu --namespace ml
  cd orders-etl && pytest && nexusctl workflow compile dag.py --show`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var name string
			var gpu, force, list bool
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.StringVar(&name, "name", "", "Workflow name (default: the directory name)")
				fs.BoolVar(&gpu, "gpu", false, "Request GPUs where the template supports it")
				fs.BoolVar(&force, "force", false, "Write into a non-empty directory")
				fs.BoolVar(&list, "list", false, "List the templates")
			})
			if err != nil {
				return err
			}
			if list {
				return printTemplates(env)
			}
			if len(rest) < 1 || len(rest) > 2 {
				return usagef("expected a template and an optional directory\n"+
					"Usage: nexusctl init <template> [dir]\nTemplates: %s", strings.Join(templates.Names(), ", "))
			}
			tmpl, ok := templates.Lookup(rest[0])
			if !ok {
				return usagef("unknown template %q\nAvailable: %s\nSee: nexusctl init --list",
					rest[0], strings.Join(templates.Names(), ", "))
			}
			if gpu && !tmpl.GPUOption {
				return usagef("template %q has no GPU option (--gpu applies to: %s)", tmpl.Name, gpuTemplates())
			}

			dir := tmpl.Name
			if len(rest) == 2 {
				dir = rest[1]
			}
			if name == "" {
				name = workflowNameFrom(dir)
			}
			if err := templates.ValidateIdentifier("workflow name", name); err != nil {
				return usagef("%v\n  Pass --name to choose one.", err)
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

			rendered, err := templates.Render(tmpl.Name, templates.Vars{Name: name, Namespace: namespace, GPU: gpu})
			if err != nil {
				return &usageError{msg: err.Error()}
			}
			if err := checkTarget(dir, force); err != nil {
				return err
			}
			written := make([]string, 0, len(rendered))
			for _, f := range rendered {
				dst := filepath.Join(dir, filepath.FromSlash(f.Path))
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
					return fmt.Errorf("creating %s: %w", filepath.Dir(dst), err)
				}
				if err := os.WriteFile(dst, f.Content, fs.FileMode(f.Mode)); err != nil {
					return fmt.Errorf("writing %s: %w", dst, err)
				}
				written = append(written, f.Path)
			}

			result := map[string]any{
				"template": tmpl.Name, "dir": dir, "name": name, "namespace": namespace,
				"gpu": gpu, "files": written,
			}
			if done, perr := printStructured(env, result); done {
				return perr
			}
			fmt.Fprintf(env.IO.Out, "Created %s project %q in %s (namespace %s)\n\n", tmpl.Name, name, dir, namespace)
			for _, f := range written {
				fmt.Fprintf(env.IO.Out, "  %s\n", f)
			}
			fmt.Fprintf(env.IO.Out, `
Next:
  cd %s
  python -m venv .venv && . .venv/bin/activate && pip install -r requirements.txt pytest
  pytest                                    # test the task functions locally
  nexusctl workflow compile dag.py --show   # the exact spec the server will see
  docker build -t %s .                      # the task image (IMAGE in dag.py)
  nexusctl workflow submit dag.py
`, dir, name+":0.1.0")
			return nil
		},
	}
}

func templateHelp() string {
	var b strings.Builder
	for _, t := range templates.List() {
		fmt.Fprintf(&b, "  %-17s  %s\n", t.Name, t.Summary)
	}
	return b.String()
}

func gpuTemplates() string {
	var names []string
	for _, t := range templates.List() {
		if t.GPUOption {
			names = append(names, t.Name)
		}
	}
	return strings.Join(names, ", ")
}

func printTemplates(env *Environment) error {
	p, err := env.Printer()
	if err != nil {
		return err
	}
	raw := make([]any, 0)
	t := &output.Table{Columns: []string{"template", "gpu", "description"}}
	for _, tm := range templates.List() {
		raw = append(raw, map[string]any{"name": tm.Name, "gpu_option": tm.GPUOption, "summary": tm.Summary})
		gpu := "-"
		if tm.GPUOption {
			gpu = "--gpu"
		}
		t.Rows = append(t.Rows, []string{tm.Name, gpu, tm.Summary})
	}
	return p.Print(raw, t)
}

var nonIdent = regexp.MustCompile(`[^a-z0-9_.-]+`)

// workflowNameFrom derives a schema-valid workflow name from a directory:
// "./My Orders ETL" -> "my-orders-etl".
func workflowNameFrom(dir string) string {
	base := filepath.Base(filepath.Clean(dir))
	if abs, err := filepath.Abs(dir); err == nil && (base == "." || base == string(filepath.Separator)) {
		base = filepath.Base(abs)
	}
	n := nonIdent.ReplaceAllString(strings.ToLower(base), "-")
	return strings.Trim(n, "-_.")
}

// checkTarget refuses to scatter files into a directory that already holds
// something, unless --force. An init that silently overwrote someone's dag.py
// would be a very bad first experience.
func checkTarget(dir string, force bool) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		st, serr := os.Stat(dir)
		if serr == nil && !st.IsDir() {
			return usagef("%s exists and is not a directory", dir)
		}
		return fmt.Errorf("reading %s: %w", dir, err)
	}
	if len(entries) > 0 && !force {
		return usagef("%s is not empty; choose another directory, or pass --force to write the template's "+
			"files into it (other files are left alone)", dir)
	}
	return nil
}
