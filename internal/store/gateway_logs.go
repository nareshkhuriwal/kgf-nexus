// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The attempt log INDEX (ADR-0020, migration 0025). Owned by the api-gateway: it
// terminates the worker RPC that writes these rows and serves the API that reads
// them. The log bytes live in the result store; these rows only say which
// objects make up an attempt's log and where each line is.

// Log index states as stored. "partial", "expired" and "none" are derived at
// read time and never stored.
const (
	LogStateOpen     = "open"
	LogStateComplete = "complete"
)

// Server-side bounds on what one agent can make the index hold. The agent's
// defaults sit far inside them; they exist because the index is written from
// OUTSIDE the control plane and must not be an unbounded sink.
const (
	// MaxLogChunkBytes caps one chunk object. The agent seals at 256 KiB.
	MaxLogChunkBytes = 8 << 20
	// MaxLogChunksPerReport caps one ReportLogs call.
	MaxLogChunksPerReport = 256
	// MaxLogChunksPerAttempt caps an attempt's index. At the agent's slowest
	// flush cadence this is days of continuous output.
	MaxLogChunksPerAttempt = 50_000
	// MaxLogLinesPerChunk caps one chunk's records.
	MaxLogLinesPerChunk = 1 << 20
)

// Errors the worker RPC maps to Ack reasons.
var (
	// ErrLogFinalized: the attempt's log already received its final report.
	ErrLogFinalized = errors.New("log_finalized")
	// ErrInvalidLogChunk: a chunk failed validation (key outside the attempt's
	// prefix, bad bounds, too many chunks).
	ErrInvalidLogChunk = errors.New("invalid_log_chunk")
)

// LogKeyPrefix is the only prefix an attempt's log objects may live under. The
// gateway enforces it on every indexed key: without the check, a worker could
// index a key naming ANOTHER tenant's object and read it back through the log
// API.
func LogKeyPrefix(namespace, attemptID string) string {
	return namespace + "/logs/" + attemptID + "/"
}

// AttemptLogRow is the per-attempt summary.
type AttemptLogRow struct {
	AttemptID       string
	Namespace       string
	State           string
	ChunkCount      int64
	LineCount       int64
	ByteCount       int64
	TruncatedLines  int64
	TruncatedBytes  int64
	DroppedLines    int64
	CompactedKey    string
	CompactedSHA256 string
	CompactedBytes  int64
	CreatedAt       time.Time
	LastChunkAt     *time.Time
	FinalizedAt     *time.Time
}

// LogChunkRow is one indexed chunk.
type LogChunkRow struct {
	Seq        int64
	ObjectKey  string
	FirstLine  int64
	LineCount  int64
	ByteOffset int64
	SizeBytes  int64
	SHA256     string
	FirstTS    *time.Time
	LastTS     *time.Time
}

// LastLine is the chunk's last line number (inclusive); FirstLine-1 when empty.
func (c LogChunkRow) LastLine() int64 { return c.FirstLine + c.LineCount - 1 }

// LogFinalRow is the agent's final report.
type LogFinalRow struct {
	CompactedKey    string
	CompactedSHA256 string
	TotalLines      int64
	TotalBytes      int64
	TruncatedLines  int64
	TruncatedBytes  int64
	DroppedLines    int64
	ChunkCount      int64
}

// LogAppendResult says what an append actually changed, for the RPC's log line
// and for tests of idempotency.
type LogAppendResult struct {
	Inserted   int
	Duplicates int
	Finalized  bool
}

