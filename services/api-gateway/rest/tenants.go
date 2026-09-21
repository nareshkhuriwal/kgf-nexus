// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package rest

import (
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"net/http"
	"net/mail"
	"strings"
)

func (a *API) platformOperator(w http.ResponseWriter, r *http.Request, p authn.Principal) bool {
	if p.Kind != authn.KindSession || p.Role != authn.RoleAdmin || p.Grants != nil {
		writeError(w, 403, "forbidden", "Platform operator session required")
		return false
	}
	ok, err := store.GatewayPlatformOperator(r.Context(), a.db, p.UserID)
	if err != nil {
		a.fail(w, r, "check platform operator", err)
		return false
	}
	if !ok {
		writeError(w, 403, "forbidden", "Platform operator session required")
	}
	return ok
}
func (a *API) listTenants(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.platformOperator(w, r, p) {
		return
	}
	rows, err := a.db.Query(r.Context(), `SELECT id::text,namespace,name,contact_email,created_at FROM tenants ORDER BY created_at DESC LIMIT 500`)
	if err != nil {
		a.fail(w, r, "list customers", err)
		return
	}
	defer rows.Close()
	tenants := []store.TenantRow{}
	for rows.Next() {
		var t store.TenantRow
		if err = rows.Scan(&t.ID, &t.Namespace, &t.Name, &t.ContactEmail, &t.CreatedAt); err != nil {
			a.fail(w, r, "read customer", err)
			return
		}
		tenants = append(tenants, t)
	}
	if err = rows.Err(); err != nil {
		a.fail(w, r, "read customers", err)
		return
	}
	writeJSON(w, 200, map[string]any{"tenants": tenants, "limit": 500})
}
func (a *API) createTenant(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.platformOperator(w, r, p) {
		return
	}
	a.onboardTenant(w, r, p, false)
}
func (a *API) registerCustomer(w http.ResponseWriter, r *http.Request) {
	if a.rateLimited(w, r) {
		return
	}
	a.onboardTenant(w, r, authn.Principal{}, true)
}
func (a *API) onboardTenant(w http.ResponseWriter, r *http.Request, p authn.Principal, public bool) {
	var req struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	body, err := readBody(r, 16<<10)
	if err != nil {
		writeError(w, 413, "too_large", "Request too large")
		return
	}
	if json.Unmarshal(body, &req) != nil {
		writeError(w, 400, "invalid_json", "Invalid customer request")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	address, emailErr := mail.ParseAddress(req.Email)
	if req.Name == "" || len(req.Name) > 160 || emailErr != nil || address.Address != req.Email || len(req.Password) < 12 || len(req.Password) > 1024 {
		writeError(w, 422, "invalid_customer", "Customer name, email and a 12–1024 character administrator password are required")
		return
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		a.fail(w, r, "begin onboarding", err)
		return
	}
	defer tx.Rollback(r.Context())
	var registrationEmail any
	if public {
		registrationEmail = req.Email
	}
	var t store.TenantRow
	err = tx.QueryRow(r.Context(), `WITH identity AS (SELECT gen_random_uuid() AS id) INSERT INTO tenants(id,namespace,name,contact_email,registration_email) SELECT id,id::text,$1,$2,$3 FROM identity RETURNING id::text,namespace,name,contact_email,created_at`, req.Name, req.Email, registrationEmail).Scan(&t.ID, &t.Namespace, &t.Name, &t.ContactEmail, &t.CreatedAt)
	if err != nil {
		var conflict *pgconn.PgError
		if errors.As(err, &conflict) && conflict.Code == "23505" {
			writeError(w, 409, "registration_conflict", "Registration could not be completed. If you already registered, sign in with your tenant ID.")
			return
		}
		a.fail(w, r, "create customer", err)
		return
	}
	u, err := store.GatewayCreateUser(r.Context(), tx, t.Namespace, req.Email, req.Name+" Administrator", "admin", req.Password)
	if err != nil {
		a.fail(w, r, "create customer administrator", err)
		return
	}
	if _, err = tx.Exec(r.Context(), `UPDATE tenants SET owner_user_id=$1 WHERE id=$2`, u.ID, t.ID); err != nil {
		a.fail(w, r, "set customer owner", err)
		return
	}
	auditNamespaces := []string{t.Namespace}
	if !public {
		auditNamespaces = append(auditNamespaces, p.Namespace)
	} else {
		p.Kind = authn.PrincipalKind("registration")
		p.Name = req.Email
	}
	for _, ns := range auditNamespaces {
		err = store.GatewayAppendAudit(r.Context(), tx, store.AuditEntry{Namespace: ns, ActorKind: string(p.Kind), ActorLabel: p.Name, Action: "tenant.create", TargetKind: "tenant", TargetID: t.ID, Outcome: "OK", Detail: map[string]any{"tenant_id": t.ID, "administrator_id": u.ID}})
		if err != nil {
			a.fail(w, r, "audit onboarding", err)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		a.fail(w, r, "commit onboarding", err)
		return
	}
	writeJSON(w, 201, map[string]any{"tenant": t, "administrator_id": u.ID})
}

func (a *API) currentTenant(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	t, err := store.GatewayTenant(r.Context(), a.db, p.Namespace)
	if err != nil {
		a.fail(w, r, "load tenant", err)
		return
	}
	operator, err := store.GatewayPlatformOperator(r.Context(), a.db, p.UserID)
	if err != nil {
		a.fail(w, r, "load platform access", err)
		return
	}
	writeJSON(w, 200, map[string]any{"tenant": t, "platform_operator": operator && p.Kind == authn.KindSession && p.Role == authn.RoleAdmin && p.Grants == nil})
}
