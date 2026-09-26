// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Quotas are a namespace's limits (ADR-0021), stored in its platform policy at
// config -> quotas and read by the scheduler through the namespace_quota_limits
// view (migration 0027).
//
// Every limit uses 0 for UNLIMITED, so the zero value is "no quotas" and a
// namespace that never set any behaves exactly as before quotas existed.
type Quotas struct {
	// Locked is set by a platform operator. While it is set, a namespace
	// administrator may only tighten limits and may not unlock.
	Locked bool `json:"locked"`

	// Concurrent limits, enforced atomically in the dispatch claim over the
	// namespace's DISPATCHED and RUNNING attempts on every queue.
	MaxInflightAttempts int64            `json:"max_inflight_attempts"`
	MaxCPUMillis        int64            `json:"max_cpu_millis"`
	MaxMemoryBytes      int64            `json:"max_memory_bytes"`
	MaxGPUCount         int64            `json:"max_gpu_count"`
	MaxCustom           map[string]int64 `json:"max_custom,omitempty"`

	// Run-level limits, enforced when a run is created.
	MaxActiveRuns  int64 `json:"max_active_runs"`
	MaxRunsPerHour int64 `json:"max_runs_per_hour"`

	// StorageSoftLimitBytes is reported against measured storage, never
	// enforced: refusing a write after the fact would fail work that already
	// ran, which is the wrong response to a budget signal.
	StorageSoftLimitBytes int64 `json:"storage_soft_limit_bytes"`

	// RateLimits are shared token-bucket limits on calls to external
	// providers, keyed by "<provider>" or "<provider>/<model>" (ADR-0032 §3).
	// Enforced by the permit service (AcquirePermits), never in the claim.
	// Absent means no key is limited.
	RateLimits map[string]RateLimit `json:"rate_limits,omitempty"`
}

// RateLimit is one key's shared limit. At least one rate must be set; a zero
// rate leaves that dimension unlimited. Bursts default to one second of
// requests (at least 1) and one minute of tokens.
type RateLimit struct {
	RequestsPerSecond float64 `json:"requests_per_second,omitempty"`
	TokensPerMinute   int64   `json:"tokens_per_minute,omitempty"`
	BurstRequests     int64   `json:"burst_requests,omitempty"`
	BurstTokens       int64   `json:"burst_tokens,omitempty"`
}

// Rate-limit bounds. As for quotas, they reject nonsense, not ambition.
const (
	MaxRateLimitKeys          = 64
	MaxRequestsPerSecond      = 1_000_000
	MaxTokensPerMinute        = 1_000_000_000_000
	MaxRateLimitBurstRequests = 100_000_000
)

var rateLimitKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@+-]{0,127}$`)

// Validate rejects a limit the permit service could not evaluate safely.
func (r RateLimit) Validate(key string) error {
	switch {
	case !rateLimitKeyPattern.MatchString(key):
		return fmt.Errorf("rate-limit key %q must be <provider> or <provider>/<model>", key)
	case r.RequestsPerSecond < 0 || r.RequestsPerSecond > MaxRequestsPerSecond ||
		r.RequestsPerSecond != r.RequestsPerSecond: // NaN
		return fmt.Errorf("rate_limits.%s.requests_per_second must be between 0 and %d", key, MaxRequestsPerSecond)
	case r.TokensPerMinute < 0 || r.TokensPerMinute > MaxTokensPerMinute:
		return fmt.Errorf("rate_limits.%s.tokens_per_minute must be between 0 and %d", key, int64(MaxTokensPerMinute))
	case r.RequestsPerSecond == 0 && r.TokensPerMinute == 0:
		return fmt.Errorf("rate_limits.%s sets no rate; remove the key to lift the limit", key)
	case r.BurstRequests < 0 || r.BurstRequests > MaxRateLimitBurstRequests:
		return fmt.Errorf("rate_limits.%s.burst_requests must be between 0 (default) and %d", key, MaxRateLimitBurstRequests)
	case r.BurstTokens < 0 || r.BurstTokens > MaxTokensPerMinute:
		return fmt.Errorf("rate_limits.%s.burst_tokens must be between 0 (default) and %d", key, int64(MaxTokensPerMinute))
	case r.BurstRequests > 0 && r.RequestsPerSecond == 0:
		return fmt.Errorf("rate_limits.%s.burst_requests needs requests_per_second", key)
	case r.BurstTokens > 0 && r.TokensPerMinute == 0:
		return fmt.Errorf("rate_limits.%s.burst_tokens needs tokens_per_minute", key)
	}
	return nil
}

// looserThan lists the fields of next that relax r (a raised rate or burst, or
// a limited dimension made unlimited).
func (r RateLimit) looserThan(next RateLimit, key string) []string {
	var out []string
	rate := func(name string, cur, nxt float64) {
		if cur > 0 && (nxt == 0 || nxt > cur) {
			out = append(out, "rate_limits."+key+"."+name)
		}
	}
	rate("requests_per_second", r.RequestsPerSecond, next.RequestsPerSecond)
	rate("tokens_per_minute", float64(r.TokensPerMinute), float64(next.TokensPerMinute))
	// A burst is looser when it grows; 0 is the default, compared as such.
	burst := func(name string, cur, nxt, curDefault, nxtDefault float64) {
		if cur == 0 {
			cur = curDefault
		}
		if nxt == 0 {
			nxt = nxtDefault
		}
		if nxt > cur {
			out = append(out, "rate_limits."+key+"."+name)
		}
	}
	burst("burst_requests", float64(r.BurstRequests), float64(next.BurstRequests),
		defaultBurstRequests(r.RequestsPerSecond), defaultBurstRequests(next.RequestsPerSecond))
	burst("burst_tokens", float64(r.BurstTokens), float64(next.BurstTokens),
		float64(r.TokensPerMinute), float64(next.TokensPerMinute))
	return out
}

func defaultBurstRequests(rps float64) float64 {
	if rps <= 0 {
		return 0
	}
	if rps < 1 {
		return 1
	}
	if c := float64(int64(rps)); c < rps {
		return c + 1
	}
	return rps
}

// Quota bounds. Wide on purpose: they exist to reject nonsense (negative
// numbers, values that overflow when summed), not to second-guess an operator.
const (
	MaxQuotaCount      = 1_000_000
	MaxQuotaCPUMillis  = 1 << 40 // ~1.1e12 millicores
	MaxQuotaBytes      = 1 << 60 // 1 EiB
	MaxQuotaCustom     = 1 << 50
	MaxQuotaCustomKeys = 32
)

var customNamePattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._/-]{0,61}[a-zA-Z0-9])?$`)

// Validate rejects limits the claim could not evaluate safely.
func (q Quotas) Validate() error {
	check := func(name string, v, max int64) error {
		if v < 0 || v > max {
			return fmt.Errorf("%s must be between 0 (unlimited) and %d", name, max)
		}
		return nil
	}
	for _, c := range []struct {
		name   string
		v, max int64
	}{
		{"max_inflight_attempts", q.MaxInflightAttempts, MaxQuotaCount},
		{"max_cpu_millis", q.MaxCPUMillis, MaxQuotaCPUMillis},
		{"max_memory_bytes", q.MaxMemoryBytes, MaxQuotaBytes},
		{"max_gpu_count", q.MaxGPUCount, MaxQuotaCount},
		{"max_active_runs", q.MaxActiveRuns, MaxQuotaCount},
		{"max_runs_per_hour", q.MaxRunsPerHour, MaxQuotaCount},
		{"storage_soft_limit_bytes", q.StorageSoftLimitBytes, MaxQuotaBytes},
	} {
		if err := check(c.name, c.v, c.max); err != nil {
			return err
		}
	}
	if len(q.MaxCustom) > MaxQuotaCustomKeys {
		return fmt.Errorf("at most %d custom resource quotas", MaxQuotaCustomKeys)
	}
	for k, v := range q.MaxCustom {
		if !customNamePattern.MatchString(k) {
			return fmt.Errorf("custom resource name %q is not valid", k)
		}
		if v < 1 || v > MaxQuotaCustom {
			// 0 in a map is ambiguous (unlimited? zero allowed?), so it is
			// refused: remove the key to lift a custom limit.
			return fmt.Errorf("custom quota %q must be between 1 and %d; remove it to lift the limit", k, int64(MaxQuotaCustom))
		}
	}
	if len(q.RateLimits) > MaxRateLimitKeys {
		return fmt.Errorf("at most %d rate-limit keys", MaxRateLimitKeys)
	}
	keys := make([]string, 0, len(q.RateLimits))
	for k := range q.RateLimits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := q.RateLimits[k].Validate(k); err != nil {
			return err
		}
	}
	return nil
}

