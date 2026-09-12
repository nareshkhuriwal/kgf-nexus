// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
)

// TestIdempotencyReplaySameBody is the core guarantee: the same key with the same
// body returns the CACHED response rather than doing the work twice.
func TestIdempotencyReplaySameBody(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	const ns, key = "team-ml", "client-retry-001"
	body := []byte(`{"spec_name":"train","params":{"epochs":10}}`)
	hash := HashRequest(body)

	cached, replay, err := GatewayClaimIdempotencyKey(ctx, db, ns, key, hash)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if replay {
		t.Fatal("the first use of a key must not be a replay")
	}
	if cached != nil {
		t.Fatalf("the first claim returned a cached response %q; there is nothing to replay yet", cached)
	}

	response := []byte(`{"id":"run-1","state":"PENDING"}`)
	if err := GatewayStoreIdempotentResponse(ctx, db, ns, key, response); err != nil {
		t.Fatalf("store response: %v", err)
	}

	cached, replay, err = GatewayClaimIdempotencyKey(ctx, db, ns, key, hash)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if !replay {
		t.Fatal("the second use of the same key with the same body must be a replay, not a new creation")
	}
	if string(cached) != string(response) {
		t.Errorf("replayed %q, want the original response %q byte-for-byte", cached, response)
	}
}

// TestIdempotencyConflictDifferentBody is the case the lazy implementation gets
// wrong. Returning the cached response for any reuse of the key would answer a
// question about request B with the result of request A — silently, with a
// confident 200. 409 turns that into a loud client bug.
func TestIdempotencyConflictDifferentBody(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	const ns, key = "team-ml", "client-retry-002"

	firstHash := HashRequest([]byte(`{"spec_name":"train","params":{"epochs":10}}`))
	if _, _, err := GatewayClaimIdempotencyKey(ctx, db, ns, key, firstHash); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := GatewayStoreIdempotentResponse(ctx, db, ns, key, []byte(`{"id":"run-1"}`)); err != nil {
		t.Fatalf("store response: %v", err)
	}

	// Same key, DIFFERENT body: the client's epochs changed under a retry loop.
	secondHash := HashRequest([]byte(`{"spec_name":"train","params":{"epochs":500}}`))
	cached, replay, err := GatewayClaimIdempotencyKey(ctx, db, ns, key, secondHash)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("error = %v, want ErrIdempotencyConflict", err)
	}
	if replay {
		t.Error("a conflicting body must never be treated as a replay")
	}
	if cached != nil {
		t.Errorf("a conflict returned a cached response %q; returning request A's answer for request B is the exact bug this check prevents", cached)
	}
}

// TestIdempotencyCanonicalBodiesAreTheSameRequest: a client that re-serializes its
// request with different key order is NOT making a different request, and 409-ing
// it would be a false positive that breaks well-behaved clients.
func TestIdempotencyCanonicalBodiesAreTheSameRequest(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	const ns, key = "team-ml", "client-retry-003"

	// Both hashes are computed over CANONICAL bytes, which is the contract of
	// HashRequest and why the REST layer canonicalizes before fingerprinting.
	canonical := []byte(`{"params":{"epochs":10},"spec_name":"train"}`)
	hash := HashRequest(canonical)

	if _, _, err := GatewayClaimIdempotencyKey(ctx, db, ns, key, hash); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := GatewayStoreIdempotentResponse(ctx, db, ns, key, []byte(`{"id":"run-1"}`)); err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, replay, err := GatewayClaimIdempotencyKey(ctx, db, ns, key, HashRequest(canonical)); err != nil || !replay {
		t.Errorf("identical canonical bodies must replay; replay=%v err=%v", replay, err)
	}
}

// TestIdempotencyClaimedButIncomplete covers the crash window: a caller that
// claimed a key and died before storing a response must not hand the next request
// a bogus body.
func TestIdempotencyClaimedButIncomplete(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	const ns, key = "team-ml", "client-retry-004"
	hash := HashRequest([]byte(`{"spec_name":"train"}`))

	if _, _, err := GatewayClaimIdempotencyKey(ctx, db, ns, key, hash); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// The work never completed, so no response was stored.
	cached, replay, err := GatewayClaimIdempotencyKey(ctx, db, ns, key, hash)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if replay {
		t.Error("a claim with no stored response must not be reported as a replay")
	}
	if cached != nil {
		t.Errorf("cached = %q, want nil for an incomplete claim", cached)
	}
}

// TestIdempotencyReleaseUnpoisonsKey: without release, a transient failure would
// poison the key permanently and the client's correct retry could never succeed.
func TestIdempotencyReleaseUnpoisonsKey(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	const ns, key = "team-ml", "client-retry-005"
	hash := HashRequest([]byte(`{"spec_name":"train"}`))

	if _, _, err := GatewayClaimIdempotencyKey(ctx, db, ns, key, hash); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// The creation failed; release the claim.
	if err := GatewayReleaseIdempotencyKey(ctx, db, ns, key); err != nil {
		t.Fatalf("release: %v", err)
	}
	// The retry must win the claim afresh.
	_, replay, err := GatewayClaimIdempotencyKey(ctx, db, ns, key, hash)
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	if replay {
		t.Error("after a release the key must be claimable again, not a replay")
	}
}

// TestIdempotencyReleaseDoesNotDropCompletedKey: release must only clear an
// incomplete claim. Dropping a completed one would let the same key create a second
// run, which is the opposite of the guarantee.
func TestIdempotencyReleaseDoesNotDropCompletedKey(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	const ns, key = "team-ml", "client-retry-006"
	hash := HashRequest([]byte(`{"spec_name":"train"}`))

	if _, _, err := GatewayClaimIdempotencyKey(ctx, db, ns, key, hash); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := GatewayStoreIdempotentResponse(ctx, db, ns, key, []byte(`{"id":"run-1"}`)); err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := GatewayReleaseIdempotencyKey(ctx, db, ns, key); err != nil {
		t.Fatalf("release: %v", err)
	}
	cached, replay, err := GatewayClaimIdempotencyKey(ctx, db, ns, key, hash)
	if err != nil {
		t.Fatalf("claim after release of a completed key: %v", err)
	}
	if !replay || string(cached) != `{"id":"run-1"}` {
		t.Errorf("a completed key must survive release; replay=%v cached=%q", replay, cached)
	}
}

// TestIdempotencyIsNamespaceScoped: the primary key is (namespace, key), so two
// tenants choosing the same key string must not collide with each other.
func TestIdempotencyIsNamespaceScoped(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	const key = "daily-0900"
	hash := HashRequest([]byte(`{"spec_name":"train"}`))

	if _, _, err := GatewayClaimIdempotencyKey(ctx, db, "team-a", key, hash); err != nil {
		t.Fatalf("claim for team-a: %v", err)
	}
	_, replay, err := GatewayClaimIdempotencyKey(ctx, db, "team-b", key, hash)
	if err != nil {
		t.Fatalf("claim for team-b: %v", err)
	}
	if replay {
		t.Error("the same key in a different namespace must be an independent claim")
	}
}

func TestHashRequestIsStable(t *testing.T) {
	body := []byte(`{"a":1,"b":[2,3]}`)
	if HashRequest(body) != HashRequest(body) {
		t.Error("HashRequest is not deterministic")
	}
	if HashRequest(body) == HashRequest([]byte(`{"a":1,"b":[2,4]}`)) {
		t.Error("different bodies must not share a fingerprint")
	}
	if got, want := HashRequest(body), sha256Hex(string(body)); got != want {
		t.Errorf("HashRequest = %s, want plain sha256 %s", got, want)
	}
}
