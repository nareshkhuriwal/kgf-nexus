// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"bytes"
	"embed"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/khuriwalgroup/kgf-nexus/internal/specpos"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// The schema is EMBEDDED rather than read from disk at startup.
//
// A validator that loads its rules from the filesystem can start successfully
// and then reject every submission because a container image was built without
// the file, or — worse — validate against a stale copy after a partial deploy.
// Compiling it into the binary makes the grammar and the code that enforces it
// one artifact that cannot be mismatched.
//
// This file is a copy of the canonical schemas/workflow-spec-v1.json, because
// go:embed cannot reach outside a package directory. TestEmbeddedSchemaMatchesCanonical
// asserts the two are byte-identical, so drift fails the build rather than
// silently enforcing an old grammar.
//
//go:embed workflow-spec-v1.json
var schemaFS embed.FS

const schemaURL = "https://nexus.khuriwalgroup.com/schemas/workflow-spec-v1.json"

var (
	compileOnce sync.Once
	compiled    *jsonschema.Schema
	compileErr  error
)

// SchemaBytes returns the embedded schema document, so the gateway can serve the
// exact grammar it enforces. Publishing a schema that might differ from the one
// in force would be worse than publishing none.
func SchemaBytes() ([]byte, error) {
	b, err := schemaFS.ReadFile("workflow-spec-v1.json")
	if err != nil {
		return nil, fmt.Errorf("read embedded schema: %w", err)
	}
	return b, nil
}

// schema compiles the embedded document once per process. Compilation is
// nontrivial (it resolves every $ref) and the result is immutable and
// goroutine-safe, so doing it per request would be pure waste on the hot path.
func schema() (*jsonschema.Schema, error) {
	compileOnce.Do(func() {
		raw, err := SchemaBytes()
		if err != nil {
			compileErr = err
			return
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			compileErr = fmt.Errorf("parse embedded schema: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		// Draft 2020-12 explicitly, not "whatever the library defaults to":
		// the schema uses $defs and prefixItems semantics that differ across
		// drafts, and an implicit default could change under a dependency bump.
		c.DefaultDraft(jsonschema.Draft2020)
		if err := c.AddResource(schemaURL, doc); err != nil {
			compileErr = fmt.Errorf("add schema resource: %w", err)
			return
		}
		compiled, err = c.Compile(schemaURL)
		if err != nil {
			compileErr = fmt.Errorf("compile schema: %w", err)
		}
	})
	return compiled, compileErr
}

// Problem is one rejection reason, addressed to the human who wrote the spec.
//
// A path plus a message rather than a bare error string: a 40-task workflow
// rejected with "invalid spec" is useless, and errors surfacing at submit time
// instead of at 3am is one of the stated wins of the declarative model
// (ADR-0004). Making that real requires saying WHERE the problem is.
type Problem struct {
	// Path is a JSON Pointer-ish location such as "/tasks/3/resources/request".
	Path string `json:"path"`
	// Message says what is wrong.
	Message string `json:"message"`
	// Stage distinguishes a structural rejection from a semantic one; they mean
	// different things to the author ("wrong shape" vs "the graph is invalid").
	Stage string `json:"stage"`
	// Line and Column locate Path in the SUBMITTED document (1-based), for YAML
	// and JSON alike. Additive and omitted when unknown -- a document that did
	// not parse, or a path above any node. See internal/specpos.
	Line   int `json:"line,omitempty"`
	Column int `json:"column,omitempty"`
}

func (p Problem) String() string {
	loc := p.Path
	if loc == "" {
		loc = "(root)"
	}
	if p.Line > 0 {
		loc = fmt.Sprintf("line %d:%d %s", p.Line, p.Column, loc)
	}
	return fmt.Sprintf("%s: %s", loc, p.Message)
}

// locate fills Line and Column from the submitted body. Parse problems carry
// their own position in the message and are left alone.
func (v *ValidationFailure) locate(body []byte) {
	if v == nil || len(v.Problems) == 0 {
		return
	}
	idx := specpos.New(body)
	for i := range v.Problems {
		p := &v.Problems[i]
		if p.Stage == "parse" {
			continue
		}
		if at, ok := idx.Lookup(p.Path); ok {
			p.Line, p.Column = at.Line, at.Column
		}
	}
}

// ValidationFailure carries every problem found, not just the first.
//
// Returning all of them is deliberate: an author fixing a spec one error per
// round trip against a remote API is a miserable loop, and the information is
// already computed.
type ValidationFailure struct {
	Problems []Problem
}

func (v *ValidationFailure) Error() string {
	if len(v.Problems) == 0 {
		return "spec validation failed"
	}
	parts := make([]string, 0, len(v.Problems))
	for _, p := range v.Problems {
		parts = append(parts, p.String())
	}
	return "spec validation failed: " + strings.Join(parts, "; ")
}

// ValidateStructure checks a decoded document against the JSON Schema.
//
// This is stage one of the pipeline and runs before any semantic check, because
// the semantic checks read fields by type and would otherwise have to re-derive
// every type assertion the schema already guarantees.
func ValidateStructure(doc any) error {
	sch, err := schema()
	if err != nil {
		return err
	}
	if err := sch.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if ok := asValidationError(err, &ve); ok {
			return &ValidationFailure{Problems: flattenSchemaErrors(ve)}
		}
		return fmt.Errorf("validate spec structure: %w", err)
	}
	return nil
}

// enPrinter renders the library's error messages in English.
//
// The validator localizes messages through golang.org/x/text; without an explicit
// printer the ErrorKind values format as Go structs ("&{BAD QUEUE ^[a-z0-9]...}")
// which is unreadable to the author who has to fix the spec.
var enPrinter = message.NewPrinter(language.English)

// describeKind renders one schema violation.
//
// The failing keyword is prepended because "does not match pattern" without
// naming the keyword leaves an author guessing which of several constraints on a
// field rejected their value.
func describeKind(k jsonschema.ErrorKind) string {
	msg := k.LocalizedString(enPrinter)
	if kw := k.KeywordPath(); len(kw) > 0 {
		return fmt.Sprintf("%s: %s", strings.Join(kw, "/"), msg)
	}
	return msg
}

func asValidationError(err error, target **jsonschema.ValidationError) bool {
	if ve, ok := err.(*jsonschema.ValidationError); ok {
		*target = ve
		return true
	}
	return false
}

// flattenSchemaErrors turns the library's error tree into a flat, sorted list of
// leaf problems.
//
// Only LEAVES are reported. The tree's interior nodes are bookkeeping ("allOf
// failed", "oneOf failed") that restate the structure of the schema rather than
// telling the author anything about their document; surfacing them buries the one
// line that matters under a dozen that do not.
func flattenSchemaErrors(root *jsonschema.ValidationError) []Problem {
	var out []Problem
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			out = append(out, Problem{
				Path: "/" + strings.Join(e.InstanceLocation, "/"),
				// The keyword path (e.g. ["properties","queue","pattern"]) is
				// included because the bare message for a pattern failure does
				// not say WHICH constraint rejected the value.
				Message: describeKind(e.ErrorKind),
				Stage:   "schema",
			})
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(root)

	// Sorted and de-duplicated so the same bad document always produces the same
	// error payload. Nondeterministic error output makes a test suite flaky and
	// makes a support ticket hard to correlate.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Message < out[j].Message
	})
	dedup := out[:0]
	var prev Problem
	for i, p := range out {
		if i > 0 && p == prev {
			continue
		}
		dedup = append(dedup, p)
		prev = p
	}
	return dedup
}
