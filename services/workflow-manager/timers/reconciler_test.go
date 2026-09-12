// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package timers

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

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// timerDB models the timers table's claim/delete/sweep behaviour in memory, which is
// enough to test the reconciler's dispatch and failure policy. The SQL itself
// (FOR UPDATE SKIP LOCKED in particular) is exercised by the integration tests
// against real Postgres, because SKIP LOCKED semantics cannot be faked meaningfully.
type timerDB struct {
	mu      sync.Mutex
	rows    map[string]*timerRow
	claimed map[string]bool
	// claimErr makes the claim fail, to prove a transient database error does not
	// kill the loop.
	claimErr error
	// countErr makes the backlog count fail, to prove a failed sample leaves the
	// gauges at their last good value instead of zeroing them.
	countErr error
	released int64
	// stalledRuns is what the stalled-run sweep's query returns.
	stalledRuns []string
	stalledErr  error
}

// stringRows is a minimal pgx.Rows over single-column string results, which is
// the shape WFMListStalledRuns scans.
type stringRows struct {
	vals []any
	i    int
}

func (r *stringRows) Close()                        {}
func (r *stringRows) Err() error                    { return nil }
func (r *stringRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (r *stringRows) FieldDescriptions() []pgconn.FieldDescription {
	return []pgconn.FieldDescription{{Name: "id"}}
}
func (r *stringRows) Next() bool { r.i++; return r.i <= len(r.vals) }
func (r *stringRows) Scan(dest ...any) error {
	if len(dest) != 1 {
		return fmt.Errorf("stringRows: want 1 dest, got %d", len(dest))
	}
	p, ok := dest[0].(*string)
	if !ok {
		return fmt.Errorf("stringRows: dest is not *string")
	}
	*p = r.vals[r.i-1].(string)
	return nil
}
func (r *stringRows) Values() ([]any, error) { return []any{r.vals[r.i-1]}, nil }
func (r *stringRows) RawValues() [][]byte    { return nil }
func (r *stringRows) Conn() *pgx.Conn        { return nil }
func (r *stringRows) TypeMap() *pgtype.Map   { return nil }

type timerRow struct {
	id, kind, subject string
	fireAt            time.Time
	payload           string
}

func newTimerDB() *timerDB {
	return &timerDB{rows: map[string]*timerRow{}, claimed: map[string]bool{}}
}

func (d *timerDB) add(id, kind, subject string, fireAt time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rows[id] = &timerRow{id: id, kind: kind, subject: subject, fireAt: fireAt, payload: "{}"}
}

func (d *timerDB) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.rows)
}

func (d *timerDB) isClaimed(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.claimed[id]
}

func (d *timerDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case strings.Contains(sql, "DELETE FROM timers WHERE id"):
		delete(d.rows, args[0].(string))
		return pgconn.NewCommandTag("DELETE 1"), nil
	case strings.Contains(sql, "SET claimed_by = NULL"):
		var n int64
		for id := range d.claimed {
			delete(d.claimed, id)
			n++
		}
		d.released += n
		return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", n)), nil
	case strings.Contains(sql, "DELETE FROM timers"):
		var n int64
		for id, r := range d.rows {
			if r.subject == args[0].(string) && !d.claimed[id] {
				delete(d.rows, id)
				n++
			}
		}
		return pgconn.NewCommandTag(fmt.Sprintf("DELETE %d", n)), nil
	}
	return pgconn.NewCommandTag(""), fmt.Errorf("timerDB: unhandled Exec: %s", sql)
}

func (d *timerDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	d.mu.Lock()
	defer d.mu.Unlock()
	if strings.Contains(sql, "INSERT INTO timers") {
		id := fmt.Sprintf("t-%d", len(d.rows)+1)
		d.rows[id] = &timerRow{id: id, kind: args[0].(string), subject: args[1].(string)}
		return valueRow{[]any{id}}
	}
	if strings.Contains(sql, "count(*) FILTER") {
		if d.countErr != nil {
			return errRow{d.countErr}
		}
		var due int64
		for id, r := range d.rows {
			if !d.claimed[id] && !r.fireAt.After(time.Now()) {
				due++
			}
		}
		return valueRow{[]any{due, int64(len(d.rows))}}
	}
	return errRow{fmt.Errorf("timerDB: unhandled QueryRow: %s", sql)}
}

