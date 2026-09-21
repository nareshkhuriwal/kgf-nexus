// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package platform

import "math"

type Demand struct {
	Pending        int   `json:"pending_tasks"`
	Running        int   `json:"running_tasks"`
	CPU            int64 `json:"cpu_millis"`
	Memory         int64 `json:"memory_mib"`
	GPU            int   `json:"gpu_count"`
	CurrentWorkers int   `json:"current_workers"`
	IdleSeconds    int   `json:"idle_seconds"`
	Unschedulable  int   `json:"unschedulable_tasks"`
}
type Decision struct {
	Pool    string `json:"pool"`
	Current int    `json:"current"`
	Desired int    `json:"desired"`
	Reason  string `json:"reason"`
	Limited bool   `json:"limited"`
	Demand  Demand `json:"demand"`
}

// Plan is deterministic. Demand is the sum of PENDING and in-flight resources,
// not utilization. A zero/failed observation must never be passed as real data.
func Plan(p Pool, profile PodProfile, d Demand) Decision {
	r := Decision{Pool: p.Name, Current: d.CurrentWorkers, Desired: d.CurrentWorkers, Demand: d}
	if !p.Enabled {
		r.Reason = "automatic scaling disabled"
		return r
	}
	desired := int(math.Ceil(float64(d.Pending+d.Running) / float64(p.TasksPerWorker)))
	for _, v := range []int{int(math.Ceil(float64(d.CPU) / float64(profile.CPU))), int(math.Ceil(float64(d.Memory) / float64(profile.Memory)))} {
		if v > desired {
			desired = v
		}
	}
	if profile.GPU > 0 {
		n := int(math.Ceil(float64(d.GPU) / float64(profile.GPU)))
		if n > desired {
			desired = n
		}
	}
	if desired < p.MinWorkers {
		desired = p.MinWorkers
	}
	if desired > p.MaxWorkers {
		desired = p.MaxWorkers
		r.Limited = true
	}
	if desired > d.CurrentWorkers+p.MaxScaleUpStep {
		desired = d.CurrentWorkers + p.MaxScaleUpStep
		r.Limited = true
	}
	if desired < d.CurrentWorkers && (d.Running > 0 || d.Pending > 0 || d.IdleSeconds < p.IdleSeconds) {
		r.Reason = "scale-in held until the pool is idle for its cooldown"
		return r
	}
	r.Desired = desired
	switch {
	case desired > d.CurrentWorkers:
		r.Reason = "pending resource demand"
	case desired < d.CurrentWorkers:
		r.Reason = "idle pool cooldown elapsed; drain before removal"
	default:
		r.Reason = "capacity within policy bounds"
	}
	if d.Unschedulable > 0 {
		r.Reason += "; some tasks cannot fit this profile"
	}
	return r
}
