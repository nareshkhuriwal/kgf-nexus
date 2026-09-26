//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// The workload API's control-plane half (ADR-0032), against real Postgres 16:
// usage units and their copy into the usage ledger, the race of a usage report
// against every terminal transition, shared token-bucket permits across
// scheduler replicas, fenced checkpoints, and free RATE_LIMITED retries.

func (p *progressAttempt) usage(t *testing.T, token string, units map[string]int64) (*RecordUsageResponse, error) {
	t.Helper()
	return p.srv.RecordUsage(context.Background(), &RecordUsageRequest{
		AttemptID: p.id.String(), LeaseToken: token, Units: units,
	})
}

func (p *progressAttempt) storedUnits(t *testing.T) map[string]int64 {
	t.Helper()
	var raw []byte
	err := p.f.pool.QueryRow(context.Background(),
		`SELECT units FROM attempt_usage WHERE attempt_id = $1`, p.id).Scan(&raw)
	if err != nil {
		return nil
	}
	out := map[string]int64{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// ledgerCustom returns the usage ledger's custom object for the attempt, and
// whether a row exists.
func (f *fixture) ledgerCustom(id uuid.UUID) (map[string]int64, bool) {
	f.t.Helper()
	var raw []byte
	if err := f.pool.QueryRow(context.Background(),
		`SELECT custom FROM usage_ledger WHERE attempt_id = $1`, id).Scan(&raw); err != nil {
		return nil, false
	}
	out := map[string]int64{}
	if err := json.Unmarshal(raw, &out); err != nil {
		f.t.Fatal(err)
	}
	return out, true
}

func usageOnly(m map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for k, v := range m {
		if domain.ValidUsageKey(k) {
			out[k] = v
		}
	}
	return out
}

func equalUnits(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestUsageUnitsMergeFenceAndReachTheLedger(t *testing.T) {
	f := newFixture(t, newTestPool(t))
	p := newProgressAttempt(t, f, "llm", 0)
	const in, out, req = "tokens:openai/gpt-4o-mini:input", "tokens:openai/gpt-4o-mini:output", "requests:openai/gpt-4o-mini"

	// Cumulative snapshots merge with max per key: a reordered, older snapshot
	// can neither lower nor double a total.
	if _, err := p.usage(t, p.token, map[string]int64{in: 100, req: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.usage(t, p.token, map[string]int64{in: 250, out: 40, req: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.usage(t, p.token, map[string]int64{in: 100, req: 1}); err != nil { // late duplicate
		t.Fatal(err)
	}
	want := map[string]int64{in: 250, out: 40, req: 2}
	if got := p.storedUnits(t); !equalUnits(got, want) {
		t.Fatalf("stored units = %v, want %v", got, want)
	}

	// Invalid reports are refused whole, as InvalidArgument, and store nothing.
	for _, bad := range []map[string]int64{
		{"tokens openai": 1},
		{in: -1},
		{"Tokens:openai/x:input": 1},
		{"custom_resource": 5}, // no ':' -- could collide with a reserved custom resource
	} {
		_, err := p.usage(t, p.token, bad)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("report %v: err = %v, want InvalidArgument", bad, err)
		}
	}
	tooMany := map[string]int64{}
	for i := 0; i < domain.MaxUsageKeys+1; i++ {
		tooMany[fmt.Sprintf("units:k%d", i)] = 1
	}
	if _, err := p.usage(t, p.token, tooMany); status.Code(err) != codes.InvalidArgument {
		t.Errorf("65 keys: err = %v, want InvalidArgument", err)
	}
	if got := p.storedUnits(t); !equalUnits(got, want) {
		t.Fatalf("an invalid report changed the stored units: %v", got)
	}

	// A token that is not the live lease is stale.
	if resp, err := p.usage(t, "not-the-token", map[string]int64{in: 999}); err != nil || !resp.StaleLease {
		t.Fatalf("wrong token: resp=%+v err=%v, want stale", resp, err)
	}

	// The terminal transition copies the units into the ledger's custom object
	// and the outbox announcement, in the same transaction.
	applied, err := p.st.ApplyResult(context.Background(), p.clock, &nexusv1.ReportResultRequest{
		AttemptId: p.id.String(), LeaseToken: p.token,
		Outcome: nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
	})
	if err != nil || !applied.Applied || !applied.UsageRecorded {
		t.Fatalf("apply: %+v %v", applied, err)
	}
	custom, ok := f.ledgerCustom(p.id)
	if !ok || !equalUnits(usageOnly(custom), want) {
		t.Fatalf("ledger custom = %v (row %v), want the units %v", custom, ok, want)
	}
	var payload []byte
	if err := f.pool.QueryRow(context.Background(), `
SELECT payload FROM outbox WHERE topic = 'usage_recorded' AND payload->>'attempt_id' = $1`,
		p.id.String()).Scan(&payload); err != nil {
		t.Fatalf("usage_recorded outbox row: %v", err)
	}
	var ob struct {
		UsageUnits map[string]int64 `json:"usage_units"`
	}
	_ = json.Unmarshal(payload, &ob)
	if !equalUnits(ob.UsageUnits, want) {
		t.Fatalf("outbox usage_units = %v, want %v", ob.UsageUnits, want)
	}

	// After the end, the attempt accepts no more usage.
	if resp, err := p.usage(t, p.token, map[string]int64{in: 9999}); err != nil || !resp.StaleLease {
		t.Fatalf("usage after the end: resp=%+v err=%v, want stale", resp, err)
	}
}

// TestUsageNeverEscapesTheLedger races a burst of usage reports against each of
// the three terminal transitions. Whatever the interleaving, the units stored on
// the attempt must equal the units in its ledger row: a report either lands
// before the transition (and is billed) or is refused after it.
func TestUsageNeverEscapesTheLedger(t *testing.T) {
	pool := newTestPool(t)
	for _, path := range []string{"result", "expire", "cancel"} {
		for round := 0; round < 8; round++ {
			t.Run(fmt.Sprintf("%s/%d", path, round), func(t *testing.T) {
				f := newFixture(t, pool)
				p := newProgressAttempt(t, f, "race", 0)
				ctx := context.Background()
				var (
					wg      sync.WaitGroup
					stop    atomic.Bool
					counter atomic.Int64
				)
				for w := 0; w < 4; w++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for !stop.Load() {
							n := counter.Add(1)
							resp, err := p.srv.RecordUsage(ctx, &RecordUsageRequest{
								AttemptID: p.id.String(), LeaseToken: p.token,
								Units: map[string]int64{"tokens:mock/m:input": n, "requests:mock/m": n},
							})
							if err != nil && status.Code(err) != codes.Unavailable {
								t.Errorf("record usage: %v", err)
								return
							}
							if resp != nil && resp.StaleLease {
								return
							}
						}
					}()
				}
				time.Sleep(time.Duration(5+round*3) * time.Millisecond)
				switch path {
				case "result":
					if _, err := p.st.ApplyResult(ctx, time.Now().UTC(), &nexusv1.ReportResultRequest{
						AttemptId: p.id.String(), LeaseToken: p.token,
						Outcome: nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
					}); err != nil {
						t.Fatal(err)
					}
				case "expire":
					f.expireLease(p.id)
					if _, err := p.st.ReconcileExpiredLeases(ctx, time.Now().UTC(), 10); err != nil {
						t.Fatal(err)
					}
				case "cancel":
					if _, err := p.st.CancelRunAttempts(ctx, f.runID, time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
				}
				time.Sleep(20 * time.Millisecond)
				stop.Store(true)
				wg.Wait()

				if s := f.attemptState(p.id); s == "RUNNING" {
					t.Fatalf("attempt still RUNNING after %s", path)
				}
				custom, ok := f.ledgerCustom(p.id)
				if !ok {
					t.Fatal("no ledger row")
				}
				stored := p.storedUnits(t)
				if !equalUnits(usageOnly(custom), stored) {
					t.Fatalf("%s: ledger units %v != attempt units %v (a report escaped the ledger)",
						path, usageOnly(custom), stored)
				}
			})
		}
	}
}

// setRateLimits writes quotas.rate_limits straight into the policy, as the
// quotas API would, so the tests exercise the namespace_rate_limits view.
func (f *fixture) setRateLimits(limits map[string]any) {
	f.t.Helper()
	f.setQuotas(map[string]any{"rate_limits": limits})
}

func (p *progressAttempt) permits(t *testing.T, srv *ReportServer, keys []string, requests, tokens int64, settle bool) *AcquirePermitsResponse {
	t.Helper()
	resp, err := srv.AcquirePermits(context.Background(), &AcquirePermitsRequest{
		AttemptID: p.id.String(), LeaseToken: p.token, Keys: keys,
		Requests: requests, Tokens: tokens, Settle: settle,
	})
	if err != nil {
		t.Fatalf("AcquirePermits: %v", err)
	}
	return resp
}

func TestPermitsTokenBucketSemantics(t *testing.T) {
	f := newFixture(t, newTestPool(t))
	p := newProgressAttempt(t, f, "llm", 0)
	// 600 tokens/min = 10 tokens/s, burst 100.
	f.setRateLimits(map[string]any{"mock/m": map[string]any{"tokens_per_minute": 600, "burst_tokens": 100}})

	// Unconfigured keys are unlimited and touch no bucket row.
	r := p.permits(t, p.srv, []string{"other/model"}, 1, 10, false)
	if !r.Granted || len(r.UnlimitedKeys) != 1 {
		t.Fatalf("unconfigured key: %+v", r)
	}

	if r := p.permits(t, p.srv, []string{"mock/m"}, 1, 100, false); !r.Granted {
		t.Fatalf("full bucket refused 100 tokens: %+v", r)
	}
	r = p.permits(t, p.srv, []string{"mock/m"}, 1, 20, false)
	if r.Granted || r.RetryAfterMs < 1500 || r.RetryAfterMs > 2100 {
		t.Fatalf("empty bucket: %+v, want denied with ~2s to wait for 20 tokens at 10/s", r)
	}
	// A refund of an over-estimate (settle with a negative correction) makes
	// the tokens available at once.
	p.permits(t, p.srv, []string{"mock/m"}, 0, -50, true)
	if r := p.permits(t, p.srv, []string{"mock/m"}, 1, 40, false); !r.Granted {
		t.Fatalf("after a 50-token refund, 40 tokens refused: %+v", r)
	}

	// All or none: a request whose second key is exhausted takes nothing from
	// the first.
	f.setRateLimits(map[string]any{
		"mock/m":   map[string]any{"tokens_per_minute": 600, "burst_tokens": 100},
		"mock/big": map[string]any{"requests_per_second": 1000, "burst_requests": 1000},
	})
	var before float64
	_ = f.pool.QueryRow(context.Background(), `SELECT request_level FROM rate_limit_buckets b
JOIN tenants t ON t.id = b.tenant_id WHERE t.namespace = $1 AND b.key = 'mock/big'`, f.ns).Scan(&before)
	if r := p.permits(t, p.srv, []string{"mock/big", "mock/m"}, 1, 1000, false); r.Granted {
		t.Fatalf("an exhausted key granted: %+v", r)
	}
	var reqGranted int64
	_ = f.pool.QueryRow(context.Background(), `SELECT granted_requests FROM rate_limit_buckets b
JOIN tenants t ON t.id = b.tenant_id WHERE t.namespace = $1 AND b.key = 'mock/big'`, f.ns).Scan(&reqGranted)
	if reqGranted != 0 {
		t.Fatalf("a denied multi-key request took %d requests from the other key", reqGranted)
	}

	// Stale lease: nothing is taken.
	resp, err := p.srv.AcquirePermits(context.Background(), &AcquirePermitsRequest{
		AttemptID: p.id.String(), LeaseToken: "nope", Keys: []string{"mock/m"}, Requests: 1,
	})
	if err != nil || !resp.StaleLease {
		t.Fatalf("stale lease: %+v %v", resp, err)
	}
	// Invalid requests.
	for _, bad := range []*AcquirePermitsRequest{
		{Keys: nil, Requests: 1},
		{Keys: []string{"bad key"}, Requests: 1},
		{Keys: []string{"a"}, Requests: 0, Tokens: 0},
		{Keys: []string{"a"}, Requests: -1},
		{Keys: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}, Requests: 1},
	} {
		bad.AttemptID, bad.LeaseToken = p.id.String(), p.token
		if _, err := p.srv.AcquirePermits(context.Background(), bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%+v: err = %v, want InvalidArgument", bad, err)
		}
	}
}

// TestPermitsHoldAcrossReplicas is the fleet-wide claim: four scheduler
// replicas (separate connection pools, as separate processes have), 8 callers
// each, hammering one key. The permits granted over the window can never exceed
// burst + rate x elapsed, and the limiter must not starve callers either.
func TestPermitsHoldAcrossReplicas(t *testing.T) {
	f := newFixture(t, newTestPool(t))
	p := newProgressAttempt(t, f, "llm", 0)
	const rps, burst = 50.0, 10
	f.setRateLimits(map[string]any{"mock/shared": map[string]any{"requests_per_second": rps, "burst_requests": burst}})

	const replicas, callers = 4, 8
	window := 2 * time.Second
	var (
		granted, denied atomic.Int64
		wg              sync.WaitGroup
	)
	start := time.Now()
	for r := 0; r < replicas; r++ {
		st := store.NewSchedulerStore(newTestPool(t), store.DefaultClaimConfig(f.queue), "replica-"+uuid.NewString())
		srv := NewReportServer(st, p.srv.log)
		for c := 0; c < callers; c++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Since(start) < window {
					resp, err := srv.AcquirePermits(context.Background(), &AcquirePermitsRequest{
						AttemptID: p.id.String(), LeaseToken: p.token, Keys: []string{"mock/shared"}, Requests: 1,
					})
					if err != nil {
						t.Errorf("acquire: %v", err)
						return
					}
					if resp.Granted {
						granted.Add(1)
					} else {
						denied.Add(1)
						if resp.RetryAfterMs <= 0 {
							t.Errorf("denied without a retry_after")
							return
						}
						time.Sleep(time.Duration(resp.RetryAfterMs) * time.Millisecond)
					}
				}
			}()
		}
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	ceiling := float64(burst) + rps*elapsed
	g := float64(granted.Load())
	t.Logf("granted %d, denied %d in %.2fs (ceiling %.0f)", granted.Load(), denied.Load(), elapsed, ceiling)
	if g > math.Ceil(ceiling) {
		t.Fatalf("granted %.0f permits in %.2fs; the limit allows at most %.1f", g, elapsed, ceiling)
	}
	if g < 0.7*rps*window.Seconds() {
		t.Fatalf("granted only %.0f of ~%.0f available permits: the limiter starves callers", g, rps*window.Seconds())
	}
}

func (f *fixture) checkpointPrefix(p *progressAttempt) string {
	return fmt.Sprintf("%s/checkpoints/idem-%s-%s/%s/", f.ns, f.runID, p.nodeID, p.id)
}

func (p *progressAttempt) save(t *testing.T, token string, seq int64, key string) *SaveCheckpointResponse {
	t.Helper()
	resp, err := p.srv.SaveCheckpoint(context.Background(), &SaveCheckpointRequest{
		AttemptID: p.id.String(), LeaseToken: token, Seq: seq, Key: key,
		SHA256: fmt.Sprintf("%064x", seq), SizeBytes: 10 * seq,
	})
	if err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	return resp
}

func TestCheckpointsAreFencedAndForwardOnly(t *testing.T) {
	f := newFixture(t, newTestPool(t))
	p := newProgressAttempt(t, f, "shard", 0)
	ctx := context.Background()
	prefix := f.checkpointPrefix(p)

	if r := p.save(t, p.token, 1, prefix+"1"); r.StaleLease || r.Superseded {
		t.Fatalf("first save: %+v", r)
	}
	if r := p.save(t, p.token, 1, prefix+"1"); !r.Superseded {
		t.Fatalf("same seq again: %+v, want superseded", r)
	}
	if r := p.save(t, p.token, 3, prefix+"3"); r.Superseded || r.StaleLease {
		t.Fatalf("seq 3: %+v", r)
	}
	if r := p.save(t, p.token, 2, prefix+"2"); !r.Superseded {
		t.Fatalf("a reordered older save moved the pointer back: %+v", r)
	}
	// A key outside the attempt's own prefix is refused: an attempt cannot
	// point its checkpoint at another attempt's (or tenant's) object.
	for _, key := range []string{"other-ns/checkpoints/x/y/4", prefix[:len(prefix)-1] + "x/4", prefix, prefix + "../4"} {
		_, err := p.srv.SaveCheckpoint(ctx, &SaveCheckpointRequest{
			AttemptID: p.id.String(), LeaseToken: p.token, Seq: 9, Key: key,
			SHA256: fmt.Sprintf("%064x", 9), SizeBytes: 1,
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("key %q: err = %v, want InvalidArgument", key, err)
		}
	}
	if r := p.save(t, "stolen", 4, prefix+"4"); !r.StaleLease {
		t.Fatalf("wrong token: %+v", r)
	}

	// The gateway read returns the pointer by idempotency key.
	c, err := store.GatewayGetCheckpoint(ctx, f.pool, p.id.String())
	if err != nil || c.Seq != 3 || c.Key != prefix+"3" || c.AttemptNumber != 1 {
		t.Fatalf("GatewayGetCheckpoint = %+v, %v", c, err)
	}

	// The attempt is LOST and retried. Attempt 2 shares the idempotency key,
	// reads attempt 1's checkpoint, and its first save supersedes it; attempt
	// 1, now a zombie without a lease, can no longer save anything.
	f.expireLease(p.id)
	if _, err := p.st.ReconcileExpiredLeases(ctx, time.Now().UTC(), 10); err != nil {
		t.Fatal(err)
	}
	var retryID uuid.UUID
	if err := f.pool.QueryRow(ctx, `
INSERT INTO task_attempts (run_id, node_id, attempt_number, state, queue, priority,
                           resource_request, idempotency_key, scheduled_at, dispatched_at, started_at)
SELECT run_id, node_id, 2, 'RUNNING', queue, priority, resource_request, idempotency_key, now(), now(), now()
  FROM task_attempts WHERE id = $1 RETURNING id`, p.id).Scan(&retryID); err != nil {
		t.Fatal(err)
	}
	retryToken := "tok-" + uuid.NewString()
	if _, err := f.pool.Exec(ctx, `INSERT INTO leases (attempt_id, worker_id, token, expires_at, epoch)
VALUES ($1, $2, $3, now() + interval '5 minutes', 1)`, retryID, p.worker, retryToken); err != nil {
		t.Fatal(err)
	}
	if c, err := store.GatewayGetCheckpoint(ctx, f.pool, retryID.String()); err != nil || c.Seq != 3 || c.AttemptNumber != 1 {
		t.Fatalf("the retry does not see attempt 1's checkpoint: %+v %v", c, err)
	}
	if r := p.save(t, p.token, 99, prefix+"99"); !r.StaleLease {
		t.Fatalf("zombie save after LOST: %+v, want stale", r)
	}
	retry := &progressAttempt{f: f, id: retryID, worker: p.worker, token: retryToken, st: p.st, srv: p.srv, nodeID: p.nodeID}
	if r := retry.save(t, retryToken, 1, f.checkpointPrefix(retry)+"1"); r.Superseded || r.StaleLease {
		t.Fatalf("retry's first save (attempt 2, seq 1) must supersede attempt 1's seq 3: %+v", r)
	}
	if c, _ := store.GatewayGetCheckpoint(ctx, f.pool, retryID.String()); c.AttemptNumber != 2 || c.Seq != 1 {
		t.Fatalf("pointer = %+v, want attempt 2 seq 1", c)
	}
}

func TestRateLimitedRetriesAreFreeUpToTheCap(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	w := f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	// max_attempts = 1: any CHARGED retry is refused, so only a free one can
	// happen.
	id := f.addAttempt("throttled", 100, vec{CPUMillis: 100}, nodeSpec{TaskRef: "t", Version: 1, MaxAttempts: 1}, time.Now().UTC())
	if _, err := pool.Exec(ctx, `UPDATE task_attempts SET state='RUNNING', worker_id=$2, dispatched_at=now(), started_at=now() WHERE id=$1`, id, w); err != nil {
		t.Fatal(err)
	}
	st := f.newStore(func(c *store.ClaimConfig) { c.RateLimitedFreeRetries = 2 })

	fail := func(attemptID uuid.UUID, retryAfter int64) store.AppliedResult {
		t.Helper()
		res, err := st.ApplyResult(ctx, time.Now().UTC(), &nexusv1.ReportResultRequest{
			AttemptId: attemptID.String(), Outcome: nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED,
			Error: &nexusv1.TaskError{Class: nexusv1.ErrorClass_ERROR_CLASS_RATE_LIMITED, RetryAfterMs: retryAfter},
		})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	freeRetries := func() int32 {
		var n int32
		if err := pool.QueryRow(ctx, `SELECT free_retries FROM task_nodes WHERE run_id=$1 AND node_id='throttled'`, f.runID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	fireRetry := func(prev uuid.UUID, number int32) uuid.UUID {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE timers SET fire_at = now() - interval '1 second' WHERE subject_id = $1`, prev); err != nil {
			t.Fatal(err)
		}
		timers, err := st.ClaimDueTimers(ctx, time.Now().UTC(), []string{string(domain.TimerRetryBackoff)}, 10)
		if err != nil || len(timers) != 1 {
			t.Fatalf("claim retry timer: %v (%d)", err, len(timers))
		}
		created, err := st.CreatePendingRetry(ctx, time.Now().UTC(), timers[0])
		if err != nil || created == nil || created.AttemptNumber != number {
			t.Fatalf("create retry: %+v %v", created, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE task_attempts SET state='RUNNING', worker_id=$2, dispatched_at=now(), started_at=now() WHERE id=$1`, created.NewAttemptID, w); err != nil {
			t.Fatal(err)
		}
		return created.NewAttemptID
	}

	// First throttle: free retry after the provider's Retry-After.
	before := time.Now()
	if r := fail(id, 7000); !r.Retry {
		t.Fatalf("first RATE_LIMITED failure not retried: %+v", r)
	}
	if n := freeRetries(); n != 1 {
		t.Fatalf("free_retries = %d, want 1", n)
	}
	var fireAt time.Time
	if err := pool.QueryRow(ctx, `SELECT fire_at FROM timers WHERE subject_id = $1 AND kind = $2`, id, domain.TimerRetryBackoff).Scan(&fireAt); err != nil {
		t.Fatal(err)
	}
	if d := fireAt.Sub(before); d < 6900*time.Millisecond || d > 9*time.Second {
		t.Fatalf("retry fires after %v, want the provider's 7s Retry-After", d)
	}
	var charged bool
	if err := pool.QueryRow(ctx, `SELECT (payload->>'charged')::boolean FROM run_events WHERE attempt_id = $1 AND kind = $2`,
		id, domain.EventRetryScheduled).Scan(&charged); err != nil || charged {
		t.Fatalf("RetryScheduled charged = %v (%v), want false", charged, err)
	}

	a2 := fireRetry(id, 2)
	if r := fail(a2, 0); !r.Retry {
		t.Fatalf("second RATE_LIMITED failure not retried: %+v", r)
	}
	a3 := fireRetry(a2, 3)
	// The cap (2) is spent: the third throttle is charged like TRANSIENT, and
	// with max_attempts = 1 the budget is already gone.
	if r := fail(a3, 0); r.Retry {
		t.Fatalf("a throttle past the free cap was retried free: %+v", r)
	}
	if n := freeRetries(); n != 2 {
		t.Fatalf("free_retries = %d, want 2", n)
	}
}

// TestSchedulerRoleCanChargeTheRetryBudget is F149: the scheduler creates
// retry attempts and charges task_nodes.attempts_used in the same transaction,
// but migration 0005 granted that column to the DAG engine only.
func TestSchedulerRoleCanChargeTheRetryBudget(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	f.addAttempt("n", 100, vec{}, nodeSpec{TaskRef: "t", Version: 1}, time.Now().UTC())
	ctx := context.Background()
	var exists bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_scheduler')`).Scan(&exists)
	if !exists {
		t.Skip("service roles are not installed in this database")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE nexus_scheduler`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE task_nodes SET attempts_used = attempts_used + 1, free_retries = free_retries + 1
WHERE run_id = $1 AND node_id = 'n'`, f.runID); err != nil {
		t.Fatalf("the scheduler cannot charge the retry budget: %v", err)
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT s`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE task_nodes SET state = 'FAILED' WHERE run_id = $1`, f.runID); err == nil {
		t.Fatal("the scheduler may write task_nodes.state; the grant must stay column-scoped")
	}
}
