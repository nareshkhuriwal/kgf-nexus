//go:build tracing_e2e

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package tracing_e2e proves ADR-0026's promise end to end: ONE trace from the
// API request that submits a run, through workflow-manager, dag-engine and the
// scheduler, into the agent and the task process, and back through the result
// report that starts the next node.
//
// It runs the REAL binaries (built here from this checkout) against a real
// Postgres, a real S3 store and a real OpenTelemetry backend (Jaeger v2,
// pinned in the Makefile), and reads the trace back through Jaeger's query
// API. Run it with `make tracing-e2e`, which starts the pinned containers; or
// point it at existing ones:
//
//	NEXUS_E2E_PG_DSN=postgres://postgres:pw@127.0.0.1:55831/postgres?sslmode=disable
//	NEXUS_TEST_S3_ENDPOINT=127.0.0.1:55832 NEXUS_TEST_S3_ACCESS_KEY_ID=... NEXUS_TEST_S3_SECRET_ACCESS_KEY=...
//	NEXUS_E2E_OTLP_GRPC=127.0.0.1:55835 NEXUS_E2E_JAEGER_QUERY=http://127.0.0.1:55837
//	go test -tags tracing_e2e -count=1 -v ./tests/tracing/
//
// A fresh database is created per run and dropped afterwards; the services
// listen on ephemeral loopback ports.
package tracing_e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func need(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Skipf("skipping tracing end-to-end test: %s is not set (run `make tracing-e2e`)", k)
	}
	return v
}

