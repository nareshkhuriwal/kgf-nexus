//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package artifactgc

// Log-object collection (ADR-0020) against a REAL Postgres and a REAL MinIO.
//
//	NEXUS_TEST_DATABASE_URL=... NEXUS_TEST_S3_ENDPOINT=127.0.0.1:9000 \
//	NEXUS_S3_ACCESS_KEY_ID=... NEXUS_S3_SECRET_ACCESS_KEY=... \
//	  go test -tags integration -run Log ./internal/artifactgc/
//
// The database side runs in a rolled-back transaction like every test in this
// package; the object side uses a throwaway bucket that is emptied and removed.

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
)

func logTestStore(t *testing.T) (*MinIOStore, *minio.Client) {
	t.Helper()
	endpoint := os.Getenv("NEXUS_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("skipping integration test: NEXUS_TEST_S3_ENDPOINT is not set")
	}
	access, secret := os.Getenv("NEXUS_S3_ACCESS_KEY_ID"), os.Getenv("NEXUS_S3_SECRET_ACCESS_KEY")
	mc, err := minio.New(stripScheme(endpoint), &minio.Options{Creds: credentials.NewStaticV4(access, secret, "")})
	if err != nil {
		t.Fatalf("minio client: %v", err)
	}
	ctx := context.Background()
	bucket := "gc-logs-" + uuid.NewString()[:8]
	if err := mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Skipf("skipping integration test: MinIO unreachable: %v", err)
	}
	t.Cleanup(func() {
		for o := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			_ = mc.RemoveObject(ctx, bucket, o.Key, minio.RemoveObjectOptions{})
		}
		_ = mc.RemoveBucket(ctx, bucket)
	})
	st, err := NewMinIOStore(ctx, MinIOConfig{Endpoint: endpoint, Bucket: bucket, AccessKeyID: access, SecretAccessKey: secret})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return st, mc
}

