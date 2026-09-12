// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package artifactgc implements the reachability-keyed garbage-collection sweep
// over the result store that ADR-0008 mandates and nobody had written.
//
// ADR-0008 §Consequences states the requirement exactly:
//
//	"Orphaned objects accumulate when runs are pruned. Needs a garbage-collection
//	 sweep keyed on reachability from live `output_ref`s — deliberately a
//	 background reconciler, never an inline delete, because a bug in an inline
//	 delete path destroys data."
//
// Two phrases in that sentence are the whole design.
//
// "Keyed on reachability" means an object is deletable only when NO live
// database row references it. The reference set is DERIVED FROM THE DATABASE, by
// query, never inferred from a naming convention, a age heuristic, or a
// bucket-side lifecycle rule. See LiveKeys.
//
// "Never an inline delete" means nothing on the task path ever calls into this
// package. It is a sweep an operator runs, or (later) a loop that is DISABLED by
// default. Deleting is opt-in twice over: Config.DryRun defaults to true, and the
// command requires an explicit flag to turn it off.
//
// The hazard this package exists to survive is that the two stores cannot be read
// atomically — the same skew deploy/backup-artifacts.sh reasons about at length
// for the backup case. See the comment on Sweeper.Once for why the ordering here
// is list-then-query rather than query-then-list, and why it is the safe one.
package artifactgc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Querier is the subset of pgxpool.Pool this package needs. Narrow on purpose:
// the sweep is READ-ONLY against Postgres — it never writes a row, so it cannot
// corrupt the metadata store even if it is wrong. Everything it can get wrong is
// on the object-store side, and that side is gated by the dry-run default and the
// grace period.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// refSourceColumns are EVERY (table, column) pair in the schema that can hold a
// reference to a result-store object.
//
// Getting this list wrong in the omitting direction deletes live data, so it is
// not a guess. It was derived three ways, and all three agree:
//
//  1. Schema read. Every JSONB/JSON column in services/migrations/*.up.sql was
//     enumerated and classified. Only task_attempts carries ResultRef-shaped
//     values; workflow_specs.spec_json, task_nodes.spec_json, workflow_runs.params,
//     schedules.params, run_events.payload, timers.payload, idempotency_keys.response,
//     audit_log.detail, schedule_backfills.detail, task_edges.output_to_input,
//     workers.capacity/committed/labels, reservations.resources and
//     task_attempts.resource_request/error/metrics hold specs, params, resource
//     vectors or projections — none of them a BlobRef.
//
//  2. Writer read. The only producer of a blob key is
//     agent/internal/resultstore.Client.Put, whose ResultRef reaches Postgres by
//     exactly two paths: the worker's report writes it to task_attempts.output_ref
//     (internal/store/scheduler_reports.go), and the DAG engine COPIES upstream
//     output_ref values verbatim into the downstream attempt's
//     task_attempts.input_refs (services/dag-engine/engine.go resolveInputs).
//     input_refs is therefore a genuine second reference source, not a derived
//     view: the upstream attempt's row can be deleted (ON DELETE CASCADE from a
//     pruned run) while a downstream attempt row in another run still names the
//     same object. Sweeping output_ref alone would delete it.
//
//  3. Structural scan of live data. A recursive jsonb_path_exists for
//     'strict $.**.blob.key' was run against every JSONB column of the running
//     dev database. Only task_attempts.output_ref matched. Note that a substring
//     search for "blob" ALSO matched workflow_specs.spec_json and
//     idempotency_keys.response — both false positives, from workflows literally
//     named "blob-threshold-test". That is precisely why this list is built from
//     the writers and the schema, and merely CONFIRMED by the data: a data-only
//     derivation on a quiet database would have concluded that input_refs never
//     holds a blob, which is false by construction and only accidentally true of
//     this snapshot.
//
// If a future migration adds a column that can hold a ResultRef, it MUST be added
// here. There is an integration test (TestRefSourcesCoverEveryBlobBearingColumn)
// that scans the live schema structurally and fails when a blob-bearing column is
// not in this list, so the omission is caught rather than silently tolerated.
var refSourceColumns = []refSource{
	{Table: "task_attempts", Column: "output_ref"},
	{Table: "task_attempts", Column: "input_refs"},
}

