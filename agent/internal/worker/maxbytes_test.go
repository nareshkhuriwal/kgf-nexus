// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// outputExecutor is an executor that "runs" nothing and returns a fixed
// Outcome. It stands in for docker and kubernetes, whose outputs reach the
// runner through exactly the same Outcome.Outputs as the subprocess
// executor's: the bound is enforced where every executor's outputs converge,
// so this proves it for each of them without a daemon or a cluster.
type outputExecutor struct {
	kind    string
	outcome executor.Outcome
}

func (e *outputExecutor) Kind() string { return e.kind }
func (e *outputExecutor) Prepare(_ context.Context, s executor.TaskSpec) (executor.Prepared, error) {
	return executor.Prepared{AttemptID: s.AttemptID}, nil
}
func (e *outputExecutor) Start(_ context.Context, p executor.Prepared) (executor.Handle, error) {
	return executor.Handle{AttemptID: p.AttemptID, ExecutorKind: e.kind, ID: "fake", StartedAt: time.Now()}, nil
}
func (e *outputExecutor) Wait(context.Context, executor.Handle) (executor.Outcome, error) {
	return e.outcome, nil
}
func (e *outputExecutor) Signal(context.Context, executor.Handle, executor.Signal) error { return nil }
func (e *outputExecutor) Logs(context.Context, executor.Handle) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}
func (e *outputExecutor) Cleanup(context.Context, executor.Handle) error { return nil }

// runWithOutputs runs one attempt whose executor produced outs, under a spec
// declaring decl, and returns the single ReportResult it sent.
func runWithOutputs(t *testing.T, kind string, threshold int64, decl map[string]*nexusv1.ValueSpec,
	outs map[string]executor.NamedOutput, legacy []byte) *nexusv1.ReportResultRequest {
	t.Helper()
	c := &fakeReportClient{}
	r, reg, _ := newTestRunner(t, c)
	r.executors[kind] = &outputExecutor{kind: kind, outcome: executor.Outcome{Outputs: outs, Output: legacy}}
	r.InlineThreshold = threshold
	a := assignment("mb-"+kind, "unused")
	a.Spec.Runtime.Executor = kind
	a.Spec.Outputs = decl
	if ok, reason := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatalf("admit: %s", reason)
	}
	r.Run(context.Background(), a)

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.results) != 1 {
		t.Fatalf("want one result report, got %d", len(c.results))
	}
	return c.results[0]
}

// TestOutputOverMaxBytesFailsTheAttempt is F125: an output larger than its
// declared ValueSpec.max_bytes fails the attempt with a terminal VALIDATION
// error naming the output, its size and the bound, and reports no outputs at
// all -- on every executor, and whether the output would have been inlined or
// uploaded. Before the fix the agent never read max_bytes, so each of these
// reported SUCCEEDED (or, for the blob case on a store-less worker, a
// retryable INFRASTRUCTURE failure that sends the broken promise around the
// fleet).
func TestOutputOverMaxBytesFailsTheAttempt(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 200)
	decl := map[string]*nexusv1.ValueSpec{
		"small":  {Type: "json", MaxBytes: 64},
		"capped": {Type: "json", MaxBytes: 100},
	}
	cases := []struct {
		name      string
		threshold int64 // below len(big) makes "capped" a blob-sized output
	}{
		{"inline", 1 << 20},
		{"blob", 16},
	}
	for _, kind := range []string{"subprocess", "docker", "kubernetes"} {
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				res := runWithOutputs(t, kind, tc.threshold, decl, map[string]executor.NamedOutput{
					"small":  {Bytes: []byte(`{"ok":true}`)},
					"capped": {Bytes: big},
				}, nil)
				if res.GetOutcome() != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED {
					t.Fatalf("outcome = %v, want FAILED", res.GetOutcome())
				}
				e := res.GetError()
				if e.GetClass() != nexusv1.ErrorClass_ERROR_CLASS_VALIDATION || e.GetRetryable() {
					t.Errorf("error = %v retryable=%v, want terminal VALIDATION", e.GetClass(), e.GetRetryable())
				}
				if e.GetType() != "OutputTooLarge" {
					t.Errorf("type = %q, want OutputTooLarge", e.GetType())
				}
				for _, want := range []string{`"capped"`, "200 bytes", "max_bytes=100"} {
					if !strings.Contains(e.GetMessage(), want) {
						t.Errorf("message %q does not mention %s", e.GetMessage(), want)
					}
				}
				if len(res.GetOutputs()) != 0 {
					t.Errorf("reported outputs %v for an attempt that broke its bound", res.GetOutputs())
				}
			})
		}
	}
}

// TestOutputWithinMaxBytesSucceeds: the bound is inclusive, an undeclared
// bound (0) and an undeclared output are unbounded, and the legacy
// single-output file is checked under its output name "result".
func TestOutputWithinMaxBytesSucceeds(t *testing.T) {
	res := runWithOutputs(t, "docker", 1<<20, map[string]*nexusv1.ValueSpec{
		"exact": {MaxBytes: 5},
		"free":  {MaxBytes: 0},
	}, map[string]executor.NamedOutput{
		"exact": {Bytes: []byte("12345")},
		"free":  {Bytes: bytes.Repeat([]byte("y"), 4096)},
	}, nil)
	if res.GetOutcome() != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		t.Fatalf("outcome = %v (%v), want SUCCEEDED", res.GetOutcome(), res.GetError())
	}
	if len(res.GetOutputs()) != 2 {
		t.Errorf("outputs = %v, want both", res.GetOutputs())
	}

	res = runWithOutputs(t, "kubernetes", 1<<20, map[string]*nexusv1.ValueSpec{
		LegacyOutputName: {MaxBytes: 4},
	}, nil, []byte(`{"too":"long"}`))
	if res.GetError().GetType() != "OutputTooLarge" {
		t.Errorf("legacy output over its bound: outcome %v error %v, want OutputTooLarge",
			res.GetOutcome(), res.GetError())
	}
}

// TestSubprocessOutputOverMaxBytes drives the bound through a real
// subprocess workload that writes its own output file -- the non-SDK path the
// finding is about.
func TestSubprocessOutputOverMaxBytes(t *testing.T) {
	c := &fakeReportClient{}
	r, reg, _ := newTestRunner(t, c)
	a := assignment("mb-real", `printf '{"payload":"far more than eight bytes"}' > "$NEXUS_OUTPUT_DIR/report"`)
	a.Spec.Outputs = map[string]*nexusv1.ValueSpec{"report": {Type: "json", MaxBytes: 8}}
	if ok, reason := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatalf("admit: %s", reason)
	}
	r.Run(context.Background(), a)

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.results) != 1 {
		t.Fatalf("want one report, got %d", len(c.results))
	}
	if got := c.results[0].GetError(); got.GetClass() != nexusv1.ErrorClass_ERROR_CLASS_VALIDATION ||
		got.GetType() != "OutputTooLarge" {
		t.Fatalf("error = %v, want VALIDATION/OutputTooLarge", got)
	}
}
