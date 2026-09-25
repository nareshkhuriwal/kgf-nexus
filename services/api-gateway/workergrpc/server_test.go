// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package workergrpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// ── test doubles ──────────────────────────────────────────────────────────

// leaseDB models only what the fencing path reads: the leases table and the two
// reconciliation projections.
type leaseDB struct {
	mu sync.Mutex
	// attemptID -> token
	tokens map[string]string
	// attemptID -> whether the lease is expired
	expired map[string]bool
	// workerID -> reservation attempt ids
	reservations map[string][]string
	// workerID -> active attempt ids
	active map[string][]string
	// failVerify makes the lease read fail, to prove a database fault is not
	// reported as a stale token.
	failVerify bool
}

func newLeaseDB() *leaseDB {
	return &leaseDB{
		tokens:       map[string]string{},
		expired:      map[string]bool{},
		reservations: map[string][]string{},
		active:       map[string][]string{},
	}
}

func (d *leaseDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag(""), fmt.Errorf("leaseDB: unexpected Exec")
}

func (d *leaseDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !strings.Contains(sql, "FROM leases") {
		return errRow{fmt.Errorf("leaseDB: unexpected QueryRow: %s", sql)}
	}
	if d.failVerify {
		return errRow{errors.New("connection reset by peer")}
	}
	attemptID := args[0].(string)
	token, ok := d.tokens[attemptID]
	if !ok {
		return errRow{pgx.ErrNoRows}
	}
	return valueRow{[]any{
		"lease-" + attemptID, attemptID, "worker-1", token,
		time.Now().Add(30 * time.Second), int32(1), d.expired[attemptID],
	}}
}

func (d *leaseDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	workerID := args[0].(string)
	switch {
	case strings.Contains(sql, "FROM reservations"):
		return &sliceRows{vals: d.reservations[workerID]}, nil
	case strings.Contains(sql, "FROM task_attempts"):
		return &sliceRows{vals: d.active[workerID]}, nil
	}
	return nil, fmt.Errorf("leaseDB: unexpected Query: %s", sql)
}

func (d *leaseDB) Begin(ctx context.Context) (pgx.Tx, error) {
	return nil, fmt.Errorf("leaseDB: transactions not modelled")
}

type errRow struct{ err error }

func (r errRow) Scan(dest ...any) error { return r.err }

type valueRow struct{ vals []any }

func (r valueRow) Scan(dest ...any) error {
	if len(dest) != len(r.vals) {
		return fmt.Errorf("scan wants %d destinations, row has %d", len(dest), len(r.vals))
	}
	for i, d := range dest {
		switch t := d.(type) {
		case *string:
			*t = r.vals[i].(string)
		case *time.Time:
			*t = r.vals[i].(time.Time)
		case *int32:
			*t = r.vals[i].(int32)
		case *bool:
			*t = r.vals[i].(bool)
		default:
			return fmt.Errorf("unsupported destination %T", d)
		}
	}
	return nil
}

type sliceRows struct {
	vals []string
	i    int
}

func (r *sliceRows) Next() bool {
	if r.i >= len(r.vals) {
		return false
	}
	r.i++
	return true
}
func (r *sliceRows) Scan(dest ...any) error {
	p, ok := dest[0].(*string)
	if !ok {
		return fmt.Errorf("unsupported destination %T", dest[0])
	}
	*p = r.vals[r.i-1]
	return nil
}
func (r *sliceRows) Close()                                       {}
func (r *sliceRows) Err() error                                   { return nil }
func (r *sliceRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("") }
func (r *sliceRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *sliceRows) Values() ([]any, error)                       { return nil, nil }
func (r *sliceRows) RawValues() [][]byte                          { return nil }
func (r *sliceRows) Conn() *pgx.Conn                              { return nil }
func (r *sliceRows) TypeMap() *pgtype.Map                         { return nil }

// fakeRM records what the gateway asked the resource-manager to do, which is how
// the reconciliation assertions are made: the gateway must not write those tables
// itself (ADR-0011).
type fakeRM struct {
	mu               sync.Mutex
	released         []string
	registered       int
	heartbeats       int
	state            string
	exists           bool
	releaseShouldErr bool
}

func (f *fakeRM) RegisterWorker(ctx context.Context, req *nexusv1.RegisterRequest, hash string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered++
	// A FRESH id per call: that is the per-session identity guarantee.
	return fmt.Sprintf("worker-session-%d", f.registered), nil
}

