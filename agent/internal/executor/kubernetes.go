// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// KubernetesKind is the executor identifier advertised at registration and
// matched against runtime.executor.
const KubernetesKind = "kubernetes"

// KubernetesKindAlias is the spelling the workflow schema, the Python SDK and
// the proto comment used before this executor existed ("k8s"). The agent
// advertises BOTH so a spec written against the older spelling routes here
// instead of sitting PENDING forever for want of a worker that says "k8s".
const KubernetesKindAlias = "k8s"

// KubernetesExecutor runs each attempt as a Kubernetes Job (ADR-0007, Phase 3).
//
// One Job per ATTEMPT, never per task: a Nexus retry is a new attempt with a
// new id, a new lease and a new Job, and the Job itself never retries
// (backoffLimit 0). Kubernetes is the substrate; Nexus remains the only
// retry policy, the only source of error classes, and the system of record.
//
// What the cluster enforces, precisely (see also KubernetesFidelity and F92):
//   - memory: cgroup memory.max from limits.memory; an over-limit task is
//     OOM-killed by the kernel and the runtime reports reason OOMKilled.
//   - cpu: CFS quota from limits.cpu (throttling, never a kill).
//   - GPU: whole devices via the configured device-plugin resource; isolation
//     is the device plugin's.
//   - disk: ephemeral-storage limit, enforced by kubelet EVICTION on a
//     sampling interval — softer than the rest, and reported as an eviction.
//   - start_to_close: Job activeDeadlineSeconds, so it holds with the agent dead.
//   - custom resources: NOT enforced; they are Nexus ledger entries only.
type KubernetesExecutor struct {
	Config KubernetesConfig
	Client kubernetes.Interface
	// Root holds the per-attempt local scratch directory. The workload never
	// sees it (it runs in a pod); it exists so Prepared/Handle carry a real
	// WorkDir with the same lifecycle as every other executor's.
	Root string
	Log  *slog.Logger

	// logStream is swappable for tests: the fake clientset's GetLogs returns a
	// fixed body and cannot model the collector's output frames.
	logStream func(ctx context.Context, pod, container string, follow bool) (io.ReadCloser, error)

	trackerMu   sync.Mutex
	tracker     *k8sTracker
	stopTracker context.CancelFunc
}

// NewKubernetesExecutor builds the executor. client may be nil, in which case
// the executor reports Kind but refuses work — which is what lets the
// conformance suite check Kind() on a machine with no cluster.
func NewKubernetesExecutor(cfg KubernetesConfig, client kubernetes.Interface, root string, log *slog.Logger) *KubernetesExecutor {
	cfg.applyDefaults()
	return &KubernetesExecutor{Config: cfg, Client: client, Root: root, Log: log}
}

// KubernetesFidelity reports what the cluster enforces; the conformance suite
// holds the executor to it.
func KubernetesFidelity() LimitFidelity {
	return LimitFidelity{Memory: "hard", CPU: "cfs-quota", GPU: "device-isolation"}
}

func (e *KubernetesExecutor) Kind() string { return KubernetesKind }

func (e *KubernetesExecutor) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// Close stops the informers. Safe to call more than once.
func (e *KubernetesExecutor) Close() {
	e.trackerMu.Lock()
	defer e.trackerMu.Unlock()
	if e.stopTracker != nil {
		e.stopTracker()
		e.stopTracker = nil
		e.tracker = nil
	}
}

// ensureTracker starts the label-scoped informers once. A failed start is NOT
// cached: a transient API outage at the first task must not disable the
// executor for the life of the agent.
func (e *KubernetesExecutor) ensureTracker(ctx context.Context) (*k8sTracker, error) {
	if e.Client == nil {
		return nil, fmt.Errorf("%w: kubernetes executor has no client (is NEXUS_K8S_NAMESPACE set and the cluster reachable?)",
			ErrRuntimeUnavailable)
	}
	e.trackerMu.Lock()
	defer e.trackerMu.Unlock()
	if e.tracker != nil {
		return e.tracker, nil
	}
	t := newK8sTracker()
	factory := informers.NewSharedInformerFactoryWithOptions(e.Client, 0,
		informers.WithNamespace(e.Config.Namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = e.ownerSelector() }))
	podInf := factory.Core().V1().Pods().Informer()
	jobInf := factory.Batch().V1().Jobs().Informer()
	if _, err := podInf.AddEventHandler(t.podHandlers()); err != nil {
		return nil, fmt.Errorf("%w: register pod handler: %v", ErrRuntimeUnavailable, err)
	}
	if _, err := jobInf.AddEventHandler(t.jobHandlers()); err != nil {
		return nil, fmt.Errorf("%w: register job handler: %v", ErrRuntimeUnavailable, err)
	}
	// The informers outlive any one attempt, so they run on the executor's
	// own context, not the caller's.
	runCtx, cancel := context.WithCancel(context.Background())
	factory.Start(runCtx.Done())

	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), podInf.HasSynced, jobInf.HasSynced) {
		cancel()
		factory.Shutdown()
		return nil, fmt.Errorf("%w: kubernetes informers did not sync for namespace %q",
			ErrRuntimeUnavailable, e.Config.Namespace)
	}
	e.tracker = t
	e.stopTracker = func() { cancel(); factory.Shutdown() }
	return t, nil
}