func repoRoot(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ── the test ────────────────────────────────────────────────────────────────

func TestOneTraceFromAPIToAgentAndBack(t *testing.T) {
	adminDSN := need(t, "NEXUS_E2E_PG_DSN")
	s3Endpoint := need(t, "NEXUS_TEST_S3_ENDPOINT")
	s3Key := need(t, "NEXUS_TEST_S3_ACCESS_KEY_ID")
	s3Secret := need(t, "NEXUS_TEST_S3_SECRET_ACCESS_KEY")
	otlp := need(t, "NEXUS_E2E_OTLP_GRPC")
	jaeger := strings.TrimRight(need(t, "NEXUS_E2E_JAEGER_QUERY"), "/")

	root := repoRoot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// ── database ────────────────────────────────────────────────────────────
	dbName := "nexus_trace_e2e_" + randHex(4)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect %s: %v", adminDSN, err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), adminDSN)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	_ = admin.Close(ctx)
	u, _ := url.Parse(adminDSN)
	u.Path = "/" + dbName
	dsn := u.String()

	mig := exec.CommandContext(ctx, "sh", filepath.Join(root, "deploy", "migrate.sh"))
	mig.Env = append(os.Environ(), "DATABASE_URL="+dsn, "MIGRATIONS_DIR="+filepath.Join(root, "services", "migrations"))
	if out, err := mig.CombinedOutput(); err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	adminKey, workerKey := "nxs_admin_"+randHex(32), "nxs_worker_"+randHex(32)
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO tenants (namespace, name) VALUES ('default','default') ON CONFLICT DO NOTHING", nil},
		{"INSERT INTO api_keys (namespace, name, key_hash, role) VALUES ('default','e2e-admin',$1,'admin')", []any{sha(adminKey)}},
		{"INSERT INTO api_keys (namespace, name, key_hash, role) VALUES ('default','e2e-worker',$1,'worker')", []any{sha(workerKey)}},
	} {
		if _, err := db.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	defer db.Close(context.Background())

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

	bucket := "nexus-trace-e2e-" + randHex(4)
	common := []string{
		"DATABASE_URL=" + dsn,
		"NEXUS_LOG_LEVEL=info",
		// Tracing ON, exported over OTLP/gRPC to the collector. Batching kept
		// short so the trace is complete soon after the run ends.
		"OTEL_TRACES_EXPORTER=otlp",
		"OTEL_EXPORTER_OTLP_ENDPOINT=http://" + otlp,
		"OTEL_EXPORTER_OTLP_PROTOCOL=grpc",
		"OTEL_EXPORTER_OTLP_INSECURE=true",
		"OTEL_TRACES_SAMPLER=parentbased_traceidratio",
		"OTEL_TRACES_SAMPLER_ARG=1.0",
		"OTEL_BSP_SCHEDULE_DELAY=200",
		"OTEL_RESOURCE_ATTRIBUTES=deployment.environment.name=e2e,nexus.e2e.db=" + dbName,
	}
	store := []string{
		"NEXUS_RESULT_STORE_BACKEND=s3",
		"NEXUS_RESULT_STORE_ENDPOINT=" + s3Endpoint,
		"NEXUS_RESULT_STORE_BUCKET=" + bucket,
		"NEXUS_RESULT_STORE_REGION=us-east-1",
		"NEXUS_S3_ACCESS_KEY_ID=" + s3Key,
		"NEXUS_S3_SECRET_ACCESS_KEY=" + s3Secret,
	}
	createBucket(t, root, s3Endpoint, s3Key, s3Secret, bucket)

	rmGRPC, wfmGRPC, deGRPC, schGRPC := freeAddr(t), freeAddr(t), freeAddr(t), freeAddr(t)
	gwREST, gwGRPC := freeAddr(t), freeAddr(t)
	logDir := t.TempDir()
	start := func(name string, env ...string) {
		t.Helper()
		f, _ := os.Create(filepath.Join(logDir, name+".log"))
		cmd := exec.Command(filepath.Join(bin, name))
		cmd.Env = append(append(append(os.Environ(), common...), env...), "OTEL_SERVICE_NAME="+name)
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
			case <-time.After(20 * time.Second):
				_ = cmd.Process.Kill()
			}
			f.Close()
			if t.Failed() {
				b, _ := os.ReadFile(f.Name())
				t.Logf("── %s log (tail) ──\n%s", name, tail(string(b), 4000))
			}
		})
	}
	start("resource-manager", "RM_GRPC_ADDR="+rmGRPC, "RM_HTTP_ADDR="+freeAddr(t))
	start("scheduler", "NEXUS_GRPC_ADDR="+schGRPC, "NEXUS_HTTP_ADDR="+freeAddr(t))
	start("workflow-manager", "NEXUS_WFM_GRPC_ADDR="+wfmGRPC, "NEXUS_WFM_HEALTH_ADDR="+freeAddr(t),
		"NEXUS_DAG_ENGINE_ADDR="+deGRPC, "NEXUS_SCHEDULER_ADDR="+schGRPC)
	start("dag-engine", "NEXUS_GRPC_ADDR="+deGRPC, "NEXUS_HTTP_ADDR="+freeAddr(t),
		"NEXUS_WORKFLOW_MANAGER_ADDR="+wfmGRPC)
	start("api-gateway", append(store,
		"NEXUS_REST_ADDR="+gwREST, "NEXUS_GRPC_ADDR="+gwGRPC, "NEXUS_METRICS_ADDR=off",
		"NEXUS_WORKFLOW_MANAGER_ADDR="+wfmGRPC, "NEXUS_RESOURCE_MANAGER_ADDR="+rmGRPC,
		"NEXUS_SCHEDULER_ADDR="+schGRPC, "NEXUS_DAG_ENGINE_ADDR="+deGRPC,
		// Force outputs through the object store, so result-store I/O is in the trace.
		"NEXUS_INLINE_THRESHOLD_BYTES=1",
		"NEXUS_EMAIL_VERIFICATION_REQUIRED=false")...)

	api := "http://" + gwREST
	waitHTTP(t, api+"/healthz", 60*time.Second)

	work := t.TempDir()
	start("nexus-agent", append(store,
		"NEXUS_CONTROL_PLANE="+gwGRPC, "NEXUS_API_KEY="+workerKey, "NEXUS_NAMESPACE=default",
		"NEXUS_QUEUES=default", "NEXUS_EXECUTORS=subprocess", "NEXUS_CPU_MILLIS=4000",
		"NEXUS_MEMORY_BYTES=4294967296", "NEXUS_METRICS_ADDR="+freeAddr(t),
		"NEXUS_WAL_DIR="+filepath.Join(work, "wal"), "NEXUS_WORK_DIR="+filepath.Join(work, "work"),
		"NEXUS_ALLOW_UNENFORCEABLE_LIMITS=true")...)

	// ── the traced request ──────────────────────────────────────────────────
	// The client starts the trace, exactly as an instrumented SDK caller would:
	// the gateway must continue it rather than start its own.
	traceID := randHex(16)
	traceparent := "00-" + traceID + "-" + randHex(8) + "-01"

	specName := "trace-e2e-" + randHex(3)
	spec := map[string]any{
		"api_version": "nexus.dev/v1", "name": specName, "namespace": "default",
		"tasks": []any{
			task("produce", `printf '{"traceparent":"%s"}' "$TRACEPARENT" > "$NEXUS_OUTPUT_FILE"; echo "task traceparent=$TRACEPARENT"`, true),
			task("consume", `cat "$NEXUS_INPUT_DIR"/* >/dev/null 2>&1; echo "task traceparent=$TRACEPARENT"; printf '{"ok":true}' > "$NEXUS_OUTPUT_FILE"`, false),
		},
		"edges": []any{map[string]any{"from_node_id": "produce", "to_node_id": "consume",
			"output_to_input": map[string]any{"data": "result"}}},
	}
	if code, body := call(t, "POST", api+"/v1/namespaces/default/specs", adminKey, "", spec); code/100 != 2 {
		t.Fatalf("submit spec: %d %s", code, body)
	}
	code, body := call(t, "POST", api+"/v1/namespaces/default/runs", adminKey, traceparent,
		map[string]any{"spec_name": specName})
	if code/100 != 2 {
		t.Fatalf("create run: %d %s", code, body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)
	if created.ID == "" {
		t.Fatalf("create run: no id in %s", body)
	}
	t.Logf("run %s, trace %s", created.ID, traceID)

	// ── wait for the run ────────────────────────────────────────────────────
	var run map[string]any
	deadline := time.Now().Add(3 * time.Minute)
	for {
		_, b := call(t, "GET", api+"/v1/namespaces/default/runs/"+created.ID, adminKey, "", nil)
		run = map[string]any{}
		_ = json.Unmarshal(b, &run)
		state, _ := run["state"].(string)
		if state == "SUCCEEDED" {
			break
		}
		if strings.Contains("FAILED CANCELLED TIMED_OUT", state) && state != "" || time.Now().After(deadline) {
			t.Fatalf("run ended %q: %s", state, b)
		}
		time.Sleep(time.Second)
	}

	// The task process saw a TRACEPARENT in the SAME trace (the Python SDK and
	// any OTel program continue from it). Its output was written through the
	// object store (threshold 1 byte), so read it back from the log instead:
	// every attempt's log line names the traceparent the task was given.
	for _, a := range run["attempts"].([]any) {
		att := a.(map[string]any)
		id, _ := att["id"].(string)
		if id == "" {
			id, _ = att["attempt_id"].(string)
		}
		var line string
		for i := 0; i < 30 && line == ""; i++ {
			_, lb := call(t, "GET", api+"/v1/namespaces/default/attempts/"+id+"/logs?tail=50", adminKey, "", nil)
			for _, l := range strings.Split(string(lb), "task traceparent=")[1:] {
				line = strings.SplitN(l, `"`, 2)[0]
			}
			if line == "" {
				time.Sleep(time.Second)
			}
		}
		if !strings.HasPrefix(line, "00-"+traceID+"-") {
			t.Fatalf("attempt %s (%v) ran with TRACEPARENT %q; want trace %s", id, att["node_id"], line, traceID)
		}
	}

	// ── read the trace back from Jaeger ─────────────────────────────────────
	want := map[string][]string{
		"api-gateway":      {"POST /v1/namespaces/{ns}/runs"},
		"workflow-manager": {"run.create"},
		"dag-engine":       {"run.materialize", "run.reconcile"},
		"scheduler":        {"attempt.dispatch"},
		"nexus-agent":      {"attempt.execute", "attempt.run", "attempt.store_outputs", "attempt.report_result", "objstore.put"},
	}
	var spans []jaegerSpan
	var missing []string
	for i := 0; i < 60; i++ {
		spans = fetchTrace(t, jaeger, traceID)
		missing = missingSpans(spans, want)
		// Two attempts, each executed and dispatched under the run's trace.
		if len(missing) == 0 && count(spans, "attempt.execute") >= 2 && count(spans, "attempt.dispatch") >= 2 {
			break
		}
		time.Sleep(time.Second)
	}
	if len(missing) > 0 {
		t.Fatalf("trace %s is missing %v; it has %d spans: %v", traceID, missing, len(spans), names(spans))
	}
	if n := count(spans, "attempt.execute"); n < 2 {
		t.Fatalf("want both attempts in the trace, found %d attempt.execute spans", n)
	}
	// Database spans prove the pgx tracer is installed on the path.
	if count(spans, "") == 0 || !hasPrefix(spans, "query", "batch", "prepare", "SELECT", "INSERT", "UPDATE", "WITH") {
		t.Errorf("no database spans in the trace: %v", names(spans))
	}
	// Every span is in this one trace: nothing was orphaned into a new root
	// except the client's own (the parent we sent never reaches Jaeger).
	roots := 0
	for _, s := range spans {
		if len(s.References) == 0 {
			roots++
		}
	}
	if roots > 1 {
		t.Errorf("trace has %d root spans; want 1 (the gateway's, under our client span)", roots)
	}
	t.Logf("trace %s: %d spans across %v", traceID, len(spans), services(spans))
}

