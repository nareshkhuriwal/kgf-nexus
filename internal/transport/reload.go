// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package transport

// Certificate hot reload.
//
// THE FAILURE THIS FILE PREVENTS.
//
// The key pair and CA bundle used to be read once, at startup. A renewed
// certificate on disk did nothing to a running process, so every renewal had to
// be paired with a rolling restart -- and the day that pairing was forgotten (a
// reloader not installed, a cron job that renewed but did not restart), the
// process kept serving the old leaf until it expired and every handshake in the
// control plane failed at once. The renewal had happened; nothing used it.
//
// So material is re-read from disk while the process runs. Every handshake takes
// whatever snapshot is current, and a new snapshot replaces the old one only
// after it has been validated: a half-written, mismatched, expired or
// not-yet-valid pair is REJECTED and the old one keeps serving. Rotation must
// never be able to turn a working service into a broken one.
//
// WHY POLLING RATHER THAN fsnotify.
//
//   - Kubernetes secret volumes update by an atomic rename of a `..data` symlink
//     inside the mount. inotify on the file path sees nothing (the file never
//     changes; the symlink above it does), so fsnotify-based reloaders have to
//     watch the directory and special-case `..data` -- a known source of
//     "reload never fired" bugs. Reading through the path the operator
//     configured follows every symlink on every poll and cannot miss the swap.
//   - inotify is unreliable or absent on some volume types (NFS, some CSI
//     drivers, Docker Desktop bind mounts).
//   - It adds no dependency. go.mod is shared by every service, and a watcher
//     library for three small files is not worth the supply-chain surface.
//
// The cost is reading three files of a few KiB every interval, which is nothing,
// and a detection delay of up to one interval, which the renewal margin
// (days) makes irrelevant.

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// EnvReloadInterval sets how often certificate files are re-read.
//
// A Go duration ("10s", "1m"). "0" disables reloading entirely, which restores
// the old load-once behaviour -- kept as an escape hatch for an environment where
// a background goroutine reading files is unwelcome, not as a recommendation.
const EnvReloadInterval = "NEXUS_TLS_RELOAD_INTERVAL"

// defaultReloadInterval is short relative to any sane renewal margin (days) and
// long relative to the cost of reading three small files.
const defaultReloadInterval = 10 * time.Second

// maxDebounce caps the settle delay. See reloader.check for why there is one.
const maxDebounce = 2 * time.Second

// clockSkewTolerance is how far in the future a NotBefore may be and still be
// accepted.
//
// cert-manager and openssl both stamp NotBefore at "now" on the ISSUING machine.
// A node whose clock is a few seconds behind would otherwise reject a freshly
// issued certificate as not-yet-valid, and a peer verifying it would do the same
// -- so accepting a small skew here is no weaker than what the handshake will
// enforce anyway, and it avoids a spurious rejection on every renewal.
const clockSkewTolerance = 5 * time.Minute

// Roles, used as metric labels and in logs. A role names what the material is
// FOR, which is what an operator looking at an expiry alert needs to know.
const (
	roleServer = "server"
	roleClient = "client"
	roleCA     = "ca"
)

// fileSet is the paths one reloader watches. Any may be empty.
type fileSet struct {
	cert, key, ca string
}

func (f fileSet) String() string {
	var parts []string
	if f.cert != "" {
		parts = append(parts, "cert="+f.cert)
	}
	if f.key != "" {
		parts = append(parts, "key="+f.key)
	}
	if f.ca != "" {
		parts = append(parts, "ca="+f.ca)
	}
	return strings.Join(parts, " ")
}

// snapshot is one validated, immutable set of TLS material. Handshakes read a
// snapshot through an atomic pointer and never see a half-applied rotation.
type snapshot struct {
	fingerprint [sha256.Size]byte

	leaf *tls.Certificate // nil when no key pair is configured
	pool *x509.CertPool   // nil when no CA bundle is configured

	// caNotAfter is the LATEST expiry among the currently valid certificates in
	// the bundle -- the moment the bundle stops trusting anything. During a CA
	// overlap the bundle legitimately holds an older CA that expires first;
	// reporting that as the bundle's expiry would page someone for the rotation
	// working as designed.
	caNotAfter time.Time
	caCount    int

	// serverCfg is prebuilt for the server role so GetConfigForClient does no
	// work per handshake.
	serverCfg *tls.Config
}

func (s *snapshot) leafSerial() *big.Int {
	if s == nil || s.leaf == nil || s.leaf.Leaf == nil {
		return nil
	}
	return s.leaf.Leaf.SerialNumber
}

