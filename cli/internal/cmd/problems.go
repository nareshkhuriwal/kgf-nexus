// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/client"
	"github.com/khuriwalgroup/kgf-nexus/cli/internal/spec"
)

// localProblems runs the local checks and reports them at their line in the
// author's file (YAML/JSON), or at the @task / call in a .py spec.
func localProblems(doc *spec.Document) error {
	ps := doc.Problems()
	if len(ps) == 0 {
		return nil
	}
	return fmt.Errorf("%s is not a valid WorkflowSpec:\n%s", doc.Source, spec.FormatProblems(doc.Positioned(ps)))
}

// annotateSubmitError places the gateway's validation problems in the local
// file. The gateway computes positions too, but against the canonical bytes
// nexusctl sent -- one line of JSON -- so only the local file can say which
// line of YAML or Python to fix.
func annotateSubmitError(err error, doc *spec.Document) error {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 422 {
		return err
	}
	raw, _ := json.Marshal(apiErr.Details)
	var details struct {
		Problems []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"problems"`
	}
	if json.Unmarshal(raw, &details) != nil || len(details.Problems) == 0 {
		return err
	}
	ps := make([]spec.Problem, 0, len(details.Problems))
	for _, p := range details.Problems {
		ps = append(ps, spec.Problem{Path: p.Path, Message: p.Message})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "the control plane rejected %s:\n%s", doc.Source, spec.FormatProblems(doc.Positioned(ps)))
	if apiErr.RequestID != "" {
		fmt.Fprintf(&b, "\n  request id: %s", apiErr.RequestID)
	}
	return errors.New(b.String())
}

// pythonSourceMapSuffix appends the SDK's source map to the compile script's
// work, written to the file named by NEXUSCTL_SOURCE_MAP. A separate file, not
// stdout, so the canonical bytes on stdout stay exactly what is hashed. An SDK
// predating source maps simply writes nothing.
const pythonSourceMapSuffix = `
_sm = os.environ.get("NEXUSCTL_SOURCE_MAP")
if _sm:
    import json as _json
    _c = wf.compile()
    _fn = getattr(_c, "source_map_json", None)
    with open(_sm, "w") as _f:
        _json.dump(_fn() if _fn else {}, _f)
`

// compilePythonMapped is compilePython plus the source map.
func compilePythonMapped(python, path, workflowName string) ([]byte, map[string]spec.NodeSource, error) {
	f, err := os.CreateTemp("", "nexusctl-sourcemap-*.json")
	if err != nil {
		raw, cerr := compilePython(python, path, workflowName)
		return raw, nil, cerr
	}
	name := f.Name()
	_ = f.Close()
	defer os.Remove(name)

	args := []string{"-c", pythonCompileScript + pythonSourceMapSuffix, path}
	if workflowName != "" {
		args = append(args, workflowName)
	}
	cmd := exec.Command(python, args...)
	cmd.Env = append(os.Environ(), "NEXUSCTL_SOURCE_MAP="+name)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	if runErr != nil {
		// Re-run through the plain path for its carefully worded errors.
		raw, cerr := compilePython(python, path, workflowName)
		return raw, nil, cerr
	}
	var sm map[string]spec.NodeSource
	if b, rerr := os.ReadFile(name); rerr == nil && len(b) > 0 {
		_ = json.Unmarshal(b, &sm)
	}
	return out, sm, nil
}
