// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// KubernetesConfig is the kubernetes executor's static configuration.
//
// Every field has an NEXUS_K8S_* environment variable (LoadKubernetesConfigFromEnv),
// following the agent's existing NEXUS_* convention, so a Deployment manifest
// configures the agent the same way docker-compose does.
type KubernetesConfig struct {
	// Namespace is where attempt Jobs are created. Setting it is what ENABLES
	// the executor: an agent with no target namespace does not advertise
	// `kubernetes`, so the scheduler never routes kubernetes work to it.
	Namespace string
	// Kubeconfig and Context select an out-of-cluster cluster. Both empty and
	// KUBERNETES_SERVICE_HOST set means in-cluster config (the agent's own
	// service account).
	Kubeconfig string
	Context    string

	// ServiceAccount the TASK pods run as. Its token is never mounted
	// (automountServiceAccountToken=false); it exists so pull secrets and
	// admission policy can be attached to it. Empty means the namespace default.
	ServiceAccount string

	// Owner identifies THIS agent instance on every object it creates, and
	// scopes orphan reaping. It must be stable across agent restarts (so a
	// restarted agent finds its predecessor's Jobs) and unique among agents
	// sharing a namespace (so it never reaps a sibling's). Defaults to the
	// hostname, which is the pod name in-cluster.
	Owner string
	// OwnerPodName/OwnerPodUID (downward API) make each Job owned by the agent
	// pod, so deleting the agent pod garbage-collects its workloads.
	OwnerPodName string
	OwnerPodUID  string

	// HelperImage runs the output-collector sidecar. It needs only /bin/sh,
	// base64, wc and sleep. Pin it; mirror it for air-gapped clusters.
	HelperImage string

	ImagePullPolicy  corev1.PullPolicy
	ImagePullSecrets []string

	// RunAsUser is the non-root uid (and gid, fsGroup) of task pods.
	RunAsUser int64
	// WritableRootFilesystem relaxes readOnlyRootFilesystem for images that
	// insist on writing outside /nexus and /tmp. Off by default: a read-only
	// root turns "the task quietly filled the node's image layer" into an
	// immediate, legible EROFS.
	WritableRootFilesystem bool

	// NodeSelector and Tolerations pin THIS worker's task pods to a node pool.
	// Nexus placement (TaskSpec.node_selector) selects a WORKER by its labels;
	// a kubernetes worker represents a pool, so pool pinning is worker config.
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration

	// GPUResourceName is the device-plugin extended resource for GPUs, e.g.
	// nvidia.com/gpu. Empty refuses GPU tasks rather than running them with no
	// GPU at all.
	GPUResourceName string
	// GPUKindLabel is the node label carrying the GPU model (e.g.
	// nvidia.com/gpu.product); when set, gpu_kind becomes a nodeSelector.
	GPUKindLabel string

	// TTLAfterFinished lets the Job controller delete a finished Job the agent
	// never cleaned up (agent crashed). It is ALSO the window in which a
	// restarted agent can still harvest a finished attempt's result.
	TTLAfterFinished time.Duration
	// ImagePullTimeout bounds a TRANSIENT pull failure (registry down). A
	// not-found or unauthorised pull fails at once regardless.
	ImagePullTimeout time.Duration
	// StartTimeout bounds how long a pod may stay unscheduled or not started
	// (no capacity, quota, admission rejection) before the attempt fails
	// retryably so another worker can take it.
	StartTimeout time.Duration
}

// DefaultK8sHelperImage is pinned by version: an unpinned helper would make
// the output transport change underneath a running fleet.
const DefaultK8sHelperImage = "busybox:1.37.0"

// DefaultK8sRunAsUser is the conventional "nonroot" uid of distroless images.
const DefaultK8sRunAsUser = 65532

func (c *KubernetesConfig) applyDefaults() {
	if c.Owner == "" {
		c.Owner, _ = os.Hostname()
	}
	if c.HelperImage == "" {
		c.HelperImage = DefaultK8sHelperImage
	}
	if c.RunAsUser <= 0 {
		c.RunAsUser = DefaultK8sRunAsUser
	}
	if c.TTLAfterFinished <= 0 {
		c.TTLAfterFinished = 10 * time.Minute
	}
	if c.ImagePullTimeout <= 0 {
		c.ImagePullTimeout = 10 * time.Minute
	}
	if c.StartTimeout <= 0 {
		c.StartTimeout = 10 * time.Minute
	}
}

