// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package rest

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/sso"
)

// SSO administration: providers, group mappings and linked identities
// (ADR-0024). Guarded by sso.view / sso.configure / sso.mappings.update and
// users.view / users.update through routePermissions.

var (
	ssoSlug       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	ssoScope      = regexp.MustCompile(`^[\x21\x23-\x5B\x5D-\x7E]{1,128}$`) // RFC 6749 scope-token
	ssoClaimName  = regexp.MustCompile(`^[A-Za-z0-9_:./#-]{1,128}$`)
	ssoDomainName = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
)

type ssoProviderBody struct {
	Kind                     string   `json:"kind"`
	Slug                     string   `json:"slug"`
	DisplayName              string   `json:"display_name"`
	Enabled                  bool     `json:"enabled"`
	Issuer                   string   `json:"issuer"`
	ClientID                 string   `json:"client_id"`
	ClientSecretRef          string   `json:"client_secret_ref"`
	Scopes                   []string `json:"scopes"`
	IDPMetadataXML           string   `json:"idp_metadata_xml"`
	AllowedDomains           []string `json:"allowed_domains"`
	JITProvisioning          bool     `json:"jit_provisioning"`
	LinkByVerifiedEmail      bool     `json:"link_by_verified_email"`
	GroupsClaim              string   `json:"groups_claim"`
	RevokeSessionsOnRoleLoss bool     `json:"revoke_sessions_on_role_loss"`
	Revision                 int64    `json:"revision"`
}

// validateProvider turns a request into store input, returning every problem
// at once like the other admin forms.
func (a *API) validateProvider(ctx context.Context, ns string, b ssoProviderBody) (store.SSOProviderInput, []string) {
	var problems []string
	in := store.SSOProviderInput{
		Kind: b.Kind, Slug: strings.TrimSpace(b.Slug), DisplayName: strings.TrimSpace(b.DisplayName), Enabled: b.Enabled,
		JITProvisioning: b.JITProvisioning, LinkByVerifiedEmail: b.LinkByVerifiedEmail,
		GroupsClaim: strings.TrimSpace(b.GroupsClaim), RevokeSessionsOnRoleLoss: b.RevokeSessionsOnRoleLoss,
	}
	if in.GroupsClaim == "" {
		in.GroupsClaim = "groups"
	}
	if !ssoSlug.MatchString(in.Slug) {
		problems = append(problems, "slug must be 1–63 lower-case letters, digits or dashes")
	}
	if in.DisplayName == "" || len(in.DisplayName) > 80 {
		problems = append(problems, "display_name is required (at most 80 characters); it is the sign-in button label")
	}
	if !ssoClaimName.MatchString(in.GroupsClaim) {
		problems = append(problems, "groups_claim must be a claim or attribute name")
	}
	domains := []string{}
	for _, d := range b.AllowedDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if !ssoDomainName.MatchString(d) {
			problems = append(problems, "allowed domain "+strconv.Quote(d)+" is not a domain name")
			continue
		}
		domains = append(domains, d)
	}
	if len(domains) > 50 {
		problems = append(problems, "at most 50 allowed domains")
	}
	in.AllowedDomains = domains
	if (in.JITProvisioning || in.LinkByVerifiedEmail) && len(domains) == 0 {
		problems = append(problems, "allowed_domains is required when just-in-time provisioning or email linking is on")
	}
	cfg := a.sso.cfg
	switch b.Kind {
	case "oidc":
		in.Issuer = strings.TrimRight(strings.TrimSpace(b.Issuer), "/")
		if strings.HasSuffix(strings.TrimSpace(b.Issuer), "/") && !strings.HasSuffix(in.Issuer, "/") {
			// Issuers are compared byte-for-byte (OIDC Discovery §4.3); keep
			// the administrator's exact value.
			in.Issuer = strings.TrimSpace(b.Issuer)
		}
		if err := cfg.ValidateIssuer(ctx, in.Issuer); err != nil {
			problems = append(problems, "issuer: "+err.Error())
		}
		in.ClientID = strings.TrimSpace(b.ClientID)
		if in.ClientID == "" || len(in.ClientID) > 256 {
			problems = append(problems, "client_id is required")
		}
		in.ClientSecretRef = strings.TrimSpace(b.ClientSecretRef)
		if err := cfg.ValidateSecretRef(in.ClientSecretRef); err != nil {
			problems = append(problems, "client_secret_ref: "+err.Error())
		}
		in.Scopes = []string{"openid"}
		for _, s := range b.Scopes {
			s = strings.TrimSpace(s)
			if s == "" || s == "openid" {
				continue
			}
			if !ssoScope.MatchString(s) {
				problems = append(problems, "scope "+strconv.Quote(s)+" is not a valid scope token")
				continue
			}
			in.Scopes = append(in.Scopes, s)
		}
		if len(in.Scopes) > 20 {
			problems = append(problems, "at most 20 scopes")
		}
	case "saml":
		md, err := sso.ParseIdPMetadata(b.IDPMetadataXML)
		if err != nil {
			problems = append(problems, "idp_metadata_xml: "+err.Error())
		} else {
			in.IDPMetadataXML = b.IDPMetadataXML
			in.IDPEntityID = md.EntityID
		}
	default:
		problems = append(problems, "kind must be oidc or saml")
	}
	if len(problems) > 0 {
		return in, problems
	}
	// Enabling is a claim that sign-in works: check what can be checked now.
	if in.Enabled && in.Kind == "oidc" {
		if _, err := cfg.ResolveSecret(ns, in.ClientSecretRef); err != nil {
			problems = append(problems, "client_secret_ref does not resolve on this gateway; provision the secret before enabling")
		}
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		probe := store.SSOProviderRow{ID: "probe-" + in.Slug, Issuer: in.Issuer, ClientID: in.ClientID, Revision: time.Now().UnixNano()}
		if _, err := a.sso.oidc.Discover(dctx, probe); err != nil {
			problems = append(problems, "issuer discovery failed: "+truncate(err.Error(), 300))
		}
	}
	return in, problems
}

