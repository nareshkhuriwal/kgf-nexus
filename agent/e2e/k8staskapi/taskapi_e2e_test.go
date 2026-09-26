//go:build k8s_e2e

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package k8staskapi proves F148 closed end to end: a task running as a
// Kubernetes pod on a local cluster reaches the agent's workload API over the
// network listener (ADR-0032 addendum) and everything it does -- usage units,
// shared rate-limit permits, checkpoints and the resume of a retry -- lands in
// the ledger and the database, through the REAL control-plane binaries built
// from this checkout. It then attacks the transport from outside: a token
// after its attempt ended, a token of another attempt, a token presented to
// another tenant's agent.
//
// What it needs, all opt-in:
//
//	NEXUS_TEST_DATABASE_URL   an ALREADY MIGRATED database the test may write
//	                          (it registers two throwaway tenants; it never
//	                          creates or drops a database)
//	NEXUS_TEST_S3_ENDPOINT, NEXUS_TEST_S3_ACCESS_KEY_ID, NEXUS_TEST_S3_SECRET_ACCESS_KEY
//	NEXUS_K8S_CONFORMANCE=1   and a LOCAL kubectl context (k8stest's guard)
//	NEXUS_K8S_TEST_NAMESPACE  the namespace task pods run in (created and
//	                          deleted by the guard)
//	NEXUS_E2E_TASK_API_PORTS  optional "a,b": host ports of the two agents'
//	                          listeners (default: free ports)
//	NEXUS_E2E_IMAGE_NODE      node container to load the task image into
//	                          (default desktop-control-plane on docker-desktop)
//	NEXUS_E2E_HOST_FROM_PODS  how pods reach this host (default
//	                          host.docker.internal, Docker Desktop)
//
//	go test -tags k8s_e2e -count=1 -v -timeout 20m ./agent/e2e/k8staskapi/
package k8staskapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor/k8stest"
	"github.com/khuriwalgroup/kgf-nexus/internal/tests3"
)

func TestMain(m *testing.M) {
	code := m.Run()
	k8stest.Teardown()
	os.Exit(code)
}

