// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"errors"
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
	c, err := compile(body, format)
	var vf *ValidationFailure
	if errors.As(err, &vf) {
		// Positions are computed only on failure: they cost a second parse, and
		// a valid document has nothing to point at.
		vf.locate(body)
	}
	return c, err
}

func compile(body []byte, format Format) (*Compiled, error) {
	doc, err := Parse(body, format)
	if err != nil {
		return nil, &ValidationFailure{Problems: []Problem{{Path: "", Message: err.Error(), Stage: "parse"}}}
	}

	if err := ValidateStructure(doc); err != nil {
		return nil, err
	}

	// Enum spellings are normalized BEFORE ToProto and before canonicalization,
	// so the SDK's proto constants and a hand-written short form become the same
	// stored bytes and therefore the same content address. Doing it after
	// canonicalization would be too late: the hash is the document's identity,
	// and two spellings of one workflow would already have become two workflows.
	NormalizeEnums(doc)

	proto, err := ToProto(doc)
	if err != nil {
		// A conversion failure after the schema passed is OUR bug, not the
		// author's, so it is not dressed up as a validation problem.
		return nil, fmt.Errorf("convert validated spec to wire form: %w", err)
	}

	graph, verrs := dag.Validate(proto, dag.DefaultValidationOptions())
	problems := semanticProblems(verrs)
	// Memoization rules are submit-time only (see dag.ValidateCache): a spec
	// stored before they existed must still materialize.
	problems = append(problems, semanticProblems(dag.ValidateCache(proto))...)

	// Timeout coherence is checked here rather than in internal/dag because it is a
	// property of a single task, not of the graph, and internal/dag is deliberately
	// pure graph logic.
	problems = append(problems, validateTimeoutCoherence(proto)...)
	problems = append(problems, validateStdoutOutput(proto)...)

	if len(problems) > 0 {
		return nil, &ValidationFailure{Problems: problems}
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

// validateTimeoutCoherence rejects timeout combinations that can never succeed.
//
// JSON Schema can bound each of the four timeouts independently but cannot compare
// them, so this is a semantic check. It matters because the failures are silent and
// total rather than noisy: a task whose schedule_to_close is smaller than its
// start_to_close can never complete even ONE attempt, so the run does not fail
// loudly at submit time — it burns a retry budget at 3am and then reports a timeout
// that looks like an infrastructure problem. Catching it here puts the error in front
// of the author who can fix it, which is the entire premise of validating at
// submission (ADR-0004). Raised as CONTRACT-FINDINGS.md F22 by the SDK workstream;
// implemented server-side because the SDK is not the only submission path.
func validateTimeoutCoherence(spec *nexusv1.WorkflowSpec) []Problem {
	var out []Problem
	for _, task := range spec.GetTasks() {
		to := task.GetTimeouts()
		if to == nil {
			continue
		}
		base := "/tasks/" + task.GetId() + "/timeouts"

		// Zero means "not enforced" on every field, so a comparison is only
		// meaningful when both sides are actually set.
		stc, s2c := to.GetStartToCloseMs(), to.GetScheduleToCloseMs()
		if stc > 0 && s2c > 0 && s2c < stc {
			out = append(out, Problem{
				Path: base + "/schedule_to_close_ms",
				Message: fmt.Sprintf(
					"schedule_to_close_ms (%d) is less than start_to_close_ms (%d), so not even one attempt can finish within the end-to-end budget",
					s2c, stc),
				Stage: "semantic",
			})
		}

		// schedule_to_start is time spent QUEUED, before the attempt begins, so it
		// too must fit inside the end-to-end budget.
		if s2s := to.GetScheduleToStartMs(); s2s > 0 && s2c > 0 && s2c < s2s {
			out = append(out, Problem{
				Path: base + "/schedule_to_close_ms",
				Message: fmt.Sprintf(
					"schedule_to_close_ms (%d) is less than schedule_to_start_ms (%d), so the attempt's own queueing allowance exceeds the total budget",
					s2c, s2s),
				Stage: "semantic",
			})
		}

		// A heartbeat interval longer than the attempt's own wall clock can never
		// fire, so the "went silent" signal is silently disabled — the operator
		// believes they have wedged-task detection and they do not.
		if hb := to.GetHeartbeatMs(); hb > 0 && stc > 0 && hb > stc {
			out = append(out, Problem{
				Path: base + "/heartbeat_ms",
				Message: fmt.Sprintf(
					"heartbeat_ms (%d) exceeds start_to_close_ms (%d), so the heartbeat timeout can never fire and wedged-task detection is disabled",
					hb, stc),
				Stage: "semantic",
			})
		}
	}
	return out
}

// validateStdoutOutput rejects a stdout_output that names no declared output.
//
// Checked here rather than in internal/dag because it is a property of one task,
// not of the graph — the same reason validateTimeoutCoherence lives here.
//
// It matters because the failure is otherwise SILENT and late. The agent
// collects only DECLARED outputs, so a stdout_output naming an undeclared name
// produces nothing: the task runs, exits 0, is recorded SUCCEEDED, and every
// downstream edge that expected the value resolves to an absent input. The
// author sees a green task and a broken pipeline, which is the hardest shape of
// bug to trace back to a one-word typo in a spec. Rejecting at submit time puts
// the error in front of the person who can fix it (ADR-0004).
func validateStdoutOutput(spec *nexusv1.WorkflowSpec) []Problem {
	var out []Problem
	for _, task := range spec.GetTasks() {
		name := task.GetRuntime().GetStdoutOutput()
		if name == "" {
			continue
		}
		if _, declared := task.GetOutputs()[name]; declared {
			continue
		}
		out = append(out, Problem{
			Path: "/tasks/" + task.GetId() + "/runtime/stdout_output",
			Message: fmt.Sprintf(
				"stdout_output names %q, which is not declared in this task's outputs; "+
					"stdout would be captured under a name nothing can reference, so the task "+
					"would succeed while producing no output at all",
				name),
			Stage: "semantic",
		})
	}
	return out
}
