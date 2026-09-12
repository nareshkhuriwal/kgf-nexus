// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package wal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeReporter records every report and can be told to fail or reject.
type fakeReporter struct {
	mu   sync.Mutex
	got  []*nexusv1.ReportResultRequest
	err  error
	deny string
}

func (f *fakeReporter) ReportResult(_ context.Context, req *nexusv1.ReportResultRequest) (*nexusv1.Ack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, req)
	if f.err != nil {
		return nil, f.err
	}
	if f.deny != "" {
		return &nexusv1.Ack{Accepted: false, Reason: f.deny}, nil
	}
	return &nexusv1.Ack{Accepted: true}, nil
}

func (f *fakeReporter) calls() []*nexusv1.ReportResultRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*nexusv1.ReportResultRequest(nil), f.got...)
}

func open(t *testing.T) *WAL {
	t.Helper()
	w, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return w
}

func completedEntry(t *testing.T, attemptID string, outcome nexusv1.AttemptOutcome) Entry {
	t.Helper()
	payload, err := MarshalResult(&nexusv1.ReportResultRequest{
		AttemptId:  attemptID,
		LeaseToken: "tok-" + attemptID,
		Outcome:    outcome,
		Outputs: map[string]*nexusv1.ResultRef{
			"result": {Kind: &nexusv1.ResultRef_Inline{Inline: []byte(`{"rows":42}`)},
				ContentType: "application/json", SizeBytes: 11},
		},
		Metrics:       &nexusv1.ExecutionMetrics{ExitCode: 0, WallMs: 1234, PeakMemoryBytes: 999},
		EndedAtUnixMs: 1700000000000,
	})
	if err != nil {
		t.Fatalf("MarshalResult: %v", err)
	}
	return Entry{
		AttemptID: attemptID, LeaseToken: "tok-" + attemptID,
		Outcome: OutcomeSucceeded, ResultJSON: payload, EndedAtUnixMs: 1700000000000,
	}
}

// TestReplayCompletedIsReReported is the core WAL guarantee: an agent that died
// after finishing work but before the report landed must RE-REPORT the real
// outcome, not re-execute the task.
func TestReplayCompletedIsReReported(t *testing.T) {
	w := open(t)
	e := completedEntry(t, "attempt-completed", nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED)
	if err := w.RecordCompleted(e); err != nil {
		t.Fatalf("RecordCompleted: %v", err)
	}

	// Simulate a restart: a brand new WAL handle over the same directory, which
	// is exactly what a fresh process sees.
	w2, err := Open(w.Dir())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	fr := &fakeReporter{}
	res := Replay(context.Background(), w2, fr, testLog())

	if len(res.Reported) != 1 || res.Reported[0] != "attempt-completed" {
		t.Fatalf("expected the completed attempt to be re-reported, got %+v", res)
	}
	if len(res.Indeterminate) != 0 {
		t.Fatalf("a COMPLETED entry must never be reported INDETERMINATE: %+v", res.Indeterminate)
	}
	calls := fr.calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one report, got %d", len(calls))
	}
	got := calls[0]
	if got.GetOutcome() != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		t.Errorf("outcome = %v, want SUCCEEDED (the observed outcome must survive verbatim)", got.GetOutcome())
	}
	if got.GetLeaseToken() != "tok-attempt-completed" {
		t.Errorf("lease token = %q, want the token recorded at completion", got.GetLeaseToken())
	}
	// The outputs and metrics must survive the round trip, or the control plane
	// finalises a SUCCEEDED attempt with no result for downstream nodes to read.
	if ref := got.GetOutputs()["result"]; ref == nil {
		t.Error("output ref was lost in replay")
	} else if string(ref.GetInline()) != `{"rows":42}` {
		t.Errorf("inline output = %q, want the original payload", ref.GetInline())
	}
	if got.GetMetrics().GetWallMs() != 1234 {
		t.Errorf("metrics lost: wall_ms = %d, want 1234", got.GetMetrics().GetWallMs())
	}

	// A durably acked entry must be DELETED, or every boot re-reports forever.
	if entries, _ := w2.List(); len(entries) != 0 {
		t.Errorf("acked entry was not deleted; %d entries remain", len(entries))
	}
}

