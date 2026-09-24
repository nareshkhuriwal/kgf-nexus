// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// fakeClock lets the refill behaviour be tested without sleeping, so the tests
// assert on the arithmetic rather than on the scheduler.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestDisabledByDefault(t *testing.T) {
	// A zero rate must disable the limiter entirely. This is what keeps every
	// existing deployment and test unchanged, so it is the most important case
	// here: if it regressed, every caller would start getting 429s.
	if l := newRateLimiter(RateLimitConfig{}, nil); l != nil {
		t.Fatal("a zero RequestsPerSecond built a limiter; rate limiting must be opt-in")
	}
	// A nil limiter must still be callable, because the guard calls it on every
	// request without checking.
	var nilLimiter *rateLimiter
	if ok, _ := nilLimiter.allow("k"); !ok {
		t.Error("a nil limiter refused a request")
	}
}

func TestBurstIsSpentThenRefunded(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 10, Burst: 5}, clk.Now)

	// The bucket starts FULL, so a cold client's first request is never refused.
	for i := 0; i < 5; i++ {
		if ok, _ := l.allow("k"); !ok {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}
	ok, wait := l.allow("k")
	if ok {
		t.Fatal("the 6th request was allowed; the burst is not bounded")
	}
	if wait <= 0 {
		t.Error("a refusal carried no Retry-After hint, which invites an immediate retry")
	}

	// At 10/s, 500ms refills 5 tokens.
	clk.advance(500 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if ok, _ := l.allow("k"); !ok {
			t.Fatalf("refill request %d was refused", i+1)
		}
	}
}

func TestRefillIsCappedAtBurst(t *testing.T) {
	// An idle key must not accumulate an unbounded allowance: otherwise a client
	// that sleeps an hour can spend 36,000 requests at once, which is precisely
	// the spike the limiter exists to prevent.
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 10, Burst: 5}, clk.Now)

	clk.advance(time.Hour)
	for i := 0; i < 5; i++ {
		if ok, _ := l.allow("k"); !ok {
			t.Fatalf("request %d after a long idle was refused", i+1)
		}
	}
	if ok, _ := l.allow("k"); ok {
		t.Error("an idle key accumulated more than its burst")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	// One noisy credential must not lock out the others, which is the failure that
	// would make the limiter itself the denial of service.
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 1, Burst: 1}, clk.Now)

	if ok, _ := l.allow("noisy"); !ok {
		t.Fatal("first request from the noisy key was refused")
	}
	if ok, _ := l.allow("noisy"); ok {
		t.Fatal("the noisy key exceeded its burst")
	}
	if ok, _ := l.allow("quiet"); !ok {
		t.Error("a different credential was refused because another key was throttled")
	}
}

func TestBurstDefaultsToTheRateButIsOtherwiseHonoured(t *testing.T) {
	// Omitting Burst must allow one second's worth at once, or a config that sets
	// only a rate would admit a single request and then throttle.
	if l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 20}, nil); l.cfg.Burst != 20 {
		t.Errorf("Burst with none set = %v, want 20", l.cfg.Burst)
	}
	// An explicitly SMALLER burst is a legitimate choice and must not be
	// overridden: silently widening it would make the configuration lie about
	// the spike the operator is allowing.
	if l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 20, Burst: 5}, nil); l.cfg.Burst != 5 {
		t.Errorf("explicit Burst 5 became %v; the operator's limit was overridden", l.cfg.Burst)
	}
	// A burst below one token could never admit a request at all.
	if l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 0.5, Burst: 0.2}, nil); l.cfg.Burst != 1 {
		t.Errorf("Burst = %v, want it floored at 1 so some request can pass", l.cfg.Burst)
	}
}

