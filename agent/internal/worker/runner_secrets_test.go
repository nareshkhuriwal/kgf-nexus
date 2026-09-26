// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/secrets"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

const leakyValue = "sup3r-s3cret-db-password"

// leakyExecutor is a substrate whose log stream and failure message both
// print the secret, like a task that echoes its credential and a Kubernetes
// termination message that carries the log tail.
type leakyExecutor struct {
	mu      sync.Mutex
	spec    executor.TaskSpec
	started bool
	phase   func(string, string)
}

func (l *leakyExecutor) Kind() string { return "leaky" }
func (l *leakyExecutor) Prepare(_ context.Context, s executor.TaskSpec) (executor.Prepared, error) {
	l.mu.Lock()
	l.spec = s
	l.mu.Unlock()
	return executor.Prepared{AttemptID: s.AttemptID}, nil
}
func (l *leakyExecutor) Start(_ context.Context, p executor.Prepared) (executor.Handle, error) {
	l.mu.Lock()
	l.started = true
	onPhase := l.spec.OnPhase
	l.mu.Unlock()
	if onPhase != nil {
		onPhase("Running", "pulled with token "+leakyValue)
	}
	return executor.Handle{AttemptID: p.AttemptID, ExecutorKind: "leaky"}, nil
}
func (l *leakyExecutor) Wait(context.Context, executor.Handle) (executor.Outcome, error) {
	time.Sleep(100 * time.Millisecond) // let the log copy drain
	return executor.Outcome{ExitCode: 1}, errors.New("pod failed: Error: connecting with " + leakyValue + " refused")
}
func (l *leakyExecutor) Signal(context.Context, executor.Handle, executor.Signal) error { return nil }
func (l *leakyExecutor) Logs(context.Context, executor.Handle) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("starting\npassword is " + leakyValue + " ok\ndone\n")), nil
}
func (l *leakyExecutor) Cleanup(context.Context, executor.Handle) error { return nil }

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type progressRecorder struct {
	fakeReportClient
	pmu      sync.Mutex
	progress []string
}

func (p *progressRecorder) ReportProgress(_ context.Context, req *nexusv1.ReportProgressRequest) (*nexusv1.Ack, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	p.progress = append(p.progress, req.GetMessage())
	return &nexusv1.Ack{Accepted: true}, nil
}

func newSecretRunner(t *testing.T, c ReportClient, ex executor.Executor, logOut io.Writer) (*Runner, *Registry) {
	t.Helper()
	reg := NewRegistry(vec(8000, 8<<30, 0))
	w := newTestWAL(t)
	log := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := NewRunner(map[string]executor.Executor{"leaky": ex, "subprocess": &executor.SubprocessExecutor{
		Root: t.TempDir(), AllowUnenforceableLimits: true}}, reg, newFakeLeases(), NewReporter(c, w, log), w, nil, log)
	r.Secrets = secrets.NewResolver(0, &secrets.EnvProvider{Allow: []string{"TEST_DB_*"}, Lookup: func(k string) (string, bool) {
		if k == "TEST_DB_PASSWORD" {
			return leakyValue, true
		}
		return "", false
	}})
	return r, reg
}

func secretAssignment(id, executorKind string, refs ...*nexusv1.SecretRef) *nexusv1.TaskAssignment {
	a := assignment(id, "true")
	a.Spec.Runtime.Executor = executorKind
	a.Spec.Runtime.Secrets = refs
	return a
}

