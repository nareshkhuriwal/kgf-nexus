package domain

import (
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Machine-readable reasons for a retry decision. They are written into
// run_events payloads and surfaced in the UI, so operators and alert rules can
// distinguish "we gave up because the budget ran out" from "we refused because
// we cannot safely re-run this".
const (
	ReasonRetryTransient = "retry_error_class_retryable"
	ReasonRetryLost      = "retry_indeterminate_idempotent"
	ReasonRetryLostBest  = "retry_indeterminate_best_effort"

	ReasonNoRetryAttemptsExhausted = "attempts_exhausted"
	ReasonNoRetryScheduleToClose   = "schedule_to_close_exceeded"
	ReasonNoRetryTerminalClass     = "error_class_terminal"
	ReasonNoRetryDenied            = "error_class_in_no_retry_on"
	ReasonNoRetryNotInRetryOn      = "error_class_not_in_retry_on"
	// ReasonIndeterminateRequiresHuman is the §6.5 refusal: the outcome is
	// unknown AND the task declared it is not safe to re-run, so the only
	// correct action is to stop and ask a human. Distinct from every other
	// no-retry reason because it is the one case where we are declining work
	// we could technically do, and the UI must offer `attempt retry --force`
	// rather than presenting the run as simply broken.
	ReasonIndeterminateRequiresHuman = "indeterminate_outcome_requires_human"
)

// RetryDecision is the outcome of evaluating retry policy for a terminal
// attempt.
//
// Reason is always populated, including when Retry is true, because "why did
// this retry?" is as much an operational question as "why did it not?".
type RetryDecision struct {
	// Retry is true when the scheduler should create a NEW task_attempts row
	// with attempt_number+1 (never mutate the terminal attempt — invariant 9).
	Retry bool
	// Reason is a stable machine-readable code, not prose for humans.
	Reason string
	// Warn is true when the retry is permitted but may cause real duplicate
	// execution, i.e. best_effort idempotency on an indeterminate outcome. The
	// caller must emit PossibleDuplicateExecution (§6.5) — we owe the operator
	// that warning rather than quietly hoping it worked out.
	Warn bool
}

// defaultRetryOn is the platform's retryable set, used when a policy declares
// no allowlist. Empty retry_on means "platform default" per the proto, not
// "retry nothing" — the latter reading would silently disable retries for
// every task that omitted the field.
var defaultRetryOn = map[nexusv1.ErrorClass]bool{
	nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT:      true,
	nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT:        true,
	nexusv1.ErrorClass_ERROR_CLASS_WORKER_LOST:    true,
	nexusv1.ErrorClass_ERROR_CLASS_OOM:            true,
	nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE: true,
}

// alwaysTerminal classes cannot be fixed by running the same code on the same
// input again. Retrying them burns capacity and delays the failure the author
// needs to see, so they are terminal even if an author mistakenly lists them in
// retry_on.
var alwaysTerminal = map[nexusv1.ErrorClass]bool{
	nexusv1.ErrorClass_ERROR_CLASS_VALIDATION:        true,
	nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED: true,
	nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND:         true,
}

// ShouldRetry implements architecture §6.5 and the §5.1 RETRY predicate.
//
// The order of the checks is itself the policy, and it is not arbitrary:
//
//  1. Budget checks first (attempts, schedule_to_close). An exhausted budget is
//     an unambiguous no regardless of error class, and reporting it as such
//     gives the operator the right lever (raise max_attempts / the SLA).
//  2. The indeterminate-outcome gate next. WORKER_LOST with
//     IDEMPOTENCY_MODE_NONE must NOT auto-retry, even though WORKER_LOST is in
//     the default retryable set and the budget allows it. This check therefore
//     has to sit ahead of the class allowlist, or the allowlist would let it
//     through. A platform that quietly double-charges a customer because
//     retrying was more convenient has chosen the wrong failure mode.
//  3. Class policy last: no_retry_on (author veto) beats retry_on, and the
//     always-terminal classes beat both.
//
// scheduleToCloseExceeded is passed in rather than computed here because it is
// a comparison against control-plane time on the RUN's deadline; worker clocks
// are never authoritative (invariant 7) and this package holds no clock.
func ShouldRetry(
	errClass nexusv1.ErrorClass,
	idem nexusv1.IdempotencyMode,
	attemptsUsed int32,
	maxAttempts int32,
	scheduleToCloseExceeded bool,
	policy *nexusv1.RetryPolicy,
) RetryDecision {
	if maxAttempts <= 0 {
		maxAttempts = MaxAttempts(policy)
	}
	if attemptsUsed >= maxAttempts {
		return RetryDecision{Reason: ReasonNoRetryAttemptsExhausted}
	}
	// schedule_to_close is the real end-to-end SLA across ALL retries. Once it
	// is blown, another attempt cannot deliver the result on time, so spending
	// cluster capacity on it is pure waste.
	if scheduleToCloseExceeded {
		return RetryDecision{Reason: ReasonNoRetryScheduleToClose}
	}

	indeterminate := errClass == nexusv1.ErrorClass_ERROR_CLASS_WORKER_LOST
	if indeterminate {
		switch idem {
		case nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_NONE:
			// We do not know whether the work ran, and the author told us it is
			// not safe to run twice. Refusing is the only honest answer; a human
			// with domain knowledge can force it (§6.5).
			return RetryDecision{Reason: ReasonIndeterminateRequiresHuman}
		case nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_BEST_EFFORT:
			if d := classAllowed(errClass, policy); !d.Retry {
				return d
			}
			return RetryDecision{Retry: true, Reason: ReasonRetryLostBest, Warn: true}
		case nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED:
			if d := classAllowed(errClass, policy); !d.Retry {
				return d
			}
			return RetryDecision{Retry: true, Reason: ReasonRetryLost}
		default:
			// UNSPECIFIED. The platform default is `required` (§4.3), but
			// defaulting an UNKNOWN declaration to "safe to re-run" would let a
			// spec bug or a version skew silently authorize double execution of
			// non-idempotent work. Treat an unstated guarantee as no guarantee.
			return RetryDecision{Reason: ReasonIndeterminateRequiresHuman}
		}
	}

	if d := classAllowed(errClass, policy); !d.Retry {
		return d
	}
	return RetryDecision{Retry: true, Reason: ReasonRetryTransient}
}

// classAllowed evaluates error-class policy alone. On success it returns
// Retry:true with an empty reason, which the caller replaces with the specific
// reason for its branch; on refusal the reason is final.
func classAllowed(errClass nexusv1.ErrorClass, policy *nexusv1.RetryPolicy) RetryDecision {
	// An explicit author veto wins over everything, including the default set.
	for _, c := range policy.GetNoRetryOn() {
		if c == errClass {
			return RetryDecision{Reason: ReasonNoRetryDenied}
		}
	}
	if alwaysTerminal[errClass] {
		return RetryDecision{Reason: ReasonNoRetryTerminalClass}
	}
	if allow := policy.GetRetryOn(); len(allow) > 0 {
		for _, c := range allow {
			if c == errClass {
				return RetryDecision{Retry: true}
			}
		}
		return RetryDecision{Reason: ReasonNoRetryNotInRetryOn}
	}
	if defaultRetryOn[errClass] {
		return RetryDecision{Retry: true}
	}
	// APPLICATION and UNSPECIFIED land here: task code raised, and only an
	// explicit allowlist should make that retryable (proto comment on
	// ERROR_CLASS_APPLICATION).
	return RetryDecision{Reason: ReasonNoRetryNotInRetryOn}
}

// ErrorClassForState maps a terminal attempt state to the error class used for
// retry evaluation when the worker supplied none — the lease-expiry and
// liveness-sweep paths, where by definition no worker report ever arrived.
//
// LOST maps to WORKER_LOST specifically so it flows through the indeterminate
// gate above, which is the entire reason LOST is a separate state from FAILED.
func ErrorClassForState(s AttemptState) nexusv1.ErrorClass {
	switch s {
	case AttemptLost:
		return nexusv1.ErrorClass_ERROR_CLASS_WORKER_LOST
	case AttemptTimedOut:
		return nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT
	case AttemptFailed:
		return nexusv1.ErrorClass_ERROR_CLASS_APPLICATION
	default:
		return nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED
	}
}
