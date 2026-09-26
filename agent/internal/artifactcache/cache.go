// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package artifactcache is the worker agent's node-local, content-addressed
// LRU disk cache for model weights and datasets (ADR-0030 §4).
//
// The cache exists so that a task placed on a node that already holds its
// multi-gigabyte artifacts starts in seconds rather than after a download. That
// benefit is only worth having if the cache can never hand a task the wrong
// bytes and can never grow without bound, so the design is built around three
// invariants:
//
//   - INTEGRITY: bytes reach the addressable location (entries/<id>/data) only
//     by an atomic rename AFTER the whole body has been hashed and matched the
//     declared sha256 key. A crash mid-download leaves only a temp directory,
//     which Open deletes. A reader therefore never observes a partial file.
//   - BOUND: every insertion first evicts unpinned least-recently-used entries
//     until the new one fits. An entry leased by a running attempt is pinned
//     and is never evicted out from under it; if pinned entries alone exceed
//     the bound the fetch is refused with ErrNoSpace, which the caller reports
//     as a retryable infrastructure failure rather than silently overcommitting
//     the disk.
//   - NO LOCK ACROSS I/O: the mutex guards only the in-memory index and a few
//     renames. Downloads and hashing run unlocked, so one slow fetch never
//     blocks hits on other keys, and concurrent requests for the same key share
//     one download (single-flight).
package artifactcache