// GatewayAppendLogChunks indexes chunks for an attempt and, when final is set,
// finalizes its log -- in ONE transaction, so a crash between the two can never
// leave a log marked complete without its last chunk.
//
// The caller has already fenced the lease. This function owns the validation
// that does not depend on the lease: the attempt must exist, every key must be
// under the attempt's own prefix, bounds must hold, and nothing may follow a
// final report. A redelivered chunk (same attempt, same seq) is ignored by the
// primary key and does not advance any counter.
func GatewayAppendLogChunks(ctx context.Context, db GWDB, attemptID string, chunks []LogChunkRow, final *LogFinalRow) (LogAppendResult, error) {
	var res LogAppendResult
	if len(chunks) > MaxLogChunksPerReport {
		return res, fmt.Errorf("%w: %d chunks in one report (max %d)", ErrInvalidLogChunk, len(chunks), MaxLogChunksPerReport)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin log append: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	var namespace string
	err = tx.QueryRow(ctx, `
		SELECT r.namespace FROM task_attempts a JOIN workflow_runs r ON r.id = a.run_id
		 WHERE a.id = $1`, attemptID).Scan(&namespace)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, ErrNotFound
	}
	if err != nil {
		return res, fmt.Errorf("resolve attempt namespace: %w", err)
	}
	prefix := LogKeyPrefix(namespace, attemptID)
	for _, c := range chunks {
		if err := validateLogChunk(prefix, c); err != nil {
			return res, err
		}
	}
	if final != nil && final.CompactedKey != "" && !validLogKey(prefix, final.CompactedKey) {
		return res, fmt.Errorf("%w: compacted key %q is outside %s", ErrInvalidLogChunk, final.CompactedKey, prefix)
	}

	// Create the summary row on first contact, then lock it: concurrent reports
	// for one attempt (an agent retrying an RPC whose first try is still in
	// flight) serialize here, which keeps the counters exact.
	if _, err := tx.Exec(ctx, `
		INSERT INTO attempt_logs (attempt_id, namespace) VALUES ($1, $2)
		ON CONFLICT (attempt_id) DO NOTHING`, attemptID, namespace); err != nil {
		return res, fmt.Errorf("create attempt log: %w", err)
	}
	var state string
	var chunkCount int64
	if err := tx.QueryRow(ctx, `
		SELECT state, chunk_count FROM attempt_logs WHERE attempt_id = $1 FOR UPDATE`,
		attemptID).Scan(&state, &chunkCount); err != nil {
		return res, fmt.Errorf("lock attempt log: %w", err)
	}
	if state == LogStateComplete {
		// A duplicate of the final report itself (the ack was lost) is fine: every
		// chunk in it already exists. Anything NEW after the final report is a
		// protocol violation and is refused rather than silently appended to a
		// log the API already presents as whole.
		for _, c := range chunks {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM attempt_log_chunks WHERE attempt_id = $1 AND seq = $2)`,
				attemptID, c.Seq).Scan(&exists); err != nil {
				return res, fmt.Errorf("check finalized chunk: %w", err)
			}
			if !exists {
				return res, ErrLogFinalized
			}
			res.Duplicates++
		}
		res.Finalized = true
		return res, tx.Commit(ctx)
	}
	if chunkCount+int64(len(chunks)) > MaxLogChunksPerAttempt {
		return res, fmt.Errorf("%w: attempt would exceed %d chunks", ErrInvalidLogChunk, MaxLogChunksPerAttempt)
	}

	var addBytes, maxLine int64
	var lastTS *time.Time
	for _, c := range chunks {
		tag, err := tx.Exec(ctx, `
			INSERT INTO attempt_log_chunks
			    (attempt_id, seq, object_key, first_line, line_count, byte_offset, size_bytes, sha256, first_ts, last_ts)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (attempt_id, seq) DO NOTHING`,
			attemptID, c.Seq, c.ObjectKey, c.FirstLine, c.LineCount, c.ByteOffset, c.SizeBytes, c.SHA256, c.FirstTS, c.LastTS)
		if err != nil {
			return res, fmt.Errorf("insert log chunk %d: %w", c.Seq, err)
		}
		if tag.RowsAffected() == 0 {
			res.Duplicates++
			continue
		}
		res.Inserted++
		addBytes += c.SizeBytes
		if c.LastLine() > maxLine {
			maxLine = c.LastLine()
		}
		if c.LastTS != nil && (lastTS == nil || c.LastTS.After(*lastTS)) {
			lastTS = c.LastTS
		}
	}
	if res.Inserted > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE attempt_logs
			   SET chunk_count   = chunk_count + $2,
			       byte_count    = byte_count + $3,
			       line_count    = GREATEST(line_count, $4),
			       last_chunk_at = now()
			 WHERE attempt_id = $1`,
			attemptID, res.Inserted, addBytes, maxLine); err != nil {
			return res, fmt.Errorf("advance attempt log: %w", err)
		}
	}

	if final != nil {
		var compacted *string
		var compactedSHA *string
		var compactedBytes *int64
		if final.CompactedKey != "" {
			k, s, b := final.CompactedKey, final.CompactedSHA256, final.TotalBytes
			compacted, compactedSHA, compactedBytes = &k, &s, &b
		}
		// The totals reported by the agent are authoritative for what it
		// OMITTED (truncation, drops) -- only the agent knows that. Line and
		// byte counts are taken from the index when it has seen more, so an
		// agent cannot make the API claim fewer lines than it can serve.
		if _, err := tx.Exec(ctx, `
			UPDATE attempt_logs
			   SET state            = 'complete',
			       finalized_at     = now(),
			       line_count       = GREATEST(line_count, $2),
			       truncated_lines  = $3,
			       truncated_bytes  = $4,
			       dropped_lines    = $5,
			       compacted_key    = $6,
			       compacted_sha256 = $7,
			       compacted_bytes  = $8
			 WHERE attempt_id = $1`,
			attemptID, final.TotalLines, final.TruncatedLines, final.TruncatedBytes, final.DroppedLines,
			compacted, compactedSHA, compactedBytes); err != nil {
			return res, fmt.Errorf("finalize attempt log: %w", err)
		}
		res.Finalized = true
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit log append: %w", err)
	}
	return res, nil
}

