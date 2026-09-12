//go:build integration

// Integration tests for the artifact GC's reachability derivation.
//
// These cover the half the unit tests deliberately do not: the SQL that turns
// live rows into a reference set. A bug there deletes data no fake can catch, so
// it is exercised against a real Postgres with real row shapes.
//
// Run with:
//
//	NEXUS_POSTGRES_DSN='postgres://nexus:nexus-dev-password@localhost:55433/nexus?sslmode=disable' \
//	  go test -tags integration -count=1 ./internal/artifactgc/...
//
// They SKIP when no DSN is reachable rather than fail, matching
// agent/internal/resultstore/integration_test.go.
//
// EVERY test here works inside a transaction that is ROLLED BACK. Nothing writes
// to the live database, and nothing touches the object store at all.
package artifactgc

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func dsn() string {
	for _, k := range []string{"NEXUS_POSTGRES_DSN", "NEXUS_DATABASE_URL", "DATABASE_URL", "NEXUS_DSN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	d := dsn()
	if d == "" {
		t.Skip("no Postgres DSN set (NEXUS_POSTGRES_DSN); skipping")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, d)
	if err != nil {
		t.Skipf("cannot open a pool (%v); skipping", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("Postgres unreachable at %s (%v); skipping", d, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// inRollbackTx runs fn against a transaction that is always rolled back, so an
// integration test can INSERT real rows into the live schema without leaving any.
func inRollbackTx(t *testing.T, pool *pgxpool.Pool, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && err != pgx.ErrTxClosed {
			t.Errorf("rollback: %v", err)
		}
	}()
	fn(tx)
}

// seedAttempt inserts a full run/spec/node/attempt chain and returns the attempt
// id. The chain is needed because task_attempts has foreign keys to task_nodes
// and workflow_runs; inserting a bare attempt is impossible, and a test that
// worked around that with its own table would not be testing the real query.
func seedAttempt(t *testing.T, tx pgx.Tx, outputRef, inputRefs string) {
	t.Helper()
	ctx := context.Background()
	var specID, runID string
	err := tx.QueryRow(ctx, `
		INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ('artifactgc-test', 'gc-test-'||gen_random_uuid()::text, 1,
		        'hash-'||gen_random_uuid()::text, '{}'::jsonb, 'test')
		RETURNING id::text`).Scan(&specID)
	if err != nil {
		t.Fatalf("insert spec: %v", err)
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO workflow_runs (spec_id, namespace, trigger_kind)
		VALUES ($1::uuid, 'artifactgc-test', 'test') RETURNING id::text`, specID).Scan(&runID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_nodes (run_id, node_id, task_ref, spec_json)
		VALUES ($1::uuid, 'n1', 'ref', '{}'::jsonb)`, runID); err != nil {
		t.Fatalf("insert node: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_attempts
		  (run_id, node_id, attempt_number, resource_request, idempotency_key, output_ref, input_refs)
		VALUES ($1::uuid, 'n1', 1, '{}'::jsonb, 'idem-'||gen_random_uuid()::text,
		        NULLIF($2,'')::jsonb, COALESCE(NULLIF($3,''),'{}')::jsonb)`,
		runID, outputRef, inputRefs); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}
}

func hasKey(set map[string]struct{}, k string) bool { _, ok := set[k]; return ok }

// TestLiveKeysFindsAnOutputRef is the base case, against the real row shape the
// restore drill documents.
func TestLiveKeysFindsAnOutputRef(t *testing.T) {
	pool := testPool(t)
	inRollbackTx(t, pool, func(tx pgx.Tx) {
		seedAttempt(t, tx, `{"result":{"blob":{"key":"artifactgc-test/aaa","bucket":"nexus-results","sha256":"aaa"},"size_bytes":"300000"}}`, "")
		live, err := LiveKeys(context.Background(), tx)
		if err != nil {
			t.Fatalf("LiveKeys: %v", err)
		}
		if !hasKey(live, "artifactgc-test/aaa") {
			t.Fatalf("an output_ref blob key was NOT found in the reference set; "+
				"that object would be deleted as an orphan. set had %d keys", len(live))
		}
	})
}

// TestLiveKeysFindsAnInputRef: the second reference source. An object can be
// named ONLY by a downstream attempt's input_refs once the producing run is
// pruned, and sweeping output_ref alone would collect it.
func TestLiveKeysFindsAnInputRef(t *testing.T) {
	pool := testPool(t)
	inRollbackTx(t, pool, func(tx pgx.Tx) {
		seedAttempt(t, tx, "", `{"features":{"blob":{"key":"artifactgc-test/from-input","bucket":"nexus-results","sha256":"bbb"}}}`)
		live, err := LiveKeys(context.Background(), tx)
		if err != nil {
			t.Fatalf("LiveKeys: %v", err)
		}
		if !hasKey(live, "artifactgc-test/from-input") {
			t.Fatal("an input_refs blob key was NOT found; a downstream attempt's input " +
				"would be collected once its producing run was pruned")
		}
	})
}

// TestLiveKeysFindsRefsUnderAnyOutputName: the output name is chosen by the
// workflow author. A fixed-path extractor keyed on "result" would miss these and
// report their objects as orphans. This is the SQL twin of the ExtractKeys unit
// test.
func TestLiveKeysFindsRefsUnderAnyOutputName(t *testing.T) {
	pool := testPool(t)
	inRollbackTx(t, pool, func(tx pgx.Tx) {
		seedAttempt(t, tx, `{"out":{"blob":{"key":"artifactgc-test/named-out","sha256":"c"}}}`, "")
		seedAttempt(t, tx, `{"some_custom_name":{"blob":{"key":"artifactgc-test/named-custom","sha256":"d"}}}`, "")
		live, err := LiveKeys(context.Background(), tx)
		if err != nil {
			t.Fatalf("LiveKeys: %v", err)
		}
		for _, k := range []string{"artifactgc-test/named-out", "artifactgc-test/named-custom"} {
			if !hasKey(live, k) {
				t.Errorf("%s was not found; a reference under a non-\"result\" output name "+
					"was missed and its object would be deleted", k)
			}
		}
	})
}

// TestLiveKeysFindsEveryRefInOneInputRefsDocument: input_refs holds one entry per
// resolved input, and missing any one of them deletes a live object.
func TestLiveKeysFindsEveryRefInOneInputRefsDocument(t *testing.T) {
	pool := testPool(t)
	inRollbackTx(t, pool, func(tx pgx.Tx) {
		seedAttempt(t, tx, "", `{
			"features":{"blob":{"key":"artifactgc-test/multi-1","sha256":"1"}},
			"labels":{"blob":{"key":"artifactgc-test/multi-2","sha256":"2"}},
			"cfg":{"inline":"e30="}
		}`)
		live, err := LiveKeys(context.Background(), tx)
		if err != nil {
			t.Fatalf("LiveKeys: %v", err)
		}
		for _, k := range []string{"artifactgc-test/multi-1", "artifactgc-test/multi-2"} {
			if !hasKey(live, k) {
				t.Errorf("%s missing from the reference set", k)
			}
		}
	})
}

// TestLiveKeysIgnoresInlineResults: an inline result names no object, so it must
// contribute nothing to the reference set (and must not error).
func TestLiveKeysIgnoresInlineResults(t *testing.T) {
	pool := testPool(t)
	inRollbackTx(t, pool, func(tx pgx.Tx) {
		before, err := LiveKeys(context.Background(), tx)
		if err != nil {
			t.Fatalf("LiveKeys: %v", err)
		}
		seedAttempt(t, tx, `{"result":{"inline":"eyJyb3dzIjo0Mn0=","size_bytes":"11"}}`, "")
		after, err := LiveKeys(context.Background(), tx)
		if err != nil {
			t.Fatalf("LiveKeys: %v", err)
		}
		if len(after) != len(before) {
			t.Fatalf("an inline result changed the reference set size (%d -> %d)", len(before), len(after))
		}
	})
}

// TestLiveKeysIgnoresTheLiteralWordBlob: the false positive that a substring
// search produced against this very database. Workflows named "blob-threshold-test"
// put the word "blob" into workflow_specs.spec_json and idempotency_keys.response
// without putting a BlobRef anywhere.
func TestLiveKeysIgnoresTheLiteralWordBlob(t *testing.T) {
	pool := testPool(t)
	inRollbackTx(t, pool, func(tx pgx.Tx) {
		before, err := LiveKeys(context.Background(), tx)
		if err != nil {
			t.Fatalf("LiveKeys: %v", err)
		}
		seedAttempt(t, tx, `{"result":{"inline":"","content_type":"must land as a blob, not inline"}}`, "")
		after, err := LiveKeys(context.Background(), tx)
		if err != nil {
			t.Fatalf("LiveKeys: %v", err)
		}
		if len(after) != len(before) {
			t.Fatalf("the literal word \"blob\" in free text was counted as a reference")
		}
	})
}

// TestLiveKeysMatchesExtractKeys: the SQL extractor and the Go extractor must
// agree on shape. They are used in different places (the sweep, and an operator
// inspecting one row), and a divergence between them is a silent correctness gap.
func TestLiveKeysMatchesExtractKeys(t *testing.T) {
	pool := testPool(t)
	docs := []string{
		`{"result":{"blob":{"key":"artifactgc-test/agree-1","sha256":"a"}}}`,
		`{"out":{"blob":{"key":"artifactgc-test/agree-2","sha256":"b"}}}`,
		`{"a":{"blob":{"key":"artifactgc-test/agree-3","sha256":"c"}},"b":{"inline":"e30="}}`,
	}
	inRollbackTx(t, pool, func(tx pgx.Tx) {
		before, err := LiveKeys(context.Background(), tx)
		if err != nil {
			t.Fatalf("LiveKeys: %v", err)
		}
		var wantFromGo []string
		for _, d := range docs {
			seedAttempt(t, tx, d, "")
			keys, err := ExtractKeys([]byte(d))
			if err != nil {
				t.Fatalf("ExtractKeys(%s): %v", d, err)
			}
			wantFromGo = append(wantFromGo, keys...)
		}
		after, err := LiveKeys(context.Background(), tx)
		if err != nil {
			t.Fatalf("LiveKeys: %v", err)
		}
		var addedBySQL []string
		for k := range after {
			if _, existed := before[k]; !existed {
				addedBySQL = append(addedBySQL, k)
			}
		}
		sort.Strings(addedBySQL)
		sort.Strings(wantFromGo)
		if len(addedBySQL) != len(wantFromGo) {
			t.Fatalf("SQL found %v, Go's ExtractKeys found %v; the two extractors disagree",
				addedBySQL, wantFromGo)
		}
		for i := range addedBySQL {
			if addedBySQL[i] != wantFromGo[i] {
				t.Fatalf("SQL found %v, Go found %v", addedBySQL, wantFromGo)
			}
		}
	})
}

// TestRefSourcesCoverEveryBlobBearingColumn is the guard against a future
// migration adding a column that can hold a ResultRef without anyone updating
// refSourceColumns.
//
// It scans EVERY jsonb column in the live schema structurally, for an object
// under a "blob" key that has a "key" member — a real BlobRef, not the substring
// "blob". Any column that holds one and is not a declared reference source is a
// column whose objects the sweep would delete.
//
// Its one honest limitation: it can only see columns that CURRENTLY hold such a
// value. A newly added column that is still empty passes. That is why
// refSourceColumns is derived from the writers and the schema in the first place,
// and this test is the backstop, not the primary argument.
func TestRefSourcesCoverEveryBlobBearingColumn(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT table_name, column_name
		  FROM information_schema.columns
		 WHERE table_schema = 'public' AND data_type = 'jsonb'
		 ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatalf("list jsonb columns: %v", err)
	}
	type col struct{ table, column string }
	var cols []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.table, &c.column); err != nil {
			rows.Close()
			t.Fatalf("scan column: %v", err)
		}
		cols = append(cols, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}
	if len(cols) == 0 {
		t.Fatal("no jsonb columns found; the scan is not doing what it claims")
	}

	declared := map[string]bool{}
	for _, s := range refSourceColumns {
		declared[s.Table+"."+s.Column] = true
	}

	var undeclared []string
	for _, c := range cols {
		var n int
		q := "SELECT count(*) FROM " + c.table +
			" WHERE " + c.column + " IS NOT NULL AND jsonb_path_exists(" + c.column +
			", 'strict $.**.blob.key')"
		if err := pool.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatalf("scan %s.%s: %v", c.table, c.column, err)
		}
		if n > 0 && !declared[c.table+"."+c.column] {
			undeclared = append(undeclared, c.table+"."+c.column)
		}
	}
	if len(undeclared) > 0 {
		t.Fatalf("these columns hold BlobRefs but are NOT declared reference sources in "+
			"refSourceColumns: %v. The sweep would treat their objects as orphans and "+
			"delete live data. Add them to refSourceColumns.", undeclared)
	}
	t.Logf("scanned %d jsonb columns; declared reference sources %v cover every "+
		"blob-bearing column", len(cols), RefSources())
}

// TestDeclaredRefSourcesActuallyExist: the reverse direction. A typo'd or dropped
// column would make LiveKeys error on every pass, so it is caught here rather
// than in production, where the sweep would simply abort.
func TestDeclaredRefSourcesActuallyExist(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	for _, s := range refSourceColumns {
		var n int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			 WHERE table_schema='public' AND table_name=$1 AND column_name=$2
			   AND data_type='jsonb'`, s.Table, s.Column).Scan(&n); err != nil {
			t.Fatalf("look up %s: %v", s, err)
		}
		if n != 1 {
			t.Errorf("declared reference source %s does not exist as a jsonb column; "+
				"LiveKeys would fail on every pass", s)
		}
	}
}

// TestLiveKeysAgainstTheRealDatabase runs the derivation over the whole live
// database, unfiltered. It asserts only that it succeeds and reports what it
// found: the point is that the query is valid against real data at real scale,
// which a transaction-scoped test cannot show.
func TestLiveKeysAgainstTheRealDatabase(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	live, err := LiveKeys(ctx, pool)
	if err != nil {
		t.Fatalf("LiveKeys against the live database: %v", err)
	}
	t.Logf("live reference set: %d distinct object keys from %v", len(live), RefSources())
}
