// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package taskapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/resultstore"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// ── fakes ────────────────────────────────────────────────────────────────────

type fakeUpstream struct {
	mu sync.Mutex

	usage       []*nexusv1.ReportUsageRequest
	usageAck    *nexusv1.Ack
	usageErr    error
	usageFailN  int // fail this many ReportUsage calls with Unavailable first
	permits     []*nexusv1.AcquirePermitsRequest
	permitsResp *nexusv1.AcquirePermitsResponse
	permitsErr  error
	saves       []*nexusv1.SaveCheckpointRequest
	saveAck     *nexusv1.Ack
	getResp     *nexusv1.GetCheckpointResponse
	getErr      error
}

func (f *fakeUpstream) ReportUsage(_ context.Context, req *nexusv1.ReportUsageRequest) (*nexusv1.Ack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = append(f.usage, req)
	if f.usageFailN > 0 {
		f.usageFailN--
		return nil, status.Error(codes.Unavailable, "gateway restarting")
	}
	if f.usageErr != nil {
		return nil, f.usageErr
	}
	if f.usageAck != nil {
		return f.usageAck, nil
	}
	return &nexusv1.Ack{Accepted: true}, nil
}

func (f *fakeUpstream) AcquirePermits(_ context.Context, req *nexusv1.AcquirePermitsRequest) (*nexusv1.AcquirePermitsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.permits = append(f.permits, req)
	if f.permitsErr != nil {
		return nil, f.permitsErr
	}
	if f.permitsResp != nil {
		return f.permitsResp, nil
	}
	return &nexusv1.AcquirePermitsResponse{Accepted: true, Granted: true}, nil
}

func (f *fakeUpstream) SaveCheckpoint(_ context.Context, req *nexusv1.SaveCheckpointRequest) (*nexusv1.Ack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves = append(f.saves, req)
	if f.saveAck != nil {
		return f.saveAck, nil
	}
	return &nexusv1.Ack{Accepted: true}, nil
}

func (f *fakeUpstream) GetCheckpoint(_ context.Context, _ *nexusv1.GetCheckpointRequest) (*nexusv1.GetCheckpointResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getResp != nil {
		return f.getResp, nil
	}
	return &nexusv1.GetCheckpointResponse{Accepted: true}, nil
}

func (f *fakeUpstream) lastUsage() map[string]int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.usage) == 0 {
		return nil
	}
	return f.usage[len(f.usage)-1].GetUnits()
}

func (f *fakeUpstream) usageCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.usage)
}

type memStore struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func newMemStore() *memStore { return &memStore{objs: map[string][]byte{}} }

func (m *memStore) PutCheckpointObject(_ context.Context, key string, body io.Reader, size int64) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return fmt.Errorf("size %d != %d", len(b), size)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.objs[key]; exists {
		return fmt.Errorf("checkpoint object %s overwritten", key)
	}
	m.objs[key] = b
	return nil
}

func (m *memStore) GetCheckpointObject(_ context.Context, _, key string, maxBytes int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", resultstore.ErrObjectNotFound, key)
	}
	if int64(len(b)) > maxBytes {
		return nil, resultstore.ErrObjectTooLarge
	}
	return append([]byte(nil), b...), nil
}

// ── harness ──────────────────────────────────────────────────────────────────

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// shortRoot is a socket root short enough for Darwin's 104-byte sun_path;
// t.TempDir() under /var/folders is not.
func shortRoot(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "nxt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

var allCaps = &nexusv1.WorkloadApiConfig{UsageReporting: true, RateLimits: true, Checkpoints: true}

func startTest(t *testing.T, mut func(*Config)) (*Server, *fakeUpstream, *memStore) {
	t.Helper()
	up, st := &fakeUpstream{}, newMemStore()
	cfg := Config{
		AttemptID: "0b7e5c3a-1111-2222-3333-444455556666", IdempotencyKey: "abc123", Namespace: "team-ml",
		Root: shortRoot(t), Workload: allCaps, Upstream: up, Store: st,
		LeaseToken: func() string { return "lease-1" }, Log: testLog(),
		UsageFlushInterval: time.Hour, FinalFlushTimeout: 5 * time.Second, RPCTimeout: time.Second,
	}
	if mut != nil {
		mut(&cfg)
	}
	s, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Close)
	return s, up, st
}

type client struct {
	t     *testing.T
	hc    *http.Client
	token string
}

func newClient(t *testing.T, s *Server) *client {
	sock := s.SocketPath()
	return &client{t: t, token: s.Token(), hc: &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}},
	}}
}

