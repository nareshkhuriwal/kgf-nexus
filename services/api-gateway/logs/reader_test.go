// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package logs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/logformat"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// ── fakes ────────────────────────────────────────────────────────────────

type fakeIndex struct {
	row    store.AttemptLogRow
	found  bool
	chunks []store.LogChunkRow
}

func (f *fakeIndex) Summary(context.Context, string) (store.AttemptLogRow, error) {
	if !f.found {
		return store.AttemptLogRow{}, store.ErrNotFound
	}
	return f.row, nil
}

func (f *fakeIndex) Chunks(_ context.Context, _ string, from int64, limit int) ([]store.LogChunkRow, error) {
	var out []store.LogChunkRow
	for _, c := range f.chunks {
		if c.FirstLine+c.LineCount > from {
			out = append(out, c)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

type fakeObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
	reads   []string
}

func (f *fakeObjects) Read(_ context.Context, key string, off, length int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, fmt.Sprintf("%s@%d+%d", key, off, length))
	b, ok := f.objects[key]
	if !ok {
		return nil, ErrObjectMissing
	}
	if length < 0 {
		return b, nil
	}
	return b[off : off+length], nil
}

// build writes lines into chunks of perChunk records, optionally compacted.
func build(t *testing.T, lines []logformat.Record, perChunk int, compact bool) (*fakeIndex, *fakeObjects) {
	t.Helper()
	idx := &fakeIndex{found: true}
	obj := &fakeObjects{objects: map[string][]byte{}}
	var all bytes.Buffer
	for i, seq := 0, int64(0); i < len(lines); i, seq = i+perChunk, seq+1 {
		end := min(i+perChunk, len(lines))
		var b bytes.Buffer
		for _, r := range lines[i:end] {
			b.Write(r.Encode())
		}
		sum := sha256.Sum256(b.Bytes())
		key := fmt.Sprintf("ns/logs/a1/%08d.ndjson", seq)
		idx.chunks = append(idx.chunks, store.LogChunkRow{
			Seq: seq, ObjectKey: key, FirstLine: lines[i].N, LineCount: int64(end - i),
			ByteOffset: int64(all.Len()), SizeBytes: int64(b.Len()), SHA256: hex.EncodeToString(sum[:]),
		})
		obj.objects[key] = b.Bytes()
		all.Write(b.Bytes())
	}
	idx.row = store.AttemptLogRow{AttemptID: "a1", State: store.LogStateComplete,
		LineCount: int64(len(lines)), ChunkCount: int64(len(idx.chunks)), ByteCount: int64(all.Len())}
	if compact {
		idx.row.CompactedKey = "ns/logs/a1/log.ndjson"
		obj.objects[idx.row.CompactedKey] = all.Bytes()
		for _, c := range idx.chunks {
			delete(obj.objects, c.ObjectKey) // compaction's chunks are orphans now
		}
	}
	return idx, obj
}

func genLines(n int) []logformat.Record {
	out := make([]logformat.Record, n)
	for i := range out {
		s := logformat.StreamStdout
		if i%3 == 2 {
			s = logformat.StreamStderr
		}
		out[i] = logformat.Record{N: int64(i + 1), TS: 1_700_000_000_000 + int64(i), S: s, M: fmt.Sprintf("line %d", i+1)}
	}
	return out
}

func numbers(lines []Line) []int64 {
	out := make([]int64, len(lines))
	for i, l := range lines {
		out[i] = l.N
	}
	return out
}

// ── tests ────────────────────────────────────────────────────────────────

func TestTailAndPagination(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprintf("compacted=%v", compact), func(t *testing.T) {
			idx, obj := build(t, genLines(95), 10, compact)
			r := NewReader(idx, obj, Limits{})
			ctx := context.Background()

			page, err := r.ReadLines(ctx, "a1", idx.row, TailStart(idx.row, 7), 7)
			if err != nil {
				t.Fatal(err)
			}
			if got := numbers(page.Lines); len(got) != 7 || got[0] != 89 || got[6] != 95 || page.NextLine != 96 {
				t.Fatalf("tail 7 = %v next=%d", got, page.NextLine)
			}

			// Walk the whole log in pages of 13 and check nothing repeats or is skipped.
			var seen []int64
			for from := int64(1); from <= idx.row.LineCount; {
				p, err := r.ReadLines(ctx, "a1", idx.row, from, 13)
				if err != nil {
					t.Fatal(err)
				}
				seen = append(seen, numbers(p.Lines)...)
				from = p.NextLine
			}
			for i, n := range seen {
				if n != int64(i+1) {
					t.Fatalf("pagination produced %v; line %d wrong", seen, i)
				}
			}
			if compact {
				for _, rd := range obj.reads {
					if !strings.HasPrefix(rd, "ns/logs/a1/log.ndjson@") || strings.HasSuffix(rd, "+-1") {
						t.Fatalf("compacted log read %q; reads must be ranges of the compacted object", rd)
					}
				}
			}
		})
	}
}

