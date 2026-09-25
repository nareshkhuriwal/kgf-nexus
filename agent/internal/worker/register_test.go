// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/agentmetrics"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics/metricstest"
)

// Registration against a control plane that is not ready yet, over a real
// gRPC connection to a fake WorkerService, so the error codes the agent sees
// are the ones the transport really produces.

// scriptedGateway answers Register with the queued errors, in order, and then
// succeeds (or keeps failing with the last error when sticky is set).
type scriptedGateway struct {
	nexusv1.UnimplementedWorkerServiceServer

	mu     sync.Mutex
	script []error
	sticky bool
	calls  int
	keys   []string
	called chan struct{}
}

func (g *scriptedGateway) Register(ctx context.Context, req *nexusv1.RegisterRequest) (*nexusv1.RegisterResponse, error) {
	g.mu.Lock()
	g.calls++
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		g.keys = append(g.keys, strings.Join(md.Get("x-api-key"), ","))
	}
	var err error
	switch {
	case len(g.script) > 1 || (len(g.script) == 1 && !g.sticky):
		err, g.script = g.script[0], g.script[1:]
	case len(g.script) == 1:
		err = g.script[0]
	}
	g.mu.Unlock()
	if g.called != nil {
		select {
		case g.called <- struct{}{}:
		default:
		}
	}
	if err != nil {
		return nil, err
	}
	return &nexusv1.RegisterResponse{
		WorkerId: "w-1", SessionToken: "sess", HeartbeatIntervalMs: 5000, LeaseTtlMs: 30000,
	}, nil
}

func (g *scriptedGateway) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// dialFake serves g on an in-memory listener and returns a real GRPCClient
// connected to it.
func dialFake(t *testing.T, g *scriptedGateway) *GRPCClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	nexusv1.RegisterWorkerServiceServer(srv, g)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	c, err := DialControlPlane(context.Background(), "passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.WithAPIKey("bootstrap-key")
}

// fastBackoff keeps retry tests in milliseconds.
var fastBackoff = RegisterBackoff{Initial: time.Millisecond, Max: 4 * time.Millisecond, PerAttemptTimeout: 2 * time.Second}

func testAgent(t *testing.T, c Client, b RegisterBackoff) (*Agent, *metrics.Registry) {
	t.Helper()
	reg := metrics.NewServiceRegistry("agent", "test")
	cfg := Config{
		Namespace: "ns", Hostname: "h", Queues: []string{"default"},
		Capacity:        &nexusv1.ResourceVector{CpuMillis: 1000, MemoryBytes: 1 << 30},
		WALDir:          t.TempDir(),
		WorkDir:         t.TempDir(),
		Metrics:         agentmetrics.New(reg),
		Readiness:       &Readiness{},
		RegisterBackoff: b,
	}
	a, err := New(cfg, c, testLog())
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a, reg
}

func registrationCount(t *testing.T, reg *metrics.Registry, result string) float64 {
	t.Helper()
	fams := metricstest.Scrape(t, metrics.OpsMux(reg), "/metrics")
	s := metricstest.Find(fams, "nexus_agent_registration_attempts_total", map[string]string{"result": result})
	if s == nil {
		return 0
	}
	return s.GetCounter().GetValue()
}

var registerReq = &nexusv1.RegisterRequest{Namespace: "ns", Hostname: "h", Queues: []string{"default"}}

// TestRegisterRetriesUntilTheGatewayIsReady is the reported crash: the gateway
// answered "authentication temporarily unavailable" while it started, and the
// agent exited. It must wait it out and register.
func TestRegisterRetriesUntilTheGatewayIsReady(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "authentication temporarily unavailable")
	g := &scriptedGateway{script: []error{unavailable, unavailable, unavailable}}
	a, reg := testAgent(t, dialFake(t, g), fastBackoff)

	resp, err := a.registerWithRetry(context.Background(), registerReq)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if resp.GetWorkerId() != "w-1" {
		t.Fatalf("worker id = %q", resp.GetWorkerId())
	}
	if got := g.callCount(); got != 4 {
		t.Errorf("Register calls = %d, want 4 (3 retried + 1 success)", got)
	}
	for i, k := range g.keys {
		if k != "bootstrap-key" {
			t.Errorf("call %d carried api key %q; every retry must present the bootstrap credential", i, k)
		}
	}
	if got := registrationCount(t, reg, "retryable_error"); got != 3 {
		t.Errorf("registration_attempts{retryable_error} = %v, want 3", got)
	}
	if got := registrationCount(t, reg, "ok"); got != 1 {
		t.Errorf("registration_attempts{ok} = %v, want 1", got)
	}
}

// TestRegisterRetriesEveryTransientCode covers each code classified as
// transient, over the real transport.
func TestRegisterRetriesEveryTransientCode(t *testing.T) {
	for _, c := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded,
		codes.ResourceExhausted, codes.Aborted, codes.Internal} {
		t.Run(c.String(), func(t *testing.T) {
			g := &scriptedGateway{script: []error{status.Error(c, "not yet")}}
			a, _ := testAgent(t, dialFake(t, g), fastBackoff)
			if _, err := a.registerWithRetry(context.Background(), registerReq); err != nil {
				t.Fatalf("register after one %s: %v", c, err)
			}
			if got := g.callCount(); got != 2 {
				t.Errorf("Register calls = %d, want 2", got)
			}
		})
	}
}

