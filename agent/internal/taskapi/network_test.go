// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package taskapi

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// The network transport of the workload API (F148, ADR-0032 addendum): the
// agent serves every remote attempt (kubernetes pods on other nodes) on ONE
// TLS listener, routed by the attempt's bearer token. These tests pin the
// properties the transport is justified by: TLS only, verified against a CA
// that exists only in this agent's memory; a token acts as exactly one
// attempt; a token stops working when its attempt ends; and nothing another
// agent (another tenant) minted is accepted.

func startNetwork(t *testing.T) *Network {
	t.Helper()
	n, err := StartNetwork(NetworkConfig{Listen: "127.0.0.1:0", Log: testLog()})
	if err != nil {
		t.Fatalf("StartNetwork: %v", err)
	}
	t.Cleanup(n.Close)
	return n
}

func startRemote(t *testing.T, n *Network, attemptID string, mut func(*Config)) (*Server, *fakeUpstream) {
	t.Helper()
	up := &fakeUpstream{}
	cfg := Config{
		AttemptID: attemptID, IdempotencyKey: "idem-" + attemptID[:4], Namespace: "team-ml",
		Network: n, Workload: allCaps, Upstream: up, Store: newMemStore(),
		LeaseToken: func() string { return "lease-" + attemptID }, Log: testLog(),
		UsageFlushInterval: time.Hour, FinalFlushTimeout: 2 * time.Second, RPCTimeout: time.Second,
	}
	if mut != nil {
		mut(&cfg)
	}
	s, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start(remote): %v", err)
	}
	t.Cleanup(s.Close)
	return s, up
}

// tlsClient trusts exactly caPEM, as the SDK does with NEXUS_TASK_API_CA.
func tlsClient(t *testing.T, caPEM []byte) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("CA PEM did not parse")
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
}

func call(t *testing.T, hc *http.Client, url, method, path, token string, body any) (int, map[string]any, error) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out := map[string]any{}
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out, nil
}

func TestNetworkServesAnAttemptOverVerifiedTLS(t *testing.T) {
	n := startNetwork(t)
	if !strings.HasPrefix(n.URL(), "https://127.0.0.1:") {
		t.Fatalf("URL %q", n.URL())
	}
	s, up := startRemote(t, n, "a1b2c3d4-0000-0000-0000-000000000001", nil)
	if s.SocketPath() != "" {
		t.Errorf("a remote attempt created a socket %q", s.SocketPath())
	}
	if s.URL() != n.URL() || !bytes.Equal(s.CAPEM(), n.CAPEM()) {
		t.Errorf("server URL/CA do not match the network's")
	}
	hc := tlsClient(t, n.CAPEM())

	code, info, err := call(t, hc, n.URL(), "GET", "/v1/info", s.Token(), nil)
	if err != nil || code != 200 || info["attempt_id"] != "a1b2c3d4-0000-0000-0000-000000000001" {
		t.Fatalf("info: %d %v %v", code, info, err)
	}
	code, _, _ = call(t, hc, n.URL(), "POST", "/v1/usage", s.Token(),
		map[string]any{"units": map[string]int{"tokens:openai/gpt-4o-mini:input": 7}})
	if code != 200 {
		t.Fatalf("usage: %d", code)
	}
	if err := s.FinalFlush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := up.lastUsage()["tokens:openai/gpt-4o-mini:input"]; got != 7 {
		t.Fatalf("upstream usage %v", up.lastUsage())
	}
	if up.usage[0].GetAttemptId() != s.cfg.AttemptID || up.usage[0].GetLeaseToken() != "lease-"+s.cfg.AttemptID {
		t.Fatalf("forwarded as %s with lease %s", up.usage[0].GetAttemptId(), up.usage[0].GetLeaseToken())
	}

	// No token, a wrong token, a token with a trailing byte: 401, and no
	// hint which part was wrong.
	for _, tok := range []string{"", "nope", s.Token() + "0", strings.ToUpper(s.Token())} {
		if code, body, _ := call(t, hc, n.URL(), "GET", "/v1/info", tok, nil); code != http.StatusUnauthorized || body["error"] != "unauthorized" {
			t.Errorf("token %q: %d %v", tok, code, body)
		}
	}
}