// HasConcurrent reports whether any limit is enforced in the claim.
func (q Quotas) HasConcurrent() bool {
	return q.MaxInflightAttempts > 0 || q.MaxCPUMillis > 0 || q.MaxMemoryBytes > 0 ||
		q.MaxGPUCount > 0 || len(q.MaxCustom) > 0
}

// Equal compares two quota documents value by value.
func (q Quotas) Equal(o Quotas) bool {
	if q.Locked != o.Locked || q.MaxInflightAttempts != o.MaxInflightAttempts ||
		q.MaxCPUMillis != o.MaxCPUMillis || q.MaxMemoryBytes != o.MaxMemoryBytes ||
		q.MaxGPUCount != o.MaxGPUCount || q.MaxActiveRuns != o.MaxActiveRuns ||
		q.MaxRunsPerHour != o.MaxRunsPerHour || q.StorageSoftLimitBytes != o.StorageSoftLimitBytes ||
		len(q.MaxCustom) != len(o.MaxCustom) {
		return false
	}
	for k, v := range q.MaxCustom {
		if w, ok := o.MaxCustom[k]; !ok || w != v {
			return false
		}
	}
	if len(q.RateLimits) != len(o.RateLimits) {
		return false
	}
	for k, v := range q.RateLimits {
		if w, ok := o.RateLimits[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// tighter reports whether limit `next` is at least as strict as `cur`, with 0
// meaning unlimited: any limit is tighter than none, and none is tighter only
// than none.
func tighter(cur, next int64) bool {
	switch {
	case cur == 0:
		return true
	case next == 0:
		return false
	default:
		return next <= cur
	}
}

// Loosened lists the limits `next` relaxes relative to q (raised, removed, or
// the lock lifted). Empty means next only tightens or keeps every limit, which
// is what a namespace administrator may do while the quotas are locked.
func (q Quotas) Loosened(next Quotas) []string {
	var out []string
	if q.Locked && !next.Locked {
		out = append(out, "locked")
	}
	for _, c := range []struct {
		name      string
		cur, next int64
	}{
		{"max_inflight_attempts", q.MaxInflightAttempts, next.MaxInflightAttempts},
		{"max_cpu_millis", q.MaxCPUMillis, next.MaxCPUMillis},
		{"max_memory_bytes", q.MaxMemoryBytes, next.MaxMemoryBytes},
		{"max_gpu_count", q.MaxGPUCount, next.MaxGPUCount},
		{"max_active_runs", q.MaxActiveRuns, next.MaxActiveRuns},
		{"max_runs_per_hour", q.MaxRunsPerHour, next.MaxRunsPerHour},
		{"storage_soft_limit_bytes", q.StorageSoftLimitBytes, next.StorageSoftLimitBytes},
	} {
		if !tighter(c.cur, c.next) {
			out = append(out, c.name)
		}
	}
	for k, v := range q.MaxCustom {
		if !tighter(v, next.MaxCustom[k]) {
			out = append(out, "max_custom."+k)
		}
	}
	// A rate limit removed is a rate limit lifted; one added only tightens.
	for k, v := range q.RateLimits {
		n, ok := next.RateLimits[k]
		if !ok {
			out = append(out, "rate_limits."+k)
			continue
		}
		out = append(out, v.looserThan(n, k)...)
	}
	sort.Strings(out)
	return out
}

// QuotaUsage is a namespace's current consumption of its concurrent limits.
type QuotaUsage struct {
	InflightAttempts int64
	CPUMillis        int64
	MemoryBytes      int64
	GPUCount         int64
	Custom           map[string]int64
}

// QuotaRequest is what one attempt would add if dispatched.
type QuotaRequest struct {
	CPUMillis   int64
	MemoryBytes int64
	GPUCount    int64
	Custom      map[string]int64
}

// Quota dimensions as they appear in pending reasons, metrics and the API.
const (
	DimInflightAttempts = "inflight_attempts"
	DimCPUMillis        = "cpu_millis"
	DimMemoryBytes      = "memory_bytes"
	DimGPUCount         = "gpu_count"
	DimCustomPrefix     = "custom:"
	DimActiveRuns       = "active_runs"
	DimRunsPerHour      = "runs_per_hour"
)

// QuotaViolation names the first limit a dispatch would exceed.
type QuotaViolation struct {
	Dimension string
	Used      int64
	Limit     int64
	Request   int64
	// Exhausted means NO attempt of the namespace can pass right now: the
	// in-flight count is at its limit, and every attempt counts against it.
	// The claim then sets the namespace aside instead of trying its other
	// candidates. A full resource dimension does not qualify, because an
	// attempt that does not request that resource still fits.
	Exhausted bool
}

// Reason is the attempt's pending_reason, in the documented form
// "quota_exceeded: <dimension> (used X of Y)". When the request alone exceeds
// the limit it says so, because that attempt will never run until the quota is
// raised and an operator must not wait for it to drain.
func (v QuotaViolation) Reason() string {
	s := fmt.Sprintf("quota_exceeded: %s (used %d of %d)", v.Dimension, v.Used, v.Limit)
	if v.Request > v.Limit {
		s = strings.TrimSuffix(s, ")") + fmt.Sprintf(", request %d exceeds the quota)", v.Request)
	}
	return s
}

// CheckConcurrent decides whether one more attempt with request fits under the
// limits given current usage. Dimensions are checked in a fixed order so the
// reason an operator sees is stable from cycle to cycle.
func (q Quotas) CheckConcurrent(used QuotaUsage, req QuotaRequest) (QuotaViolation, bool) {
	type dim struct {
		name            string
		limit, use, add int64
	}
	dims := []dim{
		{DimInflightAttempts, q.MaxInflightAttempts, used.InflightAttempts, 1},
		{DimCPUMillis, q.MaxCPUMillis, used.CPUMillis, req.CPUMillis},
		{DimMemoryBytes, q.MaxMemoryBytes, used.MemoryBytes, req.MemoryBytes},
		{DimGPUCount, q.MaxGPUCount, used.GPUCount, req.GPUCount},
	}
	names := make([]string, 0, len(q.MaxCustom))
	for k := range q.MaxCustom {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		dims = append(dims, dim{DimCustomPrefix + k, q.MaxCustom[k], used.Custom[k], req.Custom[k]})
	}
	for _, d := range dims {
		if d.limit <= 0 || (d.add <= 0 && d.name != DimInflightAttempts) {
			// Unlimited, or this attempt does not use the dimension: an
			// attempt that asks for no GPU is never refused by the GPU quota.
			continue
		}
		if d.use+d.add > d.limit {
			return QuotaViolation{
				Dimension: d.name, Used: d.use, Limit: d.limit, Request: d.add,
				Exhausted: d.name == DimInflightAttempts,
			}, false
		}
	}
	return QuotaViolation{}, true
}
