// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/wal"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func vec(cpu, mem int64, gpu int32) *nexusv1.ResourceVector {
	return &nexusv1.ResourceVector{CpuMillis: cpu, MemoryBytes: mem, GpuCount: gpu}
}

// ── Registry: local admission ────────────────────────────────────────────────

// TestLocalAdmissionRejectsOverCommit is the worker half of the both-sides-verify
// rule. The control plane's ledger can be momentarily stale — a completion it has
// not processed, a reconciliation in flight — and accepting more than the node can
// hold is how a node OOMs and takes every sibling attempt with it.
func TestLocalAdmissionRejectsOverCommit(t *testing.T) {
	r := NewRegistry(vec(2000, 2<<30, 1))

	if ok, reason := r.Admit("a1", vec(1500, 1<<30, 1)); !ok {
		t.Fatalf("first admission refused: %s", reason)
	}
	// This one does not fit: 1500+1000 > 2000 cpu, and the single GPU is taken.
	ok, reason := r.Admit("a2", vec(1000, 1<<30, 1))
	if ok {
		t.Fatal("admitted an attempt that over-commits the node")
	}
	if reason == "" {
		t.Error("refusal carried no reason; the operator gets no explanation")
	}
	if r.Count() != 1 {
		t.Errorf("active count = %d, want 1", r.Count())
	}

	// Releasing the first must make room again.
	r.Release("a1")
	if ok, reason := r.Admit("a2", vec(1000, 1<<30, 1)); !ok {
		t.Fatalf("admission refused after release: %s", reason)
	}
}

// TestAdmitIsAtomic: a check-then-commit that was not one critical section would
// let two concurrent admissions both see the same free capacity and both succeed.
func TestAdmitIsAtomic(t *testing.T) {
	// Room for exactly two of the three requests.
	r := NewRegistry(vec(2000, 2<<30, 0))

	var wg sync.WaitGroup
	results := make(chan bool, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, _ := r.Admit(string(rune('a'+i)), vec(1000, 1<<30, 0))
			results <- ok
		}(i)
	}
	wg.Wait()
	close(results)

	n := 0
	for ok := range results {
		if ok {
			n++
		}
	}
	if n != 2 {
		t.Errorf("admitted %d attempts, want exactly 2", n)
	}
	if free := r.FreeCapacity(); free.GetCpuMillis() != 0 {
		t.Errorf("free cpu = %d after saturating admission, want 0", free.GetCpuMillis())
	}
}

// TestReleaseIsIdempotent: Release runs on every terminal path, and a double
// subtraction would permanently inflate the worker's apparent free capacity.
func TestRegistryReleaseIsIdempotent(t *testing.T) {
	r := NewRegistry(vec(2000, 2<<30, 0))
	if ok, _ := r.Admit("a1", vec(1000, 1<<30, 0)); !ok {
		t.Fatal("admit failed")
	}
	r.Release("a1")
	r.Release("a1")
	r.Release("a1")

	free := r.FreeCapacity()
	if free.GetCpuMillis() != 2000 {
		t.Errorf("free cpu = %d after repeated release, want exactly 2000", free.GetCpuMillis())
	}
}

// TestDuplicateAdmitIsRefused: re-admitting the same attempt would double-count
// its reservation and permanently shrink the worker.
func TestDuplicateAdmitIsRefused(t *testing.T) {
	r := NewRegistry(vec(4000, 4<<30, 0))
	if ok, _ := r.Admit("a1", vec(1000, 1<<30, 0)); !ok {
		t.Fatal("first admit failed")
	}
	ok, reason := r.Admit("a1", vec(1000, 1<<30, 0))
	if ok {
		t.Fatal("the same attempt was admitted twice")
	}
	if reason != "already_running" {
		t.Errorf("reason = %q, want already_running", reason)
	}
	if free := r.FreeCapacity(); free.GetCpuMillis() != 3000 {
		t.Errorf("free cpu = %d; the duplicate was double-counted", free.GetCpuMillis())
	}
}

