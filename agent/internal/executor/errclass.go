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

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Classified is a failure paired with the retry-relevant class the control
// plane will act on.
type Classified struct {
	Class     nexusv1.ErrorClass
	Type      string
	Message   string
	Retryable bool
}

// ToProto renders the classification for ReportResult.
func (c Classified) ToProto() *nexusv1.TaskError {
	return &nexusv1.TaskError{
		Class:     c.Class,
		Type:      c.Type,
		Message:   c.Message,
		Retryable: c.Retryable,
	}
}

// Sentinel errors executors return so classification is a type assertion
// rather than string matching on a runtime's error text. Matching on strings
// across Docker versions is how an OOM quietly starts classifying as
// APPLICATION and stops being retried on a larger worker.
var (
	// ErrImageNotFound is terminal: no retry can conjure a missing image or
	// handler. Retrying it just burns the task's budget before the human who
	// must fix the reference ever sees a clear error.
	ErrImageNotFound = errors.New("executor: image or handler not found")
	// ErrRuntimeUnavailable means the substrate itself is broken (docker
	// daemon down, cgroup mount missing) — INFRASTRUCTURE, and retryable
	// because another worker very likely is healthy.
	ErrRuntimeUnavailable = errors.New("executor: runtime unavailable")
	// ErrResultStore is a result-store I/O failure. Classified INFRASTRUCTURE
	// per ADR-0008: the second failure domain object storage introduces.
	ErrResultStore = errors.New("executor: result store failure")
	// ErrOOMKilled is carried separately from exit code so subprocess and
	// docker can agree on it despite reporting it completely differently.
	ErrOOMKilled = errors.New("executor: workload OOM-killed")
	// ErrLimitUnenforceable means the executor could not apply a requested
	// limit. It is INFRASTRUCTURE rather than silently ignored: running a task
	// without the memory cap it was scheduled under is how one task takes down
	// a node and every other task on it.
	ErrLimitUnenforceable = errors.New("executor: resource limit not enforceable")
	// ErrValidation is a malformed spec (no command, bad image ref) — terminal.
	ErrValidation = errors.New("executor: invalid task spec")
	// ErrEvicted means the substrate took the workload away (Kubernetes
	// eviction under node pressure, preemption, node loss). The task did
	// nothing wrong, so it is INFRASTRUCTURE and retryable, and it is kept
	// distinct from ErrRuntimeUnavailable so an operator can tell "the cluster
	// is under pressure" from "the executor is broken".
	ErrEvicted = errors.New("executor: workload evicted by the substrate")
)

// ClassifyOutcome maps a completed execution to an error class. This is the
// mapping the control plane's retry policy ultimately acts on, so the ordering
// below is load-bearing: the MOST SPECIFIC cause must win.
//
// Specifically, OOM is checked before exit code because the kernel reports an
// OOM kill as SIGKILL, which a naive exit-code-first classifier would call an
// APPLICATION error — permanently terminal by default — for a failure that a
// larger worker would have completed. Likewise timeout precedes exit code: a
// task killed by our own deadline enforcement also exits non-zero, and calling
// that an application bug points the operator at the wrong thing entirely.
func ClassifyOutcome(o Outcome) (nexusv1.ErrorClass, bool) {
	switch {
	case o.Err != nil:
		c := ClassifyError(o.Err)
		return c.Class, c.Retryable
	case o.OOMKilled:
		return nexusv1.ErrorClass_ERROR_CLASS_OOM, true
	case o.TimedOut:
		return nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT, true
	case o.ExitCode != 0:
		// The task's own code decided to fail. Terminal unless the author's
		// retry policy explicitly lists APPLICATION, because blindly retrying
		// deterministic bugs is how a retry budget evaporates in seconds.
		return nexusv1.ErrorClass_ERROR_CLASS_APPLICATION, false
	default:
		return nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED, false
	}
}

