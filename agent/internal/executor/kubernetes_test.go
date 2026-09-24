// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

// Unit tests for the kubernetes executor. Pure functions are tested directly;
// the lifecycle is driven against client-go's fake clientset, with the test
// playing the parts of the Job controller and the kubelet (creating the pod,
// moving its status). No cluster is needed; the real-cluster tests are in
// kubernetes_integration_test.go.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

const fakeNS = "nexus-unit"

func newFakeExecutor(t *testing.T, objs ...runtime.Object) (*KubernetesExecutor, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset(objs...)
	e := NewKubernetesExecutor(KubernetesConfig{
		Namespace: fakeNS, Owner: "agent-0",
		GPUResourceName: "nvidia.com/gpu", GPUKindLabel: "nvidia.com/gpu.product",
		NodeSelector:     map[string]string{"pool": "tasks"},
		Tolerations:      []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}},
		OwnerPodName:     "nexus-agent-abc", OwnerPodUID: "uid-agent",
		StartTimeout:     10 * time.Second,
		ImagePullTimeout: 5 * time.Second,
	}, cs, t.TempDir(), discardLog())
	t.Cleanup(e.Close)
	return e, cs
}

func unitSpec(attemptID string) TaskSpec {
	return TaskSpec{
		AttemptID: attemptID, RunID: "run-1", NodeID: "node-1", IdempotencyKey: "idem-1",
		Runtime: &nexusv1.RuntimeSpec{
			Image: "registry.example/task:1", Command: []string{"/bin/sh", "-c"}, Args: []string{"true"},
			Env: map[string]string{"LOG_LEVEL": "debug", EnvAttemptID: "forged-by-task"},
		},
		Limit: &nexusv1.ResourceVector{CpuMillis: 1500, MemoryBytes: 256 << 20, DiskBytes: 1 << 30,
			GpuCount: 1, GpuKind: "nvidia-a100"},
		DeclaredOutputs:      []string{"model"},
		StartToCloseDeadline: time.Now().Add(10 * time.Minute),
		GraceAfterTerm:       7 * time.Second,
		WorkerID:             "worker-42",
	}
}

// ── names ───────────────────────────────────────────────────────────────────

func TestK8sJobNameIsDeterministicValidAndCollisionSafe(t *testing.T) {
	uuid := "3f2c1a9e-8d7b-4c6a-9e5f-0a1b2c3d4e5f"
	if got := k8sJobName(uuid); got != "nexus-"+uuid {
		t.Errorf("a DNS-safe attempt id should be used verbatim, got %q", got)
	}
	if k8sJobName(uuid) != k8sJobName(uuid) {
		t.Fatal("job name is not deterministic; re-attach depends on it")
	}
	for _, id := range []string{"Attempt-UPPER", "attempt_under", strings.Repeat("a", 80), "a/b", "-lead"} {
		n := k8sJobName(id)
		if !k8sDNSLabel.MatchString(n) || len(k8sSecretName(n)) > 63 {
			t.Errorf("k8sJobName(%q) = %q is not a valid name (or its secret name exceeds 63)", id, n)
		}
	}
	// Ids that differ only in case must NOT share a Job, or one attempt would
	// adopt another's workload.
	if k8sJobName("Abc") == k8sJobName("abc") {
		t.Error("case-differing attempt ids collided")
	}
	if v := k8sLabelValue(strings.Repeat("x", 100)); len(v) > 63 || !k8sLabelValueRE.MatchString(v) {
		t.Errorf("label value %q invalid", v)
	}
}

// ── pod spec: security, resources, placement ────────────────────────────────

