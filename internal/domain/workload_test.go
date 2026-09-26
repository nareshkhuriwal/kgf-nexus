// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"math"
	"strings"
	"testing"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func TestUsageKeyGrammar(t *testing.T) {
	good := []string{
		"tokens:openai/gpt-4o-mini:input",
		"tokens:anthropic/claude-3-5-sonnet-20241022:output",
		"requests:openai/gpt-4o-mini",
		"tokens:azure-openai/gpt-4o@2024-08-06:cached",
		"images:bedrock/stability.sd3",
		"units:embeddings",
	}
	for _, k := range good {
		if !ValidUsageKey(k) {
			t.Errorf("%q should be a valid usage key", k)
		}
	}
	bad := []string{
		"", "tokens", "custom_resource", "Tokens:openai/m:input", "tokens:",
		"tokens:openai/m:Input", "tokens:openai/ft:m:x:input", // ':' in the subject
		"tokens: openai/m", "tokens:-openai", strings.Repeat("a", 33) + ":x",
	}
	for _, k := range bad {
		if ValidUsageKey(k) {
			t.Errorf("%q should be refused", k)
		}
	}
	// Disjoint from custom resource names by construction: every usage key
	// has ':' and no resource name can.
	for _, k := range good {
		if !strings.Contains(k, ":") {
			t.Errorf("%q has no ':'", k)
		}
	}
}

func TestValidateAndMergeUsage(t *testing.T) {
	if err := ValidateUsage(map[string]int64{"tokens:a/b:input": MaxUsageValue + 1}); err == nil {
		t.Error("a value past the bound was accepted")
	}
	if err := ValidateUsage(map[string]int64{"tokens:a/b:input": -1}); err == nil {
		t.Error("a negative value was accepted")
	}
	stored := map[string]int64{"requests:a/b": 3, "tokens:a/b:input": 100}
	merged, changed, err := MergeUsage(stored, map[string]int64{"requests:a/b": 2, "tokens:a/b:input": 150, "tokens:a/b:output": 9})
	if err != nil || !changed {
		t.Fatalf("merge: %v changed=%v", err, changed)
	}
	if merged["requests:a/b"] != 3 || merged["tokens:a/b:input"] != 150 || merged["tokens:a/b:output"] != 9 {
		t.Fatalf("merge is not a per-key max: %v", merged)
	}
	// Idempotent: the same snapshot again changes nothing.
	if _, changed, _ := MergeUsage(merged, map[string]int64{"tokens:a/b:input": 150}); changed {
		t.Error("re-applying a snapshot reported a change")
	}
	big := map[string]int64{}
	for i := 0; i < MaxUsageKeys; i++ {
		big["units:k"+strings.Repeat("x", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26))] = 1
	}
	if _, _, err := MergeUsage(big, map[string]int64{"units:one-more": 1}); err == nil {
		t.Error("the key bound was not enforced across reports")
	}
	if UsageKind("tokens:a/b:input") != "tokens" || UsageKind("requests:a/b") != "requests" || UsageKind("images:x") != "other" {
		t.Error("UsageKind is not the closed label set")
	}
}

