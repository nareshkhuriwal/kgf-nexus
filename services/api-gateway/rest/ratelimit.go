// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"container/list"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// Rate limiting on the gateway, which is the platform's only ingress.
//
// ARCHITECTURE.md §3.2 lists "rate limit" among the gateway's responsibilities and
// nothing implemented it, so the sole entry point had no protection against a
// request flood or credential stuffing — and the CLI already carries a hint for a
// 429 the server could never send (cli/internal/client/client.go). A gateway with
// no limit also has no defence against a well-meaning client in a tight retry
// loop, which is the common way a control plane gets taken down by its own users.
//
// THREE BUCKETS, applied in this order (F99):
//
//  1. Per client address, BEFORE authentication, on every request. Nothing the
//     client sends may choose this key: it is the peer address (requesterIP's
//     proxy rule), IPv4 as-is and IPv6 folded to its /64, because a single IPv6
//     host is routinely handed a whole /64 and could otherwise take a fresh
//     bucket per address.
//  2. Per client address, for FAILED authentication. A stricter bucket that is
//     only spent when a presented credential or password is rejected, and that
//     refuses further attempts from that address once empty. This is what slows
//     credential stuffing: the address bucket alone is sized for legitimate
//     traffic from a NAT egress, which is far too generous a guessing rate.
//  3. Per credential, AFTER authentication, keyed by the credential's stable
//     database ID (api key, session or scoped token). The unit an operator can act
//     on — they can revoke it — and the one that keeps one noisy integration
//     behind a shared egress from starving its neighbours.
//
// WHY NOT KEY THE PRE-AUTH CHECK ON THE CREDENTIAL, as this file used to: the
// presented Authorization header is attacker-controlled until it is verified. A
// client sending a random value per request got a brand-new, full bucket every
// time, so the limit bound nothing for exactly the unauthenticated flood it existed
// for — and each such request cost a database lookup and a map entry holding the
// raw header, i.e. a secret-bearing, unbounded key.
//
// WHY A TOKEN BUCKET rather than a fixed window: a fixed window lets a caller
// spend its whole allowance in the last millisecond of one window and again in the
// first of the next, so the instantaneous rate is twice the configured one exactly
// when the system is already under pressure. A bucket bounds the burst explicitly
// and refills smoothly, so "100 per second with a burst of 200" means that.
//
// Reads and writes share one bucket per key deliberately. Splitting them would let
// a caller exhaust the database with reads while its write budget sat unused, and
// the resource being protected — one Postgres — does not care which verb arrived.
//
// PER REPLICA. Every bucket lives in this process's memory. N gateway replicas
// behind a load balancer admit up to N times each configured rate, and a restart
// forgets every bucket. That is the accepted trade for a limiter with no network
// dependency on the hot path; a hard global quota needs a shared store and is not
// what this is. docs/OPERATIONS.md §16 says how to size for it.

// RateLimitConfig configures one limiter. A zero RequestsPerSecond disables it,
// which is what keeps the dev path and the existing tests unchanged.
type RateLimitConfig struct {
	// RequestsPerSecond is the sustained rate allowed per key.
	RequestsPerSecond float64
	// Burst is the most a key may spend at once. Zero means "same as
	// RequestsPerSecond", so a config that sets only a rate still allows one
	// second's worth at once rather than a single request. An explicit smaller
	// burst is honoured: throttling bursts harder than the sustained rate is a
	// legitimate choice, and silently overriding it would make the configuration
	// lie.
	Burst float64
	// IdleEviction is how long an unused bucket is kept. Without eviction the map
	// grows once per key forever, which for a gateway that sees rotated or probed
	// keys is an unbounded leak in the one process every request passes through.
	// It is raised to at least the bucket's refill time (Burst/RequestsPerSecond),
	// so eviction only ever drops a bucket that had refilled anyway and never
	// forgives a throttled key early.
	IdleEviction time.Duration
	// MaxEntries bounds the number of live buckets; zero means
	// DefaultMaxRateLimitEntries. Idle eviction alone does not bound memory: a
	// client that controls many source addresses can create keys faster than they
	// go idle. At the cap the least recently used bucket is dropped (LRU).
	MaxEntries int
}

