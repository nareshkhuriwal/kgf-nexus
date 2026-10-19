// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"strings"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

func TestIdempotencyModeProto(t *testing.T) {
	tests := []struct {
		name string
		spec *TaskSpecView
		want nexusv1.IdempotencyMode
	}{
		{"authoring form required", &TaskSpecView{IdempotencyMode: "required"},
			nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED},
		{"proto enum name", &TaskSpecView{IdempotencyMode: "IDEMPOTENCY_MODE_BEST_EFFORT"},
			nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_BEST_EFFORT},
		{"none", &TaskSpecView{IdempotencyMode: "none"},
			nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_NONE},
		// An absent or unrecognised declaration must NOT be optimistically read
		// as `required`. domain.ShouldRetry treats UNSPECIFIED as "no guarantee"
		// and refuses to auto-retry indeterminate outcomes; guessing would
		// authorize double execution of work that may not tolerate it.
		{"absent is unspecified, never assumed safe", &TaskSpecView{},
			nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_UNSPECIFIED},
		{"garbage is unspecified", &TaskSpecView{IdempotencyMode: "maybe?"},
			nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_UNSPECIFIED},
		{"nil spec is unspecified", nil,
			nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_UNSPECIFIED},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.IdempotencyModeProto(); got != tc.want {
				t.Fatalf("IdempotencyModeProto = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRetryPolicyDecoding(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		check func(*testing.T, *nexusv1.RetryPolicy)
	}{
		{
			name: "authoring form",
			raw: `{"max_attempts":4,"strategy":"exponential","initial_ms":10000,
			       "max_ms":600000,"multiplier":2,"jitter":"full",
			       "retry_on":["transient","timeout","worker_lost"],
			       "no_retry_on":["validation_error","permission_denied"],
			       "retry_on_different_worker":true}`,
			check: func(t *testing.T, p *nexusv1.RetryPolicy) {
				if p.GetMaxAttempts() != 4 {
					t.Errorf("max_attempts = %d, want 4", p.GetMaxAttempts())
				}
				if p.GetStrategy() != nexusv1.BackoffStrategy_BACKOFF_STRATEGY_EXPONENTIAL {
					t.Errorf("strategy = %v", p.GetStrategy())
				}
				if len(p.GetRetryOn()) != 3 {
					t.Errorf("retry_on = %v, want 3 classes", p.GetRetryOn())
				}
				if len(p.GetNoRetryOn()) != 2 {
					t.Errorf("no_retry_on = %v, want 2 classes", p.GetNoRetryOn())
				}
				if !p.GetRetryOnDifferentWorker() {
					t.Error("retry_on_different_worker was lost")
				}
			},
		},
		{
			name: "proto enum names",
			raw:  `{"strategy":"BACKOFF_STRATEGY_LINEAR","retry_on":["ERROR_CLASS_OOM"]}`,
			check: func(t *testing.T, p *nexusv1.RetryPolicy) {
				if p.GetStrategy() != nexusv1.BackoffStrategy_BACKOFF_STRATEGY_LINEAR {
					t.Errorf("strategy = %v, want LINEAR", p.GetStrategy())
				}
				if len(p.GetRetryOn()) != 1 ||
					p.GetRetryOn()[0] != nexusv1.ErrorClass_ERROR_CLASS_OOM {
					t.Errorf("retry_on = %v, want [OOM]", p.GetRetryOn())
				}
			},
		},
		{
			// An unparseable policy on ONE node must not stop the scheduler from
			// dispatching every other node, so it degrades to platform defaults
			// rather than erroring.
			name: "malformed json degrades to defaults",
			raw:  `{"max_attempts": "not a number"}`,
			check: func(t *testing.T, p *nexusv1.RetryPolicy) {
				if p == nil {
					t.Fatal("malformed policy returned nil; the caller would panic")
				}
				if p.GetMaxAttempts() != 0 {
					t.Errorf("max_attempts = %d, want 0 so MaxAttempts() applies the default",
						p.GetMaxAttempts())
				}
			},
		},
		{
			// An unknown class must be dropped, not mapped to UNSPECIFIED and
			// added to the list: UNSPECIFIED in retry_on would make an
			// unclassified error retryable, which is not what the author asked for.
			name: "unknown error classes are dropped",
			raw:  `{"retry_on":["transient","vibes"]}`,
			check: func(t *testing.T, p *nexusv1.RetryPolicy) {
				if len(p.GetRetryOn()) != 1 {
					t.Errorf("retry_on = %v, want only the recognised class", p.GetRetryOn())
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := &TaskSpecView{RetryJSON: json.RawMessage(tc.raw)}
			tc.check(t, spec.RetryPolicy())
		})
	}

	t.Run("absent policy and nil spec are non-nil defaults", func(t *testing.T) {
		if p := (&TaskSpecView{}).RetryPolicy(); p == nil {
			t.Fatal("empty spec returned a nil policy")
		}
		if p := (*TaskSpecView)(nil).RetryPolicy(); p == nil {
			t.Fatal("nil spec returned a nil policy")
		}
	})
}

// TestTaskSpecViewToleratesUnknownFields covers ADR-0011 rule 5: the DAG engine
// owns spec_json's shape, and an additive change there must not break dispatch.
func TestTaskSpecViewToleratesUnknownFields(t *testing.T) {
	raw := `{"task_ref":"ml.train","version":3,"start_to_close_ms":14400000,
	         "node_selector":{"zone":"us-east-1a"},"executor":"docker",
	         "a_field_from_the_future":{"nested":[1,2,3]},"another":"thing"}`
	var spec TaskSpecView
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatalf("unknown fields broke decoding: %v", err)
	}
	if spec.TaskRef != "ml.train" || spec.Version != 3 {
		t.Errorf("identity fields lost: %+v", spec)
	}
	if spec.StartToCloseMs != 14400000 {
		t.Errorf("start_to_close_ms = %d", spec.StartToCloseMs)
	}
	if spec.NodeSelector["zone"] != "us-east-1a" {
		t.Errorf("node_selector lost: %v", spec.NodeSelector)
	}
	if spec.Executor != "docker" {
		t.Errorf("executor = %q", spec.Executor)
	}
}

// TestTaskSpecViewReadsExecutorAndImageInEveryDialect: the claim places a task
// that names no executor by whether it carries an image, so the image must be
// read from the DAG engine's protojson (runtime.image, nested) as well as the
// flat fixture form; reading neither would let a kubernetes-default worker
// claim an image-less task.
func TestTaskSpecViewReadsExecutorAndImageInEveryDialect(t *testing.T) {
	cases := map[string]struct {
		raw, executor, image string
	}{
		"proto names":   {`{"task_ref":"t","runtime":{"executor":"kubernetes","image":"reg/x:1","command":["a"]}}`, "kubernetes", "reg/x:1"},
		"camel case":    {`{"taskRef":"t","runtime":{"image":"reg/y:2"},"timeouts":{"startToCloseMs":"60000"}}`, "", "reg/y:2"},
		"flat fixture":  {`{"task_ref":"t","executor":"docker","image":"reg/z:3"}`, "docker", "reg/z:3"},
		"no runtime":    {`{"task_ref":"t"}`, "", ""},
		"command only":  {`{"task_ref":"t","runtime":{"command":["echo"]}}`, "", ""},
		"quoted int64s": {`{"task_ref":"t","start_to_close_ms":"5","runtime":{"image":"reg/q:4"}}`, "", "reg/q:4"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var spec TaskSpecView
			if err := json.Unmarshal([]byte(c.raw), &spec); err != nil {
				t.Fatal(err)
			}
			if spec.Executor != c.executor || spec.Image != c.image {
				t.Errorf("executor=%q image=%q, want %q and %q", spec.Executor, spec.Image, c.executor, c.image)
			}
		})
	}
}

func TestClaimOutcomeStrings(t *testing.T) {
	// These strings appear in logs and metrics labels, so they are pinned.
	tests := map[ClaimOutcome]string{
		ClaimNothingPending: "nothing_pending",
		ClaimBlocked:        "blocked",
		ClaimDispatched:     "dispatched",
		ClaimRaceLost:       "race_lost",
		ClaimOutcome(99):    "unknown",
	}
	for in, want := range tests {
		if got := in.String(); got != want {
			t.Errorf("ClaimOutcome(%d).String() = %q, want %q", in, got, want)
		}
	}
}

// TestLedgerReadsTheDefaultExecutorLabel pins the literal label in
// recordUsageSQL (a constant, so it cannot interpolate the Go name) to
// domain.LabelDefaultExecutor: a renamed label would silently meter every
// unnamed-executor attempt as NULL.
func TestLedgerReadsTheDefaultExecutorLabel(t *testing.T) {
	if !strings.Contains(recordUsageSQL, "'"+domain.LabelDefaultExecutor+"'") {
		t.Fatalf("recordUsageSQL does not read the %s label", domain.LabelDefaultExecutor)
	}
}

func TestShortfallReasonForAnUnnamedExecutor(t *testing.T) {
	r := shortfallReason([]domain.Shortfall{{Dim: shortfallUnnamedExecutor, Detail: "kubernetes"}})
	if !strings.HasPrefix(r, "waiting_for_worker: ") || !strings.Contains(r, "(kubernetes, label nexus.io/default-executor)") {
		t.Errorf("reason = %q", r)
	}
	// Any other shortfall is unchanged.
	short := []domain.Shortfall{{Dim: "gpu", Need: 4, Free: 3}}
	if got, want := shortfallReason(short), domain.ShortfallReason(short); got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
}
