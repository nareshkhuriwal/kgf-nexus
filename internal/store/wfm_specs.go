package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// workflow_specs is OWNED by workflow-manager (ADR-0011). The gateway sends a
// validated, canonicalized document here over gRPC rather than inserting it
// itself.

// WFMPutSpec stores a spec version, or returns the existing one when the content
// address already exists.
//
// Immutability and content addressing together give reproducibility: a historical
// run's exact definition is recoverable forever, because a change is always a new
// version and never an UPDATE (ADR-0004).
//
// Version assignment is `max(version)+1` computed INSIDE the insert rather than
// read-then-write, so two concurrent submissions of two different specs cannot
// both claim the same version number. If they race anyway, UNIQUE(namespace,
// name, version) rejects the loser and the caller retries — the database
// arbitrates, not application-level locking.
//
// The `created` return distinguishes a genuine new version from a no-op replay,
// which the REST layer needs in order to answer 201 vs 200.
func WFMPutSpec(ctx context.Context, db GWDB, namespace, name, specHash string, specJSON []byte, createdBy string) (row SpecRow, created bool, err error) {
	// The fast path: an identical document was already submitted. Checking first
	// avoids burning a version number on a resubmit.
	existing, err := GatewayGetSpecByHash(ctx, db, namespace, name, specHash)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return SpecRow{}, false, err
	}

	var s SpecRow
	err = db.QueryRow(ctx, `
		INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		SELECT $1, $2, COALESCE(MAX(version), 0) + 1, $3, $4::jsonb, $5
		  FROM workflow_specs
		 WHERE namespace = $1 AND name = $2
		RETURNING id::text, namespace, name, version, spec_hash, spec_json::text, created_by, created_at`,
		namespace, name, specHash, string(specJSON), createdBy,
	).Scan(&s.ID, &s.Namespace, &s.Name, &s.Version, &s.SpecHash, &s.SpecJSON, &s.CreatedBy, &s.CreatedAt)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// Lost a race. Whichever unique index fired, the correct answer is the
		// row that won, so re-read rather than surfacing a conflict to a caller
		// who did nothing wrong.
		existing, getErr := GatewayGetSpecByHash(ctx, db, namespace, name, specHash)
		if getErr == nil {
			return existing, false, nil
		}
		return SpecRow{}, false, fmt.Errorf("insert spec conflicted and re-read failed: %w", getErr)
	}
	if err != nil {
		return SpecRow{}, false, fmt.Errorf("insert spec: %w", err)
	}
	return s, true, nil
}

// WFMGetSpecByID loads a stored spec by primary key. A run pins its spec version
// for its lifetime, so this is how the run's definition is recovered even after
// the workflow has been edited many times since (ADR-0004).
func WFMGetSpecByID(ctx context.Context, q GWQuerier, specID string) (SpecRow, error) {
	var s SpecRow
	err := q.QueryRow(ctx, `
		SELECT id::text, namespace, name, version, spec_hash, spec_json::text, created_by, created_at
		  FROM workflow_specs
		 WHERE id = $1`,
		specID,
	).Scan(&s.ID, &s.Namespace, &s.Name, &s.Version, &s.SpecHash, &s.SpecJSON, &s.CreatedBy, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SpecRow{}, ErrNotFound
	}
	if err != nil {
		return SpecRow{}, fmt.Errorf("get spec by id: %w", err)
	}
	return s, nil
}

// WFMGetLatestSpec resolves a workflow name to its newest version. Run creation
// by name uses this once and then pins the resulting spec_id, so a spec submitted
// a millisecond later cannot change what an in-flight run executes.
func WFMGetLatestSpec(ctx context.Context, q GWQuerier, namespace, name string) (SpecRow, error) {
	var s SpecRow
	err := q.QueryRow(ctx, `
		SELECT id::text, namespace, name, version, spec_hash, spec_json::text, created_by, created_at
		  FROM workflow_specs
		 WHERE namespace = $1 AND name = $2
		 ORDER BY version DESC
		 LIMIT 1`,
		namespace, name,
	).Scan(&s.ID, &s.Namespace, &s.Name, &s.Version, &s.SpecHash, &s.SpecJSON, &s.CreatedBy, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SpecRow{}, ErrNotFound
	}
	if err != nil {
		return SpecRow{}, fmt.Errorf("get latest spec: %w", err)
	}
	return s, nil
}
