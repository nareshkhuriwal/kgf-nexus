// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

// Runner- and agent-level tests of the kubernetes executor against a LOCAL
// cluster: lease-loss fencing through the real Runner, and the boot-time WAL
// re-attach. Opt-in and guarded exactly like the executor's own cluster tests
// (k8stest refuses anything but a local context on a loopback API server).

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor/k8stest"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/wal"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

const k8sOwner = "nexus-worker-it"

// TestMain deletes the test namespace once, after every test in the package.
func TestMain(m *testing.M) {
	code := m.Run()
	k8stest.Teardown()
	os.Exit(code)
}

func k8sClientOrSkip(t *testing.T) kubernetes.Interface {
	t.Helper()
	cs, why := k8stest.Client()
	if why != "" {
		t.Skip(why)
	}
	return cs
}

func newK8sExec(t *testing.T, cs kubernetes.Interface) *executor.KubernetesExecutor {
	e := executor.NewKubernetesExecutor(executor.KubernetesConfig{
		Namespace: k8stest.Namespace, Owner: k8sOwner,
		ImagePullPolicy: corev1.PullIfNotPresent, StartTimeout: 3 * time.Minute,
	}, cs, t.TempDir(), testLog())
	t.Cleanup(e.Close)
	return e
}

func k8sAssignment(attemptID, script string) *nexusv1.TaskAssignment {
	a := assignment(attemptID, script)
	a.Spec.Runtime = &nexusv1.RuntimeSpec{
		Executor: executor.KubernetesKind, Image: executor.DefaultK8sHelperImage,
		Command: []string{"/bin/sh", "-c"}, Args: []string{script},
	}
	a.Spec.Resources = &nexusv1.ResourceRequest{Request: vec(250, 64<<20, 0)}
	a.StartToCloseDeadlineUnixMs = time.Now().Add(3 * time.Minute).UnixMilli()
	return a
}

func attemptSelector(id string) string { return "nexus.dev/attempt=" + id }