// TestDrainingClosesAdmissionButKeepsRunningWork is the DRAINING semantics from
// §5.5: no new tasks, finish current, then DEAD.
func TestDrainingClosesAdmissionButKeepsRunningWork(t *testing.T) {
	r := NewRegistry(vec(4000, 4<<30, 0))
	if ok, _ := r.Admit("running", vec(1000, 1<<30, 0)); !ok {
		t.Fatal("admit failed")
	}
	r.SetDraining(true)

	if ok, reason := r.Admit("new", vec(1000, 1<<30, 0)); ok {
		t.Error("a draining worker accepted new work")
	} else if reason != "worker_draining" {
		t.Errorf("reason = %q, want worker_draining", reason)
	}
	// The in-flight attempt must survive: DRAIN means finish, not abandon.
	if r.Count() != 1 {
		t.Errorf("in-flight count = %d, want 1", r.Count())
	}
	if r.Idle() {
		t.Error("Idle() true while work is still running; the agent would exit early")
	}
	r.Release("running")
	if !r.Idle() {
		t.Error("Idle() false after all work finished; the drain would never complete")
	}
}

// TestRunningAttemptIDsIsSortedGroundTruth: this list is what the control plane
// reconciles against, so it must be complete and deterministic.
func TestRunningAttemptIDsIsSortedGroundTruth(t *testing.T) {
	r := NewRegistry(vec(9000, 9<<30, 0))
	for _, id := range []string{"c", "a", "b"} {
		if ok, _ := r.Admit(id, vec(1000, 1<<30, 0)); !ok {
			t.Fatalf("admit %s failed", id)
		}
	}
	got := r.RunningAttemptIDs()
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("running ids = %v, want sorted [a b c]", got)
	}
	r.Release("b")
	got = r.RunningAttemptIDs()
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("running ids = %v after release, want [a c]", got)
	}
}

func TestUtilizationTracksCommitment(t *testing.T) {
	r := NewRegistry(vec(4000, 4<<30, 0))
	if cpu, mem := r.Utilization(); cpu != 0 || mem != 0 {
		t.Errorf("idle utilization = %v/%v, want 0/0", cpu, mem)
	}
	if ok, _ := r.Admit("a", vec(2000, 2<<30, 0)); !ok {
		t.Fatal("admit failed")
	}
	cpu, mem := r.Utilization()
	if cpu != 0.5 || mem != 0.5 {
		t.Errorf("utilization = %v/%v, want 0.5/0.5", cpu, mem)
	}
}

// ── Reporter: the WAL ordering contract ──────────────────────────────────────

type fakeReportClient struct {
	mu        sync.Mutex
	results   []*nexusv1.ReportResultRequest
	started   []*nexusv1.ReportStartedRequest
	failFor   int
	deny      string
	callsSeen int
}

func (f *fakeReportClient) ReportStarted(_ context.Context, req *nexusv1.ReportStartedRequest) (*nexusv1.Ack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, req)
	return &nexusv1.Ack{Accepted: true}, nil
}

func (f *fakeReportClient) ReportProgress(_ context.Context, _ *nexusv1.ReportProgressRequest) (*nexusv1.Ack, error) {
	return &nexusv1.Ack{Accepted: true}, nil
}

func (f *fakeReportClient) ReportResult(_ context.Context, req *nexusv1.ReportResultRequest) (*nexusv1.Ack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callsSeen++
	f.results = append(f.results, req)
	if f.callsSeen <= f.failFor {
		return nil, errors.New("transient transport failure")
	}
	if f.deny != "" {
		return &nexusv1.Ack{Accepted: false, Reason: f.deny}, nil
	}
	return &nexusv1.Ack{Accepted: true}, nil
}

func (f *fakeReportClient) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callsSeen
}

func newTestWAL(t *testing.T) *wal.WAL {
	t.Helper()
	w, err := wal.Open(t.TempDir())
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	return w
}

// TestReportResultPersistsBeforeReportingAndDeletesAfterAck is the §6.3 ordering:
// fsync the outcome, THEN report, THEN delete. Any other order reintroduces the
// lost-result window the WAL exists to close.
func TestReportResultPersistsBeforeReportingAndDeletesAfterAck(t *testing.T) {
	w := newTestWAL(t)
	c := &fakeReportClient{}
	r := NewReporter(c, w, testLog())

	req := &nexusv1.ReportResultRequest{
		AttemptId: "a1", LeaseToken: "tok",
		Outcome:       nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
		EndedAtUnixMs: time.Now().UnixMilli(),
	}
	if err := r.ReportResult(context.Background(), req, wal.Entry{AttemptID: "a1", LeaseToken: "tok"}); err != nil {
		t.Fatalf("ReportResult: %v", err)
	}
	// Acked, so the entry must be gone — otherwise every boot re-reports forever.
	if entries, _ := w.List(); len(entries) != 0 {
		t.Errorf("WAL still holds %d entries after a durable ack", len(entries))
	}
}

