package authn

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// fakeIdentity drives session and token resolution without a database, so these
// tests exercise the AUTHORIZATION decision rather than Postgres.
type fakeIdentity struct {
	sessions map[string]store.ResolvedSession
	tokens   map[string]store.ResolvedToken
	// err is returned instead of a lookup result, to simulate a database outage.
	err error
	// touched records bookkeeping calls so a test can assert they happen without
	// making them load-bearing.
	touched []string
	// touchErr makes the usage write fail, which must never fail a request.
	touchErr error
}

func (f *fakeIdentity) ResolveSession(_ context.Context, presented string) (store.ResolvedSession, error) {
	if f.err != nil {
		return store.ResolvedSession{}, f.err
	}
	rs, ok := f.sessions[presented]
	if !ok {
		return store.ResolvedSession{}, store.ErrIdentityUnknown
	}
	return rs, nil
}

func (f *fakeIdentity) ResolveScopedToken(_ context.Context, presented string) (store.ResolvedToken, error) {
	if f.err != nil {
		return store.ResolvedToken{}, f.err
	}
	rt, ok := f.tokens[presented]
	if !ok {
		return store.ResolvedToken{}, store.ErrIdentityUnknown
	}
	return rt, nil
}

func (f *fakeIdentity) TouchSession(_ context.Context, id string) error {
	f.touched = append(f.touched, "session:"+id)
	return f.touchErr
}

func (f *fakeIdentity) TouchScopedToken(_ context.Context, id string) error {
	f.touched = append(f.touched, "token:"+id)
	return f.touchErr
}

func newFakeIdentity() *fakeIdentity {
	return &fakeIdentity{
		sessions: map[string]store.ResolvedSession{
			"nxsess_alice": {
				SessionID: "sess-1",
				ExpiresAt: time.Now().Add(time.Hour),
				User: store.UserRow{
					ID: "user-1", Namespace: "team-ml", Email: "alice@example.com", Role: "admin",
				},
			},
			"nxsess_bob": {
				SessionID: "sess-2",
				ExpiresAt: time.Now().Add(time.Hour),
				User: store.UserRow{
					ID: "user-2", Namespace: "team-ml", Email: "bob@example.com", Role: "reader",
				},
			},
		},
		tokens: map[string]store.ResolvedToken{
			"nxtok_ci": {
				TokenID: "tok-1", Namespace: "team-ml", Name: "ci",
				Role: "admin", Scopes: []string{"read", "write"},
			},
			"nxtok_readonly": {
				TokenID: "tok-2", Namespace: "team-ml", Name: "dash",
				Role: "admin", Scopes: []string{"read"},
			},
			"nxtok_unscoped": {
				TokenID: "tok-3", Namespace: "team-ml", Name: "legacy",
				Role: "reader", Scopes: []string{},
			},
		},
	}
}

// ── the compatibility guarantee ──────────────────────────────────────────

// TestAPIKeyPrincipalIsUnscoped is the regression guard for the whole feature.
//
// Scopes were added to Principal, and Authorize now intersects them. If a nil
// scope list were treated as "permits nothing", every API key in the fleet -- every
// worker, every CLI -- would start getting 403s the moment this shipped. This
// asserts the zero Principal is authorized by exactly the pre-existing rule.
func TestAPIKeyPrincipalIsUnscoped(t *testing.T) {
	// Built the way the unchanged API-key path builds it: no Kind, no Scopes.
	p := Principal{KeyID: "k1", Namespace: "team-ml", Name: "ci", Role: RoleAdmin}

	for _, a := range []Action{ActionRead, ActionWrite, ActionAdminWrite, ActionWorkerRPC} {
		if err := p.Authorize("team-ml", a); err != nil {
			t.Errorf("an API-key admin was refused %s: %v; every worker and CLI client "+
				"would break on this", a, err)
		}
	}

	// A reader key keeps its original, narrower grant.
	r := Principal{KeyID: "k2", Namespace: "team-ml", Name: "dash", Role: RoleReader}
	if err := r.Authorize("team-ml", ActionRead); err != nil {
		t.Errorf("an API-key reader was refused read: %v", err)
	}
	if err := r.Authorize("team-ml", ActionWrite); !errors.Is(err, ErrForbidden) {
		t.Errorf("an API-key reader was allowed write: %v", err)
	}
}

