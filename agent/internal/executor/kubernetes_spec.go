// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Labels and annotations stamped on every Job, Pod and Secret this executor
// creates.
//
// Labels are what the executor SELECTS on (its informer, orphan reaping, "find
// the pod of this attempt"), so they are restricted to the label value grammar
// and hashed when an id does not fit. Annotations carry the exact ids and the
// facts Wait needs after an agent restart, because a re-attaching executor has
// the Job but not the TaskSpec that created it.
const (
	k8sLabelManagedBy = "app.kubernetes.io/managed-by"
	k8sManagedByValue = "nexus-agent"
	k8sLabelComponent = "app.kubernetes.io/component"
	k8sComponentValue = "task-attempt"
	// k8sLabelOwner scopes orphan reaping to ONE agent instance. Several agent
	// replicas share a namespace; reaping by "managed-by" alone would have each
	// replica delete its siblings' live work on every restart.
	k8sLabelOwner   = "nexus.dev/owner"
	k8sLabelAttempt = "nexus.dev/attempt"
	k8sLabelRun     = "nexus.dev/run"
	k8sLabelNode    = "nexus.dev/node"
	k8sLabelWorker  = "nexus.dev/worker"

	k8sAnnAttemptID       = "nexus.dev/attempt-id"
	k8sAnnRunID           = "nexus.dev/run-id"
	k8sAnnNodeID          = "nexus.dev/node-id"
	k8sAnnWorkerID        = "nexus.dev/worker-id"
	k8sAnnDeclaredOutputs = "nexus.dev/declared-outputs"
	k8sAnnDeadline        = "nexus.dev/deadline-unix-ms"
	k8sAnnGrace           = "nexus.dev/grace-ms"
	// k8sAnnEnforced records which dimensions the pod spec actually constrains,
	// so an operator reading `kubectl describe job` sees the enforcement truth
	// (F28) rather than inferring it from the resource block.
	k8sAnnEnforced = "nexus.dev/enforced-limits"

	k8sTaskContainer      = "task"
	k8sCollectorContainer = "nexus-collector"

	// k8sWorkDir is the workload's view of the attempt directory, identical to
	// the docker executor's so one handler runs unchanged on both substrates.
	k8sWorkDir = "/nexus"

	// The collector's view of the same two volumes (see buildJob).
	k8sCollectorWorkDir    = "/collect/work"
	k8sCollectorOutputsDir = "/collect/outputs"
)

// k8sMaxOutputBytes caps the TOTAL raw bytes of outputs moved out of the pod.
//
// Outputs leave the pod through the collector's log stream (see
// kubernetes_outputs.go). Base64 inflates them by 4/3, and the kubelet rotates a
// container log at containerLogMaxSize (10 MiB by default); a transfer that
// straddles a rotation can lose its head. 4 MiB raw is ~5.4 MiB encoded, which
// stays clear of the default rotation with margin. A task with a larger result
// must be run on the docker executor today; see CONTRACT-FINDINGS F96.
const k8sMaxOutputBytes = 4 << 20

// k8sMaxInputBytes bounds staged inputs, which travel in a Secret. The API
// server rejects objects over ~1 MiB; failing here gives a legible error rather
// than an opaque 413 from etcd.
const k8sMaxInputBytes = 1000 << 10

var k8sDNSLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
var k8sLabelValueRE = regexp.MustCompile(`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`)

// k8sJobName derives the Job name DETERMINISTICALLY from the attempt id.
//
// Determinism is the whole re-attach mechanism: an agent that crashed after
// creating a Job, or a Prepare that is repeated for the same attempt, computes
// the same name, gets AlreadyExists from the API server, and adopts the running
// Job instead of launching a second copy of the same attempt. A generated name
// (generateName) would make every retry of Prepare a double-launch.
//
// Ids that are not already a valid DNS label are HASHED, not lower-cased or
// stripped: two attempt ids differing only in case or punctuation must not map
// to the same Job, or one attempt would adopt the other's workload.
func k8sJobName(attemptID string) string {
	const prefix = "nexus-"
	// 40 leaves room for the "-inputs" Secret suffix inside the 63-char limit.
	if len(attemptID) <= 40 && k8sDNSLabel.MatchString(attemptID) {
		return prefix + attemptID
	}
	sum := sha256.Sum256([]byte(attemptID))
	return prefix + "h" + hex.EncodeToString(sum[:])[:32]
}

func k8sSecretName(jobName string) string { return jobName + "-inputs" }

