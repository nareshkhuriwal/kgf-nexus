// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package rest

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// updateUser edits profile fields only. Roles and account state use their
// dedicated endpoints so identity edits cannot accidentally change privileges.
func (a *API) updateUser(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	var b struct {
		Email               string  `json:"email"`
		DisplayName         string  `json:"display_name"`
		PreviousEmail       *string `json:"previous_email"`
		PreviousDisplayName *string `json:"previous_display_name"`
	}
	if err := decodeRBACBody(w, r, &b); err != nil {
		rejectBody(w, err, http.StatusBadRequest, "invalid", "Invalid user details")
		return
	}
	b.Email = strings.ToLower(strings.TrimSpace(b.Email))
	b.DisplayName = strings.TrimSpace(b.DisplayName)
	addr, err := mail.ParseAddress(b.Email)
	if err != nil || addr.Address != b.Email || len(b.Email) > 254 || len(b.DisplayName) > 200 || b.PreviousEmail == nil || b.PreviousDisplayName == nil {
		writeError(w, 400, "invalid_user", "Provide a valid email, a display name of up to 200 characters, and the original profile values")
		return
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		a.fail(w, r, "edit user", err)
		return
	}
	defer tx.Rollback(r.Context())
	var current store.UserRow
	err = tx.QueryRow(r.Context(), `SELECT id::text,namespace,email,display_name,role,created_at,disabled_at FROM users WHERE namespace=$1 AND id::text=$2 FOR UPDATE`, p.Namespace, r.PathValue("id")).Scan(&current.ID, &current.Namespace, &current.Email, &current.DisplayName, &current.Role, &current.CreatedAt, &current.DisabledAt)
	// email_verified_at is read back from the UPDATE below rather than here:
	// the database un-proves an address when it changes (migration 0017's
	// trigger), so only the row as written says whether the account is still
	// verified. The response used to leave it nil -- email_verified: false for
	// every account.
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "User not found")
		return
	}
	if err != nil {
		a.fail(w, r, "edit user", err)
		return
	}
	if current.Email != *b.PreviousEmail || current.DisplayName != *b.PreviousDisplayName {
		writeError(w, 409, "conflict", "This user changed. Close the editor, refresh the list, and reopen it before saving.")
		return
	}
	err = tx.QueryRow(r.Context(), `UPDATE users SET email=$3,display_name=$4 WHERE namespace=$1 AND id::text=$2 RETURNING email_verified_at`,
		p.Namespace, current.ID, b.Email, b.DisplayName).Scan(&current.EmailVerifiedAt)
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			writeError(w, 409, "email_taken", "That email already belongs to an account")
			return
		}
		a.fail(w, r, "edit user", err)
		return
	}
	// user.update is a security grant (it can re-point where recovery mail
	// goes), so a failed audit write rolls the edit back and answers 503
	// audit_unavailable like every other fail-closed action (F139), not 500.
	if err = auditTx(r, tx, p, "user.update", "user", current.ID,
		map[string]any{"email": b.Email, "display_name": b.DisplayName}); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		if !a.auditUnavailable(w, r, err) {
			a.fail(w, r, "edit user", err)
		}
		return
	}
	current.Email = b.Email
	current.DisplayName = b.DisplayName
	writeJSON(w, 200, userPayload(current))
}
