package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Reservation is one attempt's claim on a worker's capacity.
type Reservation struct {
	ID        string
	WorkerID  string
	AttemptID string
	Resources *nexusv1.ResourceVector
	CreatedAt time.Time
}

// ErrInsufficientCapacity is returned when a reservation does not fit. It
// carries the shortfall so the caller can render the operator-visible
// pending_reason ("waiting_for_resources: gpu (need 4, free 3)") rather than an
// unexplained stall.
type ErrInsufficientCapacity struct {
	WorkerID  string
	Shortfall []domain.Shortfall
}

func (e *ErrInsufficientCapacity) Error() string {
	return fmt.Sprintf("store: worker %s cannot fit the request: %s",
		e.WorkerID, domain.ShortfallReason(e.Shortfall))
}

// ErrReservationNotFound means there is nothing to release. Callers treat it as
// success: release must be idempotent, because it is driven by completion
// reports that are themselves at-least-once.
var ErrReservationNotFound = errors.New("store: reservation not found")

// Reserve commits capacity for an attempt in ONE transaction.
//
// The `SELECT ... FOR UPDATE` on the worker row is the whole correctness story.
// Two concurrent reservations against the same worker must not both read the
// same committed vector, both find room, and both commit — that is how a ledger
// over-commits a node into an OOM. The row lock serialises them, so the second
// sees the first's write and is correctly refused.
//
// Note the ownership context (ADR-0011): the SCHEDULER writes reservations and
// workers.committed inside its own dispatch transaction, because atomicity with
// the lease and the attempt CAS outweighs ownership purity. This method serves
// every OTHER path — an out-of-band reservation, tests, and reconciliation
// repair.
func (s *RMStore) Reserve(ctx context.Context, workerID, attemptID string, req *nexusv1.ResourceVector) (*Reservation, error) {
	var res *Reservation
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		capacity, committed, state, err := lockWorker(ctx, tx, workerID)
		if err != nil {
			return err
		}
		// Only an ACTIVE worker takes new work. A DRAINING worker is finishing
		// what it has; a DEAD one is not running anything at all.
		if state != WorkerActive {
			return fmt.Errorf("store: worker %s is %s and cannot accept new reservations", workerID, state)
		}

		// An existing reservation for this attempt means a duplicate dispatch, or
		// a retried RPC. Return it unchanged: committing a second time would
		// double-count and permanently shrink the worker.
		if existing, err := getReservationTx(ctx, tx, attemptID); err == nil {
			res = existing
			return nil
		} else if !errors.Is(err, ErrReservationNotFound) {
			return err
		}

		if ok, short := domain.Fits(capacity, committed, req); !ok {
			return &ErrInsufficientCapacity{WorkerID: workerID, Shortfall: short}
		}

		resJSON, err := marshalVec(req)
		if err != nil {
			return err
		}
		var id string
		var createdAt time.Time
		if err := tx.QueryRow(ctx, `
			INSERT INTO reservations (worker_id, attempt_id, resources)
			VALUES ($1, $2, $3)
			RETURNING id, created_at`, workerID, attemptID, resJSON).Scan(&id, &createdAt); err != nil {
			return fmt.Errorf("store: insert reservation for attempt %s: %w", attemptID, err)
		}

		// The committed vector and the reservation row are written in the SAME
		// transaction, so the ledger cannot drift from the reservations that
		// justify it. A crash between them would leave capacity committed with
		// nothing to release it.
		if err := writeCommitted(ctx, tx, workerID, domain.AddVec(committed, req)); err != nil {
			return err
		}
		res = &Reservation{ID: id, WorkerID: workerID, AttemptID: attemptID,
			Resources: req, CreatedAt: createdAt}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Release frees an attempt's reservation and returns the capacity to the pool.
//
// IDEMPOTENT by construction: it deletes the row and derives the decrement from
// what it deleted, so a second call finds nothing and subtracts nothing. That
// matters because release is driven by completion reports, lease expiry and
// reconciliation — three independent at-least-once paths that will sometimes all
// fire for the same attempt. A non-idempotent release would let the double
// subtraction drive committed below reality and over-commit the worker forever.
func (s *RMStore) Release(ctx context.Context, attemptID string) (*Reservation, error) {
	var released *Reservation
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// Find the owning worker first, then lock it, then delete. Locking the
		// worker before touching the reservation gives every mutation of a
		// worker's ledger the same lock order, which is what prevents two
		// reconcilers from deadlocking against each other.
		var workerID string
		var resJSON []byte
		err := tx.QueryRow(ctx, `
			SELECT worker_id, resources FROM reservations WHERE attempt_id = $1`,
			attemptID).Scan(&workerID, &resJSON)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrReservationNotFound
		}
		if err != nil {
			return fmt.Errorf("store: look up reservation for attempt %s: %w", attemptID, err)
		}

		_, committed, _, err := lockWorker(ctx, tx, workerID)
		if err != nil {
			return err
		}
		vec, err := unmarshalVec(resJSON)
		if err != nil {
			return err
		}

		tag, err := tx.Exec(ctx, `DELETE FROM reservations WHERE attempt_id = $1`, attemptID)
		if err != nil {
			return fmt.Errorf("store: delete reservation for attempt %s: %w", attemptID, err)
		}
		if tag.RowsAffected() == 0 {
			// A racing releaser won between our SELECT and our DELETE. Their
			// transaction performs the decrement; ours must not repeat it.
			return ErrReservationNotFound
		}
		if err := writeCommitted(ctx, tx, workerID, domain.SubVec(committed, vec)); err != nil {
			return err
		}
		released = &Reservation{WorkerID: workerID, AttemptID: attemptID, Resources: vec}
		return nil
	})
	if errors.Is(err, ErrReservationNotFound) {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	return released, nil
}

