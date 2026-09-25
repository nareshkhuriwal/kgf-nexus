//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
	"google.golang.org/grpc/status"
)

// Store-level tests for ADR-0021's run-level quotas, the quota document, the
// usage rollups and the ownership grants. The claim-side proof (concurrency,
// reasons, metering paths) is services/scheduler/quota_integration_test.go.

func putQuotas(t *testing.T, f *fixture, q platform.Quotas) int64 {
	t.Helper()
	rev, _, err := GatewayQuotasGet(f.ctx, f.pool, f.ns)
	if err != nil {
		t.Fatal(err)
	}
	rev, err = GatewayQuotasPut(f.ctx, f.pool, f.ns, rev, q, true, AuditEntry{ActorKind: "test", ActorLabel: "test"})
	if err != nil {
		t.Fatalf("put quotas: %v", err)
	}
	return rev
}

// TestIntegrationRunQuotaActiveRunsUnderConcurrency: 20 concurrent creations
// against max_active_runs=5 (one already active from the fixture) create
// exactly 4, and every refusal is the typed quota error.
func TestIntegrationRunQuotaActiveRunsUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)
	putQuotas(t, f, platform.Quotas{MaxActiveRuns: 5})

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		refused int
	)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := WFMCreateRun(ctx, pool, CreateRunParams{SpecID: f.specID, Namespace: f.ns, TriggerKind: "api"})
			mu.Lock()
			defer mu.Unlock()
			var q *RunQuotaError
			switch {
			case err == nil && ok:
				created++
			case errors.As(err, &q):
				if q.Dimension != platform.DimActiveRuns || q.Limit != 5 || q.Used != 5 || q.RetryAfter != 0 {
					t.Errorf("refusal = %+v", q)
				}
				refused++
			default:
				t.Errorf("unexpected: created=%v err=%v", ok, err)
			}
		}()
	}
	wg.Wait()
	if created != 4 || refused != 16 {
		t.Fatalf("created %d, refused %d; want 4 and 16 (quota 5, one already active)", created, refused)
	}
}

// TestIntegrationRunQuotaPerHourAndReplay: the hourly rate refuses with a
// Retry-After, a replayed idempotency key returns its run even at the limit,
// and the refusal survives the gRPC hop with its numbers.
func TestIntegrationRunQuotaPerHourAndReplay(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)
	putQuotas(t, f, platform.Quotas{MaxRunsPerHour: 3})

	first, _, err := WFMCreateRun(ctx, pool, CreateRunParams{SpecID: f.specID, Namespace: f.ns, TriggerKind: "api", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := WFMCreateRun(ctx, pool, CreateRunParams{SpecID: f.specID, Namespace: f.ns, TriggerKind: "api"}); err != nil {
		t.Fatal(err)
	}
	// The fixture's run plus two: at the limit.
	_, _, err = WFMCreateRun(ctx, pool, CreateRunParams{SpecID: f.specID, Namespace: f.ns, TriggerKind: "api"})
	var q *RunQuotaError
	if !errors.As(err, &q) || !errors.Is(err, ErrRunQuotaExceeded) || q.Dimension != platform.DimRunsPerHour ||
		q.Used != 3 || q.Limit != 3 || q.RetryAfter < 59*time.Minute || q.RetryAfter > time.Hour {
		t.Fatalf("hourly refusal: err=%v q=%+v", err, q)
	}
	if got := err.Error(); got != "namespace_quota_exceeded: runs_per_hour (used 3 of 3)" {
		t.Errorf("message %q", got)
	}
	again, created, err := WFMCreateRun(ctx, pool, CreateRunParams{SpecID: f.specID, Namespace: f.ns, TriggerKind: "api", IdempotencyKey: "k1"})
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("replay at the limit: created=%v id=%s err=%v, want the original %s", created, again.ID, err, first.ID)
	}

	// Across the wire and back.
	back := RunQuotaFromStatus(status.Convert(RunQuotaStatus("create run", q)))
	if back == nil || *back != *q {
		t.Fatalf("round trip: %+v, want %+v", back, q)
	}
}