// k8sLabelValue renders an id as a label value, hashing when it does not fit
// the grammar. Labels are for selection only; the exact id is in an annotation.
func k8sLabelValue(s string) string {
	if len(s) <= 63 && k8sLabelValueRE.MatchString(s) {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return "h" + hex.EncodeToString(sum[:])[:40]
}

// k8sSelectorForAttempt selects the objects of one attempt created by THIS
// agent instance.
func (e *KubernetesExecutor) selectorForAttempt(attemptID string) string {
	return fmt.Sprintf("%s=%s,%s=%s,%s=%s",
		k8sLabelManagedBy, k8sManagedByValue,
		k8sLabelOwner, k8sLabelValue(e.Config.Owner),
		k8sLabelAttempt, k8sLabelValue(attemptID))
}

// ownerSelector selects everything this agent instance created.
func (e *KubernetesExecutor) ownerSelector() string {
	return fmt.Sprintf("%s=%s,%s=%s",
		k8sLabelManagedBy, k8sManagedByValue,
		k8sLabelOwner, k8sLabelValue(e.Config.Owner))
}

// k8sResources translates the enforcement ceiling into a container resource
// block with requests EQUAL to limits.
//
// Equal, not request < limit, for two reasons. First, enforcement is only real
// for dimensions with a limit: cgroup memory.max and the CFS quota are derived
// from limits, so a request-only dimension is advisory. Second, requests ==
// limits on cpu and memory puts the pod in the Guaranteed QoS class, which is
// the last to be evicted under node pressure; a Burstable task pod is evicted
// first and would turn node pressure into INFRASTRUCTURE retries of work the
// Nexus scheduler believed it had a guaranteed reservation for.
//
// The returned slice names what is actually enforced, for the annotation and
// the logs.
func (e *KubernetesExecutor) k8sResources(limit *nexusv1.ResourceVector) (corev1.ResourceRequirements, []string, error) {
	rl := corev1.ResourceList{}
	var enforced []string
	if c := limit.GetCpuMillis(); c > 0 {
		rl[corev1.ResourceCPU] = *resource.NewMilliQuantity(c, resource.DecimalSI)
		enforced = append(enforced, "cpu=cfs-quota")
	}
	if m := limit.GetMemoryBytes(); m > 0 {
		rl[corev1.ResourceMemory] = *resource.NewQuantity(m, resource.BinarySI)
		enforced = append(enforced, "memory=cgroup-oom-kill")
	}
	if d := limit.GetDiskBytes(); d > 0 {
		// Ephemeral storage is enforced by kubelet EVICTION, not by the kernel:
		// usage is sampled periodically, so a burst between samples is
		// possible and the failure surfaces as an eviction, not ENOSPC.
		rl[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(d, resource.BinarySI)
		enforced = append(enforced, "ephemeral-storage=kubelet-eviction")
	}
	if g := limit.GetGpuCount(); g > 0 {
		if e.Config.GPUResourceName == "" {
			return corev1.ResourceRequirements{}, nil, fmt.Errorf(
				"%w: task requests %d GPU(s) but no GPU device-plugin resource name is configured (NEXUS_K8S_GPU_RESOURCE)",
				ErrLimitUnenforceable, g)
		}
		// Extended resources are integer and must have requests == limits;
		// the device plugin allocates whole devices and isolates them.
		rl[corev1.ResourceName(e.Config.GPUResourceName)] = *resource.NewQuantity(int64(g), resource.DecimalSI)
		enforced = append(enforced, "gpu=device-plugin:"+e.Config.GPUResourceName)
	}
	if len(rl) == 0 {
		return corev1.ResourceRequirements{}, nil, nil
	}
	return corev1.ResourceRequirements{Requests: rl.DeepCopy(), Limits: rl}, enforced, nil
}

// k8sEnv builds the container environment. Task env first, platform variables
// LAST and overriding, so a task cannot lie to itself about its own
// idempotency key — the same precedence the docker executor applies. Sorted so
// the pod spec is deterministic, which keeps `kubectl diff` and audit logs
// readable.
func k8sEnv(spec TaskSpec) []corev1.EnvVar {
	m := map[string]string{}
	for k, v := range spec.Runtime.GetEnv() {
		m[k] = v
	}
	for _, kv := range dockerEnv(TaskSpec{
		AttemptID: spec.AttemptID, RunID: spec.RunID, NodeID: spec.NodeID,
		IdempotencyKey: spec.IdempotencyKey, DeclaredOutputs: spec.DeclaredOutputs,
		AttemptNumber:        spec.AttemptNumber,
		StartToCloseDeadline: spec.StartToCloseDeadline,
		InlineThresholdBytes: spec.InlineThresholdBytes,
		// TaskAPISocket and TaskAPIToken deliberately NOT copied: the pod runs
		// on another node and has no path to this agent's socket, so exporting
		// NEXUS_TASK_API would send the SDK to a socket that does not exist.
		// Without it the SDK keeps usage, permits and checkpoints in-process
		// (ADR-0032 §1 and §6; F149, open).
		// Runtime deliberately nil: task env is already in m, and re-adding it
		// here would let it override the platform variables below.
	}, k8sWorkDir) {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]corev1.EnvVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: m[k]})
	}
	return out
}

