// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Identity extensions to the MVP authenticator: human sessions and scoped
// tokens, added ALONGSIDE static API keys rather than in place of them.
//
// The compatibility requirement is absolute. Every worker in the fleet and every
// CLI invocation presents an API key, so the key path must behave exactly as it
// did before this file existed -- same resolution, same opaque failure, same
// Principal. What follows only adds credential KINDS that the same enforcement
// point resolves, and leaves `permitted` as the single source of truth about what
// a role may do.
package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// PrincipalKind is how a caller proved who it is.
//
// Recorded on the Principal because the AUDIT trail must distinguish "a person
// clicked this" from "a machine credential did this" -- an admin action from a
// browser session and the same action from a long-lived CI key are different
// facts during an incident review, and without this they are indistinguishable.
type PrincipalKind string

const (
	// KindAPIKey is a static key from the api_keys table: the worker and CLI
	// credential, unchanged by this file.
	KindAPIKey PrincipalKind = "api_key"
	// KindSession is a human logged in through the console.
	KindSession PrincipalKind = "session"
	// KindScopedToken is a narrow, expiring machine credential.
	KindScopedToken PrincipalKind = "token"
)

// SessionTTL is how long a console login lasts.
//
// Twelve hours is a working day: long enough that an operator is not re-
// authenticating during an incident, short enough that a forgotten session on a
// shared machine dies the same day. It is deliberately NOT weeks -- a session is
// the credential most likely to be left behind on a laptop.
const SessionTTL = 12 * time.Hour

// IdentityStore is the identity surface the authenticator needs.
//
// An interface rather than the concrete store so the gateway's tests can drive
// login, expiry and revocation without a database, and so it is visible exactly
// which identity operations authentication depends on.
type IdentityStore interface {
	ResolveSession(ctx context.Context, presented string) (store.ResolvedSession, error)
	ResolveScopedToken(ctx context.Context, presented string) (store.ResolvedToken, error)
	TouchSession(ctx context.Context, sessionID string) error
	TouchScopedToken(ctx context.Context, tokenID string) error
}

// StoreIdentity adapts the pgx store to IdentityStore.
type StoreIdentity struct {
	DB store.GWQuerier
}

func (s StoreIdentity) ResolveSession(ctx context.Context, presented string) (store.ResolvedSession, error) {
	return store.GatewayResolveSession(ctx, s.DB, presented)
}

func (s StoreIdentity) ResolveScopedToken(ctx context.Context, presented string) (store.ResolvedToken, error) {
	return store.GatewayResolveScopedToken(ctx, s.DB, presented)
}

func (s StoreIdentity) TouchSession(ctx context.Context, sessionID string) error {
	return store.GatewayTouchSession(ctx, s.DB, sessionID)
}

func (s StoreIdentity) TouchScopedToken(ctx context.Context, tokenID string) error {
	return store.GatewayTouchScopedToken(ctx, s.DB, tokenID)
}

// WithIdentity enables session and scoped-token authentication.
//
// Opt-in, and nil-safe when absent: an Authenticator built without it resolves
// API keys and nothing else, which is exactly the pre-existing behaviour. That is
// what lets this ship without a flag day, and what makes the identity feature
// removable by not wiring it.
func (a *Authenticator) WithIdentity(ids IdentityStore) *Authenticator {
	a.identity = ids
	return a
}

// AuthenticateCredential resolves any supported credential to a Principal.
//
// ORDER MATTERS, and it is decided by the credential's PREFIX rather than by
// trying each store in turn. Probing three tables for every request would triple
// the authentication cost of the worker RPC path -- the hottest path in the
// system -- to serve console logins, which are rare. The prefixes are minted by
// store.GenerateSecret precisely so this dispatch is one string comparison.
//
// A credential with no recognised prefix falls through to the API-key lookup,
// because existing keys were minted before prefixes existed (`make seed` writes
// `nxs_...`, and operators hold older shapes). Falling back rather than rejecting
// is what keeps every credential already in the field working.
//
// Every failure is still ONE opaque ErrUnauthenticated, for the reason
// Authenticate documents: distinguishing them turns this into an oracle.
func (a *Authenticator) AuthenticateCredential(ctx context.Context, presented string) (Principal, error) {
	if presented == "" {
		return Principal{}, ErrUnauthenticated
	}

	switch {
	case strings.HasPrefix(presented, "nxsess_"):
		p, err := a.authenticateSession(ctx, presented)
		if err != nil {
			return p, err
		}
		return a.resolveRBAC(ctx, p)
	case strings.HasPrefix(presented, "nxtok_"):
		return a.authenticateScopedToken(ctx, presented)
	default:
		// The unchanged API-key path. A worker's request reaches exactly the same
		// code it did before identity existed.
		return a.Authenticate(ctx, presented)
	}
}