// TestIntegrationQuotaDocumentRevisionLockAndAudit: the quota PUT is revisioned
// with the platform policy, a locked document refuses a namespace admin's
// loosening but not tightening, only an operator can lock, the generic config
// PUT preserves quotas, and every change is audited.
func TestIntegrationQuotaDocumentRevisionLockAndAudit(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)
	audit := AuditEntry{ActorKind: "session", ActorLabel: "admin"}

	rev, q, err := GatewayQuotasGet(ctx, pool, f.ns)
	if err != nil || rev != 0 || q.HasConcurrent() {
		t.Fatalf("fresh namespace: rev=%d q=%+v err=%v", rev, q, err)
	}
	// A namespace admin may not lock.
	if _, err := GatewayQuotasPut(ctx, pool, f.ns, 0, platform.Quotas{Locked: true, MaxCPUMillis: 1}, false, audit); !errors.Is(err, ErrQuotaLocked) {
		t.Fatalf("admin lock: %v", err)
	}
	rev, err = GatewayQuotasPut(ctx, pool, f.ns, 0, platform.Quotas{Locked: true, MaxCPUMillis: 4000, MaxActiveRuns: 10}, true, audit)
	if err != nil || rev != 1 {
		t.Fatalf("operator put: rev=%d err=%v", rev, err)
	}
	// Stale revision.
	if _, err := GatewayQuotasPut(ctx, pool, f.ns, 0, platform.Quotas{}, true, audit); !errors.Is(err, ErrPlatformConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	// Locked: an admin may tighten...
	if rev, err = GatewayQuotasPut(ctx, pool, f.ns, rev, platform.Quotas{Locked: true, MaxCPUMillis: 2000, MaxActiveRuns: 10, MaxGPUCount: 1}, false, audit); err != nil {
		t.Fatalf("tighten while locked: %v", err)
	}
	// ...but not loosen or unlock.
	var le *QuotaLockedError
	if _, err := GatewayQuotasPut(ctx, pool, f.ns, rev, platform.Quotas{Locked: false, MaxCPUMillis: 8000}, false, audit); !errors.As(err, &le) || len(le.Loosened) != 4 {
		t.Fatalf("loosen while locked: %v %+v", err, le)
	}

	// A quota edit must not disturb the rest of the policy: the jsonb_set path
	// preserves fields it does not own. (Carrying quotas across the generic
	// platform PUT is the handler's job and is tested in the REST suite.)
	c, err := GatewayPlatformGet(ctx, pool, f.ns)
	if err != nil {
		t.Fatal(err)
	}
	c.Scheduler.TargetCPU = 70
	if _, err := GatewayPlatformPut(ctx, pool, f.ns, c, audit); err != nil {
		t.Fatalf("platform put: %v", err)
	}
	var rawTarget int
	if err := pool.QueryRow(ctx, `SELECT (config->'scheduler'->>'target_cpu_percent')::int FROM platform_config WHERE namespace=$1`, f.ns).Scan(&rawTarget); err != nil {
		t.Fatal(err)
	}
	rev, _, _ = GatewayQuotasGet(ctx, pool, f.ns)
	if _, err := GatewayQuotasPut(ctx, pool, f.ns, rev, platform.Quotas{MaxCPUMillis: 1000}, true, audit); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT (config->'scheduler'->>'target_cpu_percent')::int FROM platform_config WHERE namespace=$1`, f.ns).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if rawTarget != 70 || after != 70 {
		t.Errorf("a quota edit changed the scheduler policy: %d -> %d", rawTarget, after)
	}

	var audits int
	var detail []byte
	if err := pool.QueryRow(ctx, `SELECT count(*), max(detail::text)::jsonb FROM audit_log WHERE namespace=$1 AND action='quota.update'`, f.ns).Scan(&audits, &detail); err != nil {
		t.Fatal(err)
	}
	if audits != 3 {
		t.Errorf("quota.update audit rows = %d, want 3 (one per accepted change)", audits)
	}
	var d map[string]any
	_ = json.Unmarshal(detail, &d)
	if d["before"] == nil || d["after"] == nil {
		t.Errorf("audit detail lacks before/after: %s", detail)
	}
}

// TestIntegrationUsageRollupsAndPagination: rollups sum exactly, group by day
// and pool, paginate by key, and never read another namespace's rows.
func TestIntegrationUsageRollupsAndPagination(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)
	other := newFixture(t, ctx, pool)
	day := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	insert := func(fx *fixture, ended time.Time, pool string, cpu int64) {
		t.Helper()
		if _, err := fx.pool.Exec(ctx, `
INSERT INTO usage_ledger (attempt_id, tenant_id, namespace, run_id, node_id, attempt_number, outcome, queue, pool,
  cpu_millis, dispatched_at, ended_at, wall_seconds, reserved_seconds, cpu_core_seconds, memory_gib_seconds, gpu_seconds)
SELECT $1, t.id, t.namespace, $2, 'n', 1, 'SUCCEEDED', 'q', NULLIF($3::text,''), $4::bigint, $5::timestamptz - interval '100 seconds', $5,
       100, 100, $4::bigint::numeric * 100 / 1000, 0, 0
  FROM tenants t WHERE t.namespace = $6`,
			uuid.New(), uuid.New(), pool, cpu, ended, fx.ns); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		insert(f, day.AddDate(0, 0, i), "gpu", 1000)
		insert(f, day.AddDate(0, 0, i), "cpu", 2000)
	}
	insert(other, day, "gpu", 999000)

	q := UsageQuery{From: day.AddDate(0, 0, -1), To: day.AddDate(0, 0, 10), GroupBy: "day", Limit: 2}
	var keys []string
	var cpu float64
	for page := 0; ; page++ {
		p, err := GatewayUsageRollup(ctx, pool, f.ns, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range p.Buckets {
			keys = append(keys, b.Key)
			cpu += b.CPUCoreSeconds
			if b.Attempts != 2 {
				t.Errorf("day %s has %d attempts, want 2", b.Key, b.Attempts)
			}
		}
		if p.NextCursor == "" {
			break
		}
		q.Cursor = p.NextCursor
		if page > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(keys) != 5 || keys[0] != "2026-09-01" || keys[4] != "2026-09-05" {
		t.Errorf("day keys %v", keys)
	}
	// 5 days x (100 s x 1 core + 100 s x 2 cores) = 1500 core-seconds; the
	// other namespace's 99,900 are not in it.
	if cpu != 1500 {
		t.Errorf("cpu_core_seconds total %v, want 1500", cpu)
	}
	byPool, err := GatewayUsageRollup(ctx, pool, f.ns, UsageQuery{From: q.From, To: q.To, GroupBy: "pool"})
	if err != nil || len(byPool.Buckets) != 2 || byPool.Buckets[0].Key != "cpu" || byPool.Buckets[0].CPUCoreSeconds != 1000 {
		t.Fatalf("by pool: %+v %v", byPool, err)
	}
	if _, err := GatewayUsageRollup(ctx, pool, f.ns, UsageQuery{From: q.From, To: q.To, GroupBy: "tenant_id; DROP TABLE x"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unknown group_by accepted: %v", err)
	}
	plat, err := GatewayPlatformUsageRollup(ctx, pool, UsageQuery{From: q.From, To: q.To, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, b := range plat.Buckets {
		if b.Key == f.ns || b.Key == other.ns {
			seen++
		}
	}
	if seen != 2 {
		t.Errorf("platform rollup saw %d of the 2 namespaces", seen)
	}

	// The exporter's keyset read returns rows only once they are past the lag.
	recs, err := UsageRecordsAfter(ctx, pool, 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Namespace == f.ns {
			t.Fatal("a row younger than UsageExportLag was exported")
		}
	}
}

// TestIntegrationQuotaAndLedgerGrants: the ownership ADR-0021 documents is
// the ownership the database enforces. The scheduler inserts the ledger and
// nothing may update or delete it; every service reads the views, which in
// turn read the gateway-owned platform_config with the view owner's rights.
func TestIntegrationQuotaAndLedgerGrants(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	var roles int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname LIKE 'nexus\_%'`).Scan(&roles); err != nil || roles < 6 {
		t.Skipf("service roles not present (%d): %v", roles, err)
	}
	check := func(role, rel, priv string, want bool) {
		t.Helper()
		var got bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege($1, $2, $3)`, role, rel, priv).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s %s on %s = %v, want %v", role, priv, rel, got, want)
		}
	}
	check("nexus_scheduler", "usage_ledger", "INSERT", true)
	for _, r := range []string{"nexus_scheduler", "nexus_api_gateway", "nexus_event_service", "nexus_workflow_manager"} {
		check(r, "usage_ledger", "UPDATE", false)
		check(r, "usage_ledger", "DELETE", false)
		check(r, "usage_ledger", "SELECT", true)
		check(r, "namespace_quota_limits", "SELECT", true)
		check(r, "namespace_queue_pools", "SELECT", true)
	}
	check("nexus_api_gateway", "usage_ledger", "INSERT", false)

	// The views really are readable by a role with no grant on platform_config.
	f := newFixture(t, ctx, pool)
	putQuotas(t, f, platform.Quotas{MaxInflightAttempts: 3})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE nexus_scheduler`); err != nil {
		t.Fatal(err)
	}
	var limit int64
	if err := tx.QueryRow(ctx, `SELECT max_inflight_attempts FROM namespace_quota_limits WHERE namespace=$1`, f.ns).Scan(&limit); err != nil || limit != 3 {
		t.Fatalf("scheduler reading the quota view: %d %v", limit, err)
	}
}
