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
type Principal struct {
	KeyID     string
	Namespace string
	Name      string
	Role      Role
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
	if !p.Can(a) {
		return fmt.Errorf("%w: role %s may not %s", ErrForbidden, p.Role, a)
	}
	if namespace != "" && namespace != p.Namespace {
		return fmt.Errorf("%w: key is scoped to namespace %s, not %s", ErrForbidden, p.Namespace, namespace)
	}
	return nil
}

// Authenticator resolves credentials against the api_keys table.
type Authenticator struct {
	db store.GWQuerier
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
