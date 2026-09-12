// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package wal is the agent's local write-ahead log for attempt outcomes
// (architecture §6.3).
//
// WHY this exists at all: without it, an agent that dies between finishing a
// task and successfully reporting the result has silently destroyed completed
// work. The control plane's lease expires, the attempt becomes LOST, and the
// task is re-executed — expensive, and for an IDEMPOTENCY_MODE_NONE task, not
// even permitted. The WAL converts that expensive re-execution into a cheap
// re-report, which removes the COMMON crash case (deploys, agent OOM, node
// reboot) from the ambiguous path entirely. Only genuinely ambiguous crashes
// — those that land between "wrote STARTING" and "wrote COMPLETED" — still
// reach the indeterminate path.
//
// Design: one file per attempt, in one flat directory, written whole and
// fsync'd. A single shared append-only file would be smaller but would force
// every reader to replay the whole log and every deletion to become a
// compaction, and compaction is a category of bug we do not need. One file per
// attempt makes "delete the entry after a durable ack" an atomic unlink, and
// makes a torn write cost exactly one attempt rather than the entire log.
package wal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// State is the lifecycle position recorded in an entry. Only two values exist
// because only two are decidable locally: we know we are about to start, and we
// know we finished. Anything else is the control plane's business.
type State string

const (
	// StateStarting means: the agent intended to run this attempt, and the
	// outcome is NOT known. On replay this becomes
	// ATTEMPT_OUTCOME_INDETERMINATE — we cannot claim success we did not
	// observe, and we must not claim failure that may have had side effects.
	StateStarting State = "STARTING"

	// StateCompleted means: execution finished and the outcome below is
	// authoritative. On replay it is re-reported verbatim; the control plane
	// deduplicates on attempt_id.
	StateCompleted State = "COMPLETED"
)

// Outcome mirrors nexusv1.AttemptOutcome as a string so that the on-disk
// format does not depend on protobuf enum numbering. A wire-format renumber
// must never silently reinterpret a log written by an older agent as a
// different outcome.
type Outcome string

const (
	OutcomeSucceeded     Outcome = "SUCCEEDED"
	OutcomeFailed        Outcome = "FAILED"
	OutcomeTimedOut      Outcome = "TIMED_OUT"
	OutcomeIndeterminate Outcome = "INDETERMINATE"
)

// Entry is one attempt's durable record.
//
// The outcome payload is stored as pre-marshalled JSON rather than as typed
// protobuf structs so the WAL package stays free of the wire schema: the
// reporter owns interpretation, the WAL owns durability. That separation is
// what lets us fsync a result whose shape we have not yet finished designing.
type Entry struct {
	AttemptID  string `json:"attempt_id"`
	LeaseToken string `json:"lease_token"`
	State      State  `json:"state"`

	// RunID and NodeID are carried only for operator-legible logs during
	// replay; correctness never depends on them.
	RunID  string `json:"run_id,omitempty"`
	NodeID string `json:"node_id,omitempty"`

	Outcome Outcome `json:"outcome,omitempty"`

	// ResultJSON is the marshalled ReportResultRequest payload (outputs,
	// error, metrics) captured at completion time.
	ResultJSON json.RawMessage `json:"result_json,omitempty"`

	StartedAtUnixMs int64 `json:"started_at_unix_ms,omitempty"`
	EndedAtUnixMs   int64 `json:"ended_at_unix_ms,omitempty"`

	// ReplayCount lets an operator spot an entry that is failing to drain
	// (e.g. a permanently rejected stale token) instead of it silently
	// cycling forever.
	ReplayCount int `json:"replay_count,omitempty"`
}

// WAL is a directory of attempt records. It is safe for concurrent use: the
// executor goroutines write their own attempts while the reporter deletes
// acked ones.
type WAL struct {
	dir string
	mu  sync.Mutex
}