// k8sState is the executor's per-attempt state, shared by Prepared and Handle.
type k8sState struct {
	attemptID string
	key       string // attempt label value; the tracker key
	jobName   string
	hasSecret bool
	declared  []string
	deadline  time.Time
	grace     time.Duration
	memLimit  int64
	workDir   string
	onPhase   func(phase, detail string)
	adopted   bool

	mu        sync.Mutex
	startedAt time.Time
	signalled bool
	lastPhase string

	followOnce   sync.Once
	followCancel context.CancelFunc
	followDone   chan struct{}
	followMu     sync.Mutex
	followBuf    []byte

	waitMu  sync.Mutex
	waited  bool
	outcome Outcome
}

func (st *k8sState) markSignalled() {
	st.mu.Lock()
	st.signalled = true
	st.mu.Unlock()
}

func (st *k8sState) wasSignalled() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.signalled
}

func (st *k8sState) setStarted(t time.Time) {
	st.mu.Lock()
	if st.startedAt.IsZero() {
		st.startedAt = t
	}
	st.mu.Unlock()
}

func (st *k8sState) started() time.Time {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.startedAt
}

// reportPhase forwards a CHANGED pod phase to OnPhase and the log. Only
// changes: repeating an unchanged "Pending: Unschedulable" every informer
// resync would flood ReportProgress for no new information.
func (e *KubernetesExecutor) reportPhase(st *k8sState, snap k8sSnapshot) {
	phase, detail := podPhaseSummary(snap.Job, snap.Pod)
	key := phase + "|" + detail
	st.mu.Lock()
	changed := key != st.lastPhase
	st.lastPhase = key
	st.mu.Unlock()
	if !changed {
		return
	}
	e.logger().Info("attempt pod phase", "attempt_id", st.attemptID, "job", st.jobName,
		"pod", podName(snap.Pod), "phase", phase, "detail", detail)
	if st.onPhase != nil {
		st.onPhase(phase, detail)
	}
}

// Prepare submits the Job and waits until the task container has STARTED (or
// the attempt has already finished).
//
// Why the Job is created here and not in Start: the contract (ADR-0007) puts
// image acquisition in Prepare so a missing image is reported as terminal
// NOT_FOUND rather than as an opaque start failure. On Kubernetes the image is
// pulled BY THE NODE, after scheduling, so the only way to learn that an image
// does not exist is to submit the pod and watch the kubelet try. Prepare is
// therefore "submit, schedule, pull, start"; Start only hands back the handle.
// The workload may already be running when Prepare returns, which is harmless:
// the deadline is absolute (control-plane time) and nothing in Start can fail.
func (e *KubernetesExecutor) Prepare(ctx context.Context, spec TaskSpec) (Prepared, error) {
	if spec.Runtime.GetImage() == "" {
		return Prepared{}, fmt.Errorf("%w: runtime.image is required for the kubernetes executor (attempt %s)",
			ErrValidation, spec.AttemptID)
	}
	if spec.StdoutOutput != "" {
		// The pod log API merges stdout and stderr into one stream (the split
		// is alpha-gated and silently ignored where disabled), so an output
		// captured from it would carry the task's stderr spliced into its
		// data. Refusing is honest; a corrupt output is not.
		return Prepared{}, fmt.Errorf("%w: runtime.stdout_output is not supported by the kubernetes executor "+
			"(the pod log API cannot separate stdout from stderr); write $NEXUS_OUTPUT_DIR/%s instead",
			ErrValidation, spec.StdoutOutput)
	}
	t, err := e.ensureTracker(ctx)
	if err != nil {
		return Prepared{}, err
	}

	job, secret, err := e.buildJob(spec, spec.WorkerID)
	if err != nil {
		return Prepared{}, err
	}
	workDir, err := makeWorkDir(e.Root, spec.AttemptID)
	if err != nil {
		return Prepared{}, err
	}
	st := &k8sState{
		attemptID: spec.AttemptID,
		key:       k8sLabelValue(spec.AttemptID),
		jobName:   job.Name,
		hasSecret: secret != nil,
		declared:  spec.DeclaredOutputs,
		deadline:  spec.StartToCloseDeadline,
		grace:     orDefaultGrace(spec.GraceAfterTerm),
		memLimit:  spec.Limit.GetMemoryBytes(),
		workDir:   workDir,
		onPhase:   spec.OnPhase,
	}
	log := e.logger().With("attempt_id", spec.AttemptID, "run_id", spec.RunID,
		"node_id", spec.NodeID, "job", job.Name, "namespace", e.Config.Namespace)

	created, adopted, err := e.createOrAdopt(ctx, job, spec.AttemptID)
	if err != nil {
		os.RemoveAll(workDir)
		return Prepared{}, err
	}
	st.adopted = adopted
	if adopted {
		log.Warn("re-attached to the existing Job for this attempt instead of launching a duplicate",
			"job_uid", created.UID)
	} else {
		log.Info("attempt Job created", "image", spec.Runtime.GetImage(),
			"enforced", job.Annotations[k8sAnnEnforced])
	}

	if secret != nil {
		// Owned by the Job, so the garbage collector removes it with the Job
		// even when the agent never gets to Cleanup.
		secret.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "batch/v1", Kind: "Job", Name: created.Name, UID: created.UID,
			BlockOwnerDeletion: ptrTo(false),
		}}
		if _, err := e.Client.CoreV1().Secrets(e.Config.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil &&
			!apierrors.IsAlreadyExists(err) {
			e.teardown(ctx, st, log)
			return Prepared{}, apiErr(fmt.Sprintf("create inputs secret %s", secret.Name), err)
		}
	}

	if err := e.awaitStart(ctx, t, st, log); err != nil {
		// Prepare failed AFTER the Job exists. The runner calls Cleanup only
		// after a successful Start, so Prepare must remove its own Job here or
		// every failed pull would leave a Job behind until its TTL.
		e.teardown(ctx, st, log)
		return Prepared{}, err
	}
	return Prepared{AttemptID: spec.AttemptID, WorkDir: workDir, State: st}, nil
}

