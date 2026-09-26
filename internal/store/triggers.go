// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

// Inbound webhook triggers (ADR-0028), owned by the api-gateway. Secret
// ciphertexts are read only by WebhookTriggerSecrets.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// WebhookTrigger is a trigger WITHOUT its secrets.
type WebhookTrigger struct {
	ID                 string
	Namespace          string
	Name               string
	SpecName           string
	SpecVersion        *int32
	ParamsMapping      map[string]any
	IdempotencyKeyPath string
	Priority           int32
	ToleranceSeconds   int32
	Enabled            bool
	SecretRotatedAt    time.Time
	PreviousExpiresAt  *time.Time
	CreatedBy          string
	CreatedByID        *string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	LastFiredAt        *time.Time
	FireCount          int64
	Revision           int64
}

const triggerCols = `id::text, namespace, name, spec_name, spec_version, params_mapping, COALESCE(idempotency_key_path, ''),
  priority, tolerance_seconds, enabled, secret_rotated_at,
  CASE WHEN previous_secret_expires_at > now() THEN previous_secret_expires_at END,
  created_by, created_by_id::text, created_at, updated_at, last_fired_at, fire_count, revision`

func scanTrigger(row pgx.Row) (WebhookTrigger, error) {
	var t WebhookTrigger
	var pm []byte
	err := row.Scan(&t.ID, &t.Namespace, &t.Name, &t.SpecName, &t.SpecVersion, &pm, &t.IdempotencyKeyPath,
		&t.Priority, &t.ToleranceSeconds, &t.Enabled, &t.SecretRotatedAt, &t.PreviousExpiresAt,
		&t.CreatedBy, &t.CreatedByID, &t.CreatedAt, &t.UpdatedAt, &t.LastFiredAt, &t.FireCount, &t.Revision)
	if err != nil {
		return t, err
	}
	t.ParamsMapping = map[string]any{}
	_ = json.Unmarshal(pm, &t.ParamsMapping)
	return t, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// TriggerCreate inserts a trigger with its sealed secret. The caller chooses
// the id, because the secret's associated data names it.
func TriggerCreate(ctx context.Context, q GWQuerier, t WebhookTrigger, secret []byte) (WebhookTrigger, error) {
	pm, err := jsonb(t.ParamsMapping)
	if err != nil {
		return t, err
	}
	return scanTrigger(q.QueryRow(ctx, `
INSERT INTO webhook_triggers (id, namespace, name, spec_name, spec_version, params_mapping, idempotency_key_path,
                              priority, tolerance_seconds, secret_ciphertext, enabled, created_by, created_by_id)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11, $12, $13)
RETURNING `+triggerCols, t.ID, t.Namespace, t.Name, t.SpecName, t.SpecVersion, pm, nullStr(t.IdempotencyKeyPath),
		t.Priority, t.ToleranceSeconds, secret, t.Enabled, t.CreatedBy, t.CreatedByID))
}

// TriggerUpdate changes a trigger's configuration (never its secret) if it is
// still at revision (0 is always a conflict: stored triggers start at 1).
func TriggerUpdate(ctx context.Context, q GWQuerier, t WebhookTrigger, revision int64) (WebhookTrigger, error) {
	pm, err := jsonb(t.ParamsMapping)
	if err != nil {
		return t, err
	}
	out, err := scanTrigger(q.QueryRow(ctx, `
UPDATE webhook_triggers
   SET spec_name = $3, spec_version = $4, params_mapping = $5::jsonb, idempotency_key_path = $6,
       priority = $7, tolerance_seconds = $8, enabled = $9, updated_at = now(), revision = revision + 1
 WHERE namespace = $1 AND name = $2 AND revision = $10
RETURNING `+triggerCols, t.Namespace, t.Name, t.SpecName, t.SpecVersion, pm, nullStr(t.IdempotencyKeyPath),
		t.Priority, t.ToleranceSeconds, t.Enabled, revision))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := TriggerGet(ctx, q, t.Namespace, t.Name); gerr == nil {
			return out, ErrRevisionConflict
		}
		return out, ErrNotifyNotFound
	}
	return out, err
}

// TriggerRotate installs a new sealed secret, keeping the old one valid until
// previousExpires (nil drops it immediately).
func TriggerRotate(ctx context.Context, q GWQuerier, ns, name string, secret, previous []byte, previousExpires *time.Time) (WebhookTrigger, error) {
	out, err := scanTrigger(q.QueryRow(ctx, `
UPDATE webhook_triggers
   SET secret_ciphertext = $3, previous_secret_ciphertext = $4, previous_secret_expires_at = $5,
       secret_rotated_at = now(), updated_at = now(), revision = revision + 1
 WHERE namespace = $1 AND name = $2
RETURNING `+triggerCols, ns, name, secret, previous, previousExpires))
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotifyNotFound
	}
	return out, err
}

