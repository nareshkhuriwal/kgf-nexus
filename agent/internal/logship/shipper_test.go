// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package logship

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/logformat"
)

// ── fakes ────────────────────────────────────────────────────────────────

type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	// fail returns an error for a key when non-nil.
	fail func(key string) error
	// gate, when non-nil, blocks every PUT until closed.
	gate chan struct{}
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

func (m *memStore) PutLogObject(ctx context.Context, key string, body io.Reader, size int64) error {
	if m.gate != nil {
		select {
		case <-m.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if m.fail != nil {
		if err := m.fail(key); err != nil {
			return err
		}
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return fmt.Errorf("size mismatch: %d != %d", len(b), size)
	}
	m.mu.Lock()
	m.objects[key] = b
	m.mu.Unlock()
	return nil
}

func (m *memStore) get(key string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.objects[key]
}

type fakeIndex struct {
	mu     sync.Mutex
	reqs   []*nexusv1.ReportLogsRequest
	chunks map[int64]*nexusv1.LogChunk
	final  *nexusv1.LogFinal
	// respond overrides the default accept.
	respond func(req *nexusv1.ReportLogsRequest) (*nexusv1.Ack, error)
}

func newFakeIndex() *fakeIndex { return &fakeIndex{chunks: map[int64]*nexusv1.LogChunk{}} }

func (f *fakeIndex) ReportLogs(ctx context.Context, req *nexusv1.ReportLogsRequest) (*nexusv1.Ack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.respond != nil {
		ack, err := f.respond(req)
		if err != nil || !ack.GetAccepted() {
			return ack, err
		}
	}
	for _, c := range req.GetChunks() {
		f.chunks[c.GetSeq()] = c
	}
	if req.GetFinal() != nil {
		f.final = req.GetFinal()
	}
	return &nexusv1.Ack{Accepted: true}, nil
}

func (f *fakeIndex) indexed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.chunks)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testConfig(t *testing.T) Config {
	return Config{
		Namespace: "ns", AttemptID: "att-1", SpoolDir: t.TempDir(),
		Token: func() string { return "tok" }, Log: quiet(),
		RetryBase: time.Millisecond, RetryMax: 5 * time.Millisecond,
		FlushInterval: time.Hour, FinalizeTimeout: 5 * time.Second,
	}
}

// allRecords reads every indexed chunk from the store in seq order.
func allRecords(t *testing.T, st *memStore, idx *fakeIndex) []logformat.Record {
	t.Helper()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var out []logformat.Record
	for seq := int64(0); seq < int64(len(idx.chunks)); seq++ {
		c, ok := idx.chunks[seq]
		if !ok {
			t.Fatalf("chunk %d missing from the index", seq)
		}
		recs, bad := logformat.DecodeRecords(st.get(c.GetKey()))
		if bad != 0 {
			t.Fatalf("chunk %d has %d undecodable lines", seq, bad)
		}
		if int64(len(recs)) != c.GetLineCount() || (len(recs) > 0 && recs[0].N != c.GetFirstLine()) {
			t.Fatalf("chunk %d metadata (first=%d count=%d) disagrees with its content (%d records)",
				seq, c.GetFirstLine(), c.GetLineCount(), len(recs))
		}
		out = append(out, recs...)
	}
	return out
}

func assertContiguous(t *testing.T, recs []logformat.Record) {
	t.Helper()
	for i, r := range recs {
		if r.N != int64(i+1) {
			t.Fatalf("record %d has n=%d; line numbers must be contiguous from 1", i, r.N)
		}
	}
}

// ── tests ────────────────────────────────────────────────────────────────

func TestLinesAreNumberedContiguouslyAndTaggedByStream(t *testing.T) {
	st, idx := newMemStore(), newFakeIndex()
	s := New(testConfig(t), st, idx)
	out, errw := s.Writer(logformat.StreamStdout), s.Writer(logformat.StreamStderr)
	fmt.Fprint(out, "one\ntw")
	fmt.Fprint(errw, "oops\r\n")
	fmt.Fprint(out, "o\n")
	stats := s.Close(context.Background())

	recs := allRecords(t, st, idx)
	assertContiguous(t, recs)
	want := []struct{ s, m string }{{"stdout", "one"}, {"stderr", "oops"}, {"stdout", "two"}}
	if len(recs) != len(want) {
		t.Fatalf("got %d records, want %d: %+v", len(recs), len(want), recs)
	}
	for i, w := range want {
		if recs[i].S != w.s || recs[i].M != w.m {
			t.Errorf("record %d = %s/%q, want %s/%q", i, recs[i].S, recs[i].M, w.s, w.m)
		}
	}
	if !stats.Finalized || idx.final.GetTotalLines() != 3 {
		t.Fatalf("final = %+v stats = %+v, want finalized with 3 lines", idx.final, stats)
	}
	for _, r := range idx.reqs {
		if r.GetLeaseToken() != "tok" || r.GetAttemptId() != "att-1" {
			t.Fatalf("report carried attempt=%q token=%q; every report must be fenced by the lease", r.GetAttemptId(), r.GetLeaseToken())
		}
	}
}

func TestKeysStayUnderTheAttemptPrefix(t *testing.T) {
	st, idx := newMemStore(), newFakeIndex()
	cfg := testConfig(t)
	cfg.ChunkBytes = 64
	s := New(cfg, st, idx)
	for i := 0; i < 20; i++ {
		fmt.Fprintf(s.Writer("stdout"), "line %d\n", i)
	}
	s.Close(context.Background())
	for k := range st.objects {
		if !strings.HasPrefix(k, "ns/logs/att-1/") {
			t.Fatalf("object %q is outside the attempt prefix; the gateway would refuse to index it", k)
		}
	}
}

// TestChunksCompactIntoOneObjectWhoseOffsetsTheIndexKeeps: after compaction the
// chunk rows' byte offsets must address the same bytes inside the compacted
// object, which is what lets the reader switch to range requests.
func TestChunksCompactIntoOneObjectWhoseOffsetsTheIndexKeeps(t *testing.T) {
	st, idx := newMemStore(), newFakeIndex()
	cfg := testConfig(t)
	cfg.ChunkBytes = 200
	s := New(cfg, st, idx)
	for i := 0; i < 50; i++ {
		fmt.Fprintf(s.Writer("stdout"), "the quick brown fox %03d\n", i)
	}
	stats := s.Close(context.Background())
	if !stats.Compacted || stats.Chunks < 3 {
		t.Fatalf("stats = %+v, want several chunks and a compacted object", stats)
	}
	compacted := st.get(idx.final.GetCompactedKey())
	if compacted == nil {
		t.Fatalf("compacted object %q was not written", idx.final.GetCompactedKey())
	}
	sum := sha256.Sum256(compacted)
	if hex.EncodeToString(sum[:]) != idx.final.GetCompactedSha256() {
		t.Fatal("compacted sha256 does not match its content")
	}
	for seq, c := range idx.chunks {
		part := compacted[c.GetByteOffset() : c.GetByteOffset()+c.GetSizeBytes()]
		if !bytes.Equal(part, st.get(c.GetKey())) {
			t.Fatalf("chunk %d: the compacted range differs from the chunk object", seq)
		}
	}
	if int64(len(compacted)) != idx.final.GetTotalBytes() {
		t.Fatalf("compacted size %d != total_bytes %d", len(compacted), idx.final.GetTotalBytes())
	}
	assertContiguous(t, allRecords(t, st, idx))
}

func TestFlushIntervalShipsWhileTheTaskRuns(t *testing.T) {
	st, idx := newMemStore(), newFakeIndex()
	cfg := testConfig(t)
	cfg.FlushInterval = 10 * time.Millisecond
	s := New(cfg, st, idx)
	defer s.Close(context.Background())
	fmt.Fprint(s.Writer("stdout"), "still running\n")

	deadline := time.Now().Add(2 * time.Second)
	for idx.indexed() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a line written by a running task was not indexed within the flush interval; live tail cannot work")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPartialAndOverlongLines(t *testing.T) {
	st, idx := newMemStore(), newFakeIndex()
	cfg := testConfig(t)
	cfg.MaxLineBytes = 10
	s := New(cfg, st, idx)
	fmt.Fprint(s.Writer("stdout"), "abcdefghijklmnopqrstuvwxyz\nno newline at end")
	s.Close(context.Background())

	recs := allRecords(t, st, idx)
	assertContiguous(t, recs)
	var joined strings.Builder
	for _, r := range recs[:3] {
		joined.WriteString(r.M)
	}
	if joined.String() != "abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("split line reassembles to %q", joined.String())
	}
	if !recs[0].P || !recs[1].P || recs[2].P {
		t.Fatalf("partial flags = %v %v %v; only the pieces before the newline are partial", recs[0].P, recs[1].P, recs[2].P)
	}
	last := recs[len(recs)-1]
	if !last.P || !strings.HasSuffix(last.M, "at end") {
		t.Fatalf("output without a trailing newline must still ship, marked partial: %+v", last)
	}
}