func TestK8sBuildJobIsRestrictedAndEnforcesLimits(t *testing.T) {
	e, _ := newFakeExecutor(t)
	spec := unitSpec("sec-1")
	spec.Inputs = map[string]*nexusv1.ResultRef{"data": {Kind: &nexusv1.ResultRef_Inline{Inline: []byte("x")}}}
	job, secret, err := e.buildJob(spec, spec.WorkerID)
	if err != nil {
		t.Fatalf("buildJob: %v", err)
	}
	js := job.Spec
	if *js.BackoffLimit != 0 {
		t.Error("backoffLimit must be 0: Nexus owns retries")
	}
	if js.PodReplacementPolicy == nil || *js.PodReplacementPolicy != batchv1.Failed {
		t.Error("podReplacementPolicy must be Failed")
	}
	if js.ActiveDeadlineSeconds == nil || *js.ActiveDeadlineSeconds < 590 || *js.ActiveDeadlineSeconds > 600 {
		t.Errorf("activeDeadlineSeconds = %v, want ~600 from start_to_close", js.ActiveDeadlineSeconds)
	}
	if js.TTLSecondsAfterFinished == nil || *js.TTLSecondsAfterFinished <= 0 {
		t.Error("ttlSecondsAfterFinished must be set as the backstop for a crashed agent")
	}
	ps := js.Template.Spec
	if ps.RestartPolicy != corev1.RestartPolicyNever {
		t.Error("restartPolicy must be Never")
	}
	if ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken {
		t.Error("the service account token must not be mounted into task pods")
	}
	if ps.EnableServiceLinks == nil || *ps.EnableServiceLinks {
		t.Error("service links must be disabled")
	}
	if *ps.TerminationGracePeriodSeconds != 7 {
		t.Errorf("terminationGracePeriodSeconds = %d, want the task grace 7", *ps.TerminationGracePeriodSeconds)
	}
	psc := ps.SecurityContext
	if psc == nil || !*psc.RunAsNonRoot || *psc.RunAsUser == 0 || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context not restricted: %+v", psc)
	}
	for _, c := range append(append([]corev1.Container{}, ps.InitContainers...), ps.Containers...) {
		sc := c.SecurityContext
		if sc == nil || *sc.AllowPrivilegeEscalation || *sc.Privileged || !*sc.RunAsNonRoot ||
			len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" ||
			sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || !*sc.ReadOnlyRootFilesystem {
			t.Errorf("container %s is not restricted: %+v", c.Name, sc)
		}
	}
	if len(ps.InitContainers) != 1 || ps.InitContainers[0].RestartPolicy == nil ||
		*ps.InitContainers[0].RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Fatal("the collector must be a native sidecar (init container, restartPolicy Always)")
	}

	task := ps.Containers[0]
	want := map[corev1.ResourceName]string{
		corev1.ResourceCPU: "1500m", corev1.ResourceMemory: "256Mi",
		corev1.ResourceEphemeralStorage: "1Gi", "nvidia.com/gpu": "1",
	}
	for name, q := range want {
		l, r := task.Resources.Limits[name], task.Resources.Requests[name]
		if l.Cmp(resource.MustParse(q)) != 0 || r.Cmp(l) != 0 {
			t.Errorf("%s: request=%s limit=%s, want both %s (requests==limits)", name, r.String(), l.String(), q)
		}
	}
	if ps.NodeSelector["pool"] != "tasks" || ps.NodeSelector["nvidia.com/gpu.product"] != "nvidia-a100" {
		t.Errorf("nodeSelector = %v, want worker pool + gpu kind", ps.NodeSelector)
	}
	if len(ps.Tolerations) != 1 || ps.Tolerations[0].Key != "dedicated" {
		t.Errorf("tolerations = %v", ps.Tolerations)
	}

	env := map[string]string{}
	for _, v := range task.Env {
		env[v.Name] = v.Value
	}
	if env[EnvAttemptID] != "sec-1" {
		t.Errorf("task env overrode %s (%q): platform variables must win", EnvAttemptID, env[EnvAttemptID])
	}
	if env["LOG_LEVEL"] != "debug" || env[EnvOutputFile] != "/nexus/outputs/model" || env[EnvIdempotencyKey] != "idem-1" {
		t.Errorf("env = %v", env)
	}

	for _, l := range []string{k8sLabelAttempt, k8sLabelRun, k8sLabelNode, k8sLabelWorker, k8sLabelOwner} {
		if job.Labels[l] == "" || js.Template.Labels[l] == "" {
			t.Errorf("label %s missing on job or pod template", l)
		}
	}
	if job.Annotations[k8sAnnAttemptID] != "sec-1" || job.Annotations[k8sAnnDeclaredOutputs] != `["model"]` {
		t.Errorf("annotations = %v", job.Annotations)
	}
	if !strings.Contains(job.Annotations[k8sAnnEnforced], "memory=cgroup-oom-kill") {
		t.Errorf("enforced annotation = %q", job.Annotations[k8sAnnEnforced])
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Kind != "Pod" || *job.OwnerReferences[0].BlockOwnerDeletion {
		t.Errorf("owner references = %+v; want the agent pod, non-blocking", job.OwnerReferences)
	}
	if secret == nil || string(secret.Data["data"]) != "x" || secret.Immutable == nil || !*secret.Immutable {
		t.Errorf("inputs secret = %+v", secret)
	}
}

