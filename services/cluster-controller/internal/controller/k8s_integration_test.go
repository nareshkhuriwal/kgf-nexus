// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build k8s_integration

// End-to-end test against a LOCAL Kubernetes cluster (Docker Desktop or kind)
// and a disposable Postgres. It refuses to run against any other context.
//
//	NEXUS_TEST_DATABASE_URL=postgres://postgres:test@localhost:55531/nexus \
//	  go test -tags k8s_integration -run TestK8s -v ./services/cluster-controller/internal/controller/
//
// What is real here: the Kubernetes API server, ReplicaSet controller,
// pod-deletion-cost, RBAC (the controller runs impersonating a ServiceAccount
// bound ONLY by the roles in deploy/k8s/cluster-controller), Lease-based leader
// election, the resource-manager binary and its Postgres roles. What is
// simulated: worker agents (rows registered with the pod name as hostname and
// heartbeats touched) and task leases (inserted rows); pods run `pause`.
package controller

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	itNS        = "nexus-scaling-test"
	itDep       = "workers-cpu"
	itSA        = "nexus-cluster-controller"
	itClusterRB = "nexus-scaling-test-"
)

func repoRoot(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "../../../..")
}

// localKube refuses anything but a local cluster. A test that scales
// Deployments must never be pointed at a shared one by an ambient kubeconfig.
func localKube(t *testing.T) (*rest.Config, string) {
	t.Helper()
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	raw, err := cc.RawConfig()
	if err != nil {
		t.Skipf("no kubeconfig: %v", err)
	}
	ctxName := raw.CurrentContext
	if ctxName != "docker-desktop" && !strings.HasPrefix(ctxName, "kind-") {
		t.Fatalf("refusing to run against kube context %q: only docker-desktop or kind-* are allowed", ctxName)
	}
	cfg, err := cc.ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	host := cfg.Host
	if !strings.Contains(host, "127.0.0.1") && !strings.Contains(host, "localhost") && !strings.Contains(host, "kubernetes.docker.internal") {
		t.Fatalf("context %s points at %s, which is not local", ctxName, host)
	}
	return cfg, ctxName
}

type itEnv struct {
	t       *testing.T
	admin   kubernetes.Interface
	ctlCfg  *rest.Config
	db      *pgxpool.Pool
	gw      *pgxpool.Pool
	ns      string // Nexus namespace (tenant)
	queue   string
	rmAddr  string
	rmBin   string
	dsn     string
	rmMu    sync.Mutex
	rmCmd   *exec.Cmd
	stopBg  context.CancelFunc
	bgDone  chan struct{}
	runID   uuid.UUID
	nodeSeq int
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func setupIT(t *testing.T) *itEnv {
	cfg, ctxName := localKube(t)
	t.Logf("kube context %s (%s)", ctxName, cfg.Host)
	dsn := os.Getenv("NEXUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NEXUS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	e := &itEnv{t: t, dsn: dsn, ns: "k8sit-" + uuid.NewString()[:8]}
	e.queue = "q-" + e.ns
	admin, err := kubernetes.NewForConfig(cfg)
	must(t, err)
	e.admin = admin
	db, err := pgxpool.New(ctx, dsn)
	must(t, err)
	must(t, db.Ping(ctx))
	e.db = db
	gwCfg, _ := pgxpool.ParseConfig(dsn)
	gwCfg.ConnConfig.RuntimeParams["role"] = "nexus_api_gateway"
	e.gw, err = pgxpool.NewWithConfig(ctx, gwCfg)
	must(t, err)

	// Fresh namespace: a leftover from an aborted run is removed first.
	if _, err := admin.CoreV1().Namespaces().Get(ctx, itNS, metav1.GetOptions{}); err == nil {
		_ = admin.CoreV1().Namespaces().Delete(ctx, itNS, metav1.DeleteOptions{})
		e.wait("stale namespace deleted", 120*time.Second, func() bool {
			_, err := admin.CoreV1().Namespaces().Get(ctx, itNS, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		})
	}
	_, err = admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: itNS}}, metav1.CreateOptions{})
	must(t, err)
	t.Cleanup(e.cleanup)
	e.installRBAC()

	// The controller runs as the ServiceAccount, so a permission missing from
	// the shipped manifests fails this test instead of passing as cluster-admin.
	ctl := rest.CopyConfig(cfg)
	ctl.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + itNS + ":" + itSA}
	e.ctlCfg = ctl

	e.buildRM()
	e.startRM()
	return e
}