func TestSplitNeverCutsAMultiByteCharacter(t *testing.T) {
	st, idx := newMemStore(), newFakeIndex()
	cfg := testConfig(t)
	cfg.MaxLineBytes = 5
	s := New(cfg, st, idx)
	fmt.Fprint(s.Writer("stdout"), "ééééééé\n") // 2 bytes each
	s.Close(context.Background())
	for _, r := range allRecords(t, st, idx) {
		if strings.ContainsRune(r.M, '�') {
			t.Fatalf("a split produced an invalid rune: %q", r.M)
		}
	}
}

// TestCapKeepsHeadAndTailAndSaysWhatItOmitted is ADR-0020's truncation rule.
func TestCapKeepsHeadAndTailAndSaysWhatItOmitted(t *testing.T) {
	st, idx := newMemStore(), newFakeIndex()
	cfg := testConfig(t)
	cfg.MaxBytes = 4 << 10
	cfg.TailBytes = 1 << 10
	s := New(cfg, st, idx)
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(s.Writer("stdout"), "line %04d\n", i)
	}
	stats := s.Close(context.Background())
	recs := allRecords(t, st, idx)
	assertContiguous(t, recs)

	if recs[0].M != "line 0000" {
		t.Fatalf("the head was not kept: first record %q", recs[0].M)
	}
	if recs[len(recs)-1].M != "line 0999" {
		t.Fatalf("the tail was not kept: last record %q", recs[len(recs)-1].M)
	}
	var sawMarker, sawSummary bool
	for _, r := range recs {
		if r.S == logformat.StreamNexus && r.K == logformat.KindTruncated {
			sawMarker = true
		}
		if r.S == logformat.StreamNexus && r.K == logformat.KindTruncationSummary {
			sawSummary = true
			if !strings.Contains(r.M, fmt.Sprint(stats.TruncatedLines)) {
				t.Errorf("summary %q does not state the omitted line count %d", r.M, stats.TruncatedLines)
			}
		}
	}
	if !sawMarker || !sawSummary {
		t.Fatalf("truncation must be stated, not implied (marker=%v summary=%v)", sawMarker, sawSummary)
	}
	if stats.TruncatedLines == 0 || idx.final.GetTruncatedLines() != stats.TruncatedLines {
		t.Fatalf("truncated lines stats=%d final=%d", stats.TruncatedLines, idx.final.GetTruncatedLines())
	}
	kept := 0
	for _, r := range recs {
		if r.S == "stdout" {
			kept++
		}
	}
	if int64(kept)+stats.TruncatedLines != 1000 {
		t.Fatalf("kept %d + omitted %d != 1000 written; lines were lost without being counted", kept, stats.TruncatedLines)
	}
	if stats.Bytes > cfg.MaxBytes+cfg.TailBytes+2<<10 {
		t.Fatalf("shipped %d bytes, far beyond cap %d + tail %d", stats.Bytes, cfg.MaxBytes, cfg.TailBytes)
	}
}