func TestK8sGPUWithoutDevicePluginIsRefused(t *testing.T) {
	e, _ := newFakeExecutor(t)
	e.Config.GPUResourceName = ""
	_, _, err := e.buildJob(unitSpec("gpu-1"), "")
	if !errors.Is(err, ErrLimitUnenforceable) {
		t.Fatalf("err = %v, want ErrLimitUnenforceable (never run a GPU task without a GPU)", err)
	}
}

func TestK8sInputLimits(t *testing.T) {
	_, err := k8sInputData(map[string]*nexusv1.ResultRef{
		"big": {Kind: &nexusv1.ResultRef_Inline{Inline: bytes.Repeat([]byte("x"), k8sMaxInputBytes+1)}},
	})
	if c := ClassifyError(err); c.Class != nexusv1.ErrorClass_ERROR_CLASS_VALIDATION {
		t.Errorf("oversized input: class %v (%v), want VALIDATION", c.Class, err)
	}
	_, err = k8sInputData(map[string]*nexusv1.ResultRef{
		"blob": {Kind: &nexusv1.ResultRef_Blob{Blob: &nexusv1.BlobRef{Bucket: "b", Key: "k"}}},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("unfetched blob: %v, want ErrValidation", err)
	}
}

// ── output transport ────────────────────────────────────────────────────────

func frame(rel string, b []byte) string {
	return fmt.Sprintf("%s %d %s\n%s\n\n%s\n", k8sFrameBegin, len(b), rel,
		base64.StdEncoding.EncodeToString(b), k8sFrameEnd)
}

func TestParseCollectorStream(t *testing.T) {
	bin := []byte{0, 1, 0xff, '\n', 0}
	stream := k8sFrameReady + "\n" + frame("output.json", []byte(`{"a":1}`)) +
		frame("outputs/model", bin) + frame("outputs/empty", nil) +
		k8sFrameTooBig + " 99999999 outputs/huge\n" + k8sFrameComplete + "\n"
	c, err := parseCollectorStream([]byte(stream))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !c.Complete || !bytes.Equal(c.Files["outputs/model"], bin) || string(c.Files["output.json"]) != `{"a":1}` {
		t.Errorf("parsed = %+v", c)
	}
	if b, ok := c.Files["outputs/empty"]; !ok || len(b) != 0 {
		t.Error("an empty output must be present and empty, not absent")
	}
	if len(c.TooBig) != 1 || c.TooBig[0] != "outputs/huge" {
		t.Errorf("TooBig = %v", c.TooBig)
	}

	legacy, ct, named := outputsFromCollected(c, []string{"model", "missing"})
	if string(legacy) != `{"a":1}` || ct != "application/json" || !bytes.Equal(named["model"].Bytes, bin) {
		t.Errorf("outputsFromCollected: %q %q %v", legacy, ct, named)
	}
	if _, ok := named["missing"]; ok {
		t.Error("an absent output must stay absent")
	}

	// Truncation must never produce a Complete result.
	cut := stream[:strings.Index(stream, k8sFrameEnd)]
	if c, _ := parseCollectorStream([]byte(cut + k8sFrameComplete + "\n")); c.Complete {
		t.Error("an unterminated frame was accepted as a complete transfer")
	}
	if c, _ := parseCollectorStream([]byte(frame("output.json", []byte("x")))); c.Complete {
		t.Error("a dump without the completion marker was accepted as complete")
	}
	bad := strings.Replace(frame("outputs/x", []byte("abc")), " 3 ", " 4 ", 1) + k8sFrameComplete + "\n"
	if _, err := parseCollectorStream([]byte(bad)); err == nil {
		t.Error("a size mismatch must be an error, not a silently different output")
	}
}

// Runs the REAL collector script under the local /bin/sh, with its volume
// paths pointed at temp dirs, and SIGTERMs it exactly as the kubelet would.
// This proves the script and the parser agree, without a cluster.
func TestK8sCollectorScriptAgainstParser(t *testing.T) {
	if _, err := exec.LookPath("base64"); err != nil {
		t.Skip("no base64 binary")
	}
	work, outs := t.TempDir(), t.TempDir()
	must(t, os.WriteFile(filepath.Join(work, "output.json"), []byte(`{"ok":true}`), 0o644))
	must(t, os.WriteFile(filepath.Join(outs, "model"), []byte{0, 0xfe, 'z'}, 0o644))
	must(t, os.WriteFile(filepath.Join(outs, "big"), bytes.Repeat([]byte("b"), 2000), 0o644))

	cmd := exec.Command("/bin/sh", "-c", k8sCollectorScript)
	cmd.Env = append(os.Environ(), "NEXUS_K8S_COLLECT_WORK="+work, "NEXUS_K8S_COLLECT_OUTPUTS="+outs,
		"NEXUS_K8S_OUTPUT_CAP=1000")
	// A FILE, not a buffer: the script's background `sleep` inherits stdout
	// and outlives the shell (in a pod the container's exit kills it), so a
	// pipe-backed cmd.Wait would block on it. Process.Wait below measures the
	// shell alone, which is what the kubelet's grace period measures.
	outPath := filepath.Join(t.TempDir(), "collector.out")
	outF, err := os.Create(outPath)
	must(t, err)
	defer outF.Close()
	cmd.Stdout = outF
	must(t, cmd.Start())
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(outPath); bytes.Contains(b, []byte(k8sFrameReady)) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	must(t, cmd.Process.Signal(syscall.SIGTERM))
	done := make(chan error, 1)
	go func() { _, err := cmd.Process.Wait(); done <- err }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("collector did not exit promptly on SIGTERM (trap deferred behind sleep?)")
	}
	out, err := os.ReadFile(outPath)
	must(t, err)
	c, err := parseCollectorStream(out)
	if err != nil {
		t.Fatalf("parse real collector output: %v\n%s", err, out)
	}
	if !c.Complete || string(c.Files["output.json"]) != `{"ok":true}` || !bytes.Equal(c.Files["outputs/model"], []byte{0, 0xfe, 'z'}) {
		t.Errorf("collected = %+v", c)
	}
	if len(c.TooBig) != 1 || c.TooBig[0] != "outputs/big" {
		t.Errorf("cap not applied: TooBig = %v", c.TooBig)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ── status classification ───────────────────────────────────────────────────

func waitingPod(reason, msg string) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{Name: k8sTaskContainer, Image: "img:1",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}}}}}
}

