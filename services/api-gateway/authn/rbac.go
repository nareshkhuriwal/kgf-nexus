// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

const AdministratorRole = "Administrator"

// OwnerRole is the name recorded for a session acting in a namespace through a
// cross-namespace owner grant (ADR-0024 §9). It is not assignable: it exists
// only as the explanation on such a principal.
const OwnerRole = "Owner"

// ModuleActions is the exact permission catalog. Wildcards and unknown grants
// are never accepted.
//
// The identity modules were split from coarse `manage` grants into the actions
// each route performs (ADR-0024 §7). The old identifiers were never storable in
// a custom role, so no stored role changes meaning.
var ModuleActions = map[string][]string{
	"overview":  {"view"},
	"workflows": {"view", "publish", "delete", "cache.invalidate"},
	"runs":      {"view", "create", "cancel", "pause", "resume", "retry", "delete"},
	"tasks":     {"view", "logs.view", "results.view"},
	"schedules": {"view", "create", "update", "delete"},
	"backfills": {"view", "create", "update"},
	"workers":   {"view", "drain", "quarantine"},
	// Platform scheduling/pool configuration. Cluster registration has its own
	// module since 0032.
	"infrastructure":   {"view", "configure"},
	"clusters":         {"view", "configure"},
	"users":            {"view", "create", "update", "enable", "disable", "sessions.revoke", "password.reset", "roles.assign"},
	"roles":            {"view", "create", "update", "delete"},
	"tokens":           {"view", "create", "revoke"},
	"service_accounts": {"view", "create", "update", "disable", "roles.assign"},
	"sso":              {"view", "configure", "mappings.update"},
	"owners":           {"view", "grant"},
	"audit":            {"view"},
	// ADR-0021. quotas.view is broad (it explains a quota_exceeded pending
	// reason); usage.view is cost data and is granted deliberately. The
	// namespace budget IS the quota, so there is no separate budgets module.
	"quotas": {"view", "manage"}, "usage": {"view"},
}

// nonDelegable permissions can never be placed in a custom role, assigned to a
// service account, or put in a token ceiling. Granting Owner of a namespace is
// the recovery path of last resort and stays with Administrators and Owners.
var nonDelegable = map[string]bool{"owners.grant": true}

// Delegable reports whether a permission may appear in a custom role, a
// service-account binding or a token ceiling.
func Delegable(permission string) bool { return !nonDelegable[permission] && Known(permission) }

// adminModules are the modules whose every permission, including view, is
// administrator-level in the legacy (unbound) mapping.
var adminModules = map[string]bool{
	"users": true, "tokens": true, "audit": true, "infrastructure": true, "usage": true,
	"roles": true, "clusters": true, "service_accounts": true, "sso": true, "owners": true,
}

var catalogSet = func() map[string]bool {
	out := map[string]bool{}
	for _, g := range Catalog() {
		out[g] = true
	}
	return out
}()

// Known reports whether a permission identifier is in the catalog.
func Known(permission string) bool { return catalogSet[permission] }

func Catalog() []string {
	var out []string
	for m, actions := range ModuleActions {
		for _, a := range actions {
			out = append(out, m+"."+a)
		}
	}
	sort.Strings(out)
	return out
}

func Builtins() map[string][]string {
	viewer := []string{"overview.view", "workflows.view", "runs.view", "tasks.view", "schedules.view", "backfills.view", "workers.view", "quotas.view"}
	operator := append(append([]string{}, viewer...), "runs.create", "runs.cancel", "runs.pause", "runs.resume", "runs.retry", "tasks.logs.view", "tasks.results.view", "schedules.create", "schedules.update", "backfills.create", "backfills.update")
	return map[string][]string{
		AdministratorRole:    Catalog(),
		"Viewer":             viewer,
		"Workflow Operator":  operator,
		"Workflow Developer": append(append([]string{}, operator...), "workflows.publish", "schedules.delete", "workflows.cache.invalidate"),
		"Infrastructure Operator": {"workers.view", "workers.drain", "workers.quarantine", "infrastructure.view", "infrastructure.configure",
			"clusters.view", "clusters.configure", "quotas.view", "quotas.manage", "usage.view"},
		"Auditor": {"audit.view", "users.view", "roles.view", "tokens.view", "service_accounts.view", "sso.view", "owners.view", "quotas.view", "usage.view"},
	}
}

func (a *Authenticator) WithRBAC() *Authenticator { a.rbac = true; return a }

