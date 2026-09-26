// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package authn implements the MVP authentication and authorization for the
// gateway: static API keys, hashed at rest, with three roles.
//
// The gateway is the ONLY ingress (architecture §3.2), so this package is the
// single place any request is authenticated. No other service authenticates
// end-user traffic, which is what makes "who may do what" answerable by reading
// one file.
package authn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// Role is the coarse capability grant. Three roles rather than a full policy
// engine is a deliberate MVP scope choice; the enforcement point is centralized
// here so replacing the decision with real RBAC later touches one function.
type Role string

const (
	// RoleAdmin may mutate: submit specs, create and control runs, drain workers.
	RoleAdmin Role = "admin"
	// RoleWorker may ONLY speak the worker gRPC protocol. Deliberately unable to
	// read the REST surface: a worker credential lives on every execution host,
	// which is the least trustworthy place in the system, so it must not be able
	// to enumerate other namespaces' runs if a host is compromised.
	RoleWorker Role = "worker"
	// RoleReader may read everything in its namespace and mutate nothing.
	RoleReader Role = "reader"
)

// Action is the operation being attempted.
type Action string

const (
	ActionRead       Action = "read"
	ActionWrite      Action = "write"
	ActionWorkerRPC  Action = "worker_rpc"
	ActionAdminWrite Action = "admin_write"
)

// Principal is an authenticated caller.
//
// The first four fields are the original API-key principal and are unchanged: a
// worker or CLI request produces exactly the Principal it always did. The rest
// describe credential kinds that did not exist before (sessions, scoped tokens)
// and are ZERO for an API key, which is what makes the addition invisible to
// every existing caller and handler.
type Principal struct {
	Grants    map[string]bool
	RoleNames []string
	KeyID     string
	Namespace string
	Name      string
	Role      Role

	// Kind is how the caller proved identity. Empty means an API key, so the zero
	// Principal keeps behaving as one.
	Kind PrincipalKind
	// UserID is set only for a human session, and is what lets an audit row say
	// WHICH PERSON acted rather than merely which credential was used.
	UserID string
	// Scopes NARROWS what this credential may do within its role. Nil means "no
	// narrowing" -- the role's full grant -- which is why an API key, whose Scopes
	// is always nil, is authorized by exactly the pre-existing rule.
	Scopes []Action

	// ServiceAccountID is set for a service-account token (ADR-0024 §6).
	ServiceAccountID string
	// KGF is set when the caller was authenticated by a KGF context
	// (ADR-0024): the verified KGF identity behind this principal, recorded on
	// every audit row it writes.
	KGF *KGFIdentity
}

// HasScope reports whether the credential's scopes permit an action.
//
// A nil scope list means unscoped, not empty: an unscoped credential is limited
// only by its role. Treating nil as "permits nothing" would refuse every API key
// in the fleet the moment this field was introduced.
func (p Principal) HasScope(a Action) bool {
	if p.Scopes == nil {
		return true
	}
	for _, s := range p.Scopes {
		if s == a {
			return true
		}
	}
	return false
}

// ErrUnauthenticated means no usable credential was presented.
var ErrUnauthenticated = errors.New("unauthenticated")

// ErrForbidden means the credential is valid but not permitted here.
var ErrForbidden = errors.New("forbidden")

// permitted is the authorization matrix, stated once as data so no handler can
// hold a divergent opinion about what a role may do.
var permitted = map[Role]map[Action]bool{
	RoleAdmin: {
		ActionRead: true, ActionWrite: true, ActionAdminWrite: true,
		// An admin key may also drive worker RPCs, which makes local development
		// and integration tests possible with one credential.
		ActionWorkerRPC: true,
	},
	RoleWorker: {
		ActionWorkerRPC: true,
	},
	RoleReader: {
		ActionRead: true,
	},
}

// Can reports whether this principal may perform the action.
func (p Principal) Can(a Action) bool {
	return permitted[p.Role][a]
}

