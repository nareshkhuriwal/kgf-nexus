// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

// Tenant SSO persistence (migration 0032, ADR-0024): identity providers,
// in-flight login states, one-time handoffs, SAML replay records, linked
// identities and IdP group mappings. Owned by the api-gateway.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// SSO errors. Each maps to one opaque, user-facing code; the detail stays in
// the audit log.
var (
	ErrSSOStateInvalid      = errors.New("sso: login state unknown, expired or already used")
	ErrSSOHandoffInvalid    = errors.New("sso: handoff unknown, expired, already used or bound to another browser")
	ErrSSOReplay            = errors.New("sso: assertion already used")
	ErrSSODomainNotAllowed  = errors.New("sso: email domain is not allowed for this provider")
	ErrSSOAccountUnlinked   = errors.New("sso: an account with this email exists and cannot be linked automatically")
	ErrSSONoAccount         = errors.New("sso: no account and just-in-time provisioning is off")
	ErrSSOUserDisabled      = errors.New("sso: account is disabled")
	ErrSSOEmailMissing      = errors.New("sso: the identity provider asserted no email address")
	ErrSSOConflict          = errors.New("sso: provider changed; reload before saving")
	ErrSSODuplicateProvider = errors.New("sso: this identity provider application is already configured (here or in another tenant), or the slug is taken")
)

// SSOProviderRow is one tenant identity provider. It carries no secret: the
// client secret is a reference resolved at use.
type SSOProviderRow struct {
	ID                       string    `json:"id"`
	Namespace                string    `json:"namespace"`
	Kind                     string    `json:"kind"`
	Slug                     string    `json:"slug"`
	DisplayName              string    `json:"display_name"`
	Enabled                  bool      `json:"enabled"`
	Issuer                   string    `json:"issuer"`
	ClientID                 string    `json:"client_id"`
	ClientSecretRef          string    `json:"client_secret_ref"`
	Scopes                   []string  `json:"scopes"`
	IDPMetadataXML           string    `json:"idp_metadata_xml"`
	IDPEntityID              string    `json:"idp_entity_id"`
	AllowedDomains           []string  `json:"allowed_domains"`
	JITProvisioning          bool      `json:"jit_provisioning"`
	LinkByVerifiedEmail      bool      `json:"link_by_verified_email"`
	GroupsClaim              string    `json:"groups_claim"`
	RevokeSessionsOnRoleLoss bool      `json:"revoke_sessions_on_role_loss"`
	Revision                 int64     `json:"revision"`
	MappingsRevision         int64     `json:"mappings_revision"`
	CreatedAt                time.Time `json:"created_at"`
	UpdatedAt                time.Time `json:"updated_at"`
}

const ssoProviderCols = `id::text, namespace, kind, slug, display_name, enabled, issuer, client_id,
	client_secret_ref, scopes, idp_metadata_xml, idp_entity_id, allowed_domains, jit_provisioning,
	link_by_verified_email, groups_claim, revoke_sessions_on_role_loss, revision, mappings_revision,
	created_at, updated_at`