func (a *API) providerPayload(p store.SSOProviderRow) map[string]any {
	out := map[string]any{
		"id": p.ID, "namespace": p.Namespace, "kind": p.Kind, "slug": p.Slug, "display_name": p.DisplayName,
		"enabled": p.Enabled, "allowed_domains": p.AllowedDomains, "jit_provisioning": p.JITProvisioning,
		"link_by_verified_email": p.LinkByVerifiedEmail, "groups_claim": p.GroupsClaim,
		"revoke_sessions_on_role_loss": p.RevokeSessionsOnRoleLoss, "revision": p.Revision,
		"mappings_revision": p.MappingsRevision, "created_at": p.CreatedAt, "updated_at": p.UpdatedAt,
	}
	cfg := a.sso.cfg
	switch p.Kind {
	case "oidc":
		_, err := cfg.ResolveSecret(p.Namespace, p.ClientSecretRef)
		out["issuer"], out["client_id"], out["scopes"] = p.Issuer, p.ClientID, p.Scopes
		// The reference is shown; the secret never is.
		out["client_secret_ref"] = p.ClientSecretRef
		out["client_secret_resolves"] = err == nil
		out["redirect_uri"] = cfg.OIDCRedirectURI(p.ID)
		out["post_logout_redirect_uri"] = cfg.PostLogoutRedirect()
	case "saml":
		out["idp_entity_id"] = p.IDPEntityID
		out["idp_metadata_xml"] = p.IDPMetadataXML
		out["sp_entity_id"] = cfg.SAMLEntityID(p.ID)
		out["acs_url"] = cfg.SAMLACSURL(p.ID)
		out["slo_url"] = cfg.SAMLSLOURL(p.ID)
		out["metadata_url"] = cfg.SAMLEntityID(p.ID)
	}
	return out
}

