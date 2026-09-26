// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Claim-time placement: hard eligibility and the soft placement score
// (ADR-0029 §4, ADR-0030, ADR-0031).
//
// Nothing here decides over-allocation. That is Fits, evaluated under the
// worker's row lock. Eligibility only removes workers a task must never run on,
// and the score only chooses among (push) or defers from (pull) workers that
// already fit -- so a wrong score costs placement quality, never correctness.

// Well-known labels. Workers carry them; cluster-scoped ones are overlaid from
// the worker's cluster (cluster_status.labels) at claim time.
const (
	LabelCluster          = "nexus.io/cluster"
	LabelRegion           = "nexus.io/region"
	LabelProvider         = "nexus.io/provider"
	LabelCostTier         = "nexus.io/cost-tier"
	LabelDataLocality     = "nexus.io/data-locality"
	LabelCapacityType     = "nexus.io/capacity-type"
	LabelK8sTaskNamespace = "nexus.io/k8s-task-namespace"

	CapacityTypeSpot     = "spot"
	CapacityTypeOnDemand = "on-demand"
)

// EffectiveLabels overlays a worker's cluster labels on its own. Cluster labels
// win: the controller observes them, the agent merely claims them (ADR-0031).
func EffectiveLabels(worker, cluster map[string]string) map[string]string {
	if len(cluster) == 0 {
		return worker
	}
	out := make(map[string]string, len(worker)+len(cluster))
	for k, v := range worker {
		out[k] = v
	}
	for k, v := range cluster {
		out[k] = v
	}
	return out
}

// IsSpot reports whether labels mark spot/preemptible capacity.
func IsSpot(labels map[string]string) bool {
	return strings.EqualFold(labels[LabelCapacityType], CapacityTypeSpot)
}