// DefaultRateLimit is deliberately generous. The limiter exists to stop a runaway
// client or a brute-force attempt, not to meter legitimate use: the console polls,
// the CLI batches, and a limit tight enough to shape traffic would break them while
// barely inconveniencing an attacker who can simply use more keys.
var DefaultRateLimit = RateLimitConfig{
	RequestsPerSecond: 50,
	Burst:             100,
	IdleEviction:      10 * time.Minute,
}

// DefaultMaxRateLimitEntries caps each limiter's map. A bucket is a short key
// plus a list element — under 200 bytes — so the cap holds a limiter under
// ~20 MB however many addresses or credentials an attacker brings.
const DefaultMaxRateLimitEntries = 100_000

// DefaultAuthFailureLimit is the per-address budget for rejected credentials and
// passwords: ten at once, then one every six seconds. A person mistyping a
// password never meets it; a stuffing run from one address is held to ten guesses
// a minute instead of the address bucket's thousands.
var DefaultAuthFailureLimit = RateLimitConfig{
	RequestsPerSecond: 1.0 / 6,
	Burst:             10,
	IdleEviction:      15 * time.Minute,
}

// addressRateMultiplier sizes the default per-address bucket from the
// per-credential one. The address bucket must admit several principals sharing
// one egress (a CI fleet behind NAT, an office), so it cannot equal one
// credential's allowance without making the per-credential limit the address
// limit. It still stops a single unauthenticated client flooding, and the
// auth-failure bucket — not this one — is what bounds guessing.
const addressRateMultiplier = 4

// GatewayRateLimits configures the gateway's three limiters. Each is disabled
// independently by a zero RequestsPerSecond.
type GatewayRateLimits struct {
	// PerAddress applies before authentication to every request, keyed by client
	// address (IPv6 by /64).
	PerAddress RateLimitConfig
	// AuthFailures is spent once per rejected credential or password, keyed by
	// client address, and refuses authentication attempts once empty.
	AuthFailures RateLimitConfig
	// PerCredential applies after authentication, keyed by credential ID.
	PerCredential RateLimitConfig
}

// GatewayRateLimitsFor derives the three limiters from the one per-credential
// setting that predates them (NEXUS_RATE_LIMIT_RPS / _BURST), so a deployment
// that set only those keeps working and gains the address and auth-failure
// buckets with defaults scaled from its own numbers.
func GatewayRateLimitsFor(perCredential RateLimitConfig) GatewayRateLimits {
	if perCredential.RequestsPerSecond <= 0 {
		return GatewayRateLimits{}
	}
	burst := perCredential.Burst
	if burst <= 0 {
		burst = perCredential.RequestsPerSecond
	}
	return GatewayRateLimits{
		PerAddress: RateLimitConfig{
			RequestsPerSecond: perCredential.RequestsPerSecond * addressRateMultiplier,
			Burst:             burst * addressRateMultiplier,
			IdleEviction:      perCredential.IdleEviction,
			MaxEntries:        perCredential.MaxEntries,
		},
		AuthFailures:  withMaxEntries(DefaultAuthFailureLimit, perCredential.MaxEntries),
		PerCredential: perCredential,
	}
}

func withMaxEntries(c RateLimitConfig, n int) RateLimitConfig {
	c.MaxEntries = n
	return c
}

type bucket struct {
	key    string
	tokens float64
	// last is when tokens was last refilled; it doubles as the LRU recency,
	// because every access refills first.
	last time.Time
}

// verdict is one limiter decision, carrying what a 429 needs to explain itself.
type verdict struct {
	ok bool
	// limit is the bucket's burst: the most requests it admits at once.
	limit int
	// remaining is whole tokens left after this decision.
	remaining int
	// retryAfter is how long until one token exists; zero when ok.
	retryAfter time.Duration
}

// rateLimiter is a per-key token bucket with bounded memory.
//
// Buckets sit on a recency list (front = most recently used). Idle eviction pops
// from the back while the back is idle, so it costs O(evicted) rather than a scan
// of every key, and the size cap evicts the same end.
type rateLimiter struct {
	cfg   RateLimitConfig
	clock func() time.Time

	mu      sync.Mutex
	buckets map[string]*list.Element
	lru     *list.List
	// pressureEvictions counts buckets dropped by the size cap rather than by
	// idling. Non-zero means MaxEntries is too small for the traffic, or the
	// gateway is being sprayed from many addresses.
	pressureEvictions uint64
}

