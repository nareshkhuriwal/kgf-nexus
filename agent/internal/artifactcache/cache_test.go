// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package artifactcache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memFetcher serves in-memory bodies and counts fetches per URI. When gate is
// non-nil every fetch blocks until it is closed, which lets tests pile up
// concurrent waiters on one download.
type memFetcher struct {
	mu     sync.Mutex
	bodies map[string][]byte
	calls  map[string]int
	gate   chan struct{}
	total  atomic.Int64
}

func newMemFetcher() *memFetcher {
	return &memFetcher{bodies: map[string][]byte{}, calls: map[string]int{}}
}

func (f *memFetcher) put(uri string, body []byte) { f.mu.Lock(); f.bodies[uri] = body; f.mu.Unlock() }

func (f *memFetcher) count(uri string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls[uri] }

func (f *memFetcher) Fetch(ctx context.Context, uri string) (io.ReadCloser, error) {
	f.total.Add(1)
	f.mu.Lock()
	f.calls[uri]++
	b, ok := f.bodies[uri]
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if !ok {
		return nil, fmt.Errorf("no such uri %s", uri)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

type countObserver struct {
	hits, misses, evictions, integrity, fetches atomic.Int64
	evictedBytes, size                          atomic.Int64
}

func (o *countObserver) Hit()                           { o.hits.Add(1) }
func (o *countObserver) Miss()                          { o.misses.Add(1) }
func (o *countObserver) Evicted(b int64)                { o.evictions.Add(1); o.evictedBytes.Add(b) }
func (o *countObserver) IntegrityFailure()              { o.integrity.Add(1) }
func (o *countObserver) FetchDone(time.Duration, error) { o.fetches.Add(1) }
func (o *countObserver) SizeBytes(n int64)              { o.size.Store(n) }

// fakeClock advances one second per reading so every touch has a distinct,
// ordered timestamp and LRU order after a reopen is deterministic.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

type fixture struct {
	dir   string
	f     *memFetcher
	obs   *countObserver
	clock *fakeClock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{
		dir:   t.TempDir(),
		f:     newMemFetcher(),
		obs:   &countObserver{},
		clock: &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
}

func (fx *fixture) open(t *testing.T, mut func(*Config)) *Cache {
	t.Helper()
	cfg := Config{
		Dir:      fx.dir,
		MaxBytes: 1 << 20,
		Fetcher:  fx.f,
		Metrics:  fx.obs,
		Now:      fx.clock.Now,
	}
	if mut != nil {
		mut(&cfg)
	}
	c, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return c
}

// art registers body under uri and returns a sha256-keyed artifact for it.
func (fx *fixture) art(name string, body []byte) Artifact {
	uri := "mem://" + name
	fx.f.put(uri, body)
	return Artifact{Name: name, Key: digestKey(body), URI: uri, SizeBytes: int64(len(body))}
}

func digestKey(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func mustAcquire(t *testing.T, c *Cache, a Artifact) *Lease {
	t.Helper()
	l, err := c.Acquire(context.Background(), a)
	if err != nil {
		t.Fatalf("Acquire(%s): %v", a.Name, err)
	}
	return l
}

func acquireRelease(t *testing.T, c *Cache, a Artifact) {
	t.Helper()
	mustAcquire(t, c, a).Release()
}

func tmpEntries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(filepath.Join(dir, tmpDirName))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}

func equalKeys(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("keys = %v, want %v", got, want)
		}
	}
}

func TestMissThenHit(t *testing.T) {
	fx := newFixture(t)
	c := fx.open(t, nil)
	body := []byte("model weights v1")
	a := fx.art("model", body)

	l := mustAcquire(t, c, a)
	if l.Hit {
		t.Fatal("first Acquire reported a hit")
	}
	if l.Key != a.Key {
		t.Fatalf("lease key = %s, want %s", l.Key, a.Key)
	}
	got, err := os.ReadFile(l.Path)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("cached content = %q, %v", got, err)
	}
	l.Release()
	l.Release() // idempotent

	l2 := mustAcquire(t, c, a)
	if !l2.Hit || l2.Path != l.Path {
		t.Fatalf("second Acquire: hit=%v path=%s", l2.Hit, l2.Path)
	}
	l2.Release()

	if h, m, f := fx.obs.hits.Load(), fx.obs.misses.Load(), fx.f.count(a.URI); h != 1 || m != 1 || f != 1 {
		t.Fatalf("hits=%d misses=%d fetches=%d, want 1/1/1", h, m, f)
	}
	if fx.obs.fetches.Load() != 1 {
		t.Fatalf("FetchDone calls = %d", fx.obs.fetches.Load())
	}
	if c.SizeBytes() != int64(len(body)) || fx.obs.size.Load() != int64(len(body)) {
		t.Fatalf("size = %d / observed %d", c.SizeBytes(), fx.obs.size.Load())
	}
	fi, err := os.Stat(fx.dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir mode = %v, %v", fi.Mode().Perm(), err)
	}
}