// TestNilIdentityStoreStillResolvesAPIKeys covers the other half of the
// compatibility story: a gateway that never calls WithIdentity behaves exactly as
// it did before this package gained a second credential kind.
func TestNilIdentityStoreStillResolvesAPIKeys(t *testing.T) {
	a := New(nil) // no identity wired, and no database needed for these paths

	// A session token presented to a gateway with no identity store is simply not
	// a credential it accepts -- and the refusal is the same opaque one.
	if _, err := a.AuthenticateCredential(context.Background(), "nxsess_whatever"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("got %v, want ErrUnauthenticated", err)
	}
	if _, err := a.AuthenticateCredential(context.Background(), ""); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("empty credential: got %v, want ErrUnauthenticated", err)
	}
}

// ── sessions ─────────────────────────────────────────────────────────────

func TestSessionAuthenticates(t *testing.T) {
	ids := newFakeIdentity()
	a := New(nil).WithIdentity(ids)

	p, err := a.AuthenticateCredential(context.Background(), "nxsess_alice")
	if err != nil {
		t.Fatalf("authenticate session: %v", err)
	}
	if p.Kind != KindSession {
		t.Errorf("kind is %q, want %q", p.Kind, KindSession)
	}
	// UserID is what lets an audit row name the PERSON rather than the credential.
	if p.UserID != "user-1" {
		t.Errorf("user id is %q, want user-1", p.UserID)
	}
	if p.Namespace != "team-ml" || p.Role != RoleAdmin {
		t.Errorf("resolved %+v, want namespace team-ml role admin", p)
	}
	// A session is unscoped: a logged-in human has their role's full authority.
	if p.Scopes != nil {
		t.Errorf("a session carried scopes %v; scopes narrow machine credentials", p.Scopes)
	}
	if err := p.Authorize("team-ml", ActionAdminWrite); err != nil {
		t.Errorf("an admin session was refused admin_write: %v", err)
	}
}

func TestSessionNamespaceBoundaryHolds(t *testing.T) {
	ids := newFakeIdentity()
	a := New(nil).WithIdentity(ids)

	p, err := a.AuthenticateCredential(context.Background(), "nxsess_alice")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	// A session valid for team-ml must not act on another tenant, exactly as an
	// API key may not: namespaces are the tenancy boundary.
	if err := p.Authorize("team-other", ActionRead); !errors.Is(err, ErrForbidden) {
		t.Errorf("a session acted across namespaces: %v", err)
	}
}

// TestUnknownSessionIsOpaque: the login surface must not confirm which sessions
// exist, for the reason Authenticate documents about api_keys.
func TestUnknownSessionIsOpaque(t *testing.T) {
	ids := newFakeIdentity()
	a := New(nil).WithIdentity(ids)

	_, err := a.AuthenticateCredential(context.Background(), "nxsess_does_not_exist")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("got %v, want the same opaque ErrUnauthenticated as every other failure", err)
	}
}

// TestDatabaseOutageIsNotAnAuthFailure is the 503-not-401 rule. Returning 401
// during a database incident tells every console user to log in again, turning a
// blip into a credential stampede -- authn.go's stated reason.
func TestDatabaseOutageIsNotAnAuthFailure(t *testing.T) {
	ids := newFakeIdentity()
	ids.err = errors.New("connection refused")
	a := New(nil).WithIdentity(ids)

	_, err := a.AuthenticateCredential(context.Background(), "nxsess_alice")
	if err == nil {
		t.Fatal("a database outage authenticated successfully")
	}
	if errors.Is(err, ErrUnauthenticated) {
		t.Error("a database outage was reported as ErrUnauthenticated; the guard would " +
			"answer 401 and every client would rotate credentials during an incident")
	}
}