func (f *fakeRM) RecordHeartbeat(ctx context.Context, workerID string, free *nexusv1.ResourceVector, cpu, mem float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats++
	return nil
}

func (f *fakeRM) ReleaseReservation(ctx context.Context, workerID, attemptID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.releaseShouldErr {
		return errors.New("resource manager unavailable")
	}
	f.released = append(f.released, attemptID)
	return nil
}

func (f *fakeRM) WorkerState(ctx context.Context, workerID string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.exists, nil
}

// fakeSched records whether an attempt RPC's effect was applied. A stale report
// must leave every counter here at zero — that is what "never apply a stale
// report" means concretely.
type fakeSched struct {
	mu        sync.Mutex
	started   int
	progress  int
	results   int
	cancels   []string
	claimFunc func() []*nexusv1.TaskAssignment
	// progressStale makes RecordProgress answer as a scheduler whose own lease
	// fence rejected the report (the lease ended after the gateway's check).
	progressStale bool
	lastProgress  progressCall
}

// progressCall is what the gateway forwarded on the last RecordProgress.
type progressCall struct {
	attemptID, token, msg string
	frac                  float64
}

func (f *fakeSched) ClaimForWorker(ctx context.Context, workerID string, free *nexusv1.ResourceVector, queues []string, maxTasks int32) ([]*nexusv1.TaskAssignment, error) {
	if f.claimFunc == nil {
		return nil, nil
	}
	return f.claimFunc(), nil
}
func (f *fakeSched) MarkStarted(ctx context.Context, attemptID string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started++
	return nil
}
func (f *fakeSched) RecordProgress(ctx context.Context, attemptID, token, msg string, frac float64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.progressStale {
		return true, nil
	}
	f.progress++
	f.lastProgress = progressCall{attemptID: attemptID, token: token, msg: msg, frac: frac}
	return false, nil
}
func (f *fakeSched) ApplyResult(ctx context.Context, req *nexusv1.ReportResultRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results++
	return nil
}
func (f *fakeSched) PendingCancellations(ctx context.Context, workerID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancels, nil
}