func (c *client) do(method, path string, body []byte) (*http.Response, []byte) {
	c.t.Helper()
	req, err := http.NewRequest(method, "http://nexus-task"+path, bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (c *client) json(method, path string, v any) (int, map[string]any) {
	c.t.Helper()
	var body []byte
	switch x := v.(type) {
	case nil:
	case string:
		body = []byte(x)
	default:
		body, _ = json.Marshal(x)
	}
	resp, b := c.do(method, path, body)
	out := map[string]any{}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	return resp.StatusCode, out
}

// ── socket and auth ──────────────────────────────────────────────────────────

func TestSocketLayoutAndModes(t *testing.T) {
	s, _, _ := startTest(t, nil)
	if got, want := filepath.Base(filepath.Dir(s.SocketPath())), "nx-0b7e5c3a1111"; got != want {
		t.Errorf("attempt dir = %q, want %q (first 12 id characters without dashes)", got, want)
	}
	if filepath.Base(s.SocketPath()) != SocketName {
		t.Errorf("socket name = %q", filepath.Base(s.SocketPath()))
	}
	fi, err := os.Stat(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o711 {
		t.Errorf("dir mode = %v, want 0711", fi.Mode().Perm())
	}
	si, err := os.Stat(s.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	if si.Mode()&os.ModeSocket == 0 || si.Mode().Perm() != 0o666 {
		t.Errorf("socket mode = %v, want a 0666 socket", si.Mode())
	}
	if len(s.Token()) != 64 {
		t.Errorf("token is %d hex chars, want 64 (256 bits)", len(s.Token()))
	}
}

func TestSocketPathTooLongFailsClearly(t *testing.T) {
	_, _, err := SocketPathFor("/tmp/"+strings.Repeat("x", 120), "attempt-1")
	if !errors.Is(err, ErrSocketPathTooLong) {
		t.Fatalf("err = %v, want ErrSocketPathTooLong", err)
	}
	if err := CheckRoot("/tmp/" + strings.Repeat("y", 120)); !errors.Is(err, ErrSocketPathTooLong) {
		t.Fatalf("CheckRoot err = %v, want ErrSocketPathTooLong", err)
	}
	if _, _, err := SocketPathFor(DefaultRoot, "----"); err == nil {
		t.Fatal("an attempt id with no alphanumerics must be refused")
	}
}

func TestBearerTokenIsRequired(t *testing.T) {
	s, _, _ := startTest(t, nil)
	c := newClient(t, s)
	for _, tok := range []string{"", "wrong", s.Token()[:63], s.Token() + "x"} {
		c.token = tok
		if code, body := c.json(http.MethodGet, "/v1/info", nil); code != http.StatusUnauthorized || body["error"] != "unauthorized" {
			t.Errorf("token %q: %d %v, want 401 unauthorized", tok, code, body)
		}
	}
	c.token = s.Token()
	if code, _ := c.json(http.MethodGet, "/v1/info", nil); code != http.StatusOK {
		t.Errorf("valid token: %d", code)
	}
	// Unauthenticated probing of unknown paths also gets 401, not a map of the API.
	c.token = ""
	if code, _ := c.json(http.MethodGet, "/v1/nope", nil); code != http.StatusUnauthorized {
		t.Errorf("unknown path without token: %d, want 401", code)
	}
}

func TestCloseRemovesSocketAndDirectory(t *testing.T) {
	s, _, _ := startTest(t, nil)
	sock, dir := s.SocketPath(), s.Dir()
	s.Close()
	s.Close() // idempotent
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("attempt dir still present after Close: %v", err)
	}
	if c, err := net.Dial("unix", sock); err == nil {
		c.Close()
		t.Fatal("socket still accepts connections after Close")
	}
}

func TestStaleDirectoryIsRecoveredButLiveOneIsNot(t *testing.T) {
	root := shortRoot(t)
	dir, sock, err := SocketPathFor(root, "attempt-9")
	if err != nil {
		t.Fatal(err)
	}
	// A crashed agent's leftover: directory and a dead socket file.
	if err := os.MkdirAll(dir, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Start(Config{AttemptID: "attempt-9", Root: root, Log: testLog()})
	if err != nil {
		t.Fatalf("stale dir not recovered: %v", err)
	}
	defer s.Close()
	if _, err := Start(Config{AttemptID: "attempt-9", Root: root, Log: testLog()}); err == nil {
		t.Fatal("a second server for a live socket must be refused, not clobber it")
	}
}

func TestInfoReportsEffectiveCapabilities(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want Capabilities
	}{
		{"all offered", nil, Capabilities{Usage: true, RateLimits: true, Checkpoints: true, Progress: true}},
		{"old gateway", func(c *Config) { c.Workload = nil }, Capabilities{Progress: true}},
		{"no upstream", func(c *Config) { c.Upstream = nil }, Capabilities{Progress: true}},
		{"no store", func(c *Config) { c.Store = nil }, Capabilities{Usage: true, RateLimits: true, Progress: true}},
		{"odd idempotency key", func(c *Config) { c.IdempotencyKey = "a/b" },
			Capabilities{Usage: true, RateLimits: true, Progress: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := startTest(t, tc.mut)
			code, body := newClient(t, s).json(http.MethodGet, "/v1/info", nil)
			if code != http.StatusOK {
				t.Fatalf("info: %d", code)
			}
			b, _ := json.Marshal(body["capabilities"])
			var got Capabilities
			_ = json.Unmarshal(b, &got)
			if got != tc.want {
				t.Errorf("capabilities = %+v, want %+v", got, tc.want)
			}
			if body["attempt_id"] != s.cfg.AttemptID {
				t.Errorf("attempt_id = %v", body["attempt_id"])
			}
		})
	}
}

func TestUnknownPathAndWrongMethod(t *testing.T) {
	s, _, _ := startTest(t, nil)
	c := newClient(t, s)
	if code, body := c.json(http.MethodGet, "/v1/nope", nil); code != http.StatusNotFound || body["error"] != "unknown_endpoint" {
		t.Errorf("unknown path: %d %v", code, body)
	}
	if code, _ := c.json(http.MethodDelete, "/v1/checkpoint", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("wrong method: %d, want 405", code)
	}
}

// ── usage ────────────────────────────────────────────────────────────────────

func TestUsageKeyGrammar(t *testing.T) {
	for key, ok := range map[string]bool{
		"tokens:openai/gpt-4o-mini:input":             true,
		"tokens:anthropic/claude-3-5-sonnet:output":   true,
		"requests:openai/gpt-4o":                      true,
		"images:mock/m@v1+beta":                       true,
		"units:embeddings":                            true,
		"tokens:azure-openai/gpt-4o:cached":           true,
		"Tokens:openai/x":                             false, // kind is lower-case
		"tokens":                                      false, // no subject
		"tokens:":                                     false,
		"tokens:-bad":                                 false, // subject starts alnum
		"tokens:openai/x:Input":                       false, // qualifier lower-case
		"tokens:openai/x:input:extra":                 false,
		"tokens:open ai":                              false,
		"cpu":                                         false, // a custom resource name can never be a usage key
		"k:" + strings.Repeat("s", 128):               true,
		"k:" + strings.Repeat("s", 129):               false,
		strings.Repeat("k", 33) + ":s":                false,
		"k:" + strings.Repeat("s", 128) + ":" + "abc": true,
	} {
		if got := ValidUsageKey(key); got != ok {
			t.Errorf("ValidUsageKey(%q) = %v, want %v", key, got, ok)
		}
	}
	// The longest key the grammar admits (194 bytes) is inside the 200-byte
	// whole-key cap, so every grammatical key is storable.
	long := strings.Repeat("k", 32) + ":" + strings.Repeat("s", 128) + ":" + strings.Repeat("q", 32)
	if len(long) > MaxUsageKeyBytes || !ValidUsageKey(long) {
		t.Errorf("the longest grammatical key (%d bytes) must be accepted", len(long))
	}
}

func TestParseUsageIncrements(t *testing.T) {
	cases := map[string]bool{
		`{"tokens:a/b:input": 10}`:                   true,
		`{"tokens:a/b:input": 0}`:                    true,
		`{"tokens:a/b:input": 1000000000000}`:        true,
		`{"tokens:a/b:input": 1000000000001}`:        false,
		`{"tokens:a/b:input": -1}`:                   false,
		`{"tokens:a/b:input": 1.5}`:                  false,
		`{"tokens:a/b:input": 1e3}`:                  false,
		`{"tokens:a/b:input": "10"}`:                 false,
		`{"tokens:a/b:input": null}`:                 false,
		`{"tokens:a/b:input": 99999999999999999999}`: false,
		`{"BAD": 1}`:                                 false,
	}
	for in, ok := range cases {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(in), &raw); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		_, err := parseUsageIncrements(raw)
		if (err == nil) != ok {
			t.Errorf("%s: err = %v, want ok=%v", in, err, ok)
		}
	}
	if _, err := parseUsageIncrements(nil); err == nil {
		t.Error("a missing units object must be refused")
	}
}

func TestUsageIncrementsBecomeCumulativeSnapshots(t *testing.T) {
	s, up, _ := startTest(t, func(c *Config) { c.UsageFlushInterval = 20 * time.Millisecond })
	c := newClient(t, s)

	code, body := c.json(http.MethodPost, "/v1/usage", map[string]any{"units": map[string]int64{
		"tokens:openai/gpt-4o-mini:input": 100, "requests:openai/gpt-4o-mini": 1}})
	if code != http.StatusOK {
		t.Fatalf("usage: %d %v", code, body)
	}
	code, body = c.json(http.MethodPost, "/v1/usage", map[string]any{"units": map[string]int64{
		"tokens:openai/gpt-4o-mini:input": 50, "tokens:openai/gpt-4o-mini:output": 7, "requests:openai/gpt-4o-mini": 1}})
	if code != http.StatusOK {
		t.Fatalf("usage: %d %v", code, body)
	}
	totals := body["totals"].(map[string]any)
	if totals["tokens:openai/gpt-4o-mini:input"] != float64(150) || totals["requests:openai/gpt-4o-mini"] != float64(2) {
		t.Errorf("totals = %v", totals)
	}

	// The background flusher sends the cumulative snapshot, not the increments.
	deadline := time.Now().Add(5 * time.Second)
	for up.lastUsage()["tokens:openai/gpt-4o-mini:input"] != 150 {
		if time.Now().After(deadline) {
			t.Fatalf("background flush never sent the cumulative totals; last = %v", up.lastUsage())
		}
		time.Sleep(10 * time.Millisecond)
	}
	n := up.usageCalls()
	time.Sleep(100 * time.Millisecond)
	if up.usageCalls() != n {
		t.Errorf("the flusher re-sent an unchanged snapshot (%d -> %d calls)", n, up.usageCalls())
	}

	s.Close()
	if err := s.FinalFlush(context.Background()); err != nil {
		t.Fatalf("FinalFlush: %v", err)
	}
	if up.usageCalls() != n {
		t.Errorf("FinalFlush re-sent an already-acknowledged snapshot")
	}
	up.mu.Lock()
	req := up.usage[len(up.usage)-1]
	up.mu.Unlock()
	if req.GetAttemptId() != s.cfg.AttemptID || req.GetLeaseToken() != "lease-1" {
		t.Errorf("flush carried attempt %q lease %q", req.GetAttemptId(), req.GetLeaseToken())
	}
	want := map[string]int64{"tokens:openai/gpt-4o-mini:input": 150, "tokens:openai/gpt-4o-mini:output": 7,
		"requests:openai/gpt-4o-mini": 2}
	for k, v := range want {
		if req.GetUnits()[k] != v {
			t.Errorf("units[%s] = %d, want %d", k, req.GetUnits()[k], v)
		}
	}
}

func TestFinalFlushRetriesTransientFailures(t *testing.T) {
	s, up, _ := startTest(t, nil)
	up.usageFailN = 2
	c := newClient(t, s)
	if code, _ := c.json(http.MethodPost, "/v1/usage", `{"units":{"tokens:mock/m:input":3}}`); code != http.StatusOK {
		t.Fatalf("usage: %d", code)
	}
	s.Close()
	if err := s.FinalFlush(context.Background()); err != nil {
		t.Fatalf("FinalFlush: %v", err)
	}
	if up.usageCalls() != 3 || up.lastUsage()["tokens:mock/m:input"] != 3 {
		t.Errorf("calls = %d, last = %v; want 3 calls ending in the totals", up.usageCalls(), up.lastUsage())
	}
}

func TestFinalFlushIsBounded(t *testing.T) {
	s, up, _ := startTest(t, func(c *Config) { c.FinalFlushTimeout = 300 * time.Millisecond })
	up.usageFailN = 1 << 30
	c := newClient(t, s)
	c.json(http.MethodPost, "/v1/usage", `{"units":{"tokens:mock/m:input":3}}`)
	s.Close()
	start := time.Now()
	err := s.FinalFlush(context.Background())
	if !errors.Is(err, ErrFinalFlushIncomplete) {
		t.Fatalf("err = %v, want ErrFinalFlushIncomplete", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("FinalFlush took %v against a 300ms budget", d)
	}
}

func TestStaleLeaseStopsUsage(t *testing.T) {
	s, up, _ := startTest(t, nil)
	up.usageAck = &nexusv1.Ack{Accepted: false, Reason: "stale_lease_token"}
	c := newClient(t, s)
	c.json(http.MethodPost, "/v1/usage", `{"units":{"tokens:mock/m:input":3}}`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if res := s.flushOnce(ctx); res != flushFinal {
		t.Fatalf("flush result = %v, want final", res)
	}
	if code, body := c.json(http.MethodPost, "/v1/usage", `{"units":{"tokens:mock/m:input":1}}`); code != http.StatusConflict || body["error"] != "stale_lease" {
		t.Errorf("usage after stale: %d %v, want 409 stale_lease", code, body)
	}
	calls := up.usageCalls()
	s.Close()
	if err := s.FinalFlush(context.Background()); err != nil {
		t.Errorf("FinalFlush after a stale lease: %v (fenced is not an agent failure)", err)
	}
	if up.usageCalls() != calls {
		t.Error("FinalFlush kept flushing after the lease was reported stale")
	}
}

func TestUsageBoundsRejectTheWholeRequest(t *testing.T) {
	s, _, _ := startTest(t, nil)
	c := newClient(t, s)
	units := map[string]int64{}
	for i := 0; i < MaxUsageKeys; i++ {
		units[fmt.Sprintf("units:k%d", i)] = 1
	}
	if code, body := c.json(http.MethodPost, "/v1/usage", map[string]any{"units": units}); code != http.StatusOK {
		t.Fatalf("64 keys: %d %v", code, body)
	}
	code, body := c.json(http.MethodPost, "/v1/usage", `{"units":{"units:k0":1,"units:k64":1}}`)
	if code != http.StatusBadRequest || body["error"] != "invalid_usage" {
		t.Errorf("65th key: %d %v, want 400 invalid_usage", code, body)
	}
	if got := s.UsageTotals()["units:k0"]; got != 1 {
		t.Errorf("a refused request was partly applied: units:k0 = %d", got)
	}

	// Cumulative cap per key: bring units:k1 to exactly 10^15, then one more.
	s.usage.mu.Lock()
	s.usage.totals["units:k1"] = MaxUsageTotalPerKey - 5
	s.usage.mu.Unlock()
	if code, body := c.json(http.MethodPost, "/v1/usage", `{"units":{"units:k1":5}}`); code != http.StatusOK {
		t.Fatalf("up to 1e15: %d %v", code, body)
	}
	if code, body := c.json(http.MethodPost, "/v1/usage", `{"units":{"units:k1":1}}`); code != http.StatusBadRequest {
		t.Errorf("past 1e15: %d %v, want 400", code, body)
	}
	if code, _ := c.json(http.MethodPost, "/v1/usage", `{"units":{"tokens:a/b":1}} trailing`); code != http.StatusBadRequest {
		t.Errorf("trailing data: %d, want 400", code)
	}
	if code, _ := c.json(http.MethodPost, "/v1/usage", `{"units":{"x:`+strings.Repeat("a", 70000)+`":1}}`); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d, want 413", code)
	}
}

func TestUsageUnsupportedWithoutCapability(t *testing.T) {
	s, up, _ := startTest(t, func(c *Config) { c.Workload = &nexusv1.WorkloadApiConfig{RateLimits: true} })
	c := newClient(t, s)
	if code, body := c.json(http.MethodPost, "/v1/usage", `{"units":{"tokens:a/b":1}}`); code != http.StatusNotImplemented || body["error"] != "unsupported" {
		t.Errorf("usage without capability: %d %v, want 501 unsupported", code, body)
	}
	s.Close()
	if err := s.FinalFlush(context.Background()); err != nil || up.usageCalls() != 0 {
		t.Errorf("FinalFlush without capability: err=%v calls=%d", err, up.usageCalls())
	}
}

// ── permits ──────────────────────────────────────────────────────────────────

func TestPermitsValidation(t *testing.T) {
	cases := []struct {
		body permitsBody
		ok   bool
	}{
		{permitsBody{Keys: []string{"openai/gpt-4o-mini"}, Requests: 1, Tokens: 900}, true},
		{permitsBody{Keys: []string{"anthropic"}, Tokens: 1}, true},
		{permitsBody{Keys: nil, Requests: 1}, false},
		{permitsBody{Keys: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}, Requests: 1}, false},
		{permitsBody{Keys: []string{"openai", "openai"}, Requests: 1}, false},
		{permitsBody{Keys: []string{"openai:x"}, Requests: 1}, false},
		{permitsBody{Keys: []string{"-openai"}, Requests: 1}, false},
		{permitsBody{Keys: []string{"openai"}}, false},
		{permitsBody{Keys: []string{"openai"}, Requests: -1, Tokens: 5}, false},
		{permitsBody{Keys: []string{"openai"}, Requests: MaxPermitRequests + 1}, false},
		{permitsBody{Keys: []string{"openai"}, Tokens: MaxPermitTokens + 1}, false},
	}
	for _, tc := range cases {
		if err := tc.body.validate(); (err == nil) != tc.ok {
			t.Errorf("%+v: err = %v, want ok=%v", tc.body, err, tc.ok)
		}
	}
}

func TestPermitsMapping(t *testing.T) {
	ok := map[string]any{"keys": []string{"openai/gpt-4o-mini", "openai"}, "requests": 1, "tokens": 1200}
	cases := []struct {
		name     string
		resp     *nexusv1.AcquirePermitsResponse
		err      error
		wantCode int
		wantErr  string
	}{
		{"granted", &nexusv1.AcquirePermitsResponse{Accepted: true, Granted: true, UnlimitedKeys: []string{"openai"}}, nil, 200, ""},
		{"denied", &nexusv1.AcquirePermitsResponse{Accepted: true, Granted: false, RetryAfterMs: 750}, nil, 200, ""},
		{"stale", &nexusv1.AcquirePermitsResponse{Accepted: false, Reason: "stale_lease_token"}, nil, 409, "stale_lease"},
		{"unimplemented", nil, status.Error(codes.Unimplemented, "no"), 501, "unsupported"},
		{"unavailable", nil, status.Error(codes.Unavailable, "down"), 503, "unavailable"},
		{"transport", nil, errors.New("connection reset"), 503, "unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, up, _ := startTest(t, nil)
			up.permitsResp, up.permitsErr = tc.resp, tc.err
			code, body := newClient(t, s).json(http.MethodPost, "/v1/permits", ok)
			if code != tc.wantCode {
				t.Fatalf("code = %d (%v), want %d", code, body, tc.wantCode)
			}
			if tc.wantErr != "" && body["error"] != tc.wantErr {
				t.Errorf("error = %v, want %s", body["error"], tc.wantErr)
			}
			up.mu.Lock()
			req := up.permits[0]
			up.mu.Unlock()
			if req.GetAttemptId() != s.cfg.AttemptID || req.GetLeaseToken() != "lease-1" || req.GetSettle() ||
				req.GetRequests() != 1 || req.GetTokens() != 1200 || len(req.GetKeys()) != 2 {
				t.Errorf("upstream request = %v", req)
			}
			if code == 200 {
				if body["granted"] != (tc.resp.GetGranted()) || body["retry_after_ms"] != float64(tc.resp.GetRetryAfterMs()) {
					t.Errorf("body = %v", body)
				}
				if _, isList := body["unlimited_keys"].([]any); !isList {
					t.Errorf("unlimited_keys = %v, want a list (never null)", body["unlimited_keys"])
				}
			}
		})
	}
}

func TestPermitsUnsupportedWithoutCapability(t *testing.T) {
	s, up, _ := startTest(t, func(c *Config) { c.Workload = &nexusv1.WorkloadApiConfig{UsageReporting: true} })
	code, _ := newClient(t, s).json(http.MethodPost, "/v1/permits", `{"keys":["openai"],"requests":1}`)
	if code != http.StatusNotImplemented || len(up.permits) != 0 {
		t.Errorf("code = %d, upstream calls = %d; want 501 and none", code, len(up.permits))
	}
}

func TestSettle(t *testing.T) {
	s, up, _ := startTest(t, nil)
	c := newClient(t, s)
	if code, body := c.json(http.MethodPost, "/v1/permits/settle", `{"keys":["openai/gpt-4o"],"tokens":-300}`); code != http.StatusNoContent {
		t.Fatalf("settle: %d %v", code, body)
	}
	if code, _ := c.json(http.MethodPost, "/v1/permits/settle", `{"keys":["openai/gpt-4o"],"tokens":0}`); code != http.StatusNoContent {
		t.Fatalf("zero settle: %d", code)
	}
	if code, _ := c.json(http.MethodPost, "/v1/permits/settle", `{"keys":["openai/gpt-4o"],"tokens":2000000000}`); code != http.StatusBadRequest {
		t.Fatalf("oversized settle: %d, want 400", code)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.permits) != 1 {
		t.Fatalf("upstream calls = %d, want 1 (a zero correction makes no round trip)", len(up.permits))
	}
	if r := up.permits[0]; !r.GetSettle() || r.GetTokens() != -300 || r.GetRequests() != 0 {
		t.Errorf("settle request = %v", r)
	}
}

// ── checkpoints ──────────────────────────────────────────────────────────────

func TestCheckpointRoundTrip(t *testing.T) {
	s, up, st := startTest(t, nil)
	c := newClient(t, s)

	for i, payload := range []string{"state-one", "state-two"} {
		resp, b := c.do(http.MethodPut, "/v1/checkpoint", []byte(payload))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("put %d: %d %s", i, resp.StatusCode, b)
		}
		var got map[string]any
		_ = json.Unmarshal(b, &got)
		seq := int64(i + 1)
		wantKey := "team-ml/checkpoints/abc123/" + s.cfg.AttemptID + "/" + fmt.Sprint(seq)
		sum := sha256.Sum256([]byte(payload))
		if got["seq"] != float64(seq) || got["key"] != wantKey || got["sha256"] != hex.EncodeToString(sum[:]) ||
			got["size_bytes"] != float64(len(payload)) {
			t.Errorf("put %d response = %v", i, got)
		}
		if string(st.objs[wantKey]) != payload {
			t.Errorf("object %s = %q, want %q", wantKey, st.objs[wantKey], payload)
		}
		up.mu.Lock()
		save := up.saves[i]
		up.mu.Unlock()
		if save.GetSeq() != seq || save.GetKey() != wantKey || save.GetSha256() != hex.EncodeToString(sum[:]) ||
			save.GetSizeBytes() != int64(len(payload)) || save.GetLeaseToken() != "lease-1" {
			t.Errorf("SaveCheckpoint %d = %v", i, save)
		}
	}

	// Read back what the control plane points at (the second one).
	up.mu.Lock()
	last := up.saves[1]
	up.getResp = &nexusv1.GetCheckpointResponse{Accepted: true, Found: true, Key: last.GetKey(), Sha256: last.GetSha256(),
		SizeBytes: last.GetSizeBytes(), Seq: 2, AttemptNumber: 3, SavedAtUnixMs: 1_700_000_000_123}
	up.mu.Unlock()
	resp, b := c.do(http.MethodGet, "/v1/checkpoint", nil)
	if resp.StatusCode != http.StatusOK || string(b) != "state-two" {
		t.Fatalf("get: %d %q", resp.StatusCode, b)
	}
	for h, want := range map[string]string{
		"Content-Type": "application/octet-stream", "X-Nexus-Checkpoint-Seq": "2",
		"X-Nexus-Checkpoint-Attempt": "3", "X-Nexus-Checkpoint-Saved-At": "1700000000123",
	} {
		if got := resp.Header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
}

func TestCheckpointRefusals(t *testing.T) {
	t.Run("stale checkpoint", func(t *testing.T) {
		s, up, _ := startTest(t, nil)
		up.saveAck = &nexusv1.Ack{Accepted: false, Reason: "stale_checkpoint"}
		code, body := newClient(t, s).json(http.MethodPut, "/v1/checkpoint", "x")
		if code != http.StatusConflict || body["error"] != "stale_checkpoint" {
			t.Errorf("%d %v, want 409 stale_checkpoint", code, body)
		}
	})
	t.Run("stale lease", func(t *testing.T) {
		s, up, _ := startTest(t, nil)
		up.saveAck = &nexusv1.Ack{Accepted: false, Reason: "stale_lease_token"}
		code, body := newClient(t, s).json(http.MethodPut, "/v1/checkpoint", "x")
		if code != http.StatusConflict || body["error"] != "stale_lease" {
			t.Errorf("%d %v, want 409 stale_lease", code, body)
		}
	})
	t.Run("too large", func(t *testing.T) {
		s, up, st := startTest(t, func(c *Config) {
			c.Workload = &nexusv1.WorkloadApiConfig{Checkpoints: true, MaxCheckpointBytes: 8}
		})
		code, body := newClient(t, s).json(http.MethodPut, "/v1/checkpoint", "123456789")
		if code != http.StatusRequestEntityTooLarge || body["error"] != "too_large" {
			t.Errorf("%d %v, want 413 too_large", code, body)
		}
		if len(st.objs) != 0 || len(up.saves) != 0 {
			t.Error("an oversized checkpoint reached the store or the control plane")
		}
	})
	t.Run("no store", func(t *testing.T) {
		s, _, _ := startTest(t, func(c *Config) { c.Store = nil })
		if code, _ := newClient(t, s).json(http.MethodPut, "/v1/checkpoint", "x"); code != http.StatusNotImplemented {
			t.Errorf("%d, want 501", code)
		}
	})
	t.Run("unimplemented upstream", func(t *testing.T) {
		s, up, _ := startTest(t, nil)
		up.getErr = status.Error(codes.Unimplemented, "old gateway")
		if code, _ := newClient(t, s).json(http.MethodGet, "/v1/checkpoint", nil); code != http.StatusNotImplemented {
			t.Errorf("%d, want 501", code)
		}
	})
}

func TestCheckpointGetFailsSafe(t *testing.T) {
	good := []byte("state")
	sum := sha256.Sum256(good)
	digest := hex.EncodeToString(sum[:])
	key := "team-ml/checkpoints/abc123/prev-attempt/4"
	cases := []struct {
		name     string
		resp     *nexusv1.GetCheckpointResponse
		stored   []byte
		wantCode int
		wantErr  string
	}{
		{"none saved", &nexusv1.GetCheckpointResponse{Accepted: true}, nil, 404, "not_found"},
		{"object missing", &nexusv1.GetCheckpointResponse{Accepted: true, Found: true, Key: key, Sha256: digest, SizeBytes: 5}, nil, 404, "checkpoint_unavailable"},
		{"digest mismatch", &nexusv1.GetCheckpointResponse{Accepted: true, Found: true, Key: key, Sha256: digest, SizeBytes: 5}, []byte("STATE"), 404, "checkpoint_unavailable"},
		{"stale lease", &nexusv1.GetCheckpointResponse{Accepted: false, Reason: "stale_lease_token"}, nil, 409, "stale_lease"},
		{"ok", &nexusv1.GetCheckpointResponse{Accepted: true, Found: true, Key: key, Sha256: digest, SizeBytes: 5}, good, 200, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, up, st := startTest(t, nil)
			up.getResp = tc.resp
			if tc.stored != nil {
				st.objs[key] = tc.stored
			}
			code, body := newClient(t, s).json(http.MethodGet, "/v1/checkpoint", nil)
			if code != tc.wantCode {
				t.Fatalf("code = %d (%v), want %d", code, body, tc.wantCode)
			}
			if tc.wantErr != "" && body["error"] != tc.wantErr {
				t.Errorf("error = %v, want %s", body["error"], tc.wantErr)
			}
		})
	}
}

