// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package logs reads durable attempt logs for the gateway (ADR-0020).
//
// The agent wrote the bytes to the result store and the index to Postgres.
// This package joins them: the index says which object (or which byte range of
// the compacted object) holds any line, and the object store serves exactly
// that. It reads ONLY keys the index names -- never a listing -- so what the
// API serves is exactly what a lease-holding agent indexed.
//
// Everything here is bounded: lines per page, bytes read per request, and the
// bytes and wall time a search may scan. A pathological log makes a request
// incomplete (and says so), never slow for everyone.
package logs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/logformat"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// ErrObjectMissing is returned by an ObjectReader when the key does not exist.
var ErrObjectMissing = errors.New("log object missing")

// ErrBadQuery is a client error in a search request.
var ErrBadQuery = errors.New("bad log query")

// Index is the read side of the log index.
type Index interface {
	Summary(ctx context.Context, attemptID string) (store.AttemptLogRow, error)
	Chunks(ctx context.Context, attemptID string, fromLine int64, limit int) ([]store.LogChunkRow, error)
}

// ObjectReader reads a byte range of one object; length < 0 means to the end.
type ObjectReader interface {
	Read(ctx context.Context, key string, offset, length int64) ([]byte, error)
}

// StoreIndex adapts the store package's queries to Index.
type StoreIndex struct{ DB store.GWQuerier }

// Summary implements Index.
func (s StoreIndex) Summary(ctx context.Context, attemptID string) (store.AttemptLogRow, error) {
	return store.GatewayGetAttemptLog(ctx, s.DB, attemptID)
}

// Chunks implements Index.
func (s StoreIndex) Chunks(ctx context.Context, attemptID string, fromLine int64, limit int) ([]store.LogChunkRow, error) {
	return store.GatewayListLogChunks(ctx, s.DB, attemptID, fromLine, limit)
}

// Limits bounds every request.
type Limits struct {
	// MaxPageLines caps lines per page and per tail.
	MaxPageLines int
	// MaxReadBytes caps object bytes read to serve one page.
	MaxReadBytes int64
	// DefaultScanBytes and MaxScanBytes bound a search's scan.
	DefaultScanBytes int64
	MaxScanBytes     int64
	// MaxMatches caps a search's result.
	MaxMatches int
	// MaxPatternBytes caps a search pattern.
	MaxPatternBytes int
	// SearchTimeout bounds a search's wall time.
	SearchTimeout time.Duration
}

// DefaultLimits are ADR-0020's numbers.
func DefaultLimits() Limits {
	return Limits{
		MaxPageLines:     10_000,
		MaxReadBytes:     32 << 20,
		DefaultScanBytes: 64 << 20,
		MaxScanBytes:     256 << 20,
		MaxMatches:       1000,
		MaxPatternBytes:  512,
		SearchTimeout:    15 * time.Second,
	}
}

// Reader serves pages, tails and searches.
type Reader struct {
	idx    Index
	obj    ObjectReader
	limits Limits
}

// NewReader builds a reader. Zero limits take the defaults.
func NewReader(idx Index, obj ObjectReader, limits Limits) *Reader {
	d := DefaultLimits()
	if limits.MaxPageLines <= 0 {
		limits.MaxPageLines = d.MaxPageLines
	}
	if limits.MaxReadBytes <= 0 {
		limits.MaxReadBytes = d.MaxReadBytes
	}
	if limits.DefaultScanBytes <= 0 {
		limits.DefaultScanBytes = d.DefaultScanBytes
	}
	if limits.MaxScanBytes <= 0 {
		limits.MaxScanBytes = d.MaxScanBytes
	}
	if limits.MaxMatches <= 0 {
		limits.MaxMatches = d.MaxMatches
	}
	if limits.MaxPatternBytes <= 0 {
		limits.MaxPatternBytes = d.MaxPatternBytes
	}
	if limits.SearchTimeout <= 0 {
		limits.SearchTimeout = d.SearchTimeout
	}
	return &Reader{idx: idx, obj: obj, limits: limits}
}

// Limits reports the effective limits.
func (r *Reader) Limits() Limits { return r.limits }