// TestReplayStartingIsIndeterminate is the honesty requirement: when the agent
// knows it started a task but cannot know the outcome, it must say so. Claiming
// SUCCEEDED would be a lie; claiming FAILED would be a more dangerous lie, since
// a FAILED attempt gets retried under a policy that assumes nothing happened.
func TestReplayStartingIsIndeterminate(t *testing.T) {
	w := open(t)
	if err := w.RecordStarting(Entry{
		AttemptID: "attempt-starting", LeaseToken: "tok-s", RunID: "run-1", NodeID: "node-a",
	}); err != nil {
		t.Fatalf("RecordStarting: %v", err)
	}

	fr := &fakeReporter{}
	res := Replay(context.Background(), w, fr, testLog())

	if len(res.Indeterminate) != 1 || res.Indeterminate[0] != "attempt-starting" {
		t.Fatalf("expected the STARTING entry to be reported INDETERMINATE, got %+v", res)
	}
	calls := fr.calls()
	if len(calls) != 1 {
		t.Fatalf("expected one report, got %d", len(calls))
	}
	if got := calls[0].GetOutcome(); got != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_INDETERMINATE {
		t.Errorf("outcome = %v, want INDETERMINATE", got)
	}
	// WORKER_LOST is the class the control plane's §6.5 policy keys on for
	// ambiguous outcomes. Any other class would route this through the wrong
	// retry decision.
	if got := calls[0].GetError().GetClass(); got != nexusv1.ErrorClass_ERROR_CLASS_WORKER_LOST {
		t.Errorf("error class = %v, want ERROR_CLASS_WORKER_LOST", got)
	}
	if entries, _ := w.List(); len(entries) != 0 {
		t.Errorf("acked entry was not deleted; %d remain", len(entries))
	}
}

// TestReplayMixedBatch checks that both kinds drain in one pass and neither
// contaminates the other's classification.
func TestReplayMixedBatch(t *testing.T) {
	w := open(t)
	if err := w.RecordCompleted(completedEntry(t, "a-done", nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED)); err != nil {
		t.Fatal(err)
	}
	if err := w.RecordStarting(Entry{AttemptID: "b-unknown", LeaseToken: "tok-b"}); err != nil {
		t.Fatal(err)
	}
	if err := w.RecordCompleted(completedEntry(t, "c-done", nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED)); err != nil {
		t.Fatal(err)
	}

	fr := &fakeReporter{}
	res := Replay(context.Background(), w, fr, testLog())

	if len(res.Reported) != 3 {
		t.Fatalf("expected all three entries reported, got %+v", res.Reported)
	}
	if len(res.Indeterminate) != 1 || res.Indeterminate[0] != "b-unknown" {
		t.Errorf("only the STARTING entry may be indeterminate, got %+v", res.Indeterminate)
	}
	if entries, _ := w.List(); len(entries) != 0 {
		t.Errorf("WAL not drained; %d entries remain", len(entries))
	}
}

// TestReplayRetainsOnTransportFailure is the durability promise: an undeliverable
// outcome must SURVIVE on disk rather than be dropped, because the whole point is
// that the outcome outlives our inability to deliver it.
func TestReplayRetainsOnTransportFailure(t *testing.T) {
	w := open(t)
	if err := w.RecordCompleted(completedEntry(t, "a-stuck", nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED)); err != nil {
		t.Fatal(err)
	}

	fr := &fakeReporter{err: errors.New("connection refused")}
	res := Replay(context.Background(), w, fr, testLog())

	if len(res.Retained) != 1 {
		t.Fatalf("expected the entry to be retained, got %+v", res)
	}
	if len(res.Reported) != 0 {
		t.Fatalf("nothing was acked, so nothing may be reported: %+v", res.Reported)
	}
	entries, _ := w.List()
	if len(entries) != 1 {
		t.Fatalf("entry was lost despite a failed report; %d remain", len(entries))
	}
	if entries[0].ReplayCount != 1 {
		t.Errorf("replay count = %d, want 1 so an undrainable entry is visible to an operator",
			entries[0].ReplayCount)
	}

	// A later pass against a healthy control plane must deliver it.
	fr2 := &fakeReporter{}
	res2 := Replay(context.Background(), w, fr2, testLog())
	if len(res2.Reported) != 1 {
		t.Fatalf("retained entry did not deliver on the next pass: %+v", res2)
	}
	if entries, _ := w.List(); len(entries) != 0 {
		t.Errorf("delivered entry not deleted; %d remain", len(entries))
	}
}

// TestReplayDropsRejected: a stale lease token is a permanent refusal. Keeping the
// entry would grow the WAL forever against a server that will never accept it.
func TestReplayDropsRejected(t *testing.T) {
	w := open(t)
	if err := w.RecordCompleted(completedEntry(t, "a-fenced", nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED)); err != nil {
		t.Fatal(err)
	}
	fr := &fakeReporter{deny: "stale_lease_token"}
	res := Replay(context.Background(), w, fr, testLog())

	if len(res.Rejected) != 1 {
		t.Fatalf("expected a rejection, got %+v", res)
	}
	if entries, _ := w.List(); len(entries) != 0 {
		t.Errorf("a permanently rejected entry must be dropped; %d remain", len(entries))
	}
}

