// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"testing"
)

func TestNextHPAStepBoundsReplicaMovesAndConverges(t *testing.T) {
	s := func(min, max, cpu, win int32) HPASettings {
		return HPASettings{MinReplicas: min, MaxReplicas: max, TargetCPUPercent: cpu, ScaleDownStabilizationSeconds: win}
	}
	cases := []struct {
		name      string
		cur, want HPASettings
		steps     []HPASettings // every intermediate patch, ending at want
	}{
		{"one step when within a factor of two", s(2, 6, 65, 300), s(3, 10, 70, 600),
			[]HPASettings{s(3, 10, 70, 600)}},
		{"max lowered sharply is halved per patch", s(2, 20, 65, 300), s(1, 2, 65, 300),
			[]HPASettings{s(1, 10, 65, 300), s(1, 5, 65, 300), s(1, 3, 65, 300), s(1, 2, 65, 300)}},
		{"min raised sharply is doubled per patch", s(1, 4, 65, 300), s(8, 8, 65, 300),
			[]HPASettings{s(2, 8, 65, 300), s(4, 8, 65, 300), s(8, 8, 65, 300)}},
		{"cpu and window move outright", s(2, 6, 0, 300), s(2, 6, 50, 900),
			[]HPASettings{s(2, 6, 50, 900)}},
		{"both bounds down together keep min <= max", s(8, 10, 65, 300), s(1, 1, 65, 300),
			[]HPASettings{s(4, 5, 65, 300), s(2, 3, 65, 300), s(1, 2, 65, 300), s(1, 1, 65, 300)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur := tc.cur
			for i, want := range tc.steps {
				next := NextHPAStep(cur, tc.want)
				if next != want {
					t.Fatalf("step %d from %+v: got %+v, want %+v", i+1, cur, next, want)
				}
				if next.MinReplicas > next.MaxReplicas || next.MinReplicas < 1 {
					t.Fatalf("step %d produced invalid bounds %+v", i+1, next)
				}
				cur = next
			}
			if cur != tc.want {
				t.Fatalf("did not converge: %+v", cur)
			}
			if NextHPAStep(cur, tc.want) != tc.want {
				t.Fatalf("a converged HPA must not move")
			}
		})
	}
}

func TestHPASettingsDiffNamesOnlyWhatChanges(t *testing.T) {
	cur := HPASettings{MinReplicas: 2, MaxReplicas: 6, TargetCPUPercent: 65, ScaleDownStabilizationSeconds: 300}
	if d := cur.Diff(cur); d != "" {
		t.Fatalf("equal settings diff %q", d)
	}
	want := HPASettings{MinReplicas: 3, MaxReplicas: 6, TargetCPUPercent: 70, ScaleDownStabilizationSeconds: 600}
	if d := cur.Diff(want); d != "min 2→3, cpu 65%→70%, scale-down window 300s→600s" {
		t.Fatalf("diff %q", d)
	}
	noCPU := cur
	noCPU.TargetCPUPercent = 0
	if d := noCPU.Diff(cur); d != "cpu (no CPU utilization metric)→65%" {
		t.Fatalf("diff without a CPU metric %q", d)
	}
	if got := (Scheduler{MinReplicas: 1, MaxReplicas: 4, TargetCPU: 65, ScaleDownSeconds: 300}).HPASettings(); got !=
		(HPASettings{MinReplicas: 1, MaxReplicas: 4, TargetCPUPercent: 65, ScaleDownStabilizationSeconds: 300}) {
		t.Fatalf("saved policy in HPA terms: %+v", got)
	}
}

func TestSchedulerHPAIsControllerOwned(t *testing.T) {
	m := SchedulerHPA(Scheduler{MinReplicas: 2, MaxReplicas: 6, TargetCPU: 65, ScaleDownSeconds: 300}, "kgf-nexus", "scheduler")
	meta := m["metadata"].(map[string]any)
	if meta["annotations"].(map[string]string)[HPAOwnerAnnotation] != HPAOwnerValue {
		t.Fatalf("a generated scheduler HPA must carry the ownership opt-in: %v", meta)
	}
	labels := meta["labels"].(map[string]string)
	if labels["app.kubernetes.io/part-of"] != "kgf-nexus" || labels["app.kubernetes.io/component"] != "scheduler" {
		t.Fatalf("labels %v", labels)
	}
	spec := m["spec"].(map[string]any)
	if spec["minReplicas"] != 2 || spec["maxReplicas"] != 6 {
		t.Fatalf("spec %v", spec)
	}
}
