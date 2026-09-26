//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package artifactgc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestSweepKeepsOnlyLiveCheckpoints is ADR-0032 §4's cleanup claim, end to end
// through a DELETING pass against a real S3-compatible store (Silo):
//
//   - the checkpoint POINTER of an unfinished node is KEPT (a retry resumes
//     from it);
//   - a checkpoint the pointer moved past is COLLECTED;
//   - an upload whose registration was fenced (never pointed at) is COLLECTED;
//   - the pointer of a SUCCEEDED node is COLLECTED (the work is done);
//   - everything of a deleted run is COLLECTED (the pointer row cascades).
func TestSweepKeepsOnlyLiveCheckpoints(t *testing.T) {
	pool := testPool(t)
	store, mc := logTestStore(t)
	bucket := store.Bucket()
	ns := "gcckpt-" + uuid.NewString()[:8]

	inRollbackTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		point := func(runID, attemptID string, seq int) string {
			t.Helper()
			var idem string
			if err := tx.QueryRow(ctx, `SELECT idempotency_key FROM task_attempts WHERE id = $1::uuid`, attemptID).Scan(&idem); err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("%s/checkpoints/%s/%s/%d", ns, idem, attemptID, seq)
			if _, err := tx.Exec(ctx, `
INSERT INTO task_checkpoints (idempotency_key, run_id, node_id, attempt_id, attempt_number, lease_epoch,
                              seq, object_key, sha256, size_bytes, saved_at)
VALUES ($1, $2::uuid, 'n1', $3::uuid, 1, 1, $4, $5, repeat('0', 64), 1, now())
ON CONFLICT (idempotency_key) DO UPDATE SET seq = EXCLUDED.seq, object_key = EXCLUDED.object_key`,
				idem, runID, attemptID, seq, key); err != nil {
				t.Fatalf("pointer: %v", err)
			}
			return key
		}

		runLive, live := seedLogAttempt(t, tx, ns)
		superseded := point(runLive, live, 1)
		livePtr := point(runLive, live, 2)               // the pointer moved past seq 1
		fenced := strings.TrimSuffix(livePtr, "2") + "3" // uploaded, registration refused

		runDone, done := seedLogAttempt(t, tx, ns)
		donePtr := point(runDone, done, 1)
		if _, err := tx.Exec(ctx, `UPDATE task_nodes SET state = 'SUCCEEDED' WHERE run_id = $1::uuid`, runDone); err != nil {
			t.Fatal(err)
		}

		runGone, gone := seedLogAttempt(t, tx, ns)
		gonePtr := point(runGone, gone, 1)

		for _, k := range []string{superseded, livePtr, fenced, donePtr, gonePtr} {
			putObj(t, mc, bucket, k)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workflow_runs WHERE id = $1::uuid`, runGone); err != nil {
			t.Fatal(err)
		}

		sw := NewSweeper(tx, store, discardLogger(), Config{DryRun: false, Prefix: ns + "/"})
		sw.now = func() time.Time { return time.Now().Add(DefaultGracePeriod + time.Hour) }
		st, err := sw.Once(ctx)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if st.CheckpointObjects != 5 {
			t.Errorf("checkpoint objects examined = %d, want 5", st.CheckpointObjects)
		}
		left := listKeys(t, mc, bucket)
		if !left[livePtr] {
			t.Errorf("the live checkpoint %s was DELETED", livePtr)
		}
		for _, k := range []string{superseded, fenced, donePtr, gonePtr} {
			if left[k] {
				t.Errorf("orphaned checkpoint %s was kept", k)
			}
		}
	})
}

func TestCheckpointKeyClassification(t *testing.T) {
	if !IsCheckpointKey("ns/checkpoints/abc/def/1") || IsCheckpointKey("ns/logs/x") || IsCheckpointKey("/checkpoints/x") {
		t.Fatal("IsCheckpointKey misclassifies")
	}
	found := false
	for _, s := range RefSources() {
		found = found || s == "task_checkpoints.object_key"
	}
	if !found {
		t.Fatal("task_checkpoints.object_key is not a declared reference source")
	}
}
