// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/artifactcache"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/eviction"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func runUntilRunning(t *testing.T, r *Runner, reg *Registry, a *nexusv1.TaskAssignment) chan struct{} {
	t.Helper()
	if ok, why := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatal(why)
	}
	done := make(chan struct{})
	go func() { r.Run(context.Background(), a); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && r.RunningCount() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if r.RunningCount() == 0 {
		t.Fatal("attempt never started")
	}
	return done
}

// A preempted attempt that checkpoints and exits non-zero is reported
// PREEMPTED; the workload saw SIGTERM first (its checkpoint signal).
func TestPreemptedAttemptReportsPreemptedAfterCheckpointSignal(t *testing.T) {
	c := &fakeReportClient{}
	r, reg, _ := newTestRunner(t, c)
	mark := filepath.Join(t.TempDir(), "checkpointed")
	a := assignment("p1", `trap 'echo saved > `+mark+`; exit 143' TERM; i=0; while [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done`)
	a.StartToCloseDeadlineUnixMs = time.Now().Add(2 * time.Minute).UnixMilli()
	done := runUntilRunning(t, r, reg, a)
	time.Sleep(400 * time.Millisecond) // let the trap install

	r.Preempt("p1", 5*time.Second)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("preempted attempt did not stop")
	}
	if _, err := os.Stat(mark); err != nil {
		t.Fatal("the task never received its checkpoint signal (SIGTERM)")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.results) == 0 {
		t.Fatal("no result reported")
	}
	res := c.results[len(c.results)-1]
	if res.GetError().GetClass() != nexusv1.ErrorClass_ERROR_CLASS_PREEMPTED ||
		res.GetOutcome() != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED {
		t.Fatalf("reported %v / %v, want FAILED / PREEMPTED", res.GetOutcome(), res.GetError().GetClass())
	}
}

// A task that finishes cleanly (exit 0) after the signal is a success: it
// decided it was done.
func TestPreemptedAttemptThatExitsZeroIsASuccess(t *testing.T) {
	c := &fakeReportClient{}
	r, reg, _ := newTestRunner(t, c)
	a := assignment("p2", `trap 'exit 0' TERM; i=0; while [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done`)
	a.StartToCloseDeadlineUnixMs = time.Now().Add(2 * time.Minute).UnixMilli()
	done := runUntilRunning(t, r, reg, a)
	time.Sleep(400 * time.Millisecond)
	r.Preempt("p2", 5*time.Second)
	<-done
	c.mu.Lock()
	defer c.mu.Unlock()
	if got := c.results[len(c.results)-1].GetOutcome(); got != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		t.Fatalf("outcome %v, want SUCCEEDED", got)
	}
}

func TestEvictionGraceUsesTheNoticeDeadline(t *testing.T) {
	now := time.Now()
	if g := EvictionGrace(now, eviction.Notice{Deadline: now.Add(2 * time.Minute)}, 0, 0); g != DefaultEvictionMaxGrace {
		t.Fatalf("AWS-style two-minute notice: grace %v, want the cap", g)
	}
	if g := EvictionGrace(now, eviction.Notice{Deadline: now.Add(30 * time.Second)}, 0, 0); g != 25*time.Second {
		t.Fatalf("30 s notice: grace %v, want 25 s (5 s kept to report)", g)
	}
	if g := EvictionGrace(now, eviction.Notice{}, 0, 0); g != DefaultEvictionGrace {
		t.Fatalf("no deadline: %v", g)
	}
	if g := EvictionGrace(now, eviction.Notice{Deadline: now.Add(-time.Second)}, 0, 0); g != time.Second {
		t.Fatalf("past deadline: %v, want the 1 s floor", g)
	}
}

// Artifacts declared by the task reach the workload as NEXUS_ARTIFACT_<NAME>,
// from the node-local cache, and a second attempt hits the cache.
func TestRunnerServesDeclaredArtifactsFromTheCache(t *testing.T) {
	c := &fakeReportClient{}
	r, reg, _ := newTestRunner(t, c)
	src := filepath.Join(t.TempDir(), "model.bin")
	content := []byte("weights")
	if err := os.WriteFile(src, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	key := "sha256:" + hex.EncodeToString(sum[:])
	cache, err := artifactcache.Open(artifactcache.Config{Dir: t.TempDir(), MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	r.Cache = cache
	for i, id := range []string{"c1", "c2"} {
		a := assignment(id, `cat "$NEXUS_ARTIFACT_MODEL_V1" > "$NEXUS_OUTPUT_FILE"`)
		a.Spec.Placement = &nexusv1.PlacementSpec{Artifacts: []*nexusv1.CacheArtifact{{
			Name: "model-v1", Key: key, Uri: "file://" + src, SizeBytes: int64(len(content))}}}
		if ok, why := reg.Admit(id, a.GetSpec().GetResources().GetRequest()); !ok {
			t.Fatal(why)
		}
		r.Run(context.Background(), a)
		c.mu.Lock()
		res := c.results[len(c.results)-1]
		c.mu.Unlock()
		if res.GetOutcome() != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
			t.Fatalf("run %d: %v %v", i, res.GetOutcome(), res.GetError())
		}
		if got := string(res.GetOutputs()["result"].GetInline()); !strings.Contains(got, "weights") {
			t.Fatalf("run %d: workload read %q from the artifact", i, got)
		}
	}
	if keys := cache.Keys(10); len(keys) != 1 || keys[0] != key {
		t.Fatalf("cache keys %v", keys)
	}

	// A wrong digest fails VALIDATION, not a retryable class.
	bad := assignment("c3", `true`)
	bad.Spec.Placement = &nexusv1.PlacementSpec{Artifacts: []*nexusv1.CacheArtifact{{
		Name: "m", Key: "sha256:" + strings.Repeat("0", 64), Uri: "file://" + src}}}
	if ok, why := reg.Admit("c3", bad.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatal(why)
	}
	r.Run(context.Background(), bad)
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl := c.results[len(c.results)-1].GetError().GetClass(); cl != nexusv1.ErrorClass_ERROR_CLASS_VALIDATION {
		t.Fatalf("digest mismatch reported as %v", cl)
	}
}