// installRBAC applies deploy/k8s/cluster-controller's RBAC with every
// namespace rewritten to the test namespace and cluster-scoped names prefixed.
func (e *itEnv) installRBAC() {
	ctx := context.Background()
	root := repoRoot(e.t)
	for _, f := range []string{"deploy/k8s/cluster-controller/base/rbac.yaml", "deploy/k8s/cluster-controller/apply/rbac-apply.yaml"} {
		raw, err := os.ReadFile(filepath.Join(root, f))
		must(e.t, err)
		dec := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
		for {
			doc, err := dec.Read()
			if err == io.EOF {
				break
			}
			must(e.t, err)
			if len(bytes.TrimSpace(doc)) == 0 {
				continue
			}
			obj, _, err := scheme.Codecs.UniversalDeserializer().Decode(doc, nil, nil)
			must(e.t, err)
			switch o := obj.(type) {
			case *corev1.ServiceAccount:
				o.Namespace = itNS
				_, err = e.admin.CoreV1().ServiceAccounts(itNS).Create(ctx, o, metav1.CreateOptions{})
			case *rbacv1.Role:
				o.Namespace = itNS
				_, err = e.admin.RbacV1().Roles(itNS).Create(ctx, o, metav1.CreateOptions{})
			case *rbacv1.RoleBinding:
				o.Namespace = itNS
				for i := range o.Subjects {
					o.Subjects[i].Namespace = itNS
				}
				_, err = e.admin.RbacV1().RoleBindings(itNS).Create(ctx, o, metav1.CreateOptions{})
			case *rbacv1.ClusterRole:
				o.Name = itClusterRB + o.Name
				_ = e.admin.RbacV1().ClusterRoles().Delete(ctx, o.Name, metav1.DeleteOptions{})
				_, err = e.admin.RbacV1().ClusterRoles().Create(ctx, o, metav1.CreateOptions{})
			case *rbacv1.ClusterRoleBinding:
				o.Name = itClusterRB + o.Name
				o.RoleRef.Name = itClusterRB + o.RoleRef.Name
				for i := range o.Subjects {
					o.Subjects[i].Namespace = itNS
				}
				_ = e.admin.RbacV1().ClusterRoleBindings().Delete(ctx, o.Name, metav1.DeleteOptions{})
				_, err = e.admin.RbacV1().ClusterRoleBindings().Create(ctx, o, metav1.CreateOptions{})
			default:
				e.t.Fatalf("unexpected object %T in %s", obj, f)
			}
			must(e.t, err)
		}
	}
}