func TestTokenBucket(t *testing.T) {
	l := BucketLimit{RequestsPerSecond: 2, TokensPerMinute: 600} // bursts 2 requests, 600 tokens
	s := l.FullBucket()
	if s.Requests != 2 || s.Tokens != 600 {
		t.Fatalf("full bucket = %+v", s)
	}
	s, ok, _ := l.Take(s, 1, 500)
	if !ok {
		t.Fatal("first take refused")
	}
	_, ok, wait := l.Take(s, 1, 200)
	if ok || math.Abs(wait-10) > 1e-9 { // 100 tokens short at 10 tokens/s
		t.Fatalf("second take: ok=%v wait=%v, want refused with 10s", ok, wait)
	}
	// Refill is capped at the burst.
	if r := l.Refill(s, 3600); r.Requests != 2 || r.Tokens != 600 {
		t.Fatalf("refill past the burst: %+v", r)
	}
	// A negative elapsed (clock step) refills nothing.
	if r := l.Refill(s, -5); r != s {
		t.Fatalf("negative elapsed changed the bucket: %+v", r)
	}
	// An oversized request is granted only from a full bucket, and goes into
	// debt bounded at -burst.
	full := l.FullBucket()
	n, ok, _ := l.Take(full, 1, 5000)
	if !ok || n.Tokens != -600 {
		t.Fatalf("oversized from full: ok=%v tokens=%v", ok, n.Tokens)
	}
	if _, ok, _ := l.Take(BucketState{Requests: 2, Tokens: 599}, 1, 5000); ok {
		t.Fatal("an oversized request was granted from a bucket that was not full")
	}
	// Settlement: refunds and debt, clamped.
	if st := l.Settle(BucketState{Tokens: 100}, -1000); st.Tokens != 600 {
		t.Fatalf("refund not clamped to the burst: %v", st.Tokens)
	}
	if st := l.Settle(BucketState{Tokens: 100}, 250); st.Tokens != -150 {
		t.Fatalf("debt: %v", st.Tokens)
	}
	// Unlimited dimension: tokens ignored when no tokens_per_minute.
	rl := BucketLimit{RequestsPerSecond: 0.5}
	if rl.RequestBurst() != 1 {
		t.Fatalf("burst for 0.5 rps = %v, want 1", rl.RequestBurst())
	}
	if _, ok, _ := rl.Take(rl.FullBucket(), 1, 1e9); !ok {
		t.Fatal("tokens were limited without a tokens_per_minute")
	}
}

func TestRateLimitedDecision(t *testing.T) {
	idem := nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED
	d, free := RateLimitedDecision(idem, 5, 1, 0, 3, false, nil)
	if !d.Retry || !free || d.Reason != ReasonRetryRateLimited {
		t.Fatalf("within the cap: %+v free=%v", d, free)
	}
	// Past the cap: charged like TRANSIENT against the ordinary budget.
	if d, free := RateLimitedDecision(idem, 0, 3, 3, 3, false, nil); !d.Retry || free {
		t.Fatalf("past the cap with budget: %+v free=%v", d, free)
	}
	if d, _ := RateLimitedDecision(idem, 3, 3, 3, 3, false, nil); d.Retry {
		t.Fatalf("past the cap without budget: %+v", d)
	}
	veto := &nexusv1.RetryPolicy{NoRetryOn: []nexusv1.ErrorClass{nexusv1.ErrorClass_ERROR_CLASS_RATE_LIMITED}}
	if d, _ := RateLimitedDecision(idem, 0, 3, 0, 3, false, veto); d.Retry || d.Reason != ReasonNoRetryDenied {
		t.Fatalf("no_retry_on veto: %+v", d)
	}
	// An explicit retry_on that omits rate_limited still gets the free retries.
	only := &nexusv1.RetryPolicy{RetryOn: []nexusv1.ErrorClass{nexusv1.ErrorClass_ERROR_CLASS_WORKER_LOST}}
	if d, free := RateLimitedDecision(idem, 0, 3, 0, 3, false, only); !d.Retry || !free {
		t.Fatalf("retry_on without rate_limited: %+v", d)
	}
	if d, _ := RateLimitedDecision(idem, 0, 3, 0, 3, true, nil); d.Retry {
		t.Fatal("schedule_to_close exceeded but retried")
	}
	if got := RateLimitedDelay(7000, nil, 1); got != 7*time.Second {
		t.Errorf("delay = %v, want the Retry-After", got)
	}
	if got := RateLimitedDelay(10, nil, 1); got != MinRateLimitedDelay {
		t.Errorf("delay = %v, want the floor", got)
	}
	if got := RateLimitedDelay(int64(24*time.Hour/time.Millisecond), nil, 1); got != MaxRateLimitedDelay {
		t.Errorf("delay = %v, want the ceiling", got)
	}
}