// createOrAdopt creates the Job, or adopts the existing one for the SAME
// attempt. This is the double-launch guard: the name is a pure function of the
// attempt id, so the API server itself arbitrates "is this attempt already
// running" atomically — no agent-side bookkeeping can race it.
func (e *KubernetesExecutor) createOrAdopt(ctx context.Context, job *batchv1.Job, attemptID string) (*batchv1.Job, bool, error) {
	jobs := e.Client.BatchV1().Jobs(e.Config.Namespace)
	created, err := jobs.Create(ctx, job, metav1.CreateOptions{})
	if err == nil {
		return created, false, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil, false, apiErr("create job "+job.Name, err)
	}
	existing, gerr := jobs.Get(ctx, job.Name, metav1.GetOptions{})
	if gerr != nil {
		return nil, false, apiErr("get existing job "+job.Name, gerr)
	}
	if existing.Annotations[k8sAnnAttemptID] != attemptID {
		// Only reachable through a hash collision or a hand-made Job: adopting
		// it would report someone else's workload as this attempt's.
		return nil, false, fmt.Errorf("%w: job %s exists but belongs to attempt %q, not %q",
			ErrRuntimeUnavailable, job.Name, existing.Annotations[k8sAnnAttemptID], attemptID)
	}
	if existing.DeletionTimestamp != nil {
		return nil, false, fmt.Errorf("%w: job %s for this attempt is still being deleted", ErrRuntimeUnavailable, job.Name)
	}
	return existing, true, nil
}

// awaitStart blocks until the task container runs, the attempt is already
// terminal, or start-up fails.
func (e *KubernetesExecutor) awaitStart(ctx context.Context, t *k8sTracker, st *k8sState, log *slog.Logger) error {
	ch, unsub := t.subscribe(st.key)
	defer unsub()

	began := time.Now()
	var pullingSince time.Time
	var lastCause string
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	for {
		snap := t.snapshot(st.key)
		e.reportPhase(st, snap)
		obs := startVerdict(snap.Job, snap.Pod)
		switch obs.Verdict {
		case k8sStartStarted, k8sStartTerminal:
			st.setStarted(time.Now())
			return nil
		case k8sStartFailed:
			log.Warn("attempt failed to start", "error", obs.Err)
			return obs.Err
		}
		if snap.JobGone {
			return fmt.Errorf("%w: job %s was deleted before its pod started", ErrRuntimeUnavailable, st.jobName)
		}
		if obs.Cause != "" {
			lastCause = obs.Cause
		}
		if obs.Pulling {
			if pullingSince.IsZero() {
				pullingSince = time.Now()
			}
			if time.Since(pullingSince) > e.Config.ImagePullTimeout {
				return fmt.Errorf("%w: image pull did not succeed within %v: %s",
					ErrRuntimeUnavailable, e.Config.ImagePullTimeout, lastCause)
			}
		} else {
			pullingSince = time.Time{}
		}
		if time.Since(began) > e.Config.StartTimeout {
			phase, detail := podPhaseSummary(snap.Job, snap.Pod)
			// Retryable INFRASTRUCTURE: the cluster could not run it NOW
			// (capacity, quota, admission), which another worker or a later
			// retry may well be able to.
			return fmt.Errorf("%w: attempt pod did not start within %v (phase %s: %s; last cause: %s)",
				ErrRuntimeUnavailable, e.Config.StartTimeout, phase, detail, lastCause)
		}
		if !st.deadline.IsZero() && time.Now().After(st.deadline) {
			// Out of time before it even started. The Job's activeDeadline is
			// killing it; Wait reports that as TIMED_OUT, which is accurate.
			return nil
		}
		select {
		case <-ch:
		case <-tick.C:
			if time.Since(snap.LastSeen) > 30*time.Second {
				e.refresh(ctx, t, st)
			}
		case <-ctx.Done():
			return fmt.Errorf("prepare abandoned while waiting for the attempt pod: %w", ctx.Err())
		}
	}
}

