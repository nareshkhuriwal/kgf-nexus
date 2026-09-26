// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuditSink is a SIEM delivery target (migration 0033). The signing secret is
// held only as ciphertext here; it is never returned by the API.
type AuditSink struct {
	ID                  string     `json:"id"`
	Namespace           string     `json:"namespace"`
	Name                string     `json:"name"`
	Kind                string     `json:"kind"`
	URL                 string     `json:"url"`
	Enabled             bool       `json:"enabled"`
	BatchSize           int        `json:"batch_size"`
	CursorSeq           int64      `json:"cursor_seq"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	NextAttemptAt       time.Time  `json:"next_attempt_at"`
	LastAttemptAt       *time.Time `json:"last_attempt_at"`
	LastSuccessAt       *time.Time `json:"last_success_at"`
	LastError           string     `json:"last_error"`
	CreatedBy           string     `json:"created_by"`
	CreatedAt           time.Time  `json:"created_at"`
	Revision            int64      `json:"revision"`

	SecretCiphertext []byte `json:"-"`
	SecretNonce      []byte `json:"-"`
	KeyID            string `json:"-"`
}

const sinkColumns = `id::text, namespace, name, kind, url, enabled, batch_size, cursor_seq,
	consecutive_failures, next_attempt_at, last_attempt_at, last_success_at, last_error,
	created_by, created_at, revision, secret_ciphertext, secret_nonce, key_id`

func scanSink(row pgx.Row) (AuditSink, error) {
	var s AuditSink
	err := row.Scan(&s.ID, &s.Namespace, &s.Name, &s.Kind, &s.URL, &s.Enabled, &s.BatchSize, &s.CursorSeq,
		&s.ConsecutiveFailures, &s.NextAttemptAt, &s.LastAttemptAt, &s.LastSuccessAt, &s.LastError,
		&s.CreatedBy, &s.CreatedAt, &s.Revision, &s.SecretCiphertext, &s.SecretNonce, &s.KeyID)
	return s, err
}

// GatewayCreateAuditSink inserts a sink and its audit row in one transaction.
//
// A new sink starts at the CURRENT head by default (fromStart=false): it
// receives what happens from now on. fromStart=true back-fills everything
// still retained, which is what an auditor setting up a SIEM usually wants.
func GatewayCreateAuditSink(ctx context.Context, db GWDB, s AuditSink, fromStart bool, audit AuditEntry) (AuditSink, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return AuditSink{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var cursor int64
	if !fromStart {
		_ = tx.QueryRow(ctx, `SELECT last_seq FROM audit_chain_heads WHERE namespace = $1`, s.Namespace).Scan(&cursor)
	}
	out, err := scanSink(tx.QueryRow(ctx, `
		INSERT INTO audit_sinks (namespace, name, kind, url, enabled, batch_size, cursor_seq,
		                         secret_ciphertext, secret_nonce, key_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+sinkColumns,
		s.Namespace, s.Name, s.Kind, s.URL, s.Enabled, s.BatchSize, cursor,
		s.SecretCiphertext, s.SecretNonce, s.KeyID, s.CreatedBy))
	if err != nil {
		if isUniqueViolation(err) {
			return AuditSink{}, ErrConflict
		}
		return AuditSink{}, fmt.Errorf("create audit sink: %w", err)
	}
	audit.TargetID = out.ID
	if err := GatewayAppendAudit(ctx, tx, audit); err != nil {
		return AuditSink{}, err
	}
	return out, tx.Commit(ctx)
}

// ErrConflict is a uniqueness or revision conflict.
var ErrConflict = errors.New("conflict")

