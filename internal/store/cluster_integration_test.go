// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
)

// rolePool connects as the given service role (0005), so the test exercises
// the table grants 0018 adds rather than superuser privileges that would let
// any ownership mistake pass unnoticed.
func rolePool(t *testing.T, role string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testdb.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["role"] = role
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		testdb.SkipUnreachable(t, err)
	}
	if err := p.Ping(context.Background()); err != nil {
		testdb.SkipUnreachable(t, err)
	}
	t.Cleanup(p.Close)
	return p
}

func seedClusterNamespace(t *testing.T, admin *pgxpool.Pool) (ns string, cfg platform.Config) {
	t.Helper()
	ctx := context.Background()
	ns = "cl-" + uuid.NewString()[:8]
	if _, err := admin.Exec(ctx, `INSERT INTO tenants(namespace,name) VALUES($1,'test')`, ns); err != nil {
		t.Fatal(err)
	}
	cfg = platform.Default()
	cfg.Profiles = []platform.PodProfile{{Name: "cpu", Cloud: "kubernetes", KubernetesNamespace: "workers", ServiceAccount: "tasks",
		Image: "example/t@sha256:" + strings.Repeat("a", 64), CPU: 1000, Memory: 1024, EphemeralStorage: 1024, DeadlineSeconds: 3600, GraceSeconds: 30}}
	cfg.Pools = []platform.Pool{{Name: "cpu", Queue: "q-" + ns, Profile: "cpu", MinWorkers: 1, MaxWorkers: 20, TasksPerWorker: 2,
		IdleSeconds: 60, MaxScaleUpStep: 3, Enabled: true}}
	var err error
	if cfg, err = GatewayPlatformPut(ctx, admin, ns, cfg, AuditEntry{ActorKind: "test"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = admin.Exec(c, `DELETE FROM clusters WHERE namespace=$1`, ns)
		_, _ = admin.Exec(c, `DELETE FROM task_attempts WHERE run_id IN (SELECT id FROM workflow_runs WHERE namespace=$1)`, ns)
		_, _ = admin.Exec(c, `DELETE FROM task_nodes WHERE run_id IN (SELECT id FROM workflow_runs WHERE namespace=$1)`, ns)
		_, _ = admin.Exec(c, `DELETE FROM workflow_runs WHERE namespace=$1`, ns)
		_, _ = admin.Exec(c, `DELETE FROM workflow_specs WHERE namespace=$1`, ns)
		_, _ = admin.Exec(c, `DELETE FROM workers WHERE namespace=$1`, ns)
	})
	return ns, cfg
}

