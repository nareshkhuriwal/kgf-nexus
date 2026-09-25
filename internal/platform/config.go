// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
// Package platform defines versioned, validated infrastructure policy. It never
// changes task delivery semantics or puts cloud credentials into workflow data.
package platform

import (
	"fmt"
	"regexp"
	"strings"
)

type Config struct {
	Revision  int64        `json:"revision"`
	Scheduler Scheduler    `json:"scheduler"`
	Pools     []Pool       `json:"pools"`
	Profiles  []PodProfile `json:"profiles"`
	// Quotas are the namespace's limits (ADR-0021). A pointer so a document
	// that omits them -- an older console, a scripted PUT -- is told apart from
	// one that sets them to unlimited: the platform-config route keeps the
	// stored quotas in the first case, and quotas have their own route and
	// permission in any case.
	Quotas *Quotas `json:"quotas,omitempty"`
}
type Scheduler struct {
	MinReplicas      int `json:"min_replicas"`
	MaxReplicas      int `json:"max_replicas"`
	TargetCPU        int `json:"target_cpu_percent"`
	ScaleDownSeconds int `json:"scale_down_seconds"`
	// FairShareWeight is this namespace's weight in the dispatch claim's
	// weighted fair share (ADR-0017): with weights 3 and 1, two namespaces with
	// unbounded backlogs on one queue are served 3:1. The scheduler reads it
	// through the scheduler_namespace_weights view (migration 0022). Bounded to
	// 1-100 so no namespace can be configured out of service (0) or made
	// effectively exclusive.
	FairShareWeight int `json:"fair_share_weight"`
}

// Fair-share weight bounds, exported so the API, the view's clamp and the UI
// agree on one range.
const (
	MinFairShareWeight     = 1
	MaxFairShareWeight     = 100
	DefaultFairShareWeight = 1
)