// TestWritesNeverBlockOnAStalledStore: the writer is the workload's stdout. A
// store that hangs must cost log lines (counted, marked), never task progress.
func TestWritesNeverBlockOnAStalledStore(t *testing.T) {
	st, idx := newMemStore(), newFakeIndex()
	st.gate = make(chan struct{})
	cfg := testConfig(t)
	cfg.ChunkBytes = 256
	cfg.MaxPendingBytes = 2 << 10
	s := New(cfg, st, idx)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ {
			fmt.Fprintf(s.Writer("stdout"), "line %d with some padding to make it longer\n", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writes blocked on a stalled object store; the workload would be wedged by its own log")
	}
	close(st.gate)
	stats := s.Close(context.Background())
	if stats.DroppedLines == 0 || idx.final.GetDroppedLines() != stats.DroppedLines {
		t.Fatalf("dropped stats=%d final=%d; a bounded buffer must drop and count", stats.DroppedLines, idx.final.GetDroppedLines())
	}
	recs := allRecords(t, st, idx)
	assertContiguous(t, recs)
	var marker bool
	for _, r := range recs {
		if r.K == logformat.KindDropped {
			marker = true
		}
	}
	if !marker {
		t.Fatal("dropped lines were not marked in the log itself")
	}
}

func TestStaleLeaseStopsShipping(t *testing.T) {
	st, idx := newMemStore(), newFakeIndex()
	idx.respond = func(*nexusv1.ReportLogsRequest) (*nexusv1.Ack, error) {
		return &nexusv1.Ack{Accepted: false, Reason: "stale_lease_token"}, nil
	}
	cfg := testConfig(t)
	cfg.ChunkBytes = 64
	s := New(cfg, st, idx)
	for i := 0; i < 50; i++ {
		fmt.Fprintf(s.Writer("stdout"), "line %d\n", i)
	}
	stats := s.Close(context.Background())
	if !stats.Fenced || stats.Finalized {
		t.Fatalf("stats = %+v, want fenced and not finalized", stats)
	}
	idx.mu.Lock()
	n := len(idx.reqs)
	idx.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d ReportLogs calls after a stale-lease rejection; the first rejection must stop shipping", n)
	}
}

