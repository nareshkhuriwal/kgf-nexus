//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The live-infrastructure tables (migrations 0041 and 0042, ADR-0037) against
// a real database: the registry's upsert, staleness and prune; placement's
// fencing, tenancy and cascade; the task-runner listing's scoping and summary;
// and the grants every writer depends on.

func findInstance(rows []ServiceInstanceView, component, instance string) (ServiceInstanceView, int) {
	var found ServiceInstanceView
	n := 0
	for _, r := range rows {
		if r.Component == component && r.Instance == instance {
			found = r
			n++
		}
	}
	return found, n
}

func TestIntegrationServiceInstanceRegistry(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	instance := uniqueName(t)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM service_instances WHERE instance = $1`, instance) })

	started := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	row := ServiceInstance{
		Component: "scheduler", Instance: instance, PodNamespace: "kgf-nexus",
		Node: "node-a", Cluster: "aks-it", Version: "1.9.0", StartedAt: started, Ready: true,
		Detail: map[string]any{"queue": "default", "claims_total": 12},
	}
	if err := UpsertServiceInstance(ctx, pool, row); err != nil {
		t.Fatal(err)
	}
	at, rows, err := GatewayListServiceInstances(ctx, pool, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, n := findInstance(rows, "scheduler", instance)
	if n != 1 {
		t.Fatalf("rows for the instance = %d, want 1", n)
	}
	if !got.Ready || got.Stale || got.PodNamespace != "kgf-nexus" || got.Node != "node-a" ||
		got.Cluster != "aks-it" || got.Version != "1.9.0" || !got.StartedAt.Equal(started) {
		t.Errorf("listed %+v", got)
	}
	if d := at.Sub(got.LastSeenAt); d < 0 || d > 5*time.Second {
		t.Errorf("last_seen_at %v is not the database's now (%v)", got.LastSeenAt, at)
	}
	var detail map[string]any
	if err := json.Unmarshal(got.Detail, &detail); err != nil || detail["queue"] != "default" {
		t.Errorf("detail = %s (%v)", got.Detail, err)
	}

	// The next beat updates the SAME row: shutting down, a new node.
	row.Ready, row.Node, row.Detail = false, "node-b", map[string]any{"shutting_down": true}
	if err := UpsertServiceInstance(ctx, pool, row); err != nil {
		t.Fatal(err)
	}
	_, rows, err = GatewayListServiceInstances(ctx, pool, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, n = findInstance(rows, "scheduler", instance)
	if n != 1 || got.Ready || got.Node != "node-b" || !strings.Contains(string(got.Detail), "shutting_down") {
		t.Fatalf("after the second beat: %d rows, %+v", n, got)
	}

	// Silent past the threshold, by the DATABASE clock: stale.
	mustExec(t, ctx, pool, `UPDATE service_instances SET last_seen_at = now() - interval '31 seconds' WHERE instance = $1`, instance)
	_, rows, _ = GatewayListServiceInstances(ctx, pool, 30*time.Second)
	if got, _ = findInstance(rows, "scheduler", instance); !got.Stale {
		t.Fatal("an instance silent for 31 s is not stale")
	}

	// A day silent: pruned.
	mustExec(t, ctx, pool, `UPDATE service_instances SET last_seen_at = now() - interval '25 hours' WHERE instance = $1`, instance)
	pruned, err := PruneServiceInstances(ctx, pool, 24*time.Hour)
	if err != nil || pruned < 1 {
		t.Fatalf("prune = %d, %v; want at least this instance", pruned, err)
	}
	_, rows, _ = GatewayListServiceInstances(ctx, pool, 30*time.Second)
	if _, n = findInstance(rows, "scheduler", instance); n != 0 {
		t.Fatal("a row unseen for 25 hours survived the prune")
	}
}

// TestIntegrationServiceInstanceIdentityIsTheKey: the same pod name in two
// clusters (two clusters sharing one database) is two instances.
func TestIntegrationServiceInstanceIdentityIsTheKey(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	instance := uniqueName(t)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM service_instances WHERE instance = $1`, instance) })
	for _, cluster := range []string{"aks-a", "aks-b"} {
		if err := UpsertServiceInstance(ctx, pool, ServiceInstance{
			Component: "dag-engine", Instance: instance, Cluster: cluster, StartedAt: time.Now(), Ready: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, rows, err := GatewayListServiceInstances(ctx, pool, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, n := findInstance(rows, "dag-engine", instance); n != 2 {
		t.Fatalf("rows = %d, want one per cluster", n)
	}
}

// asRole runs fn in a transaction as role (SET LOCAL ROLE), so a test proves
// what the service's own grants allow rather than the superuser's.
func asRole(t *testing.T, ctx context.Context, pool *pgxpool.Pool, service string, fn func(pgx.Tx) error) error {
	t.Helper()
	var role string
	if err := pool.QueryRow(ctx, `SELECT nexus_role($1)`, service).Scan(&role); err != nil {
		t.Fatalf("resolve the %s role: %v", service, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('role', $1, true)`, role); err != nil {
		t.Fatalf("set role %s: %v", role, err)
	}
	return fn(tx)
}

func isPermissionDenied(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}

// TestIntegrationEveryServiceWritesItsOwnInstanceRow is ADR-0037 §2's grant:
// every service role can upsert (INSERT ... ON CONFLICT DO UPDATE, which needs
// SELECT too) and prune, as itself.
func TestIntegrationEveryServiceWritesItsOwnInstanceRow(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	for _, svc := range []string{"api_gateway", "workflow_manager", "dag_engine", "scheduler", "resource_manager", "event_service"} {
		t.Run(svc, func(t *testing.T) {
			err := asRole(t, ctx, pool, svc, func(tx pgx.Tx) error {
				in := ServiceInstance{Component: strings.ReplaceAll(svc, "_", "-"), Instance: uniqueName(t), StartedAt: time.Now()}
				for i := 0; i < 2; i++ { // insert, then the conflict update
					if err := UpsertServiceInstance(ctx, tx, in); err != nil {
						return err
					}
				}
				_, err := PruneServiceInstances(ctx, tx, 24*time.Hour)
				return err
			})
			if err != nil {
				t.Fatalf("%s cannot write its registry row: %v", svc, err)
			}
		})
	}
}

// TestIntegrationOnlyTheGatewayWritesPlacement is 0042's ownership: every
// service reads attempt_runtime, only the api-gateway writes it.
func TestIntegrationOnlyTheGatewayWritesPlacement(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	for _, svc := range []string{"workflow_manager", "dag_engine", "scheduler", "resource_manager", "event_service"} {
		err := asRole(t, ctx, pool, svc, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT count(*) FROM attempt_runtime`); err != nil {
				t.Errorf("%s cannot read attempt_runtime: %v", svc, err)
			}
			_, err := tx.Exec(ctx, `DELETE FROM attempt_runtime WHERE attempt_id = gen_random_uuid()`)
			return err
		})
		if !isPermissionDenied(err) {
			t.Errorf("%s may write attempt_runtime (err %v); only the api-gateway owns it", svc, err)
		}
	}
	for _, priv := range []string{"INSERT", "UPDATE", "DELETE"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege(nexus_role('api_gateway'), 'attempt_runtime', $1)`, priv).Scan(&ok); err != nil || !ok {
			t.Errorf("the api-gateway lacks %s on attempt_runtime (%v)", priv, err)
		}
	}
}

// liveLease gives the fixture's attempt a live lease held by a fresh worker
// and moves the attempt to RUNNING on it, returning the lease the gateway's
// fence would.
func liveLease(t *testing.T, f *fixture, attemptID string, epoch int) (LeaseView, string) {
	t.Helper()
	workerID := f.worker()
	mustExec(t, f.ctx, f.pool, `UPDATE task_attempts SET state = 'RUNNING', worker_id = $2,
		dispatched_at = now(), started_at = now() WHERE id = $1`, attemptID, workerID)
	mustExec(t, f.ctx, f.pool, `DELETE FROM leases WHERE attempt_id = $1`, attemptID)
	mustExec(t, f.ctx, f.pool, `INSERT INTO leases (attempt_id, worker_id, token, expires_at, epoch)
		VALUES ($1, $2, 'tok', now() + interval '30 seconds', $3)`, attemptID, workerID, epoch)
	lv, err := GatewayVerifyLease(f.ctx, f.pool, attemptID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	return lv, workerID
}

func TestIntegrationAttemptPlacementIsFenced(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)
	attemptID := f.attempt("train", 1)
	lease, _ := liveLease(t, f, attemptID, 0)

	pending := AttemptPlacement{Executor: "kubernetes", Cluster: "aks-it", K8sNamespace: "nexus-tasks", Pod: "pod-1"}
	// As the gateway's own role: the INSERT, the tenant trigger (SECURITY
	// DEFINER) and the reads of leases and task_attempts all need to work.
	if err := asRole(t, ctx, pool, "api_gateway", func(tx pgx.Tx) error {
		ok, err := GatewayRecordAttemptPlacement(ctx, tx, lease, pending)
		if err == nil && !ok {
			t.Error("a live lease's report was not stored")
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		return err
	}); err != nil {
		t.Fatalf("the api-gateway cannot record a placement: %v", err)
	}

	type stored struct {
		tenant, run, executor, pod, node string
		epoch                            int
		first, last                      time.Time
	}
	read := func() stored {
		t.Helper()
		var s stored
		if err := pool.QueryRow(ctx, `SELECT tenant_id::text, run_id::text, executor, pod, k8s_node,
			lease_epoch, first_reported_at, reported_at FROM attempt_runtime WHERE attempt_id = $1`, attemptID).
			Scan(&s.tenant, &s.run, &s.executor, &s.pod, &s.node, &s.epoch, &s.first, &s.last); err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := read()
	var tenant string
	if err := pool.QueryRow(ctx, `SELECT tenant_id::text FROM task_attempts WHERE id = $1`, attemptID).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if first.tenant != tenant || first.run != f.runID || first.executor != "kubernetes" || first.node != "" {
		t.Fatalf("stored %+v; want the attempt's tenant and run", first)
	}

	// Scheduled: the node arrives; the first-report time stays.
	pending.K8sNode = "node-7"
	if ok, err := GatewayRecordAttemptPlacement(ctx, pool, lease, pending); err != nil || !ok {
		t.Fatalf("update = %v, %v", ok, err)
	}
	second := read()
	if second.node != "node-7" || !second.first.Equal(first.first) || second.last.Before(first.last) {
		t.Fatalf("after scheduling: %+v (first %+v)", second, first)
	}

	// A stored row from a newer epoch is never overwritten by this lease.
	mustExec(t, ctx, pool, `UPDATE attempt_runtime SET lease_epoch = 5 WHERE attempt_id = $1`, attemptID)
	if ok, err := GatewayRecordAttemptPlacement(ctx, pool, lease, AttemptPlacement{Executor: "docker"}); err != nil || ok {
		t.Fatalf("an older epoch's report = %v, %v; want ignored", ok, err)
	}
	if read().executor != "kubernetes" {
		t.Fatal("an older epoch overwrote a newer one's placement")
	}

	// The lease expires (by the database clock): the write's own fence holds.
	mustExec(t, ctx, pool, `UPDATE leases SET expires_at = now() - interval '1 second' WHERE attempt_id = $1`, attemptID)
	if ok, err := GatewayRecordAttemptPlacement(ctx, pool, lease, pending); err != nil || ok {
		t.Fatalf("an expired lease's report = %v, %v; want refused", ok, err)
	}

	// Refused before any SQL.
	if _, err := GatewayRecordAttemptPlacement(ctx, pool, lease, AttemptPlacement{Executor: "Not Valid"}); !errors.Is(err, ErrInvalidPlacement) {
		t.Fatalf("invalid placement = %v", err)
	}

	// Run deletion takes the placement with it.
	mustExec(t, ctx, pool, `DELETE FROM workflow_runs WHERE id = $1`, f.runID)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM attempt_runtime WHERE attempt_id = $1`, attemptID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("placement rows after run deletion = %d (%v)", n, err)
	}
}

// TestIntegrationNewLeaseEpochRestartsThePlacement: a re-dispatch of the same
// attempt under a new lease epoch replaces the placement, first-report time
// included.
func TestIntegrationNewLeaseEpochRestartsThePlacement(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)
	attemptID := f.attempt("train", 1)
	old, _ := liveLease(t, f, attemptID, 0)
	if ok, err := GatewayRecordAttemptPlacement(ctx, pool, old, AttemptPlacement{Executor: "subprocess", Pod: "agent-a"}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	mustExec(t, ctx, pool, `UPDATE attempt_runtime SET first_reported_at = now() - interval '1 hour' WHERE attempt_id = $1`, attemptID)

	newer, _ := liveLease(t, f, attemptID, 1)
	if ok, err := GatewayRecordAttemptPlacement(ctx, pool, old, AttemptPlacement{Executor: "subprocess", Pod: "zombie"}); err != nil || ok {
		t.Fatalf("the replaced lease could still write: %v %v", ok, err)
	}
	if ok, err := GatewayRecordAttemptPlacement(ctx, pool, newer, AttemptPlacement{Executor: "subprocess", Pod: "agent-b"}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	var pod string
	var epoch int
	var age time.Duration
	if err := pool.QueryRow(ctx, `SELECT pod, lease_epoch, extract(epoch FROM now() - first_reported_at)::bigint * interval '1 second'
		FROM attempt_runtime WHERE attempt_id = $1`, attemptID).Scan(&pod, &epoch, &age); err != nil {
		t.Fatal(err)
	}
	if pod != "agent-b" || epoch != 1 || age > time.Minute {
		t.Fatalf("pod=%s epoch=%d first-report age=%v; want the new lease's placement, from now", pod, epoch, age)
	}
}

func TestIntegrationAttemptRuntimePresent(t *testing.T) {
	ok, err := GatewayAttemptRuntimePresent(context.Background(), testPool(t))
	if err != nil || !ok {
		t.Fatalf("present = %v, %v; migration 0042 is applied in the test database", ok, err)
	}
}

func TestIntegrationTaskRunnersListing(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)

	// a1: RUNNING on Kubernetes, placed and reporting progress, dispatched 3m ago.
	a1 := f.attempt("train", 1)
	l1, w1 := liveLease(t, f, a1, 0)
	mustExec(t, ctx, pool, `UPDATE task_attempts SET dispatched_at = now() - interval '3 minutes',
		started_at = now() - interval '3 minutes' WHERE id = $1`, a1)
	if ok, err := GatewayRecordAttemptPlacement(ctx, pool, l1, AttemptPlacement{
		Executor: "kubernetes", Cluster: "aks-it", K8sNamespace: "nexus-tasks", Pod: "nexus-train-x7k2p", K8sNode: "aks-gpu-3",
	}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	mustExec(t, ctx, pool, `INSERT INTO attempt_progress (attempt_id, run_id, lease_epoch, message, reported_at)
		VALUES ($1, $2, 0, 'pod Running: task container running on node aks-gpu-3', now())`, a1, f.runID)

	// a2: DISPATCHED a minute ago, nothing reported (an old agent).
	a2 := f.attempt("extract", 1)
	mustExec(t, ctx, pool, `UPDATE task_attempts SET state = 'DISPATCHED', worker_id = $2,
		dispatched_at = now() - interval '1 minute' WHERE id = $1`, a2, w1)

	// a3: finished; never listed or counted.
	a3 := f.attempt("done", 1)
	mustExec(t, ctx, pool, `UPDATE task_attempts SET state = 'SUCCEEDED', worker_id = $2,
		dispatched_at = now(), ended_at = now() WHERE id = $1`, a3, w1)

	// Another namespace's running attempt: invisible here.
	other := newFixture(t, ctx, pool)
	b1 := other.attempt("train", 1)
	liveLease(t, other, b1, 0)

	at, sum, runners, err := GatewayListTaskRunners(ctx, pool, f.ns, 0)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(at) > time.Minute || at.IsZero() {
		t.Errorf("generated_at %v is not the database's now", at)
	}
	if sum.Running != 2 || sum.ByExecutor["kubernetes"] != 1 || sum.ByExecutor[""] != 1 ||
		sum.ByCluster["aks-it"] != 1 || sum.ByCluster[""] != 1 || len(sum.ByExecutor) != 2 {
		t.Errorf("summary = %+v", sum)
	}
	if len(runners) != 2 {
		t.Fatalf("runners = %d, want the namespace's two in-flight attempts: %+v", len(runners), runners)
	}
	// Newest dispatch first.
	if runners[0].AttemptID != a2 || runners[1].AttemptID != a1 {
		t.Fatalf("order = %s, %s; want a2 (newer dispatch) then a1", runners[0].AttemptID, runners[1].AttemptID)
	}
	d, k := runners[0], runners[1]
	if d.State != "DISPATCHED" || d.StartedAt != nil || d.Executor != "" || d.Pod != "" ||
		d.K8sNode != "" || d.PlacementReportedAt != nil || d.ProgressMessage != "" || d.WorkerID != w1 {
		t.Errorf("unreported runner = %+v", d)
	}
	if k.State != "RUNNING" || k.StartedAt == nil || k.Executor != "kubernetes" || k.Cluster != "aks-it" ||
		k.K8sNamespace != "nexus-tasks" || k.Pod != "nexus-train-x7k2p" || k.K8sNode != "aks-gpu-3" ||
		k.PlacementReportedAt == nil || k.Workflow != "fixture" || k.NodeID != "train" || k.RunID != f.runID ||
		!strings.HasPrefix(k.WorkerHost, "host-") || !strings.Contains(k.ProgressMessage, "task container running") {
		t.Errorf("placed runner = %+v", k)
	}

	// The limit bounds the page, never the summary.
	_, sum, runners, err = GatewayListTaskRunners(ctx, pool, f.ns, 1)
	if err != nil || len(runners) != 1 || runners[0].AttemptID != a2 || sum.Running != 2 {
		t.Fatalf("limit 1: %d runners, summary %+v, err %v", len(runners), sum, err)
	}

	// A namespace with nothing in flight: an empty page and zero counts.
	_, sum, runners, err = GatewayListTaskRunners(ctx, pool, uniqueNS(t, pool), 0)
	if err != nil || len(runners) != 0 || sum.Running != 0 || len(sum.ByExecutor) != 0 {
		t.Fatalf("empty namespace: %+v %+v %v", runners, sum, err)
	}
}