import (
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Errors callers classify on. They are sentinels wrapped with detail, so use
// errors.Is.
var (
	// ErrIntegrity means the fetched content did not match the declared digest
	// or declared size. The task declared something the source does not serve;
	// retrying elsewhere will not help (VALIDATION).
	ErrIntegrity = errors.New("artifactcache: integrity check failed")
	// ErrTooLarge means a single artifact exceeds the whole cache bound, so no
	// amount of eviction could ever admit it.
	ErrTooLarge = errors.New("artifactcache: artifact larger than cache bound")
	// ErrNoSpace means the artifact would fit in an empty cache but pinned
	// entries currently leave no room. It is transient: pins are released as
	// attempts finish (INFRASTRUCTURE, retryable).
	ErrNoSpace = errors.New("artifactcache: no space: pinned entries fill the cache")
	// ErrInvalidArtifact means the Artifact descriptor itself is malformed.
	ErrInvalidArtifact = errors.New("artifactcache: invalid artifact")
)

const (
	entriesDirName = "entries"
	tmpDirName     = "tmp"
	dataFileName   = "data"
	metaFileName   = "meta.json"
	sha256Prefix   = "sha256:"
	uriKeyPrefix   = "uri:"

	// maxHitAttempts bounds the hit-verify-discard loop in Acquire. A failed
	// verification removes the entry, so the second pass normally goes straight
	// to a fetch; the bound only guards against a disk that corrupts every
	// fresh entry between commit and read.
	maxHitAttempts = 3
)

// Config configures a Cache.
type Config struct {
	// Dir is the cache root. It is created 0o700 because cached artifacts may
	// be licensed weights or private datasets that other local users must not
	// read.
	Dir string
	// MaxBytes bounds the total size of cached content. Required, > 0.
	MaxBytes int64
	// MaxEntries bounds the number of entries; 0 means unbounded. Useful when
	// the filesystem has an inode budget or many tiny artifacts are expected.
	MaxEntries int
	// VerifyOnHit re-hashes content on every hit for sha256-keyed entries.
	// Without it a hit verifies only the size, which catches truncation but
	// not bit rot; re-hashing a large model on every attempt is expensive, so
	// it is opt-in (NEXUS_ARTIFACT_CACHE_VERIFY=hit).
	VerifyOnHit bool
	// Fetcher retrieves content by URI. nil selects DefaultFetcher().
	Fetcher Fetcher
	// Log receives operational events. nil discards them.
	Log *slog.Logger
	// Metrics observes cache behaviour. nil is a no-op.
	Metrics Observer
	// Now is the clock; nil means time.Now. Injected so tests can order LRU
	// deterministically.
	Now func() time.Time
}

// Artifact describes one cacheable input declared by a task.
type Artifact struct {
	// Name is the task-facing name (NEXUS_ARTIFACT_<NAME>); informational here.
	Name string
	// Key is "sha256:<64 lowercase hex>" of the content, or "" to key by URI.
	// A URI-keyed entry is cached but its content cannot be verified, because
	// there is nothing to verify it against.
	Key string
	// URI is where to fetch the content from.
	URI string
	// SizeBytes is the declared content size, or 0 if unknown. When set, a
	// body of any other length is rejected, and the size is reserved against
	// the bound before the download starts.
	SizeBytes int64
}

// Fetcher retrieves artifact content. Implementations must honour ctx
// cancellation for the whole body read, not only the initial request.
type Fetcher interface {
	Fetch(ctx context.Context, uri string) (io.ReadCloser, error)
}

// Observer receives cache metrics. Methods are called without the cache lock
// held and must be cheap and non-blocking.
type Observer interface {
	Hit()
	Miss()
	Evicted(bytes int64)
	IntegrityFailure()
	FetchDone(d time.Duration, err error)
	SizeBytes(n int64)
}

type nopObserver struct{}

func (nopObserver) Hit()                           {}
func (nopObserver) Miss()                          {}
func (nopObserver) Evicted(int64)                  {}
func (nopObserver) IntegrityFailure()              {}
func (nopObserver) FetchDone(time.Duration, error) {}
func (nopObserver) SizeBytes(int64)                {}

// entry is one committed cache entry. All fields except the immutable
// identity (key, uri, dir, size, created) are guarded by Cache.mu.
type entry struct {
	key     string
	uri     string
	dir     string // absolute path of entries/<id>
	size    int64
	created time.Time

	lastUsed time.Time
	pins     int
	elem     *list.Element
	// detached is set when the entry has been removed from the index while
	// still pinned (a hit found it corrupt). Its directory has already been
	// moved under tmp/ so a fresh download can take the canonical path; the
	// moved copy is deleted when the last pin is released.
	detached string
}

// call is one in-flight download shared by every concurrent Acquire of a key.
type call struct {
	done   chan struct{}
	cancel context.CancelFunc

	// Guarded by Cache.mu.
	refs     int // parties still waiting; each is pinned on success
	finished bool
	entry    *entry
	err      error
}

// meta is the on-disk record beside each data file. It lets Open rebuild the
// index after an agent restart without re-hashing every artifact.
type meta struct {
	Key string `json:"key"`
	// URI is informational and stored redacted (RedactURI): a presigned query
	// string is a credential and must not outlive the fetch on disk.
	URI       string    `json:"uri"`
	Name      string    `json:"name,omitempty"`
	SizeBytes int64     `json:"size_bytes"`
	Created   time.Time `json:"created"`
	// LastUsed is the value at commit. The meta file's mtime is what is kept
	// current on every hit (a cheap Chtimes instead of a rewrite) and is what
	// Open orders by.
	LastUsed time.Time `json:"last_used"`
}

// Cache is a bounded, content-addressed, single-flight artifact cache. It is
// safe for concurrent use.
type Cache struct {
	dir        string
	entriesDir string
	tmpDir     string
	maxBytes   int64
	maxEntries int
	verifyHit  bool
	fetcher    Fetcher
	log        *slog.Logger
	obs        Observer
	now        func() time.Time

	mu       sync.Mutex
	entries  map[string]*entry
	lru      *list.List // front = most recently used
	size     int64      // committed bytes
	reserved int64      // bytes reserved by in-flight downloads with a declared size
	inflight map[string]*call
}

// Lease pins a cached entry until Release. The file at Path is guaranteed not
// to be evicted while the lease is held.
type Lease struct {
	// Path is the absolute path of the cached content. Treat it as read-only:
	// it is shared by every attempt using the same artifact.
	Path string
	// Key is the entry key (EntryKey of the requested artifact).
	Key string
	// Hit reports whether the content was already cached when requested.
	Hit bool

	c    *Cache
	e    *entry
	once sync.Once
}

// Release unpins the entry. It is idempotent and safe on a nil Lease.
func (l *Lease) Release() {
	if l == nil || l.c == nil {
		return
	}
	l.once.Do(func() { l.c.unpin(l.e) })
}

// EntryKey returns the cache key for a: its declared content key when set,
// otherwise "uri:" plus the sha256 hex of the URI. Hashing the URI keeps the
// key a fixed, filesystem- and label-safe length whatever the URI contains.
func EntryKey(a Artifact) string {
	if a.Key != "" {
		return a.Key
	}
	return uriKeyPrefix + sha256Hex([]byte(a.URI))
}

// Open creates or reopens the cache at cfg.Dir. It rebuilds the index from
// complete entries on disk (ordered by last use), deletes leftover temp files
// from interrupted downloads, and drops entries whose metadata is unreadable
// or whose data size disagrees with the recorded size. If the bound has
// shrunk since the last run, the least recently used entries are evicted.
func Open(cfg Config) (*Cache, error) {
	if cfg.Dir == "" {
		return nil, errors.New("artifactcache: Dir is required")
	}
	if cfg.MaxBytes <= 0 {
		return nil, errors.New("artifactcache: MaxBytes must be > 0")
	}
	if cfg.MaxEntries < 0 {
		return nil, errors.New("artifactcache: MaxEntries must be >= 0")
	}
	dir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("artifactcache: resolve dir: %w", err)
	}
	c := &Cache{
		dir:        dir,
		entriesDir: filepath.Join(dir, entriesDirName),
		tmpDir:     filepath.Join(dir, tmpDirName),
		maxBytes:   cfg.MaxBytes,
		maxEntries: cfg.MaxEntries,
		verifyHit:  cfg.VerifyOnHit,
		fetcher:    cfg.Fetcher,
		log:        cfg.Log,
		obs:        cfg.Metrics,
		now:        cfg.Now,
		entries:    make(map[string]*entry),
		lru:        list.New(),
		inflight:   make(map[string]*call),
	}
	if c.fetcher == nil {
		c.fetcher = DefaultFetcher()
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.obs == nil {
		c.obs = nopObserver{}
	}
	if c.now == nil {
		c.now = time.Now
	}
	for _, d := range []string{c.dir, c.entriesDir, c.tmpDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("artifactcache: create %s: %w", d, err)
		}
	}
	// Tighten a pre-existing root: MkdirAll does not change the mode of a
	// directory that already exists.
	if err := os.Chmod(c.dir, 0o700); err != nil {
		return nil, fmt.Errorf("artifactcache: chmod %s: %w", c.dir, err)
	}
	if err := c.cleanTmp(); err != nil {
		return nil, err
	}
	if err := c.load(); err != nil {
		return nil, err
	}
	c.obs.SizeBytes(c.SizeBytes())
	return c, nil
}