// TriggerGet reads a trigger by name or id.
func TriggerGet(ctx context.Context, q GWQuerier, ns, nameOrID string) (WebhookTrigger, error) {
	t, err := scanTrigger(q.QueryRow(ctx, `SELECT `+triggerCols+` FROM webhook_triggers WHERE namespace = $1 AND (name = $2 OR id::text = $2)`, ns, nameOrID))
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotifyNotFound
	}
	return t, err
}

// TriggerSecrets returns the current and (unexpired) previous sealed secrets.
func TriggerSecrets(ctx context.Context, q GWQuerier, ns, id string) (current, previous []byte, err error) {
	err = q.QueryRow(ctx, `
SELECT secret_ciphertext, CASE WHEN previous_secret_expires_at > now() THEN previous_secret_ciphertext END
  FROM webhook_triggers WHERE namespace = $1 AND id::text = $2`, ns, id).Scan(&current, &previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotifyNotFound
	}
	return current, previous, err
}

// TriggerList lists a namespace's triggers.
func TriggerList(ctx context.Context, q GWQuerier, ns string) ([]WebhookTrigger, error) {
	rows, err := q.Query(ctx, `SELECT `+triggerCols+` FROM webhook_triggers WHERE namespace = $1 ORDER BY name LIMIT 1000`, ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebhookTrigger
	for rows.Next() {
		t, err := scanTrigger(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TriggerDelete removes a trigger and its receipts.
func TriggerDelete(ctx context.Context, q GWQuerier, ns, name string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `DELETE FROM webhook_triggers WHERE namespace = $1 AND name = $2 RETURNING id::text`, ns, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotifyNotFound
	}
	return id, err
}

// WebhookReceipt is one inbound delivery.
type WebhookReceipt struct {
	TriggerID       string
	SignatureDigest string
	ReceivedAt      time.Time
	Outcome         string
	HTTPStatus      int32
	RunID           string
	IdempotencyKey  string
	Detail          string
}

// TriggerClaimReceipt records a signed delivery. false means the same
// signature was already received and not rejected: a replay. A delivery whose
// earlier attempt was REJECTED (the run could not be created) may be claimed
// again, so a sender retrying the identical request after an outage succeeds.
func TriggerClaimReceipt(ctx context.Context, q GWQuerier, ns, triggerID, digest string) (bool, error) {
	tag, err := q.Exec(ctx, `
INSERT INTO webhook_receipts AS w (trigger_id, signature_digest, namespace, outcome, http_status)
VALUES ($1, $2, $3, 'accepted', 202)
ON CONFLICT (trigger_id, signature_digest) DO UPDATE
   SET outcome = 'accepted', http_status = 202, received_at = now(), detail = ''
 WHERE w.outcome = 'rejected'`, triggerID, digest, ns)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// TriggerFinishReceipt records a delivery's outcome and, when a run was
// created or found, counts the fire.
func TriggerFinishReceipt(ctx context.Context, q GWQuerier, triggerID, digest, outcome string, status int, runID, idemKey, detail string) error {
	if _, err := q.Exec(ctx, `
UPDATE webhook_receipts SET outcome = $3, http_status = $4, run_id = $5, idempotency_key = $6, detail = $7
 WHERE trigger_id = $1 AND signature_digest = $2`, triggerID, digest, outcome, status, nullUUID(runID), nullStr(idemKey), detail); err != nil {
		return err
	}
	if runID != "" {
		_, err := q.Exec(ctx, `UPDATE webhook_triggers SET last_fired_at = now(), fire_count = fire_count + 1 WHERE id = $1`, triggerID)
		return err
	}
	return nil
}

// TriggerPruneReceipts removes a trigger's receipts older than keep. Receipts
// only need to outlive the timestamp tolerance to stop replays; the rest is a
// delivery log.
func TriggerPruneReceipts(ctx context.Context, q GWQuerier, triggerID string, keep time.Duration) error {
	_, err := q.Exec(ctx, `
DELETE FROM webhook_receipts w USING (
  SELECT signature_digest FROM webhook_receipts
   WHERE trigger_id = $1 AND received_at < now() - make_interval(secs => $2)
   LIMIT 500
) x WHERE w.trigger_id = $1 AND w.signature_digest = x.signature_digest`, triggerID, keep.Seconds())
	return err
}

// TriggerListReceipts returns a trigger's recent deliveries.
func TriggerListReceipts(ctx context.Context, q GWQuerier, triggerID string, limit int) ([]WebhookReceipt, error) {
	limit = ClampLimit(limit, DefaultReceipts, MaxReceiptsPage)
	rows, err := q.Query(ctx, `
SELECT trigger_id::text, signature_digest, received_at, outcome, http_status, COALESCE(run_id::text, ''),
       COALESCE(idempotency_key, ''), detail
  FROM webhook_receipts WHERE trigger_id = $1 ORDER BY received_at DESC LIMIT $2`, triggerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebhookReceipt
	for rows.Next() {
		var r WebhookReceipt
		if err := rows.Scan(&r.TriggerID, &r.SignatureDigest, &r.ReceivedAt, &r.Outcome, &r.HTTPStatus, &r.RunID,
			&r.IdempotencyKey, &r.Detail); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
