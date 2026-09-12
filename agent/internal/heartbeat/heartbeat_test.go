// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package heartbeat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeClient struct {
	mu    sync.Mutex
	reqs  []*nexusv1.HeartbeatRequest
	resp  *nexusv1.HeartbeatResponse
	err   error
	block chan struct{}
	calls int
}

func (f *fakeClient) Heartbeat(ctx context.Context, req *nexusv1.HeartbeatRequest) (*nexusv1.HeartbeatResponse, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.calls++
	block, err, resp := f.block, f.err, f.resp
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	if resp == nil {
		resp = &nexusv1.HeartbeatResponse{}
	}
	return resp, nil
}

func (f *fakeClient) requests() []*nexusv1.HeartbeatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*nexusv1.HeartbeatRequest(nil), f.reqs...)
}

func (f *fakeClient) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeClient) setResp(r *nexusv1.HeartbeatResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resp = r
}

// fakeState is a snapshot source whose read can be made to BLOCK, which is how we
// simulate a wedged task holding the executor's state.
type fakeState struct {
	mu      sync.Mutex
	running []string
	free    *nexusv1.ResourceVector
	block   chan struct{}
}

func (f *fakeState) RunningAttemptIDs() []string {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.running...)
}
func (f *fakeState) FreeCapacity() *nexusv1.ResourceVector {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.free == nil {
		return &nexusv1.ResourceVector{}
	}
	return f.free
}
func (f *fakeState) Utilization() (float64, float64) { return 0.25, 0.5 }

type fakeHandler struct {
	mu        sync.Mutex
	drains    int
	cancels   []string
	graces    []time.Duration
	reregs    int
	blockOn   chan struct{}
	blockKind nexusv1.DirectiveKind
}

func (f *fakeHandler) OnDrain(ctx context.Context) {
	if f.blockOn != nil && f.blockKind == nexusv1.DirectiveKind_DIRECTIVE_KIND_DRAIN {
		<-f.blockOn
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drains++
}
func (f *fakeHandler) OnCancelAttempt(ctx context.Context, id string, grace time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, id)
	f.graces = append(f.graces, grace)
}
func (f *fakeHandler) OnReregister(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reregs++
}
func (f *fakeHandler) snapshot() (int, []string, []time.Duration, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drains, append([]string(nil), f.cancels...), append([]time.Duration(nil), f.graces...), f.reregs
}

// TestRunningAttemptIDsAreReportedEveryBeat: the reported list is the GROUND TRUTH
// the control plane reconciles reservations against. If it were ever wrong or
// omitted, the leak detector would either strand capacity or kill live work.
func TestRunningAttemptIDsAreReportedEveryBeat(t *testing.T) {
	c := &fakeClient{}
	st := &fakeState{running: []string{"a1", "a2"}, free: &nexusv1.ResourceVector{CpuMillis: 500}}
	l := New("w-1", c, st, &fakeHandler{}, 20*time.Millisecond, testLog())

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = l.Run(ctx)

	reqs := c.requests()
	if len(reqs) < 3 {
		t.Fatalf("expected several beats, got %d", len(reqs))
	}
	for i, r := range reqs {
		if r.GetWorkerId() != "w-1" {
			t.Errorf("beat %d: worker_id = %q", i, r.GetWorkerId())
		}
		got := r.GetRunningAttemptIds()
		if len(got) != 2 || got[0] != "a1" || got[1] != "a2" {
			t.Errorf("beat %d: running_attempt_ids = %v, want [a1 a2] as ground truth", i, got)
		}
		if r.GetFreeCapacity().GetCpuMillis() != 500 {
			t.Errorf("beat %d: free capacity not reported", i)
		}
		if r.GetCpuUtilization() != 0.25 || r.GetMemoryUtilization() != 0.5 {
			t.Errorf("beat %d: utilization = %v/%v", i, r.GetCpuUtilization(), r.GetMemoryUtilization())
		}
	}
}