func TestIntegrityFailureOnWrongDigest(t *testing.T) {
	fx := newFixture(t)
	c := fx.open(t, nil)
	a := fx.art("model", []byte("real content"))
	wrong := digestKey([]byte("something else"))
	a.Key = wrong

	_, err := c.Acquire(context.Background(), a)
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v, want ErrIntegrity", err)
	}
	if !strings.Contains(err.Error(), wrong) || !strings.Contains(err.Error(), digestKey([]byte("real content"))) {
		t.Fatalf("error does not name expected and actual digests: %v", err)
	}
	if fx.obs.integrity.Load() != 1 {
		t.Fatalf("integrity failures = %d", fx.obs.integrity.Load())
	}
	if len(c.Keys(0)) != 0 || c.SizeBytes() != 0 {
		t.Fatalf("failed fetch left an entry: %v", c.Keys(0))
	}
	if left := tmpEntries(t, fx.dir); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
	// A later correct declaration is not poisoned by the failure.
	a.Key = digestKey([]byte("real content"))
	acquireRelease(t, c, a)
}

func TestDeclaredSizeMismatch(t *testing.T) {
	fx := newFixture(t)
	c := fx.open(t, nil)
	for _, declared := range []int64{5, 500} {
		a := fx.art("model", []byte("0123456789"))
		a.SizeBytes = declared
		if _, err := c.Acquire(context.Background(), a); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("declared %d: err = %v, want ErrIntegrity", declared, err)
		}
	}
	if left := tmpEntries(t, fx.dir); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

func TestURIKeyedEntry(t *testing.T) {
	fx := newFixture(t)
	c := fx.open(t, nil)
	fx.f.put("mem://ds", []byte("dataset"))
	a := Artifact{Name: "ds", URI: "mem://ds"}

	key := EntryKey(a)
	if !strings.HasPrefix(key, "uri:") || len(key) != len("uri:")+64 {
		t.Fatalf("EntryKey = %q", key)
	}
	l := mustAcquire(t, c, a)
	if l.Key != key || l.Hit {
		t.Fatalf("lease = %+v", l)
	}
	l.Release()
	if l2 := mustAcquire(t, c, a); !l2.Hit {
		t.Fatal("URI-keyed entry not hit on second Acquire")
	} else {
		l2.Release()
	}
	equalKeys(t, c.Keys(0), key)
}

func TestInvalidArtifact(t *testing.T) {
	fx := newFixture(t)
	c := fx.open(t, nil)
	for _, a := range []Artifact{
		{URI: ""},
		{URI: "mem://x", Key: "md5:abc"},
		{URI: "mem://x", Key: "sha256:" + strings.Repeat("A", 64)},
		{URI: "mem://x", Key: "sha256:abc"},
		{URI: "mem://x", SizeBytes: -1},
	} {
		if _, err := c.Acquire(context.Background(), a); !errors.Is(err, ErrInvalidArtifact) {
			t.Errorf("%+v: err = %v, want ErrInvalidArtifact", a, err)
		}
	}
}