func scanSSOProvider(row pgx.Row) (SSOProviderRow, error) {
	var p SSOProviderRow
	var scopes, domains []byte
	err := row.Scan(&p.ID, &p.Namespace, &p.Kind, &p.Slug, &p.DisplayName, &p.Enabled, &p.Issuer, &p.ClientID,
		&p.ClientSecretRef, &scopes, &p.IDPMetadataXML, &p.IDPEntityID, &domains, &p.JITProvisioning,
		&p.LinkByVerifiedEmail, &p.GroupsClaim, &p.RevokeSessionsOnRoleLoss, &p.Revision, &p.MappingsRevision,
		&p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(scopes, &p.Scopes); err != nil {
		return p, fmt.Errorf("decode scopes: %w", err)
	}
	if err := json.Unmarshal(domains, &p.AllowedDomains); err != nil {
		return p, fmt.Errorf("decode allowed domains: %w", err)
	}
	return p, nil
}

// GatewayListSSOProviders lists a namespace's providers.
func GatewayListSSOProviders(ctx context.Context, q GWQuerier, namespace string) ([]SSOProviderRow, error) {
	rows, err := q.Query(ctx, `SELECT `+ssoProviderCols+` FROM sso_providers WHERE namespace=$1 ORDER BY display_name, id`, namespace)
	if err != nil {
		return nil, fmt.Errorf("list sso providers: %w", err)
	}
	defer rows.Close()
	out := []SSOProviderRow{}
	for rows.Next() {
		p, err := scanSSOProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GatewayGetSSOProvider reads one provider scoped to its namespace.
func GatewayGetSSOProvider(ctx context.Context, q GWQuerier, namespace, id string) (SSOProviderRow, error) {
	p, err := scanSSOProvider(q.QueryRow(ctx, `SELECT `+ssoProviderCols+` FROM sso_providers WHERE namespace=$1 AND id::text=$2`, namespace, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// GatewayGetSSOProviderByID reads one provider by id alone. Used by the public
// flow, where the provider id in the URL is the only input; the row, never the
// request, then decides the namespace.
func GatewayGetSSOProviderByID(ctx context.Context, q GWQuerier, id string) (SSOProviderRow, error) {
	if !looksLikeUUID(id) {
		return SSOProviderRow{}, ErrNotFound
	}
	p, err := scanSSOProvider(q.QueryRow(ctx, `SELECT `+ssoProviderCols+` FROM sso_providers WHERE id=$1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// SSOProviderInput is the writable part of a provider.
type SSOProviderInput struct {
	Kind                     string
	Slug                     string
	DisplayName              string
	Enabled                  bool
	Issuer                   string
	ClientID                 string
	ClientSecretRef          string
	Scopes                   []string
	IDPMetadataXML           string
	IDPEntityID              string
	AllowedDomains           []string
	JITProvisioning          bool
	LinkByVerifiedEmail      bool
	GroupsClaim              string
	RevokeSessionsOnRoleLoss bool
}

func jsonList(v []string) []byte {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return b
}

// GatewayCreateSSOProvider inserts a provider.
func GatewayCreateSSOProvider(ctx context.Context, q GWQuerier, namespace string, in SSOProviderInput, createdBy *string) (SSOProviderRow, error) {
	p, err := scanSSOProvider(q.QueryRow(ctx, `
		INSERT INTO sso_providers (namespace, kind, slug, display_name, enabled, issuer, client_id, client_secret_ref,
		       scopes, idp_metadata_xml, idp_entity_id, allowed_domains, jit_provisioning, link_by_verified_email,
		       groups_claim, revoke_sessions_on_role_loss, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12::jsonb,$13,$14,$15,$16,$17)
		RETURNING `+ssoProviderCols,
		namespace, in.Kind, in.Slug, in.DisplayName, in.Enabled, in.Issuer, in.ClientID, in.ClientSecretRef,
		jsonList(in.Scopes), in.IDPMetadataXML, in.IDPEntityID, jsonList(in.AllowedDomains), in.JITProvisioning,
		in.LinkByVerifiedEmail, in.GroupsClaim, in.RevokeSessionsOnRoleLoss, createdBy))
	if isUniqueViolation(err) {
		return p, ErrSSODuplicateProvider
	}
	return p, err
}

// GatewayUpdateSSOProvider replaces a provider's configuration if the revision
// still matches (optimistic concurrency, like roles).
func GatewayUpdateSSOProvider(ctx context.Context, q GWQuerier, namespace, id string, revision int64, in SSOProviderInput) (SSOProviderRow, error) {
	p, err := scanSSOProvider(q.QueryRow(ctx, `
		UPDATE sso_providers SET slug=$4, display_name=$5, enabled=$6, issuer=$7, client_id=$8, client_secret_ref=$9,
		       scopes=$10::jsonb, idp_metadata_xml=$11, idp_entity_id=$12, allowed_domains=$13::jsonb,
		       jit_provisioning=$14, link_by_verified_email=$15, groups_claim=$16,
		       revoke_sessions_on_role_loss=$17, revision=revision+1, updated_at=now()
		 WHERE namespace=$1 AND id::text=$2 AND revision=$3
		 RETURNING `+ssoProviderCols,
		namespace, id, revision, in.Slug, in.DisplayName, in.Enabled, in.Issuer, in.ClientID, in.ClientSecretRef,
		jsonList(in.Scopes), in.IDPMetadataXML, in.IDPEntityID, jsonList(in.AllowedDomains), in.JITProvisioning,
		in.LinkByVerifiedEmail, in.GroupsClaim, in.RevokeSessionsOnRoleLoss))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrSSOConflict
	}
	if isUniqueViolation(err) {
		return p, ErrSSODuplicateProvider
	}
	return p, err
}

// GatewayDeleteSSOProvider deletes a provider and (by cascade) its identities,
// mappings, in-flight states and handoffs.
func GatewayDeleteSSOProvider(ctx context.Context, q GWQuerier, namespace, id string, revision int64) error {
	tag, err := q.Exec(ctx, `DELETE FROM sso_providers WHERE namespace=$1 AND id::text=$2 AND revision=$3`, namespace, id, revision)
	if err != nil {
		return fmt.Errorf("delete sso provider: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSSOConflict
	}
	return nil
}

// ── login states and handoffs ───────────────────────────────────────────────

// SSOLoginState is one in-flight login. Hashes only for the state and binding.
type SSOLoginState struct {
	StateHash     string
	ProviderID    string
	Namespace     string
	Nonce         string
	CodeVerifier  string
	SAMLRequestID string
	ReturnTo      string
	BindingHash   string
	ExpiresAt     time.Time
}

// GatewayCreateLoginState stores a new login state.
func GatewayCreateLoginState(ctx context.Context, q GWQuerier, s SSOLoginState, ttl time.Duration) error {
	_, err := q.Exec(ctx, `
		INSERT INTO sso_login_states (state_hash, provider_id, namespace, nonce, code_verifier, saml_request_id,
		       return_to, binding_hash, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now() + $9::interval)`,
		s.StateHash, s.ProviderID, s.Namespace, s.Nonce, s.CodeVerifier, s.SAMLRequestID, s.ReturnTo, s.BindingHash, ttl.String())
	if err != nil {
		return fmt.Errorf("create sso login state: %w", err)
	}
	return nil
}

// GatewayConsumeLoginState marks a state used and returns it, exactly once,
// and only for the provider it was created for, before its expiry by the
// DATABASE clock. Everything else is ErrSSOStateInvalid.
func GatewayConsumeLoginState(ctx context.Context, q GWQuerier, stateHash, providerID string) (SSOLoginState, error) {
	var s SSOLoginState
	err := q.QueryRow(ctx, `
		UPDATE sso_login_states SET consumed_at = now()
		 WHERE state_hash=$1 AND provider_id::text=$2 AND consumed_at IS NULL AND expires_at > now()
		 RETURNING state_hash, provider_id::text, namespace, nonce, code_verifier, saml_request_id, return_to,
		           binding_hash, expires_at`, stateHash, providerID,
	).Scan(&s.StateHash, &s.ProviderID, &s.Namespace, &s.Nonce, &s.CodeVerifier, &s.SAMLRequestID, &s.ReturnTo,
		&s.BindingHash, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrSSOStateInvalid
	}
	if err != nil {
		return s, fmt.Errorf("consume sso login state: %w", err)
	}
	return s, nil
}

// SSOHandoff is the one-time bridge from the IdP callback to the console.
type SSOHandoff struct {
	UserID          string
	ProviderID      string
	Namespace       string
	ReturnTo        string
	SSONameID       string
	SSOSessionIndex string
}

// GatewayCreateHandoff stores a handoff (hashes only).
func GatewayCreateHandoff(ctx context.Context, q GWQuerier, codeHash, bindingHash string, h SSOHandoff, ttl time.Duration) error {
	_, err := q.Exec(ctx, `
		INSERT INTO sso_handoffs (code_hash, user_id, provider_id, namespace, binding_hash, return_to,
		       sso_name_id, sso_session_index, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now() + $9::interval)`,
		codeHash, h.UserID, h.ProviderID, h.Namespace, bindingHash, h.ReturnTo, h.SSONameID, h.SSOSessionIndex, ttl.String())
	if err != nil {
		return fmt.Errorf("create sso handoff: %w", err)
	}
	return nil
}

// GatewayConsumeHandoff uses a handoff exactly once, only from the browser it
// is bound to, and only while its user is enabled.
func GatewayConsumeHandoff(ctx context.Context, q GWQuerier, codeHash, bindingHash string) (SSOHandoff, error) {
	var h SSOHandoff
	err := q.QueryRow(ctx, `
		UPDATE sso_handoffs h SET consumed_at = now()
		  FROM users u
		 WHERE h.code_hash=$1 AND h.binding_hash=$2 AND h.consumed_at IS NULL AND h.expires_at > now()
		   AND u.id = h.user_id AND u.disabled_at IS NULL
		 RETURNING h.user_id::text, h.provider_id::text, h.namespace, h.return_to, h.sso_name_id, h.sso_session_index`,
		codeHash, bindingHash,
	).Scan(&h.UserID, &h.ProviderID, &h.Namespace, &h.ReturnTo, &h.SSONameID, &h.SSOSessionIndex)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, ErrSSOHandoffInvalid
	}
	if err != nil {
		return h, fmt.Errorf("consume sso handoff: %w", err)
	}
	return h, nil
}

// GatewayRecordAssertion records a SAML assertion ID; a second use of the same
// ID for the same provider is ErrSSOReplay.
func GatewayRecordAssertion(ctx context.Context, q GWQuerier, providerID, assertionID string, until time.Time) error {
	tag, err := q.Exec(ctx, `
		INSERT INTO sso_assertion_replay (provider_id, assertion_id, expires_at) VALUES ($1,$2,$3)
		ON CONFLICT DO NOTHING`, providerID, assertionID, until)
	if err != nil {
		return fmt.Errorf("record assertion: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSSOReplay
	}
	return nil
}

// GatewayDeleteExpiredSSO sweeps login states, handoffs and replay records that
// can no longer be used. Bounded per call like the session sweep.
func GatewayDeleteExpiredSSO(ctx context.Context, q GWQuerier, limit int) (int64, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	var total int64
	for _, stmt := range []string{
		`DELETE FROM sso_login_states WHERE state_hash IN (SELECT state_hash FROM sso_login_states WHERE expires_at < now() - interval '1 hour' LIMIT $1)`,
		`DELETE FROM sso_handoffs WHERE code_hash IN (SELECT code_hash FROM sso_handoffs WHERE expires_at < now() - interval '1 hour' LIMIT $1)`,
		`DELETE FROM sso_assertion_replay WHERE (provider_id, assertion_id) IN (SELECT provider_id, assertion_id FROM sso_assertion_replay WHERE expires_at < now() LIMIT $1)`,
	} {
		tag, err := q.Exec(ctx, stmt, limit)
		if err != nil {
			return total, fmt.Errorf("sweep sso: %w", err)
		}
		total += tag.RowsAffected()
	}
	return total, nil
}

// ── group mappings ──────────────────────────────────────────────────────────

// SSOGroupMapping maps one IdP group to one Nexus role.
type SSOGroupMapping struct {
	IDPGroup string `json:"idp_group"`
	RoleName string `json:"role_name"`
}

// GatewayListGroupMappings lists a provider's mappings.
func GatewayListGroupMappings(ctx context.Context, q GWQuerier, providerID string) ([]SSOGroupMapping, error) {
	rows, err := q.Query(ctx, `SELECT idp_group, role_name FROM sso_group_mappings WHERE provider_id::text=$1 ORDER BY idp_group, role_name`, providerID)
	if err != nil {
		return nil, fmt.Errorf("list group mappings: %w", err)
	}
	defer rows.Close()
	out := []SSOGroupMapping{}
	for rows.Next() {
		var m SSOGroupMapping
		if err := rows.Scan(&m.IDPGroup, &m.RoleName); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GatewayReplaceGroupMappings replaces a provider's mapping set if its mapping
// revision still matches. Must run in the caller's transaction.
func GatewayReplaceGroupMappings(ctx context.Context, tx pgx.Tx, namespace, providerID string, revision int64, mappings []SSOGroupMapping, actor *string) (int64, error) {
	var next int64
	err := tx.QueryRow(ctx, `UPDATE sso_providers SET mappings_revision = mappings_revision + 1
		 WHERE namespace=$1 AND id::text=$2 AND mappings_revision=$3 RETURNING mappings_revision`,
		namespace, providerID, revision).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrSSOConflict
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sso_group_mappings WHERE provider_id::text=$1`, providerID); err != nil {
		return 0, err
	}
	for _, m := range mappings {
		if _, err := tx.Exec(ctx, `INSERT INTO sso_group_mappings (provider_id, namespace, idp_group, role_name, created_by)
			VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, providerID, namespace, m.IDPGroup, m.RoleName, actor); err != nil {
			return 0, err
		}
	}
	return next, nil
}

// ── identities ──────────────────────────────────────────────────────────────

// UserIdentityRow is an external identity linked to a user.
type UserIdentityRow struct {
	ID          string     `json:"id"`
	ProviderID  string     `json:"provider_id"`
	Provider    string     `json:"provider"`
	Kind        string     `json:"kind"`
	Subject     string     `json:"subject"`
	Email       string     `json:"email"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at"`
}

// GatewayListUserIdentities lists the identities linked to a user.
func GatewayListUserIdentities(ctx context.Context, q GWQuerier, namespace, userID string) ([]UserIdentityRow, error) {
	rows, err := q.Query(ctx, `
		SELECT i.id::text, i.provider_id::text, p.display_name, p.kind, i.subject, i.email, i.created_at, i.last_login_at
		  FROM user_identities i JOIN sso_providers p ON p.id = i.provider_id
		 WHERE i.namespace=$1 AND i.user_id::text=$2 ORDER BY i.created_at`, namespace, userID)
	if err != nil {
		return nil, fmt.Errorf("list identities: %w", err)
	}
	defer rows.Close()
	out := []UserIdentityRow{}
	for rows.Next() {
		var r UserIdentityRow
		if err := rows.Scan(&r.ID, &r.ProviderID, &r.Provider, &r.Kind, &r.Subject, &r.Email, &r.CreatedAt, &r.LastLoginAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GatewayDeleteUserIdentity unlinks one identity from a user.
func GatewayDeleteUserIdentity(ctx context.Context, q GWQuerier, namespace, userID, identityID string) error {
	tag, err := q.Exec(ctx, `DELETE FROM user_identities WHERE namespace=$1 AND user_id::text=$2 AND id::text=$3`, namespace, userID, identityID)
	if err != nil {
		return fmt.Errorf("unlink identity: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ── the login decision ──────────────────────────────────────────────────────

// SSOAssertedIdentity is what a verified ID token or assertion says.
type SSOAssertedIdentity struct {
	Subject       string
	Email         string
	EmailVerified bool
	DisplayName   string
	Groups        []string
}

// SSOLoginResult reports what a login did, for the audit trail and the UI.
type SSOLoginResult struct {
	User            UserRow
	Created         bool
	Linked          bool
	RolesAdded      []string
	RolesRemoved    []string
	SessionsRevoked int64
}

// EmailDomainAllowed reports whether an address's domain is in the list
// (case-insensitive, exact domain; no suffix or wildcard matching).
func EmailDomainAllowed(email string, domains []string) bool {
	at := strings.LastIndexByte(email, '@')
	if at < 1 || at == len(email)-1 {
		return false
	}
	d := strings.ToLower(email[at+1:])
	for _, allowed := range domains {
		if strings.ToLower(strings.TrimSpace(allowed)) == d {
			return true
		}
	}
	return false
}

// GatewaySSOLogin resolves, links or provisions the Nexus user for a verified
// external identity, syncs IdP group roles, and writes the audit trail, in ONE
// transaction under the namespace's RBAC advisory lock (the lock every role and
// assignment write takes), so a concurrent role edit cannot interleave.
//
// Everything happens inside the PROVIDER's namespace. Nothing in the asserted
// identity can select another tenant (ADR-0024 §5).
func GatewaySSOLogin(ctx context.Context, db GWDB, p SSOProviderRow, id SSOAssertedIdentity, clientIP string) (SSOLoginResult, error) {
	var res SSOLoginResult
	id.Email = strings.ToLower(strings.TrimSpace(id.Email))
	tx, err := db.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin sso login: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, p.Namespace); err != nil {
		return res, err
	}
	audit := func(action, outcome string, userID *string, detail map[string]any) error {
		d := map[string]any{"provider_id": p.ID, "provider": p.Slug, "subject": id.Subject}
		for k, v := range detail {
			d[k] = v
		}
		return GatewayAppendAudit(ctx, tx, AuditEntry{Namespace: p.Namespace, ActorID: userID, ActorKind: "user",
			ActorLabel: id.Email, Action: action, TargetKind: "user", Outcome: outcome, Detail: d, ClientIP: clientIP})
	}
	// Failures are audited in their OWN statement after rollback by the caller;
	// here they are returned as typed errors.

	var userID string
	err = tx.QueryRow(ctx, `SELECT user_id::text FROM user_identities WHERE provider_id=$1 AND subject=$2 FOR UPDATE`, p.ID, id.Subject).Scan(&userID)
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		if id.Email == "" {
			return res, ErrSSOEmailMissing
		}
		if !EmailDomainAllowed(id.Email, p.AllowedDomains) {
			return res, ErrSSODomainNotAllowed
		}
		var existing string
		err = tx.QueryRow(ctx, `SELECT id::text FROM users WHERE namespace=$1 AND lower(email)=$2`, p.Namespace, id.Email).Scan(&existing)
		switch {
		case err == nil:
			// Never link on email alone: the tenant must allow it AND the IdP
			// must assert the address is verified.
			if !p.LinkByVerifiedEmail || !id.EmailVerified {
				return res, ErrSSOAccountUnlinked
			}
			userID = existing
			res.Linked = true
		case errors.Is(err, pgx.ErrNoRows):
			if !p.JITProvisioning {
				return res, ErrSSONoAccount
			}
			u, cerr := GatewayCreateUserWithoutPassword(ctx, tx, p.Namespace, id.Email, trimDisplay(id.DisplayName), "reader")
			if cerr != nil {
				return res, cerr
			}
			userID = u.ID
			res.Created = true
			// An explicit, empty binding: a JIT user never falls back to legacy
			// reader permissions. No mapped group means no module access.
			if _, err = tx.Exec(ctx, `INSERT INTO rbac_user_access (user_id, namespace, roles) VALUES ($1,$2,'[]'::jsonb)`, userID, p.Namespace); err != nil {
				return res, err
			}
			if id.EmailVerified {
				if _, err = tx.Exec(ctx, `UPDATE users SET email_verified_at = now() WHERE id=$1 AND email_verified_at IS NULL`, userID); err != nil {
					return res, err
				}
			}
		default:
			return res, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO user_identities (user_id, provider_id, namespace, subject, email) VALUES ($1,$2,$3,$4,$5)`,
			userID, p.ID, p.Namespace, id.Subject, id.Email); err != nil {
			return res, fmt.Errorf("link identity: %w", err)
		}
	default:
		return res, err
	}

	var u UserRow
	err = tx.QueryRow(ctx, `SELECT id::text, namespace, email, display_name, role, created_at, disabled_at, email_verified_at
		  FROM users WHERE id=$1 AND namespace=$2 FOR UPDATE`, userID, p.Namespace).
		Scan(&u.ID, &u.Namespace, &u.Email, &u.DisplayName, &u.Role, &u.CreatedAt, &u.DisabledAt, &u.EmailVerifiedAt)
	if err != nil {
		return res, fmt.Errorf("load sso user: %w", err)
	}
	if u.DisabledAt != nil {
		return res, ErrSSOUserDisabled
	}
	if dn := trimDisplay(id.DisplayName); dn != "" && dn != u.DisplayName {
		if _, err = tx.Exec(ctx, `UPDATE users SET display_name=$2 WHERE id=$1`, u.ID, dn); err != nil {
			return res, err
		}
		u.DisplayName = dn
	}
	if _, err = tx.Exec(ctx, `UPDATE user_identities SET last_login_at=now(), email=$3 WHERE provider_id=$1 AND subject=$2`, p.ID, id.Subject, id.Email); err != nil {
		return res, err
	}

	if err = syncIDPRoles(ctx, tx, p, u, id.Groups, &res); err != nil {
		return res, err
	}
	res.User = u
	if res.Created {
		if err = audit("user.sso_provisioned", AuditSuccess, &u.ID, map[string]any{"email": u.Email}); err != nil {
			return res, err
		}
	}
	if res.Linked {
		if err = audit("user.sso_linked", AuditSuccess, &u.ID, map[string]any{"email": u.Email, "reason": "verified email, linking allowed"}); err != nil {
			return res, err
		}
	}
	if len(res.RolesAdded) > 0 || len(res.RolesRemoved) > 0 {
		if err = audit("user.roles.idp_sync", AuditSuccess, &u.ID, map[string]any{
			"added": res.RolesAdded, "removed": res.RolesRemoved, "sessions_revoked": res.SessionsRevoked,
		}); err != nil {
			return res, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit sso login: %w", err)
	}
	return res, nil
}

func trimDisplay(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// syncIDPRoles replaces the IdP-sourced roles of a user with those its current
// groups map to. Administrator accounts are left alone: their authority comes
// from the protected system role, never from an IdP group.
//
// A provider with NO mappings does not touch roles at all, so enabling SSO for
// a tenant that manages roles by hand changes nothing.
func syncIDPRoles(ctx context.Context, tx pgx.Tx, p SSOProviderRow, u UserRow, groups []string, res *SSOLoginResult) error {
	if u.Role == "admin" {
		return nil
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM sso_group_mappings WHERE provider_id=$1`, p.ID).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	if groups == nil {
		groups = []string{}
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT role_name FROM sso_group_mappings WHERE provider_id=$1 AND idp_group = ANY($2) ORDER BY role_name`, p.ID, groups)
	if err != nil {
		return err
	}
	mapped := []string{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return err
		}
		mapped = append(mapped, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var raw []byte
	err = tx.QueryRow(ctx, `SELECT idp_roles FROM rbac_user_access WHERE user_id=$1 FOR UPDATE`, u.ID).Scan(&raw)
	var previous []string
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// A linked legacy account: from now on the IdP and explicit bindings
		// govern it, never the legacy reader fallback.
		if _, err = tx.Exec(ctx, `INSERT INTO rbac_user_access (user_id, namespace, roles, idp_roles) VALUES ($1,$2,'[]'::jsonb,'[]'::jsonb)`, u.ID, p.Namespace); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(raw, &previous); err != nil {
			return err
		}
	}
	added, removed := diffSets(previous, mapped)
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE rbac_user_access SET idp_roles=$2::jsonb, revision=revision+1 WHERE user_id=$1`, u.ID, jsonList(mapped)); err != nil {
		return err
	}
	res.RolesAdded, res.RolesRemoved = added, removed
	if len(removed) > 0 && p.RevokeSessionsOnRoleLoss {
		tag, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, u.ID)
		if err != nil {
			return err
		}
		res.SessionsRevoked = tag.RowsAffected()
	}
	return nil
}

func diffSets(before, after []string) (added, removed []string) {
	b, a := map[string]bool{}, map[string]bool{}
	for _, x := range before {
		b[x] = true
	}
	for _, x := range after {
		a[x] = true
	}
	for x := range a {
		if !b[x] {
			added = append(added, x)
		}
	}
	for x := range b {
		if !a[x] {
			removed = append(removed, x)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// GatewayCreateSSOSession creates a session and records which provider minted it.
func GatewayCreateSSOSession(ctx context.Context, q GWQuerier, userID, providerID, nameID, sessionIndex string, ttl time.Duration, userAgent, clientIP string) (string, SessionRow, error) {
	token, row, err := GatewayCreateSession(ctx, q, userID, ttl, userAgent, clientIP)
	if err != nil {
		return "", row, err
	}
	if _, err := q.Exec(ctx, `UPDATE sessions SET sso_provider_id=$2, sso_name_id=NULLIF($3,''), sso_session_index=NULLIF($4,'') WHERE id=$1`,
		row.ID, providerID, nameID, sessionIndex); err != nil {
		return "", row, fmt.Errorf("record sso session provider: %w", err)
	}
	return token, row, nil
}

// GatewaySessionSSO reads the SSO details of a session, for logout.
func GatewaySessionSSO(ctx context.Context, q GWQuerier, sessionID string) (providerID, nameID, sessionIndex string, err error) {
	err = q.QueryRow(ctx, `SELECT COALESCE(sso_provider_id::text,''), COALESCE(sso_name_id,''), COALESCE(sso_session_index,'') FROM sessions WHERE id=$1`, sessionID).
		Scan(&providerID, &nameID, &sessionIndex)
	return
}
