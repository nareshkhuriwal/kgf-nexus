// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

var catalogSet = func() map[string]bool {
	out := map[string]bool{}
	for _, g := range Catalog() {
		out[g] = true
	}
	return out
}()

// Known reports whether a permission identifier is in the catalog.
func Known(permission string) bool { return catalogSet[permission] }

// reservedIdentity are the grants that change who can do what. They stay
// with Nexus administrators (ADR-0012) and, in KGF mode, with the KGF role
// (ADR-0024 §4); they can never be placed in a service-account binding, a
// token ceiling or a custom role.
var reservedIdentity = map[string]bool{
	"users.manage": true, "roles.manage": true, "tokens.manage": true, "service_accounts.manage": true,
}

// Delegable reports whether a permission may be handed to a service account,
// a token ceiling or a custom role.
func Delegable(permission string) bool { return Known(permission) && !reservedIdentity[permission] }

// Reserved reports whether a permission is an identity-administration grant.
func Reserved(permission string) bool { return reservedIdentity[permission] }

// RoleGrants resolves one role name to its permissions within a namespace.
// Administrator is never resolvable this way: it is a system role, not a
// binding.
func RoleGrants(ctx context.Context, q store.GWQuerier, namespace, name string) ([]string, error) {
	if name == AdministratorRole {
		return nil, ErrForbidden
	}
	if grants, ok := Builtins()[name]; ok {
		return grants, nil
	}
	var encoded []byte
	if err := q.QueryRow(ctx, `SELECT permissions FROM rbac_roles WHERE namespace=$1 AND name=$2`, namespace, name).Scan(&encoded); err != nil {
		return nil, fmt.Errorf("role %q: %w", name, err)
	}
	var grants []string
	if err := json.Unmarshal(encoded, &grants); err != nil {
		return nil, err
	}
	return grants, nil
}

// RoleSetGrants unions the permissions of named roles. A role that no longer
// exists grants nothing (it fails closed rather than failing the principal).
func RoleSetGrants(ctx context.Context, q store.GWQuerier, namespace string, names []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, name := range names {
		grants, err := RoleGrants(ctx, q, namespace, name)
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrForbidden) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, g := range grants {
			if Known(g) {
				out[g] = true
			}
		}
	}
	return out, nil
}

// Subset reports whether every grant in want is allowed to p, naming the
// first that is not.
func (p Principal) Subset(want map[string]bool) (string, bool) {
	keys := make([]string, 0, len(want))
	for g, ok := range want {
		if ok {
			keys = append(keys, g)
		}
	}
	sort.Strings(keys)
	for _, g := range keys {
		if !p.Allows(g) {
			return g, false
		}
	}
	return "", true
}

// applyTokenCeiling turns a service-account token or a ceilinged token into a
// grant-bearing principal (ADR-0024 §6).
//
//   - Service-account token: grants = (the account's CURRENT roles) ∩ ceiling,
//     read on every request, so narrowing the account narrows every token at
//     once; disabling it is handled by the resolve query.
//   - Legacy token with a ceiling: grants = (legacy role/scope allowances) ∩
//     ceiling.
//
// The ceiling only removes permissions, never holds a reserved identity grant,
// and a principal with Grants set can never speak the worker protocol.
func (a *Authenticator) applyTokenCeiling(ctx context.Context, p Principal, rt store.ResolvedToken) (Principal, error) {
	if rt.ServiceAccountID == "" && rt.Permissions == nil {
		return p, nil
	}
	var base map[string]bool
	if rt.ServiceAccountID != "" {
		p.ServiceAccountID = rt.ServiceAccountID
		if a.db == nil {
			return Principal{}, ErrUnauthenticated
		}
		var raw []byte
		err := a.db.QueryRow(ctx, `SELECT roles FROM service_account_roles WHERE service_account_id=$1 AND namespace=$2`, rt.ServiceAccountID, rt.Namespace).Scan(&raw)
		var names []string
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return Principal{}, fmt.Errorf("resolve service account roles: %w", err)
		default:
			if err := json.Unmarshal(raw, &names); err != nil {
				return Principal{}, fmt.Errorf("decode service account roles: %w", err)
			}
		}
		p.RoleNames = names
		if base, err = RoleSetGrants(ctx, a.db, rt.Namespace, names); err != nil {
			return Principal{}, err
		}
	} else {
		base = map[string]bool{}
		for _, g := range Catalog() {
			if legacyAllows(p, g) {
				base[g] = true
			}
		}
	}
	grants := map[string]bool{}
	for g := range base {
		if !Delegable(g) {
			continue
		}
		if rt.Permissions == nil {
			grants[g] = true
		}
	}
	for _, g := range rt.Permissions {
		if base[g] && Delegable(g) {
			grants[g] = true
		}
	}
	p.Grants = grants
	return p, nil
}
