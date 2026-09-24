// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
)

// The reload tests drive reloader.check() by hand rather than waiting on the
// background poller. That makes each step deterministic -- "after this write,
// one poll, then assert" -- instead of a sleep-and-hope that passes on a fast
// laptop and flakes in CI (F86's lesson: a timing-dependent check can be red
// while nothing is wrong, which trains everyone to re-run).

// fakeClock lets a test move time without sleeping, so "not yet valid" can
// become "valid" inside one test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// manualReloader builds a reloader with no poll goroutine and loads it once.
func manualReloader(t *testing.T, role string, f fileSet, requireClient bool, clk *fakeClock) *reloader {
	t.Helper()
	now := time.Now
	if clk != nil {
		now = clk.now
	}
	r := newReloader(role, f, requireClient, 0, now)
	close(r.done) // never started; lets Stop() return
	s, err := r.load()
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	r.cur.Store(s)
	return r
}

// install replaces dst with src's content by write-then-rename, the way any
// careful writer (and kubelet, one level up) does it, so a poll never reads a
// torn file. Non-atomic writers are covered separately by the debounce test.
func install(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		t.Fatalf("rename %s: %v", dst, err)
	}
}

func serialOfFile(t *testing.T, path string) *big.Int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatalf("%s: no PEM", path)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return c.SerialNumber
}

func assertServing(t *testing.T, r *reloader, want *big.Int, why string) {
	t.Helper()
	got := r.current().leafSerial()
	if got == nil || got.Cmp(want) != 0 {
		t.Fatalf("%s: serving serial %s, want %s", why, fmtSerial(got), fmtSerial(want))
	}
}

func TestReloadPicksUpReplacedFiles(t *testing.T) {
	clearEnv(t)
	ca := newTestCA(t, "nexus")
	cert, key := ca.issue(t, "svc", []string{"svc"}, false)
	r := manualReloader(t, roleServer, fileSet{cert: cert, key: key}, false, nil)
	oldSerial := serialOfFile(t, cert)

	newCert, newKey := ca.issue(t, "svc-renewed", []string{"svc"}, false)
	install(t, newKey, key)
	install(t, newCert, cert)
	newSerial := serialOfFile(t, cert)
	if newSerial.Cmp(oldSerial) == 0 {
		t.Fatal("test setup: renewed certificate has the same serial; the assertion below would prove nothing")
	}

	r.check()
	assertServing(t, r, newSerial, "after replacing the files")
	if r.successes.Load() != 1 || r.failures.Load() != 0 {
		t.Fatalf("successes=%d failures=%d, want 1/0", r.successes.Load(), r.failures.Load())
	}
	// And the server config handed to handshakes must be the new one, not just
	// the bookkeeping: that is what a client actually receives.
	c, err := r.current().serverCfg.GetCertificate(nil)
	if err != nil || c.Leaf.SerialNumber.Cmp(newSerial) != 0 {
		t.Fatalf("serverCfg still hands out the old certificate (err=%v)", err)
	}

	// An unchanged poll is a no-op, not a second "reload".
	r.check()
	if r.successes.Load() != 1 {
		t.Fatalf("an unchanged poll counted as a reload: successes=%d", r.successes.Load())
	}
}

