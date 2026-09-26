// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// TestClassifyOutcomePrecedence is the load-bearing ordering. The MOST SPECIFIC
// cause must win, because the kernel reports an OOM kill as SIGKILL and a
// deadline-enforced kill also exits non-zero. An exit-code-first classifier would
// call both APPLICATION — terminal by default — for failures that are in fact
// retryable, and would point the operator at a bug in the task rather than at the
// node size or the deadline.
func TestClassifyOutcomePrecedence(t *testing.T) {
	tests := []struct {
		name      string
		outcome   Outcome
		wantClass nexusv1.ErrorClass
		retryable bool
	}{
		{
			name:      "OOM beats a non-zero exit code",
			outcome:   Outcome{OOMKilled: true, ExitCode: 137},
			wantClass: nexusv1.ErrorClass_ERROR_CLASS_OOM,
			retryable: true, // a larger worker may succeed
		},
		{
			name:      "timeout beats a non-zero exit code",
			outcome:   Outcome{TimedOut: true, ExitCode: 143},
			wantClass: nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT,
			retryable: true,
		},
		{
			name:      "OOM beats timeout",
			outcome:   Outcome{OOMKilled: true, TimedOut: true, ExitCode: 137},
			wantClass: nexusv1.ErrorClass_ERROR_CLASS_OOM,
			retryable: true,
		},
		{
			name:      "plain non-zero exit is the task's own failure",
			outcome:   Outcome{ExitCode: 1},
			wantClass: nexusv1.ErrorClass_ERROR_CLASS_APPLICATION,
			retryable: false, // retrying deterministic bugs burns the budget
		},
		{
			name:      "executor error beats everything",
			outcome:   Outcome{Err: ErrRuntimeUnavailable, ExitCode: 1},
			wantClass: nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE,
			retryable: true,
		},
		{
			name:      "clean exit is unclassified",
			outcome:   Outcome{ExitCode: 0},
			wantClass: nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED,
			retryable: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			class, retryable := ClassifyOutcome(tc.outcome)
			if class != tc.wantClass {
				t.Errorf("class = %v, want %v", class, tc.wantClass)
			}
			if retryable != tc.retryable {
				t.Errorf("retryable = %v, want %v", retryable, tc.retryable)
			}
		})
	}
}

// TestClassifyErrorMapsRealFailures covers the mapping the task brief specifies:
// OOM kill → OOM, image missing → NOT_FOUND, network → TRANSIENT, deadline →
// TIMEOUT, result store down → INFRASTRUCTURE.
func TestClassifyErrorMapsRealFailures(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantClass nexusv1.ErrorClass
		retryable bool
	}{
		{"oom kill", ErrOOMKilled, nexusv1.ErrorClass_ERROR_CLASS_OOM, true},
		{"image missing", ErrImageNotFound, nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND, false},
		{"handler missing", os.ErrNotExist, nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND, false},
		{"result store down", ErrResultStore, nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE, true},
		{"runtime unavailable", ErrRuntimeUnavailable, nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE, true},
		{"limit unenforceable", ErrLimitUnenforceable, nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE, true},
		{"invalid spec", ErrValidation, nexusv1.ErrorClass_ERROR_CLASS_VALIDATION, false},
		{"output over max_bytes", fmt.Errorf("%w: output %q", ErrOutputTooLarge, "x"), nexusv1.ErrorClass_ERROR_CLASS_VALIDATION, false},
		{"permission denied", os.ErrPermission, nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED, false},
		{"deadline", context.DeadlineExceeded, nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT, true},
		{"cancelled", context.Canceled, nexusv1.ErrorClass_ERROR_CLASS_WORKER_LOST, true},
		{"network refused", errors.New("dial tcp 1.2.3.4:9000: connection refused"),
			nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT, true},
		{"network typed", &net.OpError{Op: "dial", Err: errors.New("no route")},
			nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyError(tc.err)
			if got.Class != tc.wantClass {
				t.Errorf("class = %v, want %v", got.Class, tc.wantClass)
			}
			if got.Retryable != tc.retryable {
				t.Errorf("retryable = %v, want %v", got.Retryable, tc.retryable)
			}
			if got.Type == "" {
				t.Error("no error type; the UI has nothing to group on")
			}
		})
	}
}

// TestClassificationSurvivesWrapping: every executor wraps its errors with
// fmt.Errorf("...: %w", err), so classification must work through the wrapper.
// If it did not, every real failure would fall through to the default.
func TestClassificationSurvivesWrapping(t *testing.T) {
	wrapped := fmt.Errorf("prepare: %w", fmt.Errorf("pull image: %w", ErrImageNotFound))
	got := ClassifyError(wrapped)
	if got.Class != nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND {
		t.Errorf("class = %v through two wrappers, want NOT_FOUND", got.Class)
	}
}