func TestK8sStartVerdictClassifiesImagePulls(t *testing.T) {
	cases := []struct {
		name  string
		pod   *corev1.Pod
		v     k8sStartVerdict
		class nexusv1.ErrorClass
	}{
		{"not found", waitingPod("ErrImagePull", `failed to resolve reference "docker.io/library/nope:v0": pull access denied, repository does not exist or may require authorization`),
			k8sStartFailed, nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND},
		{"manifest unknown", waitingPod("ErrImagePull", "manifest unknown"), k8sStartFailed, nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND},
		{"unauthorized", waitingPod("ErrImagePull", "401 Unauthorized"), k8sStartFailed, nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED},
		{"invalid", waitingPod("InvalidImageName", "bad ref"), k8sStartFailed, nexusv1.ErrorClass_ERROR_CLASS_VALIDATION},
		{"transient", waitingPod("ErrImagePull", "dial tcp: i/o timeout"), k8sStartWaiting, 0},
		{"backoff", waitingPod("ImagePullBackOff", "Back-off pulling image"), k8sStartWaiting, 0},
		{"creating", waitingPod("ContainerCreating", ""), k8sStartWaiting, 0},
	}
	for _, c := range cases {
		obs := startVerdict(nil, c.pod)
		if obs.Verdict != c.v {
			t.Errorf("%s: verdict %v, want %v", c.name, obs.Verdict, c.v)
			continue
		}
		if c.v == k8sStartFailed {
			if got := ClassifyError(obs.Err).Class; got != c.class {
				t.Errorf("%s: class %v, want %v (%v)", c.name, got, c.class, obs.Err)
			}
		}
	}
	// The collector's own pull failure is OUR problem: never NOT_FOUND.
	p := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
		Name: k8sCollectorContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "ErrImagePull", Message: "not found"}}}}}}
	if obs := startVerdict(nil, p); obs.Verdict != k8sStartWaiting || !obs.Pulling {
		t.Errorf("collector pull failure verdict = %+v, want waiting (bounded by the pull timeout)", obs)
	}
}

func terminatedPod(phase corev1.PodPhase, code int32, reason, msg string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Status: corev1.PodStatus{Phase: phase,
		ContainerStatuses: []corev1.ContainerStatus{{Name: k8sTaskContainer, State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason, Message: msg}}}}}}
}

