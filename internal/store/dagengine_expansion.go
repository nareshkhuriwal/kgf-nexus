// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"google.golang.org/protobuf/encoding/protojson"
)

// Dynamic expansion (§5.4), the DAG engine's I/O half. The pure half -- what to
// expand into, and how a template's state follows its children -- lives in
// internal/dag; this file only makes one expansion durable, atomically, at most
// once.

// ExpansionChild is one child node an expansion materializes.
type ExpansionChild struct {
	NodeID   string
	MapIndex int32
	Spec     *nexusv1.TaskSpec
	// Edges are the child's INCOMING edges (inherited from the template).
	Edges []DEEdgeRow
}

// ExpansionWrite is one expansion to commit.
type ExpansionWrite struct {
	RunID           string
	TemplateNodeID  string
	TemplateTaskRef string
	TriggerRule     string
	MaxAttempts     int32
	SourceNodeID    string
	SourceOutput    string
	SourceAttemptID *string
	MaxFanout       int32
	// Children is empty both for an empty collection (Outcome EXPANDED) and for a
	// refused one (Outcome FAILED).
	Children []ExpansionChild
	// Outcome is "EXPANDED" or "FAILED"; Reason/Detail explain a FAILED one.
	Outcome string
	Reason  string
	Detail  string
}

// ExpansionOutcome* are the task_expansions.outcome values.
const (
	ExpansionOutcomeExpanded = "EXPANDED"
	ExpansionOutcomeFailed   = "FAILED"
)

// DEExpansionRow mirrors a task_expansions row.
type DEExpansionRow struct {
	RunID           string
	TemplateNodeID  string
	SourceNodeID    string
	SourceOutput    string
	SourceAttemptID *string
	ItemCount       int32
	MaxFanout       int32
	Outcome         string
	Reason          *string
	Detail          *string
	CreatedAt       time.Time
}