// TestResolvedSecretsReachTheExecutorAndAreRedactedEverywhere: the executor
// receives the VALUE, and the agent-log copy of the task output, the progress
// note and the reported error message all carry [REDACTED:name] instead
// (F102 finally has input).
func TestResolvedSecretsReachTheExecutorAndAreRedactedEverywhere(t *testing.T) {
	c := &progressRecorder{}
	ex := &leakyExecutor{}
	var logs syncBuf
	r, reg := newSecretRunner(t, c, ex, &logs)
	a := secretAssignment("sec-run-1", "leaky", &nexusv1.SecretRef{Name: "DB_PASSWORD", Ref: "env://TEST_DB_PASSWORD"})
	if ok, why := reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatal(why)
	}
	r.Run(context.Background(), a)

	ex.mu.Lock()
	got := ex.spec.Secrets
	ex.mu.Unlock()
	if len(got) != 1 || got[0].Name != "DB_PASSWORD" || got[0].Inject != secrets.InjectEnv {
		t.Fatalf("executor received %v", got)
	}
	// Wiped after the attempt: the executor's copy of the slice is zeroed.
	if strings.Contains(string(got[0].Value), leakyValue) {
		t.Error("resolved value not wiped after the attempt")
	}

	c.mu.Lock()
	if len(c.results) != 1 {
		t.Fatalf("%d results", len(c.results))
	}
	msg := c.results[0].GetError().GetMessage()
	c.mu.Unlock()
	if strings.Contains(msg, leakyValue) || !strings.Contains(msg, "[REDACTED:DB_PASSWORD]") {
		t.Errorf("reported error message = %q", msg)
	}
	out := logs.String()
	if strings.Contains(out, leakyValue) {
		t.Errorf("agent log contains the secret:\n%s", out)
	}
	if !strings.Contains(out, "password is [REDACTED:DB_PASSWORD] ok") {
		t.Errorf("agent log copy of the task output was not redacted as expected:\n%s", out)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.pmu.Lock()
		p := strings.Join(c.progress, "|")
		c.pmu.Unlock()
		if strings.Contains(p, "[REDACTED:DB_PASSWORD]") {
			if strings.Contains(p, leakyValue) {
				t.Errorf("progress leaked: %s", p)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("no redacted progress note; got %q", p)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSecretResolutionFailuresAreClassifiedAndNothingStarts(t *testing.T) {
	cases := []struct {
		name  string
		refs  []*nexusv1.SecretRef
		env   map[string]string
		class nexusv1.ErrorClass
		retry bool
	}{
		{"not allow-listed", []*nexusv1.SecretRef{{Name: "X", Ref: "env://OTHER"}}, nil, nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED, false},
		{"no vault provider", []*nexusv1.SecretRef{{Name: "X", Ref: "vault://kv/x"}}, nil, nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED, false},
		{"unset", []*nexusv1.SecretRef{{Name: "X", Ref: "env://TEST_DB_OTHER"}}, nil, nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND, false},
		{"reserved name", []*nexusv1.SecretRef{{Name: "NEXUS_IDEMPOTENCY_KEY", Ref: "env://TEST_DB_PASSWORD"}}, nil, nexusv1.ErrorClass_ERROR_CLASS_VALIDATION, false},
		{"clash with runtime.env", []*nexusv1.SecretRef{{Name: "LOG_LEVEL", Ref: "env://TEST_DB_PASSWORD"}}, map[string]string{"LOG_LEVEL": "info"}, nexusv1.ErrorClass_ERROR_CLASS_VALIDATION, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &progressRecorder{}
			ex := &leakyExecutor{}
			r, reg := newSecretRunner(t, c, ex, io.Discard)
			a := secretAssignment("sec-fail", "leaky", tc.refs...)
			a.Spec.Runtime.Env = tc.env
			reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest())
			r.Run(context.Background(), a)
			ex.mu.Lock()
			started := ex.started
			ex.mu.Unlock()
			if started {
				t.Error("the workload started although its secrets did not resolve")
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if len(c.results) != 1 {
				t.Fatalf("%d results", len(c.results))
			}
			e := c.results[0].GetError()
			if e.GetClass() != tc.class || e.GetRetryable() != tc.retry || e.GetType() != "SecretResolutionFailed" {
				t.Errorf("error = %v", e)
			}
			if reg.Count() != 0 {
				t.Error("capacity not released")
			}
		})
	}
}

// TestSubprocessTaskReceivesAResolvedSecret goes through the real subprocess
// executor: the value is in the task's environment, and the reported result
// (a digest the task computed) proves it without the value leaving.
func TestSubprocessTaskReceivesAResolvedSecret(t *testing.T) {
	c := &progressRecorder{}
	r, reg := newSecretRunner(t, c, &leakyExecutor{}, io.Discard)
	a := secretAssignment("sec-sub-run", "subprocess",
		&nexusv1.SecretRef{Name: "DB_PASSWORD", Ref: "env://TEST_DB_PASSWORD"},
		&nexusv1.SecretRef{Name: "db.pw", Ref: "env://TEST_DB_PASSWORD", Inject: "file"})
	a.Spec.Runtime.Args = []string{`[ "$DB_PASSWORD" = "` + leakyValue + `" ] && [ "$(cat "$NEXUS_SECRETS_DIR/db.pw")" = "` + leakyValue + `" ] && printf '"both"' > "$NEXUS_OUTPUT_FILE"`}
	reg.Admit(a.GetAttemptId(), a.GetSpec().GetResources().GetRequest())
	r.Run(context.Background(), a)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.results) != 1 || c.results[0].GetOutcome() != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED ||
		string(c.results[0].GetOutputs()["result"].GetInline()) != `"both"` {
		t.Fatalf("result = %v", c.results)
	}
}
