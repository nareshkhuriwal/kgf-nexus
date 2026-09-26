// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import "math"

// The token-bucket arithmetic behind shared rate limits (ADR-0032 §3). Pure, so
// the edge cases (oversized requests, debt, refunds, the wait estimate) are
// unit-tested without a database; the store only supplies the stored levels,
// the elapsed time from the DATABASE clock, and persists the result under the
// bucket row's lock.

// BucketLimit is one key's configured limit. A dimension whose rate is 0 is
// unlimited.
type BucketLimit struct {
	RequestsPerSecond float64
	TokensPerMinute   int64
	// Bursts; 0 means the default (one second of requests, at least 1; one
	// minute of tokens).
	BurstRequests int64
	BurstTokens   int64
}

// BucketState is a bucket's two levels. Levels may be negative (debt), bounded
// at -burst.
type BucketState struct {
	Requests float64
	Tokens   float64
}

// RequestBurst is the request bucket's capacity.
func (l BucketLimit) RequestBurst() float64 {
	if l.RequestsPerSecond <= 0 {
		return 0
	}
	if l.BurstRequests > 0 {
		return float64(l.BurstRequests)
	}
	return math.Max(1, math.Ceil(l.RequestsPerSecond))
}

// TokenBurst is the token bucket's capacity.
func (l BucketLimit) TokenBurst() float64 {
	if l.TokensPerMinute <= 0 {
		return 0
	}
	if l.BurstTokens > 0 {
		return float64(l.BurstTokens)
	}
	return float64(l.TokensPerMinute)
}

// Limited reports whether any dimension is limited.
func (l BucketLimit) Limited() bool { return l.RequestsPerSecond > 0 || l.TokensPerMinute > 0 }

// FullBucket is a new bucket's state: both levels at their bursts.
func (l BucketLimit) FullBucket() BucketState {
	return BucketState{Requests: l.RequestBurst(), Tokens: l.TokenBurst()}
}

// Refill advances s by elapsed seconds, capped at each burst. A negative
// elapsed (clock step on the database host) refills nothing rather than
// draining the bucket.
func (l BucketLimit) Refill(s BucketState, elapsedSeconds float64) BucketState {
	if elapsedSeconds < 0 || math.IsNaN(elapsedSeconds) {
		elapsedSeconds = 0
	}
	if l.RequestsPerSecond > 0 {
		s.Requests = math.Min(l.RequestBurst(), s.Requests+l.RequestsPerSecond*elapsedSeconds)
	}
	if l.TokensPerMinute > 0 {
		s.Tokens = math.Min(l.TokenBurst(), s.Tokens+float64(l.TokensPerMinute)/60*elapsedSeconds)
	}
	return s
}

// Take tries to take requests and tokens from an already-refilled bucket.
//
// A dimension is satisfied when its level covers the cost, or -- for a cost
// larger than the whole burst, which could otherwise never be granted -- when
// the bucket is full; the level then goes negative and the debt is paid by the
// callers after it. When not granted, wait is how long until every
// unsatisfied dimension would be, at its rate.
func (l BucketLimit) Take(s BucketState, requests, tokens int64) (next BucketState, granted bool, wait float64) {
	type dim struct {
		level *float64
		cost  float64
		burst float64
		rate  float64 // per second
	}
	next = s
	dims := []dim{}
	if l.RequestsPerSecond > 0 && requests > 0 {
		dims = append(dims, dim{&next.Requests, float64(requests), l.RequestBurst(), l.RequestsPerSecond})
	}
	if l.TokensPerMinute > 0 && tokens > 0 {
		dims = append(dims, dim{&next.Tokens, float64(tokens), l.TokenBurst(), float64(l.TokensPerMinute) / 60})
	}
	for _, d := range dims {
		need := math.Min(d.cost, d.burst)
		if *d.level < need {
			if w := (need - *d.level) / d.rate; w > wait {
				wait = w
			}
		}
	}
	if wait > 0 {
		return s, false, wait
	}
	for _, d := range dims {
		*d.level = math.Max(-d.burst, *d.level-d.cost)
	}
	return next, true, 0
}

// Settle applies a signed token correction (actual - estimated) without a
// check: positive is further debt, negative a refund. Clamped to
// [-burst, burst] so a buggy caller cannot bank or owe without bound.
func (l BucketLimit) Settle(s BucketState, tokens int64) BucketState {
	if l.TokensPerMinute <= 0 || tokens == 0 {
		return s
	}
	b := l.TokenBurst()
	s.Tokens = math.Max(-b, math.Min(b, s.Tokens-float64(tokens)))
	return s
}
