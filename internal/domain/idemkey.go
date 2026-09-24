// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// MapIndexAbsent is the placeholder substituted for a nil map_index, i.e. an
// ordinary (non-expanded) node.
//
// A literal placeholder rather than an empty string because the fields are
// concatenated with a separator: without it, a node named "a" with map_index 1
// and a node named "a1" with no index could collide, and an idempotency key
// collision means two unrelated tasks dedup against each other — silent,
// catastrophic, and very hard to diagnose.
const MapIndexAbsent = "-"

// keySep separates fields inside the hashed preimage. \x1f (ASCII unit
// separator) is used because it cannot occur in a UUID, a node_id, or a
// decimal integer, so no input value can forge a field boundary.
const keySep = "\x1f"

// IdempotencyKey derives the platform-computed idempotency key for a unit of
// logical work: sha256(run_id ‖ node_id ‖ map_index|"-" ‖ task_version).
//
// CRITICAL: attempt_number is NOT an input, and must never become one.
//
// The key identifies THE WORK, not THE TRY. Every retry of the same logical
// work — attempt 1, attempt 2 after a crash, attempt 3 after a lease expiry —
// produces the identical key, which is precisely what lets a downstream system
// (Stripe-style dedup, an ON CONFLICT DO NOTHING insert, or the platform's
// effect journal) recognise the second physical execution as the same logical
// effect and drop it.
//
// Include attempt_number and every retry becomes a fresh key, every dedup
// mechanism downstream sees a new effect, and at-least-once execution degrades
// into at-least-once *side effects*: the customer is charged twice. That is the
// exact failure mode this platform exists to prevent, so the exclusion is a
// frozen invariant (CONTRACT.md invariant 1, §6.2) rather than a design choice
// open to revision.
//
// task_version IS included, because bumping the version is the author's
// explicit statement that this is intentionally a NEW logical effect that must
// not dedup against the old one.
func IdempotencyKey(runID, nodeID string, mapIndex *int, taskVersion int32) string {
	idx := MapIndexAbsent
	if mapIndex != nil {
		idx = strconv.Itoa(*mapIndex)
	}
	h := sha256.New()
	h.Write([]byte(runID))
	h.Write([]byte(keySep))
	h.Write([]byte(nodeID))
	h.Write([]byte(keySep))
	h.Write([]byte(idx))
	h.Write([]byte(keySep))
	h.Write([]byte(strconv.FormatInt(int64(taskVersion), 10)))
	return hex.EncodeToString(h.Sum(nil))
}

// CacheKey derives the memoization key: sha256(task_ref ‖ version ‖
// canonical_hash(inputs)) (§6.4).
//
// Note how this differs from IdempotencyKey, because the two are routinely
// confused and the distinction is load-bearing: idempotency makes
// re-execution SAFE and is scoped to one node of one run; memoization makes
// re-execution UNNECESSARY and is scoped to (code, inputs) across ALL runs.
// A cache key deliberately contains no run_id — that is what lets re-running a
// 50-node pipeline after one late failure skip the 49 nodes whose inputs did
// not change.
//
// The caller must supply inputs already serialized in canonical form (sorted
// keys, no insignificant whitespace). Canonicalization is the caller's job
// because the JSON shape lives in the DAG engine, and a key computed over
// non-canonical bytes silently stops hitting when a map iteration order
// changes — a cache that misses is far harder to notice than one that errors.
func CacheKey(taskRef string, version int32, inputsCanonicalJSON []byte) string {
	h := sha256.New()
	h.Write([]byte(taskRef))
	h.Write([]byte(keySep))
	h.Write([]byte(strconv.FormatInt(int64(version), 10)))
	h.Write([]byte(keySep))
	h.Write(inputsCanonicalJSON)
	return hex.EncodeToString(h.Sum(nil))
}

// MemoKey is CacheKey scoped to a namespace: sha256(namespace ‖ task_ref ‖
// version ‖ canonical_hash(inputs)). It is the key the DAG engine actually
// memoizes on.
//
// It reuses the §6.2 derivation's construction -- sha256 over fields joined by
// the unforgeable \x1f separator -- and adds the namespace as the leading field.
// §6.4's formula has no tenant in it, which is right for a single-tenant
// platform and wrong for this one: a cached output_ref points into ITS tenant's
// result-store prefix, which another tenant's workers cannot read, and a
// cross-tenant hit would also disclose that the other tenant ran the same work on
// the same inputs. Scoping the key makes such a hit structurally impossible
// rather than something a lookup predicate must remember to prevent.
//
// Like CacheKey it contains no run_id and no attempt_number: it identifies (code,
// inputs), which is what lets a re-run skip work whose inputs did not change.
func MemoKey(namespace, taskRef string, version int32, inputsDigest []byte) string {
	h := sha256.New()
	h.Write([]byte(namespace))
	h.Write([]byte(keySep))
	h.Write([]byte(CacheKey(taskRef, version, inputsDigest)))
	return hex.EncodeToString(h.Sum(nil))
}
