// Package lease keeps this worker's ownership of its running attempts alive.
//
// A lease is a FENCING token (ADR-0010), not a lock. It cannot stop a
// partitioned worker from executing — nothing can, over a network — but it
// guarantees that a partitioned worker's result cannot corrupt control-plane
// state, because a stale token is rejected. The worker's job here is to renew
// well inside the TTL, and to STOP IMMEDIATELY when told its lease is gone.
package lease

import (
	"context"
	"log/slog"
	"sync"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Client is the batched renewal RPC.
type Client interface {
	ExtendLease(ctx context.Context, req *nexusv1.ExtendLeaseRequest) (*nexusv1.ExtendLeaseResponse, error)
}

// RevocationHandler is called for every attempt whose lease is gone.
//
// It MUST return promptly and MUST NOT wait for the workload to die: the
// teardown itself is asynchronous. Blocking here would stall renewal for every
// OTHER attempt this worker holds, so one revoked task would cascade into the
// rest of the worker's work becoming LOST.
type RevocationHandler interface {
	OnLeaseRevoked(attemptID, reason string)
}

// Keeper renews leases for every attempt this worker holds.
type Keeper struct {
	client   Client
	handler  RevocationHandler
	log      *slog.Logger
	workerID string

	// renewInterval is derived from the TTL, not configured independently: the
	// only interval that matters is one that leaves room for a retry before the
	// lease actually expires.
	renewInterval time.Duration
	ttl           time.Duration

	mu    sync.Mutex
	held  map[string]*held
	stopC chan struct{}
	once  sync.Once
}

type held struct {
	token     string
	expiresAt time.Time
	epoch     int32
}

// RenewFraction is the share of the TTL after which renewal is attempted.
//
// One third, not one half: it gives room for TWO failed renewals before the
// lease actually expires. At one half a single dropped packet leaves no retry
// budget, and an expired lease means the attempt becomes LOST and is re-executed
// somewhere else while this worker is still happily running it — the exact
// duplicate-work scenario leases exist to bound.
const RenewFraction = 3

// New builds a keeper. ttl comes from RegisterResponse.lease_ttl_ms: the control
// plane owns the TTL because deadlines use control-plane time, never worker
// clocks (invariant 7).
func New(workerID string, c Client, h RevocationHandler, ttl time.Duration, log *slog.Logger) *Keeper {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	// Derived strictly from the TTL, with no absolute floor. An absolute floor
	// (say 1s) would silently break the invariant this constant encodes: for a
	// TTL shorter than RenewFraction seconds, the floor would exceed ttl/3 and
	// leave no retry budget at all — the exact failure the fraction exists to
	// prevent. A short TTL is the control plane's choice, and honouring it with a
	// proportionally short renewal is more correct than clamping it.
	interval := ttl / RenewFraction
	if interval <= 0 {
		interval = time.Millisecond
	}
	return &Keeper{
		client: c, handler: h, workerID: workerID,
		log:           log.With("worker_id", workerID),
		ttl:           ttl,
		renewInterval: interval,
		held:          make(map[string]*held),
		stopC:         make(chan struct{}),
	}
}

// RenewInterval reports the derived renewal period, for tests and logs.
func (k *Keeper) RenewInterval() time.Duration { return k.renewInterval }

// Track begins renewing an attempt's lease. Called immediately on receipt of an
// assignment, BEFORE execution starts: an assignment we hold but do not renew
// expires under us while we are still working on it.
func (k *Keeper) Track(attemptID, token string, expiresAt time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.held[attemptID] = &held{token: token, expiresAt: expiresAt}
	k.log.Debug("tracking lease", "attempt_id", attemptID, "expires_at", expiresAt)
}

// Untrack stops renewing, called when an attempt reaches a terminal state.
// Continuing to renew a finished attempt's lease would hold its reservation and
// leak the worker's capacity until the control plane's reconciliation noticed.
func (k *Keeper) Untrack(attemptID string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.held, attemptID)
}

// SetHandler wires the revocation handler after construction. The keeper and
// the runner each need a reference to the other, and a setter is cleaner than
// building one of them twice.
func (k *Keeper) SetHandler(h RevocationHandler) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.handler = h
}

// Token returns the current (possibly renewed) token for an attempt. Callers
// must re-read it before every attempt RPC rather than caching the one from the
// assignment, because a renewal can bump the epoch.
func (k *Keeper) Token(attemptID string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	h, ok := k.held[attemptID]
	if !ok {
		return "", false
	}
	return h.token, true
}

// Count reports how many leases are being renewed.
func (k *Keeper) Count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.held)
}