func (a *API) ssoListProviders(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.ssoReady(w) {
		return
	}
	rows, err := store.GatewayListSSOProviders(r.Context(), a.db, p.Namespace)
	if err != nil {
		a.fail(w, r, "list sso providers", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, a.providerPayload(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out, "public_url": a.sso.cfg.PublicURL})
}

func (a *API) ssoCreateProvider(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	a.ssoSaveProvider(w, r, p, "")
}

func (a *API) ssoUpdateProvider(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	a.ssoSaveProvider(w, r, p, r.PathValue("id"))
}

func (a *API) ssoSaveProvider(w http.ResponseWriter, r *http.Request, p authn.Principal, id string) {
	if !a.ssoReady(w) {
		return
	}
	var b ssoProviderBody
	if err := decodeLimited(w, r, &b, 600<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "Invalid provider")
		return
	}
	if id != "" {
		current, err := store.GatewayGetSSOProvider(r.Context(), a.db, p.Namespace, id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "No such provider")
			return
		}
		if err != nil {
			a.fail(w, r, "sso provider", err)
			return
		}
		b.Kind = current.Kind // the protocol of a provider never changes
		if b.Kind == "saml" && b.IDPMetadataXML == "" {
			b.IDPMetadataXML = current.IDPMetadataXML
		}
	}
	in, problems := a.validateProvider(r.Context(), p.Namespace, b)
	if len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "invalid_provider", "message": "provider could not be saved", "problems": problems})
		return
	}
	var row store.SSOProviderRow
	var err error
	action := "sso.provider.update"
	if id == "" {
		action = "sso.provider.create"
		var creator *string
		if p.UserID != "" {
			creator = &p.UserID
		}
		row, err = store.GatewayCreateSSOProvider(r.Context(), a.db, p.Namespace, in, creator)
	} else {
		row, err = store.GatewayUpdateSSOProvider(r.Context(), a.db, p.Namespace, id, b.Revision, in)
	}
	switch {
	case errors.Is(err, store.ErrSSODuplicateProvider):
		writeError(w, http.StatusConflict, "duplicate_provider", err.Error())
		return
	case errors.Is(err, store.ErrSSOConflict):
		writeError(w, http.StatusConflict, "conflict", "Provider changed; reload before saving")
		return
	case err != nil:
		a.fail(w, r, "save sso provider", err)
		return
	}
	a.auditPrincipal(r, p, action, "sso_provider", row.ID, store.AuditSuccess, map[string]any{
		"kind": row.Kind, "slug": row.Slug, "enabled": row.Enabled, "issuer": row.Issuer, "client_id": row.ClientID,
		"idp_entity_id": row.IDPEntityID, "allowed_domains": row.AllowedDomains, "jit": row.JITProvisioning,
		"link_by_verified_email": row.LinkByVerifiedEmail, "revision": row.Revision,
	})
	status := http.StatusOK
	if id == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, a.providerPayload(row))
}

func (a *API) ssoDeleteProvider(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.ssoReady(w) {
		return
	}
	rev, err := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if err != nil || rev < 1 {
		writeError(w, http.StatusBadRequest, "revision_required", "Supply the current provider revision")
		return
	}
	id := r.PathValue("id")
	if err := store.GatewayDeleteSSOProvider(r.Context(), a.db, p.Namespace, id, rev); err != nil {
		if errors.Is(err, store.ErrSSOConflict) {
			writeError(w, http.StatusConflict, "conflict", "Provider changed or does not exist; reload")
			return
		}
		a.fail(w, r, "delete sso provider", err)
		return
	}
	a.auditPrincipal(r, p, "sso.provider.delete", "sso_provider", id, store.AuditSuccess, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) ssoGetMappings(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.ssoReady(w) {
		return
	}
	prov, err := store.GatewayGetSSOProvider(r.Context(), a.db, p.Namespace, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "No such provider")
		return
	}
	if err != nil {
		a.fail(w, r, "sso mappings", err)
		return
	}
	m, err := store.GatewayListGroupMappings(r.Context(), a.db, prov.ID)
	if err != nil {
		a.fail(w, r, "sso mappings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mappings": m, "revision": prov.MappingsRevision, "groups_claim": prov.GroupsClaim})
}