// TestReloadFollowsKubernetesSymlinkSwap reproduces the kubelet's atomic writer:
// the configured paths are symlinks through `..data`, and an update is a rename
// of `..data` to point at a new timestamped directory. Nothing at the configured
// path is ever written, which is exactly what defeats a file-level watch.
func TestReloadFollowsKubernetesSymlinkSwap(t *testing.T) {
	clearEnv(t)
	ca := newTestCA(t, "nexus")
	mount := t.TempDir()

	populate := func(dirName, leafName string) {
		d := filepath.Join(mount, dirName)
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		c, k := ca.issue(t, leafName, []string{"svc"}, false)
		install(t, c, filepath.Join(d, "tls.crt"))
		install(t, k, filepath.Join(d, "tls.key"))
		install(t, ca.caFile, filepath.Join(d, "ca.crt"))
	}
	populate("..2026_09_01_00_00_00.1", "gen1")
	if err := os.Symlink("..2026_09_01_00_00_00.1", filepath.Join(mount, "..data")); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"tls.crt", "tls.key", "ca.crt"} {
		if err := os.Symlink(filepath.Join("..data", f), filepath.Join(mount, f)); err != nil {
			t.Fatal(err)
		}
	}
	files := fileSet{cert: filepath.Join(mount, "tls.crt"), key: filepath.Join(mount, "tls.key"), ca: filepath.Join(mount, "ca.crt")}
	r := manualReloader(t, roleServer, files, true, nil)
	gen1 := serialOfFile(t, files.cert)

	// The swap, exactly as kubelet's AtomicWriter does it.
	populate("..2026_09_02_00_00_00.2", "gen2")
	tmpLink := filepath.Join(mount, "..data_tmp")
	if err := os.Symlink("..2026_09_02_00_00_00.2", tmpLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpLink, filepath.Join(mount, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(mount, "..2026_09_01_00_00_00.1")); err != nil {
		t.Fatal(err)
	}
	gen2 := serialOfFile(t, files.cert)
	if gen1.Cmp(gen2) == 0 {
		t.Fatal("test setup: symlink swap did not change the certificate at the configured path")
	}

	r.check()
	assertServing(t, r, gen2, "after the ..data symlink swap")
}

// TestReloadRejectsMismatchedPair is the non-atomic writer: the new certificate
// lands beside the OLD key. Swapping to it would make every handshake fail, so
// the old pair must keep serving and the rejection must be visible.
func TestReloadRejectsMismatchedPair(t *testing.T) {
	clearEnv(t)
	ca := newTestCA(t, "nexus")
	cert, key := ca.issue(t, "svc", []string{"svc"}, false)
	r := manualReloader(t, roleServer, fileSet{cert: cert, key: key}, false, nil)
	oldSerial := serialOfFile(t, cert)

	reg := metrics.NewRegistry()
	RegisterMetrics(reg)

	newCert, newKey := ca.issue(t, "svc-renewed", []string{"svc"}, false)
	install(t, newCert, cert) // key NOT yet replaced

	r.check()
	assertServing(t, r, oldSerial, "after a certificate arrived without its key")
	if r.failures.Load() != 1 {
		t.Fatalf("failures=%d, want 1", r.failures.Load())
	}
	if !r.stale.Load() {
		t.Fatal("stale is false although disk holds rejected material")
	}
	scrape := scrapeText(reg)
	if !strings.Contains(scrape, MetricStale+`{role="server"} 1`) {
		t.Fatalf("stale gauge not raised in scrape:\n%s", scrape)
	}
	if !strings.Contains(scrape, MetricReloads+`{result="failure",role="server"} 1`) {
		t.Fatalf("failure counter not in scrape:\n%s", scrape)
	}

	// The same bad content polled again is not a NEW failure: counting it every
	// interval would make the counter a clock rather than an event count.
	r.check()
	if r.failures.Load() != 1 {
		t.Fatalf("the same rejected content was counted again: failures=%d", r.failures.Load())
	}

	// The key arrives; the pair is now coherent and must be applied.
	install(t, newKey, key)
	r.check()
	assertServing(t, r, serialOfFile(t, newCert), "after the key arrived")
	if r.stale.Load() {
		t.Fatal("stale still set after a successful reload")
	}
	if s := scrapeText(reg); !strings.Contains(s, MetricStale+`{role="server"} 0`) {
		t.Fatalf("stale gauge not cleared:\n%s", s)
	}
}

func TestReloadRejectsExpiredCertificate(t *testing.T) {
	clearEnv(t)
	ca := newTestCA(t, "nexus")
	cert, key := ca.issue(t, "svc", []string{"svc"}, false)
	r := manualReloader(t, roleServer, fileSet{cert: cert, key: key}, false, nil)
	oldSerial := serialOfFile(t, cert)

	// A renewal job that copied a stale backup, or a clock-broken issuer.
	expCert, expKey := ca.issueValid(t, "svc-expired", []string{"svc"}, false,
		time.Now().Add(-2*time.Hour), time.Now().Add(-time.Minute))
	install(t, expKey, key)
	install(t, expCert, cert)

	r.check()
	assertServing(t, r, oldSerial, "after an expired certificate was written")
	if r.failures.Load() != 1 {
		t.Fatalf("failures=%d, want 1", r.failures.Load())
	}
}