// ReconcileWorker releases every reservation the control plane holds for a
// worker that the worker does NOT acknowledge, and returns the freed attempts.
//
// THIS IS THE LEAK DETECTOR (§3.2). The worker's reported running_attempt_ids
// are GROUND TRUTH: the worker is the only party that actually knows what it is
// running. Anything the ledger believes is committed to that worker and that the
// worker does not claim is a leak — most commonly because the worker restarted
// and forgot its work while our reservation survived. Without this, every agent
// restart permanently strands a slice of the cluster's capacity, and the ledger
// drifts further from reality with every incident until the cluster looks full
// while sitting idle.
//
// Periodic reconciliation against ground truth is the only way to keep
// distributed accounting honest. Every accounting system that lacks it eventually
// lies.
//
// GRACE PERIOD: reservations younger than `grace` are NEVER released, because a
// just-dispatched attempt legitimately has not reached the worker yet. Without
// it, reconciliation would race dispatch and cancel work the instant it was
// scheduled — turning the leak detector into a leak generator.
func (s *RMStore) ReconcileWorker(ctx context.Context, workerID string, runningAttemptIDs []string, grace time.Duration) ([]*Reservation, error) {
	var leaked []*Reservation

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		_, committed, _, err := lockWorker(ctx, tx, workerID)
		if err != nil {
			return err
		}

		// The acknowledged set MUST be a non-nil empty slice when the worker
		// reports nothing. A nil slice marshals to SQL NULL, and `x <> ALL(NULL)`
		// evaluates to NULL, not TRUE — so the predicate would match no rows and
		// the leak detector would silently do nothing in the single case it most
		// needs to get right: a restarted worker that has forgotten everything.
		acked := runningAttemptIDs
		if acked == nil {
			acked = []string{}
		}
		// `<> ALL(...)` rather than NOT IN, because NOT IN over an array with any
		// NULL element yields NULL for every row and would likewise release nothing.
		rows, err := tx.Query(ctx, `
			SELECT id, attempt_id, resources
			  FROM reservations
			 WHERE worker_id = $1
			   AND attempt_id::text <> ALL($2::text[])
			   AND created_at < now() - $3::interval`,
			workerID, acked, fmt.Sprintf("%d milliseconds", grace.Milliseconds()))
		if err != nil {
			return fmt.Errorf("store: find leaked reservations for worker %s: %w", workerID, err)
		}

		type row struct {
			id, attemptID string
			vec           *nexusv1.ResourceVector
		}
		var found []row
		for rows.Next() {
			var r row
			var resJSON []byte
			if err := rows.Scan(&r.id, &r.attemptID, &resJSON); err != nil {
				rows.Close()
				return fmt.Errorf("store: scan leaked reservation: %w", err)
			}
			if r.vec, err = unmarshalVec(resJSON); err != nil {
				rows.Close()
				return err
			}
			found = append(found, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: iterate leaked reservations: %w", err)
		}
		if len(found) == 0 {
			return nil
		}

		// Sum first, write once. One UPDATE of the worker row rather than N keeps
		// the transaction short and the lock held briefly, which matters because
		// every dispatch to this worker is queued behind that same lock.
		freed := &nexusv1.ResourceVector{}
		ids := make([]string, 0, len(found))
		for _, r := range found {
			freed = domain.AddVec(freed, r.vec)
			ids = append(ids, r.id)
			leaked = append(leaked, &Reservation{
				ID: r.id, WorkerID: workerID, AttemptID: r.attemptID, Resources: r.vec,
			})
		}
		if _, err := tx.Exec(ctx, `DELETE FROM reservations WHERE id = ANY($1::uuid[])`, ids); err != nil {
			return fmt.Errorf("store: delete leaked reservations: %w", err)
		}
		return writeCommitted(ctx, tx, workerID, domain.SubVec(committed, freed))
	})
	if err != nil {
		return nil, err
	}
	return leaked, nil
}

