//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

// Durable task logs END TO END (ADR-0020), against a REAL Postgres (migrated
// through 0025) and a REAL MinIO:
//
//	the agent's Runner executes a subprocess task
//	  -> the shipper writes NDJSON chunks straight to MinIO
//	  -> ReportLogs goes through the gateway's real worker server (real fence,
//	     real SQL)
//	  -> the gateway's log reader serves the lines back from index + MinIO,
//
// while the task is RUNNING and after it has finished. Run with:
//
//	NEXUS_TEST_DATABASE_URL=... NEXUS_TEST_S3_ENDPOINT=127.0.0.1:9000 \
//	NEXUS_TEST_S3_ACCESS_KEY_ID=... NEXUS_TEST_S3_SECRET_ACCESS_KEY=... \
//	  go test -tags integration -run Logs ./agent/internal/worker/

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/logship"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/resultstore"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/wal"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
	"github.com/khuriwalgroup/kgf-nexus/internal/tests3"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/logs"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/workergrpc"
)

// ── fixtures ──────────────────────────────────────────────────────────────

type logEnv struct {
	pool   *pgxpool.Pool
	ns     string
	bucket string
	store  *resultstore.Client
	reader *logs.Reader
	srv    *workergrpc.Server
}

func newLogEnv(t *testing.T) *logEnv {
	t.Helper()
	dsn := testdb.DSN(t)
	s3 := tests3.Get(t)
	endpoint := s3.Endpoint
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		testdb.SkipUnreachable(t, err)
	}
	if err := pool.Ping(ctx); err != nil {
		testdb.SkipUnreachable(t, err)
	}
	t.Cleanup(pool.Close)

	env := &logEnv{pool: pool, ns: "logs-it-" + uuid.NewString()[:8], bucket: "logs-it"}
	access, secret := s3.AccessKeyID, s3.SecretAccessKey
	env.store, err = resultstore.New(resultstore.Config{
		Endpoint: endpoint, Bucket: env.bucket, Namespace: env.ns,
		AccessKeyID: access, SecretAccessKey: secret, UseTLS: s3.UseTLS, Log: testLog(),
	})
	if err != nil {
		t.Fatalf("result store: %v", err)
	}
	if err := env.store.EnsureBucket(ctx); err != nil {
		tests3.SkipUnreachable(t, err)
	}
	obj, err := logs.NewMinIOReader(logs.MinIOConfig{Endpoint: endpoint, Bucket: env.bucket, AccessKeyID: access, SecretAccessKey: secret, UseTLS: s3.UseTLS})
	if err != nil {
		t.Fatalf("log reader: %v", err)
	}
	env.reader = logs.NewReader(logs.StoreIndex{DB: pool}, obj, logs.Limits{})

	cfg := workergrpc.DefaultConfig()
	cfg.LogShipping = &nexusv1.LogShippingConfig{Enabled: true}
	env.srv = workergrpc.NewServer(pool, nil, nil, cfg, testLog())
	testdb.RegisterTenant(t, pool, env.ns)
	return env
}

// seed creates a RUNNING attempt leased to a worker with the given token.
func (e *logEnv) seed(t *testing.T, token string) string {
	t.Helper()
	ctx := context.Background()
	attemptID := testdb.NewAttempt(t, e.pool, e.ns)
	var workerID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO workers (namespace, hostname, queues, capacity, agent_version, session_token_hash)
		VALUES ($1, 'it-host', '{default}', '{"cpu_millis":1000}', 'it', 'x') RETURNING id::text`, e.ns).Scan(&workerID); err != nil {
		t.Fatalf("worker: %v", err)
	}
	t.Cleanup(func() { _, _ = e.pool.Exec(context.Background(), `DELETE FROM workers WHERE id = $1`, workerID) })
	if _, err := e.pool.Exec(ctx, `UPDATE task_attempts SET state = 'RUNNING', worker_id = $2 WHERE id = $1`, attemptID, workerID); err != nil {
		t.Fatalf("attempt state: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO leases (attempt_id, worker_id, token, expires_at)
		VALUES ($1, $2, $3, now() + interval '10 minutes')`, attemptID, workerID, token); err != nil {
		t.Fatalf("lease: %v", err)
	}
	return attemptID
}