func TestLRUEvictionOrder(t *testing.T) {
	fx := newFixture(t)
	c := fx.open(t, func(cfg *Config) { cfg.MaxBytes = 30 })
	a := fx.art("a", bytes.Repeat([]byte("a"), 10))
	b := fx.art("b", bytes.Repeat([]byte("b"), 10))
	cc := fx.art("c", bytes.Repeat([]byte("c"), 10))
	d := fx.art("d", bytes.Repeat([]byte("d"), 10))

	acquireRelease(t, c, a)
	acquireRelease(t, c, b)
	acquireRelease(t, c, cc)
	acquireRelease(t, c, a) // a is now MRU; b is LRU
	equalKeys(t, c.Keys(0), a.Key, cc.Key, b.Key)

	acquireRelease(t, c, d)
	equalKeys(t, c.Keys(0), d.Key, a.Key, cc.Key)
	if fx.obs.evictions.Load() != 1 || fx.obs.evictedBytes.Load() != 10 {
		t.Fatalf("evictions=%d bytes=%d", fx.obs.evictions.Load(), fx.obs.evictedBytes.Load())
	}
	if c.SizeBytes() != 30 {
		t.Fatalf("size = %d", c.SizeBytes())
	}
	if _, err := os.Stat(filepath.Join(fx.dir, entriesDirName, entryID(b.Key))); !os.IsNotExist(err) {
		t.Fatalf("evicted entry still on disk: %v", err)
	}
	if left := tmpEntries(t, fx.dir); len(left) != 0 {
		t.Fatalf("eviction left files in tmp: %v", left)
	}

	// A large insert evicts as many LRU entries as it needs, oldest first.
	e := fx.art("e", bytes.Repeat([]byte("e"), 25))
	acquireRelease(t, c, e)
	equalKeys(t, c.Keys(0), e.Key)
}

func TestMaxEntries(t *testing.T) {
	fx := newFixture(t)
	c := fx.open(t, func(cfg *Config) { cfg.MaxEntries = 2 })
	a := fx.art("a", []byte("a"))
	b := fx.art("b", []byte("b"))
	cc := fx.art("c", []byte("c"))
	acquireRelease(t, c, a)
	acquireRelease(t, c, b)
	acquireRelease(t, c, cc)
	equalKeys(t, c.Keys(0), cc.Key, b.Key)
}

func TestPinnedEntriesNotEvictedAndErrNoSpace(t *testing.T) {
	fx := newFixture(t)
	c := fx.open(t, func(cfg *Config) { cfg.MaxBytes = 20 })
	a := fx.art("a", bytes.Repeat([]byte("a"), 10))
	b := fx.art("b", bytes.Repeat([]byte("b"), 10))
	cc := fx.art("c", bytes.Repeat([]byte("c"), 10))

	la := mustAcquire(t, c, a)
	lb := mustAcquire(t, c, b)
	if _, err := c.Acquire(context.Background(), cc); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("err = %v, want ErrNoSpace", err)
	}
	// The same with an undeclared size: the bound is enforced at commit.
	undeclared := cc
	undeclared.SizeBytes = 0
	if _, err := c.Acquire(context.Background(), undeclared); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("undeclared: err = %v, want ErrNoSpace", err)
	}
	// Refusal must not have evicted anything or lost the pinned files.
	equalKeys(t, c.Keys(0), b.Key, a.Key)
	for _, l := range []*Lease{la, lb} {
		if _, err := os.Stat(l.Path); err != nil {
			t.Fatalf("pinned file gone: %v", err)
		}
	}
	if fx.obs.evictions.Load() != 0 {
		t.Fatalf("evictions = %d", fx.obs.evictions.Load())
	}

	la.Release()
	lc := mustAcquire(t, c, cc)
	equalKeys(t, c.Keys(0), cc.Key, b.Key)
	if _, err := os.Stat(lb.Path); err != nil {
		t.Fatalf("pinned b evicted: %v", err)
	}
	lc.Release()
	lb.Release()
	if left := tmpEntries(t, fx.dir); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

func TestTooLarge(t *testing.T) {
	fx := newFixture(t)
	c := fx.open(t, func(cfg *Config) { cfg.MaxBytes = 8 })
	a := fx.art("big", bytes.Repeat([]byte("x"), 9))
	if _, err := c.Acquire(context.Background(), a); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("declared: err = %v, want ErrTooLarge", err)
	}
	if fx.f.count(a.URI) != 0 {
		t.Fatal("declared-too-large artifact was fetched anyway")
	}
	a.SizeBytes = 0
	if _, err := c.Acquire(context.Background(), a); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("undeclared: err = %v, want ErrTooLarge", err)
	}
	if left := tmpEntries(t, fx.dir); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

