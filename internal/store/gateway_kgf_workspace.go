// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

// KGF.Nexus workspaces (ADR-0034, migration 0040): one Nexus tenant namespace
// per KGF workspace resource, provisioned EXPLICITLY by the KGF.Nexus provider
// front (PUT /v1/kgf/workspace) and never on first use. Owned by the
// api-gateway, like kgf_scopes.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Workspace states.
const (
	KGFWorkspaceActive  = "active"
	KGFWorkspaceDeleted = "deleted"
)

var (
	// ErrKGFWorkspaceDeleted: the workspace was deleted. Its namespace is
	// derived from (organization, resource id), so re-creating it would
	// reactivate the old namespace and its history; that is refused.
	ErrKGFWorkspaceDeleted = errors.New("kgf: workspace was deleted and cannot be re-created under the same resource id")
	// ErrKGFWorkspaceNotProvisioned: no active workspace for the resource id.
	ErrKGFWorkspaceNotProvisioned = errors.New("kgf: workspace is not provisioned")
)

// KGFWorkspace is one KGF.Nexus workspace scope. ResourceID is the
// normalised (lower-case) id; Subscription and ResourceGroup are lower-cased
// as in KGFScope.
type KGFWorkspace struct {
	Organization  string `json:"organization"`
	Subscription  string `json:"subscription"`
	ResourceGroup string `json:"resource_group"`
	Workspace     string `json:"workspace"`
	ResourceID    string `json:"resource_id"`
}

func (w KGFWorkspace) normalized() KGFWorkspace {
	return KGFWorkspace{
		// The organization enters the hash exactly as KGF's context carries
		// it (the ADR-0034 formula); KGF organization ids are lower-case by
		// KGF's own pattern, so this only matters for a malformed context.
		Organization:  strings.TrimSpace(w.Organization),
		Subscription:  strings.ToLower(strings.TrimSpace(w.Subscription)),
		ResourceGroup: strings.ToLower(strings.TrimSpace(w.ResourceGroup)),
		Workspace:     strings.ToLower(strings.TrimSpace(w.Workspace)),
		ResourceID:    strings.ToLower(strings.TrimSpace(w.ResourceID)),
	}
}

// Complete reports whether every part is present and within bounds.
func (w KGFWorkspace) Complete() bool {
	n := w.normalized()
	return n.Organization != "" && n.Subscription != "" && n.ResourceGroup != "" && n.Workspace != "" &&
		n.ResourceID != "" && len(n.Organization) <= 128 && len(n.Subscription) <= 128 &&
		len(n.ResourceGroup) <= 128 && len(n.ResourceID) <= 512
}

// KGFWorkspaceNamespace derives the namespace of a KGF.Nexus workspace:
//
//	"kgf-" + hex(sha256("kgf-nexus/v2" 0x00 org 0x00 lower(resource id)))[:32]
//
// "v2" is the mapping's version (v1 is KGFNamespace, per resource group). The
// version string differs, so a v1 and a v2 namespace can collide only through
// a SHA-256 collision; and each provisioning path refuses a namespace the
// other recorded (namespace_conflict). The organization is inside the hash, as
// in v1, so equal resource ids in two organizations never share a namespace.
func KGFWorkspaceNamespace(organization, resourceID string) string {
	sum := sha256.Sum256([]byte("kgf-nexus/v2\x00" + strings.TrimSpace(organization) + "\x00" +
		strings.ToLower(strings.TrimSpace(resourceID))))
	return "kgf-" + hex.EncodeToString(sum[:])[:32]
}

// KGFWorkspaceRecord is a stored workspace row.
type KGFWorkspaceRecord struct {
	KGFWorkspace
	Namespace string
	State     string
	CreatedAt time.Time
	DeletedAt *time.Time
}

const kgfWorkspaceColumns = `namespace, organization, subscription, resource_group, workspace, resource_id, state, created_at, deleted_at`

func scanKGFWorkspace(row pgx.Row) (KGFWorkspaceRecord, error) {
	var r KGFWorkspaceRecord
	err := row.Scan(&r.Namespace, &r.Organization, &r.Subscription, &r.ResourceGroup, &r.Workspace,
		&r.ResourceID, &r.State, &r.CreatedAt, &r.DeletedAt)
	return r, err
}