// refresh reads the attempt's objects directly, bypassing the informer. Used
// only when the informer has been silent for a while, as insurance against a
// watch gap swallowing the event Wait is blocked on.
func (e *KubernetesExecutor) refresh(ctx context.Context, t *k8sTracker, st *k8sState) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	job, err := e.Client.BatchV1().Jobs(e.Config.Namespace).Get(rctx, st.jobName, metav1.GetOptions{})
	switch {
	case err == nil:
		t.onJob(job, false)
	case apierrors.IsNotFound(err):
		t.markJobGone(st.key)
	}
	pods, err := e.Client.CoreV1().Pods(e.Config.Namespace).List(rctx,
		metav1.ListOptions{LabelSelector: e.selectorForAttempt(st.attemptID)})
	if err != nil {
		return
	}
	if len(pods.Items) == 0 {
		t.markPodGone(st.key)
	}
	for i := range pods.Items {
		t.onPod(&pods.Items[i], false)
	}
}

// Start returns the handle for the pod Prepare already launched, and begins
// following the collector's log so outputs survive the pod being deleted.
func (e *KubernetesExecutor) Start(ctx context.Context, p Prepared) (Handle, error) {
	st, ok := p.State.(*k8sState)
	if !ok {
		return Handle{}, fmt.Errorf("%w: kubernetes: Start called with foreign Prepared", ErrValidation)
	}
	st.setStarted(time.Now())
	e.startCollectorFollow(st)
	return Handle{
		AttemptID:    p.AttemptID,
		ExecutorKind: e.Kind(),
		ID:           st.jobName,
		WorkDir:      p.WorkDir,
		StartedAt:    st.started(),
		State:        st,
	}, nil
}

// startCollectorFollow tails the collector sidecar's log for the life of the
// pod.
//
// Following from the start, rather than reading once at the end, is what
// makes outputs survive a pod the agent itself deletes (cancel, lease loss):
// the kubelet removes a deleted pod's containers, and their logs with them,
// moments after they exit. A follow stream receives the collector's dump as it
// is printed, before that happens.
func (e *KubernetesExecutor) startCollectorFollow(st *k8sState) {
	st.followOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		st.followCancel = cancel
		st.followDone = make(chan struct{})
		go func() {
			defer close(st.followDone)
			e.followCollector(ctx, st)
		}()
	})
}

func (e *KubernetesExecutor) followCollector(ctx context.Context, st *k8sState) {
	t, err := e.ensureTracker(ctx)
	if err != nil {
		return
	}
	for ctx.Err() == nil {
		snap := t.snapshot(st.key)
		if snap.Pod == nil || snap.PodGone {
			if snap.PodGone || snap.JobGone || jobFinished(snap.Job) {
				return
			}
			if !sleepCtx(ctx, 500*time.Millisecond) {
				return
			}
			continue
		}
		rc, err := e.openLogs(ctx, snap.Pod.Name, k8sCollectorContainer, true)
		if err != nil {
			// Typically "container is waiting to start"; retry until it runs
			// or the pod is finished.
			if collectorExited(snap.Pod) || podTerminal(snap.Pod) {
				return
			}
			if !sleepCtx(ctx, 500*time.Millisecond) {
				return
			}
			continue
		}
		// Each (re)open returns the WHOLE log from the start, so the buffer is
		// replaced rather than appended: a stream dropped mid-dump and
		// reopened yields one complete copy, not a spliced one.
		buf := newBoundedBuffer(k8sMaxOutputBytes*2 + 64<<10)
		_, _ = io.Copy(buf, rc)
		rc.Close()
		st.followMu.Lock()
		st.followBuf = buf.Bytes()
		st.followMu.Unlock()

		snap = t.snapshot(st.key)
		if snap.Pod == nil || snap.PodGone || collectorExited(snap.Pod) || podTerminal(snap.Pod) {
			return
		}
		// The stream ended while the collector is still alive (API server
		// restart, connection reset). Reopen.
		if !sleepCtx(ctx, time.Second) {
			return
		}
	}
}

func collectorExited(pod *corev1.Pod) bool {
	cs := findContainerStatus(pod.Status.InitContainerStatuses, k8sCollectorContainer)
	return cs != nil && cs.State.Terminated != nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (e *KubernetesExecutor) openLogs(ctx context.Context, pod, container string, follow bool) (io.ReadCloser, error) {
	if e.logStream != nil {
		return e.logStream(ctx, pod, container, follow)
	}
	return e.Client.CoreV1().Pods(e.Config.Namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container, Follow: follow,
	}).Stream(ctx)
}