// TestUnackedResultStaysInTheWALForReplay is the durability promise: an outcome we
// could not deliver must survive on disk so the next boot delivers it, converting
// an expensive re-execution into a cheap re-report.
func TestUnackedResultStaysInTheWALForReplay(t *testing.T) {
	w := newTestWAL(t)
	// Fail more times than the reporter will retry.
	c := &fakeReportClient{failFor: 100}
	r := NewReporter(c, w, testLog())
	r.MaxAttempts = 3
	r.BaseBackoff = time.Millisecond
	r.MaxBackoff = 2 * time.Millisecond

	req := &nexusv1.ReportResultRequest{
		AttemptId: "a1", LeaseToken: "tok",
		Outcome: nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
	}
	err := r.ReportResult(context.Background(), req, wal.Entry{AttemptID: "a1", LeaseToken: "tok"})
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	entries, _ := w.List()
	if len(entries) != 1 {
		t.Fatalf("the undelivered outcome was LOST: %d entries remain", len(entries))
	}
	if entries[0].State != wal.StateCompleted {
		t.Errorf("entry state = %q, want COMPLETED so replay re-reports the real outcome", entries[0].State)
	}
	if entries[0].Outcome != wal.OutcomeSucceeded {
		t.Errorf("entry outcome = %q, want SUCCEEDED", entries[0].Outcome)
	}
	if c.calls() != 3 {
		t.Errorf("made %d report attempts, want MaxAttempts=3", c.calls())
	}
}

// TestRejectedResultDropsTheWALEntry: a stale lease token is a permanent refusal,
// and the idempotency key protects the side effects. Keeping the entry would grow
// the WAL forever against a server that will never accept it.
func TestRejectedResultDropsTheWALEntry(t *testing.T) {
	w := newTestWAL(t)
	c := &fakeReportClient{deny: "stale_lease_token"}
	r := NewReporter(c, w, testLog())
	r.BaseBackoff = time.Millisecond

	req := &nexusv1.ReportResultRequest{AttemptId: "a1", LeaseToken: "stale"}
	err := r.ReportResult(context.Background(), req, wal.Entry{AttemptID: "a1"})
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("error = %v, want ErrRejected", err)
	}
	if entries, _ := w.List(); len(entries) != 0 {
		t.Errorf("a permanently rejected entry was retained; %d remain", len(entries))
	}
	// An explicit refusal must NOT consume the retry budget.
	if c.calls() != 1 {
		t.Errorf("made %d attempts against an explicit refusal, want 1", c.calls())
	}
}

// TestReportSucceedsAfterTransientFailures exercises the retry+backoff path.
func TestReportSucceedsAfterTransientFailures(t *testing.T) {
	w := newTestWAL(t)
	c := &fakeReportClient{failFor: 2}
	r := NewReporter(c, w, testLog())
	r.BaseBackoff = time.Millisecond
	r.MaxBackoff = 5 * time.Millisecond

	req := &nexusv1.ReportResultRequest{
		AttemptId: "a1", LeaseToken: "tok",
		Outcome: nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
	}
	if err := r.ReportResult(context.Background(), req, wal.Entry{AttemptID: "a1"}); err != nil {
		t.Fatalf("ReportResult: %v", err)
	}
	if c.calls() != 3 {
		t.Errorf("calls = %d, want 3 (two failures then success)", c.calls())
	}
	if entries, _ := w.List(); len(entries) != 0 {
		t.Errorf("WAL not drained after a successful retry; %d remain", len(entries))
	}
}

// TestReportStartedFailureIsNotFatal: the attempt is already recorded STARTING in
// the WAL and the lease is being renewed, so the control plane learns the outcome
// regardless. Failing the task for a missed status notification would discard real
// work for a cosmetic reason.
func TestReportStartedFailureIsNotFatal(t *testing.T) {
	w := newTestWAL(t)
	c := &failingStartedClient{}
	r := NewReporter(c, w, testLog())
	// Must not panic and must return.
	r.ReportStarted(context.Background(), "a1", "tok")
}

type failingStartedClient struct{ fakeReportClient }