// Line is one log line as the API serves it.
type Line struct {
	N       int64     `json:"n"`
	TS      time.Time `json:"ts"`
	Stream  string    `json:"stream"`
	Message string    `json:"message"`
	Partial bool      `json:"partial,omitempty"`
	// Kind is set on the platform's own lines: truncated, truncation_summary,
	// dropped, gap.
	Kind string `json:"kind,omitempty"`
	// GapEnd is the last line a gap line stands in for.
	GapEnd int64 `json:"gap_end,omitempty"`
}

// End is the last line number this line accounts for.
func (l Line) End() int64 {
	if l.GapEnd > l.N {
		return l.GapEnd
	}
	return l.N
}

// Log states as the API reports them.
const (
	StateNone     = "none"
	StateLive     = "live"
	StateComplete = "complete"
	StatePartial  = "partial"
	StateExpired  = "expired"
)

// Status is the log's state as the API reports it.
type Status struct {
	State          string     `json:"state"`
	AttemptState   string     `json:"attempt_state"`
	Lines          int64      `json:"lines"`
	Bytes          int64      `json:"bytes"`
	Chunks         int64      `json:"chunks"`
	TruncatedLines int64      `json:"truncated_lines"`
	TruncatedBytes int64      `json:"truncated_bytes"`
	DroppedLines   int64      `json:"dropped_lines"`
	Compacted      bool       `json:"compacted"`
	LastChunkAt    *time.Time `json:"last_chunk_at,omitempty"`
	FinalizedAt    *time.Time `json:"finalized_at,omitempty"`
	// Detail is one honest sentence about what the state means for THIS
	// attempt, for consoles and CLIs to show verbatim.
	Detail string `json:"detail"`
}

// TerminalAttempt reports whether an attempt state is final.
func TerminalAttempt(state string) bool {
	switch strings.ToUpper(state) {
	case "SUCCEEDED", "FAILED", "TIMED_OUT", "LOST", "CANCELLED":
		return true
	}
	return false
}

// Summary returns the index row (found=false when nothing was shipped) and the
// derived status.
func (r *Reader) Summary(ctx context.Context, attemptID, attemptState string) (store.AttemptLogRow, bool, Status, error) {
	row, err := r.idx.Summary(ctx, attemptID)
	if errors.Is(err, store.ErrNotFound) {
		return row, false, deriveStatus(row, false, attemptState), nil
	}
	if err != nil {
		return row, false, Status{}, err
	}
	return row, true, deriveStatus(row, true, attemptState), nil
}

func deriveStatus(row store.AttemptLogRow, found bool, attemptState string) Status {
	st := Status{AttemptState: attemptState}
	terminal := TerminalAttempt(attemptState)
	if !found {
		st.State = StateNone
		switch {
		case terminal:
			st.Detail = "No log was recorded for this attempt: it produced no output, or the worker that ran it did not ship logs (an agent or gateway older than log shipping, or shipping disabled)."
		case strings.EqualFold(attemptState, "RUNNING"):
			st.Detail = "The attempt is running but has not shipped any output yet."
		default:
			st.Detail = "The attempt has not started, so there is no log yet."
		}
		return st
	}
	st.Lines, st.Bytes, st.Chunks = row.LineCount, row.ByteCount, row.ChunkCount
	st.TruncatedLines, st.TruncatedBytes, st.DroppedLines = row.TruncatedLines, row.TruncatedBytes, row.DroppedLines
	st.Compacted = row.CompactedKey != ""
	st.LastChunkAt, st.FinalizedAt = row.LastChunkAt, row.FinalizedAt
	switch {
	case row.State == store.LogStateComplete:
		st.State = StateComplete
		st.Detail = "The log is complete."
	case terminal:
		st.State = StatePartial
		st.Detail = "The attempt ended without its log being finalized (the agent crashed, lost its lease, or could not reach the object store); everything shipped before that is shown."
	default:
		st.State = StateLive
		st.Detail = "The attempt is running; new output appears within a few seconds."
	}
	if st.TruncatedLines > 0 {
		st.Detail += fmt.Sprintf(" %d lines were omitted by the per-attempt size cap; the beginning and the end are kept.", st.TruncatedLines)
	}
	if st.DroppedLines > 0 {
		st.Detail += fmt.Sprintf(" %d lines were dropped because log shipping fell behind.", st.DroppedLines)
	}
	return st
}

