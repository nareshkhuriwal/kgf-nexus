// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/khuriwalgroup/kgf-nexus/internal/auditchain"
	"github.com/khuriwalgroup/kgf-nexus/internal/kgfctx"
)

// ── audit log (ADR-0025 §2, migration 0033) ──────────────────────────────

// AuditEntry is one administrative event to record.
type AuditEntry struct {
	Namespace  string
	ActorID    *string
	ActorKind  string
	ActorLabel string
	Action     string
	TargetKind string
	TargetID   string
	Outcome    string
	Detail     map[string]any
	ClientIP   string
	// KGF is the KGF Cloud attribution of a request whose X-KGF-Context the
	// gateway verified; zero when there was none (migration 0033).
	KGF auditchain.KGF
	// RequestID correlates the row with the request (KGF's request id from a
	// verified context, else the caller's X-Request-ID).
	RequestID string
}

// AuditRow is a stored audit event.
type AuditRow struct {
	ID         int64
	Namespace  string
	ActorID    *string
	ActorKind  string
	ActorLabel string
	Action     string
	TargetKind string
	TargetID   string
	Outcome    string
	Detail     []byte
	ClientIP   string
	OccurredAt time.Time
	KGF        auditchain.KGF
	RequestID  string
	// Chain position (nil for rows written before migration 0033, or by an
	// older gateway replica during a rolling upgrade).
	Seq      *int64
	PrevHash *string
	RowHash  *string
}

// Record renders the row in the export format.
func (a AuditRow) Record() auditchain.Record {
	detail := a.Detail
	if len(detail) == 0 {
		detail = []byte("{}")
	}
	return auditchain.Record{
		ID: a.ID, Namespace: a.Namespace, Seq: a.Seq, OccurredAt: a.OccurredAt.UTC().Format(auditchain.TimeLayout),
		ActorID: a.ActorID, ActorKind: a.ActorKind, ActorLabel: a.ActorLabel, Action: a.Action,
		TargetKind: a.TargetKind, TargetID: a.TargetID, Outcome: a.Outcome, Detail: json.RawMessage(detail),
		ClientIP: a.ClientIP, PrevHash: a.PrevHash, RowHash: a.RowHash, RequestID: a.RequestID,
		KGF: kgfPtr(a.KGF),
	}
}

func kgfPtr(k auditchain.KGF) *auditchain.KGF {
	if k.IsZero() {
		return nil
	}
	return &k
}

// Audit outcomes. A DENIED attempt is recorded as loudly as a successful one: an
// audit log containing only successes cannot show an attack in progress.
const (
	AuditSuccess = "success"
	AuditDenied  = "denied"
	AuditError   = "error"
)

// auditNow is the audit clock; a variable only so integration tests can write
// back-dated rows to exercise retention.
var auditNow = time.Now

