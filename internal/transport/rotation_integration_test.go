// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"fmt"
	"math/big"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
)

// These tests exercise rotation through REAL handshakes on a real gRPC server,
// with the background poller running exactly as it does in a service. Every
// assertion is on the serial number actually presented in a handshake -- by the
// server to the client, and by the client to the server -- rather than on the
// reloader's bookkeeping. A test that checked "reload succeeded" could pass
// while handshakes still used the old certificate, which is the F89 shape: an
// instrument that reports success without looking at the thing.

const testReloadInterval = "50ms"

// clientSerials records the serial of the client certificate on every RPC the
// server handles, as the server saw it.
type clientSerials struct {
	mu   sync.Mutex
	last *big.Int
}

func (c *clientSerials) interceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	if p, ok := peer.FromContext(ctx); ok {
		if ti, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(ti.State.PeerCertificates) > 0 {
			c.mu.Lock()
			c.last = ti.State.PeerCertificates[0].SerialNumber
			c.mu.Unlock()
		}
	}
	return h(ctx, req)
}

func (c *clientSerials) get() *big.Int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

type liveServer struct {
	addr    string
	health  *health.Server
	clients *clientSerials
}

func startLiveServer(t *testing.T, extra ...grpc.ServerOption) *liveServer {
	t.Helper()
	sopts, err := ServerOptions()
	if err != nil {
		t.Fatalf("ServerOptions: %v", err)
	}
	cs := &clientSerials{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	opts := append(append(sopts, grpc.UnaryInterceptor(cs.interceptor)), extra...)
	srv := grpc.NewServer(opts...)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return &liveServer{addr: lis.Addr().String(), health: hs, clients: cs}
}

// servedSerial performs one RPC on conn and returns the serial of the
// certificate the server presented in that connection's handshake.
func servedSerial(ctx context.Context, conn *grpc.ClientConn, waitForReady bool) (*big.Int, error) {
	var p peer.Peer
	_, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{},
		grpc.Peer(&p), grpc.WaitForReady(waitForReady))
	if err != nil {
		return nil, err
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.PeerCertificates) == 0 {
		return nil, fmt.Errorf("RPC completed without a TLS peer certificate (AuthInfo %T)", p.AuthInfo)
	}
	return ti.State.PeerCertificates[0].SerialNumber, nil
}