func TestIdleBucketsAreEvicted(t *testing.T) {
	// Without eviction the map grows once per key forever. A gateway sees rotated
	// and probed keys, so that is an unbounded leak in the one process every
	// request passes through.
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := newRateLimiter(RateLimitConfig{
		RequestsPerSecond: 10, Burst: 10, IdleEviction: time.Minute,
	}, clk.Now)

	for i := 0; i < 100; i++ {
		l.allow(string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}
	before := l.size()
	if before == 0 {
		t.Fatal("no buckets were created")
	}

	clk.advance(2 * time.Minute)
	l.allow("survivor")
	if got := l.size(); got != 1 {
		t.Errorf("buckets = %d after idling past eviction, want only the survivor (had %d)", got, before)
	}
}

func TestConcurrentAllowIsRaceFree(t *testing.T) {
	// The limiter sits on every request, so it is contended by definition. Run
	// with -race: a data race here corrupts the token count for real traffic.
	l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 1e6, Burst: 1e6}, nil)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				l.allow("shared")
				l.allow(string(rune('a' + i%26)))
			}
		}(i)
	}
	wg.Wait()
}

func TestIdleEvictionNeverForgivesAThrottledKey(t *testing.T) {
	// An eviction window shorter than the refill time would drop a throttled
	// bucket before it had refilled, and the key's next request would get a fresh
	// full one: a slow prober could escape the limit simply by pausing.
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 0.1, Burst: 1, IdleEviction: time.Second}, clk.Now)
	if l.cfg.IdleEviction < 10*time.Second {
		t.Fatalf("IdleEviction = %v, want it raised to the 10s refill time", l.cfg.IdleEviction)
	}
	l.allow("slow")
	clk.advance(5 * time.Second)
	l.allow("other") // gives eviction a chance to run
	if ok, _ := l.allow("slow"); ok {
		t.Fatal("a throttled key was evicted and forgiven before its bucket refilled")
	}
}

func TestLimiterMemoryIsBounded(t *testing.T) {
	// Idle eviction alone does not bound memory: a client with many source
	// addresses creates keys faster than they idle. The cap must hold, and must
	// drop the LEAST recently used key, so an active throttled key is not the one
	// forgiven.
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 1, Burst: 1, MaxEntries: 100}, clk.Now)

	l.allow("hot")
	for i := 0; i < 10_000; i++ {
		l.allow("spray-" + strconv.Itoa(i))
		if i%50 == 0 {
			// The throttled client keeps trying, which keeps it recently used.
			if ok, _ := l.allow("hot"); ok {
				t.Fatalf("iteration %d: the throttled key was evicted by the spray and got a fresh bucket", i)
			}
		}
	}
	if got := l.size(); got > 100 {
		t.Fatalf("limiter holds %d buckets, cap is 100", got)
	}
	if len(l.buckets) != l.lru.Len() {
		t.Fatalf("map (%d) and recency list (%d) disagree", len(l.buckets), l.lru.Len())
	}
	if l.pressureEvictions == 0 {
		t.Error("the cap was exceeded but no pressure eviction was counted")
	}
	if l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 1}, nil); l.cfg.MaxEntries != DefaultMaxRateLimitEntries {
		t.Errorf("MaxEntries default = %d, want %d", l.cfg.MaxEntries, DefaultMaxRateLimitEntries)
	}
}

func TestBlockedNeitherSpendsNorAllocates(t *testing.T) {
	// The auth-failure gate runs on every authentication attempt. Checking must
	// not count as a failure, and must not create a bucket for an address that has
	// never failed — otherwise the check is itself a per-address allocation.
	clk := &fakeClock{now: time.Unix(0, 0)}
	l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 1, Burst: 2}, clk.Now)
	for i := 0; i < 10; i++ {
		if v := l.blocked("never-failed"); !v.ok {
			t.Fatal("an address with no failures was blocked")
		}
	}
	if l.size() != 0 {
		t.Fatalf("blocked() allocated %d buckets", l.size())
	}
	l.take("k")
	l.take("k")
	v := l.blocked("k")
	if v.ok || v.retryAfter < time.Second {
		t.Fatalf("exhausted key: %+v, want blocked with a retry hint", v)
	}
	clk.advance(time.Second)
	if v := l.blocked("k"); !v.ok {
		t.Fatal("key still blocked after a token refilled")
	}
	if v := l.blocked("k"); !v.ok {
		t.Fatal("blocked() spent the token it only checked")
	}
}

// ── keys ─────────────────────────────────────────────────────────────────

