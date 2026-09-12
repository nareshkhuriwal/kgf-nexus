package authn

import "testing"

// A credential must not reach another namespace, whatever kind it is and
// whatever it is scoped for.
//
// The namespace check is the tenancy boundary, and adding session and token
// credentials widened the set of things that reach it. This pins the property
// for ALL THREE kinds rather than the api_key path alone: schedule params are
// author-supplied and GET /schedules/{name} returns them, so a cross-namespace
// read is a data leak, not merely an authorization slip.
func TestNoCredentialKindCrossesNamespace(t *testing.T) {
	for _, kind := range []PrincipalKind{KindAPIKey, KindSession, KindScopedToken} {
		// Deliberately the widest possible principal: admin role, every scope.
		// If anything can cross a namespace, this can.
		p := Principal{
			Namespace: "alpha", Role: RoleAdmin, Kind: kind,
			Scopes: []Action{ActionRead, ActionWrite, ActionAdminWrite},
		}
		for _, a := range []Action{ActionRead, ActionWrite, ActionAdminWrite} {
			if err := p.Authorize("beta", a); err == nil {
				t.Errorf("%s credential reached namespace beta from alpha for %s", kind, a)
			}
			if err := p.Authorize("alpha", a); err != nil {
				t.Errorf("%s credential refused in its OWN namespace for %s: %v", kind, a, err)
			}
		}
	}
}