// Wait blocks until the attempt pod is terminal and builds the Outcome.
//
// The cluster enforces start_to_close (activeDeadlineSeconds); the agent's own
// timer fires a little later as a backstop, so a Job controller that is slow or
// wedged cannot let a task overrun unboundedly while this agent is alive.
func (e *KubernetesExecutor) Wait(ctx context.Context, h Handle) (Outcome, error) {
	st, ok := h.State.(*k8sState)
	if !ok {
		return Outcome{}, fmt.Errorf("%w: kubernetes: Wait called with foreign Handle", ErrValidation)
	}
	st.waitMu.Lock()
	defer st.waitMu.Unlock()
	if st.waited {
		return st.outcome, nil
	}
	t, err := e.ensureTracker(ctx)
	if err != nil {
		return Outcome{Err: err}, nil
	}
	e.startCollectorFollow(st)
	log := e.logger().With("attempt_id", st.attemptID, "job", st.jobName)

	ch, unsub := t.subscribe(st.key)
	defer unsub()

	var timedOut bool
	var deadlineC <-chan time.Time
	if !st.deadline.IsZero() {
		// Backstop only: the Job controller normally fires first and the
		// outcome then carries DeadlineExceeded from the cluster.
		tm := time.NewTimer(time.Until(st.deadline) + 5*time.Second)
		defer tm.Stop()
		deadlineC = tm.C
	}
	ctxDone := ctx.Done()
	var giveUpC <-chan time.Time
	armGiveUp := func() {
		if giveUpC == nil {
			// Bounded: a pod that never reaches a terminal state (a kubelet
			// that lost its node) must not wedge the runner forever.
			giveUpC = time.After(st.grace + 45*time.Second)
		}
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()

	var snap k8sSnapshot
	gaveUp := false
loop:
	for {
		snap = t.snapshot(st.key)
		e.reportPhase(st, snap)
		switch {
		case podTerminal(snap.Pod), snap.PodGone:
			break loop
		case snap.Pod == nil && (snap.JobGone || jobFinished(snap.Job)):
			break loop
		case jobFinished(snap.Job):
			// The controller has finished the Job (typically
			// DeadlineExceeded) and is terminating the pod; wait for it, but
			// not forever.
			armGiveUp()
		}
		select {
		case <-ch:
		case <-tick.C:
			if time.Since(snap.LastSeen) > 60*time.Second {
				e.refresh(ctx, t, st)
			}
		case <-deadlineC:
			deadlineC = nil
			timedOut = true
			log.Warn("attempt exceeded start_to_close and the cluster has not stopped it; terminating",
				"grace", st.grace)
			e.terminate(context.WithoutCancel(ctx), st, st.grace, log)
			armGiveUp()
		case <-ctxDone:
			ctxDone = nil
			log.Warn("wait cancelled; terminating the attempt pod", "reason", ctx.Err(), "grace", st.grace)
			e.terminate(context.WithoutCancel(ctx), st, st.grace, log)
			armGiveUp()
		case <-giveUpC:
			gaveUp = true
			break loop
		}
	}

	o := outcomeFromStatus(snap.Job, snap.Pod, st.wasSignalled())
	o.TimedOut = o.TimedOut || timedOut
	if gaveUp && o.Err == nil && !o.TimedOut && !podTerminal(snap.Pod) {
		o.Err = fmt.Errorf("%w: attempt pod %s did not reach a terminal state after termination",
			ErrRuntimeUnavailable, podName(snap.Pod))
	}
	o.WallMs = e.wallMs(st, snap.Pod)
	if o.OOMKilled && st.memLimit > 0 {
		o.PeakMemoryBytes = st.memLimit
	}
	e.attachOutputs(ctx, &o, st, snap)

	log.Info("attempt pod finished", "exit_code", o.ExitCode, "oom_killed", o.OOMKilled,
		"timed_out", o.TimedOut, "wall_ms", o.WallMs, "error", errString(o.Err))
	st.waited, st.outcome = true, o
	return o, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// wallMs prefers the container's own start/finish timestamps, but they have
// one-second resolution, so a sub-second task falls back to the agent's
// observation rather than reporting a wall time of zero.
func (e *KubernetesExecutor) wallMs(st *k8sState, pod *corev1.Pod) int64 {
	if pod != nil {
		if cs := findContainerStatus(pod.Status.ContainerStatuses, k8sTaskContainer); cs != nil && cs.State.Terminated != nil {
			d := cs.State.Terminated.FinishedAt.Sub(cs.State.Terminated.StartedAt.Time)
			if d >= time.Second {
				return d.Milliseconds()
			}
		}
	}
	ms := time.Since(st.started()).Milliseconds()
	if ms < 1 {
		ms = 1
	}
	return ms
}

// attachOutputs reads the collector's dump into the Outcome.
//
// A task that exited 0 but whose dump did not arrive COMPLETE is reported as
// an INFRASTRUCTURE failure, never as a success with missing outputs: a
// downstream task would otherwise bind an output reference to nothing, and the
// failure would surface far away in someone else's task.
func (e *KubernetesExecutor) attachOutputs(ctx context.Context, o *Outcome, st *k8sState, snap k8sSnapshot) {
	if o.Err != nil {
		return
	}
	succeeded := o.ExitCode == 0 && !o.OOMKilled && !o.TimedOut
	if !succeeded && !snapHasPod(snap) {
		return
	}
	// The follower ends when the collector exits; give it a bounded moment.
	if st.followDone != nil {
		select {
		case <-st.followDone:
		case <-time.After(20 * time.Second):
			e.logger().Warn("collector log stream did not finish; falling back to a direct read",
				"attempt_id", st.attemptID)
		}
	}
	st.followMu.Lock()
	raw := st.followBuf
	st.followMu.Unlock()
	collected, perr := parseCollectorStream(raw)
	if (perr != nil || !collected.Complete) && snap.Pod != nil {
		// Fallback for a re-attached, already-finished pod, or a follow stream
		// that broke: the finished pod still exists until Cleanup.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		if rc, err := e.openLogs(rctx, snap.Pod.Name, k8sCollectorContainer, false); err == nil {
			buf := newBoundedBuffer(k8sMaxOutputBytes*2 + 64<<10)
			_, _ = io.Copy(buf, rc)
			rc.Close()
			collected, perr = parseCollectorStream(buf.Bytes())
		}
		cancel()
	}
	if !succeeded {
		// Best effort for a failed task: whatever it managed to write is
		// useful for debugging, but its absence is not an error.
		if perr == nil {
			o.Output, o.ContentType, o.Outputs = outputsFromCollected(collected, st.declared)
		}
		return
	}
	switch {
	case perr != nil:
		o.Err = fmt.Errorf("%w: collected outputs were corrupt: %v", ErrRuntimeUnavailable, perr)
	case len(collected.TooBig) > 0:
		// Terminal: every kubernetes worker would hit the same cap.
		o.Err = fmt.Errorf("%w: output(s) %v exceed the kubernetes executor's %d-byte output transfer cap; "+
			"run this task on the docker executor", ErrValidation, collected.TooBig, k8sMaxOutputBytes)
	case !collected.Complete:
		o.Err = fmt.Errorf("%w: the task exited 0 but its outputs could not be collected from pod %s "+
			"(collector dump incomplete)", ErrRuntimeUnavailable, podName(snap.Pod))
	default:
		o.Output, o.ContentType, o.Outputs = outputsFromCollected(collected, st.declared)
	}
}

func snapHasPod(s k8sSnapshot) bool { return s.Pod != nil }

// terminate is the TERM-then-KILL contract on Kubernetes: delete the pod with
// gracePeriodSeconds, and the KUBELET delivers SIGTERM, waits the grace, then
// SIGKILLs. Delegating the escalation to the kubelet (as the docker executor
// delegates it to the daemon) means it completes even if this agent dies
// mid-grace.
func (e *KubernetesExecutor) terminate(ctx context.Context, st *k8sState, grace time.Duration, log *slog.Logger) {
	st.markSignalled()
	if err := e.deleteAttemptPods(ctx, st.attemptID, st.jobName, grace); err != nil {
		log.Warn("could not delete the attempt pod", "error", err)
	}
}

// deleteAttemptPods deletes every non-terminal pod of the attempt with the
// given grace. With no pod yet (still unscheduled) it deletes the Job, which
// is the only way to stop a pod that does not exist yet from being created.
func (e *KubernetesExecutor) deleteAttemptPods(ctx context.Context, attemptID, jobName string, grace time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pods, err := e.Client.CoreV1().Pods(e.Config.Namespace).List(ctx,
		metav1.ListOptions{LabelSelector: e.selectorForAttempt(attemptID)})
	if err != nil {
		return apiErr("list attempt pods", err)
	}
	if len(pods.Items) == 0 {
		err := e.Client.BatchV1().Jobs(e.Config.Namespace).Delete(ctx, jobName,
			metav1.DeleteOptions{PropagationPolicy: ptrTo(metav1.DeletePropagationBackground)})
		if err != nil && !apierrors.IsNotFound(err) {
			return apiErr("delete job "+jobName, err)
		}
		return nil
	}
	secs := int64(grace.Round(time.Second) / time.Second)
	if secs < 1 {
		// 0 would be a FORCE delete: the API object vanishes before the
		// kubelet has stopped anything, which is exactly how a "deleted" pod
		// keeps running unobserved. One second is the fastest honest kill.
		secs = 1
	}
	var errs []error
	for i := range pods.Items {
		p := &pods.Items[i]
		if podTerminal(p) {
			continue // already stopped: the desired end state
		}
		err := e.Client.CoreV1().Pods(e.Config.Namespace).Delete(ctx, p.Name,
			metav1.DeleteOptions{GracePeriodSeconds: &secs})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, apiErr("delete pod "+p.Name, err))
		}
	}
	return errors.Join(errs...)
}