// GatewayListAuditSinks lists a namespace's sinks.
func GatewayListAuditSinks(ctx context.Context, q GWQuerier, namespace string) ([]AuditSink, error) {
	rows, err := q.Query(ctx, `SELECT `+sinkColumns+` FROM audit_sinks WHERE namespace = $1 ORDER BY name`, namespace)
	if err != nil {
		return nil, fmt.Errorf("list audit sinks: %w", err)
	}
	defer rows.Close()
	var out []AuditSink
	for rows.Next() {
		s, err := scanSink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GatewayGetAuditSink fetches one sink by id within its namespace.
func GatewayGetAuditSink(ctx context.Context, q GWQuerier, namespace, id string) (AuditSink, error) {
	s, err := scanSink(q.QueryRow(ctx, `SELECT `+sinkColumns+` FROM audit_sinks WHERE namespace = $1 AND id::text = $2`, namespace, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}

// AuditSinkUpdate changes a sink; nil fields are unchanged. A new secret
// (rotation) replaces the ciphertext.
type AuditSinkUpdate struct {
	URL       *string
	Enabled   *bool
	BatchSize *int
	// Secret rotation: all three set together.
	SecretCiphertext, SecretNonce []byte
	KeyID                         string
	Revision                      int64
}

// GatewayUpdateAuditSink applies u under optimistic concurrency with its
// audit row in the same transaction.
func GatewayUpdateAuditSink(ctx context.Context, db GWDB, namespace, id string, u AuditSinkUpdate, audit AuditEntry) (AuditSink, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return AuditSink{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, err := scanSink(tx.QueryRow(ctx, `
		UPDATE audit_sinks SET
		    url        = COALESCE($3, url),
		    enabled    = COALESCE($4, enabled),
		    batch_size = COALESCE($5, batch_size),
		    secret_ciphertext = COALESCE($6, secret_ciphertext),
		    secret_nonce      = COALESCE($7, secret_nonce),
		    key_id            = COALESCE(NULLIF($8, ''), key_id),
		    -- re-enabling or re-pointing clears the backoff
		    consecutive_failures = CASE WHEN $3::text IS NOT NULL OR $4::bool THEN 0 ELSE consecutive_failures END,
		    next_attempt_at      = CASE WHEN $3::text IS NOT NULL OR $4::bool THEN now() ELSE next_attempt_at END,
		    revision = revision + 1
		 WHERE namespace = $1 AND id::text = $2 AND revision = $9
		RETURNING `+sinkColumns,
		namespace, id, u.URL, u.Enabled, u.BatchSize, u.SecretCiphertext, u.SecretNonce, u.KeyID, u.Revision))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := GatewayGetAuditSink(ctx, tx, namespace, id); errors.Is(gerr, ErrNotFound) {
			return AuditSink{}, ErrNotFound
		}
		return AuditSink{}, ErrRevisionConflict
	}
	if err != nil {
		return AuditSink{}, fmt.Errorf("update audit sink: %w", err)
	}
	audit.TargetID = id
	if err := GatewayAppendAudit(ctx, tx, audit); err != nil {
		return AuditSink{}, err
	}
	return out, tx.Commit(ctx)
}

// GatewayDeleteAuditSink removes a sink with its audit row in one transaction.
func GatewayDeleteAuditSink(ctx context.Context, db GWDB, namespace, id string, audit AuditEntry) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `DELETE FROM audit_sinks WHERE namespace = $1 AND id::text = $2`, namespace, id)
	if err != nil {
		return fmt.Errorf("delete audit sink: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	audit.TargetID = id
	if err := GatewayAppendAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ── delivery ─────────────────────────────────────────────────────────────

// GatewayDueAuditSinks lists enabled sinks whose next attempt is due.
func GatewayDueAuditSinks(ctx context.Context, q GWQuerier, limit int) ([]AuditSink, error) {
	rows, err := q.Query(ctx, `SELECT `+sinkColumns+` FROM audit_sinks
		WHERE enabled AND next_attempt_at <= now() ORDER BY next_attempt_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditSink
	for rows.Next() {
		s, err := scanSink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GatewayAuditRowsAfter returns up to limit CHAINED rows of a namespace after
// seq, in chain order: a sink's next batch.
func GatewayAuditRowsAfter(ctx context.Context, q GWQuerier, namespace string, after int64, limit int) ([]AuditRow, error) {
	rows, err := q.Query(ctx, `SELECT `+auditColumns+` FROM audit_log
		WHERE namespace = $1 AND chain_seq > $2 ORDER BY chain_seq LIMIT $3`, namespace, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		a, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GatewayAuditSinkDelivered advances the cursor, only forward and only from
// the position the deliverer read (a concurrent config change or a second
// deliverer cannot move it backwards or skip rows).
func GatewayAuditSinkDelivered(ctx context.Context, q GWQuerier, id string, from, to int64) error {
	_, err := q.Exec(ctx, `UPDATE audit_sinks SET cursor_seq = $3, consecutive_failures = 0, last_error = '',
		last_attempt_at = now(), last_success_at = now(), next_attempt_at = now()
		WHERE id::text = $1 AND cursor_seq = $2`, id, from, to)
	return err
}

// GatewayAuditSinkIdle records an attempt with nothing to send.
func GatewayAuditSinkIdle(ctx context.Context, q GWQuerier, id string, next time.Time) error {
	_, err := q.Exec(ctx, `UPDATE audit_sinks SET next_attempt_at = $2 WHERE id::text = $1`, id, next)
	return err
}

// GatewayAuditSinkFailed records a failed delivery and when to retry.
func GatewayAuditSinkFailed(ctx context.Context, q GWQuerier, id, msg string, next time.Time) error {
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, err := q.Exec(ctx, `UPDATE audit_sinks SET consecutive_failures = consecutive_failures + 1,
		last_error = $2, last_attempt_at = now(), next_attempt_at = $3 WHERE id::text = $1`, id, msg, next)
	return err
}