// ReleaseAllForWorker frees every reservation a worker holds, used when a worker
// transitions to DEAD (§5.5: "all reservations released → capacity returns to
// the pool"). A dead worker is running nothing, so its entire committed vector is
// a leak by definition.
func (s *RMStore) ReleaseAllForWorker(ctx context.Context, workerID string) ([]*Reservation, error) {
	// Reconciling against an EMPTY running list with no grace period is exactly
	// "release everything", so the dead path reuses the leak detector rather than
	// duplicating its locking and arithmetic.
	return s.ReconcileWorker(ctx, workerID, nil, 0)
}

// ListReservations returns everything a worker holds, for debugging and for
// tests that assert the ledger.
func (s *RMStore) ListReservations(ctx context.Context, workerID string) ([]*Reservation, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, worker_id, attempt_id, resources, created_at
		  FROM reservations WHERE worker_id = $1 ORDER BY created_at`, workerID)
	if err != nil {
		return nil, fmt.Errorf("store: list reservations for worker %s: %w", workerID, err)
	}
	defer rows.Close()

	var out []*Reservation
	for rows.Next() {
		var r Reservation
		var resJSON []byte
		if err := rows.Scan(&r.ID, &r.WorkerID, &r.AttemptID, &resJSON, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan reservation: %w", err)
		}
		if r.Resources, err = unmarshalVec(resJSON); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// lockWorker takes the row lock and returns the ledger state. Every mutation of a
// worker's capacity goes through here, so the lock order is uniform and two
// concurrent ledger writers serialise rather than race or deadlock.
func lockWorker(ctx context.Context, tx pgx.Tx, workerID string) (capacity, committed *nexusv1.ResourceVector, state WorkerState, err error) {
	var capJSON, commJSON []byte
	err = tx.QueryRow(ctx, `
		SELECT capacity, committed, state FROM workers WHERE id = $1 FOR UPDATE`,
		workerID).Scan(&capJSON, &commJSON, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, "", fmt.Errorf("%w: %s", ErrWorkerNotFound, workerID)
	}
	if err != nil {
		return nil, nil, "", fmt.Errorf("store: lock worker %s: %w", workerID, err)
	}
	if capacity, err = unmarshalVec(capJSON); err != nil {
		return nil, nil, "", err
	}
	if committed, err = unmarshalVec(commJSON); err != nil {
		return nil, nil, "", err
	}
	return capacity, committed, state, nil
}

func writeCommitted(ctx context.Context, tx pgx.Tx, workerID string, v *nexusv1.ResourceVector) error {
	b, err := marshalVec(v)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE workers SET committed = $2 WHERE id = $1`, workerID, b); err != nil {
		return fmt.Errorf("store: update committed for worker %s: %w", workerID, err)
	}
	return nil
}

func getReservationTx(ctx context.Context, tx pgx.Tx, attemptID string) (*Reservation, error) {
	var r Reservation
	var resJSON []byte
	err := tx.QueryRow(ctx, `
		SELECT id, worker_id, attempt_id, resources, created_at
		  FROM reservations WHERE attempt_id = $1`, attemptID).
		Scan(&r.ID, &r.WorkerID, &r.AttemptID, &resJSON, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrReservationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get reservation for attempt %s: %w", attemptID, err)
	}
	if r.Resources, err = unmarshalVec(resJSON); err != nil {
		return nil, err
	}
	return &r, nil
}

// inTx runs fn in a transaction, rolling back on error or panic.
//
// The rollback is deferred rather than written on each error path because a
// forgotten rollback leaks a connection AND holds the worker row lock, which
// blocks every dispatch to that worker until the pool reaps the connection.
func (s *RMStore) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}
	defer func() {
		// Rollback after a successful Commit is a no-op, so this is safe on every
		// path and guarantees the lock is released even on a panic.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit transaction: %w", err)
	}
	return nil
}