// TestAHumanCannotHoldTheWorkerRole. The worker credential exists to speak the
// worker protocol from an execution host -- the least trustworthy place in the
// system. A browser session holding it would be a console that can poll for tasks.
func TestAHumanCannotHoldTheWorkerRole(t *testing.T) {
	ids := newFakeIdentity()
	ids.sessions["nxsess_evil"] = store.ResolvedSession{
		SessionID: "sess-evil",
		ExpiresAt: time.Now().Add(time.Hour),
		User:      store.UserRow{ID: "u9", Namespace: "team-ml", Email: "evil@example.com", Role: "worker"},
	}
	a := New(nil).WithIdentity(ids)

	if _, err := a.AuthenticateCredential(context.Background(), "nxsess_evil"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a session with the worker role authenticated: %v", err)
	}
}

// TestUnknownRoleFailsClosed mirrors the API-key path: a role string the matrix
// does not know must grant nothing, so a typo cannot become an accidental admin.
func TestUnknownRoleFailsClosed(t *testing.T) {
	ids := newFakeIdentity()
	ids.sessions["nxsess_typo"] = store.ResolvedSession{
		SessionID: "sess-typo",
		ExpiresAt: time.Now().Add(time.Hour),
		User:      store.UserRow{ID: "u8", Namespace: "team-ml", Email: "t@example.com", Role: "Admin"},
	}
	a := New(nil).WithIdentity(ids)

	if _, err := a.AuthenticateCredential(context.Background(), "nxsess_typo"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("role %q was accepted; the matrix must fail closed on an unknown role", "Admin")
	}
}

// TestTouchFailureDoesNotFailTheRequest: last_used_at is bookkeeping. Refusing a
// properly authenticated caller because that write failed would make a cosmetic
// column an availability dependency.
func TestTouchFailureDoesNotFailTheRequest(t *testing.T) {
	ids := newFakeIdentity()
	ids.touchErr = errors.New("disk full")
	a := New(nil).WithIdentity(ids)

	if _, err := a.AuthenticateCredential(context.Background(), "nxsess_alice"); err != nil {
		t.Errorf("a failed usage write failed the request: %v", err)
	}
}

func TestSessionUseIsRecorded(t *testing.T) {
	ids := newFakeIdentity()
	a := New(nil).WithIdentity(ids)

	if _, err := a.AuthenticateCredential(context.Background(), "nxsess_alice"); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if len(ids.touched) != 1 || ids.touched[0] != "session:sess-1" {
		t.Errorf("touched %v, want one session:sess-1", ids.touched)
	}
}

// ── scoped tokens ────────────────────────────────────────────────────────

// TestScopesNarrowButNeverExtend is the core authorization property of the whole
// scoped-token feature.
func TestScopesNarrowButNeverExtend(t *testing.T) {
	ids := newFakeIdentity()
	a := New(nil).WithIdentity(ids)

	// An ADMIN-role token scoped to read+write.
	p, err := a.AuthenticateCredential(context.Background(), "nxtok_ci")
	if err != nil {
		t.Fatalf("authenticate token: %v", err)
	}
	if p.Kind != KindScopedToken {
		t.Errorf("kind is %q, want %q", p.Kind, KindScopedToken)
	}
	if err := p.Authorize("team-ml", ActionRead); err != nil {
		t.Errorf("a read-scoped token was refused read: %v", err)
	}
	if err := p.Authorize("team-ml", ActionWrite); err != nil {
		t.Errorf("a write-scoped token was refused write: %v", err)
	}
	// The role permits admin_write, but the SCOPES do not. The narrowing must win,
	// or a scoped token is decoration.
	if err := p.Authorize("team-ml", ActionAdminWrite); !errors.Is(err, ErrForbidden) {
		t.Errorf("a token scoped to read+write performed admin_write: %v", err)
	}
}

// TestScopeCannotExceedRole: the role is checked first and independently, so a
// scope naming an action the role lacks grants nothing.
func TestScopeCannotExceedRole(t *testing.T) {
	ids := newFakeIdentity()
	// A READER-role token whose scopes claim write and admin_write.
	ids.tokens["nxtok_overreach"] = store.ResolvedToken{
		TokenID: "tok-9", Namespace: "team-ml", Name: "overreach",
		Role: "reader", Scopes: []string{"read", "write", "admin_write"},
	}
	a := New(nil).WithIdentity(ids)

	p, err := a.AuthenticateCredential(context.Background(), "nxtok_overreach")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := p.Authorize("team-ml", ActionRead); err != nil {
		t.Errorf("reader token refused read: %v", err)
	}
	for _, act := range []Action{ActionWrite, ActionAdminWrite} {
		if err := p.Authorize("team-ml", act); !errors.Is(err, ErrForbidden) {
			t.Errorf("a reader-role token performed %s because its scopes claimed it: %v", act, err)
		}
	}
}

