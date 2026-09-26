// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"math"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

func TestEligibility(t *testing.T) {
	spot := map[string]string{LabelCapacityType: "spot", LabelRegion: "eu-west-1", LabelCostTier: "2"}
	od := map[string]string{LabelRegion: "us-east-1"}
	onDemand := &Placement{Capacity: nexusv1.CapacityPreference_CAPACITY_PREFERENCE_ON_DEMAND_ONLY}
	if ok, why := Eligible(spot, onDemand); ok || why == "" {
		t.Fatal("on_demand_only task eligible on a spot worker")
	}
	if ok, _ := Eligible(od, onDemand); !ok {
		t.Fatal("on_demand_only task ineligible on an unlabelled (on-demand) worker")
	}
	region := &Placement{RequiredLabels: map[string]string{LabelRegion: "eu-west-1"}}
	if ok, _ := Eligible(od, region); ok {
		t.Fatal("required region ignored")
	}
	if ok, _ := Eligible(spot, region); !ok {
		t.Fatal("matching region rejected")
	}
	if ok, _ := Eligible(spot, &Placement{MaxCostTier: 1}); ok {
		t.Fatal("tier 2 accepted under max tier 1")
	}
	if ok, _ := Eligible(od, &Placement{MaxCostTier: 1}); !ok {
		t.Fatal("untiered worker (tier 0) rejected")
	}
	if ok, _ := Eligible(spot, nil); !ok {
		t.Fatal("nil placement must be eligible everywhere")
	}
}

func TestClusterLabelsOverrideWorkerClaims(t *testing.T) {
	eff := EffectiveLabels(map[string]string{LabelRegion: "claimed", "team": "a"},
		map[string]string{LabelRegion: "eu-west-1"})
	if eff[LabelRegion] != "eu-west-1" || eff["team"] != "a" {
		t.Fatalf("effective labels %v", eff)
	}
}

func TestScorePrefersCacheLocalitySpotAndSameHost(t *testing.T) {
	capacity := &nexusv1.ResourceVector{CpuMillis: 8000, MemoryBytes: 16 * gib}
	req := &nexusv1.ResourceVector{CpuMillis: 1000}
	base := WorkerView{Hostname: "a", Labels: map[string]string{}, Capacity: capacity, Committed: &nexusv1.ResourceVector{}}
	cfg := ScoreConfig{Policy: PolicyPack, Cost: TierCostModel{}}

	cached := base
	cached.Hostname = "b"
	cached.Cached = map[string]bool{"sha256:m": true}
	p := &Placement{Artifacts: []ArtifactRef{{Key: "sha256:m", SizeBytes: 40 * gib}}}
	if !BetterWorker(PlacementScore(cached, req, p, cfg), PlacementScore(base, req, p, cfg)) {
		t.Fatal("a worker holding the model does not beat one that must download it")
	}

	local := base
	local.Labels = map[string]string{LabelDataLocality: "lake-eu|s3-eu-west-1"}
	pl := &Placement{PreferredDataLocality: []string{"s3-eu-west-1"}}
	if !BetterWorker(PlacementScore(local, req, pl, cfg), PlacementScore(base, req, pl, cfg)) {
		t.Fatal("data locality not preferred")
	}

	spot := base
	spot.Labels = map[string]string{LabelCapacityType: "spot"}
	ps := &Placement{Capacity: nexusv1.CapacityPreference_CAPACITY_PREFERENCE_SPOT_PREFERRED}
	if !BetterWorker(PlacementScore(spot, req, ps, cfg), PlacementScore(base, req, ps, cfg)) {
		t.Fatal("spot_preferred did not prefer spot")
	}

	same := &Placement{SameHost: "a"}
	other := base
	other.Hostname = "z"
	if !BetterWorker(PlacementScore(base, req, same, cfg), PlacementScore(other, req, same, cfg)) {
		t.Fatal("prefer_same_worker_as ignored")
	}

	cheap := base
	pricey := base
	pricey.Labels = map[string]string{LabelCostTier: "3"}
	if PlacementScore(cheap, req, nil, cfg) <= PlacementScore(pricey, req, nil, cfg) {
		t.Fatal("cost hook did not favour the cheaper worker")
	}
}