// ClassifyError maps an executor-level (not workload-level) failure.
func ClassifyError(err error) Classified {
	if err == nil {
		return Classified{Class: nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED}
	}

	switch {
	case errors.Is(err, ErrOOMKilled):
		return Classified{nexusv1.ErrorClass_ERROR_CLASS_OOM, "OOMKilled",
			err.Error(), true}

	case errors.Is(err, ErrImageNotFound), errors.Is(err, os.ErrNotExist):
		// A missing executable is the subprocess analogue of a missing image,
		// and deserves the same terminal NOT_FOUND rather than being retried
		// around the fleet until the budget is gone.
		return Classified{nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND, "NotFound",
			err.Error(), false}

	case errors.Is(err, ErrValidation):
		return Classified{nexusv1.ErrorClass_ERROR_CLASS_VALIDATION, "InvalidSpec",
			err.Error(), false}

	case errors.Is(err, os.ErrPermission):
		return Classified{nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED, "PermissionDenied",
			err.Error(), false}

	case errors.Is(err, ErrResultStore):
		return Classified{nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE, "ResultStoreFailure",
			err.Error(), true}

	case errors.Is(err, ErrEvicted):
		return Classified{nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE, "Evicted",
			err.Error(), true}

	case errors.Is(err, ErrLimitUnenforceable), errors.Is(err, ErrRuntimeUnavailable):
		return Classified{nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE, "RuntimeUnavailable",
			err.Error(), true}

	case errors.Is(err, context.DeadlineExceeded):
		return Classified{nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT, "DeadlineExceeded",
			err.Error(), true}

	case errors.Is(err, context.Canceled):
		// Cancellation is not a task failure, but the attempt still did not
		// produce a result. WORKER_LOST carries "we do not know" semantics,
		// which is the honest report here.
		return Classified{nexusv1.ErrorClass_ERROR_CLASS_WORKER_LOST, "Cancelled",
			err.Error(), true}

	case isNetworkError(err):
		return Classified{nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT, "NetworkError",
			err.Error(), true}
	}

	// Unknown failures default to INFRASTRUCTURE + retryable rather than
	// APPLICATION + terminal. An unclassified executor bug is our fault, not
	// the task author's, and failing their task permanently for it is the
	// wrong direction to be wrong in.
	return Classified{nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE, "Unclassified",
		err.Error(), true}
}

// isNetworkError recognises transport failures. Substring matching is a last
// resort for wrapped errors from libraries that do not expose typed errors;
// the typed checks above are tried first and cover everything we raise
// ourselves.
func isNetworkError(err error) bool {
	var nerr net.Error
	if errors.As(err, &nerr) {
		return true
	}
	var oerr *net.OpError
	if errors.As(err, &oerr) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, frag := range []string{
		"connection refused", "connection reset", "no such host",
		"i/o timeout", "network is unreachable", "tls handshake",
		"eof", "broken pipe",
	} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

// ClassifiedFromOutcome builds the full TaskError for a failed execution,
// including a short excerpt of the workload's own logs.
//
// The excerpt is capped and TAIL-biased: the last lines are where a stack
// trace or the fatal message lives, and unbounded error text in the metadata
// store is exactly what ADR-0008's detail_ref exists to prevent.
func ClassifiedFromOutcome(o Outcome, logTail string) Classified {
	class, retryable := ClassifyOutcome(o)

	typ := "TaskFailed"
	msg := fmt.Sprintf("exit code %d", o.ExitCode)
	switch {
	case o.Err != nil:
		c := ClassifyError(o.Err)
		typ, msg = c.Type, c.Message
	case o.OOMKilled:
		typ = "OOMKilled"
		msg = fmt.Sprintf("workload exceeded its memory limit (peak %d bytes)", o.PeakMemoryBytes)
	case o.TimedOut:
		typ = "DeadlineExceeded"
		msg = fmt.Sprintf("exceeded start_to_close deadline after %dms", o.WallMs)
	case o.Signalled:
		typ = "Signalled"
		msg = fmt.Sprintf("workload terminated by signal %s", o.Signal)
	}
	if logTail != "" {
		msg = msg + "\n--- log tail ---\n" + logTail
	}
	return Classified{Class: class, Type: typ, Message: msg, Retryable: retryable}
}

// MaxLogTailBytes bounds what we inline into TaskError.message. Anything
// larger belongs behind detail_ref in the result store.
const MaxLogTailBytes = 4096

// LogTail returns at most MaxLogTailBytes of the end of b, trimmed to a line
// boundary so the excerpt does not begin mid-token.
func LogTail(b []byte) string {
	if len(b) <= MaxLogTailBytes {
		return string(b)
	}
	tail := b[len(b)-MaxLogTailBytes:]
	if i := strings.IndexByte(string(tail), '\n'); i >= 0 && i < len(tail)-1 {
		tail = tail[i+1:]
	}
	return "...(truncated)...\n" + string(tail)
}