func awaitPodRunning(t *testing.T, cs kubernetes.Interface, id string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		pods, err := cs.CoreV1().Pods(k8stest.Namespace).List(context.Background(),
			metav1.ListOptions{LabelSelector: attemptSelector(id)})
		if err == nil && len(pods.Items) == 1 {
			for _, s := range pods.Items[0].Status.ContainerStatuses {
				if s.Name == "task" && s.State.Running != nil {
					return
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("pod of %s never ran", id)
}

// assertAttemptGone asks the API server directly, never the executor's cache.
func assertAttemptGone(t *testing.T, cs kubernetes.Interface, id string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		jobs, jerr := cs.BatchV1().Jobs(k8stest.Namespace).List(context.Background(),
			metav1.ListOptions{LabelSelector: attemptSelector(id)})
		pods, perr := cs.CoreV1().Pods(k8stest.Namespace).List(context.Background(),
			metav1.ListOptions{LabelSelector: attemptSelector(id)})
		if jerr == nil && perr == nil && len(jobs.Items) == 0 && len(pods.Items) == 0 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Errorf("attempt %s still has a Job or pod after %v: the workload was not fenced", id, within)
}

// Lease loss must KILL the Job: a revoked lease means a replacement attempt
// may already be running, and leaving this one running beside it is two
// copies of the same work on the cluster.
func TestK8sRunnerLeaseRevocationKillsJob(t *testing.T) {
	cs := k8sClientOrSkip(t)
	c := &fakeReportClient{}
	r, reg, _ := newTestRunner(t, c)
	r.executors[executor.KubernetesKind] = newK8sExec(t, cs)

	id := fmt.Sprintf("lease-%d", time.Now().UnixNano())
	a := k8sAssignment(id, `i=0; while [ $i -lt 3000 ]; do sleep 0.1; i=$((i+1)); done`)
	if ok, why := reg.Admit(id, a.GetSpec().GetResources().GetRequest()); !ok {
		t.Fatal(why)
	}
	done := make(chan struct{})
	go func() { r.Run(context.Background(), a); close(done) }()
	awaitPodRunning(t, cs, id)

	r.OnLeaseRevoked(id, "test")
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("Run did not return after the lease was revoked")
	}
	assertAttemptGone(t, cs, id, 60*time.Second)
	if reg.Count() != 0 {
		t.Errorf("registry still holds %d attempts", reg.Count())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.results) != 1 || c.results[0].GetOutcome() == nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		t.Errorf("reports = %v; a fenced attempt must never report success", c.results)
	}
}

// Boot-time re-attach (§6.3): a STARTING entry whose Job FINISHED while the
// agent was down is reported with its real outcome and outputs, not
// INDETERMINATE; one whose Job is still RUNNING is killed and left STARTING.
func TestK8sAgentReattachHarvestsFinishedAndFencesRunning(t *testing.T) {
	cs := k8sClientOrSkip(t)
	c := &fakeReportClient{}
	r, _, w := newTestRunner(t, c)
	ke := newK8sExec(t, cs)
	ag := &Agent{cfg: Config{Kubernetes: ke}, wal: w, runner: r, log: testLog(),
		execs: map[string]executor.Executor{executor.KubernetesKind: ke}}

	stamp := time.Now().UnixNano()
	finished := fmt.Sprintf("reat-done-%d", stamp)
	running := fmt.Sprintf("reat-run-%d", stamp)
	previous := newK8sExec(t, cs) // the "crashed" session
	launch := func(id, script string) {
		spec := executor.TaskSpec{
			AttemptID: id, RunID: "run-r", NodeID: "node-r", IdempotencyKey: "idem-r",
			Runtime: &nexusv1.RuntimeSpec{Image: executor.DefaultK8sHelperImage,
				Command: []string{"/bin/sh", "-c"}, Args: []string{script}},
			Limit:                vec(250, 64<<20, 0),
			StartToCloseDeadline: time.Now().Add(3 * time.Minute),
		}
		p, err := previous.Prepare(context.Background(), spec)
		if err != nil {
			t.Fatalf("Prepare %s: %v", id, err)
		}
		if _, err := previous.Start(context.Background(), p); err != nil {
			t.Fatalf("Start %s: %v", id, err)
		}
		if err := w.RecordStarting(wal.Entry{AttemptID: id, LeaseToken: "tok-" + id, RunID: "run-r", NodeID: "node-r"}); err != nil {
			t.Fatal(err)
		}
	}
	launch(finished, `printf '{"harvested":1}' > "$NEXUS_OUTPUT_FILE"`)
	launch(running, `i=0; while [ $i -lt 3000 ]; do sleep 0.1; i=$((i+1)); done`)
	previous.Close()

	deadline := time.Now().Add(90 * time.Second)
	for {
		j, err := cs.BatchV1().Jobs(k8stest.Namespace).Get(context.Background(), "nexus-"+finished, metav1.GetOptions{})
		if err == nil && (j.Status.Succeeded > 0 || j.Status.Failed > 0) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the finished attempt never finished")
		}
		time.Sleep(500 * time.Millisecond)
	}
	awaitPodRunning(t, cs, running)

	ag.reattachKubernetes(context.Background())

	c.mu.Lock()
	var got *nexusv1.ReportResultRequest
	for _, res := range c.results {
		if res.GetAttemptId() == finished {
			got = res
		}
		if res.GetAttemptId() == running {
			t.Errorf("a still-running attempt was reported %v; it must be left for INDETERMINATE", res.GetOutcome())
		}
	}
	c.mu.Unlock()
	if got == nil || got.GetOutcome() != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		t.Fatalf("finished attempt report = %v; want its real SUCCEEDED outcome, not INDETERMINATE", got)
	}
	if string(got.GetOutputs()["result"].GetInline()) != `{"harvested":1}` {
		t.Errorf("harvested output = %v", got.GetOutputs())
	}
	if got.GetLeaseToken() != "tok-"+finished {
		t.Errorf("reported under token %q; must use the attempt's original lease token", got.GetLeaseToken())
	}
	if _, ok, _ := w.Get(finished); ok {
		t.Error("the harvested attempt's WAL entry was not drained after the ack")
	}
	if e, ok, _ := w.Get(running); !ok || e.State != wal.StateStarting {
		t.Error("the fenced attempt must stay STARTING so replay reports INDETERMINATE")
	}
	assertAttemptGone(t, cs, finished, 60*time.Second)
	assertAttemptGone(t, cs, running, 60*time.Second)
	if _, err := cs.BatchV1().Jobs(k8stest.Namespace).Get(context.Background(), "nexus-"+running, metav1.GetOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("unexpected error: %v", err)
	}
}