// reloader owns the current snapshot for one role and one set of files.
type reloader struct {
	role          string
	files         fileSet
	requireClient bool // server role: whether ClientCAs are enforced

	interval, debounce time.Duration
	now                func() time.Time

	cur atomic.Pointer[snapshot]

	// Rejection bookkeeping, touched only by the poll goroutine. A rejected
	// fingerprint is remembered so the SAME bad content is logged and counted
	// once, not every interval -- but it is still re-validated every interval,
	// because a not-yet-valid certificate becomes valid by the clock alone.
	rejectedFP  [sha256.Size]byte
	rejectedMsg string
	lastWarned  time.Time

	successes, failures atomic.Uint64
	stale               atomic.Bool // disk holds material we refused to apply

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// ── process-wide registry of reloaders ─────────────────────────────────────
//
// ServerOptions and DialOption are called from several places in one process
// (the gateway dials four upstreams). One reloader per distinct file set means
// one poller and one set of metrics per identity, however many call sites share
// it.

var live struct {
	mu sync.Mutex
	m  map[string]*reloader
}

func reloaderKey(role string, f fileSet, requireClient bool) string {
	return fmt.Sprintf("%s|%s|%s|%s|%t", role, f.cert, f.key, f.ca, requireClient)
}

// getReloader returns the shared reloader for this role and file set, creating
// and starting it -- with a synchronous initial load -- on first use.
//
// The initial load's error is returned rather than logged: at startup there is
// no previous snapshot to keep serving, so an unloadable file must stop the
// process exactly as it did before hot reload existed.
func getReloader(role string, f fileSet, requireClient bool) (*reloader, error) {
	interval, err := reloadInterval()
	if err != nil {
		return nil, err
	}

	key := reloaderKey(role, f, requireClient)
	live.mu.Lock()
	defer live.mu.Unlock()
	if r, ok := live.m[key]; ok {
		return r, nil
	}

	r := newReloader(role, f, requireClient, interval, time.Now)
	s, err := r.load()
	if err != nil {
		return nil, err
	}
	r.cur.Store(s)
	r.publish()
	slog.Default().Info("TLS material loaded",
		"component", "transport", "role", role, "files", f.String(),
		"serial", fmtSerial(s.leafSerial()), "reload_interval", interval.String())

	if interval > 0 {
		go r.run()
	} else {
		close(r.done)
	}
	if live.m == nil {
		live.m = make(map[string]*reloader)
	}
	live.m[key] = r
	return r, nil
}

// stopAllReloaders stops and forgets every reloader. Tests use it so one test's
// poller does not outlive its temp directory and log errors into the next.
func stopAllReloaders() {
	live.mu.Lock()
	rs := live.m
	live.m = nil
	live.mu.Unlock()
	for _, r := range rs {
		r.Stop()
	}
}

func reloadInterval() (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(EnvReloadInterval))
	if v == "" {
		return defaultReloadInterval, nil
	}
	if v == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		// An error, not a silent default: an operator who set this meant
		// something by it, and "reloading is off" versus "reloading every 10s"
		// is not a difference to guess about.
		return 0, fmt.Errorf("%s=%q is not a non-negative Go duration (e.g. 10s, 1m, or 0 to disable)", EnvReloadInterval, v)
	}
	return d, nil
}

func newReloader(role string, f fileSet, requireClient bool, interval time.Duration, now func() time.Time) *reloader {
	debounce := interval / 4
	if debounce > maxDebounce {
		debounce = maxDebounce
	}
	return &reloader{
		role: role, files: f, requireClient: requireClient,
		interval: interval, debounce: debounce, now: now,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// current returns the snapshot every handshake should use.
func (r *reloader) current() *snapshot { return r.cur.Load() }

// Stop ends the poll loop. Idempotent.
func (r *reloader) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

func (r *reloader) run() {
	defer close(r.done)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.check()
		}
	}
}

// check is one poll: detect a change, let it settle, validate it, swap or reject.
func (r *reloader) check() {
	fp := r.fingerprint()
	cur := r.current()
	if fp == cur.fingerprint {
		r.stale.Store(false)
		r.publishExpiry()
		r.warnIfNearExpiry(cur)
		return
	}

	// Debounce. Most writers are NOT atomic across files: `cp new.pem cert.pem;
	// cp new-key.pem key.pem`, or a renewal hook writing the chain then the key.
	// Polling between the two writes sees a new cert with the old key, which
	// validation would reject -- correctly, but as a spurious failure alert on
	// every renewal. Waiting for the content to stop changing absorbs that, and
	// a writer slower than the debounce is simply caught on the next poll.
	select {
	case <-r.stop:
		return
	case <-time.After(r.debounce):
	}
	if again := r.fingerprint(); again != fp {
		return // still being written; look again next interval
	}

	next, err := r.load()
	if err != nil {
		r.reject(fp, err)
		return
	}
	r.apply(cur, next)
}