// restrictedContainerSecurity is the Pod Security "restricted" profile for one
// container. Every field is set explicitly rather than relying on namespace
// defaults, so the pod is admitted by a restricted namespace and behaves the
// same in a permissive one.
func (e *KubernetesExecutor) restrictedContainerSecurity(readOnlyRoot bool) *corev1.SecurityContext {
	f := false
	t := true
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &f,
		Privileged:               &f,
		RunAsNonRoot:             &t,
		ReadOnlyRootFilesystem:   &readOnlyRoot,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// buildJob renders the Job (and, when there are inputs, the Secret that carries
// them) for one attempt. Pure: no API calls, so every security-relevant field
// is unit-testable without a cluster.
func (e *KubernetesExecutor) buildJob(spec TaskSpec, workerID string) (*batchv1.Job, *corev1.Secret, error) {
	cfg := e.Config
	name := k8sJobName(spec.AttemptID)
	grace := orDefaultGrace(spec.GraceAfterTerm)

	res, enforced, err := e.k8sResources(spec.Limit)
	if err != nil {
		return nil, nil, err
	}

	labels := map[string]string{
		k8sLabelManagedBy: k8sManagedByValue,
		k8sLabelComponent: k8sComponentValue,
		k8sLabelOwner:     k8sLabelValue(cfg.Owner),
		k8sLabelAttempt:   k8sLabelValue(spec.AttemptID),
		k8sLabelRun:       k8sLabelValue(spec.RunID),
		k8sLabelNode:      k8sLabelValue(spec.NodeID),
	}
	if workerID != "" {
		labels[k8sLabelWorker] = k8sLabelValue(workerID)
	}
	declared, _ := json.Marshal(spec.DeclaredOutputs)
	ann := map[string]string{
		k8sAnnAttemptID:       spec.AttemptID,
		k8sAnnRunID:           spec.RunID,
		k8sAnnNodeID:          spec.NodeID,
		k8sAnnWorkerID:        workerID,
		k8sAnnDeclaredOutputs: string(declared),
		k8sAnnGrace:           strconv.FormatInt(grace.Milliseconds(), 10),
		k8sAnnEnforced:        strings.Join(enforced, ","),
	}
	if !spec.StartToCloseDeadline.IsZero() {
		ann[k8sAnnDeadline] = strconv.FormatInt(spec.StartToCloseDeadline.UnixMilli(), 10)
	}

	// Volumes. The work directory, the output drop and /tmp are separate
	// emptyDirs so the root filesystem can be read-only: a task that needs to
	// write anywhere else is told so by EROFS immediately, instead of silently
	// filling the node's image layer.
	var inputsSource corev1.VolumeSource
	var secret *corev1.Secret
	if len(spec.Inputs) > 0 {
		data, err := k8sInputData(spec.Inputs)
		if err != nil {
			return nil, nil, err
		}
		mode := int32(0o440)
		inputsSource = corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: k8sSecretName(name), DefaultMode: &mode,
		}}
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: k8sSecretName(name), Namespace: cfg.Namespace,
				Labels: labels, Annotations: map[string]string{k8sAnnAttemptID: spec.AttemptID},
			},
			Type: corev1.SecretTypeOpaque,
			Data: data,
			// Immutable: nothing should ever rewrite an attempt's inputs, and
			// the kubelet stops watching immutable Secrets, which matters at
			// thousands of concurrent attempts.
			Immutable: ptrTo(true),
		}
	} else {
		inputsSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	}
	workVol := corev1.EmptyDirVolumeSource{}
	if d := spec.Limit.GetDiskBytes(); d > 0 {
		// Bound the scratch directory as well as the container's writable
		// layer; the kubelet evicts on either being exceeded.
		workVol.SizeLimit = resource.NewQuantity(d, resource.BinarySI)
	}
	volumes := []corev1.Volume{
		{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &workVol}},
		{Name: "outputs", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "inputs", VolumeSource: inputsSource},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	taskMounts := []corev1.VolumeMount{
		{Name: "work", MountPath: k8sWorkDir},
		{Name: "outputs", MountPath: k8sWorkDir + "/" + OutputDirName},
		{Name: "inputs", MountPath: k8sWorkDir + "/" + InputDirName, ReadOnly: true},
		{Name: "tmp", MountPath: "/tmp"},
	}
	// The collector mounts the two volumes SIDE BY SIDE, not nested as the
	// task does: a read-only volume cannot host the mountpoint of another, and
	// the collector starts before the task has created /nexus/outputs.
	collectorMounts := []corev1.VolumeMount{
		{Name: "work", MountPath: k8sCollectorWorkDir, ReadOnly: true},
		{Name: "outputs", MountPath: k8sCollectorOutputsDir, ReadOnly: true},
	}

	workingDir := spec.Runtime.GetWorkingDir()
	if workingDir == "" {
		workingDir = k8sWorkDir
	}
	pullPolicy := cfg.ImagePullPolicy

	task := corev1.Container{
		Name:            k8sTaskContainer,
		Image:           spec.Runtime.GetImage(),
		Command:         append([]string(nil), spec.Runtime.GetCommand()...),
		Args:            append([]string(nil), spec.Runtime.GetArgs()...),
		WorkingDir:      workingDir,
		Env:             k8sEnv(spec),
		Resources:       res,
		VolumeMounts:    taskMounts,
		ImagePullPolicy: pullPolicy,
		SecurityContext: e.restrictedContainerSecurity(!cfg.WritableRootFilesystem),
		// FallbackToLogsOnError puts the log tail in the termination message
		// when the task dies without writing one, so `kubectl describe pod`
		// shows why without a separate `kubectl logs`.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}

	always := corev1.ContainerRestartPolicyAlways
	collectorRes := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("50m"),
		corev1.ResourceMemory: resource.MustParse("32Mi"),
	}
	collector := corev1.Container{
		Name:  k8sCollectorContainer,
		Image: cfg.HelperImage,
		// A NATIVE SIDECAR (init container with restartPolicy Always): it
		// starts before the task and the kubelet terminates it only AFTER the
		// task container has exited, which is the ordering that lets it read
		// the finished outputs. See kubernetes_outputs.go for why outputs leave
		// the pod this way at all.
		RestartPolicy:   &always,
		Command:         []string{"/bin/sh", "-c", k8sCollectorScript},
		Env:             []corev1.EnvVar{{Name: "NEXUS_K8S_OUTPUT_CAP", Value: strconv.Itoa(k8sMaxOutputBytes)}},
		VolumeMounts:    collectorMounts,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Resources:       corev1.ResourceRequirements{Requests: collectorRes.DeepCopy(), Limits: collectorRes},
		SecurityContext: e.restrictedContainerSecurity(true),
	}

	var pullSecrets []corev1.LocalObjectReference
	for _, s := range cfg.ImagePullSecrets {
		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: s})
	}

	nodeSelector := map[string]string{}
	for k, v := range cfg.NodeSelector {
		nodeSelector[k] = v
	}
	if kind := spec.Limit.GetGpuKind(); kind != "" && spec.Limit.GetGpuCount() > 0 && cfg.GPUKindLabel != "" {
		// The Nexus scheduler matched gpu_kind against this WORKER's advertised
		// capacity; this pins the pod to nodes that really carry that model,
		// so the two schedulers cannot disagree about which GPU runs the task.
		nodeSelector[cfg.GPUKindLabel] = kind
	}
	if len(nodeSelector) == 0 {
		nodeSelector = nil
	}

	runAs := cfg.RunAsUser
	graceSecs := int64(math.Ceil(grace.Seconds()))
	if graceSecs < 1 {
		graceSecs = 1
	}
	podSpec := corev1.PodSpec{
		RestartPolicy:                 corev1.RestartPolicyNever,
		TerminationGracePeriodSeconds: &graceSecs,
		ServiceAccountName:            cfg.ServiceAccount,
		// A task has no business talking to the Kubernetes API as anyone; not
		// mounting the token closes the path by which a compromised task would
		// escalate to whatever its service account can do.
		AutomountServiceAccountToken: ptrTo(false),
		// Service links inject one env var per Service in the namespace — an
		// inventory of the cluster handed to untrusted code, and a startup
		// failure once there are enough of them to overflow the environment.
		EnableServiceLinks: ptrTo(false),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   ptrTo(true),
			RunAsUser:      &runAs,
			RunAsGroup:     &runAs,
			FSGroup:        &runAs,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		InitContainers:   []corev1.Container{collector},
		Containers:       []corev1.Container{task},
		Volumes:          volumes,
		NodeSelector:     nodeSelector,
		Tolerations:      append([]corev1.Toleration(nil), cfg.Tolerations...),
		ImagePullSecrets: pullSecrets,
	}

	replacement := batchv1.Failed
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: cfg.Namespace,
			Labels: labels, Annotations: ann,
		},
		Spec: batchv1.JobSpec{
			// Nexus owns retries. A Kubernetes-level retry would re-run the
			// attempt under the SAME attempt id and lease, invisible to the
			// control plane's retry budget, error classes and backoff — two
			// retry policies fighting over one task.
			BackoffLimit: ptrTo(int32(0)),
			Parallelism:  ptrTo(int32(1)),
			Completions:  ptrTo(int32(1)),
			// Never run a replacement pod while the first is still
			// terminating; with backoffLimit 0 this means never replace at all.
			PodReplacementPolicy: &replacement,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{
					k8sAnnAttemptID: spec.AttemptID,
				}},
				Spec: podSpec,
			},
		},
	}
	if ttl := cfg.TTLAfterFinished; ttl > 0 {
		job.Spec.TTLSecondsAfterFinished = ptrTo(int32(ttl / time.Second))
	}
	if !spec.StartToCloseDeadline.IsZero() {
		// The start_to_close deadline enforced BY THE CLUSTER, so it holds even
		// if this agent dies: the Job controller terminates the pod (SIGTERM,
		// then SIGKILL after terminationGracePeriodSeconds) and marks the Job
		// DeadlineExceeded. The agent's own timer remains as a backstop.
		remaining := int64(math.Ceil(time.Until(spec.StartToCloseDeadline).Seconds()))
		if remaining < 1 {
			remaining = 1
		}
		job.Spec.ActiveDeadlineSeconds = &remaining
	}
	// Only within ONE namespace: owner references cannot cross namespaces, and
	// the garbage collector treats a cross-namespace owner as ABSENT and
	// deletes the dependent at once -- every Job would vanish on creation.
	if cfg.OwnerPodName != "" && cfg.OwnerPodUID != "" && cfg.OwnerPodNamespace == cfg.Namespace {
		// Owned by the agent's own Pod: if the agent pod is deleted, the
		// garbage collector deletes its Jobs. That is lease fencing done by the
		// cluster — a dead agent's session can never renew these leases, so
		// the attempts are LOST and will be retried elsewhere, and the old
		// workloads must not keep running beside the retries.
		job.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "v1", Kind: "Pod",
			Name: cfg.OwnerPodName, UID: types.UID(cfg.OwnerPodUID),
			// false: blocking owner deletion needs update on pods/finalizers,
			// which the least-privilege Role deliberately does not grant.
			BlockOwnerDeletion: ptrTo(false),
		}}
	}
	return job, secret, nil
}

