// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package importer converts Airflow DAGs, Prefect flows and Argo Workflows
// into Nexus WorkflowSpecs, with a report that classifies every construct it
// met as translated, approximated or unsupported.
//
// Pipeline: source files -> IR (ir.go; Python sources through the embedded
// static analyzer in pyast/, Argo YAML in argo.go) -> spec (build.go) ->
// validation (validate.go) -> YAML + report (report.go). Schedules are never
// put in a spec (Nexus schedules are separate API objects); they become
// companion `nexusctl schedule create` commands (schedule.go).
package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	clispec "github.com/khuriwalgroup/kgf-nexus/cli/internal/spec"
)

// Sources.
const (
	Airflow = "airflow"
	Prefect = "prefect"
	Argo    = "argo"
)

// Options for Convert.
type Options struct {
	Source    string
	Path      string // a file or a directory
	Namespace string
	Python    string // interpreter for the Airflow/Prefect analyzer
	MaxFanout int    // default max_fanout for maps the source leaves unbounded
}

// Counts tallies findings by status.
type Counts struct {
	Translated   int `json:"translated"`
	Approximated int `json:"approximated"`
	Unsupported  int `json:"unsupported"`
}

func (c *Counts) add(f Finding) {
	switch f.Status {
	case Translated:
		c.Translated++
	case Approximated:
		c.Approximated++
	default:
		c.Unsupported++
	}
}

// Workflow is one converted workflow.
type Workflow struct {
	Name       string      `json:"name"`
	SourceID   string      `json:"source_id"`
	File       string      `json:"file"`
	Line       int         `json:"line"`
	Tasks      int         `json:"tasks"`
	Edges      int         `json:"edges"`
	Hash       string      `json:"hash,omitempty"`
	Output     string      `json:"output,omitempty"`
	Counts     Counts      `json:"counts"`
	Findings   []Finding   `json:"findings"`
	Companions []Companion `json:"companions,omitempty"`
	// Problems are validation failures of the generated spec: converter bugs.
	Problems []string `json:"problems,omitempty"`
	Spec     *Spec    `json:"spec,omitempty"`
	// YAML is the rendered spec, header comment included.
	YAML []byte `json:"-"`
}

// Result is everything Convert produced.
type Result struct {
	Source    string      `json:"source"`
	Input     string      `json:"input"`
	Files     []string    `json:"files"`
	Namespace string      `json:"namespace"`
	Written   bool        `json:"written"`
	Partial   bool        `json:"partial"`
	Summary   Counts      `json:"summary"`
	Workflows []*Workflow `json:"workflows"`
	// Findings not tied to one workflow: unreadable files, syntax errors,
	// module-level constructs.
	Findings []Finding `json:"findings,omitempty"`
}

// Unsupported reports whether any construct could not be converted.
func (r *Result) Unsupported() bool { return r.Summary.Unsupported > 0 }

// InvalidWorkflows lists workflows whose generated spec failed validation.
func (r *Result) InvalidWorkflows() []*Workflow {
	var out []*Workflow
	for _, w := range r.Workflows {
		if len(w.Problems) > 0 {
			out = append(out, w)
		}
	}
	return out
}

// Convert reads the source and converts every workflow found. An error means
// nothing could be analysed (bad path, no interpreter); problems with
// individual constructs are findings in the result.
func Convert(ctx context.Context, opts Options) (*Result, error) {
	if opts.MaxFanout <= 0 {
		opts.MaxFanout = DefaultMaxFanout
	}
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	if opts.Python == "" {
		opts.Python = "python3"
	}
	files, extra, err := collect(opts.Source, opts.Path)
	if err != nil {
		return nil, err
	}
	res := &Result{Source: opts.Source, Input: opts.Path, Files: append(files, extra...), Namespace: opts.Namespace}
	var irFiles []irFile
	switch opts.Source {
	case Airflow, Prefect:
		if len(files) > 0 {
			doc, err := analyzePython(ctx, opts.Python, opts.Source, files)
			if err != nil {
				return nil, err
			}
			irFiles = doc.Files
		}
		if opts.Source == Prefect {
			for _, y := range extra {
				fs := applyPrefectYAML(y, irFiles)
				res.Findings = append(res.Findings, fs...)
			}
		}
	case Argo:
		for _, f := range files {
			irFiles = append(irFiles, analyzeArgo(f))
		}
	default:
		return nil, fmt.Errorf("unknown source %q (airflow, prefect or argo)", opts.Source)
	}

	names := map[string]bool{}
	for _, f := range irFiles {
		if f.Error != "" {
			res.Findings = append(res.Findings, Finding{File: f.Path, Construct: "file", Status: Unsupported,
				Message: f.Error})
		}
		res.Findings = append(res.Findings, f.Findings...)
		for _, ir := range f.Workflows {
			res.Workflows = append(res.Workflows, convertOne(ir, opts, names))
		}
	}
	for _, f := range res.Findings {
		res.Summary.add(f)
	}
	for _, w := range res.Workflows {
		res.Summary.Translated += w.Counts.Translated
		res.Summary.Approximated += w.Counts.Approximated
		res.Summary.Unsupported += w.Counts.Unsupported
	}
	return res, nil
}