// Signal maps TERM/KILL onto pod deletion with the attempt's grace / one
// second. Kubernetes has no "send a signal" API; deletion with a grace period
// is the one mechanism by which the kubelet delivers SIGTERM and escalates.
func (e *KubernetesExecutor) Signal(ctx context.Context, h Handle, sig Signal) error {
	if e.Client == nil {
		return nil
	}
	grace := DefaultGraceAfterTerm
	st, _ := h.State.(*k8sState)
	if st != nil {
		grace = st.grace
		st.markSignalled()
	}
	switch sig {
	case SignalTerm:
	case SignalKill:
		grace = time.Second
	default:
		return fmt.Errorf("%w: unknown signal %q", ErrValidation, sig)
	}
	jobName := k8sJobName(h.AttemptID)
	if err := e.deleteAttemptPods(ctx, h.AttemptID, jobName, grace); err != nil {
		return fmt.Errorf("signal %s to attempt %s: %w", sig, h.AttemptID, err)
	}
	e.logger().Info("signalled attempt pod", "attempt_id", h.AttemptID, "job", jobName,
		"signal", string(sig), "grace", grace)
	return nil
}

// Logs streams the task container's log. The pod log API interleaves stdout
// and stderr in write order, which is exactly the combined log an operator
// reads. A pod that no longer exists yields an empty stream, not an error: the
// runner opens logs unconditionally, and a vanished pod is not a failure of
// the log path.
func (e *KubernetesExecutor) Logs(ctx context.Context, h Handle) (io.ReadCloser, error) {
	if e.Client == nil {
		return io.NopCloser(emptyReader{}), nil
	}
	pods, err := e.Client.CoreV1().Pods(e.Config.Namespace).List(ctx,
		metav1.ListOptions{LabelSelector: e.selectorForAttempt(h.AttemptID)})
	if err != nil {
		return nil, apiErr("list attempt pods for logs", err)
	}
	if len(pods.Items) == 0 {
		return io.NopCloser(emptyReader{}), nil
	}
	rc, err := e.openLogs(ctx, pods.Items[0].Name, k8sTaskContainer, true)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return io.NopCloser(emptyReader{}), nil
		}
		return nil, fmt.Errorf("open logs for attempt %s: %w", h.AttemptID, err)
	}
	return rc, nil
}

