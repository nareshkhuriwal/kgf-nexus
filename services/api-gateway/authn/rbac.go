// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"sort"
	"strings"
)

const AdministratorRole = "Administrator"

// Exact permission catalog. Wildcards and unknown grants are never accepted.
var ModuleActions = map[string][]string{
	"overview": {"view"}, "workflows": {"view", "publish", "delete", "cache.invalidate"}, "runs": {"view", "create", "cancel", "pause", "resume", "retry", "delete"},
	"tasks": {"view", "logs.view", "results.view"}, "schedules": {"view", "create", "update", "delete"}, "backfills": {"view", "create", "update"},
	"workers": {"view", "drain", "quarantine"}, "infrastructure": {"view", "configure"}, "users": {"view", "manage"}, "tokens": {"view", "manage"}, "audit": {"view"}, "roles": {"view", "manage"},
	// ADR-0024 §6: non-human identities for agents and CI.
	"service_accounts": {"view", "manage"},
	// ADR-0021. quotas.view is broad (it explains a quota_exceeded pending
	// reason); usage.view is cost data and is granted deliberately.
	"quotas": {"view", "manage"}, "usage": {"view"},
}

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
	return map[string][]string{AdministratorRole: Catalog(), "Viewer": viewer, "Workflow Operator": operator, "Workflow Developer": append(append([]string{}, operator...), "workflows.publish", "schedules.delete", "workflows.cache.invalidate"), "Infrastructure Operator": {"workers.view", "workers.drain", "workers.quarantine", "infrastructure.view", "infrastructure.configure", "quotas.view", "quotas.manage", "usage.view"}, "Auditor": {"audit.view", "users.view", "roles.view", "tokens.view", "service_accounts.view", "quotas.view", "usage.view"}}
}
func (a *Authenticator) WithRBAC() *Authenticator { a.rbac = true; return a }
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
	var raw []byte
	err := a.db.QueryRow(ctx, `SELECT roles FROM rbac_user_access WHERE user_id=$1 AND namespace=$2`, p.UserID, p.Namespace).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal(raw, &p.RoleNames); err != nil {
		return p, err
	}
	p.Grants = map[string]bool{}
	for _, name := range p.RoleNames {
		if name == AdministratorRole {
			return p, ErrForbidden
		}
		grants, ok := Builtins()[name]
		if !ok {
			var encoded []byte
			err = a.db.QueryRow(ctx, `SELECT permissions FROM rbac_roles WHERE namespace=$1 AND name=$2`, p.Namespace, name).Scan(&encoded)
			if err != nil {
				return p, err
			}
			if err = json.Unmarshal(encoded, &grants); err != nil {
				return p, err
			}
		}
		for _, g := range grants {
			p.Grants[g] = true
		}
	}
	return p, nil
}
func (p Principal) Allows(permission string) bool {
	if !Known(permission) {
		return false
	}
	if p.Grants != nil {
		return p.Grants[permission]
	}
	return legacyAllows(p, permission)
}

// legacyAllows is the unbound (role + scopes) mapping onto the catalog.
func legacyAllows(p Principal, permission string) bool {
	// Legacy compatibility is explicit until the account receives an assignment.
	if p.Role == RoleWorker {
		return false
	}
	if strings.HasSuffix(permission, ".view") {
		module := strings.Split(permission, ".")[0]
		if module == "users" || module == "tokens" || module == "audit" || module == "infrastructure" || module == "usage" || module == "service_accounts" {
			return p.Role == RoleAdmin && p.HasScope(ActionAdminWrite)
		}

		return p.HasScope(ActionRead) && (p.Role == RoleAdmin || (p.Role == RoleReader && !strings.HasPrefix(permission, "users.") && !strings.HasPrefix(permission, "tokens.") && !strings.HasPrefix(permission, "audit.") && !strings.HasPrefix(permission, "roles.") && !strings.HasPrefix(permission, "infrastructure.") && !strings.HasPrefix(permission, "usage.") && !strings.HasPrefix(permission, "service_accounts.")))
	}
	action := ActionWrite
	if strings.HasSuffix(permission, ".manage") || permission == "workers.drain" || permission == "workers.quarantine" || permission == "infrastructure.configure" || permission == "runs.delete" || permission == "workflows.delete" {
		action = ActionAdminWrite
	}
	return p.Role == RoleAdmin && p.HasScope(action)
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
		"overview.view":    {"runs.view", "workers.view"},
		"tasks.view":       {"runs.view"},
		"runs.create":      {"workflows.view"},
		"schedules.create": {"workflows.view", "runs.create", "runs.view"},
		"backfills.create": {"runs.create", "runs.view", "schedules.view"},
	}
	for grant, needs := range required {
		if grants[grant] {
			for _, need := range needs {
				if !grants[need] {
					return fmt.Errorf("%s requires %s", grant, need)
				}
			}
		}
	}
	return nil
}
