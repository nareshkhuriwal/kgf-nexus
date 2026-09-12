// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Rate limiting on the gateway, which is the platform's only ingress.
//
// ARCHITECTURE.md §3.2 lists "rate limit" among the gateway's responsibilities and
// nothing implemented it, so the sole entry point had no protection against a
// request flood or credential stuffing — and the CLI already carries a hint for a
// 429 the server could never send (cli/internal/client/client.go). A gateway with
// no limit also has no defence against a well-meaning client in a tight retry
// loop, which is the common way a control plane gets taken down by its own users.
//
// KEYED PER API KEY, not per IP. Every principal is already identified by the key
// it presents, and behind a load balancer or inside a container network the source
// address is frequently the proxy's, so an IP-keyed limiter would either limit
// every caller as one or limit nothing. A key is also the unit an operator can act
// on: they can revoke it.
//
// WHY A TOKEN BUCKET rather than a fixed window: a fixed window lets a caller
// spend its whole allowance in the last millisecond of one window and again in the
// first of the next, so the instantaneous rate is twice the configured one exactly
// when the system is already under pressure. A bucket bounds the burst explicitly
// and refills smoothly, so "100 per second with a burst of 200" means that.
//
// Reads and writes share one bucket per key deliberately. Splitting them would let
// a caller exhaust the database with reads while its write budget sat unused, and
// the resource being protected — one Postgres — does not care which verb arrived.

// RateLimitConfig configures the limiter. A zero RequestsPerSecond disables it,
// which is what keeps the dev path and the existing tests unchanged.
type RateLimitConfig struct {
	// RequestsPerSecond is the sustained rate allowed per API key.
	RequestsPerSecond float64
	// Burst is the most a key may spend at once. Zero means "same as
	// RequestsPerSecond", so a config that sets only a rate still allows one
	// second's worth at once rather than a single request. An explicit smaller
	// burst is honoured: throttling bursts harder than the sustained rate is a
	// legitimate choice, and silently overriding it would make the configuration
	// lie.
	Burst float64
	// IdleEviction is how long an unused bucket is kept. Without eviction the map
	// grows once per key forever, which for a gateway that sees rotated or probed
	// keys is an unbounded leak in the one process every request passes through.
	IdleEviction time.Duration
}

// DefaultRateLimit is deliberately generous. The limiter exists to stop a runaway
// client or a brute-force attempt, not to meter legitimate use: the console polls,
// the CLI batches, and a limit tight enough to shape traffic would break them while
// barely inconveniencing an attacker who can simply use more keys.
var DefaultRateLimit = RateLimitConfig{
	RequestsPerSecond: 50,
	Burst:             100,
	IdleEviction:      10 * time.Minute,
}

type bucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter is a per-key token bucket.
type rateLimiter struct {
	cfg   RateLimitConfig
	clock func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	// lastSweep bounds eviction work: sweeping on every request would make the
	// limiter's cost proportional to the number of live keys per request.
	lastSweep time.Time
}

func newRateLimiter(cfg RateLimitConfig, clock func() time.Time) *rateLimiter {
	if cfg.RequestsPerSecond <= 0 {
		return nil
	}
	if cfg.Burst <= 0 {
		cfg.Burst = cfg.RequestsPerSecond
	}
	if cfg.Burst < 1 {
		// A burst below one token can never admit a request, which would refuse
		// all traffic rather than limiting it.
		cfg.Burst = 1
	}
	if cfg.IdleEviction <= 0 {
		cfg.IdleEviction = DefaultRateLimit.IdleEviction
	}
	if clock == nil {
		clock = time.Now
	}
	return &rateLimiter{cfg: cfg, clock: clock, buckets: map[string]*bucket{}, lastSweep: clock()}
}

// allow spends one token for key, reporting whether the request may proceed and,
// when it may not, how long the caller should wait before retrying.
//
// The retry hint matters: a 429 with no Retry-After invites an immediate retry,
// which is how a limiter turns one overloaded client into a busy loop.
func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.clock()

	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastSweep) >= l.cfg.IdleEviction {
		for k, b := range l.buckets {
			if now.Sub(b.last) >= l.cfg.IdleEviction {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}

	b, ok := l.buckets[key]
	if !ok {
		// A new key starts full, so a first request is never rejected. Starting
		// empty would make every cold client's first call a 429.
		b = &bucket{tokens: l.cfg.Burst, last: now}
		l.buckets[key] = b
	}

	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * l.cfg.RequestsPerSecond
		if b.tokens > l.cfg.Burst {
			b.tokens = l.cfg.Burst
		}
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// Seconds until one token exists. Rounded up, because a Retry-After that
	// expires a moment early just produces another 429.
	wait := time.Duration((1 - b.tokens) / l.cfg.RequestsPerSecond * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	return false, wait
}

// rateLimited reports whether the request should be refused, writing the 429 if so.
//
// Keyed on the presented credential rather than the resolved principal so the
// check can run BEFORE authentication: an unauthenticated flood is the case that
// most needs limiting, and authenticating first would spend a database round trip
// per rejected request — making the limiter itself the amplifier.
func (a *API) rateLimited(w http.ResponseWriter, r *http.Request) bool {
	if a.limiter == nil {
		return false
	}
	key := bearerOrPeer(r)
	ok, wait := a.limiter.allow(key)
	if ok {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
	writeError(w, http.StatusTooManyRequests, "rate_limited",
		"too many requests for this credential; retry after "+wait.String())
	return true
}

// bearerOrPeer picks the limiter key: the credential when one is presented, and
// otherwise the peer address.
//
// Falling back to the address matters for the unauthenticated case — without it
// every credential-less request would share one bucket, so a single prober could
// lock out all of them, which turns the limiter into the denial of service.
func bearerOrPeer(r *http.Request) string {
	if cred := r.Header.Get("Authorization"); cred != "" {
		return "cred:" + cred
	}
	return "peer:" + r.RemoteAddr
}
