// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/taskapi"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// ── declared-error mapping ───────────────────────────────────────────────────

func TestDeclaredTaskErrorRules(t *testing.T) {
	rl := &taskapi.DeclaredError{Class: nexusv1.ErrorClass_ERROR_CLASS_RATE_LIMITED, Type: "RateLimited",
		Message: "429 from openai", RetryAfterMs: 4000}
	val := &taskapi.DeclaredError{Class: nexusv1.ErrorClass_ERROR_CLASS_VALIDATION}
	cases := []struct {
		name      string
		o         executor.Outcome
		f         attemptFacts
		honoured  bool
		retryable bool
	}{
		{"non-zero exit with a declaration", executor.Outcome{ExitCode: 1}, attemptFacts{Declared: rl}, true, true},
		{"self-raised signal", executor.Outcome{ExitCode: 134, Signalled: true, Signal: "abort"}, attemptFacts{Declared: rl}, true, true},
		{"validation is terminal", executor.Outcome{ExitCode: 2}, attemptFacts{Declared: val}, true, false},
		{"nothing declared", executor.Outcome{ExitCode: 1}, attemptFacts{}, false, false},
		{"exit zero", executor.Outcome{ExitCode: 0}, attemptFacts{Declared: rl}, false, false},
		{"oom killed", executor.Outcome{ExitCode: 137, OOMKilled: true}, attemptFacts{Declared: rl}, false, false},
		{"timed out", executor.Outcome{ExitCode: 143, TimedOut: true}, attemptFacts{Declared: rl}, false, false},
		{"cancelled by the agent", executor.Outcome{ExitCode: 143, Signalled: true}, attemptFacts{Declared: rl, CancelledByAgent: true}, false, false},
		{"executor error", executor.Outcome{ExitCode: 1, Err: errors.New("docker died")}, attemptFacts{Declared: rl}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			te, ok := declaredTaskError(tc.o, tc.f)
			if ok != tc.honoured {
				t.Fatalf("honoured = %v, want %v", ok, tc.honoured)
			}
			if !ok {
				return
			}
			if te.GetClass() != tc.f.Declared.Class || te.GetRetryable() != tc.retryable {
				t.Errorf("error = %v", te)
			}
			if !strings.Contains(te.GetMessage(), fmt.Sprintf("exit code %d", tc.o.ExitCode)) {
				t.Errorf("message %q does not name the exit code", te.GetMessage())
			}
		})
	}
	te, _ := declaredTaskError(executor.Outcome{ExitCode: 1}, attemptFacts{Declared: rl})
	if te.GetRetryAfterMs() != 4000 || te.GetType() != "RateLimited" || !strings.HasPrefix(te.GetMessage(), "429 from openai") {
		t.Errorf("rendered = %v", te)
	}
	te, _ = declaredTaskError(executor.Outcome{ExitCode: 1}, attemptFacts{Declared: val})
	if te.GetType() != "WorkloadDeclared" || !strings.Contains(te.GetMessage(), "validation") {
		t.Errorf("defaults = %v", te)
	}
}

// ── the runner, end to end over a real socket ────────────────────────────────

// testTaskAPIRoot is a private socket root short enough for Darwin's 104-byte
// sun_path, which t.TempDir() under /var/folders is not.
func testTaskAPIRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "nxw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return root
}

// orderedClient is a ReportClient and a taskapi.Upstream that records the
// order in which calls arrive, so a test can assert the usage flush precedes
// the result report.
type orderedClient struct {
	fakeReportClient
	mu     sync.Mutex
	events []string
	usage  []*nexusv1.ReportUsageRequest
}

func (c *orderedClient) note(ev string) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}

func (c *orderedClient) ReportResult(ctx context.Context, req *nexusv1.ReportResultRequest) (*nexusv1.Ack, error) {
	c.note("result")
	return c.fakeReportClient.ReportResult(ctx, req)
}