func TestScorePackVersusSpread(t *testing.T) {
	req := &nexusv1.ResourceVector{GpuCount: 1}
	busy := WorkerView{Capacity: &nexusv1.ResourceVector{GpuCount: 4}, Committed: &nexusv1.ResourceVector{GpuCount: 3}}
	idle := WorkerView{Capacity: &nexusv1.ResourceVector{GpuCount: 4}, Committed: &nexusv1.ResourceVector{}}
	pack := ScoreConfig{Policy: PolicyPack}
	spread := ScoreConfig{Policy: PolicySpread}
	if PlacementScore(busy, req, nil, pack) <= PlacementScore(idle, req, nil, pack) {
		t.Fatal("pack did not prefer the tighter worker")
	}
	if PlacementScore(idle, req, nil, spread) <= PlacementScore(busy, req, nil, spread) {
		t.Fatal("spread did not prefer the emptier worker")
	}
	full := WorkerView{Capacity: &nexusv1.ResourceVector{GpuCount: 1}, Committed: &nexusv1.ResourceVector{GpuCount: 1}}
	if !math.IsInf(PlacementScore(full, req, nil, pack), -1) {
		t.Fatal("an unfittable worker scored finitely")
	}
}

func TestWantsScoring(t *testing.T) {
	if WantsScoring(&nexusv1.ResourceVector{CpuMillis: 1}, nil) {
		t.Fatal("a plain CPU task must not trigger scoring (non-GPU dispatch cost must stay unchanged)")
	}
	if !WantsScoring(&nexusv1.ResourceVector{GpuShareMemoryBytes: 1}, nil) {
		t.Fatal("a shared-GPU task must be scored")
	}
	if !WantsScoring(&nexusv1.ResourceVector{}, &Placement{SameHost: "h"}) {
		t.Fatal("a preference must trigger scoring")
	}
	if WantsScoring(&nexusv1.ResourceVector{}, &Placement{RequiredLabels: map[string]string{"a": "b"}}) {
		t.Fatal("hard constraints alone are eligibility, not scoring")
	}
}

func TestPreemptionBudgetIsSeparate(t *testing.T) {
	req := nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED
	// Normal budget exhausted, preemption budget not: still retried, charged to preemptions.
	d, charge := ShouldRetryPreempted(req, 3, 3, 0, 2, false, nil)
	if !d.Retry || !charge || d.Reason != ReasonRetryPreempted {
		t.Fatalf("got %+v charge=%v", d, charge)
	}
	// Preemption budget exhausted: falls through to the normal budget.
	d, charge = ShouldRetryPreempted(req, 1, 3, 2, 2, false, nil)
	if !d.Retry || charge || d.Reason != ReasonRetryTransient {
		t.Fatalf("over the preemption cap: %+v charge=%v", d, charge)
	}
	d, _ = ShouldRetryPreempted(req, 3, 3, 2, 2, false, nil)
	if d.Retry {
		t.Fatal("both budgets exhausted but retried")
	}
	// The author's veto wins.
	d, _ = ShouldRetryPreempted(req, 0, 3, 0, 2, false,
		&nexusv1.RetryPolicy{NoRetryOn: []nexusv1.ErrorClass{nexusv1.ErrorClass_ERROR_CLASS_PREEMPTED}})
	if d.Retry {
		t.Fatal("no_retry_on: [preempted] ignored")
	}
	// Default cap applies when unset.
	d, charge = ShouldRetryPreempted(req, 0, 1, DefaultMaxPreemptions-1, 0, false, nil)
	if !d.Retry || !charge {
		t.Fatal("default cap not applied")
	}
}
