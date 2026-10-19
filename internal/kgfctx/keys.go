// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package kgfctx

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// StaticKeys trusts a fixed set of public keys, each under its derived kid.
type StaticKeys map[string]ed25519.PublicKey

// ParseStaticKeys reads a comma-separated list of base64url public keys
// (tenantctx's KGF_CONTEXT_PUBLIC_KEYS format).
func ParseStaticKeys(list string) (StaticKeys, error) {
	out := StaticKeys{}
	for _, item := range strings.Split(list, ",") {
		if strings.TrimSpace(item) == "" {
			continue
		}
		pub, err := DecodePublicKey(item)
		if err != nil {
			return nil, fmt.Errorf("public key %q: %w", item, err)
		}
		out[KeyID(pub)] = pub
	}
	return out, nil
}

// Key returns the key for kid.
func (s StaticKeys) Key(kid string) (ed25519.PublicKey, error) {
	if k, ok := s[kid]; ok {
		return k, nil
	}
	if len(s) == 0 {
		return nil, ErrNoKeys
	}
	return nil, ErrUnknownKey
}

// wellKnown is the body of /.well-known/kgf-context.json.
type wellKnown struct {
	Keys []struct {
		Kid string `json:"kid"`
		Alg string `json:"alg"`
		Key string `json:"key"`
	} `json:"keys"`
}

// RemoteKeys fetches the gateway's published keys and follows rotation.
//
//   - Refreshed every Refresh, and on an unknown kid at most once per
//     MissInterval, so a stream of garbage kids cannot turn every request
//     into a fetch.
//   - A key that disappears from the document stays trusted for Grace
//     (tenantctx's MaxTTL): tokens minted just before a rotation still verify.
//   - A key whose kid is not its derived id, or whose alg is not EdDSA, is
//     ignored: the kid is a function of the key, so a mismatch is a broken or
//     hostile document.
//   - A failed fetch keeps the keys already held.
type RemoteKeys struct {
	URL          string
	Client       *http.Client
	Refresh      time.Duration
	MissInterval time.Duration
	Grace        time.Duration
	Now          func() time.Time
	Log          *slog.Logger

	mu        sync.Mutex
	keys      map[string]ed25519.PublicKey
	retiredAt map[string]time.Time
	fetchedAt time.Time
	lastMiss  time.Time
	lastErr   error
}

// NewRemoteKeys builds a key source with the documented defaults.
func NewRemoteKeys(url string, client *http.Client, log *slog.Logger) *RemoteKeys {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	if log == nil {
		log = slog.Default()
	}
	return &RemoteKeys{URL: url, Client: client, Refresh: 5 * time.Minute, MissInterval: 30 * time.Second,
		Grace: MaxTTL, Log: log, keys: map[string]ed25519.PublicKey{}, retiredAt: map[string]time.Time{}}
}

func (r *RemoteKeys) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Key returns the key for kid, fetching when stale or unknown.
func (r *RemoteKeys) Key(kid string) (ed25519.PublicKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if r.fetchedAt.IsZero() || now.Sub(r.fetchedAt) >= r.Refresh {
		r.fetchLocked(now)
	}
	r.expireLocked(now)
	if k, ok := r.keys[kid]; ok {
		return k, nil
	}
	if now.Sub(r.lastMiss) >= r.MissInterval {
		r.lastMiss = now
		r.fetchLocked(now)
		r.expireLocked(now)
		if k, ok := r.keys[kid]; ok {
			return k, nil
		}
	}
	if len(r.keys) == 0 {
		return nil, ErrNoKeys
	}
	return nil, ErrUnknownKey
}

// Ready reports whether at least one key is held, for /readyz.
func (r *RemoteKeys) Ready(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fetchedAt.IsZero() || len(r.keys) == 0 {
		r.fetchLocked(r.now())
	}
	if len(r.keys) == 0 {
		if r.lastErr != nil {
			return fmt.Errorf("kgf context keys: %w", r.lastErr)
		}
		return ErrNoKeys
	}
	return nil
}

func (r *RemoteKeys) expireLocked(now time.Time) {
	for kid, at := range r.retiredAt {
		if now.Sub(at) >= r.Grace {
			delete(r.keys, kid)
			delete(r.retiredAt, kid)
		}
	}
}