type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// GatewayAppendAudit records one administrative event as the next link of its
// namespace's hash chain.
//
// APPEND-ONLY (invariant 8), enforced by migration 0007's INSERT-only grant
// and 0033's triggers rather than by this function's good behaviour.
//
// The append runs in a transaction: q's own when q is a pgx.Tx (a SAVEPOINT
// inside it, so the audit row commits or rolls back WITH the change it
// records), a new one when q is a pool. Inside it the namespace's chain head is
// locked FOR UPDATE, so concurrent gateway replicas serialise per namespace
// and the chain cannot fork.
//
// The caller builds Detail explicitly rather than handing over a request
// object: this row is read by humans during incidents and exported to SIEMs,
// and must never contain secret material.
func GatewayAppendAudit(ctx context.Context, q GWQuerier, e AuditEntry) error {
	if b, ok := q.(beginner); ok {
		tx, err := b.Begin(ctx)
		if err != nil {
			return fmt.Errorf("append audit %s: begin: %w", e.Action, err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := appendChained(ctx, tx, e); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("append audit %s: commit: %w", e.Action, err)
		}
		return nil
	}
	return appendChained(ctx, q, e)
}

func appendChained(ctx context.Context, q GWQuerier, e AuditEntry) error {
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	raw, err := json.Marshal(e.Detail)
	if err != nil {
		return fmt.Errorf("encode audit detail: %w", err)
	}
	if e.Outcome == "" {
		e.Outcome = AuditSuccess
	}
	if e.ActorKind == "" {
		e.ActorKind = "anonymous"
	}
	if e.ActorID != nil {
		lower := strings.ToLower(*e.ActorID)
		e.ActorID = &lower
	}
	// KGF attribution travels in the request context (set by the gateway's
	// guard only after the X-KGF-Context signature verified), so every audit
	// writer -- helper or in-transaction store function -- records it.
	if e.KGF.IsZero() {
		if k, ok := kgfctx.From(ctx); ok {
			e.KGF = auditchain.KGF{Organization: k.Organization, Subscription: k.Subscription,
				ResourceGroup: k.ResourceGroup, Principal: k.Principal, OnBehalfOf: k.OnBehalfOf}
		}
	}
	if e.RequestID == "" {
		e.RequestID = kgfctx.RequestIDFrom(ctx)
	}

	// PostgreSQL's normal form of the detail (numbers, duplicate keys), so the
	// hash is over exactly what an export will later carry.
	var normalised string
	if err := q.QueryRow(ctx, `SELECT $1::jsonb::text`, raw).Scan(&normalised); err != nil {
		return fmt.Errorf("append audit %s: normalise detail: %w", e.Action, err)
	}

	genesis := auditchain.GenesisHash(e.Namespace)
	if _, err := q.Exec(ctx, `
		WITH ins AS (
		    INSERT INTO audit_chain_heads (namespace, last_seq, last_hash)
		    VALUES ($1, 0, $2) ON CONFLICT (namespace) DO NOTHING RETURNING namespace)
		INSERT INTO audit_chain_checkpoints (namespace, seq, hash, kind)
		SELECT namespace, 0, $2, 'genesis' FROM ins`, e.Namespace, genesis); err != nil {
		return fmt.Errorf("append audit %s: start chain: %w", e.Action, err)
	}
	var lastSeq int64
	var lastHash string
	if err := q.QueryRow(ctx, `
		SELECT last_seq, last_hash FROM audit_chain_heads WHERE namespace = $1 FOR UPDATE`,
		e.Namespace).Scan(&lastSeq, &lastHash); err != nil {
		return fmt.Errorf("append audit %s: lock chain head: %w", e.Action, err)
	}

	occurred := auditNow().UTC().Truncate(time.Microsecond)
	seq := lastSeq + 1
	hash, err := auditchain.Hash(auditchain.Row{
		Namespace: e.Namespace, Seq: seq, OccurredAt: occurred, ActorID: e.ActorID,
		ActorKind: e.ActorKind, ActorLabel: e.ActorLabel, Action: e.Action, TargetKind: e.TargetKind,
		TargetID: e.TargetID, Outcome: e.Outcome, Detail: json.RawMessage(normalised),
		ClientIP: e.ClientIP, KGF: e.KGF, RequestID: e.RequestID, PrevHash: lastHash,
	})
	if err != nil {
		return fmt.Errorf("append audit %s: hash: %w", e.Action, err)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO audit_log
		    (namespace, actor_id, actor_kind, actor_label, action, target_kind, target_id,
		     outcome, detail, client_ip, occurred_at, chain_seq, prev_hash, row_hash,
		     kgf_organization, kgf_subscription, kgf_resource_group, kgf_principal, kgf_on_behalf_of, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
		e.Namespace, e.ActorID, e.ActorKind, e.ActorLabel, e.Action, e.TargetKind, e.TargetID,
		e.Outcome, normalised, e.ClientIP, occurred, seq, lastHash, hash,
		e.KGF.Organization, e.KGF.Subscription, e.KGF.ResourceGroup, e.KGF.Principal, e.KGF.OnBehalfOf, e.RequestID); err != nil {
		return fmt.Errorf("append audit %s: %w", e.Action, err)
	}
	if _, err := q.Exec(ctx, `
		UPDATE audit_chain_heads SET last_seq = $2, last_hash = $3 WHERE namespace = $1`,
		e.Namespace, seq, hash); err != nil {
		return fmt.Errorf("append audit %s: advance chain head: %w", e.Action, err)
	}
	return nil
}

// ── listing with a keyset cursor ─────────────────────────────────────────

// AuditFilter narrows an audit query.
type AuditFilter struct {
	Action  string
	ActorID string
	Outcome string
	Since   *time.Time // inclusive
	Until   *time.Time // exclusive
	// Cursor is the opaque next_cursor of a previous page ("" = first page).
	Cursor string
	Limit  int
}

// ErrBadCursor is a cursor this server did not issue.
var ErrBadCursor = errors.New("invalid audit cursor")

type auditCursor struct {
	T time.Time `json:"t"`
	I int64     `json:"i"`
}

func encodeAuditCursor(t time.Time, id int64) string {
	b, _ := json.Marshal(auditCursor{T: t.UTC(), I: id})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeAuditCursor(s string) (*auditCursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, ErrBadCursor
	}
	var c auditCursor
	if json.Unmarshal(b, &c) != nil || c.I <= 0 {
		return nil, ErrBadCursor
	}
	return &c, nil
}

const auditColumns = `id, namespace, actor_id::text, actor_kind, actor_label, action,
		       target_kind, target_id, outcome, detail::text, client_ip, occurred_at,
		       chain_seq, prev_hash, row_hash, kgf_organization, kgf_subscription,
		       kgf_resource_group, kgf_principal, kgf_on_behalf_of, request_id`

func scanAudit(rows pgx.Rows) (AuditRow, error) {
	var a AuditRow
	var detail string
	err := rows.Scan(&a.ID, &a.Namespace, &a.ActorID, &a.ActorKind, &a.ActorLabel,
		&a.Action, &a.TargetKind, &a.TargetID, &a.Outcome, &detail,
		&a.ClientIP, &a.OccurredAt, &a.Seq, &a.PrevHash, &a.RowHash,
		&a.KGF.Organization, &a.KGF.Subscription, &a.KGF.ResourceGroup, &a.KGF.Principal,
		&a.KGF.OnBehalfOf, &a.RequestID)
	a.Detail = []byte(detail)
	return a, err
}

// GatewayListAudit reads one page of a namespace's audit trail, most recent
// first, and the cursor of the next page ("" when this is the last).
//
// Keyset, not OFFSET: a page boundary is the (occurred_at, id) of the last
// row served, so rows appended while an operator pages never shift or repeat
// entries, and page N costs the same as page 1.
func GatewayListAudit(ctx context.Context, q GWQuerier, namespace string, f AuditFilter) ([]AuditRow, string, error) {
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	cur, err := decodeAuditCursor(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	var curT *time.Time
	var curI int64
	if cur != nil {
		curT, curI = &cur.T, cur.I
	}
	rows, err := q.Query(ctx, `
		SELECT `+auditColumns+`
		  FROM audit_log
		 WHERE namespace = $1
		   AND ($2 = '' OR action = $2)
		   AND ($3 = '' OR actor_id::text = $3)
		   AND ($4 = '' OR outcome = $4)
		   AND ($5::timestamptz IS NULL OR occurred_at >= $5)
		   AND ($6::timestamptz IS NULL OR occurred_at <  $6)
		   AND ($7::timestamptz IS NULL OR (occurred_at, id) < ($7, $8))
		 ORDER BY occurred_at DESC, id DESC
		 LIMIT $9`,
		namespace, f.Action, f.ActorID, f.Outcome, f.Since, f.Until, curT, curI, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()
	out := make([]AuditRow, 0, 32)
	for rows.Next() {
		a, err := scanAudit(rows)
		if err != nil {
			return nil, "", fmt.Errorf("scan audit row: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate audit: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeAuditCursor(last.OccurredAt, last.ID)
	}
	return out, next, nil
}

// ── export ───────────────────────────────────────────────────────────────

// AuditExportMeta is the chain context of an export snapshot.
type AuditExportMeta struct {
	Anchor         *auditchain.Checkpoint
	Head           *auditchain.Checkpoint
	ChainStartedAt *time.Time
}

// exportBatch bounds the rows held in memory at once during an export.
const exportBatch = 1000

// GatewayExportAudit streams a namespace's audit rows in chain (insertion)
// order to fn, from ONE consistent snapshot (REPEATABLE READ, read only), so
// the anchor, the rows and the head it reports agree even while other
// replicas keep appending. meta is called once, before the first row.
//
// Memory is bounded by exportBatch rows; the snapshot is held for the whole
// export, which on a very large trail is a long-running read transaction --
// prefer since/until windows for routine SIEM back-fills.
func GatewayExportAudit(ctx context.Context, db GWDB, namespace string, f AuditFilter,
	meta func(AuditExportMeta) error, fn func(AuditRow) error) error {
	b, ok := db.(interface {
		BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	})
	var tx pgx.Tx
	var err error
	if ok {
		tx, err = b.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	} else {
		tx, err = db.Begin(ctx)
	}
	if err != nil {
		return fmt.Errorf("export audit: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var m AuditExportMeta
	var hs int64
	var hh string
	var started time.Time
	switch err := tx.QueryRow(ctx, `SELECT last_seq, last_hash, started_at FROM audit_chain_heads WHERE namespace = $1`,
		namespace).Scan(&hs, &hh, &started); {
	case err == nil:
		m.Head = &auditchain.Checkpoint{Kind: "head", Seq: hs, Hash: hh}
		m.ChainStartedAt = &started
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return fmt.Errorf("export audit: head: %w", err)
	}
	var ak string
	var as int64
	var ah string
	switch err := tx.QueryRow(ctx, `
		SELECT kind, seq, hash FROM audit_chain_checkpoints WHERE namespace = $1
		 ORDER BY seq DESC, kind DESC LIMIT 1`, namespace).Scan(&ak, &as, &ah); {
	case err == nil:
		m.Anchor = &auditchain.Checkpoint{Kind: ak, Seq: as, Hash: ah}
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return fmt.Errorf("export audit: anchor: %w", err)
	}
	if err := meta(m); err != nil {
		return err
	}

	var after int64
	for {
		rows, err := tx.Query(ctx, `
			SELECT `+auditColumns+`
			  FROM audit_log
			 WHERE namespace = $1 AND id > $2
			   AND ($3 = '' OR action = $3)
			   AND ($4 = '' OR actor_id::text = $4)
			   AND ($5 = '' OR outcome = $5)
			   AND ($6::timestamptz IS NULL OR occurred_at >= $6)
			   AND ($7::timestamptz IS NULL OR occurred_at <  $7)
			 ORDER BY id
			 LIMIT $8`,
			namespace, after, f.Action, f.ActorID, f.Outcome, f.Since, f.Until, exportBatch)
		if err != nil {
			return fmt.Errorf("export audit: %w", err)
		}
		batch := make([]AuditRow, 0, exportBatch)
		for rows.Next() {
			a, err := scanAudit(rows)
			if err != nil {
				rows.Close()
				return fmt.Errorf("export audit: scan: %w", err)
			}
			batch = append(batch, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("export audit: %w", err)
		}
		for _, a := range batch {
			if err := fn(a); err != nil {
				return err
			}
		}
		if len(batch) < exportBatch {
			return nil
		}
		after = batch[len(batch)-1].ID
	}
}

// ── retention and legal hold ─────────────────────────────────────────────

// AuditPolicy is a namespace's retention policy and legal hold.
type AuditPolicy struct {
	Namespace       string     `json:"namespace"`
	RetentionDays   *int       `json:"retention_days"`
	LegalHold       bool       `json:"legal_hold"`
	LegalHoldReason string     `json:"legal_hold_reason"`
	LegalHoldBy     string     `json:"legal_hold_by"`
	LegalHoldAt     *time.Time `json:"legal_hold_at"`
	Revision        int64      `json:"revision"`
	UpdatedBy       string     `json:"updated_by"`
	UpdatedAt       time.Time  `json:"updated_at"`
	LastPurgeAt     *time.Time `json:"last_purge_at"`
}

// ErrRevisionConflict is an optimistic-concurrency failure.
var ErrRevisionConflict = errors.New("revision conflict")

// GatewayGetAuditPolicy returns the policy, or the default (keep forever, no
// hold, revision 0) when none was ever set.
func GatewayGetAuditPolicy(ctx context.Context, q GWQuerier, namespace string) (AuditPolicy, error) {
	p := AuditPolicy{Namespace: namespace}
	err := q.QueryRow(ctx, `
		SELECT retention_days, legal_hold, legal_hold_reason, legal_hold_by, legal_hold_at,
		       revision, updated_by, updated_at, last_purge_at
		  FROM audit_policies WHERE namespace = $1`, namespace).Scan(
		&p.RetentionDays, &p.LegalHold, &p.LegalHoldReason, &p.LegalHoldBy, &p.LegalHoldAt,
		&p.Revision, &p.UpdatedBy, &p.UpdatedAt, &p.LastPurgeAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, fmt.Errorf("get audit policy: %w", err)
	}
	return p, nil
}

// AuditPolicyChange is one policy update; nil fields are left unchanged.
type AuditPolicyChange struct {
	// SetRetention applies RetentionDays (nil = keep forever).
	SetRetention  bool
	RetentionDays *int
	// LegalHold, when non-nil, sets or clears the hold with Reason.
	LegalHold *bool
	Reason    string
	// Revision must equal the stored revision (0 when none exists yet).
	Revision int64
	Actor    string
}

// GatewayPutAuditPolicy applies a change under optimistic concurrency and
// writes its audit row IN THE SAME TRANSACTION: the change and its record
// commit together or not at all (security-sensitive, fail closed).
func GatewayPutAuditPolicy(ctx context.Context, db GWDB, namespace string, c AuditPolicyChange, audit AuditEntry) (AuditPolicy, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return AuditPolicy{}, fmt.Errorf("put audit policy: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO audit_policies (namespace, revision) VALUES ($1, 0)
		ON CONFLICT (namespace) DO NOTHING`, namespace); err != nil {
		return AuditPolicy{}, fmt.Errorf("put audit policy: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM audit_policies WHERE namespace = $1 FOR UPDATE`, namespace); err != nil {
		return AuditPolicy{}, fmt.Errorf("put audit policy: lock: %w", err)
	}
	cur, err := GatewayGetAuditPolicy(ctx, tx, namespace)
	if err != nil {
		return AuditPolicy{}, err
	}
	if cur.Revision != c.Revision {
		return cur, ErrRevisionConflict
	}
	next := cur
	if c.SetRetention {
		next.RetentionDays = c.RetentionDays
	}
	if c.LegalHold != nil {
		next.LegalHold = *c.LegalHold
		if next.LegalHold {
			now := time.Now().UTC()
			next.LegalHoldReason, next.LegalHoldBy, next.LegalHoldAt = c.Reason, c.Actor, &now
		} else {
			next.LegalHoldReason, next.LegalHoldBy, next.LegalHoldAt = "", "", nil
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE audit_policies
		   SET retention_days = $2, legal_hold = $3, legal_hold_reason = $4, legal_hold_by = $5,
		       legal_hold_at = $6, revision = revision + 1, updated_by = $7, updated_at = now()
		 WHERE namespace = $1`,
		namespace, next.RetentionDays, next.LegalHold, next.LegalHoldReason, next.LegalHoldBy,
		next.LegalHoldAt, c.Actor); err != nil {
		return AuditPolicy{}, fmt.Errorf("put audit policy: %w", err)
	}
	if err := GatewayAppendAudit(ctx, tx, audit); err != nil {
		return AuditPolicy{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AuditPolicy{}, fmt.Errorf("put audit policy: commit: %w", err)
	}
	return GatewayGetAuditPolicy(ctx, db, namespace)
}

// ErrLegalHold is audit_purge's refusal under legal hold.
var ErrLegalHold = errors.New("namespace is under legal hold")

// GatewayPurgeAudit applies the namespace's retention policy through the
// SECURITY DEFINER audit_purge() (migration 0033) and returns the rows removed.
func GatewayPurgeAudit(ctx context.Context, q GWQuerier, namespace string) (int64, error) {
	var n int64
	if err := q.QueryRow(ctx, `SELECT audit_purge($1)`, namespace).Scan(&n); err != nil {
		if strings.Contains(err.Error(), "legal hold") {
			return 0, ErrLegalHold
		}
		return 0, fmt.Errorf("purge audit: %w", err)
	}
	return n, nil
}

// GatewayAuditRetentionNamespaces lists namespaces with a retention policy and
// no legal hold: the purge loop's work list.
func GatewayAuditRetentionNamespaces(ctx context.Context, q GWQuerier) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT namespace FROM audit_policies
		WHERE retention_days IS NOT NULL AND NOT legal_hold ORDER BY namespace`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
