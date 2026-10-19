// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package rest

// KGF.Nexus workspace provisioning (ADR-0034). The KGF.Nexus provider front
// provisions and deletes a workspace's namespace with its OWN service
// context: iss in NEXUS_KGF_FRONT_ISSUERS, role service, no obo, and `res`
// naming the workspace. The resource id always comes from that signed claim,
// never from a body or a path.

import (
	"errors"
	"net/http"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// The provisioning routes: the only routes a provisioner context may reach
// (guard), and the only callers these routes serve (requireProvisioner).
const (
	kgfWorkspacePutRoute    = "PUT /v1/kgf/workspace"
	kgfWorkspaceDeleteRoute = "DELETE /v1/kgf/workspace"
)

var kgfProvisioningRoutes = map[string]bool{kgfWorkspacePutRoute: true, kgfWorkspaceDeleteRoute: true}

// registerKGFWorkspaceRoutes adds the provisioning routes. Registered in every
// mode: outside kgf/both no principal can be a provisioner, so they answer
// 403 kgf_provisioner_required to every Nexus credential.
func (a *API) registerKGFWorkspaceRoutes(mux *http.ServeMux) {
	mux.Handle(kgfWorkspacePutRoute, a.guard(authn.ActionAdminWrite, a.provisionKGFWorkspace))
	mux.Handle(kgfWorkspaceDeleteRoute, a.guard(authn.ActionAdminWrite, a.deleteKGFWorkspace))
}

type kgfWorkspaceResponse struct {
	Namespace  string `json:"namespace,omitempty"`
	ResourceID string `json:"resource_id,omitempty"`
	State      string `json:"state"`
}

func requireProvisioner(w http.ResponseWriter, p authn.Principal) bool {
	if p.KGF == nil || !p.KGF.Provisioner || p.KGF.ResourceID == "" {
		writeError(w, http.StatusForbidden, "kgf_provisioner_required",
			"only a KGF provider front's own service context naming a workspace may provision or delete one")
		return false
	}
	return true
}

// principalWorkspace is the workspace a provisioner principal's verified
// context names.
func principalWorkspace(p authn.Principal) store.KGFWorkspace {
	return store.KGFWorkspace{Organization: p.KGF.Organization, Subscription: p.KGF.Subscription,
		ResourceGroup: p.KGF.ResourceGroup, Workspace: p.KGF.Workspace, ResourceID: p.KGF.ResourceID}
}

// provisionKGFWorkspace is PUT /v1/kgf/workspace: idempotent. 201 when this
// call created the namespace, 200 when it was already active; 409
// workspace_deleted for a deleted workspace (its derived namespace would be
// reactivated with its history, so it is refused); 409 namespace_conflict
// when the derived namespace belongs to something else. The row and its
// audit record commit together (a security grant, ADR-0025).
func (a *API) provisionKGFWorkspace(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !requireProvisioner(w, p) {
		return
	}
	ws := principalWorkspace(p)
	ctx := r.Context()
	tx, err := a.db.Begin(ctx)
	if err != nil {
		a.log.ErrorContext(ctx, "kgf workspace provisioning: begin", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "workspace provisioning is temporarily unavailable")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rec, created, err := store.GatewayProvisionKGFWorkspace(ctx, tx, ws, p.KGF.Principal)
	switch {
	case errors.Is(err, store.ErrKGFWorkspaceDeleted):
		_ = tx.Rollback(ctx)
		a.auditKGFWorkspace(r, p, rec.Namespace, "kgf.workspace.provision", store.AuditDenied,
			map[string]any{"reason": "workspace_deleted"})
		writeError(w, http.StatusConflict, "workspace_deleted",
			"this workspace was deleted; a deleted workspace's resource id cannot be provisioned again")
		return
	case errors.Is(err, store.ErrKGFNamespaceConflict):
		a.log.WarnContext(ctx, "kgf workspace namespace conflict", "namespace", p.Namespace, "resource_id", ws.ResourceID)
		writeError(w, http.StatusConflict, "namespace_conflict",
			"the namespace derived for this workspace already exists and was not provisioned for it")
		return
	case err != nil:
		a.log.ErrorContext(ctx, "kgf workspace provisioning failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "workspace provisioning is temporarily unavailable")
		return
	}
	e := principalEntry(r, p, "kgf.workspace.provision", "namespace", rec.Namespace, store.AuditSuccess,
		map[string]any{"resource_id": rec.ResourceID, "created": created})
	e.Namespace = rec.Namespace
	if err := store.GatewayAppendAudit(ctx, tx, e); err != nil {
		countAuditFailure(AuditGrant, true)
		a.log.ErrorContext(ctx, "audit write failed; the workspace provisioning was rolled back", "error", err)
		writeError(w, http.StatusServiceUnavailable, "audit_unavailable",
			"the change was not made because its audit record could not be written; retry shortly")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		a.log.ErrorContext(ctx, "kgf workspace provisioning: commit", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "workspace provisioning is temporarily unavailable")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, kgfWorkspaceResponse{Namespace: rec.Namespace, ResourceID: rec.ResourceID, State: store.KGFWorkspaceActive})
}

// deleteKGFWorkspace is DELETE /v1/kgf/workspace: idempotent, always 200
// {"state":"deleted"} -- also for a workspace already deleted or never
// provisioned. It marks the workspace deleted and revokes the namespace's
// machine credentials in one transaction; nothing is hard-deleted. The audit
// row is a revocation: written after the change, never blocking it.
func (a *API) deleteKGFWorkspace(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !requireProvisioner(w, p) {
		return
	}
	ws := principalWorkspace(p)
	res, err := store.GatewayDeleteKGFWorkspace(r.Context(), a.db, ws.Organization, ws.ResourceID, p.KGF.Principal)
	if err != nil {
		a.log.ErrorContext(r.Context(), "kgf workspace deletion failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "workspace deletion is temporarily unavailable")
		return
	}
	action := "kgf.workspace.delete"
	if res.Existed {
		// A workspace never provisioned has no namespace (no tenant) to hold
		// an audit chain; that no-op is logged instead.
		a.auditKGFWorkspace(r, p, res.Namespace, action, store.AuditSuccess, map[string]any{
			"resource_id": p.KGF.ResourceID, "already_deleted": res.AlreadyDeleted,
			"revoked_api_keys": res.RevokedAPIKeys, "revoked_tokens": res.RevokedTokens,
			"disabled_service_accounts": res.DisabledServiceAccounts,
		})
	} else {
		a.log.InfoContext(r.Context(), "kgf workspace delete for a workspace never provisioned",
			"resource_id", p.KGF.ResourceID, "principal", p.KGF.Principal, "request_id", p.KGF.RequestID)
	}
	writeJSON(w, http.StatusOK, kgfWorkspaceResponse{State: store.KGFWorkspaceDeleted})
}

// auditKGFWorkspace writes a workspace audit row outside the change's
// transaction (revocations and denials, which are never blocked). A failure
// is logged as a lost record and counted, like auditAttempt.
func (a *API) auditKGFWorkspace(r *http.Request, p authn.Principal, namespace, action, outcome string, detail map[string]any) {
	if namespace == "" {
		return
	}
	e := principalEntry(r, p, action, "namespace", namespace, outcome, detail)
	e.Namespace = namespace
	if err := store.GatewayAppendAudit(r.Context(), a.db, e); err != nil {
		countAuditFailure(ClassOf(action), false)
		a.log.ErrorContext(r.Context(), "AUDIT RECORD LOST: could not write audit record",
			"action", action, "outcome", outcome, "target_kind", "namespace", "target_id", namespace, "error", err)
	}
}
