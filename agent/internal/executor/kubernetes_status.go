// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"fmt"
	"os"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// This file is the pure half of the kubernetes executor: functions from
// observed Pod/Job status to facts (phase summaries, start-up verdicts,
// outcomes). No API calls, so every mapping to an error class is unit-tested
// against hand-built statuses instead of being discovered in production.

// k8sStartVerdict is what Prepare concludes from one observation of the pod
// while waiting for the task container to start.
type k8sStartVerdict int

const (
	k8sStartWaiting  k8sStartVerdict = iota // keep waiting
	k8sStartStarted                         // task container running or already finished
	k8sStartTerminal                        // Job/pod finished before we saw it run; Wait classifies
	k8sStartFailed                          // a terminal start-up failure; err explains
)

func findContainerStatus(ss []corev1.ContainerStatus, name string) *corev1.ContainerStatus {
	for i := range ss {
		if ss[i].Name == name {
			return &ss[i]
		}
	}
	return nil
}

func jobCondition(job *batchv1.Job, t batchv1.JobConditionType) *batchv1.JobCondition {
	if job == nil {
		return nil
	}
	for i := range job.Status.Conditions {
		c := &job.Status.Conditions[i]
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return c
		}
	}
	return nil
}

// jobFinished reports a Job the controller has declared terminal.
func jobFinished(job *batchv1.Job) bool {
	return jobCondition(job, batchv1.JobComplete) != nil || jobCondition(job, batchv1.JobFailed) != nil
}

// jobDeadlineExceeded is the cluster-side start_to_close enforcement firing.
func jobDeadlineExceeded(job *batchv1.Job) bool {
	c := jobCondition(job, batchv1.JobFailed)
	return c != nil && c.Reason == batchv1.JobReasonDeadlineExceeded
}

func podTerminal(pod *corev1.Pod) bool {
	return pod != nil && (pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed)
}

// classifyImagePull maps a kubelet image-pull failure to the SAME sentinels
// the docker executor uses for `docker pull`, because the class — not the
// substrate — is what the retry policy acts on. "Not found" is terminal (a
// retry cannot conjure an image); "unauthorised" is terminal (a human must
// fix the pull secret); anything else — registry down, DNS, rate limit — is
// transient and gets a bounded wait before failing retryably.
func classifyImagePull(image, reason, msg string) error {
	s := strings.ToLower(msg)
	switch {
	case reason == "InvalidImageName":
		return fmt.Errorf("%w: image %q is not a valid reference: %s", ErrValidation, image, msg)
	case reason == "ErrImageNeverPull":
		return fmt.Errorf("%w: image %q is not present on the node and pull policy is Never", ErrImageNotFound, image)
	case strings.Contains(s, "not found"), strings.Contains(s, "manifest unknown"),
		strings.Contains(s, "repository does not exist"), strings.Contains(s, "no such image"):
		return fmt.Errorf("%w: image %q: %s", ErrImageNotFound, image, msg)
	case strings.Contains(s, "unauthorized"), strings.Contains(s, "authentication required"),
		strings.Contains(s, "access denied"), strings.Contains(s, "denied"):
		return fmt.Errorf("%w: pull %q: %s", os.ErrPermission, image, msg)
	}
	return nil // transient; caller waits up to ImagePullTimeout
}

func isImagePullReason(r string) bool {
	switch r {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull", "RegistryUnavailable":
		return true
	}
	return false
}

// k8sStartObs is one observation of the pod while Prepare waits for the task
// container to start.
type k8sStartObs struct {
	Verdict k8sStartVerdict
	// Err is set only for k8sStartFailed.
	Err error
	// Cause is the last transient problem (pull back-off, config error), kept
	// so a timeout reports the real cause rather than just "timed out".
	Cause string
	// Pulling marks Cause as an image-pull problem, which has its own timeout.
	Pulling bool
}