func TestLinesCarryStreamAndTime(t *testing.T) {
	idx, obj := build(t, genLines(3), 10, false)
	page, _ := NewReader(idx, obj, Limits{}).ReadLines(context.Background(), "a1", idx.row, 1, 10)
	l := page.Lines[2]
	if l.Stream != "stderr" || l.Message != "line 3" || !l.TS.Equal(time.UnixMilli(1_700_000_000_002).UTC()) {
		t.Fatalf("line = %+v", l)
	}
}

// TestMissingAndCorruptChunksBecomeGapLines: nothing is skipped silently.
func TestMissingAndCorruptChunksBecomeGapLines(t *testing.T) {
	idx, obj := build(t, genLines(30), 10, false)
	delete(obj.objects, idx.chunks[1].ObjectKey)    // lines 11-20 gone
	obj.objects[idx.chunks[2].ObjectKey][5] ^= 0xff // lines 21-30 corrupt
	page, err := NewReader(idx, obj, Limits{}).ReadLines(context.Background(), "a1", idx.row, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 12 || page.Unreadable != 2 || page.NextLine != 31 {
		t.Fatalf("got %d lines, %d unreadable, next %d", len(page.Lines), page.Unreadable, page.NextLine)
	}
	g1, g2 := page.Lines[10], page.Lines[11]
	if g1.Kind != "gap" || g1.N != 11 || g1.GapEnd != 20 || !strings.Contains(g1.Message, "missing") {
		t.Fatalf("gap 1 = %+v", g1)
	}
	if g2.Kind != "gap" || g2.N != 21 || !strings.Contains(g2.Message, "checksum") {
		t.Fatalf("gap 2 = %+v", g2)
	}
}

func TestAChunkNeverIndexedIsAGap(t *testing.T) {
	idx, obj := build(t, genLines(30), 10, false)
	idx.chunks = append(idx.chunks[:1], idx.chunks[2:]...) // seq 1 never reported
	page, _ := NewReader(idx, obj, Limits{}).ReadLines(context.Background(), "a1", idx.row, 1, 100)
	if page.Lines[10].Kind != "gap" || page.Lines[10].GapEnd != 20 || page.Lines[11].N != 21 {
		t.Fatalf("lines around the hole: %+v %+v", page.Lines[10], page.Lines[11])
	}
}

func TestSearch(t *testing.T) {
	lines := genLines(200)
	lines[41].M = "ERROR: connection refused"
	lines[150].M = "error: disk full"
	idx, obj := build(t, lines, 25, true)
	r := NewReader(idx, obj, Limits{})
	ctx := context.Background()

	res, err := r.Search(ctx, "a1", idx.row, SearchQuery{Pattern: "ERROR"})
	if err != nil || len(res.Matches) != 1 || res.Matches[0].N != 42 || !res.Complete {
		t.Fatalf("substring: %+v %v", res, err)
	}
	res, _ = r.Search(ctx, "a1", idx.row, SearchQuery{Pattern: "error", CaseInsensitive: true})
	if len(res.Matches) != 2 {
		t.Fatalf("case-insensitive found %d", len(res.Matches))
	}
	res, err = r.Search(ctx, "a1", idx.row, SearchQuery{Pattern: `^line 1\d$`, Regex: true})
	if err != nil || len(res.Matches) != 10 {
		t.Fatalf("regex found %d: %v", len(res.Matches), err)
	}
	res, _ = r.Search(ctx, "a1", idx.row, SearchQuery{Pattern: "line", Stream: "stderr"})
	for _, m := range res.Matches {
		if m.Stream != "stderr" {
			t.Fatalf("stream filter leaked %+v", m)
		}
	}
}

// TestSearchIsBoundedAndResumable: a bound stops the scan, says which, and
// hands back a cursor that continues without repeating a match.
func TestSearchIsBoundedAndResumable(t *testing.T) {
	idx, obj := build(t, genLines(500), 50, false)
	r := NewReader(idx, obj, Limits{})
	ctx := context.Background()

	var all []int64
	q := SearchQuery{Pattern: "line", MaxMatches: 120, FromLine: 1}
	for i := 0; i < 10; i++ {
		res, err := r.Search(ctx, "a1", idx.row, q)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, numbers(res.Matches)...)
		if res.Complete {
			break
		}
		if res.StoppedBy != "matches" {
			t.Fatalf("stopped_by = %q, want matches", res.StoppedBy)
		}
		q.FromLine = res.NextLine
	}
	if len(all) != 500 || all[499] != 500 {
		t.Fatalf("resumed search found %d matches", len(all))
	}

	res, _ := r.Search(ctx, "a1", idx.row, SearchQuery{Pattern: "zzz", ScanBytes: 1})
	if res.Complete || res.StoppedBy != "bytes" || res.NextLine > 51 {
		t.Fatalf("byte budget: %+v", res)
	}
}

