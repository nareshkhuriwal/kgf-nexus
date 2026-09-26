// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// Service accounts and permission-ceilinged tokens (ADR-0024 §6).
//
// A service account is a non-human identity with its own role bindings. Its
// tokens are ordinary scoped tokens that carry the account's id and an
// explicit permission ceiling; at every request the effective grants are the
// account's CURRENT roles intersected with that ceiling (authn), so narrowing
// or disabling the account takes effect immediately and a ceiling never widens.

var serviceAccountName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,62}$`)

func (a *API) registerServiceAccountRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/namespaces/{ns}/service-accounts", a.guard(authn.ActionRead, a.listServiceAccounts))
	mux.Handle("POST /v1/namespaces/{ns}/service-accounts", a.guard(authn.ActionRead, a.createServiceAccount))
	mux.Handle("PUT /v1/namespaces/{ns}/service-accounts/{id}", a.guard(authn.ActionRead, a.updateServiceAccount))
	mux.Handle("POST /v1/namespaces/{ns}/service-accounts/{id}/disable", a.guard(authn.ActionRead, a.setServiceAccountDisabled(true)))
	mux.Handle("POST /v1/namespaces/{ns}/service-accounts/{id}/enable", a.guard(authn.ActionRead, a.setServiceAccountDisabled(false)))
	mux.Handle("GET /v1/namespaces/{ns}/service-accounts/{id}/roles", a.guard(authn.ActionRead, a.serviceAccountRoles))
	mux.Handle("PUT /v1/namespaces/{ns}/service-accounts/{id}/roles", a.guard(authn.ActionRead, a.serviceAccountRoles))
}

type serviceAccountRow struct {
	ID          string     `json:"id"`
	Namespace   string     `json:"namespace"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Disabled    bool       `json:"disabled"`
	DisabledAt  *time.Time `json:"disabled_at"`
	CreatedAt   time.Time  `json:"created_at"`
	Revision    int64      `json:"revision"`
	Roles       []string   `json:"roles"`
	LiveTokens  int        `json:"live_tokens"`
}

const serviceAccountSelect = `SELECT s.id::text, s.namespace, s.name, s.description, s.disabled_at, s.created_at, s.revision,
	COALESCE(r.roles, '[]'::jsonb),
	(SELECT count(*) FROM scoped_tokens t WHERE t.service_account_id=s.id AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at > now()))
	FROM service_accounts s LEFT JOIN service_account_roles r ON r.service_account_id=s.id`

func scanServiceAccount(row pgx.Row) (serviceAccountRow, error) {
	var s serviceAccountRow
	var roles []byte
	if err := row.Scan(&s.ID, &s.Namespace, &s.Name, &s.Description, &s.DisabledAt, &s.CreatedAt, &s.Revision, &roles, &s.LiveTokens); err != nil {
		return s, err
	}
	s.Disabled = s.DisabledAt != nil
	if err := json.Unmarshal(roles, &s.Roles); err != nil {
		return s, err
	}
	return s, nil
}