func (a *Authenticator) authenticateSession(ctx context.Context, presented string) (Principal, error) {
	if a.identity == nil {
		// Identity is not wired, so a session token is simply not a credential this
		// gateway accepts. Opaque, like every other authn failure.
		return Principal{}, ErrUnauthenticated
	}
	rs, err := a.identity.ResolveSession(ctx, presented)
	if errors.Is(err, store.ErrIdentityUnknown) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		// A database fault must surface as 503, not 401 (see Authenticate): telling
		// every console user to log in again during an incident turns a database
		// blip into a support queue.
		return Principal{}, fmt.Errorf("resolve session: %w", err)
	}

	role := Role(rs.User.Role)
	if _, known := permitted[role]; !known {
		// Fail closed on an unknown role string, exactly as the API-key path does.
		return Principal{}, ErrUnauthenticated
	}
	if role == RoleWorker {
		// A human must never hold the worker role. The worker credential exists to
		// speak the worker protocol from an execution host; a browser session with
		// it would be a console that can poll for tasks. The schema discourages this
		// and the enforcement point refuses it.
		return Principal{}, ErrUnauthenticated
	}

	// Best-effort usage recording. A failure here must NOT fail the request: the
	// caller is properly authenticated, and refusing them because a bookkeeping
	// write failed would turn a cosmetic column into an availability dependency.
	if err := a.identity.TouchSession(ctx, rs.SessionID); err != nil {
		a.touchFailed(ctx, "session", rs.SessionID, err)
	}

	return Principal{
		KeyID:     rs.SessionID,
		Namespace: rs.User.Namespace,
		Name:      rs.User.Email,
		Role:      role,
		Kind:      KindSession,
		UserID:    rs.User.ID,
		// Which IdP minted the session, so logout can offer the IdP's logout.
		SSOProviderID: rs.SSOProviderID,
		// A session carries no scope narrowing: a logged-in human has their role's
		// full authority. Scopes exist to narrow MACHINE credentials, which are the
		// ones that sit unattended in a CI config.
	}, nil
}

func (a *Authenticator) authenticateScopedToken(ctx context.Context, presented string) (Principal, error) {
	if a.identity == nil {
		return Principal{}, ErrUnauthenticated
	}
	rt, err := a.identity.ResolveScopedToken(ctx, presented)
	if errors.Is(err, store.ErrIdentityUnknown) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("resolve scoped token: %w", err)
	}

	role := Role(rt.Role)
	if _, known := permitted[role]; !known {
		return Principal{}, ErrUnauthenticated
	}

	if err := a.identity.TouchScopedToken(ctx, rt.TokenID); err != nil {
		a.touchFailed(ctx, "token", rt.TokenID, err)
	}

	p := Principal{
		KeyID:     rt.TokenID,
		Namespace: rt.Namespace,
		Name:      rt.Name,
		Role:      role,
		Kind:      KindScopedToken,
		Scopes:    parseScopes(rt.Scopes),
	}
	return a.applyTokenCeiling(ctx, p, rt)
}

// applyTokenCeiling turns a service-account token or a ceilinged token into a
// grant-bearing principal (ADR-0024 §8).
//
//   - Service-account token: grants = (the account's CURRENT roles) ∩ ceiling.
//     Read on every request, so narrowing the account's roles narrows every
//     token at once; disabling it is handled by the resolve query.
//   - Legacy token with a ceiling: grants = (legacy role/scope allowances) ∩
//     ceiling.
//
// The ceiling only ever removes permissions. A principal with Grants set can
// never speak the worker protocol (Authorize refuses it).
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
			// No binding: the account holds nothing.
		case err != nil:
			return Principal{}, fmt.Errorf("resolve service account roles: %w", err)
		default:
			if err := json.Unmarshal(raw, &names); err != nil {
				return Principal{}, fmt.Errorf("decode service account roles: %w", err)
			}
		}
		p.RoleNames = names
		base, err = grantsFor(ctx, a.db, rt.Namespace, names, true)
		if err != nil {
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
	if rt.Permissions != nil {
		for _, g := range rt.Permissions {
			if base[g] && Delegable(g) {
				grants[g] = true
			}
		}
	} else {
		for g := range base {
			if Delegable(g) {
				grants[g] = true
			}
		}
	}
	p.Grants = grants
	return p, nil
}

// parseScopes converts stored scope strings to Actions, DISCARDING any the
// authorization matrix does not know.
//
// Dropping an unknown scope rather than keeping it is what makes a typo'd scope
// grant nothing instead of something unintended. Combined with Authorize's
// intersection, a token can only ever hold a subset of real, enforced actions.
func parseScopes(raw []string) []Action {
	if raw == nil {
		// Genuinely unscoped: the credential is limited only by its role. Only an
		// API key reaches this, because the scoped_tokens column defaults to '[]'.
		return nil
	}
	// An EMPTY list is not nil. It means "explicitly scoped to nothing", and it
	// must stay non-nil all the way to HasScope: collapsing it to nil would hand a
	// token created with no usable scopes its role's full authority, which is the
	// opposite of what its creator asked for.
	known := map[Action]bool{
		ActionRead: true, ActionWrite: true,
		ActionAdminWrite: true, ActionWorkerRPC: true,
	}
	out := make([]Action, 0, len(raw))
	for _, s := range raw {
		act := Action(strings.TrimSpace(s))
		if known[act] {
			out = append(out, act)
		}
	}
	return out
}