func (f *failingStartedClient) ReportStarted(_ context.Context, _ *nexusv1.ReportStartedRequest) (*nexusv1.Ack, error) {
	return nil, errors.New("unreachable")
}

// ── Runner ───────────────────────────────────────────────────────────────────

type fakeLeases struct {
	mu     sync.Mutex
	tokens map[string]string
}

func newFakeLeases() *fakeLeases { return &fakeLeases{tokens: map[string]string{}} }
func (f *fakeLeases) Track(id, tok string, _ time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[id] = tok
}
func (f *fakeLeases) Untrack(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tokens, id)
}
func (f *fakeLeases) Token(id string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tokens[id]
	return t, ok
}

func newTestRunner(t *testing.T, c ReportClient) (*Runner, *Registry, *wal.WAL) {
	t.Helper()
	reg := NewRegistry(vec(8000, 8<<30, 0))
	w := newTestWAL(t)
	execs := map[string]executor.Executor{
		"subprocess": &executor.SubprocessExecutor{
			Root: t.TempDir(), Log: testLog(), AllowUnenforceableLimits: true,
		},
	}
	r := NewRunner(execs, reg, newFakeLeases(), NewReporter(c, w, testLog()), w, nil, testLog())
	// Each test's attempt sockets live in a private root rather than the
	// shared default under /tmp (ADR-0032).
	r.TaskAPIRoot = testTaskAPIRoot(t)
	return r, reg, w
}

func assignment(attemptID, script string) *nexusv1.TaskAssignment {
	return &nexusv1.TaskAssignment{
		AttemptId: attemptID, RunId: "run-1", NodeId: "node-1", AttemptNumber: 1,
		IdempotencyKey: "idem-" + attemptID,
		LeaseToken:     "tok-" + attemptID,
		Spec: &nexusv1.TaskSpec{
			Id: "t1",
			Runtime: &nexusv1.RuntimeSpec{
				Executor: "subprocess", Command: []string{"/bin/sh", "-c"}, Args: []string{script},
			},
			Resources: &nexusv1.ResourceRequest{Request: vec(1000, 1<<30, 0)},
		},
		StartToCloseDeadlineUnixMs: time.Now().Add(30 * time.Second).UnixMilli(),
	}
}

// TestRunnerReportsSuccessWithOutput covers the happy path end to end, including
// that the WAL is drained and local capacity is released.
func TestRunnerReportsSuccessWithOutput(t *testing.T) {
	c := &fakeReportClient{}
	r, reg, w := newTestRunner(t, c)
	a := assignment("a1", `printf '{"ok":true}' > "$NEXUS_OUTPUT_FILE"`)

	if ok, reason := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatalf("admit: %s", reason)
	}
	r.Run(context.Background(), a)

	c.mu.Lock()
	results := append([]*nexusv1.ReportResultRequest(nil), c.results...)
	c.mu.Unlock()
	if len(results) != 1 {
		t.Fatalf("expected one result report, got %d", len(results))
	}
	if got := results[0].GetOutcome(); got != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		t.Errorf("outcome = %v, want SUCCEEDED", got)
	}
	if ref := results[0].GetOutputs()["result"]; ref == nil {
		t.Error("no output ref reported")
	} else if string(ref.GetInline()) != `{"ok":true}` {
		t.Errorf("inline output = %q", ref.GetInline())
	}
	if results[0].GetMetrics().GetExitCode() != 0 {
		t.Errorf("exit code = %d", results[0].GetMetrics().GetExitCode())
	}
	// Terminal cleanup must have released local capacity, or the worker shrinks
	// permanently with every task it runs.
	if reg.Count() != 0 {
		t.Errorf("registry still holds %d attempts after completion", reg.Count())
	}
	if entries, _ := w.List(); len(entries) != 0 {
		t.Errorf("WAL not drained; %d entries remain", len(entries))
	}
}

