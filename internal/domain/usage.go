// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Usage units (ADR-0032 §2): what a running task consumed outside the
// platform's own resources -- LLM tokens, provider requests, images -- keyed by
// a convention the price book (the cost layer) prices:
//
//	tokens:<provider>/<model>:input
//	tokens:<provider>/<model>:output
//	tokens:<provider>/<model>:cached
//	requests:<provider>/<model>
//	<kind>:<subject>[:<qualifier>]
//
// Every key contains ':' and no custom resource name can (platform
// customNamePattern and the spec schema), so usage keys and reserved custom
// resources share the usage ledger's `custom` object without colliding.

// Usage bounds. Wide enough for any real workload, narrow enough that one
// attempt cannot bloat a ledger row or overflow a sum.
const (
	// MaxUsageKeys is the most distinct keys one attempt may report.
	MaxUsageKeys = 64
	// MaxUsageKeyBytes bounds one key.
	MaxUsageKeyBytes = 200
	// MaxUsageValue bounds one key's cumulative total (10^15: a million
	// billion tokens), so a sum over millions of ledger rows stays exact in
	// int64 range for any realistic horizon, and NUMERIC anyway.
	MaxUsageValue = 1_000_000_000_000_000
)

var (
	usageKeyPattern = regexp.MustCompile(
		`^[a-z][a-z0-9_]{0,31}:[A-Za-z0-9][A-Za-z0-9._/@+-]{0,127}(:[a-z][a-z0-9_]{0,31})?$`)
	// RateLimitKeyPattern is a rate-limit key: a usage-key SUBJECT, i.e.
	// "<provider>" or "<provider>/<model>".
	rateLimitKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@+-]{0,127}$`)
)

// ValidUsageKey reports whether key follows the ADR-0032 grammar.
func ValidUsageKey(key string) bool {
	return len(key) <= MaxUsageKeyBytes && usageKeyPattern.MatchString(key)
}

// ValidRateLimitKey reports whether key is a valid rate-limit key.
func ValidRateLimitKey(key string) bool {
	return rateLimitKeyPattern.MatchString(key)
}

// UsageKind is the key's kind (the part before the first ':'), collapsed to a
// closed set for metric labels: tokens, requests, or other. Keys are
// task-author controlled, so they never become labels themselves.
func UsageKind(key string) string {
	kind, _, _ := strings.Cut(key, ":")
	switch kind {
	case "tokens", "requests":
		return kind
	}
	return "other"
}

// ValidateUsage checks one report: key grammar, value range and key count.
// The whole report is refused on the first problem, so a partially valid
// report is never half-applied.
func ValidateUsage(units map[string]int64) error {
	if len(units) > MaxUsageKeys {
		return fmt.Errorf("usage report has %d keys; at most %d are allowed per attempt", len(units), MaxUsageKeys)
	}
	keys := make([]string, 0, len(units))
	for k := range units {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic error message
	for _, k := range keys {
		if !ValidUsageKey(k) {
			return fmt.Errorf("usage key %q does not follow <kind>:<subject>[:<qualifier>] (ADR-0032)", truncateForError(k))
		}
		if v := units[k]; v < 0 || v > MaxUsageValue {
			return fmt.Errorf("usage %q = %d is outside [0, %d]", k, v, int64(MaxUsageValue))
		}
	}
	return nil
}

// MergeUsage merges a cumulative snapshot into the stored totals with max per
// key. Totals only grow, so the merge is idempotent and order-insensitive: a
// duplicated, retried or reordered report can never lower or double a total.
// The result is bounded by MaxUsageKeys; a snapshot that would exceed it is
// refused whole.
func MergeUsage(stored, snapshot map[string]int64) (map[string]int64, bool, error) {
	out := make(map[string]int64, len(stored)+len(snapshot))
	for k, v := range stored {
		out[k] = v
	}
	changed := false
	for k, v := range snapshot {
		if cur, ok := out[k]; !ok || v > cur {
			out[k] = v
			changed = true
		}
	}
	if len(out) > MaxUsageKeys {
		return stored, false, fmt.Errorf("attempt would report %d distinct usage keys; at most %d are allowed", len(out), MaxUsageKeys)
	}
	return out, changed, nil
}

func truncateForError(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}
