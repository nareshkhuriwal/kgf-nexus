package lease

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeClient struct {
	mu       sync.Mutex
	requests []*nexusv1.ExtendLeaseRequest
	resp     *nexusv1.ExtendLeaseResponse
	err      error
}

func (f *fakeClient) ExtendLease(ctx context.Context, req *nexusv1.ExtendLeaseRequest) (*nexusv1.ExtendLeaseResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	resp, err := f.resp, f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if resp == nil {
		// Default: grant every renewal, revoke nothing.
		grants := make([]*nexusv1.LeaseGrant, 0, len(req.GetRenewals()))
		for _, r := range req.GetRenewals() {
			grants = append(grants, &nexusv1.LeaseGrant{
				AttemptId:       r.GetAttemptId(),
				ExpiresAtUnixMs: time.Now().Add(30 * time.Second).UnixMilli(),
				Epoch:           1,
			})
		}
		return &nexusv1.ExtendLeaseResponse{Grants: grants}, nil
	}
	return resp, nil
}

func (f *fakeClient) reqs() []*nexusv1.ExtendLeaseRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*nexusv1.ExtendLeaseRequest(nil), f.requests...)
}

func (f *fakeClient) set(resp *nexusv1.ExtendLeaseResponse, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resp, f.err = resp, err
}

type fakeRevoker struct {
	mu      sync.Mutex
	revoked []string
	reasons []string
	// block makes the handler slow, to prove the keeper does not hold its lock
	// across the teardown.
	block chan struct{}
}

func (f *fakeRevoker) OnLeaseRevoked(attemptID, reason string) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, attemptID)
	f.reasons = append(f.reasons, reason)
}

func (f *fakeRevoker) list() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.revoked...)
	sort.Strings(out)
	return out
}

// TestRenewalIsBATCHED is the ADR-0010 requirement. At 10k concurrent tasks,
// per-attempt renewal is thousands of RPS of pure bookkeeping against the control
// plane's hottest table; one call per WORKER collapses that to the worker count.
func TestRenewalIsBatched(t *testing.T) {
	c := &fakeClient{}
	k := New("w-1", c, &fakeRevoker{}, 300*time.Millisecond, testLog())
	for _, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		k.Track(id, "tok-"+id, time.Now().Add(30*time.Second))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_ = k.Run(ctx)

	reqs := c.reqs()
	if len(reqs) == 0 {
		t.Fatal("no renewal was sent")
	}
	for i, r := range reqs {
		if r.GetWorkerId() != "w-1" {
			t.Errorf("call %d: worker_id = %q", i, r.GetWorkerId())
		}
		if got := len(r.GetRenewals()); got != 5 {
			t.Fatalf("call %d carried %d renewals, want ALL 5 in ONE call — per-attempt "+
				"renewal is exactly the overhead ADR-0010 exists to avoid", i, got)
		}
		seen := map[string]string{}
		for _, rn := range r.GetRenewals() {
			seen[rn.GetAttemptId()] = rn.GetLeaseToken()
		}
		for _, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
			if seen[id] != "tok-"+id {
				t.Errorf("renewal for %s carried token %q, want %q", id, seen[id], "tok-"+id)
			}
		}
	}
}

// TestRenewIntervalLeavesRetryBudget: renewing at one third of the TTL leaves room
// for TWO failed renewals before the lease actually expires. At one half a single
// dropped packet leaves no budget, and an expired lease means the attempt becomes
// LOST and is re-executed elsewhere while this worker is still running it.
func TestRenewIntervalLeavesRetryBudget(t *testing.T) {
	ttl := 30 * time.Second
	k := New("w-1", &fakeClient{}, &fakeRevoker{}, ttl, testLog())
	got := k.RenewInterval()
	if got != ttl/3 {
		t.Fatalf("renew interval = %v, want ttl/3 = %v", got, ttl/3)
	}
	if ttl/got < 3 {
		t.Errorf("only %d renewal attempts fit inside the TTL; need at least 3", ttl/got)
	}
}

// TestIdleWorkerSendsNothing: leases are not worker liveness, and an idle worker
// emitting empty renewals would be pure load on the control plane.
func TestIdleWorkerSendsNothing(t *testing.T) {
	c := &fakeClient{}
	k := New("w-1", c, &fakeRevoker{}, 60*time.Millisecond, testLog())

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = k.Run(ctx)

	if n := len(c.reqs()); n != 0 {
		t.Errorf("an idle keeper sent %d renewal calls, want 0", n)
	}
}