// Cleanup deletes the attempt's pod, Job and inputs Secret and the local work
// directory. Idempotent: every delete tolerates NotFound, so a second Cleanup
// — and a Cleanup given only an AttemptID after a failed Start — is a no-op
// rather than an error.
func (e *KubernetesExecutor) Cleanup(ctx context.Context, h Handle) error {
	var errs []error
	st, _ := h.State.(*k8sState)
	if st != nil && st.followCancel != nil {
		st.followCancel()
	}
	if e.Client != nil && h.AttemptID != "" {
		jobName := k8sJobName(h.AttemptID)
		grace := DefaultGraceAfterTerm
		if st != nil {
			grace = st.grace
		}
		// Pods first, with the task's grace: the Job's background deletion
		// would also remove them, but on the garbage collector's schedule, and
		// "Cleanup returned" should mean "the workload is being stopped now".
		if err := e.deleteAttemptPods(ctx, h.AttemptID, jobName, grace); err != nil {
			errs = append(errs, err)
		}
		err := e.Client.BatchV1().Jobs(e.Config.Namespace).Delete(ctx, jobName,
			metav1.DeleteOptions{PropagationPolicy: ptrTo(metav1.DeletePropagationBackground)})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, apiErr("delete job "+jobName, err))
		}
		// The Secret is Job-owned and would be garbage-collected, but deleting
		// it explicitly shortens the time an attempt's inputs exist at rest.
		if st == nil || st.hasSecret {
			err := e.Client.CoreV1().Secrets(e.Config.Namespace).Delete(ctx, k8sSecretName(jobName), metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, apiErr("delete inputs secret", err))
			}
		}
		e.trackerMu.Lock()
		if e.tracker != nil {
			e.tracker.forget(k8sLabelValue(h.AttemptID))
		}
		e.trackerMu.Unlock()
	}
	if h.WorkDir != "" {
		if err := os.RemoveAll(h.WorkDir); err != nil {
			errs = append(errs, fmt.Errorf("cleanup workdir %s: %w", h.WorkDir, err))
		}
	}
	return errors.Join(errs...)
}

// teardown is Cleanup for a Prepare that failed after creating the Job.
func (e *KubernetesExecutor) teardown(ctx context.Context, st *k8sState, log *slog.Logger) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := e.Cleanup(cctx, Handle{AttemptID: st.attemptID, WorkDir: st.workDir, State: st}); err != nil {
		log.Warn("could not remove the Job of a failed Prepare; its TTL will", "error", err)
	}
}

// ReattachState is what a restarted agent finds for an attempt it had started.
type ReattachState int