func (r *reloader) apply(prev, next *snapshot) {
	r.cur.Store(next)
	r.stale.Store(false)
	r.rejectedMsg = ""
	r.rejectedFP = [sha256.Size]byte{}

	sinks.mu.Lock()
	r.successes.Add(1)
	for _, s := range sinks.list {
		s.reloads.With(reloadLabels(r.role, "success")).Inc()
	}
	sinks.mu.Unlock()
	r.publish()

	attrs := []any{
		"component", "transport", "role", r.role, "files", r.files.String(),
		"old_serial", fmtSerial(prev.leafSerial()), "new_serial", fmtSerial(next.leafSerial()),
	}
	if next.leaf != nil {
		attrs = append(attrs, "not_after", next.leaf.Leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	if next.pool != nil {
		attrs = append(attrs, "ca_certs", next.caCount)
	}
	slog.Default().Info("TLS material reloaded; new handshakes use it, established connections are unaffected until they reconnect", attrs...)
}

func (r *reloader) reject(fp [sha256.Size]byte, err error) {
	r.stale.Store(true)
	r.publishStale()
	msg := err.Error()
	if fp == r.rejectedFP && msg == r.rejectedMsg {
		return // already reported this exact content and reason
	}
	r.rejectedFP, r.rejectedMsg = fp, msg

	sinks.mu.Lock()
	r.failures.Add(1)
	for _, s := range sinks.list {
		s.reloads.With(reloadLabels(r.role, "failure")).Inc()
	}
	sinks.mu.Unlock()

	slog.Default().Error("TLS material on disk REJECTED; still serving the previous certificate",
		"component", "transport", "role", r.role, "files", r.files.String(),
		"serving_serial", fmtSerial(r.current().leafSerial()), "error", err)
}

// warnIfNearExpiry logs, at most hourly, when the SERVING leaf is in the last
// fifth of its lifetime.
//
// Relative to lifetime rather than a fixed number of days, so it means the same
// thing for a 24h dev cert and a 90-day one: cert-manager renews at two thirds
// of the lifetime by default, so a leaf still being served in its last fifth is
// one whose renewal did not land. This is a log line for the services that do
// not expose /metrics; the metrics are what alerting should use.
func (r *reloader) warnIfNearExpiry(s *snapshot) {
	if s.leaf == nil || s.leaf.Leaf == nil {
		return
	}
	now := r.now()
	leaf := s.leaf.Leaf
	lifetime := leaf.NotAfter.Sub(leaf.NotBefore)
	remaining := leaf.NotAfter.Sub(now)
	if remaining > lifetime/5 || now.Sub(r.lastWarned) < time.Hour {
		return
	}
	r.lastWarned = now
	slog.Default().Warn("serving TLS certificate is close to expiry and has not been renewed on disk",
		"component", "transport", "role", r.role, "files", r.files.String(),
		"serial", fmtSerial(leaf.SerialNumber), "not_after", leaf.NotAfter.UTC().Format(time.RFC3339),
		"remaining", remaining.Round(time.Second).String())
}

// ── reading and validating ─────────────────────────────────────────────────

type rawMaterial struct {
	cert, key, ca []byte
}

func (r *reloader) read() (rawMaterial, error) {
	var m rawMaterial
	var err error
	if r.files.cert != "" {
		if m.cert, err = os.ReadFile(r.files.cert); err != nil {
			return m, fmt.Errorf("read certificate %s: %w", r.files.cert, err)
		}
	}
	if r.files.key != "" {
		if m.key, err = os.ReadFile(r.files.key); err != nil {
			return m, fmt.Errorf("read private key %s: %w", r.files.key, err)
		}
	}
	if r.files.ca != "" {
		if m.ca, err = os.ReadFile(r.files.ca); err != nil {
			return m, fmt.Errorf("read CA bundle %s (%s): %w", r.files.ca, EnvCACert, err)
		}
	}
	return m, nil
}

// fingerprint identifies what is on disk now. A read error has a fingerprint too
// (of its message), so a file that is briefly missing mid-swap is debounced like
// any other change instead of being reported the instant it is seen.
func (r *reloader) fingerprint() [sha256.Size]byte {
	m, err := r.read()
	if err != nil {
		return sha256.Sum256([]byte("error:" + err.Error()))
	}
	return m.fingerprint()
}

func (m rawMaterial) fingerprint() [sha256.Size]byte {
	h := sha256.New()
	// Length-prefixed so moving bytes from one file to the next cannot produce
	// the same digest.
	for _, b := range [][]byte{m.cert, m.key, m.ca} {
		fmt.Fprintf(h, "%d:", len(b))
		h.Write(b)
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// load reads and validates the files into a snapshot, or explains why not.
func (r *reloader) load() (*snapshot, error) {
	m, err := r.read()
	if err != nil {
		return nil, err
	}
	s, err := buildSnapshot(m, r.files, r.now())
	if err != nil {
		return nil, err
	}
	if r.role == roleServer {
		s.serverCfg = serverConfig(s, r.requireClient)
	}
	return s, nil
}

// buildSnapshot is the validation gate. Everything that could make a rotation
// break a working service is checked here, BEFORE the swap.
func buildSnapshot(m rawMaterial, f fileSet, now time.Time) (*snapshot, error) {
	s := &snapshot{fingerprint: m.fingerprint()}

	if f.cert != "" || f.key != "" {
		// X509KeyPair verifies that the private key matches the certificate's
		// public key. That is the check that catches the non-atomic writer: a new
		// certificate beside the old key.
		pair, err := tls.X509KeyPair(m.cert, m.key)
		if err != nil {
			return nil, fmt.Errorf("load key pair from %s and %s: %w", f.cert, f.key, err)
		}
		leaf := pair.Leaf
		if leaf == nil {
			if leaf, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
				return nil, fmt.Errorf("parse leaf certificate %s: %w", f.cert, err)
			}
			pair.Leaf = leaf
		}
		if err := checkValidity(leaf, now); err != nil {
			return nil, fmt.Errorf("certificate %s: %w", f.cert, err)
		}
		s.leaf = &pair
	}

	if f.ca != "" {
		pool, count, notAfter, err := parseCABundle(m.ca, f.ca, now)
		if err != nil {
			return nil, err
		}
		s.pool, s.caCount, s.caNotAfter = pool, count, notAfter
	}
	return s, nil
}

func checkValidity(c *x509.Certificate, now time.Time) error {
	if now.After(c.NotAfter) {
		return fmt.Errorf("serial %s expired at %s", fmtSerial(c.SerialNumber), c.NotAfter.UTC().Format(time.RFC3339))
	}
	if now.Add(clockSkewTolerance).Before(c.NotBefore) {
		return fmt.Errorf("serial %s is not valid until %s", fmtSerial(c.SerialNumber), c.NotBefore.UTC().Format(time.RFC3339))
	}
	return nil
}

// parseCABundle builds a pool from every certificate in the bundle.
//
// A bundle may hold several CAs at once, and that is how CA rotation works
// without an outage: publish old+new, re-issue every leaf from the new CA, then
// remove the old one. Expired CAs in the bundle are tolerated (they are inert --
// the verifier rejects chains through them) as long as at least one is still
// valid, because a bundle that trusts nothing would fail every handshake.
func parseCABundle(data []byte, path string, now time.Time) (*x509.CertPool, int, time.Time, error) {
	pool := x509.NewCertPool()
	var count, valid int
	var latest time.Time
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, 0, time.Time{}, fmt.Errorf("CA bundle %s (%s): certificate %d does not parse: %w", path, EnvCACert, count+1, err)
		}
		pool.AddCert(c)
		count++
		if checkValidity(c, now) == nil {
			valid++
			if c.NotAfter.After(latest) {
				latest = c.NotAfter
			}
		}
	}
	if count == 0 {
		// pem.Decode reports only "nothing found", so the message has to supply
		// the likely cause: this is what pointing the variable at a DER file, a
		// key, or a truncated download looks like.
		return nil, 0, time.Time{}, fmt.Errorf("CA bundle %s (%s) contains no PEM certificates; a DER-encoded or truncated file looks exactly like this", path, EnvCACert)
	}
	if valid == 0 {
		return nil, 0, time.Time{}, fmt.Errorf("CA bundle %s (%s): all %d certificates are expired or not yet valid", path, EnvCACert, count)
	}
	return pool, count, latest, nil
}

// serverConfig is the tls.Config a server hands out for one snapshot.
func serverConfig(s *snapshot, requireClient bool) *tls.Config {
	leaf := s.leaf
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// GetCertificate rather than Certificates, so the SNI-less and SNI paths
		// both return exactly this snapshot's leaf and nothing else.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return leaf, nil },
		// credentials.NewTLS adds "h2" to the BASE config, but the config
		// returned from GetConfigForClient replaces the base for the handshake.
		// Without this, ALPN negotiates nothing and grpc-go refuses the
		// connection ("missing selected ALPN property").
		NextProtos: []string{"h2"},
		// A resumed TLS session skips certificate presentation and keeps the
		// identity -- and the CA verification -- of the ORIGINAL handshake. After
		// a rotation that is exactly what must not happen: a peer whose
		// certificate we just stopped trusting would resume past the new bundle.
		// gRPC connections are long-lived, so a full handshake per connection
		// costs nothing measurable.
		SessionTicketsDisabled: true,
	}
	if requireClient {
		cfg.ClientCAs = s.pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg
}

func fmtSerial(n *big.Int) string {
	if n == nil {
		return "none"
	}
	return fmt.Sprintf("%x", n)
}

// errNotClient is returned if the client credentials are ever used to accept a
// connection, which would be a wiring mistake.
var errNotClient = errors.New("transport: client credentials cannot perform a server handshake")
