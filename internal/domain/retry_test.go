package domain

import (
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

const (
	ecTransient = nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT
	ecTimeout   = nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT
	ecLost      = nexusv1.ErrorClass_ERROR_CLASS_WORKER_LOST
	ecValid     = nexusv1.ErrorClass_ERROR_CLASS_VALIDATION
	ecPerm      = nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED
	ecApp       = nexusv1.ErrorClass_ERROR_CLASS_APPLICATION
	ecOOM       = nexusv1.ErrorClass_ERROR_CLASS_OOM
	ecNotFound  = nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND
	ecInfra     = nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE

	idemRequired = nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED
	idemBest     = nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_BEST_EFFORT
	idemNone     = nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_NONE
	idemUnspec   = nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_UNSPECIFIED
)

// TestShouldRetry65Matrix is the §6.5 table, asserted cell by cell. The three
// LOST rows are the ones that matter most: they are the difference between a
// platform that refuses to double-charge a customer and one that does it
// quietly because retrying was more convenient.
func TestShouldRetry65Matrix(t *testing.T) {
	tests := []struct {
		name        string
		errClass    nexusv1.ErrorClass
		idem        nexusv1.IdempotencyMode
		used        int32
		maxAttempts int32
		stcExceeded bool
		policy      *nexusv1.RetryPolicy
		wantRetry   bool
		wantReason  string
		wantWarn    bool
	}{
		// ── §6.5 row 1: idempotency required ───────────────────────────────
		{
			name:     "LOST + required auto-retries",
			errClass: ecLost, idem: idemRequired, used: 1, maxAttempts: 3,
			wantRetry: true, wantReason: ReasonRetryLost,
		},
		// ── §6.5 row 2: best_effort ────────────────────────────────────────
		{
			name:     "LOST + best_effort retries WITH a duplicate-execution warning",
			errClass: ecLost, idem: idemBest, used: 1, maxAttempts: 3,
			wantRetry: true, wantReason: ReasonRetryLostBest, wantWarn: true,
		},
		// ── §6.5 row 3: the refusal. THE load-bearing cell. ────────────────
		{
			name:     "LOST + none does NOT auto-retry and demands a human",
			errClass: ecLost, idem: idemNone, used: 1, maxAttempts: 3,
			wantRetry: false, wantReason: ReasonIndeterminateRequiresHuman,
		},
		{
			// An unstated idempotency guarantee is no guarantee. Defaulting
			// UNSPECIFIED to "safe" would let a spec bug or a version skew
			// authorize double execution of non-idempotent work.
			name:     "LOST + unspecified idempotency is treated as unsafe",
			errClass: ecLost, idem: idemUnspec, used: 1, maxAttempts: 3,
			wantRetry: false, wantReason: ReasonIndeterminateRequiresHuman,
		},
		{
			// Budget checks precede the indeterminate gate: an exhausted budget
			// is an unambiguous no whose reason points at the right lever.
			name:     "LOST + none with the budget already spent reports exhaustion",
			errClass: ecLost, idem: idemNone, used: 3, maxAttempts: 3,
			wantRetry: false, wantReason: ReasonNoRetryAttemptsExhausted,
		},
		{
			name:     "LOST + required but attempts exhausted",
			errClass: ecLost, idem: idemRequired, used: 3, maxAttempts: 3,
			wantRetry: false, wantReason: ReasonNoRetryAttemptsExhausted,
		},
		{
			name:     "LOST + required but schedule_to_close blown",
			errClass: ecLost, idem: idemRequired, used: 1, maxAttempts: 5, stcExceeded: true,
			wantRetry: false, wantReason: ReasonNoRetryScheduleToClose,
		},
		{
			// An author veto beats even the indeterminate-but-idempotent path.
			name:     "LOST + required but worker_lost is explicitly vetoed",
			errClass: ecLost, idem: idemRequired, used: 1, maxAttempts: 3,
			policy:    &nexusv1.RetryPolicy{NoRetryOn: []nexusv1.ErrorClass{ecLost}},
			wantRetry: false, wantReason: ReasonNoRetryDenied,
		},
		{
			name:     "LOST + best_effort but worker_lost absent from an explicit retry_on",
			errClass: ecLost, idem: idemBest, used: 1, maxAttempts: 3,
			policy:    &nexusv1.RetryPolicy{RetryOn: []nexusv1.ErrorClass{ecTransient}},
			wantRetry: false, wantReason: ReasonNoRetryNotInRetryOn,
		},

		// ── §5.1 retryable classes (determinate outcomes) ──────────────────
		{
			name:     "TRANSIENT retries under the default set",
			errClass: ecTransient, idem: idemRequired, used: 1, maxAttempts: 3,
			wantRetry: true, wantReason: ReasonRetryTransient,
		},
		{
			name:     "TIMEOUT retries",
			errClass: ecTimeout, idem: idemRequired, used: 1, maxAttempts: 3,
			wantRetry: true, wantReason: ReasonRetryTransient,
		},
		{
			name:     "OOM retries; a larger worker may succeed",
			errClass: ecOOM, idem: idemRequired, used: 1, maxAttempts: 3,
			wantRetry: true, wantReason: ReasonRetryTransient,
		},
		{
			name:     "INFRASTRUCTURE retries",
			errClass: ecInfra, idem: idemRequired, used: 1, maxAttempts: 3,
			wantRetry: true, wantReason: ReasonRetryTransient,
		},
		{
			// idempotency mode is irrelevant for a DETERMINATE outcome: we know
			// the work did not succeed, so re-running it is not a duplicate.
			name:     "TRANSIENT + none still retries; the outcome is not indeterminate",
			errClass: ecTransient, idem: idemNone, used: 1, maxAttempts: 3,
			wantRetry: true, wantReason: ReasonRetryTransient,
		},
		{
			name:     "TIMEOUT + none still retries",
			errClass: ecTimeout, idem: idemNone, used: 1, maxAttempts: 3,
			wantRetry: true, wantReason: ReasonRetryTransient,
		},

		// ── Always-terminal classes ────────────────────────────────────────
		{
			name:     "VALIDATION never retries; the same input will fail again",
			errClass: ecValid, idem: idemRequired, used: 1, maxAttempts: 3,
			wantRetry: false, wantReason: ReasonNoRetryTerminalClass,
		},
		{
			name:     "PERMISSION_DENIED never retries",
			errClass: ecPerm, idem: idemRequired, used: 1, maxAttempts: 3,
			wantRetry: false, wantReason: ReasonNoRetryTerminalClass,
		},
		{
			name:     "NOT_FOUND never retries",
			errClass: ecNotFound, idem: idemRequired, used: 1, maxAttempts: 3,
			wantRetry: false, wantReason: ReasonNoRetryTerminalClass,
		},
		{
			// Even an author who lists VALIDATION in retry_on does not get
			// retries: retrying burns capacity and delays the failure they need
			// to see.
			name:     "an always-terminal class in retry_on is still terminal",
			errClass: ecValid, idem: idemRequired, used: 1, maxAttempts: 3,
			policy:    &nexusv1.RetryPolicy{RetryOn: []nexusv1.ErrorClass{ecValid}},
			wantRetry: false, wantReason: ReasonNoRetryTerminalClass,
		},

		// ── APPLICATION: opt-in only ───────────────────────────────────────
		{
			name:     "APPLICATION does not retry by default",
			errClass: ecApp, idem: idemRequired, used: 1, maxAttempts: 3,
			wantRetry: false, wantReason: ReasonNoRetryNotInRetryOn,
		},
		{
			name:     "APPLICATION retries when explicitly allowlisted",
			errClass: ecApp, idem: idemRequired, used: 1, maxAttempts: 3,
			policy:    &nexusv1.RetryPolicy{RetryOn: []nexusv1.ErrorClass{ecApp}},
			wantRetry: true, wantReason: ReasonRetryTransient,
		},
		{
			name:     "UNSPECIFIED error class does not retry",
			errClass: nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED, idem: idemRequired,
			used: 1, maxAttempts: 3,
			wantRetry: false, wantReason: ReasonNoRetryNotInRetryOn,
		},

		// ── no_retry_on beats retry_on ─────────────────────────────────────
		{
			name:     "no_retry_on wins over retry_on for the same class",
			errClass: ecTransient, idem: idemRequired, used: 1, maxAttempts: 3,
			policy: &nexusv1.RetryPolicy{
				RetryOn:   []nexusv1.ErrorClass{ecTransient},
				NoRetryOn: []nexusv1.ErrorClass{ecTransient},
			},
			wantRetry: false, wantReason: ReasonNoRetryDenied,
		},

		// ── Budget edges ───────────────────────────────────────────────────
		{
			name:     "last permitted attempt still retries",
			errClass: ecTransient, idem: idemRequired, used: 2, maxAttempts: 3,
			wantRetry: true, wantReason: ReasonRetryTransient,
		},
		{
			name:     "used beyond max is exhausted, not negative-budget retryable",
			errClass: ecTransient, idem: idemRequired, used: 9, maxAttempts: 3,
			wantRetry: false, wantReason: ReasonNoRetryAttemptsExhausted,
		},
		{
			name:     "maxAttempts 1 means no retries at all",
			errClass: ecTransient, idem: idemRequired, used: 1, maxAttempts: 1,
			wantRetry: false, wantReason: ReasonNoRetryAttemptsExhausted,
		},
		{
			// A zero ceiling must resolve from policy, never mean "unlimited".
			name:     "zero maxAttempts resolves from the policy",
			errClass: ecTransient, idem: idemRequired, used: 4, maxAttempts: 0,
			policy:    &nexusv1.RetryPolicy{MaxAttempts: 5},
			wantRetry: true, wantReason: ReasonRetryTransient,
		},
		{
			name:     "zero maxAttempts with no policy uses the platform default",
			errClass: ecTransient, idem: idemRequired, used: DefaultMaxAttempts, maxAttempts: 0,
			wantRetry: false, wantReason: ReasonNoRetryAttemptsExhausted,
		},
		{
			name:     "schedule_to_close outranks a retryable class",
			errClass: ecTransient, idem: idemRequired, used: 1, maxAttempts: 3, stcExceeded: true,
			wantRetry: false, wantReason: ReasonNoRetryScheduleToClose,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldRetry(tc.errClass, tc.idem, tc.used, tc.maxAttempts, tc.stcExceeded, tc.policy)
			if got.Retry != tc.wantRetry {
				t.Errorf("Retry = %v, want %v (reason %q)", got.Retry, tc.wantRetry, got.Reason)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if got.Warn != tc.wantWarn {
				t.Errorf("Warn = %v, want %v", got.Warn, tc.wantWarn)
			}
			if got.Reason == "" {
				t.Error("Reason must always be populated; it is written to run_events")
			}
		})
	}
}

// TestOnlyBestEffortLostWarns pins the exact scope of the
// PossibleDuplicateExecution warning: over the entire class x mode cross
// product, Warn is true in exactly one cell. Warning too broadly trains
// operators to ignore it; warning too narrowly hides a real double execution.
func TestOnlyBestEffortLostWarns(t *testing.T) {
	classes := []nexusv1.ErrorClass{ecTransient, ecTimeout, ecLost, ecValid, ecPerm, ecApp, ecOOM, ecNotFound, ecInfra}
	modes := []nexusv1.IdempotencyMode{idemUnspec, idemRequired, idemBest, idemNone}
	warned := 0
	for _, c := range classes {
		for _, m := range modes {
			d := ShouldRetry(c, m, 1, 5, false, nil)
			if !d.Warn {
				continue
			}
			warned++
			if c != ecLost || m != idemBest {
				t.Errorf("unexpected warn for class=%v mode=%v", c, m)
			}
			if !d.Retry {
				t.Errorf("warn without retry is meaningless for class=%v mode=%v", c, m)
			}
		}
	}
	if warned != 1 {
		t.Fatalf("warned in %d cells, want exactly 1 (best_effort + LOST)", warned)
	}
}

func TestErrorClassForState(t *testing.T) {
	tests := []struct {
		state AttemptState
		want  nexusv1.ErrorClass
	}{
		// LOST must map to WORKER_LOST so it reaches the §6.5 indeterminate
		// gate. Mapping it to APPLICATION would let non-idempotent work be
		// auto-retried, which is the bug this whole design exists to prevent.
		{AttemptLost, ecLost},
		{AttemptTimedOut, ecTimeout},
		{AttemptFailed, ecApp},
		{AttemptSucceeded, nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED},
		{AttemptPending, nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED},
		{AttemptCancelled, nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			if got := ErrorClassForState(tc.state); got != tc.want {
				t.Fatalf("ErrorClassForState(%s) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

// TestLostStateFlowsToRefusal is the end-to-end composition the lease-expiry
// reconciler actually performs: an expired lease on a non-idempotent task must
// come out as a refusal to retry, not a silent re-dispatch.
func TestLostStateFlowsToRefusal(t *testing.T) {
	d := ShouldRetry(ErrorClassForState(AttemptLost), idemNone, 1, 3, false, nil)
	if d.Retry || d.Reason != ReasonIndeterminateRequiresHuman {
		t.Fatalf("expired lease on a non-idempotent task = %+v, want a human-required refusal", d)
	}
}