// GatewayKGFWorkspace reads the workspace provisioned for (organization,
// resource id), in any state. ErrNotFound when there is none.
func GatewayKGFWorkspace(ctx context.Context, q GWQuerier, organization, resourceID string) (KGFWorkspaceRecord, error) {
	r, err := scanKGFWorkspace(q.QueryRow(ctx, `SELECT `+kgfWorkspaceColumns+` FROM kgf_workspaces
		WHERE organization = $1 AND resource_id = $2`,
		strings.TrimSpace(organization), strings.ToLower(strings.TrimSpace(resourceID))))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// GatewayActiveKGFWorkspace returns the namespace of the ACTIVE workspace for
// (organization, resource id), or ErrKGFWorkspaceNotProvisioned. It never
// creates anything: a delegated workspace request for a workspace KGF has not
// provisioned (or has deleted) is refused, not provisioned lazily.
func GatewayActiveKGFWorkspace(ctx context.Context, q GWQuerier, w KGFWorkspace) (string, error) {
	n := w.normalized()
	r, err := GatewayKGFWorkspace(ctx, q, n.Organization, n.ResourceID)
	if errors.Is(err, ErrNotFound) {
		return "", ErrKGFWorkspaceNotProvisioned
	}
	if err != nil {
		return "", fmt.Errorf("read kgf workspace: %w", err)
	}
	if r.State != KGFWorkspaceActive {
		return "", ErrKGFWorkspaceNotProvisioned
	}
	if r.Subscription != n.Subscription || r.ResourceGroup != n.ResourceGroup ||
		r.Namespace != KGFWorkspaceNamespace(n.Organization, n.ResourceID) {
		// The row cannot disagree with its own key unless it was edited by
		// hand; refuse rather than route the caller somewhere unexpected.
		return "", ErrKGFNamespaceConflict
	}
	return r.Namespace, nil
}

// GatewayProvisionKGFWorkspace creates the tenant and kgf_workspaces rows for
// a workspace if absent and returns the record and whether this call created
// it. Idempotent and safe under concurrent calls, like the v1 provisioning:
// the tenant insert is ON CONFLICT DO NOTHING, the workspace row is written
// only by the transaction that created the tenant, and the stored row is read
// back and compared.
//
//   - already active for this workspace: returned, created=false;
//   - deleted: ErrKGFWorkspaceDeleted (the namespace is derived, so the only
//     way to "re-create" would be to reactivate the deleted namespace with its
//     history and credentials, which is refused);
//   - the namespace exists but not as this workspace (a v1 scope, a
//     registered tenant, or another workspace): ErrKGFNamespaceConflict.
//
// q may be a pool or a transaction (a savepoint then), so the caller can write
// the audit row in the same transaction.
func GatewayProvisionKGFWorkspace(ctx context.Context, q GWDB, w KGFWorkspace, createdBy string) (KGFWorkspaceRecord, bool, error) {
	if !w.Complete() {
		return KGFWorkspaceRecord{}, false, errors.New("kgf: organization, subscription, resource group, workspace and resource id are required")
	}
	n := w.normalized()
	ns := KGFWorkspaceNamespace(n.Organization, n.ResourceID)
	tx, err := q.Begin(ctx)
	if err != nil {
		return KGFWorkspaceRecord{}, false, fmt.Errorf("begin kgf workspace provisioning: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	name := fmt.Sprintf("KGF workspace %s / %s / %s / %s", n.Organization, n.Subscription, n.ResourceGroup, n.Workspace)
	if len(name) > 160 {
		name = name[:160]
	}
	var createdTenant bool
	err = tx.QueryRow(ctx, `INSERT INTO tenants (namespace, name) VALUES ($1, $2)
		ON CONFLICT (namespace) DO NOTHING RETURNING true`, ns, name).Scan(&createdTenant)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return KGFWorkspaceRecord{}, false, fmt.Errorf("provision kgf workspace tenant: %w", err)
	}
	if createdTenant {
		if _, err := tx.Exec(ctx, `INSERT INTO kgf_workspaces
			(namespace, organization, subscription, resource_group, workspace, resource_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			ns, n.Organization, n.Subscription, n.ResourceGroup, n.Workspace, n.ResourceID, createdBy); err != nil {
			return KGFWorkspaceRecord{}, false, fmt.Errorf("provision kgf workspace: %w", err)
		}
	}
	got, err := scanKGFWorkspace(tx.QueryRow(ctx, `SELECT `+kgfWorkspaceColumns+` FROM kgf_workspaces WHERE namespace = $1`, ns))
	if errors.Is(err, pgx.ErrNoRows) {
		// A tenant with this name exists and was not provisioned as a workspace.
		return KGFWorkspaceRecord{}, false, ErrKGFNamespaceConflict
	}
	if err != nil {
		return KGFWorkspaceRecord{}, false, fmt.Errorf("read kgf workspace: %w", err)
	}
	if got.KGFWorkspace != n {
		return KGFWorkspaceRecord{}, false, ErrKGFNamespaceConflict
	}
	if got.State == KGFWorkspaceDeleted {
		return got, false, ErrKGFWorkspaceDeleted
	}
	if err := tx.Commit(ctx); err != nil {
		return KGFWorkspaceRecord{}, false, fmt.Errorf("commit kgf workspace provisioning: %w", err)
	}
	return got, createdTenant, nil
}

// KGFWorkspaceDeletion reports what GatewayDeleteKGFWorkspace did.
type KGFWorkspaceDeletion struct {
	Namespace string
	// Existed is false when no workspace was ever provisioned for the id.
	Existed bool
	// AlreadyDeleted is true when the workspace was deleted before this call.
	AlreadyDeleted bool
	// Revoked counts the machine credentials of the namespace this call
	// revoked: API keys, scoped tokens and service accounts.
	RevokedAPIKeys, RevokedTokens, DisabledServiceAccounts int64
}

// GatewayDeleteKGFWorkspace marks a workspace deleted. Idempotent: a workspace
// already deleted, or never provisioned, is reported and left as it is.
//
// In the same transaction, every machine credential of the namespace is
// revoked -- API keys, scoped (including worker and service-account) tokens,
// and service accounts -- so nothing authenticates to a deleted workspace by
// any route: KGF contexts get workspace_not_provisioned, machine credentials
// get 401. Nothing is hard-deleted: the tenant, its specs, runs, attempts,
// logs, usage ledger and audit chain are retained under Nexus's ordinary
// retention (ADR-0034 §5).
func GatewayDeleteKGFWorkspace(ctx context.Context, q GWDB, organization, resourceID, deletedBy string) (KGFWorkspaceDeletion, error) {
	org := strings.TrimSpace(organization)
	rid := strings.ToLower(strings.TrimSpace(resourceID))
	out := KGFWorkspaceDeletion{Namespace: KGFWorkspaceNamespace(org, rid)}
	tx, err := q.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("begin kgf workspace deletion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var state, ns string
	err = tx.QueryRow(ctx, `SELECT state, namespace FROM kgf_workspaces
		WHERE organization = $1 AND resource_id = $2 FOR UPDATE`, org, rid).Scan(&state, &ns)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("read kgf workspace: %w", err)
	}
	out.Existed, out.Namespace = true, ns
	if state == KGFWorkspaceDeleted {
		out.AlreadyDeleted = true
		return out, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE kgf_workspaces SET state = 'deleted', deleted_at = now(), deleted_by = $2
		WHERE namespace = $1`, ns, deletedBy); err != nil {
		return out, fmt.Errorf("mark kgf workspace deleted: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE namespace = $1 AND revoked_at IS NULL`, ns)
	if err != nil {
		return out, fmt.Errorf("revoke workspace api keys: %w", err)
	}
	out.RevokedAPIKeys = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `UPDATE scoped_tokens SET revoked_at = now() WHERE namespace = $1 AND revoked_at IS NULL`, ns)
	if err != nil {
		return out, fmt.Errorf("revoke workspace tokens: %w", err)
	}
	out.RevokedTokens = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `UPDATE service_accounts SET disabled_at = now(), revision = revision + 1
		WHERE namespace = $1 AND disabled_at IS NULL`, ns)
	if err != nil {
		return out, fmt.Errorf("disable workspace service accounts: %w", err)
	}
	out.DisabledServiceAccounts = tag.RowsAffected()
	if err := tx.Commit(ctx); err != nil {
		return out, fmt.Errorf("commit kgf workspace deletion: %w", err)
	}
	return out, nil
}