// cleanTmp removes everything under tmp/. Nothing there is ever referenced
// after a restart: it holds only interrupted downloads and entries that were
// being deleted.
func (c *Cache) cleanTmp() error {
	des, err := os.ReadDir(c.tmpDir)
	if err != nil {
		return fmt.Errorf("artifactcache: read tmp: %w", err)
	}
	for _, de := range des {
		p := filepath.Join(c.tmpDir, de.Name())
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("artifactcache: remove leftover %s: %w", p, err)
		}
		c.log.Info("artifact cache removed leftover temp file", "path", p)
	}
	return nil
}

func (c *Cache) load() error {
	des, err := os.ReadDir(c.entriesDir)
	if err != nil {
		return fmt.Errorf("artifactcache: read entries: %w", err)
	}
	loaded := make([]*entry, 0, len(des))
	for _, de := range des {
		p := filepath.Join(c.entriesDir, de.Name())
		e, reason := c.loadEntry(de, p)
		if e == nil {
			c.log.Warn("artifact cache dropping invalid entry", "path", p, "reason", reason)
			if err := os.RemoveAll(p); err != nil {
				return fmt.Errorf("artifactcache: remove invalid entry %s: %w", p, err)
			}
			continue
		}
		loaded = append(loaded, e)
	}
	// Oldest first, each pushed to the front, leaves the MRU at the front.
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].lastUsed.Before(loaded[j].lastUsed) })
	for _, e := range loaded {
		e.elem = c.lru.PushFront(e)
		c.entries[e.key] = e
		c.size += e.size
	}

	// Enforce a bound that may have been lowered by configuration since the
	// last run. Nothing is pinned yet, so this always succeeds.
	var victims []*entry
	for (c.size > c.maxBytes || (c.maxEntries > 0 && len(c.entries) > c.maxEntries)) && c.lru.Len() > 0 {
		e := c.lru.Back().Value.(*entry)
		c.removeLocked(e)
		victims = append(victims, e)
	}
	for _, e := range victims {
		if err := os.RemoveAll(e.dir); err != nil {
			return fmt.Errorf("artifactcache: evict %s: %w", e.dir, err)
		}
		c.obs.Evicted(e.size)
	}
	c.log.Info("artifact cache opened", "dir", c.dir, "entries", len(c.entries),
		"bytes", c.size, "max_bytes", c.maxBytes, "evicted_on_open", len(victims))
	return nil
}

