package sessions

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeQuerier records the DELETEs the sweeper issues and can be told to fail, so
// the loop's behaviour is asserted without a database. Only Exec is reachable:
// the sweeper issues exactly one statement, and a panic in the others is a louder
// failure than a silent zero if that ever stops being true.
type fakeQuerier struct {
	mu       sync.Mutex
	calls    int
	deleted  int64
	lastArgs []any
	err      error
	fired    chan struct{}
}

func newFakeQuerier() *fakeQuerier {
	return &fakeQuerier{fired: make(chan struct{}, 16)}
}

func (f *fakeQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	f.calls++
	f.lastArgs = args
	err, deleted := f.err, f.deleted
	f.mu.Unlock()

	select {
	case f.fired <- struct{}{}:
	default:
	}
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	return pgconn.NewCommandTag("DELETE " + itoa(deleted)), nil
}

func (f *fakeQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("the sweeper must not issue a Query")
}

func (f *fakeQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("the sweeper must not issue a QueryRow")
}

func (f *fakeQuerier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeQuerier) args() []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastArgs
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDefaultsAreApplied(t *testing.T) {
	// The defaults are the whole configuration for a deployment that sets no env
	// vars, so a constructor that left them zero would give a ticker that panics
	// and a limit the store silently rewrites.
	sw := New(newFakeQuerier(), quietLogger())
	if sw.Interval != DefaultInterval {
		t.Errorf("interval = %v, want %v", sw.Interval, DefaultInterval)
	}
	if sw.BatchLimit != DefaultBatchLimit {
		t.Errorf("batch limit = %d, want %d", sw.BatchLimit, DefaultBatchLimit)
	}
}

func TestSweepOnceDeletesOnlyAlreadyExpiredRows(t *testing.T) {
	q := newFakeQuerier()
	q.deleted = 7
	sw := New(q, quietLogger())

	before := time.Now()
	if n := sw.SweepOnce(context.Background()); n != 7 {
		t.Errorf("swept %d, want 7", n)
	}
	args := q.args()
	if len(args) != 2 {
		t.Fatalf("DELETE took %d args, want 2 (cutoff, limit)", len(args))
	}
	// The cutoff must be "now", never a future instant: a cutoff ahead of the
	// clock would delete sessions that can still authenticate, logging real
	// operators out. This is the single most important property of the sweep.
	cutoff, ok := args[0].(time.Time)
	if !ok {
		t.Fatalf("cutoff arg is %T, want time.Time", args[0])
	}
	if cutoff.After(time.Now()) || cutoff.Before(before) {
		t.Errorf("cutoff %v is outside the call window [%v, now]; a cutoff in the future deletes live sessions", cutoff, before)
	}
	if args[1] != DefaultBatchLimit {
		t.Errorf("limit arg = %v, want the configured %d", args[1], DefaultBatchLimit)
	}
}

func TestSweepSurvivesATransientDatabaseError(t *testing.T) {
	q := newFakeQuerier()
	q.err = errors.New("connection reset by peer")
	sw := New(q, quietLogger())

	// A failing sweep must return zero rather than panic or propagate: the gateway
	// still authenticates and serves with a larger sessions table, so an error here
	// is never a reason to take the process down.
	if n := sw.SweepOnce(context.Background()); n != 0 {
		t.Errorf("a failed sweep reported %d deletions", n)
	}

	// And the loop must keep ticking after one, rather than exiting and leaving the
	// table to grow forever.
	q.mu.Lock()
	q.err = nil
	q.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw.Interval = time.Millisecond
	done := make(chan error, 1)
	go func() { done <- sw.Run(ctx) }()

	for i := 0; i < 2; i++ {
		select {
		case <-q.fired:
		case <-time.After(2 * time.Second):
			t.Fatalf("the sweeper stopped ticking after %d sweeps", i)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v; a clean shutdown must not look like a failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestRunStopsOnContextCancellationWithoutLeaking(t *testing.T) {
	q := newFakeQuerier()
	sw := New(q, quietLogger())
	sw.Interval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sw.Run(ctx) }()

	select {
	case <-q.fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the sweeper never swept")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil on cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run outlived its context; the gateway would hang on shutdown")
	}

	// No sweep may start after Run returned — that is what "does not leak a
	// goroutine" means in practice, and a ticker left running would keep issuing
	// DELETEs against a closed pool during shutdown.
	settled := q.count()
	time.Sleep(50 * time.Millisecond)
	if got := q.count(); got != settled {
		t.Errorf("%d sweeps ran after Run returned; the goroutine outlived shutdown", got-settled)
	}
}

func TestRunDoesNotSweepBeforeTheFirstTick(t *testing.T) {
	// time.Ticker does not fire immediately, and the sweeper deliberately does not
	// force a sweep at startup: a rolling deploy of N replicas would otherwise fire
	// N simultaneous DELETEs at the moment the fleet is already busiest.
	q := newFakeQuerier()
	sw := New(q, quietLogger())
	sw.Interval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sw.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if n := q.count(); n != 0 {
		t.Errorf("%d sweeps ran before the first tick elapsed", n)
	}
}