func putObj(t *testing.T, mc *minio.Client, bucket, key string) {
	t.Helper()
	b := []byte(`{"n":1,"ts":1,"s":"stdout","m":"x"}` + "\n")
	if _, err := mc.PutObject(context.Background(), bucket, key, bytes.NewReader(b), int64(len(b)), minio.PutObjectOptions{}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// seedLogAttempt makes an attempt in ns and returns (runID, attemptID).
func seedLogAttempt(t *testing.T, tx pgx.Tx, ns string) (string, string) {
	t.Helper()
	ctx := context.Background()
	testdb.RegisterTenant(t, tx, ns)
	var specID, runID, attemptID string
	if err := tx.QueryRow(ctx, `INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1, 'gc-log-'||gen_random_uuid()::text, 1, 'h', '{}'::jsonb, 't') RETURNING id::text`, ns).Scan(&specID); err != nil {
		t.Fatalf("spec: %v", err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO workflow_runs (spec_id, namespace, trigger_kind)
		VALUES ($1::uuid, $2, 'test') RETURNING id::text`, specID, ns).Scan(&runID); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_nodes (run_id, node_id, task_ref, spec_json) VALUES ($1::uuid, 'n1', 'r', '{}')`, runID); err != nil {
		t.Fatalf("node: %v", err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO task_attempts (run_id, node_id, attempt_number, resource_request, idempotency_key)
		VALUES ($1::uuid, 'n1', 1, '{}', 'i-'||gen_random_uuid()::text) RETURNING id::text`, runID).Scan(&attemptID); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	return runID, attemptID
}

func indexLog(t *testing.T, tx pgx.Tx, ns, attemptID string, compacted bool) (chunkKey, compactedKey string) {
	t.Helper()
	ctx := context.Background()
	prefix := ns + "/logs/" + attemptID + "/"
	chunkKey = prefix + "00000000.ndjson"
	var ck *string
	if compacted {
		compactedKey = prefix + "log.ndjson"
		ck = &compactedKey
	}
	if _, err := tx.Exec(ctx, `INSERT INTO attempt_logs (attempt_id, namespace, state, line_count, chunk_count, compacted_key, finalized_at)
		VALUES ($1::uuid, $2, 'complete', 1, 1, $3, now())`, attemptID, ns, ck); err != nil {
		t.Fatalf("attempt_logs: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO attempt_log_chunks (attempt_id, seq, object_key, first_line, line_count, byte_offset, size_bytes, sha256)
		VALUES ($1::uuid, 0, $2, 1, 1, 0, 1, repeat('0', 64))`, attemptID, chunkKey); err != nil {
		t.Fatalf("attempt_log_chunks: %v", err)
	}
	return chunkKey, compactedKey
}

func listKeys(t *testing.T, mc *minio.Client, bucket string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for o := range mc.ListObjects(context.Background(), bucket, minio.ListObjectsOptions{Recursive: true}) {
		if o.Err != nil {
			t.Fatalf("list: %v", o.Err)
		}
		out[o.Key] = true
	}
	return out
}

// TestSweepCollectsOnlyOrphanedLogObjects is ADR-0020's retention claim, end to
// end through a DELETING pass:
//
//   - an indexed, uncompacted chunk is KEPT (it is the log);
//   - a compacted log's compacted object is KEPT and its chunk is COLLECTED;
//   - an object a fenced (stale) attempt uploaded but could not index is
//     COLLECTED;
//   - a deleted run's log becomes an orphan and is COLLECTED;
//   - with -log-retention, a log older than the window is COLLECTED even though
//     its index rows remain, and without it the same log is KEPT.
func TestSweepCollectsOnlyOrphanedLogObjects(t *testing.T) {
	pool := testPool(t)
	store, mc := logTestStore(t)
	bucket := store.Bucket()
	ns := "gclog-" + uuid.NewString()[:8]

	inRollbackTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		_, live := seedLogAttempt(t, tx, ns)
		liveChunk, _ := indexLog(t, tx, ns, live, false)

		_, comp := seedLogAttempt(t, tx, ns)
		compChunk, compKey := indexLog(t, tx, ns, comp, true)

		_, stale := seedLogAttempt(t, tx, ns)
		staleKey := ns + "/logs/" + stale + "/00000000.ndjson" // uploaded, never indexed

		deletedRun, gone := seedLogAttempt(t, tx, ns)
		goneChunk, _ := indexLog(t, tx, ns, gone, false)

		_, old := seedLogAttempt(t, tx, ns)
		oldChunk, oldCompacted := indexLog(t, tx, ns, old, true)
		if _, err := tx.Exec(ctx, `UPDATE attempt_logs SET finalized_at = now() - interval '40 days', created_at = now() - interval '41 days' WHERE attempt_id = $1::uuid`, old); err != nil {
			t.Fatalf("age log: %v", err)
		}

		for _, k := range []string{liveChunk, compChunk, compKey, staleKey, goneChunk, oldChunk, oldCompacted} {
			putObj(t, mc, bucket, k)
		}
		// Deleting the run cascades through task_attempts to the log index.
		if _, err := tx.Exec(ctx, `DELETE FROM workflow_runs WHERE id = $1::uuid`, deletedRun); err != nil {
			t.Fatalf("delete run: %v", err)
		}

		sweep := func(retention time.Duration) Stats {
			sw := NewSweeper(tx, store, discardLogger(), Config{DryRun: false, Prefix: ns + "/", LogRetention: retention})
			// Everything was just written; move the clock past the grace period
			// rather than waiting a day.
			sw.now = func() time.Time { return time.Now().Add(DefaultGracePeriod + time.Hour) }
			st, err := sw.Once(ctx)
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			return st
		}

		st := sweep(0)
		if st.LogObjects != 7 {
			t.Errorf("log objects examined = %d, want 7", st.LogObjects)
		}
		left := listKeys(t, mc, bucket)
		for _, k := range []string{liveChunk, compKey, oldCompacted} {
			if !left[k] {
				t.Errorf("live log object %s was DELETED", k)
			}
		}
		for _, k := range []string{compChunk, oldChunk, staleKey, goneChunk} {
			if left[k] {
				t.Errorf("orphaned log object %s was kept", k)
			}
		}
		if !left[oldCompacted] {
			t.Fatal("without retention an old log must be kept")
		}

		sweep(720 * time.Hour)
		left = listKeys(t, mc, bucket)
		if left[oldCompacted] {
			t.Error("a log older than -log-retention was kept")
		}
		if !left[liveChunk] || !left[compKey] {
			t.Error("retention collected a log inside the window")
		}
		if !strings.HasPrefix(liveChunk, ns+"/logs/") {
			t.Fatalf("key shape changed: %s", liveChunk)
		}
	})
}

// TestLogRefSourcesAreReported: the operator-facing list of reference sources
// must name the log index, or a dry-run report would not explain why log
// objects are kept.
func TestLogRefSourcesAreReported(t *testing.T) {
	joined := strings.Join(RefSources(), ",")
	for _, s := range []string{"attempt_logs.compacted_key", "attempt_log_chunks.object_key"} {
		if !strings.Contains(joined, s) {
			t.Errorf("RefSources() = %s; missing %s", joined, s)
		}
	}
}
