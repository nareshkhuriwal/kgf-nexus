// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"testing"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func TestBackoffLadders(t *testing.T) {
	// jitter=none isolates the deterministic ladder from the jitter, so the
	// growth curve itself can be asserted exactly.
	tests := []struct {
		name     string
		policy   *nexusv1.RetryPolicy
		attempts []int32
		want     []time.Duration
	}{
		{
			name: "fixed does not grow",
			policy: &nexusv1.RetryPolicy{
				Strategy:  nexusv1.BackoffStrategy_BACKOFF_STRATEGY_FIXED,
				InitialMs: 1000, MaxMs: 60000, Jitter: JitterNone,
			},
			attempts: []int32{1, 2, 3, 10},
			want:     []time.Duration{time.Second, time.Second, time.Second, time.Second},
		},
		{
			name: "linear grows by initial each step",
			policy: &nexusv1.RetryPolicy{
				Strategy:  nexusv1.BackoffStrategy_BACKOFF_STRATEGY_LINEAR,
				InitialMs: 1000, MaxMs: 60000, Jitter: JitterNone,
			},
			attempts: []int32{1, 2, 3, 4},
			want:     []time.Duration{1 * time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second},
		},
		{
			name: "exponential doubles by default multiplier",
			policy: &nexusv1.RetryPolicy{
				Strategy:  nexusv1.BackoffStrategy_BACKOFF_STRATEGY_EXPONENTIAL,
				InitialMs: 1000, MaxMs: 600000, Multiplier: 2, Jitter: JitterNone,
			},
			attempts: []int32{1, 2, 3, 4, 5},
			want: []time.Duration{
				1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
			},
		},
		{
			name: "exponential honours a custom multiplier",
			policy: &nexusv1.RetryPolicy{
				Strategy:  nexusv1.BackoffStrategy_BACKOFF_STRATEGY_EXPONENTIAL,
				InitialMs: 1000, MaxMs: 600000, Multiplier: 3, Jitter: JitterNone,
			},
			attempts: []int32{1, 2, 3},
			want:     []time.Duration{time.Second, 3 * time.Second, 9 * time.Second},
		},
		{
			name: "max_ms is a hard ceiling",
			policy: &nexusv1.RetryPolicy{
				Strategy:  nexusv1.BackoffStrategy_BACKOFF_STRATEGY_EXPONENTIAL,
				InitialMs: 1000, MaxMs: 5000, Multiplier: 2, Jitter: JitterNone,
			},
			attempts: []int32{1, 2, 3, 4, 20},
			want: []time.Duration{
				time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second,
			},
		},
		{
			// A huge attempt number overflows math.Pow to +Inf; without the
			// clamp the int64 conversion yields a negative duration and the
			// retry fires immediately, which is the opposite of backing off.
			name: "extreme attempt numbers clamp, never overflow negative",
			policy: &nexusv1.RetryPolicy{
				Strategy:  nexusv1.BackoffStrategy_BACKOFF_STRATEGY_EXPONENTIAL,
				InitialMs: 1000, MaxMs: 60000, Multiplier: 10, Jitter: JitterNone,
			},
			attempts: []int32{500, 100000},
			want:     []time.Duration{60 * time.Second, 60 * time.Second},
		},
		{
			name: "unspecified strategy falls back to exponential",
			policy: &nexusv1.RetryPolicy{
				InitialMs: 1000, MaxMs: 60000, Multiplier: 2, Jitter: JitterNone,
			},
			attempts: []int32{1, 2, 3},
			want:     []time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
		},
		{
			name: "max below initial honours the ceiling",
			policy: &nexusv1.RetryPolicy{
				Strategy:  nexusv1.BackoffStrategy_BACKOFF_STRATEGY_FIXED,
				InitialMs: 60000, MaxMs: 1000, Jitter: JitterNone,
			},
			attempts: []int32{1, 2},
			want:     []time.Duration{time.Second, time.Second},
		},
		{
			name: "attempt 0 and negatives are treated as the first step",
			policy: &nexusv1.RetryPolicy{
				Strategy:  nexusv1.BackoffStrategy_BACKOFF_STRATEGY_EXPONENTIAL,
				InitialMs: 1000, MaxMs: 60000, Multiplier: 2, Jitter: JitterNone,
			},
			attempts: []int32{0, -5},
			want:     []time.Duration{time.Second, time.Second},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for i, a := range tc.attempts {
				got := Backoff(tc.policy, a)
				if got != tc.want[i] {
					t.Errorf("Backoff(attempt=%d) = %v, want %v", a, got, tc.want[i])
				}
			}
		})
	}
}

