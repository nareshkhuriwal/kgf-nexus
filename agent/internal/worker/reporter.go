package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/wal"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// ReportClient is the reporting slice of the worker RPC surface.
type ReportClient interface {
	ReportStarted(ctx context.Context, req *nexusv1.ReportStartedRequest) (*nexusv1.Ack, error)
	ReportProgress(ctx context.Context, req *nexusv1.ReportProgressRequest) (*nexusv1.Ack, error)
	ReportResult(ctx context.Context, req *nexusv1.ReportResultRequest) (*nexusv1.Ack, error)
}

// Reporter delivers outcomes to the control plane and drains the WAL behind
// them.
type Reporter struct {
	client ReportClient
	wal    *wal.WAL
	log    *slog.Logger

	// MaxAttempts bounds retries. Not unbounded: an agent that retries forever
	// against a control plane that has permanently moved on holds its WAL entry
	// and its slot in the reporting goroutine indefinitely. On exhaustion the
	// entry STAYS in the WAL and is replayed at next boot, so nothing is lost —
	// the retry budget bounds effort, not durability.
	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// NewReporter builds a reporter with sane retry bounds.
func NewReporter(c ReportClient, w *wal.WAL, log *slog.Logger) *Reporter {
	return &Reporter{
		client: c, wal: w, log: log,
		MaxAttempts: 8,
		BaseBackoff: 250 * time.Millisecond,
		MaxBackoff:  15 * time.Second,
	}
}

// ReportStarted tells the control plane the attempt is RUNNING.
//
// Best-effort by design: a failure here is logged and swallowed. The attempt is
// already recorded STARTING in the WAL and the lease is already being renewed, so
// the control plane will learn the outcome regardless. Failing the task because a
// status notification did not land would discard real work for a cosmetic reason.
func (r *Reporter) ReportStarted(ctx context.Context, attemptID, token string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ack, err := r.client.ReportStarted(ctx, &nexusv1.ReportStartedRequest{
		AttemptId: attemptID, LeaseToken: token,
		StartedAtUnixMs: time.Now().UnixMilli(),
	})
	switch {
	case err != nil:
		r.log.Warn("ReportStarted failed; continuing (the WAL and lease carry the attempt)",
			"attempt_id", attemptID, "error", err)
	case !ack.GetAccepted():
		r.log.Warn("ReportStarted rejected", "attempt_id", attemptID, "reason", ack.GetReason())
	}
}

// ReportProgress carries task-level liveness for long-running work, resetting the
// heartbeat timeout for THIS attempt without touching worker liveness. The
// distinction matters: a worker can be perfectly alive while one task is wedged,
// and keeping the two signals separate lets the control plane time out the task
// instead of killing the worker and every sibling attempt with it.
func (r *Reporter) ReportProgress(ctx context.Context, attemptID, token, msg string, fraction float64) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := r.client.ReportProgress(ctx, &nexusv1.ReportProgressRequest{
		AttemptId: attemptID, LeaseToken: token, Message: msg, FractionComplete: fraction,
	}); err != nil {
		r.log.Warn("ReportProgress failed", "attempt_id", attemptID, "error", err)
	}
}

// ErrRejected means the control plane authoritatively refused the report, almost
// always a stale lease token. Distinct from a transport error because it is
// terminal: retrying cannot make a fenced-off token valid again.
var ErrRejected = errors.New("report rejected by control plane")

