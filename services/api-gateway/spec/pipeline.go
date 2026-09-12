package spec

import (
	"fmt"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/dag"
)

// Compiled is the result of a successful submission pipeline run.
type Compiled struct {
	// Doc is the decoded document, defaults NOT applied. Canonical bytes are
	// derived from exactly what the author submitted, so the content address is a
	// function of the author's document and not of the platform's current default
	// values. Otherwise changing a default in a future release would silently
	// re-hash every stored spec.
	Doc any
	// Canonical is the deterministic byte rendering that gets stored and hashed.
	Canonical []byte
	// Hash is the sha256 content address of Canonical.
	Hash string
	// Proto is the wire form, with defaults resolved, for semantic use.
	Proto *nexusv1.WorkflowSpec
	// Graph is the compiled DAG, returned so a caller can render it.
	Graph *dag.Graph
}

// Compile runs the full submission pipeline: parse -> structural validation ->
// semantic validation -> canonicalize -> content-address.
//
// The order is not interchangeable. Structure first, because the semantic layer
// reads typed fields. Semantics second, because a spec with a cycle must never
// reach storage — a stored cyclic spec would wedge the DAG engine's frontier
// computation forever, and the whole point of validating at submit time is that
// the failure lands on the author who can fix it rather than on an operator at
// 3am (ADR-0004).
//
// Canonicalization is LAST so the hash is only ever computed over a document that
// has been fully accepted. Hashing first would content-address documents we then
// reject, and a retry of the same bad document would appear to be "already
// submitted".
func Compile(body []byte, format Format) (*Compiled, error) {
	doc, err := Parse(body, format)
	if err != nil {
		return nil, &ValidationFailure{Problems: []Problem{{Path: "", Message: err.Error(), Stage: "parse"}}}
	}

	if err := ValidateStructure(doc); err != nil {
		return nil, err
	}

	proto, err := ToProto(doc)
	if err != nil {
		// A conversion failure after the schema passed is OUR bug, not the
		// author's, so it is not dressed up as a validation problem.
		return nil, fmt.Errorf("convert validated spec to wire form: %w", err)
	}

	graph, verrs := dag.Validate(proto, dag.DefaultValidationOptions())
	if len(verrs) > 0 {
		return nil, &ValidationFailure{Problems: semanticProblems(verrs)}
	}

	canonical, err := Canonicalize(doc)
	if err != nil {
		return nil, fmt.Errorf("canonicalize spec: %w", err)
	}

	return &Compiled{
		Doc:       doc,
		Canonical: canonical,
		Hash:      ContentAddress(canonical),
		Proto:     proto,
		Graph:     graph,
	}, nil
}

// semanticProblems translates dag validation errors into the gateway's wire shape.
//
// The dag package's stable ErrorCode is carried into the message rather than
// discarded, because a UI that has to regex prose to decide how to render an
// error breaks on every wording change.
func semanticProblems(errs dag.ValidationErrors) []Problem {
	out := make([]Problem, 0, len(errs))
	for _, e := range errs {
		path := ""
		if e.NodeID != "" {
			path = "/tasks/" + e.NodeID
			if e.Field != "" {
				path += "/" + e.Field
			}
		}
		out = append(out, Problem{
			Path:    path,
			Message: e.Error(),
			Stage:   "semantic",
		})
	}
	return out
}