// loadEntry validates one entries/<id> directory. It returns nil and a reason
// when the directory is not a complete, self-consistent entry.
func (c *Cache) loadEntry(de os.DirEntry, p string) (*entry, string) {
	if !de.IsDir() {
		return nil, "not a directory"
	}
	mp := filepath.Join(p, metaFileName)
	raw, err := os.ReadFile(mp)
	if err != nil {
		return nil, "unreadable meta: " + err.Error()
	}
	var m meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, "corrupt meta: " + err.Error()
	}
	if validateKey(m.Key) != nil || m.Key == "" {
		return nil, "invalid key in meta"
	}
	// The directory name is derived from the key; a mismatch means the meta
	// belongs elsewhere and the entry cannot be trusted to be what it claims.
	if de.Name() != entryID(m.Key) {
		return nil, "directory name does not match key"
	}
	fi, err := os.Stat(filepath.Join(p, dataFileName))
	if err != nil {
		return nil, "missing data: " + err.Error()
	}
	if !fi.Mode().IsRegular() || fi.Size() != m.SizeBytes {
		return nil, fmt.Sprintf("data size %d does not match recorded %d", fi.Size(), m.SizeBytes)
	}
	lastUsed := m.LastUsed
	if mfi, err := os.Stat(mp); err == nil {
		lastUsed = mfi.ModTime()
	}
	return &entry{
		key:      m.Key,
		uri:      m.URI,
		dir:      p,
		size:     m.SizeBytes,
		created:  m.Created,
		lastUsed: lastUsed,
	}, ""
}

// Acquire ensures a is cached, fetching it on a miss, and pins it until the
// returned Lease is released. Concurrent Acquires of the same key share one
// download and all observe its result. The download continues while at least
// one caller is still waiting; it is cancelled once every waiter has given up.
func (c *Cache) Acquire(ctx context.Context, a Artifact) (*Lease, error) {
	if err := validateArtifact(a); err != nil {
		return nil, err
	}
	key := EntryKey(a)
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if e := c.entries[key]; e != nil {
			e.pins++
			e.lastUsed = c.now()
			c.lru.MoveToFront(e.elem)
			c.mu.Unlock()

			// Verification runs unlocked: the pin keeps the entry from being
			// evicted, and hashing a large file must not stall other keys.
			verr := c.verifyEntry(e)
			if verr == nil {
				c.touch(e)
				c.obs.Hit()
				return c.newLease(e, true), nil
			}
			c.obs.IntegrityFailure()
			c.log.Error("artifact cache entry failed verification; discarding",
				"key", key, "path", e.dir, "error", verr)
			c.discard(e)
			if attempt+1 >= maxHitAttempts {
				return nil, fmt.Errorf("%w: key %s: entry repeatedly failed verification: %v", ErrIntegrity, key, verr)
			}
			continue
		}
		cl := c.inflight[key]
		if cl == nil {
			fctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			cl = &call{done: make(chan struct{}), cancel: cancel, refs: 1}
			c.inflight[key] = cl
			go c.fetch(fctx, cl, a, key)
		} else {
			cl.refs++
		}
		c.mu.Unlock()
		c.obs.Miss()
		return c.wait(ctx, cl, key)
	}
}