func (c *orderedClient) ReportUsage(_ context.Context, req *nexusv1.ReportUsageRequest) (*nexusv1.Ack, error) {
	c.mu.Lock()
	c.usage = append(c.usage, req)
	c.mu.Unlock()
	c.note("usage")
	return &nexusv1.Ack{Accepted: true}, nil
}

func (c *orderedClient) AcquirePermits(context.Context, *nexusv1.AcquirePermitsRequest) (*nexusv1.AcquirePermitsResponse, error) {
	return &nexusv1.AcquirePermitsResponse{Accepted: true, Granted: true}, nil
}

func (c *orderedClient) SaveCheckpoint(context.Context, *nexusv1.SaveCheckpointRequest) (*nexusv1.Ack, error) {
	return &nexusv1.Ack{Accepted: true}, nil
}

func (c *orderedClient) GetCheckpoint(context.Context, *nexusv1.GetCheckpointRequest) (*nexusv1.GetCheckpointResponse, error) {
	return &nexusv1.GetCheckpointResponse{Accepted: true}, nil
}

// apiWorkload is an executor whose "workload" is a Go function that talks to
// the attempt's socket the way the SDK would, then exits with a code. It
// exercises the runner's wiring without depending on curl in the test image.
type apiWorkload struct {
	exitCode int
	script   func(t *testing.T, call func(method, path, body string) int)
	t        *testing.T

	mu       sync.Mutex
	socket   string
	socketGo bool // whether the socket existed while the workload ran
}

func (e *apiWorkload) Kind() string { return "apitest" }

func (e *apiWorkload) Prepare(_ context.Context, spec executor.TaskSpec) (executor.Prepared, error) {
	return executor.Prepared{AttemptID: spec.AttemptID, State: spec}, nil
}

func (e *apiWorkload) Start(_ context.Context, p executor.Prepared) (executor.Handle, error) {
	return executor.Handle{AttemptID: p.AttemptID, ExecutorKind: e.Kind(), State: p.State}, nil
}

