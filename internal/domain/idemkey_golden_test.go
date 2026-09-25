// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import "testing"

// TestIdempotencyKeyGoldenValues pins the derivation to literal digests that the
// Python SDK asserts too (sdk/python/tests/test_local_runner.py,
// test_idempotency_key_matches_the_control_plane). The SDK derives the key to
// verify the one it is sent and the local runner (ADR-0022) derives it outright,
// so the two implementations must agree byte for byte. They did not: the SDK
// joined the fields with \x00 (CONTRACT-FINDINGS F116).
func TestIdempotencyKeyGoldenValues(t *testing.T) {
	cases := []struct {
		run, node string
		mapIndex  *int
		version   int32
		want      string
	}{
		{"run-1", "node-1", nil, 1, "f32346b2310e2593016da7f96b7d3e3a29db83715b033b8966446983933c3755"},
		{"run-1", "tmpl", idx(0), 2, "62abba5d696c14b1da10f3cd8ecbffca7740b9e073b28a1e7fea6dd956bc2933"},
	}
	for _, c := range cases {
		if got := IdempotencyKey(c.run, c.node, c.mapIndex, c.version); got != c.want {
			t.Errorf("IdempotencyKey(%q, %q, %v, %d) = %s, want %s", c.run, c.node, c.mapIndex, c.version, got, c.want)
		}
	}
}
