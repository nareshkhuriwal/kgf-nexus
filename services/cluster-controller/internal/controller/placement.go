// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
)

// Placement facts the controller publishes for multi-cluster placement
// (ADR-0031): the cluster's labels and, per task namespace, how much room its
// ResourceQuotas leave. Read-only against the cluster, like all observation.

// Well-known placement label keys (internal/domain mirrors them; duplicated
// here to keep the controller free of control-plane packages).
const (
	labelRegion   = "nexus.io/region"
	labelProvider = "nexus.io/provider"
)

// clusterLabels merges the operator's declared labels with what the Nodes say:
// the most common topology.kubernetes.io/region and the providerID scheme.
// Declared labels win, so an operator can correct an observation.
func clusterLabels(declared map[string]string, nodes []corev1.Node) map[string]string {
	out := map[string]string{}
	regions, providers := map[string]int{}, map[string]int{}
	for i := range nodes {
		if r := nodes[i].Labels["topology.kubernetes.io/region"]; r != "" {
			regions[r]++
		}
		if scheme, _, ok := strings.Cut(nodes[i].Spec.ProviderID, "://"); ok && scheme != "" {
			providers[scheme]++
		}
	}
	if r := mostCommon(regions); r != "" {
		out[labelRegion] = r
	}
	if p := mostCommon(providers); p != "" {
		out[labelProvider] = p
	}
	for k, v := range declared {
		out[k] = v
	}
	return out
}

func mostCommon(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	best, n := "", 0
	for _, k := range keys {
		if m[k] > n {
			best, n = k, m[k]
		}
	}
	return best
}

// NamespaceHeadroom is min(hard - used) over the namespace's quotas, per
// resource the claim understands; -1 where no quota constrains it. ok=false
// when some quota's usage is not computed yet: an unknown room is not reported,
// so the claim gates nothing on it (ADR-0031 §3), rather than reporting zero.
func NamespaceHeadroom(ns string, quotas []corev1.ResourceQuota, gpuResources []string) (*rmv1.NamespaceHeadroom, bool) {
	h := &rmv1.NamespaceHeadroom{Namespace: ns, CpuMillis: -1, MemoryBytes: -1, GpuCount: -1, Pods: -1,
		Extended: map[string]int64{}}
	isGPU := map[string]bool{}
	for _, g := range gpuResources {
		isGPU[g] = true
	}
	lower := func(cur *int64, v int64) {
		if *cur < 0 || v < *cur {
			*cur = v
		}
	}
	for _, rq := range quotas {
		for name, hard := range rq.Spec.Hard {
			used, known := rq.Status.Used[name]
			if !known {
				return nil, false
			}
			room := hard.DeepCopy()
			room.Sub(used)
			n := string(name)
			base := strings.TrimPrefix(n, "requests.")
			switch {
			case n == "pods" || n == "count/pods":
				lower(&h.Pods, clampNonNeg(room.Value()))
			case base == "cpu" && !strings.HasPrefix(n, "limits."):
				lower(&h.CpuMillis, clampNonNeg(room.MilliValue()))
			case base == "memory" && !strings.HasPrefix(n, "limits."):
				lower(&h.MemoryBytes, clampNonNeg(room.Value()))
			case strings.HasPrefix(n, "requests.") && isGPU[base]:
				lower(&h.GpuCount, clampNonNeg(room.Value()))
			case strings.HasPrefix(n, "requests.") && strings.Contains(base, "/"):
				v := clampNonNeg(room.Value())
				if cur, ok := h.Extended[base]; !ok || v < cur {
					h.Extended[base] = v
				}
			}
		}
	}
	return h, true
}

func clampNonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	if v > math.MaxInt64/2 {
		return math.MaxInt64 / 2
	}
	return v
}

// taskNamespaces are where task pods run: the configured
// NEXUS_CLUSTER_TASK_NAMESPACES, else the bound worker namespaces (where an
// agent whose kubernetes executor uses its own namespace runs its Jobs).
func (c *Controller) taskNamespaces(a *rmv1.ClusterAssignment) []string {
	if len(c.cfg.TaskNamespaces) > 0 {
		return c.cfg.TaskNamespaces
	}
	var out []string
	seen := map[string]bool{}
	for _, b := range a.GetBindings() {
		if ns := b.GetKubernetesNamespace(); !seen[ns] {
			seen[ns] = true
			out = append(out, ns)
		}
	}
	return out
}

// headroom lists the task namespaces' quotas and computes their room. Returns
// reported=false when any list failed, so a partial view never replaces the
// stored one.
func (c *Controller) headroom(ctx context.Context, a *rmv1.ClusterAssignment) ([]*rmv1.NamespaceHeadroom, bool, error) {
	namespaces := c.taskNamespaces(a)
	gpus := c.cfg.GPUResourceNames
	if len(gpus) == 0 {
		gpus = []string{"nvidia.com/gpu"}
	}
	var out []*rmv1.NamespaceHeadroom
	for _, ns := range namespaces {
		kctx, cancel := c.rpcCtx(ctx)
		list, err := c.kube.CoreV1().ResourceQuotas(ns).List(kctx, metav1.ListOptions{})
		cancel()
		if err != nil {
			return nil, false, fmt.Errorf("observe resource quotas in %s: %w", ns, err)
		}
		if h, ok := NamespaceHeadroom(ns, list.Items, gpus); ok {
			out = append(out, h)
		}
	}
	return out, true, nil
}