func (d *timerDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.claimErr != nil {
		return nil, d.claimErr
	}
	if strings.Contains(sql, "FROM workflow_runs r") {
		if d.stalledErr != nil {
			return nil, d.stalledErr
		}
		rows := make([]any, 0, len(d.stalledRuns))
		for _, id := range d.stalledRuns {
			rows = append(rows, id)
		}
		return &stringRows{vals: rows}, nil
	}
	if !strings.Contains(sql, "FOR UPDATE SKIP LOCKED") {
		return nil, fmt.Errorf("timerDB: unhandled Query: %s", sql)
	}
	limit := args[1].(int)
	var out []*timerRow
	now := time.Now()
	for id, r := range d.rows {
		if d.claimed[id] || r.fireAt.After(now) {
			continue
		}
		d.claimed[id] = true
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return &timerRows{rows: out}, nil
}

func (d *timerDB) Begin(ctx context.Context) (pgx.Tx, error) { return fakeTx{d}, nil }

// fakeTx delegates straight through: the in-memory store has no isolation to model,
// and the transaction's real purpose (holding row locks across the claim) is only
// meaningful against Postgres.
type fakeTx struct{ d *timerDB }

func (t fakeTx) Begin(ctx context.Context) (pgx.Tx, error) { return t, nil }
func (t fakeTx) Commit(ctx context.Context) error          { return nil }
func (t fakeTx) Rollback(ctx context.Context) error        { return nil }
func (t fakeTx) CopyFrom(ctx context.Context, table pgx.Identifier, cols []string, src pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (t fakeTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults { return nil }
func (t fakeTx) LargeObjects() pgx.LargeObjects                               { return pgx.LargeObjects{} }
func (t fakeTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	return nil, nil
}
func (t fakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.d.Exec(ctx, sql, args...)
}
func (t fakeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.d.Query(ctx, sql, args...)
}
func (t fakeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.d.QueryRow(ctx, sql, args...)
}
func (t fakeTx) Conn() *pgx.Conn { return nil }

type errRow struct{ err error }

func (r errRow) Scan(dest ...any) error { return r.err }

type valueRow struct{ vals []any }

func (r valueRow) Scan(dest ...any) error {
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = r.vals[i].(string)
		case *int64:
			*p = r.vals[i].(int64)
		default:
			return fmt.Errorf("unsupported destination %T", d)
		}
	}
	return nil
}

type timerRows struct {
	rows []*timerRow
	i    int
}

func (r *timerRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}
func (r *timerRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	vals := []any{row.id, row.kind, row.subject, row.fireAt, []byte(row.payload)}
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = vals[i].(string)
		case *time.Time:
			*p = vals[i].(time.Time)
		case *[]byte:
			*p = vals[i].([]byte)
		default:
			return fmt.Errorf("unsupported destination %T", d)
		}
	}
	return nil
}
func (r *timerRows) Close()                                       {}
func (r *timerRows) Err() error                                   { return nil }
func (r *timerRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("") }
func (r *timerRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *timerRows) Values() ([]any, error)                       { return nil, nil }
func (r *timerRows) RawValues() [][]byte                          { return nil }
func (r *timerRows) Conn() *pgx.Conn                              { return nil }
func (r *timerRows) TypeMap() *pgtype.Map                         { return nil }

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ── tests ─────────────────────────────────────────────────────────────────

