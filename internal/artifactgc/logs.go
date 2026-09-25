// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package artifactgc

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Log objects (ADR-0020).
//
// Agents write attempt logs to <namespace>/logs/<attempt_id>/ and index them in
// attempt_logs / attempt_log_chunks (migration 0025). Before this file the
// sweep knew nothing of them, so EVERY log object looked like an orphan and a
// deleting pass would have removed every log older than the grace period. The
// log index is therefore a reference source exactly like task_attempts.
//
// Which log keys are live:
//
//   - a log that was COMPACTED is served entirely from its compacted object,
//     so that key is live and its chunk objects are not (they are orphans the
//     sweep may collect -- that is how compaction reclaims them);
//   - a log that was NOT compacted (still running, a single chunk, or an agent
//     that stopped before finalizing) is served from its chunks, so every
//     indexed chunk key is live.
//
// Everything else under a logs/ prefix -- chunks a stale attempt uploaded but
// could not index because its lease was fenced, uploads whose ReportLogs never
// landed -- is an ordinary orphan, subject to the same grace period and the
// same list-then-query ordering as outputs. The ordering argument holds because
// the agent writes the object strictly before the RPC that indexes it.
//
// Deleting a run cascades to task_attempts and from there to both log tables,
// so a pruned run's logs become orphans with no extra machinery.
//
// RETENTION is the one deliberate departure from pure reachability, and it is
// OFF unless configured: with LogRetention > 0, a log whose index says it was
// last written (finalized, or its last chunk indexed) longer ago than that is
// excluded from the live set and its objects are collected. That is safe for
// logs and only for logs, because nothing downstream consumes a log -- a task's
// inputs are outputs, never logs. The index rows remain, so the API reports the
// log as "expired" rather than pretending it never existed.
var logRefSources = []string{"attempt_logs.compacted_key", "attempt_log_chunks.object_key"}

// LiveLogKeys returns the log object keys reachable from the log index.
//
// retention <= 0 keeps every indexed log. The cutoff is computed by the
// DATABASE (now() - interval) so the sweep host's clock cannot make it err.
func LiveLogKeys(ctx context.Context, q Querier, retention time.Duration) (map[string]struct{}, error) {
	live := make(map[string]struct{})
	// Treat "no retention" as an interval nothing can exceed, so one query
	// serves both modes and the retention branch is not an untested path.
	secs := int64(retention / time.Second)
	if retention <= 0 {
		secs = 0
	}
	rows, err := q.Query(ctx, `
		SELECT l.compacted_key AS key
		  FROM attempt_logs l
		 WHERE l.compacted_key IS NOT NULL
		   AND ($1::bigint = 0 OR COALESCE(l.finalized_at, l.last_chunk_at, l.created_at) > now() - ($1::bigint * interval '1 second'))
		UNION
		SELECT c.object_key
		  FROM attempt_log_chunks c
		  JOIN attempt_logs l ON l.attempt_id = c.attempt_id
		 WHERE l.compacted_key IS NULL
		   AND ($1::bigint = 0 OR COALESCE(l.finalized_at, l.last_chunk_at, l.created_at) > now() - ($1::bigint * interval '1 second'))`,
		secs)
	if err != nil {
		// Same rule as LiveKeys: an error is never "no references".
		return nil, fmt.Errorf("artifactgc: enumerate live log keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("artifactgc: scan live log key: %w", err)
		}
		live[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifactgc: iterate live log keys: %w", err)
	}
	return live, nil
}

// IsLogKey reports whether a key is under a namespace's logs/ prefix.
func IsLogKey(key string) bool {
	ns, rest, ok := strings.Cut(key, "/")
	return ok && ns != "" && strings.HasPrefix(rest, "logs/")
}