// startVerdict inspects the pod (and Job) once.
func startVerdict(job *batchv1.Job, pod *corev1.Pod) k8sStartObs {
	if jobFinished(job) || podTerminal(pod) {
		return k8sStartObs{Verdict: k8sStartTerminal}
	}
	if pod == nil {
		return k8sStartObs{Verdict: k8sStartWaiting}
	}

	// The collector sidecar is OUR infrastructure: its failure to pull is an
	// agent/cluster fault, never the task author's, so it is never NOT_FOUND.
	if cs := findContainerStatus(pod.Status.InitContainerStatuses, k8sCollectorContainer); cs != nil {
		if w := cs.State.Waiting; w != nil && isImagePullReason(w.Reason) {
			return k8sStartObs{Verdict: k8sStartWaiting, Pulling: true,
				Cause: fmt.Sprintf("collector helper image %s: %s: %s", cs.Image, w.Reason, w.Message)}
		}
	}

	cs := findContainerStatus(pod.Status.ContainerStatuses, k8sTaskContainer)
	if cs == nil {
		return k8sStartObs{Verdict: k8sStartWaiting}
	}
	switch {
	case cs.State.Running != nil, cs.State.Terminated != nil:
		return k8sStartObs{Verdict: k8sStartStarted}
	case cs.State.Waiting != nil:
		w := cs.State.Waiting
		if isImagePullReason(w.Reason) {
			if err := classifyImagePull(cs.Image, w.Reason, w.Message); err != nil {
				return k8sStartObs{Verdict: k8sStartFailed, Err: err}
			}
			return k8sStartObs{Verdict: k8sStartWaiting, Pulling: true, Cause: w.Reason + ": " + w.Message}
		}
		if w.Reason == "CreateContainerConfigError" || w.Reason == "CreateContainerError" {
			// Often transient (the inputs Secret is created just after the
			// Job); kept as the last-known cause for the start timeout.
			return k8sStartObs{Verdict: k8sStartWaiting, Cause: w.Reason + ": " + w.Message}
		}
	}
	return k8sStartObs{Verdict: k8sStartWaiting}
}

// podPhaseSummary renders the pod's state as one line for ReportProgress.
//
// This is ADR-0007's mitigation for nested scheduling: "why is my task
// pending" spans two schedulers, and without this the Nexus UI would show
// RUNNING while Kubernetes says "0/12 nodes available: insufficient
// nvidia.com/gpu". The Kubernetes reason is surfaced verbatim.
func podPhaseSummary(job *batchv1.Job, pod *corev1.Pod) (phase, detail string) {
	if pod == nil {
		if jobFinished(job) {
			return "JobFinished", jobConditionMessage(job)
		}
		return "Pending", "waiting for the Job controller to create the pod"
	}
	phase = string(pod.Status.Phase)
	if pod.DeletionTimestamp != nil && !podTerminal(pod) {
		return "Terminating", "pod is being deleted"
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return phase, strings.TrimSpace(c.Reason + ": " + c.Message)
		}
	}
	if cs := findContainerStatus(pod.Status.InitContainerStatuses, k8sCollectorContainer); cs != nil && cs.State.Waiting != nil {
		return phase, "collector: " + cs.State.Waiting.Reason
	}
	if cs := findContainerStatus(pod.Status.ContainerStatuses, k8sTaskContainer); cs != nil {
		switch {
		case cs.State.Waiting != nil:
			return phase, strings.TrimSpace(cs.State.Waiting.Reason + ": " + cs.State.Waiting.Message)
		case cs.State.Running != nil:
			return phase, "task container running on node " + pod.Spec.NodeName
		case cs.State.Terminated != nil:
			return phase, fmt.Sprintf("task container exited %d (%s)",
				cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
		}
	}
	if pod.Status.Reason != "" {
		return phase, pod.Status.Reason + ": " + pod.Status.Message
	}
	return phase, ""
}

func jobConditionMessage(job *batchv1.Job) string {
	for _, t := range []batchv1.JobConditionType{batchv1.JobFailed, batchv1.JobComplete} {
		if c := jobCondition(job, t); c != nil {
			return strings.TrimSpace(string(t) + " " + c.Reason + ": " + c.Message)
		}
	}
	return ""
}

// podDisrupted reports an eviction or preemption: the CLUSTER took the pod
// away. That is INFRASTRUCTURE and retryable — the task did nothing wrong, and
// another node (or worker) very likely has room.
func podDisrupted(pod *corev1.Pod) (bool, string) {
	if pod == nil {
		return false, ""
	}
	if pod.Status.Reason == "Evicted" || pod.Status.Reason == "Preempting" {
		return true, strings.TrimSpace(pod.Status.Reason + ": " + pod.Status.Message)
	}
	for _, c := range pod.Status.Conditions {
		// Set by the API-initiated eviction, scheduler preemption, taint
		// manager, pod GC and kubelet node-pressure paths — never by a plain
		// DELETE, so the agent's own cancellation does not trip it.
		if c.Type == corev1.DisruptionTarget && c.Status == corev1.ConditionTrue {
			return true, strings.TrimSpace("DisruptionTarget " + c.Reason + ": " + c.Message)
		}
	}
	return false, ""
}

