// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"math"
	"math/rand/v2"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Jitter modes as spelled in RetryPolicy.jitter (a string in the frozen proto).
const (
	JitterNone  = "none"
	JitterFull  = "full"
	JitterEqual = "equal"
)

// Defaults applied when a RetryPolicy leaves a field at zero. A zero-valued
// policy is common — the DAG engine may hand us whatever the author omitted —
// and a zero backoff would turn a retryable infrastructure fault into a tight
// hot loop hammering a system that is already unhealthy.
const (
	DefaultInitialBackoff = 10 * time.Second
	DefaultMaxBackoff     = 10 * time.Minute
	DefaultMultiplier     = 2.0
	DefaultMaxAttempts    = 3
)

// Backoff returns the delay before the given attempt number is retried, with
// jitter applied.
//
// attemptNumber is the number of the attempt that JUST finished (1-based), so
// the first retry — after attempt 1 — waits roughly `initial`.
//
// Full jitter is the default and the reason this function exists rather than a
// bare multiply. When a shared dependency fails, every in-flight attempt in the
// fleet fails within milliseconds of the others; deterministic backoff then
// retries them all at the same instant, and the recovering dependency is
// knocked over again by a synchronized thundering herd. Spreading the retries
// uniformly over [0, delay] decorrelates them, which is what actually lets the
// dependency recover. The cost is that an individual retry may fire very early;
// that is a good trade, because the fleet-wide behaviour is what breaks
// systems, not one eager retry.
func Backoff(p *nexusv1.RetryPolicy, attemptNumber int32) time.Duration {
	return backoffWith(p, attemptNumber, rand.Float64)
}

// backoffWith is Backoff with an injectable uniform source, so tests can assert
// the bounds and the deterministic (jitter=none) ladder exactly instead of
// sampling and hoping.
func backoffWith(p *nexusv1.RetryPolicy, attemptNumber int32, uniform func() float64) time.Duration {
	initial := DefaultInitialBackoff
	if p.GetInitialMs() > 0 {
		initial = time.Duration(p.GetInitialMs()) * time.Millisecond
	}
	maxDelay := DefaultMaxBackoff
	if p.GetMaxMs() > 0 {
		maxDelay = time.Duration(p.GetMaxMs()) * time.Millisecond
	}
	// A policy whose max sits below its initial is a config error, not an
	// instruction to wait longer than the declared ceiling: honour the ceiling.
	if maxDelay < initial {
		initial = maxDelay
	}

	// Attempt 1 is the first try, so its retry uses one step of growth.
	steps := int(attemptNumber) - 1
	if steps < 0 {
		steps = 0
	}

	mult := p.GetMultiplier()
	if mult <= 1 {
		mult = DefaultMultiplier
	}

	var base float64
	switch p.GetStrategy() {
	case nexusv1.BackoffStrategy_BACKOFF_STRATEGY_FIXED:
		base = float64(initial)
	case nexusv1.BackoffStrategy_BACKOFF_STRATEGY_LINEAR:
		base = float64(initial) * (1 + float64(steps))
	default:
		// UNSPECIFIED falls through to exponential. An author who set a retry
		// policy but no strategy wants retries to back off; exponential is the
		// only choice that protects a struggling dependency.
		base = float64(initial) * math.Pow(mult, float64(steps))
	}

	// math.Pow overflows to +Inf for large attempt counts; clamping before the
	// int64 conversion avoids a negative duration from the wraparound.
	if math.IsInf(base, 0) || base > float64(maxDelay) {
		base = float64(maxDelay)
	}
	delay := time.Duration(base)

	switch p.GetJitter() {
	case JitterNone:
		return delay
	case JitterEqual:
		// Half fixed, half random: decorrelates retries while still
		// guaranteeing a minimum pause, for dependencies that need breathing
		// room more than they need spread.
		half := delay / 2
		return half + time.Duration(uniform()*float64(half))
	default:
		// Empty string (field unset) means full jitter, matching the proto's
		// documented default.
		return time.Duration(uniform() * float64(delay))
	}
}

// MaxAttempts resolves the effective attempt ceiling, substituting the platform
// default for an unset policy so an omitted field cannot mean "retry forever".
func MaxAttempts(p *nexusv1.RetryPolicy) int32 {
	if p.GetMaxAttempts() > 0 {
		return p.GetMaxAttempts()
	}
	return DefaultMaxAttempts
}
