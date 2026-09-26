// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package rest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// Cross-namespace owners (ADR-0024 §9).
//
// A user of namespace A may be granted Owner of namespace B by B's
// Administrator (or an existing Owner of B). The grant is one row per target
// namespace: there is no wildcard and no global owner. While acting in B the
// session has Administrator authority there, is recorded as role Owner, and is
// attributed to the real person in B's audit trail.

func (a *API) registerOwnerRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/namespaces/{ns}/owners", a.guard(authn.ActionRead, a.listOwners))
	mux.Handle("POST /v1/namespaces/{ns}/owners", a.guard(authn.ActionAdminWrite, a.grantOwner))
	mux.Handle("DELETE /v1/namespaces/{ns}/owners/{user}", a.guard(authn.ActionAdminWrite, a.revokeOwner))
	mux.Handle("GET /v1/auth/memberships", a.guard(authn.ActionRead, a.memberships))
}

// ownerPrincipal returns the principal a session acts as in ns through an
// active owner grant. ok is false when there is none.
func (a *API) ownerPrincipal(ctx context.Context, p authn.Principal, ns string) (authn.Principal, bool, error) {
	var exists bool
	err := a.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM namespace_owner_grants g JOIN tenants t ON t.namespace=g.namespace
		 WHERE g.namespace=$1 AND g.user_id::text=$2 AND g.revoked_at IS NULL)`, ns, p.UserID).Scan(&exists)
	if err != nil || !exists {
		return p, false, err
	}
	return authn.Principal{
		KeyID: p.KeyID, Namespace: ns, Name: p.Name, Role: authn.RoleAdmin, Kind: authn.KindSession,
		UserID: p.UserID, HomeNamespace: p.Namespace, RoleNames: []string{authn.OwnerRole},
		SSOProviderID: p.SSOProviderID,
	}, true, nil
}

type ownerRow struct {
	UserID        string    `json:"user_id"`
	Email         string    `json:"email"`
	DisplayName   string    `json:"display_name"`
	HomeNamespace string    `json:"home_namespace"`
	GrantedBy     *string   `json:"granted_by"`
	CreatedAt     time.Time `json:"created_at"`
}

func (a *API) listOwners(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	rows, err := a.db.Query(r.Context(), `SELECT g.user_id::text, u.email, u.display_name, u.namespace, g.granted_by::text, g.created_at
		  FROM namespace_owner_grants g JOIN users u ON u.id=g.user_id
		 WHERE g.namespace=$1 AND g.revoked_at IS NULL ORDER BY g.created_at`, p.Namespace)
	if err != nil {
		a.fail(w, r, "list owners", err)
		return
	}
	defer rows.Close()
	out := []ownerRow{}
	for rows.Next() {
		var o ownerRow
		if err := rows.Scan(&o.UserID, &o.Email, &o.DisplayName, &o.HomeNamespace, &o.GrantedBy, &o.CreatedAt); err != nil {
			a.fail(w, r, "list owners", err)
			return
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "list owners", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"owners": out})
}

// grantOwner makes a user (usually of another namespace) Owner of this one.
// The grantee is named by user ID AND email: the ID is the person's own
// (visible in their whoami), and requiring the matching email means this
// endpoint cannot be used to probe which accounts exist elsewhere.
func (a *API) grantOwner(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !p.IsAdministrator() || p.Kind != authn.KindSession {
		writeError(w, http.StatusForbidden, "forbidden", "Granting ownership requires an administrator or owner session")
		return
	}
	var b struct {
		UserID string `json:"user_id"`
		Email  string `json:"email"`
	}
	if decodeRBACBody(w, r, &b) != nil || b.UserID == "" || b.Email == "" {
		writeError(w, http.StatusBadRequest, "invalid", "Provide the user_id and email of the account to make an owner")
		return
	}
	if b.UserID == p.UserID {
		writeError(w, http.StatusConflict, "self_grant", "You cannot grant ownership to yourself")
		return
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		a.fail(w, r, "grant owner", err)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, p.Namespace); err != nil {
		a.fail(w, r, "owner lock", err)
		return
	}
	var home string
	err = tx.QueryRow(r.Context(), `SELECT namespace FROM users WHERE id::text=$1 AND lower(email)=$2 AND disabled_at IS NULL`,
		b.UserID, strings.ToLower(strings.TrimSpace(b.Email))).Scan(&home)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No enabled account has that user ID and email")
		return
	}
	if err != nil {
		a.fail(w, r, "grant owner", err)
		return
	}
	if home == p.Namespace {
		writeError(w, http.StatusConflict, "same_namespace", "That account already belongs to this namespace; assign it a role instead")
		return
	}
	var granter *string
	if p.UserID != "" {
		granter = &p.UserID
	}
	tag, err := tx.Exec(r.Context(), `INSERT INTO namespace_owner_grants (namespace, user_id, granted_by) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
		p.Namespace, b.UserID, granter)
	if err != nil {
		a.fail(w, r, "grant owner", err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusConflict, "already_owner", "That account is already an owner of this namespace")
		return
	}
	if err = rbacAudit(r, tx, p, "owner.grant", b.UserID, map[string]any{"email": b.Email, "home_namespace": home}); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		a.fail(w, r, "grant owner", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user_id": b.UserID, "namespace": p.Namespace, "home_namespace": home})
}

func (a *API) revokeOwner(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !p.IsAdministrator() || p.Kind != authn.KindSession {
		writeError(w, http.StatusForbidden, "forbidden", "Revoking ownership requires an administrator or owner session")
		return
	}
	user := r.PathValue("user")
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		a.fail(w, r, "revoke owner", err)
		return
	}
	defer tx.Rollback(r.Context())
	tag, err := tx.Exec(r.Context(), `UPDATE namespace_owner_grants SET revoked_at=now() WHERE namespace=$1 AND user_id::text=$2 AND revoked_at IS NULL`, p.Namespace, user)
	if err != nil {
		a.fail(w, r, "revoke owner", err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "not_found", "That account is not an owner of this namespace")
		return
	}
	if err = rbacAudit(r, tx, p, "owner.revoke", user); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		a.fail(w, r, "revoke owner", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user_id": user, "revoked": true})
}

// memberships lists the namespaces the caller may act in: its own, plus every
// namespace it owns through a grant. The console's namespace picker reads it.
func (a *API) memberships(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	home := p.Namespace
	if p.HomeNamespace != "" {
		home = p.HomeNamespace
	}
	out := []map[string]any{{"namespace": home, "relation": "member"}}
	if p.Kind == authn.KindSession && p.UserID != "" {
		rows, err := a.db.Query(r.Context(), `SELECT g.namespace, t.name FROM namespace_owner_grants g JOIN tenants t ON t.namespace=g.namespace
			 WHERE g.user_id::text=$1 AND g.revoked_at IS NULL ORDER BY t.name`, p.UserID)
		if err != nil {
			a.fail(w, r, "memberships", err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var ns, name string
			if err := rows.Scan(&ns, &name); err != nil {
				a.fail(w, r, "memberships", err)
				return
			}
			out = append(out, map[string]any{"namespace": ns, "name": name, "relation": "owner"})
		}
		if err := rows.Err(); err != nil {
			a.fail(w, r, "memberships", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"memberships": out})
}