func reqFrom(remote, realIP string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	r.RemoteAddr = remote
	if realIP != "" {
		r.Header.Set("X-Real-IP", realIP)
	}
	return r
}

// An unauthenticated client must land in the same bucket on every connection.
// Keying on raw RemoteAddr included the source port, which changes per TCP
// connection, so reconnecting was enough to escape the limit entirely.
func TestAddressKeyIgnoresTheSourcePort(t *testing.T) {
	if a, b := clientAddressKey(reqFrom("203.0.113.7:50001", "")), clientAddressKey(reqFrom("203.0.113.7:50002", "")); a != b {
		t.Fatalf("two connections from one host keyed apart: %q vs %q", a, b)
	}
	if clientAddressKey(reqFrom("203.0.113.7:50001", "")) == clientAddressKey(reqFrom("203.0.113.8:50001", "")) {
		t.Fatal("two different hosts share one bucket")
	}
}

func TestAddressKeyFoldsIPv6ToItsSlash64(t *testing.T) {
	// One IPv6 host is normally handed a whole /64. Keying on the full address
	// would give it 2^64 buckets.
	same := []string{
		"[2001:db8:1:2::1]:443",
		"[2001:db8:1:2:ffff:ffff:ffff:fffe]:443",
		"[2001:db8:1:2:dead:beef::9]:1",
	}
	want := clientAddressKey(reqFrom(same[0], ""))
	if want != "ip6:2001:db8:1:2::/64" {
		t.Fatalf("key = %q, want the /64 prefix", want)
	}
	for _, a := range same[1:] {
		if got := clientAddressKey(reqFrom(a, "")); got != want {
			t.Errorf("%s keyed %q, want %q (same /64)", a, got, want)
		}
	}
	if got := clientAddressKey(reqFrom("[2001:db8:1:3::1]:443", "")); got == want {
		t.Error("a neighbouring /64 shares the bucket; the fold is too wide")
	}
	// An IPv4-mapped address is the IPv4 client, not a separate IPv6 bucket.
	if a, b := clientAddressKey(reqFrom("[::ffff:203.0.113.7]:1", "")), clientAddressKey(reqFrom("203.0.113.7:1", "")); a != b {
		t.Errorf("IPv4-mapped %q and IPv4 %q keyed apart", a, b)
	}

	// End to end: rotating through the /64 does not refill the bucket.
	l := newRateLimiter(RateLimitConfig{RequestsPerSecond: 1, Burst: 2}, func() time.Time { return time.Unix(0, 0) })
	refused := 0
	for i := 1; i <= 10; i++ {
		if ok, _ := l.allow(clientAddressKey(reqFrom(fmt.Sprintf("[2001:db8:1:2::%x]:443", i), ""))); !ok {
			refused++
		}
	}
	if refused != 8 {
		t.Fatalf("10 requests from 10 addresses in one /64 with burst 2: %d refused, want 8", refused)
	}
}

func TestAddressKeyTrustsXRealIPOnlyFromPrivatePeers(t *testing.T) {
	// A public peer choosing its own bucket by header is the bypass this guards.
	public := clientAddressKey(reqFrom("203.0.113.7:1", ""))
	for _, spoof := range []string{"198.51.100.1", "2001:db8::1", "garbage"} {
		if got := clientAddressKey(reqFrom("203.0.113.7:2", spoof)); got != public {
			t.Errorf("public peer with X-Real-IP %q keyed %q, want its own address %q", spoof, got, public)
		}
	}
	xff := reqFrom("203.0.113.7:3", "")
	xff.Header.Set("X-Forwarded-For", "198.51.100.1")
	if got := clientAddressKey(xff); got != public {
		t.Errorf("X-Forwarded-For chose the bucket: %q", got)
	}

	// The console's nginx on a private network or loopback overwrites X-Real-IP
	// with what it saw, so from those peers it is the client.
	for _, proxy := range []string{"10.0.0.5:1", "172.18.0.4:1", "192.168.1.2:1", "127.0.0.1:1", "[::1]:1", "[fd00::5]:1"} {
		if got := clientAddressKey(reqFrom(proxy, "198.51.100.1")); got != "ip4:198.51.100.1" {
			t.Errorf("private proxy %s: keyed %q, want the forwarded client", proxy, got)
		}
		if got := clientAddressKey(reqFrom(proxy, "2001:db8:5:6::77")); got != "ip6:2001:db8:5:6::/64" {
			t.Errorf("private proxy %s with an IPv6 client: keyed %q, want its /64", proxy, got)
		}
	}
	// A private proxy that sent an unparseable X-Real-IP is keyed as itself
	// rather than trusting junk.
	if got := clientAddressKey(reqFrom("172.18.0.4:1", "not-an-ip")); got != "ip4:172.18.0.4" {
		t.Errorf("unparseable X-Real-IP from a proxy: keyed %q", got)
	}
}