func eventually(t *testing.T, within time.Duration, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last error
	for time.Now().Before(deadline) {
		if last = f(); last == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s: not true within %s; last: %v", what, within, last)
}

// rotatePair installs a freshly issued pair over the live paths, key first.
// Each file is replaced atomically, but the PAIR is not -- which is realistic,
// and is what the debounce and pair validation exist for.
func rotatePair(t *testing.T, newCert, newKey, liveCert, liveKey string) {
	t.Helper()
	install(t, newKey, liveKey)
	install(t, newCert, liveCert)
}

// livePaths copies an issued pair to stable "mounted" paths that the test will
// later overwrite, as a secret volume or a renewal job would.
func livePaths(t *testing.T, cert, key string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	lc, lk := dir+"/tls.crt", dir+"/tls.key"
	install(t, cert, lc)
	install(t, key, lk)
	return lc, lk
}

// TestRotationWithoutRestart is the end-to-end claim: the server and client
// certificates are rotated on disk while a server is running and a connection
// is open, and
//
//   - new handshakes present the NEW certificates, in both directions;
//   - the connection established before the rotation keeps working, on its
//     original certificates, and a stream opened on it is never interrupted --
//     which is impossible if anything restarted.
func TestRotationWithoutRestart(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvReloadInterval, testReloadInterval)
	ca := newTestCA(t, "nexus")

	s1c, s1k := ca.issue(t, "server-gen1", []string{"scheduler"}, false)
	c1c, c1k := ca.issue(t, "client-gen1", nil, true)
	srvCert, srvKey := livePaths(t, s1c, s1k)
	cliCert, cliKey := livePaths(t, c1c, c1k)

	t.Setenv(EnvServerCert, srvCert)
	t.Setenv(EnvServerKey, srvKey)
	t.Setenv(EnvCACert, ca.caFile)
	t.Setenv(EnvServerRequireClientCert, "1")
	t.Setenv(EnvClientCert, cliCert)
	t.Setenv(EnvClientKey, cliKey)

	srv := startLiveServer(t)
	dopt, err := DialOption()
	if err != nil {
		t.Fatal(err)
	}

	s1, c1 := serialOfFile(t, s1c), serialOfFile(t, c1c)

	// A connection established BEFORE rotation, with a stream on it.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connA, err := grpc.NewClient(srv.addr, dopt)
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Close()
	got, err := servedSerial(ctx, connA, true)
	if err != nil {
		t.Fatalf("initial RPC: %v", err)
	}
	if got.Cmp(s1) != 0 {
		t.Fatalf("initial handshake served %s, want gen1 %s", fmtSerial(got), fmtSerial(s1))
	}
	if cs := srv.clients.get(); cs == nil || cs.Cmp(c1) != 0 {
		t.Fatalf("server saw client serial %s, want gen1 %s", fmtSerial(cs), fmtSerial(c1))
	}
	watch, err := healthpb.NewHealthClient(connA).Watch(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := watch.Recv(); err != nil {
		t.Fatalf("first watch update: %v", err)
	}

	// Rotate both identities on disk.
	s2c, s2k := ca.issue(t, "server-gen2", []string{"scheduler"}, false)
	c2c, c2k := ca.issue(t, "client-gen2", nil, true)
	s2, c2 := serialOfFile(t, s2c), serialOfFile(t, c2c)
	if s2.Cmp(s1) == 0 || c2.Cmp(c1) == 0 {
		t.Fatal("test setup: gen2 serials equal gen1; the assertions below could not fail")
	}
	rotatePair(t, s2c, s2k, srvCert, srvKey)
	rotatePair(t, c2c, c2k, cliCert, cliKey)

	// New handshakes must converge on gen2 in BOTH directions. A fresh ClientConn
	// per attempt guarantees each attempt is a new handshake.
	eventually(t, 10*time.Second, "a new handshake presents the gen2 certificates", func() error {
		conn, err := grpc.NewClient(srv.addr, dopt)
		if err != nil {
			return err
		}
		defer conn.Close()
		rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
		defer rcancel()
		got, err := servedSerial(rctx, conn, false)
		if err != nil {
			return err
		}
		if got.Cmp(s2) != 0 {
			return fmt.Errorf("server presented %s, want gen2 %s", fmtSerial(got), fmtSerial(s2))
		}
		if cs := srv.clients.get(); cs == nil || cs.Cmp(c2) != 0 {
			return fmt.Errorf("server saw client %s, want gen2 %s", fmtSerial(cs), fmtSerial(c2))
		}
		return nil
	})

	// The pre-rotation connection is untouched: still gen1, still serving, and
	// its stream still delivers updates.
	got, err = servedSerial(ctx, connA, false)
	if err != nil {
		t.Fatalf("pre-rotation connection broke after rotation: %v", err)
	}
	if got.Cmp(s1) != 0 {
		t.Fatalf("pre-rotation connection reports %s; it should still be on its original gen1 handshake %s", fmtSerial(got), fmtSerial(s1))
	}
	srv.health.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	upd, err := watch.Recv()
	if err != nil {
		t.Fatalf("stream opened before rotation was interrupted: %v", err)
	}
	if upd.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("stream delivered %v, want NOT_SERVING", upd.GetStatus())
	}

	// Now a BAD rotation: a certificate without its key. New handshakes must
	// keep presenting gen2, not fail.
	s3c, _ := ca.issue(t, "server-gen3", []string{"scheduler"}, false)
	install(t, s3c, srvCert)
	time.Sleep(300 * time.Millisecond) // several poll intervals
	conn, err := grpc.NewClient(srv.addr, dopt)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	got, err = servedSerial(ctx, conn, true)
	if err != nil {
		t.Fatalf("after a rejected rotation, new handshakes FAIL (the old pair should still serve): %v", err)
	}
	if got.Cmp(s2) != 0 {
		t.Fatalf("after a rejected rotation, server presented %s, want gen2 %s", fmtSerial(got), fmtSerial(s2))
	}
}

// TestRotationReachesAnExistingClientConn models production: the server carries
// a MaxConnectionAge (config.PeerServerOptions uses 30s), so an established
// ClientConn is periodically re-handshaken and picks up rotated certificates on
// its own -- same ClientConn object, no redial by the application.
func TestRotationReachesAnExistingClientConn(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvReloadInterval, testReloadInterval)
	ca := newTestCA(t, "nexus")

	s1c, s1k := ca.issue(t, "server-gen1", []string{"scheduler"}, false)
	c1c, c1k := ca.issue(t, "client-gen1", nil, true)
	srvCert, srvKey := livePaths(t, s1c, s1k)
	cliCert, cliKey := livePaths(t, c1c, c1k)
	t.Setenv(EnvServerCert, srvCert)
	t.Setenv(EnvServerKey, srvKey)
	t.Setenv(EnvCACert, ca.caFile)
	t.Setenv(EnvServerRequireClientCert, "1")
	t.Setenv(EnvClientCert, cliCert)
	t.Setenv(EnvClientKey, cliKey)

	srv := startLiveServer(t, grpc.KeepaliveParams(keepalive.ServerParameters{
		MaxConnectionAge:      400 * time.Millisecond,
		MaxConnectionAgeGrace: 200 * time.Millisecond,
	}))
	dopt, err := DialOption()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(srv.addr, dopt)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s1 := serialOfFile(t, s1c)
	if got, err := servedSerial(ctx, conn, true); err != nil || got.Cmp(s1) != 0 {
		t.Fatalf("initial: serial %s err %v, want %s", fmtSerial(got), err, fmtSerial(s1))
	}

	s2c, s2k := ca.issue(t, "server-gen2", []string{"scheduler"}, false)
	c2c, c2k := ca.issue(t, "client-gen2", nil, true)
	s2, c2 := serialOfFile(t, s2c), serialOfFile(t, c2c)
	rotatePair(t, s2c, s2k, srvCert, srvKey)
	rotatePair(t, c2c, c2k, cliCert, cliKey)

	eventually(t, 10*time.Second, "the SAME ClientConn re-handshakes onto gen2", func() error {
		got, err := servedSerial(ctx, conn, true)
		if err != nil {
			return err
		}
		if got.Cmp(s2) != 0 {
			return fmt.Errorf("served %s, want %s", fmtSerial(got), fmtSerial(s2))
		}
		if cs := srv.clients.get(); cs == nil || cs.Cmp(c2) != 0 {
			return fmt.Errorf("server saw client %s, want %s", fmtSerial(cs), fmtSerial(c2))
		}
		return nil
	})
}