func TestOldGatewayDisablesShipping(t *testing.T) {
	st, idx := newMemStore(), newFakeIndex()
	idx.respond = func(*nexusv1.ReportLogsRequest) (*nexusv1.Ack, error) {
		return nil, fmt.Errorf("ReportLogs: %w", status.Error(codes.Unimplemented, "unknown method"))
	}
	s := New(testConfig(t), st, idx)
	fmt.Fprint(s.Writer("stdout"), "x\n")
	if stats := s.Close(context.Background()); !stats.Fenced {
		t.Fatalf("UNIMPLEMENTED must disable shipping, stats=%+v", stats)
	}
}

// TestRetriedReportIsIdenticalSoTheServerCanDeduplicate: a lost ack makes the
// agent resend, and the gateway's idempotency is by (attempt, seq) -- so the
// resend must carry the same seq and metadata.
func TestRetriedReportIsIdenticalSoTheServerCanDeduplicate(t *testing.T) {
	st, idx := newMemStore(), newFakeIndex()
	calls := 0
	idx.respond = func(*nexusv1.ReportLogsRequest) (*nexusv1.Ack, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("connection reset")
		}
		return &nexusv1.Ack{Accepted: true}, nil
	}
	s := New(testConfig(t), st, idx)
	fmt.Fprint(s.Writer("stdout"), "hello\n")
	s.Close(context.Background())
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if len(idx.reqs) < 2 {
		t.Fatalf("expected a retry, got %d calls", len(idx.reqs))
	}
	a, b := idx.reqs[0].GetChunks(), idx.reqs[1].GetChunks()
	if len(a) != 1 || len(b) != 1 || a[0].GetSeq() != b[0].GetSeq() || a[0].GetSha256() != b[0].GetSha256() {
		t.Fatalf("retry changed the chunk: %v vs %v", a, b)
	}
}