func (e *itEnv) buildRM() {
	e.rmBin = filepath.Join(e.t.TempDir(), "resource-manager")
	cmd := exec.Command("go", "build", "-o", e.rmBin, "./services/resource-manager/cmd/resource-manager")
	cmd.Dir = repoRoot(e.t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("build resource-manager: %v\n%s", err, out)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(e.t, err)
	e.rmAddr = l.Addr().String()
	_ = l.Close()
}

// startRM runs the real resource-manager binary as its own Postgres role.
func (e *itEnv) startRM() {
	e.rmMu.Lock()
	defer e.rmMu.Unlock()
	sep := "?"
	if strings.Contains(e.dsn, "?") {
		sep = "&"
	}
	cmd := exec.Command(e.rmBin, "-grpc-addr", e.rmAddr, "-http-addr", "127.0.0.1:0",
		"-database-url", e.dsn+sep+"role=nexus_resource_manager", "-log-level", "warn")
	cmd.Stdout, cmd.Stderr = io.Discard, os.Stderr
	must(e.t, cmd.Start())
	e.rmCmd = cmd
	e.wait("resource manager listening", 30*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", e.rmAddr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	})
}

func (e *itEnv) stopRM() {
	e.rmMu.Lock()
	defer e.rmMu.Unlock()
	if e.rmCmd != nil && e.rmCmd.Process != nil {
		_ = e.rmCmd.Process.Kill()
		_ = e.rmCmd.Wait()
		e.rmCmd = nil
	}
}

func (e *itEnv) cleanup() {
	ctx := context.Background()
	if e.stopBg != nil {
		e.stopBg()
		<-e.bgDone
	}
	e.stopRM()
	_ = e.admin.CoreV1().Namespaces().Delete(ctx, itNS, metav1.DeleteOptions{})
	for _, n := range []string{"nexus-cluster-controller-nodes"} {
		_ = e.admin.RbacV1().ClusterRoleBindings().Delete(ctx, itClusterRB+n, metav1.DeleteOptions{})
		_ = e.admin.RbacV1().ClusterRoles().Delete(ctx, itClusterRB+n, metav1.DeleteOptions{})
	}
	for _, q := range []string{
		`DELETE FROM clusters WHERE namespace=$1`,
		`DELETE FROM leases WHERE worker_id IN (SELECT id FROM workers WHERE namespace=$1)`,
		`DELETE FROM reservations WHERE worker_id IN (SELECT id FROM workers WHERE namespace=$1)`,
		`DELETE FROM task_attempts WHERE run_id IN (SELECT id FROM workflow_runs WHERE namespace=$1)`,
		`DELETE FROM task_nodes WHERE run_id IN (SELECT id FROM workflow_runs WHERE namespace=$1)`,
		`DELETE FROM workflow_runs WHERE namespace=$1`,
		`DELETE FROM workflow_specs WHERE namespace=$1`,
		`DELETE FROM workers WHERE namespace=$1`,
	} {
		if _, err := e.db.Exec(ctx, q, e.ns); err != nil {
			e.t.Logf("cleanup %q: %v", q, err)
		}
	}
	e.gw.Close()
	e.db.Close()
}

func (e *itEnv) wait(what string, timeout time.Duration, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	e.t.Fatalf("timed out after %s waiting for: %s", timeout, what)
}

// simulateAgents registers a worker row for every Running pod (hostname = pod
// name, exactly what the agent does) and keeps heartbeats fresh.
func (e *itEnv) simulateAgents() {
	ctx, cancel := context.WithCancel(context.Background())
	e.stopBg, e.bgDone = cancel, make(chan struct{})
	go func() {
		defer close(e.bgDone)
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			pods, err := e.admin.CoreV1().Pods(itNS).List(ctx, metav1.ListOptions{LabelSelector: "app=nexus-worker"})
			if err == nil {
				var live []string
				for _, p := range pods.Items {
					if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
						continue
					}
					live = append(live, p.Name)
					_, _ = e.db.Exec(ctx, `INSERT INTO workers(namespace,hostname,queues,capacity,agent_version,session_token_hash)
						SELECT $1,$2,ARRAY[$3],'{"cpu_millis":2000}','it','it'
						 WHERE NOT EXISTS (SELECT 1 FROM workers WHERE namespace=$1 AND hostname=$2)`, e.ns, p.Name, e.queue)
				}
				_, _ = e.db.Exec(ctx, `UPDATE workers SET last_heartbeat_at=now() WHERE namespace=$1 AND hostname=ANY($2) AND state<>'DEAD'`, e.ns, live)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func (e *itEnv) seedTenantAndPolicy() {
	ctx := context.Background()
	_, err := e.db.Exec(ctx, `INSERT INTO tenants(namespace,name) VALUES($1,'k8s integration')`, e.ns)
	must(e.t, err)
	cfg := platform.Default()
	cfg.Profiles = []platform.PodProfile{{Name: "cpu", Cloud: "kubernetes", KubernetesNamespace: itNS, ServiceAccount: "tasks",
		Image: "example/t@sha256:" + strings.Repeat("a", 64), CPU: 1000, Memory: 1024, EphemeralStorage: 1024, DeadlineSeconds: 3600, GraceSeconds: 30}}
	cfg.Pools = []platform.Pool{{Name: "cpu", Queue: e.queue, Profile: "cpu", MinWorkers: 1, MaxWorkers: 10, TasksPerWorker: 2,
		IdleSeconds: 60, MaxScaleUpStep: 3, Enabled: true}}
	_, err = store.GatewayPlatformPut(ctx, e.db, e.ns, cfg, store.AuditEntry{ActorKind: "test"})
	must(e.t, err)
	spec := uuid.New()
	e.runID = uuid.New()
	_, err = e.db.Exec(ctx, `INSERT INTO workflow_specs(id,namespace,name,version,spec_hash,spec_json,created_by) VALUES($1,$2,'s',1,'h','{}','t')`, spec, e.ns)
	must(e.t, err)
	_, err = e.db.Exec(ctx, `INSERT INTO workflow_runs(id,spec_id,namespace,state,trigger_kind) VALUES($1,$2,$3,'RUNNING','t')`, e.runID, spec, e.ns)
	must(e.t, err)
}

// addAttempt inserts an attempt in the given queue and state; with worker set,
// it also takes a lease for that worker.
func (e *itEnv) addAttempt(queue, state string, workerHost string) uuid.UUID {
	ctx := context.Background()
	e.nodeSeq++
	node := fmt.Sprintf("n%d", e.nodeSeq)
	id := uuid.New()
	_, err := e.db.Exec(ctx, `INSERT INTO task_nodes(run_id,node_id,task_ref,spec_json) VALUES($1,$2,'t','{}')`, e.runID, node)
	must(e.t, err)
	var worker *uuid.UUID
	if workerHost != "" {
		var w uuid.UUID
		must(e.t, e.db.QueryRow(ctx, `SELECT id FROM workers WHERE namespace=$1 AND hostname=$2 ORDER BY registered_at DESC LIMIT 1`, e.ns, workerHost).Scan(&w))
		worker = &w
	}
	_, err = e.db.Exec(ctx, `INSERT INTO task_attempts(id,run_id,node_id,attempt_number,resource_request,idempotency_key,queue,state,worker_id)
		VALUES($1,$2,$3,1,'{"cpu_millis":500}',$3,$4,$5,$6)`, id, e.runID, node, queue, state, worker)
	must(e.t, err)
	if worker != nil {
		_, err = e.db.Exec(ctx, `INSERT INTO leases(attempt_id,worker_id,token,expires_at) VALUES($1,$2,'tok',now()+interval '1 hour')`, id, *worker)
		must(e.t, err)
	}
	return id
}

func (e *itEnv) replicas() int32 {
	d, err := e.admin.AppsV1().Deployments(itNS).Get(context.Background(), itDep, metav1.GetOptions{})
	must(e.t, err)
	return *d.Spec.Replicas
}

func (e *itEnv) runningPods() []string {
	pods, err := e.admin.CoreV1().Pods(itNS).List(context.Background(), metav1.ListOptions{LabelSelector: "app=nexus-worker"})
	must(e.t, err)
	var out []string
	for _, p := range pods.Items {
		if p.DeletionTimestamp == nil && podReady(&p) {
			out = append(out, p.Name)
		}
	}
	return out
}

func (e *itEnv) podExists(name string) bool {
	p, err := e.admin.CoreV1().Pods(itNS).Get(context.Background(), name, metav1.GetOptions{})
	return err == nil && p.DeletionTimestamp == nil
}

func (e *itEnv) statusIdentity() (string, time.Time) {
	var id string
	var at *time.Time
	_ = e.db.QueryRow(context.Background(), `SELECT s.controller_identity, s.last_report_at FROM cluster_status s JOIN clusters c ON c.id=s.cluster_id WHERE c.namespace=$1`, e.ns).Scan(&id, &at)
	if at == nil {
		return id, time.Time{}
	}
	return id, *at
}

// startController runs one leader-elected controller replica as the SA.
func (e *itEnv) startController(identity string, leading *Leading) context.CancelFunc {
	kube, err := kubernetes.NewForConfig(e.ctlCfg)
	must(e.t, err)
	conn, err := grpc.NewClient(e.rmAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	must(e.t, err)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})).With("replica", identity)
	c, err := New(Config{ClusterName: e.ns, Identity: identity, AllowApply: true, Interval: time.Second, RPCTimeout: 5 * time.Second},
		kube, rmv1.NewClusterControllerServiceClient(conn), log)
	must(e.t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = RunLeaderElected(ctx, kube, LeaderConfig{Namespace: itNS, LeaseName: "nexus-cluster-controller-it", Identity: identity,
			LeaseDuration: 4 * time.Second, RenewDeadline: 3 * time.Second, RetryPeriod: 500 * time.Millisecond}, leading, log, c.Run)
		_ = conn.Close()
	}()
	return func() { cancel(); <-done }
}

func TestK8sControllerEndToEnd(t *testing.T) {
	e := setupIT(t)
	ctx := context.Background()
	e.seedTenantAndPolicy()

	// A 1-replica worker Deployment of pause pods.
	one := int32(1)
	grace := int64(2)
	_, err := e.admin.AppsV1().Deployments(itNS).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: itDep, Namespace: itNS},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "nexus-worker"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "nexus-worker"}},
				Spec: corev1.PodSpec{TerminationGracePeriodSeconds: &grace, Containers: []corev1.Container{{
					Name: "agent", Image: "registry.k8s.io/pause:3.10",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: mustQ("10m"), corev1.ResourceMemory: mustQ("8Mi")}},
				}}},
			},
		},
	}, metav1.CreateOptions{})
	must(t, err)
	e.wait("first worker pod ready", 120*time.Second, func() bool { return len(e.runningPods()) == 1 })
	e.simulateAgents()
	e.wait("first worker registered", 30*time.Second, func() bool {
		var n int
		_ = e.db.QueryRow(ctx, `SELECT count(*) FROM workers WHERE namespace=$1`, e.ns).Scan(&n)
		return n == 1
	})

	// Namespace quota: at most 3 pods, whatever the demand.
	_, err = e.admin.CoreV1().ResourceQuotas(itNS).Create(ctx, &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-pods"},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourcePods: mustQ("3")}},
	}, metav1.CreateOptions{})
	must(t, err)
	e.wait("quota usage calculated", 60*time.Second, func() bool {
		q, err := e.admin.CoreV1().ResourceQuotas(itNS).Get(ctx, "worker-pods", metav1.GetOptions{})
		_, ok := q.Status.Used[corev1.ResourcePods]
		return err == nil && ok
	})

	// Register the cluster in APPLY mode with its real identity.
	ks, err := e.admin.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	must(t, err)
	_, err = store.GatewayClusterPut(ctx, e.gw, e.ns, "it", store.ClusterSpec{Name: e.ns, Provider: "kubernetes",
		ExpectedClusterUID: string(ks.UID), ControlPlaneNamespace: itNS, Mode: "apply",
		Bindings: []store.ClusterBindingSpec{{KubernetesNamespace: itNS, WorkerDeployment: itDep, Pool: "cpu", MaxReplicas: 10,
			MaxScaleDownStep: 1, ScaleUpCooldownSeconds: 0, ScaleDownCooldownSeconds: 60}}}, store.AuditEntry{ActorKind: "test"})
	must(t, err)

	// ── leader election with two replicas ──
	var la, lb Leading
	stopA := e.startController("ctl-a", &la)
	stopB := e.startController("ctl-b", &lb)
	e.wait("one replica leads", 30*time.Second, func() bool { return la.Is() || lb.Is() })
	time.Sleep(3 * time.Second)
	if la.Is() && lb.Is() {
		t.Fatal("both replicas lead")
	}
	leader, stopLeader, stopFollower, followerLeading := "ctl-a", stopA, stopB, &lb
	if lb.Is() {
		leader, stopLeader, stopFollower, followerLeading = "ctl-b", stopB, stopA, &la
	}
	e.wait("leader reports", 30*time.Second, func() bool { id, _ := e.statusIdentity(); return id == leader })

	// ── scale-out on demand, bounded by the namespace quota ──
	for i := 0; i < 10; i++ {
		e.addAttempt(e.queue, "PENDING", "")
	}
	// 10 pending / 2 per worker wants 5; step allows 4; the quota allows 3.
	e.wait("scaled out to the quota bound", 90*time.Second, func() bool { return e.replicas() == 3 && len(e.runningPods()) == 3 })
	time.Sleep(3 * time.Second)
	if r := e.replicas(); r != 3 {
		t.Fatalf("exceeded the quota bound: %d replicas", r)
	}
	var reason string
	_ = e.db.QueryRow(ctx, `SELECT reason FROM cluster_scaling_events e JOIN clusters c ON c.id=e.cluster_id
		WHERE c.namespace=$1 AND action='scale_out' AND applied ORDER BY e.id DESC LIMIT 1`, e.ns).Scan(&reason)
	if !strings.Contains(reason, "quota") {
		t.Fatalf("scale-out reason %q does not name the quota", reason)
	}

	// ── leader failover ──
	stopLeader()
	e.wait("standby takes over", 30*time.Second, func() bool { return followerLeading.Is() })
	follower := map[string]string{"ctl-a": "ctl-b", "ctl-b": "ctl-a"}[leader]
	e.wait("standby reports", 30*time.Second, func() bool { id, _ := e.statusIdentity(); return id == follower })

	// ── scale-in: drain first, never remove a lease holder ──
	e.wait("all three workers registered", 30*time.Second, func() bool {
		var n int
		_ = e.db.QueryRow(ctx, `SELECT count(*) FROM workers WHERE namespace=$1 AND state='ACTIVE'`, e.ns).Scan(&n)
		return n == 3
	})
	pods := e.runningPods()
	// Every worker holds a lease on work in ANOTHER queue, so the pool's own
	// queue is idle (scale-in is wanted) while each pod still runs something.
	for _, p := range pods {
		e.addAttempt("other-"+e.ns, "RUNNING", p)
	}
	_, err = e.db.Exec(ctx, `DELETE FROM task_attempts WHERE run_id=$1 AND queue=$2`, e.runID, e.queue)
	must(t, err)
	// Pre-age the persisted idle clock and cooldown instead of sleeping for them.
	e.wait("idle recorded", 30*time.Second, func() bool {
		var idle *time.Time
		_ = e.db.QueryRow(ctx, `SELECT idle_since FROM cluster_scaling_state s JOIN clusters c ON c.id=s.cluster_id WHERE c.namespace=$1`, e.ns).Scan(&idle)
		return idle != nil
	})
	_, err = e.db.Exec(ctx, `UPDATE cluster_scaling_state SET idle_since=now()-interval '10 minutes', last_scale_at=now()-interval '10 minutes'
		WHERE cluster_id=(SELECT id FROM clusters WHERE namespace=$1)`, e.ns)
	must(t, err)

	var victim string
	e.wait("one worker drained", 30*time.Second, func() bool {
		return e.db.QueryRow(ctx, `SELECT hostname FROM workers WHERE namespace=$1 AND state='DRAINING'`, e.ns).Scan(&victim) == nil
	})
	t.Logf("drained %s", victim)
	// It still holds a lease: several cycles later it must still be there.
	time.Sleep(6 * time.Second)
	if !e.podExists(victim) || e.replicas() != 3 {
		t.Fatalf("removed capacity while %s held a lease (replicas %d)", victim, e.replicas())
	}
	var holds string
	_ = e.db.QueryRow(ctx, `SELECT last_decision::text FROM cluster_scaling_state s JOIN clusters c ON c.id=s.cluster_id WHERE c.namespace=$1`, e.ns).Scan(&holds)
	if !strings.Contains(holds, "lease") {
		t.Fatalf("hold reason does not mention the lease: %s", holds)
	}
	// Only the drained worker may be DRAINING -- one batch at a time.
	var draining int
	_ = e.db.QueryRow(ctx, `SELECT count(*) FROM workers WHERE namespace=$1 AND state='DRAINING'`, e.ns).Scan(&draining)
	if draining != 1 {
		t.Fatalf("%d workers draining; expected exactly one batch", draining)
	}

	// Work finishes: the lease goes, and THAT pod -- not a busy one -- is removed.
	_, err = e.db.Exec(ctx, `DELETE FROM leases WHERE worker_id IN (SELECT id FROM workers WHERE namespace=$1 AND hostname=$2)`, e.ns, victim)
	must(t, err)
	e.wait("drained pod removed", 60*time.Second, func() bool { return e.replicas() == 2 && !e.podExists(victim) })
	for _, p := range pods {
		if p != victim && !e.podExists(p) {
			t.Fatalf("busy pod %s was removed instead of / as well as %s", p, victim)
		}
	}

	// ── controller restart: persisted cooldown holds across it ──
	stopFollower()
	var lc Leading
	stopC := e.startController("ctl-c", &lc)
	defer stopC()
	e.wait("restarted controller leads", 30*time.Second, func() bool { return lc.Is() })
	e.wait("restarted controller reports", 30*time.Second, func() bool { id, _ := e.statusIdentity(); return id == "ctl-c" })
	time.Sleep(5 * time.Second)
	_ = e.db.QueryRow(ctx, `SELECT count(*) FROM workers WHERE namespace=$1 AND state='DRAINING' AND hostname<>$2`, e.ns, victim).Scan(&draining)
	if draining != 0 {
		t.Fatalf("restarted controller ignored the 60s scale-down cooldown and drained again")
	}

	// ── resource-manager failure: nothing changes, then recovery ──
	_, before := e.statusIdentity()
	// Demand returns while the resource manager is down: nothing may happen.
	for i := 0; i < 10; i++ {
		e.addAttempt(e.queue, "PENDING", "")
	}
	e.stopRM()
	time.Sleep(6 * time.Second)
	if r := e.replicas(); r != 2 {
		t.Fatalf("replicas changed to %d while the resource manager was down", r)
	}
	e.startRM()
	e.wait("reports resume after recovery", 30*time.Second, func() bool { _, at := e.statusIdentity(); return at.After(before) })
	e.wait("scale-out resumes after recovery", 60*time.Second, func() bool { return e.replicas() == 3 })

	// The audit trail recorded what happened.
	var events string
	_ = e.db.QueryRow(ctx, `SELECT string_agg(action||':'||applied::text, ',' ORDER BY e.id) FROM cluster_scaling_events e JOIN clusters c ON c.id=e.cluster_id WHERE c.namespace=$1`, e.ns).Scan(&events)
	t.Logf("scaling events: %s", events)
	for _, want := range []string{"scale_out:true", "drain:true", "scale_in:true"} {
		if !strings.Contains(events, want) {
			t.Fatalf("event trail %q lacks %s", events, want)
		}
	}
}