// TestCABundleOverlapRotation walks a CA rotation through the three states an
// operator moves through, with real handshakes at each:
//
//  1. bundle = old CA:        old-CA client accepted, new-CA client refused
//  2. bundle = old + new CA:  both accepted; the server leaf moves to the new CA
//  3. bundle = new CA only:   old-CA client refused, new-CA client accepted
//
// State 2 is the point: without it, rotating a CA is an outage, because leaves
// cannot all be re-issued at the same instant the bundle changes.
func TestCABundleOverlapRotation(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvReloadInterval, testReloadInterval)
	oldCA := newTestCA(t, "old")
	newCA := newTestCA(t, "new")

	caPEM := func(cas ...*testPKI) []byte {
		var out []byte
		for _, c := range cas {
			b, err := os.ReadFile(c.caFile)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, b...)
		}
		return out
	}
	bundle := t.TempDir() + "/ca.crt"
	writeBundle := func(cas ...*testPKI) {
		if err := os.WriteFile(bundle+".tmp", caPEM(cas...), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(bundle+".tmp", bundle); err != nil {
			t.Fatal(err)
		}
	}
	writeBundle(oldCA)

	sc, sk := oldCA.issue(t, "server-old", []string{"scheduler"}, false)
	srvCert, srvKey := livePaths(t, sc, sk)
	t.Setenv(EnvServerCert, srvCert)
	t.Setenv(EnvServerKey, srvKey)
	t.Setenv(EnvCACert, bundle)
	t.Setenv(EnvServerRequireClientCert, "1")
	srv := startLiveServer(t)

	// Two client identities in one process, one per CA. Each DialOption call
	// binds to its own reloader; both share the bundle.
	dialAs := func(ca *testPKI, name string) grpc.DialOption {
		c, k := ca.issue(t, name, nil, true)
		t.Setenv(EnvClientCert, c)
		t.Setenv(EnvClientKey, k)
		d, err := DialOption()
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	oldClient := dialAs(oldCA, "client-old")
	newClient := dialAs(newCA, "client-new")

	accepts := func(d grpc.DialOption) func() error {
		return func() error { return check(t, srv.addr, d) }
	}
	refuses := func(d grpc.DialOption) func() error {
		return func() error {
			if check(t, srv.addr, d) == nil {
				return fmt.Errorf("handshake succeeded")
			}
			return nil
		}
	}

	// State 1.
	if err := accepts(oldClient)(); err != nil {
		t.Fatalf("state 1: old-CA client refused: %v", err)
	}
	if err := refuses(newClient)(); err != nil {
		t.Fatal("state 1: new-CA client accepted before its CA was trusted -- the negative half of this test is not testing anything")
	}

	// State 2: overlap, and the server moves to a new-CA leaf.
	writeBundle(oldCA, newCA)
	nsc, nsk := newCA.issue(t, "server-new", []string{"scheduler"}, false)
	rotatePair(t, nsc, nsk, srvCert, srvKey)
	eventually(t, 10*time.Second, "state 2: new-CA client accepted during overlap", accepts(newClient))
	if err := accepts(oldClient)(); err != nil {
		t.Fatalf("state 2: old-CA client refused during overlap: %v", err)
	}

	// State 3: old CA withdrawn.
	writeBundle(newCA)
	eventually(t, 10*time.Second, "state 3: old-CA client refused after withdrawal", refuses(oldClient))
	if err := accepts(newClient)(); err != nil {
		t.Fatalf("state 3: new-CA client refused: %v", err)
	}
}