// outcomeFromStatus builds the factual Outcome from the final pod and Job. It
// does NOT fill outputs or wall time; the caller owns those.
//
// signalled is true when the agent itself asked for termination (cancel,
// lease loss, its own deadline backstop); it disambiguates a disruption
// condition that the agent's own delete also produces.
func outcomeFromStatus(job *batchv1.Job, pod *corev1.Pod, agentSignalled bool) Outcome {
	var o Outcome
	if jobDeadlineExceeded(job) {
		// Cluster-side start_to_close enforcement. Checked FIRST, exactly as
		// ClassifyOutcome checks TimedOut before the exit code: the pod the
		// Job controller killed exits 137/143, and calling that an application
		// error would point the operator at a bug that is not there.
		o.TimedOut = true
	}

	var cs *corev1.ContainerStatus
	if pod != nil {
		cs = findContainerStatus(pod.Status.ContainerStatuses, k8sTaskContainer)
	}
	if cs == nil || cs.State.Terminated == nil {
		if cs != nil && cs.LastTerminationState.Terminated != nil {
			// Defensive: restartPolicy is Never, but read it if present.
			return fillTerminated(o, cs.LastTerminationState.Terminated)
		}
		if o.TimedOut {
			// Killed by the deadline before the container ever reported a
			// state (e.g. never scheduled). The timeout is the whole truth.
			o.ExitCode = -1
			return o
		}
		if disrupted, why := podDisrupted(pod); disrupted && !agentSignalled {
			o.Err = fmt.Errorf("%w: pod %s: %s", ErrEvicted, podName(pod), why)
			return o
		}
		if agentSignalled {
			// We tore it down before it ran; there is no exit code to report
			// and the caller (cancel / lease loss) already knows why.
			o.ExitCode = -1
			o.Signalled, o.Signal = true, "deleted"
			return o
		}
		// Never fabricate an exit code: without a terminated state we do not
		// know how the task ended, and exit 0 would be an invented success.
		o.Err = fmt.Errorf("%w: attempt pod %s ended without a terminated task container status (phase %s)",
			ErrRuntimeUnavailable, podName(pod), podPhase(pod))
		return o
	}

	o = fillTerminated(o, cs.State.Terminated)
	if o.Err == nil && !o.OOMKilled && !o.TimedOut && !agentSignalled {
		// An eviction kills the container (137) and would otherwise read as an
		// application failure; it is the cluster's doing, not the task's.
		if disrupted, why := podDisrupted(pod); disrupted && o.ExitCode != 0 {
			o.Err = fmt.Errorf("%w: pod %s: %s", ErrEvicted, podName(pod), why)
		}
	}
	return o
}

func fillTerminated(o Outcome, t *corev1.ContainerStateTerminated) Outcome {
	o.ExitCode = int(t.ExitCode)
	switch t.Reason {
	case "OOMKilled":
		// Reported by the runtime from the cgroup's oom_kill counter: the
		// authoritative signal the subprocess executor can only infer.
		o.OOMKilled = true
	case "StartError", "ContainerCannotRun":
		m := strings.ToLower(t.Message)
		if strings.Contains(m, "no such file or directory") || strings.Contains(m, "executable file not found") ||
			strings.Contains(m, "not found in $path") {
			// The container analogue of a missing handler binary: terminal,
			// like the subprocess executor's ENOENT.
			o.Err = fmt.Errorf("%w: task command could not be started: %s", os.ErrNotExist, t.Message)
		} else {
			o.Err = fmt.Errorf("%w: task container could not start: %s", ErrRuntimeUnavailable, t.Message)
		}
	}
	switch o.ExitCode {
	case 137:
		o.Signalled, o.Signal = true, "killed"
	case 143:
		o.Signalled, o.Signal = true, "terminated"
	}
	return o
}

func podName(p *corev1.Pod) string {
	if p == nil {
		return "<none>"
	}
	return p.Name
}

func podPhase(p *corev1.Pod) string {
	if p == nil {
		return "<no pod>"
	}
	return string(p.Status.Phase)
}