func TestCredentialKeyHoldsNoSecret(t *testing.T) {
	for _, tc := range []struct {
		p    authn.Principal
		want string
	}{
		{authn.Principal{KeyID: "k-1"}, "cred:api_key:k-1"},
		{authn.Principal{KeyID: "s-1", Kind: authn.KindSession}, "cred:session:s-1"},
		{authn.Principal{KeyID: "t-1", Kind: authn.KindScopedToken}, "cred:token:t-1"},
	} {
		if got := credentialKey(tc.p); got != tc.want {
			t.Errorf("credentialKey(%+v) = %q, want %q", tc.p, got, tc.want)
		}
	}
}

// ── through the HTTP guard ───────────────────────────────────────────────

func newLimitedAPI(t *testing.T, limits GatewayRateLimits, clk *fakeClock) (*API, http.Handler) {
	t.Helper()
	db := newRestDB()
	db.addKey(adminKey, "team-ml", "ci", "admin")
	db.addKey(readerKey, "team-ml", "dashboard", "reader")
	ids := newFakeIdentityAdmin()
	auth := authn.New(db).WithIdentity(sessionBridge{ids})
	api := New(db, auth, &fakeWFM{}, fakeRM{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithIdentity(ids)
	api.clock = clk.Now
	api.WithRateLimits(limits)
	return api, api.Routes()
}

func send(h http.Handler, method, path, remote, authz, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = remote
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// generous is a bucket that never refuses in these tests, so each test isolates
// the one limiter it is about.
var generous = RateLimitConfig{RequestsPerSecond: 1e6, Burst: 1e6}

// TestRandomAuthorizationCannotBypassTheLimit is the F99 regression. The
// pre-auth limiter used to key on the raw Authorization header, so a client
// sending a different random value per request got a fresh bucket each time and
// was never limited — while every such request cost an authentication lookup and
// left its header in the limiter's map.
func TestRandomAuthorizationCannotBypassTheLimit(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	api, h := newLimitedAPI(t, GatewayRateLimits{
		PerAddress:    RateLimitConfig{RequestsPerSecond: 1, Burst: 5},
		PerCredential: generous,
	}, clk)

	var limited int
	for i := 0; i < 50; i++ {
		rec := send(h, "GET", "/v1/auth/whoami", "203.0.113.50:"+strconv.Itoa(40000+i),
			fmt.Sprintf("Bearer random-%d-%d", i, time.Now().UnixNano()), "")
		switch rec.Code {
		case http.StatusUnauthorized:
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Fatalf("request %d: status %d", i, rec.Code)
		}
	}
	if limited != 45 {
		t.Fatalf("50 random-credential requests from one address with burst 5: %d limited, want 45", limited)
	}
	if n := api.limits.address.size(); n != 1 {
		t.Errorf("address limiter holds %d buckets, want 1 — the key must not depend on the header", n)
	}
	if n := api.limits.credential.size(); n != 0 {
		t.Errorf("credential limiter holds %d buckets for UNAUTHENTICATED requests, want 0", n)
	}
	// And a different address is unaffected.
	if rec := send(h, "GET", "/v1/auth/whoami", "203.0.113.51:1", "Bearer "+adminKey, ""); rec.Code != http.StatusOK {
		t.Errorf("another address: %d, want 200", rec.Code)
	}
}

func TestRateLimitedResponseCarriesRetryHeaders(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	_, h := newLimitedAPI(t, GatewayRateLimits{
		// 0.4/s: the next token is 2.5s away, so Retry-After must round UP to 3.
		PerAddress: RateLimitConfig{RequestsPerSecond: 0.4, Burst: 1},
	}, clk)
	send(h, "GET", "/v1/auth/whoami", "203.0.113.60:1", "", "")
	rec := send(h, "GET", "/v1/auth/whoami", "203.0.113.60:2", "", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", rec.Code)
	}
	for hdr, want := range map[string]string{
		"Retry-After":         "3",
		"RateLimit-Limit":     "1",
		"RateLimit-Remaining": "0",
		"RateLimit-Reset":     "3",
	} {
		if got := rec.Header().Get(hdr); got != want {
			t.Errorf("%s = %q, want %q", hdr, got, want)
		}
	}
	body := decode(t, rec)
	if body["error"] != "rate_limited" || !strings.Contains(body["message"].(string), "address") {
		t.Errorf("body = %v, want error rate_limited naming the address bucket", body)
	}
}

func TestPerCredentialLimitAppliesAfterAuthentication(t *testing.T) {
	// Two valid credentials behind ONE address (a CI egress): the noisy one is
	// held to its own allowance and the quiet one is not starved by it.
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	api, h := newLimitedAPI(t, GatewayRateLimits{
		PerAddress:    generous,
		PerCredential: RateLimitConfig{RequestsPerSecond: 1, Burst: 2},
	}, clk)
	const addr = "203.0.113.70:1"
	for i := 0; i < 2; i++ {
		if rec := send(h, "GET", "/v1/auth/whoami", addr, "Bearer "+adminKey, ""); rec.Code != http.StatusOK {
			t.Fatalf("admin request %d: %d", i+1, rec.Code)
		}
	}
	rec := send(h, "GET", "/v1/auth/whoami", addr, "Bearer "+adminKey, "")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "credential") {
		t.Fatalf("third admin request: %d %s, want 429 naming the credential", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" || rec.Header().Get("RateLimit-Limit") != "2" {
		t.Errorf("credential 429 headers: %v", rec.Header())
	}
	// The same key presented in another spelling (bare, not "Bearer ...") is the
	// same credential and the same bucket: the key is the resolved ID.
	if rec := send(h, "GET", "/v1/auth/whoami", addr, adminKey, ""); rec.Code != http.StatusTooManyRequests {
		t.Errorf("bare spelling of the throttled key: %d, want 429 (same credential)", rec.Code)
	}
	if rec := send(h, "GET", "/v1/auth/whoami", addr, "Bearer "+readerKey, ""); rec.Code != http.StatusOK {
		t.Errorf("a different credential at the same address: %d, want 200", rec.Code)
	}
	// Buckets are keyed by credential ID; no secret is held in memory.
	api.limits.credential.mu.Lock()
	defer api.limits.credential.mu.Unlock()
	for k := range api.limits.credential.buckets {
		if strings.Contains(k, adminKey) || strings.Contains(k, readerKey) {
			t.Errorf("limiter key %q contains credential material", k)
		}
	}
	if _, ok := api.limits.credential.buckets["cred:api_key:key-ci"]; !ok {
		t.Error("no bucket keyed by the admin key's ID")
	}
}

func TestFailedAuthenticationSpendsAStricterAddressBudget(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	api, h := newLimitedAPI(t, GatewayRateLimits{
		PerAddress:    generous,
		AuthFailures:  RateLimitConfig{RequestsPerSecond: 0.1, Burst: 3},
		PerCredential: generous,
	}, clk)
	const addr = "203.0.113.80:1"

	// Successful authentication never spends the failure budget.
	for i := 0; i < 10; i++ {
		if rec := send(h, "GET", "/v1/auth/whoami", addr, "Bearer "+adminKey, ""); rec.Code != http.StatusOK {
			t.Fatalf("valid request %d: %d", i, rec.Code)
		}
	}
	if n := api.limits.authFailures.size(); n != 0 {
		t.Fatalf("successful requests created %d failure buckets", n)
	}
	for i := 0; i < 3; i++ {
		if rec := send(h, "GET", "/v1/auth/whoami", addr, "Bearer guess-"+strconv.Itoa(i), ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d: %d, want 401", i+1, rec.Code)
		}
	}
	rec := send(h, "GET", "/v1/auth/whoami", addr, "Bearer guess-4", "")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "failed authentication") {
		t.Fatalf("fourth guess: %d %s, want 429 for failed attempts", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") != "10" {
		t.Errorf("Retry-After = %q, want 10 (one failure token at 0.1/s)", rec.Header().Get("Retry-After"))
	}
	// Blocked means blocked: the address may not try ANY credential until the
	// budget refills, or the attacker would simply interleave a known-good key.
	if rec := send(h, "GET", "/v1/auth/whoami", addr, "Bearer "+adminKey, ""); rec.Code != http.StatusTooManyRequests {
		t.Errorf("valid key from a blocked address: %d, want 429", rec.Code)
	}
	// No credential at all is not a guess and still gets its honest 401.
	if rec := send(h, "GET", "/v1/auth/whoami", addr, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("credential-less request from a blocked address: %d, want 401", rec.Code)
	}
	// Other addresses are unaffected.
	if rec := send(h, "GET", "/v1/auth/whoami", "203.0.113.81:1", "Bearer "+adminKey, ""); rec.Code != http.StatusOK {
		t.Errorf("another address: %d, want 200", rec.Code)
	}
	// It is a throttle, not a lockout: one token back, one more attempt.
	clk.advance(10 * time.Second)
	if rec := send(h, "GET", "/v1/auth/whoami", addr, "Bearer "+adminKey, ""); rec.Code != http.StatusOK {
		t.Errorf("after refill: %d, want 200", rec.Code)
	}
}

func TestFailedLoginsSpendTheAddressFailureBudget(t *testing.T) {
	// Login is the unguarded route and the one credential stuffing targets.
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	_, h := newLimitedAPI(t, GatewayRateLimits{
		PerAddress:   generous,
		AuthFailures: RateLimitConfig{RequestsPerSecond: 0.01, Burst: 2},
	}, clk)
	const addr = "203.0.113.90:1"
	right := `{"namespace":"team-ml","email":"admin@example.com","password":"correct-password-1234"}`

	// Distinct guesses, as a stuffing list would make (a repeat of the same wrong
	// password is charged once; see TestRepeatingOneRejectedCredentialIsChargedOnce).
	for i := 0; i < 2; i++ {
		wrong := `{"namespace":"team-ml","email":"admin@example.com","password":"wrong-password-` + strconv.Itoa(i) + `"}`
		if rec := send(h, "POST", "/v1/auth/login", addr, "", wrong); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d: %d", i+1, rec.Code)
		}
	}
	// Refused before the password is even checked — including the right one.
	if rec := send(h, "POST", "/v1/auth/login", addr, "", right); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("login after the budget is spent: %d, want 429", rec.Code)
	}
	if rec := send(h, "POST", "/v1/auth/login", "203.0.113.91:1", "", right); rec.Code != http.StatusOK {
		t.Errorf("login from another address: %d, want 200", rec.Code)
	}
}

func TestGuardUnderConcurrentLoadIsRaceFree(t *testing.T) {
	// All three limiters on the real handler, contended. Run with -race. Also
	// checks the arithmetic holds under contention: exactly Burst requests pass
	// the address bucket with a frozen clock.
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	api, h := newLimitedAPI(t, GatewayRateLimits{
		PerAddress:    RateLimitConfig{RequestsPerSecond: 1, Burst: 100},
		AuthFailures:  RateLimitConfig{RequestsPerSecond: 1, Burst: 1000},
		PerCredential: RateLimitConfig{RequestsPerSecond: 1, Burst: 1000},
	}, clk)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		pass int
	)
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				authz := "Bearer " + adminKey
				if j%3 == 0 {
					authz = "Bearer bogus-" + strconv.Itoa(g*100+j)
				}
				rec := send(h, "GET", "/v1/auth/whoami", "198.51.100.7:"+strconv.Itoa(1000+g), authz, "")
				if rec.Code != http.StatusTooManyRequests {
					mu.Lock()
					pass++
					mu.Unlock()
				}
			}
		}(g)
	}
	wg.Wait()
	if pass != 100 {
		t.Fatalf("%d requests passed a frozen address bucket of 100 under contention", pass)
	}
	if n := api.limits.address.size(); n != 1 {
		t.Errorf("address buckets = %d, want 1", n)
	}
}

