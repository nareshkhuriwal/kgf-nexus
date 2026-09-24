// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

// Integration tests for the kubernetes executor against a LOCAL cluster
// (Docker Desktop / kind). Opt-in via NEXUS_K8S_CONFORMANCE=1 and guarded by
// k8stest, which refuses any context that is not recognisably local. They run
// in namespace nexus-k8s-exec-test (Pod Security "restricted" enforced), which
// TestMain deletes afterwards.
//
// The shared conformance suite (conformance_test.go) already covers success,
// non-zero exit, OOM kill, timeout, SIGTERM/grace, cancel, image-pull NOT_FOUND,
// validation and no-orphan cleanup for this executor. The tests here cover what
// is specific to Kubernetes: re-attach without a duplicate Job, harvest after
// an agent restart, named-output and input round-trips, pod-phase reporting,
// and eviction classification.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor/k8stest"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func TestMain(m *testing.M) {
	code := m.Run()
	k8stest.Teardown()
	os.Exit(code)
}

// k8sConformanceImage is pinned and the same as the default helper image, so
// the node pulls one image for the whole suite.
func k8sConformanceImage() string {
	if v := os.Getenv("NEXUS_K8S_CONFORMANCE_IMAGE"); v != "" {
		return v
	}
	return DefaultK8sHelperImage
}

func k8sConformanceSkip() string {
	_, why := k8stest.Client()
	return why
}

func newK8sConformanceExecutor(t *testing.T, root string) *KubernetesExecutor {
	return newK8sTestExecutor(t, root, "nexus-conformance")
}

func newK8sTestExecutor(t *testing.T, root, owner string) *KubernetesExecutor {
	t.Helper()
	cs, why := k8stest.Client()
	cfg := KubernetesConfig{
		Namespace:        k8stest.Namespace,
		Owner:            owner,
		ImagePullPolicy:  corev1.PullIfNotPresent,
		ImagePullTimeout: 90 * time.Second,
		StartTimeout:     3 * time.Minute,
		TTLAfterFinished: 5 * time.Minute,
	}
	if why != "" {
		// Kind() must still work (TestConformanceKindIsStable runs unskipped).
		return NewKubernetesExecutor(cfg, nil, root, discardLog())
	}
	e := NewKubernetesExecutor(cfg, cs, root, discardLog())
	t.Cleanup(e.Close)
	return e
}

func requireK8s(t *testing.T) {
	t.Helper()
	if why := k8sConformanceSkip(); why != "" {
		t.Skip(why)
	}
}