func TestK8sOutcomeFromStatus(t *testing.T) {
	deadlineJob := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded}}}}
	evicted := terminatedPod(corev1.PodFailed, 137, "Error", "")
	evicted.Status.Reason, evicted.Status.Message = "Evicted", "The node was low on resource: memory."
	disrupted := terminatedPod(corev1.PodFailed, 143, "Error", "")
	disrupted.Status.Conditions = []corev1.PodCondition{{Type: corev1.DisruptionTarget,
		Status: corev1.ConditionTrue, Reason: "EvictionByEvictionAPI"}}

	cases := []struct {
		name      string
		job       *batchv1.Job
		pod       *corev1.Pod
		signalled bool
		class     nexusv1.ErrorClass
		success   bool
	}{
		{"success", nil, terminatedPod(corev1.PodSucceeded, 0, "Completed", ""), false, 0, true},
		{"app error", nil, terminatedPod(corev1.PodFailed, 3, "Error", ""), false, nexusv1.ErrorClass_ERROR_CLASS_APPLICATION, false},
		{"oom", nil, terminatedPod(corev1.PodFailed, 137, "OOMKilled", ""), false, nexusv1.ErrorClass_ERROR_CLASS_OOM, false},
		{"deadline", deadlineJob, terminatedPod(corev1.PodFailed, 143, "Error", ""), false, nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT, false},
		{"deadline no pod", deadlineJob, nil, false, nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT, false},
		// 1.31+: FailureTarget precedes Failed while the pod is terminated.
		{"deadline failure-target", &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded}}}},
			terminatedPod(corev1.PodFailed, 137, "Error", ""), false, nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT, false},
		{"evicted", nil, evicted, false, nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE, false},
		{"disruption", nil, disrupted, false, nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE, false},
		{"missing command", nil, terminatedPod(corev1.PodFailed, 128, "StartError",
			`exec: "/nope": stat /nope: no such file or directory`), false, nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND, false},
		{"no status", nil, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}}, false,
			nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE, false},
	}
	for _, c := range cases {
		o := outcomeFromStatus(c.job, c.pod, c.signalled)
		if o.Succeeded() != c.success {
			t.Errorf("%s: succeeded=%v, want %v (%+v)", c.name, o.Succeeded(), c.success, o)
			continue
		}
		if !c.success {
			if got := ClassifiedFromOutcome(o, "").Class; got != c.class {
				t.Errorf("%s: class %v, want %v (%+v, err=%v)", c.name, got, c.class, o, o.Err)
			}
		}
	}
	// A pod the AGENT deleted before it ran has no exit code and is not an
	// infrastructure fault; the caller already knows why it stopped.
	if o := outcomeFromStatus(nil, &corev1.Pod{}, true); o.Err != nil || !o.Signalled {
		t.Errorf("agent-signalled pod with no status = %+v", o)
	}
}

// ── lifecycle against the fake clientset ────────────────────────────────────

// kubelet plays the node: it creates the Job's pod and moves it through states.
type kubelet struct {
	t   *testing.T
	cs  *fake.Clientset
	job *batchv1.Job
	pod *corev1.Pod
}

func awaitJob(t *testing.T, cs *fake.Clientset, name string) *batchv1.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, err := cs.BatchV1().Jobs(fakeNS).Get(context.Background(), name, metav1.GetOptions{}); err == nil {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s was never created", name)
	return nil
}

func (k *kubelet) createPod() {
	k.pod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: k.job.Name + "-pod", Namespace: fakeNS, Labels: k.job.Spec.Template.Labels},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	if _, err := k.cs.CoreV1().Pods(fakeNS).Create(context.Background(), k.pod, metav1.CreateOptions{}); err != nil {
		k.t.Fatalf("create pod: %v", err)
	}
}

func (k *kubelet) setStatus(s corev1.PodStatus) {
	k.pod.Status = s
	if _, err := k.cs.CoreV1().Pods(fakeNS).UpdateStatus(context.Background(), k.pod, metav1.UpdateOptions{}); err != nil {
		k.t.Fatalf("update pod status: %v", err)
	}
}

func runningStatus() corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: k8sTaskContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}
}

func doneStatus(code int32) corev1.PodStatus {
	phase := corev1.PodSucceeded
	if code != 0 {
		phase = corev1.PodFailed
	}
	return corev1.PodStatus{Phase: phase,
		InitContainerStatuses: []corev1.ContainerStatus{{Name: k8sCollectorContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: k8sTaskContainer, State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: code}}}}}
}

