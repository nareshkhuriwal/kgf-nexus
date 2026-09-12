package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func idx(i int) *int { return &i }

// TestIdempotencyKeyIsStableAcrossAttempts is THE test for invariant 1. If it
// ever fails, the platform has lost at-least-once-with-dedup and will
// double-charge customers on retry.
func TestIdempotencyKeyIsStableAcrossAttempts(t *testing.T) {
	const runID = "11111111-1111-1111-1111-111111111111"
	const nodeID = "charge_customer"

	// The signature takes no attempt number at all, which is the structural
	// guarantee. This test pins the observable consequence: the key a retry
	// would compute equals the key the first try computed, for every field
	// combination we support.
	cases := []struct {
		name     string
		mapIndex *int
		version  int32
	}{
		{"plain node", nil, 1},
		{"expanded child 0", idx(0), 1},
		{"expanded child 7", idx(7), 3},
		{"high version", nil, 999},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := IdempotencyKey(runID, nodeID, tc.mapIndex, tc.version)
			// Simulate attempts 2..5 recomputing the key after LOST/FAILED.
			for attempt := 2; attempt <= 5; attempt++ {
				again := IdempotencyKey(runID, nodeID, tc.mapIndex, tc.version)
				if again != first {
					t.Fatalf("attempt %d key %s != attempt 1 key %s", attempt, again, first)
				}
			}
		})
	}
}

func TestIdempotencyKeyDistinguishesLogicalWork(t *testing.T) {
	const runA = "aaaaaaaa-0000-0000-0000-000000000000"
	const runB = "bbbbbbbb-0000-0000-0000-000000000000"

	base := IdempotencyKey(runA, "node", nil, 1)

	tests := []struct {
		name string
		key  string
	}{
		{"different run is different work", IdempotencyKey(runB, "node", nil, 1)},
		{"different node is different work", IdempotencyKey(runA, "other", nil, 1)},
		{"map index present differs from absent", IdempotencyKey(runA, "node", idx(0), 1)},
		{"different map index is different work", IdempotencyKey(runA, "node", idx(1), 1)},
		// A version bump is the author's explicit statement that this is
		// intentionally a NEW logical effect and must not dedup against the old.
		{"version bump is a new logical effect", IdempotencyKey(runA, "node", nil, 2)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.key == base {
				t.Fatalf("key collision with base: %s", tc.key)
			}
		})
	}
}

// TestIdempotencyKeyNoFieldBoundaryForgery guards the collision class that a
// naive concatenation admits: two different (node_id, map_index) pairs hashing
// to the same key would make unrelated tasks dedup against each other.
func TestIdempotencyKeyNoFieldBoundaryForgery(t *testing.T) {
	const runID = "cccccccc-0000-0000-0000-000000000000"
	pairs := [][2]string{
		{"a", "1"},  // node "a", map_index 1
		{"a1", "-"}, // node "a1", no map_index
	}
	seen := map[string][2]string{}
	for _, p := range pairs {
		var k string
		if p[1] == "-" {
			k = IdempotencyKey(runID, p[0], nil, 1)
		} else {
			k = IdempotencyKey(runID, p[0], idx(1), 1)
		}
		if prev, dup := seen[k]; dup {
			t.Fatalf("forged field boundary: %v collides with %v", p, prev)
		}
		seen[k] = p
	}
}

func TestIdempotencyKeyMatchesSpecifiedDerivation(t *testing.T) {
	// Pin the exact preimage so a refactor cannot silently change every
	// existing key in the database, which would break dedup for every
	// in-flight run at deploy time.
	const runID = "dddddddd-0000-0000-0000-000000000000"
	want := sha256.Sum256([]byte(runID + keySep + "node" + keySep + "4" + keySep + "7"))
	got := IdempotencyKey(runID, "node", idx(4), 7)
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("IdempotencyKey = %s, want %s", got, hex.EncodeToString(want[:]))
	}
}

func TestIdempotencyKeyShape(t *testing.T) {
	k := IdempotencyKey("run", "node", nil, 1)
	if len(k) != 64 {
		t.Fatalf("key length = %d, want 64 hex chars of sha256", len(k))
	}
	if strings.ToLower(k) != k {
		t.Fatalf("key %q must be lowercase hex for stable DB comparison", k)
	}
}

func TestCacheKey(t *testing.T) {
	inputs := []byte(`{"a":1,"b":2}`)
	base := CacheKey("ml.train", 3, inputs)

	tests := []struct {
		name  string
		key   string
		equal bool
	}{
		{"identical inputs hit", CacheKey("ml.train", 3, inputs), true},
		{"different task_ref misses", CacheKey("ml.eval", 3, inputs), false},
		{"version bump invalidates the cache namespace", CacheKey("ml.train", 4, inputs), false},
		{"different inputs miss", CacheKey("ml.train", 3, []byte(`{"a":1,"b":3}`)), false},
		// Canonicalization is the caller's contract: byte-different JSON with
		// the same meaning is a MISS, not a hit. Asserted so nobody "fixes"
		// this by parsing JSON inside the key function.
		{"non-canonical bytes miss", CacheKey("ml.train", 3, []byte(`{"b":2,"a":1}`)), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if (tc.key == base) != tc.equal {
				t.Fatalf("equality = %v, want %v", tc.key == base, tc.equal)
			}
		})
	}
}

// TestCacheKeyIsRunIndependent is the distinction from IdempotencyKey: a cache
// key deliberately contains no run_id, which is what lets a re-run of a 50-node
// pipeline skip the 49 nodes whose inputs did not change.
func TestCacheKeyIsRunIndependent(t *testing.T) {
	a := CacheKey("ml.train", 1, []byte(`{"x":1}`))
	b := CacheKey("ml.train", 1, []byte(`{"x":1}`))
	if a != b {
		t.Fatalf("cache keys differ across calls: %s vs %s", a, b)
	}
	if a == IdempotencyKey("run", "ml.train", nil, 1) {
		t.Fatal("cache key must not coincide with an idempotency key")
	}
}