func (a *API) listServiceAccounts(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	rows, err := a.db.Query(r.Context(), serviceAccountSelect+` WHERE s.namespace=$1 ORDER BY s.name LIMIT 500`, p.Namespace)
	if err != nil {
		a.fail(w, r, "list service accounts", err)
		return
	}
	defer rows.Close()
	out := []serviceAccountRow{}
	for rows.Next() {
		s, err := scanServiceAccount(rows)
		if err != nil {
			a.fail(w, r, "list service accounts", err)
			return
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "list service accounts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"service_accounts": out})
}

func (a *API) createServiceAccount(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	var b struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Roles       []string `json:"roles"`
	}
	if decodeRBACBody(w, r, &b) != nil {
		writeError(w, http.StatusBadRequest, "invalid", "Invalid service account")
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	if !serviceAccountName.MatchString(b.Name) || len(b.Description) > 500 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_service_account",
			"Name must be 2–63 lower-case letters, digits, dots, dashes or underscores; description at most 500 characters")
		return
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		a.fail(w, r, "create service account", err)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, p.Namespace); err != nil {
		a.fail(w, r, "role lock", err)
		return
	}
	if !a.checkAssignableRoles(w, r, tx, p, b.Roles) {
		return
	}
	// Recorded as the actor's name: a Nexus user or a KGF principal, which
	// has no Nexus user id.
	creator := p.Name
	var id string
	err = tx.QueryRow(r.Context(), `INSERT INTO service_accounts (namespace, name, description, created_by) VALUES ($1,$2,$3,$4) RETURNING id::text`,
		p.Namespace, b.Name, b.Description, creator).Scan(&id)
	if isUnique(err) {
		writeError(w, http.StatusConflict, "name_taken", "A service account with that name already exists")
		return
	}
	if err != nil {
		a.fail(w, r, "create service account", err)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO service_account_roles (service_account_id, namespace, roles) VALUES ($1,$2,$3)`,
		id, p.Namespace, jsonStrings(b.Roles)); err != nil {
		a.fail(w, r, "create service account", err)
		return
	}
	if err = auditTx(r, tx, p, "service_account.create", "service_account", id, map[string]any{"name": b.Name, "roles": b.Roles}); err != nil {
		if !a.auditUnavailable(w, r, err) {
			a.fail(w, r, "create service account", err)
		}
		return
	}
	s, err := scanServiceAccount(tx.QueryRow(r.Context(), serviceAccountSelect+` WHERE s.id=$1`, id))
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		a.fail(w, r, "create service account", err)
		return
	}
	writeJSON(w, http.StatusCreated, s)
}

func (a *API) updateServiceAccount(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	var b struct {
		Description string `json:"description"`
		Revision    int64  `json:"revision"`
	}
	if decodeRBACBody(w, r, &b) != nil || len(b.Description) > 500 {
		writeError(w, http.StatusBadRequest, "invalid", "Invalid service account")
		return
	}
	id := r.PathValue("id")
	tag, err := a.db.Exec(r.Context(), `UPDATE service_accounts SET description=$3, revision=revision+1 WHERE namespace=$1 AND id::text=$2 AND revision=$4`,
		p.Namespace, id, b.Description, b.Revision)
	if err != nil {
		a.fail(w, r, "update service account", err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusConflict, "conflict", "Service account changed or does not exist; reload before saving")
		return
	}
	a.auditPrincipal(r, p, "service_account.update", "service_account", id, store.AuditSuccess, nil)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "updated": true})
}

// setServiceAccountDisabled disables or enables an account. Disabling makes
// every token of the account fail authentication on its next request (the
// resolve query joins the account), without touching the token rows, so
// re-enabling restores them; revoke tokens explicitly to end them for good.
func (a *API) setServiceAccountDisabled(disabled bool) func(http.ResponseWriter, *http.Request, authn.Principal) {
	return func(w http.ResponseWriter, r *http.Request, p authn.Principal) {
		id := r.PathValue("id")
		// A delegate may only disable or enable an account no more powerful
		// than itself.
		grants, err := serviceAccountGrants(r.Context(), a.db, p.Namespace, id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Service account not found")
			return
		}
		if err != nil {
			a.fail(w, r, "service account", err)
			return
		}
		if g, ok := p.Subset(grants); !ok {
			writeError(w, http.StatusForbidden, "target_above_actor", "This service account holds "+g+", which you do not have")
			return
		}
		var at *time.Time
		action := "service_account.enable"
		if disabled {
			now := time.Now().UTC()
			at = &now
			action = "service_account.disable"
		}
		if disabled {
			// A revocation: it takes effect even if its audit row cannot be
			// written (that failure is logged and counted, F139).
			tag, err := a.db.Exec(r.Context(), `UPDATE service_accounts SET disabled_at=$3, revision=revision+1 WHERE namespace=$1 AND id::text=$2`, p.Namespace, id, at)
			if err != nil {
				a.fail(w, r, "service account", err)
				return
			}
			if tag.RowsAffected() == 0 {
				writeError(w, http.StatusNotFound, "not_found", "Service account not found")
				return
			}
			a.auditPrincipal(r, p, action, "service_account", id, store.AuditSuccess, nil)
			writeJSON(w, http.StatusOK, map[string]any{"id": id, "disabled": disabled})
			return
		}
		// Enabling restores every token of the account: a security grant, so
		// the change and its audit row commit together or not at all.
		tx, err := a.db.Begin(r.Context())
		if err != nil {
			a.fail(w, r, "service account", err)
			return
		}
		defer tx.Rollback(r.Context())
		tag, err := tx.Exec(r.Context(), `UPDATE service_accounts SET disabled_at=$3, revision=revision+1 WHERE namespace=$1 AND id::text=$2`, p.Namespace, id, at)
		if err != nil {
			a.fail(w, r, "service account", err)
			return
		}
		if tag.RowsAffected() == 0 {
			writeError(w, http.StatusNotFound, "not_found", "Service account not found")
			return
		}
		if err = auditTx(r, tx, p, action, "service_account", id, nil); err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			if !a.auditUnavailable(w, r, err) {
				a.fail(w, r, "service account", err)
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "disabled": disabled})
	}
}

func (a *API) serviceAccountRoles(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	id := r.PathValue("id")
	if r.Method == http.MethodGet {
		var raw []byte
		var rev int64
		err := a.db.QueryRow(r.Context(), `SELECT COALESCE(r.roles,'[]'::jsonb), COALESCE(r.revision,0) FROM service_accounts s
			LEFT JOIN service_account_roles r ON r.service_account_id=s.id WHERE s.namespace=$1 AND s.id::text=$2`, p.Namespace, id).Scan(&raw, &rev)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "Service account not found")
			return
		}
		if err != nil {
			a.fail(w, r, "service account roles", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"roles": json.RawMessage(raw), "revision": rev})
		return
	}
	var b struct {
		Roles    []string `json:"roles"`
		Revision int64    `json:"revision"`
	}
	if decodeRBACBody(w, r, &b) != nil || len(b.Roles) > 20 {
		writeError(w, http.StatusBadRequest, "invalid", "Invalid roles")
		return
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		a.fail(w, r, "assign roles", err)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, p.Namespace); err != nil {
		a.fail(w, r, "role lock", err)
		return
	}
	current, err := serviceAccountGrants(r.Context(), tx, p.Namespace, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Service account not found")
		return
	}
	if err != nil {
		a.fail(w, r, "assign roles", err)
		return
	}
	if g, ok := p.Subset(current); !ok {
		writeError(w, http.StatusForbidden, "target_above_actor", "This service account holds "+g+", which you do not have")
		return
	}
	if !a.checkAssignableRoles(w, r, tx, p, b.Roles) {
		return
	}
	tag, err := tx.Exec(r.Context(), `UPDATE service_account_roles SET roles=$3, revision=revision+1 WHERE namespace=$1 AND service_account_id::text=$2 AND revision=$4`,
		p.Namespace, id, jsonStrings(b.Roles), b.Revision)
	if err != nil {
		a.fail(w, r, "assign roles", err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusConflict, "conflict", "Assignment changed; reload before saving")
		return
	}
	if err = auditTx(r, tx, p, "service_account.roles.assign", "service_account", id, map[string]any{"roles": b.Roles, "previous_revision": b.Revision}); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		if !a.auditUnavailable(w, r, err) {
			a.fail(w, r, "assign roles", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": b.Roles})
}

// checkAssignableRoles validates roles for a binding: known, not
// Administrator, and every grant delegable by the actor.
func (a *API) checkAssignableRoles(w http.ResponseWriter, r *http.Request, q store.GWQuerier, p authn.Principal, roles []string) bool {
	for _, name := range roles {
		if strings.EqualFold(strings.TrimSpace(name), authn.AdministratorRole) {
			writeError(w, http.StatusConflict, "protected_role", "Administrator cannot be assigned to a service account")
			return false
		}
		grants, err := authn.RoleGrants(r.Context(), q, p.Namespace, name)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "unknown_role", "Unknown role: "+name)
			return false
		}
		if err != nil {
			a.fail(w, r, "role lookup", err)
			return false
		}
		if g, ok := checkDelegable(p, grants); !ok {
			writeError(w, http.StatusForbidden, "invalid_grant", "Cannot delegate permission: "+g)
			return false
		}
	}
	return true
}

// serviceAccountGrants returns the current grants of a service account.
func serviceAccountGrants(ctx context.Context, q store.GWQuerier, namespace, id string) (map[string]bool, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT COALESCE(r.roles,'[]'::jsonb) FROM service_accounts s
		LEFT JOIN service_account_roles r ON r.service_account_id=s.id WHERE s.namespace=$1 AND s.id::text=$2`, namespace, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, err
	}
	return authn.RoleSetGrants(ctx, q, namespace, names)
}