// TestReloadNotYetValidIsAppliedOnceValid covers the one rejection that fixes
// itself: a certificate stamped slightly in the future (issuer clock ahead by
// more than the tolerance). The content never changes, so a reloader that only
// retried on content change would reject it forever.
func TestReloadNotYetValidIsAppliedOnceValid(t *testing.T) {
	clearEnv(t)
	clk := &fakeClock{t: time.Now()}
	ca := newTestCA(t, "nexus")
	cert, key := ca.issue(t, "svc", []string{"svc"}, false)
	r := manualReloader(t, roleServer, fileSet{cert: cert, key: key}, false, clk)
	oldSerial := serialOfFile(t, cert)

	futCert, futKey := ca.issueValid(t, "svc-future", []string{"svc"}, false,
		clk.now().Add(10*time.Minute), clk.now().Add(50*time.Minute))
	install(t, futKey, key)
	install(t, futCert, cert)

	r.check()
	assertServing(t, r, oldSerial, "while the new certificate is not yet valid")

	clk.advance(6 * time.Minute) // inside clockSkewTolerance of NotBefore
	r.check()
	assertServing(t, r, serialOfFile(t, futCert), "once the new certificate became valid")
}

func TestStartupRefusesExpiredCertificate(t *testing.T) {
	clearEnv(t)
	ca := newTestCA(t, "nexus")
	cert, key := ca.issueValid(t, "svc", []string{"svc"}, false,
		time.Now().Add(-2*time.Hour), time.Now().Add(-time.Minute))
	t.Setenv(EnvServerCert, cert)
	t.Setenv(EnvServerKey, key)
	_, err := ServerOptions()
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("ServerOptions accepted an expired certificate at startup (err=%v); every handshake would fail with nothing at startup to say why", err)
	}
}

// TestReloadDebouncesNonAtomicWriter: the certificate and key are written a
// moment apart. A poll that lands between them must wait for the writer to
// finish rather than reject the half-written state.
func TestReloadDebouncesNonAtomicWriter(t *testing.T) {
	clearEnv(t)
	ca := newTestCA(t, "nexus")
	cert, key := ca.issue(t, "svc", []string{"svc"}, false)
	r := manualReloader(t, roleServer, fileSet{cert: cert, key: key}, false, nil)
	r.debounce = 300 * time.Millisecond

	newCert, newKey := ca.issue(t, "svc-renewed", []string{"svc"}, false)
	keyPEM, err := os.ReadFile(newKey)
	if err != nil {
		t.Fatal(err)
	}
	install(t, newCert, cert)
	// The writer goroutine must not call t.Fatal (illegal off the test
	// goroutine) and must finish before the test does: a write landing after
	// the test returned panics the whole binary and hides every later test's
	// result, which is how this test first misled a mutation run.
	writeErr := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond) // well inside the debounce
		if err := os.WriteFile(key+".tmp", keyPEM, 0o600); err != nil {
			writeErr <- err
			return
		}
		writeErr <- os.Rename(key+".tmp", key)
	}()

	r.check() // sees the change, waits, sees it still changing, backs off
	if err := <-writeErr; err != nil {
		t.Fatalf("writer: %v", err)
	}
	if r.failures.Load() != 0 {
		t.Fatalf("a writer mid-write was reported as a failure: %s", r.rejectedMsg)
	}
	r.check() // content now stable
	assertServing(t, r, serialOfFile(t, newCert), "after the writer finished")
}