// TestReplaySkipsCorruptEntry: a torn write during a power loss is an expected
// event. One corrupt file must not block replay of every healthy one, and it must
// be left in place for inspection rather than deleted.
func TestReplaySkipsCorruptEntry(t *testing.T) {
	w := open(t)
	if err := w.RecordCompleted(completedEntry(t, "healthy", nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED)); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(w.Dir(), "torn.json")
	if err := os.WriteFile(corrupt, []byte(`{"attempt_id":"torn","state":"COMPL`), 0o600); err != nil {
		t.Fatal(err)
	}

	fr := &fakeReporter{}
	res := Replay(context.Background(), w, fr, testLog())

	if len(res.Reported) != 1 || res.Reported[0] != "healthy" {
		t.Fatalf("the healthy entry must still replay, got %+v", res.Reported)
	}
	if len(res.Corrupt) == 0 {
		t.Error("corrupt entry was not surfaced to the caller")
	}
	if _, err := os.Stat(corrupt); err != nil {
		t.Errorf("corrupt file must be left in place as evidence: %v", err)
	}
}

// TestPutIsAtomic verifies the temp-file-then-rename guarantee: no reader ever
// sees a half-written entry, and no temp files leak into the replay set.
func TestPutIsAtomic(t *testing.T) {
	w := open(t)
	for i := 0; i < 20; i++ {
		if err := w.RecordStarting(Entry{AttemptID: "a", LeaseToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	names, err := os.ReadDir(w.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 {
		var got []string
		for _, n := range names {
			got = append(got, n.Name())
		}
		t.Errorf("expected exactly one entry file, got %v (temp files must not leak)", got)
	}
	entries, errs := w.List()
	if len(errs) != 0 {
		t.Errorf("unexpected list errors: %v", errs)
	}
	if len(entries) != 1 {
		t.Errorf("expected one entry, got %d", len(entries))
	}
}

// TestStateTransitionOverwrites: RecordCompleted over a STARTING entry must
// REPLACE it, so replay sees the known outcome rather than reporting
// INDETERMINATE for work that actually finished.
func TestStateTransitionOverwrites(t *testing.T) {
	w := open(t)
	if err := w.RecordStarting(Entry{AttemptID: "x", LeaseToken: "t1"}); err != nil {
		t.Fatal(err)
	}
	if err := w.RecordCompleted(completedEntry(t, "x", nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED)); err != nil {
		t.Fatal(err)
	}
	e, ok, err := w.Get("x")
	if err != nil || !ok {
		t.Fatalf("Get: %v (found=%v)", err, ok)
	}
	if e.State != StateCompleted {
		t.Fatalf("state = %q, want COMPLETED", e.State)
	}

	fr := &fakeReporter{}
	res := Replay(context.Background(), w, fr, testLog())
	if len(res.Indeterminate) != 0 {
		t.Error("a completed attempt must never replay as INDETERMINATE")
	}
	if got := fr.calls()[0].GetOutcome(); got != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		t.Errorf("outcome = %v, want SUCCEEDED", got)
	}
}

// TestDeleteIsIdempotent: the ack path can be retried, so deleting an absent
// entry must succeed.
func TestDeleteIsIdempotent(t *testing.T) {
	w := open(t)
	if err := w.RecordStarting(Entry{AttemptID: "gone", LeaseToken: "t"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := w.Delete("gone"); err != nil {
			t.Fatalf("delete #%d: %v", i+1, err)
		}
	}
}

// TestSanitizeBlocksPathEscape: an attempt id must never be able to write outside
// the WAL directory.
func TestSanitizeBlocksPathEscape(t *testing.T) {
	w := open(t)
	if err := w.RecordStarting(Entry{AttemptID: "../../escape", LeaseToken: "t"}); err != nil {
		t.Fatalf("RecordStarting: %v", err)
	}
	outside := filepath.Join(filepath.Dir(w.Dir()), "escape.json")
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("a hostile attempt_id escaped the WAL directory")
	}
	entries, _ := w.List()
	if len(entries) != 1 {
		t.Fatalf("expected the entry to be written inside the WAL dir, got %d", len(entries))
	}
}

// TestConcurrentWritesAndDeletes exercises the lock: executor goroutines write
// their own attempts while the reporter deletes acked ones.
func TestConcurrentWritesAndDeletes(t *testing.T) {
	w := open(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i))
			if err := w.RecordStarting(Entry{AttemptID: id, LeaseToken: "t"}); err != nil {
				t.Errorf("RecordStarting(%s): %v", id, err)
				return
			}
			if err := w.RecordCompleted(completedEntry(t, id, nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED)); err != nil {
				t.Errorf("RecordCompleted(%s): %v", id, err)
				return
			}
			if err := w.Delete(id); err != nil {
				t.Errorf("Delete(%s): %v", id, err)
			}
		}(i)
	}
	wg.Wait()
	entries, errs := w.List()
	if len(errs) != 0 {
		t.Errorf("list errors: %v", errs)
	}
	if len(entries) != 0 {
		t.Errorf("expected an empty WAL, got %d entries", len(entries))
	}
}

// TestCompletedEntryRequiresOutcome guards the invariant that a COMPLETED record
// without an outcome is meaningless and must be refused rather than written.
func TestCompletedEntryRequiresOutcome(t *testing.T) {
	w := open(t)
	if err := w.RecordCompleted(Entry{AttemptID: "x", ResultJSON: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("expected an error for a COMPLETED entry with no outcome")
	}
}