// TestRegisterFailsFastOnPermanentErrors: a wrong key or a bad request is not
// waited out. One call, a typed error, and a message that names the fix.
func TestRegisterFailsFastOnPermanentErrors(t *testing.T) {
	for _, tc := range []struct {
		code codes.Code
		hint string
	}{
		{codes.Unauthenticated, "NEXUS_API_KEY"},
		{codes.PermissionDenied, "worker-role key"},
		{codes.InvalidArgument, "rejected the registration request"},
		{codes.FailedPrecondition, "retrying cannot fix"},
		{codes.Unimplemented, "NEXUS_CONTROL_PLANE"},
	} {
		t.Run(tc.code.String(), func(t *testing.T) {
			g := &scriptedGateway{script: []error{status.Error(tc.code, "no")}, sticky: true}
			a, reg := testAgent(t, dialFake(t, g), fastBackoff)
			_, err := a.registerWithRetry(context.Background(), registerReq)
			if !errors.Is(err, ErrRegistrationRejected) {
				t.Fatalf("err = %v, want ErrRegistrationRejected", err)
			}
			if !strings.Contains(err.Error(), tc.hint) {
				t.Errorf("message %q does not name the fix (%q)", err, tc.hint)
			}
			if status.Code(err) != tc.code {
				t.Errorf("status code lost from the error chain: %s", status.Code(err))
			}
			if got := g.callCount(); got != 1 {
				t.Errorf("Register calls = %d, want 1: a permanent error must not be retried", got)
			}
			if got := registrationCount(t, reg, "permanent_error"); got != 1 {
				t.Errorf("registration_attempts{permanent_error} = %v, want 1", got)
			}
		})
	}
}

// TestRegisterStopsOnShutdownDuringBackoff: SIGTERM while waiting for the
// gateway must end the wait at once, not after the backoff.
func TestRegisterStopsOnShutdownDuringBackoff(t *testing.T) {
	g := &scriptedGateway{
		script: []error{status.Error(codes.Unavailable, "down")}, sticky: true,
		called: make(chan struct{}, 1),
	}
	a, _ := testAgent(t, dialFake(t, g), RegisterBackoff{Initial: time.Minute, Max: time.Minute})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.registerWithRetry(ctx, registerReq)
		done <- err
	}()
	<-g.called
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("took %s to stop after cancellation", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("registration kept waiting after shutdown")
	}
}

// TestRunExitsCleanlyAndStaysNotReadyWhenStoppedBeforeRegistering is the
// process-level view of the same case: Run returns nil (exit 0, not a crash)
// and the agent never reported ready.
func TestRunExitsCleanlyAndStaysNotReadyWhenStoppedBeforeRegistering(t *testing.T) {
	g := &scriptedGateway{
		script: []error{status.Error(codes.Unavailable, "down")}, sticky: true,
		called: make(chan struct{}, 1),
	}
	a, reg := testAgent(t, dialFake(t, g), RegisterBackoff{Initial: time.Minute, Max: time.Minute})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	<-g.called
	if a.cfg.Readiness.Ready() {
		t.Error("ready before registration")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil on shutdown during registration", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
	if a.cfg.Readiness.Ready() {
		t.Error("ready after a registration that never succeeded")
	}
	fams := metricstest.Scrape(t, metrics.OpsMux(reg), "/metrics")
	if s := metricstest.Find(fams, "nexus_agent_registered", nil); s == nil || s.GetGauge().GetValue() != 0 {
		t.Errorf("registered gauge = %v, want 0", s)
	}
}

// TestRunReturnsPermanentRegistrationError: a permanent failure still ends
// the process, with the typed error main reports.
func TestRunReturnsPermanentRegistrationError(t *testing.T) {
	g := &scriptedGateway{script: []error{status.Error(codes.Unauthenticated, "bad key")}, sticky: true}
	a, _ := testAgent(t, dialFake(t, g), fastBackoff)
	err := a.Run(context.Background())
	if !errors.Is(err, ErrRegistrationRejected) {
		t.Fatalf("Run = %v, want ErrRegistrationRejected", err)
	}
	if a.cfg.Readiness.Ready() {
		t.Error("ready after a rejected registration")
	}
}

// TestRegistrationRetryableClassification pins the table, including errors
// that carry no gRPC status at all.
func TestRegistrationRetryableClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{status.Error(codes.Unavailable, ""), true},
		{status.Error(codes.DeadlineExceeded, ""), true},
		{status.Error(codes.ResourceExhausted, ""), true},
		{status.Error(codes.Aborted, ""), true},
		{status.Error(codes.Internal, ""), true},
		{status.Error(codes.Unauthenticated, ""), false},
		{status.Error(codes.PermissionDenied, ""), false},
		{status.Error(codes.InvalidArgument, ""), false},
		{status.Error(codes.Canceled, ""), false},
		{errors.New("plain"), false},
		{nil, false},
	} {
		if got := RegistrationRetryable(tc.err); got != tc.want {
			t.Errorf("RegistrationRetryable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestRegisterBackoffIsCappedExponential: the ceiling doubles from Initial and
// stops at Max; the zero value takes the defaults.
func TestRegisterBackoffIsCappedExponential(t *testing.T) {
	b := RegisterBackoff{}.withDefaults()
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if got := b.ceiling(i + 1); got != w {
			t.Errorf("ceiling(%d) = %s, want %s", i+1, got, w)
		}
	}
	if got := b.ceiling(1000); got != 30*time.Second {
		t.Errorf("ceiling(1000) = %s, want the 30s cap", got)
	}
	if b := (RegisterBackoff{Initial: time.Minute, Max: time.Second}).withDefaults(); b.Max != time.Minute {
		t.Errorf("Max below Initial = %s, want raised to Initial", b.Max)
	}
}
