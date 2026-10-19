// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"fmt"
	"strings"
)

// Scheduler autoscaling applied by the cluster controller (ADR-0036).
//
// The console saves four HPA settings per namespace (Scheduler.MinReplicas,
// MaxReplicas, TargetCPU, ScaleDownSeconds). The controller compares them with
// the scheduler Deployment's HorizontalPodAutoscaler and, in apply mode, moves
// the HPA toward them; the gateway compares the same two to tell the console
// whether they agree. Both sides use this file, so "in sync" and "how far one
// patch may go" have one definition.

// HPAOwnerAnnotation, set to HPAOwnerValue on a scheduler HPA, is the opt-in
// that lets the cluster controller change the four settings below on that HPA.
// An HPA without it belongs to whoever created it (Helm, a GitOps tool, a
// human): the controller reports it and never writes it.
const (
	HPAOwnerAnnotation = "nexus.khuriwalgroup.com/autoscaling-owner"
	HPAOwnerValue      = "cluster-controller"
)

// DefaultScaleDownStabilizationSeconds is what Kubernetes applies when an HPA
// sets no behavior.scaleDown.stabilizationWindowSeconds, so an HPA without
// one is compared as if it said 300.
const DefaultScaleDownStabilizationSeconds = 300

// HPASettings are the HorizontalPodAutoscaler settings Nexus manages: the
// replica bounds, the CPU utilization target and the scale-down stabilization
// window. Nothing else on an HPA is Nexus's to change.
type HPASettings struct {
	MinReplicas int32 `json:"min_replicas"`
	MaxReplicas int32 `json:"max_replicas"`
	// TargetCPUPercent is 0 when the HPA has no CPU utilization metric, which
	// a saved setting (10-90) can never equal.
	TargetCPUPercent              int32 `json:"target_cpu_percent"`
	ScaleDownStabilizationSeconds int32 `json:"scale_down_stabilization_seconds"`
}

// HPASettings is the saved policy in HPA terms.
func (s Scheduler) HPASettings() HPASettings {
	return HPASettings{
		MinReplicas: int32(s.MinReplicas), MaxReplicas: int32(s.MaxReplicas),
		TargetCPUPercent: int32(s.TargetCPU), ScaleDownStabilizationSeconds: int32(s.ScaleDownSeconds),
	}
}

// Diff describes, one clause per differing setting, how cur would change to
// reach want ("min 2→3, max 6→10"). Empty when they are equal.
func (cur HPASettings) Diff(want HPASettings) string {
	var parts []string
	add := func(name string, a, b int32, unit string) {
		if a != b {
			parts = append(parts, fmt.Sprintf("%s %d%s→%d%s", name, a, unit, b, unit))
		}
	}
	add("min", cur.MinReplicas, want.MinReplicas, "")
	add("max", cur.MaxReplicas, want.MaxReplicas, "")
	if cur.TargetCPUPercent == 0 && want.TargetCPUPercent != 0 {
		parts = append(parts, fmt.Sprintf("cpu (no CPU utilization metric)→%d%%", want.TargetCPUPercent))
	} else {
		add("cpu", cur.TargetCPUPercent, want.TargetCPUPercent, "%")
	}
	add("scale-down window", cur.ScaleDownStabilizationSeconds, want.ScaleDownStabilizationSeconds, "s")
	return strings.Join(parts, ", ")
}

// NextHPAStep is how far one patch moves cur toward want.
//
// The CPU target and the stabilization window are set outright: they change
// how the HPA decides, not how many schedulers run, and the HPA's own
// behavior policies bound the scaling that follows. The replica bounds act at
// once -- an HPA clamps its Deployment to [min, max] on its next sync,
// ignoring the stabilization window -- so each moves by at most a factor of two
// per patch (and always by at least one): lowering max from 20 to 2 takes
// four patches (10, 5, 3, 2) rather than removing eighteen schedulers in one
// sync. The caller spaces patches out; together that bounds the rate at which
// a saved setting resizes the scheduler fleet.
func NextHPAStep(cur, want HPASettings) HPASettings {
	next := want
	next.MaxReplicas = boundedReplicas(cur.MaxReplicas, want.MaxReplicas)
	next.MinReplicas = boundedReplicas(cur.MinReplicas, want.MinReplicas)
	if next.MinReplicas > next.MaxReplicas {
		// Cannot arise from valid inputs (both bounds move monotonically
		// toward a valid target); kept so a malformed observation can never
		// produce an HPA the API server rejects.
		next.MinReplicas = next.MaxReplicas
	}
	return next
}

func boundedReplicas(cur, want int32) int32 {
	if cur < 1 {
		// An HPA's minReplicas defaults to 1 and maxReplicas is at least 1;
		// anything lower is a malformed observation, not a starting point.
		cur = 1
	}
	switch {
	case want > cur:
		return min(want, cur*2)
	case want < cur:
		return max(want, (cur+1)/2)
	}
	return cur
}
