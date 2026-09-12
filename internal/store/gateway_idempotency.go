package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrIdempotencyConflict means the same key arrived with a DIFFERENT request
// body.
//
// This is the interesting case and the reason the table stores request_hash at
// all. The lazy implementation — return the cached response for any reuse of the
// key — would silently answer a caller's question about request B with the
// result of request A. A client that reuses a key by accident (a retry loop
// whose body changed, a key derived from too little of the input) then gets a
// confident, wrong, 200. Surfacing 409 turns a silent data-integrity bug into a
// loud client bug at the moment it is cheapest to find.
var ErrIdempotencyConflict = errors.New("idempotency key reused with a different request body")

// HashRequest fingerprints a request body for idempotency comparison. The caller
// passes CANONICAL bytes, so two semantically identical bodies that differ only
// in key order or whitespace are correctly treated as the same request.
func HashRequest(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// GatewayClaimIdempotencyKey attempts to reserve (namespace, key) for this
// request, returning the cached response if the key was already used.
//
// Returned values:
//   - (nil, false, nil)       -> claim won; the caller must do the work and then
//     call GatewayStoreIdempotentResponse.
//   - (response, true, nil)   -> same key, same body: replay the cached response.
//   - (nil, false, ErrIdempotencyConflict) -> same key, different body.
//
// The claim is a single INSERT ... ON CONFLICT with a RETURNING-less follow-up
// read rather than a SELECT-then-INSERT, because two concurrent requests
// carrying the same new key would both see "absent" under SELECT-first and both
// create a run. Letting the unique primary key arbitrate means exactly one
// caller wins, which is the whole point of the mechanism.
func GatewayClaimIdempotencyKey(ctx context.Context, q GWQuerier, namespace, key, requestHash string) ([]byte, bool, error) {
	// An empty response marks "claimed but not yet completed". A caller that
	// crashes between claim and store leaves this row behind; a subsequent
	// identical request sees an empty cached response and is told to retry
	// rather than being handed a bogus body.
	tag, err := q.Exec(ctx, `
		INSERT INTO idempotency_keys (namespace, key, request_hash, response)
		VALUES ($1, $2, $3, 'null'::jsonb)
		ON CONFLICT (namespace, key) DO NOTHING`,
		namespace, key, requestHash,
	)
	if err != nil {
		return nil, false, fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil, false, nil
	}

	var storedHash string
	var response []byte
	err = q.QueryRow(ctx, `
		SELECT request_hash, response::text
		  FROM idempotency_keys
		 WHERE namespace = $1 AND key = $2`,
		namespace, key,
	).Scan(&storedHash, &response)
	if errors.Is(err, pgx.ErrNoRows) {
		// Vanishingly rare: the row was deleted between our INSERT losing and
		// this read. Treating it as "claim not won, nothing cached" makes the
		// caller retry, which is safe.
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read idempotency key: %w", err)
	}

	// Compare fingerprints, not bodies: the bodies may be megabytes and we only
	// ever need equality.
	if storedHash != requestHash {
		return nil, false, ErrIdempotencyConflict
	}
	if len(response) == 0 || string(response) == "null" {
		return nil, false, nil
	}
	return response, true, nil
}

// GatewayStoreIdempotentResponse records the response to replay for future
// requests carrying this key.
func GatewayStoreIdempotentResponse(ctx context.Context, q GWQuerier, namespace, key string, response []byte) error {
	_, err := q.Exec(ctx, `
		UPDATE idempotency_keys
		   SET response = $3::jsonb
		 WHERE namespace = $1 AND key = $2`,
		namespace, key, string(response),
	)
	if err != nil {
		return fmt.Errorf("store idempotent response: %w", err)
	}
	return nil
}

// GatewayReleaseIdempotencyKey drops a claim whose work FAILED.
//
// Without this, a transient failure (the database was briefly unavailable)
// would poison the key permanently: the caller's correct, identical retry would
// find a claimed row with no cached response and could never make progress.
// Only failures release; a successful response is cached forever within
// retention.
func GatewayReleaseIdempotencyKey(ctx context.Context, q GWQuerier, namespace, key string) error {
	_, err := q.Exec(ctx, `
		DELETE FROM idempotency_keys
		 WHERE namespace = $1 AND key = $2
		   AND response = 'null'::jsonb`,
		namespace, key,
	)
	if err != nil {
		return fmt.Errorf("release idempotency key: %w", err)
	}
	return nil
}
