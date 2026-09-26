// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// fakeIdentityAdmin is an in-memory IdentityAdmin, so these tests exercise the
// HTTP contract -- status codes, what is disclosed, what is audited -- rather
// than Postgres, which the store's own integration tests already cover.
type fakeIdentityAdmin struct {
	users    map[string]store.UserRow // by id
	byEmail  map[string]string        // namespace\x00email -> id
	password map[string]string        // user id -> password
	sessions map[string]store.SessionRow
	tokens   map[string]store.ScopedTokenRow
	audit    []store.AuditEntry

	// failAudit makes the audit write fail, to prove an operation still succeeds.
	failAudit bool
	// authErr overrides password authentication, to simulate a database outage.
	authErr error
	nextID  int
}

func newFakeIdentityAdmin() *fakeIdentityAdmin {
	f := &fakeIdentityAdmin{
		users:    map[string]store.UserRow{},
		byEmail:  map[string]string{},
		password: map[string]string{},
		sessions: map[string]store.SessionRow{},
		tokens:   map[string]store.ScopedTokenRow{},
	}
	f.addUser("team-ml", "admin@example.com", "admin", "correct-password-1234")
	f.addUser("team-ml", "reader@example.com", "reader", "reader-password-1234")
	return f
}

func (f *fakeIdentityAdmin) addUser(ns, email, role, password string) store.UserRow {
	f.nextID++
	id := "user-" + string(rune('0'+f.nextID))
	u := store.UserRow{
		ID: id, Namespace: ns, Email: email, DisplayName: email,
		Role: role, CreatedAt: time.Now(),
	}
	f.users[id] = u
	f.byEmail[ns+"\x00"+email] = id
	f.password[id] = password
	return u
}

func (f *fakeIdentityAdmin) AuthenticatePassword(_ context.Context, ns, email, password string) (store.UserRow, error) {
	if f.authErr != nil {
		return store.UserRow{}, f.authErr
	}
	id, ok := f.byEmail[ns+"\x00"+strings.ToLower(email)]
	if !ok {
		return store.UserRow{}, store.ErrIdentityUnknown
	}
	u := f.users[id]
	if u.DisabledAt != nil || f.password[id] != password {
		return store.UserRow{}, store.ErrIdentityUnknown
	}
	return u, nil
}

func (f *fakeIdentityAdmin) CreateSession(_ context.Context, userID string, ttl time.Duration, ua, ip string) (string, store.SessionRow, error) {
	id := "sess-" + userID
	row := store.SessionRow{
		ID: id, UserID: userID, ExpiresAt: time.Now().Add(ttl),
		CreatedAt: time.Now(), UserAgent: ua, ClientIP: ip,
	}
	f.sessions[id] = row
	return "nxsess_" + id, row, nil
}

func (f *fakeIdentityAdmin) RevokeSession(_ context.Context, sessionID string) error {
	s, ok := f.sessions[sessionID]
	if !ok {
		return nil
	}
	now := time.Now()
	s.RevokedAt = &now
	f.sessions[sessionID] = s
	return nil
}

func (f *fakeIdentityAdmin) RevokeUserSessions(_ context.Context, userID string) (int64, error) {
	var n int64
	for id, s := range f.sessions {
		if s.UserID == userID && s.RevokedAt == nil {
			now := time.Now()
			s.RevokedAt = &now
			f.sessions[id] = s
			n++
		}
	}
	return n, nil
}

func (f *fakeIdentityAdmin) CreateUser(_ context.Context, ns, email, display, role, password string) (store.UserRow, error) {
	if _, exists := f.byEmail[ns+"\x00"+strings.ToLower(email)]; exists {
		return store.UserRow{}, store.ErrEmailTaken
	}
	u := f.addUser(ns, strings.ToLower(email), role, password)
	u.DisplayName = display
	f.users[u.ID] = u
	return u, nil
}