// LoadKubernetesConfigFromEnv reads NEXUS_K8S_*. enabled is false when no
// target namespace is configured — the executor is opt-in per agent.
func LoadKubernetesConfigFromEnv(getenv func(string) string) (cfg KubernetesConfig, enabled bool, err error) {
	cfg.Namespace = strings.TrimSpace(getenv("NEXUS_K8S_NAMESPACE"))
	if cfg.Namespace == "" {
		return cfg, false, nil
	}
	cfg.Kubeconfig = getenv("NEXUS_K8S_KUBECONFIG")
	cfg.Context = getenv("NEXUS_K8S_CONTEXT")
	cfg.ServiceAccount = getenv("NEXUS_K8S_SERVICE_ACCOUNT")
	cfg.Owner = getenv("NEXUS_K8S_OWNER")
	cfg.OwnerPodName = getenv("NEXUS_K8S_OWNER_POD_NAME")
	cfg.OwnerPodUID = getenv("NEXUS_K8S_OWNER_POD_UID")
	cfg.HelperImage = getenv("NEXUS_K8S_HELPER_IMAGE")
	cfg.GPUResourceName = getenv("NEXUS_K8S_GPU_RESOURCE")
	cfg.GPUKindLabel = getenv("NEXUS_K8S_GPU_KIND_LABEL")
	cfg.ImagePullSecrets = splitList(getenv("NEXUS_K8S_IMAGE_PULL_SECRETS"))

	switch p := corev1.PullPolicy(getenv("NEXUS_K8S_IMAGE_PULL_POLICY")); p {
	case "", corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
		cfg.ImagePullPolicy = p
	default:
		return cfg, true, fmt.Errorf("NEXUS_K8S_IMAGE_PULL_POLICY=%q: want Always, IfNotPresent or Never", p)
	}
	if v := getenv("NEXUS_K8S_RUN_AS_USER"); v != "" {
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || n <= 0 {
			// uid 0 is refused outright: runAsNonRoot would reject it anyway,
			// and failing at boot beats every task failing at admission.
			return cfg, true, fmt.Errorf("NEXUS_K8S_RUN_AS_USER=%q: want a positive non-root uid", v)
		}
		cfg.RunAsUser = n
	}
	cfg.WritableRootFilesystem = getenv("NEXUS_K8S_WRITABLE_ROOT_FS") == "true"

	if cfg.NodeSelector, err = parseKV(getenv("NEXUS_K8S_NODE_SELECTOR")); err != nil {
		return cfg, true, fmt.Errorf("NEXUS_K8S_NODE_SELECTOR: %w", err)
	}
	if cfg.Tolerations, err = parseTolerations(getenv("NEXUS_K8S_TOLERATIONS")); err != nil {
		return cfg, true, fmt.Errorf("NEXUS_K8S_TOLERATIONS: %w", err)
	}
	for name, dst := range map[string]*time.Duration{
		"NEXUS_K8S_TTL_AFTER_FINISHED": &cfg.TTLAfterFinished,
		"NEXUS_K8S_IMAGE_PULL_TIMEOUT": &cfg.ImagePullTimeout,
		"NEXUS_K8S_START_TIMEOUT":      &cfg.StartTimeout,
	} {
		if v := getenv(name); v != "" {
			d, derr := time.ParseDuration(v)
			if derr != nil {
				return cfg, true, fmt.Errorf("%s=%q: %w", name, v, derr)
			}
			*dst = d
		}
	}
	cfg.applyDefaults()
	return cfg, true, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseKV(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range splitList(s) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("malformed entry %q; want key=value", kv)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// parseTolerations reads "key=value:Effect,key:Effect,key=value" (the kubectl
// taint spelling). A value means operator Equal; no value means Exists.
func parseTolerations(s string) ([]corev1.Toleration, error) {
	var out []corev1.Toleration
	for _, item := range splitList(s) {
		kv, effect, _ := strings.Cut(item, ":")
		t := corev1.Toleration{Effect: corev1.TaintEffect(effect)}
		switch t.Effect {
		case "", corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute:
		default:
			return nil, fmt.Errorf("toleration %q: unknown effect %q", item, effect)
		}
		if k, v, ok := strings.Cut(kv, "="); ok {
			t.Key, t.Value, t.Operator = k, v, corev1.TolerationOpEqual
		} else {
			t.Key, t.Operator = kv, corev1.TolerationOpExists
		}
		if t.Key == "" {
			return nil, fmt.Errorf("toleration %q has no key", item)
		}
		out = append(out, t)
	}
	return out, nil
}

// RestConfig resolves how to reach the cluster: explicit kubeconfig/context
// first, in-cluster when running in a pod, else the default loading rules
// ($KUBECONFIG, ~/.kube/config) for a developer's machine.
func (c KubernetesConfig) RestConfig() (*rest.Config, error) {
	if c.Kubeconfig == "" && c.Context == "" && os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		rc, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("in-cluster kubernetes config: %w", err)
		}
		return rc, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if c.Kubeconfig != "" {
		rules.ExplicitPath = c.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: c.Context}
	rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig (path=%q context=%q): %w", c.Kubeconfig, c.Context, err)
	}
	return rc, nil
}