func convertOne(ir *IRWorkflow, opts Options, names map[string]bool) *Workflow {
	b := build(ir, BuildOptions{Source: opts.Source, Namespace: opts.Namespace, MaxFanout: opts.MaxFanout})
	if names[b.Spec.Name] {
		n := uniqueName(b.Spec.Name, func(s string) bool { return names[s] })
		b.Findings = append(b.Findings, Finding{File: ir.File, Line: ir.Line, Construct: "name", Status: Approximated,
			Message: fmt.Sprintf("another converted workflow is already named %s; this one is %s", b.Spec.Name, n),
			Nexus:   "name"})
		b.Spec.Name = n
	}
	names[b.Spec.Name] = true
	comps, sfs := schedules(ir, b.Spec.Name, opts.Namespace, opts.Source)
	w := &Workflow{
		Name: b.Spec.Name, SourceID: ir.SourceID, File: ir.File, Line: ir.Line,
		Tasks: len(b.Spec.Tasks), Edges: len(b.Spec.Edges), Spec: b.Spec, Companions: comps,
	}
	w.Findings = append(append(append([]Finding{}, ir.Findings...), b.Findings...), sfs...)
	sortFindings(w.Findings)
	for _, f := range w.Findings {
		w.Counts.add(f)
	}
	w.Problems = validate(b.Spec)
	if len(w.Problems) == 0 {
		raw, _ := json.Marshal(b.Spec)
		if doc, err := clispec.Parse(raw, w.Name); err == nil {
			w.Hash = doc.Hash
		}
	}
	w.YAML = renderYAML(w, opts.Source)
	return w
}

// sortFindings orders by file, line, then status severity, keeping the
// analyzer's order otherwise.
func sortFindings(fs []Finding) {
	rank := map[string]int{Unsupported: 0, Approximated: 1, Translated: 2}
	sort.SliceStable(fs, func(i, j int) bool {
		if fs[i].File != fs[j].File {
			return fs[i].File < fs[j].File
		}
		if fs[i].Line != fs[j].Line {
			return fs[i].Line < fs[j].Line
		}
		return rank[fs[i].Status] < rank[fs[j].Status]
	})
}

func renderYAML(w *Workflow, source string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Generated by `nexusctl import %s` from %s:%d (%s).\n", source, filepath.ToSlash(w.File),
		w.Line, w.SourceID)
	fmt.Fprintf(&b, "# %d translated, %d approximated, %d unsupported. Read the import report before submitting.\n",
		w.Counts.Translated, w.Counts.Approximated, w.Counts.Unsupported)
	if w.Counts.Unsupported > 0 {
		b.WriteString("# PARTIAL: tasks with task_ref nexus-import/unsupported stand in for constructs that were\n")
		b.WriteString("# not converted, and FAIL when run. Replace them before relying on this workflow.\n")
	}
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	_ = enc.Encode(w.Spec)
	_ = enc.Close()
	return b.Bytes()
}

// collect lists the files to analyse, plus side files (prefect.yaml).
func collect(source, path string) ([]string, []string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	want := func(p string) (main, side bool) {
		base := strings.ToLower(filepath.Base(p))
		ext := filepath.Ext(base)
		switch source {
		case Airflow:
			return ext == ".py", false
		case Prefect:
			if base == "prefect.yaml" || base == "prefect.yml" {
				return false, true
			}
			return ext == ".py", false
		case Argo:
			return ext == ".yaml" || ext == ".yml" || ext == ".json", false
		}
		return false, false
	}
	if !st.IsDir() {
		main, side := want(path)
		if side {
			return nil, []string{path}, nil
		}
		if !main && source != Argo {
			return nil, nil, fmt.Errorf("%s is not a .py file", path)
		}
		return []string{path}, nil, nil
	}
	var files, extra []string
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if p != path && (strings.HasPrefix(name, ".") || name == "__pycache__" || name == "node_modules" ||
				name == "venv" || name == ".venv" || name == "site-packages") {
				return filepath.SkipDir
			}
			return nil
		}
		main, side := want(p)
		if main {
			if source == Argo {
				if b, err := os.ReadFile(p); err == nil && !bytes.Contains(b, []byte("argoproj.io")) {
					return nil
				}
			}
			files = append(files, p)
		} else if side {
			extra = append(extra, p)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(files)
	sort.Strings(extra)
	if len(files) == 0 && len(extra) == 0 {
		return nil, nil, fmt.Errorf("no %s files found under %s", source, path)
	}
	return files, extra, nil
}