func (c *Cache) wait(ctx context.Context, cl *call, key string) (*Lease, error) {
	select {
	case <-cl.done:
	case <-ctx.Done():
		c.mu.Lock()
		if !cl.finished {
			cl.refs--
			if cl.refs == 0 {
				// Nobody wants the result any more; stop spending bandwidth.
				cl.cancel()
			}
			c.mu.Unlock()
			return nil, ctx.Err()
		}
		c.mu.Unlock()
		// The download finished concurrently with cancellation and this party
		// was already pinned; hand over the result rather than waste it.
	}
	if cl.err != nil {
		return nil, cl.err
	}
	return c.newLease(cl.entry, false), nil
}

func (c *Cache) newLease(e *entry, hit bool) *Lease {
	return &Lease{Path: filepath.Join(e.dir, dataFileName), Key: e.key, Hit: hit, c: c, e: e}
}

// fetch runs one shared download and publishes its result to every waiter.
func (c *Cache) fetch(ctx context.Context, cl *call, a Artifact, key string) {
	defer cl.cancel()
	start := c.now()
	e, err := c.download(ctx, cl, a, key)
	c.obs.FetchDone(c.now().Sub(start), err)
	if err != nil {
		if errors.Is(err, ErrIntegrity) {
			c.obs.IntegrityFailure()
		}
		c.log.Warn("artifact fetch failed", "key", key, "uri", RedactURI(a.URI), "name", a.Name, "error", err)
	} else {
		c.log.Info("artifact cached", "key", key, "name", a.Name, "bytes", e.size,
			"duration", c.now().Sub(start))
	}
	// download() committed and pinned the entry under the same critical
	// section that marked the call finished, so no waiter can see a finished
	// call whose entry has already been evicted.
	close(cl.done)
}

// download streams the artifact into a temp directory, verifies it, and
// commits it. On success the entry is in the index with one pin per waiter.
func (c *Cache) download(ctx context.Context, cl *call, a Artifact, key string) (_ *entry, retErr error) {
	var reservation int64
	tmp := ""
	defer func() {
		if retErr == nil {
			return
		}
		c.mu.Lock()
		c.reserved -= reservation
		c.finishLocked(cl, key, nil, retErr)
		c.mu.Unlock()
		if tmp != "" {
			_ = os.RemoveAll(tmp)
		}
	}()

	if a.SizeBytes > c.maxBytes {
		return nil, fmt.Errorf("%w: key %s declares %d bytes, cache bound is %d", ErrTooLarge, key, a.SizeBytes, c.maxBytes)
	}
	if a.SizeBytes > 0 {
		// Reserve the declared size up front so that two concurrent large
		// downloads cannot each believe the same free space is theirs and
		// both fill the disk before either commits.
		c.mu.Lock()
		victims, err := c.makeRoomLocked(a.SizeBytes, false)
		if err == nil {
			c.reserved += a.SizeBytes
			reservation = a.SizeBytes
		}
		c.mu.Unlock()
		c.finishEvictions(victims)
		if err != nil {
			return nil, fmt.Errorf("%w (key %s, %d bytes)", err, key, a.SizeBytes)
		}
	}

	var err error
	tmp, err = os.MkdirTemp(c.tmpDir, "dl-")
	if err != nil {
		return nil, fmt.Errorf("artifactcache: create temp dir: %w", err)
	}
	n, digest, err := c.stream(ctx, a, key, filepath.Join(tmp, dataFileName))
	if err != nil {
		return nil, err
	}
	if a.SizeBytes > 0 && n != a.SizeBytes {
		return nil, fmt.Errorf("%w: key %s uri %s: expected %d bytes, got %d", ErrIntegrity, key, RedactURI(a.URI), a.SizeBytes, n)
	}
	if strings.HasPrefix(key, sha256Prefix) {
		actual := sha256Prefix + digest
		if actual != key {
			return nil, fmt.Errorf("%w: uri %s: expected %s, actual %s", ErrIntegrity, RedactURI(a.URI), key, actual)
		}
	}
	now := c.now()
	if err := writeMeta(filepath.Join(tmp, metaFileName), meta{
		Key: key, URI: RedactURI(a.URI), Name: a.Name, SizeBytes: n, Created: now, LastUsed: now,
	}, now); err != nil {
		return nil, err
	}
	if err := syncDir(tmp); err != nil {
		return nil, err
	}

	target := filepath.Join(c.entriesDir, entryID(key))
	e := &entry{key: key, uri: RedactURI(a.URI), dir: target, size: n, created: now, lastUsed: now}

	c.mu.Lock()
	c.reserved -= reservation
	reservation = 0
	victims, err := c.makeRoomLocked(n, true)
	if err == nil {
		err = c.commitLocked(tmp, e)
	}
	if err != nil {
		c.mu.Unlock()
		c.finishEvictions(victims)
		return nil, fmt.Errorf("%w (key %s, %d bytes)", err, key, n)
	}
	tmp = "" // now owned by the index
	c.finishLocked(cl, key, e, nil)
	size := c.size
	c.mu.Unlock()

	c.finishEvictions(victims)
	// Make the rename durable. Failure here is not fatal to this process: the
	// entry is valid, it just might not survive a power loss, in which case
	// Open simply won't find it.
	if err := syncDir(c.entriesDir); err != nil {
		c.log.Warn("artifact cache fsync of entries dir failed", "error", err)
	}
	c.obs.SizeBytes(size)
	return e, nil
}