func newRateLimiter(cfg RateLimitConfig, clock func() time.Time) *rateLimiter {
	if cfg.RequestsPerSecond <= 0 {
		return nil
	}
	if cfg.Burst <= 0 {
		cfg.Burst = cfg.RequestsPerSecond
	}
	if cfg.Burst < 1 {
		// A burst below one token can never admit a request, which would refuse
		// all traffic rather than limiting it.
		cfg.Burst = 1
	}
	if cfg.IdleEviction <= 0 {
		cfg.IdleEviction = DefaultRateLimit.IdleEviction
	}
	if refill := time.Duration(cfg.Burst / cfg.RequestsPerSecond * float64(time.Second)); cfg.IdleEviction < refill {
		// Evicting a bucket that has not refilled would hand its key a fresh full
		// bucket early — a slow client could escape the limit by pausing.
		cfg.IdleEviction = refill
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = DefaultMaxRateLimitEntries
	}
	if clock == nil {
		clock = time.Now
	}
	return &rateLimiter{cfg: cfg, clock: clock, buckets: map[string]*list.Element{}, lru: list.New()}
}

// allow spends one token for key, reporting whether the request may proceed and,
// when it may not, how long the caller should wait before retrying.
//
// The retry hint matters: a 429 with no Retry-After invites an immediate retry,
// which is how a limiter turns one overloaded client into a busy loop.
func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	v := l.take(key)
	return v.ok, v.retryAfter
}

// take spends one token for key.
func (l *rateLimiter) take(key string) verdict {
	if l == nil {
		return verdict{ok: true}
	}
	now := l.clock()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.evictIdle(now)

	b := l.touch(key, now)
	if b.tokens >= 1 {
		b.tokens--
		return verdict{ok: true, limit: l.limit(), remaining: int(b.tokens)}
	}
	return l.refusal(b)
}

// blocked reports, WITHOUT spending, whether key's bucket is empty. It is the
// auth-failure gate: an address is refused another attempt while its failure
// budget is exhausted, but a check must not itself count as a failure. An
// unknown key is not blocked and is not created, so the check allocates nothing
// for the vast majority of callers who have never failed.
func (l *rateLimiter) blocked(key string) verdict {
	if l == nil {
		return verdict{ok: true}
	}
	now := l.clock()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.evictIdle(now)

	if _, ok := l.buckets[key]; !ok {
		return verdict{ok: true, limit: l.limit(), remaining: l.limit()}
	}
	b := l.touch(key, now)
	if b.tokens >= 1 {
		return verdict{ok: true, limit: l.limit(), remaining: int(b.tokens)}
	}
	return l.refusal(b)
}

// touch returns key's bucket refilled to now, creating it full if absent and
// marking it most recently used. Caller holds mu.
func (l *rateLimiter) touch(key string, now time.Time) *bucket {
	if el, ok := l.buckets[key]; ok {
		b := el.Value.(*bucket)
		if elapsed := now.Sub(b.last); elapsed > 0 {
			b.tokens += elapsed.Seconds() * l.cfg.RequestsPerSecond
			if b.tokens > l.cfg.Burst {
				b.tokens = l.cfg.Burst
			}
			b.last = now
		}
		l.lru.MoveToFront(el)
		return b
	}
	for len(l.buckets) >= l.cfg.MaxEntries {
		l.removeBack()
		l.pressureEvictions++
	}
	// A new key starts full, so a first request is never rejected. Starting
	// empty would make every cold client's first call a 429.
	b := &bucket{key: key, tokens: l.cfg.Burst, last: now}
	l.buckets[key] = l.lru.PushFront(b)
	return b
}

// evictIdle drops buckets unused for IdleEviction, oldest first, stopping at the
// first one still in use. Caller holds mu.
func (l *rateLimiter) evictIdle(now time.Time) {
	for el := l.lru.Back(); el != nil; el = l.lru.Back() {
		if now.Sub(el.Value.(*bucket).last) < l.cfg.IdleEviction {
			return
		}
		l.removeBack()
	}
}

