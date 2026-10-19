// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package rest

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/khuriwalgroup/kgf-nexus/internal/kgfctx"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// AuthMode selects what the REST listener accepts (ADR-0024 §1).
type AuthMode string

const (
	// AuthStandalone: Nexus credentials only (sessions, API keys, tokens).
	// The default; every deployment that predates KGF mode.
	AuthStandalone AuthMode = "standalone"
	// AuthKGF: people arrive only through KGF contexts; Nexus sessions and
	// the password, registration and email routes are off. Machine
	// credentials (API keys, scoped and service-account tokens) still work.
	AuthKGF AuthMode = "kgf"
	// AuthBoth: KGF contexts and every Nexus credential.
	AuthBoth AuthMode = "both"
)

// ParseAuthMode reads NEXUS_AUTH_MODE; empty is standalone.
func ParseAuthMode(s string) (AuthMode, error) {
	switch AuthMode(strings.ToLower(strings.TrimSpace(s))) {
	case "", AuthStandalone:
		return AuthStandalone, nil
	case AuthKGF:
		return AuthKGF, nil
	case AuthBoth:
		return AuthBoth, nil
	}
	return "", fmt.Errorf("NEXUS_AUTH_MODE %q: want standalone, kgf or both", s)
}

// WithKGF enables KGF context authentication. A nil authenticator with a mode
// other than standalone refuses every context (fail closed).
func (a *API) WithKGF(mode AuthMode, k *authn.KGFAuthenticator) *API {
	a.authMode = mode
	a.kgf = k
	return a
}

func (a *API) mode() AuthMode {
	if a.authMode == "" {
		return AuthStandalone
	}
	return a.authMode
}

// kgfEnabled reports whether KGF contexts are accepted at all.
func (a *API) kgfEnabled() bool { return a.mode() != AuthStandalone }

// passwordRoutesEnabled reports whether Nexus's own human sign-in routes are
// served. In KGF mode people are KGF's.
func (a *API) passwordRoutesEnabled() bool { return a.mode() != AuthKGF }

var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// authenticateRequest resolves the caller from either the KGF context or the
// bearer credential, and never from both. handled is true when it has already
// written the response.
func (a *API) authenticateRequest(w http.ResponseWriter, r *http.Request, presented string) (authn.Principal, error, bool) {
	ctxToken := r.Header.Get(kgfctx.Header)
	if ctxToken == "" {
		// A Nexus human session is not a credential in KGF mode: people sign
		// in through KGF there, and a leftover session must not bypass that.
		if a.mode() == AuthKGF && strings.HasPrefix(presented, "nxsess_") {
			return authn.Principal{}, authn.ErrUnauthenticated, false
		}
		p, err := a.auth.AuthenticateCredential(r.Context(), presented)
		return p, err, false
	}
	if !a.kgfEnabled() && a.kgfAttribution != nil {
		// Standalone with KGF's keys configured (ADR-0025 §2): the context is
		// ATTRIBUTION only. The Nexus credential alone authenticates and
		// authorizes; attachKGF records the context if it verifies.
		p, err := a.auth.AuthenticateCredential(r.Context(), presented)
		return p, err, false
	}
	if !a.kgfEnabled() {
		// Ignoring the header would authenticate the caller as whatever bearer
		// it also sent while it believed it was acting as a KGF principal.
		writeError(w, http.StatusBadRequest, "kgf_context_not_accepted",
			"this Nexus is not configured for KGF context authentication (NEXUS_AUTH_MODE=standalone)")
		return authn.Principal{}, nil, true
	}
	if presented != "" || r.Header.Get("Authorization") != "" {
		// kgf-gateway strips the customer's Authorization when it forwards to
		// a provider that trusts the context, so both together is a caller
		// that is not the gateway, and resolving one silently would pick an
		// identity the caller may not have meant.
		writeError(w, http.StatusBadRequest, "ambiguous_credentials",
			"send either an X-KGF-Context or an Authorization credential, not both")
		return authn.Principal{}, nil, true
	}
	if a.authAttemptBlocked(w, r) {
		return authn.Principal{}, nil, true
	}
	if a.kgf == nil {
		return authn.Principal{}, authn.ErrUnauthenticated, false
	}
	p, err := a.kgf.Authenticate(r.Context(), ctxToken)
	switch {
	case errors.Is(err, authn.ErrKGFScopeRequired):
		writeError(w, http.StatusForbidden, "kgf_scope_required",
			"the KGF context names no subscription and resource group; Nexus work lives in a resource group")
		return p, nil, true
	case errors.Is(err, authn.ErrKGFServiceNotAllowed):
		writeError(w, http.StatusForbidden, "kgf_service_not_allowed",
			"this KGF service is not allowed to call Nexus for itself (NEXUS_KGF_SERVICE_PRINCIPALS)")
		return p, nil, true
	case errors.Is(err, authn.ErrKGFResourceInvalid):
		writeError(w, http.StatusForbidden, "kgf_resource_invalid",
			"the KGF context's res is not a KGF.Nexus workspace id in the context's own subscription and resource group")
		return p, nil, true
	case errors.Is(err, authn.ErrKGFResourceNotAllowed):
		writeError(w, http.StatusForbidden, "kgf_resource_not_allowed",
			"only a KGF provider front (NEXUS_KGF_FRONT_ISSUERS) may name a workspace in a KGF context")
		return p, nil, true
	case errors.Is(err, authn.ErrKGFResourceRequired):
		writeError(w, http.StatusForbidden, "kgf_resource_required",
			"a KGF context from a provider front must name the workspace it is for (res)")
		return p, nil, true
	case errors.Is(err, authn.ErrKGFFrontShape):
		writeError(w, http.StatusForbidden, "kgf_front_context_invalid",
			"a KGF context from a provider front must be a service context, delegated (obo) or the provisioner")
		return p, nil, true
	case errors.Is(err, authn.ErrWorkspaceNotProvisioned):
		writeError(w, http.StatusNotFound, "workspace_not_provisioned",
			"this workspace is not provisioned, or it was deleted")
		return p, nil, true
	case err == nil && p.KGF != nil && safeRequestID.MatchString(p.KGF.RequestID):
		// One identifier follows the request across KGF and Nexus (ADR-0024 §5).
		w.Header().Set(kgfctx.RequestIDHeader, p.KGF.RequestID)
	}
	if err != nil && errors.Is(err, authn.ErrUnauthenticated) {
		a.log.InfoContext(r.Context(), "kgf context refused", "reason", err.Error())
	}
	return p, err, false
}
