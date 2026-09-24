// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
)

// QueueBacklog is the PENDING backlog of one queue.
type QueueBacklog struct {
	Queue string
	// Pending counts attempts in PENDING on the queue.
	Pending int64
	// OldestAgeSeconds is how long the oldest PENDING attempt has been
	// schedulable (now - scheduled_at), clamped at zero for a retry whose
	// backoff has not elapsed yet.
	OldestAgeSeconds float64
}

// PendingBacklog reports the PENDING backlog per queue, largest first, for at
// most limit queues.
//
// Every queue is reported, not only the one this replica drains: a queue with
// NO scheduler replica is the failure this number exists to expose (its
// attempts wait forever and look exactly like a capacity shortage), and only a
// fleet-wide view shows it. Every replica therefore reports the same values;
// aggregate with max by (queue), not sum.
//
// It is a read of the partial PENDING index (idx_attempts_dispatch, migration
// 0001) and is sampled on a coarse interval, never per scrape.
func (s *SchedulerStore) PendingBacklog(ctx context.Context, limit int) ([]QueueBacklog, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
SELECT queue,
       count(*),
       GREATEST(EXTRACT(EPOCH FROM (now() - min(scheduled_at))), 0)::float8
  FROM task_attempts
 WHERE state = 'PENDING'
 GROUP BY queue
 ORDER BY count(*) DESC, queue
 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("pending backlog: %w", err)
	}
	defer rows.Close()
	var out []QueueBacklog
	for rows.Next() {
		var b QueueBacklog
		if err := rows.Scan(&b.Queue, &b.Pending, &b.OldestAgeSeconds); err != nil {
			return nil, fmt.Errorf("pending backlog: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