// tokenMinter is the ceiling-aware token creation; the store adapter
// implements it.
type tokenMinter interface {
	CreateScopedTokenWith(ctx context.Context, spec store.TokenSpec) (string, store.ScopedTokenRow, error)
}

// createCeilingToken mints a service-account token or a legacy token with an
// explicit permission ceiling.
func (a *API) createCeilingToken(w http.ResponseWriter, r *http.Request, p authn.Principal, ns, name, role string, scopes []string, days int, saID string, perms *[]string) {
	minter, ok := a.identity.(tokenMinter)
	if !ok || a.db == nil {
		writeError(w, http.StatusNotImplemented, "not_configured", "this gateway cannot mint ceilinged tokens")
		return
	}
	var problems []string
	if name == "" {
		problems = append(problems, "name is required, so the token can be recognised later")
	}
	if perms == nil || len(*perms) == 0 {
		problems = append(problems, "permissions is required and must name at least one permission")
	}
	ceiling := []string{}
	if perms != nil {
		seen := map[string]bool{}
		for _, g := range *perms {
			g = strings.TrimSpace(g)
			if !authn.Known(g) {
				problems = append(problems, "unknown permission "+g)
				continue
			}
			if !seen[g] {
				seen[g] = true
				ceiling = append(ceiling, g)
			}
		}
	}
	sort.Strings(ceiling)
	if saID == "" && authn.Role(role) == authn.RoleWorker {
		problems = append(problems, "a worker token cannot carry a permission ceiling; worker tokens speak only the worker protocol")
	}
	if saID == "" {
		if authn.Role(role) != authn.RoleAdmin && authn.Role(role) != authn.RoleReader && authn.Role(role) != authn.RoleWorker {
			problems = append(problems, "role must be admin or reader")
		}
	}
	if len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "invalid_token_request", "message": "token could not be created", "problems": problems})
		return
	}
	if g, ok := checkDelegable(p, ceiling); !ok {
		writeError(w, http.StatusForbidden, "invalid_grant", "Cannot delegate permission: "+g)
		return
	}
	spec := store.TokenSpec{Namespace: ns, Name: name, Permissions: ceiling}
	if saID != "" {
		grants, err := serviceAccountGrants(r.Context(), a.db, ns, saID)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Service account not found")
			return
		}
		if err != nil {
			a.fail(w, r, "service account", err)
			return
		}
		for _, g := range ceiling {
			if !grants[g] {
				writeError(w, http.StatusUnprocessableEntity, "outside_account", "The service account does not hold "+g+"; assign it a role that does first")
				return
			}
		}
		var disabled bool
		if err := a.db.QueryRow(r.Context(), `SELECT disabled_at IS NOT NULL FROM service_accounts WHERE id::text=$1 AND namespace=$2`, saID, ns).Scan(&disabled); err != nil {
			a.fail(w, r, "service account", err)
			return
		}
		if disabled {
			writeError(w, http.StatusConflict, "account_disabled", "Enable the service account before minting tokens for it")
			return
		}
		// Stored role reader with NO scopes: a pre-0032 gateway replica that
		// cannot interpret service accounts grants such a token nothing.
		spec.Role, spec.Scopes, spec.ServiceAccountID = string(authn.RoleReader), []string{}, &saID
	} else {
		if !isAdministrator(p) && authn.Role(role) == authn.RoleAdmin {
			// The stored role is the fallback an old replica would honour; a
			// delegate may not mint an admin-role credential.
			role = string(authn.RoleReader)
		}
		spec.Role = role
		spec.Scopes = coarseScopes(ceiling, scopes)
	}
	if days == 0 {
		days = defaultTokenDays
	}
	if days > 0 {
		t := time.Now().UTC().AddDate(0, 0, days)
		spec.ExpiresAt = &t
	} else if !isAdministrator(p) {
		writeError(w, http.StatusForbidden, "expiry_required", "Only an administrator can mint a token that never expires")
		return
	}
	if p.UserID != "" {
		spec.CreatedBy = &p.UserID
	}
	// Minted and audited in one transaction (fail closed, F139), like every
	// other token.
	var secret string
	var row store.ScopedTokenRow
	err := a.auditedTx(r, func(id IdentityAdmin) (store.AuditEntry, error) {
		m, ok := id.(tokenMinter)
		if !ok {
			m = minter
		}
		var err error
		secret, row, err = m.CreateScopedTokenWith(r.Context(), spec)
		return principalEntry(r, p, "token.create", "token", row.ID, store.AuditSuccess, map[string]any{
			"name": row.Name, "role": row.Role, "scopes": row.Scopes, "permissions": row.Permissions,
			"service_account_id": row.ServiceAccountID, "expires_at": row.ExpiresAt,
		}), err
	})
	if err != nil {
		if !a.auditUnavailable(w, r, err) {
			a.fail(w, r, "create token", err)
		}
		return
	}
	payload := tokenPayload(row)
	payload["token"] = secret
	payload["token_shown_once"] = true
	writeJSON(w, http.StatusCreated, payload)
}

