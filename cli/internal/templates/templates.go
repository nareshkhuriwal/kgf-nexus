// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package templates holds the starter projects `nexusctl init` writes.
//
// Each template is a real project -- a dag.py using the SDK decorators, tests
// for its task functions, a Dockerfile and a README -- embedded in the binary so
// `nexusctl init` works offline and the project matches the CLI that made it.
//
// Substitution is plain token replacement, not text/template: the files are
// Python, whose f-strings and dict literals are full of braces, and a template
// engine would force escaping all of them. Tokens are valid Python identifiers
// (__WORKFLOW_NAME__), so an unrendered file still parses.
package templates

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

//go:embed all:files
var files embed.FS

// Template describes one starter project.
type Template struct {
	Name    string
	Summary string
	// GPUOption reports whether the template honours Vars.GPU.
	GPUOption bool
}

var catalog = []Template{
	{Name: "etl", Summary: "Extract a CSV, validate, transform, and load with idempotent effects."},
	{Name: "ml-training", Summary: "Hyperparameter grid via .map, train, evaluate, select the best, quality gate."},
	{Name: "batch-inference", Summary: "Shard a dataset, .map inference with per-shard checkpoints, aggregate.", GPUOption: true},
	{Name: "file-fanout", Summary: "List objects under a prefix, process each, fan in a manifest with max_bytes."},
	{Name: "llm-batch-inference", Summary: "Shard prompts, .map LLM calls under shared rate limits, resume mid-shard from checkpoints, total tokens."},
	{Name: "llm-eval", Summary: "Answer a reference set, grade (exact, regex, LLM-as-judge), metrics, fail the run below min_accuracy."},
	{Name: "fine-tune", Summary: "Validate data, submit a fine-tuning job idempotently, poll, gate on held-out accuracy vs base, register.", GPUOption: true},
}

// List returns the templates in display order.
func List() []Template {
	out := make([]Template, len(catalog))
	copy(out, catalog)
	return out
}

// Lookup finds a template by name.
func Lookup(name string) (Template, bool) {
	for _, t := range catalog {
		if t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}

// Names returns the template names, for help text and shell completion.
func Names() []string {
	names := make([]string, 0, len(catalog))
	for _, t := range catalog {
		names = append(names, t.Name)
	}
	return names
}

// Vars are the substitutions applied to every file.
type Vars struct {
	// Name is the workflow name; it is also the task_ref prefix and image name.
	Name string
	// Namespace is written into the spec.
	Namespace string
	// GPU turns on GPU resource requests where the template supports it.
	GPU bool
}

// identifier is the spec schema's $defs/identifier: what `name` and
// `namespace` must match for the compiled spec to be accepted.
var identifier = regexp.MustCompile(`^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$`)

// ValidateIdentifier checks a workflow name or namespace against the schema.
func ValidateIdentifier(kind, v string) error {
	if len(v) == 0 || len(v) > 253 || !identifier.MatchString(v) {
		return fmt.Errorf("%s %q is not valid: use lower-case letters, digits, '-', '_' and '.', "+
			"starting and ending with a letter or digit", kind, v)
	}
	return nil
}

// File is one rendered file.
type File struct {
	Path    string // slash-separated, relative to the project root
	Content []byte
	Mode    uint32
}

// commonDir holds files every template shares; a template's own file of the
// same name wins.
const commonDir = "files/_common"

// renames maps embedded names to their on-disk names. Dotfiles are stored
// without the dot so they are visible in the repository and not mistaken for
// this repository's own configuration.
var renames = map[string]string{"gitignore": ".gitignore"}

// Render produces the files of a template with vars substituted, sorted by path.
func Render(name string, v Vars) ([]File, error) {
	t, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("unknown template %q (available: %s)", name, strings.Join(Names(), ", "))
	}
	if err := ValidateIdentifier("workflow name", v.Name); err != nil {
		return nil, err
	}
	if err := ValidateIdentifier("namespace", v.Namespace); err != nil {
		return nil, err
	}
	if v.GPU && !t.GPUOption {
		return nil, fmt.Errorf("template %q has no GPU option", name)
	}
	gpu := "False"
	if v.GPU {
		gpu = "True"
	}
	replacer := strings.NewReplacer(
		"__WORKFLOW_NAME__", v.Name,
		"__NAMESPACE__", v.Namespace,
		"__USE_GPU__", gpu,
	)

	byPath := map[string]File{}
	for _, root := range []string{commonDir, "files/" + name} {
		err := fs.WalkDir(files, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// Never ship interpreter caches that a local test run may have
				// left in the source tree.
				if d.Name() == "__pycache__" || d.Name() == ".pytest_cache" {
					return fs.SkipDir
				}
				return nil
			}
			raw, err := files.ReadFile(p)
			if err != nil {
				return err
			}
			rel := strings.TrimPrefix(p, root+"/")
			if to, ok := renames[path.Base(rel)]; ok {
				rel = path.Join(path.Dir(rel), to)
			}
			byPath[rel] = File{Path: rel, Content: []byte(replacer.Replace(string(raw))), Mode: 0o644}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("reading template %s: %w", name, err)
		}
	}
	out := make([]File, 0, len(byPath))
	for _, f := range byPath {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
