// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package eviction

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DefaultEvictionTaints are the taint keys that mean "this node is about to
// be removed", as placed by the common termination handlers and autoscalers.
// Each is set before the node goes away, which is the whole point: the pod
// sees it while there is still time to drain.
//
// Deliberately NOT included: aws-node-termination-handler/rebalance-recommendation
// and DeletionCandidateOfClusterAutoscaler. Both mean "this node MAY go away"
// (elevated reclaim risk, a soft scale-down candidate), not "it is going", and
// draining on them would checkpoint-kill work on nodes that usually survive.
// Operators who want that trade can list them in NEXUS_EVICTION_TAINTS.
var DefaultEvictionTaints = []string{
	"cloud.google.com/impending-node-termination", // GKE graceful node shutdown
	"aws-node-termination-handler/spot-itn",       // NTH: spot interruption notice
	"karpenter.sh/disruption",                     // Karpenter consolidation / interruption
	"ToBeDeletedByClusterAutoscaler",              // cluster-autoscaler scale-down in progress
}

// KubernetesNodeSource watches the agent's own Node for an eviction taint or
// for the Node object being deleted.
//
// It polls with GET rather than a watch: one small request per interval is
// negligible load, it needs only `get` on nodes (a ClusterRole, since Nodes
// are cluster-scoped, bound to the agent's service account; a
// resourceNames-restricted rule is enough if the node name is known), and it
// has no reconnect/resourceVersion bookkeeping to get wrong.
type KubernetesNodeSource struct {
	Client   kubernetes.Interface
	NodeName string
	// Taints are the taint keys that signal eviction; nil means
	// DefaultEvictionTaints.
	Taints []string
	Poll   time.Duration

	seen bool // the node has been read successfully at least once
}

// Name implements Source.
func (s *KubernetesNodeSource) Name() string { return SourceKubernetes }

// Watch implements Source.
func (s *KubernetesNodeSource) Watch(ctx context.Context, out chan<- Notice) error {
	if s.Client == nil {
		return errors.New("eviction: kubernetes source needs a client")
	}
	if s.NodeName == "" {
		return errors.New("eviction: kubernetes source needs a node name (NEXUS_NODE_NAME)")
	}
	return pollLoop(ctx, s.Poll, out, s.check)
}

func (s *KubernetesNodeSource) check(ctx context.Context) (Notice, bool, error) {
	node, err := s.Client.CoreV1().Nodes().Get(ctx, s.NodeName, metav1.GetOptions{})
	switch {
	case err == nil:
	case apierrors.IsNotFound(err):
		if s.seen {
			// The node we were running on has been deleted from the
			// cluster: it is going, if not already gone.
			return Notice{
				Source: SourceKubernetes,
				Reason: fmt.Sprintf("kubernetes node %s deleted", s.NodeName),
			}, true, nil
		}
		// Never seen: NEXUS_NODE_NAME is wrong, and retrying will not fix it.
		return Notice{}, false, permanent(fmt.Errorf("eviction: kubernetes node %q not found", s.NodeName))
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		return Notice{}, false, permanent(fmt.Errorf(
			"eviction: kubernetes source cannot read node %q (grant `get` on nodes): %w", s.NodeName, err))
	default:
		return Notice{}, false, fmt.Errorf("eviction: get node %q: %w", s.NodeName, err)
	}
	s.seen = true

	if node.DeletionTimestamp != nil {
		return Notice{
			Source: SourceKubernetes,
			Reason: fmt.Sprintf("kubernetes node %s is being deleted", s.NodeName),
		}, true, nil
	}
	if t, ok := matchTaint(node.Spec.Taints, s.taints()); ok {
		return Notice{
			Source: SourceKubernetes,
			Reason: fmt.Sprintf("kubernetes node %s tainted %s", s.NodeName, describeTaint(t)),
		}, true, nil
	}
	return Notice{}, false, nil
}

func (s *KubernetesNodeSource) taints() []string {
	if s.Taints == nil {
		return DefaultEvictionTaints
	}
	return s.Taints
}

func matchTaint(taints []corev1.Taint, keys []string) (corev1.Taint, bool) {
	for _, t := range taints {
		for _, k := range keys {
			if t.Key == k {
				return t, true
			}
		}
	}
	return corev1.Taint{}, false
}

func describeTaint(t corev1.Taint) string {
	var b strings.Builder
	b.WriteString(t.Key)
	if t.Value != "" {
		b.WriteByte('=')
		b.WriteString(t.Value)
	}
	if t.Effect != "" {
		b.WriteByte(':')
		b.WriteString(string(t.Effect))
	}
	return b.String()
}

// CapacityTypeFromNodeLabels derives the capacity type ("spot" or
// "on-demand") from the well-known labels provisioners put on Nodes, or ""
// when the labels say nothing (ADR-0030 §1: absent means the operator did not
// say, and the agent then falls back to NEXUS_CAPACITY_TYPE or on-demand).
//
// Karpenter's label is checked first because Karpenter also runs on EKS, where
// it is authoritative over the managed-node-group label.
func CapacityTypeFromNodeLabels(labels map[string]string) string {
	switch strings.ToLower(labels["karpenter.sh/capacity-type"]) {
	case "spot":
		return "spot"
	case "on-demand":
		return "on-demand"
	}
	switch strings.ToUpper(labels["eks.amazonaws.com/capacityType"]) {
	case "SPOT":
		return "spot"
	case "ON_DEMAND":
		return "on-demand"
	}
	if strings.EqualFold(labels["cloud.google.com/gke-spot"], "true") ||
		strings.EqualFold(labels["cloud.google.com/gke-preemptible"], "true") {
		return "spot"
	}
	switch strings.ToLower(labels["kubernetes.azure.com/scalesetpriority"]) {
	case "spot":
		return "spot"
	case "regular":
		return "on-demand"
	}
	return ""
}
