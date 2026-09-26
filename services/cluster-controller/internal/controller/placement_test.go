// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestClusterLabelsObservedAndDeclared(t *testing.T) {
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"topology.kubernetes.io/region": "eu-west-1"}},
			Spec: corev1.NodeSpec{ProviderID: "aws:///eu-west-1a/i-1"}},
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"topology.kubernetes.io/region": "eu-west-1"}},
			Spec: corev1.NodeSpec{ProviderID: "aws:///eu-west-1b/i-2"}},
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"topology.kubernetes.io/region": "us-east-1"}}},
	}
	l := clusterLabels(map[string]string{"nexus.io/cost-tier": "2"}, nodes)
	if l[labelRegion] != "eu-west-1" || l[labelProvider] != "aws" || l["nexus.io/cost-tier"] != "2" {
		t.Fatalf("labels %v", l)
	}
	if l := clusterLabels(map[string]string{labelRegion: "override"}, nodes); l[labelRegion] != "override" {
		t.Fatal("a declared label did not override the observation")
	}
}

func quota(hard, used corev1.ResourceList) corev1.ResourceQuota {
	return corev1.ResourceQuota{Spec: corev1.ResourceQuotaSpec{Hard: hard}, Status: corev1.ResourceQuotaStatus{Hard: hard, Used: used}}
}

func TestNamespaceHeadroomTakesTheTightestQuota(t *testing.T) {
	q1 := quota(corev1.ResourceList{"requests.cpu": mustQ("8"), "requests.memory": mustQ("32Gi"),
		"requests.nvidia.com/gpu": mustQ("4"), "pods": mustQ("20"), "requests.nvidia.com/mig-1g.10gb": mustQ("7")},
		corev1.ResourceList{"requests.cpu": mustQ("6500m"), "requests.memory": mustQ("8Gi"),
			"requests.nvidia.com/gpu": mustQ("3"), "pods": mustQ("5"), "requests.nvidia.com/mig-1g.10gb": mustQ("2")})
	q2 := quota(corev1.ResourceList{"cpu": mustQ("4")}, corev1.ResourceList{"cpu": mustQ("3")})
	h, ok := NamespaceHeadroom("tasks", []corev1.ResourceQuota{q1, q2}, []string{"nvidia.com/gpu"})
	if !ok {
		t.Fatal("headroom not computed")
	}
	if h.CpuMillis != 1000 || h.MemoryBytes != 24<<30 || h.GpuCount != 1 || h.Pods != 15 ||
		h.Extended["nvidia.com/mig-1g.10gb"] != 5 {
		t.Fatalf("headroom %+v", h)
	}
	// No quota on a resource: unbounded (-1), never zero.
	h, _ = NamespaceHeadroom("free", nil, nil)
	if h.CpuMillis != -1 || h.Pods != -1 {
		t.Fatalf("unconstrained namespace %+v", h)
	}
	// Usage not computed yet: not reported at all.
	fresh := quota(corev1.ResourceList{"pods": mustQ("1")}, corev1.ResourceList{})
	if _, ok := NamespaceHeadroom("new", []corev1.ResourceQuota{fresh}, nil); ok {
		t.Fatal("reported headroom for a quota whose usage is unknown")
	}
}

// The report carries labels (from the Node list) and headroom for the bound
// worker namespace.
func TestReportCarriesPlacementFacts(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1), newFakeRM("plan")
	registerAll(fc, rm)
	if _, err := fc.kube.CoreV1().Nodes().Create(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"topology.kubernetes.io/region": "eu-west-1"}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c := newController(t, fc, rm, false, clk)
	c.cfg.ClusterLabels = map[string]string{"nexus.io/data-locality": "lake-eu"}
	_ = c.RunOnce(context.Background())
	rm.mu.Lock()
	defer rm.mu.Unlock()
	last := rm.reports[len(rm.reports)-1]
	if !last.GetLabelsReported() || last.GetLabels()[labelRegion] != "eu-west-1" ||
		last.GetLabels()["nexus.io/data-locality"] != "lake-eu" {
		t.Fatalf("labels %v reported=%v", last.GetLabels(), last.GetLabelsReported())
	}
	if !last.GetHeadroomReported() {
		t.Fatal("headroom not reported")
	}
}