func cannedLogs(collector string) func(context.Context, string, string, bool) (io.ReadCloser, error) {
	return func(_ context.Context, _, container string, _ bool) (io.ReadCloser, error) {
		if container == k8sCollectorContainer {
			return io.NopCloser(strings.NewReader(collector)), nil
		}
		return io.NopCloser(strings.NewReader("task log line\n")), nil
	}
}

func TestK8sLifecycleSuccessWithOutputsAndPhases(t *testing.T) {
	e, cs := newFakeExecutor(t)
	e.logStream = cannedLogs(frame("outputs/model", []byte("weights")) + k8sFrameComplete + "\n")
	spec := unitSpec("life-1")
	spec.Inputs = map[string]*nexusv1.ResultRef{"in": {Kind: &nexusv1.ResultRef_Inline{Inline: []byte("v")}}}
	var mu sync.Mutex
	var phases []string
	spec.OnPhase = func(p, _ string) { mu.Lock(); phases = append(phases, p); mu.Unlock() }

	type res struct {
		p   Prepared
		err error
	}
	ch := make(chan res, 1)
	go func() { p, err := e.Prepare(context.Background(), spec); ch <- res{p, err} }()

	k := &kubelet{t: t, cs: cs, job: awaitJob(t, cs, k8sJobName("life-1"))}
	k.createPod()
	k.setStatus(runningStatus())
	r := <-ch
	if r.err != nil {
		t.Fatalf("Prepare: %v", r.err)
	}
	sec, err := cs.CoreV1().Secrets(fakeNS).Get(context.Background(), k8sSecretName(k.job.Name), metav1.GetOptions{})
	if err != nil || len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Kind != "Job" {
		t.Fatalf("inputs secret missing or not Job-owned: %v %+v", err, sec)
	}
	h, err := e.Start(context.Background(), r.p)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	k.setStatus(doneStatus(0))
	o, err := e.Wait(context.Background(), h)
	if err != nil || !o.Succeeded() {
		t.Fatalf("Wait: %+v err=%v oerr=%v", o, err, o.Err)
	}
	if string(o.Outputs["model"].Bytes) != "weights" {
		t.Errorf("outputs = %v", o.Outputs)
	}
	mu.Lock()
	got := strings.Join(phases, ",")
	mu.Unlock()
	if !strings.Contains(got, "Pending") || !strings.Contains(got, "Running") || !strings.Contains(got, "Succeeded") {
		t.Errorf("phases reported = %q, want Pending, Running and Succeeded", got)
	}

	for i := 0; i < 3; i++ {
		if err := e.Cleanup(context.Background(), h); err != nil {
			t.Fatalf("Cleanup #%d: %v", i+1, err)
		}
	}
	if _, err := cs.BatchV1().Jobs(fakeNS).Get(context.Background(), k.job.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("job survived Cleanup: %v", err)
	}
	if _, err := cs.CoreV1().Secrets(fakeNS).Get(context.Background(), sec.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("inputs secret survived Cleanup: %v", err)
	}
}

// Exit 0 with an incomplete collector dump must NOT be a success.
func TestK8sIncompleteOutputDumpIsInfrastructure(t *testing.T) {
	e, cs := newFakeExecutor(t)
	e.logStream = cannedLogs(frame("outputs/model", []byte("w"))) // no completion marker
	spec := unitSpec("life-2")
	done := make(chan Prepared, 1)
	go func() { p, _ := e.Prepare(context.Background(), spec); done <- p }()
	k := &kubelet{t: t, cs: cs, job: awaitJob(t, cs, k8sJobName("life-2"))}
	k.createPod()
	k.setStatus(runningStatus())
	h, _ := e.Start(context.Background(), <-done)
	k.setStatus(doneStatus(0))
	o, _ := e.Wait(context.Background(), h)
	if o.Succeeded() || ClassifyError(o.Err).Class != nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE {
		t.Errorf("outcome = %+v err=%v; a lost output must never be reported as success", o, o.Err)
	}
}