func TestSingleFlight(t *testing.T) {
	fx := newFixture(t)
	fx.f.gate = make(chan struct{})
	c := fx.open(t, nil)
	a := fx.art("model", []byte("shared download"))

	const n = 16
	leases := make([]*Lease, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			leases[i], errs[i] = c.Acquire(context.Background(), a)
		}()
	}
	// Wait until every goroutine has registered as a miss, then let the one
	// download proceed.
	deadline := time.Now().Add(5 * time.Second)
	for fx.obs.misses.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d Acquires reached the miss path", fx.obs.misses.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
	close(fx.f.gate)
	wg.Wait()

	if got := fx.f.count(a.URI); got != 1 {
		t.Fatalf("fetcher called %d times, want 1", got)
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("Acquire %d: %v", i, errs[i])
		}
		if leases[i].Path != leases[0].Path {
			t.Fatalf("lease %d path %s != %s", i, leases[i].Path, leases[0].Path)
		}
	}
	// Every waiter holds its own pin: the entry must survive until the last
	// lease is released.
	c.mu.Lock()
	pins := c.entries[a.Key].pins
	c.mu.Unlock()
	if pins != n {
		t.Fatalf("pins = %d, want %d", pins, n)
	}
	for _, l := range leases {
		l.Release()
	}
	c.mu.Lock()
	pins = c.entries[a.Key].pins
	c.mu.Unlock()
	if pins != 0 {
		t.Fatalf("pins after release = %d", pins)
	}
}

func TestSingleFlightSharesFailure(t *testing.T) {
	fx := newFixture(t)
	fx.f.gate = make(chan struct{})
	c := fx.open(t, nil)
	a := fx.art("model", []byte("content"))
	a.Key = digestKey([]byte("other"))

	const n = 8
	var wg sync.WaitGroup
	var integrity atomic.Int64
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Acquire(context.Background(), a); errors.Is(err, ErrIntegrity) {
				integrity.Add(1)
			}
		}()
	}
	for fx.obs.misses.Load() < n {
		time.Sleep(time.Millisecond)
	}
	close(fx.f.gate)
	wg.Wait()
	if integrity.Load() != n || fx.f.count(a.URI) != 1 {
		t.Fatalf("integrity errors=%d fetches=%d", integrity.Load(), fx.f.count(a.URI))
	}
}

func TestCancelledWaiterDoesNotCancelSharedDownload(t *testing.T) {
	fx := newFixture(t)
	fx.f.gate = make(chan struct{})
	c := fx.open(t, nil)
	a := fx.art("model", []byte("content"))

	ctx, cancel := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() { _, err := c.Acquire(ctx, a); firstErr <- err }()
	for fx.obs.misses.Load() < 1 {
		time.Sleep(time.Millisecond)
	}
	second := make(chan *Lease, 1)
	go func() { l, _ := c.Acquire(context.Background(), a); second <- l }()
	for fx.obs.misses.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	cancel() // the party that started the download leaves
	if err := <-firstErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("first err = %v", err)
	}
	close(fx.f.gate)
	l := <-second
	if l == nil {
		t.Fatal("remaining waiter did not get the shared download")
	}
	l.Release()
	c.mu.Lock()
	pins := c.entries[a.Key].pins
	c.mu.Unlock()
	if pins != 0 {
		t.Fatalf("leaked pins = %d", pins)
	}
}

func TestAllWaitersCancelledStopsDownload(t *testing.T) {
	fx := newFixture(t)
	fx.f.gate = make(chan struct{}) // never opened
	c := fx.open(t, nil)
	a := fx.art("model", []byte("content"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.Acquire(ctx, a); done <- err }()
	for fx.obs.misses.Load() < 1 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	// The detached download observes the cancellation and clears in-flight
	// state, so a later Acquire starts afresh.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.inflight)
		c.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("abandoned download never finished")
		}
		time.Sleep(time.Millisecond)
	}
	fx.f.mu.Lock()
	fx.f.gate = nil
	fx.f.mu.Unlock()
	acquireRelease(t, c, a)
}

