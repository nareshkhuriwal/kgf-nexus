// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package rest

import (
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func effectivePermissions(p authn.Principal) []string {
	out := []string{}
	for _, g := range authn.Catalog() {
		if p.Allows(g) {
			out = append(out, g)
		}
	}
	sort.Strings(out)
	return out
}
func (a *API) registerRBACRoutes(m *http.ServeMux) {
	for _, x := range []struct {
		route, permission string
		handler           func(http.ResponseWriter, *http.Request, authn.Principal)
	}{
		{"GET /v1/namespaces/{ns}/roles", "roles.view", a.listRoles},
		{"PUT /v1/namespaces/{ns}/roles/{name}", "roles.manage", a.putRole},
		{"DELETE /v1/namespaces/{ns}/roles/{name}", "roles.manage", a.deleteRole},
		{"GET /v1/namespaces/{ns}/users/{id}/roles", "users.view", a.userRoles},
		{"PUT /v1/namespaces/{ns}/users/{id}/roles", "roles.manage", a.userRoles},
	} {
		// Permissions are declared statically; concurrent router creation is read-only.
		m.Handle(x.route, a.guard(authn.ActionRead, x.handler))
	}
}
func (a *API) listRoles(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	out := []map[string]any{}
	for name, grants := range authn.Builtins() {
		out = append(out, map[string]any{"name": name, "permissions": grants, "builtin": true, "revision": 0, "immutable": true, "assignable": name != authn.AdministratorRole})
	}
	rows, err := a.db.Query(r.Context(), `SELECT name,permissions,revision FROM rbac_roles WHERE namespace=$1 ORDER BY name`, p.Namespace)
	if err != nil {
		a.fail(w, r, "roles", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var grants json.RawMessage
		var rev int64
		if err = rows.Scan(&name, &grants, &rev); err != nil {
			a.fail(w, r, "roles", err)
			return
		}
		out = append(out, map[string]any{"name": name, "permissions": grants, "revision": rev, "builtin": false})
	}
	if err = rows.Err(); err != nil {
		a.fail(w, r, "roles", err)
		return
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i]["name"].(string), out[j]["name"].(string)
		if a == authn.AdministratorRole {
			return b != a
		}
		if b == authn.AdministratorRole {
			return false
		}
		return a < b
	})
	writeJSON(w, 200, map[string]any{"roles": out, "catalog": authn.ModuleActions})
}
func (a *API) putRole(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	var b struct {
		Permissions []string `json:"permissions"`
		Revision    int64    `json:"revision"`
	}
	if err := decodeRBACBody(w, r, &b); err != nil {
		rejectBody(w, err, http.StatusBadRequest, "invalid", "Invalid role")
		return
	}
	if !revisionOK(w, b.Revision) {
		return
	}
	name := r.PathValue("name")
	if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 _-]{1,63}$`).MatchString(name) {
		writeError(w, 400, "invalid", "Use a role name of 2–64 letters, numbers, spaces or dashes")
		return
	}
	if isProtectedRole(name) {
		writeError(w, 409, "builtin", "Built-in roles cannot be edited")
		return
	}
	allowed := map[string]bool{}
	for _, g := range authn.Catalog() {
		allowed[g] = true
	}
	for _, g := range b.Permissions {
		if !allowed[g] || !p.Allows(g) || authn.Reserved(g) {
			writeError(w, 403, "invalid_grant", "Cannot delegate permission: "+g)
			return
		}
	}
	grants := map[string]bool{}
	for _, g := range b.Permissions {
		grants[g] = true
	}
	if err := authn.ValidateGrants(grants); err != nil {
		writeError(w, 400, "invalid_grants", err.Error())
		return
	}
	raw, _ := json.Marshal(b.Permissions)
	if b.Permissions == nil {
		raw = []byte("[]")
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		a.fail(w, r, "role", err)
		return
	}
	defer tx.Rollback(r.Context())
	// Serialize role writes and assignments within a namespace.
	_, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, p.Namespace)
	if err != nil {
		a.fail(w, r, "role lock", err)
		return
	}
	tag, err := tx.Exec(r.Context(), `INSERT INTO rbac_roles(namespace,name,permissions,revision) SELECT $1,$2,$3,1 WHERE $4::bigint=0 ON CONFLICT DO NOTHING`, p.Namespace, name, raw, b.Revision)
	if err == nil && b.Revision > 0 {
		tag, err = tx.Exec(r.Context(), `UPDATE rbac_roles SET permissions=$3,revision=revision+1 WHERE namespace=$1 AND name=$2 AND revision=$4`, p.Namespace, name, raw, b.Revision)
	}
	if err != nil {
		a.fail(w, r, "save role", err)
		return
	}
	if tag.RowsAffected() != 1 {
		writeError(w, 409, "conflict", "Role changed; reload before saving")
		return
	}
	if err = rbacAudit(r, tx, p, "role.save", name, map[string]any{"permissions": b.Permissions, "previous_revision": b.Revision}); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		if !a.auditUnavailable(w, r, err) {
			a.fail(w, r, "save role", err)
		}
		return
	}
	writeJSON(w, 200, map[string]any{"saved": true})
}
func (a *API) deleteRole(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	name := r.PathValue("name")
	if isProtectedRole(name) {
		writeError(w, 409, "builtin", "Built-in roles cannot be deleted")
		return
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		a.fail(w, r, "delete role", err)
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, p.Namespace)
	if err != nil {
		a.fail(w, r, "role lock", err)
		return
	}
	var bound bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM rbac_user_access WHERE namespace=$1 AND roles ? $2)`, p.Namespace, name).Scan(&bound)
	if err != nil {
		a.fail(w, r, "delete role", err)
		return
	}
	if bound {
		writeError(w, 409, "assigned", "Remove user assignments before deleting this role")
		return
	}
	revision, parseErr := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if parseErr != nil || revision < 1 {
		writeError(w, 400, "revision_required", "Supply the current role revision")
		return
	}
	tag, err := tx.Exec(r.Context(), `DELETE FROM rbac_roles WHERE namespace=$1 AND name=$2 AND revision=$3`, p.Namespace, name, revision)
	if err == nil && tag.RowsAffected() != 1 {
		writeError(w, 409, "conflict", "Role changed; reload before deleting")
		return
	}
	if err == nil {
		err = rbacAudit(r, tx, p, "role.delete", name)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		if !a.auditUnavailable(w, r, err) {
			a.fail(w, r, "delete role", err)
		}
		return
	}
	w.WriteHeader(204)
}
func (a *API) userRoles(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	id := r.PathValue("id")
	if r.Method == "GET" {
		var roles json.RawMessage
		var revision int64
		var accountRole string
		err := a.db.QueryRow(r.Context(), `SELECT a.roles,COALESCE(a.revision,0),u.role FROM users u LEFT JOIN rbac_user_access a ON a.user_id=u.id AND a.namespace=u.namespace WHERE u.id::text=$1 AND u.namespace=$2`, id, p.Namespace).Scan(&roles, &revision, &accountRole)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, 404, "not_found", "User not found")
			return
		}
		if err != nil {
			a.fail(w, r, "user roles", err)
			return
		}
		if accountRole == "admin" {
			writeJSON(w, 200, map[string]any{"roles": []string{authn.AdministratorRole}, "legacy": false, "revision": 0, "protected": true})
			return
		}
		writeJSON(w, 200, map[string]any{"roles": roles, "legacy": roles == nil, "revision": revision})
		return
	}
	if id == p.UserID {
		writeError(w, 409, "self_assignment", "Use another administrator to change your own role assignments")
		return
	}
	var b struct {
		Roles    []string `json:"roles"`
		Revision int64    `json:"revision"`
	}
	if err := decodeRBACBody(w, r, &b); err != nil || len(b.Roles) > 20 {
		rejectBody(w, err, http.StatusBadRequest, "invalid", "Invalid roles")
		return
	}
	if !revisionOK(w, b.Revision) {
		return
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		a.fail(w, r, "assign roles", err)
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, p.Namespace)
	if err != nil {
		a.fail(w, r, "role lock", err)
		return
	}
	var role string
	err = tx.QueryRow(r.Context(), `SELECT role::text FROM users WHERE id::text=$1 AND namespace=$2 FOR UPDATE`, id, p.Namespace).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "User not found")
		return
	}
	if err != nil {
		a.fail(w, r, "assign roles", err)
		return
	}
	// Keep existing namespace administrators as recovery principals. Assign fine-grained roles to reader accounts.
	if role == "admin" {
		writeError(w, 409, "protected_admin", "Existing administrator accounts retain recovery access; assign module roles to a reader account")
		return
	}

	for _, name := range b.Roles {
		if strings.EqualFold(strings.TrimSpace(name), authn.AdministratorRole) {
			writeError(w, 409, "protected_role", "Administrator is assigned automatically to administrator accounts during account setup")
			return
		}
		grants, builtin := authn.Builtins()[name]
		if !builtin {
			var encoded []byte
			err = tx.QueryRow(r.Context(), `SELECT permissions FROM rbac_roles WHERE namespace=$1 AND name=$2`, p.Namespace, name).Scan(&encoded)
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, 400, "unknown_role", "Unknown role: "+name)
				return
			}
			if err != nil {
				a.fail(w, r, "role lookup", err)
				return
			}
			if err = json.Unmarshal(encoded, &grants); err != nil {
				a.fail(w, r, "role decode", err)
				return
			}
		}
		for _, grant := range grants {
			if !p.Allows(grant) {
				writeError(w, 403, "invalid_grant", "Cannot delegate permission: "+grant)
				return
			}
		}
	}

	if b.Roles == nil {
		b.Roles = []string{}
	}
	raw, _ := json.Marshal(b.Roles)
	var currentRevision int64
	err = tx.QueryRow(r.Context(), `SELECT COALESCE((SELECT revision FROM rbac_user_access WHERE user_id::text=$1),0)`, id).Scan(&currentRevision)
	if err != nil {
		a.fail(w, r, "assignment revision", err)
		return
	}
	if currentRevision != b.Revision {
		writeError(w, 409, "conflict", "Assignment changed; reload before saving")
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO rbac_user_access(user_id,namespace,roles) VALUES($1,$2,$3) ON CONFLICT(user_id) DO UPDATE SET roles=excluded.roles,revision=rbac_user_access.revision+1 WHERE rbac_user_access.revision=$4`, id, p.Namespace, raw, b.Revision)
	if err == nil {
		err = rbacAudit(r, tx, p, "user.roles.assign", id, map[string]any{"roles": b.Roles, "previous_revision": b.Revision})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		if !a.auditUnavailable(w, r, err) {
			a.fail(w, r, "assign roles", err)
		}
		return
	}
	writeJSON(w, 200, map[string]any{"roles": b.Roles})
}
func rbacAudit(r *http.Request, tx pgx.Tx, p authn.Principal, action, target string, details ...map[string]any) error {
	var detail map[string]any
	if len(details) > 0 {
		detail = details[0]
	}
	var actorID *string
	if p.UserID != "" {
		actorID = &p.UserID
	}
	e := store.AuditEntry{Namespace: p.Namespace, ActorID: actorID, Detail: detail, ActorKind: string(p.Kind), ActorLabel: p.Name, Action: action, TargetKind: "rbac", TargetID: target, Outcome: "success", ClientIP: clientIP(r)}
	attributeKGF(p, &e)
	// A failure is an AUDIT failure, reported as such: the change rolls back
	// with it (it is in this transaction) and the caller answers 503
	// audit_unavailable through auditUnavailable, the F139 fail-closed rule.
	// It used to surface as a bare error and a 500, which told the operator
	// the server was broken rather than "retry shortly, nothing changed".
	if err := store.GatewayAppendAudit(r.Context(), tx, e); err != nil {
		countAuditFailure(ClassOf(action), true)
		return auditWriteError{err}
	}
	return nil
}

func permittedEventPayload(e store.EventRow, p authn.Principal) map[string]any {
	out := eventPayload(e)
	if !p.Allows("tasks.results.view") {
		out["payload"] = map[string]any{"redacted": true}
	}
	return out
}

// decodeRBACBody decodes one strict JSON object of at most 32 KiB. A body over
// the limit comes back as *bodyTooLargeError, which the callers answer with
// 413 through rejectBody; anything else is theirs to call malformed.
func decodeRBACBody(w http.ResponseWriter, r *http.Request, out any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return bodyError(err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}

// Reserve built-in names case-insensitively to prevent confusing duplicates.
func isProtectedRole(name string) bool {
	for builtin := range authn.Builtins() {
		if strings.EqualFold(strings.TrimSpace(name), builtin) {
			return true
		}
	}
	return false
}

// Create the identity and initial assignment in one transaction: a failed role
// lookup/assignment must never leave an account with legacy reader permissions.
func (a *API) createUserWithRole(w http.ResponseWriter, r *http.Request, p authn.Principal, email, displayName, roleName, password string) {
	if !p.Allows("roles.manage") {
		writeError(w, 403, "forbidden", "Role assignment requires roles.manage")
		return
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		a.fail(w, r, "create user", err)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, p.Namespace); err != nil {
		a.fail(w, r, "role lock", err)
		return
	}
	grants, builtin := authn.Builtins()[roleName]
	if !builtin {
		var raw []byte
		err = tx.QueryRow(r.Context(), `SELECT permissions FROM rbac_roles WHERE namespace=$1 AND name=$2`, p.Namespace, roleName).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, 422, "unknown_role", "Selected role no longer exists; reload the role list")
			return
		}
		if err != nil {
			a.fail(w, r, "role lookup", err)
			return
		}
		if err = json.Unmarshal(raw, &grants); err != nil {
			a.fail(w, r, "role decode", err)
			return
		}
	}
	for _, grant := range grants {
		if !p.Allows(grant) {
			writeError(w, 403, "invalid_grant", "Cannot delegate permission: "+grant)
			return
		}
	}
	accountRole := "reader"
	if roleName == authn.AdministratorRole {
		accountRole = "admin"
	}
	user, err := store.GatewayCreateUser(r.Context(), tx, p.Namespace, email, displayName, accountRole, password)
	if errors.Is(err, store.ErrEmailTaken) {
		writeError(w, 409, "email_taken", "That email already has an account in this namespace")
		return
	}
	if err != nil {
		a.fail(w, r, "create user", err)
		return
	}
	if accountRole != "admin" {
		raw, _ := json.Marshal([]string{roleName})
		if _, err = tx.Exec(r.Context(), `INSERT INTO rbac_user_access(user_id,namespace,roles) VALUES($1,$2,$3)`, user.ID, p.Namespace, raw); err != nil {
			a.fail(w, r, "assign initial role", err)
			return
		}
	}
	if err = rbacAudit(r, tx, p, "user.create", user.ID, map[string]any{"email": user.Email, "roles": []string{roleName}}); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		if !a.auditUnavailable(w, r, err) {
			a.fail(w, r, "create user", err)
		}
		return
	}
	result := userPayload(user)
	result["roles"] = []string{roleName}
	writeJSON(w, 201, result)
}