// TestRevokedAttemptStopsWorkImmediately is the critical behaviour: a revoked
// lease means the control plane has already given the attempt to someone else (or
// cancelled it). Any result we produce will be REJECTED on token mismatch, so
// continuing burns capacity for an outcome guaranteed to be discarded.
func TestRevokedAttemptStopsWorkImmediately(t *testing.T) {
	c := &fakeClient{}
	rev := &fakeRevoker{}
	k := New("w-1", c, rev, 60*time.Millisecond, testLog())
	k.Track("a1", "tok-a1", time.Now().Add(time.Minute))
	k.Track("a2", "tok-a2", time.Now().Add(time.Minute))
	k.Track("a3", "tok-a3", time.Now().Add(time.Minute))

	c.set(&nexusv1.ExtendLeaseResponse{
		Grants: []*nexusv1.LeaseGrant{
			{AttemptId: "a1", ExpiresAtUnixMs: time.Now().Add(time.Minute).UnixMilli(), Epoch: 2},
		},
		RevokedAttemptIds: []string{"a2", "a3"},
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = k.Run(ctx)

	got := rev.list()
	if len(got) != 2 || got[0] != "a2" || got[1] != "a3" {
		t.Fatalf("revoked = %v, want [a2 a3]", got)
	}
	// A revoked attempt must also stop being renewed: continuing to renew a lease
	// the control plane has reclaimed is both useless and misleading.
	if _, ok := k.Token("a2"); ok {
		t.Error("a2 is still tracked after revocation")
	}
	if _, ok := k.Token("a1"); !ok {
		t.Error("a1 was dropped despite being granted")
	}
	if k.Count() != 1 {
		t.Errorf("tracked count = %d, want 1 (only a1 survives)", k.Count())
	}
}

// TestRevocationHandlerIsCalledOutsideTheLock: the handler tears down a workload,
// and holding the keeper's lock across that would block renewal of every surviving
// attempt — so one revoked task would cascade into the rest becoming LOST.
func TestRevocationHandlerIsCalledOutsideTheLock(t *testing.T) {
	c := &fakeClient{}
	block := make(chan struct{})
	rev := &fakeRevoker{block: block}
	k := New("w-1", c, rev, time.Hour, testLog())
	k.Track("victim", "tok", time.Now().Add(time.Hour))
	k.Track("survivor", "tok2", time.Now().Add(time.Hour))

	c.set(&nexusv1.ExtendLeaseResponse{RevokedAttemptIds: []string{"victim"}}, nil)

	// Trigger one renewal pass in the background; the handler will block.
	go k.renewOnce(context.Background())

	// While the handler is blocked, the keeper's own methods must remain usable.
	// If the lock were held across the handler, these would deadlock and the test
	// would time out.
	done := make(chan struct{})
	go func() {
		_ = k.Count()
		_, _ = k.Token("survivor")
		k.Track("newcomer", "tok3", time.Now().Add(time.Hour))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		close(block)
		t.Fatal("keeper methods blocked while a revocation handler was running: the lock is held across teardown")
	}
	close(block)
}

// TestRenewalFailureDoesNotAbandonWork: the control plane will expire the lease on
// its own schedule if we truly cannot reach it. Abandoning after one failure would
// throw away work a successful retry two seconds later would have kept.
func TestRenewalFailureDoesNotAbandonWork(t *testing.T) {
	c := &fakeClient{}
	rev := &fakeRevoker{}
	k := New("w-1", c, rev, 60*time.Millisecond, testLog())
	// Expiry well in the future, so the partition heuristic must NOT fire.
	k.Track("a1", "tok-a1", time.Now().Add(time.Hour))
	c.set(nil, errors.New("connection refused"))

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_ = k.Run(ctx)

	if got := rev.list(); len(got) != 0 {
		t.Errorf("abandoned %v after a transient renewal failure; the lease had not expired", got)
	}
	if k.Count() != 1 {
		t.Errorf("tracked count = %d, want 1: the attempt must still be tracked", k.Count())
	}
}

// TestAbandonsLocallyExpiredWhenUnreachable is the partition case. Once our own
// last-known expiry (which came FROM the control plane) is in the past, the lease
// has certainly expired and the attempt has very likely been re-dispatched.
// Continuing is pure waste and keeps a local reservation that makes this worker
// look busier than it is.
func TestAbandonsLocallyExpiredWhenUnreachable(t *testing.T) {
	c := &fakeClient{}
	rev := &fakeRevoker{}
	k := New("w-1", c, rev, 50*time.Millisecond, testLog())
	// Already expired, per the control plane's own timestamp.
	k.Track("stale", "tok", time.Now().Add(-time.Second))
	k.Track("fresh", "tok2", time.Now().Add(time.Hour))
	c.set(nil, errors.New("i/o timeout"))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = k.Run(ctx)

	got := rev.list()
	if len(got) != 1 || got[0] != "stale" {
		t.Fatalf("abandoned %v, want only [stale]", got)
	}
	if _, ok := k.Token("fresh"); !ok {
		t.Error("an unexpired attempt was abandoned during a partition")
	}
	for _, r := range rev.reasons {
		if r != "lease_expired_while_unreachable" {
			t.Errorf("reason = %q, want lease_expired_while_unreachable", r)
		}
	}
}

// TestGrantUpdatesExpiry: a successful renewal must push the recorded expiry
// forward, or the partition heuristic above would eventually abandon healthy work.
func TestGrantUpdatesExpiry(t *testing.T) {
	c := &fakeClient{}
	rev := &fakeRevoker{}
	k := New("w-1", c, rev, 40*time.Millisecond, testLog())
	k.Track("a1", "tok", time.Now().Add(80*time.Millisecond))

	// Grant a far-future expiry on every renewal.
	c.set(nil, nil) // the fake's default grants +30s

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_ = k.Run(ctx)

	// The original 80ms expiry has long passed; if renewals did not advance it,
	// a later failure would have abandoned the attempt.
	c.set(nil, errors.New("now unreachable"))
	k.renewOnce(context.Background())
	if got := rev.list(); len(got) != 0 {
		t.Errorf("abandoned %v: the granted expiry was not recorded", got)
	}
}

// TestUntrackStopsRenewal: continuing to renew a finished attempt's lease would
// hold its reservation and leak the worker's capacity until reconciliation noticed.
func TestUntrackStopsRenewal(t *testing.T) {
	c := &fakeClient{}
	k := New("w-1", c, &fakeRevoker{}, 40*time.Millisecond, testLog())
	k.Track("a1", "tok", time.Now().Add(time.Hour))
	k.Untrack("a1")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = k.Run(ctx)

	if n := len(c.reqs()); n != 0 {
		t.Errorf("sent %d renewals for an untracked attempt", n)
	}
	if _, ok := k.Token("a1"); ok {
		t.Error("a1 is still tracked after Untrack")
	}
}

// TestRevokeIsIdempotent: a revocation can arrive twice (two renewal passes, or a
// revocation racing local completion). The handler must not be invoked twice for
// the same attempt, or teardown would run against a handle that is already gone.
func TestRevokeIsIdempotent(t *testing.T) {
	rev := &fakeRevoker{}
	k := New("w-1", &fakeClient{}, rev, time.Hour, testLog())
	k.Track("a1", "tok", time.Now().Add(time.Hour))

	k.revoke("a1", "first")
	k.revoke("a1", "second")

	if got := rev.list(); len(got) != 1 {
		t.Errorf("handler called %d times for one attempt, want 1", len(got))
	}
}

// TestSetHandlerWiresRevocation covers the post-construction wiring the agent uses
// (the keeper revokes into the runner while the runner renews through the keeper).
func TestSetHandlerWiresRevocation(t *testing.T) {
	rev := &fakeRevoker{}
	k := New("w-1", &fakeClient{}, nil, time.Hour, testLog())
	k.Track("a1", "tok", time.Now().Add(time.Hour))

	// With no handler, revoking must not panic.
	k.revoke("a1", "no-handler")

	k.Track("a2", "tok2", time.Now().Add(time.Hour))
	k.SetHandler(rev)
	k.revoke("a2", "wired")
	if got := rev.list(); len(got) != 1 || got[0] != "a2" {
		t.Errorf("revoked = %v, want [a2]", got)
	}
}

// TestStopIsSafeToCallRepeatedly guards the shutdown path.
func TestStopIsSafeToCallRepeatedly(t *testing.T) {
	k := New("w-1", &fakeClient{}, &fakeRevoker{}, time.Hour, testLog())
	done := make(chan struct{})
	go func() { _ = k.Run(context.Background()); close(done) }()
	k.Stop()
	k.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
}