// k8sInputData renders staged inputs as Secret data keyed by sanitised name —
// the same file names the other executors write under inputs/.
func k8sInputData(inputs map[string]*nexusv1.ResultRef) (map[string][]byte, error) {
	data := map[string][]byte{}
	total := 0
	for name, ref := range inputs {
		if ref == nil {
			continue
		}
		switch k := ref.GetKind().(type) {
		case *nexusv1.ResultRef_Inline:
			data[sanitizeID(name)] = k.Inline
			total += len(k.Inline)
		case *nexusv1.ResultRef_Blob:
			return nil, fmt.Errorf("%w: input %q is an unfetched blob ref (%s/%s)",
				ErrValidation, name, k.Blob.GetBucket(), k.Blob.GetKey())
		}
	}
	if total > k8sMaxInputBytes {
		// Terminal rather than retryable: every kubernetes worker would refuse
		// it the same way, so a retry only burns budget. The message names the
		// fix instead of leaving the author to decode an API-server 413.
		return nil, fmt.Errorf("%w: staged inputs total %d bytes; the kubernetes executor stages inputs "+
			"through a Secret capped at %d bytes (run this task on the docker executor, or pass a reference "+
			"the task fetches itself)", ErrValidation, total, k8sMaxInputBytes)
	}
	return data, nil
}

func ptrTo[T any](v T) *T { return &v }