func TestK8sPrepareAdoptsExistingJobForSameAttempt(t *testing.T) {
	e, cs := newFakeExecutor(t)
	spec := unitSpec("adopt-1")
	existing, _, _ := e.buildJob(spec, "old-worker")
	existing.UID = "uid-original"
	if _, err := cs.BatchV1().Jobs(fakeNS).Create(context.Background(), existing, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	k := &kubelet{t: t, cs: cs, job: existing}
	k.createPod()
	k.setStatus(runningStatus())

	p, err := e.Prepare(context.Background(), spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !p.State.(*k8sState).adopted {
		t.Error("Prepare did not adopt the existing Job")
	}
	jobs, _ := cs.BatchV1().Jobs(fakeNS).List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 1 || jobs.Items[0].UID != "uid-original" {
		t.Errorf("jobs after re-Prepare = %d (uid %v); want the one original Job", len(jobs.Items), jobs.Items)
	}

	// A Job with the same name but ANOTHER attempt's annotation is never adopted.
	e2, cs2 := newFakeExecutor(t)
	foreign, _, _ := e2.buildJob(spec, "")
	foreign.Annotations[k8sAnnAttemptID] = "someone-else"
	_, _ = cs2.BatchV1().Jobs(fakeNS).Create(context.Background(), foreign, metav1.CreateOptions{})
	if _, err := e2.Prepare(context.Background(), spec); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Errorf("foreign job adopted? err = %v", err)
	}
}

func TestK8sPrepareImageNotFoundIsTerminalAndCleansUp(t *testing.T) {
	e, cs := newFakeExecutor(t)
	spec := unitSpec("pull-1")
	errc := make(chan error, 1)
	go func() { _, err := e.Prepare(context.Background(), spec); errc <- err }()
	k := &kubelet{t: t, cs: cs, job: awaitJob(t, cs, k8sJobName("pull-1"))}
	k.createPod()
	k.setStatus(waitingPod("ErrImagePull", "manifest unknown").Status)
	err := <-errc
	if c := ClassifyError(err); c.Class != nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND || c.Retryable {
		t.Fatalf("class = %v retryable=%v (%v), want terminal NOT_FOUND", c.Class, c.Retryable, err)
	}
	if _, err := cs.BatchV1().Jobs(fakeNS).Get(context.Background(), k.job.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("a failed Prepare left its Job behind; the runner never calls Cleanup for it")
	}
}

func TestK8sSignalDeletesPodWithGrace(t *testing.T) {
	e, cs := newFakeExecutor(t)
	spec := unitSpec("sig-1")
	done := make(chan Prepared, 1)
	go func() { p, _ := e.Prepare(context.Background(), spec); done <- p }()
	k := &kubelet{t: t, cs: cs, job: awaitJob(t, cs, k8sJobName("sig-1"))}
	k.createPod()
	k.setStatus(runningStatus())
	h, _ := e.Start(context.Background(), <-done)

	cs.ClearActions()
	if err := e.Signal(context.Background(), h, SignalTerm); err != nil {
		t.Fatalf("Signal TERM: %v", err)
	}
	var grace *int64
	for _, a := range cs.Actions() {
		if d, ok := a.(k8stesting.DeleteActionImpl); ok && d.GetResource().Resource == "pods" {
			grace = d.DeleteOptions.GracePeriodSeconds
		}
	}
	if grace == nil || *grace != 7 {
		t.Fatalf("TERM deleted the pod with grace %v, want the task's 7s (never 0: that is a force delete)", grace)
	}

	// Signalling an already-finished pod is a no-op. (The fake deleted the pod
	// outright on TERM; stand up a finished one in its place.)
	k.createPod()
	k.setStatus(doneStatus(0))
	cs.ClearActions()
	if err := e.Signal(context.Background(), h, SignalKill); err != nil {
		t.Fatalf("Signal KILL after exit: %v", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "delete" {
			t.Errorf("KILL issued a delete (%s) for a pod that had already finished", a.GetResource().Resource)
		}
	}
}

// Cancelling Wait's context (lease revoked) must terminate the pod: fencing.
func TestK8sWaitCancellationTerminatesPod(t *testing.T) {
	e, cs := newFakeExecutor(t)
	e.logStream = cannedLogs(k8sFrameComplete + "\n")
	spec := unitSpec("cancel-1")
	done := make(chan Prepared, 1)
	go func() { p, _ := e.Prepare(context.Background(), spec); done <- p }()
	k := &kubelet{t: t, cs: cs, job: awaitJob(t, cs, k8sJobName("cancel-1"))}
	k.createPod()
	k.setStatus(runningStatus())
	h, _ := e.Start(context.Background(), <-done)

	ctx, cancel := context.WithCancel(context.Background())
	waited := make(chan Outcome, 1)
	go func() { o, _ := e.Wait(ctx, h); waited <- o }()
	time.Sleep(100 * time.Millisecond)
	cancel()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := cs.CoreV1().Pods(fakeNS).Get(context.Background(), k.pod.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			break // the fake deletes immediately; the kubelet would honour the grace
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelling Wait did not delete the attempt pod")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case o := <-waited:
		if o.Succeeded() {
			t.Errorf("a cancelled attempt reported success: %+v", o)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after cancellation")
	}
}

func TestK8sReapOrphansOnlyTouchesOwnJobs(t *testing.T) {
	e, cs := newFakeExecutor(t)
	mine, _, _ := e.buildJob(unitSpec("orphan-mine"), "")
	keep, _, _ := e.buildJob(unitSpec("orphan-keep"), "")
	e.Config.Owner = "agent-1"
	sibling, _, _ := e.buildJob(unitSpec("orphan-sibling"), "")
	e.Config.Owner = "agent-0"
	for _, j := range []*batchv1.Job{mine, keep, sibling} {
		if _, err := cs.BatchV1().Jobs(fakeNS).Create(context.Background(), j, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := e.ReapOrphans(context.Background(), func(id string) bool { return id == "orphan-keep" })
	if err != nil || n != 1 {
		t.Fatalf("ReapOrphans = %d, %v; want exactly 1", n, err)
	}
	left, _ := cs.BatchV1().Jobs(fakeNS).List(context.Background(), metav1.ListOptions{})
	names := map[string]bool{}
	for _, j := range left.Items {
		names[j.Annotations[k8sAnnAttemptID]] = true
	}
	if names["orphan-mine"] || !names["orphan-keep"] || !names["orphan-sibling"] {
		t.Errorf("remaining = %v; must reap only this owner's unkept Jobs, never a sibling agent's", names)
	}
}

func TestK8sProbe(t *testing.T) {
	e, cs := newFakeExecutor(t)
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "v1.34.3"}
	allowed := true
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		r := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		r.Status.Allowed = allowed
		return true, r, nil
	})
	if err := e.Probe(context.Background()); err != nil {
		t.Fatalf("Probe on a healthy cluster: %v", err)
	}
	allowed = false
	if err := e.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "may not") {
		t.Errorf("Probe with missing RBAC = %v, want a refusal naming the verb", err)
	}
	allowed = true
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "v1.27.0"}
	if err := e.Probe(context.Background()); err == nil {
		t.Error("Probe accepted a server without native sidecars")
	}
	if err := (&KubernetesExecutor{}).Probe(context.Background()); err == nil {
		t.Error("Probe with no client must fail")
	}
}

func TestLoadKubernetesConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, on, err := LoadKubernetesConfigFromEnv(env(nil)); on || err != nil {
		t.Errorf("no namespace must mean disabled, got on=%v err=%v", on, err)
	}
	cfg, on, err := LoadKubernetesConfigFromEnv(env(map[string]string{
		"NEXUS_K8S_NAMESPACE":          "tasks",
		"NEXUS_K8S_NODE_SELECTOR":      "pool=gpu, zone=a",
		"NEXUS_K8S_TOLERATIONS":        "dedicated=nexus:NoSchedule,spot:NoExecute",
		"NEXUS_K8S_IMAGE_PULL_SECRETS": "regcred, other",
		"NEXUS_K8S_START_TIMEOUT":      "3m",
		"NEXUS_K8S_RUN_AS_USER":        "1000",
	}))
	if err != nil || !on {
		t.Fatalf("load: on=%v err=%v", on, err)
	}
	if cfg.NodeSelector["zone"] != "a" || len(cfg.Tolerations) != 2 ||
		cfg.Tolerations[0].Operator != corev1.TolerationOpEqual || cfg.Tolerations[1].Operator != corev1.TolerationOpExists ||
		len(cfg.ImagePullSecrets) != 2 || cfg.StartTimeout != 3*time.Minute || cfg.RunAsUser != 1000 ||
		cfg.HelperImage != DefaultK8sHelperImage {
		t.Errorf("cfg = %+v", cfg)
	}
	for k, v := range map[string]string{
		"NEXUS_K8S_RUN_AS_USER": "0", "NEXUS_K8S_TOLERATIONS": "x:Sometimes",
		"NEXUS_K8S_IMAGE_PULL_POLICY": "Maybe", "NEXUS_K8S_START_TIMEOUT": "soon",
	} {
		if _, _, err := LoadKubernetesConfigFromEnv(env(map[string]string{"NEXUS_K8S_NAMESPACE": "t", k: v})); err == nil {
			t.Errorf("%s=%q accepted; a malformed config must fail at boot", k, v)
		}
	}
}