// roleGrants resolves one role name to its permissions within a namespace.
func roleGrants(ctx context.Context, q store.GWQuerier, namespace, name string) ([]string, error) {
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

// RoleGrants is roleGrants for callers outside the package.
func RoleGrants(ctx context.Context, q store.GWQuerier, namespace, name string) ([]string, error) {
	return roleGrants(ctx, q, namespace, name)
}

// grantsFor unions the permissions of a set of roles. A role that no longer
// exists (deleted after an IdP mapping named it) fails closed: it grants
// nothing rather than failing the whole principal, because an IdP-sourced role
// name is data an administrator can edit independently.
func grantsFor(ctx context.Context, q store.GWQuerier, namespace string, names []string, tolerateMissing bool) (map[string]bool, error) {
	out := map[string]bool{}
	for _, name := range names {
		grants, err := roleGrants(ctx, q, namespace, name)
		if tolerateMissing && errors.Is(err, pgx.ErrNoRows) {
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

func (a *Authenticator) resolveRBAC(ctx context.Context, p Principal) (Principal, error) {
	if !a.rbac || p.UserID == "" {
		return p, nil
	}
	// The first administrator created during setup (and subsequent admin accounts)
	// belongs to the system role by definition, never by an editable role binding.
	if p.Role == RoleAdmin {
		p.RoleNames = []string{AdministratorRole}
		return p, nil
	}
	var raw, idpRaw []byte
	err := a.db.QueryRow(ctx, `SELECT roles, idp_roles FROM rbac_user_access WHERE user_id=$1 AND namespace=$2`, p.UserID, p.Namespace).Scan(&raw, &idpRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	var assigned, idp []string
	if err = json.Unmarshal(raw, &assigned); err != nil {
		return p, err
	}
	if err = json.Unmarshal(idpRaw, &idp); err != nil {
		return p, err
	}
	for _, name := range assigned {
		if name == AdministratorRole {
			return p, ErrForbidden
		}
	}
	p.RoleNames = unionStrings(assigned, idp)
	grants, err := grantsFor(ctx, a.db, p.Namespace, assigned, false)
	if err != nil {
		return p, err
	}
	idpGrants, err := grantsFor(ctx, a.db, p.Namespace, idp, true)
	if err != nil {
		return p, err
	}
	for g := range idpGrants {
		grants[g] = true
	}
	p.Grants = grants
	return p, nil
}

func unionStrings(lists ...[]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range lists {
		for _, s := range l {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (p Principal) Allows(permission string) bool {
	if !Known(permission) {
		return false
	}
	if p.Grants != nil {
		return p.Grants[permission]
	}
	// Legacy compatibility is explicit until the account receives an assignment.
	return legacyAllows(p, permission)
}

// legacyAllows is the unbound (role + scopes) mapping onto the catalog.
func legacyAllows(p Principal, permission string) bool {
	if p.Role == RoleWorker {
		return false
	}
	module := strings.SplitN(permission, ".", 2)[0]
	if strings.HasSuffix(permission, ".view") {
		if adminModules[module] {
			return p.Role == RoleAdmin && p.HasScope(ActionAdminWrite)
		}
		return p.HasScope(ActionRead) && (p.Role == RoleAdmin || p.Role == RoleReader)
	}
	if nonDelegable[permission] {
		// Only a human administrator (or an owner acting as one) may grant
		// ownership; never a machine credential, however broad its scopes.
		return p.Role == RoleAdmin && p.HasScope(ActionAdminWrite) && p.Kind == KindSession
	}
	action := ActionWrite
	if adminModules[module] || strings.HasSuffix(permission, ".manage") || permission == "workers.drain" || permission == "workers.quarantine" || permission == "runs.delete" || permission == "workflows.delete" {
		action = ActionAdminWrite
	}
	return p.Role == RoleAdmin && p.HasScope(action)
}

// IsAdministrator reports whether a principal holds the Administrator system
// role (an admin account, or an owner acting in a namespace). Bound principals
// never do, whatever their grants.
func (p Principal) IsAdministrator() bool {
	return p.Grants == nil && p.Role == RoleAdmin && (p.Kind == KindSession || p.Kind == "" || p.Kind == KindAPIKey)
}

// Subset reports whether every grant in want is allowed to p.
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

// Action permissions require the corresponding module visibility grant.
func ValidateGrants(grants map[string]bool) error {
	for g := range grants {
		module := strings.Split(g, ".")[0]
		if !grants[module+".view"] {
			return fmt.Errorf("%s requires %s.view", g, module)
		}
	}
	required := map[string][]string{
		"overview.view":                 {"runs.view", "workers.view"},
		"tasks.view":                    {"runs.view"},
		"runs.create":                   {"workflows.view"},
		"schedules.create":              {"workflows.view", "runs.create", "runs.view"},
		"backfills.create":              {"runs.create", "runs.view", "schedules.view"},
		"users.roles.assign":            {"roles.view"},
		"service_accounts.roles.assign": {"roles.view"},
		"sso.mappings.update":           {"roles.view"},
	}
	keys := make([]string, 0, len(required))
	for k := range required {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, grant := range keys {
		if grants[grant] {
			for _, need := range required[grant] {
				if !grants[need] {
					return fmt.Errorf("%s requires %s", grant, need)
				}
			}
		}
	}
	return nil
}