func need(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Skipf("skipping kubernetes workload-API end-to-end test: %s is not set", k)
	}
	return v
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func freePort(t *testing.T, host string) string {
	t.Helper()
	l, err := net.Listen("tcp", host+":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// ── the test ────────────────────────────────────────────────────────────────

func TestKubernetesTaskUsesTheWorkloadAPIEndToEnd(t *testing.T) {
	dsn := need(t, "NEXUS_TEST_DATABASE_URL")
	s3c := tests3.Get(t)
	cs, why := k8stest.Client()
	if why != "" {
		t.Skip(why)
	}
	podHost := os.Getenv("NEXUS_E2E_HOST_FROM_PODS")
	if podHost == "" {
		podHost = "host.docker.internal"
	}
	portA, portB := "", ""
	if p := os.Getenv("NEXUS_E2E_TASK_API_PORTS"); p != "" {
		portA, portB, _ = strings.Cut(p, ",")
	} else {
		portA, portB = freePort(t, "0.0.0.0"), freePort(t, "0.0.0.0")
	}

	root := repoRoot()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// ── tenants and keys ────────────────────────────────────────────────────
	suffix := randHex(3)
	nsA, nsB := "k8s-e2e-a-"+suffix, "k8s-e2e-b-"+suffix
	adminKey := "nxs_admin_" + randHex(32)
	workerA, workerB := "nxs_worker_"+randHex(32), "nxs_worker_"+randHex(32)
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close(context.Background())
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO tenants (namespace, name) VALUES ($1,$1), ($2,$2) ON CONFLICT DO NOTHING", []any{nsA, nsB}},
		{"INSERT INTO api_keys (namespace, name, key_hash, role) VALUES ($1,'e2e-admin',$2,'admin')", []any{nsA, sha(adminKey)}},
		{"INSERT INTO api_keys (namespace, name, key_hash, role) VALUES ($1,'e2e-worker',$2,'worker')", []any{nsA, sha(workerA)}},
		{"INSERT INTO api_keys (namespace, name, key_hash, role) VALUES ($1,'e2e-worker',$2,'worker')", []any{nsB, sha(workerB)}},
	} {
		if _, err := db.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), "UPDATE api_keys SET revoked_at = now() WHERE namespace IN ($1,$2)", nsA, nsB)
	})

	// ── the task image ──────────────────────────────────────────────────────
	image := os.Getenv("NEXUS_E2E_TASK_IMAGE")
	if image == "" {
		image = "nexus-k8s-e2e-task:" + suffix
		build := exec.CommandContext(ctx, "docker", "build", "-q", "-f", "agent/e2e/k8staskapi/task/Dockerfile", "-t", image, ".")
		build.Dir = root
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("docker build: %v\n%s", err, out)
		}
		t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", image).Run() })
		// Docker Desktop's kind-provisioned cluster does not share the docker
		// image store: load the image into the node's containerd.
		if node := nodeForImages(); node != "" {
			load := exec.CommandContext(ctx, "sh", "-c",
				`docker save "$1" | docker exec -i "$2" ctr -n k8s.io images import -`, "load", image, node)
			if out, err := load.CombinedOutput(); err != nil {
				t.Fatalf("load %s into node %s: %v\n%s", image, node, err, out)
			}
			t.Cleanup(func() {
				_ = exec.Command("docker", "exec", node, "ctr", "-n", "k8s.io", "images", "rm",
					"docker.io/library/"+image).Run()
			})
		}
	}

	// ── binaries ────────────────────────────────────────────────────────────
	bin := t.TempDir()
	build := exec.CommandContext(ctx, "go", "build", "-o", bin,
		"./services/api-gateway/cmd/api-gateway", "./services/workflow-manager/cmd/workflow-manager",
		"./services/dag-engine", "./services/scheduler", "./services/resource-manager/cmd/resource-manager",
		"./agent/cmd/nexus-agent")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	bucket := "nexus-k8s-e2e-" + suffix
	createBucket(t, s3c, bucket)
	store := []string{
		"NEXUS_RESULT_STORE_BACKEND=s3",
		"NEXUS_RESULT_STORE_ENDPOINT=" + s3c.Endpoint,
		"NEXUS_RESULT_STORE_BUCKET=" + bucket,
		"NEXUS_RESULT_STORE_REGION=us-east-1",
		"NEXUS_S3_ACCESS_KEY_ID=" + s3c.AccessKeyID,
		"NEXUS_S3_SECRET_ACCESS_KEY=" + s3c.SecretAccessKey,
	}
	common := []string{"DATABASE_URL=" + dsn, "NEXUS_LOG_LEVEL=info"}
	loop := func() string { return "127.0.0.1:" + freePort(t, "127.0.0.1") }
	rmGRPC, wfmGRPC, deGRPC, schGRPC := loop(), loop(), loop(), loop()
	gwREST, gwGRPC := loop(), loop()
	logDir := t.TempDir()
	start := func(name, logName string, env ...string) {
		t.Helper()
		f, _ := os.Create(filepath.Join(logDir, logName+".log"))
		cmd := exec.Command(filepath.Join(bin, name))
		cmd.Env = append(append(os.Environ(), common...), env...)
		cmd.Stdout, cmd.Stderr = f, f
		if err := cmd.Start(); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Signal(os.Interrupt)
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
			}
			f.Close()
			if t.Failed() {
				b, _ := os.ReadFile(f.Name())
				t.Logf("── %s log (tail) ──\n%s", logName, tail(string(b), 6000))
			}
		})
	}
	start("resource-manager", "resource-manager", "RM_GRPC_ADDR="+rmGRPC, "RM_HTTP_ADDR="+loop())
	start("scheduler", "scheduler", "NEXUS_GRPC_ADDR="+schGRPC, "NEXUS_HTTP_ADDR="+loop())
	start("workflow-manager", "workflow-manager", "NEXUS_WFM_GRPC_ADDR="+wfmGRPC, "NEXUS_WFM_HEALTH_ADDR="+loop(),
		"NEXUS_DAG_ENGINE_ADDR="+deGRPC, "NEXUS_SCHEDULER_ADDR="+schGRPC)
	start("dag-engine", "dag-engine", "NEXUS_GRPC_ADDR="+deGRPC, "NEXUS_HTTP_ADDR="+loop(),
		"NEXUS_WORKFLOW_MANAGER_ADDR="+wfmGRPC)
	start("api-gateway", "api-gateway", append(store,
		"NEXUS_REST_ADDR="+gwREST, "NEXUS_GRPC_ADDR="+gwGRPC, "NEXUS_METRICS_ADDR=off",
		"NEXUS_WORKFLOW_MANAGER_ADDR="+wfmGRPC, "NEXUS_RESOURCE_MANAGER_ADDR="+rmGRPC,
		"NEXUS_SCHEDULER_ADDR="+schGRPC, "NEXUS_DAG_ENGINE_ADDR="+deGRPC,
		"NEXUS_EMAIL_VERIFICATION_REQUIRED=false")...)
	api := "http://" + gwREST
	waitHTTP(t, api+"/healthz", 90*time.Second)

	// Two agents, one per tenant, both running the kubernetes executor with
	// their own network listener. Only tenant A gets work.
	agent := func(tag, ns, key, port string) {
		work := t.TempDir()
		start("nexus-agent", "agent-"+tag, append(store,
			"NEXUS_CONTROL_PLANE="+gwGRPC, "NEXUS_API_KEY="+key, "NEXUS_NAMESPACE="+ns,
			"NEXUS_QUEUES=default", "NEXUS_CPU_MILLIS=4000", "NEXUS_MEMORY_BYTES=4294967296",
			"NEXUS_METRICS_ADDR="+loop(),
			"NEXUS_WAL_DIR="+filepath.Join(work, "wal"), "NEXUS_WORK_DIR="+filepath.Join(work, "work"),
			"NEXUS_K8S_NAMESPACE="+k8stest.Namespace, "NEXUS_K8S_CONTEXT="+k8stest.ContextName(),
			"NEXUS_K8S_OWNER=k8s-e2e-"+tag+"-"+suffix, "NEXUS_K8S_IMAGE_PULL_POLICY=Never",
			"NEXUS_K8S_START_TIMEOUT=3m",
			"NEXUS_K8S_TASK_API_ADVERTISE="+net.JoinHostPort(podHost, port),
			"NEXUS_K8S_TASK_API_LISTEN=0.0.0.0:"+port,
			// So this test, on the host, can verify the certificate too.
			"NEXUS_K8S_TASK_API_EXTRA_NAMES=127.0.0.1")...)
	}
	agent("a", nsA, workerA, portA)
	agent("b", nsB, workerB, portB)
	waitWorkerWithKubernetes(t, ctx, db, nsA)
	waitWorkerWithKubernetes(t, ctx, db, nsB)

	// ── a shared rate limit on the key the task asks for ────────────────────
	_, qb := call(t, "GET", api+"/v1/namespaces/"+nsA+"/quotas", adminKey, nil)
	var qdoc struct{ Revision int64 }
	_ = json.Unmarshal(qb, &qdoc)
	if code, body := call(t, "PUT", api+"/v1/namespaces/"+nsA+"/quotas", adminKey, map[string]any{
		"revision": qdoc.Revision,
		"quotas":   map[string]any{"rate_limits": map[string]any{"mock": map[string]any{"requests_per_second": 20}}},
	}); code != http.StatusOK {
		t.Fatalf("set rate limits: %d %s", code, body)
	}

	// ── the workflow ────────────────────────────────────────────────────────
	specName := "k8s-e2e-" + suffix
	spec := map[string]any{
		"api_version": "nexus.dev/v1", "name": specName, "namespace": nsA,
		"tasks": []any{map[string]any{
			"id": "work", "task_ref": "k8s_e2e.work", "version": 1, "queue": "default",
			"runtime": map[string]any{
				"executor": "kubernetes", "image": image,
				"command": []string{"python3", "-m", "nexus.entrypoint", "--module", "/opt/nexus/task/workload.py"},
				"args":    []string{"k8s_e2e.work"},
				"env":     map[string]string{"E2E_HOLD_S": "15"},
			},
			"outputs":   map[string]any{"result": map[string]any{"type": "json", "required": true}},
			"resources": map[string]any{"request": map[string]any{"cpu_millis": 100, "memory_bytes": 134217728}},
			"retry": map[string]any{"max_attempts": 2, "initial_ms": 1000, "max_ms": 2000,
				"retry_on": []string{"ERROR_CLASS_TRANSIENT"}},
			"timeouts": map[string]any{"start_to_close_ms": 300000, "heartbeat_ms": 120000},
		}},
	}
	if code, body := call(t, "POST", api+"/v1/namespaces/"+nsA+"/specs", adminKey, spec); code/100 != 2 {
		t.Fatalf("submit spec: %d %s", code, body)
	}
	code, body := call(t, "POST", api+"/v1/namespaces/"+nsA+"/runs", adminKey, map[string]any{"spec_name": specName})
	if code/100 != 2 {
		t.Fatalf("create run: %d %s", code, body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)
	if created.ID == "" {
		t.Fatalf("create run: no id in %s", body)
	}
	runID := created.ID
	t.Logf("run %s in %s; task pods in %s reach %s:%s", runID, nsA, k8stest.Namespace, podHost, portA)

	// ── steal each attempt's token while it runs ────────────────────────────
	thief := newThief(cs, runID)
	go thief.watch(ctx)

	a1 := thief.await(t, 1, 3*time.Minute)
	t.Logf("attempt 1 %s holds its token", a1.attemptID)
	listenerA := "https://127.0.0.1:" + portA
	hcA := pinned(t, a1.caPEM)
	// The token acts as exactly its own attempt.
	if code, info := apiCall(t, hcA, listenerA, "GET", "/v1/info", a1.token, nil); code != 200 || info["attempt_id"] != a1.attemptID {
		t.Fatalf("attempt 1's token on its own agent: %d %v", code, info)
	}
	// Another tenant's agent never minted it. (TLS verification is not what
	// is under test here; the other agent's CA is not in hand.)
	listenerB := "https://127.0.0.1:" + portB
	if code, body := apiCall(t, insecure(), listenerB, "GET", "/v1/info", a1.token, nil); code != http.StatusUnauthorized {
		t.Fatalf("tenant A's token on tenant B's agent: %d %v; want 401", code, body)
	}
	// Tenant B's agent does not present a certificate tenant A's pods trust.
	if _, err := hcA.Get(listenerB + "/v1/info"); err == nil {
		t.Fatal("tenant A's pinned CA accepted tenant B's agent")
	}

	a2 := thief.await(t, 2, 3*time.Minute)
	t.Logf("attempt 2 %s holds its token", a2.attemptID)
	// Attempt 1 has ended (its lease with it): its token is revoked, and it
	// cannot act on the live attempt 2 either.
	if code, body := apiCall(t, hcA, listenerA, "POST", "/v1/usage", a1.token,
		map[string]any{"units": map[string]int{"units:e2e_stolen": 1}}); code != http.StatusUnauthorized {
		t.Fatalf("attempt 1's token after its attempt ended: %d %v; want 401", code, body)
	}
	// Attempt 2's live token works -- as attempt 2, and only while it lives.
	if code, body := apiCall(t, pinned(t, a2.caPEM), listenerA, "POST", "/v1/usage", a2.token,
		map[string]any{"units": map[string]int{"units:e2e_probe": 1}}); code != 200 {
		t.Fatalf("attempt 2's token while it runs: %d %v", code, body)
	}
	if code, _ := apiCall(t, insecure(), listenerB, "GET", "/v1/info", a2.token, nil); code != http.StatusUnauthorized {
		t.Fatalf("tenant A's attempt-2 token on tenant B's agent: %d; want 401", code)
	}
	// The task's heartbeats (ctx.heartbeat -> ReportProgress through the
	// listener) reach the attempt while it runs. Only the latest message is
	// stored, and the executor's own pod-phase notes overwrite it at the end,
	// so it is read during the attempt's hold.
	heartbeat := false
	for hbDeadline := time.Now().Add(60 * time.Second); time.Now().Before(hbDeadline) && !heartbeat; {
		var msg string
		_ = db.QueryRow(ctx, `SELECT coalesce(message, '') FROM attempt_progress WHERE attempt_id = $1`, a2.attemptID).Scan(&msg)
		heartbeat = strings.HasPrefix(msg, "call ")
		if !heartbeat {
			time.Sleep(250 * time.Millisecond)
		}
	}

	// ── the run ends ────────────────────────────────────────────────────────
	var run map[string]any
	deadline := time.Now().Add(4 * time.Minute)
	for {
		_, b := call(t, "GET", api+"/v1/namespaces/"+nsA+"/runs/"+runID, adminKey, nil)
		run = map[string]any{}
		_ = json.Unmarshal(b, &run)
		state, _ := run["state"].(string)
		if state == "SUCCEEDED" {
			break
		}
		if (state == "FAILED" || state == "CANCELLED" || state == "TIMED_OUT") || time.Now().After(deadline) {
			t.Fatalf("run ended %q: %s", state, b)
		}
		time.Sleep(time.Second)
	}
	if code, _ := apiCall(t, pinned(t, a2.caPEM), listenerA, "GET", "/v1/info", a2.token, nil); code != http.StatusUnauthorized {
		t.Fatalf("attempt 2's token after the run ended: %d; want 401", code)
	}

	// ── what reached the database ───────────────────────────────────────────
	perCall := map[string]int64{"tokens:mock/e2e:input": 10, "tokens:mock/e2e:output": 5, "requests:mock/e2e": 1}
	want := func(n int64, extra map[string]int64) map[string]int64 {
		m := map[string]int64{}
		for k, v := range perCall {
			m[k] = v * n
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for _, c := range []struct {
		a     stolen
		state string
		units map[string]int64
	}{
		// Attempt 1 made 3 calls and failed; attempt 2 RESUMED from its
		// checkpoint and made only the other 3 -- plus the probe unit this
		// test posted with attempt 2's stolen token, which landed on attempt
		// 2 and nowhere else.
		{a1, "FAILED", want(3, nil)},
		{a2, "SUCCEEDED", want(3, map[string]int64{"units:e2e_probe": 1})},
	} {
		var state string
		if err := db.QueryRow(ctx, `SELECT state FROM task_attempts WHERE id = $1`, c.a.attemptID).Scan(&state); err != nil || state != c.state {
			t.Errorf("attempt %d state = %q (%v); want %s", c.a.number, state, err, c.state)
		}
		stored := jsonUnits(t, db, `SELECT units FROM attempt_usage WHERE attempt_id = $1`, c.a.attemptID)
		if !equal(stored, c.units) {
			t.Errorf("attempt %d attempt_usage = %v; want %v", c.a.number, stored, c.units)
		}
		ledger := usageOnly(jsonUnits(t, db, `SELECT custom FROM usage_ledger WHERE attempt_id = $1`, c.a.attemptID))
		if !equal(ledger, c.units) {
			t.Errorf("attempt %d usage_ledger.custom = %v; want %v", c.a.number, ledger, c.units)
		}
	}
	if _, stolenLanded := jsonUnits(t, db, `SELECT units FROM attempt_usage WHERE attempt_id = $1`, a1.attemptID)["units:e2e_stolen"]; stolenLanded {
		t.Error("a unit posted with attempt 1's revoked token was recorded")
	}

	// The checkpoint pointer is attempt 2's, its object is in the store, and
	// it lives under that attempt's own prefix.
	var ckAttempt, ckKey, ckSHA string
	var ckSize int64
	if err := db.QueryRow(ctx, `SELECT attempt_id::text, object_key, sha256, size_bytes FROM task_checkpoints WHERE run_id = $1`,
		runID).Scan(&ckAttempt, &ckKey, &ckSHA, &ckSize); err != nil {
		t.Fatalf("task_checkpoints: %v", err)
	}
	if ckAttempt != a2.attemptID || !strings.Contains(ckKey, "/checkpoints/") || !strings.Contains(ckKey, "/"+a2.attemptID+"/") {
		t.Errorf("checkpoint pointer attempt=%s key=%s; want attempt 2 (%s) under its own prefix", ckAttempt, ckKey, a2.attemptID)
	}
	mc := minioClient(t, s3c)
	info, err := mc.StatObject(ctx, bucket, ckKey, minio.StatObjectOptions{})
	if err != nil || info.Size != ckSize {
		t.Errorf("checkpoint object %s: %v (size %d, pointer says %d)", ckKey, err, info.Size, ckSize)
	}
	// Attempt 1's checkpoint object exists too: it is what attempt 2 resumed from.
	found := false
	for o := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: nsA + "/checkpoints/", Recursive: true}) {
		if strings.Contains(o.Key, "/"+a1.attemptID+"/") {
			found = true
		}
	}
	if !found {
		t.Error("no checkpoint object from attempt 1")
	}

	// Every call took a permit from the namespace's shared bucket.
	var granted, denied int64
	if err := db.QueryRow(ctx, `SELECT granted_requests, denied FROM rate_limit_buckets b JOIN tenants t ON t.id = b.tenant_id
		WHERE t.namespace = $1 AND b.key = 'mock'`, nsA).Scan(&granted, &denied); err != nil {
		t.Fatalf("rate_limit_buckets: %v", err)
	}
	if granted != 6 {
		t.Errorf("granted_requests = %d; want 6 (one per call across both attempts)", granted)
	}
	if !heartbeat {
		t.Error("no heartbeat from the task reached attempt 2's progress while it ran")
	}
	t.Logf("ledger, checkpoint, permits and heartbeats verified; permits granted=%d denied=%d", granted, denied)
}