// TestEmptyScopeListGrantsNothing. An empty list is NOT nil: it means "explicitly
// scoped to nothing", which is what a token created with a typo'd scope must be.
func TestEmptyScopeListGrantsNothing(t *testing.T) {
	ids := newFakeIdentity()
	a := New(nil).WithIdentity(ids)

	p, err := a.AuthenticateCredential(context.Background(), "nxtok_unscoped")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	// store returns []string{}; parseScopes maps it to an empty (non-nil) []Action.
	if p.Scopes == nil {
		t.Fatal("an empty scope list became nil, which means unscoped; a token created " +
			"with no usable scopes must grant nothing rather than everything its role allows")
	}
	if err := p.Authorize("team-ml", ActionRead); !errors.Is(err, ErrForbidden) {
		t.Errorf("a token with no scopes was allowed read: %v", err)
	}
}

// TestUnknownScopesAreDiscarded: a scope the matrix does not know must not be
// retained, so a typo grants nothing rather than something unintended.
func TestUnknownScopesAreDiscarded(t *testing.T) {
	got := parseScopes([]string{"read", "delete_everything", "  write  ", ""})
	want := map[Action]bool{ActionRead: true, ActionWrite: true}

	if len(got) != 2 {
		t.Fatalf("parsed %v, want exactly the two known actions", got)
	}
	for _, a := range got {
		if !want[a] {
			t.Errorf("unknown scope %q survived parsing", a)
		}
	}
}

func TestScopedTokenNamespaceBoundaryHolds(t *testing.T) {
	ids := newFakeIdentity()
	a := New(nil).WithIdentity(ids)

	p, err := a.AuthenticateCredential(context.Background(), "nxtok_ci")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := p.Authorize("team-other", ActionRead); !errors.Is(err, ErrForbidden) {
		t.Errorf("a scoped token acted across namespaces: %v", err)
	}
}

// TestCredentialDispatchIsByPrefix documents why the hot path stays cheap: a
// worker's key must not cost three table probes per request.
func TestCredentialDispatchIsByPrefix(t *testing.T) {
	ids := newFakeIdentity()
	a := New(nil).WithIdentity(ids)

	// An unprefixed credential must NOT reach the session or token stores at all --
	// it falls through to the API-key path.
	db := &recordingQuerier{}
	a = New(db).WithIdentity(ids)
	_, _ = a.AuthenticateCredential(context.Background(), "nxs_legacy_api_key")

	if len(ids.touched) != 0 {
		t.Errorf("an API key triggered identity lookups (%v); the worker RPC path would "+
			"pay for console features it never uses", ids.touched)
	}
	// It DID consult api_keys, which is the path it must take.
	if db.queries != 1 {
		t.Errorf("the API-key path made %d queries, want exactly 1", db.queries)
	}

	// A session token, by contrast, must not probe api_keys at all.
	db2 := &recordingQuerier{}
	a2 := New(db2).WithIdentity(ids)
	if _, err := a2.AuthenticateCredential(context.Background(), "nxsess_alice"); err != nil {
		t.Fatalf("authenticate session: %v", err)
	}
	if db2.queries != 0 {
		t.Errorf("a session token probed api_keys %d times; each credential kind must "+
			"cost exactly one lookup", db2.queries)
	}
}

// recordingQuerier counts how many times the api_keys path was consulted, without
// a database. It returns no rows, so the lookup resolves to ErrAPIKeyUnknown.
type recordingQuerier struct{ queries int }

func (r *recordingQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (r *recordingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}

func (r *recordingQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	r.queries++
	return noRow{}
}

type noRow struct{}

func (noRow) Scan(...any) error { return pgx.ErrNoRows }