func TestReopenRebuildsIndex(t *testing.T) {
	fx := newFixture(t)
	c := fx.open(t, nil)
	a := fx.art("a", []byte("aaaa"))
	b := fx.art("b", []byte("bbbb"))
	cc := fx.art("c", []byte("cccc"))
	bad := fx.art("bad", []byte("dddd"))
	acquireRelease(t, c, a)
	acquireRelease(t, c, b)
	acquireRelease(t, c, cc)
	acquireRelease(t, c, bad)
	acquireRelease(t, c, a) // MRU order now: a, bad, c, b

	// Simulate a crash mid-download and a truncated/extended entry.
	if err := os.WriteFile(filepath.Join(fx.dir, tmpDirName, "dl-crashed"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(fx.dir, tmpDirName, "dl-dir", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	badData := filepath.Join(fx.dir, entriesDirName, entryID(bad.Key), dataFileName)
	if err := os.WriteFile(badData, []byte("dddd-extra"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory with no meta is junk from an unknown source.
	if err := os.MkdirAll(filepath.Join(fx.dir, entriesDirName, "junk"), 0o700); err != nil {
		t.Fatal(err)
	}

	c2 := fx.open(t, nil)
	equalKeys(t, c2.Keys(0), a.Key, cc.Key, b.Key)
	if c2.SizeBytes() != 12 {
		t.Fatalf("size = %d, want 12", c2.SizeBytes())
	}
	if left := tmpEntries(t, fx.dir); len(left) != 0 {
		t.Fatalf("tmp not cleaned: %v", left)
	}
	for _, p := range []string{filepath.Dir(badData), filepath.Join(fx.dir, entriesDirName, "junk")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived reopen: %v", p, err)
		}
	}
	before := fx.f.count(a.URI)
	l := mustAcquire(t, c2, a)
	if !l.Hit || fx.f.count(a.URI) != before {
		t.Fatal("reopened entry was not a hit")
	}
	l.Release()

	// A lowered bound evicts LRU entries on open.
	c3 := fx.open(t, func(cfg *Config) { cfg.MaxBytes = 8 })
	equalKeys(t, c3.Keys(0), a.Key, cc.Key)
}

func TestVerifyOnHitDetectsCorruption(t *testing.T) {
	fx := newFixture(t)
	c := fx.open(t, func(cfg *Config) { cfg.VerifyOnHit = true })
	body := []byte("pristine weights")
	a := fx.art("model", body)
	l := mustAcquire(t, c, a)
	l.Release()

	// Same size, different bytes: only a re-hash can notice.
	if err := os.WriteFile(l.Path, bytes.Repeat([]byte("X"), len(body)), 0o600); err != nil {
		t.Fatal(err)
	}
	l2 := mustAcquire(t, c, a)
	if l2.Hit {
		t.Fatal("corrupt entry served as a hit")
	}
	got, _ := os.ReadFile(l2.Path)
	if !bytes.Equal(got, body) {
		t.Fatalf("refetched content = %q", got)
	}
	l2.Release()
	if fx.obs.integrity.Load() != 1 || fx.f.count(a.URI) != 2 {
		t.Fatalf("integrity=%d fetches=%d", fx.obs.integrity.Load(), fx.f.count(a.URI))
	}
	if left := tmpEntries(t, fx.dir); len(left) != 0 {
		t.Fatalf("discarded entry left in tmp: %v", left)
	}
}

func TestSizeCheckOnHitWithoutVerify(t *testing.T) {
	fx := newFixture(t)
	c := fx.open(t, nil)
	a := fx.art("model", []byte("0123456789"))
	l := mustAcquire(t, c, a)
	l.Release()
	if err := os.WriteFile(l.Path, []byte("0123"), 0o600); err != nil {
		t.Fatal(err)
	}
	l2 := mustAcquire(t, c, a)
	if l2.Hit || fx.obs.integrity.Load() != 1 {
		t.Fatalf("truncated entry: hit=%v integrity=%d", l2.Hit, fx.obs.integrity.Load())
	}
	l2.Release()
}

func TestCorruptEntryKeptForOtherPinHolders(t *testing.T) {
	fx := newFixture(t)
	c := fx.open(t, func(cfg *Config) { cfg.VerifyOnHit = true })
	body := []byte("weights")
	a := fx.art("model", body)
	holder := mustAcquire(t, c, a)
	if err := os.WriteFile(holder.Path, []byte("WEIGHTS"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := mustAcquire(t, c, a) // discards and refetches
	if fresh.Hit {
		t.Fatal("corrupt entry was a hit")
	}
	if left := tmpEntries(t, fx.dir); len(left) != 1 {
		t.Fatalf("detached entry should wait for its holder, tmp = %v", left)
	}
	holder.Release()
	if left := tmpEntries(t, fx.dir); len(left) != 0 {
		t.Fatalf("detached entry not removed on last release: %v", left)
	}
	fresh.Release()
	equalKeys(t, c.Keys(0), a.Key)
}

func TestKeysOrderingAndBound(t *testing.T) {
	fx := newFixture(t)
	c := fx.open(t, nil)
	var arts []Artifact
	for i := range 5 {
		a := fx.art(fmt.Sprintf("a%d", i), []byte(fmt.Sprintf("content-%d", i)))
		arts = append(arts, a)
		acquireRelease(t, c, a)
	}
	acquireRelease(t, c, arts[1])
	equalKeys(t, c.Keys(3), arts[1].Key, arts[4].Key, arts[3].Key)
	if got := c.Keys(0); len(got) != 5 {
		t.Fatalf("Keys(0) = %d keys", len(got))
	}
	if got := c.Keys(100); len(got) != 5 {
		t.Fatalf("Keys(100) = %d keys", len(got))
	}
}

func TestOpenValidatesConfig(t *testing.T) {
	if _, err := Open(Config{Dir: t.TempDir()}); err == nil {
		t.Fatal("MaxBytes 0 accepted")
	}
	if _, err := Open(Config{MaxBytes: 1}); err == nil {
		t.Fatal("empty Dir accepted")
	}
}

func TestDefaultFetcherHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model.bin" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("served over http"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	c, err := Open(Config{Dir: dir, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("served over http")
	l, err := c.Acquire(context.Background(), Artifact{Name: "m", Key: digestKey(body), URI: srv.URL + "/model.bin"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(l.Path)
	if !bytes.Equal(got, body) {
		t.Fatalf("content = %q", got)
	}
	l.Release()

	_, err = c.Acquire(context.Background(), Artifact{URI: srv.URL + "/missing?sig=secret"})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404 err = %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaks query string: %v", err)
	}
}

func TestDefaultFetcherFileAndUnsupported(t *testing.T) {
	src := filepath.Join(t.TempDir(), "weights.bin")
	body := []byte("local file content")
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	f := DefaultFetcher()
	rc, err := f.Fetch(context.Background(), "file://"+filepath.ToSlash(src))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("content = %q", got)
	}
	if _, err := f.Fetch(context.Background(), "s3://bucket/key"); !errors.Is(err, ErrUnsupportedScheme) ||
		!strings.Contains(err.Error(), "unsupported artifact URI scheme") {
		t.Fatalf("s3 err = %v", err)
	}
	if _, err := f.Fetch(context.Background(), "file://remotehost/etc/passwd"); err == nil {
		t.Fatal("remote file host accepted")
	}
	if _, err := f.Fetch(context.Background(), "file://"+filepath.ToSlash(filepath.Dir(src))); err == nil {
		t.Fatal("directory accepted as file artifact")
	}
}

func TestRedactURI(t *testing.T) {
	for in, want := range map[string]string{
		"https://b.example/m.bin?X-Amz-Signature=abc": "https://b.example/m.bin?<redacted>",
		"https://user:pw@b.example/m.bin#frag":        "https://b.example/m.bin",
		"file:///data/m.bin":                          "file:///data/m.bin",
		"http://%zz":                                  "<unparsable uri>",
	} {
		if got := RedactURI(in); got != want {
			t.Errorf("RedactURI(%q) = %q, want %q", in, got, want)
		}
	}
}