func validLogKey(prefix, key string) bool {
	if !strings.HasPrefix(key, prefix) || len(key) == len(prefix) || len(key) > 1024 {
		return false
	}
	rest := key[len(prefix):]
	// No traversal-shaped keys: S3 keys are opaque, but a reader that ever maps
	// them onto a filesystem (a backup restore) must not be handed "../".
	return !strings.Contains(rest, "..") && !strings.Contains(rest, "/")
}

func validateLogChunk(prefix string, c LogChunkRow) error {
	switch {
	case c.Seq < 0:
		return fmt.Errorf("%w: negative seq", ErrInvalidLogChunk)
	case !validLogKey(prefix, c.ObjectKey):
		return fmt.Errorf("%w: key %q is outside %s", ErrInvalidLogChunk, c.ObjectKey, prefix)
	case c.FirstLine < 1:
		return fmt.Errorf("%w: first_line must be >= 1", ErrInvalidLogChunk)
	case c.LineCount < 0 || c.LineCount > MaxLogLinesPerChunk:
		return fmt.Errorf("%w: line_count %d out of range", ErrInvalidLogChunk, c.LineCount)
	case c.SizeBytes < 0 || c.SizeBytes > MaxLogChunkBytes:
		return fmt.Errorf("%w: size_bytes %d out of range", ErrInvalidLogChunk, c.SizeBytes)
	case c.ByteOffset < 0:
		return fmt.Errorf("%w: negative byte_offset", ErrInvalidLogChunk)
	case len(c.SHA256) != 64:
		return fmt.Errorf("%w: sha256 must be 64 hex characters", ErrInvalidLogChunk)
	}
	return nil
}

// GatewayGetAttemptLog reads an attempt's log summary. ErrNotFound when the
// attempt has shipped nothing.
func GatewayGetAttemptLog(ctx context.Context, q GWQuerier, attemptID string) (AttemptLogRow, error) {
	var r AttemptLogRow
	var compactedKey, compactedSHA *string
	var compactedBytes *int64
	err := q.QueryRow(ctx, `
		SELECT attempt_id::text, namespace, state, chunk_count, line_count, byte_count,
		       truncated_lines, truncated_bytes, dropped_lines,
		       compacted_key, compacted_sha256, compacted_bytes,
		       created_at, last_chunk_at, finalized_at
		  FROM attempt_logs WHERE attempt_id = $1`, attemptID,
	).Scan(&r.AttemptID, &r.Namespace, &r.State, &r.ChunkCount, &r.LineCount, &r.ByteCount,
		&r.TruncatedLines, &r.TruncatedBytes, &r.DroppedLines,
		&compactedKey, &compactedSHA, &compactedBytes,
		&r.CreatedAt, &r.LastChunkAt, &r.FinalizedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AttemptLogRow{}, ErrNotFound
	}
	if err != nil {
		return AttemptLogRow{}, fmt.Errorf("get attempt log: %w", err)
	}
	if compactedKey != nil {
		r.CompactedKey = *compactedKey
	}
	if compactedSHA != nil {
		r.CompactedSHA256 = *compactedSHA
	}
	if compactedBytes != nil {
		r.CompactedBytes = *compactedBytes
	}
	return r, nil
}

// GatewayListLogChunks returns up to limit chunks, in seq order, whose last line
// is at or after fromLine -- i.e. the chunks a read starting at fromLine needs.
// Served by attempt_log_chunks_line_idx.
func GatewayListLogChunks(ctx context.Context, q GWQuerier, attemptID string, fromLine int64, limit int) ([]LogChunkRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	if fromLine < 1 {
		fromLine = 1
	}
	rows, err := q.Query(ctx, `
		SELECT seq, object_key, first_line, line_count, byte_offset, size_bytes, sha256, first_ts, last_ts
		  FROM attempt_log_chunks
		 WHERE attempt_id = $1 AND (first_line + line_count) > $2
		 ORDER BY seq
		 LIMIT $3`, attemptID, fromLine, limit)
	if err != nil {
		return nil, fmt.Errorf("list log chunks: %w", err)
	}
	defer rows.Close()
	var out []LogChunkRow
	for rows.Next() {
		var c LogChunkRow
		if err := rows.Scan(&c.Seq, &c.ObjectKey, &c.FirstLine, &c.LineCount, &c.ByteOffset,
			&c.SizeBytes, &c.SHA256, &c.FirstTS, &c.LastTS); err != nil {
			return nil, fmt.Errorf("scan log chunk: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