func TestNetworkIsTLSOnlyAndPinnedToItsOwnCA(t *testing.T) {
	n := startNetwork(t)
	s, _ := startRemote(t, n, "a1b2c3d4-0000-0000-0000-000000000002", nil)

	// Plain HTTP to the TLS port gets nothing usable back: the bearer token
	// would otherwise cross the pod network in clear.
	plain := &http.Client{Timeout: 3 * time.Second}
	if code, _, err := call(t, plain, strings.Replace(n.URL(), "https://", "http://", 1), "GET", "/v1/info", s.Token(), nil); err == nil && code == 200 {
		t.Fatal("the workload API answered plain HTTP")
	}
	// The system trust store does not trust it, and neither does a client
	// pinned to ANOTHER agent's CA: the certificate is valid only for pods
	// this agent launched.
	sys := &http.Client{Timeout: 3 * time.Second}
	if _, _, err := call(t, sys, n.URL(), "GET", "/v1/info", s.Token(), nil); err == nil {
		t.Fatal("a client with the system roots accepted the agent's certificate")
	}
	other := startNetwork(t)
	if _, _, err := call(t, tlsClient(t, other.CAPEM()), n.URL(), "GET", "/v1/info", s.Token(), nil); err == nil {
		t.Fatal("another agent's CA validated this agent's certificate")
	}
	// The CA is a CA, the key never leaves memory: only a certificate is
	// exported.
	block, rest := pem.Decode(n.CAPEM())
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("CAPEM exports %v (rest %q); want exactly one certificate", block, rest)
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !ca.IsCA {
		t.Fatalf("CA %v %v", ca, err)
	}
	// TLS 1.2 is the floor.
	old := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: x509.NewCertPool(), MaxVersion: tls.VersionTLS11, InsecureSkipVerify: true, //nolint:gosec // the point of the test
	}}}
	if _, _, err := call(t, old, n.URL(), "GET", "/v1/info", s.Token(), nil); err == nil {
		t.Fatal("TLS 1.1 was accepted")
	}
}

// One token acts as exactly one attempt. There is no attempt id in any
// request to forge; a header naming another attempt changes nothing.
func TestNetworkTokenActsAsExactlyItsOwnAttempt(t *testing.T) {
	n := startNetwork(t)
	a, upA := startRemote(t, n, "aaaaaaaa-0000-0000-0000-000000000001", nil)
	b, upB := startRemote(t, n, "bbbbbbbb-0000-0000-0000-000000000002", nil)
	hc := tlsClient(t, n.CAPEM())

	req, _ := http.NewRequest("POST", n.URL()+"/v1/usage",
		strings.NewReader(`{"units":{"requests:mock/m":3},"attempt_id":"bbbbbbbb-0000-0000-0000-000000000002"}`))
	req.Header.Set("Authorization", "Bearer "+a.Token())
	req.Header.Set("X-Nexus-Attempt", b.cfg.AttemptID)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("usage with A's token: %d", resp.StatusCode)
	}
	if err := a.FinalFlush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := b.FinalFlush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if upA.lastUsage()["requests:mock/m"] != 3 {
		t.Fatalf("A's upstream %v", upA.lastUsage())
	}
	if upB.usageCalls() != 0 {
		t.Fatalf("A's token reached B's attempt: %v", upB.usage)
	}
	if code, info, _ := call(t, hc, n.URL(), "GET", "/v1/info", b.Token(), nil); code != 200 || info["attempt_id"] != b.cfg.AttemptID {
		t.Fatalf("B's own token: %d %v", code, info)
	}
}

// Revocation: the moment an attempt's API is closed (its Run returned: it
// finished, was cancelled, or lost its lease) its token is refused, while the
// other attempts on the same listener keep working.
func TestNetworkTokenIsRevokedWhenItsAttemptEnds(t *testing.T) {
	n := startNetwork(t)
	a, _ := startRemote(t, n, "aaaaaaaa-0000-0000-0000-00000000000a", nil)
	b, _ := startRemote(t, n, "bbbbbbbb-0000-0000-0000-00000000000b", nil)
	hc := tlsClient(t, n.CAPEM())
	if code, _, _ := call(t, hc, n.URL(), "GET", "/v1/info", a.Token(), nil); code != 200 {
		t.Fatalf("before close: %d", code)
	}
	a.Close()
	if code, body, _ := call(t, hc, n.URL(), "POST", "/v1/usage", a.Token(),
		map[string]any{"units": map[string]int{"requests:mock/m": 1}}); code != http.StatusUnauthorized {
		t.Fatalf("after close: %d %v; want 401", code, body)
	}
	if code, _, _ := call(t, hc, n.URL(), "GET", "/v1/info", b.Token(), nil); code != 200 {
		t.Fatalf("a sibling attempt stopped working: %d", code)
	}
	if n.Attempts() != 1 {
		t.Fatalf("network still routes %d attempts; want 1", n.Attempts())
	}
}

