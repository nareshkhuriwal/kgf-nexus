package wal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// Reporter is the narrow slice of the worker RPC surface that replay needs.
// Narrow on purpose: replay is the logic we most want to unit-test without a
// control plane, and a one-method interface makes the fake trivial.
type Reporter interface {
	ReportResult(ctx context.Context, req *nexusv1.ReportResultRequest) (*nexusv1.Ack, error)
}

// ReplayResult summarises one pass for logging and for tests.
type ReplayResult struct {
	// Reported entries whose result was durably acked and whose WAL entry has
	// been deleted.
	Reported []string
	// Indeterminate is the subset of Reported that was a STARTING entry, i.e.
	// work whose outcome we could not determine.
	Indeterminate []string
	// Rejected entries the control plane refused (typically a stale lease
	// token because another attempt already owns the work). These are deleted:
	// re-reporting a token the server has fenced off can never succeed, and
	// keeping it would make the WAL grow without bound.
	Rejected []string
	// Retained entries that could not be reported at all (control plane
	// unreachable). They stay on disk for the next pass.
	Retained []string
	Corrupt  []error
}

// Replay drains the WAL against the control plane and MUST run after
// registration but BEFORE the agent accepts new work (architecture §6.3).
//
// Order matters: a COMPLETED entry re-reported before we start polling means
// the control plane can finalise the attempt and release its reservation
// before we ask it for more work, so our own restart does not look like a
// capacity leak.
//
// The two cases and why they differ:
//
//   - COMPLETED: we observed the outcome. Re-report it verbatim. The server is
//     idempotent on attempt_id, so a duplicate report of an already-finalised
//     attempt is a no-op. This is the case that makes the WAL worth having.
//
//   - STARTING: we started the task and cannot know whether it ran, is still
//     running, or committed side effects. Reporting SUCCEEDED would be a lie;
//     reporting FAILED would be a different lie, and a dangerous one, since a
//     FAILED attempt with committed side effects gets retried under a policy
//     that assumes it did nothing. So we report INDETERMINATE, the control
//     plane records LOST, and §6.5 policy — which knows the task's
//     idempotency mode — decides whether a retry is safe. The agent must not
//     make that decision: it does not have the information.
func Replay(ctx context.Context, w *WAL, r Reporter, log *slog.Logger) ReplayResult {
	var out ReplayResult

	entries, errs := w.List()
	out.Corrupt = errs
	for _, err := range errs {
		log.Error("wal entry unreadable during replay", "error", err)
	}
	if len(entries) == 0 {
		return out
	}
	log.Info("replaying worker WAL", "entries", len(entries), "dir", w.Dir())

	for _, e := range entries {
		req, err := e.ToReportRequest()
		if err != nil {
			out.Corrupt = append(out.Corrupt, err)
			log.Error("wal entry could not be converted to a report", "attempt_id", e.AttemptID, "error", err)
			continue
		}
		lg := log.With("attempt_id", e.AttemptID, "wal_state", string(e.State),
			"outcome", req.GetOutcome().String(), "replay_count", e.ReplayCount)

		ack, err := r.ReportResult(ctx, req)
		if err != nil {
			// Transport failure. Keep the entry: the whole point is that the
			// outcome outlives our inability to deliver it.
			out.Retained = append(out.Retained, e.AttemptID)
			lg.Warn("wal replay report failed; entry retained for the next pass", "error", err)
			if berr := w.BumpReplayCount(e.AttemptID); berr != nil {
				lg.Warn("could not bump wal replay count", "error", berr)
			}
			continue
		}
		if !ack.GetAccepted() {
			// The server has authoritatively refused this report, almost
			// always "stale_lease_token": our lease expired and the work was
			// reassigned. No amount of retrying changes that, and the
			// idempotency key protects the side effects, so drop the entry.
			out.Rejected = append(out.Rejected, e.AttemptID)
			lg.Warn("wal replay rejected by control plane; dropping entry", "reason", ack.GetReason())
			if derr := w.Delete(e.AttemptID); derr != nil {
				lg.Error("could not delete rejected wal entry", "error", derr)
			}
			continue
		}

		out.Reported = append(out.Reported, e.AttemptID)
		if e.State == StateStarting {
			out.Indeterminate = append(out.Indeterminate, e.AttemptID)
		}
		if err := w.Delete(e.AttemptID); err != nil {
			// The report was acked, so correctness is safe; the entry will be
			// re-reported next boot and deduplicated. Log loudly because a
			// persistent failure here means an undrainable WAL.
			lg.Error("could not delete acked wal entry", "error", err)
		} else {
			lg.Info("wal entry replayed and acked")
		}
	}
	return out
}

// ToReportRequest rebuilds the ReportResult RPC from a durable entry.
//
// For a COMPLETED entry the stored payload is authoritative and is used as-is.
// For a STARTING entry we synthesise an INDETERMINATE report with the
// WORKER_LOST error class, which is the class the control plane's retry policy
// keys on for ambiguous outcomes.
func (e Entry) ToReportRequest() (*nexusv1.ReportResultRequest, error) {
	if e.State == StateCompleted {
		if len(e.ResultJSON) == 0 {
			return nil, fmt.Errorf("wal: COMPLETED entry %s has no result payload", e.AttemptID)
		}
		var req nexusv1.ReportResultRequest
		// protojson, not encoding/json: the payload contains protobuf enums,
		// oneofs and maps whose JSON shape only protojson gets right.
		if err := protojson.Unmarshal(e.ResultJSON, &req); err != nil {
			return nil, fmt.Errorf("wal: unmarshal result for %s: %w", e.AttemptID, err)
		}
		// The on-disk identity fields win: they are what the WAL guaranteed.
		req.AttemptId = e.AttemptID
		req.LeaseToken = e.LeaseToken
		if req.GetEndedAtUnixMs() == 0 {
			req.EndedAtUnixMs = e.EndedAtUnixMs
		}
		return &req, nil
	}

	return &nexusv1.ReportResultRequest{
		AttemptId:  e.AttemptID,
		LeaseToken: e.LeaseToken,
		Outcome:    nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_INDETERMINATE,
		Error: &nexusv1.TaskError{
			Class: nexusv1.ErrorClass_ERROR_CLASS_WORKER_LOST,
			Type:  "AgentRestart",
			Message: "agent restarted while this attempt was in flight; " +
				"the outcome is unknowable from the worker side",
			Retryable: true,
		},
		EndedAtUnixMs: e.EndedAtUnixMs,
	}, nil
}

// MarshalResult renders a completed report for durable storage. It lives here
// so the exact bytes the WAL persists and the exact bytes replay parses are
// produced by one pair of functions.
func MarshalResult(req *nexusv1.ReportResultRequest) (json.RawMessage, error) {
	b, err := protojson.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("wal: marshal result for %s: %w", req.GetAttemptId(), err)
	}
	return b, nil
}