// TestDispatchesDueTimersByKind: each kind reaches its own handler. Routing them all
// to one generic handler would erase the four-timeout distinction (§4.3), which is
// the entire reason there are four.
func TestDispatchesDueTimersByKind(t *testing.T) {
	db := newTimerDB()
	past := time.Now().Add(-time.Second)
	db.add("t1", KindRunDeadline, "run-1", past)
	db.add("t2", KindStartToClose, "attempt-1", past)
	db.add("t3", KindHeartbeat, "attempt-2", past)
	// Not yet due: must NOT be dispatched.
	db.add("t4", KindRetryBackoff, "attempt-3", time.Now().Add(time.Hour))

	var mu sync.Mutex
	seen := map[string]string{}
	rec := New(db, DefaultConfig("test"), quietLogger())
	for _, kind := range []string{KindRunDeadline, KindStartToClose, KindHeartbeat, KindRetryBackoff} {
		k := kind
		rec.Handle(k, func(ctx context.Context, tm store.Timer) error {
			mu.Lock()
			defer mu.Unlock()
			seen[k] = tm.SubjectID
			return nil
		})
	}

	n, err := rec.Cycle(context.Background())
	if err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if n != 3 {
		t.Errorf("dispatched %d, want 3 (the fourth is not due)", n)
	}

	mu.Lock()
	defer mu.Unlock()
	if seen[KindRunDeadline] != "run-1" {
		t.Errorf("run_deadline handler got %q, want run-1", seen[KindRunDeadline])
	}
	if seen[KindStartToClose] != "attempt-1" {
		t.Errorf("start_to_close handler got %q, want attempt-1", seen[KindStartToClose])
	}
	if _, fired := seen[KindRetryBackoff]; fired {
		t.Error("a timer whose fire_at is in the future was dispatched")
	}
}

// TestHandledTimerIsDeleted: a fired timer must not fire forever.
func TestHandledTimerIsDeleted(t *testing.T) {
	db := newTimerDB()
	db.add("t1", KindRunDeadline, "run-1", time.Now().Add(-time.Second))

	rec := New(db, DefaultConfig("test"), quietLogger())
	rec.Handle(KindRunDeadline, func(ctx context.Context, tm store.Timer) error { return nil })

	if _, err := rec.Cycle(context.Background()); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if db.count() != 0 {
		t.Error("a successfully handled timer was not deleted; it would fire again forever")
	}
}

// TestFailedHandlerLeavesClaimForRetry: the claim stays, so the stale-claim sweeper
// returns it to the pool after ClaimTimeout. That gives a BOUNDED retry rather than
// a hot loop against a consistently failing handler — and, crucially, the deadline is
// not silently lost, which is the worst way a deadline system can fail.
func TestFailedHandlerLeavesClaimForRetry(t *testing.T) {
	db := newTimerDB()
	db.add("t1", KindRunDeadline, "run-1", time.Now().Add(-time.Second))

	rec := New(db, DefaultConfig("test"), quietLogger())
	rec.Handle(KindRunDeadline, func(ctx context.Context, tm store.Timer) error {
		return errors.New("downstream service is down")
	})

	n, err := rec.Cycle(context.Background())
	if err != nil {
		t.Fatalf("a failing handler must not fail the whole cycle: %v", err)
	}
	if n != 0 {
		t.Errorf("dispatched = %d, want 0 for a failing handler", n)
	}
	if db.count() != 1 {
		t.Error("a timer whose handler failed was deleted; the deadline would be silently lost")
	}
	if !db.isClaimed("t1") {
		t.Error("the claim was released immediately, which would hot-loop against the failing handler")
	}
}

// TestUnregisteredKindIsNotDiscarded: a timer armed by a newer version than this
// replica understands must be left for a replica that does understand it, not
// dropped.
func TestUnregisteredKindIsNotDiscarded(t *testing.T) {
	db := newTimerDB()
	db.add("t1", "some_future_kind", "subject-1", time.Now().Add(-time.Second))

	rec := New(db, DefaultConfig("test"), quietLogger())
	if _, err := rec.Cycle(context.Background()); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if db.count() != 1 {
		t.Error("a timer with no registered handler was deleted; a real deadline would vanish on a rolling deploy")
	}
}