func (f *fakeIdentityAdmin) ListUsers(_ context.Context, ns string, _ int) ([]store.UserRow, error) {
	var out []store.UserRow
	for _, u := range f.users {
		if u.Namespace == ns {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *fakeIdentityAdmin) GetUser(_ context.Context, ns, id string) (store.UserRow, error) {
	u, ok := f.users[id]
	if !ok || u.Namespace != ns {
		return store.UserRow{}, store.ErrNotFound
	}
	return u, nil
}

func (f *fakeIdentityAdmin) SetUserDisabled(_ context.Context, ns, id string, disabled bool) (store.UserRow, error) {
	u, ok := f.users[id]
	if !ok || u.Namespace != ns {
		return store.UserRow{}, store.ErrNotFound
	}
	if disabled {
		now := time.Now()
		u.DisabledAt = &now
		_, _ = f.RevokeUserSessions(context.Background(), id)
	} else {
		u.DisabledAt = nil
	}
	f.users[id] = u
	return u, nil
}

func (f *fakeIdentityAdmin) SetUserPassword(_ context.Context, userID, password string) error {
	if _, ok := f.users[userID]; !ok {
		return store.ErrNotFound
	}
	f.password[userID] = password
	return nil
}

func (f *fakeIdentityAdmin) ListUserSessions(_ context.Context, userID string, _ int) ([]store.SessionRow, error) {
	var out []store.SessionRow
	for _, s := range f.sessions {
		if s.UserID == userID {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeIdentityAdmin) CreateScopedToken(_ context.Context, ns, name, role string, scopes []string, expiresAt *time.Time, createdBy *string) (string, store.ScopedTokenRow, error) {
	f.nextID++
	id := "tok-" + string(rune('0'+f.nextID))
	row := store.ScopedTokenRow{
		ID: id, Namespace: ns, Name: name, Role: role, Scopes: scopes,
		ExpiresAt: expiresAt, CreatedBy: createdBy, CreatedAt: time.Now(),
	}
	f.tokens[id] = row
	return "nxtok_" + id, row, nil
}

func (f *fakeIdentityAdmin) ListScopedTokens(_ context.Context, ns string, _ int) ([]store.ScopedTokenRow, error) {
	var out []store.ScopedTokenRow
	for _, t := range f.tokens {
		if t.Namespace == ns {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeIdentityAdmin) RevokeScopedToken(_ context.Context, ns, id string) error {
	t, ok := f.tokens[id]
	if !ok || t.Namespace != ns {
		return store.ErrNotFound
	}
	now := time.Now()
	t.RevokedAt = &now
	f.tokens[id] = t
	return nil
}

func (f *fakeIdentityAdmin) AppendAudit(_ context.Context, e store.AuditEntry) error {
	if f.failAudit {
		return errors.New("audit table unavailable")
	}
	f.audit = append(f.audit, e)
	return nil
}

// WithinTx gives the fake real rollback semantics: everything fn changed is
// restored when it fails, exactly as the Postgres transaction would.
func (f *fakeIdentityAdmin) WithinTx(_ context.Context, fn func(IdentityAdmin) error) error {
	snap := f.snapshot()
	if err := fn(f); err != nil {
		f.restore(snap)
		return err
	}
	return nil
}

type fakeIdentitySnapshot struct {
	users    map[string]store.UserRow
	byEmail  map[string]string
	password map[string]string
	sessions map[string]store.SessionRow
	tokens   map[string]store.ScopedTokenRow
	audit    []store.AuditEntry
	nextID   int
}

func cloneMap[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (f *fakeIdentityAdmin) snapshot() fakeIdentitySnapshot {
	return fakeIdentitySnapshot{cloneMap(f.users), cloneMap(f.byEmail), cloneMap(f.password),
		cloneMap(f.sessions), cloneMap(f.tokens), append([]store.AuditEntry(nil), f.audit...), f.nextID}
}

func (f *fakeIdentityAdmin) restore(s fakeIdentitySnapshot) {
	f.users, f.byEmail, f.password, f.sessions, f.tokens, f.audit, f.nextID =
		s.users, s.byEmail, s.password, s.sessions, s.tokens, s.audit, s.nextID
}

func (f *fakeIdentityAdmin) ListAudit(_ context.Context, ns string, filter store.AuditFilter) ([]store.AuditRow, string, error) {
	var out []store.AuditRow
	for i, e := range f.audit {
		if e.Namespace != ns {
			continue
		}
		if filter.Action != "" && e.Action != filter.Action {
			continue
		}
		if filter.Outcome != "" && e.Outcome != filter.Outcome {
			continue
		}
		out = append(out, store.AuditRow{
			ID: int64(i), Namespace: e.Namespace, ActorKind: e.ActorKind,
			ActorLabel: e.ActorLabel, Action: e.Action, Outcome: e.Outcome,
			Detail: []byte("{}"), OccurredAt: time.Now(),
		})
	}
	return out, "", nil
}

// auditedActions returns the actions recorded so far, for readable assertions.
func (f *fakeIdentityAdmin) auditedActions() []string {
	var out []string
	for _, e := range f.audit {
		out = append(out, e.Action+"/"+e.Outcome)
	}
	return out
}

// newAdminAPI builds an API with identity wired, plus the API keys the existing
// harness uses, so these tests can assert that BOTH credential kinds work.
func newAdminAPI(t *testing.T) (http.Handler, *restDB, *fakeIdentityAdmin) {
	t.Helper()
	db := newRestDB()
	db.addKey(adminKey, "team-ml", "ci", "admin")
	db.addKey(readerKey, "team-ml", "dashboard", "reader")
	ids := newFakeIdentityAdmin()

	// The authenticator resolves sessions and scoped tokens as well as API keys,
	// exactly as main.go wires it, so these tests exercise the real credential
	// dispatch rather than a key-only shortcut.
	auth := authn.New(db).WithIdentity(sessionBridge{ids})
	api := New(db, auth, &fakeWFM{}, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithIdentity(ids)
	return api.Routes(), db, ids
}

func doJSON(t *testing.T, h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v\nbody=%s", err, rec.Body)
	}
	return out
}

// ── the compatibility guarantee, at the HTTP layer ───────────────────────

// TestAPIKeysStillWorkAfterIdentity is the regression test that matters most:
// the CLI and every worker authenticate with an API key, and adding identity must
// not have changed that by one status code.
func TestAPIKeysStillWorkAfterIdentity(t *testing.T) {
	h, _, _ := newAdminAPI(t)

	// An admin API key still authenticates and is still authorized. whoami is used
	// rather than a run listing because restDB models only api_keys and
	// idempotency_keys -- the AUTHENTICATION decision is what this asserts, and it
	// is the same guard() for every route.
	if rec := doJSON(t, h, "GET", "/v1/auth/whoami", adminKey, ""); rec.Code != http.StatusOK {
		t.Errorf("admin API key: status %d, want 200. body=%s", rec.Code, rec.Body)
	}
	// A reader API key still authenticates and keeps its narrower grant.
	if rec := doJSON(t, h, "GET", "/v1/auth/whoami", readerKey, ""); rec.Code != http.StatusOK {
		t.Errorf("reader API key: status %d, want 200. body=%s", rec.Code, rec.Body)
	}
	// A reader API key is still refused a write-level route.
	if rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/runs", readerKey, `{"spec_name":"x"}`); rec.Code != http.StatusForbidden {
		t.Errorf("reader API key creating a run: status %d, want 403", rec.Code)
	}
	// An unknown key is still one opaque 401, not a hint that it was unrecognised.
	if rec := doJSON(t, h, "GET", "/v1/auth/whoami", "nx_not_a_real_key", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown key: status %d, want 401", rec.Code)
	}
	// No credential is still one opaque 401.
	if rec := doJSON(t, h, "GET", "/v1/auth/whoami", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no credential: status %d, want 401", rec.Code)
	}
}

// TestAdminAPIKeyCanUseAdminRoutes: an API key with the admin role reaches the new
// admin surface, so an operator is not forced onto sessions to administer.
func TestAdminAPIKeyCanUseAdminRoutes(t *testing.T) {
	h, _, _ := newAdminAPI(t)

	rec := doJSON(t, h, "GET", "/v1/namespaces/team-ml/users", adminKey, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin key listing users: status %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if _, ok := decode(t, rec)["users"]; !ok {
		t.Error("response has no users field")
	}
}

// TestReaderCannotReachAdminRoutes covers the authorization boundary on every new
// admin route at once. A missed guard here is a privilege escalation.
func TestReaderCannotReachAdminRoutes(t *testing.T) {
	h, _, _ := newAdminAPI(t)

	cases := []struct{ method, path, body string }{
		{"GET", "/v1/namespaces/team-ml/users", ""},
		{"POST", "/v1/namespaces/team-ml/users", `{"email":"x@y.com","password":"long-enough-password"}`},
		{"POST", "/v1/namespaces/team-ml/users/user-1/disable", ""},
		{"POST", "/v1/namespaces/team-ml/users/user-1/enable", ""},
		{"POST", "/v1/namespaces/team-ml/users/user-1/password", `{"password":"long-enough-password"}`},
		{"GET", "/v1/namespaces/team-ml/users/user-1/sessions", ""},
		{"POST", "/v1/namespaces/team-ml/users/user-1/sessions/revoke", ""},
		{"GET", "/v1/namespaces/team-ml/tokens", ""},
		{"POST", "/v1/namespaces/team-ml/tokens", `{"name":"t","scopes":["read"]}`},
		{"POST", "/v1/namespaces/team-ml/tokens/tok-1/revoke", ""},
		{"GET", "/v1/namespaces/team-ml/audit", ""},
	}
	for _, c := range cases {
		rec := doJSON(t, h, c.method, c.path, readerKey, c.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with a reader credential: status %d, want 403", c.method, c.path, rec.Code)
		}
	}
}

// TestAdminRoutesRequireACredential: every admin route must 401 unauthenticated.
// Together with the 403 test above, this is the "no route without an authorization
// decision" property that guard() exists to guarantee, checked route by route.
func TestAdminRoutesRequireACredential(t *testing.T) {
	h, _, _ := newAdminAPI(t)

	paths := []struct{ method, path string }{
		{"GET", "/v1/namespaces/team-ml/users"},
		{"POST", "/v1/namespaces/team-ml/users"},
		{"GET", "/v1/namespaces/team-ml/tokens"},
		{"POST", "/v1/namespaces/team-ml/tokens"},
		{"GET", "/v1/namespaces/team-ml/audit"},
		{"POST", "/v1/auth/logout"},
		{"GET", "/v1/auth/whoami"},
	}
	for _, p := range paths {
		rec := doJSON(t, h, p.method, p.path, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no credential: status %d, want 401", p.method, p.path, rec.Code)
		}
	}
}

// ── login ────────────────────────────────────────────────────────────────

func TestLoginSucceedsAndReturnsTokenOnce(t *testing.T) {
	h, _, ids := newAdminAPI(t)

	rec := doJSON(t, h, "POST", "/v1/auth/login", "",
		`{"namespace":"team-ml","email":"admin@example.com","password":"correct-password-1234"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, want 200. body=%s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	token, _ := body["token"].(string)
	if !strings.HasPrefix(token, "nxsess_") {
		t.Errorf("login returned token %q, want a session token", token)
	}
	if body["expires_at"] == nil {
		t.Error("login did not report when the session expires")
	}
	// The successful login is recorded.
	if got := ids.auditedActions(); len(got) != 1 || got[0] != "session.login/success" {
		t.Errorf("audit recorded %v, want one session.login/success", got)
	}
}

// TestLoginFailureIsOpaqueAndAudited is the anti-oracle property at the HTTP edge.
func TestLoginFailureIsOpaqueAndAudited(t *testing.T) {
	h, _, ids := newAdminAPI(t)

	noSuchUser := doJSON(t, h, "POST", "/v1/auth/login", "",
		`{"namespace":"team-ml","email":"ghost@example.com","password":"whatever-long"}`)
	wrongPassword := doJSON(t, h, "POST", "/v1/auth/login", "",
		`{"namespace":"team-ml","email":"admin@example.com","password":"whatever-long"}`)

	if noSuchUser.Code != http.StatusUnauthorized || wrongPassword.Code != http.StatusUnauthorized {
		t.Fatalf("statuses %d and %d, want both 401", noSuchUser.Code, wrongPassword.Code)
	}
	// Byte-identical bodies: any difference identifies which accounts exist.
	if noSuchUser.Body.String() != wrongPassword.Body.String() {
		t.Errorf("login responses differ and so enumerate accounts:\n  no such user:   %s\n  wrong password: %s",
			noSuchUser.Body, wrongPassword.Body)
	}
	// Both denials are recorded, because a burst of them is an attack in progress.
	got := ids.auditedActions()
	if len(got) != 2 {
		t.Fatalf("audit recorded %v, want two denied logins", got)
	}
	for _, a := range got {
		if a != "session.login/denied" {
			t.Errorf("recorded %q, want session.login/denied", a)
		}
	}
}

// TestLoginNeverAuditsThePassword. An audit log an operator reads during an
// incident must never be a place credentials leak to.
func TestLoginNeverAuditsThePassword(t *testing.T) {
	h, _, ids := newAdminAPI(t)
	const secret = "correct-password-1234"

	doJSON(t, h, "POST", "/v1/auth/login", "",
		`{"namespace":"team-ml","email":"admin@example.com","password":"`+secret+`"}`)
	doJSON(t, h, "POST", "/v1/auth/login", "",
		`{"namespace":"team-ml","email":"admin@example.com","password":"wrong-but-secret-2"}`)

	for _, e := range ids.audit {
		blob, _ := json.Marshal(e)
		if strings.Contains(string(blob), secret) || strings.Contains(string(blob), "wrong-but-secret-2") {
			t.Errorf("an audit entry contains password material: %s", blob)
		}
	}
}

// TestLoginDatabaseOutageIs503 -- not 401. A 401 during an incident tells every
// console user their password stopped working.
func TestLoginDatabaseOutageIs503(t *testing.T) {
	h, _, ids := newAdminAPI(t)
	ids.authErr = errors.New("connection refused")

	rec := doJSON(t, h, "POST", "/v1/auth/login", "",
		`{"namespace":"team-ml","email":"admin@example.com","password":"correct-password-1234"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("login during a database outage: status %d, want 503", rec.Code)
	}
}

func TestLoginRejectsMalformedBody(t *testing.T) {
	h, _, _ := newAdminAPI(t)
	rec := doJSON(t, h, "POST", "/v1/auth/login", "", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed login body: status %d, want 400", rec.Code)
	}
}

// ── the session round trip ───────────────────────────────────────────────

// TestSessionTokenAuthenticatesSubsequentRequests proves login actually yields a
// usable credential rather than a string that looks like one.
func TestSessionTokenAuthenticatesSubsequentRequests(t *testing.T) {
	h, _, _ := newAdminAPI(t)

	login := doJSON(t, h, "POST", "/v1/auth/login", "",
		`{"namespace":"team-ml","email":"admin@example.com","password":"correct-password-1234"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login: status %d. body=%s", login.Code, login.Body)
	}
	token := decode(t, login)["token"].(string)

	// whoami with the session token.
	who := doJSON(t, h, "GET", "/v1/auth/whoami", token, "")
	if who.Code != http.StatusOK {
		t.Fatalf("whoami with a session token: status %d, want 200. body=%s", who.Code, who.Body)
	}
	body := decode(t, who)
	if body["kind"] != "session" {
		t.Errorf("whoami reports kind %v, want session", body["kind"])
	}
	if body["role"] != "admin" {
		t.Errorf("whoami reports role %v, want admin", body["role"])
	}
	// Permissions are reported by the server so the UI never re-derives the matrix.
	perms, ok := body["permissions"].(map[string]any)
	if !ok || perms["admin_write"] != true {
		t.Errorf("whoami permissions %v, want admin_write true", body["permissions"])
	}

	// The session can reach an admin route.
	if rec := doJSON(t, h, "GET", "/v1/namespaces/team-ml/users", token, ""); rec.Code != http.StatusOK {
		t.Errorf("session listing users: status %d, want 200. body=%s", rec.Code, rec.Body)
	}

	// Logout, and the token must stop working.
	if rec := doJSON(t, h, "POST", "/v1/auth/logout", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("logout: status %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, "GET", "/v1/auth/whoami", token, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("a logged-out session still worked: status %d, want 401", rec.Code)
	}
}

// sessionBridge adapts the REST fake to the authn.IdentityStore interface, so one
// fake drives both the login endpoint and credential resolution.
type sessionBridge struct{ f *fakeIdentityAdmin }

func (b sessionBridge) ResolveSession(_ context.Context, presented string) (store.ResolvedSession, error) {
	id := strings.TrimPrefix(presented, "nxsess_")
	s, ok := b.f.sessions[id]
	if !ok || s.RevokedAt != nil || s.ExpiresAt.Before(time.Now()) {
		return store.ResolvedSession{}, store.ErrIdentityUnknown
	}
	u, ok := b.f.users[s.UserID]
	if !ok || u.DisabledAt != nil {
		return store.ResolvedSession{}, store.ErrIdentityUnknown
	}
	return store.ResolvedSession{SessionID: s.ID, User: u, ExpiresAt: s.ExpiresAt}, nil
}

func (b sessionBridge) ResolveScopedToken(_ context.Context, presented string) (store.ResolvedToken, error) {
	id := strings.TrimPrefix(presented, "nxtok_")
	t, ok := b.f.tokens[id]
	if !ok || t.RevokedAt != nil {
		return store.ResolvedToken{}, store.ErrIdentityUnknown
	}
	return store.ResolvedToken{
		TokenID: t.ID, Namespace: t.Namespace, Name: t.Name, Role: t.Role, Scopes: t.Scopes,
	}, nil
}

func (b sessionBridge) TouchSession(context.Context, string) error     { return nil }
func (b sessionBridge) TouchScopedToken(context.Context, string) error { return nil }

// TestLogoutRejectsAnAPIKey: an API key has no session to end, and pretending
// otherwise would report success for an operation that did nothing.
func TestLogoutRejectsAnAPIKey(t *testing.T) {
	h, _, _ := newAdminAPI(t)
	rec := doJSON(t, h, "POST", "/v1/auth/logout", adminKey, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("logout with an API key: status %d, want 400", rec.Code)
	}
}

// TestWhoamiWorksForAnAPIKey: the console needs one code path for every
// credential kind.
func TestWhoamiWorksForAnAPIKey(t *testing.T) {
	h, _, _ := newAdminAPI(t)
	rec := doJSON(t, h, "GET", "/v1/auth/whoami", adminKey, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("whoami with an API key: status %d, want 200. body=%s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	if body["kind"] != "api_key" {
		t.Errorf("kind is %v, want api_key", body["kind"])
	}
	if body["namespace"] != "team-ml" {
		t.Errorf("namespace is %v, want team-ml", body["namespace"])
	}
}

// ── user administration ──────────────────────────────────────────────────

func TestCreateUserValidatesAndAudits(t *testing.T) {
	h, _, ids := newAdminAPI(t)

	rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/users", adminKey,
		`{"email":"new@example.com","display_name":"New","role":"reader","password":"a-long-enough-password"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user: status %d, want 201. body=%s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	if body["email"] != "new@example.com" {
		t.Errorf("created %v, want new@example.com", body["email"])
	}
	// The response must never carry password material, hashed or otherwise.
	if _, leaked := body["password"]; leaked {
		t.Error("the create-user response echoed a password field")
	}
	if _, leaked := body["password_hash"]; leaked {
		t.Error("the create-user response leaked password_hash")
	}

	var found bool
	for _, a := range ids.auditedActions() {
		if a == "user.create/success" {
			found = true
		}
	}
	if !found {
		t.Errorf("audit recorded %v, want a user.create/success", ids.auditedActions())
	}
}

func TestCreateUserRejectsBadInput(t *testing.T) {
	h, _, _ := newAdminAPI(t)

	cases := map[string]string{
		"no email":       `{"password":"a-long-enough-password"}`,
		"bad email":      `{"email":"not-an-email","password":"a-long-enough-password"}`,
		"short password": `{"email":"a@b.com","password":"short"}`,
		"worker role":    `{"email":"a@b.com","role":"worker","password":"a-long-enough-password"}`,
		"unknown role":   `{"email":"a@b.com","role":"superuser","password":"a-long-enough-password"}`,
	}
	for name, body := range cases {
		rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/users", adminKey, body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422. body=%s", name, rec.Code, rec.Body)
			continue
		}
		// Problems are reported as a LIST so a form is not filled in four times.
		if _, ok := decode(t, rec)["problems"]; !ok {
			t.Errorf("%s: response has no problems list", name)
		}
	}
}

// TestWorkerRoleIsRefusedForHumans. The worker credential belongs on an execution
// host; a person holding it would be a console that can poll for tasks.
func TestWorkerRoleIsRefusedForHumans(t *testing.T) {
	h, _, _ := newAdminAPI(t)
	rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/users", adminKey,
		`{"email":"machine@example.com","role":"worker","password":"a-long-enough-password"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "worker") {
		t.Errorf("the refusal does not explain the worker role: %s", rec.Body)
	}
}

func TestDuplicateEmailIs409(t *testing.T) {
	h, _, _ := newAdminAPI(t)
	rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/users", adminKey,
		`{"email":"admin@example.com","password":"a-long-enough-password"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate email: status %d, want 409. body=%s", rec.Code, rec.Body)
	}
}

// TestDisableUserRevokesSessions covers the offboarding guarantee end to end at
// the HTTP layer.
func TestDisableUserRevokesSessions(t *testing.T) {
	h, _, ids := newAdminAPI(t)

	// Give the reader a live session.
	readerID := ids.byEmail["team-ml\x00reader@example.com"]
	if _, _, err := ids.CreateSession(context.Background(), readerID, time.Hour, "ua", "ip"); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/users/"+readerID+"/disable", adminKey, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: status %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if decode(t, rec)["disabled"] != true {
		t.Error("the response does not report the user as disabled")
	}
	for _, s := range ids.sessions {
		if s.UserID == readerID && s.RevokedAt == nil {
			t.Error("disabling a user left a live session; that is the access window " +
				"an offboarding exists to close")
		}
	}
}

// TestCannotDisableSelf: an admin who disables their own last admin account
// leaves the namespace unreachable, and there is no out-of-band recovery here.
func TestCannotDisableSelf(t *testing.T) {
	h, _, ids := newAdminAPI(t)

	login := doJSON(t, h, "POST", "/v1/auth/login", "",
		`{"namespace":"team-ml","email":"admin@example.com","password":"correct-password-1234"}`)
	token := decode(t, login)["token"].(string)
	adminID := ids.byEmail["team-ml\x00admin@example.com"]

	rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/users/"+adminID+"/disable", token, "")
	if rec.Code != http.StatusConflict {
		t.Errorf("disabling own account: status %d, want 409. body=%s", rec.Code, rec.Body)
	}
}

func TestPasswordResetRevokesSessions(t *testing.T) {
	h, _, ids := newAdminAPI(t)
	readerID := ids.byEmail["team-ml\x00reader@example.com"]
	if _, _, err := ids.CreateSession(context.Background(), readerID, time.Hour, "ua", "ip"); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/users/"+readerID+"/password", adminKey,
		`{"password":"a-brand-new-password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset password: status %d, want 200. body=%s", rec.Code, rec.Body)
	}
	// A reset that leaves sessions alive does not lock out whoever prompted it.
	if n, _ := decode(t, rec)["sessions_revoked"].(float64); n < 1 {
		t.Errorf("sessions_revoked is %v, want at least 1", decode(t, rec)["sessions_revoked"])
	}
	if ids.password[readerID] != "a-brand-new-password" {
		t.Error("the password was not actually changed")
	}
}

func TestPasswordResetIsNamespaceScoped(t *testing.T) {
	h, _, ids := newAdminAPI(t)
	// A user in another tenant.
	other := ids.addUser("team-other", "victim@example.com", "admin", "victim-password-12")

	rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/users/"+other.ID+"/password", adminKey,
		`{"password":"attacker-chosen-pw"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant password reset: status %d, want 404", rec.Code)
	}
	if ids.password[other.ID] != "victim-password-12" {
		t.Error("a cross-tenant password reset actually changed the password")
	}
}

// ── scoped tokens ────────────────────────────────────────────────────────

func TestCreateTokenReturnsSecretOnceAndAuditsWithoutIt(t *testing.T) {
	h, _, ids := newAdminAPI(t)

	rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/tokens", adminKey,
		`{"name":"ci-pipeline","role":"admin","scopes":["read","write"],"expires_in_days":30}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create token: status %d, want 201. body=%s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	secret, _ := body["token"].(string)
	if !strings.HasPrefix(secret, "nxtok_") {
		t.Fatalf("token is %q, want an nxtok_ secret", secret)
	}
	if body["token_shown_once"] != true {
		t.Error("the response does not tell the client the secret is shown once")
	}

	// The secret must NOT be in the audit trail: that table is read by humans.
	for _, e := range ids.audit {
		blob, _ := json.Marshal(e)
		if strings.Contains(string(blob), secret) {
			t.Errorf("the token secret leaked into the audit log: %s", blob)
		}
	}

	// Listing must never return the secret either -- it cannot, since only the hash
	// is stored, and this asserts the handler does not reconstruct it from anywhere.
	list := doJSON(t, h, "GET", "/v1/namespaces/team-ml/tokens", adminKey, "")
	if strings.Contains(list.Body.String(), secret) {
		t.Errorf("the token list returned the secret: %s", list.Body)
	}
}

func TestCreateTokenRejectsEmptyScopes(t *testing.T) {
	h, _, _ := newAdminAPI(t)
	// A token with no scopes can do nothing; minting one silently would produce a
	// credential that mysteriously 403s on every call.
	rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/tokens", adminKey,
		`{"name":"useless","scopes":[]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty scopes: status %d, want 422. body=%s", rec.Code, rec.Body)
	}
}

func TestCreateTokenRejectsUnknownScope(t *testing.T) {
	h, _, _ := newAdminAPI(t)
	rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/tokens", adminKey,
		`{"name":"typo","scopes":["read","delete_everything"]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown scope: status %d, want 422. body=%s", rec.Code, rec.Body)
	}
}

// TestTokenDefaultsToAnExpiry. An unbounded credential is exactly what scoped
// tokens exist to replace, so omitting the field must not mean "forever".
func TestTokenDefaultsToAnExpiry(t *testing.T) {
	h, _, _ := newAdminAPI(t)
	rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/tokens", adminKey,
		`{"name":"no-expiry-given","scopes":["read"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d. body=%s", rec.Code, rec.Body)
	}
	if decode(t, rec)["expires_at"] == nil {
		t.Error("a token created without an explicit lifetime got no expiry; the default " +
			"must be bounded")
	}
}

func TestRevokeTokenIsNamespaceScoped(t *testing.T) {
	h, _, ids := newAdminAPI(t)
	_, row, err := ids.CreateScopedToken(context.Background(), "team-other", "victim", "admin", []string{"read"}, nil, nil)
	if err != nil {
		t.Fatalf("seed token: %v", err)
	}

	rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/tokens/"+row.ID+"/revoke", adminKey, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant revoke: status %d, want 404", rec.Code)
	}
	if ids.tokens[row.ID].RevokedAt != nil {
		t.Error("a token in another namespace was revoked")
	}
}

// ── audit ────────────────────────────────────────────────────────────────

func TestAuditListIsFilterable(t *testing.T) {
	h, _, _ := newAdminAPI(t)

	doJSON(t, h, "POST", "/v1/namespaces/team-ml/users", adminKey,
		`{"email":"one@example.com","password":"a-long-enough-password"}`)
	doJSON(t, h, "POST", "/v1/namespaces/team-ml/tokens", adminKey,
		`{"name":"t","scopes":["read"]}`)

	all := decode(t, doJSON(t, h, "GET", "/v1/namespaces/team-ml/audit", adminKey, ""))
	entries, _ := all["entries"].([]any)
	if len(entries) < 2 {
		t.Fatalf("audit returned %d entries, want at least 2", len(entries))
	}

	filtered := decode(t, doJSON(t, h, "GET", "/v1/namespaces/team-ml/audit?action=token.create", adminKey, ""))
	got, _ := filtered["entries"].([]any)
	if len(got) != 1 {
		t.Errorf("action filter returned %d entries, want 1", len(got))
	}
}

// TestAuditFailureDoesNotFailTheOperation documents a deliberate trade: refusing
// an administrative action because its audit row could not be written would block
// administration during exactly the incident when it is needed.
//
// ADR-0025 (F139) replaced that trade for SECURITY actions: a grant is now
// refused, and rolled back, when its audit row cannot be written; a
// revocation still goes through.
func TestFailedAuditBlocksAndRollsBackSecurityGrants(t *testing.T) {
	h, _, ids := newAdminAPI(t)
	ids.failAudit = true

	rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/users", adminKey,
		`{"email":"unaudited@example.com","password":"a-long-enough-password"}`)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "audit_unavailable") {
		t.Fatalf("user.create with the audit store down: status %d body=%s, want 503 audit_unavailable", rec.Code, rec.Body)
	}
	if _, exists := ids.byEmail["team-ml\x00unaudited@example.com"]; exists {
		t.Error("the user was created although its audit row was not written")
	}

	before := len(ids.tokens)
	rec = doJSON(t, h, "POST", "/v1/namespaces/team-ml/tokens", adminKey,
		`{"name":"ci","role":"reader","scopes":["read"]}`)
	if rec.Code != http.StatusServiceUnavailable || len(ids.tokens) != before {
		t.Errorf("token.create: status %d, tokens %d -> %d; want 503 and no token minted", rec.Code, before, len(ids.tokens))
	}

	sessions := len(ids.sessions)
	rec = doJSON(t, h, "POST", "/v1/auth/login", "",
		`{"namespace":"team-ml","email":"admin@example.com","password":"correct-password-1234"}`)
	if rec.Code != http.StatusServiceUnavailable || len(ids.sessions) != sessions {
		t.Errorf("login: status %d, sessions %d -> %d; want 503 and no session", rec.Code, sessions, len(ids.sessions))
	}
}

func TestFailedAuditNeverBlocksARevocation(t *testing.T) {
	h, _, ids := newAdminAPI(t)
	rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/tokens", adminKey,
		`{"name":"ci","role":"reader","scopes":["read"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create token: %d %s", rec.Code, rec.Body)
	}
	var tok struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	ids.failAudit = true
	rec = doJSON(t, h, "POST", "/v1/namespaces/team-ml/tokens/"+tok.ID+"/revoke", adminKey, `{}`)
	if rec.Code != http.StatusOK || ids.tokens[tok.ID].RevokedAt == nil {
		t.Errorf("revoke with the audit store down: status %d revoked=%v; a revocation must go through",
			rec.Code, ids.tokens[tok.ID].RevokedAt != nil)
	}
}

// TestIdentityRoutesAbsentWhenNotWired: a deployment without identity serves
// exactly the route set it served before, and a 404 is clearer than a 501 from a
// route that exists but cannot work.
func TestIdentityRoutesAbsentWhenNotWired(t *testing.T) {
	h, _, _ := newTestAPI(t) // the original harness, no WithIdentity

	for _, p := range []string{"/v1/auth/login", "/v1/auth/whoami"} {
		rec := doJSON(t, h, "GET", p, adminKey, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s on a gateway without identity: status %d, want 404", p, rec.Code)
		}
	}
	// And a pre-existing route is still registered and still authenticates: adding
	// identity must not have removed anything.
	if rec := doJSON(t, h, "GET", "/v1/namespaces/team-ml/specs/train/versions", adminKey, ""); rec.Code == http.StatusNotFound {
		t.Error("a pre-existing route disappeared from a gateway without identity")
	}
}
