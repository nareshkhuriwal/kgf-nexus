// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/rest"
)

func clearRateLimitEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"NEXUS_RATE_LIMIT_RPS", "NEXUS_RATE_LIMIT_BURST", "NEXUS_RATE_LIMIT_MAX_KEYS",
		"NEXUS_RATE_LIMIT_IP_RPS", "NEXUS_RATE_LIMIT_IP_BURST",
		"NEXUS_RATE_LIMIT_AUTH_FAIL_RPS", "NEXUS_RATE_LIMIT_AUTH_FAIL_BURST",
	} {
		t.Setenv(k, "")
	}
}

// Unset stays off: rate limiting is opt-in, and a deployment that never set
// NEXUS_RATE_LIMIT_RPS must see no 429s after upgrading — even if it set one of
// the newer knobs on its own.
func TestRateLimitsFromEnvIsOptIn(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("NEXUS_RATE_LIMIT_IP_RPS", "10")
	if _, ok := rateLimitsFromEnv(); ok {
		t.Fatal("rate limiting enabled without NEXUS_RATE_LIMIT_RPS")
	}
}

// The two original knobs keep their meaning: the per-credential allowance.
func TestRateLimitsFromEnvKeepsTheOriginalKnobs(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("NEXUS_RATE_LIMIT_RPS", "20")
	t.Setenv("NEXUS_RATE_LIMIT_BURST", "40")
	got, ok := rateLimitsFromEnv()
	if !ok {
		t.Fatal("not enabled")
	}
	want := rest.GatewayRateLimitsFor(rest.RateLimitConfig{RequestsPerSecond: 20, Burst: 40})
	if got != want {
		t.Fatalf("got %+v, want the derived defaults %+v", got, want)
	}
}

func TestRateLimitsFromEnvOverrides(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("NEXUS_RATE_LIMIT_RPS", "20")
	t.Setenv("NEXUS_RATE_LIMIT_MAX_KEYS", "5000")
	t.Setenv("NEXUS_RATE_LIMIT_IP_RPS", "300")
	t.Setenv("NEXUS_RATE_LIMIT_AUTH_FAIL_BURST", "25")
	got, _ := rateLimitsFromEnv()

	// A rate override without a burst takes burst = rate, the rule the original
	// NEXUS_RATE_LIMIT_BURST follows.
	if got.PerAddress.RequestsPerSecond != 300 || got.PerAddress.Burst != 0 {
		t.Errorf("per-address = %+v, want 300/s with burst defaulting to the rate", got.PerAddress)
	}
	// A burst override alone keeps the default rate.
	if got.AuthFailures.Burst != 25 || got.AuthFailures.RequestsPerSecond != rest.DefaultAuthFailureLimit.RequestsPerSecond {
		t.Errorf("auth failures = %+v, want the default rate with burst 25", got.AuthFailures)
	}
	if got.PerCredential.MaxEntries != 5000 || got.AuthFailures.MaxEntries != 5000 {
		t.Errorf("MaxEntries not applied to every limiter: %+v", got)
	}
}