// ReportResult persists the outcome to the WAL, reports it with retry+backoff,
// and deletes the WAL entry only after a durable ack.
//
// THE ORDERING IS THE CONTRACT (§6.3):
//  1. fsync the outcome,
//  2. then report,
//  3. then delete.
//
// Any other order reintroduces the lost-result window. Writing after reporting
// loses the outcome to a crash in between; deleting before the ack loses it to a
// crash after a report the control plane never committed.
func (r *Reporter) ReportResult(ctx context.Context, req *nexusv1.ReportResultRequest, entry wal.Entry) error {
	payload, err := wal.MarshalResult(req)
	if err != nil {
		return err
	}
	entry.AttemptID = req.GetAttemptId()
	entry.LeaseToken = req.GetLeaseToken()
	entry.Outcome = outcomeToWAL(req.GetOutcome())
	entry.ResultJSON = payload
	entry.EndedAtUnixMs = req.GetEndedAtUnixMs()

	if err := r.wal.RecordCompleted(entry); err != nil {
		// We could not make the outcome durable. Report it anyway — an
		// un-durable report is still better than no report — but say so loudly,
		// because a WAL that cannot be written means the next crash DOES lose
		// results, and that is an operator-actionable disk problem.
		r.log.Error("could not persist outcome to the WAL before reporting; a crash now WOULD lose this result",
			"attempt_id", req.GetAttemptId(), "error", err)
	}

	if err := r.deliver(ctx, req); err != nil {
		if errors.Is(err, ErrRejected) {
			// Fenced off. The entry is useless: the server will reject it
			// forever, and the idempotency key protects the side effects.
			if derr := r.wal.Delete(req.GetAttemptId()); derr != nil {
				r.log.Error("could not delete rejected WAL entry", "attempt_id", req.GetAttemptId(), "error", derr)
			}
			return err
		}
		// Transport exhausted. LEAVE the entry: replay at next boot delivers it.
		r.log.Error("exhausted report retries; outcome remains in the WAL for replay on restart",
			"attempt_id", req.GetAttemptId(), "error", err)
		return err
	}

	if err := r.wal.Delete(req.GetAttemptId()); err != nil {
		// Harmless for correctness (a re-report is deduplicated server-side) but
		// a persistent failure means an undrainable WAL, so it is an error.
		r.log.Error("outcome acked but WAL entry could not be deleted",
			"attempt_id", req.GetAttemptId(), "error", err)
	}
	return nil
}

// deliver retries with exponential backoff and FULL JITTER.
//
// Full jitter, not fixed backoff: when a control-plane instance restarts, every
// worker's in-flight report fails at the same instant. Synchronised backoff
// retries them all at the same instant too, re-DoSing the instance that just came
// back. Randomising across the whole window spreads the herd.
func (r *Reporter) deliver(ctx context.Context, req *nexusv1.ReportResultRequest) error {
	var lastErr error
	for attempt := 0; attempt < r.MaxAttempts; attempt++ {
		if attempt > 0 {
			d := r.backoff(attempt)
			select {
			case <-ctx.Done():
				return fmt.Errorf("report cancelled while backing off: %w", ctx.Err())
			case <-time.After(d):
			}
		}
		rpcCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		ack, err := r.client.ReportResult(rpcCtx, req)
		cancel()

		if err == nil && ack.GetAccepted() {
			r.log.Info("result reported and durably acked",
				"attempt_id", req.GetAttemptId(), "outcome", req.GetOutcome().String(),
				"report_attempts", attempt+1)
			return nil
		}
		if err == nil {
			// An explicit refusal is terminal; do not spend the retry budget.
			return fmt.Errorf("%w: %s", ErrRejected, ack.GetReason())
		}
		lastErr = err
		r.log.Warn("ReportResult failed; will retry",
			"attempt_id", req.GetAttemptId(), "try", attempt+1, "of", r.MaxAttempts, "error", err)
	}
	return fmt.Errorf("report result for %s after %d tries: %w", req.GetAttemptId(), r.MaxAttempts, lastErr)
}

func (r *Reporter) backoff(attempt int) time.Duration {
	d := float64(r.BaseBackoff) * math.Pow(2, float64(attempt-1))
	if d > float64(r.MaxBackoff) {
		d = float64(r.MaxBackoff)
	}
	return time.Duration(rand.Int63n(int64(d)) + 1)
}

func outcomeToWAL(o nexusv1.AttemptOutcome) wal.Outcome {
	switch o {
	case nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED:
		return wal.OutcomeSucceeded
	case nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_TIMED_OUT:
		return wal.OutcomeTimedOut
	case nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_INDETERMINATE:
		return wal.OutcomeIndeterminate
	default:
		return wal.OutcomeFailed
	}
}