const (
	// ReattachNotFound: no Job for the attempt (never created, or already
	// cleaned up / expired by TTL).
	ReattachNotFound ReattachState = iota
	// ReattachRunning: the Job is still running.
	ReattachRunning
	// ReattachFinished: the Job has finished and its outcome can be harvested.
	ReattachFinished
)

// Reattach rebuilds a Handle for an attempt from its Job alone — the path a
// restarted agent takes for an attempt its WAL says it started (§6.3).
//
// Everything Wait needs (declared outputs, deadline, grace) is recovered from
// the Job's annotations, because the restarted agent no longer has the
// TaskSpec. A FINISHED Job lets the agent report the real outcome instead of
// INDETERMINATE — turning a would-be LOST attempt and a re-execution into a
// cheap re-report, which is the WAL's whole purpose.
func (e *KubernetesExecutor) Reattach(ctx context.Context, attemptID string) (Handle, ReattachState, error) {
	t, err := e.ensureTracker(ctx)
	if err != nil {
		return Handle{}, ReattachNotFound, err
	}
	jobName := k8sJobName(attemptID)
	job, err := e.Client.BatchV1().Jobs(e.Config.Namespace).Get(ctx, jobName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Handle{}, ReattachNotFound, nil
	}
	if err != nil {
		return Handle{}, ReattachNotFound, apiErr("get job "+jobName, err)
	}
	if job.Annotations[k8sAnnAttemptID] != attemptID || job.DeletionTimestamp != nil {
		return Handle{}, ReattachNotFound, nil
	}
	st := &k8sState{
		attemptID: attemptID,
		key:       k8sLabelValue(attemptID),
		jobName:   jobName,
		hasSecret: true,
		grace:     DefaultGraceAfterTerm,
		adopted:   true,
	}
	_ = json.Unmarshal([]byte(job.Annotations[k8sAnnDeclaredOutputs]), &st.declared)
	if ms, err := strconv.ParseInt(job.Annotations[k8sAnnDeadline], 10, 64); err == nil {
		st.deadline = time.UnixMilli(ms)
	}
	if ms, err := strconv.ParseInt(job.Annotations[k8sAnnGrace], 10, 64); err == nil && ms > 0 {
		st.grace = time.Duration(ms) * time.Millisecond
	}
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.Name == k8sTaskContainer {
			if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
				st.memLimit = q.Value()
			}
		}
	}
	if job.Status.StartTime != nil {
		st.setStarted(job.Status.StartTime.Time)
	} else {
		st.setStarted(time.Now())
	}
	e.refresh(ctx, t, st)
	snap := t.snapshot(st.key)
	state := ReattachRunning
	if jobFinished(snap.Job) || podTerminal(snap.Pod) {
		state = ReattachFinished
	}
	e.logger().Info("found the Job of an attempt started by a previous agent session",
		"attempt_id", attemptID, "job", jobName, "finished", state == ReattachFinished)
	return Handle{
		AttemptID: attemptID, ExecutorKind: e.Kind(), ID: jobName,
		StartedAt: st.started(), State: st,
	}, state, nil
}

// ReapOrphans deletes every Job this agent instance owns except those keep
// accepts. It is the restart half of "no orphaned workload": a SIGKILLed agent
// leaves Jobs the cluster happily keeps running, holding node capacity that
// the Nexus ledger believes is free. Scoped by the owner label so it never
// touches a sibling agent's work in a shared namespace.
func (e *KubernetesExecutor) ReapOrphans(ctx context.Context, keep func(attemptID string) bool) (int, error) {
	if e.Client == nil {
		return 0, nil
	}
	jobs, err := e.Client.BatchV1().Jobs(e.Config.Namespace).List(ctx,
		metav1.ListOptions{LabelSelector: e.ownerSelector()})
	if err != nil {
		return 0, apiErr("list owned jobs", err)
	}
	n := 0
	var errs []error
	for i := range jobs.Items {
		j := &jobs.Items[i]
		id := j.Annotations[k8sAnnAttemptID]
		if keep != nil && keep(id) {
			continue
		}
		if err := e.Cleanup(ctx, Handle{AttemptID: id}); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
		e.logger().Warn("reaped an orphaned attempt Job left by a previous agent session",
			"attempt_id", id, "job", j.Name)
	}
	return n, errors.Join(errs...)
}

// apiErr classifies a Kubernetes API error for the retry policy.
//
// Invalid (422) means the spec we rendered was rejected — a bad env var name,
// an unparseable image reference — which no retry fixes: VALIDATION. Every
// other API failure (forbidden by RBAC or quota, throttled, unreachable) is the
// cluster's or the agent's problem, never the task author's: INFRASTRUCTURE
// and retryable, so another worker or a later attempt can succeed.
func apiErr(what string, err error) error {
	if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return fmt.Errorf("%w: %s: %v", ErrValidation, what, err)
	}
	return fmt.Errorf("%w: %s: %v", ErrRuntimeUnavailable, what, err)
}
