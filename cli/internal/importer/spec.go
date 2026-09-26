// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package importer

// The WorkflowSpec the importer writes. Structs rather than maps so the YAML
// reads top-down in a sensible order (identity, params, tasks, edges); maps
// inside are emitted with sorted keys by yaml.v3, so output is deterministic.

// Spec is a WorkflowSpec (schemas/workflow-spec-v1.json).
type Spec struct {
	APIVersion    string               `json:"api_version" yaml:"api_version"`
	Namespace     string               `json:"namespace" yaml:"namespace"`
	Name          string               `json:"name" yaml:"name"`
	Description   string               `json:"description,omitempty" yaml:"description,omitempty"`
	Labels        map[string]string    `json:"labels,omitempty" yaml:"labels,omitempty"`
	Params        map[string]ValueSpec `json:"params,omitempty" yaml:"params,omitempty"`
	RunTimeoutMs  int64                `json:"run_timeout_ms,omitempty" yaml:"run_timeout_ms,omitempty"`
	MaxActiveRuns int                  `json:"max_active_runs,omitempty" yaml:"max_active_runs,omitempty"`
	FailFast      bool                 `json:"fail_fast,omitempty" yaml:"fail_fast,omitempty"`
	Tasks         []TaskSpec           `json:"tasks" yaml:"tasks"`
	Edges         []EdgeSpec           `json:"edges,omitempty" yaml:"edges,omitempty"`
}

// ValueSpec is an input/output/param type declaration.
type ValueSpec struct {
	Type        string `json:"type" yaml:"type"`
	Required    bool   `json:"required,omitempty" yaml:"required,omitempty"`
	DefaultJSON string `json:"default_json,omitempty" yaml:"default_json,omitempty"`
}

// TaskSpec is one node.
type TaskSpec struct {
	ID               string               `json:"id" yaml:"id"`
	TaskRef          string               `json:"task_ref" yaml:"task_ref"`
	Inputs           map[string]ValueSpec `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	Outputs          map[string]ValueSpec `json:"outputs,omitempty" yaml:"outputs,omitempty"`
	InputLiterals    map[string]string    `json:"input_literals,omitempty" yaml:"input_literals,omitempty"`
	InputParams      map[string]string    `json:"input_params,omitempty" yaml:"input_params,omitempty"`
	Map              *MapSpec             `json:"map,omitempty" yaml:"map,omitempty"`
	Resources        *ResourceRequest     `json:"resources,omitempty" yaml:"resources,omitempty"`
	Queue            string               `json:"queue,omitempty" yaml:"queue,omitempty"`
	Priority         *int                 `json:"priority,omitempty" yaml:"priority,omitempty"`
	TriggerRule      string               `json:"trigger_rule,omitempty" yaml:"trigger_rule,omitempty"`
	Timeouts         *Timeouts            `json:"timeouts,omitempty" yaml:"timeouts,omitempty"`
	Retry            *Retry               `json:"retry,omitempty" yaml:"retry,omitempty"`
	Cache            *Cache               `json:"cache,omitempty" yaml:"cache,omitempty"`
	ConcurrencyKey   string               `json:"concurrency_key,omitempty" yaml:"concurrency_key,omitempty"`
	ConcurrencyLimit int                  `json:"concurrency_limit,omitempty" yaml:"concurrency_limit,omitempty"`
	Runtime          *RuntimeSpec         `json:"runtime,omitempty" yaml:"runtime,omitempty"`
}

// MapSpec is dynamic expansion.
type MapSpec struct {
	OverNodeID string `json:"over_node_id" yaml:"over_node_id"`
	OverOutput string `json:"over_output" yaml:"over_output"`
	ItemInput  string `json:"item_input" yaml:"item_input"`
	MaxFanout  int    `json:"max_fanout" yaml:"max_fanout"`
}

// ResourceVector holds base units only.
type ResourceVector struct {
	CPUMillis   int64            `json:"cpu_millis,omitempty" yaml:"cpu_millis,omitempty"`
	MemoryBytes int64            `json:"memory_bytes,omitempty" yaml:"memory_bytes,omitempty"`
	GPUCount    int64            `json:"gpu_count,omitempty" yaml:"gpu_count,omitempty"`
	GPUKind     string           `json:"gpu_kind,omitempty" yaml:"gpu_kind,omitempty"`
	DiskBytes   int64            `json:"disk_bytes,omitempty" yaml:"disk_bytes,omitempty"`
	Custom      map[string]int64 `json:"custom,omitempty" yaml:"custom,omitempty"`
}

func (v *ResourceVector) empty() bool {
	return v == nil || (v.CPUMillis == 0 && v.MemoryBytes == 0 && v.GPUCount == 0 && v.DiskBytes == 0 &&
		v.GPUKind == "" && len(v.Custom) == 0)
}

// ResourceRequest is request (scheduling) and limit (enforcement).
type ResourceRequest struct {
	Request *ResourceVector `json:"request,omitempty" yaml:"request,omitempty"`
	Limit   *ResourceVector `json:"limit,omitempty" yaml:"limit,omitempty"`
}

// RuntimeSpec is how the worker runs the task.
type RuntimeSpec struct {
	Executor     string            `json:"executor" yaml:"executor"`
	Image        string            `json:"image,omitempty" yaml:"image,omitempty"`
	Command      []string          `json:"command,omitempty" yaml:"command,omitempty"`
	Args         []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Env          map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	WorkingDir   string            `json:"working_dir,omitempty" yaml:"working_dir,omitempty"`
	StdoutOutput string            `json:"stdout_output,omitempty" yaml:"stdout_output,omitempty"`
}

// EdgeSpec is a dependency, optionally carrying data.
type EdgeSpec struct {
	From          string            `json:"from_node_id" yaml:"from_node_id"`
	To            string            `json:"to_node_id" yaml:"to_node_id"`
	OutputToInput map[string]string `json:"output_to_input,omitempty" yaml:"output_to_input,omitempty"`
}
