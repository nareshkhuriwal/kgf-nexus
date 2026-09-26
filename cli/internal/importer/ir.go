// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package importer

// The intermediate representation (IR) every source is converted to first.
//
// Airflow and Prefect files are analysed by the embedded Python program
// (pyast/), which prints this shape as JSON; Argo YAML is read in Go and
// produces the same structs. One builder (build.go) then turns the IR into a
// WorkflowSpec, so naming rules, synthetic nodes and validation are the same
// for every source. The JSON tags are the contract with pyast/nx_values.py.

// Status classifies how faithfully a construct was converted.
const (
	Translated   = "translated"   // same behaviour in Nexus
	Approximated = "approximated" // converted, but behaviour differs in a way the message states
	Unsupported  = "unsupported"  // not converted; the spec is incomplete without manual work
)

// Finding is one row of the report.
type Finding struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Construct string `json:"construct"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	// Nexus names what the construct became, e.g. "tasks[load].retry".
	Nexus string `json:"nexus,omitempty"`
}

// ValueDecl is an input/output/param declaration.
type ValueDecl struct {
	Type        string  `json:"type"`
	Required    bool    `json:"required,omitempty"`
	DefaultJSON *string `json:"default_json,omitempty"`
	Line        int     `json:"line,omitempty"`
}

// Retry is already in the schema's RetryPolicy form.
type Retry struct {
	MaxAttempts            int      `json:"max_attempts" yaml:"max_attempts"`
	Strategy               string   `json:"strategy,omitempty" yaml:"strategy,omitempty"`
	InitialMs              int64    `json:"initial_ms,omitempty" yaml:"initial_ms,omitempty"`
	MaxMs                  int64    `json:"max_ms,omitempty" yaml:"max_ms,omitempty"`
	Multiplier             float64  `json:"multiplier,omitempty" yaml:"multiplier,omitempty"`
	Jitter                 string   `json:"jitter,omitempty" yaml:"jitter,omitempty"`
	RetryOn                []string `json:"retry_on,omitempty" yaml:"retry_on,omitempty"`
	RetryOnDifferentWorker bool     `json:"retry_on_different_worker,omitempty" yaml:"retry_on_different_worker,omitempty"`
}

// Timeouts is the schema's Timeouts.
type Timeouts struct {
	ScheduleToStartMs int64 `json:"schedule_to_start_ms,omitempty" yaml:"schedule_to_start_ms,omitempty"`
	StartToCloseMs    int64 `json:"start_to_close_ms,omitempty" yaml:"start_to_close_ms,omitempty"`
	ScheduleToCloseMs int64 `json:"schedule_to_close_ms,omitempty" yaml:"schedule_to_close_ms,omitempty"`
	HeartbeatMs       int64 `json:"heartbeat_ms,omitempty" yaml:"heartbeat_ms,omitempty"`
}

// Cache is the schema's CachePolicy.
type Cache struct {
	Enabled bool  `json:"enabled" yaml:"enabled"`
	TTLMs   int64 `json:"ttl_ms,omitempty" yaml:"ttl_ms,omitempty"`
}

// K8sResources are Kubernetes-style quantities ("500m", "1Gi", "nvidia.com/gpu": "1"),
// normalized to the schema's scalar ResourceVector by the builder.
type K8sResources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// IRMap describes a map template. Exactly one collection source is set:
// Over (an upstream task's output), LiteralItemsJSON (a list known now), or
// OverParam (a workflow param).
type IRMap struct {
	Over             string `json:"over,omitempty"`
	OverOutput       string `json:"over_output,omitempty"`
	ItemInput        string `json:"item_input"`
	MaxFanout        int    `json:"max_fanout,omitempty"` // 0 = the --max-fanout default
	LiteralItemsJSON string `json:"literal_items_json,omitempty"`
	OverParam        string `json:"over_param,omitempty"`
}

// Task kinds.
const (
	KindShell       = "shell"       // Script run by Interpreter -c, subprocess executor
	KindPython      = "python"      // Module.Callable through nexus.entrypoint
	KindContainer   = "container"   // Image + Command/Args, docker or kubernetes executor
	KindNoop        = "noop"        // succeeds immediately
	KindPlaceholder = "placeholder" // fails with Reason: an unsupported construct stands here
)

// IRTask is one node.
type IRTask struct {
	ID          string   `json:"id"`
	Line        int      `json:"line"`
	Kind        string   `json:"kind"`
	Script      string   `json:"script,omitempty"`
	Interpreter string   `json:"interpreter,omitempty"`
	Command     []string `json:"command,omitempty"`
	Args        []string `json:"args,omitempty"`
	Image       string   `json:"image,omitempty"`
	Executor    string   `json:"executor,omitempty"`
	Module      string   `json:"module,omitempty"`
	Callable    string   `json:"callable,omitempty"`
	Reason      string   `json:"reason,omitempty"`

	Env           map[string]string    `json:"env,omitempty"`
	WorkingDir    string               `json:"working_dir,omitempty"`
	Inputs        map[string]ValueDecl `json:"inputs,omitempty"`
	Outputs       map[string]ValueDecl `json:"outputs,omitempty"`
	InputLiterals map[string]string    `json:"input_literals,omitempty"`
	InputParams   map[string]string    `json:"input_params,omitempty"`
	StdoutOutput  string               `json:"stdout_output,omitempty"`

	Retry            *Retry        `json:"retry,omitempty"`
	Timeouts         *Timeouts     `json:"timeouts,omitempty"`
	TriggerRule      string        `json:"trigger_rule,omitempty"`
	Queue            string        `json:"queue,omitempty"`
	Priority         *int          `json:"priority,omitempty"`
	ConcurrencyKey   string        `json:"concurrency_key,omitempty"`
	ConcurrencyLimit int           `json:"concurrency_limit,omitempty"`
	Cache            *Cache        `json:"cache,omitempty"`
	Map              *IRMap        `json:"map,omitempty"`
	K8sResources     *K8sResources `json:"k8s_resources,omitempty"`
}

// IREdge is a dependency; Inputs maps downstream input -> upstream output.
type IREdge struct {
	From   string            `json:"from"`
	To     string            `json:"to"`
	Inputs map[string]string `json:"inputs,omitempty"`
	Line   int               `json:"line"`
}

// IRSchedule is what the source says about WHEN runs start. Nexus schedules
// are separate API objects, so these become companion commands, never spec.
type IRSchedule struct {
	// File is set when the schedule comes from another file than the
	// workflow (a prefect.yaml deployment), so its findings point there.
	File            string         `json:"file,omitempty"`
	Line            int            `json:"line"`
	Cron            string         `json:"cron,omitempty"`
	IntervalSeconds float64        `json:"interval_seconds,omitempty"`
	None            bool           `json:"none,omitempty"`
	Unspecified     bool           `json:"unspecified,omitempty"`
	Unsupported     string         `json:"unsupported,omitempty"`
	Timezone        string         `json:"timezone,omitempty"`
	StartDate       string         `json:"start_date,omitempty"`
	EndDate         string         `json:"end_date,omitempty"`
	Catchup         *bool          `json:"catchup,omitempty"`
	MaxActiveRuns   *int           `json:"max_active_runs,omitempty"`
	Disabled        bool           `json:"disabled,omitempty"`
	Note            string         `json:"note,omitempty"`
	Via             string         `json:"via,omitempty"`
	Params          map[string]any `json:"params,omitempty"`
	Overlap         string         `json:"overlap,omitempty"`
	OverlapNote     string         `json:"overlap_note,omitempty"`
}

// IRWorkflow is one DAG / flow / Argo workflow.
type IRWorkflow struct {
	SourceID      string               `json:"source_id"`
	File          string               `json:"file"`
	Line          int                  `json:"line"`
	Description   string               `json:"description,omitempty"`
	Params        map[string]ValueDecl `json:"params,omitempty"`
	Tasks         []*IRTask            `json:"tasks"`
	Edges         []IREdge             `json:"edges,omitempty"`
	Schedules     []IRSchedule         `json:"schedules,omitempty"`
	RunTimeoutMs  *int64               `json:"run_timeout_ms,omitempty"`
	MaxActiveRuns *int                 `json:"max_active_runs,omitempty"`
	FailFast      *bool                `json:"fail_fast,omitempty"`
	Labels        map[string]string    `json:"labels,omitempty"`
	Function      string               `json:"function,omitempty"`
	Findings      []Finding            `json:"findings"`
}

func (w *IRWorkflow) find(line int, construct, status, message, nexus string) {
	w.Findings = append(w.Findings, Finding{File: w.File, Line: line, Construct: construct,
		Status: status, Message: message, Nexus: nexus})
}

// irFile is one analysed file.
type irFile struct {
	Path      string        `json:"path"`
	Workflows []*IRWorkflow `json:"workflows"`
	Findings  []Finding     `json:"findings"`
	Error     string        `json:"error,omitempty"`
}

// irDocument is the Python analyzer's output.
type irDocument struct {
	IRVersion int      `json:"ir_version"`
	Source    string   `json:"source"`
	Python    string   `json:"python"`
	Files     []irFile `json:"files"`
}