// ssoPutMappings replaces a provider's group→role mappings. Mapping a group to
// a role hands that role to everyone the IdP puts in the group, so it passes
// the same delegation rule as an assignment: the actor must hold every grant
// of every mapped role, and Administrator cannot be mapped.
func (a *API) ssoPutMappings(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if !a.ssoReady(w) {
		return
	}
	var b struct {
		Mappings []store.SSOGroupMapping `json:"mappings"`
		Revision int64                   `json:"revision"`
	}
	if decodeLimited(w, r, &b, 128<<10) != nil || len(b.Mappings) > 200 {
		writeError(w, http.StatusBadRequest, "invalid", "Invalid mappings (at most 200)")
		return
	}
	id := r.PathValue("id")
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		a.fail(w, r, "sso mappings", err)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, p.Namespace); err != nil {
		a.fail(w, r, "role lock", err)
		return
	}
	if _, err := store.GatewayGetSSOProvider(r.Context(), tx, p.Namespace, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "No such provider")
			return
		}
		a.fail(w, r, "sso mappings", err)
		return
	}
	var problems []string
	roles := map[string]bool{}
	clean := make([]store.SSOGroupMapping, 0, len(b.Mappings))
	for _, m := range b.Mappings {
		m.IDPGroup, m.RoleName = strings.TrimSpace(m.IDPGroup), strings.TrimSpace(m.RoleName)
		if m.IDPGroup == "" || len(m.IDPGroup) > 256 {
			problems = append(problems, "each mapping needs an IdP group of at most 256 characters")
			continue
		}
		if strings.EqualFold(m.RoleName, authn.AdministratorRole) {
			problems = append(problems, "Administrator cannot be granted by an IdP group; map a custom role instead")
			continue
		}
		roles[m.RoleName] = true
		clean = append(clean, m)
	}
	if len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "invalid_mappings", "message": "mappings could not be saved", "problems": problems})
		return
	}
	names := make([]string, 0, len(roles))
	for n := range roles {
		names = append(names, n)
	}
	if !a.checkAssignableRoles(w, r, tx, p, names) {
		return
	}
	var actor *string
	if p.UserID != "" {
		actor = &p.UserID
	}
	next, err := store.GatewayReplaceGroupMappings(r.Context(), tx, p.Namespace, id, b.Revision, clean, actor)
	if errors.Is(err, store.ErrSSOConflict) {
		writeError(w, http.StatusConflict, "conflict", "Mappings changed; reload before saving")
		return
	}
	if err == nil {
		err = auditTx(r, tx, p, "sso.mappings.update", "sso_provider", id, map[string]any{"mappings": clean, "previous_revision": b.Revision})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		a.fail(w, r, "sso mappings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mappings": clean, "revision": next})
}

func (a *API) listUserIdentities(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	rows, err := store.GatewayListUserIdentities(r.Context(), a.db, p.Namespace, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, "list identities", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"identities": rows})
}

func (a *API) unlinkUserIdentity(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	user := r.PathValue("id")
	if a.targetCheck(w, r, p, "user.sso_unlinked", user) {
		return
	}
	identity := r.PathValue("identity")
	if err := store.GatewayDeleteUserIdentity(r.Context(), a.db, p.Namespace, user, identity); err != nil {
		a.fail(w, r, "unlink identity", err)
		return
	}
	a.auditPrincipal(r, p, "user.sso_unlinked", "user", user, store.AuditSuccess, map[string]any{"identity_id": identity})
	w.WriteHeader(http.StatusNoContent)
}

// decodeLimited decodes one strict JSON object with a size bound.
func decodeLimited(w http.ResponseWriter, r *http.Request, out any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	body, err := readBody(r, limit)
	if err != nil {
		return err
	}
	return jsonUnmarshalStrict(body, out)
}