// TestReloadSurvivesMissingFile: a file briefly absent (deleted, mid-copy) is a
// rejection, not a crash and not a downgrade -- the old material keeps serving.
func TestReloadSurvivesMissingFile(t *testing.T) {
	clearEnv(t)
	ca := newTestCA(t, "nexus")
	cert, key := ca.issue(t, "svc", []string{"svc"}, false)
	r := manualReloader(t, roleServer, fileSet{cert: cert, key: key}, false, nil)
	oldSerial := serialOfFile(t, cert)

	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	r.check()
	assertServing(t, r, oldSerial, "while the key file is missing")
	if !r.stale.Load() {
		t.Fatal("a missing file did not mark the material stale")
	}
}

func TestCABundleRejectsAllExpired(t *testing.T) {
	now := time.Now()
	ca := newTestCA(t, "old")
	b, err := os.ReadFile(ca.caFile)
	if err != nil {
		t.Fatal(err)
	}
	// newTestCA's CA is valid for +-1h, so two hours on it has expired.
	if _, _, _, err := parseCABundle(b, ca.caFile, now.Add(2*time.Hour)); err == nil {
		t.Fatal("a bundle whose every CA has expired was accepted; it would fail every handshake")
	}
	if _, _, _, err := parseCABundle(b, ca.caFile, now); err != nil {
		t.Fatalf("a valid bundle was rejected: %v", err)
	}
}

// TestMetricsReportServedExpiry pins what the expiry alert reads: the NotAfter
// of what is SERVED, per role, and it must move when a reload lands.
func TestMetricsReportServedExpiry(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvReloadInterval, "0")
	ca := newTestCA(t, "nexus")
	cert, key := ca.issue(t, "svc", []string{"svc"}, false)
	ccert, ckey := ca.issue(t, "svc-client", nil, true)
	t.Setenv(EnvServerCert, cert)
	t.Setenv(EnvServerKey, key)
	t.Setenv(EnvCACert, ca.caFile)
	t.Setenv(EnvServerRequireClientCert, "1")
	t.Setenv(EnvClientCert, ccert)
	t.Setenv(EnvClientKey, ckey)
	if _, err := ServerOptions(); err != nil {
		t.Fatal(err)
	}
	if _, err := DialOption(); err != nil {
		t.Fatal(err)
	}

	// Registered AFTER the credentials were resolved, as a service main may do.
	reg := metrics.NewRegistry()
	RegisterMetrics(reg)
	s := scrapeText(reg)

	for role, path := range map[string]string{"server": cert, "client": ccert, "ca": ca.caFile} {
		na := notAfterOfFile(t, path)
		want := MetricNotAfter + `{role="` + role + `"} ` + formatUnix(na)
		if !strings.Contains(s, want) {
			t.Errorf("scrape lacks %q:\n%s", want, s)
		}
	}

	// Reload a leaf with a different expiry and watch the series move.
	later := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	nc, nk := ca.issueValid(t, "svc-renewed", []string{"svc"}, false, time.Now().Add(-time.Minute), later)
	install(t, nk, key)
	install(t, nc, cert)
	serverReloader(t).check()
	s = scrapeText(reg)
	if want := MetricNotAfter + `{role="server"} ` + formatUnix(later); !strings.Contains(s, want) {
		t.Errorf("after reload, scrape lacks %q:\n%s", want, s)
	}
	if want := MetricReloads + `{result="success",role="server"} 1`; !strings.Contains(s, want) {
		t.Errorf("scrape lacks %q:\n%s", want, s)
	}
}

func serverReloader(t *testing.T) *reloader {
	t.Helper()
	live.mu.Lock()
	defer live.mu.Unlock()
	for _, r := range live.m {
		if r.role == roleServer {
			return r
		}
	}
	t.Fatal("no server reloader registered")
	return nil
}

func notAfterOfFile(t *testing.T, path string) time.Time {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c.NotAfter
}

// formatUnix renders a timestamp the way the metrics package does, so the
// comparison is against the real exposition rather than a guess at it.
func formatUnix(t time.Time) string {
	return strconv.FormatFloat(float64(t.Unix()), 'g', -1, 64)
}

func scrapeText(reg *metrics.Registry) string {
	var b strings.Builder
	reg.WriteTo(&b)
	return b.String()
}