func TestGatewayRateLimitsForKeepsTheOriginalKnobsMeaning(t *testing.T) {
	if got := GatewayRateLimitsFor(RateLimitConfig{}); got != (GatewayRateLimits{}) {
		t.Fatalf("no per-credential rate enabled %+v; rate limiting must stay opt-in", got)
	}
	g := GatewayRateLimitsFor(RateLimitConfig{RequestsPerSecond: 50, Burst: 100})
	if g.PerCredential.RequestsPerSecond != 50 || g.PerCredential.Burst != 100 {
		t.Errorf("per-credential = %+v, want the configured 50/100 unchanged", g.PerCredential)
	}
	if g.PerAddress.RequestsPerSecond != 200 || g.PerAddress.Burst != 400 {
		t.Errorf("per-address = %+v, want 4x the per-credential allowance", g.PerAddress)
	}
	if g.AuthFailures != DefaultAuthFailureLimit {
		t.Errorf("auth failures = %+v, want DefaultAuthFailureLimit", g.AuthFailures)
	}
	// A rate with no burst: the address burst scales the implied burst (= rate).
	if g := GatewayRateLimitsFor(RateLimitConfig{RequestsPerSecond: 10}); g.PerAddress.Burst != 40 {
		t.Errorf("per-address burst with no burst set = %v, want 40", g.PerAddress.Burst)
	}
	// A nil set of limiters (rate limiting off) admits everything.
	var off gatewayLimiters
	if !off.address.take("k").ok || !off.authFailures.blocked("k").ok || !off.credential.take("k").ok {
		t.Error("a disabled limiter refused")
	}
}

