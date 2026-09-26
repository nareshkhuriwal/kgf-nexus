// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// Delegated identity administration (ADR-0024 §7).
//
// Once custom roles can hold users.* and service_accounts.*, the question
// "may this actor act on that account" has a second half besides "does the
// actor hold the permission": a delegate with users.password.reset must not be
// able to reset the recovery Administrator's password and take the namespace
// over. The rule: an Administrator (or Owner) may act on anyone; a delegated
// actor may act only on accounts whose effective grants are a subset of its
// own, and never on an Administrator.

var errTargetAboveActor = errors.New("target account holds permissions the actor does not")

// userEffectiveGrants computes a user's grants from the database, inside the
// caller's transaction. admin reports the Administrator system role.
func userEffectiveGrants(ctx context.Context, q store.GWQuerier, namespace, userID string) (grants map[string]bool, admin bool, err error) {
	var role string
	if err = q.QueryRow(ctx, `SELECT role FROM users WHERE id::text=$1 AND namespace=$2`, userID, namespace).Scan(&role); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, store.ErrNotFound
		}
		return nil, false, err
	}
	if role == string(authn.RoleAdmin) {
		return nil, true, nil
	}
	var raw, idp []byte
	err = q.QueryRow(ctx, `SELECT roles, idp_roles FROM rbac_user_access WHERE user_id::text=$1 AND namespace=$2`, userID, namespace).Scan(&raw, &idp)
	if errors.Is(err, pgx.ErrNoRows) {
		// Legacy reader: its authority is the legacy reader mapping.
		p := authn.Principal{Role: authn.RoleReader, Namespace: namespace}
		out := map[string]bool{}
		for _, g := range authn.Catalog() {
			if p.Allows(g) {
				out[g] = true
			}
		}
		return out, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var names, idpNames []string
	if err = json.Unmarshal(raw, &names); err != nil {
		return nil, false, err
	}
	if err = json.Unmarshal(idp, &idpNames); err != nil {
		return nil, false, err
	}
	grants, err = roleSetGrants(ctx, q, namespace, append(names, idpNames...))
	return grants, false, err
}

// roleSetGrants unions the permissions of named roles; unknown roles grant
// nothing.
func roleSetGrants(ctx context.Context, q store.GWQuerier, namespace string, names []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, n := range names {
		gs, err := authn.RoleGrants(ctx, q, namespace, n)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, g := range gs {
			out[g] = true
		}
	}
	return out, nil
}

// checkTargetAuthority enforces the target half of delegation for a user.
func checkTargetAuthority(ctx context.Context, q store.GWQuerier, p authn.Principal, namespace, userID string) error {
	if p.IsAdministrator() {
		return nil
	}
	grants, admin, err := userEffectiveGrants(ctx, q, namespace, userID)
	if err != nil {
		return err
	}
	if admin {
		return errTargetAboveActor
	}
	if _, ok := p.Subset(grants); !ok {
		return errTargetAboveActor
	}
	return nil
}

// refuseTarget writes the response for a failed target check and reports
// whether the handler should stop.
func (a *API) refuseTarget(w http.ResponseWriter, r *http.Request, p authn.Principal, action, target string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errTargetAboveActor):
		a.auditPrincipal(r, p, action, "user", target, store.AuditDenied,
			map[string]any{"reason": "target account holds permissions the actor does not"})
		writeError(w, http.StatusForbidden, "target_above_actor",
			"This account holds permissions you do not have; ask an administrator")
	default:
		a.fail(w, r, "check account authority", err)
	}
	return true
}

// targetCheck runs checkTargetAuthority outside a transaction for handlers that
// act through the IdentityAdmin interface, and reports whether to stop.
func (a *API) targetCheck(w http.ResponseWriter, r *http.Request, p authn.Principal, action, userID string) bool {
	if p.IsAdministrator() {
		return false
	}
	if a.db == nil {
		return a.refuseTarget(w, r, p, action, userID, errTargetAboveActor)
	}
	return a.refuseTarget(w, r, p, action, userID, checkTargetAuthority(r.Context(), a.db, p, p.Namespace, userID))
}

// checkDelegable verifies every permission in a set can be handed out by p:
// it is in the catalog, it is delegable, and p holds it.
func checkDelegable(p authn.Principal, grants []string) (string, bool) {
	for _, g := range grants {
		if !authn.Delegable(g) || !p.Allows(g) {
			return g, false
		}
	}
	return "", true
}
