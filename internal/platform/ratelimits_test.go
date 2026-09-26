// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestRateLimitValidation(t *testing.T) {
	ok := Quotas{RateLimits: map[string]RateLimit{
		"openai":             {RequestsPerSecond: 10},
		"openai/gpt-4o-mini": {RequestsPerSecond: 0.5, TokensPerMinute: 200000, BurstTokens: 50000},
	}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid limits refused: %v", err)
	}
	cases := map[string]RateLimit{
		"no rate":              {},
		"negative rps":         {RequestsPerSecond: -1},
		"NaN rps":              {RequestsPerSecond: math.NaN()},
		"huge rps":             {RequestsPerSecond: MaxRequestsPerSecond + 1},
		"negative tpm":         {RequestsPerSecond: 1, TokensPerMinute: -1},
		"burst without rate":   {TokensPerMinute: 100, BurstRequests: 5},
		"token burst w/o rate": {RequestsPerSecond: 1, BurstTokens: 5},
	}
	for name, rl := range cases {
		q := Quotas{RateLimits: map[string]RateLimit{"k": rl}}
		if err := q.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, key := range []string{"", "has space", "ft:model", "-lead"} {
		q := Quotas{RateLimits: map[string]RateLimit{key: {RequestsPerSecond: 1}}}
		if err := q.Validate(); err == nil {
			t.Errorf("key %q accepted", key)
		}
	}
	many := Quotas{RateLimits: map[string]RateLimit{}}
	for i := 0; i <= MaxRateLimitKeys; i++ {
		many.RateLimits[strings.Repeat("k", i%10+1)+string(rune('a'+i%26))+string(rune('a'+i/26))] = RateLimit{RequestsPerSecond: 1}
	}
	if err := many.Validate(); err == nil {
		t.Error("too many rate-limit keys accepted")
	}
}

func TestRateLimitsLockedLoosening(t *testing.T) {
	cur := Quotas{Locked: true, RateLimits: map[string]RateLimit{
		"openai": {RequestsPerSecond: 10, TokensPerMinute: 100000},
	}}
	tighten := Quotas{Locked: true, RateLimits: map[string]RateLimit{
		"openai":    {RequestsPerSecond: 5, TokensPerMinute: 50000},
		"anthropic": {RequestsPerSecond: 1}, // adding a limit only tightens
	}}
	if l := cur.Loosened(tighten); len(l) != 0 {
		t.Fatalf("tightening reported as loosening: %v", l)
	}
	for name, next := range map[string]Quotas{
		"removed":        {Locked: true},
		"raised rps":     {Locked: true, RateLimits: map[string]RateLimit{"openai": {RequestsPerSecond: 11, TokensPerMinute: 100000}}},
		"tpm unlimited":  {Locked: true, RateLimits: map[string]RateLimit{"openai": {RequestsPerSecond: 10}}},
		"burst above tp": {Locked: true, RateLimits: map[string]RateLimit{"openai": {RequestsPerSecond: 10, TokensPerMinute: 100000, BurstTokens: 200000}}},
	} {
		if l := cur.Loosened(next); len(l) == 0 {
			t.Errorf("%s: not reported as loosening", name)
		}
	}
	if cur.Equal(tighten) || !cur.Equal(cur) {
		t.Error("Equal ignores rate limits")
	}
}

func TestRateLimitsJSONIsAdditive(t *testing.T) {
	// A quotas document written before ADR-0032 decodes with no limits, and a
	// document without limits encodes without the key.
	var q Quotas
	if err := json.Unmarshal([]byte(`{"max_inflight_attempts": 5}`), &q); err != nil || q.RateLimits != nil {
		t.Fatalf("old document: %+v %v", q, err)
	}
	b, _ := json.Marshal(Quotas{MaxInflightAttempts: 5})
	if strings.Contains(string(b), "rate_limits") {
		t.Fatalf("empty rate_limits encoded: %s", b)
	}
}
