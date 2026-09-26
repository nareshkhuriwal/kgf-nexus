// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/secrets"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/wal"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/logs"
)

// TestSecretsAreRedactedFromShippedTaskLogs is the end-to-end proof that the
// ADR-0020 redactor finally has input (F102, ADR-0025): a real subprocess task
// prints its resolved secret; the log is shipped through the real result store
// and indexed through the real gateway server; what an operator reads back
// has [REDACTED:NAME] and never the value -- neither in the chunks nor in the
// compacted object nor through search.
func TestSecretsAreRedactedFromShippedTaskLogs(t *testing.T) {
	env := newLogEnv(t)
	ctx := context.Background()
	token := "tok-" + uuid.NewString()
	attemptID := env.seed(t, token)
	const value = "db-pw-" + "4f1c9e7a2b"

	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	reports := &lgReports{pool: env.pool}
	runner := NewRunner(
		map[string]executor.Executor{"subprocess": &executor.SubprocessExecutor{Root: filepath.Join(dir, "work"), AllowUnenforceableLimits: true, Log: testLog()}},
		NewRegistry(vec(4000, 1<<30, 0)), lgLeases{token: token}, NewReporter(reports, w, testLog()), w, env.store, testLog())
	runner.LogShipping = &nexusv1.LogShippingConfig{Enabled: true, FlushIntervalMs: 100}
	runner.LogIndexer = gatewayIndexer{srv: env.srv, ns: env.ns}
	runner.LogSpoolDir = filepath.Join(dir, "spool")
	runner.Namespace = env.ns
	runner.Secrets = secrets.NewResolver(0, &secrets.EnvProvider{Allow: []string{"IT_*"},
		Lookup: func(k string) (string, bool) { return value, k == "IT_DB_PASSWORD" }})
	if err := os.MkdirAll(runner.LogSpoolDir, 0o700); err != nil {
		t.Fatal(err)
	}

	script := `echo "connecting with $DB_PASSWORD"; echo "file says $(cat "$NEXUS_SECRETS_DIR/pw")" >&2; ` +
		`for i in $(seq 1 30); do echo "line $i"; done; echo "bye $DB_PASSWORD"`
	runner.Run(ctx, &nexusv1.TaskAssignment{
		AttemptId: attemptID, RunId: "run", NodeId: "node", AttemptNumber: 1, LeaseToken: token,
		Spec: &nexusv1.TaskSpec{Runtime: &nexusv1.RuntimeSpec{
			Executor: "subprocess", Command: []string{"/bin/sh", "-c", script},
			Secrets: []*nexusv1.SecretRef{
				{Name: "DB_PASSWORD", Ref: "env://IT_DB_PASSWORD"},
				{Name: "pw", Ref: "env://IT_DB_PASSWORD", Inject: "file"},
			},
		}},
	})
	reports.mu.Lock()
	res := reports.result
	reports.mu.Unlock()
	if res == nil || res.GetOutcome() != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		t.Fatalf("result = %v", res)
	}
	env.setAttemptState(t, attemptID, "SUCCEEDED")

	var row struct{ lines int64 }
	waitUntil(t, "the log to be complete", 15*time.Second, func() bool {
		r, found, _, err := env.reader.Summary(ctx, attemptID, "SUCCEEDED")
		row.lines = r.LineCount
		return err == nil && found && r.LineCount >= 33
	})
	r, _, _, _ := env.reader.Summary(ctx, attemptID, "SUCCEEDED")
	page, err := env.reader.ReadLines(ctx, attemptID, r, 1, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, l := range page.Lines {
		all = append(all, l.Stream+": "+l.Message)
	}
	joined := strings.Join(all, "\n")
	if strings.Contains(joined, value) {
		t.Fatalf("the secret value is in the shipped log:\n%s", joined)
	}
	// Both secrets hold the same value, so the placeholder names whichever
	// of the two the redactor indexed; what matters is that it is replaced.
	for _, want := range []string{
		"stdout: connecting with [REDACTED:",
		"stdout: bye [REDACTED:",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("log lacks %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "stderr: file says [REDACTED:") {
		t.Errorf("file-injected value printed by the task was not redacted:\n%s", joined)
	}
	// Search reads the stored objects themselves: the value is not findable.
	sr, err := env.reader.Search(ctx, attemptID, r, logs.SearchQuery{Pattern: value})
	if err != nil || len(sr.Matches) != 0 {
		t.Errorf("search for the value: %d matches, err=%v", len(sr.Matches), err)
	}
	t.Logf("%d lines shipped, value redacted", len(page.Lines))
}