// Page is a run of consecutive lines.
type Page struct {
	Lines []Line
	// NextLine is the cursor to continue from.
	NextLine int64
	// Unreadable counts chunks the index names that could not be read.
	Unreadable int
	// Readable counts chunks that were read.
	Readable int
	// BytesRead is the object bytes this page cost.
	BytesRead int64
}

// ReadLines returns up to limit lines starting at from.
func (r *Reader) ReadLines(ctx context.Context, attemptID string, row store.AttemptLogRow, from int64, limit int) (Page, error) {
	if from < 1 {
		from = 1
	}
	if limit <= 0 || limit > r.limits.MaxPageLines {
		limit = r.limits.MaxPageLines
	}
	page := Page{NextLine: from}
	err := r.walk(ctx, attemptID, row, from, r.limits.MaxReadBytes, func(l Line) bool {
		page.Lines = append(page.Lines, l)
		page.NextLine = l.End() + 1
		return len(page.Lines) < limit
	}, &page)
	return page, err
}

// TailStart is the first line of the last n lines.
func TailStart(row store.AttemptLogRow, n int) int64 {
	start := row.LineCount - int64(n) + 1
	if start < 1 {
		start = 1
	}
	return start
}

// walk visits lines from `from` in order until visit returns false, the index
// is exhausted, or the byte budget is spent. Unreadable chunks and holes in the
// numbering are surfaced as gap lines rather than skipped silently.
func (r *Reader) walk(ctx context.Context, attemptID string, row store.AttemptLogRow, from, budget int64,
	visit func(Line) bool, page *Page) error {
	expect := from
	for {
		chunks, err := r.idx.Chunks(ctx, attemptID, expect, 100)
		if err != nil {
			return err
		}
		if len(chunks) == 0 {
			return nil
		}
		for _, c := range chunks {
			if err := ctx.Err(); err != nil {
				return err
			}
			if c.FirstLine > expect {
				// A chunk the index never received (its upload or report failed).
				if !visit(gapLine(expect, c.FirstLine-1, "never indexed")) {
					page.NextLine = c.FirstLine
					return nil
				}
				expect = c.FirstLine
			}
			if page.BytesRead >= budget {
				return nil
			}
			recs, readErr := r.readChunk(ctx, row, c)
			page.BytesRead += c.SizeBytes
			if readErr != nil {
				page.Unreadable++
				start := max(expect, c.FirstLine)
				if !visit(gapLine(start, c.LastLine(), readErr.Error())) {
					page.NextLine = c.LastLine() + 1
					return nil
				}
				expect = c.LastLine() + 1
				continue
			}
			page.Readable++
			for _, rec := range recs {
				if rec.N < expect {
					continue
				}
				if !visit(toLine(rec)) {
					return nil
				}
				expect = rec.N + 1
			}
			if c.LastLine()+1 > expect {
				expect = c.LastLine() + 1
			}
		}
		if len(chunks) < 100 {
			return nil
		}
	}
}

