// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import "testing"

// TestInlineThresholdBounds pins ADR-0016's range on both sides of the wire:
// the gateway refuses to start outside [1, 1 MiB], and the agent clamps anything
// it receives into that range rather than trusting it.
func TestInlineThresholdBounds(t *testing.T) {
	if DefaultInlineThresholdBytes != 64*1024 {
		t.Fatalf("default = %d; changing it is an ADR-0016 decision, not a refactor", DefaultInlineThresholdBytes)
	}
	for _, tc := range []struct {
		n       int64
		valid   bool
		clamped int64
	}{
		{0, false, DefaultInlineThresholdBytes}, // proto3 "unset"
		{-1, false, DefaultInlineThresholdBytes},
		{1, true, 1},
		{64 * 1024, true, 64 * 1024},
		{MaxInlineThresholdBytes, true, MaxInlineThresholdBytes},
		{MaxInlineThresholdBytes + 1, false, MaxInlineThresholdBytes},
		{50 << 20, false, MaxInlineThresholdBytes},
	} {
		if err := ValidateInlineThreshold(tc.n); (err == nil) != tc.valid {
			t.Errorf("Validate(%d): err=%v, want valid=%v", tc.n, err, tc.valid)
		}
		if got := ClampInlineThreshold(tc.n); got != tc.clamped {
			t.Errorf("Clamp(%d) = %d, want %d", tc.n, got, tc.clamped)
		}
	}
}
