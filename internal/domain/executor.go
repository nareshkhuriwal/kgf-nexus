// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

// Executors (ADR-0007): the substrate an attempt runs on. A worker advertises
// the ones it runs; a task may name one in runtime.executor, and a task that
// names none runs on the claiming worker's DEFAULT executor.
//
// The default is advertised as a label rather than a registration field so
// that it reaches every reader of the worker row -- both claim directions,
// the usage ledger, the console -- without a schema or wire change, exactly
// like nexus.io/k8s-task-namespace. The claim reads it from the worker's OWN
// labels, never from the cluster overlay: it describes what this agent
// process does, which no cluster label can change.
const (
	ExecutorSubprocess = "subprocess"
	ExecutorDocker     = "docker"
	ExecutorKubernetes = "kubernetes"
	// ExecutorK8sAlias is the workflow schema's other spelling of kubernetes.
	// An agent that runs kubernetes advertises both.
	ExecutorK8sAlias = "k8s"

	// LabelDefaultExecutor names the executor this worker runs a task on when
	// the task names none (the agent's NEXUS_DEFAULT_EXECUTOR). The agent sets
	// it on every registration; an operator never does. Absent on agents that
	// predate it, which run such tasks as subprocesses -- so absent means
	// subprocess.
	LabelDefaultExecutor = "nexus.io/default-executor"
)

// KnownExecutors are the executor names an agent can build, canonical
// spellings only, in the order an agent lists them.
var KnownExecutors = []string{ExecutorSubprocess, ExecutorDocker, ExecutorKubernetes}

// CanonicalExecutor folds the k8s alias into kubernetes; any other name is
// returned unchanged.
func CanonicalExecutor(kind string) string {
	if kind == ExecutorK8sAlias {
		return ExecutorKubernetes
	}
	return kind
}

// IsKnownExecutor reports whether kind (either kubernetes spelling) is an
// executor an agent can build.
func IsKnownExecutor(kind string) bool {
	switch CanonicalExecutor(kind) {
	case ExecutorSubprocess, ExecutorDocker, ExecutorKubernetes:
		return true
	}
	return false
}

// WorkerDefaultExecutor is the executor a worker runs a task on when the task
// names none, read from the worker's OWN labels. A worker without the label
// predates it and runs such tasks as subprocesses.
func WorkerDefaultExecutor(ownLabels map[string]string) string {
	if def := CanonicalExecutor(ownLabels[LabelDefaultExecutor]); def != "" {
		return def
	}
	return ExecutorSubprocess
}

// EffectiveExecutor is the executor a task runs on when this worker claims it:
// the one the task names, else the worker's default.
func EffectiveExecutor(named string, ownLabels map[string]string) string {
	if named != "" {
		return CanonicalExecutor(named)
	}
	return WorkerDefaultExecutor(ownLabels)
}

// ExecutorRunsImage reports whether an executor runs the task's runtime.image
// (docker and kubernetes) and so cannot run a task that has none. The workflow
// schema states the same rule for a task that NAMES one of them.
func ExecutorRunsImage(kind string) bool {
	switch CanonicalExecutor(kind) {
	case ExecutorDocker, ExecutorKubernetes:
		return true
	}
	return false
}

// CanRunUnnamed reports whether a worker whose default executor is def can run
// a task that names no executor. A subprocess default runs anything, which is
// every worker before NEXUS_DEFAULT_EXECUTOR existed; a container default
// needs the task to carry an image. Without this check a kubernetes-only pool
// would claim an image-less task and fail it VALIDATION, while a subprocess
// worker elsewhere could have run it.
func CanRunUnnamed(def string, hasImage bool) bool {
	return hasImage || !ExecutorRunsImage(def)
}