func (e *logEnv) setAttemptState(t *testing.T, attemptID, state string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE task_attempts SET state = $2::attempt_state WHERE id = $1`, attemptID, state); err != nil {
		t.Fatalf("set state: %v", err)
	}
}

func workerPrincipalCtx(ctx context.Context, ns string) context.Context {
	return authn.WithPrincipal(ctx, authn.Principal{KeyID: "k", Namespace: ns, Name: "agent", Role: authn.RoleWorker})
}

// gatewayIndexer calls the REAL worker server in process, as the gRPC
// interceptor would after authenticating the agent.
type gatewayIndexer struct {
	srv *workergrpc.Server
	ns  string
}

func (g gatewayIndexer) ReportLogs(ctx context.Context, req *nexusv1.ReportLogsRequest) (*nexusv1.Ack, error) {
	return g.srv.ReportLogs(workerPrincipalCtx(ctx, g.ns), req)
}

type lgLeases struct{ token string }

func (l lgLeases) Track(string, string, time.Time) {}
func (l lgLeases) Untrack(string)                  {}
func (l lgLeases) Token(string) (string, bool)     { return l.token, true }

// lgReports records the result report AND the log index state at the moment it
// arrived -- which is how "the log is finalized BEFORE the result" is proven.
type lgReports struct {
	mu          sync.Mutex
	pool        *pgxpool.Pool
	result      *nexusv1.ReportResultRequest
	logAtResult string
}

func (r *lgReports) ReportStarted(context.Context, *nexusv1.ReportStartedRequest) (*nexusv1.Ack, error) {
	return &nexusv1.Ack{Accepted: true}, nil
}
func (r *lgReports) ReportProgress(context.Context, *nexusv1.ReportProgressRequest) (*nexusv1.Ack, error) {
	return &nexusv1.Ack{Accepted: true}, nil
}
func (r *lgReports) ReportResult(ctx context.Context, req *nexusv1.ReportResultRequest) (*nexusv1.Ack, error) {
	var state string
	_ = r.pool.QueryRow(ctx, `SELECT state FROM attempt_logs WHERE attempt_id = $1`, req.GetAttemptId()).Scan(&state)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.result, r.logAtResult = req, state
	return &nexusv1.Ack{Accepted: true}, nil
}

func waitUntil(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ── the end-to-end test ───────────────────────────────────────────────────

// TestLogsOfASubprocessTaskAreReadableWhileRunningAndAfterCompletion is the
// acceptance test for ADR-0020.
func TestLogsOfASubprocessTaskAreReadableWhileRunningAndAfterCompletion(t *testing.T) {
	env := newLogEnv(t)
	ctx := context.Background()
	token := "tok-" + uuid.NewString()
	attemptID := env.seed(t, token)

	dir := t.TempDir()
	gate := filepath.Join(dir, "gate")
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	reports := &lgReports{pool: env.pool}
	runner := NewRunner(
		map[string]executor.Executor{"subprocess": &executor.SubprocessExecutor{Root: filepath.Join(dir, "work"), AllowUnenforceableLimits: true, Log: testLog()}},
		NewRegistry(vec(4000, 1<<30, 0)), lgLeases{token: token}, NewReporter(reports, w, testLog()), w, env.store, testLog())
	runner.LogShipping = &nexusv1.LogShippingConfig{Enabled: true, FlushIntervalMs: 100}
	runner.LogIndexer = gatewayIndexer{srv: env.srv, ns: env.ns}
	runner.LogSpoolDir = filepath.Join(dir, "spool")
	runner.Namespace = env.ns
	if err := os.MkdirAll(runner.LogSpoolDir, 0o700); err != nil {
		t.Fatal(err)
	}

	script := fmt.Sprintf(`for i in $(seq 1 20); do echo "out $i"; echo "err $i" >&2; done
while [ ! -f %q ]; do sleep 0.05; done
echo "after the gate: connection refused"; exit 3`, gate)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.Run(ctx, &nexusv1.TaskAssignment{
			AttemptId: attemptID, RunId: "run", NodeId: "node", AttemptNumber: 1, LeaseToken: token,
			Spec: &nexusv1.TaskSpec{Runtime: &nexusv1.RuntimeSpec{
				Executor: "subprocess", Command: []string{"/bin/sh", "-c", script},
			}},
		})
	}()

	// ── WHILE RUNNING ──
	var row store.AttemptLogRow
	waitUntil(t, "40 lines indexed while the task runs", 15*time.Second, func() bool {
		r, found, st, err := env.reader.Summary(ctx, attemptID, "RUNNING")
		row = r
		return err == nil && found && st.State == logs.StateLive && r.LineCount >= 40
	})
	page, err := env.reader.ReadLines(ctx, attemptID, row, 1, 1000)
	if err != nil {
		t.Fatalf("read while running: %v", err)
	}
	var outs, errs int
	for i, l := range page.Lines {
		if l.N != int64(i+1) {
			t.Fatalf("line numbers not contiguous while running: %d at %d", l.N, i)
		}
		switch {
		case l.Stream == "stdout" && strings.HasPrefix(l.Message, "out "):
			outs++
		case l.Stream == "stderr" && strings.HasPrefix(l.Message, "err "):
			errs++
		}
	}
	if outs != 20 || errs != 20 {
		t.Fatalf("while running: %d stdout and %d stderr lines, want 20 each", outs, errs)
	}
	select {
	case <-done:
		t.Fatal("the task finished before the gate; the running-state read proved nothing")
	default:
	}

	// ── AFTER COMPLETION ──
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the attempt did not finish")
	}
	reports.mu.Lock()
	if reports.result == nil || reports.logAtResult != store.LogStateComplete {
		t.Fatalf("result=%v log state when the result was reported=%q; the log must be finalized BEFORE ReportResult (its fence needs the lease)",
			reports.result != nil, reports.logAtResult)
	}
	reports.mu.Unlock()
	env.setAttemptState(t, attemptID, "FAILED")

	row, found, st, err := env.reader.Summary(ctx, attemptID, "FAILED")
	if err != nil || !found || st.State != logs.StateComplete || row.LineCount != 41 {
		t.Fatalf("after completion: state=%q lines=%d err=%v", st.State, row.LineCount, err)
	}
	if row.ChunkCount >= 2 && row.CompactedKey == "" {
		t.Errorf("%d chunks but no compacted object", row.ChunkCount)
	}
	page, err = env.reader.ReadLines(ctx, attemptID, row, logs.TailStart(row, 1), 1)
	if err != nil || len(page.Lines) != 1 || page.Lines[0].Message != "after the gate: connection refused" {
		t.Fatalf("tail after completion = %+v err=%v", page.Lines, err)
	}
	res, err := env.reader.Search(ctx, attemptID, row, logs.SearchQuery{Pattern: `connection\s+refused`, Regex: true})
	if err != nil || len(res.Matches) != 1 || res.Matches[0].N != 41 || !res.Complete {
		t.Fatalf("search = %+v err=%v", res, err)
	}
	t.Logf("indexed %d lines in %d chunks, compacted=%v", row.LineCount, row.ChunkCount, row.CompactedKey != "")

	// ── the index refuses what a finished log must not accept ──
	ack, err := env.srv.ReportLogs(workerPrincipalCtx(ctx, env.ns), &nexusv1.ReportLogsRequest{
		AttemptId: attemptID, LeaseToken: token,
		Chunks: []*nexusv1.LogChunk{{Seq: 999, Key: store.LogKeyPrefix(env.ns, attemptID) + "x.ndjson",
			FirstLine: 42, LineCount: 1, SizeBytes: 1, Sha256: strings.Repeat("0", 64)}},
	})
	if err != nil || ack.GetAccepted() || ack.GetReason() != "log_finalized" {
		t.Fatalf("a chunk after the final report: ack=%+v err=%v", ack, err)
	}
}

// TestLogIndexFencingIdempotencyAndTenancy drives ReportLogs through the real
// fence and SQL: a stale token cannot write, a duplicate is a no-op, a key
// outside the attempt's prefix is refused, and a crashed agent's log stays
// readable as partial.
func TestLogIndexFencingIdempotencyAndTenancy(t *testing.T) {
	env := newLogEnv(t)
	ctx := context.Background()
	token := "tok-" + uuid.NewString()
	attemptID := env.seed(t, token)
	idx := gatewayIndexer{srv: env.srv, ns: env.ns}

	// A shipper that "crashes": it ships while running and is never closed.
	ship := logship.New(logship.Config{
		Namespace: env.ns, AttemptID: attemptID, FlushInterval: 50 * time.Millisecond,
		Token: func() string { return token }, Log: testLog(), SpoolDir: t.TempDir(),
	}, env.store, idx)
	for i := 0; i < 5; i++ {
		fmt.Fprintf(ship.Writer("stdout"), "before the crash %d\n", i)
	}
	waitUntil(t, "5 lines indexed", 10*time.Second, func() bool {
		r, found, _, err := env.reader.Summary(ctx, attemptID, "RUNNING")
		return err == nil && found && r.LineCount == 5
	})

	var chunk store.LogChunkRow
	if err := env.pool.QueryRow(ctx, `SELECT seq, object_key, first_line, line_count, byte_offset, size_bytes, sha256
		FROM attempt_log_chunks WHERE attempt_id = $1 ORDER BY seq LIMIT 1`, attemptID).Scan(
		&chunk.Seq, &chunk.ObjectKey, &chunk.FirstLine, &chunk.LineCount, &chunk.ByteOffset, &chunk.SizeBytes, &chunk.SHA256); err != nil {
		t.Fatal(err)
	}
	proto := &nexusv1.LogChunk{Seq: chunk.Seq, Key: chunk.ObjectKey, FirstLine: chunk.FirstLine, LineCount: chunk.LineCount,
		ByteOffset: chunk.ByteOffset, SizeBytes: chunk.SizeBytes, Sha256: chunk.SHA256}
	counts := func() (n, lines int64) {
		_ = env.pool.QueryRow(ctx, `SELECT chunk_count, line_count FROM attempt_logs WHERE attempt_id = $1`, attemptID).Scan(&n, &lines)
		return
	}
	n0, l0 := counts()

	// Duplicate delivery: accepted, changes nothing.
	ack, err := idx.ReportLogs(ctx, &nexusv1.ReportLogsRequest{AttemptId: attemptID, LeaseToken: token, Chunks: []*nexusv1.LogChunk{proto}})
	if err != nil || !ack.GetAccepted() {
		t.Fatalf("duplicate: ack=%+v err=%v", ack, err)
	}
	if n, l := counts(); n != n0 || l != l0 {
		t.Fatalf("a duplicate chunk changed the counters: %d/%d -> %d/%d", n0, l0, n, l)
	}

	// Stale lease: rejected with the contract's reason; nothing indexed.
	stale := &nexusv1.LogChunk{Seq: 900, Key: store.LogKeyPrefix(env.ns, attemptID) + "00000900.ndjson",
		FirstLine: 100, LineCount: 1, SizeBytes: 1, Sha256: strings.Repeat("0", 64)}
	ack, err = idx.ReportLogs(ctx, &nexusv1.ReportLogsRequest{AttemptId: attemptID, LeaseToken: "zombie", Chunks: []*nexusv1.LogChunk{stale}})
	if err != nil || ack.GetAccepted() || ack.GetReason() != store.ReasonStaleLeaseToken {
		t.Fatalf("stale token: ack=%+v err=%v", ack, err)
	}
	if n, _ := counts(); n != n0 {
		t.Fatal("a stale attempt added to the index")
	}

	// A key outside the attempt's own prefix: the tenancy check.
	evil := &nexusv1.LogChunk{Seq: 901, Key: "other-tenant/logs/" + attemptID + "/00000901.ndjson",
		FirstLine: 100, LineCount: 1, SizeBytes: 1, Sha256: strings.Repeat("0", 64)}
	ack, _ = idx.ReportLogs(ctx, &nexusv1.ReportLogsRequest{AttemptId: attemptID, LeaseToken: token, Chunks: []*nexusv1.LogChunk{evil}})
	if ack.GetAccepted() || ack.GetReason() != "invalid_log_chunk" {
		t.Fatalf("a foreign key was accepted: %+v", ack)
	}

	// The agent "crashed": the attempt is LOST and the log never finalized.
	env.setAttemptState(t, attemptID, "LOST")
	row, _, st, err := env.reader.Summary(ctx, attemptID, "LOST")
	if err != nil || st.State != logs.StatePartial {
		t.Fatalf("crashed agent: state=%q err=%v", st.State, err)
	}
	page, err := env.reader.ReadLines(ctx, attemptID, row, 1, 100)
	if err != nil || len(page.Lines) != 5 || page.Lines[4].Message != "before the crash 4" {
		t.Fatalf("partial log unreadable: %+v err=%v", page.Lines, err)
	}
	// Release the shipper's goroutines; the lease row is gone, so its final
	// report is fenced like any zombie's.
	if _, err := env.pool.Exec(ctx, `DELETE FROM leases WHERE attempt_id = $1`, attemptID); err != nil {
		t.Fatal(err)
	}
	if st := ship.Close(ctx); st.Finalized || !st.Fenced {
		t.Fatalf("a shipper whose lease is gone finalized anyway: %+v", st)
	}
}