func TestSecretsAreRedactedBeforeUpload(t *testing.T) {
	st, idx := newMemStore(), newFakeIndex()
	cfg := testConfig(t)
	cfg.Redactor = NewRedactor(map[string]string{"db_password": "hunter2-very-secret", "x": "ab"})
	s := New(cfg, st, idx)
	fmt.Fprint(s.Writer("stderr"), "connecting with hunter2-very-secret now\n")
	stats := s.Close(context.Background())
	for k, v := range st.objects {
		if bytes.Contains(v, []byte("hunter2")) {
			t.Fatalf("object %s contains the secret value", k)
		}
	}
	recs := allRecords(t, st, idx)
	if recs[0].M != "connecting with [REDACTED:db_password] now" || stats.Redactions != 1 {
		t.Fatalf("record = %q redactions=%d", recs[0].M, stats.Redactions)
	}
	if got := cfg.Redactor.Skipped(); len(got) != 1 || got[0] != "x" {
		t.Fatalf("too-short secret not reported as skipped: %v", got)
	}
}

func TestFinalizeIsBoundedWhenTheStoreIsDown(t *testing.T) {
	st, idx := newMemStore(), newFakeIndex()
	st.fail = func(string) error { return errors.New("store down") }
	cfg := testConfig(t)
	cfg.FinalizeTimeout = 200 * time.Millisecond
	s := New(cfg, st, idx)
	fmt.Fprint(s.Writer("stdout"), "x\n")
	start := time.Now()
	stats := s.Close(context.Background())
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Close took %s with a %s budget; the result report would be held hostage", el, cfg.FinalizeTimeout)
	}
	if stats.Finalized {
		t.Fatal("a log whose chunks never uploaded must not be reported finalized")
	}
}

// TestCompactionCoversAChunkWhoseUploadFailed: the spool holds every sealed
// byte, so a chunk whose own PUT keeps failing is still readable through the
// compacted object once the final report indexes it.
func TestCompactionCoversAChunkWhoseUploadFailed(t *testing.T) {
	st, idx := newMemStore(), newFakeIndex()
	st.gate = make(chan struct{}) // hold uploads until Close, so the failure happens while closing
	st.fail = func(key string) error {
		if strings.HasSuffix(key, "/00000001.ndjson") {
			return errors.New("permanently broken key")
		}
		return nil
	}
	cfg := testConfig(t)
	cfg.ChunkBytes = 64
	s := New(cfg, st, idx)
	for i := 0; i < 20; i++ {
		fmt.Fprintf(s.Writer("stdout"), "line %d\n", i)
	}
	go func() { time.Sleep(50 * time.Millisecond); close(st.gate) }()
	stats := s.Close(context.Background())
	if !stats.Finalized || !stats.Compacted {
		t.Fatalf("stats = %+v, want finalized and compacted", stats)
	}
	if st.get("ns/logs/att-1/00000001.ndjson") != nil {
		t.Fatal("test setup: the broken chunk was stored")
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if int64(len(idx.chunks)) != stats.Chunks {
		t.Fatalf("indexed %d of %d chunks; the failed chunk must be indexed against the compacted object", len(idx.chunks), stats.Chunks)
	}
	c := idx.chunks[1]
	comp := st.get(idx.final.GetCompactedKey())
	recs, _ := logformat.DecodeRecords(comp[c.GetByteOffset() : c.GetByteOffset()+c.GetSizeBytes()])
	if int64(len(recs)) != c.GetLineCount() || recs[0].N != c.GetFirstLine() {
		t.Fatal("the failed chunk's range in the compacted object does not hold its lines")
	}
}