// TestBeatsImmediatelyOnStart: the gap between registration and the first beat is
// a window in which the control plane holds a worker it has never heard from.
func TestBeatsImmediatelyOnStart(t *testing.T) {
	c := &fakeClient{}
	l := New("w-1", c, &fakeState{}, &fakeHandler{}, time.Hour, testLog())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = l.Run(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.requests()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if len(c.requests()) == 0 {
		t.Fatal("no beat was sent before the first tick of an hour-long interval")
	}
}

// TestServerControlsInterval: next_heartbeat_ms lets the control plane shed load
// by slowing the fleet instead of being overwhelmed by it.
func TestServerControlsInterval(t *testing.T) {
	c := &fakeClient{resp: &nexusv1.HeartbeatResponse{NextHeartbeatMs: 250}}
	l := New("w-1", c, &fakeState{}, &fakeHandler{}, 10*time.Millisecond, testLog())

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = l.Run(ctx)

	if got := l.Interval(); got != 250*time.Millisecond {
		t.Errorf("interval = %v, want 250ms as instructed by the control plane", got)
	}
	// With the interval stretched to 250ms inside a 200ms window, only the
	// immediate beat should have fired.
	if n := len(c.requests()); n > 2 {
		t.Errorf("sent %d beats; the server-supplied interval was not honoured", n)
	}
}

// TestHeartbeatFailureIsNotFatal: the control plane is entitled to be briefly
// unreachable. Tearing down on one failed beat would turn a load-balancer restart
// into a fleet-wide outage.
func TestHeartbeatFailureIsNotFatal(t *testing.T) {
	c := &fakeClient{err: errors.New("connection refused")}
	l := New("w-1", c, &fakeState{}, &fakeHandler{}, 10*time.Millisecond, testLog())

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if err := l.Run(ctx); err != nil {
		t.Fatalf("Run returned an error on heartbeat failure: %v", err)
	}
	if l.ConsecutiveFailures() < 2 {
		t.Errorf("consecutive failures = %d; the loop should have kept trying", l.ConsecutiveFailures())
	}

	// And it must RECOVER without intervention.
	c.setErr(nil)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel2()
	_ = l.Run(ctx2)
	if l.ConsecutiveFailures() != 0 {
		t.Errorf("failure streak = %d after recovery, want 0", l.ConsecutiveFailures())
	}
}

// TestRPCTimeoutIsShorterThanInterval is the structural guarantee against a false
// death declaration. A per-beat RPC that could outlast the beat period would let
// one slow control-plane response stall the NEXT beat, which is precisely how a
// live worker gets declared dead and its work duplicated.
func TestRPCTimeoutIsShorterThanInterval(t *testing.T) {
	if RPCTimeout >= DefaultInterval {
		t.Fatalf("RPCTimeout (%v) must be well under DefaultInterval (%v): otherwise one slow "+
			"response stalls the next beat and the control plane wrongly declares the worker dead",
			RPCTimeout, DefaultInterval)
	}
}

// TestSlowRPCDoesNotStallSubsequentBeats: a hung control plane must not be able to
// wedge the loop; the per-beat timeout must cut it loose.
func TestSlowRPCDoesNotStallSubsequentBeats(t *testing.T) {
	block := make(chan struct{})
	c := &fakeClient{block: block}
	l := New("w-1", c, &fakeState{}, &fakeHandler{}, 20*time.Millisecond, testLog())

	ctx, cancel := context.WithTimeout(context.Background(), RPCTimeout+2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = l.Run(ctx); close(done) }()

	// After the RPC timeout elapses the first beat is abandoned and the loop must
	// beat again rather than remaining blocked forever.
	time.Sleep(RPCTimeout + 500*time.Millisecond)
	n := len(c.requests())
	close(block)
	cancel()
	<-done

	if n < 2 {
		t.Errorf("only %d beats in %v against a hung server; the RPC timeout did not release the loop",
			n, RPCTimeout+500*time.Millisecond)
	}
}

// TestDirectivesAreApplied covers all three directive kinds riding the heartbeat
// RESPONSE, which is what keeps the design fully pull-based (ADR-0005).
func TestDirectivesAreApplied(t *testing.T) {
	h := &fakeHandler{}
	c := &fakeClient{resp: &nexusv1.HeartbeatResponse{
		Directives: []*nexusv1.Directive{
			{Kind: nexusv1.DirectiveKind_DIRECTIVE_KIND_DRAIN},
			{Kind: nexusv1.DirectiveKind_DIRECTIVE_KIND_CANCEL_ATTEMPT, AttemptId: "a-99", GraceMs: 7000},
			{Kind: nexusv1.DirectiveKind_DIRECTIVE_KIND_REREGISTER},
		},
	}}
	l := New("w-1", c, &fakeState{}, h, time.Hour, testLog())

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = l.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if d, cs, _, r := h.snapshot(); d > 0 && len(cs) > 0 && r > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	drains, cancels, graces, reregs := h.snapshot()
	if drains != 1 {
		t.Errorf("drains = %d, want 1", drains)
	}
	if len(cancels) != 1 || cancels[0] != "a-99" {
		t.Errorf("cancels = %v, want [a-99]", cancels)
	}
	if len(graces) != 1 || graces[0] != 7*time.Second {
		t.Errorf("grace = %v, want 7s from grace_ms", graces)
	}
	if reregs != 1 {
		t.Errorf("reregisters = %d, want 1", reregs)
	}
}

// TestUnknownDirectiveIsIgnored: a rolling upgrade adds directives before every
// agent understands them. An agent that died on the unknown would turn a routine
// deploy into a fleet outage.
func TestUnknownDirectiveIsIgnored(t *testing.T) {
	h := &fakeHandler{}
	c := &fakeClient{resp: &nexusv1.HeartbeatResponse{
		Directives: []*nexusv1.Directive{{Kind: nexusv1.DirectiveKind(9999)}},
	}}
	l := New("w-1", c, &fakeState{}, h, 10*time.Millisecond, testLog())

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := l.Run(ctx); err != nil {
		t.Fatalf("an unknown directive must not be fatal: %v", err)
	}
	if d, cs, _, r := h.snapshot(); d != 0 || len(cs) != 0 || r != 0 {
		t.Errorf("an unknown directive triggered a handler: drains=%d cancels=%v reregs=%d", d, cs, r)
	}
}

// TestCancelDirectiveWithoutAttemptIDIsIgnored: a malformed directive must not
// cancel something arbitrary.
func TestCancelDirectiveWithoutAttemptIDIsIgnored(t *testing.T) {
	h := &fakeHandler{}
	c := &fakeClient{resp: &nexusv1.HeartbeatResponse{
		Directives: []*nexusv1.Directive{
			{Kind: nexusv1.DirectiveKind_DIRECTIVE_KIND_CANCEL_ATTEMPT, AttemptId: ""},
		},
	}}
	l := New("w-1", c, &fakeState{}, h, 10*time.Millisecond, testLog())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = l.Run(ctx)

	if _, cs, _, _ := h.snapshot(); len(cs) != 0 {
		t.Errorf("cancelled %v for a directive with no attempt_id", cs)
	}
}

// TestLastSuccessTracksAcks: the agent uses this to decide it has been partitioned
// long enough that its leases must have expired.
func TestLastSuccessTracksAcks(t *testing.T) {
	c := &fakeClient{}
	l := New("w-1", c, &fakeState{}, &fakeHandler{}, 10*time.Millisecond, testLog())
	before := l.LastSuccess()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_ = l.Run(ctx)

	if !l.LastSuccess().After(before) {
		t.Error("LastSuccess did not advance after successful beats")
	}
}