// k8sMinVersion is the oldest API server the executor supports: native sidecar
// containers (the output collector) are on by default from 1.29.
var k8sMinVersion = version.MustParseGeneric("1.29.0")

// Probe verifies that the cluster is reachable, new enough, and that this
// agent may do what the executor needs in the target namespace.
//
// It exists so the agent advertises `kubernetes` only when it can actually run
// kubernetes work. Advertising an executor it cannot use would turn every task
// routed here into an INFRASTRUCTURE failure — and the scheduler would keep
// routing them here, because we said we could.
func (e *KubernetesExecutor) Probe(ctx context.Context) error {
	if e.Client == nil {
		return fmt.Errorf("%w: kubernetes client not configured", ErrRuntimeUnavailable)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	info, err := e.Client.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("%w: kubernetes API unreachable: %v", ErrRuntimeUnavailable, err)
	}
	v, err := version.ParseGeneric(info.GitVersion)
	if err == nil && v.LessThan(k8sMinVersion) {
		return fmt.Errorf("%w: kubernetes %s is older than the %s the executor needs (native sidecars)",
			ErrRuntimeUnavailable, info.GitVersion, k8sMinVersion)
	}
	if _, err := e.Client.BatchV1().Jobs(e.Config.Namespace).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		return fmt.Errorf("%w: list jobs in namespace %q: %v", ErrRuntimeUnavailable, e.Config.Namespace, err)
	}
	// Listing proves read access only; the executor's first real act is a
	// create, so ask the authorizer rather than discover the gap on a task.
	for _, ra := range []authorizationv1.ResourceAttributes{
		{Namespace: e.Config.Namespace, Verb: "create", Group: "batch", Resource: "jobs"},
		{Namespace: e.Config.Namespace, Verb: "delete", Resource: "pods"},
		{Namespace: e.Config.Namespace, Verb: "get", Resource: "pods", Subresource: "log"},
		{Namespace: e.Config.Namespace, Verb: "create", Resource: "secrets"},
	} {
		ra := ra
		r, err := e.Client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx,
			&authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &ra}},
			metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("%w: access review for %s %s/%s: %v", ErrRuntimeUnavailable, ra.Verb, ra.Resource, ra.Subresource, err)
		}
		if !r.Status.Allowed {
			return fmt.Errorf("%w: agent may not %s %s%s in namespace %q (see deploy/k8s/agent-rbac.yaml)",
				ErrRuntimeUnavailable, ra.Verb, ra.Resource, subresSuffix(ra.Subresource), e.Config.Namespace)
		}
	}
	return nil
}

func subresSuffix(s string) string {
	if s == "" {
		return ""
	}
	return "/" + s
}

// NewKubernetesClient builds a clientset for cfg. Separate from the executor so
// tests can inject a fake clientset.
func NewKubernetesClient(cfg KubernetesConfig) (kubernetes.Interface, error) {
	rc, err := cfg.RestConfig()
	if err != nil {
		return nil, err
	}
	// A task-per-pod executor issues bursts of creates and deletes; the
	// client-go defaults (5 QPS) would throttle a busy agent into missing its
	// own deadlines.
	rc.QPS, rc.Burst = 50, 100
	rc.UserAgent = "nexus-agent/kubernetes-executor"
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}
	return cs, nil
}
