package artifactgc

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// Dangling references: rows that name an object the store does not have.
//
// THE OPPOSITE DIRECTION FROM THE SWEEP, and the more serious one. An orphan is
// an object no row references: it wastes disk and nothing breaks. A DANGLING REF
// is a row whose output_ref names a key that is gone, and it does break things —
// the attempt's result is unreadable, and a downstream task that maps that output
// into its own inputs fails when it tries to fetch it. OPERATIONS §7.6 records
// that nothing reports these in the live system; this closes that.
//
// WHY THIS IS READ-ONLY, PERMANENTLY. There is no safe automatic repair. The
// object is gone: the row cannot be "fixed", only deleted or rewritten, and both
// destroy the evidence an operator needs to work out what happened. Worse, the
// most likely cause of a dangling ref is a MISCONFIGURED CHECK — the wrong
// bucket, the wrong endpoint, a restore only half finished — and a repairer
// pointed at an empty bucket would cheerfully erase every reference in the
// database. So this reports, and a human decides. The sweep's dry-run default
// exists for the same reason; here the property is structural rather than a
// default, because there is no mode in which deleting is right.
//
// The backup ordering (§7.2: objects first, then Postgres) is designed to produce
// orphans and never dangling refs, so a nonzero count here is a REAL SIGNAL, not
// routine drift: it means a restore ran out of order, an object was deleted out
// of band, or the GC sweep deleted something it should not have. Any of those is
// worth waking someone for.

// DanglingRef is one reference whose object is missing from the store.
type DanglingRef struct {
	// Where the reference lives, so an operator can look at the actual row
	// rather than searching for a key.
	Table  string
	Column string
	// RowID is the task_attempts.id the reference was found on.
	RowID string
	// RunID and NodeID name the work, which is what an operator actually thinks
	// in. A key alone does not tell anyone which run is damaged.
	RunID  string
	NodeID string
	State  string
	// Key is the object the row names and the store does not have.
	Key string
}

func (d DanglingRef) String() string {
	return fmt.Sprintf("%s.%s row=%s run=%s node=%s state=%s key=%s",
		d.Table, d.Column, d.RowID, d.RunID, d.NodeID, d.State, d.Key)
}

// ObjectChecker reports whether an object exists. Narrower than ObjectLister on
// purpose: this side of the package must not be able to delete anything, and an
// interface that cannot express a delete is the cheapest way to guarantee it.
type ObjectChecker interface {
	StatObject(ctx context.Context, key string) (bool, error)
	Bucket() string
}

// DanglingReport is the outcome of one check.
type DanglingReport struct {
	// RefsChecked is distinct KEYS looked up, not references scanned: several
	// rows legitimately name one object (an output copied into a downstream
	// attempt's input_refs), and the store is asked about each key once.
	RefsChecked int
	RowsScanned int
	Dangling    []DanglingRef
	// MissingKeys is the distinct keys behind Dangling, which is the number that
	// matters for "how much data is actually gone" — one lost object can
	// dangle from many rows.
	MissingKeys int
	Bucket      string
	Duration    time.Duration
	Truncated   bool
}

// DefaultMaxRefs bounds one check. Generous, because the cost per reference is a
// HEAD request and the failure this catches is one an operator wants completely
// enumerated rather than sampled — but bounded, so a check against a database
// with millions of attempts cannot run unboundedly.
const DefaultMaxRefs = 50000

// CheckDanglingRefs enumerates every reference in the database and reports those
// whose object is missing from the store.
//
// ORDERING, and why it is the reverse of the sweep's. The sweep lists objects
// BEFORE querying the database, so an object written between the two steps is
// simply not a candidate. Here the danger is the mirror image — reporting a
// reference as broken when the object is merely being written concurrently — so
// the database is read FIRST and the store is asked SECOND. A row committed
// before its object is durable cannot happen (resultstore.Put stores the object,
// then the report writes the row), so a reference read at T1 whose object is
// checked at T2 > T1 must already have had its object stored. The one ordering
// that would produce a false alarm is the one this avoids.
//
// A false NEGATIVE is possible and acceptable: an object deleted after the check
// is not reported until the next run. A false POSITIVE would send an operator
// hunting for data loss that never happened, which is the expensive mistake.
func CheckDanglingRefs(
	ctx context.Context, q Querier, store ObjectChecker, log *slog.Logger, maxRefs int,
) (DanglingReport, error) {
	if maxRefs <= 0 {
		maxRefs = DefaultMaxRefs
	}
	started := time.Now()
	rep := DanglingReport{Bucket: store.Bucket()}

	// key -> every row that names it. Grouped so one HEAD answers for all of
	// them, and so a missing object reports every damaged row rather than the
	// first one found.
	byKey := map[string][]DanglingRef{}

	for _, src := range refSourceColumns {
		// Identifiers come from the package-level refSourceColumns list, never
		// from input; pgx cannot parameterise an identifier.
		sql := fmt.Sprintf(`
			SELECT a.id::text, a.run_id::text, a.node_id, a.state::text,
			       jsonb_path_query(a.%s, 'strict $.**.blob') ->> 'key' AS key
			  FROM %s a
			 WHERE a.%s IS NOT NULL
			   AND jsonb_path_exists(a.%s, 'strict $.**.blob.key')`,
			src.Column, src.Table, src.Column, src.Column)

		rows, err := q.Query(ctx, sql)
		if err != nil {
			// Never degrade to "no references found": that would report a clean
			// bill of health for a database we could not read.
			return rep, fmt.Errorf("artifactgc: enumerate references from %s: %w", src, err)
		}
		for rows.Next() {
			var id, runID, nodeID, state string
			var key *string
			if err := rows.Scan(&id, &runID, &nodeID, &state, &key); err != nil {
				rows.Close()
				return rep, fmt.Errorf("artifactgc: scan reference from %s: %w", src, err)
			}
			rep.RowsScanned++
			if key == nil || *key == "" {
				continue
			}
			byKey[*key] = append(byKey[*key], DanglingRef{
				Table: src.Table, Column: src.Column,
				RowID: id, RunID: runID, NodeID: nodeID, State: state, Key: *key,
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return rep, fmt.Errorf("artifactgc: iterate references from %s: %w", src, err)
		}
		rows.Close()
	}

	// Sorted so two runs over an unchanged database produce identical output.
	// A report an operator cannot diff is a report they stop reading.
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if len(keys) > maxRefs {
		keys = keys[:maxRefs]
		rep.Truncated = true
	}

	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.RefsChecked++
		exists, err := store.StatObject(ctx, key)
		if err != nil {
			// One key's lookup failing is not evidence that the object is gone,
			// and treating it as such is how a network blip becomes a data-loss
			// report. Abort instead: a partial check reported as complete is
			// worse than no check.
			return rep, fmt.Errorf("artifactgc: stat %s: %w", key, err)
		}
		if !exists {
			rep.MissingKeys++
			rep.Dangling = append(rep.Dangling, byKey[key]...)
			if log != nil {
				for _, d := range byKey[key] {
					// WARN, not INFO: every line here is a task output that
					// cannot be read any more.
					log.Warn("dangling reference: the row names an object the store does not have",
						"table", d.Table, "column", d.Column, "row_id", d.RowID,
						"run_id", d.RunID, "node_id", d.NodeID, "key", d.Key)
				}
			}
		}
	}

	rep.Duration = time.Since(started)
	return rep, nil
}
