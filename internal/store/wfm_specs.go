// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

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

// ── deletion ──────────────────────────────────────────────────────────────

// ErrSpecReferenced means a spec version could not be deleted because something
// still points at it. Its fields name WHAT, so the caller can tell the operator.
//
// A typed error rather than a bare sentinel because "you cannot delete this" is
// useless on its own: the operator's next action depends entirely on whether the
// blocker is 3 runs of history (they must decide whether to accept losing it) or
// a live schedule (they must repoint or delete the schedule first). An error that
// forces them to go and look is an error that gets ignored.
type ErrSpecReferenced struct {
	Namespace string
	Name      string
	// Runs is how many workflow_runs rows pin a version of this spec. These are
	// the immutable execution history: audit rows, event streams and attempt
	// records all hang off them.
	Runs int
	// Schedules names the schedules whose spec_name is this workflow. Named
	// rather than counted because a schedule is a thing an operator must go and
	// edit, and "2 schedules" does not tell them which.
	Schedules []string
}

func (e *ErrSpecReferenced) Error() string {
	return fmt.Sprintf("workflow %s/%s is still referenced by %d run(s) and %d schedule(s)",
		e.Namespace, e.Name, e.Runs, len(e.Schedules))
}

// WFMCountSpecReferences reports what still points at a workflow NAME.
//
// Both halves are counted in one round trip and both are counted even when the
// first is already non-zero, because the refusal must name EVERY blocker. An
// operator told only about the runs would delete the runs, retry, and be refused
// again for the schedule -- a two-step discovery where one step was available.
//
// Runs are joined through workflow_specs rather than filtered by spec name on
// workflow_runs, because workflow_runs has no name column: a run pins a spec_id
// (ADR-0004) and the name is a property of the spec version it pinned.
func WFMCountSpecReferences(ctx context.Context, q GWQuerier, namespace, name string) (runs int, schedules []string, err error) {
	err = q.QueryRow(ctx, `
		SELECT COUNT(*)
		  FROM workflow_runs r
		  JOIN workflow_specs s ON s.id = r.spec_id
		 WHERE s.namespace = $1 AND s.name = $2`,
		namespace, name,
	).Scan(&runs)
	if err != nil {
		return 0, nil, fmt.Errorf("count runs referencing spec %s/%s: %w", namespace, name, err)
	}

	// Schedules point at a spec NAME, not an id (migration 0006), and there is NO
	// foreign key on that column -- so the database will not stop a delete that
	// breaks a schedule. This query is the only thing that will, which is exactly
	// why it must run before the delete rather than being left to the FK.
	rows, qerr := q.Query(ctx, `
		SELECT name FROM schedules
		 WHERE namespace = $1 AND spec_name = $2
		 ORDER BY name`,
		namespace, name)
	if qerr != nil {
		return 0, nil, fmt.Errorf("list schedules referencing spec %s/%s: %w", namespace, name, qerr)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if serr := rows.Scan(&n); serr != nil {
			return 0, nil, fmt.Errorf("scan schedule name: %w", serr)
		}
		schedules = append(schedules, n)
	}
	if rerr := rows.Err(); rerr != nil {
		return 0, nil, fmt.Errorf("iterate schedules referencing spec: %w", rerr)
	}
	return runs, schedules, nil
}

// WFMDeleteSpec removes EVERY version of a workflow name, or refuses.
//
// SEMANTICS: refuse with a named blocker, never cascade and never soft-delete.
//
// Why not cascade. workflow_runs.spec_id is a hard FK to workflow_specs(id) with
// no ON DELETE clause, so a cascade would have to be written deliberately -- and
// it would destroy immutable execution history that the audit trail and the event
// stream reference. ADR-0004's whole promise is that a historical run's exact
// definition is recoverable forever; a cascade breaks that promise silently, for
// every run, to satisfy one click.
//
// Why not soft-delete. A deleted_at column would have to be honoured by every
// existing read (the catalog, version listing, GetSpec's latest-version
// resolution, and the scheduler's fire-time name resolution). Any read that
// forgot it would resurrect the workflow, and the one that matters most -- the
// scheduler resolving spec_name at fire time -- lives in another service. A soft
// delete whose tombstone one reader ignores is worse than no delete at all,
// because the operator believes the workflow is gone.
//
// So: refuse, and NAME the blocker. The operator can then act -- repoint or
// delete the schedule, or accept that a workflow with history is permanent. The
// check-then-delete is not atomic against a concurrent run creation, which is
// why the FK is still the backstop: a run inserted between the count and the
// DELETE makes the DELETE fail with 23503 rather than orphaning it, and that is
// mapped below to the same refusal the count would have produced.
//
// The whole name is deleted rather than one version, because versions are not
// independently meaningful: deleting v3 of a five-version workflow leaves a
// catalog entry whose "latest" silently rolls back to v2, which is a worse
// surprise than refusing.
func WFMDeleteSpec(ctx context.Context, db GWDB, namespace, name string) error {
	runs, schedules, err := WFMCountSpecReferences(ctx, db, namespace, name)
	if err != nil {
		return err
	}
	if runs > 0 || len(schedules) > 0 {
		return &ErrSpecReferenced{Namespace: namespace, Name: name, Runs: runs, Schedules: schedules}
	}

	tag, err := db.Exec(ctx, `DELETE FROM workflow_specs WHERE namespace = $1 AND name = $2`, namespace, name)
	if err != nil {
		// 23503 is foreign_key_violation: a run was created for this workflow
		// between the count above and this statement. The database refused it, so
		// nothing was orphaned -- report it as the same referenced-refusal the
		// count would have produced rather than as an internal error, because the
		// operator's situation and next action are identical.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return &ErrSpecReferenced{Namespace: namespace, Name: name, Runs: 1}
		}
		return fmt.Errorf("delete spec %s/%s: %w", namespace, name, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