// k8sAwaitTaskRunning blocks until the attempt's task container is running.
func k8sAwaitTaskRunning(t *testing.T, e Executor, h Handle) {
	t.Helper()
	ke := e.(*KubernetesExecutor)
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		pods, err := ke.Client.CoreV1().Pods(ke.Config.Namespace).List(context.Background(),
			metav1.ListOptions{LabelSelector: ke.selectorForAttempt(h.AttemptID)})
		if err == nil && len(pods.Items) > 0 {
			if cs := findContainerStatus(pods.Items[0].Status.ContainerStatuses, k8sTaskContainer); cs != nil && cs.State.Running != nil {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("task container of attempt %s never reached Running", h.AttemptID)
}

// k8sWaitForAttemptGone polls the API server — not the executor's own cache —
// until neither the Job nor any pod of the attempt exists. Asking the API
// server directly is the point: a check that consults the component under test
// can agree with that component's bug (F89).
func k8sWaitForAttemptGone(e Executor, attemptID string, timeout time.Duration) error {
	ke := e.(*KubernetesExecutor)
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		_, jerr := ke.Client.BatchV1().Jobs(ke.Config.Namespace).Get(ctx, k8sJobName(attemptID), metav1.GetOptions{})
		pods, perr := ke.Client.CoreV1().Pods(ke.Config.Namespace).List(ctx,
			metav1.ListOptions{LabelSelector: ke.selectorForAttempt(attemptID)})
		if apierrors.IsNotFound(jerr) && perr == nil && len(pods.Items) == 0 {
			return nil
		}
		last = fmt.Sprintf("job err=%v, pods=%d", jerr, len(pods.Items))
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("attempt %s still has objects after %v (%s)", attemptID, timeout, last)
}

func k8sSpec(attemptID, script string) TaskSpec {
	return TaskSpec{
		AttemptID: attemptID, RunID: "run-k8s-it", NodeID: "node-k8s-it", IdempotencyKey: "idem-k8s-it",
		Runtime: &nexusv1.RuntimeSpec{
			Image: k8sConformanceImage(), Command: []string{"/bin/sh", "-c"}, Args: []string{script},
		},
		Limit:                &nexusv1.ResourceVector{CpuMillis: 250, MemoryBytes: 64 << 20},
		StartToCloseDeadline: time.Now().Add(3 * time.Minute),
		GraceAfterTerm:       2 * time.Second,
	}
}

func uniqueAttempt(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func countAttemptJobs(t *testing.T, e *KubernetesExecutor, attemptID string) int {
	t.Helper()
	jobs, err := e.Client.BatchV1().Jobs(e.Config.Namespace).List(context.Background(),
		metav1.ListOptions{LabelSelector: k8sLabelAttempt + "=" + k8sLabelValue(attemptID)})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	return len(jobs.Items)
}

// An agent that crashes after creating a Job and is restarted must ADOPT that
// Job, not launch a second copy of the same attempt. Modelled with two executor
// instances sharing an owner: the first is abandoned mid-run (no Cleanup, no
// Wait), exactly as a SIGKILLed agent abandons it.
func TestK8sIntegrationRestartReattachesWithoutDuplicate(t *testing.T) {
	requireK8s(t)
	ctx := context.Background()
	attempt := uniqueAttempt("reattach")
	spec := k8sSpec(attempt, `sleep 4; printf '{"adopted":true}' > "$NEXUS_OUTPUT_FILE"`)

	first := newK8sTestExecutor(t, t.TempDir(), "nexus-it-restart")
	p1, err := first.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("first Prepare: %v", err)
	}
	h1, err := first.Start(ctx, p1)
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	job1, err := first.Client.BatchV1().Jobs(first.Config.Namespace).Get(ctx, h1.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get first job: %v", err)
	}
	first.Close() // the "crash": informers gone, no Wait, no Cleanup

	second := newK8sTestExecutor(t, t.TempDir(), "nexus-it-restart")
	p2, err := second.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	h2, err := second.Start(ctx, p2)
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	t.Cleanup(func() { _ = second.Cleanup(context.Background(), h2) })

	if n := countAttemptJobs(t, second, attempt); n != 1 {
		t.Fatalf("DOUBLE LAUNCH: %d Jobs exist for attempt %s after re-Prepare, want exactly 1", n, attempt)
	}
	job2, err := second.Client.BatchV1().Jobs(second.Config.Namespace).Get(ctx, h2.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job after re-attach: %v", err)
	}
	if job2.UID != job1.UID {
		t.Fatalf("re-Prepare replaced the Job (uid %s -> %s) instead of adopting it", job1.UID, job2.UID)
	}
	if !p2.State.(*k8sState).adopted {
		t.Error("second Prepare did not report adopting the existing Job")
	}
	o, err := second.Wait(ctx, h2)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !o.Succeeded() || string(o.Output) != `{"adopted":true}` {
		t.Fatalf("adopted attempt outcome = %+v (err=%v), want success with the original run's output", o, o.Err)
	}
	pods, _ := second.Client.CoreV1().Pods(second.Config.Namespace).List(ctx,
		metav1.ListOptions{LabelSelector: second.selectorForAttempt(attempt)})
	if len(pods.Items) != 1 {
		t.Errorf("attempt ran %d pods, want exactly 1", len(pods.Items))
	}
}

// The WAL path: an attempt FINISHED while its agent was down. Reattach must
// find it and Wait must harvest the real outcome and outputs from the Job
// alone — no TaskSpec — so the agent can report it instead of INDETERMINATE.
func TestK8sIntegrationReattachHarvestsFinishedAttempt(t *testing.T) {
	requireK8s(t)
	ctx := context.Background()
	attempt := uniqueAttempt("harvest")
	spec := k8sSpec(attempt, `printf 'metrics-bytes' > "$NEXUS_OUTPUT_DIR/metrics"; exit 0`)
	spec.DeclaredOutputs = []string{"metrics", "model"}

	first := newK8sTestExecutor(t, t.TempDir(), "nexus-it-harvest")
	p, err := first.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := first.Start(ctx, p); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first.Close() // crash before Wait

	// Let it finish unobserved.
	deadline := time.Now().Add(90 * time.Second)
	for {
		j, err := first.Client.BatchV1().Jobs(first.Config.Namespace).Get(ctx, k8sJobName(attempt), metav1.GetOptions{})
		if err == nil && jobFinished(j) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("attempt never finished")
		}
		time.Sleep(500 * time.Millisecond)
	}

	restarted := newK8sTestExecutor(t, t.TempDir(), "nexus-it-harvest")
	h, state, err := restarted.Reattach(ctx, attempt)
	if err != nil {
		t.Fatalf("Reattach: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Cleanup(context.Background(), h) })
	if state != ReattachFinished {
		t.Fatalf("Reattach state = %v, want ReattachFinished", state)
	}
	o, err := restarted.Wait(ctx, h)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !o.Succeeded() {
		t.Fatalf("harvested outcome = %+v (err=%v), want success", o, o.Err)
	}
	if got := string(o.Outputs["metrics"].Bytes); got != "metrics-bytes" {
		t.Errorf("harvested output metrics = %q; declared outputs must be recovered from the Job annotations", got)
	}
	if _, ok := o.Outputs["model"]; ok {
		t.Error("an output the task never wrote must be absent, not empty")
	}

	if _, st, err := restarted.Reattach(ctx, uniqueAttempt("never-existed")); err != nil || st != ReattachNotFound {
		t.Errorf("Reattach of an unknown attempt = (%v, %v), want ReattachNotFound", st, err)
	}
}

// Named outputs, the legacy output file, staged inputs and binary data all
// survive the pod boundary byte-for-byte, and the pod phase reaches OnPhase.
func TestK8sIntegrationOutputsAndInputsRoundTrip(t *testing.T) {
	requireK8s(t)
	e := newK8sTestExecutor(t, t.TempDir(), "nexus-it-outputs")
	attempt := uniqueAttempt("outputs")
	script := `
set -e
cat "$NEXUS_INPUT_DIR/greeting" > "$NEXUS_OUTPUT_DIR/echo"
printf '{"n":1}' > "$NEXUS_OUTPUT_DIR/metrics"
printf '\000\001\377binary\n\000' > "$NEXUS_OUTPUT_DIR/model"
printf 'stray' > "$NEXUS_OUTPUT_DIR/undeclared"
printf 'not-json' > "$NEXUS_WORK_DIR/output.json"
[ "$NEXUS_OUTPUT_NAMES" = "echo metrics model" ]
touch /should-fail 2>/dev/null && exit 9 || true
`
	spec := k8sSpec(attempt, script)
	spec.DeclaredOutputs = []string{"echo", "metrics", "model"}
	spec.Inputs = map[string]*nexusv1.ResultRef{
		"greeting": {Kind: &nexusv1.ResultRef_Inline{Inline: []byte("hello from upstream\x00\xff")}},
	}
	var mu sync.Mutex
	var phases []string
	spec.OnPhase = func(phase, detail string) {
		mu.Lock()
		phases = append(phases, phase)
		mu.Unlock()
	}

	o, _ := runToCompletion(t, e, spec)
	if !o.Succeeded() {
		t.Fatalf("task failed: %+v (err=%v)", o, o.Err)
	}
	if got := string(o.Outputs["echo"].Bytes); got != "hello from upstream\x00\xff" {
		t.Errorf("input->output round trip = %q", got)
	}
	if got := o.Outputs["metrics"]; string(got.Bytes) != `{"n":1}` || got.ContentType != "application/json" {
		t.Errorf("metrics = %q (%s)", got.Bytes, got.ContentType)
	}
	if got := string(o.Outputs["model"].Bytes); got != "\x00\x01\xffbinary\n\x00" {
		t.Errorf("binary output corrupted: %q", got)
	}
	if _, ok := o.Outputs["undeclared"]; ok {
		t.Error("an undeclared file was collected as an output")
	}
	if string(o.Output) != "not-json" || o.ContentType != "application/octet-stream" {
		t.Errorf("legacy output.json = %q (%s)", o.Output, o.ContentType)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(phases, ",")
	if !strings.Contains(joined, "Running") {
		t.Errorf("OnPhase never reported Running; saw %q", joined)
	}
}

// An eviction is the cluster taking the pod away: INFRASTRUCTURE and
// retryable, never APPLICATION, even though the container dies non-zero.
func TestK8sIntegrationEvictionIsInfrastructure(t *testing.T) {
	requireK8s(t)
	ctx := context.Background()
	e := newK8sTestExecutor(t, t.TempDir(), "nexus-it-evict")
	spec := k8sSpec(uniqueAttempt("evict"), `i=0; while [ $i -lt 3000 ]; do sleep 0.1; i=$((i+1)); done`)

	p, err := e.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h, err := e.Start(ctx, p)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = e.Cleanup(context.Background(), h) })
	k8sAwaitTaskRunning(t, e, h)

	pods, err := e.Client.CoreV1().Pods(e.Config.Namespace).List(ctx,
		metav1.ListOptions{LabelSelector: e.selectorForAttempt(h.AttemptID)})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("list pods: %v (%d)", err, len(pods.Items))
	}
	// The Eviction API is what node-pressure drains and `kubectl drain` use.
	if err := e.Client.PolicyV1().Evictions(e.Config.Namespace).Evict(ctx, &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: pods.Items[0].Name, Namespace: e.Config.Namespace},
	}); err != nil {
		t.Fatalf("evict: %v", err)
	}

	o, err := e.Wait(ctx, h)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	c := ClassifiedFromOutcome(o, "")
	if c.Class != nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE || !c.Retryable {
		t.Errorf("evicted attempt classified %v retryable=%v (%+v, err=%v), want INFRASTRUCTURE retryable",
			c.Class, c.Retryable, o, o.Err)
	}
}

// Prepare rejects stdout_output rather than returning stderr spliced into the
// output (the pod log API cannot separate the streams). No cluster needed.
func TestK8sStdoutOutputRejectedAsValidation(t *testing.T) {
	e := NewKubernetesExecutor(KubernetesConfig{Namespace: "x"}, nil, t.TempDir(), discardLog())
	spec := k8sSpec("stdout-reject", "echo hi")
	spec.DeclaredOutputs = []string{"out"}
	spec.StdoutOutput = "out"
	_, err := e.Prepare(context.Background(), spec)
	if c := ClassifyError(err); c.Class != nexusv1.ErrorClass_ERROR_CLASS_VALIDATION {
		t.Fatalf("class = %v (%v), want VALIDATION", c.Class, err)
	}
}