// A lease that ends while the pod still runs: the control plane's fence
// answers stale, the workload gets 409 and every later usage report is
// refused at once, before any upstream call.
func TestNetworkStaleLeaseIsAConflict(t *testing.T) {
	n := startNetwork(t)
	s, up := startRemote(t, n, "cccccccc-0000-0000-0000-00000000000c", nil)
	up.permitsResp = &nexusv1.AcquirePermitsResponse{Accepted: false, Reason: reasonStaleLease}
	hc := tlsClient(t, n.CAPEM())
	code, body, _ := call(t, hc, n.URL(), "POST", "/v1/permits", s.Token(),
		map[string]any{"keys": []string{"mock"}, "requests": 1})
	if code != http.StatusConflict || body["error"] != "stale_lease" {
		t.Fatalf("permits on a stale lease: %d %v", code, body)
	}
	if code, _, _ := call(t, hc, n.URL(), "POST", "/v1/usage", s.Token(),
		map[string]any{"units": map[string]int{"requests:mock/m": 1}}); code != http.StatusConflict {
		t.Fatalf("usage after a stale answer: %d; want 409", code)
	}
}

// Tenancy: an agent serves one tenant, and its network knows only the
// attempts it launched. A token minted by another agent -- another tenant's
// pod -- is refused, and so is the reverse.
func TestNetworkRefusesAnotherAgentsTokens(t *testing.T) {
	mine, theirs := startNetwork(t), startNetwork(t)
	a, _ := startRemote(t, mine, "aaaaaaaa-0000-0000-0000-0000000000a1", nil)
	b, _ := startRemote(t, theirs, "bbbbbbbb-0000-0000-0000-0000000000b1", nil)
	if code, _, _ := call(t, tlsClient(t, mine.CAPEM()), mine.URL(), "GET", "/v1/info", b.Token(), nil); code != http.StatusUnauthorized {
		t.Fatalf("another tenant's token on my agent: %d", code)
	}
	if code, _, _ := call(t, tlsClient(t, theirs.CAPEM()), theirs.URL(), "GET", "/v1/info", a.Token(), nil); code != http.StatusUnauthorized {
		t.Fatalf("my token on another tenant's agent: %d", code)
	}
}

func TestNetworkCertificateNamesTheAdvertisedHost(t *testing.T) {
	for _, c := range []struct{ advertise, host string }{
		{"host.docker.internal:0", "host.docker.internal"},
		{"10.1.2.3:9443", "10.1.2.3"},
	} {
		n, err := StartNetwork(NetworkConfig{Listen: "127.0.0.1:0", Advertise: c.advertise,
			ExtraNames: []string{"nexus-agent.nexus-exec.svc"}, Log: testLog()})
		if err != nil {
			t.Fatal(err)
		}
		cert := n.leaf()
		if err := cert.VerifyHostname(c.host); err != nil {
			t.Errorf("%s: %v", c.advertise, err)
		}
		if err := cert.VerifyHostname("nexus-agent.nexus-exec.svc"); err != nil {
			t.Errorf("extra name: %v", err)
		}
		if err := cert.VerifyHostname("evil.example"); err == nil {
			t.Error("the certificate matches an unrelated name")
		}
		if !strings.HasPrefix(n.URL(), "https://"+c.host+":") {
			t.Errorf("URL %q", n.URL())
		}
		n.Close()
	}
	for _, bad := range []string{"no-port", "http://x:1", ":9443"} {
		if _, err := StartNetwork(NetworkConfig{Listen: "127.0.0.1:0", Advertise: bad, Log: testLog()}); err == nil {
			t.Errorf("advertise %q accepted", bad)
		}
	}
}

// Closing the network refuses new attempts rather than serving them nowhere.
func TestNetworkClosedRefusesNewAttempts(t *testing.T) {
	n, err := StartNetwork(NetworkConfig{Listen: "127.0.0.1:0", Log: testLog()})
	if err != nil {
		t.Fatal(err)
	}
	n.Close()
	if _, err := Start(Config{AttemptID: "dddddddd-0000-0000-0000-00000000000d", Network: n, Log: testLog()}); err == nil {
		t.Fatal("an attempt registered on a closed network")
	}
	if _, err := net.DialTimeout("tcp", n.Addr().String(), time.Second); err == nil {
		t.Fatal("a closed network still accepts connections")
	}
}