// stream copies the fetched body into path, hashing as it goes, and enforces
// the declared size and the cache bound while copying so an oversized or
// endless body is cut off early instead of filling the disk.
func (c *Cache) stream(ctx context.Context, a Artifact, key, path string) (int64, string, error) {
	rc, err := c.fetcher.Fetch(ctx, a.URI)
	if err != nil {
		return 0, "", fmt.Errorf("artifactcache: fetch %s: %w", RedactURI(a.URI), err)
	}
	defer rc.Close()

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, "", fmt.Errorf("artifactcache: create temp file: %w", err)
	}
	defer f.Close()

	limit := c.maxBytes
	if a.SizeBytes > 0 {
		limit = a.SizeBytes
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(ctxReader{ctx, rc}, limit+1))
	if err != nil {
		return n, "", fmt.Errorf("artifactcache: download %s: %w", RedactURI(a.URI), err)
	}
	if n > limit {
		if a.SizeBytes > 0 {
			return n, "", fmt.Errorf("%w: key %s uri %s: body exceeds declared %d bytes", ErrIntegrity, key, RedactURI(a.URI), a.SizeBytes)
		}
		return n, "", fmt.Errorf("%w: key %s uri %s: body exceeds cache bound of %d bytes", ErrTooLarge, key, RedactURI(a.URI), c.maxBytes)
	}
	if err := f.Sync(); err != nil {
		return n, "", fmt.Errorf("artifactcache: fsync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return n, "", fmt.Errorf("artifactcache: close temp file: %w", err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// makeRoomLocked evicts unpinned LRU entries until need more bytes (and, when
// forInsert, one more entry) fit alongside committed and reserved bytes. It
// checks feasibility first so that a request that cannot be satisfied evicts
// nothing. Evicted directories are moved under tmp/ while locked (a cheap
// rename) so a concurrent download of the same key can immediately reuse the
// canonical path; the returned victims are deleted by finishEvictions after
// the lock is dropped. c.mu must be held.
func (c *Cache) makeRoomLocked(need int64, forInsert bool) ([]*entry, error) {
	if need > c.maxBytes {
		return nil, ErrTooLarge
	}
	var unpinnedBytes int64
	unpinnedCount := 0
	for el := c.lru.Front(); el != nil; el = el.Next() {
		if e := el.Value.(*entry); e.pins == 0 {
			unpinnedBytes += e.size
			unpinnedCount++
		}
	}
	if c.size+c.reserved-unpinnedBytes+need > c.maxBytes {
		return nil, ErrNoSpace
	}
	countOver := func() bool {
		return forInsert && c.maxEntries > 0 && len(c.entries)+1 > c.maxEntries
	}
	if forInsert && c.maxEntries > 0 && len(c.entries)-unpinnedCount+1 > c.maxEntries {
		return nil, ErrNoSpace
	}
	var victims []*entry
	for el := c.lru.Back(); el != nil && (c.size+c.reserved+need > c.maxBytes || countOver()); {
		prev := el.Prev()
		if e := el.Value.(*entry); e.pins == 0 {
			c.removeLocked(e)
			c.detachLocked(e)
			victims = append(victims, e)
		}
		el = prev
	}
	return victims, nil
}

// commitLocked renames the verified temp directory into its canonical place
// and inserts e into the index with one pin per waiting party. c.mu must be
// held.
func (c *Cache) commitLocked(tmp string, e *entry) error {
	// The index and the disk agree after Open, and every removal detaches the
	// directory, so the target should not exist. If something left it there
	// anyway, move it aside rather than fail the fetch forever.
	if _, err := os.Lstat(e.dir); err == nil {
		aside := c.asidePath("stale-")
		if err := os.Rename(e.dir, aside); err != nil {
			return fmt.Errorf("artifactcache: move stale entry aside: %w", err)
		}
		c.log.Warn("artifact cache moved unexpected stale entry aside", "path", e.dir)
		go func() { _ = os.RemoveAll(aside) }()
	}
	if err := os.Rename(tmp, e.dir); err != nil {
		return fmt.Errorf("artifactcache: commit entry: %w", err)
	}
	e.elem = c.lru.PushFront(e)
	c.entries[e.key] = e
	c.size += e.size
	return nil
}

// finishLocked publishes the call result, pins the entry once per waiting
// party, and removes the call from the in-flight table in the same critical
// section as the entry insert, so a new Acquire sees either the call or the
// entry, never neither. c.mu must be held.
func (c *Cache) finishLocked(cl *call, key string, e *entry, err error) {
	if cl.finished {
		return
	}
	cl.finished = true
	cl.entry = e
	cl.err = err
	if e != nil {
		e.pins += cl.refs
	}
	delete(c.inflight, key)
}

// removeLocked drops e from the index. c.mu must be held.
func (c *Cache) removeLocked(e *entry) {
	if e.elem == nil {
		return
	}
	c.lru.Remove(e.elem)
	e.elem = nil
	delete(c.entries, e.key)
	c.size -= e.size
}

// detachLocked moves e's directory out of the canonical path into tmp/. A
// rename is atomic and cheap, which is why it is acceptable under the lock.
// If the rename fails the directory is deleted in place, which is also safe
// because the entry is already out of the index. c.mu must be held.
func (c *Cache) detachLocked(e *entry) {
	aside := c.asidePath("evict-")
	if err := os.Rename(e.dir, aside); err != nil {
		c.log.Warn("artifact cache could not move entry aside; deleting in place", "path", e.dir, "error", err)
		_ = os.RemoveAll(e.dir)
		e.detached = ""
		return
	}
	e.detached = aside
}

func (c *Cache) finishEvictions(victims []*entry) {
	if len(victims) == 0 {
		return
	}
	for _, e := range victims {
		if e.detached != "" {
			if err := os.RemoveAll(e.detached); err != nil {
				c.log.Warn("artifact cache eviction cleanup failed", "path", e.detached, "error", err)
			}
		}
		c.log.Info("artifact cache evicted entry", "key", e.key, "bytes", e.size)
		c.obs.Evicted(e.size)
	}
	c.obs.SizeBytes(c.SizeBytes())
}

// discard removes a corrupt entry that the caller has pinned. Other leases
// may still point at it, so the directory is only moved aside now and deleted
// when the last pin goes (see unpin); new Acquires miss and refetch.
func (c *Cache) discard(e *entry) {
	c.mu.Lock()
	if c.entries[e.key] == e {
		c.removeLocked(e)
		c.detachLocked(e)
	}
	e.pins--
	rm := ""
	if e.pins == 0 && e.elem == nil {
		rm, e.detached = e.detached, ""
	}
	size := c.size
	c.mu.Unlock()
	if rm != "" {
		_ = os.RemoveAll(rm)
	}
	c.obs.SizeBytes(size)
}

func (c *Cache) unpin(e *entry) {
	c.mu.Lock()
	e.pins--
	rm := ""
	if e.pins == 0 && e.elem == nil && e.detached != "" {
		rm, e.detached = e.detached, ""
	}
	c.mu.Unlock()
	if rm != "" {
		_ = os.RemoveAll(rm)
	}
}

// verifyEntry checks a pinned entry before handing it out: always the size
// (catches truncation and external tampering cheaply), and the full digest
// when VerifyOnHit is set and the key is a content digest.
func (c *Cache) verifyEntry(e *entry) error {
	p := filepath.Join(e.dir, dataFileName)
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Size() != e.size {
		return fmt.Errorf("on-disk size %d, recorded %d", fi.Size(), e.size)
	}
	if !c.verifyHit || !strings.HasPrefix(e.key, sha256Prefix) {
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if actual := sha256Prefix + hex.EncodeToString(h.Sum(nil)); actual != e.key {
		return fmt.Errorf("expected %s, actual %s", e.key, actual)
	}
	return nil
}

// touch records the use on disk so that LRU order survives a restart. The
// mtime of the meta file is the persistent lastUsed; failure only degrades
// ordering after a restart, so it is logged and ignored.
func (c *Cache) touch(e *entry) {
	t := c.now()
	if err := os.Chtimes(filepath.Join(e.dir, metaFileName), t, t); err != nil {
		c.log.Debug("artifact cache touch failed", "key", e.key, "error", err)
	}
}

// Keys returns up to n cached keys, most recently used first. n <= 0 returns
// all keys. This is what the heartbeat advertises for cache-aware placement.
func (c *Cache) Keys(n int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n <= 0 || n > c.lru.Len() {
		n = c.lru.Len()
	}
	out := make([]string, 0, n)
	for el := c.lru.Front(); el != nil && len(out) < n; el = el.Next() {
		out = append(out, el.Value.(*entry).key)
	}
	return out
}

// SizeBytes returns the total size of committed entries.
func (c *Cache) SizeBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size
}

// Dir returns the absolute cache root.
func (c *Cache) Dir() string { return c.dir }

func (c *Cache) asidePath(prefix string) string {
	return filepath.Join(c.tmpDir, prefix+randomHex())
}

func validateArtifact(a Artifact) error {
	if a.URI == "" {
		return fmt.Errorf("%w: uri is required", ErrInvalidArtifact)
	}
	if a.SizeBytes < 0 {
		return fmt.Errorf("%w: size_bytes must be >= 0", ErrInvalidArtifact)
	}
	return validateKey(a.Key)
}

// validateKey accepts "" (URI-keyed), a sha256 content key, or a URI-derived
// key as stored in meta. Only lowercase hex is accepted so that one content
// has exactly one key and therefore one cache entry.
func validateKey(k string) error {
	var hexPart string
	switch {
	case k == "":
		return nil
	case strings.HasPrefix(k, sha256Prefix):
		hexPart = k[len(sha256Prefix):]
	case strings.HasPrefix(k, uriKeyPrefix):
		hexPart = k[len(uriKeyPrefix):]
	default:
		return fmt.Errorf("%w: key %q must be sha256:<64 lowercase hex>", ErrInvalidArtifact, k)
	}
	if len(hexPart) != 64 {
		return fmt.Errorf("%w: key %q must be sha256:<64 lowercase hex>", ErrInvalidArtifact, k)
	}
	for _, r := range hexPart {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return fmt.Errorf("%w: key %q must be sha256:<64 lowercase hex>", ErrInvalidArtifact, k)
		}
	}
	return nil
}

// entryID is the on-disk directory name for a key: the sha256 of the key, so
// it is fixed-length and free of path separators whatever the key contains.
func entryID(key string) string { return sha256Hex([]byte(key)) }

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func randomHex() string {
	var b [12]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b[:])
}

func writeMeta(path string, m meta, mtime time.Time) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("artifactcache: encode meta: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("artifactcache: create meta: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return fmt.Errorf("artifactcache: write meta: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("artifactcache: fsync meta: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("artifactcache: close meta: %w", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		return fmt.Errorf("artifactcache: set meta mtime: %w", err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("artifactcache: open dir for fsync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("artifactcache: fsync dir %s: %w", dir, err)
	}
	return nil
}

// ctxReader makes a body read observe cancellation between chunks, for
// Fetchers (like a plain file) whose reads do not watch ctx themselves.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
