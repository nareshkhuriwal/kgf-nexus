// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// These pin ADR-0015. Each was mutation-checked: sending SIGTERM on the
// uninterruptible path fails the first test, dropping the SIGKILL at the bound
// fails the second and third, and removing the `deferring` guard fails the
// fourth (see the ADR's evidence section).

func uninterruptible(a *nexusv1.TaskAssignment, maxMs int64) *nexusv1.TaskAssignment {
	a.Spec.Cancellation = &nexusv1.CancellationPolicy{Uninterruptible: true, MaxUninterruptibleMs: maxMs}
	a.StartToCloseDeadlineUnixMs = time.Now().Add(2 * time.Minute).UnixMilli()
	return a
}

// runAsync admits and starts an attempt, waits until its process is running and
// its TERM trap is installed, and returns a channel closed when Run returns.
func runAsync(t *testing.T, r *Runner, reg *Registry, a *nexusv1.TaskAssignment) <-chan struct{} {
	t.Helper()
	if ok, reason := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatalf("admit: %s", reason)
	}
	done := make(chan struct{})
	go func() { r.Run(context.Background(), a); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		rt := r.running[a.GetAttemptId()]
		r.mu.Unlock()
		if rt != nil {
			rt.handleMu.Lock()
			started := rt.handle != nil
			rt.handleMu.Unlock()
			if started {
				time.Sleep(300 * time.Millisecond) // let the shell install its trap
				return done
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("attempt never started")
	return nil
}

func waitDone(t *testing.T, done <-chan struct{}, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	select {
	case <-done:
		return time.Since(start)
	case <-time.After(within):
		t.Fatalf("attempt still running after %v", within)
		return 0
	}
}

// TestUninterruptibleCancelSendsNoSigtermAndLetsTaskFinish: the task traps
// SIGTERM and records it. Cancelled mid-run with a generous bound, it must
// never see a TERM and must finish on its own, successfully.
func TestUninterruptibleCancelSendsNoSigtermAndLetsTaskFinish(t *testing.T) {
	c := &fakeReportClient{}
	r, reg, _ := newTestRunner(t, c)
	marker := filepath.Join(t.TempDir(), "termed")
	a := uninterruptible(assignment("u1",
		`trap 'touch `+marker+`; exit 1' TERM; i=0; while [ $i -lt 15 ]; do sleep 0.1; i=$((i+1)); done`),
		30_000)

	done := runAsync(t, r, reg, a)
	r.Cancel("u1", time.Second, "test_cancel")
	waitDone(t, done, 20*time.Second)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an uninterruptible task received SIGTERM on cancellation")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.results) != 1 || c.results[0].GetOutcome() != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		t.Fatalf("want one SUCCEEDED report from the task finishing on its own, got %+v", c.results)
	}
}

// TestUninterruptibleCancelSigkillsAtTheDeclaredBound: a task that would run for
// a minute and ignores SIGTERM is cancelled with a 1s declared bound. It must
// survive at least the bound and die shortly after it.
func TestUninterruptibleCancelSigkillsAtTheDeclaredBound(t *testing.T) {
	c := &fakeReportClient{}
	r, reg, _ := newTestRunner(t, c)
	a := uninterruptible(assignment("u2", `trap '' TERM; sleep 60`), 1000)

	done := runAsync(t, r, reg, a)
	r.Cancel("u2", time.Second, "test_cancel")
	took := waitDone(t, done, 15*time.Second)
	if took < 900*time.Millisecond {
		t.Fatalf("killed after %v, before its 1s bound", took)
	}
	if took > 8*time.Second {
		t.Fatalf("took %v to die after a 1s bound", took)
	}
	if reg.Count() != 0 {
		t.Errorf("registry still holds %d attempts", reg.Count())
	}
}

// TestUninterruptibleBoundIsClampedByThePlatformCeiling: the spec asks for 60s,
// the agent's ceiling is 1s. The ceiling wins.
func TestUninterruptibleBoundIsClampedByThePlatformCeiling(t *testing.T) {
	c := &fakeReportClient{}
	r, reg, _ := newTestRunner(t, c)
	r.UninterruptibleCeiling = time.Second
	a := uninterruptible(assignment("u3", `trap '' TERM; sleep 60`), 60_000)

	done := runAsync(t, r, reg, a)
	r.Cancel("u3", time.Second, "test_cancel")
	if took := waitDone(t, done, 15*time.Second); took > 8*time.Second {
		t.Fatalf("took %v: the 60s declaration was not clamped to the 1s ceiling", took)
	}
}

// TestRepeatedCancelDoesNotRestartTheBound: CANCEL_ATTEMPT repeats on every
// heartbeat and a revoked lease cancels again. The task must still die on the
// FIRST cancel's clock, and exactly one deferred wait may be started -- one per
// repeat would park a goroutine and a redundant SIGKILL per heartbeat.
func TestRepeatedCancelDoesNotRestartTheBound(t *testing.T) {
	c := &fakeReportClient{}
	r, reg, _ := newTestRunner(t, c)
	var logs lockedBuffer
	r.log = slog.New(slog.NewTextHandler(&logs, nil))
	a := uninterruptible(assignment("u4", `trap '' TERM; sleep 60`), 2000)

	done := runAsync(t, r, reg, a)
	start := time.Now()
	r.Cancel("u4", time.Second, "first")
	stop := make(chan struct{})
	go func() {
		tk := time.NewTicker(300 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				r.Cancel("u4", time.Second, "heartbeat_repeat")
				r.OnLeaseRevoked("u4", "repeat")
			}
		}
	}()
	defer close(stop)
	waitDone(t, done, 20*time.Second)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("killed %v after the FIRST cancel with a 2s bound: repeats restarted the clock", took)
	}
	if n := strings.Count(logs.String(), "cancellation deferred"); n != 1 {
		t.Fatalf("%d deferred waits started for one attempt, want exactly 1", n)
	}
}

// lockedBuffer is an io.Writer safe for the concurrent log writes of the runner
// and its deferral goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestEffectiveUninterruptibleBound(t *testing.T) {
	p := func(u bool, ms int64) *nexusv1.CancellationPolicy {
		return &nexusv1.CancellationPolicy{Uninterruptible: u, MaxUninterruptibleMs: ms}
	}
	for _, tc := range []struct {
		name    string
		p       *nexusv1.CancellationPolicy
		ceiling time.Duration
		want    time.Duration
	}{
		{"no policy", nil, time.Minute, 0},
		{"interruptible ignores its bound", p(false, 5000), time.Minute, 0},
		{"declared under ceiling", p(true, 5000), time.Minute, 5 * time.Second},
		{"declared over ceiling", p(true, 120_000), time.Minute, time.Minute},
		{"undeclared bound is the ceiling", p(true, 0), time.Minute, time.Minute},
		{"unset ceiling is the default", p(true, 0), 0, DefaultUninterruptibleCeiling},
	} {
		if got := EffectiveUninterruptibleBound(tc.p, tc.ceiling); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
