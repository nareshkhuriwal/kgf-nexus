// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
)

// Namespace quotas through the gateway (ADR-0021). Quotas are a sub-document of
// the namespace's platform policy, which the gateway owns (0010), so they share
// its revision: the quota editor and the infrastructure editor cannot silently
// overwrite each other.

// ErrQuotaLocked means a namespace administrator tried to loosen quotas a
// platform operator locked. The concrete error is *QuotaLockedError.
var ErrQuotaLocked = errors.New("quotas are locked by a platform operator")

// QuotaLockedError lists what the refused change would have loosened.
type QuotaLockedError struct{ Loosened []string }

func (e *QuotaLockedError) Error() string {
	return fmt.Sprintf("%v: only a platform operator may loosen %s", ErrQuotaLocked, strings.Join(e.Loosened, ", "))
}

// Is makes errors.Is(err, ErrQuotaLocked) true.
func (e *QuotaLockedError) Is(target error) bool { return target == ErrQuotaLocked }

// GatewayQuotasGet returns the namespace's quotas and the policy revision they
// belong to. A namespace with no policy, or a policy without quotas, has the
// zero Quotas: unlimited.
func GatewayQuotasGet(ctx context.Context, q GWQuerier, ns string) (int64, platform.Quotas, error) {
	c, err := GatewayPlatformGet(ctx, q, ns)
	if err != nil {
		return 0, platform.Quotas{}, err
	}
	if c.Quotas == nil {
		return c.Revision, platform.Quotas{}, nil
	}
	return c.Revision, *c.Quotas, nil
}

// GatewayQuotasPut replaces the namespace's quotas if the policy is still at
// expectRevision, and writes a quota.update audit row in the same transaction.
//
// operator says whether the caller is a platform operator. Without it, a change
// that loosens locked quotas, or that sets or clears the lock, is refused.
// Returns the new revision.
func GatewayQuotasPut(
	ctx context.Context, db GWDB, ns string, expectRevision int64, next platform.Quotas,
	operator bool, audit AuditEntry,
) (int64, error) {
	if err := next.Validate(); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// FOR UPDATE: the revision check and the write are one decision, and the
	// generic platform-config PUT races us on the same row.
	var (
		rev int64
		raw []byte
	)
	err = tx.QueryRow(ctx, `SELECT revision, config FROM platform_config WHERE namespace=$1 FOR UPDATE`, ns).Scan(&rev, &raw)
	exists := true
	if errors.Is(err, pgx.ErrNoRows) {
		exists, rev = false, 0
	} else if err != nil {
		return 0, err
	}
	if rev != expectRevision {
		return 0, ErrPlatformConflict
	}
	var cur platform.Quotas
	if exists {
		var stored struct {
			Quotas *platform.Quotas `json:"quotas"`
		}
		if err := json.Unmarshal(raw, &stored); err != nil {
			return 0, fmt.Errorf("decode stored quotas: %w", err)
		}
		if stored.Quotas != nil {
			cur = *stored.Quotas
		}
	}
	if !operator {
		if cur.Locked != next.Locked && !cur.Locked {
			return 0, &QuotaLockedError{Loosened: []string{"locked (only a platform operator may lock quotas)"}}
		}
		if cur.Locked {
			if l := cur.Loosened(next); len(l) > 0 {
				return 0, &QuotaLockedError{Loosened: l}
			}
		}
	}

	quotasJSON, err := json.Marshal(next)
	if err != nil {
		return 0, err
	}
	newRev := rev + 1
	if exists {
		// jsonb_set rather than re-marshalling the whole policy: fields this
		// binary does not know (a newer replica's) survive a quota edit.
		_, err = tx.Exec(ctx, `
UPDATE platform_config
   SET revision = $2,
       config = jsonb_set(jsonb_set(config, '{quotas}', $3::jsonb), '{revision}', to_jsonb($2::bigint)),
       updated_at = now()
 WHERE namespace = $1 AND revision = $4`, ns, newRev, quotasJSON, rev)
	} else {
		c := platform.Default()
		c.Revision = newRev
		c.Quotas = &next
		var doc []byte
		if doc, err = json.Marshal(c); err != nil {
			return 0, err
		}
		var tag pgconn.CommandTag
		tag, err = tx.Exec(ctx, `INSERT INTO platform_config(namespace,revision,config) VALUES ($1,$2,$3) ON CONFLICT(namespace) DO NOTHING`, ns, newRev, doc)
		if err == nil && tag.RowsAffected() != 1 {
			return 0, ErrPlatformConflict
		}
	}
	if err != nil {
		return 0, err
	}

	audit.Action = "quota.update"
	audit.TargetKind = "namespace_quotas"
	audit.TargetID = ns
	audit.Namespace = ns
	audit.Outcome = AuditSuccess
	audit.Detail = map[string]any{"revision": newRev, "before": cur, "after": next, "operator": operator}
	if l := cur.Loosened(next); len(l) > 0 {
		audit.Detail["loosened"] = l
	}
	if err := GatewayAppendAudit(ctx, tx, audit); err != nil {
		return 0, err
	}
	return newRev, tx.Commit(ctx)
}

// NamespaceUsageSnapshot is a namespace's current consumption of every quota
// dimension, for the quotas API and page.
type NamespaceUsageSnapshot struct {
	Inflight      platform.QuotaUsage
	ActiveRuns    int64
	RunsLastHour  int64
	StorageBytes  int64
	TenantID      uuid.UUID
	HasTenant     bool
	StorageSource string
}

// GatewayNamespaceUsage reads current usage without taking the quota locks: it
// is a display, and a moment-stale number is fine there.
//
// Storage is measured from durable task-log bytes (attempt_logs, 0025). Result
// store artifacts are not indexed by size in the database, so they are not
// counted; StorageSource says so, and the API passes that on rather than
// presenting a partial number as the whole.
func GatewayNamespaceUsage(ctx context.Context, q GWQuerier, ns string) (NamespaceUsageSnapshot, error) {
	var out NamespaceUsageSnapshot
	out.StorageSource = "task_logs"
	err := q.QueryRow(ctx, `SELECT id FROM tenants WHERE namespace=$1`, ns).Scan(&out.TenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		out.Inflight.Custom = map[string]int64{}
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.HasTenant = true
	if out.Inflight, err = NamespaceInflightUsage(ctx, q, out.TenantID); err != nil {
		return out, err
	}
	err = q.QueryRow(ctx, `
SELECT (SELECT count(*) FROM workflow_runs
         WHERE namespace=$1 AND state IN ('PENDING','RUNNING','PAUSING','PAUSED','CANCELLING')),
       (SELECT count(*) FROM workflow_runs WHERE namespace=$1 AND created_at > now() - interval '1 hour'),
       (SELECT COALESCE(sum(byte_count), 0)::bigint FROM attempt_logs WHERE tenant_id=$2)`,
		ns, out.TenantID).Scan(&out.ActiveRuns, &out.RunsLastHour, &out.StorageBytes)
	return out, err
}