// CostTier reads nexus.io/cost-tier; absent or malformed is tier 0.
func CostTier(labels map[string]string) int {
	n, err := strconv.Atoi(strings.TrimSpace(labels[LabelCostTier]))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// LocalityTags splits nexus.io/data-locality ("a|b|c").
func LocalityTags(labels map[string]string) []string {
	v := labels[LabelDataLocality]
	if v == "" {
		return nil
	}
	var out []string
	for _, t := range strings.Split(v, "|") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Placement is the scheduler's view of a task's PlacementSpec, decoded from
// task_nodes.spec_json in either dialect.
type Placement struct {
	Capacity              nexusv1.CapacityPreference
	RequiredLabels        map[string]string
	MaxCostTier           int
	PreferredDataLocality []string
	PreferredLabels       map[string]string
	Artifacts             []ArtifactRef
	MaxPreemptions        int32
	// SameHost is the hostname prefer_same_worker_as resolved to (the worker
	// whose attempt of that node most recently succeeded in this run), or "".
	SameHost string
}

// ArtifactRef is one declared cacheable artifact, as scoring needs it.
type ArtifactRef struct {
	Key       string
	SizeBytes int64
}

// HasHardConstraints reports whether eligibility must be evaluated at all.
func (p *Placement) HasHardConstraints() bool {
	return p != nil && (p.Capacity == nexusv1.CapacityPreference_CAPACITY_PREFERENCE_ON_DEMAND_ONLY ||
		len(p.RequiredLabels) > 0 || p.MaxCostTier > 0)
}

// HasPreferences reports whether any soft preference exists, i.e. whether a
// different worker could ever score better for a reason other than packing.
func (p *Placement) HasPreferences() bool {
	return p != nil && (p.Capacity == nexusv1.CapacityPreference_CAPACITY_PREFERENCE_SPOT_PREFERRED ||
		len(p.PreferredDataLocality) > 0 || len(p.PreferredLabels) > 0 ||
		len(p.Artifacts) > 0 || p.SameHost != "")
}

// WantsScoring reports whether placing this request is a choice worth making:
// a GPU request (packing matters) or any soft preference. A CPU-only task with
// no placement hints returns false, and the pull claim then never reads other
// workers -- which is what keeps non-GPU dispatch cost unchanged.
func WantsScoring(req *nexusv1.ResourceVector, p *Placement) bool {
	return UsesGPU(req) || p.HasPreferences()
}

// Eligible applies the hard placement constraints to a worker's effective
// labels. The reason is a pending_reason fragment.
func Eligible(labels map[string]string, p *Placement) (bool, string) {
	if p == nil {
		return true, ""
	}
	if p.Capacity == nexusv1.CapacityPreference_CAPACITY_PREFERENCE_ON_DEMAND_ONLY && IsSpot(labels) {
		return false, "on_demand_only (worker is spot)"
	}
	for k, v := range p.RequiredLabels {
		if labels[k] != v {
			return false, fmt.Sprintf("required label %s=%s (worker has %q)", k, v, labels[k])
		}
	}
	if p.MaxCostTier > 0 && CostTier(labels) > p.MaxCostTier {
		return false, fmt.Sprintf("cost tier %d above max %d", CostTier(labels), p.MaxCostTier)
	}
	return true, ""
}

// CostModel prices placing a request on a worker; lower is cheaper. The hook a
// cost layer replaces later (ADR-0030 §6). Implementations must be pure and
// cheap: they run inside the claim transaction.
type CostModel interface {
	Cost(labels map[string]string, req *nexusv1.ResourceVector) float64
}

// TierCostModel is the default CostModel: the worker's nexus.io/cost-tier.
type TierCostModel struct{}

// Cost implements CostModel.
func (TierCostModel) Cost(labels map[string]string, _ *nexusv1.ResourceVector) float64 {
	return float64(CostTier(labels))
}

// WorkerView is one worker as scoring sees it.
type WorkerView struct {
	Hostname  string
	Labels    map[string]string // effective labels
	Capacity  *nexusv1.ResourceVector
	Committed *nexusv1.ResourceVector
	// Cached is the set of artifact keys in the worker's node-local cache.
	Cached map[string]bool
}

// Score weights. Cache affinity dominates packing on purpose: a cache miss on
// a 40 GB model costs minutes, a slightly worse bin costs nothing measurable.
const (
	weightPack     = 1.0
	weightCache    = 4.0
	weightSameHost = 3.0
	weightLocality = 2.0
	weightLabels   = 1.0
	weightSpot     = 1.0
	weightCost     = 0.25

	// ScoreMargin is how much better another worker must score before the pull
	// claim defers to it. Without a margin, floating-point noise and a worker's
	// own reservation would make two workers defer to each other.
	ScoreMargin = 0.05
)

// ScoreConfig parameterizes PlacementScore.
type ScoreConfig struct {
	Policy PlacementPolicy
	Cost   CostModel
}

// PlacementScore rates placing req on w: HIGHER IS BETTER. -Inf when req does
// not fit, so an unfittable worker can never win.
func PlacementScore(w WorkerView, req *nexusv1.ResourceVector, p *Placement, cfg ScoreConfig) float64 {
	bf := BestFitScore(w.Capacity, w.Committed, req)
	if math.IsInf(bf, 1) {
		return math.Inf(-1)
	}
	var s float64
	if cfg.Policy == PolicySpread {
		s = weightPack * bf
	} else {
		s = weightPack * (1 - bf)
	}
	if p != nil {
		s += weightCache * cacheFraction(w.Cached, p.Artifacts)
		if p.SameHost != "" && strings.EqualFold(p.SameHost, w.Hostname) {
			s += weightSameHost
		}
		if len(p.PreferredDataLocality) > 0 && intersects(LocalityTags(w.Labels), p.PreferredDataLocality) {
			s += weightLocality
		}
		if n := len(p.PreferredLabels); n > 0 {
			hit := 0
			for k, v := range p.PreferredLabels {
				if w.Labels[k] == v {
					hit++
				}
			}
			s += weightLabels * float64(hit) / float64(n)
		}
		if p.Capacity == nexusv1.CapacityPreference_CAPACITY_PREFERENCE_SPOT_PREFERRED && IsSpot(w.Labels) {
			s += weightSpot
		}
	}
	if cfg.Cost != nil {
		s -= weightCost * cfg.Cost.Cost(w.Labels, req)
	}
	return s
}

// cacheFraction is the share of declared artifact bytes already cached (by
// count when no sizes are declared).
func cacheFraction(cached map[string]bool, arts []ArtifactRef) float64 {
	if len(arts) == 0 || len(cached) == 0 {
		return 0
	}
	var total, hit float64
	for _, a := range arts {
		w := float64(a.SizeBytes)
		if w <= 0 {
			w = 1
		}
		total += w
		if a.Key != "" && cached[a.Key] {
			hit += w
		}
	}
	if total == 0 {
		return 0
	}
	return hit / total
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// BetterWorker reports whether candidate beats mine by the deferral margin.
func BetterWorker(candidate, mine float64) bool {
	if math.IsInf(candidate, -1) {
		return false
	}
	if math.IsInf(mine, -1) {
		return true
	}
	return candidate > mine+ScoreMargin
}

// SortedKeys is a helper for deterministic iteration in callers and tests.
func SortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