// TestCycleSurvivesClaimFailure: the reconciler is the only thing that makes
// deadlines fire, so a transient database error must not stop it.
func TestCycleSurvivesClaimFailure(t *testing.T) {
	db := newTimerDB()
	db.claimErr = errors.New("connection refused")

	rec := New(db, DefaultConfig("test"), quietLogger())
	if _, err := rec.Cycle(context.Background()); err == nil {
		t.Error("a claim failure should be reported to the caller so the loop can log and retry")
	}

	// Recovery: once the database is back, the same reconciler proceeds.
	db.mu.Lock()
	db.claimErr = nil
	db.mu.Unlock()
	db.add("t1", KindRunDeadline, "run-1", time.Now().Add(-time.Second))
	rec.Handle(KindRunDeadline, func(ctx context.Context, tm store.Timer) error { return nil })
	if n, err := rec.Cycle(context.Background()); err != nil || n != 1 {
		t.Errorf("after recovery: n=%d err=%v, want 1 and no error", n, err)
	}
}

// TestBatchSizeIsRespected: bounding the batch bounds transaction size and the blast
// radius of a crash mid-batch.
func TestBatchSizeIsRespected(t *testing.T) {
	db := newTimerDB()
	past := time.Now().Add(-time.Second)
	for i := 0; i < 10; i++ {
		db.add(fmt.Sprintf("t%d", i), KindRunDeadline, fmt.Sprintf("run-%d", i), past)
	}
	cfg := DefaultConfig("test")
	cfg.BatchSize = 3

	rec := New(db, cfg, quietLogger())
	rec.Handle(KindRunDeadline, func(ctx context.Context, tm store.Timer) error { return nil })

	n, err := rec.Cycle(context.Background())
	if err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if n != 3 {
		t.Errorf("dispatched %d in one cycle, want the batch size of 3", n)
	}
}