// ── progress and declared errors ─────────────────────────────────────────────

func TestProgressIsForwardedAndDeduplicated(t *testing.T) {
	var mu sync.Mutex
	var got []string
	s, _, _ := startTest(t, func(c *Config) {
		c.Workload = nil // progress needs no capability
		c.Progress = func(m string, f float64) {
			mu.Lock()
			got = append(got, fmt.Sprintf("%s@%.2f", m, f))
			mu.Unlock()
		}
	})
	c := newClient(t, s)
	for _, b := range []string{
		`{"message":"step 1","fraction":0.1}`,
		`{"message":"step 1","fraction":0.1}`, // identical within a second: dropped
		`{"message":"step 2","fraction":null}`,
		`{"message":"step 3","fraction":7}`, // clamped
	} {
		if code, _ := c.json(http.MethodPost, "/v1/progress", b); code != http.StatusNoContent {
			t.Fatalf("progress %s: %d", b, code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"step 1@0.10", "step 2@0.00", "step 3@1.00"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("forwarded %v, want %v", got, want)
	}
}

func TestDeclaredErrorValidation(t *testing.T) {
	i := func(v int64) *int64 { return &v }
	cases := []struct {
		body declaredErrorBody
		ok   bool
		want nexusv1.ErrorClass
	}{
		{declaredErrorBody{Class: "rate_limited", RetryAfterMs: i(1500)}, true, nexusv1.ErrorClass_ERROR_CLASS_RATE_LIMITED},
		{declaredErrorBody{Class: "ERROR_CLASS_RATE_LIMITED"}, true, nexusv1.ErrorClass_ERROR_CLASS_RATE_LIMITED},
		{declaredErrorBody{Class: "Transient"}, true, nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT},
		{declaredErrorBody{Class: "validation"}, true, nexusv1.ErrorClass_ERROR_CLASS_VALIDATION},
		{declaredErrorBody{Class: "permission_denied"}, true, nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED},
		{declaredErrorBody{Class: "application"}, true, nexusv1.ErrorClass_ERROR_CLASS_APPLICATION},
		{declaredErrorBody{Class: "not_found"}, true, nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND},
		{declaredErrorBody{Class: "worker_lost"}, false, 0},
		{declaredErrorBody{Class: "ERROR_CLASS_OOM"}, false, 0},
		{declaredErrorBody{Class: "timeout"}, false, 0},
		{declaredErrorBody{Class: "infrastructure"}, false, 0},
		{declaredErrorBody{Class: "unknown"}, false, 0},
		{declaredErrorBody{Class: "unspecified"}, false, 0},
		{declaredErrorBody{Class: ""}, false, 0},
		{declaredErrorBody{Class: "rate_limited", RetryAfterMs: i(-1)}, false, 0},
	}
	for _, tc := range cases {
		d, err := tc.body.validate()
		if (err == nil) != tc.ok {
			t.Errorf("%+v: err = %v, want ok=%v", tc.body, err, tc.ok)
			continue
		}
		if tc.ok && d.Class != tc.want {
			t.Errorf("%+v: class = %v, want %v", tc.body, d.Class, tc.want)
		}
	}
	d, _ := declaredErrorBody{Class: "rate_limited", Message: strings.Repeat("é", 3000),
		RetryAfterMs: i(int64(48 * time.Hour / time.Millisecond))}.validate()
	if len(d.Message) > MaxDeclaredMessageBytes || !strings.HasPrefix(d.Message, "é") || strings.HasSuffix(d.Message, "\xc3") {
		t.Errorf("message not truncated on a rune boundary: %d bytes", len(d.Message))
	}
	if d.RetryAfterMs != MaxDeclaredRetryAfter.Milliseconds() {
		t.Errorf("retry_after_ms = %d, want capped at 24h", d.RetryAfterMs)
	}
	if !d.Retryable() {
		t.Error("rate_limited must be retryable")
	}
	if (DeclaredError{Class: nexusv1.ErrorClass_ERROR_CLASS_VALIDATION}).Retryable() {
		t.Error("validation must not be retryable")
	}
}

func TestDeclaredErrorEndpoint(t *testing.T) {
	s, _, _ := startTest(t, func(c *Config) { c.Workload = nil })
	c := newClient(t, s)
	if _, ok := s.Declared(); ok {
		t.Fatal("nothing declared yet")
	}
	if code, body := c.json(http.MethodPost, "/v1/error", `{"class":"oom","message":"x"}`); code != http.StatusBadRequest || body["error"] != "invalid_error" {
		t.Errorf("oom: %d %v, want 400 invalid_error", code, body)
	}
	if code, _ := c.json(http.MethodPost, "/v1/error",
		`{"class":"rate_limited","type":"RateLimited","message":"429 from openai","retry_after_ms":2000}`); code != http.StatusNoContent {
		t.Fatalf("declare: %d", code)
	}
	d, ok := s.Declared()
	if !ok || d.Class != nexusv1.ErrorClass_ERROR_CLASS_RATE_LIMITED || d.Type != "RateLimited" ||
		d.Message != "429 from openai" || d.RetryAfterMs != 2000 {
		t.Errorf("declared = %+v, %v", d, ok)
	}
}