// TestUnknownErrorDefaultsToRetryableInfrastructure: an unclassified executor bug
// is OUR fault, not the task author's, and failing their task permanently for it
// is the wrong direction to be wrong in.
func TestUnknownErrorDefaultsToRetryableInfrastructure(t *testing.T) {
	got := ClassifyError(errors.New("something nobody anticipated"))
	if got.Class != nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE {
		t.Errorf("class = %v, want INFRASTRUCTURE for an unclassified failure", got.Class)
	}
	if !got.Retryable {
		t.Error("an unclassified failure must be retryable: it is more likely our bug than the task's")
	}
}

func TestToProtoCarriesEverything(t *testing.T) {
	c := Classified{
		Class: nexusv1.ErrorClass_ERROR_CLASS_OOM, Type: "OOMKilled",
		Message: "exceeded 2 GiB", Retryable: true,
	}
	p := c.ToProto()
	if p.GetClass() != c.Class || p.GetType() != c.Type ||
		p.GetMessage() != c.Message || p.GetRetryable() != c.Retryable {
		t.Errorf("ToProto lost data: %+v", p)
	}
}

// TestClassifiedFromOutcomeDescribesTheCause: the message must name the real cause,
// because it is what an operator reads first.
func TestClassifiedFromOutcomeDescribesTheCause(t *testing.T) {
	oom := ClassifiedFromOutcome(Outcome{OOMKilled: true, PeakMemoryBytes: 2 << 30}, "")
	if oom.Type != "OOMKilled" || !strings.Contains(oom.Message, "memory limit") {
		t.Errorf("OOM description = %+v", oom)
	}
	to := ClassifiedFromOutcome(Outcome{TimedOut: true, WallMs: 61000}, "")
	if to.Type != "DeadlineExceeded" || !strings.Contains(to.Message, "start_to_close") {
		t.Errorf("timeout description = %+v", to)
	}
	sig := ClassifiedFromOutcome(Outcome{Signalled: true, Signal: "killed", ExitCode: 137}, "")
	if sig.Type != "Signalled" {
		t.Errorf("signal description = %+v", sig)
	}
}

// TestLogTailIsBoundedAndTailBiased: unbounded error text in the metadata store is
// exactly what ADR-0008's detail_ref exists to prevent, and the LAST lines are
// where the stack trace lives.
func TestLogTailIsBoundedAndTailBiased(t *testing.T) {
	short := []byte("a short log\n")
	if got := LogTail(short); got != string(short) {
		t.Errorf("a short log was altered: %q", got)
	}

	var big strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&big, "line %d filler filler filler\n", i)
	}
	big.WriteString("FATAL: the actual cause\n")

	got := LogTail([]byte(big.String()))
	if len(got) > MaxLogTailBytes+64 {
		t.Errorf("tail is %d bytes, want at most ~%d", len(got), MaxLogTailBytes)
	}
	if !strings.Contains(got, "FATAL: the actual cause") {
		t.Error("the tail dropped the END of the log, which is where the cause is")
	}
	if !strings.Contains(got, "truncated") {
		t.Error("a truncated tail must say so, or an operator reads it as the whole log")
	}
}

// TestOutcomeSucceededIsTheSingleDefinition keeps subprocess and docker from
// drifting on what success means.
func TestOutcomeSucceededIsTheSingleDefinition(t *testing.T) {
	if !(Outcome{ExitCode: 0}).Succeeded() {
		t.Error("exit 0 must be success")
	}
	for name, o := range map[string]Outcome{
		"non-zero exit": {ExitCode: 1},
		"oom killed":    {ExitCode: 0, OOMKilled: true},
		"timed out":     {ExitCode: 0, TimedOut: true},
		"executor err":  {ExitCode: 0, Err: ErrRuntimeUnavailable},
	} {
		if o.Succeeded() {
			t.Errorf("%s was reported as success", name)
		}
	}
}

// TestSubprocessFidelityIsHonest: the whole point of LimitFidelity is that an
// executor states what it can actually enforce. A value outside the documented set
// would silently break the conformance suite's conditional assertions.
func TestSubprocessFidelityIsHonest(t *testing.T) {
	f := SubprocessFidelity()
	valid := map[string]bool{"hard": true, "address-space": true, "none": true}
	if !valid[f.Memory] {
		t.Errorf("Memory fidelity = %q, not one of hard|address-space|none", f.Memory)
	}
	// Docker claims hard enforcement, and the conformance suite holds it to that.
	if DockerFidelity().Memory != "hard" {
		t.Errorf("docker Memory fidelity = %q, want hard", DockerFidelity().Memory)
	}
}
