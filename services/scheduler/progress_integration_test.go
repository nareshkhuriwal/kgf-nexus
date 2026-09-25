//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/google/uuid"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// ReportProgress persistence (CONTRACT-FINDINGS F96), against real Postgres.
//
// The Kubernetes executor's pod status ("pod Pending: Unschedulable: ...") must
// reach the attempt, fenced on the lease, and a chatty or hostile worker must
// not be able to flood run_events, forge lines, or overwrite a newer lease's
// report.

const podPending = "pod Pending: Unschedulable: 0/3 nodes are available: 3 Insufficient cpu"

// progressAttempt is a RUNNING attempt holding a live lease, as a real dispatch
// leaves it, plus the handler under test with a controllable clock.
type progressAttempt struct {
	f       *fixture
	id      uuid.UUID
	worker  uuid.UUID
	token   string
	st      *store.SchedulerStore
	srv     *ReportServer
	clock   time.Time
	nodeID  string
	cleanup func()
}

func newProgressAttempt(t *testing.T, f *fixture, nodeID string, heartbeatMs int64) *progressAttempt {
	t.Helper()
	ctx := context.Background()
	w := f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	id := f.occupy(w, nodeID, vec{CPUMillis: 1000, MemoryBytes: 1 << 30})
	if heartbeatMs > 0 {
		if _, err := f.pool.Exec(ctx, `
UPDATE task_nodes SET spec_json = spec_json || jsonb_build_object('heartbeat_ms', $3::bigint)
 WHERE run_id = $1 AND node_id = $2`, f.runID, nodeID, heartbeatMs); err != nil {
			t.Fatalf("set heartbeat: %v", err)
		}
	}
	token := "tok-" + uuid.NewString()
	if _, err := f.pool.Exec(ctx, `
INSERT INTO leases (attempt_id, worker_id, token, expires_at, epoch)
VALUES ($1, $2, $3, now() + interval '5 minutes', 1)`, id, w, token); err != nil {
		t.Fatalf("insert lease: %v", err)
	}
	st := f.newStore(nil)
	p := &progressAttempt{f: f, id: id, worker: w, token: token, st: st, nodeID: nodeID,
		clock: time.Now().UTC().Truncate(time.Microsecond)}
	p.srv = NewReportServer(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.srv.now = func() time.Time { return p.clock }
	return p
}

func (p *progressAttempt) report(t *testing.T, token, msg string, frac float64) *RecordProgressResponse {
	t.Helper()
	resp, err := p.srv.RecordProgress(context.Background(), &RecordProgressRequest{
		AttemptID: p.id.String(), LeaseToken: token, Message: msg, Fraction: frac,
	})
	if err != nil {
		t.Fatalf("RecordProgress: %v", err)
	}
	return resp
}

type progressRow struct {
	ok          bool
	epoch       int32
	message     *string
	fraction    *float64
	reportedAt  time.Time
	changedAt   *time.Time
	eventCount  int
	suppressed  int
	lastEventAt *time.Time
}

func (p *progressAttempt) row(t *testing.T) progressRow {
	t.Helper()
	var r progressRow
	err := p.f.pool.QueryRow(context.Background(), `
SELECT lease_epoch, message, fraction, reported_at, message_changed_at, event_count,
       events_suppressed, last_event_at
  FROM attempt_progress WHERE attempt_id = $1`, p.id).Scan(
		&r.epoch, &r.message, &r.fraction, &r.reportedAt, &r.changedAt, &r.eventCount,
		&r.suppressed, &r.lastEventAt)
	if err == nil {
		r.ok = true
	}
	return r
}

// progressEvents returns the payloads of the attempt's AttemptProgress events.
func (p *progressAttempt) progressEvents(t *testing.T) []map[string]any {
	t.Helper()
	rows, err := p.f.pool.Query(context.Background(), `
SELECT payload, actor FROM run_events
 WHERE attempt_id = $1 AND kind = 'AttemptProgress' ORDER BY seq`, p.id)
	if err != nil {
		t.Fatalf("list progress events: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var body []byte
		var actor string
		if err := rows.Scan(&body, &actor); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		m := map[string]any{}
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		m["_actor"] = actor
		out = append(out, m)
	}
	return out
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestProgressStoresPodReasonWithoutHeartbeatTimeout is the F96 defect itself:
// the task declares no heartbeat budget (the common case), and the message
// must still be stored, with one event, attributed to the worker.
func TestProgressStoresPodReasonWithoutHeartbeatTimeout(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	p := newProgressAttempt(t, f, "train", 0)

	resp := p.report(t, p.token, podPending, 0)
	if resp.StaleLease || !resp.Stored {
		t.Fatalf("response = %+v, want stored and not stale", resp)
	}
	r := p.row(t)
	if !r.ok || str(r.message) != podPending {
		t.Fatalf("stored message = %q, want %q", str(r.message), podPending)
	}
	if r.fraction != nil {
		t.Errorf("fraction = %v, want NULL: 0 is proto3's unset value, not a report", *r.fraction)
	}
	if r.epoch != 1 || !r.reportedAt.Equal(p.clock) || r.changedAt == nil || !r.changedAt.Equal(p.clock) {
		t.Errorf("row = %+v, want epoch 1 and both times = control-plane now %v", r, p.clock)
	}
	evs := p.progressEvents(t)
	if len(evs) != 1 {
		t.Fatalf("progress events = %d, want 1", len(evs))
	}
	if evs[0]["message"] != podPending || evs[0]["phase"] != "pod Pending" {
		t.Errorf("event payload = %v", evs[0])
	}
	if evs[0]["_actor"] != domain.ActorWorkerPrefix+p.worker.String() {
		t.Errorf("event actor = %v, want the reporting worker: the message is its claim", evs[0]["_actor"])
	}
	if got := f.timersFor(p.id); len(got) != 0 {
		t.Errorf("timers = %v; no heartbeat budget was declared, so none may be armed", got)
	}
}

// TestProgressDeduplicatesAndRateLimitsEvents: the attempt always carries the
// latest message, but the history gets a row only for a real change, and a
// phase flip inside the window is counted, not written.
func TestProgressDeduplicatesAndRateLimitsEvents(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	p := newProgressAttempt(t, f, "train", 0)

	p.report(t, p.token, podPending, 0)
	first := p.clock

	// Same message again, much later: a heartbeat, not news.
	p.clock = p.clock.Add(10 * time.Minute)
	p.report(t, p.token, podPending, 0)
	r := p.row(t)
	if n := len(p.progressEvents(t)); n != 1 {
		t.Fatalf("a duplicate message produced an event (%d events)", n)
	}
	if !r.reportedAt.Equal(p.clock) || !r.changedAt.Equal(first) {
		t.Errorf("reported_at=%v changed_at=%v; want reported_at advanced and changed_at kept at %v",
			r.reportedAt, r.changedAt, first)
	}

	// A phase change 1s after the last EVENT would be inside the window if the
	// last event were recent; make it so.
	p.clock = p.clock.Add(time.Second)
	p.report(t, p.token, "pod Running", 0) // due: last event was 10m ago
	p.clock = p.clock.Add(500 * time.Millisecond)
	p.report(t, p.token, podPending, 0) // phase flip inside 2s: suppressed
	if r := p.row(t); str(r.message) != podPending || r.suppressed != 1 {
		t.Fatalf("after a suppressed flip: message=%q suppressed=%d, want the new message stored and 1 suppressed",
			str(r.message), r.suppressed)
	}
	if n := len(p.progressEvents(t)); n != 2 {
		t.Fatalf("events = %d, want 2 (the flip inside the window must not be written)", n)
	}

	p.clock = p.clock.Add(domain.ProgressPhaseEventInterval)
	p.report(t, p.token, "pod Running", 0)
	evs := p.progressEvents(t)
	if len(evs) != 3 {
		t.Fatalf("events = %d, want 3", len(evs))
	}
	if evs[2]["suppressed_changes"] != float64(1) || evs[2]["previous_phase"] != "pod Pending" {
		t.Errorf("event after a suppression = %v, want suppressed_changes=1 and previous_phase", evs[2])
	}
	if r := p.row(t); r.suppressed != 0 || r.eventCount != 3 {
		t.Errorf("row after event = %+v, want suppressed reset and event_count 3", r)
	}
}

// TestProgressEventsAreCappedPerAttempt: whatever the worker does, the number
// of rows it can add to the append-only history is bounded.
func TestProgressEventsAreCappedPerAttempt(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	p := newProgressAttempt(t, f, "chatty", 0)

	for i := 0; i < domain.MaxProgressEventsPerAttempt+25; i++ {
		p.clock = p.clock.Add(domain.ProgressPhaseEventInterval)
		p.report(t, p.token, "phase-"+uuid.NewString()[:8], 0)
	}
	evs := p.progressEvents(t)
	if len(evs) != domain.MaxProgressEventsPerAttempt {
		t.Fatalf("progress events = %d, want the cap %d", len(evs), domain.MaxProgressEventsPerAttempt)
	}
	if evs[len(evs)-1]["last_recorded"] != true {
		t.Errorf("the last event before the cap must say so: %v", evs[len(evs)-1])
	}
	r := p.row(t)
	if r.eventCount != domain.MaxProgressEventsPerAttempt || r.suppressed != 25 {
		t.Errorf("event_count=%d suppressed=%d, want %d and 25", r.eventCount, r.suppressed, domain.MaxProgressEventsPerAttempt)
	}
	if !strings.HasPrefix(str(r.message), "phase-") {
		t.Errorf("the attempt must still carry the LATEST message past the cap, got %q", str(r.message))
	}
}

// TestProgressStaleLeaseCannotWrite covers every way a token stops being the
// owner's: wrong token, expired lease, reclaimed (deleted) lease, and a row
// already written by a newer lease generation.
func TestProgressStaleLeaseCannotWrite(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	t.Run("wrong token", func(t *testing.T) {
		f := newFixture(t, pool)
		p := newProgressAttempt(t, f, "n", 0)
		p.report(t, p.token, "pod Running", 0)
		resp := p.report(t, "tok-zombie", podPending, 0.9)
		if !resp.StaleLease || resp.Stored {
			t.Fatalf("response = %+v, want stale and not stored", resp)
		}
		if r := p.row(t); str(r.message) != "pod Running" || r.fraction != nil {
			t.Errorf("a zombie overwrote the owner's progress: %+v", r)
		}
	})

	t.Run("expired lease with matching token", func(t *testing.T) {
		f := newFixture(t, pool)
		p := newProgressAttempt(t, f, "n", 0)
		f.expireLease(p.id)
		if resp := p.report(t, p.token, podPending, 0); !resp.StaleLease {
			t.Fatalf("an expired lease was honoured: %+v", resp)
		}
		if r := p.row(t); r.ok {
			t.Errorf("progress stored for an expired lease: %+v", r)
		}
	})

	t.Run("reclaimed lease", func(t *testing.T) {
		f := newFixture(t, pool)
		p := newProgressAttempt(t, f, "n", 0)
		if _, err := pool.Exec(ctx, `DELETE FROM leases WHERE attempt_id = $1`, p.id); err != nil {
			t.Fatal(err)
		}
		if resp := p.report(t, p.token, podPending, 0); !resp.StaleLease {
			t.Fatalf("a report with no lease was honoured: %+v", resp)
		}
		if n := len(p.progressEvents(t)); n != 0 {
			t.Errorf("a stale report wrote %d events", n)
		}
	})

	t.Run("older epoch than stored", func(t *testing.T) {
		f := newFixture(t, pool)
		p := newProgressAttempt(t, f, "n", 0)
		p.report(t, p.token, "pod Running", 0)
		// A newer lease generation has written; this token's lease is epoch 1.
		if _, err := pool.Exec(ctx, `UPDATE attempt_progress SET lease_epoch = 2 WHERE attempt_id = $1`, p.id); err != nil {
			t.Fatal(err)
		}
		if resp := p.report(t, p.token, podPending, 0); !resp.StaleLease {
			t.Fatalf("an older lease epoch overwrote a newer one: %+v", resp)
		}
		if r := p.row(t); str(r.message) != "pod Running" {
			t.Errorf("message = %q, want the newer generation's", str(r.message))
		}
	})
}

// TestProgressNotStoredForTerminalAttempt: a cancelled attempt keeps its lease
// until the worker acknowledges, so the token still matches; the state guard is
// what refuses it.
func TestProgressNotStoredForTerminalAttempt(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	p := newProgressAttempt(t, f, "n", 60_000)
	if _, err := pool.Exec(context.Background(),
		`UPDATE task_attempts SET state = 'CANCELLED', ended_at = now() WHERE id = $1`, p.id); err != nil {
		t.Fatal(err)
	}
	resp := p.report(t, p.token, podPending, 0.5)
	if resp.Stored || resp.StaleLease {
		t.Fatalf("response = %+v, want a silent no-op", resp)
	}
	if r := p.row(t); r.ok {
		t.Errorf("progress stored for a terminal attempt: %+v", r)
	}
	if got := f.timersFor(p.id); len(got) != 0 {
		t.Errorf("a heartbeat was re-armed for a terminal attempt: %v", got)
	}
}

// TestProgressSanitizesAndCapsUntrustedMessage: the message is worker input
// that ends up in a browser and in logs.
func TestProgressSanitizesAndCapsUntrustedMessage(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	p := newProgressAttempt(t, f, "n", 0)

	hostile := "pod Pending\n[fake] ADMIN LOGIN OK\x1b[31m‮" + strings.Repeat("x\x00", 20_000)
	p.report(t, p.token, hostile, 0)
	msg := str(p.row(t).message)
	if len(msg) > domain.MaxProgressMessageBytes {
		t.Fatalf("stored %d bytes, cap is %d", len(msg), domain.MaxProgressMessageBytes)
	}
	for _, r := range msg {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			t.Fatalf("stored message contains control/format rune %U: %q", r, msg)
		}
	}
	if !strings.HasPrefix(msg, "pod Pending [fake] ADMIN LOGIN OK[31m") || !strings.HasSuffix(msg, "…") {
		t.Errorf("stored = %q..., want one line, visibly truncated", msg[:60])
	}
	evs := p.progressEvents(t)
	if len(evs) != 1 || evs[0]["message"] != msg {
		t.Errorf("the event must carry the SANITIZED message, got %v", evs)
	}

	// Control characters only: nothing printable, so a heartbeat, and the
	// previous message stays.
	p.clock = p.clock.Add(time.Hour)
	p.report(t, p.token, "\x00\x07\n", 0)
	if got := str(p.row(t).message); got != msg {
		t.Errorf("an empty-after-sanitizing report erased the message: %q", got)
	}
}

// TestProgressFractionValidation: only a finite value in (0, 1] is stored; a
// bad value is dropped without failing the report or erasing a good one.
func TestProgressFractionValidation(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	p := newProgressAttempt(t, f, "n", 0)
	ctx := context.Background()

	p.report(t, p.token, "", 0.5)
	if r := p.row(t); r.fraction == nil || *r.fraction != 0.5 || r.message != nil {
		t.Fatalf("row = %+v, want fraction 0.5 and no message", r)
	}
	for _, bad := range []float64{1.5, -0.25, math.NaN(), math.Inf(1)} {
		// Through the store directly: NaN and Inf have no JSON encoding, so a
		// real gateway cannot send them over this hop, but the store must still
		// refuse them.
		out, err := p.st.RecordProgress(ctx, store.ProgressReport{
			AttemptID: p.id, LeaseToken: p.token, Fraction: bad,
		}, p.clock)
		if err != nil {
			t.Fatalf("fraction %v failed the report: %v", bad, err)
		}
		if !out.FractionRejected {
			t.Errorf("fraction %v was not reported as rejected", bad)
		}
		if r := p.row(t); r.fraction == nil || *r.fraction != 0.5 {
			t.Errorf("fraction %v overwrote the valid 0.5: %+v", bad, r.fraction)
		}
	}
	p.report(t, p.token, "pod Running", 0)
	if r := p.row(t); r.fraction == nil || *r.fraction != 0.5 {
		t.Errorf("a phase message (fraction 0 = unset) erased the fraction: %v", r.fraction)
	}
	p.report(t, p.token, "", 1)
	if r := p.row(t); r.fraction == nil || *r.fraction != 1 {
		t.Errorf("fraction 1 not stored: %v", r.fraction)
	}
}

// TestProgressRearmsHeartbeat keeps the pre-existing behaviour: a RUNNING
// attempt with a heartbeat budget gets its deadline pushed out.
func TestProgressRearmsHeartbeat(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	p := newProgressAttempt(t, f, "n", 60_000)

	p.report(t, p.token, "pod Running", 0)
	var fireAt time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT fire_at FROM timers WHERE subject_id = $1 AND kind = 'heartbeat'`, p.id).Scan(&fireAt); err != nil {
		t.Fatalf("heartbeat timer not armed: %v", err)
	}
	if want := p.clock.Add(time.Minute); !fireAt.Equal(want) {
		t.Errorf("heartbeat fires at %v, want %v", fireAt, want)
	}
}

// TestProgressWithoutTokenIsHeartbeatOnly is the rolling-deploy case: a
// gateway that predates lease_token forwarding. The heartbeat still counts
// (the gateway fenced it), but nothing is stored, because nothing here can
// prove the message is current.
func TestProgressWithoutTokenIsHeartbeatOnly(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	p := newProgressAttempt(t, f, "n", 60_000)

	resp := p.report(t, "", podPending, 0.3)
	if resp.StaleLease || resp.Stored {
		t.Fatalf("response = %+v, want heartbeat-only", resp)
	}
	if r := p.row(t); r.ok {
		t.Errorf("an unfenced report was stored: %+v", r)
	}
	if got := f.timersFor(p.id); len(got) != 1 || got[0] != domain.TimerHeartbeat {
		t.Errorf("timers = %v, want the heartbeat re-armed as before", got)
	}
}

// TestProgressNeverBlocksTheResult is why progress lives in a side table: the
// terminal transition waits for the attempt row only up to a bounded lock
// timeout (F104). If progress held that row, a result arriving alongside a
// burst of progress reports would be delayed and could be bounced back to the
// agent to resend. Before F104 it was skipped and lost outright.
func TestProgressNeverBlocksTheResult(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	p := newProgressAttempt(t, f, "n", 60_000)
	ctx := context.Background()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = p.st.RecordProgress(ctx, store.ProgressReport{
					AttemptID: p.id, LeaseToken: p.token,
					Message: "pod Running: step " + uuid.NewString()[:6],
				}, time.Now().UTC())
			}
		}(g)
	}
	time.Sleep(50 * time.Millisecond)
	applied, err := p.st.ApplyResult(ctx, time.Now().UTC(), &nexusv1.ReportResultRequest{
		AttemptId: p.id.String(), LeaseToken: p.token,
		Outcome: nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
	})
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("ApplyResult: %v", err)
	}
	if !applied.Applied {
		t.Fatal("the result was SKIPPED while progress reports were in flight; it would be lost and the attempt would expire as LOST")
	}
	if s := f.attemptState(p.id); s != "SUCCEEDED" {
		t.Fatalf("attempt state = %s, want SUCCEEDED", s)
	}
	// And once the lease is gone, progress is refused.
	out, err := p.st.RecordProgress(ctx, store.ProgressReport{
		AttemptID: p.id, LeaseToken: p.token, Message: "pod Succeeded",
	}, time.Now().UTC())
	if err != nil || !out.StaleLease {
		t.Fatalf("progress after the result = %+v, %v; want stale (the lease ended with the attempt)", out, err)
	}
}