func (r *RemoteKeys) fetchLocked(now time.Time) {
	fresh, err := r.fetch()
	if err != nil {
		r.lastErr = err
		// Retry sooner than a full refresh interval, but not on every request.
		r.fetchedAt = now.Add(-r.Refresh + r.MissInterval)
		r.Log.Warn("could not refresh KGF context keys; keeping the keys already held", "url", r.URL, "error", err, "held", len(r.keys))
		return
	}
	r.lastErr = nil
	r.fetchedAt = now
	for kid, k := range fresh {
		r.keys[kid] = k
		delete(r.retiredAt, kid)
	}
	for kid := range r.keys {
		if _, still := fresh[kid]; !still {
			if _, marked := r.retiredAt[kid]; !marked {
				r.retiredAt[kid] = now
				r.Log.Info("KGF context key rotated out; trusted until its tokens expire", "kid", kid, "grace", r.Grace)
			}
		}
	}
}

func (r *RemoteKeys) fetch() (map[string]ed25519.PublicKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", r.URL, resp.Status)
	}
	var doc wellKnown
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode %s: %w", r.URL, err)
	}
	out := map[string]ed25519.PublicKey{}
	for _, k := range doc.Keys {
		pub, err := DecodePublicKey(k.Key)
		if err != nil || k.Alg != "EdDSA" || k.Kid != KeyID(pub) {
			r.Log.Warn("ignoring a KGF context key that is not a well-formed EdDSA key under its derived kid", "kid", k.Kid)
			continue
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, errors.New("the key document holds no usable keys")
	}
	return out, nil
}

// MemoryReplayGuard refuses a token id already seen within its lifetime, per
// process (tenantctx.MemoryReplayGuard, including its per-replica limit).
type MemoryReplayGuard struct {
	mu   sync.Mutex
	seen map[string]time.Time
	Now  func() time.Time
}

// NewMemoryReplayGuard builds an empty guard.
func NewMemoryReplayGuard() *MemoryReplayGuard {
	return &MemoryReplayGuard{seen: map[string]time.Time{}}
}

// Observe records id and reports whether it is new.
func (g *MemoryReplayGuard) Observe(id string, expires time.Time) bool {
	if id == "" {
		return true
	}
	at := time.Now()
	if g.Now != nil {
		at = g.Now()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if until, ok := g.seen[id]; ok && until.After(at) {
		return false
	}
	if len(g.seen) > 4096 {
		for key, until := range g.seen {
			if !until.After(at) {
				delete(g.seen, key)
			}
		}
	}
	g.seen[id] = expires
	return true
}

// MultiKeys trusts the union of several key sources: static keys
// (NEXUS_KGF_CONTEXT_PUBLIC_KEYS) and one RemoteKeys per URL in
// NEXUS_KGF_CONTEXT_KEYS_URL -- kgf-gateway's document and each provider
// front's. A kid is looked up in each source in order; the first that holds
// it answers. A kid is a function of its key (KeyID), so two sources cannot
// disagree about which key a kid names.
type MultiKeys []KeySource

// Key returns the key for kid from the first source that holds it.
func (m MultiKeys) Key(kid string) (ed25519.PublicKey, error) {
	sawKeys := false
	for _, src := range m {
		k, err := src.Key(kid)
		if err == nil {
			return k, nil
		}
		if !errors.Is(err, ErrNoKeys) {
			sawKeys = true
		}
	}
	if !sawKeys {
		return nil, ErrNoKeys
	}
	return nil, ErrUnknownKey
}

// readySource is a key source that can report readiness (RemoteKeys).
type readySource interface {
	Ready(context.Context) error
}

// Ready reports ready when at least one source holds a key: kgf-gateway and
// the fronts start independently, and a gateway that can verify one issuer's
// contexts must not be taken out of rotation because another's document is
// not up yet. Each unready remote source is logged by RemoteKeys itself.
func (m MultiKeys) Ready(ctx context.Context) error {
	var firstErr error
	for _, src := range m {
		if rs, ok := src.(readySource); ok {
			err := rs.Ready(ctx)
			if err == nil {
				return nil
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if s, ok := src.(StaticKeys); ok && len(s) > 0 {
			return nil
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return ErrNoKeys
}

// ParseKeyURLs splits NEXUS_KGF_CONTEXT_KEYS_URL: a comma-separated list of
// key-document URLs, each serving {"keys":[{"kid","alg":"EdDSA","key"}]}.
// Blank items are ignored; duplicates are kept once.
func ParseKeyURLs(list string) []string {
	var out []string
	seen := map[string]bool{}
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}
