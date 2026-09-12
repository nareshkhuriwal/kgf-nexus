// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrAPIKeyUnknown means no live key matched. Callers must map every distinct
// authn failure onto ONE opaque 401: telling a caller whether the key existed,
// was revoked, or was for the wrong namespace turns the endpoint into an oracle
// for probing valid keys.
var ErrAPIKeyUnknown = errors.New("api key not recognized")

// APIKey is a resolved principal. The key material itself is never a field here
// — it is hashed on the way in and discarded, so nothing downstream can log it
// by accident.
type APIKey struct {
	ID        string
	Namespace string
	Name      string
	Role      string
}

// HashAPIKey derives the at-rest representation of a presented key.
//
// Plain SHA-256 rather than a password KDF (bcrypt/argon2) is the right choice
// for THIS credential and would be wrong for a password: these keys are
// generated with full machine entropy, so there is no dictionary to attack, and
// a per-request KDF on a path hit by every worker RPC would be a self-inflicted
// denial of service. A KDF protects low-entropy secrets; it buys nothing here.
func HashAPIKey(presented string) string {
	sum := sha256.Sum256([]byte(presented))
	return hex.EncodeToString(sum[:])
}

// GatewayLookupAPIKey resolves a presented key to its principal.
//
// The lookup is BY HASH, not by scanning and comparing: the unique index on
// key_hash turns authn into a single index probe whose timing does not vary with
// how many keys exist or how close the presented key came to a real one. That
// makes the database query itself constant-time in the way that matters, which
// is a stronger guarantee than comparing candidates in Go.
func GatewayLookupAPIKey(ctx context.Context, q GWQuerier, presented string) (APIKey, error) {
	var k APIKey
	err := q.QueryRow(ctx, `
		SELECT id::text, namespace, name, role
		  FROM api_keys
		 WHERE key_hash = $1
		   AND revoked_at IS NULL`,
		HashAPIKey(presented),
	).Scan(&k.ID, &k.Namespace, &k.Name, &k.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, ErrAPIKeyUnknown
	}
	if err != nil {
		return APIKey{}, fmt.Errorf("lookup api key: %w", err)
	}
	return k, nil
}

// GatewayCreateAPIKey stores a new key by hash and returns its id. The caller
// holds the only copy of the plaintext and must surface it exactly once; we
// cannot re-derive it, which is the property that makes a leak of this table
// non-catastrophic.
func GatewayCreateAPIKey(ctx context.Context, q GWQuerier, namespace, name, role, plaintext string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO api_keys (namespace, name, key_hash, role)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text`,
		namespace, name, HashAPIKey(plaintext), role,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create api key: %w", err)
	}
	return id, nil
}