// ── helpers ─────────────────────────────────────────────────────────────────

func task(id, script string, root bool) map[string]any {
	t := map[string]any{
		"id": id, "task_ref": "e2e." + id, "version": 1, "queue": "default",
		"runtime": map[string]any{"executor": "subprocess", "command": []string{"sh", "-c"}, "args": []string{script}},
		"outputs": map[string]any{"result": map[string]any{"type": "json", "required": true}},
		"resources": map[string]any{"request": map[string]any{"cpu_millis": 100, "memory_bytes": 67108864}},
		"retry":     map[string]any{"max_attempts": 1},
	}
	if !root {
		t["inputs"] = map[string]any{"data": map[string]any{"type": "json", "required": true}}
	}
	return t
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func call(t *testing.T, method, u, key, traceparent string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, u, r)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	if traceparent != "" {
		req.Header.Set("traceparent", traceparent)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func waitHTTP(t *testing.T, u string, d time.Duration) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if resp, err := http.Get(u); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("%s not ready after %v", u, d)
}

func createBucket(t *testing.T, root, endpoint, key, secret, bucket string) {
	t.Helper()
	// Through the repo's own objstore package, via a tiny go run, so the test
	// needs no mc or aws CLI.
	prog := fmt.Sprintf(`package main
import ("context"; "github.com/khuriwalgroup/kgf-nexus/internal/objstore")
func main() {
	s, err := objstore.Open(context.Background(), objstore.Config{Backend: "s3", Endpoint: %q, Bucket: %q,
		S3: objstore.S3Config{Credentials: "static", AccessKeyID: %q, SecretAccessKey: %q}})
	if err != nil { panic(err) }
	if err := s.CreateBucket(context.Background()); err != nil { panic(err) }
}`, endpoint, bucket, key, secret)
	dir, err := os.MkdirTemp(root, ".e2e-mkbucket-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte(prog), 0o600)
	cmd := exec.Command("go", "run", "./"+filepath.Base(dir))
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create bucket: %v\n%s", err, out)
	}
}

type jaegerSpan struct {
	SpanID        string `json:"spanID"`
	OperationName string `json:"operationName"`
	ProcessID     string `json:"processID"`
	References    []any  `json:"references"`
	service       string
}

func fetchTrace(t *testing.T, jaeger, traceID string) []jaegerSpan {
	t.Helper()
	resp, err := http.Get(jaeger + "/api/traces/" + traceID)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			Spans     []jaegerSpan `json:"spans"`
			Processes map[string]struct {
				ServiceName string `json:"serviceName"`
			} `json:"processes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Data) == 0 {
		return nil
	}
	spans := out.Data[0].Spans
	for i := range spans {
		spans[i].service = out.Data[0].Processes[spans[i].ProcessID].ServiceName
	}
	return spans
}

func missingSpans(spans []jaegerSpan, want map[string][]string) []string {
	var missing []string
	for svc, ops := range want {
		for _, op := range ops {
			found := false
			for _, s := range spans {
				if s.service == svc && s.OperationName == op {
					found = true
				}
			}
			if !found {
				missing = append(missing, svc+":"+op)
			}
		}
	}
	return missing
}

func count(spans []jaegerSpan, op string) int {
	n := 0
	for _, s := range spans {
		if op == "" || s.OperationName == op {
			n++
		}
	}
	return n
}

func hasPrefix(spans []jaegerSpan, prefixes ...string) bool {
	for _, s := range spans {
		for _, p := range prefixes {
			if strings.HasPrefix(s.OperationName, p) {
				return true
			}
		}
	}
	return false
}

func names(spans []jaegerSpan) []string {
	var out []string
	for _, s := range spans {
		out = append(out, s.service+":"+s.OperationName)
	}
	return out
}

func services(spans []jaegerSpan) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range spans {
		if !seen[s.service] {
			seen[s.service] = true
			out = append(out, s.service)
		}
	}
	return out
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