type Pool struct {
	Name           string `json:"name"`
	Queue          string `json:"queue"`
	Profile        string `json:"profile"`
	MinWorkers     int    `json:"min_workers"`
	MaxWorkers     int    `json:"max_workers"`
	TasksPerWorker int    `json:"tasks_per_worker"`
	IdleSeconds    int    `json:"idle_seconds"`
	MaxScaleUpStep int    `json:"max_scale_up_step"`
	Enabled        bool   `json:"enabled"`
}
type PodProfile struct {
	Name                string            `json:"name"`
	Cloud               string            `json:"cloud"`
	KubernetesNamespace string            `json:"kubernetes_namespace"`
	ServiceAccount      string            `json:"service_account"`
	Image               string            `json:"image"`
	CPU                 int64             `json:"cpu_millis"`
	Memory              int64             `json:"memory_mib"`
	GPU                 int               `json:"gpu_count"`
	EphemeralStorage    int64             `json:"ephemeral_storage_mib"`
	NodeSelector        map[string]string `json:"node_selector"`
	Tolerations         []Toleration      `json:"tolerations"`
	DeadlineSeconds     int               `json:"deadline_seconds"`
	GraceSeconds        int               `json:"grace_seconds"`
}
type Toleration struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func Name(s string) bool { return len(s) > 0 && len(s) <= 63 && namePattern.MatchString(s) }
func Default() Config {
	return Config{Scheduler: Scheduler{1, 4, 65, 300, DefaultFairShareWeight}, Pools: []Pool{}, Profiles: []PodProfile{}}
}
func (c Config) Validate() error {
	if c.Revision < 0 {
		return fmt.Errorf("revision cannot be negative")
	}
	s := c.Scheduler
	if s.MinReplicas < 1 || s.MaxReplicas < s.MinReplicas || s.MaxReplicas > 100 || s.TargetCPU < 10 || s.TargetCPU > 90 || s.ScaleDownSeconds < 60 || s.ScaleDownSeconds > 3600 {
		return fmt.Errorf("scheduler requires 1 ≤ min ≤ max ≤ 100, CPU target 10–90%% and cooldown 60–3600 seconds")
	}
	if s.FairShareWeight < MinFairShareWeight || s.FairShareWeight > MaxFairShareWeight {
		return fmt.Errorf("fair-share weight must be %d–%d", MinFairShareWeight, MaxFairShareWeight)
	}
	if c.Quotas != nil {
		if err := c.Quotas.Validate(); err != nil {
			return fmt.Errorf("quotas: %w", err)
		}
	}
	if len(c.Pools) > 100 || len(c.Profiles) > 100 {
		return fmt.Errorf("at most 100 pools and profiles per namespace")
	}
	profiles := map[string]bool{}
	for _, p := range c.Profiles {
		if !Name(p.Name) || profiles[p.Name] {
			return fmt.Errorf("profile names must be unique DNS labels")
		}
		profiles[p.Name] = true
		if p.Cloud != "aws" && p.Cloud != "azure" && p.Cloud != "kubernetes" {
			return fmt.Errorf("unsupported cloud %q", p.Cloud)
		}
		if !Name(p.KubernetesNamespace) || !Name(p.ServiceAccount) {
			return fmt.Errorf("profile %s requires a Kubernetes namespace and service account", p.Name)
		}
		if len(p.Image) > 512 || !strings.Contains(p.Image, "@sha256:") || len(strings.Split(p.Image, "@sha256:")[1]) != 64 {
			return fmt.Errorf("profile %s requires an immutable image@sha256 digest", p.Name)
		}
		digest := strings.Split(p.Image, "@sha256:")[1]
		if matched, _ := regexp.MatchString(`^[a-f0-9]{64}$`, digest); !matched {
			return fmt.Errorf("invalid image digest")
		}
		if p.CPU < 100 || p.CPU > 1000000 || p.Memory < 128 || p.Memory > 4194304 || p.GPU < 0 || p.GPU > 128 || p.EphemeralStorage < 128 || p.EphemeralStorage > 4194304 {
			return fmt.Errorf("profile %s resource values are outside supported bounds", p.Name)
		}
		if p.DeadlineSeconds < 30 || p.DeadlineSeconds > 604800 || p.GraceSeconds < 1 || p.GraceSeconds > 600 {
			return fmt.Errorf("profile %s deadline must be 30–604800 seconds and grace 1–600 seconds", p.Name)
		}
		if len(p.NodeSelector) > 32 || len(p.Tolerations) > 32 {
			return fmt.Errorf("too many placement constraints")
		}
		for k, v := range p.NodeSelector {
			if len(k) > 253 || len(v) > 63 || strings.TrimSpace(k) == "" {
				return fmt.Errorf("invalid node selector")
			}
		}
		for _, t := range p.Tolerations {
			if t.Key == "" || len(t.Key) > 253 || len(t.Value) > 63 || (t.Effect != "NoSchedule" && t.Effect != "PreferNoSchedule" && t.Effect != "NoExecute") {
				return fmt.Errorf("invalid toleration")
			}
		}
	}
	pools := map[string]bool{}
	queues := map[string]bool{}
	for _, p := range c.Pools {
		if !Name(p.Name) || pools[p.Name] || !Name(p.Queue) || queues[p.Queue] {
			return fmt.Errorf("pool names and queues must be unique DNS labels")
		}
		pools[p.Name] = true
		queues[p.Queue] = true
		if !profiles[p.Profile] {
			return fmt.Errorf("pool %s references an unknown pod profile", p.Name)
		}
		if p.MinWorkers < 0 || p.MaxWorkers < p.MinWorkers || p.MaxWorkers > 10000 || p.TasksPerWorker < 1 || p.TasksPerWorker > 1000 || p.IdleSeconds < 60 || p.IdleSeconds > 86400 || p.MaxScaleUpStep < 1 || p.MaxScaleUpStep > 100 {
			return fmt.Errorf("invalid scaling bounds for pool %s", p.Name)
		}
	}
	return nil
}