// TestRunnerReportsApplicationFailure: a non-zero exit is the task's own decision
// and must be classified APPLICATION, not retried around the fleet.
func TestRunnerReportsApplicationFailure(t *testing.T) {
	c := &fakeReportClient{}
	r, reg, _ := newTestRunner(t, c)
	a := assignment("a2", `echo "bad input" >&2; exit 4`)
	if ok, _ := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatal("admit failed")
	}
	r.Run(context.Background(), a)

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.results) != 1 {
		t.Fatalf("expected one report, got %d", len(c.results))
	}
	got := c.results[0]
	if got.GetOutcome() != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED {
		t.Errorf("outcome = %v, want FAILED", got.GetOutcome())
	}
	if got.GetError().GetClass() != nexusv1.ErrorClass_ERROR_CLASS_APPLICATION {
		t.Errorf("class = %v, want ERROR_CLASS_APPLICATION", got.GetError().GetClass())
	}
	if got.GetMetrics().GetExitCode() != 4 {
		t.Errorf("exit code = %d, want 4", got.GetMetrics().GetExitCode())
	}
}

// TestRunnerReportsTimeout: the deadline must produce TIMED_OUT with the TIMEOUT
// class, so the operator is pointed at the deadline or the code rather than at a
// bug that is not there.
func TestRunnerReportsTimeout(t *testing.T) {
	c := &fakeReportClient{}
	r, reg, _ := newTestRunner(t, c)
	a := assignment("a3", `i=0; while [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done`)
	a.StartToCloseDeadlineUnixMs = time.Now().Add(1500 * time.Millisecond).UnixMilli()
	if ok, _ := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatal("admit failed")
	}
	r.Run(context.Background(), a)

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.results) != 1 {
		t.Fatalf("expected one report, got %d", len(c.results))
	}
	if got := c.results[0].GetOutcome(); got != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_TIMED_OUT {
		t.Errorf("outcome = %v, want TIMED_OUT", got)
	}
	if got := c.results[0].GetError().GetClass(); got != nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT {
		t.Errorf("class = %v, want ERROR_CLASS_TIMEOUT", got)
	}
}

// TestRunnerWritesSTARTINGBeforeExecuting proves the WAL intent record precedes
// execution: without it, a crash mid-task leaves no record that we ever started.
func TestRunnerWritesStartingBeforeExecuting(t *testing.T) {
	c := &fakeReportClient{}
	r, reg, w := newTestRunner(t, c)

	// The task blocks until we observe the WAL entry, which can only exist if it
	// was written BEFORE Start.
	a := assignment("a4", `i=0; while [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done`)
	a.StartToCloseDeadlineUnixMs = time.Now().Add(3 * time.Second).UnixMilli()
	if ok, _ := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatal("admit failed")
	}

	done := make(chan struct{})
	go func() { r.Run(context.Background(), a); close(done) }()

	var seen bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e, ok, _ := w.Get("a4"); ok && e.State == wal.StateStarting {
			seen = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !seen {
		t.Error("no STARTING entry was observed while the task was running")
	}
	<-done
}

// TestRunnerUnsupportedExecutorIsInfrastructure: the scheduler should never place
// a task here whose executor we did not advertise, so this is OUR bug, and failing
// the user's task terminally for our routing mistake is the wrong direction.
func TestRunnerUnsupportedExecutorIsInfrastructure(t *testing.T) {
	c := &fakeReportClient{}
	r, reg, _ := newTestRunner(t, c)
	a := assignment("a5", "true")
	a.Spec.Runtime.Executor = "firecracker"
	if ok, _ := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatal("admit failed")
	}
	r.Run(context.Background(), a)

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.results) != 1 {
		t.Fatalf("expected one report, got %d", len(c.results))
	}
	if got := c.results[0].GetError().GetClass(); got != nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE {
		t.Errorf("class = %v, want ERROR_CLASS_INFRASTRUCTURE", got)
	}
	if !c.results[0].GetError().GetRetryable() {
		t.Error("an unsupported-executor failure must be retryable: another worker may support it")
	}
}

// TestOnLeaseRevokedTearsDownTheAttempt: a revoked lease means the work is already
// someone else's, so any result we produce will be rejected — continuing is
// guaranteed waste.
func TestOnLeaseRevokedTearsDownTheAttempt(t *testing.T) {
	c := &fakeReportClient{}
	r, reg, _ := newTestRunner(t, c)
	a := assignment("a6", `trap 'exit 0' TERM; i=0; while [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done`)
	a.StartToCloseDeadlineUnixMs = time.Now().Add(2 * time.Minute).UnixMilli()
	if ok, _ := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatal("admit failed")
	}

	done := make(chan struct{})
	go func() { r.Run(context.Background(), a); close(done) }()

	// Wait for the attempt to be running before revoking.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && r.RunningCount() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if r.RunningCount() == 0 {
		t.Fatal("attempt never started")
	}
	time.Sleep(500 * time.Millisecond) // let the trap install
	r.OnLeaseRevoked("a6", "test")

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the attempt did not stop after its lease was revoked")
	}
	if reg.Count() != 0 {
		t.Errorf("registry still holds %d attempts", reg.Count())
	}
}