// ApplyExpansion commits one expansion in ONE transaction:
//
//  1. CAS the template PENDING -> RUNNING (children to run), SUCCEEDED (an empty
//     collection: nothing to wait for) or FAILED (a refused expansion);
//  2. INSERT the task_expansions record;
//  3. INSERT every child task_nodes row and every inherited task_edges row.
//
// applied is false when another reconciler already expanded (or settled) this
// template. That is the expected outcome under concurrency and on replay, not an
// error: the whole transaction -- children included -- rolls back, so a lost
// race can never leave a second copy of any child. The CAS is the primary guard;
// the record's primary key is the backstop, exactly as the READY CAS and
// UNIQUE(run_id,node_id,attempt_number) are for attempt creation.
//
// Children are inserted with one set-based statement per table rather than one
// round trip per child, so a max_fanout-sized expansion is a handful of
// statements, not ten thousand.
func (s *DAGEngineStore) ApplyExpansion(ctx context.Context, w ExpansionWrite) (applied bool, err error) {
	target := domain.NodeRunning
	switch {
	case w.Outcome == ExpansionOutcomeFailed:
		target = domain.NodeFailed
	case len(w.Children) == 0:
		target = domain.NodeSucceeded
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("begin expansion tx for %s/%s: %w", w.RunID, w.TemplateNodeID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE task_nodes SET state = $3::node_state
		 WHERE run_id = $1::uuid AND node_id = $2 AND state = 'PENDING'`,
		w.RunID, w.TemplateNodeID, string(target))
	if err != nil {
		return false, fmt.Errorf("cas template %s/%s to %s: %w", w.RunID, w.TemplateNodeID, target, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	var reason, detail *string
	if w.Reason != "" {
		reason = &w.Reason
	}
	if w.Detail != "" {
		detail = &w.Detail
	}
	tag, err = tx.Exec(ctx, `
		INSERT INTO task_expansions
		  (run_id, template_node_id, source_node_id, source_output, source_attempt_id,
		   item_count, max_fanout, outcome, reason, detail)
		VALUES ($1::uuid, $2, $3, $4, $5::uuid, $6, $7, $8, $9, $10)
		ON CONFLICT (run_id, template_node_id) DO NOTHING`,
		w.RunID, w.TemplateNodeID, w.SourceNodeID, w.SourceOutput, w.SourceAttemptID,
		len(w.Children), w.MaxFanout, w.Outcome, reason, detail)
	if err != nil {
		return false, fmt.Errorf("record expansion %s/%s: %w", w.RunID, w.TemplateNodeID, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	if len(w.Children) > 0 {
		ids := make([]string, len(w.Children))
		idx := make([]int32, len(w.Children))
		specs := make([]string, len(w.Children))
		var eFrom, eTo, eMap []string
		for i, c := range w.Children {
			raw, merr := protojson.MarshalOptions{UseProtoNames: true}.Marshal(c.Spec)
			if merr != nil {
				return false, fmt.Errorf("marshal spec for child %s: %w", c.NodeID, merr)
			}
			ids[i], idx[i], specs[i] = c.NodeID, c.MapIndex, string(raw)
			for _, e := range c.Edges {
				m := e.OutputToInput
				if m == nil {
					m = map[string]string{}
				}
				mb, merr := json.Marshal(m)
				if merr != nil {
					return false, fmt.Errorf("marshal edge for child %s: %w", c.NodeID, merr)
				}
				eFrom, eTo, eMap = append(eFrom, e.From), append(eTo, c.NodeID), append(eMap, string(mb))
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO task_nodes
			  (run_id, node_id, task_ref, state, trigger_rule, max_attempts,
			   attempts_used, parent_node_id, map_index, spec_json)
			SELECT $1::uuid, c.node_id, $2, 'PENDING'::node_state, $3, $4, 0, $5, c.map_index, c.spec::jsonb
			  FROM unnest($6::text[], $7::int[], $8::text[]) AS c(node_id, map_index, spec)
			ON CONFLICT (run_id, node_id) DO NOTHING`,
			w.RunID, w.TemplateTaskRef, w.TriggerRule, w.MaxAttempts, w.TemplateNodeID,
			ids, idx, specs); err != nil {
			return false, fmt.Errorf("insert %d children of %s/%s: %w", len(ids), w.RunID, w.TemplateNodeID, err)
		}
		if len(eFrom) > 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO task_edges (run_id, from_node_id, to_node_id, output_to_input)
				SELECT $1::uuid, e.f, e.t, e.m::jsonb
				  FROM unnest($2::text[], $3::text[], $4::text[]) AS e(f, t, m)
				ON CONFLICT (run_id, from_node_id, to_node_id) DO NOTHING`,
				w.RunID, eFrom, eTo, eMap); err != nil {
				return false, fmt.Errorf("insert child edges of %s/%s: %w", w.RunID, w.TemplateNodeID, err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit expansion %s/%s: %w", w.RunID, w.TemplateNodeID, err)
	}
	return true, nil
}

// LoadExpansions reads every expansion record of a run, for the run-detail view
// and for tests. The engine itself never needs it to decide anything: the
// template's node state and its children's rows are the state; this is history.
func (s *DAGEngineStore) LoadExpansions(ctx context.Context, runID string) ([]DEExpansionRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT run_id::text, template_node_id, source_node_id, source_output,
		       source_attempt_id::text, item_count, max_fanout, outcome, reason, detail, created_at
		  FROM task_expansions WHERE run_id = $1::uuid ORDER BY template_node_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("load expansions for run %s: %w", runID, err)
	}
	defer rows.Close()
	var out []DEExpansionRow
	for rows.Next() {
		var r DEExpansionRow
		if err := rows.Scan(&r.RunID, &r.TemplateNodeID, &r.SourceNodeID, &r.SourceOutput,
			&r.SourceAttemptID, &r.ItemCount, &r.MaxFanout, &r.Outcome, &r.Reason, &r.Detail,
			&r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan expansion row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expansion rows: %w", err)
	}
	return out, nil
}

// StreamChildOutputs feeds a template's children's values for one output to fn,
// in map_index order, ONE ROW AT A TIME: a child's value is its latest SUCCEEDED
// attempt's output (nil when it has none). fn returning an error stops the scan
// and closes the result set, so a fan-in that hits its size limit stops reading
// rows too -- the rest are never transferred.
func (s *DAGEngineStore) StreamChildOutputs(
	ctx context.Context, runID, templateID, output string, fn func(mapIndex int, ref []byte) error,
) (total int, err error) {
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM task_nodes WHERE run_id = $1::uuid AND parent_node_id = $2`,
		runID, templateID).Scan(&total); err != nil {
		return 0, fmt.Errorf("count children of %s/%s: %w", runID, templateID, err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT n.map_index,
		       (SELECT a.output_ref -> $3 FROM task_attempts a
		         WHERE a.run_id = n.run_id AND a.node_id = n.node_id AND a.state = 'SUCCEEDED'
		         ORDER BY a.attempt_number DESC LIMIT 1)
		  FROM task_nodes n
		 WHERE n.run_id = $1::uuid AND n.parent_node_id = $2
		 ORDER BY n.map_index`, runID, templateID, output)
	if err != nil {
		return total, fmt.Errorf("stream children of %s/%s: %w", runID, templateID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var idx int
		var ref []byte
		if err := rows.Scan(&idx, &ref); err != nil {
			return total, fmt.Errorf("scan child output: %w", err)
		}
		if err := fn(idx, ref); err != nil {
			return total, err
		}
	}
	if err := rows.Err(); err != nil {
		return total, fmt.Errorf("iterate child outputs: %w", err)
	}
	return total, nil
}

// FanInRefusal records a fan-in the engine refused to assemble (§5.4).
type FanInRefusal struct {
	NodeID              string
	TemplateNodeID      string
	TemplateOutput      string
	LimitBytes          int64
	ChildrenTotal       int
	ChildrenBeforeLimit int
	Reason              string
	Detail              string
}