// Open prepares dir as a WAL, creating it if needed, and fsyncs the directory
// so that the directory entry itself survives a power loss. Skipping the
// directory fsync is a classic durability bug: the file's contents are on disk
// but the name that finds it is not.
func Open(dir string) (*WAL, error) {
	if dir == "" {
		return nil, errors.New("wal: empty directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("wal: mkdir %s: %w", dir, err)
	}
	if err := fsyncDir(dir); err != nil {
		return nil, fmt.Errorf("wal: fsync dir %s: %w", dir, err)
	}
	return &WAL{dir: dir}, nil
}

// Dir reports the backing directory, for log lines and tests.
func (w *WAL) Dir() string { return w.dir }

// RecordStarting durably records the intent to execute, and MUST return before
// the executor is started. If it has not returned, a crash leaves no record
// and the attempt simply becomes LOST via lease expiry — which is correct.
// What must never happen is the reverse: the task running with no record.
func (w *WAL) RecordStarting(e Entry) error {
	if e.AttemptID == "" {
		return errors.New("wal: attempt_id required")
	}
	e.State = StateStarting
	if e.StartedAtUnixMs == 0 {
		e.StartedAtUnixMs = time.Now().UnixMilli()
	}
	return w.put(e)
}

// RecordCompleted durably records the outcome and MUST be called BEFORE the
// result is reported. This ordering is the entire point of the WAL: if the
// agent dies after this returns but before the report is acked, the outcome
// survives and is replayed.
func (w *WAL) RecordCompleted(e Entry) error {
	if e.AttemptID == "" {
		return errors.New("wal: attempt_id required")
	}
	if e.Outcome == "" {
		return errors.New("wal: outcome required for COMPLETED entry")
	}
	e.State = StateCompleted
	if e.EndedAtUnixMs == 0 {
		e.EndedAtUnixMs = time.Now().UnixMilli()
	}
	return w.put(e)
}

// Delete removes an entry. It is called ONLY after a durable ack from the
// control plane; deleting earlier would reintroduce exactly the lost-result
// window the WAL exists to close. Deleting an absent entry is not an error,
// because a retried ack path must be idempotent.
func (w *WAL) Delete(attemptID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := os.Remove(w.path(attemptID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("wal: delete %s: %w", attemptID, err)
	}
	// The unlink must also be durable, or a crash could resurrect an entry we
	// already reported and acked. A duplicate re-report is harmless (the
	// server is idempotent on attempt_id) but it is noise we can cheaply avoid.
	return fsyncDir(w.dir)
}

// List returns every entry currently in the log, ordered by attempt_id so
// replay is deterministic and test assertions are stable.
//
// A file that fails to parse is SKIPPED rather than returned as an error: a
// torn final write during a power loss is an expected event, and one corrupt
// entry must not block replay of every healthy one. The corrupt file is left
// in place for an operator to inspect rather than deleted, because deleting
// evidence of a durability bug is how durability bugs stay unfixed.
func (w *WAL) List() ([]Entry, []error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	names, err := os.ReadDir(w.dir)
	if err != nil {
		return nil, []error{fmt.Errorf("wal: read dir %s: %w", w.dir, err)}
	}
	var (
		out  []Entry
		errs []error
	)
	for _, n := range names {
		if n.IsDir() || !strings.HasSuffix(n.Name(), entrySuffix) {
			continue
		}
		full := filepath.Join(w.dir, n.Name())
		b, err := os.ReadFile(full)
		if err != nil {
			errs = append(errs, fmt.Errorf("wal: read %s: %w", full, err))
			continue
		}
		var e Entry
		if err := json.Unmarshal(b, &e); err != nil {
			errs = append(errs, fmt.Errorf("wal: corrupt entry %s (left in place for inspection): %w", full, err))
			continue
		}
		if e.AttemptID == "" {
			errs = append(errs, fmt.Errorf("wal: entry %s has no attempt_id", full))
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AttemptID < out[j].AttemptID })
	return out, errs
}

// Get returns one entry, reporting false if it is absent.
func (w *WAL) Get(attemptID string) (Entry, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	b, err := os.ReadFile(w.path(attemptID))
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("wal: read %s: %w", attemptID, err)
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return Entry{}, false, fmt.Errorf("wal: corrupt entry %s: %w", attemptID, err)
	}
	return e, true, nil
}

// BumpReplayCount records that an entry has been through replay again. Purely
// diagnostic; a failure here must never block replay, so callers log and
// continue.
func (w *WAL) BumpReplayCount(attemptID string) error {
	e, ok, err := w.Get(attemptID)
	if err != nil || !ok {
		return err
	}
	e.ReplayCount++
	return w.put(e)
}

const entrySuffix = ".json"

// put writes the entry via a temp file + rename, then fsyncs both the file and
// the directory.
//
// Write-temp-then-rename is what makes an entry all-or-nothing. Writing in
// place would allow a crash to leave a half-written JSON document that parses
// as neither the old nor the new state; rename is atomic on POSIX, so a reader
// sees exactly one of the two.
func (w *WAL) put(e Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("wal: marshal %s: %w", e.AttemptID, err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	final := w.path(e.AttemptID)
	tmp, err := os.CreateTemp(w.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("wal: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("wal: write %s: %w", tmpName, err)
	}
	// fsync BEFORE rename: a rename that lands before the data reaches stable
	// storage gives us a durable name pointing at garbage.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("wal: fsync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("wal: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("wal: rename to %s: %w", final, err)
	}
	if err := fsyncDir(w.dir); err != nil {
		return fmt.Errorf("wal: fsync dir: %w", err)
	}
	return nil
}

func (w *WAL) path(attemptID string) string {
	return filepath.Join(w.dir, sanitize(attemptID)+entrySuffix)
}

// sanitize keeps a hostile or merely surprising attempt_id from escaping the
// WAL directory via path separators or "..". Attempt ids are server-generated
// UUIDs today, but a WAL that can be made to write outside its directory is
// not a WAL we want to trust.
func sanitize(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, id)
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// Some filesystems reject fsync on a directory handle. Durability of the
	// directory entry is best-effort there; the file contents are still
	// fsynced, which is the part that carries the outcome.
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}