// TestBackoffNilPolicyDefaults covers the nil/zero policy the DAG engine may
// hand us for a task whose author omitted the retry block. A nil policy leaves
// jitter unset, which means FULL jitter, so the delay is a random point in
// [0, DefaultInitialBackoff] — the assertion is on the bound, not a point.
// What must never happen is a zero ceiling, which would turn a retryable
// infrastructure fault into a hot loop against an already-unhealthy system.
func TestBackoffNilPolicyDefaults(t *testing.T) {
	if got := backoffWith(nil, 1, func() float64 { return 1 }); got != DefaultInitialBackoff {
		t.Fatalf("nil-policy ceiling = %v, want %v", got, DefaultInitialBackoff)
	}
	for i := 0; i < 200; i++ {
		if d := Backoff(nil, 1); d < 0 || d > DefaultInitialBackoff {
			t.Fatalf("Backoff(nil) = %v, outside [0, %v]", d, DefaultInitialBackoff)
		}
	}
}

func TestBackoffJitterBounds(t *testing.T) {
	// Full jitter must span the whole [0, delay] window: that uniform spread is
	// what decorrelates a fleet-wide retry storm. Bounds are asserted with a
	// fixed uniform source rather than sampled, so the test cannot flake.
	pol := &nexusv1.RetryPolicy{
		Strategy:  nexusv1.BackoffStrategy_BACKOFF_STRATEGY_EXPONENTIAL,
		InitialMs: 1000, MaxMs: 600000, Multiplier: 2,
	}
	tests := []struct {
		name    string
		jitter  string
		uniform float64
		want    time.Duration
	}{
		{"full jitter at the low end reaches zero", JitterFull, 0, 0},
		{"full jitter at the high end reaches the full delay", JitterFull, 1, 4 * time.Second},
		{"full jitter mid-range", JitterFull, 0.5, 2 * time.Second},
		{"unset jitter defaults to full", "", 0.25, time.Second},
		{"equal jitter never returns less than half", JitterEqual, 0, 2 * time.Second},
		{"equal jitter tops out at the full delay", JitterEqual, 1, 4 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &nexusv1.RetryPolicy{
				Strategy: pol.Strategy, InitialMs: pol.InitialMs,
				MaxMs: pol.MaxMs, Multiplier: pol.Multiplier, Jitter: tc.jitter,
			}
			got := backoffWith(p, 3, func() float64 { return tc.uniform })
			if got != tc.want {
				t.Fatalf("backoff = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBackoffFullJitterStaysInRange(t *testing.T) {
	// The real random source, exercised for range only. A delay outside
	// [0, base] would mean a retry either fires instantly in a loop or waits
	// past the declared ceiling.
	pol := &nexusv1.RetryPolicy{
		Strategy:  nexusv1.BackoffStrategy_BACKOFF_STRATEGY_EXPONENTIAL,
		InitialMs: 100, MaxMs: 10000, Multiplier: 2, Jitter: JitterFull,
	}
	for i := 0; i < 500; i++ {
		d := Backoff(pol, 4) // base = 800ms
		if d < 0 || d > 800*time.Millisecond {
			t.Fatalf("Backoff = %v, outside [0, 800ms]", d)
		}
	}
}

func TestMaxAttempts(t *testing.T) {
	tests := []struct {
		name   string
		policy *nexusv1.RetryPolicy
		want   int32
	}{
		{"explicit", &nexusv1.RetryPolicy{MaxAttempts: 7}, 7},
		// An omitted ceiling must not mean "retry forever".
		{"unset falls back to the platform default", &nexusv1.RetryPolicy{}, DefaultMaxAttempts},
		{"nil policy", nil, DefaultMaxAttempts},
		{"negative is nonsense and falls back", &nexusv1.RetryPolicy{MaxAttempts: -1}, DefaultMaxAttempts},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MaxAttempts(tc.policy); got != tc.want {
				t.Fatalf("MaxAttempts = %d, want %d", got, tc.want)
			}
		})
	}
}