func (r *Reader) readChunk(ctx context.Context, row store.AttemptLogRow, c store.LogChunkRow) ([]logformat.Record, error) {
	var b []byte
	var err error
	if row.CompactedKey != "" {
		b, err = r.obj.Read(ctx, row.CompactedKey, c.ByteOffset, c.SizeBytes)
	} else {
		b, err = r.obj.Read(ctx, c.ObjectKey, 0, -1)
	}
	if errors.Is(err, ErrObjectMissing) {
		return nil, errors.New("object missing (expired by retention or deleted)")
	}
	if err != nil {
		return nil, fmt.Errorf("object unreadable: %w", err)
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != c.SHA256 {
		return nil, errors.New("checksum mismatch")
	}
	recs, _ := logformat.DecodeRecords(b)
	return recs, nil
}

func gapLine(from, to int64, why string) Line {
	return Line{
		N: from, GapEnd: to, Stream: logformat.StreamNexus, Kind: logformat.KindGap,
		Message: fmt.Sprintf("[nexus] lines %d-%d are unavailable: %s", from, to, why),
	}
}

func toLine(r logformat.Record) Line {
	return Line{
		N: r.N, TS: time.UnixMilli(r.TS).UTC(), Stream: r.S, Message: r.M, Partial: r.P, Kind: r.K,
	}
}

// SearchQuery is a bounded scan.
type SearchQuery struct {
	Pattern         string
	Regex           bool
	CaseInsensitive bool
	Stream          string
	FromLine        int64
	MaxMatches      int
	ScanBytes       int64
}

// SearchResult is what a bounded scan found.
type SearchResult struct {
	Matches      []Line `json:"matches"`
	NextLine     int64  `json:"next_line"`
	ScannedLines int64  `json:"scanned_lines"`
	ScannedBytes int64  `json:"scanned_bytes"`
	// Complete is true when the scan reached the end of the indexed log.
	Complete bool `json:"complete"`
	// StoppedBy names the bound that ended an incomplete scan: matches,
	// bytes or time.
	StoppedBy  string `json:"stopped_by,omitempty"`
	Unreadable int    `json:"unreadable_chunks,omitempty"`
}

// Search scans forward from q.FromLine. Regular expressions are Go RE2:
// matching is linear in the input, so no pattern can make a scan
// catastrophically slow; the bounds cap the input.
func (r *Reader) Search(ctx context.Context, attemptID string, row store.AttemptLogRow, q SearchQuery) (SearchResult, error) {
	if q.Pattern == "" {
		return SearchResult{}, fmt.Errorf("%w: q is required", ErrBadQuery)
	}
	if len(q.Pattern) > r.limits.MaxPatternBytes {
		return SearchResult{}, fmt.Errorf("%w: pattern longer than %d bytes", ErrBadQuery, r.limits.MaxPatternBytes)
	}
	var match func(string) bool
	if q.Regex {
		expr := q.Pattern
		if q.CaseInsensitive {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return SearchResult{}, fmt.Errorf("%w: invalid regular expression: %v", ErrBadQuery, err)
		}
		match = re.MatchString
	} else if q.CaseInsensitive {
		needle := strings.ToLower(q.Pattern)
		match = func(s string) bool { return strings.Contains(strings.ToLower(s), needle) }
	} else {
		match = func(s string) bool { return strings.Contains(s, q.Pattern) }
	}
	maxMatches := q.MaxMatches
	if maxMatches <= 0 || maxMatches > r.limits.MaxMatches {
		maxMatches = min(200, r.limits.MaxMatches)
	}
	budget := q.ScanBytes
	if budget <= 0 {
		budget = r.limits.DefaultScanBytes
	}
	if budget > r.limits.MaxScanBytes {
		budget = r.limits.MaxScanBytes
	}
	from := max(q.FromLine, 1)

	sctx, cancel := context.WithTimeout(ctx, r.limits.SearchTimeout)
	defer cancel()
	res := SearchResult{NextLine: from}
	page := Page{NextLine: from}
	stopped := ""
	err := r.walk(sctx, attemptID, row, from, budget, func(l Line) bool {
		res.ScannedLines++
		res.NextLine = l.End() + 1
		if l.Kind == logformat.KindGap {
			return true
		}
		if q.Stream != "" && l.Stream != q.Stream {
			return true
		}
		if match(l.Message) {
			res.Matches = append(res.Matches, l)
			if len(res.Matches) >= maxMatches {
				stopped = "matches"
				return false
			}
		}
		return true
	}, &page)
	res.ScannedBytes = page.BytesRead
	res.Unreadable = page.Unreadable
	switch {
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		stopped = "time"
	case err != nil:
		return res, err
	}
	if stopped == "" && page.BytesRead >= budget && res.NextLine <= row.LineCount {
		stopped = "bytes"
	}
	res.StoppedBy = stopped
	// Without a bound firing, the walk ended because the index had nothing
	// further -- the end of what has been shipped so far.
	res.Complete = stopped == ""
	if res.Matches == nil {
		res.Matches = []Line{}
	}
	return res, nil
}
