// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"sync"
	"testing"
	"time"
)

// fakeClock lets the refill behaviour be tested without sleeping, so the tests
// assert on the arithmetic rather than on the scheduler.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestDisabledByDefault(t *testing.T) {
	// A zero rate must disable the limiter entirely. This is what keeps every
	// existing deployment and test unchanged, so it is the most important case
	// here: if it regressed, every caller would start getting 429s.
	if l := newRateLimiter(RateLimitConfig{}, nil); l != nil {
		t.Fatal("a zero RequestsPerSecond built a limiter; rate limiting must be opt-in")
	}
	// A nil limiter must still be callable, because the guard calls it on every
	// request without checking.
	var nilLimiter *rateLimiter
	if ok, _ := nilLimiter.allow("k"); !ok {
		t.Error("a nil limiter refused a request")
	}
}

func TestBurstIsSpentThenRefunded(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 10, Burst: 5}, clk.Now)

	// The bucket starts FULL, so a cold client's first request is never refused.
	for i := 0; i < 5; i++ {
		if ok, _ := l.allow("k"); !ok {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}
	ok, wait := l.allow("k")
	if ok {
		t.Fatal("the 6th request was allowed; the burst is not bounded")
	}
	if wait <= 0 {
		t.Error("a refusal carried no Retry-After hint, which invites an immediate retry")
	}

	// At 10/s, 500ms refills 5 tokens.
	clk.advance(500 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if ok, _ := l.allow("k"); !ok {
			t.Fatalf("refill request %d was refused", i+1)
		}
	}
}

func TestRefillIsCappedAtBurst(t *testing.T) {
	// An idle key must not accumulate an unbounded allowance: otherwise a client
	// that sleeps an hour can spend 36,000 requests at once, which is precisely
	// the spike the limiter exists to prevent.
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 10, Burst: 5}, clk.Now)

	clk.advance(time.Hour)
	for i := 0; i < 5; i++ {
		if ok, _ := l.allow("k"); !ok {
			t.Fatalf("request %d after a long idle was refused", i+1)
		}
	}
	if ok, _ := l.allow("k"); ok {
		t.Error("an idle key accumulated more than its burst")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	// One noisy credential must not lock out the others, which is the failure that
	// would make the limiter itself the denial of service.
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 1, Burst: 1}, clk.Now)

	if ok, _ := l.allow("noisy"); !ok {
		t.Fatal("first request from the noisy key was refused")
	}
	if ok, _ := l.allow("noisy"); ok {
		t.Fatal("the noisy key exceeded its burst")
	}
	if ok, _ := l.allow("quiet"); !ok {
		t.Error("a different credential was refused because another key was throttled")
	}
}

func TestBurstDefaultsToTheRateButIsOtherwiseHonoured(t *testing.T) {
	// Omitting Burst must allow one second's worth at once, or a config that sets
	// only a rate would admit a single request and then throttle.
	if l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 20}, nil); l.cfg.Burst != 20 {
		t.Errorf("Burst with none set = %v, want 20", l.cfg.Burst)
	}
	// An explicitly SMALLER burst is a legitimate choice and must not be
	// overridden: silently widening it would make the configuration lie about
	// the spike the operator is allowing.
	if l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 20, Burst: 5}, nil); l.cfg.Burst != 5 {
		t.Errorf("explicit Burst 5 became %v; the operator's limit was overridden", l.cfg.Burst)
	}
	// A burst below one token could never admit a request at all.
	if l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 0.5, Burst: 0.2}, nil); l.cfg.Burst != 1 {
		t.Errorf("Burst = %v, want it floored at 1 so some request can pass", l.cfg.Burst)
	}
}

func TestIdleBucketsAreEvicted(t *testing.T) {
	// Without eviction the map grows once per key forever. A gateway sees rotated
	// and probed keys, so that is an unbounded leak in the one process every
	// request passes through.
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := newRateLimiter(RateLimitConfig{
		RequestsPerSecond: 10, Burst: 10, IdleEviction: time.Minute,
	}, clk.Now)

	for i := 0; i < 100; i++ {
		l.allow(string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}
	before := len(l.buckets)
	if before == 0 {
		t.Fatal("no buckets were created")
	}

	clk.advance(2 * time.Minute)
	l.allow("survivor")
	if len(l.buckets) >= before {
		t.Errorf("buckets = %d after idling past eviction, want fewer than %d", len(l.buckets), before)
	}
}

func TestConcurrentAllowIsRaceFree(t *testing.T) {
	// The limiter sits on every request, so it is contended by definition. Run
	// with -race: a data race here corrupts the token count for real traffic.
	l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 1e6, Burst: 1e6}, nil)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				l.allow("shared")
				l.allow(string(rune('a' + i%26)))
			}
		}(i)
	}
	wg.Wait()
}