// Run renews on its own goroutine until ctx is cancelled.
func (k *Keeper) Run(ctx context.Context) error {
	t := time.NewTicker(k.renewInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			k.log.Info("lease keeper stopping", "reason", ctx.Err())
			return nil
		case <-k.stopC:
			return nil
		case <-t.C:
			k.renewOnce(ctx)
		}
	}
}

// Stop ends the loop. Safe to call repeatedly.
func (k *Keeper) Stop() { k.once.Do(func() { close(k.stopC) }) }

// renewOnce sends ONE batched ExtendLease covering every held attempt.
//
// Batching is the whole reason this RPC exists in this shape (ADR-0010). At 10k
// concurrent tasks with a 10s renewal period, per-attempt renewal is 1k RPS of
// pure bookkeeping against the control plane's hottest table; one call per worker
// collapses that to the worker count. The RPC also returns revocations, so the
// same round-trip that proves ownership also tells us where ownership was lost.
func (k *Keeper) renewOnce(ctx context.Context) {
	k.mu.Lock()
	renewals := make([]*nexusv1.LeaseRenewal, 0, len(k.held))
	for id, h := range k.held {
		renewals = append(renewals, &nexusv1.LeaseRenewal{AttemptId: id, LeaseToken: h.token})
	}
	k.mu.Unlock()

	if len(renewals) == 0 {
		return // an idle worker sends nothing; leases are not worker liveness
	}

	// Bounded by the renewal interval: a renewal that outlives its own period
	// would pile up calls and still not beat the expiry it was racing.
	rpcCtx, cancel := context.WithTimeout(ctx, k.renewInterval)
	resp, err := k.client.ExtendLease(rpcCtx, &nexusv1.ExtendLeaseRequest{
		WorkerId: k.workerID, Renewals: renewals,
	})
	cancel()
	if err != nil {
		// Do NOT abandon the work on a failed renewal. The control plane will
		// expire the lease on its own schedule if we truly cannot reach it, and
		// abandoning after one failure would throw away work that a successful
		// retry two seconds from now would have kept. We do, however, check
		// whether our own recorded expiry has passed — see below.
		k.log.Warn("batched lease renewal failed", "attempts", len(renewals), "error", err)
		k.abandonLocallyExpired()
		return
	}

	now := time.Now()
	k.mu.Lock()
	for _, g := range resp.GetGrants() {
		if h, ok := k.held[g.GetAttemptId()]; ok {
			h.expiresAt = time.UnixMilli(g.GetExpiresAtUnixMs())
			h.epoch = g.GetEpoch()
		}
	}
	k.mu.Unlock()
	_ = now

	// Revocations are handled OUTSIDE the lock: the handler tears down a
	// workload, and holding the keeper's lock across that would block renewal of
	// every surviving attempt.
	for _, id := range resp.GetRevokedAttemptIds() {
		k.revoke(id, "lease_revoked_by_control_plane")
	}
}

// revoke stops work on an attempt immediately.
//
// STOP IMMEDIATELY is the correct response and it is not merely an optimisation.
// A revoked lease means the control plane has already given this attempt to
// someone else (or cancelled it). Any result we produce will be REJECTED on token
// mismatch, so continuing burns CPU, memory and a reservation for an outcome that
// is guaranteed to be discarded — and on a node where that capacity could be
// running work that will actually count.
func (k *Keeper) revoke(attemptID, reason string) {
	k.mu.Lock()
	_, tracked := k.held[attemptID]
	delete(k.held, attemptID)
	h := k.handler
	k.mu.Unlock()
	if !tracked {
		return // already finished or already revoked; nothing to tear down
	}
	k.log.Warn("lease revoked; stopping work immediately",
		"attempt_id", attemptID, "reason", reason)
	if h != nil {
		h.OnLeaseRevoked(attemptID, reason)
	}
}

// abandonLocallyExpired gives up on attempts whose recorded expiry has passed
// while we were unable to renew.
//
// This is the partition case. Once our own last-known expiry is in the past, the
// control plane has certainly expired the lease and very likely re-dispatched
// the attempt. Continuing is pure waste, and worse, it keeps a local reservation
// that makes this worker look busier than it is. Using OUR recorded expiry (which
// came from the control plane) rather than a locally computed deadline keeps
// invariant 7 intact: the timestamp is control-plane time, we only compare against
// it.
func (k *Keeper) abandonLocallyExpired() {
	now := time.Now()
	k.mu.Lock()
	var expired []string
	for id, h := range k.held {
		if !h.expiresAt.IsZero() && now.After(h.expiresAt) {
			expired = append(expired, id)
		}
	}
	k.mu.Unlock()

	for _, id := range expired {
		k.revoke(id, "lease_expired_while_unreachable")
	}
}