func testServer(db store.GWDB, rm ResourceManager, sch Scheduler) *Server {
	cfg := DefaultConfig()
	cfg.PollInterval = time.Millisecond
	cfg.MaxPollWait = 20 * time.Millisecond
	return NewServer(db, rm, sch, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// workerCtx carries an authenticated worker principal, as the gRPC interceptor
// would.
func workerCtx() context.Context {
	return authn.WithPrincipal(context.Background(), authn.Principal{
		KeyID: "k1", Namespace: "team-ml", Name: "agent", Role: authn.RoleWorker,
	})
}

// ── fencing: the load-bearing tests ───────────────────────────────────────

// TestStaleLeaseTokenRejectedOnEveryAttemptRPC is CONTRACT.md invariant 3.
//
// Every attempt RPC must reject a stale token with the EXACT reason string, and
// must not apply the report. Checked on all four RPCs rather than one, because
// fencing that holds on ReportResult but leaks on ReportStarted is not fencing —
// a zombie could still drag an attempt back to RUNNING after its replacement
// started.
func TestStaleLeaseTokenRejectedOnEveryAttemptRPC(t *testing.T) {
	ctx := workerCtx()

	rpcs := []struct {
		name string
		call func(*Server) (*nexusv1.Ack, error)
	}{
		{"ReportStarted", func(s *Server) (*nexusv1.Ack, error) {
			return s.ReportStarted(ctx, &nexusv1.ReportStartedRequest{
				AttemptId: "a1", LeaseToken: "tok-stale", StartedAtUnixMs: time.Now().UnixMilli(),
			})
		}},
		{"ReportProgress", func(s *Server) (*nexusv1.Ack, error) {
			return s.ReportProgress(ctx, &nexusv1.ReportProgressRequest{
				AttemptId: "a1", LeaseToken: "tok-stale", Message: "half way", FractionComplete: 0.5,
			})
		}},
		{"ReportResult", func(s *Server) (*nexusv1.Ack, error) {
			return s.ReportResult(ctx, &nexusv1.ReportResultRequest{
				AttemptId: "a1", LeaseToken: "tok-stale",
				Outcome: nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
			})
		}},
	}

	for _, rpc := range rpcs {
		t.Run(rpc.name, func(t *testing.T) {
			db := newLeaseDB()
			// The live lease carries a DIFFERENT token: the attempt was reclaimed
			// and re-dispatched while this worker was partitioned.
			db.tokens["a1"] = "tok-current"
			sch := &fakeSched{}
			s := testServer(db, &fakeRM{exists: true, state: "ACTIVE"}, sch)

			ack, err := rpc.call(s)
			if err != nil {
				t.Fatalf("a stale token must be a protocol-level rejection, not a transport error: %v", err)
			}
			if ack.GetAccepted() {
				t.Fatal("a stale lease token was ACCEPTED; a zombie worker can now corrupt state")
			}
			if got := ack.GetReason(); got != store.ReasonStaleLeaseToken {
				t.Errorf("reason = %q, want %q exactly (worker agents match on this string)", got, store.ReasonStaleLeaseToken)
			}

			sch.mu.Lock()
			defer sch.mu.Unlock()
			if sch.started+sch.progress+sch.results != 0 {
				t.Errorf("a stale report was APPLIED (started=%d progress=%d results=%d); it must never reach the scheduler",
					sch.started, sch.progress, sch.results)
			}
		})
	}
}

// TestCurrentLeaseTokenAccepted proves the fence is not simply refusing everything.
func TestCurrentLeaseTokenAccepted(t *testing.T) {
	ctx := workerCtx()
	db := newLeaseDB()
	db.tokens["a1"] = "tok-current"
	sch := &fakeSched{}
	s := testServer(db, &fakeRM{exists: true, state: "ACTIVE"}, sch)

	ack, err := s.ReportResult(ctx, &nexusv1.ReportResultRequest{
		AttemptId: "a1", LeaseToken: "tok-current",
		Outcome: nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
	})
	if err != nil {
		t.Fatalf("ReportResult: %v", err)
	}
	if !ack.GetAccepted() {
		t.Fatalf("the legitimate lease holder was rejected: %q", ack.GetReason())
	}
	sch.mu.Lock()
	defer sch.mu.Unlock()
	if sch.results != 1 {
		t.Errorf("results applied = %d, want 1", sch.results)
	}
}

// TestReportProgressForwardsMessageFractionAndToken is F96: the executor's
// status line and fraction must reach the scheduler, which stores them, and the
// lease token must travel with them so the scheduler can fence inside its own
// write. The message is forwarded unmodified: sanitizing it is the store's job,
// so there is one place that decides what a stored message may contain.
func TestReportProgressForwardsMessageFractionAndToken(t *testing.T) {
	ctx := workerCtx()
	db := newLeaseDB()
	db.tokens["a1"] = "tok-current"
	sch := &fakeSched{}
	s := testServer(db, &fakeRM{exists: true, state: "ACTIVE"}, sch)

	const msg = "pod Pending: Unschedulable: 0/3 nodes are available: insufficient cpu"
	ack, err := s.ReportProgress(ctx, &nexusv1.ReportProgressRequest{
		AttemptId: "a1", LeaseToken: "tok-current", Message: msg, FractionComplete: 0.25,
	})
	if err != nil {
		t.Fatalf("ReportProgress: %v", err)
	}
	if !ack.GetAccepted() {
		t.Fatalf("the lease holder's progress was rejected: %q", ack.GetReason())
	}
	sch.mu.Lock()
	defer sch.mu.Unlock()
	want := progressCall{attemptID: "a1", token: "tok-current", msg: msg, frac: 0.25}
	if sch.progress != 1 || sch.lastProgress != want {
		t.Fatalf("forwarded %+v (%d calls), want %+v once", sch.lastProgress, sch.progress, want)
	}
}

// TestReportProgressStaleAtTheSchedulerIsRejected: the lease can end between
// the gateway's fence and the scheduler's write. The scheduler fences again and
// says so, and the worker must get the same stale_lease_token answer it would
// have got a moment later from the gateway, not a silent "accepted".
func TestReportProgressStaleAtTheSchedulerIsRejected(t *testing.T) {
	ctx := workerCtx()
	db := newLeaseDB()
	db.tokens["a1"] = "tok-current"
	sch := &fakeSched{progressStale: true}
	s := testServer(db, &fakeRM{exists: true, state: "ACTIVE"}, sch)

	ack, err := s.ReportProgress(ctx, &nexusv1.ReportProgressRequest{
		AttemptId: "a1", LeaseToken: "tok-current", Message: "pod Running",
	})
	if err != nil {
		t.Fatalf("a stale report is a protocol answer, not a transport error: %v", err)
	}
	if ack.GetAccepted() || ack.GetReason() != store.ReasonStaleLeaseToken {
		t.Fatalf("ack = %+v, want rejected with %q", ack, store.ReasonStaleLeaseToken)
	}
}

// TestExpiredLeaseIsStaleEvenWithMatchingToken: the expiry timer may not have
// reclaimed the row yet, but the attempt is already eligible to be declared LOST.
func TestExpiredLeaseIsStaleEvenWithMatchingToken(t *testing.T) {
	ctx := workerCtx()
	db := newLeaseDB()
	db.tokens["a1"] = "tok-1"
	db.expired["a1"] = true
	sch := &fakeSched{}
	s := testServer(db, &fakeRM{exists: true, state: "ACTIVE"}, sch)

	ack, _ := s.ReportResult(ctx, &nexusv1.ReportResultRequest{
		AttemptId: "a1", LeaseToken: "tok-1",
		Outcome: nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
	})
	if ack.GetAccepted() {
		t.Error("an expired lease was accepted; this races the retry that may already be running")
	}
}

// TestMissingTokenIsRejected: an empty token must not be treated as "no fencing
// required". That would be a trivially exploitable bypass.
func TestMissingTokenIsRejected(t *testing.T) {
	ctx := workerCtx()
	db := newLeaseDB()
	db.tokens["a1"] = "tok-current"
	s := testServer(db, &fakeRM{exists: true, state: "ACTIVE"}, &fakeSched{})

	ack, _ := s.ReportStarted(ctx, &nexusv1.ReportStartedRequest{AttemptId: "a1", LeaseToken: ""})
	if ack.GetAccepted() {
		t.Error("an empty lease token was accepted; fencing must not be bypassable by omission")
	}
}

// TestDatabaseFailureIsNotReportedAsStaleToken: saying "stale_lease_token" during a
// database blip would tell every worker in the fleet to abandon work it legitimately
// owns, turning a transient fault into mass duplicate execution.
func TestDatabaseFailureIsNotReportedAsStaleToken(t *testing.T) {
	ctx := workerCtx()
	db := newLeaseDB()
	db.tokens["a1"] = "tok-current"
	db.failVerify = true
	s := testServer(db, &fakeRM{exists: true, state: "ACTIVE"}, &fakeSched{})

	ack, err := s.ReportResult(ctx, &nexusv1.ReportResultRequest{
		AttemptId: "a1", LeaseToken: "tok-current",
	})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if ack.GetAccepted() {
		t.Error("a report was accepted without a successful lease verification")
	}
	if ack.GetReason() == store.ReasonStaleLeaseToken {
		t.Error("a database fault was reported as a stale token; workers would wrongly abandon valid work")
	}
}

// ── registration ──────────────────────────────────────────────────────────

// TestRegisterIssuesPerSessionIdentity is §5.5 / ADR-0010 point 7: a restarted
// agent must NEVER be able to reclaim its predecessor's leases, which is only true
// if identity is per-session rather than per-host.
func TestRegisterIssuesPerSessionIdentity(t *testing.T) {
	ctx := workerCtx()
	rm := &fakeRM{exists: true, state: "ACTIVE"}
	s := testServer(newLeaseDB(), rm, &fakeSched{})

	req := &nexusv1.RegisterRequest{
		Namespace: "team-ml", Hostname: "gpu-box-7", Queues: []string{"gpu"},
		Capacity: &nexusv1.ResourceVector{CpuMillis: 8000}, AgentVersion: "1.0.0",
	}

	first, err := s.Register(ctx, req)
	if err != nil {
		t.Fatalf("first Register: %v", err)
	}
	// The SAME hostname registers again, as a restarted agent would.
	second, err := s.Register(ctx, req)
	if err != nil {
		t.Fatalf("second Register: %v", err)
	}

	if first.GetWorkerId() == second.GetWorkerId() {
		t.Error("a restarted agent on the same hostname received the SAME worker_id; it could now reclaim its predecessor's leases")
	}
	if first.GetSessionToken() == second.GetSessionToken() {
		t.Error("two sessions share a session token")
	}
	if first.GetSessionToken() == "" {
		t.Error("no session token was issued")
	}
	if first.GetHeartbeatIntervalMs() <= 0 {
		t.Error("the server must dictate the heartbeat interval so it can shed load under pressure")
	}
	if first.GetLeaseTtlMs() <= 0 {
		t.Error("no lease TTL was returned; the worker would not know when to renew")
	}
}

func TestRegisterRejectsIncompleteRegistration(t *testing.T) {
	ctx := workerCtx()
	s := testServer(newLeaseDB(), &fakeRM{exists: true}, &fakeSched{})

	cases := []struct {
		name string
		req  *nexusv1.RegisterRequest
	}{
		{"no hostname", &nexusv1.RegisterRequest{Namespace: "team-ml", Queues: []string{"q"}, Capacity: &nexusv1.ResourceVector{}}},
		{"no queues", &nexusv1.RegisterRequest{Namespace: "team-ml", Hostname: "h", Capacity: &nexusv1.ResourceVector{}}},
		{"no capacity", &nexusv1.RegisterRequest{Namespace: "team-ml", Hostname: "h", Queues: []string{"q"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.Register(ctx, c.req); status.Code(err) != codes.InvalidArgument {
				t.Errorf("err = %v, want InvalidArgument", err)
			}
		})
	}
}

// ── heartbeat reconciliation ──────────────────────────────────────────────

// TestHeartbeatReleasesLeakedReservations is the capacity-ledger self-correction
// from §3.2: running_attempt_ids is GROUND TRUTH, and any reservation the worker
// does not acknowledge is a leak. Without this, a worker that restarts and forgets
// its work leaves capacity committed forever and its GPUs never become schedulable
// again.
func TestHeartbeatReleasesLeakedReservations(t *testing.T) {
	ctx := workerCtx()
	db := newLeaseDB()
	// The ledger believes three attempts are running on this worker...
	db.reservations["w1"] = []string{"a1", "a2", "a3"}
	db.active["w1"] = []string{"a1", "a2", "a3"}
	rm := &fakeRM{exists: true, state: "ACTIVE"}
	s := testServer(db, rm, &fakeSched{})

	// ...but the worker acknowledges only one.
	if _, err := s.Heartbeat(ctx, &nexusv1.HeartbeatRequest{
		WorkerId:          "w1",
		RunningAttemptIds: []string{"a1"},
		FreeCapacity:      &nexusv1.ResourceVector{CpuMillis: 4000},
	}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	rm.mu.Lock()
	defer rm.mu.Unlock()
	got := map[string]bool{}
	for _, id := range rm.released {
		got[id] = true
	}
	if !got["a2"] || !got["a3"] {
		t.Errorf("leaked reservations were not released: released = %v, want a2 and a3", rm.released)
	}
	if got["a1"] {
		t.Error("released a reservation the worker DID acknowledge; that would free capacity still in use")
	}
}

// TestHeartbeatDoesNotAdoptUnknownWork: reconciliation is one-directional. We
// release what the worker does not claim, but a claim alone must never establish
// ownership — otherwise a buggy or compromised agent could fabricate work.
func TestHeartbeatDoesNotAdoptUnknownWork(t *testing.T) {
	ctx := workerCtx()
	db := newLeaseDB()
	// The control plane knows of nothing for this worker.
	rm := &fakeRM{exists: true, state: "ACTIVE"}
	s := testServer(db, rm, &fakeSched{})

	resp, err := s.Heartbeat(ctx, &nexusv1.HeartbeatRequest{
		WorkerId:          "w1",
		RunningAttemptIds: []string{"ghost-attempt"},
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	var cancelled bool
	for _, d := range resp.GetDirectives() {
		if d.GetKind() == nexusv1.DirectiveKind_DIRECTIVE_KIND_CANCEL_ATTEMPT && d.GetAttemptId() == "ghost-attempt" {
			cancelled = true
		}
	}
	if !cancelled {
		t.Error("work the control plane does not own was neither adopted nor cancelled; it would run unsupervised forever")
	}
}

// TestHeartbeatDeliversDrainDirective is ADR-0005's key trick: the control plane
// cannot dial a worker, so DRAIN rides the heartbeat RESPONSE.
func TestHeartbeatDeliversDrainDirective(t *testing.T) {
	ctx := workerCtx()
	rm := &fakeRM{exists: true, state: "DRAINING"}
	s := testServer(newLeaseDB(), rm, &fakeSched{})

	resp, err := s.Heartbeat(ctx, &nexusv1.HeartbeatRequest{WorkerId: "w1"})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	var found bool
	for _, d := range resp.GetDirectives() {
		if d.GetKind() == nexusv1.DirectiveKind_DIRECTIVE_KIND_DRAIN {
			found = true
			if d.GetGraceMs() <= 0 {
				t.Error("a DRAIN directive with no grace period gives the worker no time to finish")
			}
		}
	}
	if !found {
		t.Error("a DRAINING worker received no DRAIN directive; the fleet cannot be commanded at all")
	}
	if resp.GetNextHeartbeatMs() <= 0 {
		t.Error("no next_heartbeat_ms; the control plane cannot slow the fleet under load")
	}
}

// TestHeartbeatDeliversCancelDirective: cancel also rides the heartbeat, and only
// for work the worker says it is actually running.
func TestHeartbeatDeliversCancelDirective(t *testing.T) {
	ctx := workerCtx()
	db := newLeaseDB()
	db.reservations["w1"] = []string{"a1"}
	db.active["w1"] = []string{"a1"}
	sch := &fakeSched{cancels: []string{"a1"}}
	s := testServer(db, &fakeRM{exists: true, state: "ACTIVE"}, sch)

	resp, err := s.Heartbeat(ctx, &nexusv1.HeartbeatRequest{
		WorkerId: "w1", RunningAttemptIds: []string{"a1"},
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	var found bool
	for _, d := range resp.GetDirectives() {
		if d.GetKind() == nexusv1.DirectiveKind_DIRECTIVE_KIND_CANCEL_ATTEMPT && d.GetAttemptId() == "a1" {
			found = true
		}
	}
	if !found {
		t.Error("a pending cancellation was not delivered as a directive")
	}
}

// TestHeartbeatFromUnknownSessionOrdersReregister: a hard error would leave a
// healthy host idle until a human restarted it; REREGISTER lets it recover itself.
func TestHeartbeatFromUnknownSessionOrdersReregister(t *testing.T) {
	ctx := workerCtx()
	rm := &fakeRM{exists: false}
	s := testServer(newLeaseDB(), rm, &fakeSched{})

	resp, err := s.Heartbeat(ctx, &nexusv1.HeartbeatRequest{WorkerId: "stale-session"})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if len(resp.GetDirectives()) != 1 ||
		resp.GetDirectives()[0].GetKind() != nexusv1.DirectiveKind_DIRECTIVE_KIND_REREGISTER {
		t.Errorf("directives = %v, want a single REREGISTER", resp.GetDirectives())
	}
}

// TestHeartbeatSurvivesResourceManagerFailure: one stuck release must not abort the
// heartbeat, because the worker still needs its directives and the next heartbeat
// retries the release anyway.
func TestHeartbeatSurvivesResourceManagerFailure(t *testing.T) {
	ctx := workerCtx()
	db := newLeaseDB()
	db.reservations["w1"] = []string{"a1"}
	rm := &fakeRM{exists: true, state: "DRAINING", releaseShouldErr: true}
	s := testServer(db, rm, &fakeSched{})

	resp, err := s.Heartbeat(ctx, &nexusv1.HeartbeatRequest{WorkerId: "w1"})
	if err != nil {
		t.Fatalf("a failed reservation release aborted the whole heartbeat: %v", err)
	}
	if len(resp.GetDirectives()) == 0 {
		t.Error("directives were lost because a release failed")
	}
}

// ── long polling ──────────────────────────────────────────────────────────

// TestPollTaskReturnsWorkImmediately: when work is waiting, the poll must not wait
// out its wait_ms.
func TestPollTaskReturnsWorkImmediately(t *testing.T) {
	ctx := workerCtx()
	sch := &fakeSched{claimFunc: func() []*nexusv1.TaskAssignment {
		return []*nexusv1.TaskAssignment{{AttemptId: "a1", LeaseToken: "tok"}}
	}}
	s := testServer(newLeaseDB(), &fakeRM{exists: true, state: "ACTIVE"}, sch)

	start := time.Now()
	resp, err := s.PollTask(ctx, &nexusv1.PollTaskRequest{
		WorkerId: "w1", WaitMs: 5000, MaxTasks: 1,
	})
	if err != nil {
		t.Fatalf("PollTask: %v", err)
	}
	if len(resp.GetAssignments()) != 1 {
		t.Fatalf("assignments = %d, want 1", len(resp.GetAssignments()))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("PollTask waited %v with work available; it must return immediately", elapsed)
	}
}

// TestPollTaskLongPollsThenReturnsEmpty: an empty poll is a NORMAL answer, not an
// error, and the hold must end.
func TestPollTaskLongPollsThenReturnsEmpty(t *testing.T) {
	ctx := workerCtx()
	s := testServer(newLeaseDB(), &fakeRM{exists: true, state: "ACTIVE"}, &fakeSched{})

	start := time.Now()
	resp, err := s.PollTask(ctx, &nexusv1.PollTaskRequest{WorkerId: "w1", WaitMs: 20})
	if err != nil {
		t.Fatalf("an empty poll must not be an error: %v", err)
	}
	if len(resp.GetAssignments()) != 0 {
		t.Errorf("assignments = %d, want 0", len(resp.GetAssignments()))
	}
	if resp.GetPollAgainAfterMs() <= 0 {
		t.Error("no advisory backoff was returned; an idle fleet would hammer the gateway")
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Errorf("returned after %v; the request should have been HELD for its wait_ms", elapsed)
	}
}

// TestPollTaskCapsWaitMs: an unbounded hold would outlive any load balancer's idle
// timeout and surface to the worker as a confusing transport error.
func TestPollTaskCapsWaitMs(t *testing.T) {
	ctx := workerCtx()
	s := testServer(newLeaseDB(), &fakeRM{exists: true, state: "ACTIVE"}, &fakeSched{})

	start := time.Now()
	if _, err := s.PollTask(ctx, &nexusv1.PollTaskRequest{WorkerId: "w1", WaitMs: 600000}); err != nil {
		t.Fatalf("PollTask: %v", err)
	}
	// MaxPollWait is 20ms in the test config.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("a 10-minute wait_ms was honoured (%v); it must be capped at MaxPollWait", elapsed)
	}
}

// TestPollTaskReturnsOnClientDisconnect: the hold must end when the caller goes
// away, or a gateway replica would accumulate parked goroutines.
func TestPollTaskReturnsOnClientDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(workerCtx())
	s := testServer(newLeaseDB(), &fakeRM{exists: true, state: "ACTIVE"}, &fakeSched{})

	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	done := make(chan struct{})
	go func() {
		_, _ = s.PollTask(ctx, &nexusv1.PollTaskRequest{WorkerId: "w1", WaitMs: 20})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("PollTask did not return after the client disconnected")
	}
}

// ── authorization ─────────────────────────────────────────────────────────

// TestUnauthenticatedCallsRejected: the worker protocol must not be open. Every RPC
// checks, because one unchecked RPC is a full bypass.
func TestUnauthenticatedCallsRejected(t *testing.T) {
	s := testServer(newLeaseDB(), &fakeRM{exists: true}, &fakeSched{})
	bare := context.Background()

	if _, err := s.Register(bare, &nexusv1.RegisterRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Register: %v, want Unauthenticated", err)
	}
	if _, err := s.Heartbeat(bare, &nexusv1.HeartbeatRequest{WorkerId: "w1"}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Heartbeat: %v, want Unauthenticated", err)
	}
	if _, err := s.PollTask(bare, &nexusv1.PollTaskRequest{WorkerId: "w1"}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("PollTask: %v, want Unauthenticated", err)
	}
	if _, err := s.ReportResult(bare, &nexusv1.ReportResultRequest{AttemptId: "a"}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("ReportResult: %v, want Unauthenticated", err)
	}
	if _, err := s.ExtendLease(bare, &nexusv1.ExtendLeaseRequest{WorkerId: "w1"}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("ExtendLease: %v, want Unauthenticated", err)
	}
}

// TestReaderKeyCannotSpeakWorkerProtocol: a reader credential must not be usable to
// claim work.
func TestReaderKeyCannotSpeakWorkerProtocol(t *testing.T) {
	s := testServer(newLeaseDB(), &fakeRM{exists: true}, &fakeSched{})
	ctx := authn.WithPrincipal(context.Background(), authn.Principal{
		Namespace: "team-ml", Role: authn.RoleReader,
	})
	if _, err := s.PollTask(ctx, &nexusv1.PollTaskRequest{WorkerId: "w1"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("PollTask with a reader key: %v, want PermissionDenied", err)
	}
}