func TestIntegrationClusterRegistryAndControllerAPI(t *testing.T) {
	admin := testPool(t)
	ctx := context.Background()
	ns, cfg := seedClusterNamespace(t, admin)
	gw := rolePool(t, "nexus_api_gateway")
	rm := NewRMStore(rolePool(t, "nexus_resource_manager"))

	// ── registry (gateway role) ──
	spec := ClusterSpec{Name: ns, Provider: "kubernetes", Mode: "plan",
		Bindings: []ClusterBindingSpec{{KubernetesNamespace: "workers", WorkerDeployment: "workers-cpu", Pool: "cpu", MaxReplicas: 5,
			MaxScaleDownStep: 1, ScaleUpCooldownSeconds: 30, ScaleDownCooldownSeconds: 120}}}
	saved, err := GatewayClusterPut(ctx, gw, ns, "tester", spec, AuditEntry{ActorKind: "test"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if saved.DesiredRevision != 1 {
		t.Fatalf("revision %d", saved.DesiredRevision)
	}
	if _, err := GatewayClusterPut(ctx, gw, ns, "tester", spec, AuditEntry{}); !errors.Is(err, ErrClusterConflict) {
		t.Fatalf("re-register with revision 0: %v, want conflict", err)
	}
	bad := spec
	bad.DesiredRevision, bad.Mode = saved.DesiredRevision, "apply"
	if _, err := GatewayClusterPut(ctx, gw, ns, "tester", bad, AuditEntry{}); !errors.Is(err, ErrClusterInvalid) {
		t.Fatalf("apply without UID accepted: %v", err)
	}
	unknown := saved
	unknown.Bindings = []ClusterBindingSpec{{KubernetesNamespace: "w", WorkerDeployment: "d", Pool: "gpu"}}
	if _, err := GatewayClusterPut(ctx, gw, ns, "tester", unknown, AuditEntry{}); !errors.Is(err, ErrClusterInvalid) {
		t.Fatalf("unknown pool accepted: %v", err)
	}

	// A binding cannot point at another tenant's namespace, even by direct SQL.
	var clusterID string
	if err := admin.QueryRow(ctx, `SELECT id FROM clusters WHERE name=$1`, ns).Scan(&clusterID); err != nil {
		t.Fatal(err)
	}
	other, _ := seedClusterNamespace(t, admin)
	if _, err := admin.Exec(ctx, `INSERT INTO cluster_namespace_bindings(cluster_id,namespace,kubernetes_namespace,worker_deployment,pool)
		VALUES($1,$2,'x','y','cpu')`, clusterID, other); err == nil {
		t.Fatal("cross-namespace binding accepted by the schema")
	}

	// The gateway role cannot write observed state; the RM role cannot write the registry.
	if _, err := gw.Exec(ctx, `INSERT INTO cluster_status(cluster_id) VALUES($1)`, clusterID); err == nil {
		t.Fatal("gateway role wrote observed state")
	}
	if _, err := rm.Pool().Exec(ctx, `UPDATE clusters SET mode='apply' WHERE id=$1`, clusterID); err == nil {
		t.Fatal("resource-manager role wrote the registry")
	}
	if _, err := rm.Pool().Exec(ctx, `DELETE FROM cluster_scaling_events`); err == nil {
		t.Fatal("scaling events are not append-only for their writer")
	}

	// ── assignment + observations (RM role) ──
	a, err := rm.ClusterAssignmentByName(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Bindings) != 1 || a.Bindings[0].EffectiveMax() != 5 || a.Bindings[0].Pool.Queue != cfg.Pools[0].Queue || a.Bindings[0].ConfigError != "" {
		t.Fatalf("assignment %+v", a.Bindings)
	}
	two := int32(2)
	desired, err := rm.ReportObservations(ctx, ObservationReport{ClusterID: clusterID, ControllerIdentity: "c-0",
		ObservedClusterUID: "uid-1", ObservedRevision: 1, EffectiveMode: "plan", CompleteKinds: []string{"Node", "WorkerDeployment"},
		Objects: []ObservedObject{{Kind: "Node", Name: "n1", Ready: true, SummaryJSON: []byte(`{"ready":true}`)},
			{Kind: "WorkerDeployment", Namespace: "workers", Name: "workers-cpu", Generation: 3, ObservedGeneration: 3, SpecReplicas: &two}}})
	if err != nil || desired != 1 {
		t.Fatalf("report: %d %v", desired, err)
	}
	// A later report with Node incomplete keeps the old Node row.
	if _, err := rm.ReportObservations(ctx, ObservationReport{ClusterID: clusterID, ControllerIdentity: "c-0",
		ObservedClusterUID: "uid-1", CompleteKinds: []string{"WorkerDeployment"}}); err != nil {
		t.Fatal(err)
	}
	views, err := GatewayClusterList(ctx, gw, ns)
	if err != nil || len(views) != 1 {
		t.Fatalf("list: %v %v", views, err)
	}
	v := views[0]
	kinds := map[string]int{}
	for _, o := range v.Observations {
		kinds[o.Kind]++
	}
	if kinds["Node"] != 1 || kinds["WorkerDeployment"] != 0 {
		t.Fatalf("snapshot replacement wrong: %v", kinds)
	}
	if v.Status == nil || v.Status.ObservedRevision != 1 {
		t.Fatalf("observed revision regressed: %+v", v.Status)
	}
	if _, err := rm.ReportObservations(ctx, ObservationReport{ClusterID: clusterID, ControllerIdentity: "c",
		CompleteKinds: []string{"Secret"}}); !errors.Is(err, ErrInvalidClusterReport) {
		t.Fatalf("unknown kind: %v", err)
	}

	// ── workers, leases, drain gate ──
	host := "workers-cpu-abc"
	var workerID, attempt uuid.UUID = uuid.New(), uuid.New()
	spec2, run := uuid.New(), uuid.New()
	for _, sql := range []string{
		`INSERT INTO workers(id,namespace,hostname,queues,capacity,agent_version,session_token_hash) VALUES('` + workerID.String() + `',$1,'` + host + `',ARRAY[$2],'{"cpu_millis":2000}','t','t')`,
		`INSERT INTO workflow_specs(id,namespace,name,version,spec_hash,spec_json,created_by) VALUES('` + spec2.String() + `',$1,'s',1,'h','{}','t')`,
		`INSERT INTO workflow_runs(id,spec_id,namespace,state,trigger_kind) VALUES('` + run.String() + `','` + spec2.String() + `',$1,'RUNNING','t')`,
	} {
		args := []any{ns}
		if strings.Contains(sql, "$2") {
			args = append(args, cfg.Pools[0].Queue)
		}
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.Exec(ctx, `INSERT INTO task_nodes(run_id,node_id,task_ref,spec_json) VALUES($1,'n','t','{}')`, run); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO task_attempts(id,run_id,node_id,attempt_number,resource_request,idempotency_key,queue,state,worker_id)
		VALUES($1,$2,'n',1,'{}','k',$3,'RUNNING',$4)`, attempt, run, cfg.Pools[0].Queue, workerID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO leases(attempt_id,worker_id,token,expires_at) VALUES($1,$2,'tok',now()+interval '1 minute')`, attempt, workerID); err != nil {
		t.Fatal(err)
	}

	states, err := rm.PodWorkerStates(ctx, clusterID, ns, []string{host, "unregistered"})
	if err != nil || states[host].Active != 1 || states[host].LeaseCount != 1 || states["unregistered"].Active != 0 {
		t.Fatalf("pod states %+v %v", states, err)
	}
	b := a.Bindings[0]
	if d, err := rm.PoolDemand(ctx, b); err != nil || d.Running != 1 || d.CurrentWorkers != 1 {
		t.Fatalf("demand %+v %v", d, err)
	}

	// Plan mode: the drain is refused by the resource manager itself.
	if _, _, err := rm.DrainPod(ctx, clusterID, ns, "workers", "workers-cpu", host, "uid-1", "test", "c-0"); !errors.Is(err, ErrClusterNotApplying) {
		t.Fatalf("drain in plan mode: %v", err)
	}
	apply := saved
	apply.Mode, apply.ExpectedClusterUID = "apply", "uid-1"
	if saved, err = GatewayClusterPut(ctx, gw, ns, "tester", apply, AuditEntry{ActorKind: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rm.DrainPod(ctx, clusterID, ns, "workers", "workers-cpu", host, "uid-OTHER", "test", "c-0"); !errors.Is(err, ErrClusterNotApplying) {
		t.Fatalf("drain from the wrong cluster: %v", err)
	}
	if _, _, err := rm.DrainPod(ctx, clusterID, ns, "workers", "not-bound", host, "uid-1", "test", "c-0"); !errors.Is(err, ErrBindingNotFound) {
		t.Fatalf("drain for an unbound deployment: %v", err)
	}
	drained, _, err := rm.DrainPod(ctx, clusterID, ns, "workers", "workers-cpu", host, "uid-1", "test", "c-0")
	if err != nil || drained != 1 {
		t.Fatalf("drain: %d %v", drained, err)
	}
	var state string
	_ = admin.QueryRow(ctx, `SELECT state FROM workers WHERE id=$1`, workerID).Scan(&state)
	if state != "DRAINING" {
		t.Fatalf("worker state %s", state)
	}
	// Drained but still holding a lease: NOT removable.
	if _, ok, why, err := rm.CheckPodRemovable(ctx, clusterID, ns, host); err != nil || ok || !strings.Contains(why, "lease") {
		t.Fatalf("removable with a live lease: ok=%v why=%q err=%v", ok, why, err)
	}
	if _, err := admin.Exec(ctx, `DELETE FROM leases WHERE attempt_id=$1`, attempt); err != nil {
		t.Fatal(err)
	}
	if _, ok, _, err := rm.CheckPodRemovable(ctx, clusterID, ns, host); err != nil || !ok {
		t.Fatalf("drained lease-free pod not removable: %v %v", ok, err)
	}

	// ── persisted controller memory ──
	from, to := int32(2), int32(1)
	idle := time.Now().Add(-5 * time.Minute).Truncate(time.Millisecond)
	if err := rm.RecordScaling(ctx, ScalingRecord{ClusterID: clusterID, KubernetesNamespace: "workers", WorkerDeployment: "workers-cpu",
		Action: "scale_in", Mode: "apply", Applied: true, FromReplicas: &from, ToReplicas: &to, ScaledDirection: "in",
		IdleUpdate: "set", IdleSince: idle, ObservedPlatformRevision: 1}); err != nil {
		t.Fatal(err)
	}
	// A second SET must not move the idle clock forward.
	if err := rm.RecordScaling(ctx, ScalingRecord{ClusterID: clusterID, KubernetesNamespace: "workers", WorkerDeployment: "workers-cpu",
		IdleUpdate: "set", IdleSince: time.Now()}); err != nil {
		t.Fatal(err)
	}
	a, err = rm.ClusterAssignmentByName(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	if a.Bindings[0].IdleSince == nil || !a.Bindings[0].IdleSince.Equal(idle) || a.Bindings[0].LastScaleDirection != "in" || a.Bindings[0].LastScaleAt == nil {
		t.Fatalf("scaling state not persisted: %+v", a.Bindings[0])
	}
	if err := rm.RecordScaling(ctx, ScalingRecord{ClusterID: clusterID, KubernetesNamespace: "x", WorkerDeployment: "y"}); !errors.Is(err, ErrBindingNotFound) {
		t.Fatalf("state for an unbound deployment: %v", err)
	}
	if err := rm.ReportReconcileError(ctx, clusterID, "workers/workers-cpu", "boom", "c-0"); err != nil {
		t.Fatal(err)
	}
	views, _ = GatewayClusterList(ctx, gw, ns)
	v = views[0]
	if len(v.Events) < 2 || len(v.Errors) != 1 || v.Status.LastError == "" || !v.Status.IdentityVerified || v.Status.ObservedRevision >= v.DesiredRevision {
		t.Fatalf("view: events=%d errors=%d status=%+v desired=%d", len(v.Events), len(v.Errors), v.Status, v.DesiredRevision)
	}

	// ── delete ──
	if err := GatewayClusterDelete(ctx, gw, ns, ns, saved.DesiredRevision, AuditEntry{}); !errors.Is(err, ErrClusterInvalid) {
		t.Fatalf("deleted a cluster in apply mode: %v", err)
	}
	plan := saved
	plan.Mode = "plan"
	if saved, err = GatewayClusterPut(ctx, gw, ns, "tester", plan, AuditEntry{}); err != nil {
		t.Fatal(err)
	}
	if err := GatewayClusterDelete(ctx, gw, other, ns, saved.DesiredRevision, AuditEntry{}); !errors.Is(err, ErrClusterMissing) {
		t.Fatalf("another namespace deleted the cluster: %v", err)
	}
	if err := GatewayClusterDelete(ctx, gw, ns, ns, saved.DesiredRevision, AuditEntry{ActorKind: "test"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var left int
	_ = admin.QueryRow(ctx, `SELECT count(*) FROM cluster_observations WHERE cluster_id=$1`, clusterID).Scan(&left)
	if left != 0 {
		t.Fatalf("observations not removed with the cluster")
	}
}

// TestIntegrationPodWorkersAreClusterScoped pins the pod <-> worker join to
// the cluster: two clusters registered by one namespace, whose pods share a
// name, never see or drain each other's workers. A worker naming no cluster
// (an agent without NEXUS_CLUSTER_NAME) still matches, as before.
func TestIntegrationPodWorkersAreClusterScoped(t *testing.T) {
	admin := testPool(t)
	ctx := context.Background()
	ns, cfg := seedClusterNamespace(t, admin)
	gw := rolePool(t, "nexus_api_gateway")
	rm := NewRMStore(rolePool(t, "nexus_resource_manager"))

	east, west := ns+"-east", ns+"-west"
	for _, name := range []string{east, west} {
		if _, err := GatewayClusterPut(ctx, gw, ns, "tester", ClusterSpec{Name: name, Provider: "kubernetes", Mode: "apply",
			ExpectedClusterUID: "uid-" + name}, AuditEntry{ActorKind: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	// One pool may be bound to one cluster only; bind it to east.
	spec := ClusterSpec{Name: east, Provider: "kubernetes", Mode: "apply", ExpectedClusterUID: "uid-" + east, DesiredRevision: 1,
		Bindings: []ClusterBindingSpec{{KubernetesNamespace: "workers", WorkerDeployment: "workers-cpu", Pool: cfg.Pools[0].Name,
			MaxReplicas: 5, MaxScaleDownStep: 1, ScaleUpCooldownSeconds: 30, ScaleDownCooldownSeconds: 120}}}
	if _, err := GatewayClusterPut(ctx, gw, ns, "tester", spec, AuditEntry{ActorKind: "test"}); err != nil {
		t.Fatal(err)
	}
	var eastID string
	if err := admin.QueryRow(ctx, `SELECT id FROM clusters WHERE name=$1`, east).Scan(&eastID); err != nil {
		t.Fatal(err)
	}
	// The same pod name in both clusters, plus an agent that names no cluster.
	for host, labels := range map[string]string{
		"workers-cpu-1": `{"nexus.io/cluster":"` + east + `"}`,
		"workers-cpu-2": `{"nexus.io/cluster":"` + west + `"}`,
		"workers-cpu-3": `{}`,
	} {
		if _, err := admin.Exec(ctx, `INSERT INTO workers(id,namespace,hostname,queues,capacity,agent_version,session_token_hash,labels)
			VALUES($1,$2,$3,ARRAY[$4],'{"cpu_millis":1000}','t','t',$5)`, uuid.New(), ns, host, cfg.Pools[0].Queue, labels); err != nil {
			t.Fatal(err)
		}
	}
	// west's worker shares east's pod name.
	if _, err := admin.Exec(ctx, `INSERT INTO workers(id,namespace,hostname,queues,capacity,agent_version,session_token_hash,labels)
		VALUES($1,$2,'workers-cpu-1',ARRAY[$3],'{"cpu_millis":1000}','t','t',$4)`, uuid.New(), ns, cfg.Pools[0].Queue,
		`{"nexus.io/cluster":"`+west+`"}`); err != nil {
		t.Fatal(err)
	}

	states, err := rm.PodWorkerStates(ctx, eastID, ns, []string{"workers-cpu-1", "workers-cpu-2", "workers-cpu-3"})
	if err != nil {
		t.Fatal(err)
	}
	if states["workers-cpu-1"].Active != 1 || states["workers-cpu-2"].Active != 0 || states["workers-cpu-3"].Active != 1 {
		t.Fatalf("east sees %+v; want its own pod (once, not west's namesake), not west's, and the unlabelled one", states)
	}
	drained, _, err := rm.DrainPod(ctx, eastID, ns, "workers", "workers-cpu", "workers-cpu-1", "uid-"+east, "test", "ctl")
	if err != nil || drained != 1 {
		t.Fatalf("drain: %d %v", drained, err)
	}
	var westState string
	if err := admin.QueryRow(ctx, `SELECT state FROM workers WHERE namespace=$1 AND hostname='workers-cpu-1' AND labels->>'nexus.io/cluster'=$2`,
		ns, west).Scan(&westState); err != nil {
		t.Fatal(err)
	}
	if westState != "ACTIVE" {
		t.Fatalf("east's drain reached west's worker of the same pod name: %s", westState)
	}
	if drained, _, err = rm.DrainPod(ctx, eastID, ns, "workers", "workers-cpu", "workers-cpu-2", "uid-"+east, "test", "ctl"); err != nil || drained != 0 {
		t.Fatalf("east drained west's pod: %d %v", drained, err)
	}
	// Leave apply mode so the cleanup can delete the clusters.
	for _, name := range []string{east, west} {
		_, _ = admin.Exec(ctx, `UPDATE clusters SET mode='plan' WHERE name=$1`, name)
	}
}