func TestSearchRejectsBadQueries(t *testing.T) {
	idx, obj := build(t, genLines(5), 10, false)
	r := NewReader(idx, obj, Limits{})
	for _, q := range []SearchQuery{
		{Pattern: ""},
		{Pattern: "(unclosed", Regex: true},
		{Pattern: strings.Repeat("a", 513)},
	} {
		if _, err := r.Search(context.Background(), "a1", idx.row, q); !errors.Is(err, ErrBadQuery) {
			t.Errorf("query %+v: err = %v, want ErrBadQuery", q, err)
		}
	}
}

func TestStatusIsStatedHonestly(t *testing.T) {
	cases := []struct {
		name         string
		found        bool
		state        string
		attemptState string
		want         string
	}{
		{"not started", false, "", "PENDING", StateNone},
		{"running, nothing yet", false, "", "RUNNING", StateNone},
		{"old agent", false, "", "SUCCEEDED", StateNone},
		{"live", true, store.LogStateOpen, "RUNNING", StateLive},
		{"agent crashed", true, store.LogStateOpen, "LOST", StatePartial},
		{"complete", true, store.LogStateComplete, "FAILED", StateComplete},
	}
	for _, c := range cases {
		idx := &fakeIndex{found: c.found, row: store.AttemptLogRow{State: c.state, TruncatedLines: 7}}
		_, _, st, err := NewReader(idx, &fakeObjects{}, Limits{}).Summary(context.Background(), "a1", c.attemptState)
		if err != nil || st.State != c.want || st.Detail == "" {
			t.Errorf("%s: state=%q detail=%q err=%v, want %q", c.name, st.State, st.Detail, err, c.want)
		}
		if c.found && !strings.Contains(st.Detail, "7 lines were omitted") {
			t.Errorf("%s: truncation not stated: %q", c.name, st.Detail)
		}
	}
}