// ── helpers ─────────────────────────────────────────────────────────────────

// nodeForImages is the node container to load a locally built image into:
// NEXUS_E2E_IMAGE_NODE, else Docker Desktop's desktop-control-plane on the
// docker-desktop context, else none (a cluster that shares the docker store).
func nodeForImages() string {
	if n := os.Getenv("NEXUS_E2E_IMAGE_NODE"); n != "" {
		return n
	}
	if k8stest.ContextName() == "docker-desktop" &&
		exec.Command("docker", "exec", "desktop-control-plane", "true").Run() == nil {
		return "desktop-control-plane"
	}
	return ""
}

type stolen struct {
	number    int
	attemptID string
	token     string
	caPEM     []byte
}

// thief watches the run's per-attempt Secrets and Jobs, as an attacker with
// read access to the namespace would, and records each attempt's token and
// the CA its pod was told to trust.
type thief struct {
	cs    kubernetes.Interface
	runID string
	mu    sync.Mutex
	seen  []stolen
	ids   map[string]bool
}

func newThief(cs kubernetes.Interface, runID string) *thief {
	return &thief{cs: cs, runID: runID, ids: map[string]bool{}}
}

func (th *thief) watch(ctx context.Context) {
	for ctx.Err() == nil {
		secrets, err := th.cs.CoreV1().Secrets(k8stest.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "nexus.dev/run=" + th.runID})
		if err == nil {
			for _, s := range secrets.Items {
				id := s.Annotations["nexus.dev/attempt-id"]
				tok := s.Data["nexus-task-api.token"]
				if id == "" || len(tok) == 0 || th.known(id) {
					continue
				}
				ca := jobCA(ctx, th.cs, id)
				if ca == nil {
					continue
				}
				th.mu.Lock()
				th.ids[id] = true
				th.seen = append(th.seen, stolen{number: len(th.seen) + 1, attemptID: id, token: string(tok), caPEM: ca})
				th.mu.Unlock()
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (th *thief) known(id string) bool {
	th.mu.Lock()
	defer th.mu.Unlock()
	return th.ids[id]
}

func (th *thief) await(t *testing.T, n int, within time.Duration) stolen {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		th.mu.Lock()
		if len(th.seen) >= n {
			s := th.seen[n-1]
			th.mu.Unlock()
			return s
		}
		th.mu.Unlock()
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("attempt %d's Secret never appeared", n)
	return stolen{}
}

func jobCA(ctx context.Context, cs kubernetes.Interface, attemptID string) []byte {
	jobs, err := cs.BatchV1().Jobs(k8stest.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "nexus.dev/attempt=" + attemptID})
	if err != nil || len(jobs.Items) != 1 {
		return nil
	}
	var env []corev1.EnvVar
	for _, c := range jobs.Items[0].Spec.Template.Spec.Containers {
		if c.Name == "task" {
			env = c.Env
		}
	}
	for _, e := range env {
		if e.Name == "NEXUS_TASK_API_CA" {
			return []byte(e.Value)
		}
	}
	return nil
}

func pinned(t *testing.T, caPEM []byte) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("the pod's NEXUS_TASK_API_CA does not parse: %q", caPEM)
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

func insecure() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // negative test of token acceptance only
}

func apiCall(t *testing.T, hc *http.Client, base, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, base+path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func call(t *testing.T, method, u, key string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, u, r)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func waitHTTP(t *testing.T, u string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(u); err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s not healthy after %s", u, within)
}

// waitWorkerWithKubernetes waits until an ACTIVE worker of ns advertises the
// kubernetes executor, i.e. its Probe of the local cluster passed.
func waitWorkerWithKubernetes(t *testing.T, ctx context.Context, db *pgx.Conn, ns string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM workers WHERE namespace = $1 AND state = 'ACTIVE'
			AND 'kubernetes' = ANY(executors)`, ns).Scan(&n)
		if n > 0 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("no ACTIVE worker in %s advertises the kubernetes executor", ns)
}

func jsonUnits(t *testing.T, db *pgx.Conn, q string, arg any) map[string]int64 {
	t.Helper()
	var raw []byte
	if err := db.QueryRow(context.Background(), q, arg).Scan(&raw); err != nil {
		t.Errorf("%s [%v]: %v", q, arg, err)
		return nil
	}
	out := map[string]int64{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// usageOnly keeps usage keys (they contain ':'), dropping reserved custom
// resources, which the ledger's custom object also carries.
func usageOnly(m map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for k, v := range m {
		if strings.Contains(k, ":") {
			out[k] = v
		}
	}
	return out
}

func equal(a, b map[string]int64) bool {
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

func minioClient(t *testing.T, c tests3.Config) *minio.Client {
	t.Helper()
	mc, err := minio.New(c.HostPort(), &minio.Options{Creds: credentials.NewStaticV4(c.AccessKeyID, c.SecretAccessKey, ""), Secure: c.UseTLS})
	if err != nil {
		t.Fatal(err)
	}
	return mc
}

func createBucket(t *testing.T, c tests3.Config, bucket string) {
	t.Helper()
	mc := minioClient(t, c)
	if err := mc.MakeBucket(context.Background(), bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("make bucket %s: %v", bucket, err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for o := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			_ = mc.RemoveObject(ctx, bucket, o.Key, minio.RemoveObjectOptions{})
		}
		_ = mc.RemoveBucket(ctx, bucket)
	})
}