// coarseScopes derives the legacy scopes a ceiling implies, intersected with
// any the caller asked for. It is what a pre-0032 replica (which ignores the
// ceiling) would honour, so it must never exceed the ceiling's reach.
func coarseScopes(ceiling, requested []string) []string {
	implied := map[string]bool{}
	for _, g := range ceiling {
		switch {
		case strings.HasSuffix(g, ".view"):
			implied[string(authn.ActionRead)] = true
		default:
			p := authn.Principal{Role: authn.RoleAdmin, Scopes: []authn.Action{authn.ActionWrite}}
			if p.Allows(g) {
				implied[string(authn.ActionWrite)] = true
			} else {
				implied[string(authn.ActionAdminWrite)] = true
			}
		}
	}
	out := []string{}
	for _, s := range []string{"read", "write", "admin_write"} {
		if !implied[s] {
			continue
		}
		if len(requested) > 0 {
			found := false
			for _, q := range requested {
				if q == s {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

func jsonStrings(v []string) []byte {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return b
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLSTATE 23505")
}

// auditTx writes an audit row inside the change's own transaction, so the
// change and its record commit together or not at all (fail closed, F139).
// A failed write is an auditWriteError: the caller answers 503
// audit_unavailable (auditUnavailable) and the transaction rolls back.
func auditTx(r *http.Request, tx pgx.Tx, p authn.Principal, action, targetKind, target string, detail map[string]any) error {
	e := principalEntry(r, p, action, targetKind, target, store.AuditSuccess, detail)
	if err := store.GatewayAppendAudit(r.Context(), tx, e); err != nil {
		countAuditFailure(ClassOf(action), true)
		return auditWriteError{err}
	}
	return nil
}

// isAdministrator reports whether a principal holds the Administrator system
// role: an unbound admin credential. Bound principals, including every KGF
// principal, never do.
func isAdministrator(p authn.Principal) bool { return p.Grants == nil && p.Role == authn.RoleAdmin }

// checkDelegable verifies every permission in a set can be handed out by p:
// it is in the catalog, it is not a reserved identity grant, and p holds it.
func checkDelegable(p authn.Principal, grants []string) (string, bool) {
	for _, g := range grants {
		if !authn.Delegable(g) || !p.Allows(g) {
			return g, false
		}
	}
	return "", true
}