// Authorize is the enforcement point. It checks BOTH the role's capability and
// the namespace, because a valid key for namespace A must not act on namespace B:
// namespaces are the tenancy and quota boundary (architecture §3.2), and a
// boundary that is not checked on every request is not a boundary.
func (p Principal) Authorize(namespace string, a Action) error {
	if p.Kind == KindSession && a == ActionWorkerRPC {
		return ErrForbidden
	}
	if p.Grants != nil {
		if namespace != "" && namespace != p.Namespace {
			return ErrForbidden
		}
		if a != ActionRead && a != ActionWrite && a != ActionAdminWrite {
			return ErrForbidden
		}
		return nil // Exact REST permission requirements are checked by the route guard.
	}
	if !p.Can(a) {
		return fmt.Errorf("%w: role %s may not %s", ErrForbidden, p.Role, a)
	}
	// Scopes INTERSECT with the role and never extend it: the role is checked
	// first and independently, so a scope naming an action the role lacks grants
	// nothing. That ordering is what makes scoped tokens safe to add to an
	// enforcement point that already works -- the worst a bad scope list can do is
	// refuse a request.
	if !p.HasScope(a) {
		return fmt.Errorf("%w: credential is not scoped for %s", ErrForbidden, a)
	}
	if namespace != "" && namespace != p.Namespace {
		return fmt.Errorf("%w: key is scoped to namespace %s, not %s", ErrForbidden, p.Namespace, namespace)
	}
	return nil
}

// Authenticator resolves credentials against the api_keys table, and — when
// identity is wired via WithIdentity — against sessions and scoped tokens too.
type Authenticator struct {
	rbac bool
	db   store.GWQuerier
	// identity is nil unless WithIdentity was called. Nil means this gateway
	// accepts API keys only, which is precisely the behaviour before identity
	// existed and is what makes the feature safe to wire incrementally.
	identity IdentityStore
	// log records bookkeeping failures that must not fail a request. Nil-safe:
	// touchFailed falls back to the default logger.
	log *slog.Logger
}

// WithLogger sets where non-fatal authentication bookkeeping failures are
// reported. These are deliberately not returned to the caller, so without a
// logger they would be invisible.
func (a *Authenticator) WithLogger(l *slog.Logger) *Authenticator {
	a.log = l
	return a
}

// touchFailed reports a failed last-used write. It is a warning, never an error
// that reaches the caller: the request is properly authenticated and refusing it
// because a usage column could not be updated would make a cosmetic write an
// availability dependency.
func (a *Authenticator) touchFailed(ctx context.Context, kind, id string, err error) {
	l := a.log
	if l == nil {
		l = slog.Default()
	}
	l.WarnContext(ctx, "could not record credential use", "credential_kind", kind, "credential_id", id, "error", err)
}

// New builds an Authenticator over the given querier.
func New(db store.GWQuerier) *Authenticator {
	return &Authenticator{db: db}
}

// Authenticate resolves a presented key to a Principal.
//
// Every failure returns the SAME opaque error. Distinguishing "no such key" from
// "revoked" from "wrong role" would turn this endpoint into an oracle an attacker
// can use to confirm which keys exist, so the distinction stays in the server
// log (without key material) and never reaches the caller.
func (a *Authenticator) Authenticate(ctx context.Context, presented string) (Principal, error) {
	if presented == "" {
		return Principal{}, ErrUnauthenticated
	}
	key, err := store.GatewayLookupAPIKey(ctx, a.db, presented)
	if errors.Is(err, store.ErrAPIKeyUnknown) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		// A database outage is NOT an authentication failure. Returning 401 here
		// would tell every client to re-authenticate during an incident, turning
		// a database blip into a credential-rotation stampede.
		return Principal{}, fmt.Errorf("resolve api key: %w", err)
	}

	role := Role(key.Role)
	if _, known := permitted[role]; !known {
		// A role string the matrix does not know grants nothing. Failing closed
		// means a typo in a seeded row cannot accidentally grant admin.
		return Principal{}, ErrUnauthenticated
	}
	return Principal{KeyID: key.ID, Namespace: key.Namespace, Name: key.Name, Role: role}, nil
}

// ExtractBearer pulls the credential out of an Authorization header value.
//
// Both "Bearer <key>" and a bare key are accepted, because the worker agent, the
// CLI, and curl-by-hand all reach for different conventions and rejecting one of
// them buys nothing.
func ExtractBearer(header string) string {
	h := strings.TrimSpace(header)
	if h == "" {
		return ""
	}
	if len(h) >= 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return h
}

// principalKey is an unexported context key type, so no other package can
// overwrite the authenticated principal in a context it was handed.
type principalKey struct{}

// WithPrincipal attaches the authenticated caller to a context.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext retrieves the authenticated caller.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