func TestRepeatingOneRejectedCredentialIsChargedOnce(t *testing.T) {
	// A console tab polling with a revoked session, or a CI job with a rotated-out
	// key, repeats ONE rejected value many times a second. Charging each repeat
	// would exhaust the address's failure budget and then 429 that person's own
	// sign-in. Guessing presents a different value each time and is still charged.
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	api, h := newLimitedAPI(t, GatewayRateLimits{
		PerAddress:   generous,
		AuthFailures: RateLimitConfig{RequestsPerSecond: 0.01, Burst: 3},
	}, clk)
	const addr = "203.0.113.95:1"
	for i := 0; i < 50; i++ {
		if rec := send(h, "GET", "/v1/auth/whoami", addr, "Bearer nx_revoked_key", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("stale poll %d: %d, want 401 (repeats must not exhaust the budget)", i+1, rec.Code)
		}
	}
	right := `{"namespace":"team-ml","email":"admin@example.com","password":"correct-password-1234"}`
	if rec := send(h, "POST", "/v1/auth/login", addr, "", right); rec.Code != http.StatusOK {
		t.Fatalf("sign-in after a stale tab polled: %d, want 200", rec.Code)
	}
	// The fingerprint store holds a keyed MAC, not the credential.
	api.limits.failureSeen.mu.Lock()
	for k := range api.limits.failureSeen.buckets {
		if strings.Contains(k, "nx_revoked_key") {
			t.Errorf("failure fingerprint %q contains the credential", k)
		}
	}
	api.limits.failureSeen.mu.Unlock()
	// Two more DISTINCT guesses use up the remaining budget (one was charged).
	for i := 0; i < 2; i++ {
		send(h, "GET", "/v1/auth/whoami", addr, "Bearer guess-"+strconv.Itoa(i), "")
	}
	if rec := send(h, "GET", "/v1/auth/whoami", addr, "Bearer guess-x", ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after 3 distinct failures: %d, want 429", rec.Code)
	}
}
