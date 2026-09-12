// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestVerifyLeaseAcceptsCurrentToken is the baseline: a worker holding the live
// lease may act on its attempt.
func TestVerifyLeaseAcceptsCurrentToken(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	db.putLease("attempt-1", "worker-1", "tok-current", 30*time.Second)

	lv, err := GatewayVerifyLease(ctx, db, "attempt-1", "tok-current")
	if err != nil {
		t.Fatalf("the current lease holder was rejected: %v", err)
	}
	if lv.WorkerID != "worker-1" {
		t.Errorf("worker_id = %q, want worker-1", lv.WorkerID)
	}
}

// TestVerifyLeaseRejectsStaleToken is the FENCING GUARANTEE (CONTRACT.md invariant
// 3, ADR-0010).
//
// The scenario is the one that corrupts state in naive designs: a worker is
// partitioned, its lease expires, the control plane retries the attempt elsewhere
// under a NEW token, and then the original worker reconnects and reports success.
// Accepting that report would mark SUCCEEDED an attempt whose replacement is still
// running, and release capacity twice.
func TestVerifyLeaseRejectsStaleToken(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	db.putLease("attempt-1", "worker-old", "tok-old", 30*time.Second)

	// The lease expires while worker-old is partitioned...
	db.advance(31 * time.Second)
	// ...and the control plane reclaims it and dispatches to a new worker.
	db.rotateLease("attempt-1", "worker-new", "tok-new", 30*time.Second)

	// The zombie reconnects with its old token.
	_, err := GatewayVerifyLease(ctx, db, "attempt-1", "tok-old")
	if !errors.Is(err, ErrStaleLeaseToken) {
		t.Fatalf("a zombie worker's stale token was not rejected: err = %v", err)
	}

	// The legitimate new holder is unaffected.
	if _, err := GatewayVerifyLease(ctx, db, "attempt-1", "tok-new"); err != nil {
		t.Errorf("the current lease holder was rejected after rotation: %v", err)
	}
}

// TestVerifyLeaseRejectsExpiredButMatchingToken: an expired lease is stale even
// when the token matches. The expiry timer may not have reclaimed the row yet, but
// the attempt is already eligible to be declared LOST and retried, and accepting a
// report here would race that retry.
func TestVerifyLeaseRejectsExpiredButMatchingToken(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	db.putLease("attempt-2", "worker-1", "tok-1", 10*time.Second)
	db.advance(11 * time.Second)

	_, err := GatewayVerifyLease(ctx, db, "attempt-2", "tok-1")
	if !errors.Is(err, ErrStaleLeaseToken) {
		t.Fatalf("an expired lease with a matching token must be stale: err = %v", err)
	}
}

// TestVerifyLeaseMissingLease: no lease at all means the caller is not the owner,
// which is the same thing as a stale token from the worker's point of view.
func TestVerifyLeaseMissingLease(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	_, err := GatewayVerifyLease(ctx, db, "attempt-nonexistent", "tok")
	if !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("err = %v, want ErrLeaseNotFound", err)
	}
}

// TestReasonStaleLeaseTokenMatchesContract pins the exact wire string. Worker
// agents match on it to decide to abandon an attempt, so a typo would be a silent
// protocol break that no type checker catches.
func TestReasonStaleLeaseTokenMatchesContract(t *testing.T) {
	if ReasonStaleLeaseToken != "stale_lease_token" {
		t.Errorf("ReasonStaleLeaseToken = %q, but CONTRACT.md invariant 3 fixes it at \"stale_lease_token\"", ReasonStaleLeaseToken)
	}
}

// TestRenewLeasesBatched exercises the batched renewal: one call renews everything
// this worker holds, and anything that does not renew comes back as revoked.
func TestRenewLeasesBatched(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	db.putLease("a1", "worker-1", "tok-a1", 30*time.Second)
	db.putLease("a2", "worker-1", "tok-a2", 30*time.Second)
	// a3 belongs to a DIFFERENT worker; worker-1 must not be able to renew it.
	db.putLease("a3", "worker-2", "tok-a3", 30*time.Second)

	granted, revoked, err := GatewayRenewLeases(ctx, db, "worker-1", map[string]string{
		"a1": "tok-a1",
		"a2": "tok-a2",
		"a3": "tok-a3",
		// An attempt with no lease at all.
		"a4": "tok-a4",
	}, 30*time.Second)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}

	grantedSet := map[string]bool{}
	for _, g := range granted {
		grantedSet[g.AttemptID] = true
		if g.ExpiresAt.IsZero() {
			t.Errorf("grant for %s carries no absolute deadline; the worker has nothing to renew against", g.AttemptID)
		}
	}
	if !grantedSet["a1"] || !grantedSet["a2"] {
		t.Errorf("worker-1's own live leases were not renewed: granted = %v", grantedSet)
	}
	if grantedSet["a3"] {
		t.Error("worker-1 renewed a lease belonging to worker-2; renewal must be guarded by worker id")
	}

	revokedSet := map[string]bool{}
	for _, id := range revoked {
		revokedSet[id] = true
	}
	// a3 (wrong worker) and a4 (no lease) must both come back revoked: the worker
	// MUST stop work on them, because a replacement attempt may already be running.
	for _, want := range []string{"a3", "a4"} {
		if !revokedSet[want] {
			t.Errorf("%s was not revoked; the worker would keep burning CPU on work it cannot report", want)
		}
	}
}

// TestRenewLeasesRejectsWrongToken: presenting the wrong token for an attempt you
// do own is still a revocation, not a renewal.
func TestRenewLeasesRejectsWrongToken(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	db.putLease("a1", "worker-1", "tok-real", 30*time.Second)

	granted, revoked, err := GatewayRenewLeases(ctx, db, "worker-1",
		map[string]string{"a1": "tok-guessed"}, 30*time.Second)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if len(granted) != 0 {
		t.Errorf("a wrong token was granted a renewal: %+v", granted)
	}
	if len(revoked) != 1 || revoked[0] != "a1" {
		t.Errorf("revoked = %v, want [a1]", revoked)
	}
}

// TestRenewLeasesDoesNotRenewExpired: an expired lease cannot be resurrected by
// renewal. It must be reclaimed by the expiry path and re-dispatched, because the
// control plane may already have declared the attempt LOST.
func TestRenewLeasesDoesNotRenewExpired(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	db.putLease("a1", "worker-1", "tok-a1", 10*time.Second)
	db.advance(11 * time.Second)

	granted, revoked, err := GatewayRenewLeases(ctx, db, "worker-1",
		map[string]string{"a1": "tok-a1"}, 30*time.Second)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if len(granted) != 0 {
		t.Error("an expired lease was renewed; it must be reclaimed and re-dispatched instead")
	}
	if len(revoked) != 1 {
		t.Errorf("revoked = %v, want the expired attempt", revoked)
	}
}

func TestHashAPIKeyIsPlainSHA256(t *testing.T) {
	// Pinned because rotating the hash function would invalidate every stored key
	// at once, which is a migration rather than a code change.
	if got, want := HashAPIKey("nx_secret"), sha256Hex("nx_secret"); got != want {
		t.Errorf("HashAPIKey = %s, want %s", got, want)
	}
	if HashAPIKey("a") == HashAPIKey("b") {
		t.Error("distinct keys must hash differently")
	}
}