// ── Puller ───────────────────────────────────────────────────────────────────

type fakePollClient struct {
	mu    sync.Mutex
	reqs  []*nexusv1.PollTaskRequest
	resp  *nexusv1.PollTaskResponse
	err   error
	drain int
}

func (f *fakePollClient) PollTask(ctx context.Context, req *nexusv1.PollTaskRequest) (*nexusv1.PollTaskResponse, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	resp, err := f.resp, f.err
	if f.drain > 0 {
		f.drain--
	} else {
		resp = &nexusv1.PollTaskResponse{PollAgainAfterMs: 10}
	}
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if resp == nil {
		resp = &nexusv1.PollTaskResponse{}
	}
	return resp, nil
}

func (f *fakePollClient) requests() []*nexusv1.PollTaskRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*nexusv1.PollTaskRequest(nil), f.reqs...)
}

// TestPollerUsesLongPolling: Phase 1 is poll-only (NATS deferred), so long polling
// is what carries dispatch latency instead of forcing a choice between a tight
// poll loop and seconds of delay.
func TestPullerUsesLongPolling(t *testing.T) {
	c := &fakePollClient{}
	reg := NewRegistry(vec(4000, 4<<30, 0))
	p := NewPuller("w-1", []string{"default", "gpu"}, c, reg, newFakeLeases(), nil, testLog())
	p.LongPollWait = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = p.Run(ctx)

	reqs := c.requests()
	if len(reqs) == 0 {
		t.Fatal("no poll was sent")
	}
	r := reqs[0]
	if r.GetWaitMs() != 50 {
		t.Errorf("wait_ms = %d, want the long-poll window (50)", r.GetWaitMs())
	}
	if r.GetWorkerId() != "w-1" {
		t.Errorf("worker_id = %q", r.GetWorkerId())
	}
	if len(r.GetQueues()) != 2 {
		t.Errorf("queues = %v, want both advertised queues", r.GetQueues())
	}
	if r.GetFreeCapacity().GetCpuMillis() != 4000 {
		t.Errorf("free capacity not reported: %+v", r.GetFreeCapacity())
	}
	if r.GetMaxTasks() <= 0 {
		t.Error("max_tasks must be positive so batching can amortise the round-trip")
	}
}

// TestPullerStopsWhenDraining: DRAINING means no new work, which for the puller
// means stop polling entirely (§5.5).
func TestPullerStopsWhenDraining(t *testing.T) {
	c := &fakePollClient{}
	reg := NewRegistry(vec(4000, 4<<30, 0))
	reg.SetDraining(true)
	p := NewPuller("w-1", []string{"default"}, c, reg, newFakeLeases(), nil, testLog())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("a draining puller did not return promptly")
	}
	if n := len(c.requests()); n != 0 {
		t.Errorf("a draining worker sent %d polls, want 0", n)
	}
}

// TestPullerRejectsOverCapacityAssignmentWithoutTrackingIt: rejecting means NOT
// tracking the lease, so letting it expire is how the control plane learns the
// placement failed. Reporting a failure instead would consume one of the task's
// retry attempts for a scheduling mismatch that is not the task's fault.
func TestPullerRejectsOverCapacityAssignmentWithoutTrackingIt(t *testing.T) {
	reg := NewRegistry(vec(1000, 1<<30, 0))
	leases := newFakeLeases()
	c := &fakePollClient{}
	p := NewPuller("w-1", []string{"default"}, c, reg, leases, nil, testLog())

	// Asks for far more than this worker advertises.
	a := assignment("too-big", "true")
	a.Spec.Resources = &nexusv1.ResourceRequest{Request: vec(9000, 9<<30, 0)}
	p.dispatch(context.Background(), a)

	if reg.Count() != 0 {
		t.Errorf("an over-capacity assignment was admitted")
	}
	if _, ok := leases.Token("too-big"); ok {
		t.Error("the lease was tracked for a rejected assignment; it must be allowed to expire")
	}
}