func (e *apiWorkload) Wait(_ context.Context, h executor.Handle) (executor.Outcome, error) {
	spec := h.State.(executor.TaskSpec)
	e.mu.Lock()
	e.socket = spec.TaskAPISocket
	_, err := os.Stat(spec.TaskAPISocket)
	e.socketGo = err == nil
	e.mu.Unlock()
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", spec.TaskAPISocket)
		}}}
	call := func(method, path, body string) int {
		req, _ := http.NewRequest(method, "http://task"+path, bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer "+spec.TaskAPIToken)
		resp, err := hc.Do(req)
		if err != nil {
			e.t.Errorf("%s %s: %v", method, path, err)
			return 0
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if spec.TaskAPISocket == "" {
		e.t.Error("the runner gave the workload no task API socket")
	} else {
		e.script(e.t, call)
	}
	return executor.Outcome{ExitCode: e.exitCode}, nil
}

func (e *apiWorkload) Signal(context.Context, executor.Handle, executor.Signal) error { return nil }
func (e *apiWorkload) Logs(context.Context, executor.Handle) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (e *apiWorkload) Cleanup(context.Context, executor.Handle) error { return nil }

func runAPIWorkload(t *testing.T, w *apiWorkload) (*orderedClient, []*nexusv1.ReportResultRequest) {
	t.Helper()
	w.t = t
	root := testTaskAPIRoot(t)

	c := &orderedClient{}
	reg := NewRegistry(vec(8000, 8<<30, 0))
	wl := newTestWAL(t)
	r := NewRunner(map[string]executor.Executor{"apitest": w}, reg, newFakeLeases(),
		NewReporter(c, wl, testLog()), wl, nil, testLog())
	r.TaskAPIRoot = root
	r.WorkloadAPI = &nexusv1.WorkloadApiConfig{UsageReporting: true, RateLimits: true}
	r.WorkloadUpstream = c

	a := assignment("7d1f0c2e-aaaa-bbbb-cccc-000000000001", "")
	a.Spec.Runtime.Executor = "apitest"
	if ok, reason := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatalf("admit: %s", reason)
	}
	r.Run(context.Background(), a)

	c.fakeReportClient.mu.Lock()
	defer c.fakeReportClient.mu.Unlock()
	return c, append([]*nexusv1.ReportResultRequest(nil), c.results...)
}

// TestRunnerFlushesUsageBeforeTheResultAndHonoursTheDeclaredError is the
// ADR-0032 contract in one attempt: increments become one cumulative
// snapshot delivered BEFORE ReportResult (the ledger is written in the
// terminal transaction), a declared rate_limited failure becomes the attempt's
// error with its retry_after_ms, and the socket is gone after Run.
func TestRunnerFlushesUsageBeforeTheResultAndHonoursTheDeclaredError(t *testing.T) {
	w := &apiWorkload{exitCode: 3, script: func(t *testing.T, call func(string, string, string) int) {
		for _, b := range []string{
			`{"units":{"tokens:openai/gpt-4o-mini:input":1200,"requests:openai/gpt-4o-mini":1}}`,
			`{"units":{"tokens:openai/gpt-4o-mini:input":800,"requests:openai/gpt-4o-mini":1}}`,
		} {
			if code := call(http.MethodPost, "/v1/usage", b); code != http.StatusOK {
				t.Errorf("usage: %d", code)
			}
		}
		if code := call(http.MethodPost, "/v1/progress", `{"message":"halfway","fraction":0.5}`); code != http.StatusNoContent {
			t.Errorf("progress: %d", code)
		}
		if code := call(http.MethodPost, "/v1/error",
			`{"class":"rate_limited","type":"RateLimited","message":"provider throttled","retry_after_ms":9000}`); code != http.StatusNoContent {
			t.Errorf("error: %d", code)
		}
	}}
	c, results := runAPIWorkload(t, w)

	if !w.socketGo {
		t.Error("the socket did not exist while the workload ran")
	}
	if _, err := os.Stat(w.socket); !os.IsNotExist(err) {
		t.Errorf("socket %s still present after Run: %v", w.socket, err)
	}
	c.mu.Lock()
	events := append([]string(nil), c.events...)
	usage := append([]*nexusv1.ReportUsageRequest(nil), c.usage...)
	c.mu.Unlock()
	if len(usage) == 0 {
		t.Fatal("usage was never flushed")
	}
	if got := strings.Join(events, ","); !strings.HasSuffix(got, "usage,result") {
		t.Errorf("events = %s; the final usage flush must precede the result report", got)
	}
	last := usage[len(usage)-1]
	if last.GetUnits()["tokens:openai/gpt-4o-mini:input"] != 2000 || last.GetUnits()["requests:openai/gpt-4o-mini"] != 2 {
		t.Errorf("final snapshot = %v, want cumulative totals", last.GetUnits())
	}
	if last.GetLeaseToken() == "" {
		t.Error("usage flushed without a lease token")
	}

	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	e := results[0].GetError()
	if results[0].GetOutcome() != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED ||
		e.GetClass() != nexusv1.ErrorClass_ERROR_CLASS_RATE_LIMITED || !e.GetRetryable() ||
		e.GetRetryAfterMs() != 9000 || e.GetType() != "RateLimited" {
		t.Errorf("result = %v %v", results[0].GetOutcome(), e)
	}
}

// Without a declaration the failure is classified exactly as before.
func TestRunnerWithoutDeclarationKeepsTheObservedClass(t *testing.T) {
	w := &apiWorkload{exitCode: 4, script: func(t *testing.T, call func(string, string, string) int) {
		if code := call(http.MethodGet, "/v1/info", ""); code != http.StatusOK {
			t.Errorf("info: %d", code)
		}
	}}
	c, results := runAPIWorkload(t, w)
	if len(results) != 1 || results[0].GetError().GetClass() != nexusv1.ErrorClass_ERROR_CLASS_APPLICATION {
		t.Fatalf("results = %v", results)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.usage) != 0 {
		t.Errorf("an attempt that reported no usage sent %d usage snapshots", len(c.usage))
	}
}