type refSource struct {
	Table  string
	Column string
}

func (r refSource) String() string { return r.Table + "." + r.Column }

// RefSources reports the (table, column) pairs the sweep treats as reference
// sources, for logging and for the coverage test.
func RefSources() []string {
	out := make([]string, 0, len(refSourceColumns))
	for _, r := range refSourceColumns {
		out = append(out, r.String())
	}
	return out
}

// LiveKeys returns the set of object keys reachable from ANY live database row.
//
// The query walks each reference column recursively with jsonb_path_query rather
// than reaching into a fixed path like output_ref->'result'->'blob'->>'key'. The
// fixed path is what the restore drill uses, and it is right for a drill that
// constructs its own row — but it is WRONG here. Real rows do not all use the
// output name "result": live data in this very database contains
// {"out": {...}} and {"features": {...}} as well, because the key is the task's
// declared OUTPUT NAME, which the workflow author chooses. A fixed-path extractor
// would see no reference in an attempt whose output is named anything else and
// would report its object as an orphan. Deleting on that basis is the exact data
// loss this package exists to prevent, so the extraction is shape-driven:
// "anywhere in this document, an object under a 'blob' key that has a 'key'
// member".
//
// Note also what is NOT filtered: the query does not restrict to SUCCEEDED
// attempts, non-terminal runs, or any recency window. Reachability is reachability
// — if a row of any state names an object, that object is live. Narrowing the
// reference set is the one class of change that turns this from a space reclaimer
// into a data destroyer.
func LiveKeys(ctx context.Context, q Querier) (map[string]struct{}, error) {
	live := make(map[string]struct{})
	for _, src := range refSourceColumns {
		// The column name is interpolated, never user input: refSourceColumns is
		// a package-level constant list, so there is no injection surface. pgx
		// cannot parameterise an identifier.
		sql := fmt.Sprintf(`
			SELECT DISTINCT jsonb_path_query(%s, 'strict $.**.blob') ->> 'key' AS key
			  FROM %s
			 WHERE %s IS NOT NULL
			   AND jsonb_path_exists(%s, 'strict $.**.blob.key')`,
			src.Column, src.Table, src.Column, src.Column)

		rows, err := q.Query(ctx, sql)
		if err != nil {
			// A failure to enumerate references must NEVER be treated as "no
			// references exist". Returning the error aborts the sweep, so a
			// database blip cannot be read as "everything is an orphan".
			return nil, fmt.Errorf("artifactgc: enumerate live keys from %s: %w", src, err)
		}
		for rows.Next() {
			var key *string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return nil, fmt.Errorf("artifactgc: scan live key from %s: %w", src, err)
			}
			if key != nil && *key != "" {
				live[*key] = struct{}{}
			}
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("artifactgc: iterate live keys from %s: %w", src, err)
		}
		rows.Close()
	}
	return live, nil
}

// ExtractKeys pulls every blob key out of one ResultRef-bearing JSON document.
//
// This is the in-process twin of the SQL above, used by the unit tests so the
// reachability rule can be exercised without a database, and available to
// operators debugging a specific row. The two must agree on shape; the
// integration test pins that by running both over the same documents.
func ExtractKeys(doc []byte) ([]string, error) {
	if len(doc) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return nil, fmt.Errorf("artifactgc: parse ref document: %w", err)
	}
	var out []string
	walkForBlobs(v, &out)
	return out, nil
}

// walkForBlobs recurses looking for {"blob": {"key": "..."}} anywhere in the
// document, mirroring the 'strict $.**.blob' jsonpath.
func walkForBlobs(v any, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		if b, ok := t["blob"]; ok {
			if bm, ok := b.(map[string]any); ok {
				if k, ok := bm["key"].(string); ok && k != "" {
					*out = append(*out, k)
				}
			}
		}
		for _, child := range t {
			walkForBlobs(child, out)
		}
	case []any:
		for _, child := range t {
			walkForBlobs(child, out)
		}
	}
}

// Candidate is one object the sweep considered.
type Candidate struct {
	Key          string
	SizeBytes    int64
	LastModified time.Time
}