// TestRunStopsOnContextCancel: a graceful shutdown is a successful outcome, not an
// error; reporting it as one makes a clean deploy look like a failure.
func TestRunStopsOnContextCancel(t *testing.T) {
	db := newTimerDB()
	cfg := DefaultConfig("test")
	cfg.Interval = time.Millisecond
	rec := New(db, cfg, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v on a clean shutdown, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// TestRunDispatchesOnTick proves the loop actually fires timers, not just Cycle.
func TestRunDispatchesOnTick(t *testing.T) {
	db := newTimerDB()
	db.add("t1", KindRunDeadline, "run-1", time.Now().Add(-time.Second))

	cfg := DefaultConfig("test")
	cfg.Interval = 2 * time.Millisecond
	rec := New(db, cfg, quietLogger())

	fired := make(chan string, 1)
	rec.Handle(KindRunDeadline, func(ctx context.Context, tm store.Timer) error {
		select {
		case fired <- tm.SubjectID:
		default:
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rec.Run(ctx) }()

	select {
	case got := <-fired:
		if got != "run-1" {
			t.Errorf("fired for %q, want run-1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the reconciler loop never dispatched a due timer")
	}
}

func TestDefaultConfigIsSane(t *testing.T) {
	cfg := DefaultConfig("claimant-1")
	if cfg.Interval <= 0 || cfg.BatchSize <= 0 || cfg.ClaimTimeout <= 0 {
		t.Errorf("DefaultConfig has a non-positive field: %+v", cfg)
	}
	if cfg.Claimant != "claimant-1" {
		t.Errorf("Claimant = %q, want claimant-1", cfg.Claimant)
	}
	// The interval bounds how LATE a deadline fires, so it must stay well inside the
	// 30s lease TTL from ADR-0010.
	if cfg.Interval > 5*time.Second {
		t.Errorf("Interval of %v is too coarse: a 30s lease could expire long before it is noticed", cfg.Interval)
	}
}

func TestNewNormalizesZeroConfig(t *testing.T) {
	rec := New(newTimerDB(), Config{}, nil)
	if rec.cfg.Interval <= 0 || rec.cfg.BatchSize <= 0 || rec.cfg.ClaimTimeout <= 0 {
		t.Errorf("a zero Config was not normalized: %+v", rec.cfg)
	}
}

func TestDecodePayload(t *testing.T) {
	var into struct {
		AttemptNumber int `json:"attempt_number"`
	}
	tm := store.Timer{ID: "t1", Payload: []byte(`{"attempt_number":3}`)}
	if err := DecodePayload(tm, &into); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if into.AttemptNumber != 3 {
		t.Errorf("attempt_number = %d, want 3", into.AttemptNumber)
	}

	// An empty payload is not an error: most timers carry none.
	if err := DecodePayload(store.Timer{ID: "t2"}, &into); err != nil {
		t.Errorf("an empty payload should decode to a no-op, got %v", err)
	}
}

// ── backlog observability ─────────────────────────────────────────────────

// TestSampleBacklogPublishesGauges: the backlog must remain visible to an operator
// after being removed from the readiness probe. If this signal disappears, a
// reconciler falling behind becomes silent again.
func TestSampleBacklogPublishesGauges(t *testing.T) {
	db := newTimerDB()
	past := time.Now().Add(-time.Second)
	db.add("t1", KindRunDeadline, "run-1", past)
	db.add("t2", KindRunDeadline, "run-2", past)
	db.add("t3", KindRunDeadline, "run-3", time.Now().Add(time.Hour))

	cfg := DefaultConfig("test")
	cfg.Metrics = NewMetrics(metrics.NewRegistry())
	rec := New(db, cfg, quietLogger())

	rec.SampleBacklog(context.Background())

	if got := cfg.Metrics.BacklogDue.Value(); got != 2 {
		t.Errorf("BacklogDue = %v, want 2", got)
	}
	if got := cfg.Metrics.BacklogTotal.Value(); got != 3 {
		t.Errorf("BacklogTotal = %v, want 3; due alone cannot distinguish an incoming burst from an idle table", got)
	}
}

// TestSampleBacklogKeepsLastValueOnError: zeroing the gauges on a database error
// would render identically to "the backlog just drained", turning an unreachable
// database into an apparent recovery on every dashboard.
func TestSampleBacklogKeepsLastValueOnError(t *testing.T) {
	db := newTimerDB()
	db.add("t1", KindRunDeadline, "run-1", time.Now().Add(-time.Second))

	cfg := DefaultConfig("test")
	cfg.Metrics = NewMetrics(metrics.NewRegistry())
	rec := New(db, cfg, quietLogger())
	rec.SampleBacklog(context.Background())

	db.mu.Lock()
	db.countErr = errors.New("connection refused")
	db.mu.Unlock()
	rec.SampleBacklog(context.Background())

	if got := cfg.Metrics.BacklogDue.Value(); got != 1 {
		t.Errorf("BacklogDue = %v after a failed sample, want the last good value of 1", got)
	}
}

// TestBacklogIsNotAReadinessSignal documents the availability hazard structurally:
// every replica reads the SAME shared table and computes the SAME number, so a
// readiness check on it would fail the whole tier at once — and this reconciler is
// what drains the backlog, so shedding its replicas makes recovery strictly slower.
func TestBacklogIsNotAReadinessSignal(t *testing.T) {
	db := newTimerDB()
	past := time.Now().Add(-time.Second)
	for i := 0; i < 5; i++ {
		db.add(fmt.Sprintf("t%d", i), KindRunDeadline, fmt.Sprintf("run-%d", i), past)
	}

	// Two replicas, as in production.
	a := New(db, DefaultConfig("replica-a"), quietLogger())
	b := New(db, DefaultConfig("replica-b"), quietLogger())

	dueA, _, err := a.Backlog(context.Background())
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	dueB, _, err := b.Backlog(context.Background())
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	if dueA != dueB {
		t.Fatalf("replicas disagree (%d vs %d); the premise of this test is wrong", dueA, dueB)
	}
	if dueA == 0 {
		t.Fatal("expected a nonzero backlog to reason about")
	}

	// Both replicas still serve: the backlog is reported, never gated on.
	cfgA := DefaultConfig("replica-a")
	cfgA.Metrics = NewMetrics(metrics.NewRegistry())
	withMetrics := New(db, cfgA, quietLogger())
	withMetrics.SampleBacklog(context.Background())
	if got := cfgA.Metrics.BacklogDue.Value(); got != float64(dueA) {
		t.Errorf("BacklogDue = %v, want %d: the backlog must still be observable", got, dueA)
	}
}

// TestLatenessIsRecordedForEveryTimer: a distribution containing only the samples
// past the warning threshold cannot show a reconciler drifting from "always prompt"
// to "usually prompt", which is the early shape of the problem.
func TestLatenessIsRecordedForEveryTimer(t *testing.T) {
	db := newTimerDB()
	db.add("t1", KindRunDeadline, "run-1", time.Now().Add(-2*time.Second))
	db.add("t2", KindRunDeadline, "run-2", time.Now())

	cfg := DefaultConfig("test")
	cfg.Metrics = NewMetrics(metrics.NewRegistry())
	rec := New(db, cfg, quietLogger())
	rec.Handle(KindRunDeadline, func(ctx context.Context, tm store.Timer) error { return nil })

	if _, err := rec.Cycle(context.Background()); err != nil {
		t.Fatalf("Cycle: %v", err)
	}

	_, sum, count := cfg.Metrics.Lateness.Snapshot()
	if count != 2 {
		t.Errorf("lateness count = %d, want 2 (both timers, not only the late one)", count)
	}
	if sum < 2 {
		t.Errorf("lateness sum = %v, want at least the 2s the first timer was late", sum)
	}
}

// TestFiredCounterSeparatesOutcomes: an operator must be able to tell a failing
// handler from an idle reconciler, which one undifferentiated counter cannot do.
func TestFiredCounterSeparatesOutcomes(t *testing.T) {
	db := newTimerDB()
	past := time.Now().Add(-time.Second)
	db.add("t1", KindRunDeadline, "run-1", past)
	db.add("t2", KindStartToClose, "attempt-1", past)
	db.add("t3", "some_future_kind", "subject-1", past)

	cfg := DefaultConfig("test")
	cfg.Metrics = NewMetrics(metrics.NewRegistry())
	rec := New(db, cfg, quietLogger())
	rec.Handle(KindRunDeadline, func(ctx context.Context, tm store.Timer) error { return nil })
	rec.Handle(KindStartToClose, func(ctx context.Context, tm store.Timer) error {
		return errors.New("scheduler is down")
	})

	if _, err := rec.Cycle(context.Background()); err != nil {
		t.Fatalf("Cycle: %v", err)
	}

	fired := cfg.Metrics.Fired
	if got := fired.With(metrics.Labels{"kind": KindRunDeadline, "result": "ok"}).Value(); got != 1 {
		t.Errorf("run_deadline/ok = %v, want 1", got)
	}
	if got := fired.With(metrics.Labels{"kind": KindStartToClose, "result": "error"}).Value(); got != 1 {
		t.Errorf("start_to_close/error = %v, want 1", got)
	}
	if got := fired.With(metrics.Labels{"kind": "some_future_kind", "result": "no_handler"}).Value(); got != 1 {
		t.Errorf("some_future_kind/no_handler = %v, want 1; a missing registration must be distinguishable", got)
	}
}

// TestStaleClaimsAreCounted: a nonzero sweep means a replica died holding claims or
// a handler is persistently failing, and a counter makes the rate alertable rather
// than something an operator must grep the logs for.
func TestStaleClaimsAreCounted(t *testing.T) {
	db := newTimerDB()
	db.add("t1", KindRunDeadline, "run-1", time.Now().Add(-time.Second))

	cfg := DefaultConfig("test")
	cfg.Metrics = NewMetrics(metrics.NewRegistry())
	rec := New(db, cfg, quietLogger())
	rec.Handle(KindRunDeadline, func(ctx context.Context, tm store.Timer) error {
		return errors.New("downstream is down")
	})

	if _, err := rec.Cycle(context.Background()); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	rec.sweepStaleClaims(context.Background())

	if got := cfg.Metrics.StaleClaims.Value(); got != 1 {
		t.Errorf("StaleClaims = %v, want 1", got)
	}
}

// TestReconcilerWorksWithoutMetrics: a nil Metrics must be a no-op, so the
// reconciler stays usable in tests and in any process that has not wired a registry.
func TestReconcilerWorksWithoutMetrics(t *testing.T) {
	db := newTimerDB()
	db.add("t1", KindRunDeadline, "run-1", time.Now().Add(-time.Second))

	rec := New(db, DefaultConfig("test"), quietLogger())
	rec.Handle(KindRunDeadline, func(ctx context.Context, tm store.Timer) error { return nil })

	if _, err := rec.Cycle(context.Background()); err != nil {
		t.Fatalf("Cycle with nil Metrics: %v", err)
	}
	rec.SampleBacklog(context.Background())
	rec.sweepStaleClaims(context.Background())
}

func TestDefaultConfigHasBacklogDefaults(t *testing.T) {
	cfg := DefaultConfig("test")
	// The backlog query aggregates over the whole timers table, so sampling it at
	// the dispatch interval would add real load to the database exactly when a large
	// backlog already has it as the bottleneck.
	if cfg.BacklogInterval <= cfg.Interval {
		t.Errorf("BacklogInterval %v is not coarser than the dispatch interval %v", cfg.BacklogInterval, cfg.Interval)
	}
	if cfg.BacklogAlert <= 0 {
		t.Errorf("BacklogAlert = %d, want a positive threshold", cfg.BacklogAlert)
	}
}

// TestRunSamplesBacklogAtStartup: a replica coming up into an existing backlog must
// report it immediately. A zero gauge for the first sampling interval reads as
// "healthy" at exactly the moment an operator is looking for the truth.
func TestRunSamplesBacklogAtStartup(t *testing.T) {
	db := newTimerDB()
	db.add("t1", "some_unregistered_kind", "subject-1", time.Now().Add(-time.Second))

	cfg := DefaultConfig("test")
	cfg.Interval = time.Hour
	cfg.BacklogInterval = time.Hour
	cfg.Metrics = NewMetrics(metrics.NewRegistry())
	rec := New(db, cfg, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = rec.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for cfg.Metrics.BacklogTotal.Value() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if got := cfg.Metrics.BacklogTotal.Value(); got != 1 {
		t.Errorf("BacklogTotal = %v after startup, want 1 sampled before the first tick", got)
	}
}

// TestStalledRunSweepReportsBeforeItRepairs pins the property that makes this
// sweep safe to have at all.
//
// A sweep that force-transitions runs can hide the bug that stranded them. Every
// run it touches reached a state nothing else would ever have moved, so the
// DETECTION is the valuable half and the repair is secondary: a quiet sweep means
// the point fixes (F62, F67, b97d1ac) are holding, and a sweep that keeps finding
// work means something upstream is broken. If it ever repaired silently, that
// signal would be lost and the next such bug would be found by an operator
// noticing a run that never finished -- seven hours, in the observed case.
func TestStalledRunSweepReportsBeforeItRepairs(t *testing.T) {
	db := newTimerDB()
	db.stalledRuns = []string{"run-a", "run-b"}

	var logs strings.Builder
	reg := metrics.NewRegistry()
	r := New(db, Config{
		Claimant:      "test",
		BatchSize:     10,
		StalledRunAge: time.Minute,
		Metrics:       NewMetrics(reg),
	}, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))

	var reconciled []string
	r.OnStalledRun(func(_ context.Context, runID string) error {
		reconciled = append(reconciled, runID)
		return nil
	})

	r.sweepStalledRuns(context.Background())

	if len(reconciled) != 2 {
		t.Errorf("reconciled %v, want both stalled runs", reconciled)
	}
	// WARN, not INFO or DEBUG: the whole point is that an operator sees this.
	out := logs.String()
	for _, id := range db.stalledRuns {
		if !strings.Contains(out, id) {
			t.Errorf("stalled run %s was repaired without being logged; "+
				"a silent repair hides the upstream defect that stranded it", id)
		}
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("stalled runs were not logged at WARN; got: %s", out)
	}
}

// TestStalledRunSweepSurvivesAFailingRepair proves one unrecoverable run does not
// block the rest. A sweep that aborted on the first error would leave every later
// stalled run stranded, which is the failure it exists to prevent.
func TestStalledRunSweepSurvivesAFailingRepair(t *testing.T) {
	db := newTimerDB()
	db.stalledRuns = []string{"bad", "good"}

	r := New(db, Config{Claimant: "test", BatchSize: 10, StalledRunAge: time.Minute},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	var seen []string
	r.OnStalledRun(func(_ context.Context, runID string) error {
		seen = append(seen, runID)
		if runID == "bad" {
			return errors.New("engine unreachable")
		}
		return nil
	})

	r.sweepStalledRuns(context.Background())

	if len(seen) != 2 {
		t.Errorf("attempted %v; a failing repair must not stop the others", seen)
	}
}

// TestStalledRunSweepDetectsWithoutARepairer proves detection stands alone. A
// deployment that would rather alert than self-heal can leave the callback unset
// and still get the signal, which is the half that matters.
func TestStalledRunSweepDetectsWithoutARepairer(t *testing.T) {
	db := newTimerDB()
	db.stalledRuns = []string{"orphan"}

	var logs strings.Builder
	r := New(db, Config{Claimant: "test", BatchSize: 10, StalledRunAge: time.Minute},
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))

	// No OnStalledRun.
	r.sweepStalledRuns(context.Background())

	if !strings.Contains(logs.String(), "orphan") {
		t.Error("a stalled run went unreported when no repairer was registered; " +
			"detection must not depend on being able to fix it")
	}
}

// TestStalledRunSweepIsQuietWhenNothingIsStalled guards against alert fatigue: a
// backstop that logs on every pass trains operators to ignore it, and this one is
// only meaningful precisely because it is normally silent.
func TestStalledRunSweepIsQuietWhenNothingIsStalled(t *testing.T) {
	db := newTimerDB()

	var logs strings.Builder
	r := New(db, Config{Claimant: "test", BatchSize: 10, StalledRunAge: time.Minute},
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	r.OnStalledRun(func(context.Context, string) error {
		t.Error("the repairer was called with nothing stalled")
		return nil
	})

	r.sweepStalledRuns(context.Background())

	if logs.String() != "" {
		t.Errorf("the sweep logged with nothing stalled: %s", logs.String())
	}
}

// TestStalledRunSweepSurvivesAQueryFailure proves a database blip does not kill
// the loop, matching how every other cycle in this reconciler behaves.
func TestStalledRunSweepSurvivesAQueryFailure(t *testing.T) {
	db := newTimerDB()
	db.stalledErr = errors.New("connection reset")

	r := New(db, Config{Claimant: "test", BatchSize: 10, StalledRunAge: time.Minute},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.OnStalledRun(func(context.Context, string) error {
		t.Error("the repairer ran despite the query failing")
		return nil
	})

	r.sweepStalledRuns(context.Background()) // must not panic
}

// TestNewNormalizesStalledRunConfig pins the fix for a bug this code shipped
// with: main.go built Config as a literal and never set these fields, so
// StalledRunAge was zero and the race guard was disabled in production. The live
// log said `stalled_for_at_least: 0`.
//
// Zero here is not "no minimum age" -- it is "sweep runs that settled a
// millisecond ago", which is exactly the case the guard exists to prevent. A
// caller that forgets the field must get the safe default, not the dangerous one.
func TestNewNormalizesStalledRunConfig(t *testing.T) {
	r := New(newTimerDB(), Config{Claimant: "test"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if r.cfg.StalledRunAge <= 0 {
		t.Errorf("StalledRunAge = %v; a zero value disables the race guard and lets the "+
			"sweep conclude runs that are only mid-conclusion", r.cfg.StalledRunAge)
	}
	if r.cfg.StalledRunInterval <= 0 {
		t.Errorf("StalledRunInterval = %v, want a positive default", r.cfg.StalledRunInterval)
	}
}