func (l *rateLimiter) removeBack() {
	el := l.lru.Back()
	if el == nil {
		return
	}
	l.lru.Remove(el)
	delete(l.buckets, el.Value.(*bucket).key)
}

func (l *rateLimiter) limit() int { return int(l.cfg.Burst) }

// refusal is the verdict for an empty bucket. Caller holds mu.
func (l *rateLimiter) refusal(b *bucket) verdict {
	// Time until one whole token exists, never under a second: Retry-After is in
	// whole seconds, and one that expires a moment early just produces another 429.
	wait := time.Duration((1 - b.tokens) / l.cfg.RequestsPerSecond * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	return verdict{ok: false, limit: l.limit(), remaining: 0, retryAfter: wait}
}

// size reports the number of live buckets.
func (l *rateLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// ── keys ─────────────────────────────────────────────────────────────────

// clientAddressKey is the per-address limiter key: requesterIP (the peer, or the
// X-Real-IP a private-network proxy set), with IPv6 folded to its /64.
//
// The /64 matters because it is the unit a single IPv6 subscriber or host is
// normally assigned: keying on the full address would let one machine rotate
// through 2^64 source addresses, each with a fresh bucket. An IPv4-mapped IPv6
// address is treated as the IPv4 it carries, so dual-stack listeners do not give
// one client two buckets.
func clientAddressKey(r *http.Request) string {
	host := requesterIP(r)
	ip := net.ParseIP(host)
	if ip == nil {
		// Not an address (a unix socket, a test harness): key on the text, which
		// is still something the client did not choose.
		return "addr:" + host
	}
	if v4 := ip.To4(); v4 != nil {
		return "ip4:" + v4.String()
	}
	return "ip6:" + ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// credentialKey is the per-credential limiter key: the credential's kind and its
// database row ID. Never the presented secret: the ID is stable across requests,
// is not usable to authenticate, and is short, so the limiter's memory holds no
// credential material and no attacker-sized strings.
func credentialKey(p authn.Principal) string {
	kind := p.Kind
	if kind == "" {
		kind = authn.KindAPIKey
	}
	return "cred:" + string(kind) + ":" + p.KeyID
}

// ── HTTP ─────────────────────────────────────────────────────────────────

// rateLimited applies the per-address limit, writing the 429 if refused. Every
// route calls it first, before any authentication or database work.
//
// Before authentication on purpose: an unauthenticated flood is the case that
// most needs limiting, and authenticating first would spend a database round trip
// per rejected request — making the limiter itself the amplifier. It is keyed on
// the address, not on anything in the request, so the client cannot pick a fresh
// bucket.
func (a *API) rateLimited(w http.ResponseWriter, r *http.Request) bool {
	v := a.limits.address.take(clientAddressKey(r))
	if v.ok {
		return false
	}
	writeRateLimited(w, v, "too many requests from this address; retry after "+retryAfterSeconds(v)+"s")
	return true
}

// authAttemptBlocked refuses an authentication attempt from an address whose
// failure budget is exhausted. It does not spend: only a failure does.
func (a *API) authAttemptBlocked(w http.ResponseWriter, r *http.Request) bool {
	v := a.limits.authFailures.blocked(clientAddressKey(r))
	if v.ok {
		return false
	}
	writeRateLimited(w, v, "too many failed authentication attempts from this address; retry after "+retryAfterSeconds(v)+"s")
	return true
}

// recordAuthFailure spends one token of the address's failure budget for a
// rejected attempt. material is what was presented (the credential, or the
// login's namespace, email and password); it is only ever hashed.
//
// The SAME rejected material from the same address is charged once per
// repeatFailureWindow, not once per request. A guessing run presents a
// different value every time, so this costs it nothing; but a console tab still
// polling with a session an admin revoked, or a CI job with a rotated-out key,
// repeats one value many times a second, and charging each repeat would
// exhaust the budget and then 429 that person's own sign-in from that address.
func (a *API) recordAuthFailure(r *http.Request, material string) {
	if a.limits.authFailures == nil {
		return
	}
	addr := clientAddressKey(r)
	if !a.limits.failureSeen.take(a.limits.fingerprint(addr, material)).ok {
		return // a repeat within the window: already charged
	}
	a.limits.authFailures.take(addr)
}

// credentialRateLimited applies the per-credential limit to an AUTHENTICATED
// principal, writing the 429 if refused.
func (a *API) credentialRateLimited(w http.ResponseWriter, p authn.Principal) bool {
	v := a.limits.credential.take(credentialKey(p))
	if v.ok {
		return false
	}
	writeRateLimited(w, v, "too many requests for this credential; retry after "+retryAfterSeconds(v)+"s")
	return true
}

// writeRateLimited writes a 429 with the retry hint in both header forms.
//
// Retry-After (RFC 9110 §10.2.3) is what every HTTP client, the CLI and the
// Python SDK already honour. The RateLimit-Limit / -Remaining / -Reset trio
// follows the IETF httpapi rate-limit header draft as widely deployed, so a
// generic client can back off without parsing the message. The body keeps the
// gateway's usual {"error","message"} shape with the stable code rate_limited.
func writeRateLimited(w http.ResponseWriter, v verdict, message string) {
	secs := retryAfterSeconds(v)
	h := w.Header()
	h.Set("Retry-After", secs)
	h.Set("RateLimit-Limit", strconv.Itoa(v.limit))
	h.Set("RateLimit-Remaining", strconv.Itoa(v.remaining))
	h.Set("RateLimit-Reset", secs)
	writeError(w, http.StatusTooManyRequests, "rate_limited", message)
}

// retryAfterSeconds is the wait in whole seconds, rounded UP and at least one.
func retryAfterSeconds(v verdict) string {
	secs := int(math.Ceil(v.retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return strconv.Itoa(secs)
}

// repeatFailureWindow is how long one rejected (address, material) pair is
// remembered so its repeats are not charged again. See recordAuthFailure.
const repeatFailureWindow = 5 * time.Minute

// gatewayLimiters holds the three buckets. Each is nil when disabled, and a nil
// limiter admits everything, so call sites need no checks.
type gatewayLimiters struct {
	address      *rateLimiter
	authFailures *rateLimiter
	credential   *rateLimiter

	// failureSeen remembers recently charged failures: a one-token bucket per
	// fingerprint that refills over repeatFailureWindow, so take() succeeds the
	// first time a fingerprint is seen in the window and fails on repeats. It is
	// bounded like every other limiter.
	failureSeen *rateLimiter
	// fpKey keys the fingerprint HMAC. Random per process, so the map holds
	// nothing an offline guess could be checked against: what it stores is not a
	// hash of the credential or password but a keyed MAC that dies with the
	// process.
	fpKey []byte
}

func newGatewayLimiters(cfg GatewayRateLimits, clock func() time.Time) gatewayLimiters {
	g := gatewayLimiters{
		address:      newRateLimiter(cfg.PerAddress, clock),
		authFailures: newRateLimiter(cfg.AuthFailures, clock),
		credential:   newRateLimiter(cfg.PerCredential, clock),
	}
	if g.authFailures != nil {
		g.failureSeen = newRateLimiter(RateLimitConfig{
			RequestsPerSecond: 1 / repeatFailureWindow.Seconds(),
			Burst:             1,
			IdleEviction:      repeatFailureWindow,
			MaxEntries:        cfg.AuthFailures.MaxEntries,
		}, clock)
		g.fpKey = make([]byte, 32)
		if _, err := rand.Read(g.fpKey); err != nil {
			// crypto/rand does not fail on supported platforms; if it ever did,
			// running without repeat suppression is safer than running without
			// the failure budget.
			g.failureSeen = nil
		}
	}
	return g
}

// fingerprint is the failureSeen key for one presented value from one address.
func (g gatewayLimiters) fingerprint(addr, material string) string {
	m := hmac.New(sha256.New, g.fpKey)
	m.Write([]byte(addr))
	m.Write([]byte{0})
	m.Write([]byte(material))
	return hex.EncodeToString(m.Sum(nil)[:16])
}
